// Package metrics writes drill outcomes in the Prometheus textfile
// exposition format — plain text, atomically renamed into place, no
// client library dependency (AGENTS.md: minimal and boring). Trend data
// comes from the evidence log itself: Probavi has no daemon, so rolling
// quantiles are recomputed after each drill from the canonical history.
package metrics

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/probavi/probavi/internal/evidence"
)

// TrendWindow is how many recent restore samples feed the rolling
// quantiles. With daily drills this is roughly a quarter of history —
// enough for a stable P95, short enough that regressions surface.
const TrendWindow = 100

// Trend summarizes restore durations (integer milliseconds, like the
// records they come from) over the most recent TrendWindow drills of one
// drill name that completed a restore.
type Trend struct {
	Samples int
	P50     int64
	P95     int64
	Max     int64
}

// RestoreTrend scans an evidence log and computes the rolling
// restore-duration quantiles for one drill. Metrics are observability,
// not evidence: unparseable lines are skipped and signatures are not
// checked — `probavi evidence verify` is the integrity tool.
func RestoreTrend(logPath, drillName string) (*Trend, error) {
	f, err := os.Open(filepath.Clean(logPath))
	if err != nil {
		return nil, fmt.Errorf("open evidence log: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor

	ring := make([]int64, 0, TrendWindow)
	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), evidence.MaxRecordBytes)
	for sc.Scan() {
		rec := evidence.Record{}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // torn tail or crash artifact; verify reports those
		}
		if rec.Drill.Name != drillName || rec.Timings.Restore == nil {
			continue
		}
		if len(ring) < TrendWindow {
			ring = append(ring, *rec.Timings.Restore)
		} else {
			ring[count%TrendWindow] = *rec.Timings.Restore
		}
		count++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan evidence log: %w", err)
	}
	if len(ring) == 0 {
		return &Trend{}, nil
	}
	sorted := append([]int64(nil), ring...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return &Trend{
		Samples: len(sorted),
		P50:     quantile(sorted, 0.50),
		P95:     quantile(sorted, 0.95),
		Max:     sorted[len(sorted)-1],
	}, nil
}

// quantile is the nearest-rank quantile of an ascending-sorted sample set.
func quantile(sorted []int64, q float64) int64 {
	rank := int(float64(len(sorted))*q + 0.9999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// WriteTextfile renders the record's headline metrics plus the rolling
// trend (nil or empty: omitted) and atomically replaces the file at path
// (node_exporter textfile collector contract: readers must never observe
// a half-written file).
func WriteTextfile(path string, rec *evidence.Record, trend *Trend) error {
	content, err := render(rec, trend)
	if err != nil {
		return err
	}
	path = filepath.Clean(path)

	// A name of its own, created in the destination's own directory so
	// the rename stays inside one filesystem. The fixed "<path>.tmp" this
	// replaces was the same name for every drill writing the same file,
	// and nothing stops two: the game-day config guards a shared evidence
	// log and says nothing about a shared textfile. Two drills then wrote
	// one temporary file at once and published whichever finished last,
	// with the other's bytes possibly still in it.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create metrics tempfile: %w", err)
	}
	name := tmp.Name()
	// Every failure from here removes the temporary file. A leftover is
	// not harmless: the collector scrapes a directory, and a name ending
	// in .tmp that it decides to read is a metric nobody wrote on purpose.
	if _, err := tmp.WriteString(content); err != nil {
		return errors.Join(fmt.Errorf("write metrics tempfile: %w", err), tmp.Close(), os.Remove(name))
	}
	// CreateTemp makes the file 0600; the collector usually runs as
	// somebody else. The mode is set before the rename so the file is
	// never briefly readable at the published name and not by the reader.
	if err := tmp.Chmod(0o644); err != nil {
		return errors.Join(fmt.Errorf("set metrics file mode: %w", err), tmp.Close(), os.Remove(name))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("close metrics tempfile: %w", err), os.Remove(name))
	}
	// No fsync, and that is a decision rather than an omission. The
	// rename is what the collector's contract needs — a reader never sees
	// a half-written file — and this file is an observability artifact
	// that the next drill rewrites, not a record anybody has to be able
	// to reconstruct. Losing it to a crash costs one scrape of staleness.
	// The evidence log, which cannot be rewritten, does fsync.
	if err := os.Rename(name, path); err != nil {
		return errors.Join(fmt.Errorf("publish metrics file: %w", err), os.Remove(name))
	}
	return nil
}

func render(rec *evidence.Record, trend *Trend) (string, error) {
	ts, err := time.Parse(evidence.TimestampFormat, rec.TS)
	if err != nil {
		return "", fmt.Errorf("record ts: %w", err)
	}
	label := `drill="` + escapeLabel(rec.Drill.Name) + `"`
	passed := 0
	for _, c := range rec.Checks {
		if c.OK {
			passed++
		}
	}

	b := &strings.Builder{}
	if rec.Timings.Restore != nil {
		writeMetric(b, "probavi_restore_duration_seconds",
			"Duration of the engine restore phase of the last drill.",
			label, formatFloat(float64(*rec.Timings.Restore)/1000))
	}
	writeMetric(b, "probavi_checks_passed",
		"Number of checks that passed in the last drill.",
		label, strconv.Itoa(passed))
	writeMetric(b, "probavi_checks_total",
		"Number of checks executed in the last drill.",
		label, strconv.Itoa(len(rec.Checks)))
	writeMetric(b, "probavi_last_run_timestamp_seconds",
		"Unix time of the last drill's evidence record.",
		label, formatFloat(float64(ts.UnixMilli())/1000))
	if rec.Outcome == evidence.OutcomePass {
		writeMetric(b, "probavi_last_success_timestamp_seconds",
			"Unix time of the last drill that proved the backup restorable.",
			label, formatFloat(float64(ts.UnixMilli())/1000))
	}
	renderTrend(b, label, trend)
	return b.String(), nil
}

// renderTrend emits the alert-friendly rolling series: quantiles as one
// metric with a quantile label (alert on {quantile="0.95"}), plus the
// sample count so dashboards can tell a stable P95 from a two-sample one.
func renderTrend(b *strings.Builder, label string, trend *Trend) {
	if trend == nil || trend.Samples == 0 {
		return
	}
	const name = "probavi_restore_duration_rolling_seconds"
	fmt.Fprintf(b, "# HELP %s Rolling restore-duration quantiles over the most recent drills (window %d).\n# TYPE %s gauge\n",
		name, TrendWindow, name)
	for _, s := range []struct {
		q  string
		ms int64
	}{{"0.5", trend.P50}, {"0.95", trend.P95}, {"1", trend.Max}} {
		fmt.Fprintf(b, "%s{%s,quantile=%q} %s\n", name, label, s.q, formatFloat(float64(s.ms)/1000))
	}
	writeMetric(b, "probavi_restore_trend_samples",
		"Number of restore-duration samples inside the rolling window.",
		label, strconv.Itoa(trend.Samples))
}

func writeMetric(b *strings.Builder, name, help, label, value string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s{%s} %s\n", name, help, name, name, label, value)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// escapeLabel applies the exposition-format label escaping rules.
func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}
