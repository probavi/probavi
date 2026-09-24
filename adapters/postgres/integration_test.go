//go:build integration

package main_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// TestPgBackRestNonDefaultRoleAndDatabase drills a cluster that has no
// postgres role and keeps its data outside the postgres database — the
// shape issue #273 was reported against, and the one the official image
// produces for anybody who sets POSTGRES_USER. Until the physical path read
// options.user and options.database, this restore succeeded and the drill
// then reported engine_not_ready after two minutes of polling a role that
// was never in the backup.
func TestPgBackRestNonDefaultRoleAndDatabase(t *testing.T) {
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
	makeBackRestRepoAs(t, ctx, image, hostRepo, "rig", "rigdb")

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
		Options: map[string]string{"user": "rig", "database": "rigdb"},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Connection.User != "rig" || res.Connection.Database != "rigdb" {
		t.Errorf("connection = %+v, want the checks pointed at the configured role and database", res.Connection)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v err=%v", health, err)
	}

	// The rows live in rigdb, which no connection to the postgres database
	// could ever see: PostgreSQL does not query across databases.
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "rig", "-d", "rigdb", "-tA", "-c",
		"SELECT count(*) FROM events"}})
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

// makeBackRestRepoAs seeds a repo whose cluster is bootstrapped with a
// superuser other than postgres and whose data lives in its own database —
// what initdb -U does, and what the official image does for POSTGRES_USER.
// The cluster has no postgres role at all, so pgbackrest itself has to be
// told which role to connect as.
func makeBackRestRepoAs(t *testing.T, ctx context.Context, image, dest, user, database string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--label", docker.LabelSandbox+"=1", "--label", "com.probavi.pid="+strconv.Itoa(os.Getpid()),
		"--network", "none", image, "sleep", "infinity").Output()
	if err != nil {
		t.Fatalf("start seed container: %v", err)
	}
	id := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", "-v", id).Run() //nolint:errcheck // best-effort cleanup

	seedScript := fmt.Sprintf(`set -e
mkdir -p /tmp/repo /etc/pgbackrest "$PGDATA"
printf '[global]\nrepo1-path=/tmp/repo\n\n[demo]\npg1-path=%%s\npg1-user=%s\n' "$PGDATA" > /etc/pgbackrest/pgbackrest.conf
chown -R postgres:postgres /tmp/repo /etc/pgbackrest "$PGDATA"
gosu postgres initdb -U %s -D "$PGDATA"
printf "archive_mode=on\narchive_command='pgbackrest --stanza=demo archive-push %%%%p'\n" >> "$PGDATA"/postgresql.conf
gosu postgres pg_ctl -D "$PGDATA" -w -l /tmp/pg.log start
gosu postgres psql -U %s -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE %s"
gosu postgres psql -U %s -d %s -v ON_ERROR_STOP=1 -c "CREATE TABLE events (id bigserial PRIMARY KEY, total numeric(10,2)); INSERT INTO events (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,500);"
gosu postgres pgbackrest --stanza=demo stanza-create
gosu postgres pgbackrest --stanza=demo --type=full backup
gosu postgres psql -U %s -d %s -v ON_ERROR_STOP=1 -c "SELECT pg_switch_wal();" > /dev/null
gosu postgres pg_ctl -D "$PGDATA" -w stop`,
		user, user, user, database, user, database, user, database)
	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "sh", "-c", seedScript).CombinedOutput(); err != nil {
		t.Fatalf("seed pgbackrest repo: %v: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", id+":/tmp/repo", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract repo: %v: %s", err, out)
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
//
// The postgis image needs more than a drop of its own extension, and the
// reason is worth stating because it is also what an operator meets. It
// installs postgis, postgis_topology and postgis_tiger_geocoder, and the
// last two live in schemas of their own — `topology`, `tiger`,
// `tiger_data`. pg_dump emits those as plain `CREATE SCHEMA`, which has
// no IF NOT EXISTS, so a dump taken from such a database does not restore
// into another instance of the same image: `schema "tiger" already
// exists`, and the restore fails (measured). Dropping the family here
// leaves a database that states only what a test put in it. The order is
// the dependency order; fuzzystrmatch is the tiger geocoder's.
func dropPreinstalledExtensions(t *testing.T, ctx context.Context, seed *docker.Sandbox) {
	t.Helper()
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres",
		"-v", "ON_ERROR_STOP=1", "-c", strings.Join([]string{
			"DROP EXTENSION IF EXISTS timescaledb",
			"DROP EXTENSION IF EXISTS postgis_tiger_geocoder",
			"DROP EXTENSION IF EXISTS postgis_topology",
			"DROP EXTENSION IF EXISTS postgis",
			"DROP EXTENSION IF EXISTS fuzzystrmatch",
			"DROP SCHEMA IF EXISTS tiger CASCADE",
			"DROP SCHEMA IF EXISTS tiger_data CASCADE",
			"DROP SCHEMA IF EXISTS topology CASCADE",
		}, "; "))
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
	// The expiry check is waived for this one build because an image's
	// own base can outlive its distribution's support and nothing here
	// controls when. Measured 2026-09-10: the postgis variant is Debian
	// 11, whose security suite stopped being refreshed — `apt-get update`
	// exits 100 on "Release file for …/bullseye-security/InRelease is
	// expired (invalid since 2d 22h)" — and the `&&` then keeps the
	// install from running at all. Nothing about the image or the package
	// had changed; the clock had. pgbackrest itself comes from the
	// PostgreSQL project's own repository, which is current
	// (2.59.1-1.pgdg11+1, measured), and this image is a throwaway the
	// drill never ships: waiving the check says that out loud where
	// deleting the stale suite from the image would hide it.
	dockerfile := "FROM " + verifiedImage(t) + "\n" +
		"RUN apt-get -o Acquire::Check-Valid-Until=false update" +
		" && apt-get install -y --no-install-recommends pgbackrest && rm -rf /var/lib/apt/lists/*\n"
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
mkdir -p /tmp/repo /etc/pgbackrest "$PGDATA"
printf '[global]\nrepo1-path=/tmp/repo\n\n[demo]\npg1-path=%s\n' "$PGDATA" > /etc/pgbackrest/pgbackrest.conf
chown -R postgres:postgres /tmp/repo /etc/pgbackrest "$PGDATA"
gosu postgres initdb -D "$PGDATA"
printf "archive_mode=on\narchive_command='pgbackrest --stanza=demo archive-push %%p'\n" >> "$PGDATA"/postgresql.conf
gosu postgres pg_ctl -D "$PGDATA" -w -l /tmp/pg.log start
gosu postgres psql -v ON_ERROR_STOP=1 -c "CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2)); INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,500);"
gosu postgres pgbackrest --stanza=demo stanza-create
gosu postgres pgbackrest --stanza=demo --type=full backup
sleep 1
gosu postgres psql -tA -c "SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')" > /tmp/pitr-target
sleep 1
gosu postgres psql -v ON_ERROR_STOP=1 -c "INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,200);"
gosu postgres psql -v ON_ERROR_STOP=1 -c "SELECT pg_switch_wal();" > /dev/null
gosu postgres pg_ctl -D "$PGDATA" -w stop`
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

// TestDirectorySelectionProvesTheOldestBackup is the exit criterion of
// the selection policy: a drill can prove the *oldest* backup in a
// retention window, and the record says which one it proved.
//
// That is what a newest-only policy cannot do. A drill running every
// night proves last night's backup every night, and the backup an
// incident reaches for — once the damage turns out to predate yesterday —
// is the one nothing has ever restored. The two generations here hold
// different row counts, so which one the sandbox received is a
// measurement rather than an inference, and the checksum the adapter
// reports for the evidence record is compared against the older file's
// own bytes.
func TestDirectorySelectionProvesTheOldestBackup(t *testing.T) {
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
		Source: adapter.ProvisionSource{
			Kind: "pgdump_dir", Path: dir, Params: map[string]string{"select": "oldest"},
		},
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
	if out.ExitCode != 0 || count != strconv.Itoa(staleRowCount) {
		t.Fatalf("row count = %q (exit %d), want %d — the drill restored the newest backup "+
			"while the config asked for the oldest", count, out.ExitCode, staleRowCount)
	}

	// The record has to name what was proved, because source.params never
	// reaches it (docs/drill-config.md §7): the checksum is what tells an
	// auditor which of the two backups this run stands for.
	if want := fileSum(t, filepath.Join(dir, "stale.dump")); res.SourceIdentity.Checksum != want {
		t.Errorf("backup checksum = %s, want %s — the record would name the wrong artifact",
			res.SourceIdentity.Checksum, want)
	}
}

// fileSum is the artifact identity the adapter reports, computed
// independently here so the two have to agree.
func fileSum(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
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

// TestPostGISRestoreDrill earns the manifest's postgis entry the way the
// pgvector and timescaledb ones are earned: listed means exercised, and
// for a variant image the exercise must cover what makes it a variant.
// Restoring an ordinary table on the postgis image would say nothing
// about spatial data, so the fixture is a geometry column under a GiST
// index, and the checks go through the probe-declared runner.
//
// No source kind of its own, deliberately, and that is the finding rather
// than an omission: pg_dump records `CREATE EXTENSION IF NOT EXISTS
// postgis WITH SCHEMA public`, so the existing pgdump kinds restore a
// spatial database unchanged (measured). The entry is still load-bearing
// — the same dump against a plain postgres image fails with `extension
// "postgis" is not available` — which is exactly why it is exercised here
// rather than asserted in a document.
//
// The fixture is two thousand rows on purpose. The five named points are
// what the spatial queries select; the filler sits a hemisphere away so
// the planner reaches for the GiST index unaided, which is the property a
// restored index has to have and a merely present one need not.
func TestPostGISRestoreDrill(t *testing.T) {
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

	fixture := filepath.Join(t.TempDir(), "spatial.dump")
	if !makePostGISFixture(t, ctx, provider, params, fixture) {
		t.Skip("image does not provide the postgis extension; the postgis matrix job exercises this test")
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

	assertRunnerRow(t, ctx, sbx, probe, res, "SELECT count(*) FROM places", "2000")
	// A bounding-box query and a distance query: the first is what the
	// GiST index answers, the second is computed geography rather than
	// stored geometry, so together they prove the extension is doing work
	// and not merely present.
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM places WHERE geom && ST_MakeEnvelope(16, 45, 23, 49, 4326)", "5")
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM places WHERE ST_DWithin(geom::geography, "+
			"ST_SetSRID(ST_MakePoint(19.0402, 47.4979), 4326)::geography, 60000)", "1")
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM pg_indexes WHERE indexname = 'places_geom_gix' AND indexdef LIKE '%USING gist%'", "1")
	// spatial_ref_sys travels selectively: postgis registers it with a
	// filter, so the extension's own eight and a half thousand rows stay
	// out of the dump and only what the operator added comes back. A
	// restored drill that lost a custom projection would answer every
	// query above and still be missing what the operator declared.
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM spatial_ref_sys WHERE srid = 990001 AND auth_name = 'PROBAVI'", "1")

	// Present is not the same as usable: a restored GiST index the planner
	// will not touch is a slower recovery than the backup promised, and
	// nothing in the row counts above would notice.
	plan, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", "postgres", "-tA", "-c",
		"EXPLAIN (FORMAT JSON) SELECT count(*) FROM places WHERE geom && ST_MakeEnvelope(16, 45, 23, 49, 4326)"}})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if plan.ExitCode != 0 || !strings.Contains(string(plan.Stdout), "places_geom_gix") {
		t.Errorf("the planner did not reach for the restored GiST index (exit %d): %s", plan.ExitCode, plan.Stdout)
	}
}

