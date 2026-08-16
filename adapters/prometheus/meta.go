package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// meta.go reads what a snapshot states about itself, out of its own
// files. A Prometheus TSDB snapshot is a directory of block directories,
// each carrying a meta.json that records the time range its samples
// cover as epoch milliseconds (measured — minTime/maxTime, stats, and a
// format version). Two facts follow: the artifact dates itself exactly,
// with no timezone question, and the host can count the blocks a restore
// must serve — the census ops.go compares against the restored server's
// own prometheus_tsdb_blocks_loaded, because a server skips an unloadable
// block and stays up (measured), which no drill may report as green.

// blockMeta is the slice of a block's meta.json this adapter reads.
type blockMeta struct {
	ULID    string `json:"ulid"`
	MaxTime int64  `json:"maxTime"`
}

// snapshotInfo is what an artifact states about itself.
type snapshotInfo struct {
	// blocks is how many TSDB blocks the artifact holds.
	blocks int
	// maxTimeMs is the newest instant the blocks claim to cover, epoch
	// milliseconds; 0 when nothing plausible was read.
	maxTimeMs int64
}

// liveMarkers are the entries only a live (or crashed) data directory
// contains — the snapshot API never writes them (measured: a snapshot
// holds block directories and nothing else, while the data directory
// holds wal, chunks_head, lock and queries.active beside them).
var liveMarkers = []string{"wal", "chunks_head", "lock"}

// inspectSnapshotDir vets a snapshot directory by what its own entries
// state, and refuses the two shapes this adapter exists to catch: a raw
// copy of a live data directory, and a snapshot whose blocks it could
// not honestly count.
func inspectSnapshotDir(dir string) (snapshotInfo, *protoError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return snapshotInfo{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	if perr := refuseLiveMarkers(entries, dir); perr != nil {
		return snapshotInfo{}, perr
	}
	info := snapshotInfo{}
	for _, e := range entries {
		if !e.IsDir() {
			// Stray files beside the blocks (a checksum sidecar, a README)
			// are not the snapshot's problem.
			continue
		}
		meta, ok := readBlockMeta(filepath.Join(dir, e.Name(), "meta.json"))
		if !ok {
			return snapshotInfo{}, protoErr("source_corrupt", false,
				"block %s carries no readable meta.json — the snapshot is damaged, or this is "+
					"not a snapshot directory at all", e.Name())
		}
		info.blocks++
		if meta.MaxTime > info.maxTimeMs {
			info.maxTimeMs = meta.MaxTime
		}
	}
	if info.blocks == 0 {
		return snapshotInfo{}, protoErr("source_corrupt", false,
			"backup directory %s holds no TSDB blocks — not a snapshot directory "+
				"(a snapshot from the API is a directory of block directories, each with a meta.json)", dir)
	}
	return info, nil
}

// refuseLiveMarkers is the raw-copy fence: the markers are positive
// evidence the directory was copied from under a running server, whose
// write-ahead log the blocks alone do not carry — restoring it would
// prove a backup nobody actually has. The message teaches the fix.
func refuseLiveMarkers(entries []os.DirEntry, dir string) *protoError {
	for _, e := range entries {
		for _, marker := range liveMarkers {
			if e.Name() == marker {
				return protoErr("unsupported_source", false,
					"%s contains %q, which only a live data directory holds: this is a raw copy "+
						"taken from under a running server, not a snapshot, and its blocks alone "+
						"miss whatever was still in the write-ahead log — take backups with the "+
						"snapshot API (POST /api/v1/admin/tsdb/snapshot) instead", dir, marker)
			}
		}
	}
	return nil
}

// readBlockMeta reads one block's meta.json; ok is false when the file
// is missing, unreadable, not JSON, or claims nothing plausible.
func readBlockMeta(path string) (blockMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return blockMeta{}, false
	}
	meta, ok := decodeBlockMeta(io.LimitReader(f, metaMaxBytes))
	if err := f.Close(); err != nil {
		return blockMeta{}, false
	}
	return meta, ok
}

