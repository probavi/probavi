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
	// kind classifies what the restore has to do with it.
	form sourceForm
	// batches is how many document batches a couchbackup file holds. The
	// restore loop is held to this number: every batch must be accepted,
	// or the restore did not finish.
	batches int
}

// sourceForm is the shape of a resolved artifact.
type sourceForm int

const (
	// formBackup is a couchbackup file: a header line and one JSON array
	// of documents per line after it.
	formBackup sourceForm = iota
	// formDataDir is a copy of CouchDB's own data directory.
	formDataDir
	// formDataTar is a tar of one.
	formDataTar
)

// resolveSource maps a source kind to one restorable artifact.
//
//	couchbackup        — one `couchbackup` file
//	couchbackup_dir    — a directory of them; the newest by file time is
//	                     restored
//	couchdb_data       — one copy of CouchDB's data directory (the tree
//	                     holding _dbs.couch, _nodes.couch and shards/)
//	couchdb_data_tar   — a tar of one
//
// No CouchDB artifact records when it was taken — a couchbackup header
// names the tool and mode but carries no clock, and the shard filenames
// carry the database's own creation instant rather than the backup's
// (both measured) — so created_at is always null and directories rank by
// modification time, the etcd adapter's precedent.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "couchbackup":
		if perr := refuseDirectory(path, "couchbackup_dir"); perr != nil {
			return nil, perr
		}
		return resolveBackup(path)
	case "couchbackup_dir":
		latest, perr := latestIn(ctx, path, candidateBackup)
		if perr != nil {
			return nil, perr
		}
		return resolveBackup(latest)
	case "couchdb_data":
		return resolveDataDir(path)
	case "couchdb_data_tar":
		if perr := refuseDirectory(path, "couchdb_data"); perr != nil {
			return nil, perr
		}
		return resolveDataTar(path)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: couchbackup, couchbackup_dir, "+
				"couchdb_data, couchdb_data_tar)", kind)
	}
}

func refuseDirectory(path, other string) *protoError {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return protoErr("invalid_request", false,
			"source path %s is a directory; use kind %s for directories", path, other)
	}
	return nil
}

// resolveBackup vets a couchbackup artifact. The header line is positive
// evidence that this is one, and the batch scan is the only completeness
// check the format allows: couchbackup writes nothing at the end, so a
// file cut between two lines is a shorter backup as far as any reader can
// tell, while one cut inside a line ends without a newline and is caught
// here rather than halfway through a restore.
func resolveBackup(path string) (*resolvedSource, *protoError) {
	info, head, perr := statAndHead(path)
	if perr != nil {
		return nil, perr
	}
	if perr := refuseGzip(head); perr != nil {
		return nil, perr
	}
	if perr := refuseEmpty(info.Size(), "file"); perr != nil {
		return nil, perr
	}
	if !hasBackupSignature(head) {
		return nil, protoErr("unsupported_source", false,
			"the file does not open with couchbackup's header line: this kind restores what "+
				"couchbackup writes, and its first line names the tool")
	}
	batches, torn, err := batchLines(path)
	switch {
	case err != nil:
		return nil, protoErr("source_unreadable", false, "read backup source: %v", err)
	case torn:
		return nil, protoErr("source_corrupt", false,
			"the backup's last line has no newline: it was cut mid-batch, and replaying it "+
				"would restore part of a batch and stop")
	case batches == 0:
		return nil, protoErr("source_corrupt", false,
			"the backup holds its header line and no document batches: the job that wrote it "+
				"produced nothing to restore")
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: checksum, sizeBytes: info.Size(),
		form: formBackup, batches: batches,
	}, nil
}

// resolveDataDir vets a copy of CouchDB's data directory. The registry
// file is what makes the tree a database rather than a pile of shards:
// CouchDB reads _dbs.couch to learn which databases exist, and a tree
// without it serves nothing (measured).
func resolveDataDir(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; use kind couchbackup for a couchbackup file, or "+
				"couchdb_data_tar for a tar of a data directory", path)
	}
	if _, err := os.Stat(filepath.Join(path, registryFile)); err != nil {
		return nil, protoErr("source_corrupt", false,
			"the directory holds no %s: CouchDB reads that file to learn which databases "+
				"exist, and a tree without it serves none of them", registryFile)
	}
	size, checksum, perr := treeIdentity(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: size, form: formDataDir}, nil
}

