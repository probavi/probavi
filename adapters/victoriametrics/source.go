package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// completeMarker is the empty file vmbackup writes last. vmrestore
	// refuses a directory without it — "this means either incomplete
	// backup or old backup" (measured) — and offers
	// -skipBackupCompleteCheck to override, which this adapter never
	// passes: a drill that restores past the tool's own refusal proves
	// nothing about the backup it was pointed at.
	completeMarker = "backup_complete.ignore"
	// metadataMarker carries what the backup says about itself:
	// {"created_at":"2026-08-18T18:23:25Z","completed_at":"…"} (measured).
	// created_at is the instant the snapshot froze the data, which is the
	// instant a drill's checks must evaluate at.
	metadataMarker = "backup_metadata.ignore"
	// partsName is the per-partition list of parts that partition holds.
	// In a backup it is a directory holding one file, because vmbackup's
	// filesystem layout stores every file as one part named
	// <size>_<offset>_<size> in hex (measured).
	partsName = "parts.json"
)

// liveMarkers are entries only a running server's -storageDataPath
// holds, and a vmbackup output never does (both measured). They are the
// raw-copy fence: a copy of a live data directory starts and serves in a
// quiet moment, which is exactly why it must be refused by name instead
// of trusted.
var liveMarkers = []string{"flock.lock", "snapshots", "tmp"}

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes (or tree)
	sizeBytes int64
	// tarball reports that the artifact is an archive to unpack rather
	// than a directory to transfer.
	tarball bool
	// info is what the artifact states about itself. For an archive the
	// host could not walk, the zero value — ops.go then recovers the same
	// facts from the unpacked tree.
	info backupInfo
}

// backupInfo is the artifact's own account of itself.
type backupInfo struct {
	// createdAtMs is the snapshot instant from metadataMarker, 0 when the
	// artifact did not state one.
	createdAtMs int64
	// parts is how many parts the partitions declare between them.
	parts int
}

// backupMetadata is metadataMarker's content.
type backupMetadata struct {
	CreatedAt   string `json:"created_at"`
	CompletedAt string `json:"completed_at"`
}

// resolveSource maps a source kind to one restorable artifact.
//
//	victoriametrics_backup_tar — path is one tar archive (plain or gzip)
//	                             of a vmbackup output, its files at the
//	                             root or under one wrapping directory
//	victoriametrics_backup     — path is one vmbackup output directory
//	victoriametrics_backup_dir — path is a directory of them; the one
//	                             whose own metadata claims the newest
//	                             instant is restored
//
// Every fact used for ranking and for backup.created_at comes from what
// the artifact states about itself, never from file times a copy would
// reset.
func resolveSource(kind, sourcePath string) (*resolvedSource, *protoError) {
	switch kind {
	case "victoriametrics_backup_tar":
		return resolveTar(sourcePath)
	case "victoriametrics_backup":
		return resolveBackupDir(sourcePath)
	case "victoriametrics_backup_dir":
		latest, perr := newestBackupIn(sourcePath)
		if perr != nil {
			return nil, perr
		}
		return resolveBackupDir(latest)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: victoriametrics_backup_tar, "+
				"victoriametrics_backup, victoriametrics_backup_dir)", kind)
	}
}

// resolveTar vets an archive with what the host can read out of it. The
// listing is a bonus — an archive the host cannot walk still resolves,
// and the sandbox extraction is the authority — except where an entry is
// positive evidence: a live-data-directory marker, or a walkable archive
// that is not a backup at all.
func resolveTar(archivePath string) (*resolvedSource, *protoError) {
	info, err := os.Stat(archivePath)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", archivePath)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind victoriametrics_backup for one backup "+
				"directory, or victoriametrics_backup_dir for a directory of them", archivePath)
	}
	walked, perr := vetTarEntries(archivePath)
	if perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(archivePath)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: archivePath, checksum: checksum, sizeBytes: info.Size(),
		tarball: true, info: walked,
	}, nil
}

