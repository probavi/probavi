package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// namespace is the one the backup was taken from, read from the
	// artifact's own header. The sandbox is configured to offer a
	// namespace of that name, because asrestore writes records back into
	// the namespace they name and an engine offering another fails the
	// whole batch (measured: exit 1, nothing inserted).
	namespace string
	// dir is true when the artifact is a directory asbackup filled, in
	// which case every file in it is part of the one backup.
	dir bool
}

// resolveSource maps a source kind to one restorable artifact.
//
//	asbackup      — one .asb file, as `asbackup -o` writes
//	asbackup_dir  — a directory `asbackup -d` filled; every .asb file in
//	                it is part of the same backup, and asrestore reads
//	                the directory rather than any one file
//
// No Aerospike backup records when it was taken. The header is three
// lines — the format version, the namespace, and the marker on the file
// written first — with no timestamp, no engine version and no record
// count (measured), so created_at is always null.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "asbackup":
		return resolveFile(ctx, path)
	case "asbackup_dir":
		return resolveDir(ctx, path)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: asbackup, asbackup_dir)", kind)
	}
}

// resolveFile vets one .asb file. The header is positive evidence that
// this is a backup at all, and the first-file marker is the only evidence
// the format offers that it is a whole one: asbackup splits a large backup
// across files and writes the marker into exactly one of them, so a file
// without it is a fragment whose restore would silently be partial.
func resolveFile(ctx context.Context, path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind asbackup_dir for a directory asbackup filled", path)
	}
	head, rerr := readHead(path, headMax)
	if rerr != nil {
		return nil, protoErr("source_unreadable", false, "read backup source: %v", rerr)
	}
	if perr := refuseGzip(head); perr != nil {
		return nil, perr
	}
	h, ok := parseHeader(head)
	if !ok {
		return nil, protoErr("unsupported_source", false,
			"the file does not open the way asbackup opens one: its first two lines state the "+
				"format version and the namespace, and this file's do not")
	}
	if !h.firstFile {
		return nil, protoErr("source_corrupt", false,
			"the file carries no %q marker, so it is one part of a backup asbackup split across "+
				"files rather than a whole one; point the drill at the directory holding them "+
				"with kind asbackup_dir", firstFileMarker)
	}
	if perr := assertSettled(ctx, path, settleWindow); perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: checksum, sizeBytes: info.Size(), namespace: h.namespace,
	}, nil
}

// resolveDir vets a directory asbackup filled. Every file in it must be
// part of the same backup, and exactly one must carry the first-file
// marker — which is what tells a whole backup from a directory holding
// the tail of one, or two backups' files mixed together.
func resolveDir(ctx context.Context, dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; use kind asbackup for one .asb file", dir)
	}
	scan, perr := scanDir(ctx, dir)
	if perr != nil {
		return nil, perr
	}
	files, firstFiles, skipped := scan.files, scan.firstFiles, scan.skipped
	switch {
	case files == 0 && skipped > 0:
		return nil, protoErr("source_not_found", false,
			"backup directory %s holds no asbackup files (%d files whose first lines state no "+
				"format version and namespace were passed over)", dir, skipped)
	case files == 0:
		return nil, protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	case firstFiles == 0:
		return nil, protoErr("source_corrupt", false,
			"none of the %d files carries the %q marker: asbackup writes it into the file it "+
				"wrote first, so this directory holds the tail of a backup rather than a whole one",
			files, firstFileMarker)
	case firstFiles > 1:
		return nil, protoErr("source_corrupt", false,
			"%d of the %d files carry the %q marker, so the directory holds more than one backup "+
				"and asrestore would replay them all into one engine",
			firstFiles, files, firstFileMarker)
	}
	size, checksum, perr := treeIdentity(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: dir, checksum: checksum, sizeBytes: size, namespace: scan.namespace, dir: true,
	}, nil
}

// dirScan is what one pass over a backup directory found.
type dirScan struct {
	namespace  string
	files      int
	firstFiles int
	skipped    int
}

// scanDir reads every regular file's header and refuses a directory whose
// files disagree about which backup they belong to. It counts rather than
// judges: which counts make a whole backup is resolveDir's decision.
func scanDir(ctx context.Context, dir string) (dirScan, *protoError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dirScan{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	scan := dirScan{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		head, rerr := readHead(path, headMax)
		if rerr != nil {
			return dirScan{}, protoErr("source_unreadable", false, "read %s: %v", e.Name(), rerr)
		}
		h, ok := parseHeader(head)
		if !ok {
			scan.skipped++
			continue
		}
		switch {
		case scan.namespace == "":
			scan.namespace = h.namespace
		case h.namespace != scan.namespace:
			return dirScan{}, protoErr("source_corrupt", false,
				"the directory holds files from two backups: %s names namespace %s where an "+
					"earlier file names %s, and asrestore would replay both into one engine",
				e.Name(), h.namespace, scan.namespace)
		}
		if h.firstFile {
			scan.firstFiles++
		}
		scan.files++
		if perr := assertSettled(ctx, path, settleWindow); perr != nil {
			return dirScan{}, perr
		}
	}
	return scan, nil
}

func refuseGzip(head []byte) *protoError {
	if !isGzip(head) {
		return nil
	}
	return protoErr("unsupported_source", false,
		"backup source is gzip-compressed; this adapter restores plain artifacts — decompress "+
			"it first, or point the drill at an uncompressed copy")
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
	if cerr != nil {
		return "", protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// treeIdentity hashes a backup directory in a defined order so the same
// directory always yields the same identity: relative path, then contents,
// file by file in sorted order. asbackup names its files by partition
// range and a directory is restored whole, so the identity has to name
// every byte of it — the couchdb adapter's rule, and the README states it.
func treeIdentity(root string) (int64, string, *protoError) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return 0, "", protoErr("source_unreadable", false, "walk backup source: %v", err)
	}
	slices.Sort(files)
	h := sha256.New()
	var total int64
	for _, p := range files {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return 0, "", protoErr("source_unreadable", false, "resolve %s: %v", p, err)
		}
		if _, err := fmt.Fprintf(h, "%s\n", filepath.ToSlash(rel)); err != nil {
			return 0, "", protoErr("internal", false, "hash backup source: %v", err)
		}
		f, err := os.Open(p)
		if err != nil {
			return 0, "", protoErr("source_unreadable", false, "open %s: %v", rel, err)
		}
		n, cerr := io.Copy(h, f)
		if err := f.Close(); err != nil && cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return 0, "", protoErr("source_unreadable", false, "read %s: %v", rel, cerr)
		}
		total += n
	}
	return total, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// backupTimezoneParam names the IANA zone the backup host was in. No
// Aerospike artifact records when it was taken, so this adapter only
// refuses the declaration: an operator who wrote it expects an accuracy no
// artifact here can deliver, and silence would hide that.
const backupTimezoneParam = "backup_timezone"

func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: an .asb header states the format "+
			"version and the namespace and no clock, so backup.created_at stays empty",
		backupTimezoneParam)
}
