//go:build integration

package main_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/capabilities"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

// engineMemoryLimit caps every container this suite starts, so a developer
// machine running the whole suite stays usable.
const engineMemoryLimit = "512m"

// verifiedEngine is what this run builds its sandbox from: the manifest's
// baseline pair, or the version-matrix job's PROBAVI_IT_ENGINE_VERSION
// when it names one the manifest already lists. H2 ships as a jar rather
// than as an image, so a run is a base plus an artifact and the version
// resolves the pair (docs/engine-versions.md §1, §2).
func verifiedEngine(t *testing.T) (base, artifact string) {
	t.Helper()
	m, err := capabilities.LoadAdapterManifest(".")
	if err != nil {
		t.Fatalf("load adapter manifest: %v", err)
	}
	base, artifact, err = m.SandboxEngine(os.Getenv("PROBAVI_IT_ENGINE_VERSION"))
	if err != nil {
		t.Fatalf("adapter manifest: %v", err)
	}
	if artifact == "" {
		t.Fatalf("manifest entry names no engine_artifact; H2 is not inside any image")
	}
	return base, artifact
}

// wrapperImage builds (once per artifact, cached afterwards by docker) the
// image a drill sandbox actually runs: the JRE base with the H2 jar at the
// path this adapter contracts for — the exact two lines the adapter README
// documents. There is no H2 image to use instead: the only versioned
// community one is two release lines behind, on Java 11, and carries
// neither grep nor sed, which the check runner needs (measured).
func wrapperImage(t *testing.T, ctx context.Context, base, artifact string) string {
	t.Helper()
	version := artifact[strings.LastIndex(artifact, "/")+1:]
	tag := "probavi-it-h2:" + strings.TrimSuffix(strings.TrimPrefix(version, "h2-"), ".jar")
	dir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM %s\nADD %s /opt/h2/h2.jar\n", base, artifact)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build wrapper image: %v: %s", err, out)
	}
	return tag
}

func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-h2"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}

// shell runs one H2 Shell invocation inside a sandbox and returns its exit
// code and combined output. It is the seeding tool, deliberately not the
// adapter's check script: fixtures are built the way an operator builds
// them, with the engine's own front end.
func shell(t *testing.T, ctx context.Context, sbx *docker.Sandbox, url, sql string) (int, string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"java", "-cp", "/opt/h2/h2.jar", "org.h2.tools.Shell",
		"-url", url, "-user", "sa", "-password", "", "-sql", sql}})
	if err != nil {
		t.Fatalf("exec shell: %v", err)
	}
	return out.ExitCode, string(out.Stdout) + string(out.Stderr)
}

// seedSQL builds a database with enough in it that a lost tail would show:
// a thousand rows, an index, and a second table.
const seedSQL = `CREATE TABLE orders(id INT PRIMARY KEY, customer VARCHAR(64) NOT NULL, total DECIMAL(10,2) NOT NULL);
INSERT INTO orders SELECT X, 'cust-'||X, X*1.5 FROM SYSTEM_RANGE(1,1000);
CREATE INDEX orders_customer_ix ON orders(customer);
CREATE TABLE audit(id INT PRIMARY KEY, note VARCHAR(64));
INSERT INTO audit VALUES (1,'seeded');`

// makeFixtures seeds a real database in a throwaway sandbox and extracts
// both artifact forms the way the README tells operators to produce them:
// the archive from H2's own BACKUP TO, the file from a database that has
// been closed.
func makeFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, image, dbDest, archiveDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	if code, out := shell(t, ctx, seed, "jdbc:h2:file:/tmp/prod", seedSQL+"BACKUP TO '/tmp/prod.zip';"); code != 0 {
		t.Fatalf("seed database: exit %d: %s", code, out)
	}
	// The Shell exits after the statements, so the database is closed by
	// the time the file is copied — which is the only way a plain file copy
	// is a sound backup (see the adapter README).
	for _, f := range []struct{ in, out string }{
		{"/tmp/prod.mv.db", dbDest}, {"/tmp/prod.zip", archiveDest},
	} {
		if out, err := exec.CommandContext(ctx, "docker", "cp",
			seed.ID()+":"+f.in, f.out).CombinedOutput(); err != nil {
			t.Fatalf("extract %s: %v: %s", f.in, err, out)
		}
	}
}

