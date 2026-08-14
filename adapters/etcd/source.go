package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
}

// resolveSource maps a source kind to one restorable artifact.
//
//	etcd_snapshot     — path is one snapshot file from `etcdctl snapshot save`
//	etcd_snapshot_dir — path is a directory of them; the newest file is
//	                    restored (a snapshot carries no timestamp of its own)
//
// An etcd snapshot is a bbolt database file. It records revisions and
// raft terms about the cluster, and nothing about the wall clock of the
// moment it was taken (measured: `etcdutl snapshot status` reports hash,
// revision, key count and size only). That has two consequences the
// mongodb adapter's archive kinds already established: created_at is
// always null rather than an mtime that dates a copy, and a directory is
// ranked by mtime because there is nothing better to rank by.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "etcd_snapshot":
		return resolveFile(path)
	case "etcd_snapshot_dir":
		latest, perr := latestSnapshotIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: etcd_snapshot, etcd_snapshot_dir)", kind)
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
			"source path %s is a directory; use kind etcd_snapshot_dir for directories", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}, nil
}

// latestSnapshotIn picks the newest regular file in dir; ties break toward
// the lexicographically larger name so the choice is deterministic.
func latestSnapshotIn(ctx context.Context, dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best string
	var bestInfo os.FileInfo
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		if best == "" || info.ModTime().After(bestInfo.ModTime()) ||
			(info.ModTime().Equal(bestInfo.ModTime()) && e.Name() > filepath.Base(best)) {
			best, bestInfo = filepath.Join(dir, e.Name()), info
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
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// sibling adapters read it to place a backup's own wall clock in a zone;
// an etcd snapshot records no wall clock at all, so this adapter only
// refuses it — an operator who wrote it is expecting an accuracy this
// format cannot deliver, and silence would hide that.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this adapter cannot honour.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: an etcd snapshot records revisions and "+
			"raft terms, not the wall clock it was taken at, so backup.created_at stays empty",
		backupTimezoneParam)
}
