package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/probavi/probavi/internal/evidence"
)

func i64(n int64) *int64   { return &n }
func str(s string) *string { return &s }

func sampleRecord(outcome evidence.Outcome) *evidence.Record {
	return &evidence.Record{
		TS:    "2026-07-31T02:00:11.482Z",
		Drill: evidence.Drill{Name: "prod-orders-db"},
		Timings: evidence.Timings{
			Restore: i64(190),
		},
		Checks: []evidence.Check{
			{Name: "service_healthy", OK: true, Detail: str("ok")},
			{Name: "row_count:orders", OK: outcome == evidence.OutcomePass},
		},
		Outcome: outcome,
	}
}

func TestWriteTextfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probavi.prom")
	if err := WriteTextfile(path, sampleRecord(evidence.OutcomePass), nil); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	content := string(raw)
	for _, want := range []string{
		`probavi_restore_duration_seconds{drill="prod-orders-db"} 0.19` + "\n",
		`probavi_checks_passed{drill="prod-orders-db"} 2` + "\n",
		`probavi_checks_total{drill="prod-orders-db"} 2` + "\n",
		`probavi_last_run_timestamp_seconds{drill="prod-orders-db"} 1785463211.482` + "\n",
		`probavi_last_success_timestamp_seconds{drill="prod-orders-db"} 1785463211.482` + "\n",
		"# HELP probavi_restore_duration_seconds ",
		"# TYPE probavi_checks_passed gauge",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("metrics output missing %q:\n%s", want, content)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("tempfile left behind — the rename must consume it")
	}
}

func TestWriteTextfileFailedDrill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probavi.prom")
	rec := sampleRecord(evidence.OutcomeFail)
	rec.Timings.Restore = nil // restore never ran
	if err := WriteTextfile(path, rec, nil); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	content := string(raw)
	if strings.Contains(content, "probavi_last_success_timestamp_seconds") {
		t.Error("failed drill must not advance the last-success timestamp")
	}
	if strings.Contains(content, "probavi_restore_duration_seconds") {
		t.Error("a phase that never ran must not be reported")
	}
	if !strings.Contains(content, `probavi_checks_passed{drill="prod-orders-db"} 1`) {
		t.Errorf("checks_passed must count only OK checks:\n%s", content)
	}
}

func TestWriteTextfileEdges(t *testing.T) {
	rec := sampleRecord(evidence.OutcomePass)
	rec.Drill.Name = "we\"ird\\name\nx"
	path := filepath.Join(t.TempDir(), "probavi.prom")
	if err := WriteTextfile(path, rec, nil); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if !strings.Contains(string(raw), `drill="we\"ird\\name\nx"`) {
		t.Errorf("label escaping broken:\n%s", raw)
	}

	rec.TS = "not-a-timestamp"
	if err := WriteTextfile(path, rec, nil); err == nil {
		t.Error("malformed record ts must be an error")
	}
	if err := WriteTextfile(filepath.Join(t.TempDir(), "no", "dir", "x.prom"),
		sampleRecord(evidence.OutcomePass), nil); err == nil {
		t.Error("unwritable path must be an error")
	}
}

// logLine builds one evidence-log line with just the fields the trend
// scanner reads; metrics never verify, so the rest may be absent.
func logLine(drill string, restoreMS int64) string {
	return fmt.Sprintf(`{"drill":{"name":%q},"timings_ms":{"restore":%d}}`, drill, restoreMS)
}

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

func TestRestoreTrend(t *testing.T) {
	lines := []string{
		logLine("other-drill", 99999),            // other drills never mix in
		`{"drill":{"name":"d"},"timings_ms":{}}`, // restore never ran: no sample
		"{torn tail fragment",                    // damage is verify's business
		logLine("d", 300), logLine("d", 100), logLine("d", 200),
	}
	trend, err := RestoreTrend(writeLog(t, lines...), "d")
	if err != nil {
		t.Fatalf("RestoreTrend: %v", err)
	}
	if trend.Samples != 3 || trend.P50 != 200 || trend.P95 != 300 || trend.Max != 300 {
		t.Errorf("trend = %+v, want 3 samples with p50 200, p95 300, max 300", trend)
	}
}

