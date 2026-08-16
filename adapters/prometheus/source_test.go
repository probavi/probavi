package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveSourceUnknownKind(t *testing.T) {
	_, perr := resolveSource("prometheus_backup", "/nowhere")
	if perr == nil || perr.Code != "unsupported_source" {
		t.Fatalf("perr = %+v, want unsupported_source", perr)
	}
	for _, kind := range []string{"prometheus_snapshot_tar", "prometheus_snapshot", "prometheus_snapshot_dir"} {
		if !strings.Contains(perr.Message, kind) {
			t.Errorf("message %q does not list %s", perr.Message, kind)
		}
	}
}

func TestResolveTar(t *testing.T) {
	t.Run("a walkable archive resolves with its own census", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "snap.tar.gz"), true,
			snapshotTarEntries("snapname", maxAug2026-60000, maxAug2026))
		src, perr := resolveSource("prometheus_snapshot_tar", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if !src.tarball || !strings.HasPrefix(src.checksum, "sha256:") ||
			src.info.blocks != 2 || src.info.maxTimeMs != maxAug2026 {
			t.Errorf("resolved = %+v info=%+v", src, src.info)
		}
	})

	t.Run("an opaque archive resolves with nothing claimed", func(t *testing.T) {
		// Metadata is a bonus: the sandbox extraction is the authority on
		// whether the bytes unpack.
		path := filepath.Join(t.TempDir(), "opaque.bin")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("prometheus_snapshot_tar", path)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.info.blocks != 0 || src.info.maxTimeMs != 0 {
			t.Errorf("info = %+v, want nothing claimed", src.info)
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
		}, "invalid_request", "prometheus_snapshot_dir"},
		{"a tar of a live data directory", func(t *testing.T, dir string) string {
			entries := append(snapshotTarEntries("", maxAug2026),
				tarEntry{name: "wal/", dir: true},
				tarEntry{name: "wal/00000001", content: "segment"})
			return buildTar(t, filepath.Join(dir, "datadir.tar"), false, entries)
		}, "unsupported_source", "snapshot API"},
		{"a walkable archive without blocks", func(t *testing.T, dir string) string {
			return buildTar(t, filepath.Join(dir, "other.tar"), false,
				[]tarEntry{{name: "README.md", content: "hello"}})
		}, "source_corrupt", "no TSDB blocks"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.prepare(t, t.TempDir())
			_, perr := resolveSource("prometheus_snapshot_tar", path)
			if perr == nil || perr.Code != tc.wantCode {
				t.Fatalf("perr = %+v, want %s", perr, tc.wantCode)
			}
			if tc.wantWord != "" && !strings.Contains(perr.Message, tc.wantWord) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantWord)
			}
		})
	}
}

