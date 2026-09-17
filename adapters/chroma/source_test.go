package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDir lays out a persistence directory: the metadata database with a
// real SQLite header, and one segment directory beside it.
// segmentDirName is the fixture's vector segment, named the way Chroma
// names one: the segment's own uuid.
const segmentDirName = "e41808a1-af22-40bc-9ebd-31fc8e917b52"

func writeDir(t *testing.T, extra map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sqliteFile), append(sqliteMagic, 'x'), 0o600); err != nil {
		t.Fatalf("write %s: %v", sqliteFile, err)
	}
	seg := filepath.Join(dir, segmentDirName)
	if err := os.MkdirAll(seg, 0o750); err != nil {
		t.Fatalf("mkdir segment: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seg, "header.bin"), []byte("hnsw"), 0o600); err != nil {
		t.Fatalf("write segment file: %v", err)
	}
	for name, body := range extra {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestInspectAcceptsAPersistenceDirectory(t *testing.T) {
	src, perr := inspect(kindData, writeDir(t, nil))
	if perr != nil {
		t.Fatalf("inspect: %+v", perr)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || len(src.checksum) != 71 {
		t.Errorf("checksum %q is not a sha256 reference", src.checksum)
	}
	if src.sizeBytes == 0 {
		t.Error("size_bytes is zero for a directory holding files")
	}
}

// TestInspectRefusesTheShapesAnOperatorGetsWrong keeps every host-side
// refusal on the code §5 gives it, because the code is what an evidence
// record carries: source_corrupt says the backup is bad, unsupported_source
// says the config is.
func TestInspectRefusesTheShapesAnOperatorGetsWrong(t *testing.T) {
	dir := writeDir(t, nil)
	file := filepath.Join(t.TempDir(), "backup.tar")
	if err := os.WriteFile(file, []byte("not really a tar"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	empty := t.TempDir()
	wrongMagic := t.TempDir()
	if err := os.WriteFile(filepath.Join(wrongMagic, sqliteFile), []byte("this is not sqlite at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	short := t.TempDir()
	if err := os.WriteFile(filepath.Join(short, sqliteFile), []byte("SQL"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	emptyTar := filepath.Join(t.TempDir(), "empty.tar")
	if err := os.WriteFile(emptyTar, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name, kind, path, wantCode, wantIn string
	}{
		{"an unknown kind", "chroma_snapshot", dir, "unsupported_source", "unsupported source kind"},
		{"a missing path", kindData, filepath.Join(dir, "absent"), "source_not_found", "does not exist"},
		{"a file where the directory kind was asked for", kindData, file, "unsupported_source", kindDataTar},
		{"a directory where the archive kind was asked for", kindDataTar, dir, "unsupported_source", kindData},
		{"a directory with no metadata database", kindData, empty, "unsupported_source", sqliteFile},
		{"a metadata database that is not SQLite", kindData, wrongMagic, "source_corrupt", "SQLite file header"},
		{"a metadata database shorter than its header", kindData, short, "source_corrupt", "shorter than"},
		{"an empty archive", kindDataTar, emptyTar, "source_corrupt", "is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := inspect(tc.kind, tc.path)
			if perr == nil {
				t.Fatal("inspect accepted it")
			}
			if perr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", perr.Code, tc.wantCode, perr.Message)
			}
			if !strings.Contains(perr.Message, tc.wantIn) {
				t.Errorf("message %q does not mention %q", perr.Message, tc.wantIn)
			}
		})
	}
}

// TestUnknownKindOutranksAMissingPath pins the order the §10 conformance
// suite checks: it asks for an unsupported kind at a path that does not
// exist, and the answer must be about the kind. An operator whose config
// names a kind this adapter never had should not be sent looking for a
// file.
func TestUnknownKindOutranksAMissingPath(t *testing.T) {
	_, perr := inspect("chroma_snapshot", filepath.Join(t.TempDir(), "nowhere"))
	if perr == nil || perr.Code != "unsupported_source" {
		t.Fatalf("got %+v, want unsupported_source", perr)
	}
}

// TestATornCopyIsRefused is the live-copy fence, shaped by measurement
// rather than by the sibling adapters' habits: Chroma's SQLite runs in
// rollback-journal mode (`PRAGMA journal_mode` reports `delete`, header
// bytes 18 and 19 are both 1), so the file that betrays a copy taken
// mid-write is `-journal`.
func TestATornCopyIsRefused(t *testing.T) {
	dir := writeDir(t, map[string][]byte{rollbackJournal: []byte("mid-write")})
	_, perr := inspect(kindData, dir)
	if perr == nil {
		t.Fatal("inspect accepted a directory carrying a rollback journal")
	}
	if perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "stop the server") {
		t.Errorf("got %+v, want source_corrupt naming the remedy", perr)
	}
}

// TestAWalDatabaseIsNotJudged separates the two things a sidecar can mean.
// A `-wal` pair is not a torn copy: it is a database in a journal mode this
// adapter has not measured Chroma using, holding writes the main file does
// not. Calling that `source_corrupt` would blame a backup for a
// configuration, so it is unsupported_source instead.
func TestAWalDatabaseIsNotJudged(t *testing.T) {
	for _, sidecar := range walSidecars {
		t.Run(sidecar, func(t *testing.T) {
			dir := writeDir(t, map[string][]byte{sidecar: []byte("wal")})
			_, perr := inspect(kindData, dir)
			if perr == nil {
				t.Fatal("inspect accepted a WAL-mode database")
			}
			if perr.Code != "unsupported_source" || !strings.Contains(perr.Message, "WAL mode") {
				t.Errorf("got %+v, want unsupported_source naming WAL mode", perr)
			}
		})
	}
}

// TestArchiveCompressionComesFromTheBytes keeps the name out of the
// decision: `tar czf backup.tar` writes gzip under a name that says
// otherwise, and a .gz that is not gzip is an operator's mistake worth
// reporting from the engine rather than assuming from the suffix.
func TestArchiveCompressionComesFromTheBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		head  []byte
		wantZ bool
	}{
		{"gzip magic under a .tar name", []byte{0x1f, 0x8b, 0x08, 0x00, 'x'}, true},
		{"plain tar", []byte("ustar\x00rest of it"), false},
		{"one byte", []byte{0x1f}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "backup.tar.gz")
			if err := os.WriteFile(path, tc.head, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			src, perr := inspect(kindDataTar, path)
			if perr != nil {
				t.Fatalf("inspect: %+v", perr)
			}
			if src.gzip != tc.wantZ {
				t.Errorf("gzip = %v, want %v", src.gzip, tc.wantZ)
			}
			if src.sizeBytes != int64(len(tc.head)) {
				t.Errorf("size_bytes = %d, want %d", src.sizeBytes, len(tc.head))
			}
		})
	}
}

// TestTreeChecksumIsStableAndPositional pins the documented hashing rule:
// the same tree always hashes the same, and moving a file changes the sum
// even when every byte is still there.
func TestTreeChecksumIsStableAndPositional(t *testing.T) {
	dir := writeDir(t, nil)
	first, _, perr := treeChecksum(dir)
	if perr != nil {
		t.Fatalf("treeChecksum: %+v", perr)
	}
	again, _, _ := treeChecksum(dir)
	if first != again {
		t.Errorf("treeChecksum is not stable: %s then %s", first, again)
	}
	seg := filepath.Join(dir, segmentDirName)
	if err := os.Rename(filepath.Join(seg, "header.bin"), filepath.Join(dir, "header.bin")); err != nil {
		t.Fatalf("move file: %v", err)
	}
	moved, _, _ := treeChecksum(dir)
	if moved == first {
		t.Error("moving a file did not change the tree checksum")
	}
}

func TestTreeChecksumRefusesAnEmptyDirectory(t *testing.T) {
	if _, _, perr := treeChecksum(t.TempDir()); perr == nil || perr.Code != "source_not_found" {
		t.Fatalf("got %+v, want source_not_found", perr)
	}
}

func TestLooksLikeSegmentDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"e41808a1-af22-40bc-9ebd-31fc8e917b52", true},
		{"E41808A1-AF22-40BC-9EBD-31FC8E917B52", true},
		{"e41808a1af2240bc9ebd31fc8e917b52", false},
		{"data", false},
		{"e41808a1-af22-40bc-9ebd-31fc8e917b5z", false},
	} {
		if got := looksLikeSegmentDir(tc.name); got != tc.want {
			t.Errorf("looksLikeSegmentDir(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// denyRead makes a path unreadable and restores it when the test ends, so
// t.TempDir can still clean up after itself.
func denyRead(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode()); err != nil {
			t.Errorf("restore mode on %s: %v", path, err)
		}
	})
}

// wantUnreadable asserts the one verdict every permission failure must
// reach.
func wantUnreadable(t *testing.T, kind, path string) {
	t.Helper()
	if _, perr := inspect(kind, path); perr == nil || perr.Code != "source_unreadable" {
		t.Fatalf("got %+v, want source_unreadable", perr)
	}
}

// TestUnreadableBytesAreReportedAsUnreadable keeps the permission case off
// the corrupt verdict. A backup nobody can read is not a backup that failed
// to restore, and an evidence record saying source_corrupt would send an
// operator hunting a damaged file that is fine.
func TestUnreadableBytesAreReportedAsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny this process")
	}
	t.Run("the metadata database", func(t *testing.T) {
		dir := writeDir(t, nil)
		denyRead(t, filepath.Join(dir, sqliteFile))
		wantUnreadable(t, kindData, dir)
	})
	t.Run("a segment file", func(t *testing.T) {
		dir := writeDir(t, nil)
		denyRead(t, filepath.Join(dir, segmentDirName, "header.bin"))
		wantUnreadable(t, kindData, dir)
	})
	t.Run("an archive", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "backup.tar")
		if err := os.WriteFile(path, []byte("ustar\x00"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		denyRead(t, path)
		wantUnreadable(t, kindDataTar, path)
	})
	t.Run("a directory that cannot be walked", func(t *testing.T) {
		dir := writeDir(t, nil)
		denyRead(t, filepath.Join(dir, segmentDirName))
		wantUnreadable(t, kindData, dir)
	})
}

