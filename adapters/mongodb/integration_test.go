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

// verifiedImage is the engine image this run restores from: the manifest's
// baseline, or the version-matrix job's PROBAVI_IT_IMAGE when it names one
// the manifest already lists. The manifest and this suite read the same
// values, so docs/capabilities.json can never claim an engine version CI
// does not actually restore from (docs/capabilities.md §1,
// docs/engine-versions.md §2).
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

// sandboxParams returns the documented drill-config sandbox params: the
// image runs bare — no MONGO_INITDB_* variables — so mongod starts without
// access control and without the first-boot temporary server. Zero-ingress
// sandboxes (--network none, no ports expressible) are the only reason
// that is acceptable.
func sandboxParams(t *testing.T) map[string]string {
	return map[string]string{"image": verifiedImage(t)}
}

// TestEndToEndRestoreDrill proves the third engine through the unchanged
// core: the docker provider, the core-side protocol client, and this
// adapter — as separate processes — restore genuine mongodump archives
// (plain and gzip, distinguished only by their bytes) and validate them
// through the probe-declared sql_runner's mongosh --eval bridge.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)

	// Phase A: seed a database and take real mongodump fixtures — one
	// plain archive, one gzip — from the same 500 documents.
	fixtureDir := t.TempDir()
	plain := filepath.Join(fixtureDir, "orders.archive")
	gzipped := filepath.Join(fixtureDir, "orders.archive.gz")
	makeFixtures(t, ctx, provider, plain, gzipped)

	for name, fixture := range map[string]string{"plain": plain, "gzip": gzipped} {
		t.Run(name, func(t *testing.T) {
			driveDrill(t, ctx, provider, fixture)
		})
	}
}

