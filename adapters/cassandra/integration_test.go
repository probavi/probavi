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

// verifiedImage is the official image this run restores with: the
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

// sandboxParams caps the JVM the way the README documents: exec inherits
// the sandbox env, so the heap settings reach the node the adapter
// starts.
func sandboxParams(image string) map[string]string {
	return map[string]string{
		"image": image, "command": "sleep infinity", "memory": "1536m",
		"env.MAX_HEAP_SIZE": "512M", "env.HEAP_NEWSIZE": "100M",
	}
}

// seedScript boots a node the same measured way the adapter does, seeds
// two tables, snapshots them, and collects the snapshot with the exact
// loop the README documents — so the recipe operators read is the recipe
// the suite proves.
const seedScript = `set -e
grep -q "$(hostname)" /etc/hosts || echo "127.0.0.1 $(hostname)" >> /etc/hosts
sed -i -e "s/^listen_address:.*/listen_address: 127.0.0.1/" \
  -e "s/^rpc_address:.*/rpc_address: 127.0.0.1/" \
  -e "s/- seeds:.*/- seeds: 127.0.0.1/" /etc/cassandra/cassandra.yaml
cassandra -R > /tmp/cassandra.log 2>&1
for i in $(seq 1 120); do
  cqlsh -e "SELECT release_version FROM system.local;" >/dev/null 2>&1 && break
  sleep 2
done
cqlsh -e "SELECT release_version FROM system.local;" >/dev/null
{
  echo "CREATE KEYSPACE probavi WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1};"
  echo "CREATE TABLE probavi.orders (id int PRIMARY KEY, v text);"
  echo "CREATE TABLE probavi.meta (k text PRIMARY KEY, v text);"
  echo "INSERT INTO probavi.meta (k, v) VALUES ('origin', 'restored-ok');"
  for i in $(seq 1 500); do echo "INSERT INTO probavi.orders (id, v) VALUES ($i, 'row$i');"; done
} > /tmp/seed.cql
cqlsh -f /tmp/seed.cql
nodetool flush
nodetool snapshot -t drill probavi >/dev/null
dest=/tmp/collect
for snap in /var/lib/cassandra/data/probavi/*/snapshots/drill; do
  tbl=${snap%/snapshots/*}; name=$(basename "$tbl"); name=${name%-*}
  mkdir -p "$dest/probavi/$name" && cp -a "$snap/." "$dest/probavi/$name/"
done`

// makeTree seeds a real node in a container of the given image and
// extracts the collected snapshot tree to dest (which must not exist).
func makeTree(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest string) {
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
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/collect", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// expiringTTL is the fixture's time-to-live. It only has to outlast
// seeding — a fixture whose rows expired before the snapshot was taken
// would prove something else — and be short enough that the drill,
// which cannot start a Cassandra node in less than half a minute,
// certainly reads after it.
const expiringTTL = 15 * time.Second

// expiringSeedScript is seedScript with one table added: rows that expire
// while the snapshot sits on disk, which is every backup of a table with
// a time-to-live.
var expiringSeedScript = strings.Replace(seedScript,
	`  echo "CREATE TABLE probavi.meta (k text PRIMARY KEY, v text);"`,
	`  echo "CREATE TABLE probavi.meta (k text PRIMARY KEY, v text);"
  echo "CREATE TABLE probavi.sessions (id int PRIMARY KEY, v text) WITH default_time_to_live = 15;"
  for i in $(seq 1 100); do echo "INSERT INTO probavi.sessions (id, v) VALUES ($i, 'session$i');"; done`, 1)

// TestExpiredRowsFailTheDrillInsteadOfPassingAsEmpty is this adapter's
// half of issue #166. Cassandra filters expired cells on read and offers
// no setting that suspends it, so unlike the sibling engines the drill
// cannot hold the policy back — what it can do is refuse to call a table
// proven when the artifact says it held rows and the engine serves none.
//
// The two healthy tables in the same snapshot are the control: they are
// probed first and must pass, so the refusal is this table's and not the
// fence firing at everything.
func TestExpiredRowsFailTheDrillInsteadOfPassingAsEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collect")
	expiry := makeExpiringTree(t, ctx, provider, image, tree)

	// Starting a node takes longer than this on its own; waiting for it
	// explicitly is what makes the test's premise a fact rather than an
	// assumption about how slow Cassandra is.
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

	runner, err := adapter.New("cassandra", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "cassandra_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)

	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed for a table the drill cannot read", err)
	}
	for _, want := range []string{"probavi.sessions", "15 seconds", "backup is intact"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", aerr.Message, want)
		}
	}
}

