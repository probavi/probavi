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
	"github.com/probavi/probavi/internal/checks"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/sandbox"
	"github.com/probavi/probavi/internal/sandbox/docker"
)

// verifiedImage is the official image this run's server comes from: the
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

// wrapperImage builds (once per base, cached afterwards by docker) the
// image a drill sandbox actually runs: the official image with its
// entrypoint pin lifted — the exact recipe the adapter README documents,
// because the official images cannot idle (measured).
func wrapperImage(t *testing.T, ctx context.Context, base string) string {
	t.Helper()
	tag := "probavi-it-prometheus:" + strings.ReplaceAll(base[strings.LastIndex(base, ":")+1:], "/", "-")
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
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "512m"}
}

// makeFixtures seeds a real self-scraping server in a wrapper container,
// takes genuine API snapshots, and extracts them — optionally two
// snapshots (the second strictly newer) and a gzip tar of the first, the
// artifact forms this adapter restores, produced the way the README
// tells operators to produce them.
func makeFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, image string,
	snapDest, secondSnapDest, tarDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
cat > /tmp/cfg.yml <<CFG
global:
  scrape_interval: 1s
scrape_configs:
  - job_name: self
    static_configs:
      - targets: ['127.0.0.1:9090']
CFG
prometheus --config.file=/tmp/cfg.yml --storage.tsdb.path=/tmp/data \
  --web.listen-address=127.0.0.1:9090 --web.enable-admin-api > /tmp/prom.log 2>&1 &
i=0
until wget -q -O /dev/null http://127.0.0.1:9090/-/ready 2>/dev/null; do
  i=$((i+1)); [ "$i" -gt 60 ] && { tail -n 5 /tmp/prom.log >&2; exit 1; }
  sleep 1
