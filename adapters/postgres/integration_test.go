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

// engineMemoryLimit caps every engine container this suite starts. The
// fixtures are a few rows each, and an unbounded engine sizing its caches
// against the whole host makes a suite run compete with everything else on
// a developer's machine.
const engineMemoryLimit = "1g"

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

// TestEndToEndRestoreDrill is the first real vertical slice: the docker
// provider, the core-side protocol client, and this adapter — as separate
// processes — prove a genuine pg_dump restorable, end to end.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Build the adapter binary and put it on PATH under its protocol name.
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "probavi-adapter-postgres")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}

	// Phase A: seed a database and take a real pg_dump fixture.
	fixture := filepath.Join(t.TempDir(), "orders.dump")
	makeFixture(t, ctx, provider, params, fixture)

	// Phase B: the drill — fresh sandbox, restore through the protocol.
	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "postgres" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "pgdump", Path: fixture},
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
	// exactly how internal/checks will run checks without engine knowledge.
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
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "1000" {
		t.Fatalf("row count = %q (exit %d), want 1000 — the restore did not carry the data", count, out.ExitCode)
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	corrupt := filepath.Join(t.TempDir(), "corrupt.dump")
	if err := os.WriteFile(corrupt, []byte("this is not a pg_dump archive"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, map[string]string{
		"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "pgdump", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestGlobalsDrillEndToEnd is the reproduction of the gap the
// pgdump_with_globals kind closes, driven end to end against real Docker.
//
// The setup is the common one: per-database `pg_dump -Fc`, cluster
// objects taken separately with `pg_dumpall --globals-only`. A logical
// recovery runs the globals first, then the dumps — and a drill that can
// only do the second half proves less than the recovery it stands for.
// `pg_restore --no-owner` drops OWNER TO but never GRANT, so the restore
// dies on the first grant naming a role that was never created.
//
// The test asserts both halves, because either alone is worthless: the
// plain pgdump kind must still FAIL on this backup (the drill was right,
// the backup really is incomplete on its own), and the with-globals kind
// must PASS with the grant present and pointing at the restored role.
func TestGlobalsDrillEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}

	fixtureDir := t.TempDir()
	dump := filepath.Join(fixtureDir, "orders.dump")
	globals := filepath.Join(fixtureDir, "globals.sql")
	makeGrantedFixture(t, ctx, provider, params, dump, globals)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("without the globals the restore fails", func(t *testing.T) {
		sbx, err := provider.Create(ctx, params)
		if err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		defer destroy(t, sbx)

		_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "pgdump", Path: dump},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		var aerr *adapter.Error
		if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
			t.Fatalf("provision error = %v, want restore_failed — the dump's grants name a "+
				"role no dump carries", err)
		}
	})

	t.Run("with the globals the restore passes", func(t *testing.T) {
		sbx, err := provider.Create(ctx, params)
		if err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		defer destroy(t, sbx)

		res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source: adapter.ProvisionSource{
				Kind: "pgdump_with_globals", Path: fixtureDir,
				Params: map[string]string{
					"globals": filepath.Base(globals), "dump": filepath.Base(dump),
				},
			},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if res.Timings.RestoreSeconds <= 0 {
			t.Errorf("timings = %+v, want the globals load and the restore measured", res.Timings)
		}

		health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
		if err != nil || !health.Healthy {
			t.Fatalf("healthcheck = %+v err=%v", health, err)
		}

		// The grant is the proof: it exists only if the role it names was
		// created by the globals load before pg_restore replayed it.
		out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
			"psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres", "-tA", "-c",
			"SELECT grantee FROM information_schema.role_table_grants " +
				"WHERE table_name = 'orders' AND grantee = 'app_ro'"}})
		if err != nil {
			t.Fatalf("grant query: %v", err)
		}
		if got := strings.TrimSpace(string(out.Stdout)); got != "app_ro" {
			t.Fatalf("grantee = %q (exit %d, stderr %s), want app_ro — the cluster role did not "+
				"survive the restore", got, out.ExitCode, out.Stderr)
		}
		if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
			t.Fatalf("teardown: %v", err)
		}
	})
}

