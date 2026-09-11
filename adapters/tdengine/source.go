package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	// path is the directory taosdump restores from — the inner
	// `taosdump.<serial>` tree, never the directory `-o` was given.
	path      string
	checksum  string // "sha256:<hex>" over the tree
	sizeBytes int64
	// createdAt is when taosdump says it started, read from the
	// artifact's own dump_result.txt. The engine's record, not a file's
	// mtime, which would date a copy.
	createdAt *string
	// started is createdAt as a time, for the retention fence.
	started time.Time
	// database is the database the dump holds, read from its own
	// CREATE DATABASE line rather than from drill config.
	database string
	// keep is the retention the dump declares, in days, and 0 when the
	// artifact does not say. It is the operator's own policy travelling
	// inside the backup (see fence.go).
	keepDays int
	// rows is how many rows the dump's own accounting claims, and
	// rowsKnown whether it claimed. The restore is judged against it.
	rows      int
	rowsKnown bool
	// tarball reports that the artifact is an archive the sandbox has to
	// unpack rather than a directory to transfer as it stands.
	tarball bool
}

// resolveSource maps a source kind to one restorable artifact.
//
//	taosdump     — one taosdump output directory
//	taosdump_dir — a directory of them; the newest is chosen
//	taosdump_tar — one tar archive of a dump directory
//
// `taosdump -o DIR` writes DIR/dump_result.txt, a header-only DIR/dbs.sql
// and DIR/taosdump.<serial>/ holding everything that matters: the schema
// with its CREATE DATABASE line, the tag files and the avro data. The
// kinds accept either level and resolve to the inner one, because
// `taosdump -i` pointed at the outer directory exits 0 having restored
// nothing at all (measured).
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "taosdump":
		return resolveDump(path)
	case "taosdump_dir":
		latest, perr := latestDumpIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveDump(latest)
	case "taosdump_tar":
		return resolveTar(path)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: taosdump, taosdump_dir, taosdump_tar)", kind)
	}
}

// resolveDump reads one dump directory and reports what it holds.
func resolveDump(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"%s is a file; a taosdump backup is a directory (use kind taosdump_tar for an archive)", path)
	}
	inner, perr := innerDump(path)
	if perr != nil {
		return nil, perr
	}
	database, keepDays, perr := readSchema(inner)
	if perr != nil {
		return nil, perr
	}
	result := resultFile(path, inner)
	createdAt, started := dumpStartTime(result)
	rows, rowsKnown := artifactRows(result)
	sum, size, perr := treeChecksum(inner)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: inner, checksum: sum, sizeBytes: size,
		createdAt: createdAt, started: started,
		database: database, keepDays: keepDays,
		rows: rows, rowsKnown: rowsKnown,
	}, nil
}

// resolveTar vets an archive with what the host can read out of it in one
// streaming pass, so an archive says the same things about itself a
// directory does: the database it holds, the retention it declares, when
// it was taken and how many rows it claims. Without that pass the fences
// would apply to two kinds out of three.
func resolveTar(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"%s is a directory; use kind taosdump for a dump directory", path)
	}
	schema, result, perr := inspectDumpTar(path)
	if perr != nil {
		return nil, perr
	}
	m := createDatabase.FindSubmatch(schema)
	if m == nil {
		return nil, protoErr("source_corrupt", false,
			"%s holds no taosdump backup: nothing inside it is a dbs.sql that creates a database",
			filepath.Base(path))
	}
	keep := 0
	if k := keepClause.FindSubmatch(schema); k != nil {
		if days, err := strconv.Atoi(string(k[1])); err == nil {
			keep = days
		}
	}
	createdAt, started := dumpStartTime(result)
	rows, rowsKnown := artifactRows(result)
	sum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: sum, sizeBytes: info.Size(), tarball: true,
		database: string(m[1]), keepDays: keep,
		createdAt: createdAt, started: started,
		rows: rows, rowsKnown: rowsKnown,
	}, nil
}

const (
	// maxMetaBytes bounds what one metadata entry may contribute. A
	// schema and a result file are kilobytes; anything claiming more is
	// not one, and the host reads this before the drill has decided
	// anything.
	maxMetaBytes = 1 << 20
	// maxTarEntries bounds the walk itself, so an archive cannot hold the
	// host in a loop it chose.
	maxTarEntries = 200000
)

// inspectDumpTar streams an archive and returns the first schema that
// creates a database, and the dump's own result file if it carries one.
func inspectDumpTar(path string) (schema, result []byte, perr *protoError) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, nil, protoErr("source_unreadable", false, "open archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	tr := tar.NewReader(f)
	for range maxTarEntries {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Not tar-shaped, or truncated: the sandbox is the authority
			// on what the engine's own tools can read, so say what was
			// seen rather than guess at the rest.
			return nil, nil, protoErr("source_corrupt", false, "read archive: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if schema, result, perr = takeMeta(tr, hdr.Name, schema, result); perr != nil {
			return nil, nil, perr
		}
	}
	return schema, result, nil
}

// takeMeta reads one archive entry when it is one of the two files this
// adapter reads host-side, and leaves what it already has alone: the
// first of each wins, so a nested archive cannot talk over the outer one.
func takeMeta(r io.Reader, name string, schema, result []byte) ([]byte, []byte, *protoError) {
	switch filepath.Base(name) {
	case dumpMarker:
		if schema != nil {
			return schema, result, nil
		}
		body, perr := readCapped(r, name)
		if perr != nil {
			return nil, nil, perr
		}
		if createDatabase.Match(body) {
			schema = body
		}
	case "dump_result.txt":
		if result != nil {
			return schema, result, nil
		}
		body, perr := readCapped(r, name)
		if perr != nil {
			return nil, nil, perr
		}
		result = body
	}
	return schema, result, nil
}

