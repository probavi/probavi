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
	marker string // binlogsPath is the directory of binary logs to replay after the
	// physical restore, for the mariadb_backup_with_binlogs kind; empty
	// for every other kind (binlog.go).
	binlogsPath string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	mariadb_dump     — path is a mariadb-dump (or mysqldump) SQL file
//	mariadb_dump_dir — path is a directory; which member is chosen is
//	                   params.select, newest by default (selection.go)
//	mariadb_backup   — path is a mariadb-backup full-backup directory
func resolveSource(ctx context.Context, kind, path string, params map[string]string) (*resolvedSource, *protoError) {
	loc, perr := backupLocation(params)
	if perr != nil {
		return nil, perr
	}
	policy, perr := backupSelection(kind, params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "mariadb_dump":
		return resolveFile(ctx, path, loc)
	case "mariadb_dump_dir":
		chosen, perr := chosenDumpIn(ctx, path, policy)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(ctx, chosen, loc)
	case "mariadb_backup_with_binlogs":
		return resolveWithBinlogs(path, params, loc)
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

// resolveWithBinlogs plans the two-member source of the
// mariadb_backup_with_binlogs kind: a physical full and the binary logs
// written after it, both inside one source directory.
//
// One directory rather than two independent paths because the core only
// hands an adapter files belonging to the drill's configured backup source
// (protocol §4.2) — a guard that exists so an adapter, which is a
// third-party binary, cannot copy arbitrary host files into a sandbox it
// controls. A server's live binary log directory is therefore not
// something a drill can point at: an archive copies the logs beside the
// full they belong to, which is the layout a run-book wants anyway.
//
// Both members are named explicitly in params rather than recognised by
// layout: renaming a directory must not silently change what a drill
// proves, and the same source directory may hold several nights' fulls.
func resolveWithBinlogs(dir string, params map[string]string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the mariadb_backup_with_binlogs kind expects a directory "+
				"holding the backup and the binary logs", dir)
	}

	backupName, perr := memberName(params["backup"], "backup", "mariadb-backup directory")
	if perr != nil {
		return nil, perr
	}
	binlogsName, perr := memberName(params["binlogs"], "binlogs", "binary log directory")
	if perr != nil {
		return nil, perr
	}
	if backupName == binlogsName {
		return nil, protoErr("invalid_request", false,
			"source.params.backup and source.params.binlogs both name %s", backupName)
	}

	backupPath := filepath.Join(dir, backupName)
	if perr := mustBeDirectory(backupPath, "backup"); perr != nil {
		return nil, perr
	}
	// The same two names source.go accepts for the single-backup kind: the
	// 11.0 rename is a fact about the release that took the backup, not
	// about the kind that restores it.
	if !anyExists(backupPath, "mariadb_backup_checkpoints", "xtrabackup_checkpoints") {
		return nil, protoErr("source_corrupt", false,
			"backup directory %s lacks mariadb_backup_checkpoints (and the pre-11 "+
				"xtrabackup_checkpoints) — not a mariadb-backup backup", backupPath)
	}
	binlogsPath := filepath.Join(dir, binlogsName)
	if perr := mustBeDirectory(binlogsPath, "binary log"); perr != nil {
		return nil, perr
	}

	// Both members are restored, so both are in the identity: a checksum
	// covering only the full would let a log change without the evidence
	// record noticing, and the logs are exactly what this kind exists to
	// prove. Each member contributes its own canonical tree digest rather
	// than its bytes a second time — dirChecksum is already a measurement
	// of every byte under it, and re-streaming two trees would double the
	// read for no added guarantee. The framing is the two-member one the
	// rest of the catalogue uses (role NUL size NUL value, fixed order).
	backupSum, backupSize, perr := dirChecksum(backupPath)
	if perr != nil {
		return nil, perr
	}
	binlogSum, binlogSize, perr := dirChecksum(binlogsPath)
	if perr != nil {
		return nil, perr
	}
	h := sha256.New()
	for _, m := range []struct {
		role  string
		size  int64
		value string
	}{
		{"backup", backupSize, backupSum},
		{"binlogs", binlogSize, binlogSum},
	} {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", m.role, m.size, m.value)
	}

	// The full dates this source, through its own backup metadata. The
	// logs reach further forward in time and the record does not claim
	// otherwise: backup.created_at is when the backup was taken, and how
	// far the replay carried it is what drill.pitr_target records.
	return &resolvedSource{
		path:        backupPath,
		checksum:    fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))),
		sizeBytes:   backupSize + binlogSize,
		createdAt:   backupCreatedAt(backupPath, loc),
		binlogsPath: binlogsPath,
	}, nil
}

