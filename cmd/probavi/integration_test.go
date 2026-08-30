//go:build integration

package main_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// TestFullDrillViaCLI is the README quickstart as a test: build the real
// binaries, generate keys, run a drill against a real pg_dump in a real
// Docker sandbox, then verify the evidence log offline — including a
// second, failing drill chained onto the same log.
func TestFullDrillViaCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	work := t.TempDir()

	probavi := build(t, ctx, work, "probavi", ".")
	build(t, ctx, work, "probavi-adapter-postgres", "../../adapters/postgres")
	t.Setenv("PATH", work+string(os.PathListSeparator)+os.Getenv("PATH"))

	fixture := filepath.Join(work, "orders.dump")
	makeFixture(t, ctx, fixture)

	keyPath := filepath.Join(work, "ed25519.key")
	mustRun(t, ctx, probavi, "evidence", "keygen", "--out", keyPath)

	// A webhook receiver observes both drills; the CLI subprocess inherits
	// the URL and HMAC secret through the environment, never through config.
	hook := &webhookCapture{}
	hookSrv := httptest.NewServer(hook)
	defer hookSrv.Close()
	t.Setenv("PROBAVI_IT_WEBHOOK_URL", hookSrv.URL)
	t.Setenv("PROBAVI_IT_WEBHOOK_SECRET", "it-webhook-secret")
	notifyBlock := `notify:
  webhooks:
    - url_env: PROBAVI_IT_WEBHOOK_URL
      secret_env: PROBAVI_IT_WEBHOOK_SECRET
`

	logPath := filepath.Join(work, "evidence.jsonl")
	metricsPath := filepath.Join(work, "probavi.prom")
	configPath := writeDrillConfig(t, work, "cli-e2e-drill", fixture, logPath, keyPath, metricsPath, notifyBlock)

	// Drill 1: a healthy backup must prove restorable, exit 0.
	out := mustRun(t, ctx, probavi, "run", "--config", configPath)
	summary := struct {
		Outcome      string `json:"outcome"`
		Seq          int64  `json:"seq"`
		ChecksPassed int    `json:"checks_passed"`
		ChecksTotal  int    `json:"checks_total"`
		RestoreMS    *int64 `json:"restore_ms"`
	}{}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("run summary is not JSON: %v (%q)", err, out)
	}
	if summary.Outcome != "pass" || summary.Seq != 1 || summary.ChecksPassed != summary.ChecksTotal ||
		summary.RestoreMS == nil || *summary.RestoreMS <= 0 {
		t.Fatalf("summary = %+v, want a passing drill with measured restore time", summary)
	}
	if raw, err := os.ReadFile(metricsPath); err != nil ||
		!strings.Contains(string(raw), "probavi_last_success_timestamp_seconds") ||
		!strings.Contains(string(raw), `probavi_restore_duration_rolling_seconds`) ||
		!strings.Contains(string(raw), `quantile="0.95"`) ||
		!strings.Contains(string(raw), "probavi_restore_trend_samples") {
		t.Errorf("metrics file must carry the last-run and rolling-trend series: err=%v content=%s", err, raw)
	}

	// Drill 2: a corrupt backup must fail with exit 1 — and still leave a
	// signed record on the same chain.
	corrupt := filepath.Join(work, "corrupt.dump")
	if err := os.WriteFile(corrupt, []byte("not an archive"), 0o600); err != nil {
		t.Fatalf("write corrupt dump: %v", err)
	}
	corruptConfig := writeDrillConfig(t, t.TempDir(), "cli-e2e-drill", corrupt, logPath, keyPath, metricsPath, notifyBlock)
	out, code := run(t, ctx, probavi, "run", "--config", corruptConfig)
	if code != 1 {
		t.Fatalf("corrupt drill exit = %d (%s), want 1 — a bad backup is a recoverability failure", code, out)
	}

	// Both drills must have notified: pass then fail, HMAC-signed, pointing
	// at the records just written (docs/notifications.md).
	deliveries := hook.snapshot()
	if len(deliveries) != 2 {
		t.Fatalf("webhook received %d deliveries, want 2", len(deliveries))
	}
	for i, want := range []struct {
		outcome string
		seq     int64
	}{{"pass", 1}, {"fail", 2}} {
		d := deliveries[i]
		if got := d.header.Get("X-Probavi-Event"); got != "drill.completed" {
			t.Errorf("delivery %d: X-Probavi-Event = %q", i, got)
		}
		mac := hmac.New(sha256.New, []byte("it-webhook-secret"))
		if _, err := mac.Write(d.body); err != nil {
			t.Fatalf("hmac: %v", err)
		}
		if got := d.header.Get("X-Probavi-Signature-256"); got != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Errorf("delivery %d: signature %q does not verify against the shared secret", i, got)
		}
		payload := struct {
			Schema  string `json:"schema"`
			Outcome string `json:"outcome"`
			Seq     int64  `json:"seq"`
			Drill   struct {
				Name string `json:"name"`
			} `json:"drill"`
		}{}
		if err := json.Unmarshal(d.body, &payload); err != nil {
			t.Fatalf("delivery %d payload: %v (%s)", i, err, d.body)
		}
		if payload.Schema != "probavi-notification/1" || payload.Outcome != want.outcome ||
			payload.Seq != want.seq || payload.Drill.Name != "cli-e2e-drill" {
			t.Errorf("delivery %d payload = %+v, want %s at seq %d", i, payload, want.outcome, want.seq)
		}
	}

	// Offline verification: two records, chained, VALID, exit 0.
	out = mustRun(t, ctx, probavi, "evidence", "verify", "--log", logPath, "--key", keyPath+".pub")
	verify := struct {
		Status  string `json:"status"`
		Records int    `json:"records"`
	}{}
	if err := json.Unmarshal([]byte(out), &verify); err != nil {
		t.Fatalf("verify output: %v", err)
	}
	if verify.Status != "VALID" || verify.Records != 2 {
		t.Fatalf("verify = %+v, want VALID with 2 records", verify)
	}

	// No ORPHANED sandbox may survive a drill: containers whose owner
	// process is dead. Live-owner containers belong to integration tests
	// of other packages running in parallel and must be tolerated, which
	// is why the question is asked through sandbox.OwnerAlive rather than
	// by hand: the label is an owner id, and since owner ids grew a
	// pid-reuse token it has not been a bare pid.
	for _, id := range strings.Fields(dockerOut(t, ctx, "ps", "-aq", "--filter", "label=com.probavi.sandbox=1")) {
		out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
			`{{ index .Config.Labels "com.probavi.pid" }}`, id).Output()
		if err != nil {
			continue // vanished between ps and inspect — that IS the cleanup working
		}
		owner := strings.TrimSpace(string(out))
		if !sandbox.OwnerAlive(owner) {
			t.Errorf("orphaned sandbox %s (dead owner %s) survived the drill", id, owner)
		}
	}

	// probavi adapter probe round-trips through the real adapter binary.
	out = mustRun(t, ctx, probavi, "adapter", "probe", "postgres")
	if !strings.Contains(out, `"name":"postgres"`) {
		t.Errorf("adapter probe output: %s", out)
	}
}

