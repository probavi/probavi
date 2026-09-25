package main

import (
	"context"
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
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// createdAt is the backup's own creation time, read from the artifact
	// and placed in the operator-declared zone (see timestamp.go); nil
	// when the backup does not carry one or no zone was declared.
	createdAt *string
	// usersPath is the accounts-and-grants script to replay before the
	// dump, for the mysqldump_with_users kind; empty for every other kind.
	usersPath string
	// binlogsPath is the directory of binary logs to replay after the
	// physical restore, for the xtrabackup_with_binlogs kind; empty for
	// every other kind (binlog.go).
	binlogsPath string
	// compressed and usersCompressed report whether each SQL member is
	// stored gzip-compressed, sniffed from its own bytes (see compress.go).
	// The two members are sniffed separately: a backup pipeline may well
	// compress a multi-gigabyte dump and leave a small grants script plain.
	compressed      bool
	usersCompressed bool
	// marker and usersMarker are the patterns each member's end has to
	// match for the replay to count as complete, or "" for a member that
	// carries no ending to check (see complete.go).
	marker      string
	usersMarker string
}

// resolveSource maps a source kind to one restorable artifact.
//
//	mysqldump            — path is a mysqldump SQL file
//	mysqldump_dir        — path is a directory; which member is chosen is
//	                       params.select, newest by default (selection.go)
//	mysqldump_with_users — path is a directory holding an accounts-and-grants
//	                       script (params.users) and one dump
//	xtrabackup           — path is an XtraBackup full-backup directory
func resolveSource(ctx context.Context, kind, path string, params map[string]string) (*resolvedSource, *protoError) {
	loc, perr := backupLocation(params)
	if perr != nil {
		return nil, perr
	}
	policy, perr := backupSelection(kind, params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "mysqldump":
		return resolveFile(ctx, path, loc)
	case "mysqldump_dir":
		chosen, perr := chosenDumpIn(ctx, path, policy)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(ctx, chosen, loc)
	case "mysqldump_with_users":
		return resolveWithUsers(ctx, path, params, policy, loc)
	case "xtrabackup_with_binlogs":
		return resolveWithBinlogs(path, params, loc)
	case "xtrabackup":
		src, perr := resolveRepo(path, loc)
		if perr != nil {
			return nil, perr
		}
		// Every xtrabackup backup carries this metadata file; its absence
		// means the directory is something else — refuse before a single
		// byte is transferred.
		if _, err := os.Stat(filepath.Join(path, "xtrabackup_checkpoints")); err != nil {
			return nil, protoErr("source_corrupt", false,
				"backup directory %s lacks xtrabackup_checkpoints — not an xtrabackup backup", path)
		}
		return src, nil
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: mysqldump, mysqldump_dir, mysqldump_with_users, xtrabackup)", kind)
	}
}

