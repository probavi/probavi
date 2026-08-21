//go:build integration

package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// verifiedImage is the official image this run's node comes from: the
// manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE when
// it names one the manifest already lists (docs/engine-versions.md §2).
// No wrapper is needed: the official images idle under sleep infinity
// (their entrypoint passes non-elasticsearch commands through, measured).
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
	// The image sizes its heap from the container limit; 2g keeps the
	// node inside the cap while leaving room for the restore.
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "2g"}
}

// launchNode runs a node the way the adapter does — loopback dev mode,
// the JDK hosts file the 8.x line needs under --network none, the
// repository path allowed — plus whatever EXTRA_SETTINGS the caller
// passes, and waits for it. Seed nodes keep the engine's lifecycle
// defaults: a fixture must be what production would have produced.
const launchNode = `mkdir -p /tmp/repo
printf '127.0.0.1 localhost %s\n::1 localhost\n' "$HOSTNAME" > /tmp/hosts
(ES_JAVA_OPTS="-Djdk.net.hosts.file=/tmp/hosts" elasticsearch -E discovery.type=single-node \
  -E xpack.security.enabled=false -E node.store.allow_mmap=false -E path.repo=/tmp/repo \
  -E ingest.geoip.downloader.enabled=false ${EXTRA_SETTINGS:-} > /tmp/es.log 2>&1 &)
i=0
until curl -sf -o /dev/null http://127.0.0.1:9200/_cluster/health; do
  i=$((i+1)); [ "$i" -gt 90 ] && { tail -n 20 /tmp/es.log >&2; exit 1; }
  sleep 2
done
H='Content-Type: application/json'
`

// seedScript seeds an index and takes two genuine snapshots into an fs
// repository — the second strictly newer and holding one more document,
// so a drill that restores it proves selection by the snapshots' own
// instants. The repository is also zipped the way the README tells
// operators to (the wrapping-directory layout `zip -r` produces).
const seedScript = `set -e
` + launchNode + `
curl -sf -XPUT http://127.0.0.1:9200/orders -H "$H" \
  --data-binary '{"settings":{"number_of_shards":1,"number_of_replicas":0},"mappings":{"properties":{"created_at":{"type":"date"}}}}' > /dev/null
for n in 1 2 3; do
  curl -sf -XPOST "http://127.0.0.1:9200/orders/_doc/$n?refresh=true" -H "$H" \
    --data-binary "{\"sku\":\"item-$n\",\"qty\":$n,\"created_at\":\"2026-08-0${n}T10:00:00Z\"}" > /dev/null
done
curl -sf -XPUT http://127.0.0.1:9200/_snapshot/seed -H "$H" \
  --data-binary '{"type":"fs","settings":{"location":"/tmp/repo"}}' > /dev/null
curl -sf -XPUT 'http://127.0.0.1:9200/_snapshot/seed/snap-1?wait_for_completion=true' > /dev/null
sleep 2
curl -sf -XPOST 'http://127.0.0.1:9200/orders/_doc/4?refresh=true' -H "$H" \
  --data-binary '{"sku":"item-4","qty":4,"created_at":"2026-08-04T10:00:00Z"}' > /dev/null
curl -sf -XPUT 'http://127.0.0.1:9200/_snapshot/seed/snap-2?wait_for_completion=true' > /dev/null
(cd /tmp && zip -qr repo.zip repo)`

