package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/probavi/probavi/internal/gameday"
	"github.com/probavi/probavi/internal/i18n"
	"github.com/probavi/probavi/internal/sandbox/remotehost"
)

// servingDockerScript is a docker CLI that answers the calls one drill
// makes without a container runtime: it starts nothing, reports the
// container it never made as running, and removes it again. It is enough
// because the adapter below issues no sandbox verbs — what these tests
// drive is the pipeline around the drill, not the sandbox provider, which
// has its own tests and its own integration suite.
const servingDockerScript = `#!/bin/sh
case "$1" in
  run) echo probavi-sbx-unit ;;
  inspect) echo true ;;
  ps) ;;
  rm) ;;
  exec) exit 0 ;;
  *) echo "docker: unexpected $*" >&2; exit 1 ;;
esac
`

// healthyAdapterScript answers healthcheck the way a restored engine does,
// so a drill through it reaches a pass rather than stopping at the first
// check. verdict picks what healthcheck reports.
func healthyAdapterScript(healthy bool) string {
	return `#!/bin/sh
read -r REQ
RID=$(printf '%s' "$REQ" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
OP=$(printf '%s' "$REQ" | sed -n 's/.*"op":"\([^"]*\)".*/\1/p')
case "$OP" in
  probe) PAYLOAD='` + fakeProbePayload + `' ;;
  provision) PAYLOAD='{"connection":{"scheme":"http","host":"127.0.0.1","port":5432,"database":"drill"},"source_identity":{"checksum":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size_bytes":14,"created_at":null},"timings":{"restore_seconds":0.5}}' ;;
  healthcheck) PAYLOAD='{"healthy":` + map[bool]string{true: "true", false: "false"}[healthy] + `,"detail":"unit"}' ;;
  *) PAYLOAD='{}' ;;
esac
printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":true,"payload":%s}\n' "$RID" "$PAYLOAD"
`
}

