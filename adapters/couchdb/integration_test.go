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

// engineMemoryLimit caps every container this suite starts.
const engineMemoryLimit = "512m"

// sandboxPassword is the documented public constant the adapter starts the
// engine with. The suite needs it to seed fixtures through the same
// account; it protects nothing reachable (see the adapter README).
const sandboxPassword = "probavi-drill-sandbox"

// verifiedImage is the engine image this run restores from: the manifest's
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

// sandboxParams start the sandbox idle, which this adapter requires: the
// engine must not have read its data directory before the artifact is in
// place (see the adapter README).
func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-couchdb"), ".").CombinedOutput(); err != nil {
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

// run executes one shell command inside a sandbox and returns its exit
// code with the combined output.
func run(t *testing.T, ctx context.Context, sbx *docker.Sandbox, script string, args ...string) (int, string) {
	t.Helper()
	argv := append([]string{"sh", "-c", script, "sh"}, args...)
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return out.ExitCode, string(out.Stdout) + string(out.Stderr)
}

// startEngine brings CouchDB up in a sandbox the way the adapter does, so
// fixtures are seeded through the same door a drill uses.
func startEngine(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", `nohup /docker-entrypoint.sh /opt/couchdb/bin/couchdb >/tmp/c.log 2>&1 &
i=0; while [ $i -lt 120 ]; do curl -sf --user "admin:$COUCHDB_PASSWORD" http://127.0.0.1:5984/ >/dev/null 2>&1 && exit 0; i=$((i+1)); sleep 1; done
tail -20 /tmp/c.log >&2; exit 1`},
		Env: map[string]string{"COUCHDB_USER": "admin", "COUCHDB_PASSWORD": sandboxPassword},
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("start engine: %v (exit %d): %s", err, out.ExitCode, out.Stderr)
	}
}

// seedScript creates a database and fills it with documents through the
// engine's own API.
const seedScript = `set -u
db=$1; n=$2
curl -sf -X PUT --user "admin:$COUCHDB_PASSWORD" "http://127.0.0.1:5984/$db" >/dev/null
{ printf '{"docs":['
  i=1
  while [ $i -le "$n" ]; do
    [ $i -gt 1 ] && printf ','
    printf '{"_id":"order-%04d","customer":"cust-%d","total":%d.50}' "$i" "$i" "$i"
    i=$((i+1))
  done
  printf ']}'
} > /tmp/seed.json
curl -sf -X POST -H 'Content-Type: application/json' -d @/tmp/seed.json \
  --user "admin:$COUCHDB_PASSWORD" "http://127.0.0.1:5984/$db/_bulk_docs" >/dev/null`

func seed(t *testing.T, ctx context.Context, sbx *docker.Sandbox, db string, docs int) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", seedScript, "sh", db, fmt.Sprint(docs)},
		Env:  map[string]string{"COUCHDB_PASSWORD": sandboxPassword},
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("seed %s: %v (exit %d): %s", db, err, out.ExitCode, out.Stderr)
	}
}

// makeDataTar seeds a database and extracts a tar of the engine's data
// directory — the artifact an operator takes by copying the files.
func makeDataTar(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest string, docs int) {
	t.Helper()
	seedBox, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seedBox)
	startEngine(t, ctx, seedBox)
	seed(t, ctx, seedBox, "orders", docs)
	if code, out := run(t, ctx, seedBox, `cd /opt/couchdb/data && tar cf /tmp/data.tar .`); code != 0 {
		t.Fatalf("tar the data directory: %s", out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seedBox.ID()+":/tmp/data.tar", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// backupArtifact writes a couchbackup-shaped file. The format is the one
// measured from couchbackup 2.11.19: a header line naming the tool, then
// one JSON array of documents per line, each already _bulk_docs shaped.
// The suite builds it rather than running the tool because couchbackup is
// a Node program and the engine image carries no Node — what this test
// proves is that the restore reads the format, which is the adapter's
// half of the contract.
func backupArtifact(t *testing.T, path string, batches, perBatch int) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"name":"@cloudant/couchbackup","version":"2.11.19","mode":"full","attachments":false}` + "\n")
	id := 1
	for range batches {
		b.WriteString("[")
		for j := range perBatch {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"_id":"order-%04d","_rev":"1-%032x","customer":"cust-%d","total":%d.50}`, id, id, id, id)
			id++
		}
		b.WriteString("]\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write backup fixture: %v", err)
	}
}

// assertCheck runs one check through the probe-declared runner — exactly
// how internal/checks runs checks without engine knowledge — and asserts
// the answer.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText, want string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: runnerArgv(probe, res, checkText), Env: probe.SQLRunner.Env})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q", checkText, got, out.ExitCode, out.Stderr, want)
	}
}

// assertCheckContains is assertCheck for the answers CouchDB states no
// number for, where the runner passes the body through.
func assertCheckContains(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText, want string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: runnerArgv(probe, res, checkText), Env: probe.SQLRunner.Env})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if out.ExitCode != 0 || !strings.Contains(string(out.Stdout), want) {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want it to contain %q",
			checkText, out.Stdout, out.ExitCode, out.Stderr, want)
	}
}

// runnerArgv fills the probe-declared template the way internal/checks
// does: the core substitutes the connection's user and database, and the
// check's own text.
func runnerArgv(probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText string) []string {
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	return argv
}

// TestEndToEndRestoreDrill proves both artifact families through the whole
// stack: the docker provider, the core-side protocol client and this
// adapter, as separate processes.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	dataTar := filepath.Join(dir, "data.tar")
	makeDataTar(t, ctx, provider, image, dataTar, 500)
	backup := filepath.Join(dir, "nightly.jsonl")
	backupArtifact(t, backup, 5, 100)

	runner, err := adapter.New("couchdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "couchdb" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	for _, tc := range []struct{ kind, path, database string }{
		{"couchdb_data_tar", dataTar, "orders"},
		{"couchbackup", backup, "orders"},
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
				Options: map[string]string{"database": tc.database},
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
				t.Errorf("created_at = %v, want null — no CouchDB artifact dates the backup",
					*res.SourceIdentity.CreatedAt)
			}

			health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
			if err != nil {
				t.Fatalf("healthcheck: %v", err)
			}
			if !health.Healthy {
				t.Fatalf("healthcheck = %+v, want healthy", health)
			}

			// The whole backup came back, and a document reads as itself.
			// CouchDB states a number for _all_docs, so the runner reduces
			// the body to it; a document fetch has no such number, so the
			// body is passed through — both halves of the runner's rule.
			assertCheck(t, ctx, sbx, probe, res, "_all_docs?limit=0", "500")
			assertCheckContains(t, ctx, sbx, probe, res, "order-0042", `"customer":"cust-42"`)
			// Revisions are the backup's, not freshly minted ones: the
			// replay posts with new_edits=false, and a restore that
			// renumbered every revision would not be the database that was
			// backed up.
			assertCheckContains(t, ctx, sbx, probe, res, "order-0042", `"_rev":"1-`)

			// Issue #166: the compactor is suspended for the drill, and an
			// explicit compaction is still possible — a suspension, not a
			// rewrite.
			code, out := run(t, ctx, sbx,
				`curl -sf --user "admin:`+sandboxPassword+`" http://127.0.0.1:5984/_node/_local/_config/smoosh`)
			if code != 0 {
				t.Fatalf("read smoosh config (exit %d): %s", code, out)
			}
			for _, ch := range []string{`"db_channels":""`, `"view_channels":""`} {
				if !strings.Contains(out, ch) {
					t.Errorf("smoosh config = %s, want %s — the compactor is still enqueueing", out, ch)
				}
			}
		})
	}
}

