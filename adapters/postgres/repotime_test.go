package main

import (
	"os"
	"path/filepath"
	"testing"
)

// realBackupInfo is a pgBackRest 2.59 manifest, taken from a repository
// built on a host in Asia/Tokyo. The two timestamps are epoch seconds:
// 1786289869 is 2026-08-09 15:37:49 UTC, which pgbackrest's own info
// output renders as 00:37:49+09 — the same instant, no zone question.
const realBackupInfo = `[backrest]
backrest-format=5
backrest-version="2.59.0"

[backup:current]
20260810-003749F={"backrest-format":5,"backrest-version":"2.59.0","backup-archive-start":"000000010000000000000002","backup-archive-stop":"000000010000000000000002","backup-error":false,"backup-info-repo-size":3063894,"backup-info-size":23224206,"backup-timestamp-start":1786289869,"backup-timestamp-stop":1786289873,"backup-type":"full","db-id":1,"option-archive-check":true,"option-archive-copy":false,"option-backup-standby":false,"option-checksum-page":true,"option-compress":true,"option-hardlink":false,"option-online":true}

[db]
db-catalog-version=202307071
db-id=1
db-system-id=7536000000000000000
db-version="16"
`

const fixtureStanza = "demo"

// writeRepo lays out a repository the way pgBackRest does:
// <repo>/backup/<stanza>/backup.info.
func writeRepo(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest == "" {
		return dir
	}
	path := filepath.Join(dir, "backup", fixtureStanza)
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, backupInfoName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRepoCreatedAt(t *testing.T) {
	t.Run("a real repository dates itself, with no zone declared", func(t *testing.T) {
		got := repoCreatedAt(writeRepo(t, realBackupInfo), fixtureStanza)
		if got == nil {
			t.Fatal("createdAt = nil, want the repository's own completion time")
		}
		// Epoch seconds are an instant already: this is the one kind that
		// needs no source.params.backup_timezone.
		if *got != "2026-08-09T15:37:53.000Z" {
			t.Errorf("createdAt = %s, want the newest backup's stop instant in UTC", *got)
		}
	})
	t.Run("the newest backup dates the repository", func(t *testing.T) {
		manifest := "[backup:current]\n" +
			`20260808-000000F={"backup-timestamp-start":1786000000,"backup-timestamp-stop":1786000010}` + "\n" +
			`20260810-003749F={"backup-timestamp-start":1786289869,"backup-timestamp-stop":1786289873}` + "\n" +
			`20260809-000000F={"backup-timestamp-start":1786100000,"backup-timestamp-stop":1786100010}` + "\n"
		got := repoCreatedAt(writeRepo(t, manifest), fixtureStanza)
		if got == nil || *got != "2026-08-09T15:37:53.000Z" {
			t.Errorf("createdAt = %v, want the latest backup regardless of line order", got)
		}
	})
	t.Run("a backup with only a start time still dates it", func(t *testing.T) {
		manifest := "[backup:current]\n" + `20260810-003749F={"backup-timestamp-start":1786289869}` + "\n"
		got := repoCreatedAt(writeRepo(t, manifest), fixtureStanza)
		if got == nil || *got != "2026-08-09T15:37:49.000Z" {
			t.Errorf("createdAt = %v, want the start instant as a fallback", got)
		}
	})
}

func TestRepoCreatedAtRefusals(t *testing.T) {
	tests := []struct {
		name     string
		stanza   string
		manifest string
	}{
		{"no repository at all", "demo", ""},
		{"a manifest with no current backups", "demo", "[backrest]\nbackrest-format=5\n"},
		// Expired backups leave the section empty; the repository holds
		// history but nothing a restore can use.
		{"an empty current section", "demo", "[backup:current]\n\n[db]\ndb-id=1\n"},
		// A repository written with repo1-cipher-type has an unreadable
		// manifest, and guessing is not an option.
		{"an encrypted manifest", "demo", "[backup:current]\nnot-json-at-all\n"},
		{"timestamps that are not numbers", "demo",
			"[backup:current]\n" + `20260810F={"backup-timestamp-stop":"yesterday"}` + "\n"},
		{"an absurd epoch", "demo",
			"[backup:current]\n" + `20260810F={"backup-timestamp-stop":12}` + "\n"},
		{"the wrong stanza", "other", realBackupInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRepo(t, tt.manifest)
			if got := repoCreatedAt(dir, tt.stanza); got != nil {
				t.Errorf("createdAt = %s, want none", *got)
			}
		})
	}
}

// TestRepoIgnoresTimestampsOutsideTheCurrentSection pins the section
// boundary: [db:history] and other sections carry their own numbers, and
// reading one of those would date the repository by something that is not
// a backup.
func TestRepoIgnoresTimestampsOutsideTheCurrentSection(t *testing.T) {
	manifest := "[backup:current]\n" +
		`20260810F={"backup-timestamp-stop":1786289873}` + "\n\n" +
		"[db:history]\n" +
		`1={"backup-timestamp-stop":4102444800}` + "\n"
	got := repoCreatedAt(writeRepo(t, manifest), fixtureStanza)
	if got == nil || *got != "2026-08-09T15:37:53.000Z" {
		t.Errorf("createdAt = %v, want only the current section to count", got)
	}
}