// lifecycleSeedScript builds the fixture the lifecycle measurement used:
// two data streams, each rolled over so an older generation exists for
// the engine to judge — one under the built-in `7-days-default` ILM
// policy (every fresh node ships it), one under a one-day data stream
// retention — with every backing index dated past its age through
// `index.lifecycle.origination_date`, which is how both machineries
// measure age and how a backup older than its own retention looks when
// it is restored. The snapshot is the production default (global state
// included). The script fails if a generation is missing, because a
// fixture without one would prove nothing.
const lifecycleSeedScript = `set -e
` + launchNode + `
curl -sf -XPUT http://127.0.0.1:9200/_index_template/ilm-drill -H "$H" --data-binary \
  '{"index_patterns":["ilm-drill-*"],"priority":500,"data_stream":{},"template":{"settings":{"number_of_replicas":0,"index.lifecycle.name":"7-days-default"}}}' > /dev/null
curl -sf -XPUT http://127.0.0.1:9200/_index_template/dlm-drill -H "$H" --data-binary \
  '{"index_patterns":["dlm-drill-*"],"priority":500,"data_stream":{},"template":{"settings":{"number_of_replicas":0},"lifecycle":{"data_retention":"1d"}}}' > /dev/null
for ds in ilm-drill-app dlm-drill-app; do
  for n in 1 2 3; do
    curl -sf -XPOST "http://127.0.0.1:9200/$ds/_doc?refresh=true" -H "$H" \
      --data-binary "{\"@timestamp\":\"2026-01-0${n}T00:00:00Z\",\"msg\":\"m$n\"}" > /dev/null
  done
  curl -sf -XPOST "http://127.0.0.1:9200/$ds/_rollover" > /dev/null
  for n in 4 5; do
    curl -sf -XPOST "http://127.0.0.1:9200/$ds/_doc?refresh=true" -H "$H" \
      --data-binary "{\"@timestamp\":\"2026-08-2${n}T00:00:00Z\",\"msg\":\"m$n\"}" > /dev/null
  done
done
backing=$(curl -s 'http://127.0.0.1:9200/_cat/indices/.ds-*?h=index')
[ "$(printf '%s\n' "$backing" | wc -l)" -eq 4 ] || { echo "backing indices: $backing" >&2; exit 1; }
for idx in $backing; do
  curl -sf -XPUT "http://127.0.0.1:9200/$idx/_settings" -H "$H" \
    --data-binary '{"index.lifecycle.origination_date":1767225600000}' > /dev/null
done
curl -sf -XPUT http://127.0.0.1:9200/_snapshot/seed -H "$H" \
  --data-binary '{"type":"fs","settings":{"location":"/tmp/repo"}}' > /dev/null
curl -sf -XPUT 'http://127.0.0.1:9200/_snapshot/seed/snap-1?wait_for_completion=true' -H "$H" \
  --data-binary '{"indices":"*"}' | grep -q '"state":"SUCCESS"'`

// controlRestoreScript is what a drill without the adapter's pins would
// do: register the fixture repository and restore it into a node whose
// lifecycle machinery runs at the engine's defaults — accelerated to
// five-second polls so the verdict arrives in seconds rather than
// minutes — then watch the two data streams for ninety seconds and
// report the first moment a generation is gone.
const controlRestoreScript = `set -e
` + launchNode + `
curl -sf -XPUT http://127.0.0.1:9200/_snapshot/probavi -H "$H" \
  --data-binary '{"type":"fs","settings":{"location":"/tmp/repo","readonly":true}}' > /dev/null
curl -sf -XPOST 'http://127.0.0.1:9200/_snapshot/probavi/snap-1/_restore?wait_for_completion=true' -H "$H" \
  --data-binary '{"indices":"*","index_settings":{"index.number_of_replicas":0}}' | grep -q '"failed":0'
for i in $(seq 1 18); do
  sleep 5
  ilm=$(curl -s http://127.0.0.1:9200/ilm-drill-app/_count | sed 's/.*"count":\([0-9]*\).*/\1/')
  dlm=$(curl -s http://127.0.0.1:9200/dlm-drill-app/_count | sed 's/.*"count":\([0-9]*\).*/\1/')
  if [ "$ilm" -lt 5 ] || [ "$dlm" -lt 5 ]; then echo "ilm=$ilm dlm=$dlm after $((i*5))s"; exit 0; fi
done
echo "ilm=$ilm dlm=$dlm after 90s: nothing expired"; exit 3`

