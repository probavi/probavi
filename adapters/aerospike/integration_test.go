//go:build integration

package main_test

import (
	"context"
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

// engineMemoryLimit caps every container this suite starts. The measured
// floor for a restore is 384 MiB — at 352 MiB asrestore exits 0 while the
// engine is OOM-killed — so this leaves one step of headroom.
const engineMemoryLimit = "512m"

// seedConfig is the configuration the fixtures' engine runs on. It is the
// adapter's own, spelled out here because the suite is a separate package:
// if the two ever disagree, the drill still restores what this seeded.
const seedConfig = `service {
	node-id a1
	proto-fd-max 1024
	cluster-name probavi
}

logging {
	console {
		context any info
	}
}

network {
	service {
		address 127.0.0.1
		port 3000
	}
	heartbeat {
		mode mesh
		address 127.0.0.1
		port 3002
		interval 150
		timeout 10
	}
	fabric {
		address 127.0.0.1
		port 3001
	}
}

namespace %s {
	replication-factor 1
	nsup-period 0
	allow-ttl-without-nsup true
	storage-engine memory {
		data-size 4G
	}
}
`

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

// sandboxParams start the sandbox idle, which this adapter requires: the
// engine has to come up on a configuration naming the artifact's namespace
// (see the adapter README).
func sandboxParams(image string) map[string]string {
	return map[string]string{"image": image, "command": "sleep infinity", "memory": engineMemoryLimit}
}

func buildAdapterOnPath(t *testing.T, ctx context.Context) {
	t.Helper()
	binDir := t.TempDir()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o",
		filepath.Join(binDir, "probavi-adapter-aerospike"), ".").CombinedOutput(); err != nil {
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

// run executes one shell command inside a sandbox and returns its exit
// code with the combined output.
func run(t *testing.T, ctx context.Context, sbx *docker.Sandbox, script string, args ...string) (int, string) {
	t.Helper()
	argv := append([]string{"sh", "-c", script, "sh"}, args...)
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: argv})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return out.ExitCode, string(out.Stdout) + string(out.Stderr)
}

// startEngineScript brings the engine up and waits for the signal a client
// can work on — cluster-stable, not status, which answers ok while a
// client is still refused.
const startEngineScript = `set -u
printf '%s' "$2" > /tmp/seed.conf
asd --config-file /tmp/seed.conf > /tmp/asd.log 2>&1
i=0
while [ $i -lt 120 ]; do
  key=$(asinfo -h 127.0.0.1 -v 'cluster-stable:' 2>/dev/null | tr -d '\r')
  case "$key" in ''|*ERROR*) ;; *) printf '%s' "$key" | grep -qE '^[0-9A-F]+$' && exit 0 ;; esac
  i=$((i+1)); sleep 1
done
tail -5 /tmp/asd.log >&2; exit 1`

func startEngine(t *testing.T, ctx context.Context, sbx *docker.Sandbox, namespace string) {
	t.Helper()
	if code, out := run(t, ctx, sbx, startEngineScript, "", fmt.Sprintf(seedConfig, namespace)); code != 0 {
		t.Fatalf("start engine: %s", out)
	}
}

// seedScript writes records through the engine's own client, optionally
// with a time to live, and backs the namespace up into a directory.
const seedScript = `set -u
ns=$1; set_=$2; n=$3; ttl=$4
{
  [ "$ttl" != 0 ] && printf 'SET RECORD_TTL %s\n' "$ttl"
  i=1
  while [ $i -le "$n" ]; do
    printf 'INSERT INTO %s.%s (PK, id, customer) VALUES ("order-%04d", %d, "cust-%d");\n' \
      "$ns" "$set_" "$i" "$i" "$i"
    i=$((i+1))
  done
} > /tmp/seed.aql
aql -h 127.0.0.1 -f /tmp/seed.aql > /tmp/seed.log 2>&1
grep -qi error /tmp/seed.log && { tail -3 /tmp/seed.log >&2; exit 1; }
rm -rf /tmp/bk
asbackup -h 127.0.0.1 -n "$ns" -d /tmp/bk 2>&1 | tail -1`

// makeBackup seeds a namespace and extracts the backup directory asbackup
// wrote — the artifact an operator hands a drill.
func makeBackup(t *testing.T, ctx context.Context, provider *docker.Provider,
	image, dest, namespace string, records, ttlSeconds int) {
	t.Helper()
	box, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create seed sandbox: %v", err)
	}
	defer destroy(t, box)
	startEngine(t, ctx, box, namespace)
	if code, out := run(t, ctx, box, seedScript,
		namespace, "orders", fmt.Sprint(records), fmt.Sprint(ttlSeconds)); code != 0 {
		t.Fatalf("seed and back up: %s", out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp",
		box.ID()+":/tmp/bk", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract fixture: %v: %s", err, out)
	}
}

// runnerArgv fills the probe-declared template the way internal/checks
// does.
func runnerArgv(probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText string) []string {
	argv := make([]string, 0, len(probe.SQLRunner.Argv))
	for _, a := range probe.SQLRunner.Argv {
		a = strings.ReplaceAll(a, "{{user}}", res.Connection.User)
		a = strings.ReplaceAll(a, "{{database}}", res.Connection.Database)
		argv = append(argv, strings.ReplaceAll(a, "{{sql}}", checkText))
	}
	return argv
}