// makeGrantedFixture seeds a cluster whose dump cannot stand alone: a
// login role, a password on it (so the globals carry a real verifier), and
// a table grant that references the role. It writes the two artifacts a
// logical recovery needs — the database dump and the cluster globals.
func makeGrantedFixture(t *testing.T, ctx context.Context, provider *docker.Provider, params map[string]string, dump, globals string) {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	dropPreinstalledExtensions(t, ctx, seed)
	seedSQL := `CREATE ROLE app_ro NOLOGIN;
CREATE ROLE app_rw LOGIN PASSWORD 'seed-only-never-leaves-the-sandbox';
CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2) NOT NULL);
INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,100);
GRANT SELECT ON orders TO app_ro;`
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", seedSQL)
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/orders.dump", "postgres")
	mustExec(t, ctx, seed, "sh", "-c",
		"pg_dumpall -h 127.0.0.1 -U postgres --globals-only > /tmp/globals.sql")

	for src, dest := range map[string]string{"/tmp/orders.dump": dump, "/tmp/globals.sql": globals} {
		if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":"+src, dest).CombinedOutput(); err != nil {
			t.Fatalf("extract %s: %v: %s", src, err, out)
		}
	}
}

// TestPgBackRestEndToEnd proves the physical-restore path: a real
// pgBackRest repository (stanza-create + full backup on a seed cluster) is
// restored through the full stack into an idle sandbox, recovery replays
// the WAL archive, and the data comes back queryable.
func TestPgBackRestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	image := buildPgBackRestImage(t, ctx)

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hostRepo := filepath.Join(t.TempDir(), "repo")
	makeBackRestRepo(t, ctx, image, hostRepo)

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit})
	if err != nil {
		t.Fatalf("create idle sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind: "pgbackrest", Path: hostRepo, Params: map[string]string{"stanza": "demo"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v err=%v", health, err)
	}
	// 700 = 500 from the base backup + 200 replayed from the WAL archive:
	// end-of-WAL recovery must include the post-backup batch.
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres", "-tA", "-c",
		"SELECT count(*) FROM orders"}})
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "700" {
		t.Fatalf("row count = %q (exit %d, stderr %s), want 700", count, out.ExitCode, out.Stderr)
	}
	if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// TestPgBackRestPITREndToEnd proves point-in-time recovery through the full
// stack: the drill demands the instant captured between the two seed
// batches, so the restored database must contain the first batch only —
// even though the second batch's WAL sits in the archive — and must come up
// promoted (writable), not paused in recovery.
func TestPgBackRestPITREndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	image := buildPgBackRestImage(t, ctx)

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hostRepo := filepath.Join(t.TempDir(), "repo")
	target := makeBackRestRepo(t, ctx, image, hostRepo)

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit})
	if err != nil {
		t.Fatalf("create idle sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind: "pgbackrest", Path: hostRepo, Params: map[string]string{"stanza": "demo"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		PITR:    &adapter.PITR{TargetTime: target},
	}, sbx)
	if err != nil {
		t.Fatalf("provision with pitr target %q: %v", target, err)
	}

	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres", "-tA", "-c",
		"SELECT count(*) FROM orders"}})
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "500" {
		t.Fatalf("row count = %q (exit %d, stderr %s), want 500 — recovery must stop at %s, before the second batch",
			count, out.ExitCode, out.Stderr, target)
	}

	// Writable proves --target-action=promote took effect and the adapter
	// waited recovery out; a paused standby would fail this INSERT.
	out, err = sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c",
		"INSERT INTO orders (total) VALUES (1.00)"}})
	if err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("restored instance is not writable (exit %d, stderr %s) — recovery did not promote", out.ExitCode, out.Stderr)
	}

	if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// buildPgBackRestImage builds (once, cached afterwards) a postgres image
