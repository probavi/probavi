//go:build integration

package main_test

import (
	"context"
	"errors"
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

// verifiedImage is the official image this run restores with: the
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

// sandboxParams is the shape the README documents, and the difference
// from every sibling adapter is the command: this image appends it to the
// server's own argv, so `sleep infinity` makes the server exit before it
// serves anything. What belongs here is the engine's own flags.
func sandboxParams(image string) map[string]string {
	return map[string]string{
		"image": image, "command": "--smp 1", "memory": "4GiB",
	}
}

// awaitScript is the readiness wait the adapter itself performs; the
// fixture needs the same one before it can seed.
const awaitScript = `for i in $(seq 1 120); do
  cqlsh -e "SELECT release_version FROM system.local;" >/dev/null 2>&1 && break
  sleep 2
done
cqlsh -e "SELECT release_version FROM system.local;" >/dev/null`

// seedScript seeds two tables, snapshots them, and collects the snapshot
// with the exact loop the README documents — so the recipe operators read
// is the recipe the suite proves.
//
// SimpleStrategy is not used, and that is not style: this engine places
// data in tablets by default and a tablet keyspace refuses it outright.
const seedScript = awaitScript + `
{
  echo "CREATE KEYSPACE probavi WITH replication = {'class': 'NetworkTopologyStrategy', 'replication_factor': 1};"
  echo "CREATE TABLE probavi.orders (id int PRIMARY KEY, v text, created_at timestamp);"
  echo "CREATE TABLE probavi.meta (k text PRIMARY KEY, v text);"
  echo "INSERT INTO probavi.meta (k, v) VALUES ('origin', 'restored-ok');"
  for i in $(seq 1 500); do echo "INSERT INTO probavi.orders (id, v, created_at) VALUES ($i, 'row$i', toTimestamp(now()));"; done
} > /tmp/seed.cql
cqlsh -f /tmp/seed.cql
nodetool flush
nodetool snapshot -t drill probavi >/dev/null
dest=/tmp/collect
for snap in /var/lib/scylla/data/probavi/*/snapshots/drill; do
  tbl=${snap%/snapshots/*}; name=$(basename "$tbl"); name=${name%-*}
  mkdir -p "$dest/probavi/$name" && cp -a "$snap/." "$dest/probavi/$name/"
done`

// makeTree seeds a real node in a container of the given image and
// extracts the collected snapshot tree to dest (which must not exist).
func makeTree(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest, script string) {
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
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/collect", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// TestEndToEndRestoreDrill is the whole point: a real snapshot of a real
// node, restored into a fresh sandbox, answering the core's own checks.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collect")
	makeTree(t, ctx, provider, image, tree, seedScript)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("scylladb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "scylladb_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The address is the node's own, not the loopback every sibling uses:
	// this image pins the server to 127.0.0.2 and refuses 127.0.0.1, and
	// the value reaches the evidence record.
	if res.Connection.Host != "127.0.0.2" {
		t.Errorf("connection.host = %q, want 127.0.0.2", res.Connection.Host)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("the snapshot's manifest states an instant; created_at is null")
	}
	if res.Timings.RestoreSeconds <= 0 {
		t.Errorf("restore_seconds = %v, want a measurement", res.Timings.RestoreSeconds)
	}

	// The declared runner, driven exactly as the core drives it — which
	// is what proves the dialect the probe promises, decoration and all.
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT count(*) FROM orders;`, "500")
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT v FROM meta WHERE k = 'origin';`, "restored-ok")
	// The rewrite the runner performs for the core's table_exists probe,
	// which CQL cannot answer as written: it must exit 0 and print
	// nothing at all, so no row of restored data reaches stdout.
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT count(*) FROM "orders" WHERE 1=0`, "")
}

// TestTabletShapeNeedNotMatch is the measurement that makes this adapter
// possible: a production source has many nodes and many tablets, a drill
// sandbox has one. The snapshot is taken from a table the engine split
// into its default tablet count and restored into one the drill created,
// and every row has to come back.
//
// It is the same drill as above, asserted from the other side: the count
// is exact, so a restore that dropped a tablet's sstables would fail here
// rather than pass with fewer rows.
func TestTabletShapeNeedNotMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collect")
	makeTree(t, ctx, provider, image, tree, seedScript)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("scylladb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "scylladb_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c",
		`cqlsh --no-color -e "SELECT count(*) FROM probavi.orders;" | sed -n '4p' | tr -d ' '`}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("count restored rows: %v (exit %d) %s", err, out.ExitCode, out.Stderr)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != "500" {
		t.Errorf("restored rows = %q, want all 500 across every tablet", got)
	}
	_ = res
}

// expiringSeedScript adds one table whose rows expire while the snapshot
// sits on disk, which is every backup of a table with a time-to-live.
var expiringSeedScript = strings.Replace(seedScript,
	`  echo "CREATE TABLE probavi.meta (k text PRIMARY KEY, v text);"`,
	`  echo "CREATE TABLE probavi.meta (k text PRIMARY KEY, v text);"
  echo "CREATE TABLE probavi.sessions (id int PRIMARY KEY, v text) WITH default_time_to_live = 30;"
  for i in $(seq 1 100); do echo "INSERT INTO probavi.sessions (id, v) VALUES ($i, 'session$i');"; done`, 1)

// TestExpiredRowsFailTheDrillInsteadOfPassingAsEmpty is this adapter's
// half of issue #166. The engine filters expired cells on read and offers
// no setting that suspends it, so the drill cannot hold the policy back —
// what it can do is refuse to call a table proven when the artifact says
// it held rows and the engine serves none.
//
// The two healthy tables in the same snapshot are the control: they are
// probed first and must pass, so the refusal is this table's and not the
// fence firing at everything.
func TestExpiredRowsFailTheDrillInsteadOfPassingAsEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collect")
	start := time.Now()
	makeTree(t, ctx, provider, image, tree, expiringSeedScript)
	expiry := start.Add(30 * time.Second)

	select {
	case <-ctx.Done():
		t.Fatal("cancelled while waiting for the fixture to pass its time-to-live")
	case <-time.After(time.Until(expiry)):
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("scylladb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "scylladb_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)

	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed for a table the drill cannot read", err)
	}
	for _, want := range []string{"probavi.sessions", "backup is intact"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", aerr.Message, want)
		}
	}
}

// TestAMissingSSTableIsRefusedBeforeTransfer proves the completeness gate
// against a real manifest: the engine writes one sstable per tablet and
// names every set, so a copy that lost one is refused on the host, before
// a byte reaches the sandbox.
func TestAMissingSSTableIsRefusedBeforeTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collect")
	makeTree(t, ctx, provider, image, tree, seedScript)

	// Remove one Data.db the manifest lists. What remains still looks
	// like a snapshot, which is exactly why the manifest is the gate.
	matches, err := filepath.Glob(filepath.Join(tree, "probavi", "orders", "*-Data.db"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("fixture holds no sstable to remove: %v", err)
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatalf("remove sstable: %v", err)
	}

	runner, err := adapter.New("scylladb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "scylladb_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt for an incomplete copy", err)
	}
	if !strings.Contains(aerr.Message, "manifest.json lists") {
		t.Errorf("message = %q, want it to name the manifest as the authority", aerr.Message)
	}
}

// assertRunner drives the adapter's declared runner the way the core
// does — templates substituted, output read undecorated.
func assertRunner(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
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
	got := strings.TrimRight(string(out.Stdout), "\n")
	if out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q",
			checkText, got, out.ExitCode, out.Stderr, want)
	}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "probavi-adapter-scylladb")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, combined)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", dir, os.PathListSeparator, os.Getenv("PATH")))
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}

// TestArchiveDrillOnAnImageWithoutTar is the test whose absence let
// issue #327 ship. The archive kind unpacks inside the sandbox, and the
// only image this adapter verifies against has no tar at all — so the
// kind could not pass on it, and the drill reported `source_corrupt`,
// blaming the operator's backup for a tool the image does not carry.
//
// The archive is built on the host, which is where an operator builds
// one and the only place that can: there is no tar in the image to make
// it with either.
func TestArchiveDrillOnAnImageWithoutTar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collect")
	makeTree(t, ctx, provider, image, tree, seedScript)

	archive := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	tarCmd := exec.CommandContext(ctx, "tar", "-C", tree, "-czf", archive, ".")
	if out, err := tarCmd.CombinedOutput(); err != nil {
		t.Fatalf("pack the archive on the host: %v: %s", err, out)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("scylladb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "scylladb_snapshot_tar", Path: archive},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision from the archive kind: %v", err)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// Every row, through the archive path, on an image with no tar.
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT count(*) FROM orders;`, "500")
	assertRunner(t, ctx, sbx, probe, res.Connection.Database,
		`SELECT v FROM meta WHERE k = 'origin';`, "restored-ok")
}