// makePostGISFixture seeds a spatial database in a throwaway sandbox and
// extracts a pg_dump of it. It reports false when the image carries no
// postgis, which is how this test skips on the plain matrix jobs.
func makePostGISFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
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
		"SELECT count(*) FROM pg_available_extensions WHERE name = 'postgis'"}})
	if err != nil {
		t.Fatalf("probe extension: %v", err)
	}
	if avail.ExitCode != 0 || strings.TrimSpace(string(avail.Stdout)) != "1" {
		return false
	}

	// The seed creates the extension itself rather than inheriting the
	// image's: an operator's database is one where somebody ran CREATE
	// EXTENSION, and that is the dump this drill has to restore.
	seedSQL := `CREATE EXTENSION postgis;
CREATE TABLE places (id bigserial PRIMARY KEY, name text NOT NULL, geom geometry(Point, 4326) NOT NULL);
INSERT INTO places (name, geom) VALUES
  ('Budapest', ST_SetSRID(ST_MakePoint(19.0402, 47.4979), 4326)),
  ('Debrecen', ST_SetSRID(ST_MakePoint(21.6273, 47.5316), 4326)),
  ('Szeged',   ST_SetSRID(ST_MakePoint(20.1414, 46.2530), 4326)),
  ('Pecs',     ST_SetSRID(ST_MakePoint(18.2325, 46.0727), 4326)),
  ('Gyor',     ST_SetSRID(ST_MakePoint(17.6504, 47.6875), 4326));
INSERT INTO places (name, geom)
  SELECT 'filler-'||g, ST_SetSRID(ST_MakePoint(-60.0 + (g % 100) * 0.05, -40.0 + (g / 100) * 0.05), 4326)
  FROM generate_series(1, 1995) g;
CREATE INDEX places_geom_gix ON places USING GIST (geom);
INSERT INTO spatial_ref_sys (srid, auth_name, auth_srid, srtext, proj4text)
  VALUES (990001, 'PROBAVI', 990001, 'LOCAL_CS["probavi-drill"]', '+proj=longlat +datum=WGS84 +no_defs');
ANALYZE places;`
	mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", seedSQL)
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/spatial.dump", "postgres")
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/spatial.dump", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return true
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