// resolveWithUsers resolves the two-member source of the
// mysqldump_with_users kind: an accounts-and-grants script and one dump,
// both named inside one source directory.
//
// One directory rather than two independent paths because the core only
// hands an adapter files belonging to the drill's configured backup source
// (protocol §4.2) — a guard that exists so an adapter, which is a
// third-party binary, cannot copy arbitrary host files into a sandbox it
// controls. The members are named explicitly in params rather than
// recognised by filename pattern: renaming a backup file must not silently
// change what a drill proves.
//
// Both members are restored, so both must be in the backup identity — a
// checksum covering only the dump would let the users script change
// without the evidence record noticing, and the accounts are exactly what
// this kind exists to prove present. Only the two chosen members are
// hashed, not the whole directory: one directory may hold the script
// beside several databases' dumps, each drilled separately, and a drill's
// identity must cover what that drill restored and nothing else. The
// construction mirrors the other in-repo adapters' two-member framing
// (role NUL size NUL content, fixed order), so the same pair always hashes
// the same and any change to either member changes the hash.
func resolveWithUsers(ctx context.Context, dir string, params map[string]string,
	policy selectPolicy, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the mysqldump_with_users kind expects a directory "+
				"holding the users script and the dump", dir)
	}

	usersName, perr := memberName("mysqldump_with_users", params["users"], "users", "users script")
	if perr != nil {
		return nil, perr
	}
	usersPath := filepath.Join(dir, usersName)
	users, perr := statRegularFile(usersPath, "users script")
	if perr != nil {
		return nil, perr
	}

	dumpPath, perr := chooseDump(ctx, dir, params["dump"], usersName, policy)
	if perr != nil {
		return nil, perr
	}
	dump, perr := statRegularFile(dumpPath, "backup source")
	if perr != nil {
		return nil, perr
	}

	h := sha256.New()
	for _, m := range []struct {
		role string
		path string
		info os.FileInfo
	}{
		{"users", usersPath, users},
		{"dump", dumpPath, dump},
	} {
		fmt.Fprintf(h, "%s\x00%d\x00", m.role, m.info.Size())
		if perr := copyInto(h, m.path); perr != nil {
			return nil, perr
		}
	}

	dumpCompressed, perr := sniffCompressed(dumpPath)
	if perr != nil {
		return nil, perr
	}
	usersCompressed, perr := sniffCompressed(usersPath)
	if perr != nil {
		return nil, perr
	}

	// The dump's own trailer dates this source. An accounts-and-grants
	// script is operator-authored and carries no timestamp, so the pair's
	// freshness rests on the member that can be dated — the README says so
	// rather than letting the field imply more.
	return &resolvedSource{
		path:            dumpPath,
		checksum:        fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))),
		sizeBytes:       users.Size() + dump.Size(),
		createdAt:       dumpCompletedAt(ctx, dumpPath, loc),
		usersPath:       usersPath,
		compressed:      dumpCompressed,
		usersCompressed: usersCompressed,
		marker:          completenessMarker(dumpPath),
		usersMarker:     completenessMarker(usersPath),
	}, nil
}

// memberName validates a params entry naming a file inside the source
// directory. It is a bare filename, never a path: the core's put_file
// guard confines transfers to the configured backup source, and a plain
// name keeps a config's reach obvious to whoever reviews it.
func memberName(kind, value, param, what string) (string, *protoError) {
	if value == "" {
		return "", protoErr("invalid_request", false,
			"the %s kind requires source.params.%s: the name of the %s "+
				"inside the source directory", kind, param, what)
	}
	if value != filepath.Base(value) || value == "." || value == ".." {
		return "", protoErr("invalid_request", false,
			"source.params.%s must be a filename inside the source directory, not a path: %s",
			param, value)
	}
	return value, nil
}

// chooseDump resolves which dump the drill restores: the one params.dump
// names, or — so a drill against a rotating backup directory keeps working
// unattended — the one params.select picks from the files beside the users
// script.
func chooseDump(ctx context.Context, dir, requested, usersName string,
	policy selectPolicy) (string, *protoError) {
	if requested != "" {
		name, perr := memberName("mysqldump_with_users", requested, "dump", "dump file")
		if perr != nil {
			return "", perr
		}
		if name == usersName {
			return "", protoErr("invalid_request", false,
				"source.params.dump and source.params.users both name %s", name)
		}
		return filepath.Join(dir, name), nil
	}
	chosen, perr := chooseBackupIn(ctx, dir, usersName, policy)
	if perr != nil {
		return "", perr
	}
	if chosen == "" {
		return "", protoErr("source_not_found", false,
			"backup directory %s holds no dump beside the users script %s", dir, usersName)
	}
	// The adapter chose this file, not the operator: make sure a backup job
	// is not still writing it (see settle.go).
	if perr := assertSettled(ctx, chosen, settleWindow); perr != nil {
		return "", perr
	}
	return chosen, nil
}

// statRegularFile stats a source member that must exist as a plain file;
// what names it in diagnostics.
func statRegularFile(path, what string) (os.FileInfo, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "%s does not exist: %s", what, path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat %s: %v", what, err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false, "%s %s is a directory, not a file", what, path)
	}
	return info, nil
}

// resolveRepo resolves a directory source: the checksum is a canonical hash
// over the whole tree (documented in the adapter README), created_at is the
// newest file's mtime.
func resolveRepo(dir string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the xtrabackup kind expects a backup directory", dir)
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	// The backup dates itself through xtrabackup_info, in the declared
	// zone (see backuptime.go).
	return &resolvedSource{
		path:      dir,
		checksum:  checksum,
		sizeBytes: size,
		createdAt: backupCreatedAt(dir, loc),
	}, nil
}