// memberName validates a params entry naming a directory inside the source
// directory. It is a bare name, never a path: the core's put_file guard
// confines transfers to the configured backup source, and a plain name
// keeps a config's reach obvious to whoever reviews it.
func memberName(value, param, what string) (string, *protoError) {
	if value == "" {
		return "", protoErr("invalid_request", false,
			"the mariadb_backup_with_binlogs kind requires source.params.%s: the name of the %s "+
				"inside the source directory", param, what)
	}
	if value != filepath.Base(value) || value == "." || value == ".." {
		return "", protoErr("invalid_request", false,
			"source.params.%s must be a name inside the source directory, not a path: %s",
			param, value)
	}
	return value, nil
}

// mustBeDirectory refuses a member that is not a directory; what names it
// in the diagnostic.
func mustBeDirectory(path, what string) *protoError {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return protoErr("source_not_found", false, "%s directory does not exist: %s", what, path)
	case err != nil:
		return protoErr("source_unreadable", false, "stat %s directory: %v", what, err)
	case !info.IsDir():
		return protoErr("invalid_request", false, "%s %s is a file, not a directory", what, path)
	}
	return nil
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

// chosenDumpIn picks the dump in dir that the policy asks for (see
// chooseBackupIn and selection.go).
func chosenDumpIn(ctx context.Context, dir string, policy selectPolicy) (string, *protoError) {
	best, perr := chooseBackupIn(ctx, dir, "", policy)
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

// chooseBackupIn returns the backup a directory source should restore
// under policy, skipping the entry named except. An empty result means
// the directory is readable but holds no candidate — the caller says what
// that means.
//
// Candidates are ordered by the time the backup records about itself, not
// by the file's modification time. A backup copied into the directory
// afterwards — cp without -p, an object-store download, an rsync without
// -t — carries a fresh mtime, so under the old rule a stale artifact
// became "the newest file" and was the one the drill proved. What a
// backup says about itself does not move when the file is copied.
//
// A compressed candidate has to be decompressed to reach that sentence
// (see readDumpTail), which makes ordering a directory of compressed
// dumps cost one pass over each candidate. The rule is what matters here
// and it stays one rule for both storage forms and for every policy; the
// adapter README states the price so an operator can see it before
// pointing a drill at a directory.
func chooseBackupIn(ctx context.Context, dir, except string, policy selectPolicy) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	candidates := make([]dirCandidate, 0, len(entries))
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
		candidates = append(candidates, dirCandidate{
			path: path, name: e.Name(), clock: clock, dated: dated, mtime: info.ModTime(),
		})
	}
	if len(candidates) == 0 {
		return "", nil
	}
	return pick(candidates, policy).path, nil
}

// dirCandidate is one file a directory source could restore, with the two
// times that can order it.
type dirCandidate struct {
	path  string
	name  string
	clock time.Time // what the backup records about itself
	dated bool      // whether that clock could be read at all
	mtime time.Time
}

// beats orders two candidates for the newest policy. A backup that can be
// dated from its own bytes wins over one that cannot: the drill would
// rather restore the backup it can also say something true about. Between
// two dated candidates the newer recorded clock wins; otherwise the rule
// that applied before this ordering existed still decides — newer file,
// then the lexicographically larger name, so the choice never depends on
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

// precedes orders two candidates for the oldest policy — and is not the
// negation of beats, which is the whole reason it is written out. The
// first rule does not invert: datedness is not a clock, so a backup that
// cannot be dated at all is not "the oldest one", it is the one this
// adapter can say least about, and under either policy it loses to a
// candidate carrying its own time. Only the three comparisons after it
// turn around.
func (c dirCandidate) precedes(other dirCandidate) bool {
	switch {
	case c.dated != other.dated:
		return c.dated
	case c.dated && !c.clock.Equal(other.clock):
		return c.clock.Before(other.clock)
	case !c.mtime.Equal(other.mtime):
		return c.mtime.Before(other.mtime)
	default:
		return c.name < other.name
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
