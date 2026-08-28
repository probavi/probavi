//go:build integration

package main_test

import (
	"context"
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

// verifiedImage is the image this run restores with: the manifest's
// baseline, or the version-matrix job's PROBAVI_IT_IMAGE when it names one
// the manifest already lists (docs/engine-versions.md §2).
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

// sandboxParams idles the container deliberately. The official image
// starts a Firebird server, and this adapter needs none: isql against a
// plain file path uses the embedded engine. Overriding the command with a
// sleep is how the suite proves that claim on every run rather than
// asserting it in a comment.
func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "512m"}
}

const (
	seedRows    = 3
	purgedBelow = "50"
)

// makeFixture seeds a real database in a container of the verified image
// and extracts a genuine gbak backup — produced the way the README tells
// operators to produce one.
//
// The database carries an ON CONNECT trigger that deletes most of its own
// rows, because that is the hazard this engine's drills have to survive
// (see TestABackupsOwnTriggerCannotEmptyTheDrill). The backup is taken with
// -nodbtriggers so the fixture-making connection does not fire it first.
func makeFixture(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
printf "%s\n" \
  "CREATE DATABASE '/tmp/seed.fdb';" \
  "CREATE TABLE ORDERS (ID INTEGER NOT NULL PRIMARY KEY, TOTAL NUMERIC(9,2));" \
  "INSERT INTO ORDERS VALUES (1, 10.50);" \
  "INSERT INTO ORDERS VALUES (2, 99.00);" \
  "INSERT INTO ORDERS VALUES (3, 7.25);" \
  "COMMIT;" \
  "SET TERM ^ ;" \
  "CREATE TRIGGER PURGE ON CONNECT POSITION 0 AS BEGIN DELETE FROM ORDERS WHERE TOTAL < ` + purgedBelow + `; END^" \
  "SET TERM ; ^" \
  "COMMIT;" | isql -q
gbak -b -nodbtriggers /tmp/seed.fdb /tmp/fixture.fbk`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/fixture.fbk", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-firebird"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// assertCheck runs one SQL check through the probe-declared runner —
// exactly how internal/checks runs checks without engine knowledge.
//
// The comparison trims, and the adapter README says why: isql has no
// delimiter setting, so a value arrives padded to its column width. The
// core trims the runner's whole output for the same reason, so this is
// what a real check sees.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, database, checkText, want string) {
	t.Helper()
	out, exitCode := runCheck(t, ctx, sbx, probe, database, checkText)
	if exitCode != 0 || out != want {
		t.Fatalf("check %q = %q (exit %d), want %q", checkText, out, exitCode, want)
	}
}

func runCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, database, checkText string) (string, int) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{database}}", database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	return strings.TrimSpace(string(out.Stdout)), out.ExitCode
}

// provisionFixture runs the shared prologue: build, seed, sandbox, probe,
// provision. It returns everything the assertions need.
func provisionFixture(t *testing.T, ctx context.Context) (*docker.Sandbox, *adapter.ProbeResult,
	*adapter.Runner, *adapter.ProvisionResult, string) {
	t.Helper()
	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "nightly.fbk")
	makeFixture(t, ctx, provider, image, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	t.Cleanup(func() { destroy(t, sbx) })

	runner, err := adapter.New("firebird", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind: "firebird_gbak", Path: fixture,
			Params: map[string]string{"backup_timezone": "UTC"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	return sbx, probe, runner, res, fixture
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine gbak artifact and validate the
// restored rows through the probe-declared runner.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sbx, probe, runner, res, _ := provisionFixture(t, ctx)

	if probe.Name != "firebird" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}
	// gbak stamps the artifact, and a zone was declared, so the record can
	// date the backup from the engine's own clock rather than an mtime.
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the instant gbak stamped into the header")
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT count(*) FROM ORDERS", "3")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		"SELECT count(*) FROM ORDERS WHERE TOTAL < 0", "0")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestABackupsOwnTriggerCannotEmptyTheDrill closes this engine's line of
// issue #166, and proves it from both sides.
//
// The fixture's backup carries an ON CONNECT trigger that deletes every
// row below a threshold — two of its three. Measured before any of this
// was written: gbak restores all three and the first ordinary connection
// deletes two, irreversibly.
//
// So the first assertion is that the drill's own checks, run through the
// probe-declared runner, still count three. The second is the control that
// gives the first its meaning: one connection without the suspension, and
// the rows are gone. Without it a passing test could mean the trigger
// never fired at all, and the day the fixture stops carrying one, this
// test would keep passing while proving nothing.
func TestABackupsOwnTriggerCannotEmptyTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sbx, probe, _, res, _ := provisionFixture(t, ctx)
	database := res.Connection.Database

	// The drill's view: every row the backup held.
	assertCheck(t, ctx, sbx, probe, database, "SELECT count(*) FROM ORDERS", "3")
	// Reading twice must not change it either — the runner opens a fresh
	// connection per check, and each one is a chance to fire the trigger.
	assertCheck(t, ctx, sbx, probe, database, "SELECT count(*) FROM ORDERS", "3")

	// The control: one ordinary connection, and the trigger does what the
	// operator wrote it to do.
	naive, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c",
		`printf 'SET HEADING OFF;\nSELECT count(*) FROM ORDERS;\n' | isql -q -b "$1"`, "sh", database}})
	if err != nil {
		t.Fatalf("control exec: %v", err)
	}
	if naive.ExitCode != 0 {
		t.Fatalf("control connection failed: %s", naive.Stderr)
	}
	if got := strings.TrimSpace(string(naive.Stdout)); got == "3" {
		t.Fatalf("the control connection still counted %s rows: the fixture no longer carries a live "+
			"ON CONNECT trigger, so this test proves nothing", got)
	}

	// And the suspension is a suspension, not a rewrite: the trigger is
	// still there and still enabled, so a check reading the catalogue sees
	// what the operator declared.
	assertCheck(t, ctx, sbx, probe, database,
		"SELECT count(*) FROM RDB$TRIGGERS WHERE RDB$TRIGGER_NAME = 'PURGE' AND "+
			"COALESCE(RDB$TRIGGER_INACTIVE, 0) = 0", "1")
}

// TestACorruptBackupIsRefusedAndLeavesNoDatabase closes the false green
// this adapter exists to refuse.
//
// Measured: gbak exits non-zero on a truncated artifact and leaves behind
// a database that opens and answers queries holding every row. Anything
// judging by whether the restored database responds would call a broken
// backup proven, so the drill must fail *and* the file must be gone.
func TestACorruptBackupIsRefusedAndLeavesNoDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "nightly.fbk")
	makeFixture(t, ctx, provider, image, fixture)

	whole, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.fbk")
	if err := os.WriteFile(truncated, whole[:len(whole)*2/3], 0o600); err != nil {
		t.Fatalf("write truncated: %v", err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("firebird", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, perr := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "firebird_gbak", Path: truncated},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if perr == nil {
		t.Fatal("provision accepted a truncated backup")
	}
	if !strings.Contains(perr.Error(), "source_corrupt") {
		t.Errorf("error = %v, want source_corrupt", perr)
	}

	// The database gbak left behind must not have survived: it opens, and
	// it would answer.
	leftover := sbx.ScratchDir() + "/probavi-firebird/restored.fdb"
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"test", "-e", leftover}})
	if err != nil {
		t.Fatalf("stat exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Error("a failed restore left a database behind, which anything downstream would read as success")
	}
}
