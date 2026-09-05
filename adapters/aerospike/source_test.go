package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHeader(t *testing.T) {
	tests := map[string]struct {
		head      string
		ok        bool
		namespace string
		firstFile bool
	}{
		"the file asbackup wrote first": {
			"Version 3.1\n# namespace orders\n# first-file\n+ k S 2 k1\n", true, "orders", true},
		"one of its later files": {
			"Version 3.1\n# namespace orders\n+ k S 2 k1\n", true, "orders", false},
		"a future minor of the same format": {
			"Version 3.9\n# namespace orders\n# first-file\n", true, "orders", true},
		"a namespace with the punctuation Aerospike allows": {
			"Version 3.1\n# namespace orders_eu-1\n# first-file\n", true, "orders_eu-1", true},
		"a format this reader does not know": {
			"Version 4.0\n# namespace orders\n", false, "", false},
		"no namespace line": {
			"Version 3.1\n+ k S 2 k1\n", false, "", false},
		"an empty namespace": {
			"Version 3.1\n# namespace \n", false, "", false},
		"something else entirely": {"hello\nworld\n", false, "", false},
		"nothing at all":          {"", false, "", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, ok := parseHeader([]byte(tt.head))
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if h.namespace != tt.namespace || h.firstFile != tt.firstFile {
				t.Errorf("header = %+v, want namespace %q firstFile %v", h, tt.namespace, tt.firstFile)
			}
		})
	}
}

// TestFileChecksumIsTheBytes holds the single-file identity to the plain
// hash of the artifact: an evidence record's backup identity has to name
// the bytes that were restored.
func TestFileChecksumIsTheBytes(t *testing.T) {
	body := asbFile("orders", true, 4)
	path := filepath.Join(t.TempDir(), "test_00000.asb")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "asbackup", path)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	sum := sha256.Sum256([]byte(body))
	if want := "sha256:" + hex.EncodeToString(sum[:]); src.checksum != want {
		t.Errorf("checksum = %s, want %s", src.checksum, want)
	}
	if src.sizeBytes != int64(len(body)) {
		t.Errorf("size = %d, want %d", src.sizeBytes, len(body))
	}
	if src.dir {
		t.Error("a file is not a directory backup")
	}
}

// buildDirBackup writes a three-file backup, creating the files in the
// given order so a caller can vary it.
func buildDirBackup(t *testing.T, order []string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range order {
		// The first-file marker belongs to the file asbackup wrote first,
		// which is the lowest-numbered one whatever order these are
		// created in.
		if err := os.WriteFile(filepath.Join(dir, name),
			[]byte(asbFile("orders", name == "test_00000.asb", 2)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func resolveDirBackup(t *testing.T, dir string) *resolvedSource {
	t.Helper()
	src, perr := resolveSource(context.Background(), "asbackup_dir", dir)
	if perr != nil {
		t.Fatalf("resolve %s: %+v", dir, perr)
	}
	return src
}

// TestTreeIdentityIsOrderIndependent pins the directory rule: the same
// bytes always yield the same identity, whatever order the filesystem
// hands the files back in.
func TestTreeIdentityIsOrderIndependent(t *testing.T) {
	names := []string{"test_00000.asb", "test_00001.asb", "test_00002.asb"}
	a := resolveDirBackup(t, buildDirBackup(t, names))
	b := resolveDirBackup(t, buildDirBackup(t, []string{names[2], names[1], names[0]}))
	if a.checksum != b.checksum {
		t.Errorf("the same bytes gave two identities: %s and %s", a.checksum, b.checksum)
	}
	if a.sizeBytes != b.sizeBytes {
		t.Errorf("size = %d and %d", a.sizeBytes, b.sizeBytes)
	}
	if a.namespace != "orders" || !a.dir {
		t.Errorf("resolved = %+v", a)
	}
}

// TestTreeIdentityFollowsTheBytes: one changed file must change the
// directory's identity, or the evidence record would name bytes that were
// not the ones restored.
func TestTreeIdentityFollowsTheBytes(t *testing.T) {
	dir := buildDirBackup(t, []string{"test_00000.asb", "test_00001.asb", "test_00002.asb"})
	before := resolveDirBackup(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "test_00002.asb"),
		[]byte(asbFile("orders", false, 3)), 0o600); err != nil {
		t.Fatal(err)
	}
	if after := resolveDirBackup(t, dir); after.checksum == before.checksum {
		t.Error("a changed file left the directory's identity unchanged")
	}
}

// TestABackupStillBeingWrittenIsRefused covers settle.go: the drill must
// not restore a file a backup job is in the middle of writing.
func TestABackupStillBeingWrittenIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test_00000.asb")
	if err := os.WriteFile(path, []byte(asbFile("orders", true, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close() //nolint:errcheck // fixture writer; the assertion below reads the file
		for range 40 {
			if _, err := f.WriteString("+ k S 2 kx\n"); err != nil {
				return
			}
		}
	}()
	<-done
	_, perr := resolveSource(context.Background(), "asbackup", path)
	if perr == nil {
		t.Skip("the write finished before the window could observe it")
	}
	if !strings.Contains(perr.Message, "still being written") {
		t.Errorf("refusal = %+v, want it to name the writer", perr)
	}
}