// makeExpiringTree seeds a node whose snapshot holds a table of expiring
// rows and reports the instant after which none of them can be read.
func makeExpiringTree(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, dest string) time.Time {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"bash", "-c", expiringSeedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	// The rows were written before the snapshot was taken, so counting
	// from here is the safe side of their expiry.
	expiry := time.Now().Add(expiringTTL + 2*time.Second)
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/collect", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return expiry
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine collected snapshot into a
// fresh single node and validate the restored rows through the
// probe-declared runner, generating built-ins included.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collected")
	makeTree(t, ctx, provider, image, tree)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("cassandra", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "cassandra" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "cassandra_snapshot", Path: tree},
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
		t.Fatal("created_at = nil, want the manifest's own instant")
	}
	if _, err := time.Parse(time.RFC3339, *res.SourceIdentity.CreatedAt); err != nil {
		t.Errorf("created_at = %q does not parse: %v", *res.SourceIdentity.CreatedAt, err)
	}
	if res.Connection.Database != "probavi" {
		t.Errorf("connection.database = %q, want the restored keyspace", res.Connection.Database)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// CQL through the probe-declared template, exactly as internal/checks
	// runs the generating built-ins — the awk filter turns cqlsh's
	// decorated output into the contract's undecorated rows (measured).
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT count(*) FROM orders;", "500")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT k, v FROM meta;", "origin\trestored-ok")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestArchiveDrillUnpacksAndServes proves the tar kind end to end: a
// gzip archive of the collected tree — with the wrapping directory
// layout tar naturally produces — unpacks, restores, and serves.
func TestArchiveDrillUnpacksAndServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	base := t.TempDir()
	tree := filepath.Join(base, "collected")
	makeTree(t, ctx, provider, image, tree)
	archive := filepath.Join(base, "snap.tar.gz")
	if out, err := exec.CommandContext(ctx, "tar", "-czf", archive, "-C", base, "collected").CombinedOutput(); err != nil {
		t.Fatalf("tar fixture: %v: %s", err, out)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("cassandra", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "cassandra_snapshot_tar", Path: archive},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the archive's own claim")
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database, "SELECT count(*) FROM orders;", "500")
}

// TestMissingComponentIsRefusedBeforeTransfer drives the census end to
// end: a snapshot that lost a component the TOC lists is refused before
// a byte reaches the sandbox — the measured alternative being a loader
// that streams nothing and exits 0.
func TestMissingComponentIsRefusedBeforeTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collected")
	makeTree(t, ctx, provider, image, tree)
	removed := false
	for _, table := range []string{"orders", "meta"} {
		matches, err := filepath.Glob(filepath.Join(tree, "probavi", table, "*-Index.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
			removed = true
		}
	}
	if !removed {
		t.Fatal("fixture holds no Index.db to remove")
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("cassandra", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "cassandra_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" ||
		!strings.Contains(aerr.Message, "Index.db") {
		t.Fatalf("provision error = %v, want source_corrupt naming the missing component", err)
	}
}

// TestCorruptDataIsRefusedByItsOwnDigest drives the integrity fence end
// to end: a bit-rotted Data file contradicts its own Digest.crc32 and is
// refused before transfer — the measured alternative being a loader that
// streams the damage without a word.
func TestCorruptDataIsRefusedByItsOwnDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collected")
	makeTree(t, ctx, provider, image, tree)
	matches, err := filepath.Glob(filepath.Join(tree, "probavi", "orders", "*-Data.db"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("fixture holds no Data.db (%v)", err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32 && i < len(data); i++ {
		data[i] ^= 0xFF
	}
	if err := os.WriteFile(matches[0], data, 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("cassandra", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "cassandra_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" ||
		!strings.Contains(aerr.Message, "Digest.crc32") {
		t.Fatalf("provision error = %v, want source_corrupt naming the digest", err)
	}
}

// TestNewerSchemaIsRefusedNamingBothSides drives the version pairing end
// to end: a snapshot collected on the newest verified engine carries a
// schema.cql the baseline engine cannot parse (measured), and the drill
// states the pairing rather than a bare parse error.
func TestNewerSchemaIsRefusedNamingBothSides(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	m := manifest(t)
	baseline, err := m.BaselineImage()
	if err != nil {
		t.Fatalf("baseline image: %v", err)
	}
	newest := m.Verified[len(m.Verified)-1].Image
	if newest == baseline {
		t.Skip("manifest lists a single engine line; no newer schema to refuse")
	}

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	tree := filepath.Join(t.TempDir(), "collected")
	makeTree(t, ctx, provider, newest, tree)

	sbx, err := provider.Create(ctx, sandboxParams(baseline))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("cassandra", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "cassandra_snapshot", Path: tree},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "invalid_request" ||
		!strings.Contains(aerr.Message, "newer Cassandra") {
		t.Fatalf("provision error = %v, want invalid_request naming the pairing", err)
	}
}

// assertCheck runs one CQL check through the probe-declared runner —
// exactly how internal/checks runs checks without engine knowledge: the
// core substitutes {{database}} from the connection provision returned,
// and {{sql}} with the check text — and asserts the filtered output.
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
	got := strings.TrimRight(string(out.Stdout), "\n")
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
		filepath.Join(binDir, "probavi-adapter-cassandra"), ".").CombinedOutput(); err != nil {
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
