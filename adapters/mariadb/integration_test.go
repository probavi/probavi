//go:build integration

package main_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/capabilities"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

// verifiedImage is the engine image this run restores from: the manifest's
// baseline, or the version-matrix job's PROBAVI_IT_IMAGE when it names one
// the manifest already lists. The manifest and this suite read the same
// values, so docs/capabilities.json can never claim an engine version CI
// does not actually restore from (docs/capabilities.md §1,
// docs/engine-versions.md §2).
func verifiedImage(t *testing.T) string {
	t.Helper()
	m, err := capabilities.LoadAdapterManifest(".")
	if err != nil {
		t.Fatalf("load adapter manifest: %v", err)
	}
	image, err := m.SandboxImage(os.Getenv("PROBAVI_IT_IMAGE"))
	if err != nil {
		t.Fatalf("adapter manifest: %v", err)
	}
	return image
}

// sandboxParams returns the documented drill-config sandbox params: an
// empty root password is acceptable only because the sandbox has zero
// ingress (--network none, no ports expressible).
func sandboxParams(t *testing.T) map[string]string {
	return map[string]string{
		"image": verifiedImage(t), "env.MARIADB_ALLOW_EMPTY_ROOT_PASSWORD": "yes",
		"memory": engineMemoryLimit,
	}
}

// engineMemoryLimit caps every engine container this suite starts. The
// fixtures are a few hundred rows, and an unbounded engine sizing its
// caches against the whole host makes a suite run compete with everything
// else on a developer's machine.
const engineMemoryLimit = "1g"

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine mariadb-dump and
// validate it through the probe-declared sql_runner, including the
// ANSI_QUOTES bridge for the core's SQL-standard quoted identifiers.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Build the adapter binary and put it on PATH under its protocol name.
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "probavi-adapter-mariadb")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)

	// Phase A: seed a database and take a real mariadb-dump fixture.
	fixture := filepath.Join(t.TempDir(), "orders.sql")
	makeFixture(t, ctx, provider, fixture)

	// Phase B: the drill — fresh sandbox, restore through the protocol.
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mariadb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "mariadb" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	// No options: the defaults (root, probavi) must carry the drill, and
	// the seed dumped exactly the default database name.
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mariadb_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Validate the restored data through the probe-declared sql_runner —
	// exactly how internal/checks runs checks without engine knowledge.
	// The double-quoted identifier is the point: the core emits
	// SQL-standard quoting, and the declared template must make the
	// engine accept it.
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		a = strings.ReplaceAll(a, "{{sql}}", `SELECT count(*) FROM "orders"`)
		argv = append(argv, a)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("sql_runner exec: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "500" {
		t.Fatalf("row count = %q (exit %d, stderr %s), want 500 — the restore did not carry the data",
			count, out.ExitCode, out.Stderr)
	}

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestCorruptDumpVerdict proves a broken backup yields the right verdict
// through the whole stack, not a generic failure.
func TestCorruptDumpVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-mariadb"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	corrupt := filepath.Join(t.TempDir(), "corrupt.sql")
	if err := os.WriteFile(corrupt, []byte("this is not a mariadb dump"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mariadb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mariadb_dump", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestXtraBackupEndToEnd proves the physical-restore path: a real
// XtraBackup full backup (taken on a seed server) is restored through the
// full stack into an idle sandbox, the auth-reset init file opens
// sandbox-local access, and the data comes back queryable through the
// sql_runner with a schema-qualified ANSI-quoted identifier.
func TestMariadbBackupEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)

	provider := docker.New(nil)
	hostBackup := filepath.Join(t.TempDir(), "backup")
	makeMariadbBackupFixture(t, ctx, provider, hostBackup)

	// The restore container is the plain verified image, idle: unlike the
	// sibling adapter's XtraBackup flow there is no separate tool image to
	// build — mariadb-backup ships in the official image.
	sbx, err := provider.Create(ctx, map[string]string{"image": verifiedImage(t), "command": "sleep infinity", "memory": engineMemoryLimit})
	if err != nil {
		t.Fatalf("create idle sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mariadb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mariadb_backup", Path: hostBackup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if res.Connection.Database != "mysql" || res.Connection.User != "root" {
		t.Errorf("connection = %+v, want root on the system schema", res.Connection)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v err=%v", health, err)
	}

	// Physical drills validate restored data with schema-qualified names;
	// the double-quoted form pins the ANSI_QUOTES bridge on this path too.
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		a = strings.ReplaceAll(a, "{{sql}}", `SELECT count(*) FROM "shop"."orders"`)
		argv = append(argv, a)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "500" {
		t.Fatalf("row count = %q (exit %d, stderr %s), want 500", count, out.ExitCode, out.Stderr)
	}
	if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// makeMariadbBackupFixture seeds a real server and takes an unprepared
// mariadb-backup full backup, as a production backup job would store it.
// The seed runs the official image's own entrypoint; mariadb-backup
// connects over the local socket the entrypoint's server listens on.
func makeMariadbBackupFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	seedSQL := `CREATE DATABASE shop;
CREATE TABLE shop.orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, total DECIMAL(10,2) NOT NULL);
INSERT INTO shop.orders (total)
WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 500)
SELECT ROUND(RAND()*100, 2) FROM seq;`
	mustExec(t, ctx, seed, "mariadb", "-h", "127.0.0.1", "-u", "root", "-e", seedSQL)
	mustExec(t, ctx, seed, "mariadb-backup", "--backup", "-u", "root", "--target-dir=/tmp/backup")

	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/backup", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract backup: %v: %s", err, out)
	}
}