// with pgbackrest installed — the documented requirement for the
// pgbackrest source kind.
// dropPreinstalledExtensions strips what a variant image pre-creates in
// the default database: the timescale image installs its extension into
// `postgres` at first boot, so without this a "plain" fixture dumped
// there would create the extension too — and the timescale fence would
// rightly refuse it. A plain fixture must state only what its test
// claims; a plain image makes this a no-op.
func dropPreinstalledExtensions(t *testing.T, ctx context.Context, seed *docker.Sandbox) {
	t.Helper()
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres",
		"-v", "ON_ERROR_STOP=1", "-c", "DROP EXTENSION IF EXISTS timescaledb")
}

func buildPgBackRestImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	// The tool image installs pgbackrest with apt; an image without it
	// (the timescale variant is Alpine) cannot host this build, and the
	// variant's claim is the framed logical restore — the physical flow
	// keeps its coverage from the plain postgres matrix jobs.
	if _, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		verifiedImage(t), "sh", "-c", "command -v apt-get").CombinedOutput(); err != nil {
		t.Skipf("image %s cannot host the pgbackrest tool build (no apt-get); "+
			"the physical flow is exercised by the plain postgres matrix jobs", verifiedImage(t))
	}
	const tag = "probavi-it-pgbackrest:16"
	dir := t.TempDir()
	dockerfile := "FROM " + verifiedImage(t) + "\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends pgbackrest && rm -rf /var/lib/apt/lists/*\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build test image: %v: %s", err, out)
	}
	return tag
}

// makeBackRestRepo seeds a real cluster in an idle container, configures
// WAL archiving into a filesystem repo, takes a full backup of the first
// 500 orders, captures a pitr target instant, commits 200 more orders whose
// WAL lands in the archive only, and copies the repo to the host. It
// returns the captured target (RFC 3339): recovery to it must see exactly
// 500 rows; recovery to end of WAL must see 700.
func makeBackRestRepo(t *testing.T, ctx context.Context, image, dest string) string {
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

	// The sleeps bracket the captured instant so the two batches' commit
	// timestamps land strictly on opposite sides of it.
	seedScript := `set -e
mkdir -p /tmp/repo /etc/pgbackrest
printf '[global]\nrepo1-path=/tmp/repo\n\n[demo]\npg1-path=/var/lib/postgresql/data\n' > /etc/pgbackrest/pgbackrest.conf
chown -R postgres:postgres /tmp/repo /etc/pgbackrest /var/lib/postgresql/data
gosu postgres initdb -D /var/lib/postgresql/data
printf "archive_mode=on\narchive_command='pgbackrest --stanza=demo archive-push %%p'\n" >> /var/lib/postgresql/data/postgresql.conf
gosu postgres pg_ctl -D /var/lib/postgresql/data -w -l /tmp/pg.log start
gosu postgres psql -v ON_ERROR_STOP=1 -c "CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2)); INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,500);"
gosu postgres pgbackrest --stanza=demo stanza-create
gosu postgres pgbackrest --stanza=demo --type=full backup
sleep 1
gosu postgres psql -tA -c "SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')" > /tmp/pitr-target
sleep 1
gosu postgres psql -v ON_ERROR_STOP=1 -c "INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,200);"
gosu postgres psql -v ON_ERROR_STOP=1 -c "SELECT pg_switch_wal();" > /dev/null
gosu postgres pg_ctl -D /var/lib/postgresql/data -w stop`
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "sh", "-c", seedScript).CombinedOutput(); err != nil {
		t.Fatalf("seed pgbackrest repo: %v: %s", err, out)
	}
	target, err := exec.CommandContext(ctx, "docker", "exec", id, "cat", "/tmp/pitr-target").Output()
	if err != nil {
		t.Fatalf("read pitr target: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", id+":/tmp/repo", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract repo: %v: %s", err, out)
	}
	return strings.TrimSpace(string(target))
}

