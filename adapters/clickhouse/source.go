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
func resolveSource(ctx context.Context, kind, path string,
	params map[string]string) (*resolvedSource, *protoError) {
	policy, perr := backupSelection(kind, params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "clickhouse_backup":
		return resolveFile(path)
	case "clickhouse_backup_dir":
		chosen, perr := chooseBackupIn(ctx, path, policy)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(chosen)
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

// chooseBackupIn picks the archive the policy asks for, ordered by the
// backup time each manifest records — what the backup says about itself,
// never the file's mtime, which dates a copy rather than a backup.
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
//
// That refusal guards newest, and only newest. Under oldest and random the
// operator has named which end of the retention window the drill is about
// and the record names the artifact it proved, so a half-written archive
// elsewhere in the directory is nothing the result could be read as
// claiming — while keeping the refusal would fire on almost every
// candidate, since under oldest nearly everything is newer than the chosen
// one. It is the same scoping the weaviate adapter applies to its own
// newer-attempt refusal, and for the same reason.
func chooseBackupIn(ctx context.Context, dir string, policy selectPolicy) (string, *protoError) {
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

	if len(readable) == 0 {
		return "", protoErr("source_not_found", false,
			"backup directory %s contains no readable ClickHouse backup archive", dir)
	}
	best := pick(readable, policy)
	if policy == selectNewest {
		if perr := refuseNewerUnreadable(best, unreadable); perr != nil {
			return "", perr
		}
	}
	// The adapter chose this file, not the operator — under every policy:
	// make sure a backup job is not still writing it (see settle.go).
	if perr := assertSettled(ctx, best.path, settleWindow); perr != nil {
		return "", perr
	}
	return best.path, nil
}

// refuseNewerUnreadable fails the drill when an archive the drill cannot
// read landed after the one it picked — see chooseBackupIn for why that is
// a refusal rather than a skip, and why it guards only the newest policy.
func refuseNewerUnreadable(best candidate, unreadable []candidate) *protoError {
	for _, u := range unreadable {
		if u.mtime.After(best.mtime) {
			return protoErr("source_unreadable", false,
				"%s is a backup archive this drill cannot read and it is newer than %s: "+
					"a backup job may still be writing it. Run the drill after the job finishes, or have it "+
					"write to a temporary name and rename on completion, so a drill never sees a partial file",
				u.name, best.name)
		}
	}
	return nil
}

// beats orders two readable archives for the newest policy: the later
// recorded backup time, then the lexicographically larger name so the
// choice is deterministic when two backups share a second.
func (c candidate) beats(other candidate) bool {
	if !c.wallClock.Equal(other.wallClock) {
		return c.wallClock.After(other.wallClock)
	}
	return c.name > other.name
}

// precedes orders two readable archives for the oldest policy. Here it
// really is beats turned around, and that is worth a sentence because in
// the postgres and cassandra adapters it is not: those rank a backup
// carrying its own recorded time above one that does not, and that rule
// cannot invert. An archive whose manifest cannot be read never reaches
// this comparison — it is either not an archive, and skipped, or it is one
// and it refuses the drill — so there is nothing asymmetric left.
func (c candidate) precedes(other candidate) bool {
	if !c.wallClock.Equal(other.wallClock) {
		return c.wallClock.Before(other.wallClock)
	}
	return c.name < other.name
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
