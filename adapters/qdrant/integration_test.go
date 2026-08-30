//go:build integration

package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// engineMemoryLimit caps every container this suite starts. Qdrant
// restores this suite's fixture at 128m (measured); 512m keeps the seeding
// containers, which also build and compact, comfortable.
const engineMemoryLimit = "512m"

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

// sandboxParams start the sandbox idle, which this adapter requires:
// Qdrant restores a snapshot as a startup argument, so the engine must not
// have started before the artifact is in place (see the adapter README).
func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-qdrant"), ".").CombinedOutput(); err != nil {
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

// httpScript is the suite's own HTTP client, and it exists for the same
// reason the adapter's does: the official image carries no curl, wget, nc
// or python3, so bash's /dev/tcp is the only way in.
const httpScript = `set -u
body=$(cat)
exec 3<>/dev/tcp/127.0.0.1/6333
{
  printf '%s %s HTTP/1.1\r\n' "$1" "$2"
  printf 'Host: localhost\r\nConnection: close\r\n'
  if [ -n "$body" ]; then
    printf 'Content-Type: application/json\r\nContent-Length: %s\r\n' "${#body}"
  fi
  printf '\r\n'
  [ -n "$body" ] && printf '%s' "$body"
} >&3
cat <&3`

// engineCall speaks one request to the engine in a sandbox and returns the
// HTTP status with the response body.
func engineCall(t *testing.T, ctx context.Context, sbx *docker.Sandbox, method, path, body string) (string, string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv:  []string{"bash", "-c", httpScript, "bash", method, path},
		Stdin: []byte(body),
	})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	resp := string(out.Stdout)
	head, rest, _ := strings.Cut(resp, "\r\n\r\n")
	status := ""
	if fields := strings.Fields(head); len(fields) > 1 {
		status = fields[1]
	}
	return status, rest
}

// startEngineForSeeding brings Qdrant up on an empty storage tree so the
// suite can build fixtures through the engine's own API — the same door an
// operator takes a snapshot through.
func startEngineForSeeding(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c",
		`cd /qdrant && nohup ./qdrant --disable-telemetry >/tmp/q.log 2>&1 &
i=0
while [ $i -lt 120 ]; do
  if (exec 3<>/dev/tcp/127.0.0.1/6333) 2>/dev/null; then exit 0; fi
  i=$((i+1)); sleep 1
done
tail -20 /tmp/q.log >&2; exit 1`}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("start engine: %v (exit %d): %s", err, out.ExitCode, out.Stderr)
	}
}

// pointsBody builds a deterministic upsert body: n points with a 4-dim
// vector and a payload the checks can filter on.
func pointsBody(n int) string {
	points := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		a := float64(i) * 0.001
		points = append(points, map[string]any{
			"id":     i,
			"vector": []float64{round(math.Sin(a)), round(math.Cos(a)), round(a), round(1 - a)},
			"payload": map[string]any{
				"customer": fmt.Sprintf("cust-%d", i),
				"region":   []string{"eu", "us"}[i%2],
			},
		})
	}
	body, err := json.Marshal(map[string]any{"points": points})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func round(f float64) float64 { return math.Round(f*1e6) / 1e6 }

// seedCollection creates a collection, fills it, and soft-deletes the tail
// when deleted > 0.
func seedCollection(t *testing.T, ctx context.Context, sbx *docker.Sandbox, name string, points, deleted int) {
	t.Helper()
	if status, body := engineCall(t, ctx, sbx, "PUT", "/collections/"+name,
		`{"vectors":{"size":4,"distance":"Dot"}}`); !strings.HasPrefix(status, "2") {
		t.Fatalf("create collection: %s %s", status, body)
	}
	if points > 0 {
		if status, body := engineCall(t, ctx, sbx, "PUT",
			"/collections/"+name+"/points?wait=true", pointsBody(points)); !strings.HasPrefix(status, "2") {
			t.Fatalf("upsert points: %s %s", status, body)
		}
	}
	if deleted > 0 {
		ids := make([]string, 0, deleted)
		for i := points - deleted + 1; i <= points; i++ {
			ids = append(ids, fmt.Sprint(i))
		}
		if status, body := engineCall(t, ctx, sbx, "POST",
			"/collections/"+name+"/points/delete?wait=true",
			`{"points":[`+strings.Join(ids, ",")+`]}`); !strings.HasPrefix(status, "2") {
			t.Fatalf("delete points: %s %s", status, body)
		}
	}
}