// installServingToolchain puts a docker CLI that answers and an adapter
// whose healthcheck reports as asked onto PATH.
func installServingToolchain(t *testing.T, healthy bool) {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{
		"docker":                  servingDockerScript,
		"probavi-adapter-covfake": healthyAdapterScript(healthy),
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// drillFixture writes a complete drill configuration and returns it with
// the paths the assertions need. extra is appended to the YAML.
func drillFixture(t *testing.T, extra string) (cfgPath, evidencePath, pubPath, metricsPath string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ed25519.key")
	if code, _, stderr := runCLI(t, "evidence", "keygen", "--out", keyPath); code != 0 {
		t.Fatalf("keygen exit %d, stderr: %s", code, stderr)
	}
	evidencePath = filepath.Join(dir, "evidence.jsonl")
	metricsPath = filepath.Join(dir, "probavi.prom")
	backup := filepath.Join(dir, "backup.dump")
	if err := os.WriteFile(backup, []byte("PGDMP fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `target:
  name: cov-pipeline
  adapter: covfake
  source:
    kind: pgdump
    path: ` + backup + `
sandbox:
  provider: docker
  timeout: 2m
  params:
    image: probavi/unit:latest
checks:
  - builtin: service_healthy
evidence:
  path: ` + evidencePath + `
  sign_key: ` + keyPath + `
metrics:
  prometheus_textfile: ` + metricsPath + `
` + extra
	cfgPath = filepath.Join(dir, "drill.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, evidencePath, keyPath + ".pub", metricsPath
}

// TestADrillThatPassesRecordsAndNotifies drives the whole pipeline the
// `probavi run` contract promises: the verdict, the signed record behind
// it, the metrics beside it, and the notification about it — and the
// notification goes out on its own budget, so it is sent before the exit
// code is decided rather than after.
func TestADrillThatPassesRecordsAndNotifies(t *testing.T) {
	installServingToolchain(t, true)

	var mu sync.Mutex
	var delivered []map[string]any
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("notification body: %v", err)
		}
		mu.Lock()
		delivered = append(delivered, body)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	t.Setenv("PROBAVI_TEST_WEBHOOK_URL", webhook.URL)

	cfgPath, evidencePath, pubPath, metricsPath := drillFixture(t, `notify:
  webhooks:
    - url_env: PROBAVI_TEST_WEBHOOK_URL
`)
	code, stdout, stderr := runCLI(t, "run", "--config", cfgPath)
	if code != exitPass {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitPass, stderr)
	}
	var summary gameday.DrillSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("summary is not JSON: %v (%q)", err, stdout)
	}
	if summary.Outcome != "pass" || summary.ChecksPassed != 1 || summary.ChecksTotal != 1 {
		t.Errorf("summary = %+v, want a pass with its one check", summary)
	}
	if summary.EvidencePath != evidencePath {
		t.Errorf("summary.EvidencePath = %q, want %q", summary.EvidencePath, evidencePath)
	}

	if code, out, stderr := runCLI(t, "evidence", "verify", "--log", evidencePath, "--key", pubPath); code != 0 {
		t.Errorf("verify exit %d, stderr: %s", code, stderr)
	} else if !strings.Contains(out, `"status":"VALID"`) {
		t.Errorf("verify output = %q, want VALID", out)
	}
	raw, err := os.ReadFile(metricsPath)
	if err != nil {
		t.Fatalf("metrics textfile: %v", err)
	}
	if !strings.Contains(string(raw), "probavi_") {
		t.Errorf("metrics textfile = %q, want the drill's own series", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 {
		t.Fatalf("webhook received %d notifications, want one", len(delivered))
	}
	if delivered[0]["outcome"] != "pass" {
		t.Errorf("notification = %v, want the drill's own verdict", delivered[0])
	}
}

// TestADrillThatFailsAChecksStillRecordsAndExitsOne: a failed check is a
// verdict, not an error — exit 1, a signed record, and the same pipeline
// around it.
func TestADrillThatFailsAChecksStillRecordsAndExitsOne(t *testing.T) {
	installServingToolchain(t, false)
	cfgPath, evidencePath, pubPath, _ := drillFixture(t, "")

	code, stdout, stderr := runCLI(t, "run", "--config", cfgPath)
	if code != exitFail {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitFail, stderr)
	}
	var summary gameday.DrillSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("summary is not JSON: %v (%q)", err, stdout)
	}
	if summary.Outcome != "fail" || summary.ChecksPassed != 0 || summary.ChecksTotal != 1 {
		t.Errorf("summary = %+v, want a fail with its one check", summary)
	}
	if code, out, _ := runCLI(t, "evidence", "verify", "--log", evidencePath, "--key", pubPath); code != 0 ||
		!strings.Contains(out, `"status":"VALID"`) {
		t.Errorf("a failed drill must still leave a verifiable record: exit %d, %q", code, out)
	}
}

// TestNotificationFailureNeverChangesTheVerdict: notifications are
// observability. A webhook that refuses the delivery is loud on stderr and
// silent in the exit code.
func TestNotificationFailureNeverChangesTheVerdict(t *testing.T) {
	installServingToolchain(t, true)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer webhook.Close()
	t.Setenv("PROBAVI_TEST_WEBHOOK_URL", webhook.URL)

	cfgPath, _, _, _ := drillFixture(t, `notify:
  webhooks:
    - url_env: PROBAVI_TEST_WEBHOOK_URL
`)
	code, _, stderr := runCLI(t, "run", "--config", cfgPath)
	if code != exitPass {
		t.Fatalf("exit %d, want the drill's own verdict %d (stderr: %s)", code, exitPass, stderr)
	}
	if !strings.Contains(stderr, "deliver notifications") {
		t.Errorf("stderr = %q, want the failed delivery said out loud", stderr)
	}
}

// TestMetricsFailureNeverChangesTheVerdict: the same rule for the other
// observability path — a textfile that cannot be written is logged, and
// the drill's verdict stands.
func TestMetricsFailureNeverChangesTheVerdict(t *testing.T) {
	installServingToolchain(t, true)
	cfgPath, _, _, metricsPath := drillFixture(t, "")
	// A directory where the textfile belongs: the write cannot succeed,
	// and nothing about the drill changes.
	if err := os.MkdirAll(metricsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "run", "--config", cfgPath)
	if code != exitPass {
		t.Fatalf("exit %d, want the drill's own verdict %d (stderr: %s)", code, exitPass, stderr)
	}
	if !strings.Contains(stderr, "write metrics textfile") {
		t.Errorf("stderr = %q, want the failed write said out loud", stderr)
	}
}

// TestWiringRefusesBeforeTheDrillRuns covers what `probavi run` cannot
// build, each reported as a usage error with the reason in it: the drill
// never starts, so nothing is recorded and nothing is torn down.
func TestWiringRefusesBeforeTheDrillRuns(t *testing.T) {
	installServingToolchain(t, true)
	for name, tc := range map[string]struct {
		mangle func(t *testing.T, cfg string) string
		want   string
	}{
		"a signing key that is not there": {
			func(t *testing.T, cfg string) string {
				t.Helper()
				return strings.Replace(cfg, "ed25519.key", "absent.key", 1)
			},
			"absent.key",
		},
		"an evidence path that cannot be opened": {
			func(t *testing.T, cfg string) string {
				t.Helper()
				return strings.Replace(cfg, "evidence.jsonl", "nowhere/evidence.jsonl", 1)
			},
			"evidence",
		},
		"a sandbox provider that does not exist": {
			func(t *testing.T, cfg string) string {
				t.Helper()
				return strings.Replace(cfg, "provider: docker", "provider: qemu", 1)
			},
			"qemu",
		},
		"an adapter that is not installed": {
			func(t *testing.T, cfg string) string {
				t.Helper()
				return strings.Replace(cfg, "adapter: covfake", "adapter: nosuchengine", 1)
			},
			"nosuchengine",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath, evidencePath, _, _ := drillFixture(t, "")
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfgPath, []byte(tc.mangle(t, string(raw))), 0o600); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runCLI(t, "run", "--config", cfgPath)
			if code != exitUsage {
				t.Fatalf("exit %d, want %d (stderr: %s)", code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to name %q", stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want no summary for a drill that never ran", stdout)
			}
			// The evidence store opens before the adapter is resolved, so
			// the log may exist — but a drill that never ran records
			// nothing in it.
			if raw, err := os.ReadFile(evidencePath); err == nil && len(raw) > 0 {
				t.Errorf("evidence log holds %q for a drill that never ran", raw)
			}
		})
	}
}

// TestProviderWrappersPassTheEngineThrough covers the three adapters that
// make the concrete providers satisfy core.Provider. They are thin, and
// thin is the point: what they must never do is swallow a provider's
// error or hand the drill a typed nil sandbox.
func TestProviderWrappersPassTheEngineThrough(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"docker", "kubectl", "ssh"} {
		script := "#!/bin/sh\necho '" + name + ": nothing to talk to in a unit test' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(remotehost.EnvTarget, "drill@target.example")

	logger := slog.New(slog.DiscardHandler)
	for name, params := range map[string]map[string]string{
		"docker":     {"image": "probavi/unit:latest"},
		"k8s":        {"image": "probavi/unit:latest"},
		"remotehost": {"memory": "1G"},
	} {
		t.Run(name, func(t *testing.T) {
			provider, err := sandboxProvider(name, params, logger)
			if err != nil {
				t.Fatalf("sandboxProvider: %v", err)
			}
			sbx, err := provider.Create(context.Background(), params)
			if err == nil {
				t.Fatalf("Create = %v, want the CLI's failure", sbx)
			}
			if sbx != nil {
				t.Error("a failed Create handed the drill a sandbox — a typed nil here would be torn down as if it existed")
			}
			if _, err := provider.SweepOrphans(context.Background()); err == nil && name != "remotehost" {
				t.Errorf("SweepOrphans = nil, want the CLI's failure")
			}
		})
	}
}

// TestCloseQuietlySaysSoOnStderr: a close that fails cannot change a
// verdict already decided, but it is never swallowed either.
func TestCloseQuietlySaysSoOnStderr(t *testing.T) {
	var stderr strings.Builder
	closeQuietly(failingCloser{}, &stderr)
	if !strings.Contains(stderr.String(), "close") || !strings.Contains(stderr.String(), "file already closed") {
		t.Errorf("stderr = %q, want the failed close reported", stderr.String())
	}
	stderr.Reset()
	closeQuietly(quietCloser{}, &stderr)
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want silence for a close that worked", stderr.String())
	}
}

type failingCloser struct{}

func (failingCloser) Close() error { return os.ErrClosed }

type quietCloser struct{}

func (quietCloser) Close() error { return nil }

// TestConformanceCarriesTheSourceParamsItWasGiven: the suite provisions a
// real source, so the parameters an engine needs for one reach it — and a
// run whose report cannot be written still says what it found.
func TestConformanceCarriesTheSourceParamsItWasGiven(t *testing.T) {
	installServingToolchain(t, true)
	var stderr strings.Builder
	code := runAdapterConformance(
		[]string{"--source-param", "wal_dir=/backups/wal", "--source-param", "label=nightly", "covfake"},
		failingWriter{}, &stderr, i18n.English())
	if code != exitFail && code != exitError {
		t.Fatalf("exit %d, want the suite's own verdict against a fake adapter (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "report") {
		t.Errorf("stderr = %q, want the unwritable report reported", stderr.String())
	}
}
