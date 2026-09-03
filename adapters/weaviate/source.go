package main

import (
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

// metaFileName is the file Weaviate writes at a backup's root, and the
// signature that separates a backup directory from its neighbours.
const metaFileName = "backup_config.json"

// nodeMetaFileName is the per-node manifest below the root.
const nodeMetaFileName = "backup.json"

// maxMetaBytes caps how much of an operator-supplied metadata file the
// adapter will read. The two manifests of a real backup are kilobytes;
// a "manifest" that runs past this is not one, and an unbounded read of
// an attacker-controlled file is the archive-walk hazard this repository
// has fixed once already.
const maxMetaBytes = 8 << 20

// backupMeta is the slice of backup_config.json this adapter acts on.
type backupMeta struct {
	StartedAt     string              `json:"startedAt"`
	CompletedAt   string              `json:"completedAt"`
	ID            string              `json:"id"`
	Status        string              `json:"status"`
	Error         string              `json:"error"`
	ServerVersion string              `json:"serverVersion"`
	Nodes         map[string]metaNode `json:"nodes"`
}

type metaNode struct {
	Classes []string `json:"classes"`
	Status  string   `json:"status"`
}

// nodeMeta is the slice of <node>/backup.json this adapter acts on: the
// chunk map is the completeness contract — every chunk it names must be
// present as <class>/chunk-<n> before a byte crosses into the sandbox.
type nodeMeta struct {
	Classes []nodeMetaClass `json:"classes"`
}

type nodeMetaClass struct {
	Name   string                     `json:"name"`
	Chunks map[string]json.RawMessage `json:"chunks"`
}

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" (file bytes, or the canonical tree hash)
	sizeBytes int64
	// tarball says the artifact is an archive to unpack in the sandbox;
	// its metadata is read there, after tar has judged the bytes.
	tarball bool
	// meta is the parsed backup_config.json for the directory kinds; nil
	// for the tar kind until provision reads it inside the sandbox.
	meta *backupMeta
	// node is the single node the backup was taken on (directory kinds).
	node string
	// classes are the class names the backup holds (directory kinds).
	classes []string
	// createdAt is the completion instant the backup states about itself,
	// RFC 3339 UTC, or nil when it is not derivable.
	createdAt *string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	weaviate_backup_tar — one tar (plain or gzip) of a filesystem-backend
//	                      backup directory
//	weaviate_backup     — one backup directory, from POST /v1/backups/filesystem
//	weaviate_backup_dir — a directory of them; the one whose own metadata
//	                      claims the newest completion is restored
//
// Deliberately absent: a copy of Weaviate's persistence directory
// (PERSISTENCE_DATA_PATH). It is not an artifact anyone should ship — the
// backup API exists exactly so that operators never have to copy a live
// LSM tree — and a data-directory copy carries no backup_config.json, so
// it is refused here by the absence of the one file every real backup
// has, with a message that names the API.
func resolveSource(kind, sourcePath string) (*resolvedSource, *protoError) {
	switch kind {
	case "weaviate_backup_tar":
		return resolveTar(sourcePath)
	case "weaviate_backup":
		return resolveBackupDir(sourcePath)
	case "weaviate_backup_dir":
		winner, perr := newestBackupIn(sourcePath)
		if perr != nil {
			return nil, perr
		}
		return resolveBackupDir(winner)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: weaviate_backup_tar, weaviate_backup, "+
				"weaviate_backup_dir)", kind)
	}
}