// makeFixture seeds the default database with 500 rows and extracts a real
// mariadb-dump file to the host.
func makeFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	seedSQL := `CREATE DATABASE probavi;
USE probavi;
CREATE TABLE orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, total DECIMAL(10,2) NOT NULL);
INSERT INTO orders (total)
WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 500)
SELECT ROUND(RAND()*100, 2) FROM seq;`
	mustExec(t, ctx, seed, "mariadb", "-h", "127.0.0.1", "-u", "root", "-e", seedSQL)
	mustExec(t, ctx, seed, "mariadb-dump", "-h", "127.0.0.1", "-u", "root",
		"--result-file=/tmp/fixture.sql", "probavi")

	// The provider deliberately has no get-file verb; pulling the fixture
	// out of the seed container is test harness work, done with the CLI.
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/fixture.sql", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// awaitReady polls a TCP SELECT 1 until the seed engine serves queries.
// The first boot initializes the datadir, which takes markedly longer than
// postgres — hence the generous deadline.
func awaitReady(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		res, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv:    []string{"mariadb", "-h", "127.0.0.1", "-u", "root", "-N", "-B", "-e", "SELECT 1"},
			Timeout: 5 * time.Second,
		})
		if err == nil && res.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("seed engine never became ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func mustExec(t *testing.T, ctx context.Context, sbx *docker.Sandbox, argv ...string) {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %v: exit %d: %s", argv, res.ExitCode, res.Stderr)
	}
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}

// rootExec runs one statement through the drill engine as root.
func rootExec(t *testing.T, ctx context.Context, sbx *docker.Sandbox, stmt string) *sandbox.ExecResult {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"mariadb", "-h", "127.0.0.1", "-u", "root", "-N", "-B", "-e", stmt},
	})
	if err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
	return out
}

