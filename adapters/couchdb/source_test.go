package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backupFixture is a couchbackup-shaped artifact: the header line the tool
// writes, then one JSON array of documents per line — the exact shape
// measured on 2.11.19.
func backupFixture(batches int) []byte {
	var b strings.Builder
	b.WriteString(`{"name":"@cloudant/couchbackup","version":"2.11.19","mode":"full","attachments":false}` + "\n")
	for i := range batches {
		b.WriteString(`[{"_id":"doc-` + string(rune('a'+i)) + `","_rev":"1-abc","n":1}]` + "\n")
	}
	return []byte(b.String())
}

// tarFixture is a file carrying the ustar magic where a tar has it.
func tarFixture() []byte {
	buf := make([]byte, 1024)
	copy(buf[257:], []byte("ustar\x0000"))
	copy(buf, []byte("_dbs.couch"))
	return buf
}

func writeArtifact(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// dataDirFixture builds a data directory shaped like CouchDB's own: the
// registry file the engine reads at startup, and a shard beneath it.
func dataDirFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeArtifact(t, dir, registryFile, []byte("registry bytes\n"))
	writeArtifact(t, dir, "_nodes.couch", []byte("nodes bytes\n"))
	shard := filepath.Join(dir, "shards", "00000000-7fffffff")
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatalf("mkdir shards: %v", err)
	}
	writeArtifact(t, shard, "orders.1788098722.couch", []byte("shard bytes\n"))
	return dir
}

func TestResolveSourceAcceptsEveryKind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backup := writeArtifact(t, dir, "nightly.jsonl", backupFixture(3))
	tarball := writeArtifact(t, dir, "data.tar", tarFixture())
	data := dataDirFixture(t)

	backupDir := t.TempDir()
	writeArtifact(t, backupDir, "old.jsonl", backupFixture(1))
	newest := writeArtifact(t, backupDir, "new.jsonl", backupFixture(2))
	touchOlder(t, filepath.Join(backupDir, "old.jsonl"))

	tests := []struct {
		kind        string
		path        string
		wantPath    string
		wantForm    sourceForm
		wantBatches int
	}{
		{"couchbackup", backup, backup, formBackup, 3},
		{"couchbackup_dir", backupDir, newest, formBackup, 2},
		{"couchdb_data", data, data, formDataDir, 0},
		{"couchdb_data_tar", tarball, tarball, formDataTar, 0},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			src, perr := resolveSource(ctx, tc.kind, tc.path)
			if perr != nil {
				t.Fatalf("resolveSource: %v", perr)
			}
			if src.path != tc.wantPath || src.form != tc.wantForm {
				t.Errorf("resolved %s as form %d, want %s as form %d", src.path, src.form, tc.wantPath, tc.wantForm)
			}
			if src.batches != tc.wantBatches {
				t.Errorf("batches = %d, want %d", src.batches, tc.wantBatches)
			}
			if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
				t.Errorf("identity = %q / %d bytes, want a sha256 over a non-empty artifact",
					src.checksum, src.sizeBytes)
			}
		})
	}
}

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

// TestTheBatchCountIsTheOnlyCompletenessCheckTheFormatAllows pins what the
// scan can and cannot see. couchbackup writes nothing at the end of a
// file, so a backup cut between two lines is a shorter backup as far as
// any reader can tell — but one cut inside a line has no final newline,
// and that is refused before a byte moves.
func TestTheBatchCountIsTheOnlyCompletenessCheckTheFormatAllows(t *testing.T) {
	ctx := context.Background()
	whole := backupFixture(4)

	full, perr := resolveSource(ctx, "couchbackup", writeArtifact(t, t.TempDir(), "b.jsonl", whole))
	if perr != nil {
		t.Fatalf("resolveSource: %v", perr)
	}
	if full.batches != 4 {
		t.Fatalf("batches = %d, want 4", full.batches)
	}

	// Cut inside the last line: refused.
	torn := whole[:len(whole)-8]
	_, perr = resolveSource(ctx, "couchbackup", writeArtifact(t, t.TempDir(), "torn.jsonl", torn))
	if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "cut mid-batch") {
		t.Fatalf("a backup cut inside a batch = %v, want source_corrupt naming the mid-batch cut", perr)
	}

	// Cut exactly at a line boundary: accepted, with a smaller count. This
	// is the limit, and the adapter must not pretend otherwise — the
	// README says so and the drill's own row-count check is what closes it.
	lines := strings.SplitAfter(string(whole), "\n")
	shorter := strings.Join(lines[:3], "")
	short, perr := resolveSource(ctx, "couchbackup", writeArtifact(t, t.TempDir(), "short.jsonl", []byte(shorter)))
	if perr != nil {
		t.Fatalf("a backup cut at a line boundary must resolve, not fail: %v", perr)
	}
	if short.batches != 2 {
		t.Errorf("batches = %d, want 2 — the scan counts what is there, not what should be", short.batches)
	}
}