func makeFixture(t *testing.T, ctx context.Context, provider *docker.Provider, params map[string]string, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	dropPreinstalledExtensions(t, ctx, seed)
	seedSQL := `CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2) NOT NULL);
INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,1000);`
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", seedSQL)
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/fixture.dump", "postgres")

	// The provider deliberately has no get-file verb; pulling the fixture
	// out of the seed container is test harness work, done with the CLI.
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/fixture.dump", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

func awaitReady(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		res, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-q"}, Timeout: 5 * time.Second,
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

// TestDirectorySelectionIgnoresFileTimes is the defect issue #100 records,
// measured rather than argued. A directory source used to rank candidates
// by modification time, so a stale dump copied in afterwards — cp without
// -p, an object-store download, an rsync without -t — became "the newest
// file" and was the backup the drill proved. The two dumps here hold
// different row counts, so which one was restored is a measurement, not an
// inference.
func TestDirectorySelectionIgnoresFileTimes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{
		"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust", "memory": engineMemoryLimit,
	}

	dir := t.TempDir()
	makeTwoGenerations(t, ctx, provider, params, dir)

	// The stale dump is the newest file: this is what copying it in later
	// does, and it is exactly what must no longer decide the drill.
	stale, fresh := filepath.Join(dir, "stale.dump"), filepath.Join(dir, "fresh.dump")
	now := time.Now()
	if err := os.Chtimes(fresh, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stale, now, now); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "pgdump_dir", Path: dir},
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
// identifiable, and they are taken far enough apart that the timestamp
// each archive records about itself differs — pg_dump's header keeps
// whole seconds.
const (
	staleRowCount = 3
	freshRowCount = 11
)

// makeTwoGenerations writes two real dumps of the same database into dir:
// an older one, then a newer one taken after more rows were inserted.
func makeTwoGenerations(t *testing.T, ctx context.Context, provider *docker.Provider,
	params map[string]string, dir string) {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	dropPreinstalledExtensions(t, ctx, seed)
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c",
		"CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2) NOT NULL);"+
			"INSERT INTO orders (total) SELECT 1 FROM generate_series(1,"+strconv.Itoa(staleRowCount)+");")
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/stale.dump", "postgres")

	// A second of daylight between the two archives' own clocks.
	mustExec(t, ctx, seed, "sleep", "2")

	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c",
		"INSERT INTO orders (total) SELECT 1 FROM generate_series(1,"+strconv.Itoa(freshRowCount-staleRowCount)+");")
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/fresh.dump", "postgres")

	for _, name := range []string{"stale.dump", "fresh.dump"} {
		out, err := exec.CommandContext(ctx, "docker", "cp",
			seed.ID()+":/tmp/"+name, filepath.Join(dir, name)).CombinedOutput()
		if err != nil {
			t.Fatalf("extract %s: %v: %s", name, err, out)
		}
	}
}

// storedForms are the shapes a pg_dump artifact reaches a drill in, plus
// the three ways one of them can be broken. Every name here is produced by
// makeStoredForms from the same seeded database, so a row count is a
// measurement of what the restore actually carried.
var storedForms = []string{
	"orders.dump", "orders.dump.gz", "orders.sql", "orders.sql.gz",
	"half.sql", "half.sql.gz", "crc.sql.gz",
}

