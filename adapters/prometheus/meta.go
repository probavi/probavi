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
//
// The count is not the number of block directories: a snapshot taken
// while compaction sources still sat on disk legitimately holds both a
// compacted block and the parents it replaced, and the server must not
// load a block another present block's compaction.parents names — it
// marks such blocks obsolete and serves without them (measured). The
// census therefore expects present blocks minus present-and-superseded
// ones; see censusOf.

// blockMeta is the slice of a block's meta.json this adapter reads.
type blockMeta struct {
	ULID       string          `json:"ulid"`
	MaxTime    int64           `json:"maxTime"`
	Compaction blockCompaction `json:"compaction"`
}

// blockCompaction is the slice of a block's compaction record this
// adapter reads: which blocks this one replaced. Entries stay raw here
// because their shape has varied across server versions; parentULID
// reads one.
type blockCompaction struct {
	Parents []json.RawMessage `json:"parents"`
}

// parentULID reads one entry of a compaction parent list tolerantly:
// Prometheus versions have written both objects carrying a ulid field
// and bare ULID strings. Any other shape reads as empty and subtracts
// nothing — the census only ever shrinks on positive evidence.
func parentULID(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	obj := struct {
		ULID string `json:"ulid"`
	}{}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.ULID
	}
	return ""
}

// snapshotInfo is what an artifact states about itself.
type snapshotInfo struct {
	// blocks is how many TSDB blocks the restored server must load:
	// the blocks the artifact holds, minus compaction sources superseded
	// by another present block.
	blocks int
	// sourcesSkipped is how many present blocks another present block's
	// compaction.parents names — blocks the server deliberately skips.
	sourcesSkipped int
	// maxTimeMs is the newest instant the blocks claim to cover, epoch
	// milliseconds; 0 when nothing plausible was read.
	maxTimeMs int64
}

// censusOf derives what a snapshot's blocks jointly state. A block whose
// ULID appears in another present block's compaction.parents is a
// compaction source the server deliberately skips — its data already
// lives in the compacted block — so it moves the skip counter, not the
// census. This mirrors the server's own rule exactly: it marks the
// parents of every openable block obsolete, including parents of blocks
// it also skips, so a multi-level chain collapses to its newest block
// the same way here. Parents naming absent blocks subtract nothing, and
// the newest claimed instant still ranges over every block — a compacted
// block covers its parents' range, so the maximum is unchanged.
func censusOf(metas []blockMeta) snapshotInfo {
	superseded := map[string]bool{}
	for _, m := range metas {
		for _, raw := range m.Compaction.Parents {
			if u := parentULID(raw); u != "" {
				superseded[u] = true
			}
		}
	}
	info := snapshotInfo{}
	for _, m := range metas {
		if superseded[m.ULID] {
			info.sourcesSkipped++
		} else {
			info.blocks++
		}
		if m.MaxTime > info.maxTimeMs {
			info.maxTimeMs = m.MaxTime
		}
	}
	return info
}

// refuseSupersededOnly names the one shape censusOf can reduce to
// nothing: metadata claiming every block is a compaction source of
// another present block. No real compaction produces that cycle.
func refuseSupersededOnly(info snapshotInfo) *protoError {
	if info.blocks == 0 && info.sourcesSkipped > 0 {
		return protoErr("source_corrupt", false,
			"every block in the snapshot is named as a compaction source of another present block — "+
				"cyclic compaction metadata; the snapshot is damaged")
	}
	return nil
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
	metas := []blockMeta{}
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
		metas = append(metas, meta)
	}
	if len(metas) == 0 {
		return snapshotInfo{}, protoErr("source_corrupt", false,
			"backup directory %s holds no TSDB blocks — not a snapshot directory "+
				"(a snapshot from the API is a directory of block directories, each with a meta.json)", dir)
	}
	info := censusOf(metas)
	if perr := refuseSupersededOnly(info); perr != nil {
		return snapshotInfo{}, perr
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

const (
	// keptMaxBytes and keptMaxEntries bound what one archive walk holds
	// on to across entries: one decoded block meta per meta.json, whose
	// compaction parents are kept as raw JSON. A tar entry is a
	// 512-byte header that compresses to almost nothing, so a small
	// archive can carry any number of meta.json members, each read up to
	// metaMaxBytes, and a backup file is attacker-controlled input
	// (SECURITY.md). A TSDB snapshot holds one block directory per
	// compaction unit — hundreds on a large instance, never this many.
	keptMaxBytes   = 64 << 20
	keptMaxEntries = 200_000
)

// metaFootprint is what one decoded block meta keeps: its ULID and the
// compaction parent list, which stays raw because its shape has varied
// across server versions.
func metaFootprint(m blockMeta) int {
	n := len(m.ULID)
	for _, raw := range m.Compaction.Parents {
		n += len(raw)
	}
	return n
}

// retention accounts for what a walk keeps rather than for what it
// reads: an archive may hold any number of entries this pass ignores,
// and refusing those would turn a large legitimate copy into a failed
// drill.
type retention struct {
	entries int
	bytes   int
}

// take accounts for one retained entry of n bytes and reports whether
// the walk may keep it.
func (r *retention) take(n int) bool {
	r.entries++
	r.bytes += n
	return r.entries <= keptMaxEntries && r.bytes <= keptMaxBytes
}

// tooMuchKept is the verdict for an archive whose bookkeeping this walk
// cannot bound. The listing is a bonus everywhere else, but an archive
// built to exhaust the drill host's memory is positive evidence about
// the source.
func tooMuchKept() *protoError {
	return protoErr("source_corrupt", false,
		"the archive carries more block metadata than a snapshot holds — over %d meta.json members, "+
			"or more than %d MiB of them. Reading on would cost the drill host memory an archive gets "+
			"to choose, so the walk stops here",
		keptMaxEntries, keptMaxBytes>>20)
}

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
func listTarSnapshot(path string) (info snapshotInfo, live string, perr *protoError, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return snapshotInfo{}, "", nil, false
	}
	info, live, perr, ok = walkTarFile(f)
	if err := f.Close(); err != nil {
		return snapshotInfo{}, "", nil, false
	}
	return info, live, perr, ok
}

func walkTarFile(f *os.File) (info snapshotInfo, live string, perr *protoError, ok bool) {
	var r io.Reader = f
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil {
		return snapshotInfo{}, "", nil, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return snapshotInfo{}, "", nil, false
	}
	if head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return snapshotInfo{}, "", nil, false
		}
		r = gz
	}
	return walkTar(tar.NewReader(r))
}

// walkTar scans the archive's entries. A snapshot tars either the block
// directories at its root or one wrapping directory above them
// (measured: both layouts unpack and serve), so meta.json is expected at
// depth two or three and the live markers at depth one or two.
func walkTar(tr *tar.Reader) (info snapshotInfo, live string, perr *protoError, ok bool) {
	sawEntry := false
	metas := []blockMeta{}
	kept := retention{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return snapshotInfo{}, "", nil, false
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
			if !kept.take(metaFootprint(meta)) {
				return snapshotInfo{}, "", tooMuchKept(), true
			}
			metas = append(metas, meta)
		}
	}
	return censusOf(metas), live, nil, sawEntry
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
