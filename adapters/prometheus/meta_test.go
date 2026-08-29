package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxAug2026 is a plausible block maxTime used across the fixtures.
const maxAug2026 = int64(1786876374046)

// writeBlock lays down one TSDB-block-shaped directory. Parents, when
// given, are recorded the way current servers write them: objects under
// compaction.parents.
func writeBlock(t *testing.T, snapshotDir, ulid string, maxTime int64, parents ...string) {
	t.Helper()
	dir := filepath.Join(snapshotDir, ulid)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	compaction := ""
	if len(parents) > 0 {
		refs := make([]string, 0, len(parents))
		for _, p := range parents {
			refs = append(refs, fmt.Sprintf(`{"ulid":%q,"minTime":%d,"maxTime":%d}`, p, maxTime-3000, maxTime))
		}
		compaction = fmt.Sprintf(`,"compaction":{"level":2,"parents":[%s]}`, strings.Join(refs, ","))
	}
	meta := fmt.Sprintf(`{"ulid":%q,"minTime":%d,"maxTime":%d,"version":1%s}`,
		ulid, maxTime-3000, maxTime, compaction)
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

// blockWithParents builds one decoded meta for censusOf tests, parents
// in the bare-string shape (the object shape is covered by
// TestParentULIDShapes and the fixture-driven tests).
func blockWithParents(ulid string, maxTime int64, parents ...string) blockMeta {
	refs := make([]json.RawMessage, 0, len(parents))
	for _, p := range parents {
		refs = append(refs, json.RawMessage(fmt.Sprintf("%q", p)))
	}
	return blockMeta{ULID: ulid, MaxTime: maxTime, Compaction: blockCompaction{Parents: refs}}
}

// TestCensusOf pins the census rule the server's own block loading
// follows (issue #155): a present block named in another present block's
// compaction parent list is skipped, everything else must load.
func TestCensusOf(t *testing.T) {
	tests := []struct {
		name        string
		metas       []blockMeta
		wantBlocks  int
		wantSkipped int
		wantMax     int64
	}{
		{
			name: "independent blocks all count",
			metas: []blockMeta{
				blockWithParents("A", maxAug2026-60000), blockWithParents("B", maxAug2026),
			},
			wantBlocks: 2, wantSkipped: 0, wantMax: maxAug2026,
		},
		{
			name: "a present parent is a skipped compaction source",
			metas: []blockMeta{
				blockWithParents("A", maxAug2026-60000),
				blockWithParents("B", maxAug2026, "A"),
			},
			wantBlocks: 1, wantSkipped: 1, wantMax: maxAug2026,
		},
		{
			name: "a parent naming an absent block subtracts nothing",
			metas: []blockMeta{
				blockWithParents("B", maxAug2026, "GONE"),
			},
			wantBlocks: 1, wantSkipped: 0, wantMax: maxAug2026,
		},
		{
			name: "a multi-level chain collapses to its newest block",
			metas: []blockMeta{
				blockWithParents("A", maxAug2026-120000),
				blockWithParents("B", maxAug2026-60000, "A"),
				blockWithParents("C", maxAug2026, "B"),
			},
			wantBlocks: 1, wantSkipped: 2, wantMax: maxAug2026,
		},
		{
			name: "a parent named twice is skipped once",
			metas: []blockMeta{
				blockWithParents("A", maxAug2026-120000),
				blockWithParents("B", maxAug2026-60000, "A"),
				blockWithParents("C", maxAug2026, "A"),
			},
			wantBlocks: 2, wantSkipped: 1, wantMax: maxAug2026,
		},
		{
			name: "empty parent entries subtract nothing",
			metas: []blockMeta{
				blockWithParents("A", maxAug2026, ""),
				blockWithParents("", maxAug2026-60000),
			},
			wantBlocks: 2, wantSkipped: 0, wantMax: maxAug2026,
		},
		{
			name: "the newest instant still ranges over skipped sources",
			metas: []blockMeta{
				blockWithParents("A", maxAug2026),
				blockWithParents("B", maxAug2026-60000, "A"),
			},
			wantBlocks: 1, wantSkipped: 1, wantMax: maxAug2026,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := censusOf(tt.metas)
			if info.blocks != tt.wantBlocks || info.sourcesSkipped != tt.wantSkipped ||
				info.maxTimeMs != tt.wantMax {
				t.Errorf("censusOf = %+v, want blocks %d skipped %d max %d",
					info, tt.wantBlocks, tt.wantSkipped, tt.wantMax)
			}
		})
	}
}