func TestResolveSourceRefusals(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		kind     string
		path     func(t *testing.T) string
		wantCode string
		wantText string
	}{
		{
			name: "unknown kind", kind: "couchdb_dump",
			path:     func(t *testing.T) string { return writeArtifact(t, t.TempDir(), "x", backupFixture(1)) },
			wantCode: "unsupported_source", wantText: "couchbackup, couchbackup_dir",
		},
		{
			name: "missing file", kind: "couchbackup",
			path:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope.jsonl") },
			wantCode: "source_not_found", wantText: "does not exist",
		},
		{
			name: "a directory given to the file kind", kind: "couchbackup",
			path:     func(t *testing.T) string { return t.TempDir() },
			wantCode: "invalid_request", wantText: "use kind couchbackup_dir",
		},
		{
			name: "a file given to the data-directory kind", kind: "couchdb_data",
			path:     func(t *testing.T) string { return writeArtifact(t, t.TempDir(), "b.jsonl", backupFixture(1)) },
			wantCode: "invalid_request", wantText: "use kind couchbackup",
		},
		{
			name: "empty file", kind: "couchbackup",
			path:     func(t *testing.T) string { return writeArtifact(t, t.TempDir(), "empty.jsonl", nil) },
			wantCode: "source_corrupt", wantText: "is empty",
		},
		{
			name: "gzip-compressed", kind: "couchbackup",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "b.jsonl.gz", []byte{0x1f, 0x8b, 0x08, 0x00})
			},
			wantCode: "unsupported_source", wantText: "gzip-compressed",
		},
		{
			name: "not a couchbackup file", kind: "couchbackup",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "notes.txt", []byte("{\"some\":\"json\"}\n"))
			},
			wantCode: "unsupported_source", wantText: "couchbackup's header line",
		},
		{
			name: "a header line and nothing after it", kind: "couchbackup",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "b.jsonl", backupFixture(0))
			},
			wantCode: "source_corrupt", wantText: "no document batches",
		},
		{
			name: "a data directory without the registry", kind: "couchdb_data",
			path: func(t *testing.T) string {
				d := t.TempDir()
				writeArtifact(t, d, "_nodes.couch", []byte("nodes\n"))
				return d
			},
			wantCode: "source_corrupt", wantText: "holds no " + registryFile,
		},
		{
			name: "a couchbackup file handed to the tar kind", kind: "couchdb_data_tar",
			path: func(t *testing.T) string {
				return writeArtifact(t, t.TempDir(), "b.jsonl", backupFixture(1))
			},
			wantCode: "invalid_request", wantText: "use kind couchbackup",
		},
		{
			name: "an empty directory", kind: "couchbackup_dir",
			path:     func(t *testing.T) string { return t.TempDir() },
			wantCode: "source_not_found", wantText: "contains no files",
		},
		{
			name: "a directory holding nothing of this kind", kind: "couchbackup_dir",
			path: func(t *testing.T) string {
				d := t.TempDir()
				writeArtifact(t, d, "notes.txt", []byte("not a backup\n"))
				return d
			},
			wantCode: "source_not_found", wantText: "without its header line",
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
// honour is said out loud rather than ignored.
func TestBackupTimezoneIsRefused(t *testing.T) {
	if perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"}); perr == nil ||
		perr.Code != "invalid_request" {
		t.Fatalf("rejectBackupTimezone = %v, want invalid_request", perr)
	}
	if rejectBackupTimezone(nil) != nil {
		t.Error("an absent backup_timezone must not be refused")
	}
}