// assertCheck runs one SQL check through the probe-declared runner —
// exactly how internal/checks runs checks without engine knowledge — and
// asserts the undecorated answer. H2's own tool prints a header and a
// timing trailer and exits 0 on error, so this is also the assertion that
// the adapter's runner script is doing its job.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText, want string) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q",
			checkText, got, out.ExitCode, out.Stderr, want)
	}
}

// TestEndToEndRestoreDrill proves both artifact forms through the whole
// stack: the docker provider, the core-side protocol client and this
// adapter, as separate processes.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	base, artifact := verifiedEngine(t)
	image := wrapperImage(t, ctx, base, artifact)
	provider := docker.New(nil)

	dir := t.TempDir()
	dbFixture := filepath.Join(dir, "prod.mv.db")
	archiveFixture := filepath.Join(dir, "prod.zip")
	makeFixtures(t, ctx, provider, image, dbFixture, archiveFixture)

	runner, err := adapter.New("h2", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "h2" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	for _, tc := range []struct{ kind, path string }{
		{"h2_db", dbFixture},
		{"h2_backup", archiveFixture},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			sbx, err := provider.Create(ctx, sandboxParams(image))
			if err != nil {
				t.Fatalf("create drill sandbox: %v", err)
			}
			defer destroy(t, sbx)

			res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
				Source:  adapter.ProvisionSource{Kind: tc.kind, Path: tc.path},
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
			if res.SourceIdentity.CreatedAt != nil {
				t.Errorf("created_at = %v, want null — nothing in the artifact dates it",
					*res.SourceIdentity.CreatedAt)
			}

			health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
			if err != nil {
				t.Fatalf("healthcheck: %v", err)
			}
			if !health.Healthy {
				t.Fatalf("healthcheck = %+v, want healthy", health)
			}

			// The whole backup came back, index and second table included.
			assertCheck(t, ctx, sbx, probe, res, "SELECT COUNT(*) FROM orders", "1000")
			assertCheck(t, ctx, sbx, probe, res, "SELECT note FROM audit WHERE id = 1", "seeded")
			assertCheck(t, ctx, sbx, probe, res,
				"SELECT COUNT(*) FROM INFORMATION_SCHEMA.INDEXES WHERE INDEX_NAME = 'ORDERS_CUSTOMER_IX'", "1")
		})
	}
}

// TestTheRunnerFailsAChecksThatH2ReportsWithExitZero is the measured
// reason the declared runner is a script rather than H2's own tool. The
// Shell prints "Error: ..." on stdout and returns 0, so a runner built on
// it would report every failing check as passing — the quietest way a
// drill can lie. This test asks the real engine, through the real
// template, and requires a non-zero exit.
func TestTheRunnerFailsAChecksThatH2ReportsWithExitZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	base, artifact := verifiedEngine(t)
	image := wrapperImage(t, ctx, base, artifact)
	provider := docker.New(nil)

	dir := t.TempDir()
	dbFixture := filepath.Join(dir, "prod.mv.db")
	makeFixtures(t, ctx, provider, image, dbFixture, filepath.Join(dir, "prod.zip"))

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("h2", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "h2_db", Path: dbFixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// First: the bare tool really does exit 0 on this statement. If H2 ever
	// fixes that, this assertion fails and the script can be reconsidered
	// — which is the point of measuring it here rather than remembering it.
	bare, out := shell(t, ctx, sbx, "jdbc:h2:file:"+res.Connection.Database+";IFEXISTS=TRUE",
		"SELECT * FROM no_such_table")
	if bare != 0 || !strings.Contains(out, "Error:") {
		t.Fatalf("H2's Shell exited %d for a bad statement (%s); the runner script's premise moved", bare, out)
	}

	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", "SELECT * FROM no_such_table"))
	}
	got, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if got.ExitCode == 0 {
		t.Fatalf("the declared runner exited 0 for a failing statement: stdout %q", got.Stdout)
	}
	if strings.TrimSpace(string(got.Stdout)) != "" {
		t.Errorf("a failed check printed %q on stdout; the diagnostic belongs on stderr", got.Stdout)
	}
}

