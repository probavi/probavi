//go:build integration

package main_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
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

// verifiedImage is the official redis image this run restores from: the
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

func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "256m"}
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine `redis-cli save` RDB, start
// the server on it, and validate the restored keys through the
// probe-declared runner.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "dump.rdb")
	makeRDBFixture(t, ctx, provider, image, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "redis" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "redis_rdb", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.EngineReadySeconds <= 0 || res.Timings.RestoreSeconds < 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}
	// An RDB dates itself: the server wrote ctime moments ago, and the
	// reported instant must reflect it without any declared timezone.
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the RDB's own save instant")
	} else if saved, err := time.Parse(time.RFC3339, *res.SourceIdentity.CreatedAt); err != nil {
		t.Errorf("created_at %q does not parse: %v", *res.SourceIdentity.CreatedAt, err)
	} else if age := time.Since(saved); age < 0 || age > time.Hour {
		t.Errorf("created_at = %s, want the save instant of moments ago", *res.SourceIdentity.CreatedAt)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("healthcheck = %+v, want healthy", health)
	}

	// The check dialect this adapter documents: a line of redis-cli
	// arguments, run through the probe-declared template exactly as
	// internal/checks would run it.
	assertCheck(t, ctx, sbx, probe, "get probavi:config", "restored-ok")
	assertCheck(t, ctx, sbx, probe, "dbsize", "501")

	// The generating built-ins, run the way the core runs them — through
	// internal/checks, carrying the declarations this adapter makes. They
	// did not apply to Redis at all before it declared them: the core
	// composed SQL for an engine that has none. Without the Dialect line
	// the core composes that SQL again, which is what makes it the
	// assertion.
	t.Run("the generating built-ins work", func(t *testing.T) {
		deps := checks.Deps{
			Exec:    sbx,
			Runner:  checks.Runner{Argv: probe.SQLRunner.Argv, Env: probe.SQLRunner.Env},
			Target:  checks.Target{User: res.Connection.User, Database: res.Connection.Database},
			Dialect: checks.DialectFrom(probe),
		}
		min1, tooMany := int64(1), int64(5000)
		results, cerr := checks.Run(ctx, []config.Check{
			{Builtin: config.CheckTableExists, Table: "probavi"},
			{Builtin: config.CheckTableExists, Table: "nosuch"},
			{Builtin: config.CheckRowCount, Table: "probavi", Min: &min1},
			{Builtin: config.CheckRowCount, Table: "probavi", Min: &tooMany},
		}, deps)
		if cerr != nil {
			t.Fatalf("checks.Run: %v", cerr)
		}
		// Each asked once so it must pass and once so it must fail: a
		// check that cannot fail proves nothing.
		for i, want := range []bool{true, false, true, false} {
			if results[i].OK != want {
				t.Errorf("check %d (%s) = %v (%s), want %v",
					i, results[i].Name, results[i].OK, results[i].Detail, want)
			}
		}
		// 501 is the whole fixture, so row_count read the prefix rather
		// than some other number the engine happened to have.
		if !strings.Contains(results[2].Detail, "501") {
			t.Errorf("row_count detail = %q, want the 501 keys under probavi:", results[2].Detail)
		}
	})

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestCorruptRDBVerdict proves a broken backup yields the right verdict
// through the whole stack: redis-check-rdb inside the sandbox is the
// authority, and its refusal must surface as a claim about the backup.
func TestCorruptRDBVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)

	corrupt := filepath.Join(t.TempDir(), "corrupt.rdb")
	if err := os.WriteFile(corrupt, []byte("REDIS0011this is no rdb payload"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "redis_rdb", Path: corrupt},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
		t.Fatalf("provision error = %v, want source_corrupt", err)
	}
}