// rootRows runs one statement as root and returns its result rows,
// failing the test if the statement itself errors.
func rootRows(t *testing.T, ctx context.Context, sbx *docker.Sandbox, stmt string) []string {
	t.Helper()
	out := rootExec(t, ctx, sbx, stmt)
	if out.ExitCode != 0 {
		t.Fatalf("%q: exit %d: %s", stmt, out.ExitCode, out.Stderr)
	}
	var rows []string
	for _, line := range strings.Split(string(out.Stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-mariadb"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDirectorySelectionIgnoresFileTimes is the defect issue #100 records,
// measured rather than argued. A directory source used to rank candidates
// by modification time, so a stale dump copied in afterwards — cp without
// -p, an object-store download, an rsync without -t — became "the newest
// file" and was the backup the drill proved. The two dumps here hold
// different row counts, so which one was restored is a measurement, not an
// inference.
func TestDirectorySelectionIgnoresFileTimes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-mariadb"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	dir := t.TempDir()
	makeTwoGenerations(t, ctx, provider, dir)

	// The stale dump is the newest file: this is what copying it in later
	// does, and it is exactly what must no longer decide the drill.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "fresh.sql"), now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "stale.sql"), now, now); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mariadb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mariadb_dump_dir", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		a = strings.ReplaceAll(a, "{{sql}}", "SELECT count(*) FROM orders")
		argv = append(argv, a)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("sql_runner exec: %v", err)
	}
	count := strings.TrimSpace(string(out.Stdout))
	if out.ExitCode != 0 || count != strconv.Itoa(freshRowCount) {
		t.Fatalf("row count = %q (exit %d), want %d — the drill restored the stale dump the copy made look fresh",
			count, out.ExitCode, freshRowCount)
	}
}

// The two generations differ in row count so the restored one is
// identifiable, and they are taken far enough apart that the trailer each
// dump writes about itself differs — the dump trailer records whole seconds.
const (
	staleRowCount = 3
	freshRowCount = 11
)

// makeTwoGenerations writes two real dumps of the same database into dir:
// an older one, then a newer one taken after more rows were inserted.
func makeTwoGenerations(t *testing.T, ctx context.Context, provider *docker.Provider, dir string) {
	t.Helper()
	makeGenerations(t, ctx, provider, dir, "")
}

// makeGenerations writes the two generations, optionally through the
// compressor. suffix is what the stored artifacts are named beyond
// "stale.sql"/"fresh.sql"; ".gz" runs the dumps through the pipeline the
// field actually uses (`mysqldump … | gzip -c > db.sql.gz`) rather than
// compressing them afterwards on the host.
func makeGenerations(t *testing.T, ctx context.Context, provider *docker.Provider, dir, suffix string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	dump := func(name string) {
		t.Helper()
		if suffix == "" {
			mustExec(t, ctx, seed, "mariadb-dump", "-h", "127.0.0.1", "-u", "root",
				"--result-file=/tmp/"+name, "probavi")
			return
		}
		mustExec(t, ctx, seed, "sh", "-c",
			"mariadb-dump -h 127.0.0.1 -u root probavi | gzip -c > /tmp/"+name+suffix)
	}

	awaitReady(t, ctx, seed)
	mustExec(t, ctx, seed, "mariadb", "-h", "127.0.0.1", "-u", "root", "-e",
		"CREATE DATABASE probavi; USE probavi;"+
			"CREATE TABLE orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, total DECIMAL(10,2) NOT NULL);"+
			"INSERT INTO orders (total) VALUES "+values(staleRowCount)+";")
	dump("stale.sql")

	// A second of daylight between the two dumps' own trailers.
	mustExec(t, ctx, seed, "sleep", "2")

	mustExec(t, ctx, seed, "mariadb", "-h", "127.0.0.1", "-u", "root", "-e",
		"USE probavi; INSERT INTO orders (total) VALUES "+values(freshRowCount-staleRowCount)+";")
	dump("fresh.sql")

	for _, name := range []string{"stale.sql" + suffix, "fresh.sql" + suffix} {
		out, err := exec.CommandContext(ctx, "docker", "cp",
			seed.ID()+":/tmp/"+name, filepath.Join(dir, name)).CombinedOutput()
		if err != nil {
			t.Fatalf("extract %s: %v: %s", name, err, out)
		}
	}
}

// values builds n one-column rows for an INSERT.
func values(n int) string {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, "(1)")
	}
	return strings.Join(rows, ",")
}