// resolveTar vets one archive artifact.
//
// It reads no content and refuses nothing on the strength of the bytes,
// which is a decision rather than an omission: tar inside the sandbox and
// then the engine itself judge the archive, and both were measured to
// refuse damage with their own words (a chunk truncated mid-file fails the
// restore with "unexpected EOF", a flipped byte with "flate: corrupt
// input"). The backup's own metadata is read after unpacking, where the
// files are.
func resolveTar(archivePath string) (*resolvedSource, *protoError) {
	info, err := os.Stat(archivePath)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", archivePath)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind weaviate_backup for one backup directory "+
				"or weaviate_backup_dir for a directory of them", archivePath)
	case info.Size() == 0:
		return nil, protoErr("source_corrupt", false,
			"the backup %s is empty: no archive is ever 0 bytes — the job that wrote it failed",
			filepath.Base(archivePath))
	}
	checksum, perr := fileChecksum(archivePath)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: archivePath, checksum: checksum, sizeBytes: info.Size(), tarball: true,
	}, nil
}

// resolveBackupDir vets one backup directory against the backup's own
// claims: the root metadata must parse and report a completed backup taken
// on exactly one node, and every chunk the node manifest names must be
// present. A directory that fails these gates is refused by name — the
// engine would refuse it too (measured), but this fence fires before a
// byte crosses into the sandbox.
func resolveBackupDir(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; use kind weaviate_backup_tar for archives", dir)
	}
	meta, perr := readBackupMeta(dir)
	if perr != nil {
		return nil, perr
	}
	if perr := requireCompleted(meta, filepath.Base(dir)); perr != nil {
		return nil, perr
	}
	node, perr := singleNode(meta, filepath.Base(dir))
	if perr != nil {
		return nil, perr
	}
	classes, perr := verifyChunks(dir, node)
	if perr != nil {
		return nil, perr
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path: dir, checksum: checksum, sizeBytes: size,
		meta: meta, node: node, classes: classes,
		createdAt: createdAtFrom(meta),
	}, nil
}

// readBackupMeta reads and parses <dir>/backup_config.json.
func readBackupMeta(dir string) (*backupMeta, *protoError) {
	raw, perr := readMetaFile(filepath.Join(dir, metaFileName))
	if perr != nil {
		return nil, perr
	}
	if raw == nil {
		return nil, protoErr("source_corrupt", false,
			"%s holds no %s: every backup the filesystem backend writes has one, so this is "+
				"not a Weaviate backup directory — a copy of the persistence directory is not "+
				"an artifact, POST /v1/backups/filesystem is", dir, metaFileName)
	}
	meta := &backupMeta{}
	if err := json.Unmarshal(raw, meta); err != nil {
		return nil, protoErr("source_corrupt", false,
			"%s in %s is not the JSON Weaviate writes: %v", metaFileName, dir, err)
	}
	return meta, nil
}

// requireCompleted gates on the backup's own status field: an in-progress
// backup is not yet an artifact, and a failed one never became one.
// Restoring either would prove something the operator does not have.
func requireCompleted(meta *backupMeta, name string) *protoError {
	switch meta.Status {
	case "SUCCESS":
		return nil
	case "FAILED":
		detail := meta.Error
		if detail == "" {
			detail = "no error recorded"
		}
		return protoErr("source_corrupt", false,
			"backup %s reports status FAILED (%s): the backup job never finished writing it — "+
				"fix the job rather than the drill", name, oneLine(detail))
	default:
		return protoErr("source_unreadable", false,
			"backup %s reports status %s: the backup job is still writing it — run the drill "+
				"after the job finishes", name, orUnstated(meta.Status))
	}
}

// singleNode returns the one node the backup was taken on. The filesystem
// backend is single-node by Weaviate's own documentation, and a drill
// sandbox is a single node by construction, so a multi-node backup cannot
// be proven here and is refused rather than half-restored.
func singleNode(meta *backupMeta, name string) (string, *protoError) {
	if len(meta.Nodes) != 1 {
		return "", protoErr("invalid_request", false,
			"backup %s was taken on %d nodes: the filesystem backend and a drill sandbox are "+
				"both single-node, so only a single-node backup can be proven here",
			name, len(meta.Nodes))
	}
	for node := range meta.Nodes {
		return node, nil
	}
	return "", nil // unreachable
}