// resolveWithBinlogs plans the two-member source of the
// xtrabackup_with_binlogs kind: a physical full and the binary logs
// written after it, both inside one source directory.
//
// One directory rather than two independent paths because the core only
// hands an adapter files belonging to the drill's configured backup source
// (protocol §4.2) — a guard that exists so an adapter, which is a
// third-party binary, cannot copy arbitrary host files into a sandbox it
// controls. A server's live binary log directory is therefore not
// something a drill can point at: an archive copies the logs beside the
// full it belongs to, which is the layout a run-book wants anyway.
//
// Both members are named explicitly in params rather than recognised by
// layout: renaming a directory must not silently change what a drill
// proves, and the same source directory may hold several nights' fulls.
func resolveWithBinlogs(dir string, params map[string]string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the xtrabackup_with_binlogs kind expects a directory "+
				"holding the backup and the binary logs", dir)
	}

	backupName, perr := memberName("xtrabackup_with_binlogs", params["backup"], "backup",
		"xtrabackup backup directory")
	if perr != nil {
		return nil, perr
	}
	binlogsName, perr := memberName("xtrabackup_with_binlogs", params["binlogs"], "binlogs",
		"binary log directory")
	if perr != nil {
		return nil, perr
	}
	if backupName == binlogsName {
		return nil, protoErr("invalid_request", false,
			"source.params.backup and source.params.binlogs both name %s", backupName)
	}

	backupPath := filepath.Join(dir, backupName)
	if perr := mustBeDirectory(backupPath, "backup"); perr != nil {
		return nil, perr
	}
	if _, err := os.Stat(filepath.Join(backupPath, "xtrabackup_checkpoints")); err != nil {
		return nil, protoErr("source_corrupt", false,
			"backup directory %s lacks xtrabackup_checkpoints — not an xtrabackup backup", backupPath)
	}
	binlogsPath := filepath.Join(dir, binlogsName)
	if perr := mustBeDirectory(binlogsPath, "binary log"); perr != nil {
		return nil, perr
	}

	// Both members are restored, so both are in the identity: a checksum
	// covering only the full would let a log change without the evidence
	// record noticing, and the logs are exactly what this kind exists to
	// prove. Each member contributes its own canonical tree digest rather
	// than its bytes a second time — dirChecksum is already a measurement
	// of every byte under it, and re-streaming two trees would double the
	// read for no added guarantee. The framing is the two-member one the
	// rest of the catalogue uses (role NUL size NUL value, fixed order).
	backupSum, backupSize, perr := dirChecksum(backupPath)
	if perr != nil {
		return nil, perr
	}
	binlogSum, binlogSize, perr := dirChecksum(binlogsPath)
	if perr != nil {
		return nil, perr
	}
	h := sha256.New()
	for _, m := range []struct {
		role  string
		size  int64
		value string
	}{
		{"backup", backupSize, backupSum},
		{"binlogs", binlogSize, binlogSum},
	} {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", m.role, m.size, m.value)
	}

	// The full dates this source, through its own xtrabackup_info. The
	// logs reach further forward in time and the record does not claim
	// otherwise: backup.created_at is when the backup was taken, and how
	// far the replay carried it is what drill.pitr_target records.
	return &resolvedSource{
		path:        backupPath,
		checksum:    fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))),
		sizeBytes:   backupSize + binlogSize,
		createdAt:   backupCreatedAt(backupPath, loc),
		binlogsPath: binlogsPath,
	}, nil
}

// mustBeDirectory refuses a member that is not a directory; what names it
// in the diagnostic.
func mustBeDirectory(path, what string) *protoError {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return protoErr("source_not_found", false, "%s directory does not exist: %s", what, path)
	case err != nil:
		return protoErr("source_unreadable", false, "stat %s directory: %v", what, err)
	case !info.IsDir():
		return protoErr("invalid_request", false, "%s %s is a file, not a directory", what, path)
	}
	return nil
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
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, cerr := io.Copy(h, f)
		if err := f.Close(); err != nil && cerr == nil {
			cerr = err
		}
		return cerr
	case d.Type()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00L%s\x00", rel, target)
	}
	return nil
}

// resolveFile resolves a single-dump source. The checksum and the reported
// size cover the artifact as stored, compressed or not: those bytes are the
// ones the operator retains, and the evidence record has to identify what
// is in the backup archive rather than something derived from it.
func resolveFile(ctx context.Context, path string, loc *time.Location) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a directory; use kind mysqldump_dir for directories", path)
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	compressed, perr := sniffCompressed(path)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path:       path,
		checksum:   checksum,
		sizeBytes:  info.Size(),
		createdAt:  dumpCompletedAt(ctx, path, loc),
		compressed: compressed,
		marker:     completenessMarker(path),
	}, nil
}

