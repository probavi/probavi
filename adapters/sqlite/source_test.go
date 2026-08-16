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

// dbFixture is a database-shaped artifact for host-side tests: the real
// magic followed by filler. The host side never validates past the magic —
// sqlite3 inside the sandbox is the authority on the rest.
func dbFixture() []byte {
	return append([]byte(sqliteMagic), []byte("unit-test database body\n")...)
}

// dumpFixture is a complete .dump-shaped artifact.
func dumpFixture() []byte {
	return []byte(dumpSignature + "CREATE TABLE t(id INTEGER PRIMARY KEY);\nINSERT INTO t VALUES(1);\nCOMMIT;\n")
}

func writeArtifact(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveSourceUnknownKind(t *testing.T) {
	_, perr := resolveSource(context.Background(), "sqlite_backup", "/nowhere")
	if perr == nil || perr.Code != "unsupported_source" {
		t.Fatalf("perr = %+v, want unsupported_source", perr)
	}
	for _, kind := range []string{"sqlite_db", "sqlite_db_dir", "sqlite_dump", "sqlite_dump_dir"} {
		if !strings.Contains(perr.Message, kind) {
			t.Errorf("message %q does not list %s", perr.Message, kind)
		}
	}
}

func TestResolveDatabase(t *testing.T) {
	t.Run("a well-taken backup resolves with its measured identity", func(t *testing.T) {
		dir := t.TempDir()
		content := dbFixture()
		path := writeArtifact(t, dir, "nightly.db", content)
		src, perr := resolveSource(context.Background(), "sqlite_db", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		sum := sha256.Sum256(content)
		if want := "sha256:" + hex.EncodeToString(sum[:]); src.checksum != want {
			t.Errorf("checksum = %s, want %s", src.checksum, want)
		}
		if src.sizeBytes != int64(len(content)) || src.sql {
			t.Errorf("resolved = %+v", src)
		}
	})

	t.Run("an empty sibling does not condemn the backup", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "nightly.db", dbFixture())
		writeArtifact(t, dir, "nightly.db-wal", nil)
		if _, perr := resolveSource(context.Background(), "sqlite_db", path); perr != nil {
			t.Fatalf("resolveSource: %+v — a zero-byte -wal holds no transactions", perr)
		}
	})

	t.Run("the live-copy refusal teaches the fix", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "nightly.db", dbFixture())
		writeArtifact(t, dir, "nightly.db-wal", []byte("wal frames"))
		_, perr := resolveSource(context.Background(), "sqlite_db", path)
		if perr == nil {
			t.Fatal("a live copy resolved")
		}
		for _, want := range []string{".backup", "VACUUM INTO"} {
			if !strings.Contains(perr.Message, want) {
				t.Errorf("message %q does not name %s", perr.Message, want)
			}
		}
	})
}

func TestResolveDatabaseRefusals(t *testing.T) {
	refusals := []struct {
		name     string
		prepare  func(t *testing.T, dir string) string
		wantCode string
		wantWord string
	}{
		{"missing file", func(t *testing.T, dir string) string {
			return filepath.Join(dir, "gone.db")
		}, "source_not_found", ""},
		{"a directory needs the dir kind", func(t *testing.T, dir string) string {
			return dir
		}, "invalid_request", "sqlite_db_dir"},
		{"gzip is refused by name", func(t *testing.T, dir string) string {
			return writeArtifact(t, dir, "nightly.db.gz", []byte{0x1f, 0x8b, 0x08, 0x00})
		}, "unsupported_source", "gzip"},
		{"dump text points at the dump kind", func(t *testing.T, dir string) string {
			return writeArtifact(t, dir, "nightly.db", dumpFixture())
		}, "invalid_request", "sqlite_dump"},
		{"a zero-byte file is a broken backup job", func(t *testing.T, dir string) string {
			return writeArtifact(t, dir, "nightly.db", nil)
		}, "source_corrupt", "empty"},
		{"a non-empty -wal sibling is a live copy", func(t *testing.T, dir string) string {
			path := writeArtifact(t, dir, "nightly.db", dbFixture())
			writeArtifact(t, dir, "nightly.db-wal", []byte("wal frames"))
			return path
		}, "unsupported_source", "-wal"},
		{"a non-empty -journal sibling is a mid-write copy", func(t *testing.T, dir string) string {
			path := writeArtifact(t, dir, "nightly.db", dbFixture())
			writeArtifact(t, dir, "nightly.db-journal", []byte("journal pages"))
			return path
		}, "unsupported_source", "-journal"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.prepare(t, t.TempDir())
			_, perr := resolveSource(context.Background(), "sqlite_db", path)
			if perr == nil || perr.Code != tc.wantCode {
				t.Fatalf("perr = %+v, want %s", perr, tc.wantCode)
			}
			if tc.wantWord != "" && !strings.Contains(perr.Message, tc.wantWord) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantWord)
			}
		})
	}
}

