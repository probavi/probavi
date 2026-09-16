package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInspectFindsTheDataDirectoryWhereOperatorsPutIt(t *testing.T) {
	for _, prefix := range []string{"", "data", "backup-2026-09-16/data"} {
		t.Run("at "+prefix, func(t *testing.T) {
			dir := writeCopy(t, prefix)
			src, perr := inspect(kindData, dir)
			if perr != nil {
				t.Fatalf("inspect: %+v", perr)
			}
			if want := filepath.Join(dir, prefix); src.path != want {
				t.Errorf("path = %s, want the data directory %s", src.path, want)
			}
			if !strings.HasPrefix(src.checksum, "sha256:") || src.sizeBytes == 0 {
				t.Errorf("identity = %s / %d", src.checksum, src.sizeBytes)
			}
		})
	}
}

func TestInspectRefusesTheShapesAnOperatorGetsWrong(t *testing.T) {
	file := filepath.Join(t.TempDir(), "copy.tar")
	if err := os.WriteFile(file, []byte("tar bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.tar")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	twoNodes := t.TempDir()
	for _, node := range []string{"node1", "node2"} {
		src := writeCopy(t, "")
		if err := os.Rename(src, filepath.Join(twoNodes, node)); err != nil {
			t.Fatal(err)
		}
	}
	halfCopy := t.TempDir()
	p := filepath.Join(halfCopy, confignodeProperties)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("iotdb_version=2.0.11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tooDeep := writeCopy(t, "a/b/c/d")

	for _, tc := range []struct {
		name, kind, path, code, says string
	}{
		{"a file named as a directory", kindData, file, "unsupported_source", "is a file"},
		{"a directory named as an archive", kindDataTar, t.TempDir(), "unsupported_source", "is a directory"},
		{"a missing path", kindData, filepath.Join(t.TempDir(), "gone"), "source_not_found", "does not exist"},
		{"an empty archive", kindDataTar, empty, "source_corrupt", "is empty"},
		{"a copy of one node's half", kindData, halfCopy, "unsupported_source", "holds no IoTDB data directory"},
		{"two nodes at once", kindData, twoNodes, "unsupported_source", "holds 2 IoTDB data directories"},
		{"a data directory buried too deep", kindData, tooDeep, "unsupported_source", "within 3 levels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := inspect(tc.kind, tc.path)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.says) {
				t.Errorf("got %+v, want %s saying %q", perr, tc.code, tc.says)
			}
		})
	}
}

// TestUnknownKindOutranksAMissingPath: a kind this adapter never had is
// unsupported whether or not anything sits at the path.
func TestUnknownKindOutranksAMissingPath(t *testing.T) {
	_, perr := inspect("iotdb_tsfile", filepath.Join(t.TempDir(), "gone"))
	if perr == nil || perr.Code != "unsupported_source" {
		t.Errorf("got %+v, want unsupported_source", perr)
	}
}

func TestArchiveCompressionComesFromTheBytes(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "copy.tar")
	plain := filepath.Join(dir, "copy.tar.gz")
	if err := os.WriteFile(gz, []byte{0x1f, 0x8b, 8, 0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("ustar bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{gz: true, plain: false} {
		src, perr := inspect(kindDataTar, path)
		if perr != nil || src.gzip != want {
			t.Errorf("%s: gzip = %v (%+v), want %v", filepath.Base(path), src, perr, want)
		}
	}
}

func TestTreeChecksumIsStableAndPositional(t *testing.T) {
	dir := writeCopy(t, "")
	first, size, perr := treeChecksum(dir)
	if perr != nil {
		t.Fatal(perr)
	}
	again, _, _ := treeChecksum(dir)
	if first != again {
		t.Error("the same tree hashed two ways")
	}
	if err := os.Rename(filepath.Join(dir, datanodeProperties), filepath.Join(dir, "datanode/system/moved")); err != nil {
		t.Fatal(err)
	}
	moved, movedSize, _ := treeChecksum(dir)
	if moved == first || movedSize != size {
		t.Errorf("moving a file kept the sum (%v) or changed the size (%d vs %d)", moved == first, movedSize, size)
	}
}

func TestUnreadableBytesAreReportedAsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := writeCopy(t, "")
	target := filepath.Join(dir, datanodeProperties)
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(target, 0o600); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
	if _, perr := inspect(kindData, dir); perr == nil || perr.Code != "source_unreadable" {
		t.Errorf("got %+v, want source_unreadable", perr)
	}
}

func TestACopyStillBeingWrittenIsRefused(t *testing.T) {
	dir := writeCopy(t, "")
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			case <-time.After(50 * time.Millisecond):
				if err := os.WriteFile(filepath.Join(dir, "datanode/wal.part"), []byte(strings.Repeat("x", i+1)), 0o600); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	perr := assertSettled(t.Context(), dir, 300*time.Millisecond)
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("keep the copy moving: %v", err)
	}
	if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "still being written") {
		t.Errorf("got %+v, want a refusal of a copy in motion", perr)
	}
}