// vetTarEntries applies the archive's readable half of the fences.
func vetTarEntries(archivePath string) (backupInfo, *protoError) {
	walked, live, complete, saw, ok := listTarBackup(archivePath)
	if !ok {
		return backupInfo{}, nil
	}
	if live != "" {
		return backupInfo{}, refuseLiveCopy(live)
	}
	if !saw {
		return backupInfo{}, protoErr("source_corrupt", false,
			"the archive holds no vmbackup output — a backup carries %s and %s beside its data "+
				"(take one with vmbackup against a snapshot from POST /snapshot/create)",
			completeMarker, metadataMarker)
	}
	if !complete {
		return backupInfo{}, refuseIncomplete()
	}
	return walked, nil
}

// resolveBackupDir vets one vmbackup output directory.
func resolveBackupDir(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; use kind victoriametrics_backup_tar for tar archives", dir)
	}
	backup, perr := inspectBackupDir(dir)
	if perr != nil {
		return nil, perr
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{path: dir, checksum: checksum, sizeBytes: size, info: backup}, nil
}

// inspectBackupDir applies every fence the host can apply to a directory,
// in the order that gives the operator the most useful refusal first: the
// raw copy it should never have taken, then the backup that never
// finished, then the tree that is not a backup at all, and last the parts
// the artifact itself says must be there.
func inspectBackupDir(dir string) (backupInfo, *protoError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return backupInfo{}, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	for _, marker := range liveMarkers {
		if names[marker] {
			return backupInfo{}, refuseLiveCopy(marker)
		}
	}
	if !names[metadataMarker] {
		return backupInfo{}, protoErr("source_corrupt", false,
			"%s is not a vmbackup output: it carries no %s (take one with vmbackup against a "+
				"snapshot from POST /snapshot/create)", dir, metadataMarker)
	}
	if !names[completeMarker] {
		return backupInfo{}, refuseIncomplete()
	}
	createdAtMs, perr := readBackupMetadata(dir)
	if perr != nil {
		return backupInfo{}, perr
	}
	parts, perr := partCensus(dir)
	if perr != nil {
		return backupInfo{}, perr
	}
	return backupInfo{createdAtMs: createdAtMs, parts: parts}, nil
}

// refuseLiveCopy names the entry that proves the artifact is a copy of a
// running server's storage rather than a backup of it.
func refuseLiveCopy(marker string) *protoError {
	return protoErr("unsupported_source", false,
		"the artifact contains %q, which only a live -storageDataPath holds: this is a copy of a "+
			"running server's storage, not a backup, and a copy taken under write load is "+
			"inconsistent even though it starts and serves in a quiet moment (measured) — take "+
			"backups with POST /snapshot/create followed by vmbackup, and point the drill at "+
			"vmbackup's output", marker)
}

// refuseIncomplete surfaces the tool's own contract. vmrestore refuses
// the same artifact, and names a flag that would restore it anyway; the
// drill refuses instead of reaching for that flag.
func refuseIncomplete() *protoError {
	return protoErr("source_corrupt", false,
		"the backup carries no %s, the marker vmbackup writes last: it never finished, and "+
			"vmrestore refuses it too — the drill will not restore past that refusal, because a "+
			"restore of an unfinished backup proves nothing about the backup", completeMarker)
}

// readBackupMetadata reads the instant the snapshot froze the data.
func readBackupMetadata(dir string) (int64, *protoError) {
	raw, err := os.ReadFile(filepath.Join(dir, metadataMarker))
	if err != nil {
		return 0, protoErr("source_unreadable", false, "read %s: %v", metadataMarker, err)
	}
	ms, ok := parseBackupMetadata(raw)
	if !ok {
		return 0, protoErr("source_corrupt", false,
			"%s does not state a readable created_at — the drill would have no instant to "+
				"evaluate its checks at", metadataMarker)
	}
	return ms, nil
}

