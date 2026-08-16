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
	checksum  string // "sha256:<hex>" over the artifact bytes (or tree)
	sizeBytes int64
	// export reports that the artifact is an EXPORT DATABASE directory to
	// import rather than a database file to place.
	export bool
	// header is what a database-file artifact's own head states; the zero
	// value for export directories and unparseable heads.
	header duckHeader
}

// resolveSource maps a source kind to one restorable artifact.
//
//	duckdb_db     — path is one database file (a copy of a cleanly closed
//	                database, or one written next to production data and
//	                closed)
//	duckdb_db_dir — path is a directory of them; the newest by file time
//	                is restored
//	duckdb_export — path is one `EXPORT DATABASE` directory (schema.sql,
//	                load.sql and one data file per table, CSV or Parquet)
//
// No artifact form records when it was taken — the database header
// carries checksums and version fields only, and an export is undated SQL
// plus data files (both measured) — so created_at is always null and the
// directory kind ranks by modification time, the etcd precedent.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "duckdb_db":
		if perr := refuseDirectoryForFileKind(path); perr != nil {
			return nil, perr
		}
		return resolveDatabase(path)
	case "duckdb_db_dir":
		latest, perr := latestDatabaseIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveDatabase(latest)
	case "duckdb_export":
		return resolveExport(path)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: duckdb_db, duckdb_db_dir, duckdb_export)", kind)
	}
}

// refuseDirectoryForFileKind tells the two directory-shaped kinds apart
// in the refusal, because a directory can be either of them.
func refuseDirectoryForFileKind(path string) *protoError {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return protoErr("invalid_request", false,
			"source path %s is a directory; use kind duckdb_db_dir for a directory of database "+
				"files, or duckdb_export for an EXPORT DATABASE directory", path)
	}
	return nil
}

// resolveDatabase vets a database-file artifact with the checks only this
// side of the transfer can make; everything else — corruption above all —
// is duckdb's verdict inside the sandbox, whose block checksums make any
// read fail loudly (measured). Every refusal precedes the checksum, so
// nothing is hashed that will not be restored.
func resolveDatabase(path string) (*resolvedSource, *protoError) {
	info, head, perr := statAndHead(path)
	if perr != nil {
		return nil, perr
	}
	if perr := refuseGzip(head); perr != nil {
		return nil, perr
	}
	if perr := refuseLiveCopy(path); perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: checksum, sizeBytes: info.Size(),
		header: parseDuckHeader(head),
	}, nil
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
		"backup source is gzip-compressed; this adapter restores plain DuckDB database files — "+
			"decompress the artifact first, or point the drill at an uncompressed copy")
}

// refuseLiveCopy refuses the false green a file-copy backup invites: a
// database file copied while a connection was writing. The evidence is
// the non-empty .wal sibling DuckDB maintains between checkpoints — the
// database file alone opens cleanly and silently misses every
// transaction still in the write-ahead log (measured: 500 rows where the
// live database holds 505), and a clean close checkpoints and removes the
// file (measured), so a well-taken copy never trips this. Absence proves
// nothing and stays silent: a live copy taken without its sibling is
// indistinguishable from a cold one, which is why the README recommends
// copying only closed databases or drilling EXPORT DATABASE artifacts.
func refuseLiveCopy(path string) *protoError {
	info, err := os.Stat(path + ".wal")
	if err != nil || info.IsDir() || info.Size() == 0 {
		return nil
	}
	return protoErr("unsupported_source", false,
		"a non-empty %s.wal sits beside the backup: the database was copied while a connection "+
			"was writing, and the database file alone silently misses every transaction still in "+
			"the write-ahead log — this adapter restores self-contained artifacts, so copy the "+
			"database only after it is closed, or drill an EXPORT DATABASE directory instead",
		filepath.Base(path))
}

// resolveExport vets an EXPORT DATABASE directory. Every export writes
// schema.sql and load.sql (measured — the export of an empty database is
// exactly those two files), so their absence is refused before a byte is
// transferred, the mariadb adapter's checkpoints-file precedent; a
// missing data file is left for the import, which names it loudly
// (measured).
func resolveExport(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the duckdb_export kind expects an EXPORT DATABASE "+
				"directory — use kind duckdb_db for database files", dir)
	}
	for _, name := range []string{"schema.sql", "load.sql"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return nil, protoErr("source_corrupt", false,
				"backup directory %s lacks %s, which every EXPORT DATABASE writes — "+
					"not an export directory", dir, name)
		}
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: dir, checksum: checksum, sizeBytes: size, export: true}, nil
}

// latestDatabaseIn picks the directory's newest database file. The header
// records no wall clock (measured), so file modification time is the only
// rank available — the etcd precedent, and the README says so. Files
// without the DUCK magic are skipped as non-candidates (checksum
// sidecars, README files, and the .wal siblings themselves, which carry
// their own format); the file the ranking chooses still faces every
// single-file gate, so a live copy that wins the ranking is refused by
// name rather than silently passed over — the same not-a-filter principle
// as settle.go.
func latestDatabaseIn(ctx context.Context, dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best string
	var bestInfo os.FileInfo
	skipped := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		head, err := readHead(path, duckMagicOffset+len(duckMagic))
		if err != nil {
			return "", protoErr("source_unreadable", false, "read %s: %v", e.Name(), err)
		}
		if !hasDuckMagic(head) {
			skipped++
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		if beats(info, e.Name(), bestInfo, filepath.Base(best)) {
			best, bestInfo = path, info
		}
	}
	switch {
	case best != "":
	case skipped > 0:
		return "", protoErr("source_not_found", false,
			"backup directory %s holds no DuckDB database files (%d files without the DUCK magic "+
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

// beats orders two directory candidates: newer modification time wins,
// then the lexicographically larger name, so the choice never depends on
// directory iteration order.
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

// exportFiles lists the export directory's regular files for transfer,
// sorted by ReadDir. Exports are flat (measured: multi-schema tables
// flatten into the file name), so nothing recurses.
func exportFiles(dir string) ([]string, *protoError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names, nil
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
		return "", 0, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	if files == 0 {
		return "", 0, protoErr("source_not_found", false, "backup directory %s contains no files", root)
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
		return copyFileInto(h, path)
	case d.Type()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00L%s\x00", rel, target)
	}
	return nil
}

func copyFileInto(h io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	return cerr
}

// fileChecksum streams the artifact once. The hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored.
func fileChecksum(path string) (string, *protoError) {
	h := sha256.New()
	if err := copyFileInto(h, path); err != nil {
		return "", protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// wall-clock formats sibling adapters read need it; a DuckDB artifact
// records no clock at all — the database header carries checksums and
// version fields only, and an export is undated SQL plus data files (both
// measured) — so this adapter only refuses the declaration: an operator
// who wrote it expects an accuracy no artifact here can deliver, and
// silence would hide that.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this adapter cannot honour.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: neither a database file nor an export "+
			"records when it was taken, so backup.created_at stays empty", backupTimezoneParam)
}
