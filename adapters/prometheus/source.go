package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes (or tree)
	sizeBytes int64
	// tarball reports that the artifact is an archive to unpack rather
	// than a directory to transfer.
	tarball bool
	// info is what the artifact states about itself: the block census the
	// restore must satisfy and the newest instant the backup claims. For
	// an archive the host could not walk, the zero value — ops.go then
	// recovers the same facts from the unpacked tree.
	info snapshotInfo
}

// resolveSource maps a source kind to one restorable artifact.
//
//	prometheus_snapshot_tar — path is one tar archive (plain or gzip) of
//	                          a snapshot, blocks at the root or under one
//	                          wrapping directory
//	prometheus_snapshot     — path is one snapshot directory from
//	                          POST /api/v1/admin/tsdb/snapshot
//	prometheus_snapshot_dir — path is a directory of snapshot
//	                          directories; the one whose own blocks claim
//	                          the newest instant is restored
//
// A snapshot's blocks record their time ranges as epoch milliseconds
// (measured), so created_at and the directory ranking both come from
// what the artifact states about itself — never from file times a copy
// would reset.
func resolveSource(kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "prometheus_snapshot_tar":
		return resolveTar(path)
	case "prometheus_snapshot":
		return resolveSnapshotDir(path)
	case "prometheus_snapshot_dir":
		latest, perr := newestSnapshotIn(path)
		if perr != nil {
			return nil, perr
		}
		return resolveSnapshotDir(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: prometheus_snapshot_tar, prometheus_snapshot, "+
				"prometheus_snapshot_dir)", kind)
	}
}

// resolveTar vets an archive artifact with what the host can read out of
// it. The tar listing is a bonus — an archive the host cannot walk still
// resolves, and the sandbox extraction is the authority — except where an
// entry is positive evidence: a live-data-directory marker, or a walkable
// archive with no blocks in it at all.
func resolveTar(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind prometheus_snapshot for a snapshot directory, "+
				"or prometheus_snapshot_dir for a directory of them", path)
	}
	census, live, ok := listTarSnapshot(path)
	if ok && live != "" {
		return nil, protoErr("unsupported_source", false,
			"the archive contains %q, which only a live data directory holds: this is a tar of a "+
				"raw data-directory copy, not of a snapshot, and its blocks alone miss whatever was "+
				"still in the write-ahead log — take backups with the snapshot API "+
				"(POST /api/v1/admin/tsdb/snapshot) and tar its output instead", live)
	}
	if ok {
		if perr := refuseSupersededOnly(census); perr != nil {
			return nil, perr
		}
	}
	if ok && census.blocks == 0 {
		return nil, protoErr("source_corrupt", false,
			"the archive holds no TSDB blocks — not a snapshot archive (a snapshot is a directory "+
				"of block directories, each with a meta.json)")
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: checksum, sizeBytes: info.Size(),
		tarball: true, info: census,
	}, nil
}

// resolveSnapshotDir vets a snapshot directory: the live-copy fence, the
// block census, and the canonical tree checksum.
func resolveSnapshotDir(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; use kind prometheus_snapshot_tar for tar archives", dir)
	}
	census, perr := inspectSnapshotDir(dir)
	if perr != nil {
		return nil, perr
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: dir, checksum: checksum, sizeBytes: size, info: census}, nil
}

// snapshotCandidate is one subdirectory considered for restore.
type snapshotCandidate struct {
	name      string
	maxTimeMs int64 // the candidate's own newest claimed instant, 0 when unreadable
	mtime     time.Time
}

// newestSnapshotIn picks the snapshot whose own blocks claim the newest
// instant. A snapshot that can be dated from its own files wins over one
// that cannot — the drill would rather restore the backup it can also
// say something true about, the mariadb adapter's precedent — and the
// chosen directory still faces every single-snapshot gate, so a broken
// or live-copied candidate that wins the ranking is refused by name.
func newestSnapshotIn(dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best *snapshotCandidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		candidate := snapshotCandidate{name: e.Name(), mtime: info.ModTime()}
		if census, perr := inspectSnapshotDir(filepath.Join(dir, e.Name())); perr == nil {
			candidate.maxTimeMs = census.maxTimeMs
		}
		if best == nil || candidate.beats(*best) {
			c := candidate
			best = &c
		}
	}
	if best == nil {
		return "", protoErr("source_not_found", false,
			"backup directory %s contains no snapshot directories", dir)
	}
	return filepath.Join(dir, best.name), nil
}

// beats orders candidates: a dated snapshot outranks every undated one,
// a newer claimed instant outranks an older, undated candidates fall
// back to directory time, and remaining ties break toward the
// lexicographically larger name so the choice is deterministic.
func (c snapshotCandidate) beats(o snapshotCandidate) bool {
	switch {
	case (c.maxTimeMs != 0) != (o.maxTimeMs != 0):
		return c.maxTimeMs != 0
	case c.maxTimeMs != o.maxTimeMs:
		return c.maxTimeMs > o.maxTimeMs
	case !c.mtime.Equal(o.mtime):
		return c.mtime.After(o.mtime)
	default:
		return c.name > o.name
	}
}

// dirChecksum hashes a directory tree canonically: entries sorted by
// relative path; regular files contribute path, size, and content bytes,
// symlinks contribute path and target. The same tree always hashes the
// same, any content change changes the hash.
func dirChecksum(root string) (string, int64, *protoError) {
	h := sha256.New()
	var total int64
	var files int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return hashEntry(h, path, rel, d, &total, &files)
	})
	if err != nil {
		return "", 0, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	if files == 0 {
		return "", 0, protoErr("source_not_found", false, "backup directory %s contains no files", root)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), total, nil
}

func hashEntry(h io.Writer, path, rel string, d os.DirEntry, total *int64, files *int) error {
	switch {
	case d.Type().IsRegular():
		info, err := d.Info()
		if err != nil {
			return err
		}
		*total += info.Size()
		*files++
		fmt.Fprintf(h, "%s\x00%d\x00", rel, info.Size())
		return copyFileInto(h, path)
	case d.Type()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00L%s\x00", rel, target)
	}
	return nil
}

func copyFileInto(h io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	return cerr
}

// fileChecksum streams the artifact once. The hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored.
func fileChecksum(path string) (string, *protoError) {
	h := sha256.New()
	if err := copyFileInto(h, path); err != nil {
		return "", protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// createdAtLayout renders source_identity.created_at; the instant is
// already UTC (epoch milliseconds), so the offset is literal Z.
const createdAtLayout = "2006-01-02T15:04:05.000Z07:00"

// formatCreatedAt renders an epoch-millisecond instant, or nil for 0.
func formatCreatedAt(ms int64) *string {
	if ms == 0 {
		return nil
	}
	s := time.UnixMilli(ms).UTC().Format(createdAtLayout)
	return &s
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// wall-clock formats sibling adapters read need it; a snapshot's blocks
// record their time ranges as epoch milliseconds (measured), which carry
// no zone question at all. A declaration is refused rather than ignored:
// silence would leave the operator believing it did something.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this format makes redundant.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: a snapshot's blocks state their time "+
			"ranges as epoch milliseconds, so backup.created_at is exact without a declared zone — "+
			"remove the parameter", backupTimezoneParam)
}
