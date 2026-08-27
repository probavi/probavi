package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the tree
	sizeBytes int64
	// createdAt is when the engine says the backup started, read from the
	// artifact's own backup_N.properties. It is the engine's record
	// rather than a file's mtime, which would date a copy.
	createdAt *string
	// collection is the collection the artifact holds, read from the
	// artifact's own layout rather than from drill config.
	collection string
	// tarball reports that the artifact is an archive the sandbox has to
	// unpack rather than a directory to transfer as it stands.
	tarball bool
}

// resolveSource maps a source kind to one restorable artifact.
//
//	solr_backup_tar — path is one tar archive of a backup directory
//	solr_backup     — path is one Collections API backup directory
//	solr_backup_dir — path is a directory of them; the newest is chosen
//
// A backup directory is what `action=BACKUP&name=<name>` leaves behind:
// one subdirectory per collection, each holding backup_N.properties,
// shard_backup_metadata, zk_backup_N and index (measured on Solr 10).
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "solr_backup_tar":
		return resolveTar(path)
	case "solr_backup":
		return resolveBackup(path)
	case "solr_backup_dir":
		latest, perr := latestBackupIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveBackup(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: solr_backup_tar, solr_backup, solr_backup_dir)", kind)
	}
}

func resolveBackup(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; a Solr backup is the directory `action=BACKUP` writes", path)
	}
	collection, perr := soleCollection(path)
	if perr != nil {
		return nil, perr
	}
	checksum, size, perr := treeChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: checksum, sizeBytes: size,
		collection: collection, createdAt: backupStartTime(filepath.Join(path, collection)),
	}, nil
}

// resolveTar vets an archive artifact with what the host can read out of
// it in one streaming pass. The collection name and the fence both come
// from that pass, so a backup that would delete its own documents is
// refused without unpacking anything; where the stream is not tar-shaped
// the sandbox extraction is the authority, because a verdict is not a
// guess.
func resolveTar(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind solr_backup for a backup directory", path)
	}
	collection, expiring, perr := inspectBackupTar(path)
	if perr != nil {
		return nil, perr
	}
	if len(expiring) > 0 {
		return nil, expirationRefusal(expiring)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: checksum, sizeBytes: info.Size(),
		collection: collection, tarball: true,
	}, nil
}

// fileChecksum streams an archive once.
func fileChecksum(path string) (string, *protoError) {
	h := sha256.New()
	if _, perr := hashFile(h, path); perr != nil {
		return "", perr
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// startTimeLine matches the line the Collections API writes into
// backup_N.properties. Java's properties writer escapes the colons in the
// timestamp, so they are unescaped here — measured, not assumed:
//
//	startTime=2026-08-27T18\:34\:34.622561925Z
var startTimeLine = regexp.MustCompile(`(?m)^startTime=(.+)$`)

// backupStartTime reports when the engine says the backup began, or nil
// when the artifact does not say. Nothing is invented from an mtime: a
// copied directory would date the copy, and a wrong backup time in a
// signed record is worse than an absent one.
func backupStartTime(collectionDir string) *string {
	entries, err := os.ReadDir(collectionDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "backup_") && strings.HasSuffix(e.Name(), ".properties") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil
	}
	// Several backups can share a location; the highest id is this one.
	sort.Strings(names)
	raw, err := os.ReadFile(filepath.Join(collectionDir, names[len(names)-1])) //#nosec G304 -- inside the named artifact.
	if err != nil {
		return nil
	}
	m := startTimeLine.FindSubmatch(raw)
	if m == nil {
		return nil
	}
	value := strings.ReplaceAll(strings.TrimSpace(string(m[1])), `\:`, ":")
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return nil
	}
	return &value
}

// soleCollection reads the collection name out of the artifact's own
// layout. A backup of several collections at once is refused rather than
// guessed at: restoring one of them would silently prove less than the
// backup holds.
func soleCollection(path string) (string, *protoError) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return "", protoErr("source_corrupt", false,
			"%s holds no collection directory — a Solr backup contains one directory per collection", path)
	case 1:
		return names[0], nil
	default:
		return "", protoErr("unsupported_source", false,
			"%s holds %d collections (%s); this adapter restores one backup into one collection, so "+
				"point the drill at a backup of a single collection",
			path, len(names), strings.Join(names, ", "))
	}
}

// latestBackupIn picks the newest backup directory in dir; ties break
// toward the lexicographically larger name so the choice is deterministic.
func latestBackupIn(ctx context.Context, dir string) (string, *protoError) {
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
		if !e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), ierr)
		}
		if best == "" || info.ModTime().After(bestTime) ||
			(info.ModTime().Equal(bestTime) && e.Name() > filepath.Base(best)) {
			best = filepath.Join(dir, e.Name())
			bestTime = info.ModTime()
		}
	}
	if best == "" {
		return "", protoErr("source_not_found", false, "backup directory %s contains no backups", dir)
	}
	// The adapter chose this backup, not the operator: make sure a backup
	// job is not still writing it (see settle.go).
	if perr := assertSettled(ctx, best, settleWindow); perr != nil {
		return "", perr
	}
	return best, nil
}

// treeChecksum hashes every regular file in the artifact, path and
// content, in sorted order. The hash feeds the evidence record's backup
// identity, so it must be a measurement of the bytes that will be
// restored rather than of a name or a size.
func treeChecksum(root string) (string, int64, *protoError) {
	h := sha256.New()
	var total int64
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", 0, protoErr("source_unreadable", false, "walk backup directory: %v", err)
	}
	if len(paths) == 0 {
		return "", 0, protoErr("source_not_found", false, "backup directory %s contains no files", root)
	}
	sort.Strings(paths)
	for _, path := range paths {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		fmt.Fprintf(h, "%s\n", filepath.ToSlash(rel))
		n, perr := hashFile(h, path)
		if perr != nil {
			return "", 0, perr
		}
		total += n
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), total, nil
}

func hashFile(h io.Writer, path string) (int64, *protoError) {
	f, err := os.Open(path) //#nosec G304 -- a file inside the artifact the drill named.
	if err != nil {
		return 0, protoErr("source_unreadable", false, "open %s: %v", path, err)
	}
	n, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return 0, protoErr("source_unreadable", false, "read %s: %v", path, cerr)
	}
	return n, nil
}
