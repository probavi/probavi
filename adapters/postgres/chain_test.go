package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// chain_test.go builds pgBackRest repositories on disk in the shape one
// really has, measured against pgbackrest 2.59.1 and PostgreSQL 16: a full
// at WAL 02, an incremental at 06–07 and a differential at 09–0A, each
// naming the backup it builds on, with the archive laid out as
// archive/<stanza>/<db>/<first sixteen>/<segment>-<sha1>.gz beside the
// .backup label files pgBackRest writes there too.

// repoSpec describes a repository to write out for one test.
type repoSpec struct {
	backups  []repoBackup
	segments []string // WAL segment names present in the archive
	omitInfo bool     // write no backup.info at all
	omitDirs []string // backup labels listed in the manifest but absent on disk
}

const (
	testFull = "20260923-175627F"
	testIncr = "20260923-175627F_20260923-175629I"
	testDiff = "20260923-175627F_20260923-175659D"
)

// threeBackups is the measured repository: a full, an incremental and a
// differential, the latter two both building on the full.
func threeBackups() []repoBackup {
	return []repoBackup{
		{label: testFull, ArchiveStart: "000000010000000000000002", ArchiveStop: "000000010000000000000002",
			Start: 1790186187, Stop: 1790186188},
		{label: testIncr, Prior: testFull, ArchiveStart: "000000010000000000000006", ArchiveStop: "000000010000000000000007",
			Start: 1790186218, Stop: 1790186219},
		{label: testDiff, Prior: testFull, ArchiveStart: "000000010000000000000009", ArchiveStop: "00000001000000000000000A",
			Start: 1790186230, Stop: 1790186231},
	}
}

// allSegments is the archive of an intact repository of the above.
func allSegments() []string {
	return []string{
		"000000010000000000000001", "000000010000000000000002",
		"000000010000000000000006", "000000010000000000000007",
		"000000010000000000000009", "00000001000000000000000A",
	}
}

