//go:build integration

package main_test

import (
	"context"
	"encoding/json"
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

// engineMemoryLimit caps every container this suite starts. Weaviate
// restores this suite's fixture at 128m (measured); 512m keeps the
// seeding containers, which also index, comfortable.
const engineMemoryLimit = "512m"

// seedNode is the deliberately non-default node name the fixtures are
// taken on: the engine refuses to restore another node's backup
// (measured, HTTP 500), so a drill that restores at all proves the
// adapter pinned CLUSTER_HOSTNAME to what the backup's own metadata
// names.
const seedNode = "seed-node"

// verifiedImage is the engine image this run restores from: the
// manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE when
// it names one the manifest already lists (docs/engine-versions.md §2).
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

// buildWrapper builds the two-line wrapper the adapter README documents:
// the official image's entrypoint pin lifted, everything else untouched.
// The stock image cannot idle — /bin/weaviate ignores unknown positional
// arguments and starts serving (measured) — so `command: sleep infinity`
// needs an image whose entrypoint is empty.
func buildWrapper(t *testing.T, ctx context.Context, base string) string {
	t.Helper()
	tag := "probavi-it-weaviate:" + strings.ReplaceAll(base[strings.LastIndex(base, ":")+1:], "/", "-")
	dir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM %s\nENTRYPOINT []\n", base)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build wrapper image: %v: %s", err, out)
	}
	return tag
}

// sandboxParams start the sandbox idle, which this adapter requires: the
// backup tree must be in place and the module environment set before the
// engine starts (see the adapter README).
func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-weaviate"), ".").CombinedOutput(); err != nil {
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

// startSeedEngineScript brings Weaviate up on an empty data directory so
// the suite can build fixtures through the engine's own API — the same
// door an operator takes a backup through. The environment is the one the
// adapter documents, node name aside.
const startSeedEngineScript = `set -u
mkdir -p /tmp/backups /tmp/data
ENABLE_MODULES=backup-filesystem BACKUP_FILESYSTEM_PATH=/tmp/backups \
PERSISTENCE_DATA_PATH=/tmp/data \
AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true DISABLE_TELEMETRY=true \
CLUSTER_HOSTNAME=$1 CLUSTER_ADVERTISE_ADDR=127.0.0.1 \
nohup /bin/weaviate --host 127.0.0.1 --port 8080 --scheme http >/tmp/seed.log 2>&1 &
i=0
while [ $i -lt 240 ]; do
  if wget -q -O /dev/null -T 2 http://127.0.0.1:8080/v1/.well-known/ready 2>/dev/null; then exit 0; fi
  i=$((i+1)); sleep 0.5
done
tail -20 /tmp/seed.log >&2; exit 1`

func startEngineForSeeding(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	if code, out := run(t, ctx, sbx, startSeedEngineScript, seedNode); code != 0 {
		t.Fatalf("start seed engine (exit %d): %s", code, out)
	}
}

// postJSON sends one JSON body to the seed engine with wget, the image's
// own HTTP client, and fails on any non-2xx answer.
const postJSONScript = `set -u
cat > /tmp/req.json
wget -q -O /tmp/resp.json --header 'Content-Type: application/json' \
  --post-file /tmp/req.json "http://127.0.0.1:8080$1" 2>&1 || {
  echo "POST $1 refused"; cat /tmp/resp.json 2>/dev/null; exit 1; }
cat /tmp/resp.json`

func postJSON(t *testing.T, ctx context.Context, sbx *docker.Sandbox, path, body string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"sh", "-c", postJSONScript, "sh", path}, Stdin: []byte(body),
	})
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("POST %s (exit %d): %s%s", path, out.ExitCode, out.Stdout, out.Stderr)
	}
	return string(out.Stdout)
}

// batchDeleteScript speaks the one request busybox wget cannot: a DELETE
// with a body, over nc against loopback. The sleep keeps nc's stdin open
// until the answer arrives — busybox nc drops the connection on stdin
// EOF, which cancels the request server-side (measured: "context
// canceled" and nothing deleted).
const batchDeleteScript = `set -u
body=$(cat)
{
  printf 'DELETE /v1/batch/objects HTTP/1.1\r\n'
  printf 'Host: localhost\r\nConnection: close\r\nContent-Type: application/json\r\n'
  printf 'Content-Length: %s\r\n\r\n' "${#body}"
  printf '%s' "$body"
  sleep 4
} | nc 127.0.0.1 8080 | head -1 | grep -q ' 200 ' || { echo "batch delete refused" >&2; exit 1; }`

