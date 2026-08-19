//go:build integration

package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path"
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

// verifiedImage is the official influxdb image this run restores from:
// the manifest's baseline, or the version-matrix job's PROBAVI_IT_IMAGE
// when it names one the manifest already lists (docs/engine-versions.md §2).
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
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "1g"}
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine `influx backup`, serve it,
// pass the bucket census, and answer Flux checks through the
// probe-declared runner with the sandbox token, never a credential from
// the backup.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "bak")
	makeBackupFixture(t, ctx, provider, image, fixture, false)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("influxdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "influxdb" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "influx_backup", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.EngineReadySeconds <= 0 || res.Timings.RestoreSeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}
	if res.Connection.Database != "backup-org" {
		t.Errorf("database = %q, want the backup's own organization", res.Connection.Database)
	}
	// The backup dates itself: the CLI named the set with the UTC instant
	// it wrote it.
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the backup's own stem instant")
	} else if made, err := time.Parse(time.RFC3339, *res.SourceIdentity.CreatedAt); err != nil {
		t.Errorf("created_at %q does not parse: %v", *res.SourceIdentity.CreatedAt, err)
	} else if age := time.Since(made); age < 0 || age > time.Hour {
		t.Errorf("created_at = %s, want the instant of moments ago", *res.SourceIdentity.CreatedAt)
	}

	health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
	if err != nil || !health.Healthy {
		t.Fatalf("healthcheck = %+v (%v), want healthy", health, err)
	}

	// The check dialect this adapter documents: one Flux query through
	// the probe-declared runner, exactly as internal/checks runs it.
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		`from(bucket:"metrics") |> range(start:0) |> group() |> count()`, "500")
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		`from(bucket:"events") |> range(start:0) |> group() |> count()`, "1")

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil || !teardown.Released {
		t.Errorf("teardown = %+v (%v)", teardown, err)
	}
}

// TestReusedTargetDirRestoresTheNewest proves the stem ranking against
// real artifacts: `influx backup` into a reused directory accumulates
// timestamped sets, and the drill must restore the newest by the stems
// the backups named themselves — the second set's marker bucket exists
// only in it.
func TestReusedTargetDirRestoresTheNewest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "bak")
	makeBackupFixture(t, ctx, provider, image, fixture, true)

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("influxdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "influx_backup", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		`from(bucket:"later") |> range(start:0) |> group() |> count()`, "1")
}

// TestArchiveDrillUnpacksAndRestores proves the tar kind end to end: a
// gzip archive of a backup directory — with the wrapping directory
// layout tar naturally produces — unpacks, restores, and answers at the
// organization the archive's own table of contents named.
func TestArchiveDrillUnpacksAndRestores(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := filepath.Join(t.TempDir(), "bak")
	makeBackupFixture(t, ctx, provider, image, dir, false)
	archive := filepath.Join(t.TempDir(), "bak.tar.gz")
	if out, err := exec.CommandContext(ctx, "tar", "-czf", archive,
		"-C", filepath.Dir(dir), filepath.Base(dir)).CombinedOutput(); err != nil {
		t.Fatalf("tar fixture: %v: %s", err, out)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("influxdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "influx_backup_tar", Path: archive},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Connection.Database != "backup-org" {
		t.Errorf("database = %q, want the organization from the archive's own manifest", res.Connection.Database)
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		`from(bucket:"metrics") |> range(start:0) |> group() |> count()`, "500")
}

// TestCorruptShardVerdict proves a damaged member yields a verdict about
// the backup through the whole stack, after the host-side gates passed
// (the file exists and the manifest is whole — its bytes are the lie).
func TestCorruptShardVerdict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "bak")
	makeBackupFixture(t, ctx, provider, image, fixture, false)
	shards, err := filepath.Glob(filepath.Join(fixture, "*.tar.gz"))
	if err != nil || len(shards) == 0 {
		t.Fatalf("no shard files in the fixture: %v (%d)", err, len(shards))
	}
	if err := os.WriteFile(shards[0], []byte("garbage, not a gzip member"), 0o600); err != nil {
		t.Fatal(err)
	}

	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("influxdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "influx_backup", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	var aerr *adapter.Error
	if err == nil || !errors.As(err, &aerr) ||
		(aerr.Code != "source_corrupt" && aerr.Code != "restore_failed") {
		t.Fatalf("provision error = %v, want a verdict about the backup", err)
	}
}