// writeChainRepo materialises a repository and returns its path.
func writeChainRepo(t *testing.T, spec repoSpec) string {
	t.Helper()
	dir := t.TempDir()
	const stanza = "demo"

	omitted := make(map[string]bool, len(spec.omitDirs))
	for _, label := range spec.omitDirs {
		omitted[label] = true
	}

	if !spec.omitInfo {
		var b strings.Builder
		b.WriteString("[backrest]\nbackrest-format=5\n\n[backup:current]\n")
		for _, backup := range spec.backups {
			fields := map[string]any{
				"backup-archive-start":   backup.ArchiveStart,
				"backup-archive-stop":    backup.ArchiveStop,
				"backup-timestamp-start": backup.Start,
				"backup-timestamp-stop":  backup.Stop,
			}
			if backup.Prior != "" {
				fields["backup-prior"] = backup.Prior
			}
			payload, err := json.Marshal(fields)
			if err != nil {
				t.Fatalf("marshal %s: %v", backup.label, err)
			}
			fmt.Fprintf(&b, "%s=%s\n", backup.label, payload)
		}
		b.WriteString("\n[db]\ndb-version=\"16\"\n")
		mustWriteFile(t, filepath.Join(dir, "backup", stanza, backupInfoName), b.String())
	}

	for _, backup := range spec.backups {
		if omitted[backup.label] {
			continue
		}
		mustWriteFile(t, filepath.Join(dir, "backup", stanza, backup.label, "backup.manifest"), "manifest\n")
	}

	for _, segment := range spec.segments {
		// The stored name carries a checksum and a compression suffix —
		// the form pgbackrest writes, and the reason the lookup matches on
		// a prefix rather than the whole filename.
		name := segment + "-6152cc38e88b6941e3a36b50c7b98c10d451b31b.gz"
		mustWriteFile(t, filepath.Join(dir, archiveDirName, stanza, "16-1", segment[:walPrefixLen], name), "wal\n")
	}
	if len(spec.segments) > 0 {
		// A .backup label file sits in the same directory and is not a
		// segment; the intact repository has one, so every case proves the
		// lookup does not mistake it for the segment it marks.
		label := "000000010000000000000002.00000028.backup"
		mustWriteFile(t, filepath.Join(dir, archiveDirName, stanza, "16-1", "0000000100000000", label), "label\n")
	}
	return dir
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// withoutSegment returns the archive minus one segment.
func withoutSegment(drop string) []string {
	var out []string
	for _, s := range allSegments() {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// TestCheckRestoreChain covers what a repository can be asked host-side,
// including the cases that must NOT be refused — a pre-check that turns a
// good backup away is worse than none, which is what issue #327 was.
func TestCheckRestoreChain(t *testing.T) {
	cases := []struct {
		name   string
		spec   repoSpec
		target time.Time
		want   string // substring of the refusal; empty means the repository is accepted
	}{
		{
			name: "intact repository restores",
			spec: repoSpec{backups: threeBackups(), segments: allSegments()},
		},
		{
			name: "the newest backup's last segment is missing",
			spec: repoSpec{backups: threeBackups(), segments: withoutSegment("00000001000000000000000A")},
			want: "missing 00000001000000000000000A",
		},
		{
			name: "the newest backup's first segment is missing",
			spec: repoSpec{backups: threeBackups(), segments: withoutSegment("000000010000000000000009")},
			want: "first segment",
		},
		{
			// Measured: deleting the full's own segment left a differential
			// restore working end to end, so requiring the whole chain's WAL
			// would refuse repositories whose older segments have expired.
			name: "an ancestor's own WAL may be expired",
			spec: repoSpec{backups: threeBackups(), segments: withoutSegment("000000010000000000000002")},
		},
		{
			name: "the backup an ancestor chain rests on is gone from disk",
			spec: repoSpec{backups: threeBackups(), segments: allSegments(), omitDirs: []string{testFull}},
			want: "the repository does not hold",
		},
		{
			name: "the manifest no longer lists the ancestor",
			spec: repoSpec{
				backups:  []repoBackup{threeBackups()[2]},
				segments: allSegments(),
			},
			want: "no longer lists",
		},
		{
			name:   "a point in time older than every backup",
			spec:   repoSpec{backups: threeBackups(), segments: allSegments()},
			target: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			want:   "recovery only rolls forward",
		},
		{
			// The incremental finished at 1790186219; a target a second
			// later selects it, and its own WAL is what must be present.
			name:   "a point in time selects an older backup, whose WAL is checked",
			spec:   repoSpec{backups: threeBackups(), segments: withoutSegment("000000010000000000000007")},
			target: time.Unix(1790186220, 0).UTC(),
			want:   "missing 000000010000000000000007",
		},
		{
			name:   "a point in time selects an older backup that is intact",
			spec:   repoSpec{backups: threeBackups(), segments: withoutSegment("00000001000000000000000A")},
			target: time.Unix(1790186220, 0).UTC(),
		},
		{
			name: "an unreadable manifest is not judged",
			spec: repoSpec{backups: threeBackups(), segments: allSegments(), omitInfo: true},
		},
		{
			// An archive this code has not measured the shape of is left
			// alone: silence costs a late failure, a wrong answer costs a
			// good backup.
			name: "an archive with no segments at all is not judged",
			spec: repoSpec{backups: threeBackups()},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perr := checkRestoreChain(writeChainRepo(t, tc.spec), "demo", tc.target)
			if tc.want == "" {
				if perr != nil {
					t.Fatalf("refused a repository that restores: %s: %s", perr.Code, perr.Message)
				}
				return
			}
			if perr == nil {
				t.Fatalf("accepted a repository that cannot restore; want a refusal mentioning %q", tc.want)
			}
			if perr.Code != "source_not_found" {
				t.Errorf("code = %q, want source_not_found — a missing segment is absent, not corrupt",
					perr.Code)
			}
			if !strings.Contains(perr.Message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", perr.Message, tc.want)
			}
		})
	}
}

// TestSelectBackupFollowsRecoveryForward pins the selection rule on its
// own, because every check above rests on choosing the right backup.
func TestSelectBackupFollowsRecoveryForward(t *testing.T) {
	backups := threeBackups()
	cases := []struct {
		name   string
		target time.Time
		want   string
		found  bool
	}{
		{"no target takes the newest", time.Time{}, testDiff, true},
		{"a target after everything takes the newest", time.Unix(1790190000, 0), testDiff, true},
		{"a target between two takes the older", time.Unix(1790186220, 0), testIncr, true},
		{"a target on a backup's stop takes it", time.Unix(1790186188, 0), testFull, true},
		{"a target before everything finds none", time.Unix(1000000000, 0), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, perr := selectBackup(backups, tc.target)
			if ok != tc.found {
				t.Fatalf("found = %v, want %v (err %v)", ok, tc.found, perr)
			}
			if ok && got.label != tc.want {
				t.Errorf("selected %s, want %s", got.label, tc.want)
			}
		})
	}
}

// TestCurrentBackupsIgnoresWhatItDoesNotUnderstand keeps the parser
// indifferent to the many fields pgBackRest writes and unbothered by a
// manifest it cannot read.
func TestCurrentBackupsIgnoresWhatItDoesNotUnderstand(t *testing.T) {
	manifest := `[backrest]
backrest-format=5

[backup:current]
20260923-175627F={"backup-archive-start":"000000010000000000000002","backup-archive-stop":"000000010000000000000002","backup-info-size":12345,"backup-timestamp-start":1790186187,"backup-timestamp-stop":1790186188,"option-online":true}
not json at all
also-missing-an-equals-sign

[backup:history]
20250101-000000F={"backup-timestamp-stop":1700000000}
`
	got := currentBackups(manifest)
	if len(got) != 1 {
		t.Fatalf("parsed %d backups, want 1 — history is not current, and unparseable lines are skipped", len(got))
	}
	if got[0].label != testFull || got[0].ArchiveStop != "000000010000000000000002" {
		t.Errorf("parsed %+v, want the full backup with its archive range", got[0])
	}
}

// TestArchiveHoldsRejectsTheLabelFile proves the lookup does not accept a
// .backup label as the segment it marks: both share the segment's name as
// a prefix, and one of them is not WAL.
func TestArchiveHoldsRejectsTheLabelFile(t *testing.T) {
	dir := t.TempDir()
	const segment = "000000010000000000000002"
	mustWriteFile(t, filepath.Join(dir, "16-1", segment[:walPrefixLen], segment+".00000028.backup"), "label\n")

	ok, err := archiveHolds(dir, segment)
	if err != nil {
		t.Fatalf("archiveHolds: %v", err)
	}
	if ok {
		t.Error("accepted a .backup label file as the WAL segment it marks")
	}

	mustWriteFile(t, filepath.Join(dir, "16-1", segment[:walPrefixLen], segment+"-abc.gz"), "wal\n")
	if ok, err = archiveHolds(dir, segment); err != nil || !ok {
		t.Errorf("archiveHolds = %v (%v), want true once the segment is there", ok, err)
	}
}

// TestValidWALName keeps a manifest field this code cannot look up from
// being looked up anyway.
func TestValidWALName(t *testing.T) {
	cases := map[string]bool{
		"000000010000000000000002":        true,
		"00000001000000000000000a":        true,
		"":                                false,
		"0000000100000000000000":          false,
		"00000001000000000000000G":        false,
		"000000010000000000000002-abc.gz": false,
	}
	for in, want := range cases {
		if got := validWALName(in); got != want {
			t.Errorf("validWALName(%q) = %v, want %v", in, got, want)
		}
	}
}