// TestTimescalePolicyOwnerRoleMustExist pins both halves of what issue
// #278 found, and of the sentence the README now carries.
//
// Every TimescaleDB policy is a row in the restored catalog whose owner
// column is a regrole, and a regrole is written out as the role's name. It
// is data, so `pg_restore --no-owner` cannot touch it — the flag rewrites
// ALTER … OWNER TO statements, and there is no such statement here. A dump
// from a database owned by an application role therefore needs that role in
// the sandbox, and until this test there was no fixture with one: the
// suite's hypertable was seeded by postgres, a role every image has.
//
// The remedy is to give the sandbox that role as its own superuser. It is
// not discoverable, which is the actual defect, so both sides are asserted:
// the plain configuration fails naming the role, and the documented one
// restores the same dump.
func TestTimescalePolicyOwnerRoleMustExist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const owner = "rig"
	provider := docker.New(nil)
	image := verifiedImage(t)
	plain := map[string]string{"image": image, "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}
	asOwner := map[string]string{"image": image, "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"env.POSTGRES_USER": owner, "env.POSTGRES_DB": owner, "memory": engineMemoryLimit}

	fixture := filepath.Join(t.TempDir(), "owned.dump")
	if !makeTimescaleFixtureOwnedBy(t, ctx, provider, asOwner, owner, fixture) {
		t.Skip("image does not provide the timescaledb extension; the timescaledb matrix job exercises this test")
	}

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("a sandbox without the owner role fails naming it", func(t *testing.T) {
		sbx, err := provider.Create(ctx, plain)
		if err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		defer destroy(t, sbx)
		awaitReady(t, ctx, sbx)

		_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "timescaledb_dump", Path: fixture},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		var aerr *adapter.Error
		if err == nil || !errors.As(err, &aerr) {
			t.Fatalf("provision error = %v, want an adapter error", err)
		}
		if aerr.Code != "restore_failed" {
			t.Errorf("code = %s, want restore_failed", aerr.Code)
		}
		// The engine's own words carry the remedy. A message that did not
		// name the role would leave the operator with nothing to act on,
		// and the README's sentence would have nothing to point at.
		if !strings.Contains(aerr.Message, owner) || !strings.Contains(aerr.Message, "bgw_job") {
			t.Errorf("message = %q, want the missing role and the catalog table named", aerr.Message)
		}
		// And the remedy, not just the symptom: an operator reading this
		// record must be able to act on it without reading the source.
		if !strings.Contains(aerr.Message, "timescaledb_dump_with_globals") {
			t.Errorf("message = %q, want the source kind that carries the roles", aerr.Message)
		}
	})

	t.Run("a sandbox bootstrapped with the owner role restores it", func(t *testing.T) {
		sbx, err := provider.Create(ctx, asOwner)
		if err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		defer destroy(t, sbx)
		awaitReady(t, ctx, sbx)

		probe, err := runner.Probe(ctx)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "timescaledb_dump", Path: fixture},
			Options: map[string]string{"user": owner, "database": owner},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		}, sbx)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		assertRunnerRow(t, ctx, sbx, probe, res, "SELECT count(*) FROM metrics", ownedRows)
		// The policy is back, still owned by the role the backup named —
		// the restore reproduced the catalog rather than rewriting it.
		assertRunnerRow(t, ctx, sbx, probe, res,
			"SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND owner::text = '"+owner+"'",
			"1")
	})
}