// TestNewerRDBIsRefusedByVersion proves the pre-check against the real
// engine: an RDB whose header names a server far newer than the sandbox
// runs must be refused up front, before anything is transferred
// (docs/engine-versions.md §5).
func TestNewerRDBIsRefusedByVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)

	// A synthetic head is enough: the refusal must come before the
	// artifact's bytes matter.
	future := filepath.Join(t.TempDir(), "future.rdb")
	head := append([]byte("REDIS0011"), 0xFA)
	head = append(head, byte(len("redis-ver")))
	head = append(head, "redis-ver"...)
	head = append(head, byte(len("99.9")))
	head = append(head, "99.9"...)
	head = append(head, 0xFE, 0x00)
	if err := os.WriteFile(future, head, 0o600); err != nil {
		t.Fatal(err)
	}

	provider := docker.New(nil)
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "redis_rdb", Path: future},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "invalid_request" ||
		!strings.Contains(aerr.Message, "Redis 99.9") {
		t.Fatalf("provision error = %v, want invalid_request naming the origin version", err)
	}
}

// TestDirectoryDrillPicksTheDatedArtifact proves the directory kind end
// to end with the ranking this format allows: an RDB records its own save
// instant, so a dated artifact outranks an undated one regardless of file
// times — and the README says exactly that, so the rule an operator reads
// is the rule that runs.
func TestDirectoryDrillPicksTheDatedArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	real := filepath.Join(dir, "a-real.rdb")
	makeRDBFixture(t, ctx, provider, image, real)
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(real, past, past); err != nil {
		t.Fatal(err)
	}
	// A fresh undated decoy: RDB magic, no ctime — newest by mtime, and
	// still not the one a drill should prove.
	decoy := filepath.Join(dir, "z-decoy.rdb")
	if err := os.WriteFile(decoy, []byte("REDIS0011\xFE\x00"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "redis_rdb_dir", Path: dir},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	assertCheck(t, ctx, sbx, probe, "get probavi:config", "restored-ok")
}

// TestAOFDrillReplaysBaseAndTail proves the append-only kind end to
// end against a real server: the fixture holds a rewritten base (501
// keys) plus an incremental tail written after the rewrite, so the
// restored server can only answer everything by loading the base AND
// replaying the tail. created_at must stay null — an append-only
// directory does not date itself.
func TestAOFDrillReplaysBaseAndTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "appendonlydir")
	makeAOFFixture(t, ctx, provider, image, fixture)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "redis_aof", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %q, want null — the base ctime dates the rewrite, not the backup",
			*res.SourceIdentity.CreatedAt)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v (%v), want healthy", health, err)
	}

	// The base carries probavi:config and the 500 keys; probavi:tail was
	// written after the rewrite, so it lives only in the incremental
	// segment — 502 in total proves both halves were replayed.
	assertCheck(t, ctx, sbx, probe, "get probavi:config", "restored-ok")
	assertCheck(t, ctx, sbx, probe, "get probavi:tail", "tail-ok")
	assertCheck(t, ctx, sbx, probe, "dbsize", "502")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil || !teardown.Released {
		t.Errorf("teardown = %+v (%v)", teardown, err)
	}
}

// TestCorruptAOFVerdict proves redis-check-aof inside the sandbox is
// the authority on a damaged set, and its refusal surfaces as a claim
// about the backup.
func TestCorruptAOFVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "appendonlydir")
	makeAOFFixture(t, ctx, provider, image, fixture)
	incrs, err := filepath.Glob(filepath.Join(fixture, "*.incr.aof"))
	if err != nil || len(incrs) == 0 {
		t.Fatalf("no incremental segment in the fixture: %v (%d)", err, len(incrs))
	}
	f, err := os.OpenFile(incrs[0], os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("garbage that is not RESP\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "redis_aof", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "source_corrupt" ||
		!strings.Contains(aerr.Message, "append-only set") {
		t.Fatalf("provision error = %v, want source_corrupt from redis-check-aof", err)
	}
}

// expiringKeyTTL is how long the fixture's keys live. It only has to
// outlast writing them and saving the artifact; the drill then waits it
// out, so it is as short as that leaves room for.
const expiringKeyTTL = 10 * time.Second

// TestExpiredKeysFailTheDrillInsteadOfPassingAsEmpty is this adapter's
// half of issue #166. Redis stores an absolute instant with every
// expiring key, so a backup drilled after those instants serves none of
// them — the engine drops them as it reads the artifact and says so in
// its own counters. Nothing used to read those counters, and a drill of
// an empty server reported a successful restore.
//
// The fixture is every operator who keeps a keyspace of sessions or
// caches: all keys expiring, and a backup that outlived them.
func TestExpiredKeysFailTheDrillInsteadOfPassingAsEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	rdb := filepath.Join(t.TempDir(), "dump.rdb")
	expiry := makeExpiringRDBFixture(t, ctx, provider, image, rdb)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	// A redis sandbox starts in seconds, so unlike the slower engines
	// this wait is the whole premise rather than a formality.
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while waiting for the fixture's keys to expire")
	case <-time.After(time.Until(expiry)):
	}

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "redis_rdb", Path: rdb},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)

	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) || aerr.Code != "restore_failed" {
		t.Fatalf("provision error = %v, want restore_failed for a drill with nothing to prove", err)
	}
	for _, want := range []string{"holds no keys", "at least 100", "not damaged"} {
		if !strings.Contains(aerr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", aerr.Message, want)
		}
	}
}

