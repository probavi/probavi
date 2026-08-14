//go:build integration

package main_test

import (
	"context"
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

// verifiedImage is the official etcd image this run's binaries come from:
// the manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE
// when it names one the manifest already lists (docs/engine-versions.md
// §2). The official images are distroless, so the sandboxes below run a
// wrapper built FROM this image — the exact recipe the adapter README
// documents — and the drills exercise the binaries of the listed version.
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

// wrapperImage builds (once per base, cached afterwards) the shell-bearing
// image the README documents: alpine plus the three binaries copied from
// the official image.
func wrapperImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	base := verifiedImage(t)
	tag := "probavi-it-etcd:" + strings.ReplaceAll(base[strings.LastIndex(base, ":")+1:], "/", "-")
	dir := t.TempDir()
	dockerfile := fmt.Sprintf(`FROM %s AS etcd
FROM alpine:3.22
COPY --from=etcd /usr/local/bin/etcd /usr/local/bin/etcdctl /usr/local/bin/etcdutl /usr/local/bin/
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

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine `etcdctl snapshot save`
// artifact, start the server on it, and validate the restored keys
// through the probe-declared runner.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "snap.db")
	makeSnapshotFixture(t, ctx, provider, image, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("etcd", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "etcd" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "etcd_snapshot", Path: fixture},
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
		t.Errorf("created_at = %v, want null — a snapshot records no wall clock", *res.SourceIdentity.CreatedAt)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// The check dialect this adapter documents: a line of etcdctl
	// arguments, run through the probe-declared template exactly as
	// internal/checks would run it.
	assertCheck(t, ctx, sbx, probe, "get /probavi/config --print-value-only", "restored-ok")
	assertCheck(t, ctx, sbx, probe, "get --prefix /probavi/ --count-only --write-out=fields", `"Count" : 501`)

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestCorruptSnapshotVerdict proves a broken backup yields the right
// verdict through the whole stack: etcdutl inside the sandbox is the
// authority, and its refusal must surface as a claim about the backup.
func TestCorruptSnapshotVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx)

	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("this is not a bbolt snapshot"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("etcd", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "etcd_snapshot", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestDistrolessImageIsRefusedWithTheRecipe drives the preflight against
// the real official image: no shell, so the drill must fail up front,
// naming the problem and pointing at the documented wrapper. The
// container is kept alive by `etcd gateway start` — the only blocking
// subcommand a distroless etcd image can run (measured), and exactly the
// situation an operator who skipped the README lands in.
func TestDistrolessImageIsRefusedWithTheRecipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, map[string]string{
		"image":   verifiedImage(t),
		"command": "etcd gateway start --endpoints=127.0.0.1:9 --listen-addr=127.0.0.1:23790",
		"memory":  "256m",
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, []byte("irrelevant"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner, err := adapter.New("etcd", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "etcd_snapshot", Path: snap},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "invalid_request" ||
		!strings.Contains(aerr.Message, "distroless") {
		t.Fatalf("provision error = %v, want invalid_request naming the distroless image and the wrapper", err)
	}
}

// TestDirectoryDrillPicksNewestSnapshot proves the directory kind end to
// end with the ranking this format allows: a snapshot records nothing
// about when it was taken, so the newest file wins — and the README says
// exactly that, so the rule an operator reads is the rule that runs.
func TestDirectoryDrillPicksNewestSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx)
	provider := docker.New(nil)

	dir := t.TempDir()
	makeSnapshotFixture(t, ctx, provider, image, filepath.Join(dir, "newest.db"))
	old := filepath.Join(dir, "old.db")
	if err := os.WriteFile(old, []byte("stale placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("etcd", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "etcd_snapshot_dir", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	_ = res
	assertCheck(t, ctx, sbx, probe, "get /probavi/config --print-value-only", "restored-ok")
}

// assertCheck runs one check line through the probe-declared runner —
// exactly how internal/checks runs checks without engine knowledge — and
// asserts the last line of its output.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, checkText, want string) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out.Stdout)), "\n")
	got := strings.TrimSpace(lines[len(lines)-1])
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
		filepath.Join(binDir, "probavi-adapter-etcd"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeSnapshotFixture seeds a real server in a wrapper container and takes
// a genuine `etcdctl snapshot save` snapshot — the format with the
// appended integrity hash, which is what the adapter restores.
func makeSnapshotFixture(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
etcd --data-dir=/tmp/seed --listen-client-urls=http://127.0.0.1:2379 --advertise-client-urls=http://127.0.0.1:2379 >/tmp/etcd.log 2>&1 </dev/null &
i=0
until etcdctl --dial-timeout=2s endpoint health >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 60 ] && { tail -n 5 /tmp/etcd.log >&2; exit 1; }
  sleep 1
done
etcdctl put /probavi/config restored-ok >/dev/null
n=1
while [ "$n" -le ` + strconv.Itoa(seedKeyCount) + ` ]; do
  etcdctl put /probavi/key$n v$n >/dev/null
  n=$((n+1))
done
etcdctl snapshot save /tmp/snap.db >/dev/null 2>&1`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/snap.db", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// seedKeyCount is how many filler keys the fixture holds beside the config
// key; the count check asserts seedKeyCount+1.
const seedKeyCount = 500

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}