// TestParentULIDShapes pins the tolerant read: the parent list's shape
// has varied across server versions (objects with a ulid field, bare
// ULID strings), and anything else must read as empty rather than fail
// the block or subtract from the census.
func TestParentULIDShapes(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []string
	}{
		{"objects with a ulid field", `{"parents":[{"ulid":"A","minTime":1,"maxTime":2},{"ulid":"B"}]}`,
			[]string{"A", "B"}},
		{"bare ULID strings", `{"parents":["A","B"]}`, []string{"A", "B"}},
		{"mixed and junk entries read without failing", `{"parents":[{"ulid":"A"},"B",7,{"level":2},null]}`,
			[]string{"A", "B", "", "", ""}},
		{"no parent list at all", `{"level":1,"sources":["A"]}`, nil},
		{"a null parent list", `{"parents":null}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := blockCompaction{}
			if err := json.Unmarshal([]byte(tt.json), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(got.Parents) != len(tt.want) {
				t.Fatalf("parents = %v, want %v", got.Parents, tt.want)
			}
			for i := range got.Parents {
				if u := parentULID(got.Parents[i]); u != tt.want[i] {
					t.Errorf("parents[%d] = %q, want %q", i, u, tt.want[i])
				}
			}
		})
	}
}

// TestInspectSnapshotDirSubtractsCompactionSources is issue #155's
// shape: a snapshot taken during a compaction window holds both the
// compacted block and the sources it replaced, and the census must
// expect only what the server will load.
func TestInspectSnapshotDirSubtractsCompactionSources(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snap")
	writeBlock(t, dir, "01SOURCEAAAAAAAAAAAAAAAAAA", maxAug2026-60000)
	writeBlock(t, dir, "01SOURCEBBBBBBBBBBBBBBBBBB", maxAug2026-30000)
	writeBlock(t, dir, "01COMPACTEDCCCCCCCCCCCCCCC", maxAug2026,
		"01SOURCEAAAAAAAAAAAAAAAAAA", "01SOURCEBBBBBBBBBBBBBBBBBB")
	info, perr := inspectSnapshotDir(dir)
	if perr != nil {
		t.Fatalf("inspectSnapshotDir: %+v", perr)
	}
	if info.blocks != 1 || info.sourcesSkipped != 2 || info.maxTimeMs != maxAug2026 {
		t.Errorf("info = %+v, want 1 required block, 2 skipped sources, max %d", info, maxAug2026)
	}
}

// TestInspectSnapshotDirRefusesCyclicCompaction pins the one shape the
// subtraction can reduce to nothing: metadata claiming every block is a
// source of another. No real compaction produces that.
func TestInspectSnapshotDirRefusesCyclicCompaction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snap")
	writeBlock(t, dir, "01CYCLEAAAAAAAAAAAAAAAAAAA", maxAug2026-60000, "01CYCLEBBBBBBBBBBBBBBBBBBB")
	writeBlock(t, dir, "01CYCLEBBBBBBBBBBBBBBBBBBB", maxAug2026, "01CYCLEAAAAAAAAAAAAAAAAAAA")
	_, perr := inspectSnapshotDir(dir)
	if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "cyclic") {
		t.Fatalf("perr = %+v, want source_corrupt naming the cyclic metadata", perr)
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
		info, live, _, ok := listTarSnapshot(path)
		if !ok || live != "" || info.blocks != 2 || info.maxTimeMs != maxAug2026 {
			t.Errorf("listTarSnapshot = %+v live=%q ok=%v", info, live, ok)
		}
	})

	t.Run("a gzip archive with one wrapping directory", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "snap.tar.gz"), true,
			snapshotTarEntries("20260816T102848Z-abcd", maxAug2026))
		info, live, _, ok := listTarSnapshot(path)
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
		_, live, _, ok := listTarSnapshot(path)
		if !ok || live == "" {
			t.Errorf("live = %q ok=%v, want a named live marker", live, ok)
		}
	})

}

// TestListTarSnapshotSubtractsCompactionSources proves the host-side
// archive walk applies the same census rule the directory kinds do: a
// compaction-window archive expects only the blocks the server will
// load.
func TestListTarSnapshotSubtractsCompactionSources(t *testing.T) {
	entries := append(snapshotTarEntries("", maxAug2026-60000),
		tarEntry{name: "01COMPACTED/", dir: true},
		tarEntry{name: "01COMPACTED/meta.json",
			content: fmt.Sprintf(`{"ulid":"c","maxTime":%d,"compaction":{"level":2,"parents":[{"ulid":"b0"}]}}`,
				maxAug2026)},
		tarEntry{name: "01COMPACTED/index", content: "index bytes"})
	path := buildTar(t, filepath.Join(t.TempDir(), "window.tar"), false, entries)
	info, live, _, ok := listTarSnapshot(path)
	if !ok || live != "" || info.blocks != 1 || info.sourcesSkipped != 1 || info.maxTimeMs != maxAug2026 {
		t.Errorf("listTarSnapshot = %+v live=%q ok=%v, want 1 required block and 1 skipped source",
			info, live, ok)
	}
}

func TestListTarSnapshotEdgeCases(t *testing.T) {
	t.Run("an archive the reader cannot walk is a silent bonus miss", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opaque.bin")
		if err := os.WriteFile(path, bytes.Repeat([]byte{0xA5}, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, ok := listTarSnapshot(path); ok {
			t.Error("random bytes walked as a tar")
		}
	})

	t.Run("a walkable archive without blocks counts zero", func(t *testing.T) {
		path := buildTar(t, filepath.Join(t.TempDir(), "other.tar"), false,
			[]tarEntry{{name: "README.md", content: "hello"}})
		info, live, _, ok := listTarSnapshot(path)
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

// TestListTarSnapshotRefusesUnboundedBlockMetadata pins the retention
// bound. Every meta.json is decoded and kept — the compaction parent
// list stays raw, because its shape has varied across server versions —
// so without a bound a small archive decides how much memory the drill
// host spends. A backup file is attacker-controlled input (SECURITY.md),
// and a drill killed for memory leaves no evidence record.
func TestListTarSnapshotRefusesUnboundedBlockMetadata(t *testing.T) {
	// One parent whose raw JSON is nearly the whole read limit: what is
	// retained per block, in one allocation rather than thousands.
	meta := `{"ulid":"b","maxTime":1700000000000,"compaction":{"parents":[{"ulid":"` +
		strings.Repeat("a", metaMaxBytes-128) + `"}]}}`
	entries := make([]tarEntry, 0, keptMaxBytes/metaMaxBytes+2)
	for i := 0; i <= keptMaxBytes/metaMaxBytes+1; i++ {
		entries = append(entries, tarEntry{name: fmt.Sprintf("snap/b%d/meta.json", i), content: meta})
	}
	path := buildTar(t, filepath.Join(t.TempDir(), "snapshot.tar.gz"), true, entries)

	_, _, perr, _ := listTarSnapshot(path)
	if perr == nil {
		t.Fatal("listTarSnapshot accepted an archive whose block metadata it cannot bound")
	}
	if perr.Code != "source_corrupt" {
		t.Errorf("code = %s, want source_corrupt", perr.Code)
	}
	if !strings.Contains(perr.Message, "memory") {
		t.Errorf("message %q must say why the walk stopped", perr.Message)
	}
}