func TestResolveDump(t *testing.T) {
	t.Run("a complete dump resolves as sql", func(t *testing.T) {
		path := writeArtifact(t, t.TempDir(), "nightly.sql", dumpFixture())
		src, perr := resolveSource(context.Background(), "sqlite_dump", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if !src.sql {
			t.Error("resolved dump not marked as sql")
		}
	})

	t.Run("generic sql text skips the completeness gate", func(t *testing.T) {
		// No .dump signature: the trailer contract does not apply, and the
		// replay inside the sandbox is the judge.
		path := writeArtifact(t, t.TempDir(), "schema.sql",
			[]byte("CREATE TABLE t(id INTEGER);\nINSERT INTO t VALUES(1);\n"))
		if _, perr := resolveSource(context.Background(), "sqlite_dump", path); perr != nil {
			t.Fatalf("resolveSource: %+v — generic SQL must stay for the sandbox to judge", perr)
		}
	})

	refusals := []struct {
		name     string
		content  []byte
		wantCode string
		wantWord string
	}{
		{"a database file points at the db kind", dbFixture(), "invalid_request", "sqlite_db"},
		{"gzip is refused by name", []byte{0x1f, 0x8b, 0x08, 0x00}, "unsupported_source", "gzip"},
		{"a zero-byte file is a broken backup job", nil, "source_corrupt", "empty"},
		{"a dump without its trailer was truncated",
			[]byte(dumpSignature + "CREATE TABLE t(id INTEGER);\nINSERT INTO t VALUES(1);\n"),
			"source_corrupt", "COMMIT;"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			path := writeArtifact(t, t.TempDir(), "nightly.sql", tc.content)
			_, perr := resolveSource(context.Background(), "sqlite_dump", path)
			if perr == nil || perr.Code != tc.wantCode {
				t.Fatalf("perr = %+v, want %s", perr, tc.wantCode)
			}
			if !strings.Contains(perr.Message, tc.wantWord) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantWord)
			}
		})
	}

	t.Run("a directory needs the dir kind", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "sqlite_dump", t.TempDir())
		if perr == nil || perr.Code != "invalid_request" || !strings.Contains(perr.Message, "sqlite_dump_dir") {
			t.Fatalf("perr = %+v, want invalid_request naming sqlite_dump_dir", perr)
		}
	})
}

// age backdates a file so directory-ranking tests control the order.
func age(t *testing.T, path string, by time.Duration) {
	t.Helper()
	when := time.Now().Add(-by)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestLatestDatabaseIn(t *testing.T) {
	t.Run("the newest database wins and sidecars are not candidates", func(t *testing.T) {
		dir := t.TempDir()
		old := writeArtifact(t, dir, "monday.db", dbFixture())
		age(t, old, 48*time.Hour)
		newest := writeArtifact(t, dir, "tuesday.db", dbFixture())
		age(t, newest, 24*time.Hour)
		sidecar := writeArtifact(t, dir, "checksums.txt", []byte("sha256 sums\n"))
		age(t, sidecar, time.Hour) // newer than every database, still not a candidate
		src, perr := resolveSource(context.Background(), "sqlite_db_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newest {
			t.Errorf("picked %s, want %s", src.path, newest)
		}
	})

	t.Run("a modification-time tie breaks toward the larger name", func(t *testing.T) {
		dir := t.TempDir()
		a := writeArtifact(t, dir, "a.db", dbFixture())
		b := writeArtifact(t, dir, "b.db", dbFixture())
		age(t, a, time.Hour)
		age(t, b, time.Hour)
		src, perr := resolveSource(context.Background(), "sqlite_db_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != b {
			t.Errorf("picked %s, want the deterministic %s", src.path, b)
		}
	})

	t.Run("only sidecars is a counted refusal", func(t *testing.T) {
		dir := t.TempDir()
		writeArtifact(t, dir, "README.md", []byte("backups live here\n"))
		writeArtifact(t, dir, "checksums.txt", []byte("sums\n"))
		_, perr := resolveSource(context.Background(), "sqlite_db_dir", dir)
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "2 files") {
			t.Fatalf("perr = %+v, want source_not_found counting the 2 passed-over files", perr)
		}
	})

	t.Run("an empty directory says so", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "sqlite_db_dir", t.TempDir())
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "contains no files") {
			t.Fatalf("perr = %+v, want source_not_found for an empty directory", perr)
		}
	})

	t.Run("a missing directory is source_not_found", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "sqlite_db_dir", filepath.Join(t.TempDir(), "gone"))
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})
}