// driveDrill runs one full drill against a fresh sandbox and validates the
// restored data through the sql_runner.
func driveDrill(t *testing.T, ctx context.Context, provider *docker.Provider, fixture string) {
	t.Helper()
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mongodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "mongodb" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mongodump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
		Options: map[string]string{"database": "probavi"},
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

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// Validate the restored data through the probe-declared sql_runner —
	// exactly how internal/checks runs checks without engine knowledge.
	// The check text is a mongosh --eval expression: that is this
	// adapter's documented check dialect.
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		a = strings.ReplaceAll(a, "{{sql}}", "db.orders.countDocuments({})")
		argv = append(argv, a)
	}
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("sql_runner exec: %v", err)
	}
	if count := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || count != "500" {
		t.Fatalf("document count = %q (exit %d, stderr %s), want 500 — the restore did not carry the data",
			count, out.ExitCode, out.Stderr)
	}

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestCorruptArchiveVerdict proves a broken backup yields the right
// verdict through the whole stack, not a generic failure.
func TestCorruptArchiveVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)

	corrupt := filepath.Join(t.TempDir(), "corrupt.archive")
	if err := os.WriteFile(corrupt, []byte("this is not a mongodump archive"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("mongodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mongodump", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
// The TTL fixture: an hour's expiry over documents two hours old, so the
// artifact carries documents a restored server considers expired the
// moment it sees them. An hour-long session TTL in yesterday's backup is
// the everyday version of this; a ninety-day audit collection in a backup
// older than that is the compliance one.
const (
	ttlDatabase   = "drill"
	ttlCollection = "sessions"
	ttlControl    = "orders"
	ttlDocs       = 500
)

// ttlPassesJS reads the server's own count of TTL passes. Number() flattens
// the 64-bit shape mongosh prints so the answer parses as an integer.
const ttlPassesJS = `print(Number(db.serverStatus().metrics.ttl.passes))`

// TestTTLExpiredDocumentsSurviveTheDrill is the measured heart of the TTL
// pin. Without it MongoDB deletes every expired document about a minute
// after the sandbox starts — from a background thread, in one pass, while
// mongorestore reports success — so what a drill proved would depend on
// how long the restore took. The test proves both halves: the documents
// survive a monitor that cannot run, and the same documents disappear the
// moment the monitor is allowed to, which is what makes the first half
// mean something.
func TestTTLExpiredDocumentsSurviveTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	fixture := filepath.Join(t.TempDir(), "ttl.archive")
	makeTTLArchive(t, ctx, provider, fixture)

	sbx := freshSandbox(t, ctx, provider)
	runner, err := adapter.New("mongodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "mongodump", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision: %v — a backup of expired documents is a healthy backup", err)
	}
	if got := ttlCount(t, ctx, sbx, ttlCollection); got != ttlDocs {
		t.Fatalf("restored %d of the %d expired documents the backup holds", got, ttlDocs)
	}

	// Not merely slow: on a one-second cycle the monitor would run several
	// times over the window below, and it still never runs at all.
	mustEval(t, ctx, sbx, "admin",
		`print(db.adminCommand({setParameter: 1, ttlMonitorSleepSecs: 1}).ok)`)
	assertNoTTLPassWithin(t, ctx, sbx, 5*time.Second)
	if got := ttlCount(t, ctx, sbx, ttlCollection); got != ttlDocs {
		t.Errorf("%d of %d documents survived the pinned monitor", got, ttlDocs)
	}

	// And now the half that keeps the other honest: let the monitor run
	// and the same documents go, which is exactly what an unpinned drill
	// would have recorded as a successful restore.
	mustEval(t, ctx, sbx, "admin",
		`print(db.adminCommand({setParameter: 1, ttlMonitorEnabled: true}).ok)`)
	awaitTTLPass(t, ctx, sbx, 30*time.Second)
	if got := ttlCount(t, ctx, sbx, ttlCollection); got != 0 {
		t.Errorf("%d documents left after a TTL pass — the fixture was not expiry-eligible, "+
			"so the assertions above proved nothing", got)
	}
	if got := ttlCount(t, ctx, sbx, ttlControl); got != ttlDocs {
		t.Errorf("the control collection holds %d of %d documents — a TTL pass took "+
			"documents no index had marked", got, ttlDocs)
	}
}

// makeTTLArchive dumps a server holding already-expired documents. The
// seed's own monitor is pinned off first: without that the fixture engine
// deletes the documents before mongodump reaches them, and the artifact
// this test needs is precisely the one a real server produced while they
// were still live.
func makeTTLArchive(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)
	mustEval(t, ctx, seed, "admin",
		`print(db.adminCommand({setParameter: 1, ttlMonitorEnabled: false}).ok)`)

	seedJS := fmt.Sprintf(`db.%s.createIndex({createdAt: 1}, {expireAfterSeconds: 3600});
const stale = new Date(Date.now() - 2 * 3600 * 1000);
const docs = [];
for (let i = 0; i < %d; i++) docs.push({_id: i, createdAt: stale});
db.%s.insertMany(docs);
db.%s.insertMany(docs.map(d => ({_id: d._id, at: d.createdAt})));
print(db.%s.countDocuments({}));`,
		ttlCollection, ttlDocs, ttlCollection, ttlControl, ttlCollection)
	if got := evalJSON(t, ctx, seed, ttlDatabase, seedJS); got != strconv.Itoa(ttlDocs) {
		t.Fatalf("seeded %s documents, want %d", got, ttlDocs)
	}
	mustExec(t, ctx, seed, "mongodump", "--host", "127.0.0.1", "--archive=/tmp/ttl.archive")
	copyOut(t, ctx, seed, "/tmp/ttl.archive", dest)
}

// mustEval runs one mongosh expression that has to answer ok.
func mustEval(t *testing.T, ctx context.Context, sbx *docker.Sandbox, database, js string) {
	t.Helper()
	if got := evalJSON(t, ctx, sbx, database, js); got != "1" {
		t.Fatalf("eval %q answered %q, want 1", js, got)
	}
}

