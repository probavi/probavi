package main

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dbFixture is a database-shaped artifact for host-side tests: the real
// MVStore header line H2 2.x writes, then a body the host side never
// inspects. Every gate that keys on the header sees exactly what the
// engine writes.
func dbFixture() []byte {
	return append([]byte(mvStoreHeader("3")), []byte("unit-test database body\n")...)
}

// mvStoreHeader renders an MVStore file header declaring the given
// storage format, in the field order and syntax H2 writes it (measured on
// 1.4.200, 2.2.224, 2.3.232 and 2.4.240).
func mvStoreHeader(format string) string {
	return "H:2,block:2,blockSize:1000,chunk:d,clean:1,created:1a052269c27,format:" +
		format + ",version:d,fletcher:4b17720f\n"
}

// archiveFixture is a BACKUP TO-shaped artifact: a zip holding one
// .mv.db entry, which is the only thing that command ever writes.
func archiveFixture(t *testing.T, entry string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(entry)
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := f.Write(dbFixture()); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func writeArtifact(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestResolveSourceAcceptsEveryKind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := writeArtifact(t, dir, "prod.mv.db", dbFixture())
	archive := writeArtifact(t, dir, "prod.zip", archiveFixture(t, "prod.mv.db"))

	dbDir := t.TempDir()
	writeArtifact(t, dbDir, "old.mv.db", dbFixture())
	newestDB := writeArtifact(t, dbDir, "new.mv.db", dbFixture())
	touchOlder(t, filepath.Join(dbDir, "old.mv.db"))

	archiveDir := t.TempDir()
	writeArtifact(t, archiveDir, "old.zip", archiveFixture(t, "old.mv.db"))
	newestArchive := writeArtifact(t, archiveDir, "new.zip", archiveFixture(t, "new.mv.db"))
	touchOlder(t, filepath.Join(archiveDir, "old.zip"))

	tests := []struct {
		kind        string
		path        string
		wantPath    string
		wantArchive bool
	}{
		{"h2_db", db, db, false},
		{"h2_db_dir", dbDir, newestDB, false},
		{"h2_backup", archive, archive, true},
		{"h2_backup_dir", archiveDir, newestArchive, true},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			src, perr := resolveSource(ctx, tc.kind, tc.path)
			if perr != nil {
				t.Fatalf("resolveSource: %v", perr)
			}
			if src.path != tc.wantPath {
				t.Errorf("path = %s, want %s", src.path, tc.wantPath)
			}
			if src.archive != tc.wantArchive {
				t.Errorf("archive = %v, want %v", src.archive, tc.wantArchive)
			}
			if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
				t.Errorf("identity = %q / %d bytes, want a sha256 over a non-empty artifact",
					src.checksum, src.sizeBytes)
			}
		})
	}
}