done
sleep 8
name=$(wget -q -O- --post-data='' http://127.0.0.1:9090/api/v1/admin/tsdb/snapshot | sed 's/.*"name":"\([^"]*\)".*/\1/')
cp -r "/tmp/data/snapshots/$name" /tmp/snap1
tar -czf /tmp/snap1.tar.gz -C /tmp/data/snapshots "$name"
sleep 4
name2=$(wget -q -O- --post-data='' http://127.0.0.1:9090/api/v1/admin/tsdb/snapshot | sed 's/.*"name":"\([^"]*\)".*/\1/')
cp -r "/tmp/data/snapshots/$name2" /tmp/snap2`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	for src, dest := range map[string]string{
		"/tmp/snap1": snapDest, "/tmp/snap2": secondSnapDest, "/tmp/snap1.tar.gz": tarDest,
	} {
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
// as separate processes — restore a genuine API snapshot, serve it, pass
// the block census, and validate the restored series through the
// probe-declared runner at the backup's own instant.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "snap")
	makeFixtures(t, ctx, provider, image, fixture, "", "")

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "prometheus" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot", Path: fixture},
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
		t.Fatal("created_at = nil, want the newest instant the backup's own blocks claim")
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

	// The check dialect this adapter documents: PromQL through the
	// probe-declared template, evaluated at the backup's own instant.
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "count(up)", "1")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		"sum(max_over_time(up[1h]))", "1")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestArchiveDrillUnpacksAndServes proves the tar kind end to end: a
// gzip archive of a snapshot — with the wrapping directory layout tar
// naturally produces — unpacks, serves, and answers at the instant the
// archive's own table of contents claimed.
func TestArchiveDrillUnpacksAndServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "snap.tar.gz")
	makeFixtures(t, ctx, provider, image, "", "", fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot_tar", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the archive's own claim")
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "count(up)", "1")
}

// TestCompactionWindowSnapshotPassesTheCensus reproduces issue #155
// against a measured server: the snapshot holds both a compacted block
// and the source it replaced — the shape POST /api/v1/admin/tsdb/snapshot
// produces when it hardlinks a data directory mid-compaction — and the
// server loads every block except the superseded source. The drill must
// call that a full restore, and the census must have subtracted exactly
// the block the server skipped.
func TestCompactionWindowSnapshotPassesTheCensus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "snap")
	makeFixtures(t, ctx, provider, image, fixture, "", "")
	required := countBlockDirs(t, fixture)
	supersedeOneBlock(t, fixture)
	if got := countBlockDirs(t, fixture); got != required+1 {
		t.Fatalf("fixture holds %d block directories after superseding, want %d", got, required+1)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v — a compaction-window snapshot is healthy and must restore", err)
	}

	// The measured heart of the test: the server's own metric proves it
	// skipped exactly the superseded source, so the census can only have
	// passed by subtracting it — not by counting directories.
	if loaded := blocksLoaded(t, ctx, sbx); loaded != required {
		t.Errorf("server loaded %d blocks with %d directories present, want %d — "+
			"the superseded source must be the one block it skips", loaded, required+1, required)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v (%v), want healthy", health, err)
	}
}

// countBlockDirs counts the block directories a snapshot holds.
func countBlockDirs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

// supersedeOneBlock copies one real block under a fresh ULID and marks
// the copy as the original's compaction child — the compacted block a
// mid-window snapshot holds beside its still-present source. The copy is
// a byte-identical, fully valid block, so the server serves it exactly
// as it would the real product of a compaction.
func supersedeOneBlock(t *testing.T, snapshotDir string) {
	t.Helper()
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	source := ""
	for _, e := range entries {
		if e.IsDir() {
			source = e.Name()
			break
		}
	}
	if source == "" {
		t.Fatal("no block directory to supersede")
	}
	twin := twinULIDOf(source)
	if err := os.CopyFS(filepath.Join(snapshotDir, twin), os.DirFS(filepath.Join(snapshotDir, source))); err != nil {
		t.Fatalf("copy block: %v", err)
	}

	metaPath := filepath.Join(snapshotDir, twin, "meta.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&meta); err != nil {
		t.Fatalf("decode meta.json: %v", err)
	}
	meta["ulid"] = twin
	compaction, _ := meta["compaction"].(map[string]any)
	if compaction == nil {
		compaction = map[string]any{}
	}
	compaction["level"] = 2
	compaction["parents"] = []map[string]any{
		{"ulid": source, "minTime": meta["minTime"], "maxTime": meta["maxTime"]},
	}
	meta["compaction"] = compaction
	rewritten, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
}

// twinULIDOf derives a distinct, still-valid ULID from a real one by
// swapping the last character within the ULID alphabet.
func twinULIDOf(u string) string {
	if strings.HasSuffix(u, "Z") {
		return u[:len(u)-1] + "Y"
	}
	return u[:len(u)-1] + "Z"
}

// TestCorruptIndexSurfacesTheServerLog proves a damaged block yields the
// right verdict through the whole stack: the server refuses to start
// (measured), and its own log line reaches the drill as a claim about
// the backup — within seconds, not after the readiness budget.
// The backdated fixture: four groups of six samples, the oldest 32 days
// before the newest 2 — a span of 30 days, twice the 15-day window the
// server applies when nothing pins retention off.
const (
	historyMetric   = "probavi_history"
	historyOffsets  = "32 22 12 2"
	historySpanDays = 30
	historySamples  = 24
)

// TestLongHistorySnapshotSurvivesRetention proves the sandbox server
// applies no retention policy of its own to the artifact. Its fixture is
// a snapshot spanning more than the server's default 15-day window — the
// ordinary shape of a monitoring history kept for compliance — and it
// must restore whole. Measured, a server started without the retention
// flags pinned deletes every block outside that window from the restored
// copy as the TSDB opens, which fails a census that should pass and, more
// quietly, shrinks what every check afterwards can read.
func TestLongHistorySnapshotSurvivesRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "snap")
	makeBackdatedFixture(t, ctx, provider, image, fixture)
	required := countBlockDirs(t, fixture)
	if span := blockSpan(t, fixture); span <= 15*24*time.Hour {
		t.Fatalf("fixture spans %s over %d blocks, want more than the default retention window",
			span, required)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v — a snapshot longer than the default retention window is healthy", err)
	}

	if loaded := blocksLoaded(t, ctx, sbx); loaded != required {
		t.Errorf("server loaded %d of the %d blocks the snapshot holds — "+
			"the sandbox must not apply retention to the artifact", loaded, required)
	}
	if deleted := blocksDeleted(t, ctx, sbx, res.State); deleted != 0 {
		t.Errorf("server deleted %d blocks from the restored copy, want none", deleted)
	}

	// The operator-visible half: a range check reads every sample the
	// backup holds, including those outside the default window. A server
	// enforcing retention answers this one with a number too small and
	// nothing anywhere reports a failure.
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		fmt.Sprintf("sum(count_over_time(%s[%dd]))", historyMetric, historySpanDays+5),
		strconv.Itoa(historySamples))

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v (%v), want healthy", health, err)
	}
}

// makeBackdatedFixture builds a snapshot no live server could hand out
// in one piece: real blocks, real chunks and index, carrying sample
// groups spread over a month. promtool writes them from an OpenMetrics
// stream — the one supported way to produce blocks for instants long
// past — and its output is block directories only, so what lands here is
// a snapshot in shape as well as in content (no wal, no chunks_head, no
// lock, which the raw-copy fence would refuse).
func makeBackdatedFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, importParams(image))
	if err != nil {
		t.Fatalf("create fixture sandbox: %v", err)
	}
	defer destroy(t, seed)

	script := fmt.Sprintf(`set -e
now=$(date +%%s)
{
  echo '# HELP %[1]s A backdated gauge'
  echo '# TYPE %[1]s gauge'
  i=0
  for off in %[2]s; do
    base=$((now - off * 86400))
    n=0
    while [ $n -lt 6 ]; do
      echo "%[1]s{job=\"history\"} $i $((base + n * 300))"
      i=$((i + 1))
      n=$((n + 1))
    done
  done
  echo '# EOF'
} > /tmp/history.om
mkdir -p /tmp/blocks
promtool tsdb create-blocks-from openmetrics /tmp/history.om /tmp/blocks`,
		historyMetric, historyOffsets)
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("fixture exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("build backdated blocks: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/blocks", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// importParams gives the fixture sandbox the memory promtool's block
// writer needs: measured, the import is OOM-killed at the 512m the drill
// sandboxes run with, and completes at 768m.
func importParams(image string) map[string]string {
	params := sandboxParams(image)
	params["memory"] = "1g"
	return params
}

// blockSpan reports what the snapshot's own blocks claim to cover, so a
// fixture that quietly stopped being longer than the retention window
// fails loudly instead of turning its test into a duplicate happy path.
func blockSpan(t *testing.T, dir string) time.Duration {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var minMs, maxMs int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json"))
		if err != nil {
			t.Fatalf("read block metadata: %v", err)
		}
		var meta struct {
			MinTime int64 `json:"minTime"`
			MaxTime int64 `json:"maxTime"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("parse block metadata: %v", err)
		}
		if minMs == 0 || meta.MinTime < minMs {
			minMs = meta.MinTime
		}
		if meta.MaxTime > maxMs {
			maxMs = meta.MaxTime
		}
	}
	return time.Duration(maxMs-minMs) * time.Millisecond
}