// TestTimescaleWithGlobalsRestoresThePolicyOwner drills the kind that
// closes issue #278 properly: the cluster globals travel with the dump, so
// the role a policy is owned by is created before the catalog COPY reaches
// it — and the sandbox needs no bootstrap role of its own.
//
// This is the case the sandbox-superuser workaround cannot cover. Here the
// drill restores as postgres into a stock image, and the policy comes back
// owned by a role that image never had.
func TestTimescaleWithGlobalsRestoresThePolicyOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const owner = "rig"
	provider := docker.New(nil)
	image := verifiedImage(t)
	plain := map[string]string{"image": image, "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"memory": engineMemoryLimit}
	asOwner := map[string]string{"image": image, "env.POSTGRES_HOST_AUTH_METHOD": "trust",
		"env.POSTGRES_USER": owner, "env.POSTGRES_DB": owner, "memory": engineMemoryLimit}

	set := t.TempDir()
	if !makeTimescaleFixtureOwnedBy(t, ctx, provider, asOwner, owner, filepath.Join(set, "metrics.dump")) {
		t.Skip("image does not provide the timescaledb extension; the timescaledb matrix job exercises this test")
	}
	dumpGlobals(t, ctx, provider, asOwner, owner, filepath.Join(set, "globals.sql"))

	sbx, err := provider.Create(ctx, plain)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)
	awaitReady(t, ctx, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind: "timescaledb_dump_with_globals", Path: set,
			Params: map[string]string{"globals": "globals.sql", "dump": "metrics.dump"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	assertRunnerRow(t, ctx, sbx, probe, res, "SELECT count(*) FROM metrics", ownedRows)
	// The role came from the globals script, not from the image.
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM pg_roles WHERE rolname = '"+owner+"'", "1")
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND owner::text = '"+owner+"'",
		"1")
	// The frame still ran: the policy pin is what keeps a restored
	// retention policy from trimming the artifact in the second
	// timescaledb_post_restore() returns. Asked of the restored policy by
	// name rather than counted — the pin covers every job in the catalog,
	// TimescaleDB's own included, and how many of those a version ships is
	// not this test's business.
	assertRunnerRow(t, ctx, sbx, probe, res,
		"SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND next_start = 'infinity'", "1")
}

