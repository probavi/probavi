// Package manifest implements probavi-manifest/1: the JSON file a backup
// job writes beside its backup, stating what it produced, and the check
// the core runs against it before a sandbox exists.
//
// The checksum rule here is the core's own (docs/backup-manifest.md §3)
// and deliberately not the adapter-defined backup.checksum an evidence
// record carries. That one answers *what did this drill restore*, is
// produced during provision, and genuinely differs across the catalogue;
// this one answers *are these the bytes the backup tool wrote*, and has to
// answer it before any adapter has spoken (§4). The two are not
// comparable, and this package never compares them.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/probavi/probavi/internal/evidence"
)

// SchemaID is the contract version this package reads. A manifest naming
// any other value is refused rather than guessed at: the field exists to
// pin the shape, so honouring an unknown one would defeat it.
const SchemaID = "probavi-manifest/1"

// checksumPattern is the published form of expected_checksum
// (docs/schemas/manifest/manifest.json). A value that does not match is a
// fault in the manifest rather than a disagreement with the artifact —
// the difference matters, because one blames the operator's file and the
// other blames the backup.
var checksumPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Manifest is one probavi-manifest/1 document. Everything but Schema is
// optional, and created_at and engine_version are the backup job's claims:
// they are read so a malformed one is refused by name, and they never
// reach a record, which keeps taking both facts from the artifact itself
// (§2).
type Manifest struct {
	Schema            string `json:"schema"`
	ExpectedChecksum  string `json:"expected_checksum"`
	ExpectedSizeBytes *int64 `json:"expected_size_bytes"`
	CreatedAt         string `json:"created_at"`
	EngineVersion     string `json:"engine_version"`
}

// Fault is a check that did not pass, carrying the error code the record
// must show. The split is the one §5 draws and it is load-bearing:
// a fault in the manifest is a configuration failure (invalid_request,
// outcome error), while a disagreement between the manifest and the
// artifact is a verdict about the backup (source_corrupt, outcome fail).
// A drill must never write "the backup is the problem" into an
// append-only log about an artifact it never looked at.
type Fault struct {
	Code    string
	Message string
}

func (f *Fault) Error() string { return f.Message }

func faultf(code, format string, args ...any) *Fault {
	return &Fault{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Result is what a record carries about the check (evidence-schema.md §3):
// which manifest was believed, and whether the artifact agreed with it.
// Both are nil when no manifest was read — a configuration naming none,
// or one naming a file this package could not use.
//
// The expectation itself is deliberately not here. It is not comparable to
// the adapter's backup.checksum beside it in the record (§4), and a
// mismatch names both values in the Fault's message, which is the one
// place they inform rather than mislead.
type Result struct {
	Hash  *string
	Match *bool
}

// Check holds the artifact at sourcePath to the backup manifest at
// manifestPath. It returns a nil Fault when they agree, and one naming
// both values when they do not — and, either way, what a record says
// about the manifest it believed.
//
// The checksum is computed only when the manifest expects one: a
// size-only manifest is a deliberately cheap check, and reading every byte
// to satisfy it anyway would take that choice away from the operator who
// made it.
func Check(sourcePath, manifestPath string) (Result, *Fault) {
	m, raw, fault := read(manifestPath)
	if fault != nil {
		return Result{}, fault
	}
	// The hash is of the manifest's bytes as read, so a record pins the
	// file this drill believed rather than whatever stands there later.
	hash := digestOf(raw)
	got, fault := measure(sourcePath, m.ExpectedChecksum != "")
	if fault != nil {
		// The artifact could not be measured, so nothing compared: the
		// record names the manifest that was read and leaves the verdict
		// unstated rather than reporting a disagreement nobody found.
		return Result{Hash: &hash}, fault
	}
	fault = compare(sourcePath, manifestPath, m, got)
	matched := fault == nil
	return Result{Hash: &hash, Match: &matched}, fault
}

// digestOf is the manifest-file rule: SHA-256 over the bytes as read.
func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", sum)
}

// read parses the manifest and refuses every shape that cannot be checked.
// Unknown fields are refused because the published schema closes the
// object: a drill that silently ignored `expected_size` would check
// nothing while the config believed in it, which is the failure this
// feature exists to remove.
func read(path string) (*Manifest, []byte, *Fault) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, faultf(evidence.CodeInvalidRequest, "backup manifest not found: %s", path)
		}
		return nil, nil, faultf(evidence.CodeInvalidRequest, "backup manifest %s cannot be read: %s", path, reason(err))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, nil, faultf(evidence.CodeInvalidRequest, "backup manifest %s is not a readable %s document: %s", path, SchemaID, err)
	}
	if dec.More() {
		return nil, nil, faultf(evidence.CodeInvalidRequest, "backup manifest %s carries more than one JSON document", path)
	}
	return &m, raw, m.validate(path)
}