// TestDirectoryRefusesTheNewestWhenItIsALiveCopy pins the not-a-filter
// principle for the live-copy fence: the newest database wins the ranking
// and is then refused by name — never silently passed over for an older
// neighbour the record would not describe.
func TestDirectoryRefusesTheNewestWhenItIsALiveCopy(t *testing.T) {
	dir := t.TempDir()
	old := writeArtifact(t, dir, "monday.db", dbFixture())
	age(t, old, 48*time.Hour)
	newest := writeArtifact(t, dir, "tuesday.db", dbFixture())
	age(t, newest, 24*time.Hour)
	wal := writeArtifact(t, dir, "tuesday.db-wal", []byte("wal frames"))
	age(t, wal, 24*time.Hour)

	_, perr := resolveSource(context.Background(), "sqlite_db_dir", dir)
	if perr == nil || perr.Code != "unsupported_source" || !strings.Contains(perr.Message, "-wal") {
		t.Fatalf("perr = %+v, want the live copy refused by name", perr)
	}
	if strings.Contains(perr.Message, "monday") {
		t.Error("the drill fell back to the older backup — that would prove a backup the record does not name")
	}
}

func TestLatestDumpIn(t *testing.T) {
	t.Run("the newest file wins with no candidacy filter", func(t *testing.T) {
		dir := t.TempDir()
		old := writeArtifact(t, dir, "monday.sql", dumpFixture())
		age(t, old, 48*time.Hour)
		newest := writeArtifact(t, dir, "tuesday.sql", dumpFixture())
		age(t, newest, 24*time.Hour)
		src, perr := resolveSource(context.Background(), "sqlite_dump_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newest {
			t.Errorf("picked %s, want %s", src.path, newest)
		}
	})

	t.Run("a truncated newest dump is refused, not passed over", func(t *testing.T) {
		dir := t.TempDir()
		old := writeArtifact(t, dir, "monday.sql", dumpFixture())
		age(t, old, 48*time.Hour)
		truncated := writeArtifact(t, dir, "tuesday.sql",
			[]byte(dumpSignature+"CREATE TABLE t(id INTEGER);\nINSERT INTO t VALUES(1);\n"))
		age(t, truncated, 24*time.Hour)
		_, perr := resolveSource(context.Background(), "sqlite_dump_dir", dir)
		if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "COMMIT;") {
			t.Fatalf("perr = %+v, want the truncated newest dump refused by name", perr)
		}
		if strings.Contains(perr.Message, "monday") {
			t.Error("the drill fell back to the older dump")
		}
	})

	t.Run("an empty directory says so", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "sqlite_dump_dir", t.TempDir())
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})
}

func TestRejectBackupTimezone(t *testing.T) {
	if perr := rejectBackupTimezone(nil); perr != nil {
		t.Fatalf("perr = %+v, want nil for absent params", perr)
	}
	if perr := rejectBackupTimezone(map[string]string{"backup_timezone": ""}); perr != nil {
		t.Fatalf("perr = %+v, want nil for an empty declaration", perr)
	}
	perr := rejectBackupTimezone(map[string]string{"backup_timezone": "Europe/Budapest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want invalid_request", perr)
	}
}
