//go:build integration

package main_test

import (
	"context"
	"errors"
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

// engineMemoryLimit caps every engine this suite starts. ClickHouse sizes
// its caches against the whole host when left alone, and the fixtures here
// are 500 rows — an unbounded engine would make a suite run compete with
// everything else on a developer's machine.
const engineMemoryLimit = "2g"

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

// sandboxParams returns the documented drill-config sandbox params. The
// image runs with its own entrypoint: unlike the physical-restore kinds in
// the sibling adapters, nothing here needs the engine to start late, and
// the server needs no configuration the image does not already ship.
func sandboxParams(t *testing.T) map[string]string {
	return map[string]string{"image": verifiedImage(t), "memory": engineMemoryLimit}
}

// TestEndToEndRestoreDrill proves the fifth engine through the unchanged
// core: the docker provider, the core-side protocol client, and this
// adapter — as separate processes — restore a genuine ClickHouse backup
// archive and validate it through the probe-declared sql_runner.
//
// The validation deliberately uses the SQL the *core* generates for its
// built-in checks rather than a hand-written query: ClickHouse is the
// first engine added since MongoDB, and the point of choosing it was that
// row_count and friends reach it unchanged. A test that asked its own
// question would not prove that.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "shop.zip")
	makeFixture(t, ctx, provider, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("clickhouse", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "clickhouse" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source: adapter.ProvisionSource{
			Kind:   "clickhouse_backup",
			Path:   fixture,
			Params: map[string]string{"backup_timezone": "UTC"},
		},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "shop"},
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
	// The archive's manifest carries the moment the BACKUP ran, so unlike
	// a mongodump archive this backup can date itself.
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at is nil — the manifest timestamp was not read")
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Exactly the statements internal/checks composes for row_count and
	// table_exists, quoted the way it quotes them.
	assertQuery(t, ctx, sbx, probe, res, `SELECT count(*) FROM "shop"."orders"`, "500")
	assertQuery(t, ctx, sbx, probe, res, `SELECT count(*) FROM "shop"."orders" WHERE 1=0`, "0")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// ttlWindow is how long the fixture's rows stay inside their TTL. It has
// to outlast seeding and taking the backup — a fixture that expired while
// being written would prove nothing — and the drill then waits it out, so
// it is as short as that leaves room for.
const ttlWindow = 40 * time.Second

// ttlGrace is how long an unpinned engine is given to act before the
// assertion is made. Measured on both verified images: the TTL merge lands
// within five seconds of the rows expiring, in the same second as the
// restore when they expired before it.
const ttlGrace = 15 * time.Second

// TestRetentionPolicyDoesNotRunDuringTheDrill proves the sandbox does not
// apply the engine's own TTL to the artifact it was handed.
//
// The rows are inside their TTL when the BACKUP runs — an artifact whose
// rows were already past it cannot exist, because ClickHouse applies row
// TTL when a part is written — and past it by the time the drill reads
// them, which is the ordinary case for any backup that spent a night in
// storage.
//
// The counter-proof is what keeps the first assertion from being vacuous:
// releasing the same lock deletes the same rows, so the engine really
// would have taken them.
func TestRetentionPolicyDoesNotRunDuringTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "expiring.zip")
	expiry := makeExpiringFixture(t, ctx, provider, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("clickhouse", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "clickhouse_backup", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "shop"},
	}, sbx); err != nil {
		t.Fatalf("provision: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("cancelled while waiting for the fixture to age past its TTL")
	case <-time.After(time.Until(expiry.Add(ttlGrace))):
	}

	if got := queryValue(t, ctx, sbx, "SELECT count() FROM shop.expiring"); got != "500" {
		t.Errorf("expiring rows = %s, want 500 — the sandbox expired data the backup holds", got)
	}
	// The control table has no TTL, so it says something else: that
	// restoring in two passes lands every row exactly once.
	if got := queryValue(t, ctx, sbx, "SELECT count() FROM shop.orders"); got != "500" {
		t.Errorf("control rows = %s, want 500", got)
	}

	mustQuery(t, ctx, sbx, "SYSTEM START TTL MERGES")
	deadline := time.Now().Add(time.Minute)
	for queryValue(t, ctx, sbx, "SELECT count() FROM shop.expiring") != "0" {
		if time.Now().After(deadline) {
			t.Fatal("releasing the lock did not expire the rows: the engine would not have " +
				"taken them anyway, so the assertion above proved nothing")
		}
		time.Sleep(time.Second)
	}
}

