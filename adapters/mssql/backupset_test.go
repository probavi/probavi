package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rows below are the real shape captured from SQL Server 2022:
// BackupName and BackupDescription are NULL, BackupType is the third
// column, Position the sixth, and dozens of columns follow.

func TestParseBackupSets(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		want    []backupSet
		wantErr bool
	}{
		{"a full backup",
			headerRow(backupTypeFull, 1) + "\n",
			[]backupSet{{position: 1, backupType: backupTypeFull}}, false},
		{"a transaction log backup",
			headerRow(backupTypeLog, 1) + "\n",
			[]backupSet{{position: 1, backupType: backupTypeLog}}, false},
		{"a differential backup",
			headerRow(backupTypeDifferential, 1) + "\n",
			[]backupSet{{position: 1, backupType: backupTypeDifferential}}, false},
		{"appended sets keep their positions",
			headerRow(backupTypeFull, 1) + "\n" + headerRow(backupTypeLog, 2) + "\n" + headerRow(backupTypeFull, 3) + "\n",
			[]backupSet{
				{position: 1, backupType: backupTypeFull},
				{position: 2, backupType: backupTypeLog},
				{position: 3, backupType: backupTypeFull},
			}, false},
		{"blank lines are not rows",
			"\n" + headerRow(backupTypeFull, 1) + "\n\n",
			[]backupSet{{position: 1, backupType: backupTypeFull}}, false},
		{"short lines are noise, not rows", "a|b|c\n" + headerRow(backupTypeFull, 1),
			[]backupSet{{position: 1, backupType: backupTypeFull}}, false},
		// The engine answering without classifying anything is not a
		// verdict: a real server exits non-zero for media it refuses, and
		// the protocol's simulated sandbox answers every exec with a fixed
		// stdout (§10), so this must not fail a drill.
		{"no rows at all", "", nil, false},
		{"the simulated sandbox's fixed answer", "1\n", nil, false},
		// A backup name containing the separator shifts every later column.
		// Reading the wrong one could restore a log backup believing it is
		// a full one, so the ambiguity is refused rather than guessed at.
		{"a backup name containing the separator",
			"my|backup|NULL|1|NULL|0|1|2|sa\n", nil, true},
		{"an unknown backup type",
			"NULL|NULL|99|NULL|0|1|2|sa|host|shop|957\n", nil, true},
		{"a non-numeric position",
			"NULL|NULL|1|NULL|0|x|2|sa|host|shop|957\n", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, perr := parseBackupSets([]byte(tt.stdout))
			if tt.wantErr {
				assertRefused(t, got, perr)
				return
			}
			if perr != nil {
				t.Fatalf("parseBackupSets: %+v", perr)
			}
			assertSets(t, got, tt.want)
		})
	}
}

func assertRefused(t *testing.T, got []backupSet, perr *protoError) {
	t.Helper()
	if perr == nil {
		t.Fatalf("parseBackupSets = %+v, want a refusal", got)
	}
	if perr.Code != "source_corrupt" {
		t.Errorf("code = %s, want source_corrupt", perr.Code)
	}
}

func assertSets(t *testing.T, got, want []backupSet) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("sets = %+v, want %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("set %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestNewestFullPosition pins the rule measured on a real server:
// restoring without naming a set takes the first one on the media, which
// in an appended file is the oldest backup on it.
func TestNewestFullPosition(t *testing.T) {
	tests := []struct {
		name     string
		sets     []backupSet
		want     int
		wantHave bool
	}{
		{"one full", []backupSet{{1, backupTypeFull, ""}}, 1, true},
		{"full then log", []backupSet{{1, backupTypeFull, ""}, {2, backupTypeLog, ""}}, 1, true},
		{"the newest full wins",
			[]backupSet{{1, backupTypeFull, ""}, {2, backupTypeLog, ""}, {3, backupTypeFull, ""}}, 3, true},
		{"positions out of order",
			[]backupSet{{3, backupTypeFull, ""}, {1, backupTypeFull, ""}}, 3, true},
		{"only a log", []backupSet{{1, backupTypeLog, ""}}, 0, false},
		{"only a differential", []backupSet{{1, backupTypeDifferential, ""}}, 0, false},
		{"log and differential", []backupSet{{1, backupTypeDifferential, ""}, {2, backupTypeLog, ""}}, 0, false},
		{"a partial backup is not a full", []backupSet{{1, backupTypePartial, ""}}, 0, false},
		{"a file backup is not a full", []backupSet{{1, backupTypeFile, ""}}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, have := newestFullPosition(tt.sets)
			if got != tt.want || have != tt.wantHave {
				t.Errorf("newestFullPosition = (%d, %v), want (%d, %v)", got, have, tt.want, tt.wantHave)
			}
		})
	}
}

func TestDescribeSets(t *testing.T) {
	tests := []struct {
		name string
		sets []backupSet
		want string
	}{
		{"one log", []backupSet{{1, backupTypeLog, ""}}, "transaction log backup"},
		{"one differential", []backupSet{{1, backupTypeDifferential, ""}}, "differential backup"},
		{"several logs", []backupSet{{1, backupTypeLog, ""}, {2, backupTypeLog, ""}}, "2 transaction log backups"},
		{"mixed", []backupSet{{1, backupTypeDifferential, ""}, {2, backupTypeLog, ""}},
			"differential backup, transaction log backup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeSets(tt.sets); got != tt.want {
				t.Errorf("describeSets = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLooksLikeBackupMedia covers the host-side filter. Both magics were
// measured on a real server: TAPE for an ordinary backup, MSSQ for a
// compressed one — a single-magic check would skip every compressed
// backup in a directory.
func TestLooksLikeBackupMedia(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"an ordinary backup", write("full.bak", "TAPE\x00\x00rest"), true},
		{"a compressed backup", write("comp.bak", "MSSQ\x00\x00rest"), true},
		{"a checksum sidecar", write("SHA256SUMS", "abc123  full.bak\n"), false},
		{"a log file", write("backup.log", "2026-08-09 backup completed\n"), false},
		{"a file shorter than the magic", write("tiny", "TA"), false},
		{"an empty file", write("empty", ""), false},
		{"a missing file", filepath.Join(dir, "gone"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeBackupMedia(tt.path); got != tt.want {
				t.Errorf("looksLikeBackupMedia(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestNoFullBackupMessage(t *testing.T) {
	plan := &sourcePlan{dir: "/backups", skipped: []string{"SHA256SUMS"}}
	perr := noFullBackup(plan, []string{"log2.trn: transaction log backup", "diff.bak: differential backup"})
	if perr.Code != "source_not_found" {
		t.Errorf("code = %s, want source_not_found", perr.Code)
	}
	for _, want := range []string{"/backups", "log2.trn", "differential backup", "SHA256SUMS", "not backup media"} {
		if !strings.Contains(perr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", perr.Message, want)
		}
	}
	if strings.Contains(perr.Message, `"`) {
		t.Errorf("message %q must stay quote-free for protocol embedding", perr.Message)
	}
}

func TestNameList(t *testing.T) {
	if got := nameList([]string{"a", "b"}, 5); got != "a; b" {
		t.Errorf("nameList = %q", got)
	}
	if got := nameList([]string{"a", "b", "c"}, 2); got != "a; b and 1 more" {
		t.Errorf("nameList = %q, want the capped form", got)
	}
}
