package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveSourceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orders.sql")
	if err := os.WriteFile(path, []byte("-- dump"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	src, perr := resolveSource(context.Background(), "mariadb_dump", path, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	// The fixture carries no mysqldump trailer, so nothing dates it: the
	// file's mtime is deliberately not used, because copying a backup
	// resets it while leaving a perfectly valid backup behind.
	if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes != 7 || src.createdAt != nil {
		t.Errorf("src = %+v", src)
	}

	tests := []struct {
		name     string
		kind     string
		path     string
		wantCode string
	}{
		{"missing file", "mariadb_dump", filepath.Join(dir, "gone.sql"), "source_not_found"},
		{"directory as file", "mariadb_dump", dir, "invalid_request"},
		{"unsupported kind", "walg", path, "unsupported_source"},
		{"missing directory", "mariadb_dump_dir", filepath.Join(dir, "gone"), "source_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, perr := resolveSource(context.Background(), tt.kind, tt.path, nil); perr == nil || perr.Code != tt.wantCode {
				t.Errorf("resolveSource(context.Background(), %s, %s) = %+v, want %s", tt.kind, tt.path, perr, tt.wantCode)
			}
		})
	}
}

func TestResolveSourceDirPicksNewest(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for name, mtime := range map[string]time.Time{
		"monday.sql":  old.Add(-time.Hour),
		"tuesday.sql": old,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	src, perr := resolveSource(context.Background(), "mariadb_dump_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if filepath.Base(src.path) != "tuesday.sql" {
		t.Errorf("picked %s, want the newest file tuesday.sql", src.path)
	}

	// Equal mtimes: the lexicographically larger name must win so the
	// choice stays deterministic across runs.
	tie := filepath.Join(dir, "aaa.sql")
	if err := os.WriteFile(tie, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	newest := time.Now()
	for _, name := range []string{"tuesday.sql", "aaa.sql"} {
		if err := os.Chtimes(filepath.Join(dir, name), newest, newest); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	src, perr = resolveSource(context.Background(), "mariadb_dump_dir", dir, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if filepath.Base(src.path) != "tuesday.sql" {
		t.Errorf("tie broke to %s, want tuesday.sql", src.path)
	}

	empty := t.TempDir()
	if _, perr := resolveSource(context.Background(), "mariadb_dump_dir", empty, nil); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("empty dir: %+v, want source_not_found", perr)
	}
}

// writeDumpAs writes a dump carrying its own completion trailer into dir
// under a chosen name and modification time, so a test can set the two
// clocks — the one the backup records and the one the filesystem records —
// independently.
func writeDumpAs(t *testing.T, dir, name, clock string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "INSERT INTO orders VALUES (1);\n"
	if clock != "" {
		body += "-- Dump completed on " + clock + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

// TestDirectoryRankingIgnoresFileTimes is issue #100: a stale dump copied
// into the directory afterwards carries the newest mtime, and used to be
// the one the drill restored.
func TestDirectoryRankingIgnoresFileTimes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	writeDumpAs(t, dir, "stale.sql", "2026-08-01 03:00:00", now)
	fresh := writeDumpAs(t, dir, "fresh.sql", "2026-08-09 03:00:00", now.Add(-24*time.Hour))

	got, perr := newestBackupIn(context.Background(), dir, "")
	if perr != nil {
		t.Fatalf("newestBackupIn: %+v", perr)
	}
	if got != fresh {
		t.Errorf("picked %s, want %s — the copy's file time must not outrank the dump's own trailer",
			filepath.Base(got), filepath.Base(fresh))
	}
}

func TestDirectoryRanking(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	t.Run("a dump that can be dated beats one that cannot", func(t *testing.T) {
		dir := t.TempDir()
		dated := writeDumpAs(t, dir, "a-dated.sql", "2026-08-01 03:00:00", base.Add(-time.Hour))
		// --skip-dump-date leaves the sentence without a date behind it.
		writeDumpAs(t, dir, "z-undated.sql", "", base)
		got, perr := newestBackupIn(context.Background(), dir, "")
		if perr != nil || got != dated {
			t.Errorf("picked %s (%+v), want the dump that carries its own time", got, perr)
		}
	})

	t.Run("undatable dumps keep the file-time rule", func(t *testing.T) {
		dir := t.TempDir()
		writeDumpAs(t, dir, "old.sql", "", base.Add(-time.Hour))
		newest := writeDumpAs(t, dir, "new.sql", "", base)
		got, perr := newestBackupIn(context.Background(), dir, "")
		if perr != nil || got != newest {
			t.Errorf("picked %s (%+v), want the newest file when nothing else can rank them", got, perr)
		}
	})

	t.Run("two dumps completed in the same second break by file time", func(t *testing.T) {
		dir := t.TempDir()
		writeDumpAs(t, dir, "a.sql", "2026-08-09 03:00:00", base.Add(-time.Hour))
		want := writeDumpAs(t, dir, "b.sql", "2026-08-09 03:00:00", base)
		got, perr := newestBackupIn(context.Background(), dir, "")
		if perr != nil || got != want {
			t.Errorf("picked %s (%+v), want the newer file of two dumps recording the same clock", got, perr)
		}
	})

	t.Run("the named member is skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeDumpAs(t, dir, "users.sql", "2026-08-09 03:00:00", base)
		want := writeDumpAs(t, dir, "orders.sql", "2026-08-01 03:00:00", base)
		got, perr := newestBackupIn(context.Background(), dir, "users.sql")
		if perr != nil || got != want {
			t.Errorf("picked %s (%+v), want the dump beside the skipped member", got, perr)
		}
	})
}

// TestCandidateRankingIsATotalOrder pins the tie-breaking that keeps a
// choice from depending on directory iteration order.
func TestCandidateRankingIsATotalOrder(t *testing.T) {
	early := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	tests := []struct {
		name string
		a, b dirCandidate
	}{
		{"newer recorded clock wins",
			dirCandidate{name: "a", clock: late, dated: true, mtime: early},
			dirCandidate{name: "b", clock: early, dated: true, mtime: late}},
		{"dated beats undated even when older",
			dirCandidate{name: "a", clock: early, dated: true, mtime: early},
			dirCandidate{name: "b", mtime: late}},
		{"same clock falls through to the file time",
			dirCandidate{name: "a", clock: late, dated: true, mtime: late},
			dirCandidate{name: "b", clock: late, dated: true, mtime: early}},
		{"everything equal falls through to the name",
			dirCandidate{name: "b", clock: late, dated: true, mtime: late},
			dirCandidate{name: "a", clock: late, dated: true, mtime: late}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.a.beats(tt.b) {
				t.Errorf("a.beats(b) = false, want a to win")
			}
			if tt.b.beats(tt.a) {
				t.Error("b.beats(a) too — the ranking must be a strict order")
			}
		})
	}
	same := dirCandidate{name: "a", clock: late, dated: true, mtime: late}
	if same.beats(same) {
		t.Error("a candidate beats itself — the ranking is not strict")
	}
}

// writeCompressedDumpAs is writeDumpAs for a dump stored the way a dump
// pipeline stores one, so a test can put both storage forms in one
// directory and set their file times independently.
func writeCompressedDumpAs(t *testing.T, dir, name, clock string, mtime time.Time) string {
	t.Helper()
	body := "INSERT INTO orders VALUES (1);\n"
	if clock != "" {
		body += "-- Dump completed on " + clock + "\n"
	}
	path := writeGzipDump(t, dir, name, body)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

// TestCompressedSourceKeepsTheStoredIdentity is why the adapter takes the
// artifact as stored: decompressing it outside Probavi would leave
// backup.checksum covering a temporary file that is in no backup archive.
func TestCompressedSourceKeepsTheStoredIdentity(t *testing.T) {
	dir := t.TempDir()
	const body = "INSERT INTO orders VALUES (1);\n-- Dump completed on 2026-08-09 21:08:17\n"
	path := writeGzipDump(t, dir, "orders.sql.gz", body)
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	src, perr := resolveSource(context.Background(), "mariadb_dump", path,
		map[string]string{"backup_timezone": "UTC"})
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !src.compressed {
		t.Error("compressed = false for a gzip member")
	}
	want, perr := fileChecksum(path)
	if perr != nil {
		t.Fatalf("fileChecksum: %+v", perr)
	}
	if src.checksum != want || src.sizeBytes != int64(len(stored)) {
		t.Errorf("identity = %s/%d, want the stored bytes (%s/%d)",
			src.checksum, src.sizeBytes, want, len(stored))
	}
	if src.createdAt == nil || *src.createdAt != "2026-08-09T21:08:17.000Z" {
		t.Errorf("createdAt = %v, want the trailer read through the decompressor", src.createdAt)
	}
}

// TestDirectoryRankingSpansStorageForms is the point of reading a
// compressed candidate's trailer at all: both forms record the same
// sentence, so they rank on one scale and a stale plain dump copied in
// yesterday cannot outrank last night's compressed one.
func TestDirectoryRankingSpansStorageForms(t *testing.T) {
	copiedIn := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	t.Run("a compressed dump can be the newest", func(t *testing.T) {
		dir := t.TempDir()
		writeDumpAs(t, dir, "stale.sql", "2026-08-01 03:00:00", copiedIn)
		fresh := writeCompressedDumpAs(t, dir, "fresh.sql.gz", "2026-08-09 03:00:00",
			copiedIn.Add(-24*time.Hour))
		got, perr := newestBackupIn(context.Background(), dir, "")
		if perr != nil || got != fresh {
			t.Errorf("picked %s (%+v), want the compressed dump that records the newer time",
				filepath.Base(got), perr)
		}
	})

	t.Run("a plain dump can be the newest", func(t *testing.T) {
		dir := t.TempDir()
		writeCompressedDumpAs(t, dir, "stale.sql.gz", "2026-08-01 03:00:00", copiedIn)
		fresh := writeDumpAs(t, dir, "fresh.sql", "2026-08-09 03:00:00", copiedIn.Add(-24*time.Hour))
		got, perr := newestBackupIn(context.Background(), dir, "")
		if perr != nil || got != fresh {
			t.Errorf("picked %s (%+v), want the plain dump that records the newer time",
				filepath.Base(got), perr)
		}
	})

	t.Run("a directory of compressed dumps ranks by their own times", func(t *testing.T) {
		dir := t.TempDir()
		// Every file time says the opposite of every trailer, which is what
		// an object-store download produces.
		writeCompressedDumpAs(t, dir, "monday.sql.gz", "2026-08-03 03:00:00", copiedIn)
		writeCompressedDumpAs(t, dir, "tuesday.sql.gz", "2026-08-04 03:00:00", copiedIn.Add(-time.Hour))
		want := writeCompressedDumpAs(t, dir, "sunday.sql.gz", "2026-08-09 03:00:00",
			copiedIn.Add(-2*time.Hour))
		got, perr := newestBackupIn(context.Background(), dir, "")
		if perr != nil || got != want {
			t.Errorf("picked %s (%+v), want sunday.sql.gz", filepath.Base(got), perr)
		}
	})
}