// makeExpiringRDBFixture saves an artifact whose every key is alive when
// it is written and dead by the time a drill reads it, and reports the
// instant after which none of them can be served.
func makeExpiringRDBFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, dest string) time.Time {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
mkdir -p /tmp/seedrdb
redis-server --daemonize yes --dir /tmp/seedrdb --dbfilename dump.rdb --appendonly no \
  --save "" --logfile /tmp/seedrdb.log
i=0
until redis-cli -e ping >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 60 ] && { tail -n 5 /tmp/seedrdb.log >&2; exit 1; }
  sleep 1
done
redis-cli -e eval "for i=1,100 do redis.call('SET','session:'..i,'v'..i,'PX',10000) end return 1" 0 >/dev/null
held=$(redis-cli -e dbsize)
[ "$held" = "100" ] || { echo "fixture holds $held keys, want 100" >&2; exit 1; }
redis-cli -e save >/dev/null`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	// The keys were written before the save, so counting from here is the
	// safe side of their expiry.
	expiry := time.Now().Add(expiringKeyTTL + 2*time.Second)
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/seedrdb/dump.rdb", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
	return expiry
}

// makeAOFFixture seeds a real server with append-only persistence on,
// forces a rewrite so the set holds a genuine base, writes one more key
// so the incremental tail carries data of its own, and copies the
// append-only directory out after a clean shutdown.
func makeAOFFixture(t *testing.T, ctx context.Context, provider *docker.Provider, image, destDir string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
mkdir -p /tmp/seedaof
redis-server --daemonize yes --dir /tmp/seedaof --appendonly yes --save "" --logfile /tmp/seedaof.log
i=0
until redis-cli -e ping >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 60 ] && { tail -n 5 /tmp/seedaof.log >&2; exit 1; }
  sleep 1
done
redis-cli -e set probavi:config restored-ok >/dev/null
redis-cli -e eval "for i=1,500 do redis.call('SET','probavi:key'..i,'v'..i) end return 1" 0 >/dev/null
redis-cli -e bgrewriteaof >/dev/null
# The rewrite has installed only once the manifest names the seq-2 base;
# polling the in-progress flag alone races the fork and can sample 0
# before the child even starts (measured on the valkey mirror), which
# would quietly hand the drill the pre-rewrite set this test exists not
# to settle for.
i=0
until grep -q '\.2\.base\.' /tmp/seedaof/appendonlydir/appendonly.aof.manifest 2>/dev/null; do
  i=$((i+1)); [ "$i" -gt 60 ] && { echo "rewrite never installed" >&2; exit 1; }
  sleep 1
done
i=0
until [ "$(redis-cli info persistence | tr -d '\r' | awk -F: '/aof_rewrite_in_progress/{print $2}')" = "0" ]; do
  i=$((i+1)); [ "$i" -gt 60 ] && { echo "rewrite never finished" >&2; exit 1; }
  sleep 1
done
redis-cli -e set probavi:tail tail-ok >/dev/null
redis-cli shutdown nosave 2>/dev/null || true
i=0
while redis-cli ping >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 60 ] && { echo "server never exited" >&2; exit 1; }
  sleep 1
done`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/seedaof/appendonlydir", destDir).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
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
		filepath.Join(binDir, "probavi-adapter-redis"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeRDBFixture seeds a real server in a container and takes a genuine
// `redis-cli save` snapshot: 501 keys, ctime written by the server itself.
func makeRDBFixture(t *testing.T, ctx context.Context, provider *docker.Provider, image, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
mkdir -p /tmp/seed
redis-server --daemonize yes --dir /tmp/seed --dbfilename dump.rdb --save "" --logfile /tmp/seed.log
i=0
until redis-cli -e ping >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 60 ] && { tail -n 5 /tmp/seed.log >&2; exit 1; }
  sleep 1
done
redis-cli -e set probavi:config restored-ok >/dev/null
redis-cli -e eval "for i=1,500 do redis.call('SET','probavi:key'..i,'v'..i) end return 1" 0 >/dev/null
redis-cli -e save >/dev/null`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":/tmp/seed/dump.rdb", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// makeTwoRDBGenerations seeds one server twice, a couple of seconds
// apart, and extracts both saves. The marker key differs between them, so
// which generation a drill restored is a measurement rather than a claim,
// and the ctime each RDB header carries differs because the format records
// whole seconds.
func makeTwoRDBGenerations(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, staleDest, freshDest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
mkdir -p /tmp/seed
redis-server --daemonize yes --dir /tmp/seed --dbfilename dump.rdb --save "" --logfile /tmp/seed.log
i=0
until redis-cli -e ping >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 60 ] && { tail -n 5 /tmp/seed.log >&2; exit 1; }
  sleep 1
done
redis-cli -e set probavi:generation stale >/dev/null
redis-cli -e save >/dev/null
cp /tmp/seed/dump.rdb /tmp/stale.rdb
sleep 2
redis-cli -e set probavi:generation fresh >/dev/null
redis-cli -e save >/dev/null
cp /tmp/seed/dump.rdb /tmp/fresh.rdb`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	for src, dest := range map[string]string{"/tmp/stale.rdb": staleDest, "/tmp/fresh.rdb": freshDest} {
		if out, err := exec.CommandContext(ctx, "docker", "cp", seed.ID()+":"+src, dest).CombinedOutput(); err != nil {
			t.Fatalf("extract fixture: %v: %s", err, out)
		}
	}
}

