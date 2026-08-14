package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// wallClock is the backup's own timestamp as its manifest records it,
	// with no zone attached; zero when the archive declares none. zone.go
	// turns it into an instant, or into nothing.
	wallClock time.Time
}

// resolveSource maps a source kind to one restorable artifact.
//
//	clickhouse_backup      — path is one backup archive (BACKUP … TO File('x.zip'))
//	clickhouse_backup_dir  — path is a directory of them; the archive whose own
//	                         manifest records the newest timestamp is restored
//
// Only archive form is accepted. `BACKUP … TO Disk('backups','name')`
// without a `.zip` suffix writes an unpacked directory tree instead, which
// this adapter does not read: one artifact, one checksum, one identity in
// the evidence record. The README says how to produce the supported form.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "clickhouse_backup":
		return resolveFile(path)
	case "clickhouse_backup_dir":
		latest, perr := latestBackupIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: clickhouse_backup, clickhouse_backup_dir)", kind)
	}
}

func resolveFile(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind clickhouse_backup_dir for directories", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	src := &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}

	// A manifest that cannot be read is not fatal here. The engine is the
	// authority on whether an archive restores, and refusing one this
	// reader dislikes would fail drills over a file ClickHouse would have
	// accepted. The cost of being wrong is a null created_at, which the
	// evidence schema allows; the cost of the opposite is a false failure.
	if ts, err := readBackupWallClock(path); err == nil {
		src.wallClock = ts
	}
	return src, nil
}

// candidate is one file the directory scan considered.
type candidate struct {
	path      string
	name      string
	mtime     time.Time
	wallClock time.Time // zero for a file whose manifest could not be read
}

// latestBackupIn picks the archive whose manifest records the newest
// backup time — what the backup says about itself, never the file's mtime,
// which dates a copy rather than a backup.
//
// Files that are not archives at all are skipped: a backup directory
// routinely holds checksum files and job logs beside the artifacts. A file
// that *is* an archive but cannot be read is a different matter, and this
// is where skipping would be dangerous: a backup job still writing its zip
// leaves exactly that — an archive with no central directory yet — and
// quietly moving on would restore last night's backup while the evidence
// record named a drill the operator believes covered tonight's. So an
// unreadable archive newer than the chosen one refuses the drill.
//
// The comparison is mtime against mtime, never mtime against a manifest
// timestamp: those are different clocks (when the file was written here
// versus when the backup was taken there), and the question asked is only
// "did something land after the artifact I picked".
func latestBackupIn(ctx context.Context, dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var readable, unreadable []candidate
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		c := candidate{path: filepath.Join(dir, e.Name()), name: e.Name(), mtime: info.ModTime()}
		switch ts, err := readBackupWallClock(c.path); {
		case err == nil:
			c.wallClock = ts
			readable = append(readable, c)
		case looksLikeArchive(c.path):
			unreadable = append(unreadable, c)
		}
	}

	best, perr := newestByWallClock(dir, readable)
	if perr != nil {
		return "", perr
	}
	for _, u := range unreadable {
		if u.mtime.After(best.mtime) {
			return "", protoErr("source_unreadable", false,
				"%s is a backup archive this drill cannot read and it is newer than %s: "+
					"a backup job may still be writing it. Run the drill after the job finishes, or have it "+
					"write to a temporary name and rename on completion, so a drill never sees a partial file",
				u.name, best.name)
		}
	}
	// The adapter chose this file, not the operator: make sure a backup job
	// is not still writing it (see settle.go).
	if perr := assertSettled(ctx, best.path, settleWindow); perr != nil {
		return "", perr
	}
	return best.path, nil
}

// newestByWallClock ranks readable archives. Ties break toward the
// lexicographically larger name so the choice is deterministic when two
// backups share a second.
func newestByWallClock(dir string, candidates []candidate) (candidate, *protoError) {
	var best candidate
	for _, c := range candidates {
		if best.path == "" || c.wallClock.After(best.wallClock) ||
			(c.wallClock.Equal(best.wallClock) && c.name > best.name) {
			best = c
		}
	}
	if best.path == "" {
		return candidate{}, protoErr("source_not_found", false,
			"backup directory %s contains no readable ClickHouse backup archive", dir)
	}
	return best, nil
}

// looksLikeArchive reports whether a file opens with the zip local-header
// magic. A partially written ClickHouse backup has it — the writer emits
// local headers as it goes and the central directory only at the end — so
// this distinguishes "an archive I cannot read" from "not an archive".
func looksLikeArchive(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	var magic [4]byte
	_, rerr := io.ReadFull(f, magic[:])
	if cerr := f.Close(); cerr != nil || rerr != nil {
		return false
	}
	return magic == [4]byte{'P', 'K', 0x03, 0x04}
}

// fileChecksum streams the artifact once. The hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored.
func fileChecksum(path string) (string, *protoError) {
	f, err := os.Open(path)
	if err != nil {
		return "", protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	h := sha256.New()
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil && !errors.Is(cerr, io.EOF) {
		return "", protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}