// dumpGlobals writes a pg_dumpall --globals-only script out of the seed
// cluster, the second member the with_globals kinds take.
func dumpGlobals(t *testing.T, ctx context.Context, provider *docker.Provider,
	params map[string]string, owner, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create globals sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)
	mustExec(t, ctx, seed, "sh", "-c",
		"pg_dumpall -h 127.0.0.1 -U "+owner+" --globals-only > /tmp/globals.sql")
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/globals.sql", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract globals: %v: %s", err, out)
	}
}

// ownedRows is deliberately small: this fixture exists to carry a policy
// owned by a non-superuser role, and the hypertable's shape is proven by
// TestTimescaleRestoreDrill.
const ownedRows = "200"

// makeTimescaleFixtureOwnedBy seeds a hypertable and a retention policy as
// the role the sandbox was bootstrapped with, so the dump's bgw_job row
// names that role rather than postgres. false reports an image without the
// extension.
func makeTimescaleFixtureOwnedBy(t *testing.T, ctx context.Context, provider *docker.Provider,
	params map[string]string, owner, dest string) bool {
	t.Helper()
	seed, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)

	avail, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-h", "127.0.0.1", "-U", owner, "-d", owner, "-tA", "-c",
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
			ownedRows + ") i",
		// Parked on creation for the reason makeTimescaleFixture gives: a
		// policy that ran before pg_dump reached it would leave a fixture
		// proving something else.
		"SELECT alter_job(add_retention_policy('metrics', interval '90 days'), next_start => 'infinity')",
	} {
		mustExec(t, ctx, seed, "psql", "-h", "127.0.0.1", "-U", owner, "-d", owner, "-v", "ON_ERROR_STOP=1", "-c", sql)
	}
	mustExec(t, ctx, seed, "pg_dump", "-h", "127.0.0.1", "-U", owner, "-Fc", "--no-owner",
		"-f", "/tmp/owned.dump", owner)

	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/owned.dump", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return true
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