// parseBackupMetadata reads created_at out of the marker's JSON.
func parseBackupMetadata(raw []byte) (int64, bool) {
	meta := backupMetadata{}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.CreatedAt == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, meta.CreatedAt)
	if err != nil {
		return 0, false
	}
	return t.UTC().UnixMilli(), true
}

// partCensus checks the artifact against its own statement of what it
// holds: every partition's parts.json names the parts that partition
// requires, and a part named there but absent is a truncated copy. The
// engine makes the same check when it opens the storage — "part … is
// listed in … parts.json, but is missing on disk" (measured) — but only
// after the whole artifact has been transferred and restored, which is a
// long way to travel for an answer the artifact could give up front.
func partCensus(root string) (int, *protoError) {
	declared := 0
	var refusal *protoError
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || d.Name() != partsName {
			return nil
		}
		names, perr := readPartsList(p)
		if perr != nil {
			refusal = perr
			return filepath.SkipAll
		}
		partition := filepath.Dir(p)
		for _, name := range names {
			info, statErr := os.Stat(filepath.Join(partition, name))
			if statErr != nil || !info.IsDir() {
				refusal = protoErr("source_corrupt", false,
					"the backup is incomplete: %s names part %q, which the artifact does not "+
						"contain — a truncated copy of a backup, not a backup",
					path.Join(filepath.Base(partition), partsName), name)
				return filepath.SkipAll
			}
			declared++
		}
		return nil
	})
	if refusal != nil {
		return 0, refusal
	}
	if err != nil {
		return 0, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	return declared, nil
}

// readPartsList reads one parts.json out of the backup layout, where it
// is a directory holding a single part file.
func readPartsList(partsDir string) ([]string, *protoError) {
	entries, err := os.ReadDir(partsDir)
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read %s: %v", partsName, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(partsDir, e.Name()))
		if err != nil {
			return nil, protoErr("source_unreadable", false, "read %s: %v", partsName, err)
		}
		return declaredParts(raw), nil
	}
	return nil, nil
}

// declaredParts reads both shapes vmbackup writes: the index partitions
// list their parts as a bare array, the data partitions as an object with
// Small and Big lists (both measured). Anything else contributes nothing
// rather than refusing — the census names missing parts, it does not
// grade the engine's own metadata format.
func declaredParts(raw []byte) []string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	sized := struct {
		Small []string `json:"Small"`
		Big   []string `json:"Big"`
	}{}
	if err := json.Unmarshal(raw, &sized); err == nil {
		return append(sized.Small, sized.Big...)
	}
	return nil
}

// backupCandidate is one subdirectory considered for restore.
type backupCandidate struct {
	name        string
	createdAtMs int64 // the candidate's own claimed instant, 0 when unreadable
	mtime       time.Time
}

// newestBackupIn picks the backup whose own metadata claims the newest
// instant. A backup that can be dated from its own files wins over one
// that cannot, and the chosen directory still faces every single-backup
// fence, so a live copy or an unfinished backup that wins the ranking is
// still refused by name.
func newestBackupIn(dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best *backupCandidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		candidate := backupCandidate{name: e.Name(), mtime: info.ModTime()}
		if raw, err := os.ReadFile(filepath.Join(dir, e.Name(), metadataMarker)); err == nil {
			if ms, ok := parseBackupMetadata(raw); ok {
				candidate.createdAtMs = ms
			}
		}
		if best == nil || candidate.beats(*best) {
			c := candidate
			best = &c
		}
	}
	if best == nil {
		return "", protoErr("source_not_found", false,
			"backup directory %s contains no backup directories", dir)
	}
	return filepath.Join(dir, best.name), nil
}

// beats orders candidates: a dated backup outranks every undated one, a
// newer claimed instant outranks an older, undated candidates fall back
// to directory time, and remaining ties break toward the
// lexicographically larger name so the choice is deterministic.
func (c backupCandidate) beats(o backupCandidate) bool {
	switch {
	case (c.createdAtMs != 0) != (o.createdAtMs != 0):
		return c.createdAtMs != 0
	case c.createdAtMs != o.createdAtMs:
		return c.createdAtMs > o.createdAtMs
	case !c.mtime.Equal(o.mtime):
		return c.mtime.After(o.mtime)
	default:
		return c.name > o.name
	}
}

