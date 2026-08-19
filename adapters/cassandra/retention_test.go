package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptRunBudget bounds a stub run: the probe script is a handful of
// shell commands, so anything slower is a hang, not slow hardware.
const scriptRunBudget = 30 * time.Second

// metadataStub stands in for sstablemetadata. Its output is the real
// tool's, measured on both verified images — the surrounding lines are
// kept because they are what the parse has to step over, `TTL min` above
// all: it carries the same number and must not be read as the answer.
const metadataStub = `case "$1" in
  *expiring*) cat <<'OUT'
SSTable: /snapshot/shop/sessions/nb-1-big
Minimum timestamp: 1787158236368359 (08/19/2026 16:50:36)
SSTable max local deletion time: 1787158296 (08/19/2026 16:51:36)
TTL min: 60 (1 minute)
TTL max: 60 (1 minute)
Estimated droppable tombstones: 2.0
OUT
  ;;
  *plain*) cat <<'OUT'
SSTable: /snapshot/shop/orders/nb-1-big
SSTable max local deletion time: 2147483647 (no tombstones)
TTL min: 0
TTL max: 0
Estimated droppable tombstones: 0.0
OUT
  ;;
  *) echo "no such sstable" >&2; exit 1 ;;
esac`

// runTTLProbe runs the real script against a table directory holding the
// named sstables, with sstablemetadata stubbed unless withTool is false.
func runTTLProbe(t *testing.T, sstables []string, withTool bool) (stdout string, exit int) {
	t.Helper()
	binDir := t.TempDir()
	// The script's own tools come from the host so it runs for real;
	// anything not linked is genuinely absent.
	for _, name := range []string{"sed", "head", "cat"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s on this host: %v", name, err)
		}
		if err := os.Symlink(real, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
	}
	if withTool {
		if err := os.WriteFile(filepath.Join(binDir, "sstablemetadata"),
			[]byte("#!/bin/sh\n"+metadataStub+"\n"), 0o700); err != nil {
			t.Fatalf("write stub: %v", err)
		}
	}
	tableDir := t.TempDir()
	for _, name := range sstables {
		if err := os.WriteFile(filepath.Join(tableDir, name), []byte("sstable"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), scriptRunBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", ttlProbeScript, "bash", tableDir)
	cmd.Env = []string{"PATH=" + binDir}
	out, err := cmd.Output()
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatalf("run probe script: %v", err)
		}
		exit = exitErr.ExitCode()
	}
	return strings.TrimSpace(string(out)), exit
}

// TestTTLProbeReadsTheArtifactsOwnClaim covers what the fence rests on:
// the largest time-to-live any of a table's sstables declares.
func TestTTLProbeReadsTheArtifactsOwnClaim(t *testing.T) {
	tests := []struct {
		name     string
		sstables []string
		withTool bool
		want     string
	}{
		{"a table whose rows expire", []string{"nb-1-expiring-Data.db"}, true, "60"},
		{"a table whose rows do not", []string{"nb-1-plain-Data.db"}, true, "0"},
		{
			// The snapshot of a table that gained a TTL partway through
			// its life holds both, and one expiring sstable is enough.
			name:     "the largest claim across several sstables wins",
			sstables: []string{"nb-1-plain-Data.db", "nb-2-expiring-Data.db"},
			withTool: true,
			want:     "60",
		},
		{
			// The other components a snapshot carries must not be handed
			// to the tool at all.
			name:     "only Data files are asked",
			sstables: []string{"nb-1-plain-Data.db", "nb-1-plain-TOC.txt", "manifest.json"},
			withTool: true,
			want:     "0",
		},
		{"a table with no sstable at all", nil, true, "0"},
		{"an image without the tool makes no accusation", []string{"nb-1-expiring-Data.db"}, false, "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, exit := runTTLProbe(t, tc.sstables, tc.withTool)
			if exit != 0 {
				t.Errorf("exit = %d, want 0", exit)
			}
			if got != tc.want {
				t.Errorf("declared ttl = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProvisionRefusesATableItCannotRead is the fence itself: a restored
// table that reads nothing, while its own artifact says it held rows that
// expire, fails the drill instead of passing as an empty green.
func TestProvisionRefusesATableItCannotRead(t *testing.T) {
	dir := writeTree(t, t.TempDir(), "shop.orders")
	sim := defaultSimulated()
	sim.probe = outExec(emptyRead)
	sim.ttl = outExec("60\n")

	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "cassandra_snapshot", dir, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" {
		t.Fatalf("final = %+v, want restore_failed for a table the drill cannot read", f)
	}
	for _, want := range []string{"shop.orders", "60 seconds", "backup is intact", "younger"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
		}
	}
	if !strings.Contains(strings.Join(sequence, "|"), "probe|ttl") {
		t.Errorf("sequence = %v, want the artifact asked only after the read came back empty", sequence)
	}
}

// TestProvisionAcceptsAnEmptyTableThatNeverExpired keeps the fence from
// becoming a nuisance. Measured: a table nobody ever wrote contributes no
// sstable to a snapshot, and one whose every row was deleted contributes
// tombstones with `TTL max: 0` — both answer zero, both are legitimate,
// and neither may fail a drill.
func TestProvisionAcceptsAnEmptyTableThatNeverExpired(t *testing.T) {
	dir := writeTree(t, t.TempDir(), "shop.orders")
	sim := defaultSimulated()
	sim.probe = outExec(emptyRead)
	sim.ttl = outExec("0\n")

	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "cassandra_snapshot", dir, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v, want an empty table with no expiry to pass", exit, f)
	}
}

// TestProvisionDoesNotAskAboutATableItCouldRead keeps the common path
// free: a table that answers with a row says everything the fence needs.
func TestProvisionDoesNotAskAboutATableItCouldRead(t *testing.T) {
	dir := writeTree(t, t.TempDir(), "shop.orders")
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "cassandra_snapshot", dir, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	for _, label := range sequence {
		if label == "ttl" {
			t.Fatalf("sequence = %v, want no metadata call for a table that read a row", sequence)
		}
	}
}
