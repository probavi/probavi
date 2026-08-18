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

// promtoolImage is where the drill's query client comes from.
// VictoriaMetrics ships none, and a form-encoded HTTP one-liner is not a
// substitute: it decodes `.+` as `. ` and answers zero on a populated
// server (measured). The version is pinned like every other image this
// suite builds from.
const promtoolImage = "prom/prometheus:v3.5.5"

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

// imageTag turns an official image reference into a local tag.
func imageTag(prefix, base string) string {
	return prefix + ":" + strings.ReplaceAll(base[strings.LastIndex(base, ":")+1:], "/", "-")
}

// wrapperImage builds the image a drill sandbox actually runs: the exact
// recipe the adapter README documents. A drill needs the server, the
// restore tool and a query client together, and VictoriaMetrics ships
// them in separate images (and the client not at all), so this is the one
// place their versions are tied together.
func wrapperImage(t *testing.T, ctx context.Context, base string) string {
	t.Helper()
	tools := strings.Replace(base, "victoria-metrics:", "vmrestore:", 1)
	return buildImage(t, ctx, imageTag("probavi-it-victoriametrics", base), fmt.Sprintf(
		"FROM %s\n"+
			"COPY --from=%s /vmrestore-prod /usr/local/bin/vmrestore\n"+
			"COPY --from=%s /bin/promtool /usr/local/bin/promtool\n"+
			"RUN ln -s /victoria-metrics-prod /usr/local/bin/victoria-metrics\n"+
			"ENTRYPOINT []\n", base, tools, promtoolImage))
}

// seedImage is the wrapper plus vmbackup: the tool an operator runs on
// their own server, which this suite needs to produce a genuine artifact.
// It is deliberately not part of the drill recipe — a drill restores
// backups, it does not take them.
func seedImage(t *testing.T, ctx context.Context, base string) string {
	t.Helper()
	backup := strings.Replace(base, "victoria-metrics:", "vmbackup:", 1)
	return buildImage(t, ctx, imageTag("probavi-it-victoriametrics-seed", base), fmt.Sprintf(
		"FROM %s\n"+
			"COPY --from=%s /vmbackup-prod /usr/local/bin/vmbackup\n",
		wrapperImage(t, ctx, base), backup))
}

func buildImage(t *testing.T, ctx context.Context, tag, dockerfile string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", tag, err, out)
	}
	return tag
}

func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": "1g"}
}

// fixtureDays is how far back the seeded history reaches. It is past the
// server's own default retention of one month on purpose: a restored
// instance that inherited that default serves 48 of these samples
// (measured), and no drill may report the difference as success.
const fixtureDays = 90

// makeBackupFixture seeds a real server, freezes it with the snapshot API
// and backs it up with vmbackup — the artifact an operator's own runbook
// produces — then extracts it to the host.
func makeBackupFixture(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, dest string) int {
	t.Helper()
	seed, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, seed)

	script := fmt.Sprintf(`set -e
victoria-metrics -storageDataPath=/tmp/data -retentionPeriod=100y \
  -httpListenAddr=127.0.0.1:8428 >/tmp/vm.log 2>&1 &
i=0
until wget -q -O- http://127.0.0.1:8428/health 2>/dev/null | grep -qi ok; do
  i=$((i+1)); [ "$i" -gt 60 ] && { tail -n 5 /tmp/vm.log >&2; exit 1; }
  sleep 1
done
now=$(date +%%s)
{ i=0; while [ $i -lt %d ]; do
    echo "probavi_history{job=\"history\"} $i $(( (now - i * 86400) * 1000 ))"
    i=$((i + 1))
  done; } > /tmp/samples.txt
wget -q -O- --post-file=/tmp/samples.txt http://127.0.0.1:8428/api/v1/import/prometheus
wget -q -O- http://127.0.0.1:8428/internal/force_flush >/dev/null
sleep 2
snap=$(wget -q -O- http://127.0.0.1:8428/snapshot/create | sed 's/.*snapshot":"//;s/"}//')
[ -n "$snap" ] || { echo "no snapshot name" >&2; exit 1; }
vmbackup -storageDataPath=/tmp/data -snapshotName="$snap" -dst=fs:///tmp/backup >/dev/null 2>&1
test -f /tmp/backup/backup_complete.ignore
promtool query instant http://127.0.0.1:8428   'sum(count_over_time(probavi_history[%dd]))'`, fixtureDays, fixtureDays+10)
	res, err := seed.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("seed exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seed fixture: exit %d: %s", res.ExitCode, res.Stderr)
	}
	copyOut(t, ctx, seed, "/tmp/backup", dest)
	return sampleCount(t, string(res.Stdout))
}