// assertCheck runs one Flux query through the probe-declared runner —
// exactly how internal/checks runs checks without engine knowledge — and
// asserts the output carries the wanted fragment.
func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, database, checkText, wantFragment string) {
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
	if out.ExitCode != 0 || !strings.Contains(string(out.Stdout), wantFragment) {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want it to carry %q",
			checkText, out.Stdout, out.ExitCode, out.Stderr, wantFragment)
	}
}

// TestRetentionEnforcementDoesNotRunDuringTheDrill proves the sandbox
// does not apply the backup's own retention policy to the backup's own
// data.
//
// The fixture is an ordinary short-retention bucket: points written while
// the bucket kept two days, the retention then shortened to an hour — the
// operator action that, like a night spent in storage, leaves an artifact
// whose points sit outside the window it declares. A control bucket with
// infinite retention holds the same shape of data and must never move.
//
// The verdict is a comparison, not a wait. Nothing can be concluded from
// the drill's instance simply still holding its points: the engine's own
// default puts the first check half an hour out, so a passing read proves
// only that the test was quick. So the same artifact is restored a second
// time inside the same sandbox — its own port, its own data path, the
// enforcer left at a ten-second cadence — and the assertion is made at the
// moment that instance has provably lost points: identical bytes,
// identical engine, one pinned and one not.
func TestRetentionEnforcementDoesNotRunDuringTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	fixture := filepath.Join(t.TempDir(), "bak")
	makeExpiringBackupFixture(t, ctx, provider, image, fixture)

	// Two engines run in this sandbox before the test is done: the drill's
	// and the counter-proof's.
	params := sandboxParams(image)
	params["memory"] = "2g"
	sbx, err := provider.Create(ctx, params)
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	runner, err := adapter.New("influxdb", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "influx_backup", Path: fixture},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	org := res.Connection.Database
	backupDir := backupDirFromState(t, res.State)

	// The restore landed whole, before anything could have expired.
	if got := pointCount(t, ctx, sbx, drillPort, org, "metrics"); got != expiringPoints {
		t.Fatalf("metrics points = %d straight after the restore, want %d", got, expiringPoints)
	}
	// Direct evidence that the flag reached the engine rather than merely
	// the command line: the enforcer says what cadence it is running at.
	assertEnforcerPinned(t, ctx, sbx, path.Dir(backupDir)+"/influxd.log")

	unpinnedRestore(t, ctx, sbx, backupDir)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got := pointCount(t, ctx, sbx, counterPort, org, "metrics")
		if got < expiringPoints {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an unpinned engine still holds %d of %d points: the fixture is not past its "+
				"retention, so this test proves nothing", got, expiringPoints)
		}
		time.Sleep(2 * time.Second)
	}

	// The verdict: the unpinned instance has just expired data from these
	// very bytes, and the drill's instance still holds all of it.
	if got := pointCount(t, ctx, sbx, drillPort, org, "metrics"); got != expiringPoints {
		t.Errorf("metrics points = %d, want %d — the drill expired data the backup holds", got, expiringPoints)
	}
	if got := pointCount(t, ctx, sbx, drillPort, org, "audit"); got != 1 {
		t.Errorf("audit points = %d, want 1 — a bucket with infinite retention must not move", got)
	}
}

