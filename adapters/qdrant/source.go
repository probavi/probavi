package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// form is what the restore has to do with it.
	form sourceForm
	// declaredChecksum is the bare hex digest the artifact's own .checksum
	// sidecar states, or "" when there is no sidecar. It has already been
	// verified by the time a caller sees it.
	declaredChecksum string
}

// sourceForm is the shape of a resolved artifact.
type sourceForm int

const (
	// formSnapshot is one collection snapshot, restored with --snapshot.
	formSnapshot sourceForm = iota
	// formFullSnapshot is a whole-storage snapshot, restored with
	// --storage-snapshot.
	formFullSnapshot
)

func (f sourceForm) String() string {
	if f == formFullSnapshot {
		return "full_snapshot"
	}
	return "snapshot"
}

// snapshotSuffix is what Qdrant names every snapshot it writes, and
// checksumSuffix is the sidecar beside it.
const (
	snapshotSuffix = ".snapshot"
	checksumSuffix = ".checksum"
)

// resolveSource maps a source kind to one restorable artifact.
//
//	qdrant_snapshot           — one collection snapshot, from
//	                            POST /collections/<c>/snapshots
//	qdrant_snapshot_dir       — a directory of them; the newest is restored
//	qdrant_full_snapshot      — one whole-storage snapshot, from POST /snapshots
//	qdrant_full_snapshot_dir  — a directory of them
//
// Deliberately absent: a copy of Qdrant's own storage directory. It is
// restorable, but it is not an artifact anyone should be shipping. For a
// thousand points the tree measures 593 MB apparent against 924 KB real —
// a forest of 32 MB sparse files (the write-ahead log, one memory-mapped
// vector chunk and one payload page per segment) — so a copy made with
// anything that does not understand holes writes hundreds of megabytes of
// zeroes per drill. The snapshot API is the vendor's answer to exactly
// that, and it is the one this adapter takes.
//
// Nothing inside a snapshot records when it was taken — the creation time
// lives in the API response that made it and in the file name, neither of
// which survives a copy — so created_at is always null and directories
// rank by modification time, the etcd adapter's precedent.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "qdrant_snapshot":
		if perr := refuseDirectory(path, "qdrant_snapshot_dir"); perr != nil {
			return nil, perr
		}
		return resolveSnapshot(path, formSnapshot)
	case "qdrant_snapshot_dir":
		latest, perr := latestIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveSnapshot(latest, formSnapshot)
	case "qdrant_full_snapshot":
		if perr := refuseDirectory(path, "qdrant_full_snapshot_dir"); perr != nil {
			return nil, perr
		}
		return resolveSnapshot(path, formFullSnapshot)
	case "qdrant_full_snapshot_dir":
		latest, perr := latestIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveSnapshot(latest, formFullSnapshot)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: qdrant_snapshot, qdrant_snapshot_dir, "+
				"qdrant_full_snapshot, qdrant_full_snapshot_dir)", kind)
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

// resolveSnapshot vets one snapshot artifact.
//
// It reads no magic bytes and refuses nothing on the strength of the
// artifact's content, which is a decision rather than an omission: Qdrant
// itself is a competent judge of its own snapshots. It exits 101 and never
// listens on a snapshot truncated anywhere from 25% to 99%, and on a
// 4 KB bit flip inside a structurally valid archive (both measured) — so a
// damaged artifact fails the drill with the engine's own words instead of
// a guess made from the first sixteen bytes on the host.
//
// What the host does check is the artifact's own claim about itself. Every
// snapshot Qdrant writes gets a <name>.checksum sidecar holding the
// SHA-256 of the file, and it matches to the byte. When the operator
// copied it along with the snapshot, this is a fence that fires before a
// byte crosses into the sandbox; when they did not, the engine's refusal
// still stands, and the README says which fence a drill actually has.
func resolveSnapshot(path string, form sourceForm) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false, "source path %s is a directory", path)
	case info.Size() == 0:
		return nil, protoErr("source_corrupt", false,
			"the backup %s is empty: no Qdrant snapshot is ever 0 bytes — the job that wrote "+
				"it failed", filepath.Base(path))
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	declared, perr := verifyDeclaredChecksum(path, checksum)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: path, checksum: checksum, sizeBytes: info.Size(),
		form: form, declaredChecksum: declared,
	}, nil
}

// verifyDeclaredChecksum compares the artifact against its own sidecar and
// returns what the sidecar declared, or "" when there is none.
//
// An unreadable or malformed sidecar is not treated as an absent one. A
// file that exists and cannot be read is a different situation from a file
// nobody wrote, and quietly downgrading the first to the second would turn
// a broken backup job into a drill that passed.
func verifyDeclaredChecksum(path, actual string) (string, *protoError) {
	sidecar := path + checksumSuffix
	raw, err := os.ReadFile(sidecar)
	switch {
	case os.IsNotExist(err):
		return "", nil
	case err != nil:
		return "", protoErr("source_unreadable", false,
			"read the checksum beside %s: %v", filepath.Base(path), err)
	}
	declared := strings.ToLower(strings.TrimSpace(string(raw)))
	if len(declared) != 64 || strings.TrimLeft(declared, "0123456789abcdef") != "" {
		return "", protoErr("source_corrupt", false,
			"the checksum beside %s is not a SHA-256 digest, so nothing states what this "+
				"snapshot should contain", filepath.Base(path))
	}
	if declared != strings.TrimPrefix(actual, "sha256:") {
		return "", protoErr("source_corrupt", false,
			"%s does not match the checksum Qdrant wrote beside it: the file changed after the "+
				"snapshot was taken, so the drill would prove something the engine never wrote",
			filepath.Base(path))
	}
	return declared, nil
}

// latestIn picks the directory's newest snapshot.
//
// Candidates are chosen by the .snapshot suffix rather than by content,
// which is the opposite of the couchdb adapter's rule and right here for
// the reason that rule exists: a signature has to distinguish the artifact
// from its neighbours, and a Qdrant snapshot is an ordinary tar whose
// first bytes are a file name. The suffix is Qdrant's own, it is what the
// sidecars hang off, and it is what separates a snapshot from the
// .checksum beside it.
//
// The check is not a filter. An artifact that wins the ranking and then
// fails a gate is refused by name rather than silently passed over — the
// same principle as settle.go.
func latestIn(ctx context.Context, dir string) (string, *protoError) {
	best, skipped, perr := newestSnapshot(dir)
	if perr != nil {
		return "", perr
	}
	switch {
	case best != "":
	case skipped > 0:
		return "", protoErr("source_not_found", false,
			"backup directory %s holds no Qdrant snapshots (%d files not ending in %s were "+
				"passed over)", dir, skipped, snapshotSuffix)
	default:
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	if perr := assertSettled(ctx, best, settleWindow); perr != nil {
		return "", perr
	}
	return best, nil
}

// newestSnapshot scans dir for the newest regular .snapshot file; ties
// break toward the lexicographically larger name so the choice never
// depends on directory iteration order.
func newestSnapshot(dir string) (string, int, *protoError) {
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
		if !strings.HasSuffix(e.Name(), snapshotSuffix) {
			skipped++
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", 0, protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		if beats(info, e.Name(), bestInfo, filepath.Base(best)) {
			best, bestInfo = filepath.Join(dir, e.Name()), info
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
// that will be restored — and it is the same digest the sidecar declares,
// so the verification costs no second read.
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