// backupSpec describes a fixture to build.
type backupSpec struct {
	id      string
	objects int
	deleted int // objects with idx > objects-deleted are batch-deleted first
}

// objectsBody builds a deterministic batch: n objects with a small vector
// and properties the checks can filter on.
func objectsBody(n int) string {
	objects := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		objects = append(objects, map[string]any{
			"class":  "Books",
			"vector": []float64{float64(i) * 0.001, 0.5, 0.25, 0.125},
			"properties": map[string]any{
				"idx":    i,
				"region": []string{"us", "eu"}[i%2],
			},
		})
	}
	body, err := json.Marshal(map[string]any{"objects": objects})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// makeBackup seeds an engine and copies one filesystem-backend backup out
// of it, which is exactly the artifact an operator ships. The class
// declares its own vector-index cleanup interval — an operator's setting
// that travels in the backup, and the short value is what lets the #166
// test watch more than one cleanup window.
func makeBackup(t *testing.T, ctx context.Context, provider *docker.Provider, image string,
	spec backupSpec, destDir string) string {
	t.Helper()
	seedBox, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seedBox)
	startEngineForSeeding(t, ctx, seedBox)

	postJSON(t, ctx, seedBox, "/v1/schema", `{"class":"Books","vectorizer":"none",
		"vectorIndexConfig":{"cleanupIntervalSeconds":2},
		"properties":[{"name":"idx","dataType":["int"]},{"name":"region","dataType":["text"]}]}`)
	if spec.objects > 0 {
		postJSON(t, ctx, seedBox, "/v1/batch/objects", objectsBody(spec.objects))
	}
	if spec.deleted > 0 {
		body := fmt.Sprintf(`{"match":{"class":"Books","where":{"path":["idx"],"operator":"GreaterThan","valueInt":%d}}}`,
			spec.objects-spec.deleted)
		out, err := seedBox.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"sh", "-c", batchDeleteScript, "sh"}, Stdin: []byte(body),
		})
		if err != nil || out.ExitCode != 0 {
			t.Fatalf("batch delete: %v (exit %d): %s", err, out.ExitCode, out.Stderr)
		}
	}

	resp := postJSON(t, ctx, seedBox, "/v1/backups/filesystem", fmt.Sprintf(`{"id":%q}`, spec.id))
	if !strings.Contains(resp, `"STARTED"`) && !strings.Contains(resp, `"SUCCESS"`) {
		t.Fatalf("take backup: %s", resp)
	}
	code, out := run(t, ctx, seedBox, `i=0
while [ $i -lt 120 ]; do
  s=$(wget -q -O - "http://127.0.0.1:8080/v1/backups/filesystem/$1" 2>&1)
  case "$s" in *'"SUCCESS"'*) exit 0 ;; *'"FAILED"'*) printf '%s' "$s" >&2; exit 1 ;; esac
  i=$((i+1)); sleep 0.5
done
echo 'backup did not finish' >&2; exit 1`, spec.id)
	if code != 0 {
		t.Fatalf("await backup (exit %d): %s", code, out)
	}

	local := filepath.Join(destDir, spec.id)
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seedBox.ID()+":/tmp/backups/"+spec.id, local).CombinedOutput(); err != nil {
		t.Fatalf("copy backup out: %v: %s", err, out)
	}
	// docker cp preserves the engine's modes; the adapter reads the tree
	// as the invoking user.
	if out, err := exec.CommandContext(ctx, "chmod", "-R", "u+rwX", local).CombinedOutput(); err != nil {
		t.Fatalf("chmod backup: %v: %s", err, out)
	}
	return local
}

// tarOfDir packs a backup directory the way an operator would.
func tarOfDir(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), filepath.Base(dir)+".tar")
	if raw, err := exec.Command("tar", "-cf", out, "-C", filepath.Dir(dir),
		filepath.Base(dir)).CombinedOutput(); err != nil {
		t.Fatalf("pack %s: %v: %s", dir, err, raw)
	}
	return out
}