// TestCompressedDumpEndToEnd is issue #106 measured rather than argued.
// The fixtures are produced by the pipeline the issue names —
// `mysqldump … | gzip -c > db.sql.gz` — so what is restored here is the
// artifact an operator actually retains, and the two generations hold
// different row counts so which one was restored is a measurement.
func TestCompressedDumpEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	dir := t.TempDir()
	makeGenerations(t, ctx, provider, dir, ".gz")

	t.Run("a compressed dump restores", func(t *testing.T) {
		res, count := drillRestore(t, ctx, provider, adapter.ProvisionSource{
			Kind:   "mariadb_dump",
			Path:   filepath.Join(dir, "fresh.sql.gz"),
			Params: map[string]string{"backup_timezone": "UTC"},
		})
		if count != strconv.Itoa(freshRowCount) {
			t.Errorf("row count = %q, want %d — the compressed dump did not restore", count, freshRowCount)
		}
		// The identity must describe the artifact the backup archive keeps,
		// not something derived from it.
		stored, err := os.Stat(filepath.Join(dir, "fresh.sql.gz"))
		if err != nil {
			t.Fatal(err)
		}
		if res.SourceIdentity.SizeBytes != stored.Size() {
			t.Errorf("size_bytes = %d, want the stored %d", res.SourceIdentity.SizeBytes, stored.Size())
		}
		if res.SourceIdentity.CreatedAt == nil {
			t.Error("created_at = nil — the trailer is readable through the decompressor")
		}
	})

	t.Run("a directory of compressed dumps ranks by the trailer", func(t *testing.T) {
		// The stale dump carries the newest file time, which is what copying
		// backups in produces and what must not decide the drill.
		now := time.Now()
		if err := os.Chtimes(filepath.Join(dir, "fresh.sql.gz"), now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(dir, "stale.sql.gz"), now, now); err != nil {
			t.Fatal(err)
		}
		_, count := drillRestore(t, ctx, provider,
			adapter.ProvisionSource{Kind: "mariadb_dump_dir", Path: dir})
		if count != strconv.Itoa(freshRowCount) {
			t.Errorf("row count = %q, want %d — ranking fell back to the file times",
				count, freshRowCount)
		}
	})

	t.Run("a truncated archive fails the drill", func(t *testing.T) {
		// A decompressor that dies partway leaves a prefix the client may
		// well accept; the protocol forbids reporting that as a restore (§5).
		whole, err := os.ReadFile(filepath.Join(dir, "fresh.sql.gz"))
		if err != nil {
			t.Fatal(err)
		}
		truncated := filepath.Join(t.TempDir(), "fresh.sql.gz")
		if err := os.WriteFile(truncated, whole[:len(whole)*2/3], 0o600); err != nil {
			t.Fatal(err)
		}
		sbx, runner := drillSandbox(t, ctx, provider)
		defer destroy(t, sbx)
		_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mariadb_dump", Path: truncated},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		if err == nil {
			t.Fatal("provision succeeded on a truncated archive — a partial restore was reported as a restore")
		}
		t.Logf("truncated archive verdict: %v", err)
	})

	t.Run("an archive that fails its own integrity check is refused", func(t *testing.T) {
		// The dangerous shape, and the reason the decompressor's status is
		// captured at all: every SQL statement arrives intact and the client
		// exits 0, while the archive's own checksum says these are not the
		// bytes that were stored. Nothing on the client side can see that.
		whole, err := os.ReadFile(filepath.Join(dir, "fresh.sql.gz"))
		if err != nil {
			t.Fatal(err)
		}
		whole[len(whole)-5] ^= 0xff // the gzip trailer's CRC32
		corrupt := filepath.Join(t.TempDir(), "fresh.sql.gz")
		if err := os.WriteFile(corrupt, whole, 0o600); err != nil {
			t.Fatal(err)
		}
		sbx, runner := drillSandbox(t, ctx, provider)
		defer destroy(t, sbx)
		_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mariadb_dump", Path: corrupt},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		var aerr *adapter.Error
		if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
			t.Fatalf("provision error = %v, want source_corrupt — the client loaded every statement happily", err)
		}
		t.Logf("failed-integrity verdict: %v", err)
	})
}

// drillSandbox brings up one drill sandbox and the adapter runner.
func drillSandbox(t *testing.T, ctx context.Context, provider *docker.Provider) (*docker.Sandbox, *adapter.Runner) {
	t.Helper()
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	runner, err := adapter.New("mariadb", nil, nil)
	if err != nil {
		destroy(t, sbx)
		t.Fatalf("resolve adapter: %v", err)
	}
	return sbx, runner
}

// drillRestore runs one whole drill against a fresh sandbox and returns
// the provision result together with what the restored database holds —
// the row count is read through the adapter's own declared sql_runner, so
// the answer comes back the way the core would read it.
func drillRestore(t *testing.T, ctx context.Context, provider *docker.Provider,
	source adapter.ProvisionSource) (*adapter.ProvisionResult, string) {
	t.Helper()
	sbx, runner := drillSandbox(t, ctx, provider)
	defer destroy(t, sbx)

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  source,
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		a = strings.ReplaceAll(a, "{{sql}}", "SELECT count(*) FROM orders")
		argv = append(argv, a)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("sql_runner exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("sql_runner exited %d: %s", out.ExitCode, out.Stderr)
	}
	return res, strings.TrimSpace(string(out.Stdout))
}