// makeExpiringFixture backs up a database holding rows that are inside a
// TTL now and outside it by the time a drill reads them, and reports the
// instant they expire.
func makeExpiringFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) time.Time {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	seedRows(t, ctx, seed, 500)
	mustQuery(t, ctx, seed, "CREATE TABLE shop.expiring (ts DateTime, id UInt64) ENGINE=MergeTree "+
		"ORDER BY id TTL ts + INTERVAL "+strconv.Itoa(int(ttlWindow.Seconds()))+" SECOND DELETE")
	mustQuery(t, ctx, seed, "INSERT INTO shop.expiring SELECT now(), number FROM numbers(500)")
	expiry := time.Now().Add(ttlWindow)

	// A fixture that aged while it was being written would make the drill
	// prove nothing, and would do it quietly.
	if got := queryValue(t, ctx, seed, "SELECT count() FROM shop.expiring"); got != "500" {
		t.Fatalf("fixture holds %s rows before the backup, want 500", got)
	}
	mustQuery(t, ctx, seed, "BACKUP DATABASE shop TO File('expiring.zip')")
	extract(t, ctx, seed, "/var/lib/clickhouse/backups/expiring.zip", dest)
	return expiry
}

// queryValue runs one statement against the engine and returns its answer.
func queryValue(t *testing.T, ctx context.Context, sbx *docker.Sandbox, query string) string {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: clientArgv(query)})
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %q: exit %d: %s", query, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(string(res.Stdout))
}

// TestCorruptArchiveVerdict proves a broken backup yields the right
// verdict through the whole stack: the drill must say the backup is
// unusable, not that something went wrong.
func TestCorruptArchiveVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)

	// Zip magic, no central directory: what a half-written archive looks
	// like, and what the engine refuses to unpack.
	corrupt := filepath.Join(t.TempDir(), "corrupt.zip")
	if err := os.WriteFile(corrupt, []byte("PK\x03\x04 this is not a finished archive"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("clickhouse", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "clickhouse_backup", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestEmptyBackupIsRefused proves the drill will not report success for an
// archive that holds nothing.
//
// The artifact is a genuine BACKUP of a genuine database — one that
// happens to have no table in it — so the engine is entirely happy with
// it: both restore passes print RESTORED and the server comes up serving
// nothing. Only the count says otherwise, which is why the count is the
// last thing the restore script does.
func TestEmptyBackupIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	empty := filepath.Join(t.TempDir(), "empty.zip")
	makeEmptyFixture(t, ctx, provider, empty)

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("clickhouse", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "clickhouse_backup", Path: empty},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed", err)
	}
	if !strings.Contains(aerr.Message, "no table") {
		t.Errorf("message = %q, want it to say the restore produced nothing", aerr.Message)
	}
}

// makeEmptyFixture extracts a real BACKUP archive of a database holding no
// table — the same statement and the same format as the populated fixture,
// minus the content.
func makeEmptyFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	mustQuery(t, ctx, seed, "CREATE DATABASE shop")
	mustQuery(t, ctx, seed, "BACKUP DATABASE shop TO File('empty.zip')")
	extract(t, ctx, seed, "/var/lib/clickhouse/backups/empty.zip", dest)
}

// TestDirectoryDrillPicksTheNewestBackup proves the directory kind end to
// end, and proves it ranks by what each archive says about itself: the
// archive holding the data is written first and therefore has the older
// file time, so an mtime-ranked scan would restore the decoy.
func TestDirectoryDrillPicksTheNewestBackup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	dir := t.TempDir()
	older := filepath.Join(dir, "z-decoy.zip")
	newer := filepath.Join(dir, "a-wanted.zip")
	makeRankingFixtures(t, ctx, provider, older, newer)

	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("clickhouse", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "clickhouse_backup_dir", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "shop"},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The wanted backup holds 500 rows; the decoy holds 1.
	assertQuery(t, ctx, sbx, probe, res, `SELECT count(*) FROM "shop"."orders"`, "500")
}