// readCapped reads one archive entry, never more than a metadata file
// could honestly be.
func readCapped(r io.Reader, name string) ([]byte, *protoError) {
	body, err := io.ReadAll(io.LimitReader(r, maxMetaBytes))
	if err != nil {
		return nil, protoErr("source_corrupt", false, "read %s from the archive: %v", filepath.Base(name), err)
	}
	return body, nil
}

// dumpMarker is the file taosdump writes into the directory that holds a
// dump's payload; its presence is what makes a directory that directory.
const dumpMarker = "dbs.sql"

// innerDump finds the directory taosdump restores from.
//
// A dump directory holds dbs.sql, so the artifact the drill names may
// already be it. The directory `-o` was given also holds a dbs.sql — a
// header with no CREATE statement in it — so the schema decides, not the
// file name: the inner one carries the CREATE DATABASE line.
func innerDump(path string) (string, *protoError) {
	if hasSchema(filepath.Join(path, dumpMarker)) {
		return path, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && hasSchema(filepath.Join(path, e.Name(), dumpMarker)) {
			found = append(found, filepath.Join(path, e.Name()))
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", protoErr("source_corrupt", false,
			"%s does not look like a taosdump backup: no dbs.sql with a CREATE DATABASE line, "+
				"here or one level down", filepath.Base(path))
	default:
		sort.Strings(found)
		names := make([]string, 0, len(found))
		for _, f := range found {
			names = append(names, filepath.Base(f))
		}
		return "", protoErr("unsupported_source", false,
			"%s holds %d taosdump outputs (%s): a drill restores one backup, so name the one it "+
				"means, or use kind taosdump_dir to restore the newest",
			filepath.Base(path), len(found), strings.Join(names, ", "))
	}
}

// hasSchema reports whether a dbs.sql carries the schema rather than only
// the header taosdump writes beside it.
func hasSchema(path string) bool {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return false
	}
	return createDatabase.Match(raw)
}

var (
	// createDatabase matches the statement the dump's own schema opens
	// with, and captures the database name — backquoted or bare.
	createDatabase = regexp.MustCompile("(?i)CREATE DATABASE(?: IF NOT EXISTS)? +`?([A-Za-z0-9_]+)`?")
	// keepClause matches the retention the same statement declares.
	// TDengine prints it as one to three day counts: `KEEP 3650d,3650d,3650d`.
	keepClause = regexp.MustCompile(`(?i)KEEP +([0-9]+)d`)
	// dumpStart matches the instant taosdump records in dump_result.txt.
	dumpStart = regexp.MustCompile(`(?m)^# DumpOut start time: +(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
)

// readSchema reads the database the dump holds and the retention it
// declares, from the dump's own CREATE DATABASE statement.
func readSchema(inner string) (string, int, *protoError) {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(inner, dumpMarker)))
	if err != nil {
		return "", 0, protoErr("source_unreadable", false, "read the dump's schema: %v", err)
	}
	m := createDatabase.FindSubmatch(raw)
	if m == nil {
		return "", 0, protoErr("source_corrupt", false,
			"the dump's dbs.sql carries no CREATE DATABASE statement")
	}
	keep := 0
	if k := keepClause.FindSubmatch(raw); k != nil {
		if days, err := strconv.Atoi(string(k[1])); err == nil {
			keep = days
		}
	}
	return string(m[1]), keep, nil
}

// resultFile returns taosdump's own account of the run that produced this
// artifact. It sits beside the dump directory or inside it, depending on
// which level the drill named, and an artifact that lost it simply says
// less about itself.
func resultFile(outer, inner string) []byte {
	for _, dir := range []string{outer, inner, filepath.Dir(inner)} {
		raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "dump_result.txt")))
		if err == nil {
			return raw
		}
	}
	return nil
}

// dumpStartTime reads when taosdump says it started. A file's mtime would
// date the copy rather than the backup, so an artifact that records no
// instant dates to nothing at all.
func dumpStartTime(result []byte) (*string, time.Time) {
	m := dumpStart.FindSubmatch(result)
	if m == nil {
		return nil, time.Time{}
	}
	// taosdump prints the server's wall clock with no zone. It is read as
	// UTC and said so, rather than guessed at.
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", string(m[1]), time.UTC)
	if err != nil {
		return nil, time.Time{}
	}
	formatted := ts.UTC().Format(time.RFC3339)
	return &formatted, ts
}

// latestDumpIn picks the newest dump in a directory of them, by the
// instant each artifact records about itself rather than by file time.
func latestDumpIn(ctx context.Context, dir string) (string, *protoError) {
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
		inner, perr := innerDump(candidate)
		if perr != nil {
			continue
		}
		_, at := dumpStartTime(resultFile(candidate, inner))
		info, err := e.Info()
		if err != nil {
			continue
		}
		if at.IsZero() {
			at = info.ModTime()
		}
		if newest == "" || at.After(newestAt) {
			newest, newestAt = candidate, at
		}
	}
	if newest == "" {
		return "", protoErr("source_not_found", false,
			"%s holds no taosdump backup (a directory with a dbs.sql that creates a database)", dir)
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

// fileChecksum hashes one file.
func fileChecksum(path string) (string, *protoError) {
	h := sha256.New()
	if _, perr := hashFile(h, path); perr != nil {
		return "", perr
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
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
