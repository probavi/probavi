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
	// sql reports that the artifact is SQL text to replay rather than a
	// database file to place (the dump kinds).
	sql bool
}

// resolveSource maps a source kind to one restorable artifact.
//
//	sqlite_db       — path is one database file produced by `sqlite3
//	                  .backup` or `VACUUM INTO` (or a copy of a cleanly
//	                  closed database)
//	sqlite_db_dir   — path is a directory of them; the newest by file
//	                  time is restored
//	sqlite_dump     — path is SQL text from `sqlite3 .dump`
//	sqlite_dump_dir — path is a directory of dumps; the newest by file
//	                  time is restored
//
// Neither artifact form records when it was taken — the database header
// carries format and version fields only, and a dump is undated SQL text
// (both measured) — so created_at is always null and directories rank by
// modification time, the etcd adapter's precedent.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "sqlite_db":
		if perr := refuseDirectory(path, "sqlite_db_dir"); perr != nil {
			return nil, perr
		}
		return resolveDatabase(path)
	case "sqlite_db_dir":
		latest, perr := latestDatabaseIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveDatabase(latest)
	case "sqlite_dump":
		if perr := refuseDirectory(path, "sqlite_dump_dir"); perr != nil {
			return nil, perr
		}
		return resolveDump(path)
	case "sqlite_dump_dir":
		latest, perr := latestDumpIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveDump(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: sqlite_db, sqlite_db_dir, sqlite_dump, sqlite_dump_dir)", kind)
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

// resolveDatabase vets a database-file artifact with the checks only this
// side of the transfer can make; everything else — corruption above all —
// is sqlite3's verdict inside the sandbox. Every refusal precedes the
// checksum, so nothing is hashed that will not be restored.
func resolveDatabase(path string) (*resolvedSource, *protoError) {
	info, head, perr := statAndHead(path)
	if perr != nil {
		return nil, perr
	}
	if perr := refuseGzip(head, "SQLite database files"); perr != nil {
		return nil, perr
	}
	if perr := refuseDumpTextAsDatabase(head); perr != nil {
		return nil, perr
	}
	if perr := refuseEmptyDatabase(info.Size()); perr != nil {
		return nil, perr
	}
	if perr := refuseLiveCopy(path); perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}, nil
}

// resolveDump vets an SQL-text artifact the same way: by name where its
// own bytes give positive evidence, and not at all otherwise — generic
// SQL text that never came from `.dump` is legitimate input, and the
// replay inside the sandbox is its judge.
func resolveDump(path string) (*resolvedSource, *protoError) {
	info, head, perr := statAndHead(path)
	if perr != nil {
		return nil, perr
	}
	if perr := refuseGzip(head, "SQL text"); perr != nil {
		return nil, perr
	}
	if perr := refuseDatabaseAsDump(head); perr != nil {
		return nil, perr
	}
	if perr := refuseEmptyDump(info.Size()); perr != nil {
		return nil, perr
	}
	if perr := refuseTruncatedDump(path, head); perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size(), sql: true}, nil
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

func refuseGzip(head []byte, restores string) *protoError {
	if !isGzip(head) {
		return nil
	}
	return protoErr("unsupported_source", false,
		"backup source is gzip-compressed; this adapter restores plain %s — "+
			"decompress the artifact first, or point the drill at an uncompressed copy", restores)
}

// refuseDumpTextAsDatabase catches the likeliest kind mix-up by name: a
// file opening with `sqlite3 .dump`'s exact signature is SQL text, and
// the database kinds would only earn sqlite3's "file is not a database"
// for it minutes later. Positive evidence only — SQL text without the
// signature stays for the sandbox to judge.
func refuseDumpTextAsDatabase(head []byte) *protoError {
	if !hasDumpSignature(head) {
		return nil
	}
	return protoErr("invalid_request", false,
		"the source opens with the exact signature sqlite3 .dump writes — it is SQL text, "+
			"not a database file; use kind sqlite_dump for this artifact")
}

// refuseDatabaseAsDump is the same mix-up in the other direction.
func refuseDatabaseAsDump(head []byte) *protoError {
	if !hasSQLiteMagic(head) {
		return nil
	}
	return protoErr("invalid_request", false,
		"the source carries the SQLite database magic — it is a database file, "+
			"not SQL text; use kind sqlite_db for this artifact")
}

// refuseEmptyDatabase refuses a zero-byte artifact. This cannot be left
// to the sandbox: sqlite3 treats an empty file as a valid empty database,
// so both PRAGMA integrity_check and the healthcheck pass it (measured) —
// while no backup procedure ever produces one, because even a database
// with no schema backs up as a full header page (measured: 4096 bytes). A
// zero-byte file is a broken backup job wearing a database's name.
func refuseEmptyDatabase(size int64) *protoError {
	if size > 0 {
		return nil
	}
	return protoErr("source_corrupt", false,
		"the backup file is empty: sqlite3 would accept it as a database holding nothing, "+
			"but no backup of a real database is ever 0 bytes — the job that wrote it failed")
}