// metaMaxBytes bounds a meta.json read: real ones are a few hundred
// bytes (measured).
const metaMaxBytes = 1 << 20

func decodeBlockMeta(r io.Reader) (blockMeta, bool) {
	meta := blockMeta{}
	if err := json.NewDecoder(r).Decode(&meta); err != nil {
		return blockMeta{}, false
	}
	if !plausibleEpochMs(meta.MaxTime) {
		return blockMeta{}, false
	}
	return meta, true
}

// plausibleEpochMs rejects values no block time range produces, so a
// field that happens to parse cannot date a backup absurdly.
func plausibleEpochMs(ms int64) bool {
	const (
		year2000 = 946684800000  // no restorable snapshot predates this
		year2200 = 7258118400000 // and none is written this far ahead
	)
	return ms >= year2000 && ms <= year2200
}

// listTarSnapshot reads what a snapshot archive states about itself,
// without unpacking it: block census, newest claimed instant, and
// whether the archive is a raw data-directory copy. Metadata is a bonus
// — an archive Go's tar reader cannot walk yields ok false and every
// verdict moves into the sandbox (ops.go) — except where an entry is
// positive evidence, which the caller refuses on. Both the plain and the
// gzip form are read (measured: backup jobs produce both, and the
// in-sandbox busybox tar unpacks both).
func listTarSnapshot(path string) (info snapshotInfo, live string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return snapshotInfo{}, "", false
	}
	info, live, ok = walkTarFile(f)
	if err := f.Close(); err != nil {
		return snapshotInfo{}, "", false
	}
	return info, live, ok
}

func walkTarFile(f *os.File) (info snapshotInfo, live string, ok bool) {
	var r io.Reader = f
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil {
		return snapshotInfo{}, "", false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return snapshotInfo{}, "", false
	}
	if head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return snapshotInfo{}, "", false
		}
		r = gz
	}
	return walkTar(tar.NewReader(r))
}

// walkTar scans the archive's entries. A snapshot tars either the block
// directories at its root or one wrapping directory above them
// (measured: both layouts unpack and serve), so meta.json is expected at
// depth two or three and the live markers at depth one or two.
func walkTar(tr *tar.Reader) (info snapshotInfo, live string, ok bool) {
	sawEntry := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return snapshotInfo{}, "", false
		}
		sawEntry = true
		segments := splitTarName(hdr.Name)
		if marker := liveMarkerIn(segments); marker != "" {
			live = marker
			continue
		}
		if !isMetaEntry(segments, hdr.Typeflag) {
			continue
		}
		if meta, metaOK := decodeBlockMeta(io.LimitReader(tr, metaMaxBytes)); metaOK {
			info.blocks++
			if meta.MaxTime > info.maxTimeMs {
				info.maxTimeMs = meta.MaxTime
			}
		}
	}
	return info, live, sawEntry
}

// splitTarName normalizes an entry name into path segments.
func splitTarName(name string) []string {
	name = strings.TrimPrefix(strings.TrimSuffix(name, "/"), "./")
	if name == "" {
		return nil
	}
	return strings.Split(name, "/")
}

// liveMarkerIn reports a live-data-directory marker at the depths the
// two accepted layouts place top-level entries.
func liveMarkerIn(segments []string) string {
	for depth := 0; depth < len(segments) && depth < 2; depth++ {
		for _, marker := range liveMarkers {
			if segments[depth] == marker {
				return marker
			}
		}
	}
	return ""
}

// isMetaEntry reports a block meta.json at the depths the two accepted
// layouts place it.
func isMetaEntry(segments []string, typeflag byte) bool {
	if typeflag != tar.TypeReg {
		return false
	}
	depth := len(segments)
	return (depth == 2 || depth == 3) && segments[depth-1] == "meta.json"
}
