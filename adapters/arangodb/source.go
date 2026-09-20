package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes (or tree)
	sizeBytes int64
	// tarball reports that the artifact is an archive to unpack rather
	// than a directory to transfer.
	tarball bool
	// census is what the artifact states about itself. For an archive the
	// host could not walk, the zero value — ops.go then reads the
	// database name out of the unpacked dump.
	census dumpCensus
}

// resolveSource maps a source kind to one restorable artifact.
//
//	arangodb_dump     — path is one arangodump output directory
//	arangodb_dump_tar — path is one tar archive (plain or gzip) of such a
//	                    directory, its files at the root or under one
//	                    wrapping directory
//	arangodb_dump_dir — path is a directory of such dumps; the one whose
//	                    own dump.json claims the newest instant is
//	                    restored
//
// dump.json states when the dump was taken, in RFC 3339 with a literal Z
// (measured), so created_at and the directory ranking both come from what
// the artifact states about itself — never from file times a copy would
// reset.
func resolveSource(kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "arangodb_dump":
		return resolveDir(path)
	case "arangodb_dump_tar":
		return resolveTar(path)
	case "arangodb_dump_dir":
		latest, perr := newestDumpIn(path)
		if perr != nil {
			return nil, perr
		}
		return resolveDir(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: arangodb_dump, arangodb_dump_tar, "+
				"arangodb_dump_dir)", kind)
	}
}

func resolveDir(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; use kind arangodb_dump_tar for tar archives", dir)
	}
	census, perr := inspectDumpDir(dir)
	if perr != nil {
		return nil, perr
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: dir, checksum: checksum, sizeBytes: size, census: census}, nil
}

func resolveTar(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind arangodb_dump for an arangodump output, or "+
				"arangodb_dump_dir for a directory of them", path)
	}
	census, verdict, ok := inspectDumpTar(path)
	if ok && verdict != nil {
		return nil, verdict
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	src := &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size(), tarball: true}
	if ok {
		src.census = census
	}
	return src, nil
}

// dumpCandidate is one subdirectory considered for restore.
type dumpCandidate struct {
	name      string
	createdMs int64 // the candidate's own claimed instant, 0 when unreadable
	mtime     time.Time
}

// newestDumpIn picks the dump whose own dump.json claims the newest
// instant. A dump that can be dated from its own files wins over one that
// cannot — the drill would rather restore the backup it can also say
// something true about — and the chosen dump still faces every
// single-dump gate, so a broken candidate that wins the ranking is
// refused by name.
func newestDumpIn(dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best *dumpCandidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		candidate := dumpCandidate{name: e.Name(), mtime: info.ModTime()}
		if census, perr := inspectDumpDir(filepath.Join(dir, e.Name())); perr == nil {
			candidate.createdMs = census.createdMs
		}
		if best == nil || candidate.beats(*best) {
			c := candidate
			best = &c
		}
	}
	if best == nil {
		return "", protoErr("source_not_found", false,
			"backup directory %s contains no dumps", dir)
	}
	return filepath.Join(dir, best.name), nil
}

// beats orders candidates: a dated dump outranks every undated one, a
// newer claimed instant outranks an older, undated candidates fall back
// to directory time, and remaining ties break toward the
// lexicographically larger name so the choice is deterministic.
func (c dumpCandidate) beats(o dumpCandidate) bool {
	switch {
	case (c.createdMs != 0) != (o.createdMs != 0):
		return c.createdMs != 0
	case c.createdMs != o.createdMs:
		return c.createdMs > o.createdMs
	case !c.mtime.Equal(o.mtime):
		return c.mtime.After(o.mtime)
	default:
		return c.name > o.name
	}
}

// dirChecksum hashes a directory tree canonically: entries sorted by
// relative path; regular files contribute path, size, and content bytes,
// symlinks contribute path and target.
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

// createdAtLayout renders source_identity.created_at.
const createdAtLayout = "2006-01-02T15:04:05.000Z07:00"

// formatCreatedAt renders an epoch-millisecond instant, or nil for 0.
func formatCreatedAt(ms int64) *string {
	if ms == 0 {
		return nil
	}
	s := time.UnixMilli(ms).UTC().Format(createdAtLayout)
	return &s
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// wall-clock formats sibling adapters read need it; dump.json states its
// instant in RFC 3339 with a literal Z (measured), so this adapter only
// refuses the declaration: an operator who wrote it expects an accuracy
// the artifact delivers on its own, and silence would leave them
// believing it did something.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this format makes redundant.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: a dump's %s states its instant in UTC, "+
			"so backup.created_at is exact without a declared zone — remove the parameter",
		backupTimezoneParam, manifestName)
}
