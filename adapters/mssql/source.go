package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// resolvedSource is the backup identity of what a drill actually restored.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes
	sizeBytes int64
	// createdAt is the artifact's modification time (RFC 3339 UTC,
	// milliseconds) — the closest derivable stand-in for the backup's own
	// creation time; nil if unavailable.
	createdAt *string
	// loginsPath is the server-logins script to replay before the restore,
	// for the bak_with_logins kind; empty for every other kind.
	loginsPath string
}

// sourcePlan is everything the host can decide on its own. Which file in a
// directory is a full backup is not among them — only the engine can say
// that (see backupset.go) — so a directory source contributes candidates
// in the order they should be tried, and the choice happens in the sandbox.
type sourcePlan struct {
	// fixed is the artifact the drill config names outright; when it is
	// empty the candidates are scanned instead.
	fixed      string
	candidates []string // newest first
	skipped    []string // entries that are not backup media, for diagnostics
	dir        string   // the configured directory, for diagnostics
	loginsPath string   // bak_with_logins only
	// databaseName is the database a bak_chain drill restores, when the
	// directory holds backups of more than one and the config says which.
	databaseName string
	// loc is the zone the operator declared the backup host was in; nil
	// when none was declared, in which case no creation time is reported.
	loc *time.Location
}

// resolveSource maps a source kind to a plan for finding one restorable
// artifact.
//
//	bak             — path is a native BACKUP DATABASE file
//	bak_dir         — path is a directory of backup files
//	bak_with_logins — path is a directory holding a server-logins T-SQL
//	                  script (params.logins) and one or more .bak files
//	bak_chain       — path is a directory of backups replayed as a chain:
//	                  the newest full, its newest differential, and the
//	                  log backups that follow (see chain.go)
func resolveSource(kind, path string, params map[string]string) (*sourcePlan, *protoError) {
	loc, perr := backupLocation(params)
	if perr != nil {
		return nil, perr
	}
	switch kind {
	case "bak":
		if perr := mustBeFile(path); perr != nil {
			return nil, perr
		}
		return &sourcePlan{fixed: path, loc: loc}, nil
	case "bak_dir":
		candidates, skipped, perr := candidatesIn(path, "")
		if perr != nil {
			return nil, perr
		}
		return &sourcePlan{candidates: candidates, skipped: skipped, dir: path, loc: loc}, nil
	case "bak_chain":
		candidates, skipped, perr := candidatesIn(path, "")
		if perr != nil {
			return nil, perr
		}
		name := params["database_name"]
		if name != "" && !databasePattern.MatchString(name) {
			return nil, protoErr("invalid_request", false,
				"source.params.database_name %s must contain only letters, digits, underscores, and hyphens", name)
		}
		return &sourcePlan{candidates: candidates, skipped: skipped, dir: path, loc: loc, databaseName: name}, nil
	case "bak_with_logins":
		return planWithLogins(path, params, loc)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: bak, bak_dir, bak_with_logins, bak_chain)", kind)
	}
}

// planWithLogins plans the two-member source of the bak_with_logins kind:
// a server-logins script and one backup, both inside one source directory.
//
// One directory rather than two independent paths because the core only
// hands an adapter files belonging to the drill's configured backup source
// (protocol §4.2) — a guard that exists so an adapter, which is a
// third-party binary, cannot copy arbitrary host files into a sandbox it
// controls. The logins member is named explicitly in params rather than
// recognised by filename pattern: renaming a backup file must not silently
// change what a drill proves. The backup member may be named too; without
// it the directory is scanned like bak_dir, so a drill against a rotating
// directory keeps working unattended.
func planWithLogins(dir string, params map[string]string, loc *time.Location) (*sourcePlan, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the bak_with_logins kind expects a directory "+
				"holding the logins script and the backup", dir)
	}

	loginsName, perr := memberName(params["logins"], "logins")
	if perr != nil {
		return nil, perr
	}
	loginsPath := filepath.Join(dir, loginsName)
	if _, perr := statRegularFile(loginsPath, "logins script"); perr != nil {
		return nil, perr
	}

	plan := &sourcePlan{dir: dir, loginsPath: loginsPath, loc: loc}
	if requested := params["bak"]; requested != "" {
		name, perr := memberName(requested, "bak")
		if perr != nil {
			return nil, perr
		}
		if name == loginsName {
			return nil, protoErr("invalid_request", false,
				"source.params.bak and source.params.logins both name %s", name)
		}
		bakPath := filepath.Join(dir, name)
		if _, perr := statRegularFile(bakPath, "backup source"); perr != nil {
			return nil, perr
		}
		plan.fixed = bakPath
		return plan, nil
	}

	candidates, skipped, perr := candidatesIn(dir, loginsName)
	if perr != nil {
		return nil, perr
	}
	plan.candidates, plan.skipped = candidates, skipped
	return plan, nil
}

// identity measures the backup identity of the artifact the engine chose.
// It runs on the host, over the same bytes the sandbox received.
func (p *sourcePlan) identity(chosen string, createdAt *string) (*resolvedSource, *protoError) {
	if p.loginsPath == "" {
		info, perr := statRegularFile(chosen, "backup source")
		if perr != nil {
			return nil, perr
		}
		checksum, perr := fileChecksum(chosen)
		if perr != nil {
			return nil, perr
		}
		return &resolvedSource{
			path:      chosen,
			checksum:  checksum,
			sizeBytes: info.Size(),
			createdAt: createdAt,
		}, nil
	}
	return p.compositeIdentity(chosen, createdAt)
}