// TestPgBackRestRefusesAnIncompleteChain proves the host-side pre-check
// against a repository a real pgbackrest wrote.
//
// Measured on the same repository before this check existed: with the
// restored backup's stop segment removed, `pgbackrest restore` exits 0 and
// the failure appears only when the server will not start, logging
// "startup process exited with exit code 1" — which the adapter reported
// as "restored cluster failed to start", naming nothing. The refusal now
// happens before the repository is transferred, and the assertion below
// that the sandbox holds no copy of it is the half that says so.
func TestPgBackRestRefusesAnIncompleteChain(t *testing.T) {
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

	// The stop segment of the only backup: the WAL that carries the
	// cluster to a consistent state.
	stop := newestArchiveStop(t, hostRepo, "demo")
	removed := 0
	matches, err := filepath.Glob(filepath.Join(hostRepo, "archive", "demo", "*", stop[:16], stop+"*"))
	if err != nil {
		t.Fatalf("glob the archive: %v", err)
	}
	for _, m := range matches {
		if strings.HasSuffix(m, ".backup") {
			continue
		}
		if err := os.Remove(m); err != nil {
			t.Fatalf("remove %s: %v", m, err)
		}
		removed++
	}
	if removed == 0 {
		t.Fatalf("found no archived copy of %s to remove — the fixture is not what this test assumes", stop)
	}

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
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind: "pgbackrest", Path: hostRepo, Params: map[string]string{"stanza": "demo"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err == nil {
		t.Fatal("provisioned from a repository whose WAL cannot reach consistency")
	}
	var aerr *adapter.Error
	if !errors.As(err, &aerr) {
		t.Fatalf("error %v is not a protocol error", err)
	}
	if aerr.Code != "source_not_found" {
		t.Errorf("code = %q, want source_not_found — the segment is absent, not corrupt", aerr.Code)
	}
	if !strings.Contains(aerr.Message, stop) {
		t.Errorf("message = %q, want it to name the missing segment %s", aerr.Message, stop)
	}

	// The point of doing this host-side: the bytes never moved.
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"sh", "-c", "test -e " + sbx.ScratchDir() + "/probavi-pgbackrest-repo && echo present || echo absent",
	}})
	if err != nil {
		t.Fatalf("inspect the sandbox: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != "absent" {
		t.Errorf("the sandbox holds the repository (%s) — the refusal came after the transfer, "+
			"which is the cost this check exists to avoid", got)
	}
}

// newestArchiveStop reads the archive-stop segment of the repository's
// newest backup out of backup.info, so the test removes the segment the
// adapter will actually ask for rather than one it guessed. This package
// is the adapter's external test package, so it reads the manifest with
// its own eyes rather than through the adapter's parser.
func newestArchiveStop(t *testing.T, repo, stanza string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, "backup", stanza, "backup.info"))
	if err != nil {
		t.Fatalf("read backup.info: %v", err)
	}
	// The manifest's current section lists one backup per line as
	// LABEL={json}; the fixture takes exactly one full backup.
	stops := regexp.MustCompile(`"backup-archive-stop":"([0-9A-Fa-f]{24})"`).FindAllStringSubmatch(string(raw), -1)
	if len(stops) == 0 {
		t.Fatal("the seeded repository names no archive-stop segment")
	}
	newest := stops[len(stops)-1][1]
	return newest
}