// sampleCount reads promtool's instant-vector line, `{} => 90 @[…]`, so
// the tests compare the restored server against what the source server
// actually held rather than against an arithmetic guess.
func sampleCount(t *testing.T, out string) int {
	t.Helper()
	_, rest, found := strings.Cut(out, "=> ")
	if !found {
		t.Fatalf("seed reported no sample count: %q", out)
	}
	value, _, found := strings.Cut(rest, " @")
	if !found {
		t.Fatalf("seed reported no sample count: %q", out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n == 0 {
		t.Fatalf("seed sample count = %q: %v", value, err)
	}
	return n
}

func copyOut(t *testing.T, ctx context.Context, sbx *docker.Sandbox, containerPath, dest string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		sbx.ID()+":"+containerPath, dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// drillRig is everything a test needs to run one drill.
type drillRig struct {
	provider *docker.Provider
	drill    string // the documented wrapper image
	seed     string // the wrapper plus vmbackup
}

func newRig(t *testing.T, ctx context.Context) *drillRig {
	t.Helper()
	buildAdapterOnPath(t, ctx)
	base := verifiedImage(t)
	return &drillRig{
		provider: docker.New(nil),
		drill:    wrapperImage(t, ctx, base),
		seed:     seedImage(t, ctx, base),
	}
}

// fixture produces one genuine backup on the host, and reports how many
// samples the source server held when the snapshot froze it.
func (r *drillRig) fixture(t *testing.T, ctx context.Context) (string, int) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "backup")
	return dest, makeBackupFixture(t, ctx, r.provider, r.seed, dest)
}

// provision runs a drill against one artifact and returns what it
// answered, or the adapter's refusal.
func (r *drillRig) provision(t *testing.T, ctx context.Context, kind, path string) (*docker.Sandbox,
	*adapter.Runner, *adapter.ProbeResult, *adapter.ProvisionResult, error) {
	t.Helper()
	sbx, err := r.provider.Create(ctx, sandboxParams(r.drill))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	t.Cleanup(func() { destroy(t, sbx) })

	runner, err := adapter.New("victoriametrics", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: kind, Path: path},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	return sbx, runner, probe, res, err
}

// TestEndToEndRestoreDrill proves the engine through the unchanged core:
// the docker provider, the core-side protocol client, and this adapter —
// as separate processes — restore a genuine vmbackup output, serve it,
// and answer checks through the probe-declared runner at the instant the
// backup states.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rig := newRig(t, ctx)
	fixture, samples := rig.fixture(t, ctx)

	sbx, runner, probe, res, err := rig.provision(t, ctx, "victoriametrics_backup", fixture)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 ||
		res.Timings.TransferSeconds <= 0 {
		t.Errorf("timings = %+v, want real measurements", res.Timings)
	}
	if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source identity = %+v", res.SourceIdentity)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Fatal("created_at = nil, want the instant the backup's own metadata states")
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

	// The check dialect this adapter documents: MetricsQL through the
	// probe-declared template, evaluated at the backup's own instant. A
	// range covers the whole history, which is what a metrics backup is
	// for.
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		fmt.Sprintf("sum(count_over_time(probavi_history[%dd]))", fixtureDays+10),
		fmt.Sprintf("=> %d @", samples))

	teardown, err := runner.Teardown(ctx, res.State, "completed", sbx)
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !teardown.Released {
		t.Errorf("teardown = %+v", teardown)
	}
}

// TestRetentionDoesNotTrimTheArtifact is the measured heart of the
// retention pin. VictoriaMetrics keeps one month by default, so a
// restored history longer than that arrives whole and then serves less
// than the backup holds — with nothing anywhere reporting the loss. The
// fixture spans 90 days precisely so the difference is visible.
func TestRetentionDoesNotTrimTheArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rig := newRig(t, ctx)
	fixture, samples := rig.fixture(t, ctx)

	sbx, _, probe, res, err := rig.provision(t, ctx, "victoriametrics_backup", fixture)
	if err != nil {
		t.Fatalf("provision: %v — a history longer than the default retention is a healthy backup", err)
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		fmt.Sprintf("sum(count_over_time(probavi_history[%dd]))", fixtureDays+10),
		fmt.Sprintf("=> %d @", samples))

	// And the server really is holding data the default would have
	// dropped: the oldest sample is older than one month.
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		fmt.Sprintf("count(min_over_time(probavi_history[%dd]) offset 40d)", fixtureDays+10),
		"=> 1 @")
}

// TestArchiveDrillUnpacksAndServes proves the tar kind end to end,
// including the wrapping directory tar naturally produces.
func TestArchiveDrillUnpacksAndServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rig := newRig(t, ctx)
	fixture, samples := rig.fixture(t, ctx)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if out, err := exec.CommandContext(ctx, "tar", "-czf", archive,
		"-C", filepath.Dir(fixture), filepath.Base(fixture)).CombinedOutput(); err != nil {
		t.Fatalf("tar the fixture: %v: %s", err, out)
	}

	sbx, _, probe, res, err := rig.provision(t, ctx, "victoriametrics_backup_tar", archive)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.SourceIdentity.CreatedAt == nil {
		t.Error("created_at = nil, want the instant the archive's own metadata states")
	}
	assertCheck(t, ctx, sbx, probe, res.Connection.Database,
		fmt.Sprintf("sum(count_over_time(probavi_history[%dd]))", fixtureDays+10),
		fmt.Sprintf("=> %d @", samples))
}

