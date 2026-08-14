package main

import (
	"context"
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
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// createdAt is the backup's own creation time, read from the artifact
	// and placed in the operator-declared zone (see timestamp.go); nil
	// when the backup does not carry one or no zone was declared.
	createdAt *string
	// compressed reports whether the SQL member is stored gzip-compressed,
	// sniffed from its own bytes (see compress.go).
	compressed bool
	// marker is the pattern the member's end has to match for the replay
	// to count as complete, or "" for a member that carries no ending to
	// check (see complete.go).
	marker string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	mariadb_dump     — path is a mariadb-dump (or mysqldump) SQL file
//	mariadb_dump_dir — path is a directory; the dump whose own trailer
//	                   records the newest time is chosen
//	mariadb_backup   — path is a mariadb-backup full-backup directory
func resolveSource(ctx context.Context, kind, path string, params map[string]string) (*resolvedSource, *protoError) {
	loc, perr := backupLocation(params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "mariadb_dump":
		return resolveFile(ctx, path, loc)
	case "mariadb_dump_dir":
		latest, perr := latestDumpIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(ctx, latest, loc)
	case "mariadb_backup":
		src, perr := resolveRepo(path, loc)
		if perr != nil {
			return nil, perr
		}
		// Every mariadb-backup backup carries a checkpoints metadata file,
		// under one of two names: 10.x keeps the XtraBackup ancestry's
		// xtrabackup_checkpoints, 11.0 renamed it to
		// mariadb_backup_checkpoints (both measured). Its absence under
		// either name means the directory is something else — refuse
		// before a single byte is transferred.
		if !anyExists(path, "mariadb_backup_checkpoints", "xtrabackup_checkpoints") {
			return nil, protoErr("source_corrupt", false,
				"backup directory %s lacks mariadb_backup_checkpoints (and the pre-11 "+
					"xtrabackup_checkpoints) — not a mariadb-backup backup", path)
		}
		return src, nil
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: mariadb_dump, mariadb_dump_dir, mariadb_backup)", kind)
	}
}

// anyExists reports whether any of the named files exists inside dir.
func anyExists(dir string, names ...string) bool {
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// resolveRepo resolves a directory source: the checksum is a canonical hash
// over the whole tree (documented in the adapter README), created_at is the
// newest file's mtime.
func resolveRepo(dir string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the mariadb_backup kind expects a backup directory", dir)
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	// The backup dates itself through xtrabackup_info, in the declared
	// zone (see backuptime.go).
	return &resolvedSource{
		path:      dir,
		checksum:  checksum,
		sizeBytes: size,
		createdAt: backupCreatedAt(dir, loc),
	}, nil
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
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, cerr := io.Copy(h, f)
		if err := f.Close(); err != nil && cerr == nil {
			cerr = err
		}
		return cerr
	case d.Type()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00L%s\x00", rel, target)
	}
	return nil
}

// resolveFile resolves a single-dump source. The checksum and the reported
// size cover the artifact as stored, compressed or not: those bytes are the
// ones the operator retains, and the evidence record has to identify what
// is in the backup archive rather than something derived from it.
func resolveFile(ctx context.Context, path string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind mariadb_dump_dir for directories", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	compressed, perr := sniffCompressed(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path:       path,
		checksum:   checksum,
		sizeBytes:  info.Size(),
		createdAt:  dumpCompletedAt(ctx, path, loc),
		compressed: compressed,
		marker:     completenessMarker(path),
	}, nil
}

// latestDumpIn picks the dump in dir that records the newest time about
// itself (see newestBackupIn).
func latestDumpIn(ctx context.Context, dir string) (string, *protoError) {
	best, perr := newestBackupIn(ctx, dir, "")
	if perr != nil {
		return "", perr
	}
	if best == "" {
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	// The adapter chose this file, not the operator: make sure a backup job
	// is not still writing it (see settle.go).
	if perr := assertSettled(ctx, best, settleWindow); perr != nil {
		return "", perr
	}
	return best, nil
}

// newestBackupIn returns the backup a directory source should restore,
// skipping the entry named except. An empty result means the directory is
// readable but holds no candidate — the caller says what that means.
//
// Candidates are ranked by the time the backup records about itself, not
// by the file's modification time. A backup copied into the directory
// afterwards — cp without -p, an object-store download, an rsync without
// -t — carries a fresh mtime, so under the old rule a stale artifact
// became "the newest file" and was the one the drill proved. What a
// backup says about itself does not move when the file is copied.
//
// A compressed candidate has to be decompressed to reach that sentence
// (see readDumpTail), which makes ranking a directory of compressed dumps
// cost one pass over each candidate. The rule is what matters here and it
// stays one rule for both storage forms; the adapter README states the
// price so an operator can see it before pointing a drill at a directory.
func newestBackupIn(ctx context.Context, dir, except string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var (
		best     string
		bestRank dirCandidate
	)
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == except {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		path := filepath.Join(dir, e.Name())
		clock, dated := dumpClock(ctx, path)
		rank := dirCandidate{name: e.Name(), clock: clock, dated: dated, mtime: info.ModTime()}
		if best == "" || rank.beats(bestRank) {
			best, bestRank = path, rank
		}
	}
	return best, nil
}

// dirCandidate is one file a directory source could restore, with the two
// times that can rank it.
type dirCandidate struct {
	name  string
	clock time.Time // what the backup records about itself
	dated bool      // whether that clock could be read at all
	mtime time.Time
}

// beats orders two candidates. A backup that can be dated from its own
// bytes wins over one that cannot: the drill would rather restore the
// backup it can also say something true about. Between two dated
// candidates the newer recorded clock wins; otherwise the rule that
// applied before this ranking existed still decides — newer file, then
// the lexicographically larger name, so the choice never depends on
// directory iteration order.
func (c dirCandidate) beats(other dirCandidate) bool {
	switch {
	case c.dated != other.dated:
		return c.dated
	case c.dated && !c.clock.Equal(other.clock):
		return c.clock.After(other.clock)
	case !c.mtime.Equal(other.mtime):
		return c.mtime.After(other.mtime)
	default:
		return c.name > other.name
	}
}

// copyInto streams a file's bytes into h.
func copyInto(h io.Writer, path string) *protoError {
	f, err := os.Open(path)
	if err != nil {
		return protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return nil
}

// fileChecksum streams the artifact once; the hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored.
func fileChecksum(path string) (string, *protoError) {
	h := sha256.New()
	if perr := copyInto(h, path); perr != nil {
		return "", perr
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}
