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
const engineMemoryLimit = "1g"

// records is how many the fixture holds. Enough that a five-neighbour query
// is a real question and that the write queue is purged into a segment,
// which is the state the silent failure below only exists in.
const records = 2000

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

// wrapperImage builds the image a drill sandbox actually runs: the official
// one with its entrypoint pin lifted, which is the exact recipe the adapter
// README documents. Without it the sandbox's `command` reaches the chroma
// CLI as a subcommand and the container exits (measured).
func wrapperImage(t *testing.T, ctx context.Context, base string) string {
	t.Helper()
	tag := "probavi-it-chroma:" + strings.ReplaceAll(base[strings.LastIndex(base, ":")+1:], "/", "-")
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

func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-chroma"), ".").CombinedOutput(); err != nil {
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

// run executes one shell script inside a sandbox and returns its exit code
// with the combined output.
func run(t *testing.T, ctx context.Context, sbx *docker.Sandbox, script string, args ...string) (int, string) {
	t.Helper()
	argv := append([]string{"bash", "-c", script, "bash"}, args...)
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return out.ExitCode, string(out.Stdout) + string(out.Stderr)
}

func newProbe(t *testing.T, ctx context.Context) *adapter.Runner {
	t.Helper()
	runner, err := adapter.New("chroma", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "chroma" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}
	return runner
}

// httpFn is the same pure-bash HTTP client the adapter ships, so the suite
// seeds the engine the way the adapter reads it: the image has no curl, no
// python and no jq.
const httpFn = `http() {
  local m=$1 p=$2 b=${3-} line
  exec 3<>/dev/tcp/127.0.0.1/8000 || return 9
  { printf '%s %s HTTP/1.0\r\nHost: localhost\r\n' "$m" "$p"
    [ -n "$b" ] && printf 'Content-Type: application/json\r\nContent-Length: %d\r\n' "${#b}"
    printf '\r\n%s' "$b"; } >&3
  while IFS= read -r line <&3; do case "$line" in ''|$'\r') break;; esac; done
  cat <&3
  exec 3<&-
}
`

// seedScript starts a server, fills one collection, and waits for the write
// queue to drain into a segment — the state in which losing that segment
// stops being recoverable and starts being silent.
const seedScript = httpFn + `set -eu
n=$1
nohup chroma run --path /seed --host 127.0.0.1 --port 8000 >/tmp/seed.log 2>&1 &
for i in $(seq 1 120); do http GET /api/v2/heartbeat 2>/dev/null | grep -q heartbeat && break; sleep 1; done
B=/api/v2/tenants/default_tenant/databases/default_database/collections
http POST "$B" '{"name":"drills","get_or_create":true}' >/dev/null
cid=$(http GET "$B" | grep -oE '"id":"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"' | head -1 | cut -d'"' -f4)
ids=''; embs=''; docs=''; sep=''
i=0
while [ "$i" -lt "$n" ]; do
  ids="$ids$sep\"id$i\""
  embs="$embs$sep[$((i%7)).0,$((i%11)).0,$((i%13)).0]"
  docs="$docs$sep\"document $i\""
  sep=','
  i=$((i+1))
done
http POST "$B/$cid/add" "{\"ids\":[$ids],\"embeddings\":[$embs],\"documents\":[$docs]}" >/dev/null
http GET "$B/$cid/count"
`

// seedFixture produces a persistence directory on the host, the way the
// README tells an operator to: seed, stop the server, copy the directory.
func seedFixture(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	if code, out := run(t, ctx, seed, "mkdir -p /seed"); code != 0 {
		t.Fatalf("prepare seed dir: %s", out)
	}
	code, out := run(t, ctx, seed, seedScript, fmt.Sprint(records))
	if code != 0 {
		t.Fatalf("seed chroma: exit %d: %s", code, out)
	}
	if got := strings.TrimSpace(out); got != fmt.Sprint(records) {
		t.Fatalf("seeded collection holds %q, want %d", got, records)
	}
	// Stop the server before copying: that is the artifact this adapter
	// accepts, and the only one an operator can take honestly.
	if code, out := run(t, ctx, seed, "pkill -f 'chroma run' || true; sleep 3"); code != 0 {
		t.Fatalf("stop chroma: %s", out)
	}
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	cp := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/seed/.", dest)
	if out, err := cp.CombinedOutput(); err != nil {
		t.Fatalf("copy the persistence directory out: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "chroma.sqlite3")); err != nil {
		t.Fatalf("the fixture holds no metadata database: %v", err)
	}
}

// TestEndToEndRestoreDrill restores both artifact families through the whole
// stack against a real engine.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	dir := filepath.Join(t.TempDir(), "data")
	seedFixture(t, ctx, provider, image, dir)

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if out, err := exec.CommandContext(ctx, "tar", "-czf", archive, "-C", dir, ".").CombinedOutput(); err != nil {
		t.Fatalf("make the archive fixture: %v: %s", err, out)
	}

	runner := newProbe(t, ctx)
	for _, tc := range []struct{ kind, path string }{
		{"chroma_data", dir},
		{"chroma_data_tar", archive},
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
				t.Errorf("created_at = %v, want null — nothing dates a persistence directory",
					*res.SourceIdentity.CreatedAt)
			}
			health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
			if err != nil || !health.Healthy {
				t.Fatalf("healthcheck: %+v, %v", health, err)
			}
			// The declared runner must answer a real check.
			code, out := run(t, ctx, sbx, checkRunnerScript(t, ctx, runner), "", "drills/count")
			if code != 0 {
				t.Fatalf("the declared check runner failed: exit %d: %s", code, out)
			}
			if got := strings.TrimSpace(out); got != fmt.Sprint(records) {
				t.Errorf("count check answered %q, want %d", got, records)
			}
		})
	}
}

// checkRunnerScript is the bash script the probe declares as its runner.
func checkRunnerScript(t *testing.T, ctx context.Context, runner *adapter.Runner) string {
	t.Helper()
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	argv := probe.SQLRunner.Argv
	if len(argv) < 3 {
		t.Fatalf("runner argv: %v", argv)
	}
	return argv[2]
}

// TestASilentlyGuttedBackupIsRefused is this adapter's reason to exist,
// proved against the engine rather than against a belief.
//
// The fixture's write queue has already drained into a segment, so deleting
// that segment directory costs the collection its vector index and nothing
// else: Chroma starts, counts every record, returns every document, and logs
// nothing. Only the query is short. A drill must fail here, and must fail
// for that reason.
func TestASilentlyGuttedBackupIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	dir := filepath.Join(t.TempDir(), "data")
	seedFixture(t, ctx, provider, image, dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	var removed int
	for _, e := range entries {
		if e.IsDir() {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove segment directory: %v", err)
			}
			removed++
		}
	}
	if removed == 0 {
		t.Fatal("the fixture carries no segment directory, so this test would pass vacuously — " +
			"the write queue never drained")
	}

	runner := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, perr := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "chroma_data", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if perr == nil {
		t.Fatal("provision passed a backup whose vector index was gone — the count was right and " +
			"the search was empty, which is exactly the false green this adapter exists to refuse")
	}
	var aerr *adapter.Error
	if !asAdapterError(perr, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision failed with %v, want source_corrupt", perr)
	}
	for _, want := range []string{"nearest-neighbour", "count is not the proof"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("the refusal does not mention %q: %s", want, aerr.Message)
		}
	}
}

// asAdapterError unwraps the adapter's protocol error.
func asAdapterError(err error, target **adapter.Error) bool {
	for err != nil {
		if e, ok := err.(*adapter.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