// TestUnfinishedDumpIsRefused is the silent partial restore, measured
// rather than argued. A backup job whose mysqldump dies part-way leaves a
// dump that is valid SQL as far as it goes, and the client loads it without
// complaint: measured against a real server, a three-table dump cut where
// mysqldump would have stopped after the first restores that one table and
// exits 0. Nothing in the pipeline reports it — only the sign-off mysqldump
// never got to write.
//
// The same file is drilled in both storage forms, because compressing it
// changes nothing about the claim: `mysqldump | gzip` whose mysqldump dies
// still leaves a whole, valid gzip member behind.
func TestUnfinishedDumpIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	buildAdapterOnPath(t, ctx)

	provider := docker.New(nil)
	dir := t.TempDir()
	makeUnfinishedFixtures(t, ctx, provider, dir)

	runner, err := adapter.New("mariadb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("the whole dump restores every table", func(t *testing.T) {
		tables := drillTables(t, ctx, provider, runner, filepath.Join(dir, "full.sql"))
		if len(tables) != 3 {
			t.Errorf("tables = %v, want all three", tables)
		}
	})

	for _, name := range []string{"partial.sql", "partial.sql.gz"} {
		t.Run(name+" is refused", func(t *testing.T) {
			_, err := drillProvision(t, ctx, provider, runner, filepath.Join(dir, name))
			var aerr *adapter.Error
			if err == nil || !errors.As(err, &aerr) {
				t.Fatalf("provision = %v, want a refusal — the drill would have proved a third of the backup", err)
			}
			if aerr.Code != "source_corrupt" || !strings.Contains(aerr.Message, "not a complete dump") {
				t.Errorf("error = %s/%q, want source_corrupt naming the incomplete dump", aerr.Code, aerr.Message)
			}
		})
	}

	// A comment-free dump carries no sign-off to check, so it is exempt
	// rather than failed: refusing it would reject a backup that is fine.
	t.Run("a comment-free dump is still restorable", func(t *testing.T) {
		tables := drillTables(t, ctx, provider, runner, filepath.Join(dir, "compact.sql"))
		if len(tables) != 3 {
			t.Errorf("tables = %v, want all three", tables)
		}
	})
}

// drillProvision restores one artifact in a sandbox of its own.
func drillProvision(t *testing.T, ctx context.Context, provider *docker.Provider,
	runner *adapter.Runner, fixture string) (*adapter.ProvisionResult, error) {
	t.Helper()
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { destroy(t, sbx) })
	return runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mariadb_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
}

// drillTables restores one artifact and reports the tables that arrived,
// which is what makes "the restore was partial" a measurement.
func drillTables(t *testing.T, ctx context.Context, provider *docker.Provider,
	runner *adapter.Runner, fixture string) []string {
	t.Helper()
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mariadb_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision %s: %v", filepath.Base(fixture), err)
	}
	return rootRows(t, ctx, sbx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = '"+res.Connection.Database+"'")
}

// makeUnfinishedFixtures seeds three tables and writes four artifacts out
// of them: the whole dump, the same dump cut where mysqldump would have
// died after the first table (plain and compressed), and a --compact dump
// that carries no sign-off at all.
func makeUnfinishedFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	mustExec(t, ctx, seed, "mariadb", "-h", "127.0.0.1", "-u", "root", "-e", `CREATE DATABASE probavi;
USE probavi;
CREATE TABLE customers (id INT PRIMARY KEY);
INSERT INTO customers VALUES (1),(2),(3);
CREATE TABLE invoices (id INT PRIMARY KEY);
INSERT INTO invoices VALUES (1),(2);
CREATE TABLE orders (id INT PRIMARY KEY);
INSERT INTO orders VALUES (1);`)
	mustExec(t, ctx, seed, "sh", "-c", `set -e
cd /tmp
mariadb-dump -h 127.0.0.1 -u root probavi > full.sql
mariadb-dump -h 127.0.0.1 -u root --compact probavi > compact.sql
cut=$(grep -n "Table structure for table .invoices." full.sql | head -1 | cut -d: -f1)
head -n $(( cut - 3 )) full.sql > partial.sql
gzip -c partial.sql > partial.sql.gz`)

	for _, name := range []string{"full.sql", "compact.sql", "partial.sql", "partial.sql.gz"} {
		if out, err := exec.CommandContext(ctx, "docker", "cp",
			seed.ID()+":/tmp/"+name, filepath.Join(dest, name)).CombinedOutput(); err != nil {
			t.Fatalf("extract %s: %v: %s", name, err, out)
		}
	}
}