// TestABrokenArtifactIsRefusedAndNothingIsInvented covers the two ways a
// restore can go wrong here, and the one that would otherwise pass.
//
// A truncated .mv.db is the engine's own refusal. The second case is the
// one worth the container: pointed at a path holding no database, H2
// creates one and answers queries against it (measured), so an adapter
// that forgot IFEXISTS=TRUE would drill a database it invented and report
// every check against it as green.
func TestABrokenArtifactIsRefusedAndNothingIsInvented(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	base, artifact := verifiedEngine(t)
	image := wrapperImage(t, ctx, base, artifact)
	provider := docker.New(nil)

	dir := t.TempDir()
	whole := filepath.Join(dir, "prod.mv.db")
	makeFixtures(t, ctx, provider, image, whole, filepath.Join(dir, "prod.zip"))

	body, err := os.ReadFile(whole)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	truncated := filepath.Join(dir, "half.mv.db")
	if err := os.WriteFile(truncated, body[:len(body)/2], 0o600); err != nil {
		t.Fatalf("write truncated fixture: %v", err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("h2", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "h2_db", Path: truncated},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err == nil {
		t.Fatal("provision accepted a truncated database file")
	}

	// The invented-database case, asked of the engine directly: a path with
	// nothing at it answers queries unless IFEXISTS=TRUE is set.
	code, _ := shell(t, ctx, sbx, "jdbc:h2:file:/tmp/never-restored", "SELECT 1")
	if code != 0 {
		t.Fatal("H2 no longer creates a database on demand; the IFEXISTS premise moved")
	}
	code, out := shell(t, ctx, sbx, "jdbc:h2:file:/tmp/never-restored-2;IFEXISTS=TRUE", "SELECT 1")
	if code == 0 {
		t.Fatalf("IFEXISTS=TRUE did not refuse an absent database: %s", out)
	}
}

// TestABackupsOwnTriggerCannotRunInTheSandbox closes this engine's line of
// issue #166. An H2 trigger runs a Java class, and the sandbox's classpath
// is the H2 jar alone — so a trigger travelling inside a backup has
// nothing to load and cannot subtract from what the drill sees. The guard
// is structural, which is exactly why it needs a test: the day the wrapper
// grows a classpath, this is what notices.
//
// The assertion is from both sides: the rows survive, and the check
// against the trapped table fails rather than quietly returning a
// shortened answer.
func TestABackupsOwnTriggerCannotRunInTheSandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	base, artifact := verifiedEngine(t)
	image := wrapperImage(t, ctx, base, artifact)
	provider := docker.New(nil)

	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	if code, out := shell(t, ctx, seed, "jdbc:h2:file:/tmp/trap", seedSQL+
		`CREATE FORCE TRIGGER purge BEFORE SELECT ON orders CALL "com.example.Purge";`); code != 0 {
		t.Fatalf("seed trapped database: exit %d: %s", code, out)
	}
	fixture := filepath.Join(t.TempDir(), "trap.mv.db")
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/trap.mv.db", fixture).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	destroy(t, seed)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("h2", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "h2_db", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The untrapped table is untouched, so the trigger did not run.
	assertCheck(t, ctx, sbx, probe, res, "SELECT note FROM audit WHERE id = 1", "seeded")

	// And the trapped one refuses rather than answering short.
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", "SELECT COUNT(*) FROM orders"))
	}
	got, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if got.ExitCode == 0 {
		t.Fatalf("a table whose trigger class is absent answered %q; the drill must refuse to "+
			"prove a database it cannot fully load", got.Stdout)
	}
}