// TestLifecyclePoliciesDoNotRunInTheDrill is this adapter's instance of
// the data-lifecycle rule (issue #166), proven from both sides.
//
// The control half restores the fixture into a node exactly as the
// engine ships it and shows the loss: within ninety seconds a generation
// is deleted by the built-in ILM policy the backing index names, or by
// the retention the data stream carries, while the restore stood as a
// success. Without this half the drill half would prove nothing — a
// fixture that is not deadly cannot show a pin working.
//
// The drill half provisions through the adapter, reads both pins back
// through the engine (ILM STOPPED, the data stream lifecycle poll
// interval a hundred years), then forces ILM's poll interval down to
// one second — harsher than any default — and shows every generation
// and every document surviving, with the retention the backup declared
// still readable on the data stream. Remove either pin and this test
// goes red.
func TestLifecyclePoliciesDoNotRunInTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	repo := filepath.Join(t.TempDir(), "repo")
	seedFixture(t, ctx, provider, image, lifecycleSeedScript, map[string]string{"/tmp/repo": repo})

	t.Run("the fixture expires without the pins", func(t *testing.T) {
		control, err := provider.Create(ctx, sandboxParams(image))
		if err != nil {
			t.Fatalf("create control sandbox: %v", err)
		}
		defer destroy(t, control)
		copyIntoSandbox(t, ctx, control, repo, "/tmp/repo")
		res, err := control.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"bash", "-c", controlRestoreScript},
			Env: map[string]string{"EXTRA_SETTINGS": "-E indices.lifecycle.poll_interval=5s " +
				"-E data_streams.lifecycle.poll_interval=5s"},
		})
		if err != nil {
			t.Fatalf("control exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("the control node kept every document (exit %d: %s %s) — the fixture proves "+
				"nothing, and neither would the drill half", res.ExitCode, res.Stdout, res.Stderr)
		}
		t.Logf("control node without the pins: %s", strings.TrimSpace(string(res.Stdout)))
	})

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("elasticsearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "elasticsearch_repo", Path: repo},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision: %v", err)
	}

	if got := curlSandbox(t, ctx, sbx, "/_ilm/status"); !strings.Contains(got, `"STOPPED"`) {
		t.Errorf("_ilm/status = %s, want STOPPED before anything the artifact carries can run", got)
	}
	settings := curlSandbox(t, ctx, sbx, "/_cluster/settings?include_defaults=true&flat_settings=true")
	if !strings.Contains(settings, `"data_streams.lifecycle.poll_interval":"876000h"`) {
		t.Errorf("cluster settings carry no hundred-year data stream lifecycle poll interval: %s",
			truncate(settings, 400))
	}

	// ILM is stopped, so its poll interval must not matter — one second is
	// the proof that the stop, not the clock, is what protects the drill.
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"curl", "-sf", "-XPUT",
		"http://127.0.0.1:9200/_cluster/settings", "-H", "Content-Type: application/json",
		"--data-binary", `{"persistent":{"indices.lifecycle.poll_interval":"1s"}}`}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("accelerate ILM polling: %v (exit %d)", err, res.ExitCode)
	}
	time.Sleep(30 * time.Second)

	for _, ds := range []string{"ilm-drill-app", "dlm-drill-app"} {
		if got := jsonField(t, ctx, sbx, "/"+ds+"/_count", "count"); got != 5 {
			t.Errorf("%s holds %v documents after 30s, want all 5: the backup's lifecycle ran in the drill", ds, got)
		}
	}
	backing := curlSandbox(t, ctx, sbx, "/_cat/indices/.ds-*drill*?h=index")
	if n := len(strings.Fields(backing)); n != 4 {
		t.Errorf("backing indices = %q, want all four generations", backing)
	}

	// Suspended, not rewritten: the policy the operator declared is still
	// readable on the restored artifact.
	retention := curlSandbox(t, ctx, sbx, "/_data_stream/dlm-drill-app?filter_path=data_streams.lifecycle.data_retention")
	if !strings.Contains(retention, `"1d"`) {
		t.Errorf("data stream retention = %s, want the 1d the backup declared", retention)
	}
	policy := curlSandbox(t, ctx, sbx, "/.ds-ilm-drill-app-*-000001/_settings?flat_settings=true")
	if !strings.Contains(policy, `"index.lifecycle.name":"7-days-default"`) {
		t.Errorf("restored settings = %s, want the policy name the backup carried", truncate(policy, 300))
	}
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine fs repository's newest
// snapshot, pass the census and shard gates, and validate the restored
// documents through the core's own built-in checks: the generating
// kinds apply to this engine (the dialect takes SQL-standard quoted
// identifiers and answers max() of a date as an RFC 3339 instant,
// measured), the first non-relational engine in the catalog where they
// do.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "repo")
	seedFixture(t, ctx, provider, image, seedScript, map[string]string{"/tmp/repo": fixture})

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("elasticsearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "elasticsearch" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "elasticsearch_repo", Path: fixture},
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
	// snap-1 holds three, only snap-2 holds four. The checks are the
	// core's own — generated SQL, the freshness age computed in Go
	// against an injected clock — plus a custom check with its expect.
	four := int64(4)
	now := func() time.Time { return time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC) }
	deps := checks.Deps{
		Exec:   sbx,
		Runner: checks.Runner{Argv: probe.SQLRunner.Argv, Env: probe.SQLRunner.Env},
		Target: checks.Target{User: res.Connection.User, Database: res.Connection.Database},
		Now:    now,
	}
	results, err := checks.Run(ctx, []config.Check{
		{Name: "orders_exists", Builtin: config.CheckTableExists, Table: "orders"},
		{Name: "orders_rows", Builtin: config.CheckRowCount, Table: "orders", Min: &four, Max: &four},
		{Name: "orders_fresh", Builtin: config.CheckFreshness, Table: "orders", Column: "created_at",
			MaxAge: config.Duration(2 * 24 * time.Hour)},
		{Name: "orders_stale", Builtin: config.CheckFreshness, Table: "orders", Column: "created_at",
			MaxAge: config.Duration(time.Hour)},
		{Name: "newest_sku", SQL: "SELECT sku FROM orders WHERE qty = 4", Expect: config.ScalarFromString("item-4")},
		{Name: "missing_index", Builtin: config.CheckTableExists, Table: "nope"},
	}, deps)
	if err != nil {
		t.Fatalf("checks.Run: %v", err)
	}
	// The core labels results by kind and target, and runs them in order.
	want := []bool{true, true, true, false, true, false}
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want %d", results, len(want))
	}
	for i, r := range results {
		if r.OK != want[i] {
			t.Errorf("check %d (%s) = %+v, want ok=%v", i, r.Name, r, want[i])
		}
	}

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestArchiveDrillUnpacksAndServes proves the zip kind end to end: an
// archive of the repository — with the wrapping directory layout
// `zip -r` naturally produces — unpacks, restores, and serves the newest
// snapshot's documents.
func TestArchiveDrillUnpacksAndServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "repo.zip")
	seedFixture(t, ctx, provider, image, seedScript, map[string]string{"/tmp/repo.zip": fixture})

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("elasticsearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "elasticsearch_repo_zip", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the archive's own claim")
	}
	assertCheck(t, ctx, sbx, probe, `SELECT COUNT(*) FROM "orders"`, "4")
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
	seedFixture(t, ctx, provider, image, seedScript, map[string]string{"/tmp/repo": fixture})
	corruptDataBlobs(t, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("elasticsearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "elasticsearch_repo", Path: fixture},
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

	runner, err := adapter.New("elasticsearch", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "elasticsearch_repo", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "unsupported_source" ||
		!strings.Contains(aerr.Message, "fs") {
		t.Fatalf("provision error = %v, want unsupported_source teaching the fs repository", err)
	}
}

// seedFixture runs a seed script in a disposable container and extracts
// the named artifacts, produced the way the README tells operators to
// produce them.
func seedFixture(t *testing.T, ctx context.Context, provider *docker.Provider, image, script string,
	extract map[string]string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c", script}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	for src, dest := range extract {
		if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":"+src, dest).CombinedOutput(); err != nil {
			t.Fatalf("extract fixture: %v: %s", err, out)
		}
	}
}