// TestDirectoryDrillProvesTheOldestArtifact is this group's end-to-end
// proof that source.params.select reaches the drill: one directory, two
// real RDB files, and the record proves the other end of the retention
// window. The file times run against the headers, so only the save instant
// each artifact records can decide.
func TestDirectoryDrillProvesTheOldestArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	stale := filepath.Join(dir, "a-stale.rdb")
	fresh := filepath.Join(dir, "z-fresh.rdb")
	makeTwoRDBGenerations(t, ctx, provider, image, stale, fresh)
	// The decoy: the older save is the newest file on disk, and the name
	// order runs the other way too.
	now := time.Now()
	if err := os.Chtimes(stale, now, now); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-48 * time.Hour)
	if err := os.Chtimes(fresh, past, past); err != nil {
		t.Fatal(err)
	}

	runner, err := adapter.New("redis", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	for _, tc := range []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"the default proves last night", nil, "fresh"},
		{"select: oldest proves the far end", map[string]string{"select": "oldest"}, "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sbx, err := provider.Create(ctx, sandboxParams(image))
			if err != nil {
				t.Fatalf("create drill sandbox: %v", err)
			}
			defer destroy(t, sbx)

			res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
				Source: adapter.ProvisionSource{
					Kind: "redis_rdb_dir", Path: dir, Params: tc.params,
				},
				Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			}, sbx)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			assertCheck(t, ctx, sbx, probe, "get probavi:generation", tc.want)
			// The record has to name what it proved, not what the
			// directory happened to hold.
			want := stale
			if tc.want == "fresh" {
				want = fresh
			}
			if sum := hostSum(t, want); res.SourceIdentity.Checksum != sum {
				t.Errorf("checksum = %s, want %s's %s", res.SourceIdentity.Checksum, tc.want, sum)
			}
		})
	}
}

// hostSum is the checksum the adapter reports, computed independently.
func hostSum(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	if err := sbx.Destroy(context.Background()); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}