// assertQuery runs one statement through the probe-declared sql_runner —
// exactly how internal/checks runs checks without engine knowledge.
func assertQuery(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, res *adapter.ProvisionResult, sql, want string) {
	t.Helper()
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		a = strings.ReplaceAll(a, "{{sql}}", sql)
		argv = append(argv, a)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("sql_runner exec: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || got != want {
		t.Fatalf("%s = %q (exit %d, stderr %s), want %s", sql, got, out.ExitCode, out.Stderr, want)
	}
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-clickhouse"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeFixture seeds 500 rows and extracts a real BACKUP archive.
func makeFixture(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	seedRows(t, ctx, seed, 500)
	mustQuery(t, ctx, seed, "BACKUP DATABASE shop TO File('fixture.zip')")
	extract(t, ctx, seed, "/var/lib/clickhouse/backups/fixture.zip", dest)
}

// makeRankingFixtures writes two archives whose file times disagree with
// their backup times: the decoy is backed up first, so its manifest is the
// older of the two, and copied out last, so its file is the newer of the
// two. Ranking by mtime restores the decoy; ranking by the manifest — what
// the backup says about itself — restores the one that matters.
func makeRankingFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, decoyDest, wantedDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)

	// The decoy: one row, backed up first, so its manifest timestamp is
	// the older of the two.
	seedRows(t, ctx, seed, 1)
	mustQuery(t, ctx, seed, "BACKUP DATABASE shop TO File('decoy.zip')")

	// ClickHouse records the manifest timestamp with second precision, so
	// the two backups must not land in the same second.
	time.Sleep(1100 * time.Millisecond)

	mustQuery(t, ctx, seed, "DROP DATABASE shop")
	seedRows(t, ctx, seed, 500)
	mustQuery(t, ctx, seed, "BACKUP DATABASE shop TO File('wanted.zip')")

	// Copy the wanted one out first, the decoy second: on the host the
	// decoy now has the newest mtime while carrying the older backup.
	extract(t, ctx, seed, "/var/lib/clickhouse/backups/wanted.zip", wantedDest)
	time.Sleep(1100 * time.Millisecond)
	extract(t, ctx, seed, "/var/lib/clickhouse/backups/decoy.zip", decoyDest)
}

func seedRows(t *testing.T, ctx context.Context, sbx *docker.Sandbox, rows int) {
	t.Helper()
	mustQuery(t, ctx, sbx, "CREATE DATABASE IF NOT EXISTS shop")
	mustQuery(t, ctx, sbx,
		"CREATE TABLE IF NOT EXISTS shop.orders (id UInt64, total Float64) ENGINE=MergeTree ORDER BY id")
	mustQuery(t, ctx, sbx,
		"INSERT INTO shop.orders SELECT number, number / 3 FROM numbers("+strconv.Itoa(rows)+")")
}

// awaitReady polls until the seed engine answers a query.
func awaitReady(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		res, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv:    clientArgv("SELECT 1"),
			Timeout: 5 * time.Second,
		})
		if err == nil && res.ExitCode == 0 && strings.TrimSpace(string(res.Stdout)) == "1" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("seed engine never became ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// clientArgv mirrors what the adapter runs: the loopback host is required,
// because a zero-ingress sandbox cannot resolve its own hostname.
func clientArgv(query string) []string {
	return []string{"clickhouse-client", "--host", "127.0.0.1", "--query", query}
}

func mustQuery(t *testing.T, ctx context.Context, sbx *docker.Sandbox, query string) {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: clientArgv(query)})
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %q: exit %d: %s", query, res.ExitCode, res.Stderr)
	}
}

// extract pulls a fixture out of the seed container. The provider
// deliberately has no get-file verb; this is test harness work.
func extract(t *testing.T, ctx context.Context, sbx *docker.Sandbox, containerPath, dest string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		sbx.ID()+":"+containerPath, dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}