func TestResolveSnapshotDir(t *testing.T) {
	t.Run("a healthy snapshot resolves with a tree checksum", func(t *testing.T) {
		dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026)
		src, perr := resolveSource("prometheus_snapshot", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.tarball || !strings.HasPrefix(src.checksum, "sha256:") ||
			src.info.blocks != 1 || src.info.maxTimeMs != maxAug2026 || src.sizeBytes == 0 {
			t.Errorf("resolved = %+v info=%+v", src, src.info)
		}
	})

	t.Run("the tree checksum sees content changes", func(t *testing.T) {
		dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026)
		before, perr := resolveSource("prometheus_snapshot", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		block := "01BLOCK" + strings.Repeat("0", 19)
		if err := os.WriteFile(filepath.Join(dir, block, "chunks", "000001"),
			[]byte("changed bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, perr := resolveSource("prometheus_snapshot", dir)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if before.checksum == after.checksum {
			t.Error("checksum unchanged after a chunk changed")
		}
	})

}

func TestResolveSnapshotDirRefusals(t *testing.T) {
	t.Run("a file needs the tar kind", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "snap.tar")
		if err := os.WriteFile(path, []byte("tar bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("prometheus_snapshot", path)
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "prometheus_snapshot_tar") {
			t.Fatalf("perr = %+v, want invalid_request naming the tar kind", perr)
		}
	})

	t.Run("a raw data directory is refused by name", func(t *testing.T) {
		dir := writeSnapshot(t, filepath.Join(t.TempDir(), "data"), maxAug2026)
		if err := os.Mkdir(filepath.Join(dir, "wal"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("prometheus_snapshot", dir)
		if perr == nil || perr.Code != "unsupported_source" || !strings.Contains(perr.Message, "wal") {
			t.Fatalf("perr = %+v, want the raw copy refused by name", perr)
		}
	})
}

func TestNewestSnapshotIn(t *testing.T) {
	t.Run("the snapshot claiming the newest instant wins over a fresher mtime", func(t *testing.T) {
		base := t.TempDir()
		older := writeSnapshot(t, filepath.Join(base, "20260810T000000Z-aaaa"), maxAug2026-86400000)
		newest := writeSnapshot(t, filepath.Join(base, "20260816T000000Z-bbbb"), maxAug2026)
		// The decoy: the older snapshot's directory was touched last — a
		// copy or a checksum pass resets mtimes, what the backup claims
		// about itself does not move.
		past := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(newest, past, past); err != nil {
			t.Fatal(err)
		}
		_ = older
		src, perr := resolveSource("prometheus_snapshot_dir", base)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != newest {
			t.Errorf("picked %s, want the snapshot whose own blocks claim the newest instant", src.path)
		}
	})

	t.Run("a dated snapshot beats an undated directory", func(t *testing.T) {
		base := t.TempDir()
		dated := writeSnapshot(t, filepath.Join(base, "a-dated"), maxAug2026)
		if err := os.MkdirAll(filepath.Join(base, "z-undated"), 0o755); err != nil {
			t.Fatal(err)
		}
		src, perr := resolveSource("prometheus_snapshot_dir", base)
		if perr != nil {
			t.Fatalf("resolveSource: %+v", perr)
		}
		if src.path != dated {
			t.Errorf("picked %s, want %s", src.path, dated)
		}
	})

}

// TestNewestSnapshotInRefusesTheBrokenWinner pins the not-a-filter side
// of the ranking: the chosen candidate faces every gate by name.
func TestNewestSnapshotInRefusesTheBrokenWinner(t *testing.T) {
	t.Run("a winning broken candidate is refused, not passed over", func(t *testing.T) {
		base := t.TempDir()
		writeSnapshot(t, filepath.Join(base, "old-good"), maxAug2026-86400000)
		live := writeSnapshot(t, filepath.Join(base, "new-live"), maxAug2026)
		if err := os.Mkdir(filepath.Join(live, "wal"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The live copy cannot be dated (inspection refuses), so it can
		// only win when nothing dated competes: strip the good one's date
		// too by breaking its meta.
		goodBlock := filepath.Join(base, "old-good", "01BLOCK"+strings.Repeat("0", 19), "meta.json")
		if err := os.WriteFile(goodBlock, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(live, now, now); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("prometheus_snapshot_dir", base)
		if perr == nil || perr.Code != "unsupported_source" || !strings.Contains(perr.Message, "wal") {
			t.Fatalf("perr = %+v, want the chosen live copy refused by name", perr)
		}
	})

}

func TestNewestSnapshotInEdgeCases(t *testing.T) {
	t.Run("a directory with no subdirectories says so", func(t *testing.T) {
		base := t.TempDir()
		if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, perr := resolveSource("prometheus_snapshot_dir", base)
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})

	t.Run("a missing directory is source_not_found", func(t *testing.T) {
		_, perr := resolveSource("prometheus_snapshot_dir", filepath.Join(t.TempDir(), "gone"))
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})
}

func TestFormatCreatedAt(t *testing.T) {
	if formatCreatedAt(0) != nil {
		t.Error("formatCreatedAt(0) != nil")
	}
	got := formatCreatedAt(maxAug2026)
	if got == nil || *got != "2026-08-16T10:32:54.046Z" {
		t.Errorf("formatCreatedAt = %v, want the exact millisecond instant", got)
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
