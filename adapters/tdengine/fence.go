package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// fence.go refuses a backup the engine would empty rather than restore,
// and reads what the artifact says it holds so the restore can be judged
// against it.
//
// This is the engine's instance of the data-lifecycle rule: a drill
// sandbox proves the artifact, so it must not apply the engine's own
// retention to it. TDengine's retention is KEEP, a per-database number of
// days, and it is enforced on write: a row whose timestamp falls outside
// the window is refused with "Timestamp data out of range" (measured,
// error 1547, on a database created KEEP 3d with a row six days old).
//
// There is nothing to suspend. KEEP travels inside the backup — taosdump
// writes the operator's own CREATE DATABASE line, retention included — so
// the restore recreates the database with the policy the operator
// declared, and widening it would be rewriting what a check is entitled
// to read. taosdump offers no switch for it either; its --loose-mode is
// about escaping names (measured).
//
// What is left is a fence, and it is exact rather than a guess: a dump
// records when it was taken, so a backup older than its own KEEP holds
// nothing the engine will accept — every row in it is outside the window
// before the restore starts. That drill cannot pass, and the honest thing
// is to say so with both numbers rather than to restore an empty database
// and call it green.

// rejectAgedBackup refuses a backup whose data has aged past the
// retention it declares.
func rejectAgedBackup(src *resolvedSource, now time.Time) *protoError {
	if src.keepDays <= 0 || src.started.IsZero() {
		return nil
	}
	age := now.Sub(src.started)
	keep := time.Duration(src.keepDays) * 24 * time.Hour
	if age <= keep {
		return nil
	}
	return protoErr("restore_failed", false,
		"the backup was taken %s ago and the database it holds declares KEEP %dd, so every row in it "+
			"falls outside the retention window the restore would recreate: TDengine refuses such a row "+
			"with \"Timestamp data out of range\", and the restore would leave an empty database. Widen "+
			"KEEP on the source database, or drill a backup younger than it",
		age.Round(time.Hour), src.keepDays)
}

// dumpedRows matches the row count taosdump records about its own output.
var dumpedRows = regexp.MustCompile(`(?m)^# total row count: +(\d+)`)

// artifactRows reads how many rows the artifact says it holds, and
// whether it said. It is the number the restore has to reach: taosdump's
// exit code is 0 whatever happened, so the dump's own accounting is what
// tells a whole restore from a partial one.
func artifactRows(resultText []byte) (int, bool) {
	m := dumpedRows.FindSubmatch(resultText)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, false
	}
	return n, true
}

var (
	// restoredRows matches the tool's summary of what it put back.
	restoredRows = regexp.MustCompile(`OK: (\d+) row\(s\) dumped in!`)
	// restoreFailures matches the line it prints beside that summary when
	// some of the work did not succeed — on the same run, with exit 0.
	restoreFailures = regexp.MustCompile(`ERROR: (\d+) failures? occurred`)
	// notConnected is what it prints when the server would not have it.
	notConnected = regexp.MustCompile(`Retry to connect`)
)

// restoreOutcome is what taosdump's own output says happened.
type restoreOutcome struct {
	rows      int
	rowsKnown bool
	failures  int
	refused   bool // the tool never reached the server
	// spoke reports that the output is the tool's at all. Every real run
	// prints its "end time:" line, the failed ones included (measured), so
	// output carrying none of the tool's markers is not a restore this
	// function can judge — the simulated sandbox of the conformance suite
	// answers every command with a stand-in — and the engine-facing gate
	// after it decides on positive evidence instead.
	spoke bool
}

// toolMarkers are the lines taosdump writes about its own run.
var toolMarkers = []string{"end time:", "INFO:", "OK:", "ERROR:", "Retry to connect"}

// readRestoreOutcome parses the tool's output.
func readRestoreOutcome(out []byte) restoreOutcome {
	o := restoreOutcome{refused: notConnected.Match(out)}
	for _, marker := range toolMarkers {
		if bytes.Contains(out, []byte(marker)) {
			o.spoke = true
			break
		}
	}
	if m := restoredRows.FindSubmatch(out); m != nil {
		if n, err := strconv.Atoi(string(m[1])); err == nil {
			o.rows, o.rowsKnown = n, true
		}
	}
	if m := restoreFailures.FindSubmatch(out); m != nil {
		if n, err := strconv.Atoi(string(m[1])); err == nil {
			o.failures = n
		}
	}
	return o
}

// verdict turns what the tool said, and what the artifact claimed, into a
// drill verdict. Nothing here trusts the exit code, because it is 0 in
// every case this function exists to separate.
func (o restoreOutcome) verdict(expected int, expectedKnown bool) *protoError {
	switch {
	case !o.spoke:
		return nil
	case o.refused && !o.rowsKnown:
		return protoErr("restore_failed", false,
			"taosdump never reached the server: it retried the connection and stopped, having restored "+
				"nothing, and still exited 0")
	case o.failures > 0:
		return protoErr("source_corrupt", false,
			"taosdump reported %d failure(s) while restoring%s — a file in the backup could not be read, "+
				"and the tool exits 0 either way", o.failures, restoredSuffix(o))
	case !o.rowsKnown:
		return protoErr("restore_failed", false,
			"taosdump said nothing about restoring any rows, and exited 0: the backup directory it was "+
				"given holds no data to restore")
	case expectedKnown && o.rows != expected:
		return protoErr("source_corrupt", false,
			"the backup records %d row(s) and the restore put back %d: the artifact and the engine "+
				"disagree about what it holds", expected, o.rows)
	}
	return nil
}

func restoredSuffix(o restoreOutcome) string {
	if !o.rowsKnown {
		return ""
	}
	return fmt.Sprintf(" (%d row(s) did arrive)", o.rows)
}