// compositeIdentity is the two-member identity of the bak_with_logins
// kind. Both members are restored, so both must be in it — a checksum
// covering only the backup would let the logins change without the
// evidence record noticing, and the logins are exactly what that kind
// exists to prove present. Only the two chosen members are hashed, not the
// whole directory: one directory may hold the logins script beside several
// databases' backups, each drilled separately, and a drill's identity must
// cover what that drill restored and nothing else. The construction
// mirrors the postgres adapter's two-member framing (role NUL size NUL
// content, fixed order), so the same pair always hashes the same and any
// change to either member changes the hash.
func (p *sourcePlan) compositeIdentity(chosen string, createdAt *string) (*resolvedSource, *protoError) {
	logins, perr := statRegularFile(p.loginsPath, "logins script")
	if perr != nil {
		return nil, perr
	}
	bak, perr := statRegularFile(chosen, "backup source")
	if perr != nil {
		return nil, perr
	}

	h := sha256.New()
	for _, m := range []struct {
		role string
		path string
		info os.FileInfo
	}{
		{"logins", p.loginsPath, logins},
		{"bak", chosen, bak},
	} {
		fmt.Fprintf(h, "%s\x00%d\x00", m.role, m.info.Size())
		if perr := copyInto(h, m.path); perr != nil {
			return nil, perr
		}
	}

	// The backup's own header dates this source. A logins script is
	// operator-authored and carries no timestamp, so the pair's freshness
	// rests on the member that can be dated — the README says so rather
	// than letting the field imply more.
	return &resolvedSource{
		path:       chosen,
		checksum:   fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))),
		sizeBytes:  logins.Size() + bak.Size(),
		createdAt:  createdAt,
		loginsPath: p.loginsPath,
	}, nil
}

// memberName validates a params entry naming a file inside the source
// directory. It is a bare filename, never a path: the core's put_file
// guard confines transfers to the configured backup source, and a plain
// name keeps a config's reach obvious to whoever reviews it.
func memberName(value, param string) (string, *protoError) {
	if value == "" {
		return "", protoErr("invalid_request", false,
			"the bak_with_logins kind requires source.params.%s: the name of the %s file "+
				"inside the source directory", param, param)
	}
	if value != filepath.Base(value) || value == "." || value == ".." {
		return "", protoErr("invalid_request", false,
			"source.params.%s must be a filename inside the source directory, not a path: %s",
			param, value)
	}
	return value, nil
}

// mustBeFile refuses a directory for a kind that names one artifact.
func mustBeFile(path string) *protoError {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return protoErr("invalid_request", false,
			"source path %s is a directory; use kind bak_dir for directories", path)
	}
	return nil
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

// candidatesIn lists a directory's backup media newest first, skipping the
// entry named except; ties break toward the lexicographically larger name
// so the order is deterministic. Files that do not start like backup media
// are returned separately rather than dropped, so a failure can say what
// it passed over.
func candidatesIn(dir, except string) (candidates, skipped []string, perr *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return nil, nil, protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return nil, nil, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}

	type entry struct {
		name  string
		mtime int64
	}
	files := make([]entry, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == except {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, nil, protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		files = append(files, entry{name: e.Name(), mtime: info.ModTime().UnixNano()})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].mtime != files[j].mtime {
			return files[i].mtime > files[j].mtime
		}
		return files[i].name > files[j].name
	})

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if looksLikeBackupMedia(path) {
			candidates = append(candidates, path)
			continue
		}
		skipped = append(skipped, f.name)
	}
	if len(candidates) == 0 && len(skipped) == 0 {
		return nil, nil, protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	return candidates, skipped, nil
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

// chainIdentity is the backup identity of a restore chain: every artifact
// the chain replays, hashed in restore order with the same framing the
// two-member kinds use (role NUL size NUL content), so the same chain
// always hashes the same and a change to any member changes the hash.
//
// The whole chain is the backup here — a checksum covering only the full
// would let a log backup change without the evidence record noticing,
// while the drill's result depends on every one of them. Files outside
// the chain are not in it, for the same reason a directory's other
// databases are not: a drill's identity covers what that drill restored
// and nothing else.
//
// A chain is only as current as its last member, and that member is what
// dates it: the log backup the recovery ends on.
func (p *sourcePlan) chainIdentity(sel *chainSelection, loc *time.Location) (*resolvedSource, *protoError) {
	h := sha256.New()
	var total int64
	// One artifact can contribute more than one set to a chain (appended
	// media), and its bytes are then hashed once per member — the member
	// list is what the identity describes, not the file list.
	for i, node := range sel.nodes {
		info, perr := statRegularFile(node.hostPath, "backup source")
		if perr != nil {
			return nil, perr
		}
		fmt.Fprintf(h, "%d:%s\x00%d\x00", i, backupTypeName(node.set.backupType), info.Size())
		if perr := copyInto(h, node.hostPath); perr != nil {
			return nil, perr
		}
		total += info.Size()
	}
	last := sel.nodes[len(sel.nodes)-1]
	return &resolvedSource{
		path:      last.hostPath,
		checksum:  fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))),
		sizeBytes: total,
		createdAt: backupFinishedAt(last.set.finishedAt, loc),
	}, nil
}