// verifyChunks is the completeness fence: every chunk the node manifest
// names must exist. A truncated chunk still passes — the engine judges
// content (measured) — but a file lost in a copy is caught here, by the
// backup's own claim about itself, before a byte moves.
func verifyChunks(dir, node string) ([]string, *protoError) {
	raw, perr := readMetaFile(filepath.Join(dir, node, nodeMetaFileName))
	if perr != nil {
		return nil, perr
	}
	if raw == nil {
		return nil, protoErr("source_corrupt", false,
			"backup %s names node %s but holds no %s/%s: the node manifest did not survive "+
				"the copy", filepath.Base(dir), node, node, nodeMetaFileName)
	}
	nm := &nodeMeta{}
	if err := json.Unmarshal(raw, nm); err != nil {
		return nil, protoErr("source_corrupt", false,
			"%s/%s in backup %s is not the JSON Weaviate writes: %v",
			node, nodeMetaFileName, filepath.Base(dir), err)
	}
	classes := make([]string, 0, len(nm.Classes))
	for _, class := range nm.Classes {
		classes = append(classes, class.Name)
		for chunk := range class.Chunks {
			p := filepath.Join(dir, node, class.Name, "chunk-"+chunk)
			if _, err := os.Stat(p); err != nil {
				return nil, protoErr("source_corrupt", false,
					"backup %s names chunk %s of class %s and the file is missing: the backup "+
						"did not survive the copy that brought it here",
					filepath.Base(dir), chunk, class.Name)
			}
		}
	}
	return classes, nil
}

// createdAtFrom dates the backup from its own completion instant —
// RFC 3339 UTC with the zone stated, so no timezone declaration is ever
// needed. An unparseable instant yields nil rather than a wrong claim.
func createdAtFrom(meta *backupMeta) *string {
	if _, err := time.Parse(time.RFC3339Nano, meta.CompletedAt); err != nil {
		return nil
	}
	s := meta.CompletedAt
	return &s
}

// backupCandidate is one subdirectory considered by weaviate_backup_dir.
type backupCandidate struct {
	name      string
	status    string
	started   time.Time
	completed time.Time
}

// newestBackupIn picks the backup whose own metadata claims the newest
// completion — never file times, which do not survive a copy.
//
// The pick is not a filter. A backup newer than the winner that is still
// running, or that failed, refuses the drill by name: silently proving an
// older backup while the directory holds a newer attempt would let the
// record imply something the operator does not have.
func newestBackupIn(dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	candidates, skipped := scanCandidates(dir, entries)
	winner := pickWinner(candidates)
	if winner == nil {
		return "", noWinnerError(dir, candidates, skipped)
	}
	for _, c := range candidates {
		if c.status != "SUCCESS" && c.started.After(winner.completed) {
			return "", refuseNewerAttempt(c)
		}
	}
	return filepath.Join(dir, winner.name), nil
}

// scanCandidates reads each subdirectory's own metadata; entries without a
// parseable one are counted rather than judged — they may be anything.
func scanCandidates(dir string, entries []os.DirEntry) ([]backupCandidate, int) {
	candidates := make([]backupCandidate, 0, len(entries))
	skipped := 0
	for _, e := range entries {
		if !e.IsDir() {
			skipped++
			continue
		}
		raw, perr := readMetaFile(filepath.Join(dir, e.Name(), metaFileName))
		if perr != nil || raw == nil {
			skipped++
			continue
		}
		meta := &backupMeta{}
		if err := json.Unmarshal(raw, meta); err != nil {
			skipped++
			continue
		}
		candidates = append(candidates, backupCandidate{
			name: e.Name(), status: meta.Status,
			started:   parseInstant(meta.StartedAt),
			completed: parseInstant(meta.CompletedAt),
		})
	}
	return candidates, skipped
}

// pickWinner ranks completed backups by their claimed completion instant;
// ties break toward the lexicographically larger name so the choice never
// depends on directory iteration order.
func pickWinner(candidates []backupCandidate) *backupCandidate {
	var winner *backupCandidate
	for i := range candidates {
		c := &candidates[i]
		if c.status != "SUCCESS" || c.completed.IsZero() {
			continue
		}
		if winner == nil || c.completed.After(winner.completed) ||
			(c.completed.Equal(winner.completed) && c.name > winner.name) {
			winner = c
		}
	}
	return winner
}

