package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDirectoryRanksByTheBackupsOwnTime is the property that separates a
// real answer from a plausible one: file times date the copy, so a backup
// restored from a directory must be chosen by what the archive says about
// itself. Here the newest file is the oldest backup, and the adapter has
// to prefer the backup rather than the file.
func TestDirectoryRanksByTheBackupsOwnTime(t *testing.T) {
	dir := t.TempDir()

	older := filepath.Join(dir, "z-copied-last.zip")
	writeArchive(t, older, "2026-08-10 02:00:00")
	backdate(t, older, 0) // copied just now

	newer := filepath.Join(dir, "a-copied-first.zip")
	writeArchive(t, newer, "2026-08-14 02:00:00")
	backdate(t, newer, 48*time.Hour) // copied two days ago

	src, perr := resolveSource(context.Background(), "clickhouse_backup_dir", dir)
	if perr != nil {
		t.Fatalf("perr = %+v", perr)
	}
	if src.path != newer {
		t.Errorf("chose %s, want %s — ranking followed mtime rather than the manifest timestamp",
			filepath.Base(src.path), filepath.Base(newer))
	}
	if !src.wallClock.Equal(time.Date(2026, 8, 14, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("wallClock = %v", src.wallClock)
	}
}

func TestDirectoryTieBreaksDeterministically(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.zip", "z.zip", "m.zip"} {
		writeArchive(t, filepath.Join(dir, name), "2026-08-14 02:00:00")
	}
	for range 3 {
		src, perr := resolveSource(context.Background(), "clickhouse_backup_dir", dir)
		if perr != nil {
			t.Fatalf("perr = %+v", perr)
		}
		if filepath.Base(src.path) != "z.zip" {
			t.Fatalf("chose %s, want z.zip on every run", filepath.Base(src.path))
		}
	}
}

func TestResolveSourceRefusals(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "shop.zip")
	writeArchive(t, archive, "2026-08-14 02:00:00")

	tests := []struct {
		name, kind, path, want string
	}{
		{"unknown kind", "clickhouse_dump", archive, "unsupported_source"},
		{"missing file", "clickhouse_backup", filepath.Join(dir, "gone.zip"), "source_not_found"},
		{"a directory for the file kind", "clickhouse_backup", dir, "invalid_request"},
		{"missing directory", "clickhouse_backup_dir", filepath.Join(dir, "gone"), "source_not_found"},
		{"an empty directory", "clickhouse_backup_dir", t.TempDir(), "source_not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), tc.kind, tc.path)
			if perr == nil || perr.Code != tc.want {
				t.Errorf("perr = %+v, want %s", perr, tc.want)
			}
		})
	}
}

// TestChecksumCoversTheStoredBytes pins the identity the evidence record
// carries: the hash is of the artifact exactly as stored, not of anything
// derived from it.
func TestChecksumCoversTheStoredBytes(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "shop.zip")
	writeArchive(t, archive, "2026-08-14 02:00:00")

	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	want := "sha256:" + hex.EncodeToString(sum[:])

	src, perr := resolveSource(context.Background(), "clickhouse_backup", archive)
	if perr != nil {
		t.Fatalf("perr = %+v", perr)
	}
	if src.checksum != want {
		t.Errorf("checksum = %s, want %s", src.checksum, want)
	}
	if src.sizeBytes != int64(len(raw)) {
		t.Errorf("size = %d, want %d", src.sizeBytes, len(raw))
	}
}

// TestUnreadableManifestStillRestores keeps the engine as the authority on
// whether an archive is usable: a manifest this reader dislikes costs a
// null created_at, never a failed drill.
func TestUnreadableManifestStillRestores(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "shop.zip")
	writeArchive(t, archive, "") // a manifest with no timestamp

	src, perr := resolveSource(context.Background(), "clickhouse_backup", archive)
	if perr != nil {
		t.Fatalf("perr = %+v, want the archive accepted", perr)
	}
	if !src.wallClock.IsZero() {
		t.Errorf("wallClock = %v, want zero", src.wallClock)
	}
	if createdAt(src.wallClock, time.UTC) != nil {
		t.Error("created_at must stay null when the backup declares no time")
	}
}

func TestLooksLikeArchive(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content []byte
		want    bool
	}{
		{"zip local header", []byte("PK\x03\x04rest"), true},
		{"empty central directory only", []byte("PK\x05\x06"), false},
		{"a checksum file", []byte("abc  backup.zip\n"), false},
		{"too short to tell", []byte("PK"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.content, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := looksLikeArchive(path); got != tc.want {
				t.Errorf("looksLikeArchive = %v, want %v", got, tc.want)
			}
		})
	}
}
