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
		"image": verifiedImage(t), "env.MYSQL_ALLOW_EMPTY_PASSWORD": "yes",
		"memory": engineMemoryLimit,
	}
}

// engineMemoryLimit caps every engine container this suite starts. The
// fixtures are a few hundred rows, and an unbounded engine sizing its
// caches against the whole host makes a suite run compete with everything
// else on a developer's machine.
const engineMemoryLimit = "1g"

// TestEndToEndRestoreDrill proves the second engine through the unchanged
// core: the docker provider, the core-side protocol client, and this
// adapter — as separate processes — restore a genuine mysqldump and
// validate it through the probe-declared sql_runner, including the
// ANSI_QUOTES bridge for the core's SQL-standard quoted identifiers.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Build the adapter binary and put it on PATH under its protocol name.
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "probavi-adapter-mysql")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)

	// Phase A: seed a database and take a real mysqldump fixture.
	fixture := filepath.Join(t.TempDir(), "orders.sql")
	makeFixture(t, ctx, provider, fixture)

	// Phase B: the drill — fresh sandbox, restore through the protocol.
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mysql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "mysql" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	// No options: the defaults (root, probavi) must carry the drill, and
	// the seed dumped exactly the default database name.
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mysqldump", Path: fixture},
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
		filepath.Join(binDir, "probavi-adapter-mysql"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	corrupt := filepath.Join(t.TempDir(), "corrupt.sql")
	if err := os.WriteFile(corrupt, []byte("this is not a mysql dump"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mysql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mysqldump", Path: corrupt},
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
func TestXtraBackupEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	image := buildXtraBackupImage(t, ctx)

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-mysql"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hostBackup := filepath.Join(t.TempDir(), "backup")
	makeXtraBackupFixture(t, ctx, image, hostBackup)

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit})
	if err != nil {
		t.Fatalf("create idle sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mysql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "xtrabackup", Path: hostBackup},
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

// buildXtraBackupImage builds (once, cached afterwards) a mysql image with
// Percona XtraBackup installed — the documented requirement for the
// xtrabackup source kind. The Debian variant is used because the Percona
// apt repository makes the install reproducible.
func buildXtraBackupImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	const tag = "probavi-it-xtrabackup:8.0"
	dir := t.TempDir()
	dockerfile := `FROM mysql:8.0-debian
RUN apt-get update && apt-get install -y --no-install-recommends wget curl gnupg2 lsb-release ca-certificates \
 && wget -q https://repo.percona.com/apt/percona-release_latest.generic_all.deb \
 && dpkg -i percona-release_latest.generic_all.deb \
 && percona-release enable-only pxb-80 release \
 && apt-get update && apt-get install -y --no-install-recommends percona-xtrabackup-80 \
 && rm -rf /var/lib/apt/lists/* percona-release_latest.generic_all.deb
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build test image: %v: %s", err, out)
	}
	return tag
}

// makeXtraBackupFixture seeds a real server in an idle container, takes a
// full XtraBackup backup, and copies it to the host — unprepared, as a
// production backup job would store it.
func makeXtraBackupFixture(t *testing.T, ctx context.Context, image, dest string) {
	t.Helper()
	// The owner-pid label carries the REAL test process: a concurrent
	// sweep must spare the live seed; if this process dies, the next
	// sweep reaps the leftover.
	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--label", docker.LabelSandbox+"=1", "--label", "com.probavi.pid="+strconv.Itoa(os.Getpid()),
		"--network", "none", image, "sleep", "infinity").Output()
	if err != nil {
		t.Fatalf("start seed container: %v", err)
	}
	id := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", "-v", id).Run() //nolint:errcheck // best-effort cleanup

	seedScript := `set -e
chown -R mysql:mysql /var/lib/mysql /var/run/mysqld
gosu mysql mysqld --initialize-insecure --datadir=/var/lib/mysql
gosu mysql mysqld --daemonize --pid-file=/tmp/seed.pid --log-error=/tmp/seed.err
mysql --socket=/var/run/mysqld/mysqld.sock -u root -e "CREATE DATABASE shop; CREATE TABLE shop.orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, total DECIMAL(10,2) NOT NULL); INSERT INTO shop.orders (total) WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 500) SELECT ROUND(RAND()*100,2) FROM seq;"
xtrabackup --backup --user=root --socket=/var/run/mysqld/mysqld.sock --target-dir=/tmp/backup
mysqladmin --socket=/var/run/mysqld/mysqld.sock -u root shutdown`
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "sh", "-c", seedScript).CombinedOutput(); err != nil {
		t.Fatalf("seed xtrabackup fixture: %v: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", id+":/tmp/backup", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract backup: %v: %s", err, out)
	}
}

// makeFixture seeds the default database with 500 rows and extracts a real
// mysqldump file to the host.
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
	mustExec(t, ctx, seed, "mysql", "-h", "127.0.0.1", "-u", "root", "-e", seedSQL)
	mustExec(t, ctx, seed, "mysqldump", "-h", "127.0.0.1", "-u", "root",
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
			Argv:    []string{"mysql", "-h", "127.0.0.1", "-u", "root", "-N", "-B", "-e", "SELECT 1"},
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

const (
	// usersFixturePassword seeds the application account the users-drill
	// fixture exports; it exists only inside the test sandboxes. The
	// exported script carries its password *hash*, so a successful
	// authentication with this plaintext proves the hash round-tripped
	// through the drill.
	usersFixturePassword = "AppSeed!OnlyInSandbox1"
)

// TestUsersDrillEndToEnd reproduces issue #89 and proves the fix, and each
// half is worthless without the others. MySQL accounts and grants live in
// the mysql system schema, never in a single-database dump, so a plain
// mysqldump drill passes while the application account cannot log in and
// every SQL SECURITY DEFINER object fails at invocation. The
// mysqldump_with_users kind replays an exported accounts-and-grants script
// first and gates on the restored principal chain afterwards — including
// the measured trap that grants are database-scoped, so the drill must
// restore under the database name the script grants on.
func TestUsersDrillEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	fixtureDir := makeAccountedFixture(t, ctx, provider)

	runner, err := adapter.New("mysql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("kind mysqldump still passes and leaves the gap", func(t *testing.T) {
		assertDumpLeavesGap(t, ctx, provider, runner, fixtureDir)
	})
	t.Run("kind mysqldump_with_users restores the principal chain", func(t *testing.T) {
		assertWithUsersRestoresChain(t, ctx, provider, runner, fixtureDir)
	})
	t.Run("under the wrong database name the reachability gate refuses", func(t *testing.T) {
		assertWrongNameRefused(t, ctx, provider, runner, fixtureDir)
	})
	t.Run("an incomplete users script fails the drill", func(t *testing.T) {
		assertIncompleteUsersScriptFails(t, ctx, provider, runner, fixtureDir)
	})
}

// assertDumpLeavesGap is the reproduction half: the plain mysqldump drill
// passes while the application account does not exist, the DEFINER view
// fails at invocation, and the database default collation silently changed
// — the premises of #89.
func assertDumpLeavesGap(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, fixtureDir string) {
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mysqldump", Path: filepath.Join(fixtureDir, "shop.sql")},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if rows := rootRows(t, ctx, sbx, "SELECT user FROM mysql.user WHERE user = 'app'"); len(rows) != 0 {
		t.Errorf("app account = %v, want absent — the premise of #89", rows)
	}
	out := rootExec(t, ctx, sbx, "SELECT * FROM "+res.Connection.Database+".v_orders")
	if out.ExitCode == 0 || !strings.Contains(string(out.Stderr), "definer") {
		t.Errorf("definer view select = exit %d (%s), want the ERROR 1449 definer failure",
			out.ExitCode, out.Stderr)
	}
	collation := rootRows(t, ctx, sbx,
		"SELECT default_collation_name FROM information_schema.schemata WHERE schema_name = '"+res.Connection.Database+"'")
	if len(collation) != 1 || collation[0] == "utf8mb4_bin" {
		t.Errorf("restored collation = %v — the source was utf8mb4_bin, the loss is the premise", collation)
	}
}

// assertWithUsersRestoresChain is the fix half, with the strongest proof
// there is: the restored account authenticates with its original password
// (the hash round-tripped), reads the granted table, the DEFINER view and
// procedure work, and the pinned charset options preserve the source
// collation.
func assertWithUsersRestoresChain(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, fixtureDir string) {
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind:   "mysqldump_with_users",
			Path:   fixtureDir,
			Params: map[string]string{"users": "users.sql", "dump": "shop.sql"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "shop", "charset": "utf8mb4", "collation": "utf8mb4_bin"},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}

	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", "app", "-p" + usersFixturePassword,
			"-D", "shop", "-N", "-B", "-e", "SELECT count(*) FROM orders"},
	})
	if err != nil {
		t.Fatalf("app login exec: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "2" {
		t.Errorf("app count = %q (exit %d, stderr %s), want 2 rows through the restored grant",
			count, out.ExitCode, out.Stderr)
	}

	if rows := rootRows(t, ctx, sbx, "SELECT * FROM shop.v_orders"); len(rows) != 1 || rows[0] != "2" {
		t.Errorf("definer view = %v, want the count through the restored definer", rows)
	}
	proc := rootExec(t, ctx, sbx, "CALL shop.p_count()")
	if proc.ExitCode != 0 {
		t.Errorf("definer procedure = exit %d (%s), want success through the restored EXECUTE grant",
			proc.ExitCode, proc.Stderr)
	}
	collation := rootRows(t, ctx, sbx,
		"SELECT default_collation_name FROM information_schema.schemata WHERE schema_name = 'shop'")
	if len(collation) != 1 || collation[0] != "utf8mb4_bin" {
		t.Errorf("restored collation = %v, want the pinned utf8mb4_bin", collation)
	}
}

// assertWrongNameRefused proves the measured trap is caught loudly: grants
// are database-scoped, so restored under the default name instead of the
// name the script grants on, no account can reach the target — and the
// gate's message says how to fix it.
func assertWrongNameRefused(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, fixtureDir string) {
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind:   "mysqldump_with_users",
			Path:   fixtureDir,
			Params: map[string]string{"users": "users.sql", "dump": "shop.sql"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed from the reachability gate", err)
	}
	if !strings.Contains(aerr.Message, "no restored account can reach database") ||
		!strings.Contains(aerr.Message, "options.database") {
		t.Errorf("message = %q, want the teaching reachability verdict", aerr.Message)
	}
}

// assertIncompleteUsersScriptFails proves the definer gate bites: a script
// that loads cleanly but restores no accounts must fail the drill, or an
// incomplete export would reintroduce the defect one level down.
func assertIncompleteUsersScriptFails(t *testing.T, ctx context.Context, provider *docker.Provider, runner *adapter.Runner, fixtureDir string) {
	incomplete := t.TempDir()
	data, err := os.ReadFile(filepath.Join(fixtureDir, "shop.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomplete, "shop.sql"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomplete, "users.sql"), []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind:   "mysqldump_with_users",
			Path:   incomplete,
			Params: map[string]string{"users": "users.sql", "dump": "shop.sql"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "shop"},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed from the definer gate", err)
	}
	if !strings.Contains(aerr.Message, "definers that do not exist") {
		t.Errorf("message = %q, want the orphaned-definer verdict", aerr.Message)
	}
}

// rootExec runs one statement through the drill engine as root.
func rootExec(t *testing.T, ctx context.Context, sbx *docker.Sandbox, stmt string) *sandbox.ExecResult {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", "root", "-N", "-B", "-e", stmt},
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
		filepath.Join(binDir, "probavi-adapter-mysql"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeAccountedFixture seeds a throwaway engine with an application
// account, a granted database (non-default collation), and DEFINER
// objects; it extracts a real mysqldump (--routines, so the definer gate
// has a routine in scope) plus an accounts-and-grants script exported the
// way run-books do it — SHOW CREATE USER with the password hash printed as
// hex, and SHOW GRANTS — into one source directory.
func makeAccountedFixture(t *testing.T, ctx context.Context, provider *docker.Provider) string {
	t.Helper()
	dir := t.TempDir()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)

	for _, stmt := range []string{
		"CREATE USER 'app'@'%' IDENTIFIED BY '" + usersFixturePassword + "'",
		"CREATE DATABASE shop CHARACTER SET utf8mb4 COLLATE utf8mb4_bin",
		"CREATE TABLE shop.orders (id INT PRIMARY KEY, total DECIMAL(10,2))",
		"INSERT INTO shop.orders VALUES (1, 10.50), (2, 20.00)",
		"GRANT SELECT, EXECUTE ON shop.* TO 'app'@'%'",
		"CREATE DEFINER='app'@'%' SQL SECURITY DEFINER VIEW shop.v_orders AS SELECT count(*) AS n FROM shop.orders",
		"CREATE DEFINER='app'@'%' PROCEDURE shop.p_count() SQL SECURITY DEFINER SELECT count(*) FROM shop.orders",
	} {
		mustExec(t, ctx, seed, "mysql", "-h", "127.0.0.1", "-u", "root", "-e", stmt)
	}
	mustExec(t, ctx, seed, "mysqldump", "-h", "127.0.0.1", "-u", "root", "--routines",
		"--result-file=/tmp/shop.sql", "shop")

	// The password hash prints as hex so the script survives any encoding;
	// this is what operators' export tooling does too.
	createUser, err := seed.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", "root",
			"--init-command=SET SESSION print_identified_with_as_hex = ON",
			"-N", "-B", "-e", "SHOW CREATE USER 'app'@'%'"},
	})
	if err != nil || createUser.ExitCode != 0 {
		t.Fatalf("export create user: %v (exit %d, stderr %s)", err, createUser.ExitCode, createUser.Stderr)
	}
	grants, err := seed.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", "root", "-N", "-B",
			"-e", "SHOW GRANTS FOR 'app'@'%'"},
	})
	if err != nil || grants.ExitCode != 0 {
		t.Fatalf("export grants: %v (exit %d, stderr %s)", err, grants.ExitCode, grants.Stderr)
	}
	var script strings.Builder
	for _, line := range strings.Split(string(createUser.Stdout)+string(grants.Stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			script.WriteString(line + ";\n")
		}
	}
	if !strings.Contains(script.String(), "AS 0x") || !strings.Contains(script.String(), "GRANT SELECT") {
		t.Fatalf("exported script lacks hash or grants: %s", script.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "users.sql"), []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/shop.sql",
		filepath.Join(dir, "shop.sql")).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return dir
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
		filepath.Join(binDir, "probavi-adapter-mysql"), ".").CombinedOutput(); err != nil {
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

	runner, err := adapter.New("mysql", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mysqldump_dir", Path: dir},
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
// dump writes about itself differs — mysqldump records whole seconds.
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
			mustExec(t, ctx, seed, "mysqldump", "-h", "127.0.0.1", "-u", "root",
				"--result-file=/tmp/"+name, "probavi")
			return
		}
		mustExec(t, ctx, seed, "sh", "-c",
			"mysqldump -h 127.0.0.1 -u root probavi | gzip -c > /tmp/"+name+suffix)
	}

	awaitReady(t, ctx, seed)
	mustExec(t, ctx, seed, "mysql", "-h", "127.0.0.1", "-u", "root", "-e",
		"CREATE DATABASE probavi; USE probavi;"+
			"CREATE TABLE orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, total DECIMAL(10,2) NOT NULL);"+
			"INSERT INTO orders (total) VALUES "+values(staleRowCount)+";")
	dump("stale.sql")

	// A second of daylight between the two dumps' own trailers.
	mustExec(t, ctx, seed, "sleep", "2")

	mustExec(t, ctx, seed, "mysql", "-h", "127.0.0.1", "-u", "root", "-e",
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
			Kind:   "mysqldump",
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
			adapter.ProvisionSource{Kind: "mysqldump_dir", Path: dir})
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
			Source:  adapter.ProvisionSource{Kind: "mysqldump", Path: truncated},
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
			Source:  adapter.ProvisionSource{Kind: "mysqldump", Path: corrupt},
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
	runner, err := adapter.New("mysql", nil, nil)
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

	runner, err := adapter.New("mysql", nil, nil)
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
		Source:  adapter.ProvisionSource{Kind: "mysqldump", Path: fixture},
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
		Source:  adapter.ProvisionSource{Kind: "mysqldump", Path: fixture},
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
	mustExec(t, ctx, seed, "mysql", "-h", "127.0.0.1", "-u", "root", "-e", `CREATE DATABASE probavi;
USE probavi;
CREATE TABLE customers (id INT PRIMARY KEY);
INSERT INTO customers VALUES (1),(2),(3);
CREATE TABLE invoices (id INT PRIMARY KEY);
INSERT INTO invoices VALUES (1),(2);
CREATE TABLE orders (id INT PRIMARY KEY);
INSERT INTO orders VALUES (1);`)
	mustExec(t, ctx, seed, "sh", "-c", `set -e
cd /tmp
mysqldump -h 127.0.0.1 -u root probavi > full.sql
mysqldump -h 127.0.0.1 -u root --compact probavi > compact.sql
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