func (m *Manifest) validate(path string) *Fault {
	switch {
	case m.Schema == "":
		return faultf(evidence.CodeInvalidRequest, "backup manifest %s names no schema; %s is required", path, SchemaID)
	case m.Schema != SchemaID:
		return faultf(evidence.CodeInvalidRequest, "backup manifest %s declares schema %q, which this build does not read; it reads %s", path, m.Schema, SchemaID)
	}
	if m.ExpectedChecksum == "" && m.ExpectedSizeBytes == nil {
		return faultf(evidence.CodeInvalidRequest,
			"backup manifest %s carries neither expected_checksum nor expected_size_bytes, so it asserts nothing about the backup", path)
	}
	if m.ExpectedChecksum != "" && !checksumPattern.MatchString(m.ExpectedChecksum) {
		return faultf(evidence.CodeInvalidRequest,
			"backup manifest %s has expected_checksum %q, which is not sha256: followed by 64 lowercase hex digits", path, m.ExpectedChecksum)
	}
	if m.ExpectedSizeBytes != nil && *m.ExpectedSizeBytes < 0 {
		return faultf(evidence.CodeInvalidRequest,
			"backup manifest %s has a negative expected_size_bytes (%d)", path, *m.ExpectedSizeBytes)
	}
	return nil
}

// measurement is what §3 produces for one artifact. checksum is empty when
// the manifest expected none and none was computed.
type measurement struct {
	checksum string
	size     int64
}

// measure applies the two shapes of §3 and refuses everything else by
// name. A symlink *at* sourcePath is followed, as every other reader of
// source.path follows it; symlinks *inside* a tree are part of the tree
// and are hashed as links.
func measure(path string, wantChecksum bool) (measurement, *Fault) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return measurement{}, faultf(evidence.CodeSourceNotFound, "backup source not found: %s", path)
		}
		return measurement{}, faultf(evidence.CodeSourceUnreadable, "backup source %s cannot be read: %s", path, reason(err))
	}
	switch {
	case info.Mode().IsRegular():
		return measureFile(path, info, wantChecksum)
	case info.IsDir():
		return measureTree(path, wantChecksum)
	default:
		return measurement{}, faultf(evidence.CodeSourceUnreadable,
			"backup source %s is neither a regular file nor a directory (%s)", path, info.Mode().Type())
	}
}

func measureFile(path string, info fs.FileInfo, wantChecksum bool) (measurement, *Fault) {
	if !wantChecksum {
		return measurement{size: info.Size()}, nil
	}
	h := sha256.New()
	n, fault := hashFile(h, path)
	if fault != nil {
		return measurement{}, fault
	}
	return measurement{checksum: digest(h), size: n}, nil
}

// measureTree is the canonical tree hash of §3: every regular file and
// every symlink below root, named by its slash-separated relative path,
// those paths sorted by byte value, each contributing
// <path> NUL <size> NUL <bytes> or <path> NUL L<target> NUL.
//
// Byte-value order is not the order a directory walk produces — "a.txt"
// precedes "a/b", because '.' is 0x2E and '/' is 0x2F — so the paths are
// collected first and sorted, never hashed as they are met.
func measureTree(root string, wantChecksum bool) (measurement, *Fault) {
	rels, regular, fault := treeEntries(root)
	if fault != nil {
		return measurement{}, fault
	}
	if regular == 0 {
		return measurement{}, faultf(evidence.CodeSourceNotFound, "backup source %s holds no regular file", root)
	}
	slices.Sort(rels)
	if !wantChecksum {
		return treeSize(root, rels)
	}
	return treeDigest(root, rels)
}