// runnerArgv fills the probe-declared template the way internal/checks
// does: the core substitutes the connection's database and the check's
// own text.
func runnerArgv(probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText string) []string {
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	return argv
}

// assertCheck runs one check through the probe-declared runner — exactly
// how internal/checks runs checks without engine knowledge — and asserts
// the answer.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText, want string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: runnerArgv(probe, res, checkText), Env: probe.SQLRunner.Env,
	})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if out.ExitCode != 0 || strings.TrimSpace(string(out.Stdout)) != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q",
			checkText, out.Stdout, out.ExitCode, out.Stderr, want)
	}
}

func newProbe(t *testing.T, ctx context.Context) (*adapter.Runner, *adapter.ProbeResult) {
	t.Helper()
	runner, err := adapter.New("weaviate", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "weaviate" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}
	return runner, probe
}

func provision(t *testing.T, ctx context.Context, runner *adapter.Runner,
	sbx *docker.Sandbox, kind, path string) (*adapter.ProvisionResult, error) {
	t.Helper()
	return runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: kind, Path: path},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
}

// TestEndToEndRestoreDrill proves all three artifact packagings through
// the whole stack: the docker provider, the core-side protocol client and
// this adapter, as separate processes. The fixtures were taken on a
// non-default node name, so a green restore also proves the adapter
// pinned CLUSTER_HOSTNAME to the backup's own claim.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := buildWrapper(t, ctx, verifiedImage(t))
	provider := docker.New(nil)
	dir := t.TempDir()

	backupDir := makeBackup(t, ctx, provider, image, backupSpec{id: "nightly", objects: 1000}, dir)
	archive := tarOfDir(t, backupDir)

	runner, probe := newProbe(t, ctx)

	for _, tc := range []struct{ kind, path string }{
		{"weaviate_backup", backupDir},
		{"weaviate_backup_tar", archive},
		{"weaviate_backup_dir", dir},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			sbx, err := provider.Create(ctx, sandboxParams(image))
			if err != nil {
				t.Fatalf("create drill sandbox: %v", err)
			}
			defer destroy(t, sbx)

			res, err := provision(t, ctx, runner, sbx, tc.kind, tc.path)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if res.Timings.RestoreSeconds <= 0 {
				t.Errorf("timings = %+v, want real measurements", res.Timings)
			}
			if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
				t.Errorf("source identity = %+v", res.SourceIdentity)
			}
			if res.SourceIdentity.CreatedAt == nil {
				t.Error("created_at = nil, want the backup's own completion instant")
			}
			if res.Connection.Database != "Books" {
				t.Errorf("connection.database = %q, want Books", res.Connection.Database)
			}

			health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
			if err != nil {
				t.Fatalf("healthcheck: %v", err)
			}
			if !health.Healthy {
				t.Fatalf("healthcheck = %+v, want healthy", health)
			}

			// The whole class came back, through the runner's three
			// forms: bare GraphQL text, a path with a body, and a plain
			// path.
			assertCheck(t, ctx, sbx, probe, res, "{Aggregate{Books{meta{count}}}}", "1000")
			assertCheck(t, ctx, sbx, probe, res,
				`{Aggregate{Books(where:{path:["region"],operator:Equal,valueText:"eu"}){meta{count}}}}`, "500")
			assertCheck(t, ctx, sbx, probe, res,
				`/v1/graphql {"query":"{Aggregate{Books(where:{path:[\"idx\"],operator:LessThanEqual,valueInt:10}){meta{count}}}}"}`, "10")

			// Nothing left the sandbox: without DISABLE_TELEMETRY=true
			// the engine POSTs home at startup (measured), so the
			// environment of the live process is the assertion.
			code, out := run(t, ctx, sbx,
				`tr '\0' '\n' < /proc/$(pidof weaviate)/environ | grep -x DISABLE_TELEMETRY=true`)
			if code != 0 {
				t.Errorf("the running engine was not started with DISABLE_TELEMETRY=true (exit %d): %s", code, out)
			}
		})
	}
}