// chosenDumpIn picks the dump in dir that the policy asks for (see
// chooseBackupIn and selection.go).
func chosenDumpIn(ctx context.Context, dir string, policy selectPolicy) (string, *protoError) {
	best, perr := chooseBackupIn(ctx, dir, "", policy)
	if perr != nil {
		return "", perr
	}
	if best == "" {
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	// The adapter chose this file, not the operator: make sure a backup job
	// is not still writing it (see settle.go).
	if perr := assertSettled(ctx, best, settleWindow); perr != nil {
		return "", perr
	}
	return best, nil
}

// chooseBackupIn returns the backup a directory source should restore
// under policy, skipping the entry named except. An empty result means
// the directory is readable but holds no candidate — the caller says what
// that means.
//
// Candidates are ordered by the time the backup records about itself, not
// by the file's modification time. A backup copied into the directory
// afterwards — cp without -p, an object-store download, an rsync without
// -t — carries a fresh mtime, so under the old rule a stale artifact
// became "the newest file" and was the one the drill proved. What a
// backup says about itself does not move when the file is copied.
//
// A compressed candidate has to be decompressed to reach that sentence
// (see readDumpTail), which makes ordering a directory of compressed
// dumps cost one pass over each candidate. The rule is what matters here
// and it stays one rule for both storage forms and for every policy; the
// adapter README states the price so an operator can see it before
// pointing a drill at a directory.
func chooseBackupIn(ctx context.Context, dir, except string, policy selectPolicy) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	candidates := make([]dirCandidate, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == except {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		path := filepath.Join(dir, e.Name())
		clock, dated := dumpClock(ctx, path)
		candidates = append(candidates, dirCandidate{
			path: path, name: e.Name(), clock: clock, dated: dated, mtime: info.ModTime(),
		})
	}
	if len(candidates) == 0 {
		return "", nil
	}
	return pick(candidates, policy).path, nil
}

// dirCandidate is one file a directory source could restore, with the two
// times that can order it.
type dirCandidate struct {
	path  string
	name  string
	clock time.Time // what the backup records about itself
	dated bool      // whether that clock could be read at all
	mtime time.Time
}

// beats orders two candidates for the newest policy. A backup that can be
// dated from its own bytes wins over one that cannot: the drill would
// rather restore the backup it can also say something true about. Between
// two dated candidates the newer recorded clock wins; otherwise the rule
// that applied before this ordering existed still decides — newer file,
// then the lexicographically larger name, so the choice never depends on
// directory iteration order.
func (c dirCandidate) beats(other dirCandidate) bool {
	switch {
	case c.dated != other.dated:
		return c.dated
	case c.dated && !c.clock.Equal(other.clock):
		return c.clock.After(other.clock)
	case !c.mtime.Equal(other.mtime):
		return c.mtime.After(other.mtime)
	default:
		return c.name > other.name
	}
}

// precedes orders two candidates for the oldest policy — and is not the
// negation of beats, which is the whole reason it is written out. The
// first rule does not invert: datedness is not a clock, so a backup that
// cannot be dated at all is not "the oldest one", it is the one this
// adapter can say least about, and under either policy it loses to a
// candidate carrying its own time. Only the three comparisons after it
// turn around.
func (c dirCandidate) precedes(other dirCandidate) bool {
	switch {
	case c.dated != other.dated:
		return c.dated
	case c.dated && !c.clock.Equal(other.clock):
		return c.clock.Before(other.clock)
	case !c.mtime.Equal(other.mtime):
		return c.mtime.Before(other.mtime)
	default:
		return c.name < other.name
	}
}

// copyInto streams a file's bytes into h.
func copyInto(h io.Writer, path string) *protoError {
	f, err := os.Open(path)
	if err != nil {
		return protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	_, cerr := io.Copy(h, f)
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	return nil
}

// fileChecksum streams the artifact once; the hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored.
func fileChecksum(path string) (string, *protoError) {
	h := sha256.New()
	if perr := copyInto(h, path); perr != nil {
		return "", perr
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}
