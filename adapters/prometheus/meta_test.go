package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxAug2026 is a plausible block maxTime used across the fixtures.
const maxAug2026 = int64(1786876374046)

// writeBlock lays down one TSDB-block-shaped directory.
func writeBlock(t *testing.T, snapshotDir, ulid string, maxTime int64) {
	t.Helper()
	dir := filepath.Join(snapshotDir, ulid)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"ulid":%q,"minTime":%d,"maxTime":%d,"version":1}`,
		ulid, maxTime-3000, maxTime)
	for name, content := range map[string]string{
		"meta.json":     meta,
		"index":         "index bytes",
		"tombstones":    "",
		"chunks/000001": "chunk bytes",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeSnapshot lays down a snapshot-shaped directory with the given
// blocks, newest last.
func writeSnapshot(t *testing.T, dir string, maxTimes ...int64) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, maxTime := range maxTimes {
		writeBlock(t, dir, fmt.Sprintf("01BLOCK%019d", i), maxTime)
	}
	return dir
}

func TestInspectSnapshotDir(t *testing.T) {
	t.Run("a healthy snapshot counts its blocks and claims its newest instant", func(t *testing.T) {
		dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026-60000, maxAug2026)
		// A stray sidecar beside the blocks is not the snapshot's problem.
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("sums\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, perr := inspectSnapshotDir(dir)
		if perr != nil {
			t.Fatalf("inspectSnapshotDir: %+v", perr)
		}
		if info.blocks != 2 || info.maxTimeMs != maxAug2026 {
			t.Errorf("info = %+v, want 2 blocks claiming %d", info, maxAug2026)
		}
	})

	t.Run("a directory with no blocks is not a snapshot", func(t *testing.T) {
		_, perr := inspectSnapshotDir(t.TempDir())
		if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "no TSDB blocks") {
			t.Fatalf("perr = %+v, want source_corrupt", perr)
		}
	})
}

func TestInspectSnapshotDirRefusesLiveMarkers(t *testing.T) {
	for _, marker := range []string{"wal", "chunks_head", "lock"} {
		t.Run("the live marker "+marker+" is refused by name", func(t *testing.T) {
			dir := writeSnapshot(t, filepath.Join(t.TempDir(), "data"), maxAug2026)
			path := filepath.Join(dir, marker)
			if marker == "lock" {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			_, perr := inspectSnapshotDir(dir)
			if perr == nil || perr.Code != "unsupported_source" ||
				!strings.Contains(perr.Message, marker) || !strings.Contains(perr.Message, "snapshot API") {
				t.Fatalf("perr = %+v, want the raw copy refused by name, teaching the fix", perr)
			}
		})
	}

}

func TestInspectSnapshotDirRefusesABrokenBlock(t *testing.T) {
	dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026)
	if err := os.Mkdir(filepath.Join(dir, "01BROKEN"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, perr := inspectSnapshotDir(dir)
	if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "01BROKEN") {
		t.Fatalf("perr = %+v, want source_corrupt naming the block", perr)
	}
}

func TestReadBlockMeta(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		wantOK  bool
		wantMax int64
	}{
		{"a real meta", fmt.Sprintf(`{"ulid":"01X","maxTime":%d}`, maxAug2026), true, maxAug2026},
		{"not json", "not json at all", false, 0},
		{"an implausible instant", `{"ulid":"01X","maxTime":123}`, false, 0},
		{"a far-future instant", `{"ulid":"01X","maxTime":9999999999999999}`, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, "meta.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			meta, ok := readBlockMeta(path)
			if ok != tt.wantOK || meta.MaxTime != tt.wantMax {
				t.Errorf("readBlockMeta = %+v/%v, want max %d ok %v", meta, ok, tt.wantMax, tt.wantOK)
			}
		})
	}
	if _, ok := readBlockMeta(filepath.Join(dir, "missing.json")); ok {
		t.Error("a missing meta.json read as ok")
	}
}

// tarEntry is one entry of a test archive.
type tarEntry struct {
	name    string
	content string
	dir     bool
}

// buildTar writes a tar (optionally gzip) with the given entries.
func buildTar(t *testing.T, path string, gz bool, entries []tarEntry) string {
	t.Helper()
	buf := &bytes.Buffer{}
	var w *tar.Writer
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(buf)
		w = tar.NewWriter(gzw)
	} else {
		w = tar.NewWriter(buf)
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.content))}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if !e.dir {
			if _, err := w.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// snapshotTarEntries builds the entries of a snapshot archive, blocks at
// the root or under one wrapping directory.
func snapshotTarEntries(wrap string, maxTimes ...int64) []tarEntry {
	prefix := ""
	if wrap != "" {
		prefix = wrap + "/"
	}
	entries := []tarEntry{}
	for i, maxTime := range maxTimes {
		block := fmt.Sprintf("%s01BLOCK%019d", prefix, i)
		entries = append(entries,
			tarEntry{name: block + "/", dir: true},
			tarEntry{name: block + "/meta.json",
				content: fmt.Sprintf(`{"ulid":"b%d","maxTime":%d}`, i, maxTime)},
			tarEntry{name: block + "/chunks/000001", content: "chunk bytes"},
			tarEntry{name: block + "/index", content: "index bytes"},
		)
	}
	return entries
}

func TestListTarSnapshot(t *testing.T) {
	t.Run("a plain archive with blocks at the root", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "snap.tar"), false,
			snapshotTarEntries("", maxAug2026-60000, maxAug2026))
		info, live, ok := listTarSnapshot(path)
		if !ok || live != "" || info.blocks != 2 || info.maxTimeMs != maxAug2026 {
			t.Errorf("listTarSnapshot = %+v live=%q ok=%v", info, live, ok)
		}
	})

	t.Run("a gzip archive with one wrapping directory", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "snap.tar.gz"), true,
			snapshotTarEntries("20260816T102848Z-abcd", maxAug2026))
		info, live, ok := listTarSnapshot(path)
		if !ok || live != "" || info.blocks != 1 || info.maxTimeMs != maxAug2026 {
			t.Errorf("listTarSnapshot = %+v live=%q ok=%v", info, live, ok)
		}
	})

	t.Run("a tar of a live data directory carries its markers", func(t *testing.T) {
		entries := append(snapshotTarEntries("", maxAug2026),
			tarEntry{name: "wal/", dir: true},
			tarEntry{name: "wal/00000001", content: "wal segment"},
			tarEntry{name: "lock", content: ""})
		path := buildTar(t, filepath.Join(t.TempDir(), "datadir.tar"), false, entries)
		_, live, ok := listTarSnapshot(path)
		if !ok || live == "" {
			t.Errorf("live = %q ok=%v, want a named live marker", live, ok)
		}
	})

}

func TestListTarSnapshotEdgeCases(t *testing.T) {
	t.Run("an archive the reader cannot walk is a silent bonus miss", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opaque.bin")
		if err := os.WriteFile(path, bytes.Repeat([]byte{0xA5}, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := listTarSnapshot(path); ok {
			t.Error("random bytes walked as a tar")
		}
	})

	t.Run("a walkable archive without blocks counts zero", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "other.tar"), false,
			[]tarEntry{{name: "README.md", content: "hello"}})
		info, live, ok := listTarSnapshot(path)
		if !ok || live != "" || info.blocks != 0 {
			t.Errorf("listTarSnapshot = %+v live=%q ok=%v, want zero blocks", info, live, ok)
		}
	})
}

func TestPlausibleEpochMs(t *testing.T) {
	for _, bad := range []int64{0, -1, 123, 946684799999, 7258118400001} {
		if plausibleEpochMs(bad) {
			t.Errorf("plausibleEpochMs(%d) = true", bad)
		}
	}
	if !plausibleEpochMs(maxAug2026) {
		t.Errorf("plausibleEpochMs(%d) = false", maxAug2026)
	}
}