// TestGameDayViaCLI proves the DR game-day path end to end: three member
// drills — healthy, corrupt, and a dependent of the corrupt one — run in
// dependency order against real Docker, chain their records into one
// shared evidence log, and produce the docs/gameday.md §5 summary with
// the dependent skipped.
func TestGameDayViaCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	work := t.TempDir()

	probavi := build(t, ctx, work, "probavi", ".")
	build(t, ctx, work, "probavi-adapter-postgres", "../../adapters/postgres")
	t.Setenv("PATH", work+string(os.PathListSeparator)+os.Getenv("PATH"))

	fixture := filepath.Join(work, "orders.dump")
	makeFixture(t, ctx, fixture)
	corrupt := filepath.Join(work, "corrupt.dump")
	if err := os.WriteFile(corrupt, []byte("not an archive"), 0o600); err != nil {
		t.Fatalf("write corrupt dump: %v", err)
	}

	keyPath := filepath.Join(work, "ed25519.key")
	mustRun(t, ctx, probavi, "evidence", "keygen", "--out", keyPath)
	logPath := filepath.Join(work, "evidence.jsonl")

	// Members live in subdirectories; the game-day references them by
	// relative path, proving the resolution rule.
	members := []struct{ dir, name, source string }{
		{"m1", "gd-core-db", fixture},
		{"m2", "gd-corrupt-db", corrupt},
		{"m3", "gd-reporting-db", fixture},
	}
	for _, m := range members {
		sub := filepath.Join(work, m.dir)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
		writeDrillConfig(t, sub, m.name, m.source, logPath, keyPath, filepath.Join(sub, "probavi.prom"), "")
	}
	gdPath := filepath.Join(work, "gameday.yaml")
	gd := `name: gd-e2e
timeout: 6m
members:
  - name: core-db
    config: m1/drill.yaml
  - name: corrupt-db
    config: m2/drill.yaml
  - name: reporting-db
    config: m3/drill.yaml
    depends_on: [corrupt-db]
`
	if err := os.WriteFile(gdPath, []byte(gd), 0o600); err != nil {
		t.Fatalf("write game-day config: %v", err)
	}

	out, code := run(t, ctx, probavi, "gameday", "--config", gdPath)
	if code != 1 {
		t.Fatalf("game-day exit = %d (%s), want 1 — one member drill failed", code, out)
	}
	summary := struct {
		GameDay string `json:"gameday"`
		Outcome string `json:"outcome"`
		TotalMS int64  `json:"total_ms"`
		Members []struct {
			Name       string `json:"name"`
			Outcome    string `json:"outcome"`
			Seq        int64  `json:"seq"`
			SkipReason string `json:"skip_reason"`
			DurationMS int64  `json:"duration_ms"`
		} `json:"members"`
	}{}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("game-day summary is not JSON: %v (%q)", err, out)
	}
	if summary.GameDay != "gd-e2e" || summary.Outcome != "fail" || len(summary.Members) != 3 {
		t.Fatalf("summary = %+v, want gd-e2e fail with 3 members", summary)
	}
	if m := summary.Members[0]; m.Outcome != "pass" || m.Seq != 1 || m.DurationMS <= 0 {
		t.Errorf("core-db = %+v, want pass at seq 1 with measured duration", m)
	}
	if m := summary.Members[1]; m.Outcome != "fail" || m.Seq != 2 {
		t.Errorf("corrupt-db = %+v, want fail at seq 2", m)
	}
	if m := summary.Members[2]; m.Outcome != "skipped" || !strings.Contains(m.SkipReason, "corrupt-db did not pass (fail)") {
		t.Errorf("reporting-db = %+v, want skipped behind corrupt-db", m)
	}
	if summary.TotalMS <= 0 {
		t.Errorf("total_ms = %d, want the exercise wall clock", summary.TotalMS)
	}

	// The shared log chained both member records in execution order and
	// verifies offline; the skipped member correctly left no record.
	out = mustRun(t, ctx, probavi, "evidence", "verify", "--log", logPath, "--key", keyPath+".pub")
	verify := struct {
		Status  string `json:"status"`
		Records int    `json:"records"`
	}{}
	if err := json.Unmarshal([]byte(out), &verify); err != nil {
		t.Fatalf("verify output: %v", err)
	}
	if verify.Status != "VALID" || verify.Records != 2 {
		t.Fatalf("verify = %+v, want VALID with exactly 2 records", verify)
	}
}