// wantCode fails the test unless the refusal carries the code and the
// words that say which read or which shape was wrong.
func wantCode(t *testing.T, perr *protoError, code, message string) {
	t.Helper()
	if perr == nil || perr.Code != code || !strings.Contains(perr.Message, message) {
		t.Errorf("got %+v, want %s mentioning %q", perr, code, message)
	}
}

// TestWhatTheHostCannotReadIsUnreadable: the host stats and hashes the
// artifact before anything moves. A read that fails is the host's failure,
// and a metadata database that is not a regular file is the artifact's —
// each says which.
func TestWhatTheHostCannotReadIsUnreadable(t *testing.T) {
	t.Run("a path beneath a file", func(t *testing.T) {
		dir := writeDir(t, nil)
		beneath := filepath.Join(dir, sqliteFile, "chroma.sqlite3")
		for _, kind := range []string{kindData, kindDataTar} {
			_, perr := inspect(kind, beneath)
			wantCode(t, perr, "source_unreadable", "stat backup source")
		}
	})
	t.Run("a metadata database that is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, sqliteFile), 0o755); err != nil {
			t.Fatal(err)
		}
		_, perr := inspect(kindData, dir)
		wantCode(t, perr, "source_corrupt", "not a regular file")
	})
	t.Run("a segment file the host may not open", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		dir := writeDir(t, map[string][]byte{
			filepath.Join(segmentDirName, "data_level0.bin"): []byte("index bytes"),
		})
		denyRead(t, filepath.Join(dir, segmentDirName, "data_level0.bin"))
		_, perr := inspect(kindData, dir)
		wantCode(t, perr, "source_unreadable", "")
	})
	t.Run("an archive the host may not open", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		archive := filepath.Join(t.TempDir(), "chroma.tar")
		if err := os.WriteFile(archive, []byte("tar bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		denyRead(t, archive)
		_, perr := inspect(kindDataTar, archive)
		wantCode(t, perr, "source_unreadable", "")
	})
}

// TestASegmentIdIsReadCharacterByCharacter: the name is a diagnostic only,
// so it is recognised exactly rather than approximately — every hyphen in
// its place and every other character a hex digit.
func TestASegmentIdIsReadCharacterByCharacter(t *testing.T) {
	for name, want := range map[string]bool{
		segmentDirName:                         true,
		"E41808A1-AF22-40BC-9EBD-31FC8E917B52": true,
		"e41808a1_af22-40bc-9ebd-31fc8e917b52": false,
		"e41808a1-af22-40bc-9ebd-31fc8e917b5g": false,
		"e41808a1-af22-40bc-9ebd-31fc8e917b5":  false,
		"hnsw":                                 false,
	} {
		if got := looksLikeSegmentDir(name); got != want {
			t.Errorf("looksLikeSegmentDir(%q) = %v, want %v", name, got, want)
		}
	}
}