func TestRestoreTrendWindowSlides(t *testing.T) {
	lines := make([]string, 0, TrendWindow+5)
	for i := 1; i <= TrendWindow+5; i++ {
		lines = append(lines, logLine("d", int64(i)))
	}
	trend, err := RestoreTrend(writeLog(t, lines...), "d")
	if err != nil {
		t.Fatalf("RestoreTrend: %v", err)
	}
	// The first 5 samples fell out of the window: values are 6..105.
	if trend.Samples != TrendWindow || trend.P50 != 55 || trend.P95 != 100 || trend.Max != 105 {
		t.Errorf("trend = %+v, want the oldest samples evicted (p50 55, p95 100, max 105)", trend)
	}
}

func TestRestoreTrendEdges(t *testing.T) {
	trend, err := RestoreTrend(writeLog(t, logLine("d", 42)), "d")
	if err != nil || trend.Samples != 1 || trend.P50 != 42 || trend.P95 != 42 || trend.Max != 42 {
		t.Errorf("single sample: %+v err=%v, want every quantile 42", trend, err)
	}

	trend, err = RestoreTrend(writeLog(t, logLine("elsewhere", 1)), "d")
	if err != nil || trend.Samples != 0 {
		t.Errorf("no samples: %+v err=%v, want empty trend", trend, err)
	}

	if _, err := RestoreTrend(filepath.Join(t.TempDir(), "missing.jsonl"), "d"); err == nil {
		t.Error("a missing log must be an error, not an empty trend")
	}
}

func TestWriteTextfileRendersTrend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probavi.prom")
	trend := &Trend{Samples: 12, P50: 250, P95: 900, Max: 1200}
	if err := WriteTextfile(path, sampleRecord(evidence.OutcomePass), trend); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		`probavi_restore_duration_rolling_seconds{drill="prod-orders-db",quantile="0.5"} 0.25`,
		`probavi_restore_duration_rolling_seconds{drill="prod-orders-db",quantile="0.95"} 0.9`,
		`probavi_restore_duration_rolling_seconds{drill="prod-orders-db",quantile="1"} 1.2`,
		`probavi_restore_trend_samples{drill="prod-orders-db"} 12`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// An empty trend must leave the file free of trend series entirely.
	if err := WriteTextfile(path, sampleRecord(evidence.OutcomePass), &Trend{}); err != nil {
		t.Fatalf("WriteTextfile empty trend: %v", err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "rolling") || strings.Contains(string(raw), "trend_samples") {
		t.Errorf("empty trend must render nothing:\n%s", raw)
	}
}

func TestWriteTextfilePublishFailure(t *testing.T) {
	// The tempfile write succeeds, but renaming onto an existing directory
	// cannot: the atomic-publish step must surface its own error.
	dir := t.TempDir()
	if err := WriteTextfile(dir, sampleRecord(evidence.OutcomePass), nil); err == nil {
		t.Error("renaming over a directory must fail loudly")
	}
}

// TestWriteTextfileConcurrentWritersDoNotShareATempFile covers what the
// fixed "<path>.tmp" name allowed.
//
// Nothing stops two drills naming one prometheus_textfile: the game-day
// config guards a shared evidence log and says nothing about this. With
// one temporary name they wrote it at the same time and published
// whichever finished last, with the other's bytes possibly still in it.
// Each writer needs a name of its own, and the published file has to be
// one writer's whole output.
func TestWriteTextfileConcurrentWritersDoNotShareATempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probavi.prom")

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rec := sampleRecord(evidence.OutcomePass)
			rec.Drill.Name = fmt.Sprintf("drill-%d", n)
			errs <- WriteTextfile(path, rec, nil)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("WriteTextfile: %v", err)
		}
	}

	// The published file is exactly one writer's output: every metric
	// line carries the same drill label, not a mixture.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	labels := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		_, rest, found := strings.Cut(line, `drill="`)
		if !found {
			continue
		}
		if name, _, closed := strings.Cut(rest, `"`); closed {
			labels[name] = true
		}
	}
	if len(labels) != 1 {
		t.Errorf("published file mixes %d drills (%v) — a writer must publish its own whole output", len(labels), labels)
	}

	// And nothing is left behind for the collector to scrape.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary file %q survived", e.Name())
		}
	}

	// The collector usually runs as somebody else; CreateTemp would have
	// left this 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("published mode = %04o, want 0644", perm)
	}
}