// ttlCount counts what a collection holds right now.
func ttlCount(t *testing.T, ctx context.Context, sbx *docker.Sandbox, collection string) int {
	t.Helper()
	out := evalJSON(t, ctx, sbx, ttlDatabase,
		fmt.Sprintf(`print(db.%s.countDocuments({}))`, collection))
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("count of %s = %q: %v", collection, out, err)
	}
	return n
}

// ttlPasses reads how many times the monitor has run since the server
// started — the difference between a monitor that ran and deleted nothing
// and one that never ran at all.
func ttlPasses(t *testing.T, ctx context.Context, sbx *docker.Sandbox) int {
	t.Helper()
	out := evalJSON(t, ctx, sbx, "admin", ttlPassesJS)
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("ttl passes = %q: %v", out, err)
	}
	return n
}

// assertNoTTLPassWithin fails the moment the monitor runs, rather than
// only noticing at the end of the window.
func assertNoTTLPassWithin(t *testing.T, ctx context.Context, sbx *docker.Sandbox, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if passes := ttlPasses(t, ctx, sbx); passes != 0 {
			t.Fatalf("the TTL monitor ran %d time(s) with the pin in place", passes)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// awaitTTLPass waits for the monitor to run once it is allowed to.
func awaitTTLPass(t *testing.T, ctx context.Context, sbx *docker.Sandbox, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if ttlPasses(t, ctx, sbx) > 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the TTL monitor never ran within %s of being re-enabled", budget)
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-mongodb"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeFixtures seeds 500 documents and extracts real mongodump archives
// (plain and gzip) to the host.
func makeFixtures(t *testing.T, ctx context.Context, provider *docker.Provider, plainDest, gzipDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	awaitReady(t, ctx, seed)
	seedJS := `const docs = [];
for (let i = 1; i <= 500; i++) docs.push({_id: i, total: Math.round(Math.random()*10000)/100});
db.orders.insertMany(docs);`
	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "probavi", "--eval", seedJS)
	mustExec(t, ctx, seed, "mongodump", "--host", "127.0.0.1", "--db", "probavi",
		"--archive=/tmp/fixture.archive")
	mustExec(t, ctx, seed, "mongodump", "--host", "127.0.0.1", "--db", "probavi",
		"--archive=/tmp/fixture.archive.gz", "--gzip")

	// The provider deliberately has no get-file verb; pulling the fixtures
	// out of the seed container is test harness work, done with the CLI.
	for containerPath, dest := range map[string]string{
		"/tmp/fixture.archive":    plainDest,
		"/tmp/fixture.archive.gz": gzipDest,
	} {
		if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":"+containerPath, dest).CombinedOutput(); err != nil {
			t.Fatalf("extract fixture: %v: %s", err, out)
		}
	}
}

// awaitReady polls a ping until the seed engine answers commands.
func awaitReady(t *testing.T, ctx context.Context, sbx *docker.Sandbox) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		res, err := sbx.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"mongosh", "--quiet", "--norc",
				"mongodb://127.0.0.1:27017/admin?serverSelectionTimeoutMS=2000&connectTimeoutMS=2000",
				"--eval", "db.runCommand({ping:1}).ok"},
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

func mustExec(t *testing.T, ctx context.Context, sbx *docker.Sandbox, argv ...string) {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %v: exit %d: %s", argv, res.ExitCode, res.Stderr)
	}
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}