// TestStoredDumpFormsEndToEnd restores every shape a pg_dump artifact is
// stored in against a real engine, and proves the failures that only a real
// gzip and a real psql produce — including the one no exit code reports: a
// perfectly valid gzip file holding a dump that was never finished, which
// restores in full and would otherwise pass the drill.
func TestStoredDumpFormsEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}

	dir := t.TempDir()
	makeStoredForms(t, ctx, provider, params, dir)

	t.Run("restorable shapes", func(t *testing.T) {
		for _, name := range []string{"orders.dump", "orders.dump.gz", "orders.sql", "orders.sql.gz"} {
			t.Run(name, func(t *testing.T) {
				rows, err := drillStoredForm(t, ctx, provider, params, filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("provision %s: %v", name, err)
				}
				if rows != "1000" {
					t.Errorf("row count = %q, want 1000 — the restore did not carry the data", rows)
				}
			})
		}
	})

	t.Run("broken shapes are refused", func(t *testing.T) {
		tests := []struct {
			name   string
			file   string
			wantIn string
		}{
			// The witness earns its keep here: gzip is content, psql is
			// content, and only the dump's missing closing line is not.
			{"a valid member holding a dump that was never finished", "half.sql.gz", "not a complete dump"},
			{"a plain dump that stops halfway", "half.sql", "not a complete dump"},
			// Every byte arrived and only the trailing checksum disagreed.
			// The data may well be whole; the drill refuses anyway, because
			// "may well be" is not what a signed record rests on.
			{"a member whose checksum does not match its data", "crc.sql.gz", "could not be decompressed"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rows, err := drillStoredForm(t, ctx, provider, params, filepath.Join(dir, tt.file))
				var aerr *adapter.Error
				if err == nil || !errors.As(err, &aerr) {
					t.Fatalf("provision = %q, %v — want a refusal", rows, err)
				}
				if aerr.Code != "source_corrupt" || !strings.Contains(aerr.Message, tt.wantIn) {
					t.Errorf("error = %s/%q, want source_corrupt mentioning %q",
						aerr.Code, aerr.Message, tt.wantIn)
				}
			})
		}
	})
}

// drillStoredForm restores one stored artifact in a sandbox of its own and
// reports the row count the restore produced.
func drillStoredForm(t *testing.T, ctx context.Context, provider *docker.Provider,
	params map[string]string, fixture string) (string, error) {
	t.Helper()
	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "pgdump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		return "", err
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"psql", "-h", "127.0.0.1", "-U", res.Connection.User,
			"-d", res.Connection.Database, "-tA", "-v", "ON_ERROR_STOP=1",
			"-c", "SELECT count(*) FROM orders"},
	})
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("count rows: exit %d: %s", out.ExitCode, out.Stderr)
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}

// makeStoredForms seeds one database and writes every artifact shape out of
// it, so the shapes differ only in how they are stored.
//
// The halved dump is cut on a line boundary deliberately. Measured: cut
// mid-row, psql reports the malformed value and the drill fails for the
// wrong reason; cut cleanly, psql treats the stream's end as the end of the
// data, restores 477 of the 1000 rows and exits 0 — which is the silent
// partial restore §5 forbids, and the only thing that reports it is the
// closing line the dump never got to write.
//
// The checksum case is built by flipping one byte of the gzip trailer
// rather than truncating the member, so that every byte still arrives and
// only the decompressor's own verdict says otherwise. The replacement byte
// is derived from the original, so the fixture cannot accidentally be the
// value it already held.
func makeStoredForms(t *testing.T, ctx context.Context, provider *docker.Provider,
	params map[string]string, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	dropPreinstalledExtensions(t, ctx, seed)
	seedSQL := `CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2) NOT NULL);
INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,1000);`
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", seedSQL)
	mustExec(t, ctx, seed, "sh", "-c", `set -e
cd /tmp
pg_dump -h 127.0.0.1 -U postgres -Fc postgres > orders.dump
gzip -c orders.dump > orders.dump.gz
pg_dump -h 127.0.0.1 -U postgres -Fp postgres > orders.sql
gzip -c orders.sql > orders.sql.gz
head -n $(( $(wc -l < orders.sql) / 2 )) orders.sql > half.sql
gzip -c half.sql > half.sql.gz
sz=$(stat -c%s orders.sql.gz)
cp orders.sql.gz crc.sql.gz
orig=$(dd if=orders.sql.gz bs=1 skip=$((sz-5)) count=1 status=none | od -An -tu1 | tr -d " ")
printf "$(printf "\\\\%03o" $(( (orig + 1) % 256 )))" | dd of=crc.sql.gz bs=1 seek=$((sz-5)) conv=notrunc status=none`)

	for _, name := range storedForms {
		if out, err := exec.CommandContext(ctx, "docker", "cp",
			seed.ID()+":/tmp/"+name, filepath.Join(dest, name)).CombinedOutput(); err != nil {
			t.Fatalf("extract %s: %v: %s", name, err, out)
		}
	}
}