// TestTruncatedBackupRefusesTheDrill covers the artifact's own
// completeness contract from both sides: the host-side census refuses a
// backup missing a part its parts.json names, and — with the census
// blinded by an archive the host walks only for markers — the engine
// refuses the same artifact at startup, which the drill surfaces as the
// backup's fault.
func TestTruncatedBackupRefusesTheDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rig := newRig(t, ctx)
	fixture, _ := rig.fixture(t, ctx)
	removed := removeOnePart(t, fixture)

	t.Run("the directory kind refuses before transferring anything", func(t *testing.T) {
		_, _, _, _, err := rig.provision(t, ctx, "victoriametrics_backup", fixture)
		if err == nil {
			t.Fatal("provision succeeded on a truncated backup")
		}
		var aerr *adapter.Error
		if !errors.As(err, &aerr) || aerr.Code != "source_corrupt" ||
			!strings.Contains(aerr.Message, removed) {
			t.Errorf("error = %v, want source_corrupt naming the missing part %s", err, removed)
		}
	})

	t.Run("the archive kind refuses on the engine's own verdict", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "truncated.tar")
		if out, err := exec.CommandContext(ctx, "tar", "-cf", archive,
			"-C", filepath.Dir(fixture), filepath.Base(fixture)).CombinedOutput(); err != nil {
			t.Fatalf("tar the fixture: %v: %s", err, out)
		}
		_, _, _, _, err := rig.provision(t, ctx, "victoriametrics_backup_tar", archive)
		if err == nil {
			t.Fatal("provision succeeded on a truncated backup")
		}
		var aerr *adapter.Error
		if !errors.As(err, &aerr) || aerr.Code != "source_corrupt" {
			t.Errorf("error = %v, want source_corrupt from the restored server's own refusal", err)
		}
	})
}

// TestLiveCopyIsRefusedByName proves the fence that matters most,
// because the artifact it refuses looks healthy: a copy of a running
// server's storage path starts and serves every sample in a quiet moment
// (measured), and is inconsistent under write load.
func TestLiveCopyIsRefusedByName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rig := newRig(t, ctx)
	fixture, _ := rig.fixture(t, ctx)
	// What a live -storageDataPath carries and a backup never does.
	if err := os.WriteFile(filepath.Join(fixture, "flock.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := rig.provision(t, ctx, "victoriametrics_backup", fixture)
	if err == nil {
		t.Fatal("provision succeeded on a copy of a live storage path")
	}
	var aerr *adapter.Error
	if !errors.As(err, &aerr) || aerr.Code != "unsupported_source" ||
		!strings.Contains(aerr.Message, "flock.lock") ||
		!strings.Contains(aerr.Message, "vmbackup") {
		t.Errorf("error = %v, want unsupported_source naming the lock and the fix", err)
	}
}

// TestIncompleteBackupIsRefused covers the marker vmbackup writes last.
// vmrestore refuses the same artifact and offers a flag that would
// restore it anyway; the drill refuses instead of reaching for it.
func TestIncompleteBackupIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rig := newRig(t, ctx)
	fixture, _ := rig.fixture(t, ctx)
	if err := os.Remove(filepath.Join(fixture, "backup_complete.ignore")); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := rig.provision(t, ctx, "victoriametrics_backup", fixture)
	if err == nil {
		t.Fatal("provision succeeded on a backup that never finished")
	}
	var aerr *adapter.Error
	if !errors.As(err, &aerr) || aerr.Code != "source_corrupt" ||
		!strings.Contains(aerr.Message, "backup_complete.ignore") {
		t.Errorf("error = %v, want source_corrupt naming the missing marker", err)
	}
}

// removeOnePart deletes a part directory a partition's own parts.json
// names, and returns the part's name.
func removeOnePart(t *testing.T, backup string) string {
	t.Helper()
	var removed string
	err := filepath.WalkDir(backup, func(p string, d os.DirEntry, err error) error {
		if err != nil || removed != "" || !d.IsDir() || d.Name() != "parts.json" {
			return err
		}
		partition := filepath.Dir(p)
		entries, err := os.ReadDir(partition)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() && e.Name() != "parts.json" {
				removed = e.Name()
				return os.RemoveAll(filepath.Join(partition, e.Name()))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("truncate the fixture: %v", err)
	}
	if removed == "" {
		t.Fatal("the fixture holds no part to remove")
	}
	return removed
}

// assertCheck runs one MetricsQL check through the probe-declared runner
// — exactly how internal/checks runs checks without engine knowledge —
// and asserts the output carries the wanted fragment.
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

// buildAdapterOnPath builds the adapter binary and puts it on PATH under
// its protocol name.
func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-victoriametrics"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v: %s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func destroy(t *testing.T, sbx *docker.Sandbox) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sbx.Destroy(ctx); err != nil {
		t.Errorf("destroy sandbox: %v", err)
	}
}
