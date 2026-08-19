//go:build integration

package main_test

import (
	"context"
	"encoding/json"
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

// verifiedImage is the image this run restores with: the manifest's
// baseline, or the version-matrix job's PROBAVI_IT_IMAGE when it names
// one the manifest already lists (docs/engine-versions.md §2). There is
// no official SQLite image; the listed community image carries a POSIX
// shell and the sqlite3 CLI, which is all this adapter asks of an image.
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

func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "256m"}
}

// makeFixtures seeds a real database in a container of the verified image
// and extracts a genuine `.backup` artifact and a genuine `.dump` — the
// two formats this adapter restores, produced the way the README tells
// operators to produce them.
func makeFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, image, marker, dbDest, dumpDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
sqlite3 /tmp/seed.db "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT);
WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<500)
INSERT INTO t(v) SELECT 'row'||x FROM c;
CREATE TABLE meta(k TEXT PRIMARY KEY, v TEXT);
INSERT INTO meta VALUES('origin','` + marker + `');"
sqlite3 /tmp/seed.db ".backup /tmp/fixture.db"
sqlite3 /tmp/seed.db .dump > /tmp/fixture.sql`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	for src, dest := range map[string]string{"/tmp/fixture.db": dbDest, "/tmp/fixture.sql": dumpDest} {
		if dest == "" {
			continue
		}
		if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":"+src, dest).CombinedOutput(); err != nil {
			t.Fatalf("extract fixture: %v: %s", err, out)
		}
	}
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine `.backup` artifact and
// validate the restored rows through the probe-declared runner.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "nightly.db")
	makeFixtures(t, ctx, provider, image, "restored-ok", fixture, "")

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("sqlite", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "sqlite" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "sqlite_db", Path: fixture},
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
		t.Errorf("created_at = %v, want null — nothing in the artifact dates it", *res.SourceIdentity.CreatedAt)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Plain SQL through the probe-declared template, exactly as
	// internal/checks would run the generating built-ins.
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT count(*) FROM t;", "500")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT v FROM meta WHERE k='origin';", "restored-ok")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT id, v FROM t WHERE id = 1;", "1\trow1")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestDumpDrillReplaysSQLText proves the dump kind end to end: a genuine
// `.dump` passes the host-side completeness gate and rebuilds the
// database inside the sandbox.
func TestDumpDrillReplaysSQLText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "nightly.sql")
	makeFixtures(t, ctx, provider, image, "dump-ok", "", fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("sqlite", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "sqlite_dump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT count(*) FROM t;", "500")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT v FROM meta WHERE k='origin';", "dump-ok")
}

// TestCorruptDatabaseVerdict proves a broken backup yields the right
// verdict through the whole stack: sqlite3 inside the sandbox is the
// authority, and its refusal must surface as a claim about the backup.
func TestCorruptDatabaseVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(verifiedImage(t)))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("this is not an SQLite database, only long enough text\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner, err := adapter.New("sqlite", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "sqlite_db", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestLiveCopyIsRefusedByName drives the fence end to end: a database
// with a non-empty -wal sibling — the copy-of-a-live-database shape — is
// refused before a byte reaches the sandbox, with the message that
// teaches the fix.
func TestLiveCopyIsRefusedByName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	fixture := filepath.Join(dir, "live.db")
	makeFixtures(t, ctx, provider, image, "live", fixture, "")
	if err := os.WriteFile(fixture+"-wal", []byte("frames a live connection had not checkpointed"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("sqlite", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "sqlite_db", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "unsupported_source" ||
		!strings.Contains(aerr.Message, ".backup") {
		t.Fatalf("provision error = %v, want unsupported_source teaching .backup / VACUUM INTO", err)
	}
}

// TestDirectoryDrillPicksTheNewest proves the directory kind end to end
// with the ranking this format allows — newest by file time — and that
// sidecar files are never candidates.
func TestDirectoryDrillPicksTheNewest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	old := filepath.Join(dir, "monday.db")
	makeFixtures(t, ctx, provider, image, "stale", old, "")
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	newest := filepath.Join(dir, "tuesday.db")
	makeFixtures(t, ctx, provider, image, "fresh", newest, "")
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte("sha256 sidecar\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("sqlite", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "sqlite_db_dir", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT v FROM meta WHERE k='origin';", "fresh")
}

// assertCheck runs one SQL check through the probe-declared runner —
// exactly how internal/checks runs checks without engine knowledge: the
// core substitutes {{database}} from the connection provision returned,
// and {{sql}} with the check text — and asserts the last line of output.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, database, checkText, want string) {
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
	lines := strings.Split(strings.TrimRight(string(out.Stdout), "\n"), "\n")
	got := lines[len(lines)-1]
	if out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q",
			checkText, got, out.ExitCode, out.Stderr, want)
	}
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-sqlite"), ".").CombinedOutput(); err != nil {
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

// TestNothingTouchesTheArtifactBetweenChecks closes this engine's line of
// issue #166.
//
// SQLite is a library, not a server: the drill sandbox runs no engine
// process between checks, so nothing can expire, compact or purge on its
// own. That is a structural answer rather than a measured one, which is
// exactly why it deserves a test — the day this engine grows a background
// task, this is what notices.
//
// The assertion is the restored file's own bytes: a row count could be
// unchanged while something rewrote the file underneath it.
func TestNothingTouchesTheArtifactBetweenChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "nightly.db")
	makeFixtures(t, ctx, provider, image, "restored-ok", fixture, "")

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("sqlite", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "sqlite_db", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	dbPath := restoredPath(t, res.State)
	before := sandboxSum(t, ctx, sbx, dbPath)
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while watching the restored database")
	case <-time.After(20 * time.Second):
	}
	if after := sandboxSum(t, ctx, sbx, dbPath); after != before {
		t.Errorf("the restored database changed while nobody queried it: %s then %s", before, after)
	}
	if procs := engineProcesses(t, ctx, sbx); procs != 0 {
		t.Errorf("%d engine processes run between checks — this engine had none", procs)
	}
}

// restoredPath reads where the adapter left the restored database.
func restoredPath(t *testing.T, state json.RawMessage) string {
	t.Helper()
	got := struct {
		DBPath string `json:"db_path"`
	}{}
	if err := json.Unmarshal(state, &got); err != nil {
		t.Fatalf("provision state: %v", err)
	}
	if got.DBPath == "" {
		t.Fatal("provision state names no database file")
	}
	return got.DBPath
}

// sandboxSum hashes a file inside the sandbox.
func sandboxSum(t *testing.T, ctx context.Context, sbx *docker.Sandbox, path string) string {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sha256sum", path}})
	if err != nil {
		t.Fatalf("sha256sum %s: %v", path, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("sha256sum %s: exit %d: %s", path, res.ExitCode, res.Stderr)
	}
	fields := strings.Fields(string(res.Stdout))
	if len(fields) == 0 || len(fields[0]) != 64 {
		t.Fatalf("sha256sum %s = %q", path, res.Stdout)
	}
	return fields[0]
}

// engineProcesses counts what runs in the sandbox that could touch the
// artifact. A library engine leaves nothing behind between checks.
func engineProcesses(t *testing.T, ctx context.Context, sbx *docker.Sandbox) int {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", "ps -eo comm 2>/dev/null | grep -c sqlite3 || true"}})
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	count, convErr := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	if convErr != nil {
		t.Fatalf("process count = %q: %v", res.Stdout, convErr)
	}
	return count
}
