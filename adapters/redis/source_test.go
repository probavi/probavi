package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveFileReadsItsOwnMetadata(t *testing.T) {
	dir := t.TempDir()
	path := writeRDB(t, dir, "dump.rdb", "7.2.5", "1786289869")
	src, perr := resolveSource(context.Background(), "redis_rdb", path)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
		t.Errorf("src = %+v", src)
	}
	if src.redisVer != "7.2.5" {
		t.Errorf("redisVer = %q", src.redisVer)
	}
	if src.createdAt == nil || *src.createdAt != "2026-08-09T15:37:49.000Z" {
		t.Errorf("createdAt = %v, want the RDB's own save instant", src.createdAt)
	}
}

// TestResolveFileWithoutDate pins the fail-closed half: an artifact whose
// header carries no ctime resolves undated — never dated by its mtime,
// which dates a copy.
func TestResolveFileWithoutDate(t *testing.T) {
	dir := t.TempDir()
	path := writeRDB(t, dir, "dump.rdb", "", "")
	old := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "redis_rdb", path)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.createdAt != nil {
		t.Errorf("createdAt = %v, want nil — an mtime is not the backup's save instant", *src.createdAt)
	}
}

func TestDirectoryRanking(t *testing.T) {
	t.Run("the artifact's own instant outranks file time", func(t *testing.T) {
		dir := t.TempDir()
		// The dated-newer artifact has the OLDER mtime: self-description
		// must win over file time.
		newer := writeRDB(t, dir, "a-dated-newer.rdb", "", "1786289869")
		older := writeRDB(t, dir, "b-dated-older.rdb", "", "1786203469")
		past := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(newer, past, past); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource(context.Background(), "redis_rdb_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newer {
			t.Errorf("picked %s, want the artifact whose own header is newest", src.path)
		}
		_ = older
	})

	t.Run("a dated artifact outranks every undated one", func(t *testing.T) {
		dir := t.TempDir()
		dated := writeRDB(t, dir, "a-dated.rdb", "", "1786203469")
		undated := writeRDB(t, dir, "z-undated.rdb", "", "")
		past := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(dated, past, past); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource(context.Background(), "redis_rdb_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != dated {
			t.Errorf("picked %s, want the dated artifact", src.path)
		}
		_ = undated
	})

	t.Run("undated artifacts fall back to file time", func(t *testing.T) {
		dir := t.TempDir()
		writeRDB(t, dir, "a-old.rdb", "", "")
		older := filepath.Join(dir, "a-old.rdb")
		past := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(older, past, past); err != nil {
			t.Fatal(err)
		}
		newest := writeRDB(t, dir, "b-new.rdb", "", "")
		src, perr := resolveSource(context.Background(), "redis_rdb_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newest {
			t.Errorf("picked %s, want the newest by file time", src.path)
		}
	})

}

func TestDirectoryRefusals(t *testing.T) {
	t.Run("non-RDB files are not candidates and are counted", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"README.txt", "dump.rdb.sha256"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("not an rdb"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, perr := resolveSource(context.Background(), "redis_rdb_dir", dir)
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "2 files") {
			t.Errorf("perr = %+v, want source_not_found counting the passed-over files", perr)
		}
	})

	t.Run("an empty directory says so", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "redis_rdb_dir", t.TempDir())
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "contains no files") {
			t.Errorf("perr = %+v", perr)
		}
	})

	t.Run("a missing directory says so", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "redis_rdb_dir", filepath.Join(t.TempDir(), "gone"))
		if perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v", perr)
		}
	})
}

func TestCandidateOrdering(t *testing.T) {
	at := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		c, o rdbCandidate
		want bool
	}{
		{"dated beats undated", rdbCandidate{ctime: 5}, rdbCandidate{mtime: at}, true},
		{"undated loses to dated", rdbCandidate{mtime: at}, rdbCandidate{ctime: 5}, false},
		{"newer instant wins", rdbCandidate{ctime: 9}, rdbCandidate{ctime: 5}, true},
		{"undated: newer mtime wins", rdbCandidate{mtime: at.Add(time.Hour)}, rdbCandidate{mtime: at}, true},
		{"full tie breaks by name", rdbCandidate{path: "b", mtime: at}, rdbCandidate{path: "a", mtime: at}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.beats(tt.o); got != tt.want {
				t.Errorf("beats = %v, want %v", got, tt.want)
			}
		})
	}
}