// assertEnforcerPinned reads the engine's own account of the cadence it
// runs its retention checks at. The rendering is the engine's (876000h is
// logged as 100y), so what is asserted is that it is not the default the
// pin exists to displace.
func assertEnforcerPinned(t *testing.T, ctx context.Context, sbx *docker.Sandbox, logPath string) {
	t.Helper()
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{
		Argv: []string{"grep", "-o", "service=retention check_interval=[^ ]*", logPath}})
	if err != nil {
		t.Fatalf("read engine log: %v", err)
	}
	line := strings.TrimSpace(string(res.Stdout))
	if res.ExitCode != 0 || line == "" {
		t.Fatalf("the engine logged no retention cadence (exit %d): the pin has no evidence behind it",
			res.ExitCode)
	}
	if strings.Contains(line, "check_interval=30m") {
		t.Fatalf("engine reports %q — the launch flag did not take effect", line)
	}
}

const (
	// drillPort is where the adapter serves the restored instance.
	drillPort = "8086"
	// counterPort is where the counter-proof's unpinned engine serves.
	counterPort = "8087"
	// expiringPoints is what the fixture's short-retention bucket holds.
	expiringPoints = 7
)

// makeExpiringBackupFixture takes a genuine `influx backup` of a bucket
// whose points are outside the retention it declares.
func makeExpiringBackupFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, dest string) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	// Hourly shard groups are what retention deletes; the bucket keeps two
	// days while the points are written, because the write path rejects a
	// point that is already outside the window.
	seedScript := `set -e
mkdir -p /tmp/seed
influxd --bolt-path /tmp/seed/influxd.bolt --engine-path /tmp/seed/engine \
  --sqlite-path /tmp/seed/influxd.sqlite --http-bind-address 127.0.0.1:8086 \
  >/tmp/seed/influxd.log 2>&1 &
i=0
until influx ping --host http://127.0.0.1:8086 >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 120 ] && { tail -n 5 /tmp/seed/influxd.log >&2; exit 1; }
  sleep 1
done
influx setup -f --host http://127.0.0.1:8086 --username seed --password seed-password \
  --org backup-org --bucket probavi-init --token seed-token >/dev/null
influx bucket create --host http://127.0.0.1:8086 -t seed-token -o backup-org \
  -n metrics --retention 47h --shard-group-duration 1h >/dev/null
influx bucket create --host http://127.0.0.1:8086 -t seed-token -o backup-org \
  -n audit --retention 0 >/dev/null
now=$(date +%s)
for m in 5 35 65 95 125 155 185; do echo "cpu,host=a v=$m $(( (now - m*60) * 1000000000 ))"; done | \
  influx write --host http://127.0.0.1:8086 -t seed-token -o backup-org -b metrics --precision ns -
echo "evt,kind=a n=1 $(( (now - 10800) * 1000000000 ))" | \
  influx write --host http://127.0.0.1:8086 -t seed-token -o backup-org -b audit --precision ns -
id=$(influx bucket list --host http://127.0.0.1:8086 -t seed-token -o backup-org -n metrics --json |
  grep -o '"id": "[a-f0-9]*"' | head -1 | cut -d'"' -f4)
influx bucket update --host http://127.0.0.1:8086 -t seed-token -i "$id" --retention 1h >/dev/null
influx backup /tmp/bak --host http://127.0.0.1:8086 -t seed-token >/dev/null 2>&1`
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/bak", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// backupDirFromState reads where the adapter left the artifact inside the
// sandbox, so the counter-proof restores the very same bytes the drill did.
func backupDirFromState(t *testing.T, state json.RawMessage) string {
	t.Helper()
	got := struct {
		BackupDir string `json:"backup_dir"`
	}{}
	if err := json.Unmarshal(state, &got); err != nil {
		t.Fatalf("provision state: %v", err)
	}
	if got.BackupDir == "" {
		t.Fatal("provision state names no backup directory")
	}
	return got.BackupDir
}