// snapshotSpec describes a fixture to build.
type snapshotSpec struct {
	collection     string
	points         int
	deleted        int
	full           bool // POST /snapshots rather than the collection's own
	withChecksum   bool // copy the .checksum sidecar out beside it
	destinationDir string
}

// makeSnapshot seeds an engine and copies one snapshot out of it, which is
// exactly the artifact an operator ships.
func makeSnapshot(t *testing.T, ctx context.Context, provider *docker.Provider, image string, spec snapshotSpec) string {
	t.Helper()
	seedBox, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seedBox)
	startEngineForSeeding(t, ctx, seedBox)
	seedCollection(t, ctx, seedBox, spec.collection, spec.points, spec.deleted)

	path := "/collections/" + spec.collection + "/snapshots?wait=true"
	find := `ls /qdrant/snapshots/` + spec.collection + `/*.snapshot`
	if spec.full {
		path, find = "/snapshots?wait=true", `ls /qdrant/snapshots/*.snapshot`
	}
	if status, body := engineCall(t, ctx, seedBox, "POST", path, ""); !strings.HasPrefix(status, "2") {
		t.Fatalf("take snapshot: %s %s", status, body)
	}
	code, out := run(t, ctx, seedBox, find+` | head -1`)
	if code != 0 {
		t.Fatalf("locate snapshot (exit %d): %s", code, out)
	}
	remote := strings.TrimSpace(out)

	local := filepath.Join(spec.destinationDir, filepath.Base(remote))
	copyOut(t, ctx, seedBox, remote, local)
	if spec.withChecksum {
		copyOut(t, ctx, seedBox, remote+".checksum", local+".checksum")
	}
	return local
}

func copyOut(t *testing.T, ctx context.Context, sbx *docker.Sandbox, remote, local string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		sbx.ID()+":"+remote, local).CombinedOutput(); err != nil {
		t.Fatalf("copy %s out: %v: %s", remote, err, out)
	}
	// docker cp preserves the engine's 0600, which the adapter reads as
	// the invoking user.
	if err := os.Chmod(local, 0o644); err != nil {
		t.Fatalf("chmod %s: %v", local, err)
	}
}

// runnerArgv fills the probe-declared template the way internal/checks
// does: the core substitutes the connection's database and the check's own
// text.
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
	runner, err := adapter.New("qdrant", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "qdrant" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}
	return runner, probe
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

	collectionSnapshot := makeSnapshot(t, ctx, provider, image, snapshotSpec{
		collection: "orders", points: 1000, withChecksum: true, destinationDir: dir,
	})
	fullDir := t.TempDir()
	fullSnapshot := makeSnapshot(t, ctx, provider, image, snapshotSpec{
		collection: "orders", points: 1000, full: true, destinationDir: fullDir,
	})

	runner, probe := newProbe(t, ctx)

	for _, tc := range []struct{ kind, path string }{
		{"qdrant_snapshot", collectionSnapshot},
		{"qdrant_full_snapshot", fullSnapshot},
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
				Options: map[string]string{"collection": "orders"},
			}, sbx)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if res.Timings.RestoreSeconds <= 0 {
				t.Errorf("timings = %+v, want real measurements", res.Timings)
			}
			if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
				t.Errorf("source identity = %+v", res.SourceIdentity)
			}
			if res.SourceIdentity.CreatedAt != nil {
				t.Errorf("created_at = %v, want null — no Qdrant snapshot dates the backup",
					*res.SourceIdentity.CreatedAt)
			}

			health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
			if err != nil {
				t.Fatalf("healthcheck: %v", err)
			}
			if !health.Healthy {
				t.Fatalf("healthcheck = %+v, want healthy", health)
			}

			// The whole collection came back, through both halves of the
			// runner's rule: a bare path is a GET, and a path with a body
			// is the POST that asks Qdrant its most useful question.
			assertCheck(t, ctx, sbx, probe, res, "/collections/orders", "1000")
			assertCheck(t, ctx, sbx, probe, res, `points/count {"exact":true}`, "1000")
			assertCheck(t, ctx, sbx, probe, res,
				`points/count {"exact":true,"filter":{"must":[{"key":"region","match":{"value":"eu"}}]}}`,
				"500")

			// Nothing left the sandbox: telemetry is off by default in
			// this adapter, and the flag is what states it.
			code, out := run(t, ctx, sbx, `grep -c -- --disable-telemetry /proc/*/cmdline 2>/dev/null | grep -v ':0$' | head -1`)
			if code != 0 || out == "" {
				t.Errorf("the running engine was not started with --disable-telemetry (exit %d): %s", code, out)
			}
		})
	}
}

