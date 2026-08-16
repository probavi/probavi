package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// duckFixture builds a database-shaped artifact head for host-side tests:
// the layout the header parser reads (measured), with filler where the
// checksum and reserved bytes sit. The host side never validates past the
// magic — duckdb inside the sandbox is the authority on the rest.
func duckFixture(storage uint64, libVer string) []byte {
	buf := make([]byte, headMax)
	copy(buf[duckMagicOffset:], duckMagic)
	binary.LittleEndian.PutUint64(buf[storageVersionOffset:], storage)
	copy(buf[libraryVersionOffset:], libVer)
	return buf
}

// dbFixture is the default database-shaped artifact.
func dbFixture() []byte { return duckFixture(64, "v1.5.5") }

func writeArtifact(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeExport lays down an EXPORT DATABASE-shaped directory.
func writeExport(t *testing.T, dir string, names ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		writeArtifact(t, dir, name, []byte("-- "+name+"\n"))
	}
	return dir
}

func TestResolveSourceUnknownKind(t *testing.T) {
	_, perr := resolveSource(context.Background(), "duckdb_backup", "/nowhere")
	if perr == nil || perr.Code != "unsupported_source" {
		t.Fatalf("perr = %+v, want unsupported_source", perr)
	}
	for _, kind := range []string{"duckdb_db", "duckdb_db_dir", "duckdb_export"} {
		if !strings.Contains(perr.Message, kind) {
			t.Errorf("message %q does not list %s", perr.Message, kind)
		}
	}
}

func TestResolveDatabase(t *testing.T) {
	t.Run("a cold copy resolves with its measured identity", func(t *testing.T) {
		content := duckFixture(64, "v1.4.5")
		path := writeArtifact(t, t.TempDir(), "nightly.duckdb", content)
		src, perr := resolveSource(context.Background(), "duckdb_db", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		sum := sha256.Sum256(content)
		if want := "sha256:" + hex.EncodeToString(sum[:]); src.checksum != want {
			t.Errorf("checksum = %s, want %s", src.checksum, want)
		}
		if src.sizeBytes != int64(len(content)) || src.export {
			t.Errorf("resolved = %+v", src)
		}
		if src.header.storageVersion != 64 || src.header.libraryVersion != "v1.4.5" {
			t.Errorf("header = %+v, want the artifact's own version fields", src.header)
		}
	})

	t.Run("a file the parser cannot read still resolves", func(t *testing.T) {
		// Metadata is a bonus: the engine in the sandbox is the authority
		// on whether the bytes are a database.
		path := writeArtifact(t, t.TempDir(), "opaque.duckdb", []byte("not a duckdb file"))
		src, perr := resolveSource(context.Background(), "duckdb_db", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.header.valid {
			t.Error("header parsed from bytes without the magic")
		}
	})

	t.Run("an empty sibling does not condemn the backup", func(t *testing.T) {
		dir := t.TempDir()
		path := writeArtifact(t, dir, "nightly.duckdb", dbFixture())
		writeArtifact(t, dir, "nightly.duckdb.wal", nil)
		if _, perr := resolveSource(context.Background(), "duckdb_db", path); perr != nil {
			t.Fatalf("resolveSource: %+v — a zero-byte .wal holds no transactions", perr)
		}
	})

}

// TestLiveCopyRefusalTeachesTheFix pins the message an operator acts on:
// the refusal has to say what to do, not just what is wrong.
func TestLiveCopyRefusalTeachesTheFix(t *testing.T) {
	dir := t.TempDir()
	path := writeArtifact(t, dir, "nightly.duckdb", dbFixture())
	writeArtifact(t, dir, "nightly.duckdb.wal", []byte("wal frames"))
	_, perr := resolveSource(context.Background(), "duckdb_db", path)
	if perr == nil {
		t.Fatal("a live copy resolved")
	}
	for _, want := range []string{"closed", "EXPORT DATABASE"} {
		if !strings.Contains(perr.Message, want) {
			t.Errorf("message %q does not name %s", perr.Message, want)
		}
	}
}

func TestResolveDatabaseRefusals(t *testing.T) {
	refusals := []struct {
		name     string
		prepare  func(t *testing.T, dir string) string
		wantCode string
		wantWord string
	}{
		{"missing file", func(t *testing.T, dir string) string {
			return filepath.Join(dir, "gone.duckdb")
		}, "source_not_found", ""},
		{"a directory names both directory kinds", func(t *testing.T, dir string) string {
			return dir
		}, "invalid_request", "duckdb_export"},
		{"gzip is refused by name", func(t *testing.T, dir string) string {
			return writeArtifact(t, dir, "nightly.duckdb.gz", []byte{0x1f, 0x8b, 0x08, 0x00})
		}, "unsupported_source", "gzip"},
		{"a non-empty .wal sibling is a live copy", func(t *testing.T, dir string) string {
			path := writeArtifact(t, dir, "nightly.duckdb", dbFixture())
			writeArtifact(t, dir, "nightly.duckdb.wal", []byte("wal frames"))
			return path
		}, "unsupported_source", ".wal"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.prepare(t, t.TempDir())
			_, perr := resolveSource(context.Background(), "duckdb_db", path)
			if perr == nil || perr.Code != tc.wantCode {
				t.Fatalf("perr = %+v, want %s", perr, tc.wantCode)
			}
			if tc.wantWord != "" && !strings.Contains(perr.Message, tc.wantWord) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantWord)
			}
		})
	}
}