// TestABackupCutInsideABatchIsRefused covers the one completeness check
// the couchbackup format allows. It writes nothing at its end, so a file
// cut between two lines is a shorter backup to any reader — but a file cut
// inside a line has no final newline, and that is refused before a byte
// moves.
func TestABackupCutInsideABatchIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	whole := filepath.Join(dir, "whole.jsonl")
	backupArtifact(t, whole, 4, 50)
	body, err := os.ReadFile(whole)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	torn := filepath.Join(dir, "torn.jsonl")
	if err := os.WriteFile(torn, body[:len(body)-20], 0o600); err != nil {
		t.Fatalf("write torn fixture: %v", err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("couchdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "couchbackup", Path: torn},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err == nil {
		t.Fatal("provision accepted a backup cut inside a batch")
	}
}

// TestATruncatedShardIsNotRefused pins a documented limit rather than a
// defect, which is why it is a test: a .couch file's header sits at its
// end, so a truncated shard opens as the older database its remaining
// bytes describe — measured at HTTP 200 with 280 documents of 500 — and no
// CouchDB artifact states how much it should hold. The drill's own
// row-count check is what closes this, and the adapter README says so. If
// this ever starts failing, the engine gained something to fence with and
// the documents must say so before the test changes.
func TestATruncatedShardIsNotRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	whole := filepath.Join(dir, "data.tar")
	makeDataTar(t, ctx, provider, image, whole, 500)

	// Rebuild the tar with one shard truncated to half.
	work := t.TempDir()
	if out, err := exec.CommandContext(ctx, "tar", "xf", whole, "-C", work).CombinedOutput(); err != nil {
		t.Fatalf("unpack fixture: %v: %s", err, out)
	}
	var shard string
	if err := filepath.WalkDir(filepath.Join(work, "shards"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && shard == "" {
			shard = p
		}
		return err
	}); err != nil {
		t.Fatalf("find a shard: %v", err)
	}
	body, err := os.ReadFile(shard)
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}
	if err := os.WriteFile(shard, body[:len(body)/2], 0o600); err != nil {
		t.Fatalf("truncate shard: %v", err)
	}
	cut := filepath.Join(dir, "cut.tar")
	if out, err := exec.CommandContext(ctx, "tar", "cf", cut, "-C", work, ".").CombinedOutput(); err != nil {
		t.Fatalf("repack fixture: %v: %s", err, out)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("couchdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "couchdb_data_tar", Path: cut},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "orders"},
	}, sbx)
	if err != nil {
		t.Fatalf("provision refused a truncated shard; the engine accepts one, so the adapter "+
			"must too and the README must keep saying why: %v", err)
	}

	// And this is the point: the drill's own check is what notices.
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: runnerArgv(probe, res, "_all_docs?limit=0"), Env: probe.SQLRunner.Env})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	got := strings.TrimSpace(string(out.Stdout))
	if got == "500" {
		t.Fatalf("the truncated restore reported the whole document count; the fixture did not "+
			"lose anything and this test proves nothing (stderr %s)", out.Stderr)
	}
	t.Logf("a truncated shard restored %s documents of 500, with the engine reporting success — "+
		"which is why the verdict is a check and not the engine's word", got)
}
