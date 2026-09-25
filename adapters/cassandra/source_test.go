package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveSourceUnknownKind(t *testing.T) {
	_, perr := resolveSource("cassandra_backup", "/nowhere", nil)
	if perr == nil || perr.Code != "unsupported_source" {
		t.Fatalf("perr = %+v, want unsupported_source", perr)
	}
	for _, kind := range []string{"cassandra_snapshot_tar", "cassandra_snapshot", "cassandra_snapshot_dir"} {
		if !strings.Contains(perr.Message, kind) {
			t.Errorf("message %q does not list %s", perr.Message, kind)
		}
	}
}

func TestResolveTar(t *testing.T) {
	t.Run("a walkable archive resolves with its own census", func(t *testing.T) {
		root := writeTree(t, t.TempDir(), "probavi.orders", "probavi.meta")
		path := treeToTar(t, root, filepath.Join(t.TempDir(), "snap.tar.gz"), "snapname", true)
		src, perr := resolveSource("cassandra_snapshot_tar", path, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if !src.tarball || !strings.HasPrefix(src.checksum, "sha256:") ||
			len(src.census.tables) != 2 || src.census.maxCreatedMs != fixtureCreatedMs {
			t.Errorf("resolved = %+v census=%+v", src, src.census)
		}
	})

	t.Run("an opaque archive resolves with nothing claimed", func(t *testing.T) {
		// Metadata is a bonus: the sandbox extraction is the authority on
		// whether the bytes unpack.
		path := filepath.Join(t.TempDir(), "opaque.bin")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("cassandra_snapshot_tar", path, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if len(src.census.tables) != 0 || src.census.maxCreatedMs != 0 {
			t.Errorf("census = %+v, want nothing claimed", src.census)
		}
	})

}

func TestResolveTarRefusals(t *testing.T) {
	refusals := []struct {
		name     string
		prepare  func(t *testing.T, dir string) string
		wantCode string
		wantWord string
	}{
		{"missing file", func(t *testing.T, dir string) string {
			return filepath.Join(dir, "gone.tar")
		}, "source_not_found", ""},
		{"a directory names the directory kinds", func(t *testing.T, dir string) string {
			return dir
		}, "invalid_request", "cassandra_snapshot_dir"},
		{"a tar of a raw data-directory copy", func(t *testing.T, dir string) string {
			root := filepath.Join(dir, "data")
			writeTable(t, root, "probavi", "orders", tableFixture{liveMarker: "snapshots"})
			return treeToTar(t, root, filepath.Join(dir, "datadir.tar"), "", false)
		}, "unsupported_source", "nodetool snapshot"},
		{"a digest mismatch inside the archive", func(t *testing.T, dir string) string {
			root := filepath.Join(dir, "snap")
			writeTable(t, root, "probavi", "orders", tableFixture{corruptData: true})
			return treeToTar(t, root, filepath.Join(dir, "snap.tar"), "", false)
		}, "source_corrupt", "Digest.crc32"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.prepare(t, t.TempDir())
			_, perr := resolveSource("cassandra_snapshot_tar", path, nil)
			if perr == nil || perr.Code != tc.wantCode {
				t.Fatalf("perr = %+v, want %s", perr, tc.wantCode)
			}
			if tc.wantWord != "" && !strings.Contains(perr.Message, tc.wantWord) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantWord)
			}
		})
	}
}