// TestVectorExtensionRestoreDrill earns the manifest's pgvector entry:
// listed means exercised (docs/engine-versions.md §2), and for a variant
// image the exercise must cover what makes it a variant — a plain dump
// restoring on the pgvector image would say nothing about vectors. The
// fixture carries a vector column and an HNSW index; the drill restores
// it, proves the index was rebuilt, and answers a nearest-neighbour
// query through the probe-declared runner. On images without the
// extension the test skips: this run then claims nothing either way.
func TestVectorExtensionRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}

	fixture := filepath.Join(t.TempDir(), "vectors.dump")
	if !makeVectorFixture(t, ctx, provider, params, fixture) {
		t.Skip("image does not provide the vector extension; the pgvector matrix job exercises this test")
	}

	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "pgdump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The restored HNSW index is the point: index rebuilds are part of the
	// measured restore, and their absence would be a silently weaker
	// recovery than the backup promises.
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM pg_indexes WHERE tablename = 'items' AND indexdef LIKE '%hnsw%'", "1")
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM items", "203")
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT label FROM items ORDER BY embedding <-> '[1,0,0]' LIMIT 1", "closest")
}

// makeVectorFixture seeds vectors under an HNSW index and dumps them;
// false reports an image without the extension.
func makeVectorFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
	params map[string]string, dest string) bool {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)
	dropPreinstalledExtensions(t, ctx, seed)

	avail, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-tA", "-c",
		"SELECT count(*) FROM pg_available_extensions WHERE name = 'vector'"}})
	if err != nil {
		t.Fatalf("probe extension: %v", err)
	}
	if avail.ExitCode != 0 || strings.TrimSpace(string(avail.Stdout)) != "1" {
		return false
	}

	seedSQL := `CREATE EXTENSION vector;
CREATE TABLE items (id bigserial PRIMARY KEY, label text NOT NULL, embedding vector(3));
INSERT INTO items (label, embedding) VALUES ('closest', '[1,0,0]'), ('mid', '[0.5,0.5,0]'), ('far', '[0,0,1]');
INSERT INTO items (label, embedding)
  SELECT 'filler-'||g, ARRAY[1+random(), 1+random(), 1+random()]::vector(3) FROM generate_series(1,200) g;
CREATE INDEX ON items USING hnsw (embedding vector_l2_ops);`
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", seedSQL)
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/vectors.dump", "postgres")
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/vectors.dump", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return true
}

// assertRunnerRow runs one check through the probe-declared runner and
// asserts its single-row answer.
func assertRunnerRow(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, res *adapter.ProvisionResult, sql, want string) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", sql))
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q", sql, got, out.ExitCode, out.Stderr, want)
	}
}

// TestTimescaleRestoreDrill earns the manifest's timescaledb entry the
// way the pgvector one is earned: listed means exercised, and for a
// variant image the exercise must cover what makes it a variant. The
// fixture is production-shaped on purpose — compressed chunks, a
// continuous aggregate, a retention policy — because that is the shape
// whose unframed restore breaks (measured: partial rows, 'could not
// find hypertable'), so only the framed timescaledb_dump kind can
// restore it whole. On images without the extension the test skips.
func TestTimescaleRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}

	fixture := filepath.Join(t.TempDir(), "timescale.dump")
	if !makeTimescaleFixture(t, ctx, provider, params, fixture) {
		t.Skip("image does not provide the timescaledb extension; the timescaledb matrix job exercises this test")
	}

	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "timescaledb_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Every hypertable property the fixture carries must survive: the
	// rows across chunks, the compressed chunks still readable, the
	// continuous aggregate's data, and the restored retention policy.
	assertRunnerRow(t, ctx, sbx, probe, res, "SELECT count(*) FROM metrics", timescaleRows)
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) > 0 FROM timescaledb_information.chunks WHERE hypertable_name = 'metrics' AND is_compressed",
		"t")
	assertRunnerRow(t, ctx, sbx, probe, res, "SELECT count(*) FROM metrics_hourly", timescaleRows)
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention'", "1")
}

