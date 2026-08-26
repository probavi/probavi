package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveSourceFile(t *testing.T) {
	dir := t.TempDir()
	// A dump's file name is the backup job's business: the adapter places
	// the artifact under the name the engine expects, so nothing here may
	// depend on what the operator called it.
	path := filepath.Join(dir, "orders-2026-08-26.bak")
	body := "DZV1 pretend dump bytes"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, perr := resolveSource(context.Background(), "neo4j_dump", path)
	if perr != nil {
		t.Fatalf("resolveSource = %+v", perr)
	}
	sum := sha256.Sum256([]byte(body))
	if got.checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %s, want the hash of the bytes on disk", got.checksum)
	}
	if got.sizeBytes != int64(len(body)) || got.path != path {
		t.Errorf("resolved = %+v", got)
	}
	// A Neo4j dump carries no backup timestamp, so the record must carry
	// none either — an mtime would date a copy (see zone.go).
	if got.createdAt != nil {
		t.Errorf("created_at = %v, want nil", *got.createdAt)
	}
}

func TestResolveSourceDirectoryPicksTheNewest(t *testing.T) {
	dir := t.TempDir()
	writeAgedIn(t, dir, "monday.dump", 72*time.Hour)
	newest := writeAgedIn(t, dir, "wednesday.dump", 24*time.Hour)
	writeAgedIn(t, dir, "tuesday.dump", 48*time.Hour)

	got, perr := resolveSource(context.Background(), "neo4j_dump_dir", dir)
	if perr != nil {
		t.Fatalf("resolveSource = %+v", perr)
	}
	if got.path != newest {
		t.Errorf("chose %s, want the newest file %s", got.path, newest)
	}
}

func TestResolveSourceDirectoryBreaksTiesDeterministically(t *testing.T) {
	dir := t.TempDir()
	a := writeAgedIn(t, dir, "a.dump", time.Hour)
	b := writeAgedIn(t, dir, "b.dump", time.Hour)
	when := time.Now().Add(-time.Hour)
	for _, p := range []string{a, b} {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		got, perr := resolveSource(context.Background(), "neo4j_dump_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource = %+v", perr)
		}
		if got.path != b {
			t.Fatalf("chose %s, want %s on every run — the drill must not depend on directory order", got.path, b)
		}
	}
}

func TestResolveSourceDirectoryIgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()
	older := writeAgedIn(t, dir, "old.dump", time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "newer-directory"), 0o750); err != nil {
		t.Fatal(err)
	}
	got, perr := resolveSource(context.Background(), "neo4j_dump_dir", dir)
	if perr != nil {
		t.Fatalf("resolveSource = %+v", perr)
	}
	if got.path != older {
		t.Errorf("chose %s, want the newest regular file %s", got.path, older)
	}
}

func TestResolveSourceRefusals(t *testing.T) {
	dir := t.TempDir()
	file := writeAgedIn(t, dir, "one.dump", time.Hour)
	empty := t.TempDir()

	tests := map[string]struct {
		kind, path string
		code       string
		message    string
	}{
		"unsupported kind":     {"neo4j_backup", file, "unsupported_source", "neo4j_dump"},
		"missing file":         {"neo4j_dump", filepath.Join(dir, "gone.dump"), "source_not_found", "does not exist"},
		"directory as a file":  {"neo4j_dump", dir, "invalid_request", "neo4j_dump_dir"},
		"missing directory":    {"neo4j_dump_dir", filepath.Join(dir, "gone"), "source_not_found", "does not exist"},
		"empty directory":      {"neo4j_dump_dir", empty, "source_not_found", "contains no files"},
		"file as a directory":  {"neo4j_dump_dir", file, "source_unreadable", "read backup directory"},
		"no path at all":       {"neo4j_dump", "", "source_not_found", "does not exist"},
		"no directory path":    {"neo4j_dump_dir", "", "source_not_found", "does not exist"},
		"unsupported and gone": {"", filepath.Join(dir, "gone.dump"), "unsupported_source", "supported"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), tt.kind, tt.path)
			if perr == nil {
				t.Fatalf("resolveSource accepted %s %s", tt.kind, tt.path)
			}
			if perr.Code != tt.code {
				t.Errorf("code = %s (%s), want %s", perr.Code, perr.Message, tt.code)
			}
			if !strings.Contains(perr.Message, tt.message) {
				t.Errorf("message = %q, want it to carry %q", perr.Message, tt.message)
			}
		})
	}
}

func TestFileChecksumReportsAnUnreadableArtifact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	path := writeAgedIn(t, t.TempDir(), "locked.dump", time.Hour)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	_, perr := resolveSource(context.Background(), "neo4j_dump", path)
	if perr == nil || perr.Code != "source_unreadable" {
		t.Errorf("perr = %+v, want source_unreadable", perr)
	}
}