// buildBarmanImage adds Barman to the verified postgres image. It is the
// *seeding* image only: the drill below restores in the stock image,
// because placing a cluster and letting PostgreSQL replay WAL needs no
// Barman at all — which is this source kind's whole point.
func buildBarmanImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	image := verifiedImage(t)
	const tag = "probavi-it-barman:16"
	dir := t.TempDir()
	// Same waiver as the pgbackrest tool image: an image's base can outlive
	// its distribution's security suite, and barman comes from the
	// PostgreSQL project's own repository, which is current.
	dockerfile := "FROM " + image + "\n" +
		"RUN apt-get -o Acquire::Check-Valid-Until=false update" +
		" && apt-get install -y --no-install-recommends barman barman-cli" +
		" && rm -rf /var/lib/apt/lists/*\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput()
	if err == nil {
		return tag
	}
	// A variant image may be unable to host the seed, and that is not this
	// flow's failure: the postgis variant is Debian 11, whose security
	// archive no longer carries the python3.9 packages barman depends on
	// (measured 2026-09-23: four 404s out of debian-security), and the
	// timescale variant is Alpine with no apt at all. What those images
	// claim is an extension and a framed logical restore; the Barman flow
	// keeps its coverage from the plain postgres matrix jobs, which is the
	// same division buildPgBackRestImage already makes.
	//
	// A plain postgres image failing here is a real failure and must stay
	// one, so the forgiveness is scoped to the variants by name.
	if strings.HasPrefix(image, "postgres:") {
		t.Fatalf("build barman seed image on %s: %v: %s", image, err, out)
	}
	t.Skipf("variant image %s cannot host the barman seed build (%v); the Barman flow is exercised "+
		"by the plain postgres matrix jobs", image, err)
	return ""
}

// barmanSeedScript bootstraps Barman against a local cluster and takes one
// backup, then writes a second batch of rows whose WAL is archived after
// it. The order matters and was measured: `barman backup` refuses until a
// WAL segment has arrived through the streaming archiver, so cron starts
// the receiver, switch-wal --force --archive delivers the first segment,
// and only then is a backup possible.
const barmanSeedScript = `set -e
export PGDATA=/var/lib/postgresql/data
mkdir -p "$PGDATA" /var/lib/barman /var/log/barman /etc/barman.d
chown -R postgres:postgres "$PGDATA"
id barman >/dev/null 2>&1 || useradd -m -s /bin/bash barman
chown -R barman:barman /var/lib/barman /var/log/barman
gosu postgres initdb -U postgres -D "$PGDATA" >/dev/null 2>&1
{ echo "wal_level=replica"; echo "max_wal_senders=4"; echo "max_replication_slots=4"; \
  echo "listen_addresses='127.0.0.1'"; } >> "$PGDATA/postgresql.conf"
echo "host all all 127.0.0.1/32 trust"         >> "$PGDATA/pg_hba.conf"
echo "host replication all 127.0.0.1/32 trust" >> "$PGDATA/pg_hba.conf"
gosu postgres pg_ctl -D "$PGDATA" -w -l /tmp/pg.log start >/dev/null
gosu postgres psql -U postgres -q -c "CREATE ROLE barman SUPERUSER LOGIN;"
gosu postgres psql -U postgres -q -c "CREATE ROLE streaming_barman REPLICATION LOGIN;"
gosu postgres psql -U postgres -q -c "CREATE TABLE events AS SELECT generate_series(1,500) AS id;"
printf '[barman]\nbarman_home = /var/lib/barman\nbarman_user = barman\nlog_file = /var/log/barman/barman.log\nconfiguration_files_directory = /etc/barman.d\n' > /etc/barman.conf
printf '[demo]\ndescription = demo\nconninfo = host=127.0.0.1 user=barman dbname=postgres\nstreaming_conninfo = host=127.0.0.1 user=streaming_barman dbname=postgres\nbackup_method = postgres\nstreaming_archiver = on\nslot_name = barman\ncreate_slot = auto\n' > /etc/barman.d/demo.conf
gosu barman barman cron >/dev/null 2>&1 || true
sleep 4
gosu barman barman switch-wal --force --archive demo >/dev/null 2>&1 || true
sleep 3
gosu barman barman backup demo >/dev/null
gosu postgres psql -U postgres -qtAc "SELECT now()" > /tmp/between
sleep 2
gosu postgres psql -U postgres -q -c "CREATE TABLE later AS SELECT generate_series(1,100) AS id;"
gosu barman barman switch-wal --force --archive demo >/dev/null 2>&1 || true
sleep 3
gosu barman barman cron >/dev/null 2>&1 || true
sleep 2
gosu postgres pg_ctl -D "$PGDATA" -w stop >/dev/null`

