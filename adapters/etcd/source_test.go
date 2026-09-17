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

// bytesSum is the identity an artifact is recorded under, measured here
// rather than taken from the adapter that is under test.
func bytesSum(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// snapshotAt writes a snapshot dated exactly when, so a test can say which
// of two files is newer rather than hope.
func snapshotAt(t *testing.T, dir, name string, when time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("bbolt bytes of "+name), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestASnapshotIsIdentifiedByItsBytes(t *testing.T) {
	snap := writeAged(t, "member.snapshot.db", time.Hour)
	src, perr := resolveSource(context.Background(), "etcd_snapshot", snap)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	info, err := os.Stat(snap)
	if err != nil {
		t.Fatal(err)
	}
	if src.path != snap || src.checksum != bytesSum(t, snap) || src.sizeBytes != info.Size() {
		t.Errorf("resolved %+v, want %s identified by its own bytes", src, snap)
	}
}

// TestTheNewestSnapshotInADirectoryIsChosen pins the ranking the kind can
// have: a snapshot records revisions and raft terms, never a wall clock
// (source.go), so file time is all there is — and only a regular file is
// one, so a subdirectory of older backups or a link to the newest cannot
// become the drill's artifact.
func TestTheNewestSnapshotInADirectoryIsChosen(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	snapshotAt(t, dir, "monday.db", now.Add(-48*time.Hour))
	want := snapshotAt(t, dir, "tuesday.db", now.Add(-24*time.Hour))
	if err := os.Mkdir(filepath.Join(dir, "zz-archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(want, filepath.Join(dir, "zz-latest.db")); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "etcd_snapshot_dir", dir)
	if perr != nil {
		t.Fatalf("resolve: %+v", perr)
	}
	if src.path != want {
		t.Errorf("chose %s, want the newest regular file %s", src.path, want)
	}
}

// TestSnapshotsOfTheSameAgeBreakTowardTheLaterName: two files written in
// the same second are not ranked by chance, because a drill that restores
// a different artifact each run proves a different thing each run.
func TestSnapshotsOfTheSameAgeBreakTowardTheLaterName(t *testing.T) {
	dir := t.TempDir()
	same := time.Now().Add(-time.Hour)
	snapshotAt(t, dir, "a.db", same)
	want := snapshotAt(t, dir, "b.db", same)
	for range 3 {
		src, perr := resolveSource(context.Background(), "etcd_snapshot_dir", dir)
		if perr != nil {
			t.Fatalf("resolve: %+v", perr)
		}
		if src.path != want {
			t.Fatalf("chose %s, want %s every time", src.path, want)
		}
	}
}

func TestSourceRefusalsNameWhatWasWrong(t *testing.T) {
	dir := t.TempDir()
	snap := snapshotAt(t, dir, "member.db", time.Now().Add(-time.Hour))
	empty := t.TempDir()
	dirsOnly := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirsOnly, "yesterday"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		kind, path, code, message string
	}{
		"an unknown source kind": {
			"etcd_backup", snap, "unsupported_source", "etcd_snapshot_dir",
		},
		"a snapshot that does not exist": {
			"etcd_snapshot", filepath.Join(dir, "gone.db"), "source_not_found", "does not exist",
		},
		"a directory for the file kind": {
			"etcd_snapshot", dir, "invalid_request", "use kind etcd_snapshot_dir",
		},
		"a path beneath a file": {
			"etcd_snapshot", filepath.Join(snap, "member.db"), "source_unreadable", "stat backup source",
		},
		"a directory that does not exist": {
			"etcd_snapshot_dir", filepath.Join(dir, "gone"), "source_not_found", "does not exist",
		},
		"a file for the directory kind": {
			"etcd_snapshot_dir", snap, "source_unreadable", "read backup directory",
		},
		"a directory with nothing in it": {
			"etcd_snapshot_dir", empty, "source_not_found", "contains no files",
		},
		"a directory holding only directories": {
			"etcd_snapshot_dir", dirsOnly, "source_not_found", "contains no files",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := resolveSource(context.Background(), tc.kind, tc.path)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.message)
			}
		})
	}
}

// TestBytesTheHostCannotReadAreUnreadable covers the two ways the hash
// pass fails: the file will not open, and it opens but will not read.
// Neither is the backup's fault, and the code says so.
func TestBytesTheHostCannotReadAreUnreadable(t *testing.T) {
	t.Run("a file the host may not open", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		snap := writeAged(t, "member.db", time.Hour)
		if err := os.Chmod(snap, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(snap, 0o600); err != nil {
				t.Errorf("restore the mode: %v", err)
			}
		})
		_, perr := resolveSource(context.Background(), "etcd_snapshot", snap)
		if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "open backup source") {
			t.Errorf("got %+v, want source_unreadable", perr)
		}
	})
	t.Run("bytes that will not stream", func(t *testing.T) {
		_, perr := fileChecksum(t.TempDir())
		if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, "read backup source") {
			t.Errorf("got %+v, want source_unreadable", perr)
		}
	})
}