// blocksLoaded reads the restored server's own count of loaded blocks.
func blocksLoaded(t *testing.T, ctx context.Context, sbx *docker.Sandbox) int {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c",
		"wget -q -O- http://127.0.0.1:9090/metrics | grep '^prometheus_tsdb_blocks_loaded'"}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("read blocks_loaded: %v (exit %d, stderr %s)", err, out.ExitCode, out.Stderr)
	}
	fields := strings.Fields(strings.TrimSpace(string(out.Stdout)))
	if len(fields) != 2 {
		t.Fatalf("blocks_loaded line = %q", out.Stdout)
	}
	loaded, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("blocks_loaded value = %q: %v", fields[1], err)
	}
	return loaded
}

// blocksDeleted counts what the server removed from the restored copy,
// read from the log the adapter starts it with. This is the destructive
// half of inherited retention: the blocks are gone from disk, not merely
// unloaded.
func blocksDeleted(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	state json.RawMessage) int {
	t.Helper()
	var s struct {
		WorkDir string `json:"work_dir"`
	}
	if err := json.Unmarshal(state, &s); err != nil || s.WorkDir == "" {
		t.Fatalf("provision state = %s: %v", state, err)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c",
		"grep -c 'Deleting obsolete block' " + s.WorkDir + "/prometheus.log || true"}})
	if err != nil {
		t.Fatalf("read server log: %v", err)
	}
	deleted, err := strconv.Atoi(strings.TrimSpace(string(out.Stdout)))
	if err != nil {
		t.Fatalf("deletion count = %q: %v", out.Stdout, err)
	}
	return deleted
}

func TestCorruptIndexSurfacesTheServerLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "snap")
	makeFixtures(t, ctx, provider, image, fixture, "", "")
	blocks, err := os.ReadDir(fixture)
	if err != nil || len(blocks) == 0 {
		t.Fatalf("fixture blocks: %v (%d)", err, len(blocks))
	}
	index := filepath.Join(fixture, blocks[0].Name(), "index")
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	for i := 2000; i < 2100 && i < len(data); i++ {
		data[i] ^= 0xFF
	}
	if err := os.WriteFile(index, data, 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	begin := time.Now()
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" ||
		!strings.Contains(aerr.Message, "checksum") {
		t.Fatalf("provision error = %v, want source_corrupt carrying the server's log line", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Minute {
		t.Errorf("verdict took %v — the fatal-line watch must beat the readiness budget", elapsed)
	}
}

// TestLiveDataDirIsRefusedByName drives the raw-copy fence end to end:
// a snapshot-shaped directory carrying a wal/ is a copy of a live data
// directory and is refused before a byte reaches the sandbox.
func TestLiveDataDirIsRefusedByName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "data")
	makeFixtures(t, ctx, provider, image, fixture, "", "")
	if err := os.MkdirAll(filepath.Join(fixture, "wal"), 0o755); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "unsupported_source" ||
		!strings.Contains(aerr.Message, "snapshot API") {
		t.Fatalf("provision error = %v, want unsupported_source teaching the snapshot API", err)
	}
}

