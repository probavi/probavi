package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Source kinds this adapter accepts.
const (
	// kindData is a Chroma persistence directory, copied while the server
	// was stopped. Chroma has no backup command, so this copy is the whole
	// artifact an operator can hold.
	kindData = "chroma_data"
	// kindDataTar is a tar archive of one, plain or gzip-compressed. The
	// compression is read from the bytes, never from the file name.
	kindDataTar = "chroma_data_tar"
)

// sqliteFile is the metadata database at the root of every persistence
// directory. It is the half the engine refuses to start without (measured:
// truncating it stops the server outright), and the half that carries the
// collections, the segment declarations and the records themselves.
const sqliteFile = "chroma.sqlite3"

// sqliteMagic opens every SQLite database file, including this one.
var sqliteMagic = []byte("SQLite format 3\x00")

// rollbackJournal is what SQLite leaves beside a database copied in the
// middle of a write, in the journal mode Chroma actually uses.
//
// Which sidecar can appear is a property of that mode, and Chroma's is
// measured rather than assumed: `PRAGMA journal_mode` reports `delete` and
// the file header's bytes 18 and 19 are both 1, so this is a rollback
// journal and not WAL. Finding this file is therefore evidence about the
// copy — it was taken mid-write — and the artifact is torn.
const rollbackJournal = sqliteFile + "-journal"

// walSidecars belong to a WAL-mode database, which Chroma's is not. Finding
// either says the artifact is not the shape this adapter measured, which is
// a different statement from "torn" and gets a different code: the config
// is pointing at something this adapter cannot judge, rather than at a
// damaged backup.
var walSidecars = []string{
	sqliteFile + "-wal",
	sqliteFile + "-shm",
}

// source is what host-side inspection could establish about the artifact
// before a byte moves into the sandbox.
type source struct {
	// kind is the declared source kind.
	kind string
	// path is the artifact on the drill host.
	path string
	// checksum is the sha256 reference recorded as the backup's identity.
	checksum string
	// sizeBytes is what the checksum covered.
	sizeBytes int64
	// gzip reports whether an archive's bytes are gzip-compressed.
	gzip bool
}

// inspect reads what the artifact states about itself, refusing what this
// adapter cannot honestly judge. It never opens the SQLite database: an
// adapter is standard-library only, and a verdict read through the engine
// after the restore is worth more than one parsed beside it (see
// verifyRestore).
func inspect(kind, path string) (*source, *protoError) {
	// The kind is judged before the path: an unknown kind is unsupported
	// whether or not anything sits at the path, and answering
	// source_not_found for it would send an operator looking for a file
	// when the config names a kind this adapter has never had.
	if kind != kindData && kind != kindDataTar {
		return nil, protoErr("unsupported_source", false, "unsupported source kind %s", kind)
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, protoErr("source_not_found", false, "backup source %s does not exist", path)
	}
	if err != nil {
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	}
	switch kind {
	case kindData:
		if !info.IsDir() {
			return nil, protoErr("unsupported_source", false,
				"%s expects a Chroma persistence directory; %s is a file — use %s for an archive of one",
				kindData, path, kindDataTar)
		}
		return inspectDir(path)
	case kindDataTar:
		if info.IsDir() {
			return nil, protoErr("unsupported_source", false,
				"%s expects an archive file; %s is a directory — use %s for the directory itself",
				kindDataTar, path, kindData)
		}
		return inspectTar(path)
	default:
		return nil, protoErr("unsupported_source", false, "unsupported source kind %s", kind)
	}
}

// inspectDir judges a persistence directory: the metadata database must be
// there and must be one, and no journal sidecar may sit beside it.
func inspectDir(path string) (*source, *protoError) {
	db := filepath.Join(path, sqliteFile)
	info, err := os.Stat(db)
	if os.IsNotExist(err) {
		return nil, protoErr("unsupported_source", false,
			"%s holds no %s, so it is not a Chroma persistence directory — the artifact is the directory "+
				"`chroma run --path` was given, copied with the server stopped", path, sqliteFile)
	}
	if err != nil {
		return nil, protoErr("source_unreadable", false, "stat %s: %v", sqliteFile, err)
	}
	if !info.Mode().IsRegular() {
		return nil, protoErr("source_corrupt", false, "%s is not a regular file", sqliteFile)
	}
	if perr := checkSQLiteHeader(db); perr != nil {
		return nil, perr
	}
	if _, serr := os.Stat(filepath.Join(path, rollbackJournal)); serr == nil {
		return nil, protoErr("source_corrupt", false,
			"%s sits beside %s, so the copy caught the database in the middle of a write and the "+
				"artifact is a torn one: stop the server before copying the persistence directory",
			rollbackJournal, sqliteFile)
	}
	for _, sidecar := range walSidecars {
		if _, serr := os.Stat(filepath.Join(path, sidecar)); serr == nil {
			return nil, protoErr("unsupported_source", false,
				"%s sits beside %s, so this database is running in WAL mode. Chroma's own is not "+
					"(measured: journal_mode is `delete`), and a WAL holds writes the file does not, so "+
					"this adapter cannot say what the artifact contains", sidecar, sqliteFile)
		}
	}
	sum, size, perr := treeChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &source{kind: kindData, path: path, checksum: sum, sizeBytes: size}, nil
}

// inspectTar judges an archive by its bytes: gzip or not, and non-empty.
// What is inside it is the engine's verdict to give after extraction, not
// a claim to make from a header.
func inspectTar(path string) (*source, *protoError) {
	sum, size, gz, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	if size == 0 {
		return nil, protoErr("source_corrupt", false, "%s is empty", filepath.Base(path))
	}
	return &source{kind: kindDataTar, path: path, checksum: sum, sizeBytes: size, gzip: gz}, nil
}

// checkSQLiteHeader refuses a file that does not open like a SQLite
// database, so a directory holding something else entirely fails by name
// rather than as an engine error minutes later.
func checkSQLiteHeader(path string) *protoError {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return protoErr("source_unreadable", false, "open %s: %v", sqliteFile, err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	head := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		return protoErr("source_corrupt", false, "%s is shorter than a SQLite header", sqliteFile)
	}
	if string(head) != string(sqliteMagic) {
		return protoErr("source_corrupt", false, "%s does not begin with the SQLite file header", sqliteFile)
	}
	return nil
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

// fileChecksum hashes one file and reports whether its first bytes are the
// gzip magic. The bytes decide, never the name: an archive written by
// `tar czf backup.tar` is still gzip, and one named .tar.gz need not be.
func fileChecksum(path string) (string, int64, bool, *protoError) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", 0, false, protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	// A file shorter than two bytes is not gzip and is still hashable, so
	// a short read is not a failure here — but any other read error is.
	var magic [2]byte
	n, rerr := io.ReadFull(f, magic[:])
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return "", 0, false, protoErr("source_unreadable", false, "read backup source: %v", rerr)
	}
	head := magic[:n]
	gz := len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b
	h := sha256.New()
	h.Write(head)
	written, cerr := io.Copy(h, f)
	if cerr != nil {
		return "", 0, false, protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), written + int64(n), gz, nil
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

// looksLikeSegmentDir reports whether a directory name is a Chroma segment
// id. Used only for diagnostics: the verdict on whether the segments are
// intact comes from the engine, not from counting directories.
func looksLikeSegmentDir(name string) bool {
	if len(name) != 36 {
		return false
	}
	for i, r := range name {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}