// TestRestoredEventsDoNotRunInTheDrill is this adapter's half of issue
// #166. A dump taken with --events carries the backup's own scheduled
// jobs, and an operator's purge event deletes rows in the drill exactly
// as it does in production — measured, ten rows down to two five seconds
// after the restore, with the event arriving ENABLED.
//
// MariaDB ships the scheduler off, so this drill turns it on through
// the sandbox parameters — the configuration an operator can produce
// today, and the one MySQL made its default in 8.0.
func TestRestoredEventsDoNotRunInTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "events.sql")
	makeEventFixture(t, ctx, provider, fixture)

	params := sandboxParams(t)
	// MariaDB ships the scheduler off, so the exposure is one sandbox
	// parameter away rather than the default — which is exactly how the
	// drill must not depend on it.
	params["command"] = "--event-scheduler=ON"
	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)
	awaitReady(t, ctx, sbx)

	runner, err := adapter.New("mariadb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mariadb_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "probavi"},
	}, sbx); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The event arrived and is enabled: what the backup recorded is what a
	// check reading information_schema.events sees. Only its execution is
	// suspended.
	if got := rootRows(t, ctx, sbx,
		"SELECT status FROM information_schema.events WHERE event_schema='probavi'"); len(got) != 1 ||
		!strings.EqualFold(got[0], "ENABLED") {
		t.Errorf("restored event status = %v, want the ENABLED the backup carried", got)
	}
	if got := rootRows(t, ctx, sbx, "SELECT @@event_scheduler"); len(got) != 1 ||
		(!strings.EqualFold(got[0], "OFF") && !strings.EqualFold(got[0], "DISABLED")) {
		t.Errorf("scheduler = %v, want it suspended for the drill", got)
	}

	// Long enough that an event scheduled every second would have run many
	// times over: measured, an unpinned sandbox is down to two rows within
	// five seconds of the restore.
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while watching the restored rows")
	case <-time.After(15 * time.Second):
	}
	if got := rootRows(t, ctx, sbx, "SELECT COUNT(*) FROM probavi.orders"); len(got) != 1 || got[0] != "10" {
		t.Errorf("rows = %v, want all 10 — the drill ran the backup's purge event", got)
	}
}

// makeEventFixture dumps a database whose rows an event would delete,
// with --events so the artifact carries the job the way an operator's
// backup does.
func makeEventFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	// The seed's own scheduler is suspended while the fixture is built:
	// on the MySQL side it is on by default and would purge the rows
	// before the dump ran, leaving an artifact that proves nothing. What
	// this produces is an ordinary backup taken between two event runs.
	seedSQL := `SET GLOBAL event_scheduler = OFF;
CREATE DATABASE probavi;
USE probavi;
CREATE TABLE orders (id INT PRIMARY KEY, created DATETIME NOT NULL);
INSERT INTO orders SELECT n, NOW() - INTERVAL n DAY FROM
  (SELECT 1 n UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5
   UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9 UNION SELECT 10) t;
CREATE EVENT orders_purge ON SCHEDULE EVERY 1 SECOND
  DO DELETE FROM orders WHERE created < NOW() - INTERVAL 3 DAY;`
	mustExec(t, ctx, seed, "mariadb", "-h", "127.0.0.1", "-u", "root", "-e", seedSQL)
	mustExec(t, ctx, seed, "mariadb-dump", "-h", "127.0.0.1", "-u", "root", "--events",
		"--result-file=/tmp/events.sql", "probavi")

	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/events.sql", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}