// TestAccountLayerDrillEndToEnd reproduces the account half of issue #90
// and proves the fix. MongoDB keeps users and roles in the admin database,
// so a per-database archive carries them only when it was taken with
// --dumpDbUsersAndRoles — and mongorestore puts them back only when it is
// asked. The sharp edge: an archive that *does* carry them restores
// without them silently, so the operator did everything right and the
// drill still proved only half the recovery.
func TestAccountLayerDrillEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	fixtures := makeAccountFixtures(t, ctx, provider)

	runner, err := adapter.New("mongodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("kind mongodump silently drops the accounts", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mongodump", Path: fixtures["users"]},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			Options: map[string]string{"database": "shop"},
		}, sbx)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		// The drill passes...
		if got := evalJSON(t, ctx, sbx, "shop", "print(db.orders.countDocuments())"); got != "2" {
			t.Errorf("documents = %s, want 2 — the data half must restore", got)
		}
		// ...while the account layer the archive carried is simply gone.
		if got := evalJSON(t, ctx, sbx, "shop", userCountJS); got != "0" {
			t.Errorf("accounts = %s, want 0 — the premise of #90", got)
		}
		_ = res
	})

	t.Run("kind mongodump_with_users restores the account layer", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mongodump_with_users", Path: fixtures["users"]},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			Options: map[string]string{"database": "shop"},
		}, sbx); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if got := evalJSON(t, ctx, sbx, "shop", userCountJS); got != "1" {
			t.Errorf("accounts = %s, want the restored app account", got)
		}
		// The custom role came with it, and the account still holds it —
		// the authorization chain, not just the account row.
		roles := evalJSON(t, ctx, sbx, "shop",
			`print(db.runCommand({usersInfo:{user:"app",db:"shop"},showPrivileges:true}).users[0].inheritedPrivileges.length)`)
		if roles == "0" {
			t.Errorf("inherited privileges = %s, want the restored role's privileges", roles)
		}
	})

	t.Run("a dangling role reference fails the drill", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		_, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mongodump_with_users", Path: fixtures["crossdb"]},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			Options: map[string]string{"database": "crossdb"},
		}, sbx)
		var aerr *adapter.Error
		if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
			t.Fatalf("provision error = %v, want restore_failed from the orphaned-role gate", err)
		}
		if !strings.Contains(aerr.Message, "reference roles that do not exist") {
			t.Errorf("message = %q, want the orphaned-role verdict", aerr.Message)
		}
	})

	t.Run("an archive without accounts fails the drill", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		_, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mongodump_with_users", Path: fixtures["plain"]},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			Options: map[string]string{"database": "shop"},
		}, sbx)
		var aerr *adapter.Error
		if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
			t.Fatalf("provision error = %v, want restore_failed — declaring the kind must not pass on a plain archive", err)
		}
	})
}

// TestOplogDrillEndToEnd reproduces the consistency half of issue #90 and
// proves the fix. The race is made deterministic with a server fail point:
// the dump is blocked after it has copied shop.orders, a write lands in
// that collection while the oplog window is still open, and the block is
// released. The archive therefore carries an oplog entry whose effect is
// absent from the collection copy — exactly the state --oplog exists for.
func TestOplogDrillEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	provider := docker.New(nil)
	archive := makeOplogFixture(t, ctx, provider)

	runner, err := adapter.New("mongodb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	t.Run("kind mongodump loses the write issued during the dump", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mongodump", Path: archive},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			Options: map[string]string{"database": "shop"},
		}, sbx); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if got := evalJSON(t, ctx, sbx, "shop", "print(db.orders.countDocuments({late:true}))"); got != "0" {
			t.Errorf("late documents = %s, want 0 — the premise of #90: the captured oplog is ignored", got)
		}
	})

	t.Run("kind mongodump_with_oplog replays it", func(t *testing.T) {
		sbx := freshSandbox(t, ctx, provider)
		if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mongodump_with_oplog", Path: archive},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			Options: map[string]string{"database": "shop"},
		}, sbx); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if got := evalJSON(t, ctx, sbx, "shop", "print(db.orders.countDocuments({late:true}))"); got != "1" {
			t.Errorf("late documents = %s, want 1 — the replay must roll the window forward", got)
		}
	})

	t.Run("an archive without an oplog fails the drill", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "plain.archive")
		makePlainArchive(t, ctx, provider, plain)
		sbx := freshSandbox(t, ctx, provider)
		_, err := runner.Provision(ctx, &adapter.ProvisionRequest{
			Source:  adapter.ProvisionSource{Kind: "mongodump_with_oplog", Path: plain},
			Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			Options: map[string]string{"database": "shop"},
		}, sbx)
		var aerr *adapter.Error
		if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
			t.Fatalf("provision error = %v, want restore_failed — declaring the kind must not pass without an oplog", err)
		}
	})
}