// makeBarmanServer seeds a Barman catalogue and copies the server
// directory out, returning it and the instant between the two batches.
func makeBarmanServer(t *testing.T, ctx context.Context, image, dest string) time.Time {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--label", docker.LabelSandbox+"=1", "--label", "com.probavi.pid="+strconv.Itoa(os.Getpid()),
		"--network", "none", "--memory", engineMemoryLimit, image, "sleep", "infinity").Output()
	if err != nil {
		t.Fatalf("start seed container: %v", err)
	}
	id := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", "-v", id).Run() //nolint:errcheck // best-effort cleanup

	if out, err := exec.CommandContext(ctx, "docker", "exec", id, "sh", "-c", barmanSeedScript).CombinedOutput(); err != nil {
		t.Fatalf("seed barman catalogue: %v: %s", err, out)
	}
	raw, err := exec.CommandContext(ctx, "docker", "exec", id, "cat", "/tmp/between").Output()
	if err != nil {
		t.Fatalf("read the instant between the batches: %v", err)
	}
	between, err := time.Parse("2006-01-02 15:04:05.999999-07", strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse %q: %v", strings.TrimSpace(string(raw)), err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", id+":/var/lib/barman/demo", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract the catalogue: %v: %s", err, out)
	}
	// Barman writes the catalogue as its own user; a drill host reads it
	// as whoever runs the drill.
	if out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		"-v", dest+":/c", image, "chmod", "-R", "a+rX", "/c").CombinedOutput(); err != nil {
		t.Fatalf("make the catalogue readable: %v: %s", err, out)
	}
	return between.UTC()
}

// TestBarmanEndToEnd restores a real Barman backup in the **stock**
// postgres image — no barman, no pgbackrest — and proves the drill carries
// both what the base backup held and what was written after it and
// recovered from archived WAL.
func TestBarmanEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	seedImage := buildBarmanImage(t, ctx)
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	catalogue := filepath.Join(t.TempDir(), "demo")
	makeBarmanServer(t, ctx, seedImage, catalogue)

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, map[string]string{
		"image": verifiedImage(t), "command": "sleep infinity", "memory": engineMemoryLimit})
	if err != nil {
		t.Fatalf("create idle sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "barman", Path: catalogue},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("the catalogue dates itself in backup.info; created_at should not be null")
	}

	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-U", "postgres", "-h", "127.0.0.1", "-tA", "-c",
		"SELECT (SELECT count(*) FROM events) || '/' || (SELECT count(*) FROM later) || '/' || pg_is_in_recovery()"}})
	if err != nil {
		t.Fatalf("query the restored cluster: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != "500/100/false" {
		t.Errorf("events/later/in_recovery = %s, want 500/100/f — the second batch comes from "+
			"archived WAL, and the cluster must be promoted rather than left in recovery", got)
	}
	if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// TestBarmanPITREndToEnd demands the instant captured between the two
// batches: the restored database must hold the first and not the second,
// even though the second's WAL is in the archive.
func TestBarmanPITREndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	seedImage := buildBarmanImage(t, ctx)
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-postgres"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	catalogue := filepath.Join(t.TempDir(), "demo")
	between := makeBarmanServer(t, ctx, seedImage, catalogue)

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, map[string]string{
		"image": verifiedImage(t), "command": "sleep infinity", "memory": engineMemoryLimit})
	if err != nil {
		t.Fatalf("create idle sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("postgres", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "barman", Path: catalogue},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		PITR:    &adapter.PITR{TargetTime: between.Format(time.RFC3339Nano)},
	}, sbx); err != nil {
		t.Fatalf("provision to a point in time: %v", err)
	}

	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"psql", "-U", "postgres", "-h", "127.0.0.1", "-tA", "-c",
		"SELECT (SELECT count(*) FROM events) || '/' || (to_regclass('later') IS NULL) || '/' || pg_is_in_recovery()"}})
	if err != nil {
		t.Fatalf("query the restored cluster: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != "500/true/false" {
		t.Errorf("events/later-absent/in_recovery = %s, want 500/true/false: the first batch, no later "+
			"table, and a promoted cluster — recovery stopped at the requested instant", got)
	}
}
