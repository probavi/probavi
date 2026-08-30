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

// mvStoreSuffix is the extension H2 gives the one file a database lives
// in. It names both the artifact this adapter restores and the entry a
// BACKUP TO archive carries.
const mvStoreSuffix = ".mv.db"

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// archive reports that the artifact is a BACKUP TO zip to unpack
	// rather than a database file to place.
	archive bool
}

// resolveSource maps a source kind to one restorable artifact.
//
//	h2_backup      — path is one `BACKUP TO` archive
//	h2_backup_dir  — path is a directory of them; the newest by file time
//	                 is restored
//	h2_db          — path is one <database>.mv.db file, copied while the
//	                 database was closed
//	h2_db_dir      — path is a directory of them, ranked the same way
//
// Neither form records when it was taken — the MVStore header's `created`
// field dates the database's creation, not the backup's, and it does not
// move when a backup is taken (measured) — so created_at is always null
// and directories rank by modification time, the etcd adapter's
// precedent.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "h2_backup":
		if perr := refuseDirectory(path, "h2_backup_dir"); perr != nil {
			return nil, perr
		}
		return resolveArchive(path)
	case "h2_backup_dir":
		latest, perr := latestIn(ctx, path, candidateArchive)
		if perr != nil {
			return nil, perr
		}
		return resolveArchive(latest)
	case "h2_db":
		if perr := refuseDirectory(path, "h2_db_dir"); perr != nil {
			return nil, perr
		}
		return resolveDatabase(path)
	case "h2_db_dir":
		latest, perr := latestIn(ctx, path, candidateDatabase)
		if perr != nil {
			return nil, perr
		}
		return resolveDatabase(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: h2_backup, h2_backup_dir, h2_db, h2_db_dir)", kind)
	}
}

func refuseDirectory(path, dirKind string) *protoError {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return protoErr("invalid_request", false,
			"source path %s is a directory; use kind %s for directories", path, dirKind)
	}
	return nil
}

// resolveArchive vets a BACKUP TO artifact. Opening the archive's central
// directory is both the kind check and the completeness check: a zip
// truncated anywhere loses it, so a file that answers here is whole.
func resolveArchive(path string) (*resolvedSource, *protoError) {
	info, head, perr := statAndHead(path)
	if perr != nil {
		return nil, perr
	}
	if perr := refuseEmpty(info.Size(), "archive"); perr != nil {
		return nil, perr
	}
	if hasMVStoreMagic(head) {
		return nil, protoErr("invalid_request", false,
			"the source carries the MVStore header — it is a database file, not a BACKUP TO "+
				"archive; use kind h2_db for this artifact")
	}
	if !isZip(head) {
		return nil, protoErr("unsupported_source", false,
			"the source is not a zip archive: BACKUP TO writes one, and this adapter restores "+
				"what that command produces")
	}
	holds, err := zipHoldsDatabase(path)
	switch {
	case err != nil:
		return nil, protoErr("source_corrupt", false,
			"the archive could not be read to its end — a truncated backup: %v", err)
	case !holds:
		return nil, protoErr("source_corrupt", false,
			"the archive holds no %s entry: BACKUP TO always writes the database file into it, "+
				"so this archive is not one", mvStoreSuffix)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size(), archive: true}, nil
}

// resolveDatabase vets a database-file artifact with the checks only this
// side of the transfer can make; corruption beyond the header is H2's
// verdict inside the sandbox. Every refusal precedes the checksum, so
// nothing is hashed that will not be restored.
func resolveDatabase(path string) (*resolvedSource, *protoError) {
	info, head, perr := statAndHead(path)
	if perr != nil {
		return nil, perr
	}
	if perr := refuseGzip(head); perr != nil {
		return nil, perr
	}
	if isZip(head) {
		return nil, protoErr("invalid_request", false,
			"the source is a zip archive — BACKUP TO writes one of those; "+
				"use kind h2_backup for this artifact")
	}
	if perr := refuseEmpty(info.Size(), "database file"); perr != nil {
		return nil, perr
	}
	if perr := refuseForeignFormat(head); perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}, nil
}

// refuseForeignFormat is the storage-format fence. The MVStore header
// states the format the file was written in, and the engine versions this
// adapter is verified against read format 3 only: an H2 1.4 database is
// format 1, and every 2.x engine refuses it with "Unsupported database
// file version or invalid file header", which names neither the file's
// format nor the engine's (measured). Refusing here names both, before a
// byte moves — and the message says what actually converts such a
// database, because the answer is not "a newer sandbox".
//
// Positive evidence only: a file without the MVStore header is left to
// H2, which is the authority on what it can open.
func refuseForeignFormat(head []byte) *protoError {
	if !hasMVStoreMagic(head) {
		return nil
	}
	format := storageFormat(head)
	if format == "" || format == supportedStorageFormat {
		return nil
	}
	return protoErr("unsupported_source", false,
		"the database file states MVStore format %s and the verified engines read format %s: "+
			"this is an H2 1.x database, and no 2.x engine opens one — convert it with the 1.x "+
			"engine's own SCRIPT TO output before drilling it", format, supportedStorageFormat)
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
		"backup source is gzip-compressed; this adapter restores plain database files — "+
			"decompress the artifact first, or point the drill at an uncompressed copy")
}

// refuseEmpty refuses a zero-byte artifact. This cannot be left to the
// sandbox: pointed at a path that holds nothing, H2 creates a fresh
// database and answers queries against it (measured), so an empty file
// would drill as a healthy database with no data in it.
func refuseEmpty(size int64, what string) *protoError {
	if size > 0 {
		return nil
	}
	return protoErr("source_corrupt", false,
		"the backup %s is empty: no H2 backup is ever 0 bytes — the job that wrote it failed", what)
}

// candidateArchive and candidateDatabase decide which files in a
// directory are of the kind being ranked. Both read the artifact's own
// first bytes rather than its name: a backup job names its output
// whatever it likes, and the checksum sidecars and README files that
// share the directory carry neither signature.
func candidateArchive(path string) (bool, *protoError) {
	head, err := readHead(path, len(zipMagic))
	if err != nil {
		return false, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
	}
	return isZip(head), nil
}

func candidateDatabase(path string) (bool, *protoError) {
	head, err := readHead(path, len(mvStoreMagic))
	if err != nil {
		return false, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
	}
	return hasMVStoreMagic(head), nil
}

// latestIn picks the directory's newest artifact of the given kind. No H2
// artifact records when it was taken, so file modification time is the
// only rank available — the etcd precedent, and the README says so. The
// file the ranking chooses still faces every single-file gate, so an
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
			"backup directory %s holds no artifact of this kind (%d files of other kinds were "+
				"passed over)", dir, skipped)
	default:
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	// The adapter chose this file, not the operator: make sure a backup
	// job is not still writing it (see settle.go).
	if perr := assertSettled(ctx, best, settleWindow); perr != nil {
		return "", perr
	}
	return best, nil
}

// newestWhere scans dir for the newest regular file the candidate
// predicate accepts; ties break toward the lexicographically larger name
// so the choice never depends on directory iteration order. skipped
// counts the regular files the predicate declined.
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

// beats orders two directory candidates: newer modification time wins,
// then the lexicographically larger name.
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
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// wall-clock formats sibling adapters read need it; an H2 artifact
// records no backup clock at all, so this adapter only refuses the
// declaration: an operator who wrote it expects an accuracy no artifact
// here can deliver, and silence would hide that.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this adapter cannot honour.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: neither a BACKUP TO archive nor a "+
			"database file records when it was taken, so backup.created_at stays empty",
		backupTimezoneParam)
}