// userCountJS counts the accounts of the connected database.
const userCountJS = `print(db.runCommand({usersInfo:1}).users.length)`

func freshSandbox(t *testing.T, ctx context.Context, provider *docker.Provider) *docker.Sandbox {
	t.Helper()
	sbx, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { destroy(t, sbx) })
	return sbx
}

// evalJSON runs one mongosh expression in the sandbox and returns its
// trimmed output.
func evalJSON(t *testing.T, ctx context.Context, sbx *docker.Sandbox, database, js string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "--port", "27017",
			database, "--eval", js},
	})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("eval %q: %v (exit %d, stderr %s)", js, err, out.ExitCode, out.Stderr)
	}
	return strings.TrimSpace(string(out.Stdout))
}

// replicaSetParams starts mongod as a single-node replica set with test
// commands enabled: --oplog dumps require a replica set, and the fail
// point that makes the race deterministic requires enableTestCommands.
// This is the fixture engine, never a drill sandbox.
func replicaSetParams(t *testing.T) map[string]string {
	return map[string]string{
		"image":   verifiedImage(t),
		"command": "mongod --replSet rs0 --bind_ip_all --setParameter enableTestCommands=1",
	}
}

func startReplicaSet(t *testing.T, ctx context.Context, provider *docker.Provider) *docker.Sandbox {
	t.Helper()
	seed, err := provider.Create(ctx, replicaSetParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	t.Cleanup(func() { destroy(t, seed) })
	awaitReady(t, ctx, seed)
	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "admin",
		"--eval", `rs.initiate({_id:"rs0",members:[{_id:0,host:"127.0.0.1:27017"}]})`)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := seed.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "admin",
				"--eval", "print(db.hello().isWritablePrimary)"},
		})
		if err == nil && out.ExitCode == 0 && strings.TrimSpace(string(out.Stdout)) == "true" {
			return seed
		}
		if time.Now().After(deadline) {
			t.Fatal("seed replica set never elected a primary")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// makeAccountFixtures seeds a database with an application account, a
// custom role, and a second database whose account holds a role defined in
// admin — the reference a single-database archive cannot carry. It returns
// three archives: plain, with accounts, and the cross-database one.
func makeAccountFixtures(t *testing.T, ctx context.Context, provider *docker.Provider) map[string]string {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)

	seedJS := `db.getSiblingDB("shop").orders.insertMany([{_id:1},{_id:2}]);
db.getSiblingDB("shop").createRole({role:"orders_reader",privileges:[{resource:{db:"shop",collection:"orders"},actions:["find"]}],roles:[]});
db.getSiblingDB("shop").createUser({user:"app",pwd:"AppSeed!OnlyInSandbox1",roles:["orders_reader"]});
db.getSiblingDB("admin").createRole({role:"global_reader",privileges:[{resource:{db:"",collection:""},actions:["find"]}],roles:[]});
db.getSiblingDB("crossdb").reports.insertOne({_id:1});
db.getSiblingDB("crossdb").createUser({user:"reader",pwd:"XSeed!OnlyInSandbox1",roles:[{role:"global_reader",db:"admin"}]});`
	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "admin", "--eval", seedJS)
	mustExec(t, ctx, seed, "mongodump", "--host", "127.0.0.1", "--db", "shop", "--archive=/tmp/plain.archive")
	mustExec(t, ctx, seed, "mongodump", "--host", "127.0.0.1", "--db", "shop",
		"--dumpDbUsersAndRoles", "--archive=/tmp/users.archive")
	mustExec(t, ctx, seed, "mongodump", "--host", "127.0.0.1", "--db", "crossdb",
		"--dumpDbUsersAndRoles", "--archive=/tmp/crossdb.archive")

	dir := t.TempDir()
	out := map[string]string{}
	for _, name := range []string{"plain", "users", "crossdb"} {
		dest := filepath.Join(dir, name+".archive")
		copyOut(t, ctx, seed, "/tmp/"+name+".archive", dest)
		out[name] = dest
	}
	return out
}

