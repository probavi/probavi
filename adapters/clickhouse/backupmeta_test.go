package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadBackupWallClock(t *testing.T) {
	dir := t.TempDir()

	t.Run("the manifest header dates the backup", func(t *testing.T) {
		path := filepath.Join(dir, "ok.zip")
		writeArchive(t, path, "2026-08-14 14:37:45")
		got, err := readBackupWallClock(path)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := time.Date(2026, 8, 14, 14, 37, 45, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("a manifest without a timestamp is recognised, not guessed", func(t *testing.T) {
		path := filepath.Join(dir, "no-ts.zip")
		writeArchive(t, path, "")
		_, err := readBackupWallClock(path)
		if !errors.Is(err, errNoTimestamp) {
			t.Errorf("err = %v, want errNoTimestamp", err)
		}
	})

	t.Run("an archive that is not a ClickHouse backup", func(t *testing.T) {
		path := filepath.Join(dir, "other.zip")
		writeZipMembers(t, path, "readme.txt", "not a backup")
		_, err := readBackupWallClock(path)
		if !errors.Is(err, errNoManifest) {
			t.Errorf("err = %v, want errNoManifest", err)
		}
	})

	t.Run("a file that is not an archive at all", func(t *testing.T) {
		path := filepath.Join(dir, "plain.txt")
		if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readBackupWallClock(path); err == nil {
			t.Error("err = nil, want a zip error")
		}
	})

	t.Run("an unparseable timestamp is not a creation time", func(t *testing.T) {
		path := filepath.Join(dir, "bad-ts.zip")
		writeZipMembers(t, path, backupManifest,
			"<config><version>1</version><timestamp>yesterday</timestamp></config>")
		_, err := readBackupWallClock(path)
		if !errors.Is(err, errNoTimestamp) {
			t.Errorf("err = %v, want errNoTimestamp", err)
		}
	})
}

// TestManifestScanStopsAtTheHeader proves the reader does not decode the
// whole manifest: a backup of a large table lists one element per part
// file, and that list can reach megabytes while the field being read is in
// the first few hundred bytes. The trailing garbage here would fail any
// full parse.
func TestManifestScanStopsAtTheHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.zip")
	manifest := "<config><version>1</version><timestamp>2026-08-14 14:37:45</timestamp>" +
		strings.Repeat("<file><name>x</name>", 5000) + "<<< not valid xml past this point"
	writeZipMembers(t, path, backupManifest, manifest)

	got, err := readBackupWallClock(path)
	if err != nil {
		t.Fatalf("err = %v — the reader parsed past the header it needed", err)
	}
	if got.Minute() != 37 {
		t.Errorf("got %v", got)
	}
}

// writeZipMembers writes an archive with the given name/content pairs,
// with no assumptions about what a ClickHouse backup looks like.
func writeZipMembers(t *testing.T, path string, members ...string) {
	t.Helper()
	writeArchiveMembers(t, path, members)
}