// touchOlder moves a file's modification time back so directory ranking
// has an unambiguous newest.
func touchOlder(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	old := info.ModTime().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestResolveSourceRefusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	tests := []struct {
		name     string
		kind     string
		path     func(t *testing.T) string
		wantCode string
		wantText string
	}{
		{
			name: "unknown kind", kind: "h2_script",
			path:     func(t *testing.T) string { return writeArtifact(t, t.TempDir(), "x", dbFixture()) },
			wantCode: "unsupported_source", wantText: "h2_backup, h2_backup_dir",
		},
		{
			name: "missing file", kind: "h2_db",
			path:     func(t *testing.T) string { return filepath.Join(dir, "nope.mv.db") },
			wantCode: "source_not_found", wantText: "does not exist",
		},
		{
			name: "a directory given to a file kind", kind: "h2_db",
			path:     func(t *testing.T) string { return t.TempDir() },
			wantCode: "invalid_request", wantText: "use kind h2_db_dir",
		},
		{
			name: "empty database file", kind: "h2_db",
			path:     func(t *testing.T) string { return writeArtifact(t, t.TempDir(), "empty.mv.db", nil) },
			wantCode: "source_corrupt", wantText: "is empty",
		},
		{
			name: "gzip-compressed", kind: "h2_db",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "prod.mv.db.gz", []byte{0x1f, 0x8b, 0x08, 0x00})
			},
			wantCode: "unsupported_source", wantText: "gzip-compressed",
		},
		{
			name: "an archive handed to the file kind", kind: "h2_db",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "prod.zip", archiveFixture(t, "prod.mv.db"))
			},
			wantCode: "invalid_request", wantText: "use kind h2_backup",
		},
		{
			name: "a database file handed to the archive kind", kind: "h2_backup",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "prod.mv.db", dbFixture())
			},
			wantCode: "invalid_request", wantText: "use kind h2_db",
		},
		{
			name: "an H2 1.x database", kind: "h2_db",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "legacy.mv.db", []byte(mvStoreHeader("1")))
			},
			wantCode: "unsupported_source", wantText: "MVStore format 1",
		},
		{
			name: "a truncated archive", kind: "h2_backup",
			path: func(t *testing.T) string {
				whole := archiveFixture(t, "prod.mv.db")
				return writeArtifact(t, t.TempDir(), "cut.zip", whole[:len(whole)/2])
			},
			wantCode: "source_corrupt", wantText: "truncated",
		},
		{
			name: "an archive holding no database", kind: "h2_backup",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "notes.zip", archiveFixture(t, "README.txt"))
			},
			wantCode: "source_corrupt", wantText: "holds no .mv.db entry",
		},
		{
			name: "something that is not an archive at all", kind: "h2_backup",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "notes.txt", []byte("plain text\n"))
			},
			wantCode: "unsupported_source", wantText: "not a zip archive",
		},
		{
			name: "an empty directory", kind: "h2_db_dir",
			path:     func(t *testing.T) string { return t.TempDir() },
			wantCode: "source_not_found", wantText: "contains no files",
		},
		{
			name: "a directory holding nothing of this kind", kind: "h2_backup_dir",
			path: func(t *testing.T) string {
				d := t.TempDir()
				writeArtifact(t, d, "prod.mv.db", dbFixture())
				return d
			},
			wantCode: "source_not_found", wantText: "files of other kinds were passed over",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := resolveSource(ctx, tc.kind, tc.path(t))
			if perr == nil {
				t.Fatal("resolveSource accepted an artifact it must refuse")
			}
			if perr.Code != tc.wantCode {
				t.Errorf("code = %s, want %s (%s)", perr.Code, tc.wantCode, perr.Message)
			}
			if !strings.Contains(perr.Message, tc.wantText) {
				t.Errorf("message = %q, want it to contain %q", perr.Message, tc.wantText)
			}
		})
	}
}

// TestBackupTimezoneIsRefused pins that a declaration this adapter cannot
// honour is said out loud: no H2 artifact records when it was taken, so
// silently ignoring the parameter would leave the operator expecting a
// backup.created_at that never arrives.
func TestBackupTimezoneIsRefused(t *testing.T) {
	perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("rejectBackupTimezone = %v, want invalid_request", perr)
	}
	if rejectBackupTimezone(nil) != nil {
		t.Error("an absent backup_timezone must not be refused")
	}
}

// TestStorageFormatReadsAFormatNumberOnly covers what the header field is
// allowed to be. The value is quoted verbatim into a refusal telling the
// operator their file "states MVStore format %s", and at that point the
// file is unvetted input read on the drill host — so a field carrying
// anything but the number this reader claims to have found is dropped,
// and H2 stays the authority on a file this one cannot describe. Found by
// FuzzStorageFormat.
func TestStorageFormatReadsAFormatNumberOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		want   string
	}{
		"the format H2 writes":    {mvStoreHeader("3"), "3"},
		"the format 1.x wrote":    {mvStoreHeader("1"), "1"},
		"a field that is not one": {mvStoreHeader("3x"), ""},
		"an escape sequence":      {mvStoreHeader("\x1b[31mred\x07"), ""},
		"an empty field":          {mvStoreHeader(""), ""},
		"no header at all":        {"plain text", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := storageFormat([]byte(tc.header)); got != tc.want {
				t.Errorf("storageFormat = %q, want %q", got, tc.want)
			}
		})
	}
}