// makePlainArchive takes a full archive with no oplog, for the negative
// half of the oplog drill.
func makePlainArchive(t *testing.T, ctx context.Context, provider *docker.Provider, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(t))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)
	awaitReady(t, ctx, seed)
	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "shop",
		"--eval", "db.orders.insertOne({_id:1})")
	mustExec(t, ctx, seed, "mongodump", "--host", "127.0.0.1", "--archive=/tmp/plain.archive")
	copyOut(t, ctx, seed, "/tmp/plain.archive", dest)
}

// makeOplogFixture produces the archive at the heart of this test: a full
// --oplog dump whose captured window contains a write the collection copy
// does not. A server fail point holds the dump on a later collection
// (zzpad sorts after shop) while the write lands, which is what makes the
// race a fact instead of a coin flip.
//
// The fail point is waitAfterCommandFinishesExecution, which blocks until
// it is switched off and is registered in every listed version. Its
// predecessor here, waitInFindBeforeMakingBatch, is registered in 7.0 and
// not in 8.0 — verified by reading each mongod binary's fail-point
// registrations, after the 8.0 server answered "waitInFindBeforeMakingBatch
// not found".
func makeOplogFixture(t *testing.T, ctx context.Context, provider *docker.Provider) string {
	t.Helper()
	seed := startReplicaSet(t, ctx, provider)

	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "admin", "--eval",
		`db.getSiblingDB("shop").orders.insertMany([{_id:1},{_id:2}]);
db.getSiblingDB("zzpad").big.insertMany(Array.from({length:2000},(_,i)=>({i:i})));`)
	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "admin", "--eval",
		`db.runCommand({configureFailPoint:"waitAfterCommandFinishesExecution",mode:"alwaysOn",`+
			`data:{ns:"zzpad.big",commands:["find"]}})`)

	// mongodump runs detached; the shell returns while it keeps going, the
	// same pattern the mssql adapter uses to start its engine.
	mustExec(t, ctx, seed, "sh", "-c",
		"nohup mongodump --host 127.0.0.1 --oplog --numParallelCollections=1 "+
			"--archive=/tmp/race.archive >/tmp/race.log 2>&1 &")
	awaitLog(t, ctx, seed, "done dumping `shop.orders`", "the dump never reached shop.orders")

	// The dump has copied shop.orders and is now blocked on zzpad.big:
	// this write can only reach the restore through the oplog.
	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "shop",
		"--eval", "db.orders.insertOne({_id:999,late:true})")
	mustExec(t, ctx, seed, "mongosh", "--quiet", "--norc", "--host", "127.0.0.1", "admin", "--eval",
		`db.runCommand({configureFailPoint:"waitAfterCommandFinishesExecution",mode:"off"})`)
	awaitLog(t, ctx, seed, "oplog entr", "the dump never wrote its captured oplog")

	dest := filepath.Join(t.TempDir(), "race.archive")
	copyOut(t, ctx, seed, "/tmp/race.archive", dest)
	return dest
}

// awaitLog waits for a marker in the detached dump's log.
func awaitLog(t *testing.T, ctx context.Context, sbx *docker.Sandbox, marker, whatFailed string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"cat", "/tmp/race.log"}})
		if err == nil && strings.Contains(string(out.Stdout), marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (log: %s)", whatFailed, out.Stdout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// copyOut pulls a fixture out of the seed container; the provider
// deliberately has no get-file verb, so this is harness work done with the
// CLI.
func copyOut(t *testing.T, ctx context.Context, sbx *docker.Sandbox, containerPath, dest string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp", sbx.ID()+":"+containerPath, dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}