func TestResolveExport(t *testing.T) {
	t.Run("a complete export resolves with a tree checksum", func(t *testing.T) {
		dir := writeExport(t, filepath.Join(t.TempDir(), "nightly"),
			"schema.sql", "load.sql", "t.csv")
		src, perr := resolveSource(context.Background(), "duckdb_export", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if !src.export || !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
			t.Errorf("resolved = %+v", src)
		}
	})

	t.Run("the tree checksum sees content changes", func(t *testing.T) {
		dir := writeExport(t, filepath.Join(t.TempDir(), "nightly"),
			"schema.sql", "load.sql", "t.csv")
		before, perr := resolveSource(context.Background(), "duckdb_export", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if err := os.WriteFile(filepath.Join(dir, "t.csv"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, perr := resolveSource(context.Background(), "duckdb_export", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if before.checksum == after.checksum {
			t.Error("checksum unchanged after a data file changed")
		}
	})

}

func TestResolveExportRefusals(t *testing.T) {
	refusals := []struct {
		name     string
		prepare  func(t *testing.T, dir string) string
		wantCode string
		wantWord string
	}{
		{"missing directory", func(t *testing.T, dir string) string {
			return filepath.Join(dir, "gone")
		}, "source_not_found", ""},
		{"a database file needs the db kind", func(t *testing.T, dir string) string {
			return writeArtifact(t, dir, "nightly.duckdb", dbFixture())
		}, "invalid_request", "duckdb_db"},
		{"a directory without schema.sql is not an export", func(t *testing.T, dir string) string {
			return writeExport(t, filepath.Join(dir, "x"), "load.sql", "t.csv")
		}, "source_corrupt", "schema.sql"},
		{"a directory without load.sql is not an export", func(t *testing.T, dir string) string {
			return writeExport(t, filepath.Join(dir, "x"), "schema.sql", "t.csv")
		}, "source_corrupt", "load.sql"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.prepare(t, t.TempDir())
			_, perr := resolveSource(context.Background(), "duckdb_export", path)
			if perr == nil || perr.Code != tc.wantCode {
				t.Fatalf("perr = %+v, want %s", perr, tc.wantCode)
			}
			if tc.wantWord != "" && !strings.Contains(perr.Message, tc.wantWord) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantWord)
			}
		})
	}
}

func TestExportFiles(t *testing.T) {
	dir := writeExport(t, filepath.Join(t.TempDir(), "nightly"),
		"schema.sql", "load.sql", "t.csv")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	names, perr := exportFiles(dir)
	if perr != nil {
		t.Fatalf("exportFiles: %+v", perr)
	}
	if len(names) != 3 {
		t.Errorf("names = %v, want the three regular files only", names)
	}
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
		old := writeArtifact(t, dir, "monday.duckdb", dbFixture())
		age(t, old, 48*time.Hour)
		newest := writeArtifact(t, dir, "tuesday.duckdb", dbFixture())
		age(t, newest, 24*time.Hour)
		sidecar := writeArtifact(t, dir, "checksums.txt", []byte("sha256 sums\n"))
		age(t, sidecar, time.Hour) // newer than every database, still not a candidate
		src, perr := resolveSource(context.Background(), "duckdb_db_dir", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newest {
			t.Errorf("picked %s, want %s", src.path, newest)
		}
	})

	t.Run("a modification-time tie breaks toward the larger name", func(t *testing.T) {
		dir := t.TempDir()
		a := writeArtifact(t, dir, "a.duckdb", dbFixture())
		b := writeArtifact(t, dir, "b.duckdb", dbFixture())
		age(t, a, time.Hour)
		age(t, b, time.Hour)
		src, perr := resolveSource(context.Background(), "duckdb_db_dir", dir)
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
		_, perr := resolveSource(context.Background(), "duckdb_db_dir", dir)
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "2 files") {
			t.Fatalf("perr = %+v, want source_not_found counting the 2 passed-over files", perr)
		}
	})

	t.Run("an empty directory says so", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "duckdb_db_dir", t.TempDir())
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "contains no files") {
			t.Fatalf("perr = %+v, want source_not_found for an empty directory", perr)
		}
	})

	t.Run("a missing directory is source_not_found", func(t *testing.T) {
		_, perr := resolveSource(context.Background(), "duckdb_db_dir", filepath.Join(t.TempDir(), "gone"))
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
	old := writeArtifact(t, dir, "monday.duckdb", dbFixture())
	age(t, old, 48*time.Hour)
	newest := writeArtifact(t, dir, "tuesday.duckdb", dbFixture())
	age(t, newest, 24*time.Hour)
	wal := writeArtifact(t, dir, "tuesday.duckdb.wal", []byte("wal frames"))
	age(t, wal, 24*time.Hour)

	_, perr := resolveSource(context.Background(), "duckdb_db_dir", dir)
	if perr == nil || perr.Code != "unsupported_source" || !strings.Contains(perr.Message, ".wal") {
		t.Fatalf("perr = %+v, want the live copy refused by name", perr)
	}
	if strings.Contains(perr.Message, "monday") {
		t.Error("the drill fell back to the older backup — that would prove a backup the record does not name")
	}
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