// TestTimescalePolicyJobsCannotTouchTheArtifact is the measured heart of
// the policy pin. A restored TimescaleDB catalog brings its own
// automation with it, and timescaledb_post_restore() does not merely
// release the background workers: the retention policy runs in the same
// second it returns, because bgw_job_stat is absent from the dump and a
// job with no next_start is due immediately (measured, unpinned: 15 of 29
// chunks and 52% of the rows gone before the frame closed, with the
// restore reported successful).
//
// The test proves both halves. With the pin the whole hypertable is
// there and the policy has not run; then it hands the policy its
// next_start back and the same chunks disappear — which is what makes the
// first half mean something.
func TestTimescalePolicyJobsCannotTouchTheArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}

	fixture := filepath.Join(t.TempDir(), "timescale.dump")
	if !makeTimescaleFixture(t, ctx, provider, params, fixture) {
		t.Skip("image does not provide the timescaledb extension; the timescaledb matrix job exercises this test")
	}

	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "timescaledb_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v — a hypertable older than its own retention policy is a healthy backup", err)
	}

	// The artifact arrived whole, the policy that would trim it has not
	// run, and the flag the dump carried is untouched: the pin writes
	// next_start, which the dump never had.
	before := timescaleChunks(t, ctx, sbx)
	assertRunnerRow(t, ctx, sbx, probe, res, "SELECT count(*) FROM metrics", timescaleRows)
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT coalesce(max(js.total_runs), 0) FROM timescaledb_information.job_stats js "+
			"JOIN timescaledb_information.jobs j ON j.job_id = js.job_id "+
			"WHERE j.proc_name = 'policy_retention'", "0")
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT bool_and(scheduled) FROM timescaledb_information.jobs", "t")
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT min(ts) < now() - interval '90 days' FROM metrics", "t")

	// Hand the policy its next_start back, and it takes what the drill
	// just proved — the artifact was retention-eligible all along, so the
	// assertions above are about the pin and not about a fixture with
	// nothing to lose.
	mustExec(t, ctx, sbx, "psql", "-h", "127.0.0.1", "-U", "postgres", "-tA", "-v", "ON_ERROR_STOP=1", "-c",
		"SELECT alter_job(job_id, next_start => now()) FROM timescaledb_information.jobs "+
			"WHERE proc_name = 'policy_retention'")
	awaitRetentionRun(t, ctx, sbx, time.Minute)
	if after := timescaleChunks(t, ctx, sbx); after >= before {
		t.Errorf("the released retention policy dropped nothing (%d chunks before, %d after) — "+
			"the fixture cannot show what the pin prevents", before, after)
	}
}

// timescaleChunks counts the hypertable's chunks right now.
func timescaleChunks(t *testing.T, ctx context.Context, sbx *docker.Sandbox) int {
	t.Helper()
	out := psqlValue(t, ctx, sbx,
		"SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_name = 'metrics'")
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("chunk count = %q: %v", out, err)
	}
	return n
}

// awaitRetentionRun waits for the retention policy to run once it is
// allowed to.
func awaitRetentionRun(t *testing.T, ctx context.Context, sbx *docker.Sandbox, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if psqlValue(t, ctx, sbx,
			"SELECT coalesce(max(js.total_runs), 0) > 0 FROM timescaledb_information.job_stats js "+
				"JOIN timescaledb_information.jobs j ON j.job_id = js.job_id "+
				"WHERE j.proc_name = 'policy_retention'") == "t" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the retention policy never ran within %s of being released", budget)
}