// TestSnapshotDirDrillPicksTheClaimedNewest proves the directory kind
// ranks by what each snapshot's own blocks claim: a decoy mtime on the
// older snapshot must not win.
func TestSnapshotDirDrillPicksTheClaimedNewest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	base := t.TempDir()
	older := filepath.Join(base, "snap-a")
	newest := filepath.Join(base, "snap-b")
	makeFixtures(t, ctx, provider, image, older, newest, "")
	// The decoy: the newest snapshot's directory looks stale on disk.
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(newest, past, past); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot_dir", Path: base},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// The two snapshots came from one server run, seconds apart; the
	// restored claim must match the second snapshot's own newest instant,
	// which the first cannot reach.
	olderInfo, perr := resolveOwnClaim(older)
	if perr != nil {
		t.Fatal(perr)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Fatal("created_at = nil")
	}
	restored, err := time.Parse(time.RFC3339, *res.SourceIdentity.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.After(olderInfo) {
		t.Errorf("restored claim %v does not exceed the older snapshot's %v — the mtime decoy won", restored, olderInfo)
	}
}

// resolveOwnClaim reads the newest instant a snapshot's blocks claim,
// the same way the adapter ranks candidates.
func resolveOwnClaim(dir string) (time.Time, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, err
	}
	var maxMs int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			MaxTime int64 `json:"maxTime"`
		}
		if err := json.Unmarshal(raw, &meta); err == nil && meta.MaxTime > maxMs {
			maxMs = meta.MaxTime
		}
	}
	return time.UnixMilli(maxMs).UTC(), nil
}

// assertCheck runs one PromQL check through the probe-declared runner —
// exactly how internal/checks runs checks without engine knowledge: the
// core substitutes {{database}} from the connection provision returned,
// and {{sql}} with the check text — and asserts the output carries the
// wanted fragment.
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
	// Exact equality, not a fragment: this is what the core does with a
	// check's expect (internal/checks), and comparing any other way here
	// would hide the decoration issue #175 was about.
	if got := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want exactly %q",
			checkText, out.Stdout, out.ExitCode, out.Stderr, want)
	}
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-prometheus"), ".").CombinedOutput(); err != nil {
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

// TestCustomCheckPassesThroughTheCore is issue #175 end to end, driven by
// the core's own check machinery rather than by a fragment match.
//
// The defect was that `expect` is compared against the runner's whole
// trimmed stdout, while the runner printed promtool's annotated sample —
// so no custom check could pass, and none could ever be written to,
// because the line ends with an evaluation instant that changes with every
// backup.
//
// The counter-assertion matters as much as the first: an expect that is
// wrong must still fail, or this would only prove that checks stopped
// meaning anything.
func TestCustomCheckPassesThroughTheCore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := wrapperImage(t, ctx, verifiedImage(t))
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "snap")
	makeFixtures(t, ctx, provider, image, fixture, "", "")

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("prometheus", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "prometheus_snapshot", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	deps := checks.Deps{
		Exec:   sbx,
		Runner: checks.Runner{Argv: probe.SQLRunner.Argv, Env: probe.SQLRunner.Env},
		Target: checks.Target{User: res.Connection.User, Database: res.Connection.Database},
	}
	query := "count(up)"
	value := "1"

	results, err := checks.Run(ctx, []config.Check{
		{Name: "history_survived", SQL: query, Expect: config.ScalarFromString(value)},
		{Name: "wrong_on_purpose", SQL: query, Expect: config.ScalarFromString(value + "0")},
	}, deps)
	if err != nil {
		t.Fatalf("checks.Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want two", results)
	}
	if !results[0].OK {
		t.Errorf("check %q = %+v, want it to pass — a custom check must be able to (#175)",
			query, results[0])
	}
	if results[1].OK {
		t.Errorf("a check expecting the wrong value passed: %+v", results[1])
	}
}
