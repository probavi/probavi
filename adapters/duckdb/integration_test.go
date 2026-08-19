//go:build integration

package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// verifiedImage is the official image this run's binary comes from: the
// manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE when
// it names one the manifest already lists (docs/engine-versions.md §2).
func verifiedImage(t *testing.T) string {
	t.Helper()
	m := manifest(t)
	image, err := m.SandboxImage(os.Getenv("PROBAVI_IT_IMAGE"))
	if err != nil {
		t.Fatalf("adapter manifest: %v", err)
	}
	return image
}

func manifest(t *testing.T) *capabilities.AdapterManifest {
	t.Helper()
	m, err := capabilities.LoadAdapterManifest(".")
	if err != nil {
		t.Fatalf("load adapter manifest: %v", err)
	}
	return m
}

// wrapperImage builds (once per base, cached afterwards by docker) the
// image a drill sandbox actually runs: the official image's binary on a
// glibc base with a shell — the exact recipe the adapter README
// documents. The official images alone cannot idle (measured), and the
// binary does not start on musl (measured).
func wrapperImage(t *testing.T, ctx context.Context, base string) string {
	t.Helper()
	tag := "probavi-it-duckdb:" + strings.ReplaceAll(base[strings.LastIndex(base, ":")+1:], "/", "-")
	dir := t.TempDir()
	dockerfile := fmt.Sprintf(`FROM %s AS duckdb
FROM debian:12-slim
COPY --from=duckdb /duckdb /usr/local/bin/duckdb
`, base)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build wrapper image: %v: %s", err, out)
	}
	return tag
}

func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "512m"}
}

// makeFixtures seeds a real database in a wrapper container and extracts
// a copy of the cleanly closed database file and an EXPORT DATABASE
// directory — the artifact forms this adapter restores, produced the way
// the README tells operators to produce them.
func makeFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, image, marker, dbDest, exportDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
duckdb /tmp/seed.duckdb "CREATE TABLE t(id INTEGER PRIMARY KEY, v VARCHAR);
INSERT INTO t SELECT r, 'row'||r FROM range(1,501) x(r);
CREATE TABLE meta(k VARCHAR PRIMARY KEY, v VARCHAR);
INSERT INTO meta VALUES('origin','` + marker + `');
EXPORT DATABASE '/tmp/exp';"`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	for src, dest := range map[string]string{"/tmp/seed.duckdb": dbDest, "/tmp/exp": exportDest} {
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
// as separate processes — restore a copy of a cleanly closed database and
// validate the restored rows through the probe-declared runner.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "nightly.duckdb")
	makeFixtures(t, ctx, provider, image, "restored-ok", fixture, "")

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("duckdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "duckdb" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "duckdb_db", Path: fixture},
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

// TestExportDrillImportsTheDirectory proves the export kind end to end: a
// genuine EXPORT DATABASE directory transfers file by file and rebuilds
// the database inside the sandbox.
func TestExportDrillImportsTheDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	exportDir := filepath.Join(t.TempDir(), "nightly-export")
	makeFixtures(t, ctx, provider, image, "export-ok", "", exportDir)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("duckdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "duckdb_export", Path: exportDir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT count(*) FROM t;", "500")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT v FROM meta WHERE k='origin';", "export-ok")
}

// TestCorruptDatabaseVerdict proves a broken backup yields the right
// verdict through the whole stack: duckdb inside the sandbox is the
// authority, and its refusal must surface as a claim about the backup.
func TestCorruptDatabaseVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(wrapperImage(t, ctx, verifiedImage(t))))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	corrupt := filepath.Join(t.TempDir(), "corrupt.duckdb")
	if err := os.WriteFile(corrupt, []byte("this is not a DuckDB database, only long enough text\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner, err := adapter.New("duckdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "duckdb_db", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestLiveCopyIsRefusedByName drives the fence end to end: a database
// with a non-empty .wal sibling — the copy-of-a-live-database shape — is
// refused before a byte reaches the sandbox, with the fix in the message.
func TestLiveCopyIsRefusedByName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	dir := t.TempDir()
	fixture := filepath.Join(dir, "live.duckdb")
	makeFixtures(t, ctx, provider, image, "live", fixture, "")
	if err := os.WriteFile(fixture+".wal", []byte("frames a live connection had not checkpointed"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("duckdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "duckdb_db", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "unsupported_source" ||
		!strings.Contains(aerr.Message, "EXPORT DATABASE") {
		t.Fatalf("provision error = %v, want unsupported_source teaching the fix", err)
	}
}

// TestNewerStorageFormatIsRefusedNamingBothSides drives the version fence
// end to end: an artifact written by the newest verified engine in its
// newest storage format, drilled against the baseline engine, is refused
// as a config pairing — naming the storage format from the file's own
// header and the engine that cannot read it.
func TestNewerStorageFormatIsRefusedNamingBothSides(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	m := manifest(t)
	baseline, err := m.BaselineImage()
	if err != nil {
		t.Fatalf("baseline image: %v", err)
	}
	newest := m.Verified[len(m.Verified)-1].Image
	if newest == baseline {
		t.Skip("manifest lists a single engine line; no newer storage format to refuse")
	}

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	// The artifact: written by the newest engine, deliberately in the
	// newest format it knows rather than the compatible default.
	seed, err := provider.Create(ctx, sandboxParams(wrapperImage(t, ctx, newest)))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c",
		`duckdb /tmp/probe.duckdb "ATTACH '/tmp/future.duckdb' (STORAGE_VERSION 'latest');
CREATE TABLE future.t(i INTEGER); INSERT INTO future.t VALUES(1); DETACH future;"`}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("seed future artifact: %v exit %d: %s", err, res.ExitCode, res.Stderr)
	}
	fixture := filepath.Join(t.TempDir(), "future.duckdb")
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/future.duckdb", fixture).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}

	sbx, err := provider.Create(ctx, sandboxParams(wrapperImage(t, ctx, baseline)))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("duckdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "duckdb_db", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "invalid_request" ||
		!strings.Contains(aerr.Message, "storage format version") {
		t.Fatalf("provision error = %v, want invalid_request naming the storage format", err)
	}
}

// TestDirectoryDrillPicksTheNewest proves the directory kind end to end
// with the ranking this format allows — newest by file time — and that
// sidecar files are never candidates.
func TestDirectoryDrillPicksTheNewest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	dir := t.TempDir()
	old := filepath.Join(dir, "monday.duckdb")
	makeFixtures(t, ctx, provider, image, "stale", old, "")
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	newest := filepath.Join(dir, "tuesday.duckdb")
	makeFixtures(t, ctx, provider, image, "fresh", newest, "")
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte("sha256 sidecar\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("duckdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "duckdb_db_dir", Path: dir},
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
		filepath.Join(binDir, "probavi-adapter-duckdb"), ".").CombinedOutput(); err != nil {
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
// DuckDB is a library, not a server: the drill sandbox runs no engine
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
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "nightly.duckdb")
	makeFixtures(t, ctx, provider, image, "restored-ok", fixture, "")

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("duckdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "duckdb_db", Path: fixture},
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
		Argv: []string{"sh", "-c", "ps -eo comm 2>/dev/null | grep -c duckdb || true"}})
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	count, convErr := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	if convErr != nil {
		t.Fatalf("process count = %q: %v", res.Stdout, convErr)
	}
	return count
}
