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
	// gzip reports whether the artifact is gzip-compressed (mongodump
	// --archive --gzip), sniffed from the magic bytes so the restore flags
	// always match the actual bytes — a drill must not fail over a
	// mislabeled but valid backup.
	gzip bool
	// createdAt is the artifact's modification time (RFC 3339 UTC,
	// milliseconds) — the closest derivable stand-in for the backup's own
	// creation time; nil if unavailable.
	createdAt *string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	mongodump             — path is a mongodump --archive file (plain or --gzip)
//	mongodump_dir         — path is a directory; the newest regular file is chosen
//	mongodump_with_users  — path is an archive taken with --dumpDbUsersAndRoles
//	mongodump_with_oplog  — path is a full archive taken with --oplog
//
// The two account/consistency kinds resolve exactly like mongodump: unlike
// the sibling adapters' two-member kinds, mongodump carries users, roles,
// and the oplog inside the *same* archive, so there is no second file and
// the backup identity stays one artifact's checksum. What changes is how
// the archive is replayed and what the drill then proves — which is why
// they are distinct kinds rather than options: backup.kind is the only
// field an auditor can read the difference from.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "mongodump", "mongodump_with_users", "mongodump_with_oplog":
		return resolveFile(path)
	case "mongodump_dir":
		latest, perr := latestDumpIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: mongodump, mongodump_dir, "+
				"mongodump_with_users, mongodump_with_oplog)", kind)
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
			"source path %s is a directory; use kind mongodump_dir for directories", path)
	}
	checksum, gzip, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	created := info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
	return &resolvedSource{
		path:      path,
		checksum:  checksum,
		sizeBytes: info.Size(),
		gzip:      gzip,
		createdAt: &created,
	}, nil
}

// latestDumpIn picks the newest regular file in dir; ties break toward the
// lexicographically larger name so the choice is deterministic.
func latestDumpIn(ctx context.Context, dir string) (string, *protoError) {
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
		if !e.Type().IsRegular() {
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

// fileChecksum streams the artifact once, hashing it and sniffing the gzip
// magic from the first two bytes. The hash feeds the evidence record's
// backup identity, so it must be a real measurement of the bytes that will
// be restored.
func fileChecksum(path string) (string, bool, *protoError) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	var magic [2]byte
	n, cerr := io.ReadFull(f, magic[:])
	if errors.Is(cerr, io.EOF) || errors.Is(cerr, io.ErrUnexpectedEOF) {
		cerr = nil // shorter than two bytes: not gzip, still hashable
	}
	gzip := n == 2 && magic[0] == 0x1f && magic[1] == 0x8b
	h := sha256.New()
	h.Write(magic[:n])
	if cerr == nil {
		_, cerr = io.Copy(h, f)
	}
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", false, protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), gzip, nil
}