// unpinnedRestore repeats the adapter's flow beside the drill's instance
// with the retention enforcer left running at a ten-second cadence.
func unpinnedRestore(t *testing.T, ctx context.Context, sbx *docker.Sandbox, backupDir string) {
	t.Helper()
	script := `set -e
mkdir -p /tmp/counter
influxd --bolt-path /tmp/counter/influxd.bolt --engine-path /tmp/counter/engine   --sqlite-path /tmp/counter/influxd.sqlite --http-bind-address 127.0.0.1:` + counterPort + `   --storage-retention-check-interval 10s >/tmp/counter/influxd.log 2>&1 &
i=0
until influx ping --host http://127.0.0.1:` + counterPort + ` >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 120 ] && { tail -n 5 /tmp/counter/influxd.log >&2; exit 1; }
  sleep 1
done
influx setup -f --name counter --host http://127.0.0.1:` + counterPort + ` --username counter   --password counter-password --org counter-org --bucket counter-init --token counter-token >/dev/null
influx restore ` + backupDir + ` --host http://127.0.0.1:` + counterPort + ` -t counter-token >/dev/null 2>&1`
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("counter-proof exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("counter-proof restore: exit %d: %s", res.ExitCode, res.Stderr)
	}
}

// pointCount reads how many points a bucket holds, from whichever of the
// sandbox's instances is asked.
func pointCount(t *testing.T, ctx context.Context, sbx *docker.Sandbox, port, org, bucket string) int {
	t.Helper()
	token := "probavi-sandbox-token"
	if port == counterPort {
		token = "counter-token"
	}
	flux := `from(bucket:"` + bucket + `") |> range(start:0) |> group() |> count()`
	res, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: []string{
		"influx", "query", "--host", "http://127.0.0.1:" + port, "-t", token,
		"-o", org, "--raw", flux,
	}})
	if err != nil {
		t.Fatalf("count %s: %v", bucket, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("count %s: exit %d: %s", bucket, res.ExitCode, res.Stderr)
	}
	return parseCount(t, string(res.Stdout))
}

// parseCount reads the single value out of an annotated-CSV count result.
// An empty result is zero points: a bucket whose every shard group has
// been pruned answers with no rows at all.
func parseCount(t *testing.T, csv string) int {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 6 || fields[0] != "" || fields[1] != "" || fields[2] == "result" {
			continue
		}
		n, err := strconv.Atoi(fields[5])
		if err != nil {
			t.Fatalf("count row %q: %v", line, err)
		}
		return n
	}
	return 0
}

// makeBackupFixture seeds a real instance and takes a genuine `influx
// backup` — twice into the same target directory when twice is set, the
// second set carrying a marker bucket the first does not.
func makeBackupFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, dest string, twice bool) {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	seedScript := `set -e
mkdir -p /tmp/seed
influxd --bolt-path /tmp/seed/influxd.bolt --engine-path /tmp/seed/engine \
  --sqlite-path /tmp/seed/influxd.sqlite --http-bind-address 127.0.0.1:8086 \
  >/tmp/seed/influxd.log 2>&1 &
i=0
until influx ping --host http://127.0.0.1:8086 >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 120 ] && { tail -n 5 /tmp/seed/influxd.log >&2; exit 1; }
  sleep 1
done
influx setup -f --host http://127.0.0.1:8086 --username seed --password seed-password \
  --org backup-org --bucket metrics --token seed-token >/dev/null
for i in $(seq 1 500); do echo "cpu,host=h$((i%5)) usage=$i ${i}000000000"; done | \
  influx write --host http://127.0.0.1:8086 -t seed-token -o backup-org -b metrics --precision ns -
influx bucket create --host http://127.0.0.1:8086 -t seed-token -o backup-org -n events >/dev/null
echo "evt,kind=a n=1 1000000000" | influx write --host http://127.0.0.1:8086 -t seed-token -o backup-org -b events --precision ns -
influx backup /tmp/bak --host http://127.0.0.1:8086 -t seed-token >/dev/null 2>&1`
	if twice {
		seedScript += `
sleep 2
influx bucket create --host http://127.0.0.1:8086 -t seed-token -o backup-org -n later >/dev/null
echo "l,kind=b n=1 1000000000" | influx write --host http://127.0.0.1:8086 -t seed-token -o backup-org -b later --precision ns -
influx backup /tmp/bak --host http://127.0.0.1:8086 -t seed-token >/dev/null 2>&1`
	}
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", seedScript}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		seed.ID()+":/tmp/bak", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-influxdb"), ".").CombinedOutput(); err != nil {
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