// refuseEmptyDump mirrors refuseEmptyDatabase for SQL text: even the dump
// of an empty database is three lines ending in COMMIT; (measured).
func refuseEmptyDump(size int64) *protoError {
	if size > 0 {
		return nil
	}
	return protoErr("source_corrupt", false,
		"the backup file is empty: replaying it would exit 0 and prove a database with nothing "+
			"in it, and no .dump output is ever 0 bytes — the job that wrote it failed")
}

// refuseLiveCopy refuses the false green this adapter exists to prevent:
// a database file copied while it was open. The evidence is the sibling
// file SQLite only maintains around a live or crashed database — a
// non-empty -wal, whose committed transactions the main file alone
// silently misses while passing integrity_check (measured), or a
// non-empty -journal, proof the copy caught a rollback-mode write
// mid-flight. A clean close removes the -wal (measured), and `.backup` /
// `VACUUM INTO` never produce siblings, so a well-taken backup never
// trips this. Absence proves nothing and stays silent: a live copy whose
// siblings were not carried along is indistinguishable from a cold copy,
// which is why the README pushes the self-contained forms.
func refuseLiveCopy(path string) *protoError {
	for _, s := range []struct{ suffix, meaning string }{
		{"-wal", "committed transactions still sit in the write-ahead log, and the database file " +
			"alone silently misses every one of them"},
		{"-journal", "a rollback-journal write was in flight, and the copy may hold a state no " +
			"transaction ever committed"},
	} {
		info, err := os.Stat(path + s.suffix)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		return protoErr("unsupported_source", false,
			"a non-empty %s sits beside the backup: the database was copied while it was open — %s; "+
				"this adapter restores self-contained artifacts, so take the backup with "+
				"sqlite3 .backup or VACUUM INTO", filepath.Base(path)+s.suffix, s.meaning)
	}
	return nil
}

// dumpTrailer is the transaction close every `.dump` ends with — measured
// on every verified version, the dump of an empty database included; and
// because `.dump` escapes embedded newlines in values (measured), no data
// line can ever equal it.
const dumpTrailer = "COMMIT;"

// refuseTruncatedDump is the gate sqlite3 itself cannot be: replaying a
// dump whose tail was lost between statements exits 0 and leaves an empty
// database — the wrapping transaction never commits, and the implicit
// rollback erases every row without a word (measured). Positive evidence
// only: the gate applies exactly to files that open with the .dump
// signature, whose contract includes the trailer; generic SQL text skips
// it and the replay speaks for itself.
func refuseTruncatedDump(path string, head []byte) *protoError {
	if !hasDumpSignature(head) {
		return nil
	}
	last, err := lastNonEmptyLine(path)
	if err != nil {
		return protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	if last == dumpTrailer {
		return nil
	}
	return protoErr("source_corrupt", false,
		"the dump opens with sqlite3 .dump's signature but does not end with its %s trailer: "+
			"the file was truncated, and replaying it would exit 0 and leave an empty database "+
			"because the wrapping transaction never commits — take the backup again", dumpTrailer)
}

// latestDatabaseIn picks the directory's newest database file. The header
// records no wall clock (measured), so file modification time is the only
// rank available — the etcd precedent, and the README says so. Files
// without the SQLite magic are skipped as non-candidates (checksum
// sidecars, README files, and the -wal/-journal siblings themselves,
// which carry their own formats); the file the ranking chooses still
// faces every single-file gate, so a live copy that wins the ranking is
// refused by name rather than silently passed over — the same
// not-a-filter principle as settle.go.
func latestDatabaseIn(ctx context.Context, dir string) (string, *protoError) {
	best, skipped, perr := newestWhere(dir, func(path string) (bool, *protoError) {
		head, err := readHead(path, len(sqliteMagic))
		if err != nil {
			return false, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
		}
		return hasSQLiteMagic(head), nil
	})
	if perr != nil {
		return "", perr
	}
	switch {
	case best != "":
	case skipped > 0:
		return "", protoErr("source_not_found", false,
			"backup directory %s holds no SQLite database files (%d files without the SQLite magic "+
				"were passed over)", dir, skipped)
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

// latestDumpIn picks the directory's newest file outright: SQL text has
// no magic to filter candidates by, and skipping files that merely do not
// look like dumps would silently prove an older neighbour whenever the
// newest artifact is the odd one — the mariadb adapter's precedent is to
// rank every regular file and let the chosen one face the single-file
// gates by name.
func latestDumpIn(ctx context.Context, dir string) (string, *protoError) {
	best, _, perr := newestWhere(dir, func(string) (bool, *protoError) { return true, nil })
	if perr != nil {
		return "", perr
	}
	if best == "" {
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
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
// wall-clock formats sibling adapters read need it; an SQLite artifact
// records no clock at all — the database header carries format and
// version fields only, and a dump is undated SQL text (both measured) —
// so this adapter only refuses the declaration: an operator who wrote it
// expects an accuracy no artifact here can deliver, and silence would
// hide that.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this adapter cannot honour.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: neither a database file nor a dump "+
			"records when it was taken, so backup.created_at stays empty", backupTimezoneParam)
}
