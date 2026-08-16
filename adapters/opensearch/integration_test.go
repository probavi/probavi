//go:build integration

package main_test

import (
	"context"
	"errors"
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

// verifiedImage is the official image this run's node comes from: the
// manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE when
// it names one the manifest already lists (docs/engine-versions.md §2).
// No wrapper is needed: the official images idle under sleep infinity
// (their entrypoint passes non-opensearch commands through, measured).
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
	// The image's JVM defaults to a 1g heap; 2g keeps the node inside the
	// cap while leaving room for the restore.
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "2g"}
}

// seedScript runs a real node the way the adapter does, seeds an index,
// and takes two genuine snapshots into an fs repository — the second
// strictly newer and holding one more document, so a drill that restores
// it proves selection by the snapshots' own instants.
const seedScript = `set -e
mkdir -p /tmp/repo
(opensearch -E discovery.type=single-node -E plugins.security.disabled=true \
  -E node.store.allow_mmap=false -E path.repo=/tmp/repo > /tmp/os.log 2>&1 &)
i=0
until curl -sf -o /dev/null http://127.0.0.1:9200/_cluster/health; do
  i=$((i+1)); [ "$i" -gt 90 ] && { tail -n 20 /tmp/os.log >&2; exit 1; }
  sleep 2
done
curl -sf -XPUT http://127.0.0.1:9200/orders -H 'Content-Type: application/json' \
  --data-binary '{"settings":{"number_of_shards":1,"number_of_replicas":0}}' > /dev/null
for n in 1 2 3; do
  curl -sf -XPOST "http://127.0.0.1:9200/orders/_doc/$n?refresh=true" -H 'Content-Type: application/json' \
    --data-binary "{\"sku\":\"item-$n\",\"qty\":$n}" > /dev/null
done
curl -sf -XPUT http://127.0.0.1:9200/_snapshot/seed -H 'Content-Type: application/json' \
  --data-binary '{"type":"fs","settings":{"location":"/tmp/repo"}}' > /dev/null
curl -sf -XPUT 'http://127.0.0.1:9200/_snapshot/seed/snap-1?wait_for_completion=true' > /dev/null
sleep 2
curl -sf -XPOST 'http://127.0.0.1:9200/orders/_doc/4?refresh=true' -H 'Content-Type: application/json' \
  --data-binary '{"sku":"item-4","qty":4}' > /dev/null
curl -sf -XPUT 'http://127.0.0.1:9200/_snapshot/seed/snap-2?wait_for_completion=true' > /dev/null
tar -czf /tmp/repo.tar.gz -C /tmp repo`

// makeFixtures seeds the repository in a disposable container and
// extracts the artifact forms this adapter restores, produced the way
// the README tells operators to produce them.
func makeFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, image string,
	repoDest, tarDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	for src, dest := range map[string]string{"/tmp/repo": repoDest, "/tmp/repo.tar.gz": tarDest} {
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
// as separate processes — restore a genuine fs repository's newest
// snapshot, pass the census and shard gates, and validate the restored
// documents through the probe-declared SQL runner.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "repo")
	makeFixtures(t, ctx, provider, image, fixture, "")

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("opensearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "opensearch" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "opensearch_repo", Path: fixture},
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
	if res.SourceIdentity.CreatedAt == nil {
		t.Fatal("created_at = nil, want the restored snapshot's own instant")
	}
	if _, err := time.Parse(time.RFC3339, *res.SourceIdentity.CreatedAt); err != nil {
		t.Errorf("created_at = %q does not parse: %v", *res.SourceIdentity.CreatedAt, err)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Four documents prove the newest snapshot won by its own instant:
	// snap-1 holds three, only snap-2 holds four.
	assertCheck(t, ctx, sbx, probe, "SELECT COUNT(*) FROM orders", "4")
	assertCheck(t, ctx, sbx, probe, "SELECT sku FROM orders WHERE qty = 4", "item-4")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestArchiveDrillUnpacksAndServes proves the tar kind end to end: a
// gzip archive of the repository — with the wrapping directory layout
// tar naturally produces — unpacks, restores, and serves the newest
// snapshot's documents.
func TestArchiveDrillUnpacksAndServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "repo.tar.gz")
	makeFixtures(t, ctx, provider, image, "", fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("opensearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "opensearch_repo_tar", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the archive's own claim")
	}
	assertCheck(t, ctx, sbx, probe, "SELECT COUNT(*) FROM orders", "4")
}

// TestCorruptBlobFailsTheShardGate proves the HTTP-200 trap end to end:
// a repository whose data blobs are damaged registers and lists cleanly,
// the restore call returns 200 — and the drill still refuses, because
// the verdict is read from the shard counts (measured).
func TestCorruptBlobFailsTheShardGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "repo")
	makeFixtures(t, ctx, provider, image, fixture, "")
	corruptDataBlobs(t, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("opensearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "opensearch_repo", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" ||
		!strings.Contains(aerr.Message, "shards") {
		t.Fatalf("provision error = %v, want source_corrupt read from the shard counts", err)
	}
}

// corruptDataBlobs flips bytes in the middle of every data blob (the
// `__`-prefixed files hold the segment data, and snapshots share them),
// the damage the engine only admits per shard.
func corruptDataBlobs(t *testing.T, repo string) {
	t.Helper()
	corrupted := 0
	if err := filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(filepath.Base(p), "__") {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mid := len(data) / 2
		for i := mid; i < mid+64 && i < len(data); i++ {
			data[i] ^= 0xFF
		}
		corrupted++
		return os.WriteFile(p, data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	if corrupted == 0 {
		t.Fatal("no data blobs found in the repository fixture")
	}
}

// TestLiveDataDirIsRefusedByName drives the raw-copy fence end to end:
// a directory carrying a nodes/ entry is a copy of a live data
// directory and is refused before a byte reaches the sandbox.
func TestLiveDataDirIsRefusedByName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	fixture := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fixture, "nodes", "0"), 0o755); err != nil {
		t.Fatal(err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(verifiedImage(t)))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("opensearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "opensearch_repo", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "unsupported_source" ||
		!strings.Contains(aerr.Message, "fs") {
		t.Fatalf("provision error = %v, want unsupported_source teaching the fs repository", err)
	}
}

// assertCheck runs one OpenSearch SQL check through the probe-declared
// runner — exactly how internal/checks runs checks without engine
// knowledge — and asserts the output carries the wanted fragment.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, checkText, wantFragment string) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if out.ExitCode != 0 || !strings.Contains(string(out.Stdout), wantFragment) {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want it to carry %q",
			checkText, out.Stdout, out.ExitCode, out.Stderr, wantFragment)
	}
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-opensearch"), ".").CombinedOutput(); err != nil {
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
