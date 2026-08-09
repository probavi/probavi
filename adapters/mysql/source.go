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
	// usersPath is the accounts-and-grants script to replay before the
	// dump, for the mysqldump_with_users kind; empty for every other kind.
	usersPath string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	mysqldump            — path is a mysqldump SQL file
//	mysqldump_dir        — path is a directory; the newest regular file is chosen
//	mysqldump_with_users — path is a directory holding an accounts-and-grants
//	                       script (params.users) and one dump
//	xtrabackup           — path is an XtraBackup full-backup directory
func resolveSource(ctx context.Context, kind, path string, params map[string]string) (*resolvedSource, *protoError) {
	switch kind {
	case "mysqldump":
		return resolveFile(path)
	case "mysqldump_dir":
		latest, perr := latestDumpIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(latest)
	case "mysqldump_with_users":
		return resolveWithUsers(ctx, path, params)
	case "xtrabackup":
		src, perr := resolveRepo(path)
		if perr != nil {
			return nil, perr
		}
		// Every xtrabackup backup carries this metadata file; its absence
		// means the directory is something else — refuse before a single
		// byte is transferred.
		if _, err := os.Stat(filepath.Join(path, "xtrabackup_checkpoints")); err != nil {
			return nil, protoErr("source_corrupt", false,
				"backup directory %s lacks xtrabackup_checkpoints — not an xtrabackup backup", path)
		}
		return src, nil
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: mysqldump, mysqldump_dir, mysqldump_with_users, xtrabackup)", kind)
	}
}

// resolveWithUsers resolves the two-member source of the
// mysqldump_with_users kind: an accounts-and-grants script and one dump,
// both named inside one source directory.
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
// checksum covering only the dump would let the users script change
// without the evidence record noticing, and the accounts are exactly what
// this kind exists to prove present. Only the two chosen members are
// hashed, not the whole directory: one directory may hold the script
// beside several databases' dumps, each drilled separately, and a drill's
// identity must cover what that drill restored and nothing else. The
// construction mirrors the other in-repo adapters' two-member framing
// (role NUL size NUL content, fixed order), so the same pair always hashes
// the same and any change to either member changes the hash.
func resolveWithUsers(ctx context.Context, dir string, params map[string]string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the mysqldump_with_users kind expects a directory "+
				"holding the users script and the dump", dir)
	}

	usersName, perr := memberName(params["users"], "users")
	if perr != nil {
		return nil, perr
	}
	usersPath := filepath.Join(dir, usersName)
	users, perr := statRegularFile(usersPath, "users script")
	if perr != nil {
		return nil, perr
	}

	dumpPath, perr := chooseDump(ctx, dir, params["dump"], usersName)
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
		{"users", usersPath, users},
		{"dump", dumpPath, dump},
	} {
		fmt.Fprintf(h, "%s\x00%d\x00", m.role, m.info.Size())
		if perr := copyInto(h, m.path); perr != nil {
			return nil, perr
		}
	}

	// The older mtime, not the newer: a two-member set is only as current
	// as its stalest member. A stale users script is precisely the failure
	// this kind exists to surface — an account created after the script was
	// exported is missing, and the restored objects that depend on it are
	// unusable. (mysqldump_dir takes the newest file for the opposite and
	// equally deliberate reason: a rotation directory's newest file is its
	// latest backup.)
	created := users.ModTime()
	if dump.ModTime().Before(created) {
		created = dump.ModTime()
	}
	stamp := created.UTC().Format("2006-01-02T15:04:05.000Z")
	return &resolvedSource{
		path:      dumpPath,
		checksum:  fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))),
		sizeBytes: users.Size() + dump.Size(),
		createdAt: &stamp,
		usersPath: usersPath,
	}, nil
}

// memberName validates a params entry naming a file inside the source
// directory. It is a bare filename, never a path: the core's put_file
// guard confines transfers to the configured backup source, and a plain
// name keeps a config's reach obvious to whoever reviews it.
func memberName(value, param string) (string, *protoError) {
	if value == "" {
		return "", protoErr("invalid_request", false,
			"the mysqldump_with_users kind requires source.params.%s: the name of the %s file "+
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
// unattended — the newest file that is not the users script.
func chooseDump(ctx context.Context, dir, requested, usersName string) (string, *protoError) {
	if requested != "" {
		name, perr := memberName(requested, "dump")
		if perr != nil {
			return "", perr
		}
		if name == usersName {
			return "", protoErr("invalid_request", false,
				"source.params.dump and source.params.users both name %s", name)
		}
		return filepath.Join(dir, name), nil
	}
	newest, perr := newestFileIn(dir, usersName)
	if perr != nil {
		return "", perr
	}
	if newest == "" {
		return "", protoErr("source_not_found", false,
			"backup directory %s holds no dump beside the users script %s", dir, usersName)
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

// resolveRepo resolves a directory source: the checksum is a canonical hash
// over the whole tree (documented in the adapter README), created_at is the
// newest file's mtime.
func resolveRepo(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the xtrabackup kind expects a backup directory", dir)
	}
	checksum, size, newest, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	created := newest.UTC().Format("2006-01-02T15:04:05.000Z")
	return &resolvedSource{path: dir, checksum: checksum, sizeBytes: size, createdAt: &created}, nil
}

// dirChecksum hashes a directory tree canonically: entries sorted by
// relative path; regular files contribute path, size, and content bytes,
// symlinks contribute path and target. The same tree always hashes the
// same, any content change changes the hash.
func dirChecksum(root string) (string, int64, time.Time, *protoError) {
	h := sha256.New()
	var total int64
	var newest time.Time
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return hashEntry(h, path, rel, d, &total, &newest)
	})
	if err != nil {
		return "", 0, time.Time{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	if newest.IsZero() {
		return "", 0, time.Time{}, protoErr("source_not_found", false, "backup directory %s contains no files", root)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), total, newest, nil
}

func hashEntry(h io.Writer, path, rel string, d os.DirEntry, total *int64, newest *time.Time) error {
	switch {
	case d.Type().IsRegular():
		info, err := d.Info()
		if err != nil {
			return err
		}
		*total += info.Size()
		if info.ModTime().After(*newest) {
			*newest = info.ModTime()
		}
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

func resolveFile(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind mysqldump_dir for directories", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	created := info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
	return &resolvedSource{
		path:      path,
		checksum:  checksum,
		sizeBytes: info.Size(),
		createdAt: &created,
	}, nil
}

// latestDumpIn picks the newest regular file in dir.
func latestDumpIn(ctx context.Context, dir string) (string, *protoError) {
	best, perr := newestFileIn(dir, "")
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

// newestFileIn returns the newest regular file in dir, skipping the entry
// named except; ties break toward the lexicographically larger name so the
// choice is deterministic. An empty result means the directory is readable
// but holds no candidate — the caller says what that means.
func newestFileIn(dir, except string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var (
		best     string
		bestTime time.Time
	)
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == except {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		if best == "" || info.ModTime().After(bestTime) ||
			(info.ModTime().Equal(bestTime) && e.Name() > filepath.Base(best)) {
			best = filepath.Join(dir, e.Name())
			bestTime = info.ModTime()
		}
	}
	return best, nil
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
