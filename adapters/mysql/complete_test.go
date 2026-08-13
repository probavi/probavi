package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The three artifact shapes the completeness rule has to tell apart,
// reduced to the lines it reads. The pairing they encode was measured
// against mysqldump 8.4: the banner and the sign-off are written together
// or not at all.
const (
	announcedDump = "-- MySQL dump 10.13  Distrib 8.4.11, for Linux (x86_64)\n" +
		"INSERT INTO `orders` VALUES (1);\n" +
		"-- Dump completed on 2026-08-09 21:08:17\n"
	// unfinishedDump is what a backup job leaves behind when its mysqldump
	// dies: the banner promising an ending that never came.
	unfinishedDump = "-- MySQL dump 10.13  Distrib 8.4.11, for Linux (x86_64)\n" +
		"INSERT INTO `orders` VALUES (1);\n"
	// compactDump is --compact or --skip-comments output: no banner, no
	// sign-off, and so nothing to hold it to.
	compactDump = "INSERT INTO `orders` VALUES (1);\n"
)

func writePlainDump(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestCompletenessMarker pins which artifacts are held to an ending, and
// proves the answer does not depend on how the artifact is stored: the head
// is read through the decompressor, so a compressed dump is judged by the
// same line as a plain one.
func TestCompletenessMarker(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"a dump that announces itself", announcedDump, dumpCompleteMarker},
		{"a dump whose ending never came", unfinishedDump, dumpCompleteMarker},
		{"a comment-free dump", compactDump, ""},
		{"an empty file", "", ""},
		// A backup job that prepends a note of its own must not turn a
		// checkable dump into an uncheckable one.
		{"a dump behind someone else's header",
			"-- taken by backup-cron\n" + announcedDump, dumpCompleteMarker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if got := completenessMarker(writePlainDump(t, dir, "stored.sql", tt.body)); got != tt.want {
				t.Errorf("stored plain: marker = %q, want %q", got, tt.want)
			}
			if got := completenessMarker(writeGzipDump(t, dir, "stored.sql.gz", tt.body)); got != tt.want {
				t.Errorf("stored compressed: marker = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCompletenessMarkerOfUnreadableFile: deciding whether a dump can be
// checked is not the place to fail a drill. The restore opens the same file
// next and reports what is wrong with it in the client's own words.
func TestCompletenessMarkerOfUnreadableFile(t *testing.T) {
	if got := completenessMarker(filepath.Join(t.TempDir(), "gone.sql")); got != "" {
		t.Errorf("marker = %q, want none", got)
	}
}

// TestReadDumpHeadIsBounded proves the head read costs the same whatever
// the artifact's size, which is what keeps it affordable on a compressed
// dump that would otherwise have to be inflated whole.
func TestReadDumpHeadIsBounded(t *testing.T) {
	body := announcedDump + strings.Repeat("INSERT INTO `orders` VALUES (1);\n", 20000)
	head, ok := readDumpHead(writeGzipDump(t, t.TempDir(), "big.sql.gz", body))
	if !ok {
		t.Fatal("readDumpHead refused a valid member")
	}
	if len(head) != dumpHeadBytes {
		t.Errorf("head = %d bytes, want the bounded %d", len(head), dumpHeadBytes)
	}
	if !announcesItself(head) {
		t.Error("the banner did not survive the bounded read")
	}
}

// TestRestoreScriptProvesTheDumpWasWhole covers the stored-plain path,
// where the client's exit code cannot tell a complete dump from one that
// stops on a statement boundary.
func TestRestoreScriptProvesTheDumpWasWhole(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		marker   string
		mysql    string
		wantExit int
	}{
		{"a whole dump the client was content with",
			announcedDump, dumpCompleteMarker, drainsThenSucceeds, 0},
		{"a dump that was never finished",
			unfinishedDump, dumpCompleteMarker, drainsThenSucceeds, incompleteDumpExit},
		{"a comment-free dump is not held to an ending",
			compactDump, "", drainsThenSucceeds, 0},
		{"the client's own verdict comes first",
			unfinishedDump, dumpCompleteMarker, abortsWithoutReading, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := stubPath(t, map[string]string{"mysql": tt.mysql})
			dump := writePlainDump(t, t.TempDir(), "probavi-restore.sql", tt.body)
			got := runLoadScript(t, restoreScript, binDir, dump, "root", "orders",
				tt.marker, strconv.Itoa(markerTailBytes))
			if got != tt.wantExit {
				t.Errorf("exit = %d, want %d", got, tt.wantExit)
			}
		})
	}
}
