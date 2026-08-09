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
	// createdAt is the artifact's modification time (RFC 3339 UTC,
	// milliseconds) — the closest derivable stand-in for the backup's own
	// creation time; nil if unavailable.
	createdAt *string
	// globalsPath is the cluster-globals script to load before the dump,
	// for the pgdump_with_globals kind; empty for every other kind.
	globalsPath string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	pgdump              — path is a pg_dump custom-format file
//	pgdump_dir          — path is a directory; the backup whose own header
//	                      records the newest time is chosen
//	pgdump_with_globals — path is a directory holding a pg_dumpall
//	                      --globals-only script (params.globals) and one dump
//	pgbackrest          — path is a pgBackRest repository directory (filesystem repo)
func resolveSource(ctx context.Context, kind, path string, params map[string]string) (*resolvedSource, *protoError) {
	loc, perr := backupLocation(params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "pgdump":
		return resolveFile(path, loc)
	case "pgdump_dir":
		latest, perr := latestDumpIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(latest, loc)
	case "pgdump_with_globals":
		return resolveWithGlobals(ctx, path, params, loc)
	case "pgbackrest":
		return resolveRepo(path, params["stanza"])
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: pgdump, pgdump_dir, pgdump_with_globals, pgbackrest)", kind)
	}
}

// resolveWithGlobals resolves the two-member source of the
// pgdump_with_globals kind: a cluster-globals script and one dump, both
// named inside one source directory.
//
// One directory rather than two independent paths because the core only
// hands an adapter files belonging to the drill's configured backup source
// (protocol §4.2) — a guard that exists so an adapter, which is a
// third-party binary, cannot copy arbitrary host files into a sandbox it
// controls. The members are named explicitly in params rather than
// recognised by filename pattern: renaming a backup file must not silently
// change what a drill proves.
//
// Both members are restored, so both must be in the backup identity — a
// checksum covering only the dump would let the globals change without the
// evidence record noticing, and the globals are exactly what the drill
// exists to prove present. Only the two chosen members are hashed, not the
// whole directory: one directory may hold the globals beside several
// databases' dumps, each drilled separately, and a drill's identity must
// cover what that drill restored and nothing else. The construction
// mirrors dirChecksum's framing (role NUL size NUL content, fixed order)
// with the member's role in place of its relative path, so the same pair
// always hashes the same and any change to either member changes the hash.
func resolveWithGlobals(ctx context.Context, dir string, params map[string]string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the pgdump_with_globals kind expects a directory "+
				"holding the globals script and the dump", dir)
	}

	globalsName, perr := memberName(params["globals"], "globals")
	if perr != nil {
		return nil, perr
	}
	globalsPath := filepath.Join(dir, globalsName)
	globals, perr := statRegularFile(globalsPath, "globals script")
	if perr != nil {
		return nil, perr
	}

	dumpPath, perr := chooseDump(ctx, dir, params["dump"], globalsName)
	if perr != nil {
		return nil, perr
	}
	dump, perr := statRegularFile(dumpPath, "backup source")
	if perr != nil {
		return nil, perr
	}

	h := sha256.New()
	for _, m := range []struct {
		role string
		path string
		info os.FileInfo
	}{
		{"globals", globalsPath, globals},
		{"dump", dumpPath, dump},
	} {
		fmt.Fprintf(h, "%s\x00%d\x00", m.role, m.info.Size())
		if perr := copyInto(h, m.path); perr != nil {
			return nil, perr
		}
	}

	// The dump's own header dates this source. The globals script carries
	// no timestamp of its own (measured: pg_dumpall --globals-only writes
	// none), so the pair's freshness rests on the member that can be
	// dated — the README says so rather than letting the field imply more.
	return &resolvedSource{
		path:        dumpPath,
		checksum:    fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))),
		sizeBytes:   globals.Size() + dump.Size(),
		createdAt:   archiveCreatedAt(dumpPath, loc),
		globalsPath: globalsPath,
	}, nil
}

// memberName validates a params entry naming a file inside the source
// directory. It is a bare filename, never a path: the core's put_file
// guard confines transfers to the configured backup source, and a plain
// name keeps a config's reach obvious to whoever reviews it.
func memberName(value, param string) (string, *protoError) {
	if value == "" {
		return "", protoErr("invalid_request", false,
			"the pgdump_with_globals kind requires source.params.%s: the name of the %s file "+
				"inside the source directory", param, param)
	}
	if value != filepath.Base(value) || value == "." || value == ".." {
		return "", protoErr("invalid_request", false,
			"source.params.%s must be a filename inside the source directory, not a path: %s",
			param, value)
	}
	return value, nil
}

// chooseDump resolves which dump the drill restores: the one params.dump
// names, or — so a drill against a rotating backup directory keeps working
// unattended — the newest file that is not the globals script.
func chooseDump(ctx context.Context, dir, requested, globalsName string) (string, *protoError) {
	if requested != "" {
		name, perr := memberName(requested, "dump")
		if perr != nil {
			return "", perr
		}
		if name == globalsName {
			return "", protoErr("invalid_request", false,
				"source.params.dump and source.params.globals both name %s", name)
		}
		return filepath.Join(dir, name), nil
	}
	newest, perr := newestBackupIn(dir, globalsName)
	if perr != nil {
		return "", perr
	}
	if newest == "" {
		return "", protoErr("source_not_found", false,
			"backup directory %s holds no dump beside the globals script %s", dir, globalsName)
	}
	// The adapter chose this file, not the operator: make sure a backup job
	// is not still writing it (see settle.go).
	if perr := assertSettled(ctx, newest, settleWindow); perr != nil {
		return "", perr
	}
	return newest, nil
}

// statRegularFile stats a source member that must exist as a plain file;
// what names it in diagnostics.
func statRegularFile(path, what string) (os.FileInfo, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "%s does not exist: %s", what, path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat %s: %v", what, err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false, "%s %s is a directory, not a file", what, path)
	}
	return info, nil
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

// resolveRepo resolves a directory source: the checksum is a canonical hash
// over the whole tree (documented in the adapter README), created_at is the
// newest file's mtime.
func resolveRepo(dir, stanza string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup repository does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup repository: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the pgbackrest kind expects a repository directory", dir)
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	// The repository dates itself: backup.info records epoch seconds, so
	// this is the one kind here that needs no declared zone (see
	// repotime.go).
	return &resolvedSource{
		path:      dir,
		checksum:  checksum,
		sizeBytes: size,
		createdAt: repoCreatedAt(dir, stanza),
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
		return "", 0, protoErr("source_unreadable", false, "read backup repository: %v", err)
	}
	if files == 0 {
		return "", 0, protoErr("source_not_found", false, "backup repository %s contains no files", root)
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

func resolveFile(path string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind pgdump_dir for directories", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path:      path,
		checksum:  checksum,
		sizeBytes: info.Size(),
		createdAt: archiveCreatedAt(path, loc),
	}, nil
}

// latestDumpIn picks the dump in dir that records the newest time about
// itself (see newestBackupIn).
func latestDumpIn(ctx context.Context, dir string) (string, *protoError) {
	best, perr := newestBackupIn(dir, "")
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
func newestBackupIn(dir, except string) (string, *protoError) {
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
		clock, dated := archiveClock(path)
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
