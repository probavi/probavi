package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveSourceKinds(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "a-old.archive")
	latest := filepath.Join(dir, "b-latest.archive")
	if err := os.WriteFile(old, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(latest, []byte("NEW"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	t.Run("mongodump file", func(t *testing.T) {
		src, perr := resolveSource(context.Background(), "mongodump", latest)
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		// createdAt stays empty: a mongodump archive records no backup
		// timestamp, and a file's mtime dates a copy rather than a backup.
		if src.path != latest || src.sizeBytes != 3 || src.gzip || src.createdAt != nil {
			t.Errorf("src = %+v", src)
		}
	})

	t.Run("mongodump_dir picks the newest", func(t *testing.T) {
		src, perr := resolveSource(context.Background(), "mongodump_dir", dir)
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		if src.path != latest {
			t.Errorf("path = %s, want the newest file %s", src.path, latest)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		if _, perr := resolveSource(context.Background(), "pgdump", latest); perr == nil || perr.Code != "unsupported_source" {
			t.Errorf("perr = %+v, want unsupported_source", perr)
		}
	})
}

func TestResolveSourceGzipSniff(t *testing.T) {
	dir := t.TempDir()
	for name, tt := range map[string]struct {
		content []byte
		gzip    bool
	}{
		"gzip magic":     {[]byte{0x1f, 0x8b, 0x08, 0x00}, true},
		"plain archive":  {[]byte("plain-archive-bytes"), false},
		"one byte":       {[]byte{0x1f}, false},
		"empty artifact": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, tt.content, 0o600); err != nil {
				t.Fatal(err)
			}
			src, perr := resolveSource(context.Background(), "mongodump", path)
			if perr != nil {
				t.Fatalf("resolve: %+v", perr)
			}
			if src.gzip != tt.gzip {
				t.Errorf("gzip = %v, want %v — the flag must come from the bytes", src.gzip, tt.gzip)
			}
		})
	}
}

func TestResolveSourceErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if _, perr := resolveSource(context.Background(), "mongodump", filepath.Join(dir, "gone")); perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("directory for the file kind", func(t *testing.T) {
		if _, perr := resolveSource(context.Background(), "mongodump", dir); perr == nil || perr.Code != "invalid_request" {
			t.Errorf("perr = %+v, want invalid_request pointing at mongodump_dir", perr)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		if _, perr := resolveSource(context.Background(), "mongodump_dir", filepath.Join(dir, "gone")); perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		empty := filepath.Join(dir, "empty")
		if err := os.Mkdir(empty, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, perr := resolveSource(context.Background(), "mongodump_dir", empty); perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("file for the dir kind", func(t *testing.T) {
		file := filepath.Join(dir, "file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, perr := resolveSource(context.Background(), "mongodump_dir", file); perr == nil || perr.Code != "source_unreadable" {
			t.Errorf("perr = %+v, want source_unreadable", perr)
		}
	})
}

func TestLatestDumpTieBreak(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for _, name := range []string{"a.archive", "b.archive"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	latest, perr := latestDumpIn(context.Background(), dir)
	if perr != nil {
		t.Fatalf("latestDumpIn: %+v", perr)
	}
	if filepath.Base(latest) != "b.archive" {
		t.Errorf("latest = %s, want the lexicographically larger name on an mtime tie", latest)
	}
}
