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
	"sort"
	"strings"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the tree
	sizeBytes int64
	// checkpointed reports whether the copy was taken while a checkpoint
	// was held. QuestDB fills .checkpoint/db while CHECKPOINT CREATE is in
	// force and empties it on CHECKPOINT RELEASE (measured on 10.0.1), so
	// the artifact states its own provenance and the adapter never has to
	// take the drill's word for it.
	checkpointed bool
	// userTables is how many table directories the artifact's db/ holds,
	// not counting the engine's own. A restore that serves none of them
	// has nothing for the drill to check.
	userTables int
}

// resolveSource maps a source kind to one restorable artifact.
//
//	questdb_checkpoint     — one data root copied while a checkpoint was held
//	questdb_checkpoint_dir — a directory of them; the newest is chosen
//	questdb_data           — one data root copied with no checkpoint held
//
// A QuestDB backup is the server's whole data root — conf, db, public and
// the .checkpoint marker — as `CHECKPOINT CREATE` … copy … `CHECKPOINT
// RELEASE` leaves it. There is no archive kind: the verified images carry
// no tar (measured), and an adapter may only place bytes that belong to
// the configured source, so nothing could unpack one.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "questdb_checkpoint":
		return resolveRoot(path, true)
	case "questdb_checkpoint_dir":
		latest, perr := latestRootIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveRoot(latest, true)
	case "questdb_data":
		return resolveRoot(path, false)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: questdb_checkpoint, questdb_checkpoint_dir, questdb_data)", kind)
	}
}

// resolveRoot reads one data root and reports what it is.
func resolveRoot(path string, requireCheckpoint bool) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"%s is a file; a QuestDB backup is the server's data root, which is a directory", path)
	}
	if _, err := os.Stat(filepath.Join(path, "db")); err != nil {
		return nil, protoErr("source_corrupt", false,
			"%s does not look like a QuestDB data root: it has no db directory", filepath.Base(path))
	}
	checkpointed, perr := heldCheckpoint(path)
	if perr != nil {
		return nil, perr
	}
	if requireCheckpoint && !checkpointed {
		return nil, protoErr("invalid_request", false,
			"%s carries no checkpoint: its .checkpoint directory is empty, which is what QuestDB leaves "+
				"after CHECKPOINT RELEASE. Copy the data root between `CHECKPOINT CREATE` and `CHECKPOINT "+
				"RELEASE`, or drill this copy as source kind questdb_data, which makes no such claim",
			filepath.Base(path))
	}
	tables, perr := countUserTables(path)
	if perr != nil {
		return nil, perr
	}
	sum, size, perr := treeChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: sum, sizeBytes: size,
		checkpointed: checkpointed, userTables: tables,
	}, nil
}

// heldCheckpoint reports whether .checkpoint holds anything. The directory
// survives a release; its contents do not, so the files are the marker and
// the directory is not.
func heldCheckpoint(root string) (bool, *protoError) {
	dir := filepath.Join(root, ".checkpoint", "db")
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, protoErr("source_unreadable", false, "read checkpoint marker: %v", err)
	}
	return len(entries) > 0, nil
}

// engineOwned reports whether a table directory belongs to the engine
// rather than to the operator. QuestDB keeps its own tables beside the
// user's — telemetry, the text import log, the query trace — and a restore
// that served only those has restored nothing worth checking.
func engineOwned(name string) bool {
	return strings.HasPrefix(name, "sys.") ||
		strings.HasPrefix(name, "telemetry") ||
		strings.HasPrefix(name, "_")
}

// countUserTables counts the table directories under db/ that the operator
// created. QuestDB names a table's directory `<name>~<id>`, and keeps its
// own registry files (tables.d.*, *.lock) beside them.
func countUserTables(root string) (int, *protoError) {
	entries, err := os.ReadDir(filepath.Join(root, "db"))
	if err != nil {
		return 0, protoErr("source_unreadable", false, "read db directory: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && !engineOwned(e.Name()) {
			n++
		}
	}
	return n, nil
}

// latestRootIn picks the newest data root in a directory of them.
//
// By file time, and that is a deliberate second choice: nothing inside a
// QuestDB backup records when it was taken. The checkpoint metadata file
// is 4 KiB of zeroes on an instance with no configured id, and no other
// file in the artifact carries the instant (measured on 10.0.1) — so the
// only date available is the one the filesystem kept.
func latestRootIn(ctx context.Context, dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if ctx.Err() != nil {
			return "", protoErr("cancelled", true, "cancelled while choosing a backup")
		}
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(candidate, "db")); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = candidate, info.ModTime()
		}
	}
	if newest == "" {
		return "", protoErr("source_not_found", false,
			"%s holds no QuestDB data root (a directory with a db subdirectory)", dir)
	}
	return newest, nil
}

// treeChecksum hashes a directory as one artifact: every regular file's
// path and bytes, in sorted path order, so the same tree always hashes the
// same way and a moved file changes the sum.
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

// hashFile streams one file into the running hash.
func hashFile(h io.Writer, path string) (int64, *protoError) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return 0, protoErr("source_unreadable", false, "open %s: %v", filepath.Base(path), err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
	}
	return n, nil
}