// resolveDataTar vets a tar of a data directory. Its contents are the
// sandbox's business — tar reports its own failures — and only the one
// mix-up an operator is likely to make is refused here by name.
func resolveDataTar(path string) (*resolvedSource, *protoError) {
	info, head, perr := statAndHead(path)
	if perr != nil {
		return nil, perr
	}
	if perr := refuseGzip(head); perr != nil {
		return nil, perr
	}
	if perr := refuseEmpty(info.Size(), "archive"); perr != nil {
		return nil, perr
	}
	if hasBackupSignature(head) {
		return nil, protoErr("invalid_request", false,
			"the file opens with couchbackup's header line — it is a couchbackup file, not a "+
				"tar; use kind couchbackup for this artifact")
	}
	// Nothing else is decided here. A file that is not a tar is refused by
	// tar itself inside the sandbox, with tar's own words and before the
	// registry check that follows it — the same rule every reader in
	// sniff.go follows: speak on positive evidence, and leave the artifact
	// to the engine otherwise.
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size(), form: formDataTar}, nil
}

func statAndHead(path string) (os.FileInfo, []byte, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	}
	head, err := readHead(path, headMax)
	if err != nil {
		return nil, nil, protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	return info, head, nil
}

func refuseGzip(head []byte) *protoError {
	if !isGzip(head) {
		return nil
	}
	return protoErr("unsupported_source", false,
		"backup source is gzip-compressed; this adapter restores plain artifacts — "+
			"decompress it first, or point the drill at an uncompressed copy")
}

// refuseEmpty refuses a zero-byte artifact: no CouchDB backup is ever one,
// and the job that wrote it failed.
func refuseEmpty(size int64, what string) *protoError {
	if size > 0 {
		return nil
	}
	return protoErr("source_corrupt", false,
		"the backup %s is empty: no CouchDB backup is ever 0 bytes — the job that wrote it failed", what)
}

// candidateBackup decides which files in a directory are couchbackup
// files, by their own header line rather than by name: a backup job names
// its output whatever it likes, and the checksum sidecars and logs that
// share the directory carry no such line.
func candidateBackup(path string) (bool, *protoError) {
	head, err := readHead(path, headMax)
	if err != nil {
		return false, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
	}
	return hasBackupSignature(head), nil
}

// latestIn picks the directory's newest artifact of the given kind. No
// CouchDB artifact records when it was taken, so file modification time is
// the only rank available — the etcd precedent, and the README says so.
// The file the ranking chooses still faces every single-file gate, so an
// artifact that wins the ranking and then fails a gate is refused by name
// rather than silently passed over — the same not-a-filter principle as
// settle.go.
func latestIn(ctx context.Context, dir string, candidate func(string) (bool, *protoError)) (string, *protoError) {
	best, skipped, perr := newestWhere(dir, candidate)
	if perr != nil {
		return "", perr
	}
	switch {
	case best != "":
	case skipped > 0:
		return "", protoErr("source_not_found", false,
			"backup directory %s holds no couchbackup files (%d files without its header line "+
				"were passed over)", dir, skipped)
	default:
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	if perr := assertSettled(ctx, best, settleWindow); perr != nil {
		return "", perr
	}
	return best, nil
}

// newestWhere scans dir for the newest regular file the candidate
// predicate accepts; ties break toward the lexicographically larger name
// so the choice never depends on directory iteration order.
func newestWhere(dir string, candidate func(path string) (bool, *protoError)) (string, int, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", 0, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", 0, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best string
	var bestInfo os.FileInfo
	skipped := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ok, perr := candidate(path)
		if perr != nil {
			return "", 0, perr
		}
		if !ok {
			skipped++
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", 0, protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		if beats(info, e.Name(), bestInfo, filepath.Base(best)) {
			best, bestInfo = path, info
		}
	}
	return best, skipped, nil
}

func beats(info os.FileInfo, name string, bestInfo os.FileInfo, bestName string) bool {
	switch {
	case bestInfo == nil:
		return true
	case !info.ModTime().Equal(bestInfo.ModTime()):
		return info.ModTime().After(bestInfo.ModTime())
	default:
		return name > bestName
	}
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

// treeIdentity hashes a data directory in a defined order so the same tree
// always yields the same identity: relative path, then contents, file by
// file in sorted order. The evidence record's backup identity has to name
// the bytes restored, and for a directory that means all of them.
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
// CouchDB artifact records when it was taken, so this adapter only refuses
// the declaration: an operator who wrote it expects an accuracy no
// artifact here can deliver, and silence would hide that.
const backupTimezoneParam = "backup_timezone"

func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: no CouchDB artifact records when it "+
			"was taken, so backup.created_at stays empty", backupTimezoneParam)
}