// listTarBackup walks an archive for the facts the fences need: a live
// marker, the two backup markers, and the snapshot instant. ok reports
// whether the walk succeeded at all — an archive nothing could read is
// not evidence of anything, and the sandbox extraction stays the
// authority.
func listTarBackup(archivePath string) (info backupInfo, live string, complete, saw, ok bool) {
	f, err := os.Open(archivePath)
	if err != nil {
		return backupInfo{}, "", false, false, false
	}
	defer f.Close() //nolint:errcheck // read-only walk; the checksum pass reopens it

	var reader io.Reader = f
	if gz, err := gzip.NewReader(f); err == nil {
		defer gz.Close() //nolint:errcheck // read-only walk
		reader = gz
	} else if _, err := f.Seek(0, io.SeekStart); err != nil {
		return backupInfo{}, "", false, false, false
	}

	tr := tar.NewReader(reader)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return info, live, complete, saw, true
		}
		if err != nil {
			return info, live, complete, saw, false
		}
		recordTarEntry(tr, header, &info, &live, &complete, &saw)
	}
}

// recordTarEntry folds one archive entry into the walk's findings. The
// artifact may sit at the archive root or under one wrapping directory,
// so entries are recognised by their trailing path element.
func recordTarEntry(tr io.Reader, header *tar.Header, info *backupInfo,
	live *string, complete, saw *bool) {
	name := path.Clean(header.Name)
	base := path.Base(name)
	depth := strings.Count(strings.Trim(name, "/"), "/")
	if depth <= 1 {
		for _, marker := range liveMarkers {
			if base == marker && *live == "" {
				*live = marker
			}
		}
	}
	switch base {
	case completeMarker:
		*complete, *saw = true, true
	case metadataMarker:
		*saw = true
		raw, err := io.ReadAll(io.LimitReader(tr, 64<<10))
		if err != nil {
			return
		}
		if ms, ok := parseBackupMetadata(raw); ok {
			info.createdAtMs = ms
		}
	}
}

// dirChecksum hashes a directory tree canonically: entries sorted by
// relative path; regular files contribute path, size, and content bytes,
// symlinks contribute path and target. The same tree always hashes the
// same, any content change changes the hash.
func dirChecksum(root string) (string, int64, *protoError) {
	h := sha256.New()
	var total int64
	var files int
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return hashEntry(h, p, rel, d, &total, &files)
	})
	if err != nil {
		return "", 0, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	if files == 0 {
		return "", 0, protoErr("source_not_found", false, "backup directory %s contains no files", root)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), total, nil
}

func hashEntry(h io.Writer, p, rel string, d os.DirEntry, total *int64, files *int) error {
	switch {
	case d.Type().IsRegular():
		info, err := d.Info()
		if err != nil {
			return err
		}
		*total += info.Size()
		*files++
		fmt.Fprintf(h, "%s\x00%d\x00", rel, info.Size())
		return copyFileInto(h, p)
	case d.Type()&os.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00L%s\x00", rel, target)
	}
	return nil
}

func copyFileInto(h io.Writer, p string) error {
	f, err := os.Open(p)
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
func fileChecksum(p string) (string, *protoError) {
	h := sha256.New()
	if err := copyFileInto(h, p); err != nil {
		return "", protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// createdAtLayout renders source_identity.created_at. The instant comes
// from the artifact as RFC 3339 UTC, so the offset is literal Z.
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
// wall-clock formats sibling adapters read need it; vmbackup states its
// instant as RFC 3339 UTC (measured), which carries no zone question at
// all. A declaration is refused rather than ignored: silence would leave
// the operator believing it did something.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration this format makes redundant.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: %s states the snapshot instant as "+
			"RFC 3339 UTC, so backup.created_at is exact without a declared zone — remove the "+
			"parameter", backupTimezoneParam, metadataMarker)
}