// treeEntries collects what contributes to the hash, and counts the
// regular files: a directory of nothing but links and sockets is not a
// backup, and §3 refuses it rather than hashing the empty string.
func treeEntries(root string) ([]string, int, *Fault) {
	var rels []string
	var regular int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		// Device nodes, sockets and FIFOs contribute nothing (§3).
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if d.Type().IsRegular() {
			regular++
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, 0, faultf(evidence.CodeSourceUnreadable, "backup source %s cannot be walked: %s", root, reason(err))
	}
	return rels, regular, nil
}

// treeDigest frames the sorted entries and hashes them.
func treeDigest(root string, rels []string) (measurement, *Fault) {
	h := sha256.New()
	var total int64
	for _, rel := range rels {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, fault := lstat(full)
		if fault != nil {
			return measurement{}, fault
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return measurement{}, faultf(evidence.CodeSourceUnreadable, "backup source link %s cannot be read: %s", full, reason(err))
			}
			fmt.Fprintf(h, "%s\x00L%s\x00", rel, target)
			continue
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, info.Size())
		n, fault := hashFile(h, full)
		if fault != nil {
			return measurement{}, fault
		}
		total += n
	}
	return measurement{checksum: digest(h), size: total}, nil
}

// treeSize sums the regular files without reading a byte of them. A
// symlink's own size is the length of its target and not a byte of the
// backup, so it contributes to the checksum and not to this (§3).
func treeSize(root string, rels []string) (measurement, *Fault) {
	var total int64
	for _, rel := range rels {
		info, fault := lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if fault != nil {
			return measurement{}, fault
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			total += info.Size()
		}
	}
	return measurement{size: total}, nil
}

func lstat(path string) (fs.FileInfo, *Fault) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, faultf(evidence.CodeSourceUnreadable, "backup source file %s cannot be read: %s", path, reason(err))
	}
	return info, nil
}

func hashFile(h hash.Hash, path string) (int64, *Fault) {
	f, err := os.Open(path)
	if err != nil {
		return 0, faultf(evidence.CodeSourceUnreadable, "backup source file %s cannot be read: %s", path, reason(err))
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, faultf(evidence.CodeSourceUnreadable, "backup source file %s cannot be read: %s", path, reason(err))
	}
	return n, nil
}

func digest(h hash.Hash) string { return fmt.Sprintf("sha256:%x", h.Sum(nil)) }

// compare produces the §5 message, which names both values in the order a
// reader needs them: what the artifact is, then what the manifest expected.
// It describes only what was expected, so a size-only manifest produces a
// message about sizes rather than an empty checksum.
func compare(sourcePath, manifestPath string, m *Manifest, got measurement) *Fault {
	checksumBad := m.ExpectedChecksum != "" && got.checksum != m.ExpectedChecksum
	sizeBad := m.ExpectedSizeBytes != nil && got.size != *m.ExpectedSizeBytes
	if !checksumBad && !sizeBad {
		return nil
	}
	want := measurement{checksum: m.ExpectedChecksum}
	if m.ExpectedSizeBytes != nil {
		want.size = *m.ExpectedSizeBytes
	}
	return faultf(evidence.CodeSourceCorrupt,
		"backup manifest mismatch: %s is %s, the backup manifest at %s expects %s",
		sourcePath, describe(got, m), manifestPath, describe(want, m))
}

func describe(v measurement, m *Manifest) string {
	var parts []string
	if m.ExpectedChecksum != "" {
		parts = append(parts, v.checksum)
	}
	if m.ExpectedSizeBytes != nil {
		parts = append(parts, fmt.Sprintf("%d bytes", v.size))
	}
	if len(parts) == 2 {
		return fmt.Sprintf("%s (%s)", parts[0], parts[1])
	}
	return strings.Join(parts, "")
}

// reason strips the path an os error repeats, so a message names it once.
func reason(err error) string {
	var perr *fs.PathError
	if errors.As(err, &perr) {
		return perr.Err.Error()
	}
	return err.Error()
}