// TestADamagedSnapshotIsRefused is the fence this engine has and the h2
// and couchdb adapters do not: Qdrant validates a snapshot as it reads it
// and dies rather than serving a smaller collection. The drill must report
// that as a bad backup, not as a broken sandbox.
func TestADamagedSnapshotIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	dir := t.TempDir()

	whole := makeSnapshot(t, ctx, provider, image, snapshotSpec{
		collection: "orders", points: 1000, destinationDir: dir,
	})
	body, err := os.ReadFile(whole)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	torn := filepath.Join(t.TempDir(), "torn.snapshot")
	if err := os.WriteFile(torn, body[:len(body)*3/4], 0o644); err != nil {
		t.Fatalf("write torn fixture: %v", err)
	}

	runner, _ := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "qdrant_snapshot", Path: torn},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"collection": "orders"},
	}, sbx)
	if err == nil {
		t.Fatal("a snapshot truncated at three quarters restored without complaint")
	}
	if !strings.Contains(err.Error(), "source_corrupt") {
		t.Errorf("provision error = %v, want source_corrupt", err)
	}
}

// TestAnArtifactThatDoesNotMatchItsChecksumIsRefused exercises the fence
// that fires before a byte crosses into the sandbox. Qdrant writes the
// digest beside every snapshot, and when the operator copied it too, a
// file that changed afterwards can be named as such.
func TestAnArtifactThatDoesNotMatchItsChecksumIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	dir := t.TempDir()

	snap := makeSnapshot(t, ctx, provider, image, snapshotSpec{
		collection: "orders", points: 1000, withChecksum: true, destinationDir: dir,
	})
	// One byte, deep inside, with the sidecar left as Qdrant wrote it.
	body, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	body[len(body)/2] ^= 0xFF
	if err := os.WriteFile(snap, body, 0o644); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}

	runner, _ := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "qdrant_snapshot", Path: snap},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"collection": "orders"},
	}, sbx)
	if err == nil {
		t.Fatal("an artifact that contradicts its own checksum was restored")
	}
	if !strings.Contains(err.Error(), "source_corrupt") {
		t.Errorf("provision error = %v, want source_corrupt", err)
	}
}

// TestTheRestoredPointCountDoesNotShrink is issue #166 for this engine,
// and it is the guard shape rather than the suspend one.
//
// Qdrant has no TTL and no expiry. The only thing it runs unbidden is the
// optimizer, whose vacuum reclaims points that were already soft-deleted —
// which a point count cannot see, because it never counted them. So there
// is nothing to suspend, and the honest thing is to prove the property
// rather than assume it: restore a snapshot that carries soft-deleted
// points and watch the count across the optimizer's own window
// (flush_interval_sec is 5 by default, so ten seconds covers two).
func TestTheRestoredPointCountDoesNotShrink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	dir := t.TempDir()

	snap := makeSnapshot(t, ctx, provider, image, snapshotSpec{
		collection: "orders", points: 1000, deleted: 300, destinationDir: dir,
	})

	runner, probe := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "qdrant_snapshot", Path: snap},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"collection": "orders"},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	assertCheck(t, ctx, sbx, probe, res, "/collections/orders", "700")
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while watching the optimizer window")
	case <-time.After(10 * time.Second):
	}
	assertCheck(t, ctx, sbx, probe, res, "/collections/orders", "700")

	// An explicit compaction is still the operator's to ask for: what a
	// drill must not do is let the engine decide, not stop anyone asking.
	if status, body := engineCall(t, ctx, sbx, "POST",
		"/collections/orders/points/count", `{"exact":true}`); !strings.HasPrefix(status, "2") {
		t.Fatalf("count after the optimizer window: %s %s", status, body)
	}
}

// TestAnEmptyCollectionSnapshotIsRefused is the well-formed zero: an empty
// collection has a perfectly valid snapshot, so "the engine answered"
// cannot be the restore's verdict.
func TestAnEmptyCollectionSnapshotIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)
	dir := t.TempDir()

	snap := makeSnapshot(t, ctx, provider, image, snapshotSpec{
		collection: "orders", points: 0, destinationDir: dir,
	})

	runner, _ := newProbe(t, ctx)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "qdrant_snapshot", Path: snap},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"collection": "orders"},
	}, sbx)
	if err == nil {
		t.Fatal("a snapshot of an empty collection was reported as a proven restore")
	}
	if !strings.Contains(err.Error(), "source_corrupt") {
		t.Errorf("provision error = %v, want source_corrupt", err)
	}
}