func TestResolveTree(t *testing.T) {
	t.Run("a healthy tree resolves with a tree checksum", func(t *testing.T) {
		root := writeTree(t, t.TempDir(), "probavi.orders")
		src, perr := resolveSource("cassandra_snapshot", root, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.tarball || !strings.HasPrefix(src.checksum, "sha256:") ||
			len(src.census.tables) != 1 || src.sizeBytes == 0 {
			t.Errorf("resolved = %+v census=%+v", src, src.census)
		}
	})

	t.Run("the tree checksum sees content changes", func(t *testing.T) {
		root := writeTree(t, t.TempDir(), "probavi.orders")
		before, perr := resolveSource("cassandra_snapshot", root, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		// Change a component the digests do not cover, so only the tree
		// hash notices.
		if err := os.WriteFile(filepath.Join(root, "probavi", "orders", "nb-1-big-Summary.db"),
			[]byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, perr := resolveSource("cassandra_snapshot", root, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if before.checksum == after.checksum {
			t.Error("checksum unchanged after a component changed")
		}
	})

	t.Run("a file needs the tar kind", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "snap.tar")
		if err := os.WriteFile(path, []byte("tar bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("cassandra_snapshot", path, nil)
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "cassandra_snapshot_tar") {
			t.Fatalf("perr = %+v, want invalid_request naming the tar kind", perr)
		}
	})
}

func TestChooseTreeIn(t *testing.T) {
	older := "2026-08-10T00:00:00.000Z"
	t.Run("the tree claiming the newest instant wins over a fresher mtime", func(t *testing.T) {
		base := t.TempDir()
		writeTable(t, filepath.Join(base, "monday"), "probavi", "orders", tableFixture{createdAt: older})
		newest := filepath.Join(base, "saturday")
		writeTable(t, newest, "probavi", "orders", tableFixture{})
		// The decoy: the newest tree's directory looks stale on disk.
		past := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(newest, past, past); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("cassandra_snapshot_dir", base, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newest {
			t.Errorf("picked %s, want the tree whose own manifests claim the newest instant", src.path)
		}
	})

	t.Run("a dated tree beats an undated directory", func(t *testing.T) {
		base := t.TempDir()
		dated := filepath.Join(base, "a-dated")
		writeTable(t, dated, "probavi", "orders", tableFixture{})
		if err := os.MkdirAll(filepath.Join(base, "z-undated"), 0o755); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("cassandra_snapshot_dir", base, nil)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != dated {
			t.Errorf("picked %s, want %s", src.path, dated)
		}
	})

}

// TestChooseTreeInRefusesTheBrokenWinner pins the not-a-filter side of
// the ranking: the chosen candidate faces every gate by name.
func TestChooseTreeInRefusesTheBrokenWinner(t *testing.T) {
	t.Run("a winning broken candidate is refused, not passed over", func(t *testing.T) {
		base := t.TempDir()
		// Neither candidate can be dated (one is a live copy, the other
		// has no manifest), so mtime decides — and the live copy is newer.
		writeTable(t, filepath.Join(base, "old"), "probavi", "orders",
			tableFixture{noManifest: true})
		live := filepath.Join(base, "new-live")
		writeTable(t, live, "probavi", "orders", tableFixture{liveMarker: "snapshots"})
		old := filepath.Join(base, "old")
		past := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(old, past, past); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("cassandra_snapshot_dir", base, nil)
		if perr == nil || perr.Code != "unsupported_source" ||
			!strings.Contains(perr.Message, "nodetool snapshot") {
			t.Fatalf("perr = %+v, want the chosen live copy refused by name", perr)
		}
	})

}

func TestChooseTreeInEdgeCases(t *testing.T) {
	t.Run("a directory with no subdirectories says so", func(t *testing.T) {
		base := t.TempDir()
		if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("cassandra_snapshot_dir", base, nil)
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})

	t.Run("a missing directory is source_not_found", func(t *testing.T) {
		_, perr := resolveSource("cassandra_snapshot_dir", filepath.Join(t.TempDir(), "gone"), nil)
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})
}

func TestFormatCreatedAt(t *testing.T) {
	if formatCreatedAt(0) != nil {
		t.Error("formatCreatedAt(0) != nil")
	}
	got := formatCreatedAt(fixtureCreatedMs)
	if got == nil || *got != fixtureCreatedAt {
		t.Errorf("formatCreatedAt = %v, want the manifest's own instant", got)
	}
}

func TestRejectBackupTimezone(t *testing.T) {
	if perr := rejectBackupTimezone(nil); perr != nil {
		t.Fatalf("perr = %+v, want nil for absent params", perr)
	}
	perr := rejectBackupTimezone(map[string]string{"backup_timezone": "Europe/Budapest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want invalid_request", perr)
	}
}

// closedTo makes a path unreadable for the rest of the test. Root reads a
// mode-000 path regardless, so the test is skipped there rather than
// asserting something the filesystem is not doing.
func closedTo(t *testing.T, path string, restore os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 path")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, restore); err != nil {
			t.Errorf("restore the mode: %v", err)
		}
	})
}

// wantUnreadable fails the test unless the refusal is the host saying it
// could not read something.
func wantUnreadable(t *testing.T, perr *protoError, message string) {
	t.Helper()
	if perr == nil || perr.Code != "source_unreadable" || !strings.Contains(perr.Message, message) {
		t.Errorf("got %+v, want source_unreadable mentioning %q", perr, message)
	}
}

// TestWhatTheHostCannotReadIsUnreadable covers the reads this adapter
// does on the drill host before anything moves. None of them says
// anything about the backup, and each is refused as the host's own
// failure rather than as a damaged artifact.
func TestWhatTheHostCannotReadIsUnreadable(t *testing.T) {
	t.Run("a path beneath a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "snapshot.tar")
		if err := os.WriteFile(file, []byte("tar bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"cassandra_snapshot_tar", "cassandra_snapshot", "cassandra_snapshot_dir"} {
			_, perr := resolveSource(kind, filepath.Join(file, "snapshot"), nil)
			if perr == nil || perr.Code != "source_unreadable" {
				t.Errorf("%s: got %+v, want source_unreadable", kind, perr)
			}
		}
	})
	t.Run("an archive the host may not open", func(t *testing.T) {
		tree := writeTree(t, filepath.Join(t.TempDir(), "snap"), "shop.orders")
		archive := treeToTar(t, tree, filepath.Join(t.TempDir(), "snapshot.tar"), "", false)
		closedTo(t, archive, 0o600)
		_, perr := resolveSource("cassandra_snapshot_tar", archive, nil)
		wantUnreadable(t, perr, "")
	})
	t.Run("an sstable the host may not open", func(t *testing.T) {
		tree := writeTree(t, filepath.Join(t.TempDir(), "snap"), "shop.orders")
		closedTo(t, filepath.Join(tree, "shop", "orders", "nb-1-big-Data.db"), 0o600)
		_, perr := resolveSource("cassandra_snapshot", tree, nil)
		wantUnreadable(t, perr, "")
	})
	t.Run("a snapshot directory holding no file", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "snap")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, perr := dirChecksum(empty)
		if perr == nil || perr.Code != "source_not_found" || !strings.Contains(perr.Message, "contains no files") {
			t.Errorf("got %+v, want source_not_found", perr)
		}
	})
	t.Run("an artifact that is not there to hash", func(t *testing.T) {
		_, perr := fileChecksum(filepath.Join(t.TempDir(), "gone.tar"))
		wantUnreadable(t, perr, "")
	})
}

// TestTheTreeChecksumSeesALinkAsItself: a link inside a snapshot is part
// of the artifact's identity, so repointing it changes the checksum the
// evidence record carries.
func TestTheTreeChecksumSeesALinkAsItself(t *testing.T) {
	tree := writeTree(t, filepath.Join(t.TempDir(), "snap"), "shop.orders")
	link := filepath.Join(tree, "shop", "latest")
	if err := os.Symlink("orders", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	before, _, perr := dirChecksum(tree)
	if perr != nil {
		t.Fatalf("dirChecksum: %+v", perr)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("somewhere-else", link); err != nil {
		t.Fatal(err)
	}
	after, _, perr := dirChecksum(tree)
	if perr != nil || after == before {
		t.Errorf("checksum %s unchanged after the link moved (%+v)", after, perr)
	}
}