// copyIntoSandbox places a host tree inside a sandbox the test drives
// by hand (the control node), readable by the engine's user.
func copyIntoSandbox(t *testing.T, ctx context.Context, sbx *docker.Sandbox, src, dest string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp", src, sbx.ID()+":"+dest).CombinedOutput(); err != nil {
		t.Fatalf("copy fixture in: %v: %s", err, out)
	}
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"chmod", "-R", "a+rX", dest}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("chmod fixture: %v (exit %d: %s)", err, res.ExitCode, res.Stderr)
	}
}

// curlSandbox asks the restored node one question over loopback.
func curlSandbox(t *testing.T, ctx context.Context, sbx *docker.Sandbox, path string) string {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"curl", "-s", "http://127.0.0.1:9200" + path}})
	if err != nil {
		t.Fatalf("curl %s: %v", path, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("curl %s: exit %d: %s", path, res.ExitCode, res.Stderr)
	}
	return string(res.Stdout)
}

// jsonField reads one numeric field out of an API answer.
func jsonField(t *testing.T, ctx context.Context, sbx *docker.Sandbox, path, field string) any {
	t.Helper()
	body := curlSandbox(t, ctx, sbx, path)
	got := map[string]any{}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("parse %s: %v: %s", path, err, body)
	}
	value, ok := got[field]
	if !ok {
		t.Fatalf("%s carries no %s: %s", path, field, body)
	}
	if n, isNumber := value.(float64); isNumber {
		return int(n)
	}
	return value
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// assertCheck runs one Elasticsearch SQL check through the probe-declared
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
		filepath.Join(binDir, "probavi-adapter-elasticsearch"), ".").CombinedOutput(); err != nil {
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