// TestADamagedBackupIsRefused: the engine judges its artifact and judges
// it well — a chunk truncated mid-file fails the restore with the
// engine's own words, and the class is never created (measured). The
// drill must report that as a bad backup, not as a broken sandbox.
func TestADamagedBackupIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := buildWrapper(t, ctx, verifiedImage(t))
	provider := docker.New(nil)
	dir := t.TempDir()

	backupDir := makeBackup(t, ctx, provider, image, backupSpec{id: "torn", objects: 1000}, dir)
	chunks, err := filepath.Glob(filepath.Join(backupDir, seedNode, "Books", "chunk-*"))
	if err != nil || len(chunks) == 0 {
		t.Fatalf("locate chunks: %v (%d found)", err, len(chunks))
	}
	body, err := os.ReadFile(chunks[0])
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	if err := os.WriteFile(chunks[0], body[:len(body)/2], 0o600); err != nil {
		t.Fatalf("truncate chunk: %v", err)
	}

	runner, _ := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = provision(t, ctx, runner, sbx, "weaviate_backup", backupDir)
	if err == nil {
		t.Fatal("a backup with a truncated chunk restored without complaint")
	}
	if !strings.Contains(err.Error(), "source_corrupt") {
		t.Errorf("provision error = %v, want source_corrupt", err)
	}
}

// TestAnIncompleteBackupIsRefusedBeforeAByteMoves: the backup's own node
// manifest names every chunk, so a file lost in a copy is caught on the
// host, by the backup's claim about itself.
func TestAnIncompleteBackupIsRefusedBeforeAByteMoves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := buildWrapper(t, ctx, verifiedImage(t))
	provider := docker.New(nil)
	dir := t.TempDir()

	backupDir := makeBackup(t, ctx, provider, image, backupSpec{id: "partial", objects: 1000}, dir)
	chunks, err := filepath.Glob(filepath.Join(backupDir, seedNode, "Books", "chunk-*"))
	if err != nil || len(chunks) == 0 {
		t.Fatalf("locate chunks: %v (%d found)", err, len(chunks))
	}
	if err := os.Remove(chunks[0]); err != nil {
		t.Fatalf("drop chunk: %v", err)
	}

	runner, _ := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = provision(t, ctx, runner, sbx, "weaviate_backup", backupDir)
	if err == nil {
		t.Fatal("a backup missing a chunk its manifest names restored without complaint")
	}
	if !strings.Contains(err.Error(), "source_corrupt") {
		t.Errorf("provision error = %v, want source_corrupt", err)
	}
}

// TestAnEmptyClassBackupIsRefused is the well-formed zero: an empty class
// has a perfectly valid backup (measured), so "the engine answered"
// cannot be the restore's verdict.
func TestAnEmptyClassBackupIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := buildWrapper(t, ctx, verifiedImage(t))
	provider := docker.New(nil)
	dir := t.TempDir()

	backupDir := makeBackup(t, ctx, provider, image, backupSpec{id: "empty", objects: 0}, dir)

	runner, _ := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = provision(t, ctx, runner, sbx, "weaviate_backup", backupDir)
	if err == nil {
		t.Fatal("a backup of an empty class was reported as a proven restore")
	}
	if !strings.Contains(err.Error(), "source_corrupt") {
		t.Errorf("provision error = %v, want source_corrupt", err)
	}
}

// TestTheRestoredObjectCountDoesNotShrink is issue #166 for this engine,
// and it is the guard shape rather than the suspend one.
//
// Weaviate has no TTL and no expiry. What it runs unbidden is the vector
// index's tombstone cleanup and LSM compaction, which reclaim space for
// objects that were already deleted — objects a count never counted. So
// there is nothing to suspend, and the honest thing is to prove the
// property rather than assume it: restore a backup that carries deleted
// objects and watch the count across the cleanup interval the class
// itself declares (two seconds in this fixture, so ten seconds covers
// several windows).
func TestTheRestoredObjectCountDoesNotShrink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := buildWrapper(t, ctx, verifiedImage(t))
	provider := docker.New(nil)
	dir := t.TempDir()

	backupDir := makeBackup(t, ctx, provider, image,
		backupSpec{id: "tomb", objects: 1000, deleted: 300}, dir)

	runner, probe := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	res, err := provision(t, ctx, runner, sbx, "weaviate_backup", backupDir)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	count := "{Aggregate{Books{meta{count}}}}"
	assertCheck(t, ctx, sbx, probe, res, count, "700")
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while watching the cleanup windows")
	case <-time.After(10 * time.Second):
	}
	assertCheck(t, ctx, sbx, probe, res, count, "700")
}