func assertCheck(t *testing.T, ctx context.Context, sbx *docker.Sandbox,
	probe *adapter.ProbeResult, res *adapter.ProvisionResult, checkText, want string) {
	t.Helper()
	out, err := sbx.Exec(ctx, sandbox.ExecRequest{Argv: runnerArgv(probe, res, checkText), Env: probe.SQLRunner.Env})
	if err != nil {
		t.Fatalf("runner exec: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); out.ExitCode != 0 || got != want {
		t.Fatalf("check %q = %q (exit %d, stderr %s), want %q", checkText, got, out.ExitCode, out.Stderr, want)
	}
}

// TestEndToEndRestoreDrill proves the whole stack: the docker provider, the
// core-side protocol client and this adapter, as separate processes.
func TestEndToEndRestoreDrill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	dir := t.TempDir()
	backup := filepath.Join(dir, "nightly")
	makeBackup(t, ctx, provider, image, backup, "orders", 200, 0)

	runner, err := adapter.New("aerospike", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	probe, err := runner.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Name != "aerospike" || len(probe.SQLRunner.Argv) == 0 {
		t.Fatalf("probe = %+v", probe)
	}

	// Both kinds restore the same fixture: the directory as asbackup wrote
	// it, and the one file inside it on its own.
	entries, err := os.ReadDir(backup)
	if err != nil || len(entries) == 0 {
		t.Fatalf("fixture directory: %v (%d entries)", err, len(entries))
	}
	single := filepath.Join(backup, entries[0].Name())

	for _, tc := range []struct{ kind, path string }{
		{"asbackup_dir", backup},
		{"asbackup", single},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			sbx, err := provider.Create(ctx, sandboxParams(image))
			if err != nil {
				t.Fatalf("create drill sandbox: %v", err)
			}
			defer destroy(t, sbx)

			res, err := runner.Provision(ctx, &adapter.ProvisionRequest{
				Source:  adapter.ProvisionSource{Kind: tc.kind, Path: tc.path},
				Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
			}, sbx)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if res.Connection.Database != "orders" {
				t.Errorf("the namespace must come from the artifact: %+v", res.Connection)
			}
			if res.Timings.RestoreSeconds <= 0 || res.Timings.EngineReadySeconds <= 0 {
				t.Errorf("timings = %+v, want real measurements", res.Timings)
			}
			if !strings.HasPrefix(res.SourceIdentity.Checksum, "sha256:") || res.SourceIdentity.SizeBytes == 0 {
				t.Errorf("source identity = %+v", res.SourceIdentity)
			}
			if res.SourceIdentity.CreatedAt != nil {
				t.Errorf("created_at = %v, want null — an .asb header carries no clock",
					*res.SourceIdentity.CreatedAt)
			}

			health, err := runner.Healthcheck(ctx, &res.Connection, res.State, sbx)
			if err != nil {
				t.Fatalf("healthcheck: %v", err)
			}
			if !health.Healthy {
				t.Fatalf("healthcheck = %+v, want healthy", health)
			}

			// Both halves of the runner: the count comes from the engine's
			// own info interface, because aql has no count(*), and a
			// record reads back as itself through aql.
			assertCheck(t, ctx, sbx, probe, res, "info:sets/orders/orders objects", "200")
			assertCheck(t, ctx, sbx, probe, res,
				`SELECT customer FROM orders.orders WHERE PK = "order-0042"`, "order-0042\tcust-42")
			// The runner fails a check the engine refuses, which aql alone
			// does not: it exits 0 for an invalid namespace.
			out, err := sbx.Exec(ctx, sandbox.ExecRequest{
				Argv: runnerArgv(probe, res, "SELECT id FROM nosuch.nosuch"), Env: probe.SQLRunner.Env})
			if err != nil {
				t.Fatalf("runner exec: %v", err)
			}
			if out.ExitCode == 0 {
				t.Errorf("a check against a namespace that does not exist exited 0: %s", out.Stdout)
			}

			if _, err := runner.Teardown(ctx, res.State, "completed", sbx); err != nil {
				t.Fatalf("teardown: %v", err)
			}
		})
	}
}

// TestABackupOfExpiredDataIsRefused is the issue #166 fence against the
// real engine. A record's expiry travels inside the artifact as an
// absolute instant; once it passes, asrestore drops the record and still
// exits 0, so the drill has to refuse on the counter rather than report a
// green restore of nothing.
func TestABackupOfExpiredDataIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	backup := filepath.Join(t.TempDir(), "expiring")
	makeBackup(t, ctx, provider, image, backup, "orders", 20, 5)
	// The backup is taken the moment the records are written, so the wait
	// is what makes their recorded expiry a past instant.
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(10 * time.Second):
	}

	runner, err := adapter.New("aerospike", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "asbackup_dir", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err == nil {
		t.Fatal("a backup whose records had all expired was reported as restored")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("provision error = %v, want it to name the expiry", err)
	}
}

// TestAWellFormedZeroIsRefused covers the artifact an empty namespace
// produces: 42 bytes of valid header that restore with a zero exit code.
func TestAWellFormedZeroIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	buildAdapterOnPath(t, ctx)
	image := verifiedImage(t)
	provider := docker.New(nil)

	backup := filepath.Join(t.TempDir(), "empty")
	makeBackup(t, ctx, provider, image, backup, "orders", 0, 0)

	runner, err := adapter.New("aerospike", nil, nil)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	sbx, err := provider.Create(ctx, sandboxParams(image))
	if err != nil {
		t.Fatalf("create drill sandbox: %v", err)
	}
	defer destroy(t, sbx)

	_, err = runner.Provision(ctx, &adapter.ProvisionRequest{
		Source:  adapter.ProvisionSource{Kind: "asbackup_dir", Path: backup},
		Sandbox: adapter.SandboxInfo{ScratchDir: sbx.ScratchDir()},
	}, sbx)
	if err == nil {
		t.Fatal("an empty backup was reported as restored")
	}
	if !strings.Contains(err.Error(), "inserted no records") {
		t.Errorf("provision error = %v, want it to name the empty restore", err)
	}
}