// webhookCapture is a race-safe notification receiver.
type webhookCapture struct {
	mu         sync.Mutex
	deliveries []webhookDelivery
}

type webhookDelivery struct {
	header http.Header
	body   []byte
}

func (c *webhookCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deliveries = append(c.deliveries, webhookDelivery{header: r.Header.Clone(), body: body})
}

func (c *webhookCapture) snapshot() []webhookDelivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]webhookDelivery(nil), c.deliveries...)
}

func writeDrillConfig(t *testing.T, dir, name, source, logPath, keyPath, metricsPath, notify string) string {
	t.Helper()
	cfg := fmt.Sprintf(`target:
  name: %s
  adapter: postgres
  source:
    kind: pgdump
    path: %s
sandbox:
  provider: docker
  params:
    image: postgres:16
    env.POSTGRES_HOST_AUTH_METHOD: trust
  timeout: 5m
checks:
  - builtin: service_healthy
  - builtin: table_exists
    table: orders
  - builtin: row_count
    table: orders
    min: 1000
    max: 1000
  - name: no-negative-totals
    sql: "SELECT count(*) FROM orders WHERE total < 0"
    expect: 0
evidence:
  path: %s
  sign_key: %s
metrics:
  prometheus_textfile: %s
`, name, source, logPath, keyPath, metricsPath)
	cfg += notify
	path := filepath.Join(dir, "drill.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write drill config: %v", err)
	}
	return path
}

func makeFixture(t *testing.T, ctx context.Context, dest string) {
	t.Helper()
	id := dockerOut(t, ctx, "run", "-d", "--label", "com.probavi.test-seed=1",
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:16")
	defer func() { _ = exec.Command("docker", "rm", "-f", "-v", id).Run() }() //nolint:errcheck // best-effort cleanup

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := exec.CommandContext(ctx, "docker", "exec", id,
			"pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-q").Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, lerr := exec.CommandContext(ctx, "docker", "logs", "--tail", "30", id).CombinedOutput()
			t.Fatalf("seed engine never became ready (docker logs err=%v):\n%s", lerr, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
	dockerOut(t, ctx, "exec", id, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c",
		`CREATE TABLE orders (id bigserial PRIMARY KEY, total numeric(10,2) NOT NULL);
INSERT INTO orders (total) SELECT (random()*100)::numeric(10,2) FROM generate_series(1,1000);`)
	dockerOut(t, ctx, "exec", id, "pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-Fc", "-f", "/tmp/f.dump", "postgres")
	dockerOut(t, ctx, "cp", id+":/tmp/f.dump", dest)
}

func build(t *testing.T, ctx context.Context, dir, name, pkg string) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", name, err, out)
	}
	return bin
}

func run(t *testing.T, ctx context.Context, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if ok := isExitError(err, &exitErr); ok {
			return string(out), exitErr.ExitCode()
		}
		t.Fatalf("run %s %v: %v", bin, args, err)
	}
	return string(out), 0
}

func mustRun(t *testing.T, ctx context.Context, bin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var exitErr *exec.ExitError
		if isExitError(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("%s %v: %v\nstderr: %s", filepath.Base(bin), args, err, stderr)
	}
	return string(out)
}

func dockerOut(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	// Output, not CombinedOutput: docker streams pull progress to stderr on
	// a cold image cache, and mixing it in corrupts captured container ids.
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if isExitError(err, &exitErr) {
			t.Fatalf("docker %v: %v: %s", args, err, exitErr.Stderr)
		}
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func isExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
