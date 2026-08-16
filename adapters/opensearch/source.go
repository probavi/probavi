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
	// than a tree to transfer.
	tarball bool
	// census is what the repository states about itself; the zero value
	// for an archive the host could not walk — the sandbox then reads
	// the same claims through the engine.
	census repoCensus
}

// resolveSource maps a source kind to one restorable artifact.
//
//	opensearch_repo_tar — path is one tar archive (plain or gzip) of an
//	                      fs snapshot repository, the repository at the
//	                      root or under one wrapping directory
//	opensearch_repo     — path is one fs snapshot repository directory
//
// A repository holds every snapshot ever taken into it, so "which
// backup" is decided inside the artifact: the drill restores the
// snapshot whose own metadata claims the newest instant, and created_at
// is that claim (epoch milliseconds, read through the engine — the
// times live in the repository's binary metadata, not its JSON).
func resolveSource(kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "opensearch_repo_tar":
		return resolveTar(path)
	case "opensearch_repo":
		return resolveRepoDir(path)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: opensearch_repo_tar, opensearch_repo)", kind)
	}
}

// resolveTar vets an archive artifact with what the host can read out of
// it in one streaming pass, and falls silent where the stream is not
// tar-shaped: the sandbox extraction is then the authority (metadata is
// a bonus, verdicts are not guesses).
func resolveTar(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind opensearch_repo for a repository directory", path)
	}
	census, verdict, ok := inspectRepoTar(path)
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

// resolveRepoDir vets a repository directory: the census against its own
// files, then the canonical tree checksum.
func resolveRepoDir(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; use kind opensearch_repo_tar for tar archives", dir)
	}
	census, perr := inspectRepoDir(dir)
	if perr != nil {
		return nil, perr
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: dir, checksum: checksum, sizeBytes: size, census: census}, nil
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

// createdAtLayout renders source_identity.created_at; the snapshot's
// instant is epoch milliseconds, so the offset is literal Z.
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
// wall-clock formats sibling adapters read need it; a snapshot states
// its instant as epoch milliseconds (measured), which carry no zone
// question at all. A declaration is refused rather than ignored:
// silence would leave the operator believing it did something.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this format makes redundant.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: a snapshot states its instant as epoch "+
			"milliseconds, so backup.created_at is exact without a declared zone — remove the "+
			"parameter", backupTimezoneParam)
}