// psqlValue reads one value straight from the sandbox's server.
func psqlValue(t *testing.T, ctx context.Context, sbx *docker.Sandbox, sql string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"psql", "-h", "127.0.0.1",
		"-U", "postgres", "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("psql %q: %v (exit %d, stderr %s)", sql, err, out.ExitCode, out.Stderr)
	}
	return strings.TrimSpace(string(out.Stdout))
}

// TestTimescaleDumpIsFencedFromThePlainKind proves the fence end to end:
// the same dump under the plain pgdump kind is refused by name, with the
// framed kind in the message, before the restore that would break it.
func TestTimescaleDumpIsFencedFromThePlainKind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	provider := docker.New(nil)
	params := map[string]string{"image": verifiedImage(t), "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}

	fixture := filepath.Join(t.TempDir(), "timescale.dump")
	if !makeTimescaleFixture(t, ctx, provider, params, fixture) {
		t.Skip("image does not provide the timescaledb extension; the timescaledb matrix job exercises this test")
	}

	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "pgdump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "unsupported_source" ||
		!strings.Contains(aerr.Message, "timescaledb_dump") {
		t.Fatalf("provision error = %v, want unsupported_source teaching the framed kind", err)
	}
}

// timescaleRows is one hourly sample per row, so the fixture's hypertable
// spans 200 days — more than the 90-day retention policy it also carries.
// That is the ordinary shape of a metrics database kept for compliance,
// and the reason this fixture can tell a whole restore from a restore the
// engine trimmed on its way in.
const timescaleRows = "4800"

// makeTimescaleFixture seeds a production-shaped hypertable and dumps
// it; false reports an image without the extension.
func makeTimescaleFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
	params map[string]string, dest string) bool {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)

	avail, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-tA", "-c",
		"SELECT count(*) FROM pg_available_extensions WHERE name = 'timescaledb'"}})
	if err != nil {
		t.Fatalf("probe extension: %v", err)
	}
	if avail.ExitCode != 0 || strings.TrimSpace(string(avail.Stdout)) != "1" {
		return false
	}

	for _, sql := range []string{
		"CREATE EXTENSION IF NOT EXISTS timescaledb",
		"CREATE TABLE metrics (ts timestamptz NOT NULL, device int NOT NULL, value double precision)",
		"SELECT create_hypertable('metrics', 'ts', chunk_time_interval => interval '7 days')",
		"INSERT INTO metrics SELECT now() - (i || ' hours')::interval, i % 10, random() FROM generate_series(1, " +
			timescaleRows + ") i",
		"ALTER TABLE metrics SET (timescaledb.compress, timescaledb.compress_segmentby = 'device')",
		"SELECT count(compress_chunk(c)) FROM show_chunks('metrics', older_than => interval '2 days') c",
		"CREATE MATERIALIZED VIEW metrics_hourly WITH (timescaledb.continuous) AS " +
			"SELECT time_bucket('1 hour', ts) AS bucket, device, avg(value) FROM metrics GROUP BY 1, 2 WITH NO DATA",
		"CALL refresh_continuous_aggregate('metrics_hourly', NULL, NULL)",
		// Each policy is created and parked in one statement: the seed's
		// own scheduler runs a new retention policy within the second
		// (measured), and a fixture that expired its own history before
		// pg_dump reached it would prove nothing. next_start lives in
		// bgw_job_stat, which the dump does not carry, so the artifact is
		// the same either way.
		"SELECT alter_job(add_retention_policy('metrics', interval '90 days'), next_start => 'infinity')",
		"SELECT alter_job(add_compression_policy('metrics', interval '7 days'), next_start => 'infinity')",
		"SELECT alter_job(add_continuous_aggregate_policy('metrics_hourly', " +
			"start_offset => interval '30 days', end_offset => interval '1 hour', " +
			"schedule_interval => interval '1 hour'), next_start => 'infinity')",
	} {
		mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", sql)
	}
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc",
		"-f", "/tmp/timescale.dump", "postgres")

	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/timescale.dump", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return true
}