func noWinnerError(dir string, candidates []backupCandidate, skipped int) *protoError {
	for i := range candidates {
		// No completed backup at all, but attempts exist: say what state
		// the newest attempt is in rather than "nothing found".
		return refuseNewerAttempt(candidates[i])
	}
	if skipped > 0 {
		return protoErr("source_not_found", false,
			"backup directory %s holds no Weaviate backups (%d entries without a readable %s "+
				"were passed over)", dir, skipped, metaFileName)
	}
	return protoErr("source_not_found", false, "backup directory %s contains no files", dir)
}

func refuseNewerAttempt(c backupCandidate) *protoError {
	if c.status == "FAILED" {
		return protoErr("source_corrupt", false,
			"the newest backup %s reports status FAILED: fix the backup job, or point the "+
				"drill at the last good backup by name (kind weaviate_backup)", c.name)
	}
	return protoErr("source_unreadable", false,
		"the newest backup %s reports status %s: the backup job is still writing it — run "+
			"the drill after the job finishes", c.name, orUnstated(c.status))
}

// parseInstant reads one of the backup's own timestamps; anything
// unparseable ranks as the zero instant, which never wins.
func parseInstant(s string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// readMetaFile reads one metadata file under the size cap; a missing file
// is (nil, nil) so callers can phrase the refusal.
func readMetaFile(path string) ([]byte, *protoError) {
	f, err := os.Open(path)
	switch {
	case os.IsNotExist(err):
		return nil, nil
	case err != nil:
		return nil, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	raw, err := io.ReadAll(io.LimitReader(f, maxMetaBytes+1))
	if err != nil {
		return nil, protoErr("source_unreadable", false, "read %s: %v", filepath.Base(path), err)
	}
	if len(raw) > maxMetaBytes {
		return nil, protoErr("source_corrupt", false,
			"%s runs past %d MiB: the manifests Weaviate writes are kilobytes, so this is "+
				"not one", filepath.Base(path), maxMetaBytes>>20)
	}
	return raw, nil
}

// rejectBackupTimezone refuses the one source parameter other adapters
// accept that would be a silent no-op here: Weaviate's own timestamps
// carry their zone, so a declared one could only be ignored or wrong.
func rejectBackupTimezone(params map[string]string) *protoError {
	if _, ok := params["backup_timezone"]; !ok {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.backup_timezone is not accepted: a Weaviate backup states its "+
			"completion instant in UTC with the zone attached, so there is nothing a declared "+
			"timezone could correct")
}

// dirChecksum hashes a directory tree canonically: entries sorted by
// relative path; regular files contribute path, size, and content bytes,
// symlinks contribute path and target. The same tree always hashes the
// same, any content change changes the hash. This is the multi-file
// hashing rule §6.2 requires the README to document.
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
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func orUnstated(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unstated)"
	}
	return s
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(s, `"`, "'")
}

// treeFile pairs a host file with its destination inside the sandbox.
type treeFile struct {
	host string
	dest string
}

// treeEntries walks the backup tree once and returns the directory
// skeleton to create and the files to transfer, destination paths in
// sandbox (slash) form.
func treeEntries(hostDir, destDir string) ([]string, []treeFile, *protoError) {
	dirs := []string{destDir}
	var files []treeFile
	err := filepath.WalkDir(hostDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostDir, p)
		if err != nil || rel == "." {
			return err
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, path.Join(destDir, filepath.ToSlash(rel)))
		case d.Type().IsRegular():
			files = append(files, treeFile{
				host: p, dest: path.Join(destDir, filepath.ToSlash(rel)),
			})
		}
		return nil
	})
	if err != nil {
		return nil, nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	return dirs, files, nil
}
