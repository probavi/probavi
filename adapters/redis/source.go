package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// resolvedSource is a concrete backup artifact chosen for restore.
type resolvedSource struct {
	path      string
	checksum  string // "sha256:<hex>" over the artifact bytes (or set)
	sizeBytes int64
	createdAt *string // the RDB's own ctime aux field, nil when it has none
	redisVer  string  // the origin server's version, "" when unstated
	// aof is set when the artifact is an append-only directory rather
	// than one RDB file; the flow in ops.go branches on it.
	aof *aofArtifact
}

// resolveSource maps a source kind to one restorable artifact.
//
//	redis_rdb     — path is one RDB file (a copied dump.rdb, or the output
//	                of `redis-cli --rdb`)
//	redis_rdb_dir — path is a directory of them; the newest by the
//	                artifact's own save instant is restored
//	redis_aof     — path is a copy of the Redis 7+ append-only directory
//	                (manifest, base, incremental segments — see aof.go)
//
// An RDB records when it was saved (the ctime aux field, epoch seconds —
// see rdbmeta.go), so both the reported created_at and the directory
// ranking come from what the artifact states about itself. A file whose
// head carries no readable ctime ranks below every file that does, by
// file time among its like — an undatable artifact never outranks a dated
// one.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "redis_rdb":
		if perr := refuseDirectory(path); perr != nil {
			return nil, perr
		}
		return resolveFile(path)
	case "redis_rdb_dir":
		latest, perr := latestRDBIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(latest)
	case "redis_aof":
		return resolveAOF(path)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: redis_rdb, redis_rdb_dir, redis_aof)", kind)
	}
}

// resolveAOF vets an append-only directory and reads what its base
// states. The base RDB's header carries the same facts the rdb kinds
// read — the origin version for the pre-check, the Valkey markers for
// the dialect fence — while its ctime stays unreported: it dates the
// last rewrite, not the backup, so created_at is deliberately null
// (see aof.go).
func resolveAOF(path string) (*resolvedSource, *protoError) {
	art, perr := resolveAOFDir(path)
	if perr != nil {
		return nil, perr
	}
	src := &resolvedSource{path: path, aof: art}
	if strings.HasSuffix(art.baseName, ".rdb") {
		meta := readRDBMeta(filepath.Join(art.dir, art.baseName))
		if perr := refuseValkeyDialect(meta); perr != nil {
			return nil, perr
		}
		src.redisVer = meta.redisVer
	}
	checksum, size, perr := aofChecksum(art)
	if perr != nil {
		return nil, perr
	}
	src.checksum, src.sizeBytes = checksum, size
	return src, nil
}

func refuseDirectory(path string) *protoError {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return protoErr("source_unreadable", false, "stat backup source: %v", err)
	case info.IsDir():
		return protoErr("invalid_request", false,
			"source path %s is a directory; use kind redis_rdb_dir for directories", path)
	}
	return nil
}

func resolveFile(path string) (*resolvedSource, *protoError) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "backup source does not exist: %s", path)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat backup source: %v", err)
	}
	if perr := refuseGzip(path); perr != nil {
		return nil, perr
	}
	// Metadata is a bonus: an unparseable head resolves with nothing
	// dated and nothing versioned, and redis-check-rdb inside the sandbox
	// stays the authority on whether the file restores — except where the
	// head is positive evidence of a Valkey save, the one verdict only
	// this side can deliver by name (see refuseValkeyDialect).
	meta := readRDBMeta(path)
	if perr := refuseValkeyDialect(meta); perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	src := &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}
	src.redisVer = meta.redisVer
	if meta.ctime != 0 {
		src.createdAt = formatCreatedAt(time.Unix(meta.ctime, 0).UTC())
	}
	return src, nil
}

// refuseValkeyDialect refuses an artifact whose head is positive evidence
// of a Valkey save: the valkey-ver aux field Valkey has written — and
// redis-ver never — since the fork, or the VALKEY magic its 9.x writes
// (both measured against the official images). A pre-divergence Valkey
// RDB would in fact load — the formats are identical through version 11 —
// but a drill that restores a Valkey backup into Redis proves recovery
// into an engine the operator does not run, the false green ROADMAP.md
// names; the valkey adapter exists for that artifact. Absence stays
// silent: positive evidence only.
func refuseValkeyDialect(meta rdbMeta) *protoError {
	switch {
	case meta.valkeyVer != "":
		return protoErr("unsupported_source", false,
			"the RDB was saved by Valkey %s: restoring a Valkey backup into Redis would prove "+
				"recovery into an engine the backup does not belong to — use the valkey adapter "+
				"for this artifact", meta.valkeyVer)
	case meta.valkeyMagic:
		return protoErr("unsupported_source", false,
			"the RDB carries the VALKEY magic only Valkey 9+ writes — use the valkey adapter "+
				"for this artifact")
	}
	return nil
}

// gzipMagic is the two-byte gzip header. Compressing an RDB is a common
// backup-job habit, and handing the compressed bytes to the server would
// end in a bewildering load failure minutes later — refusing by name is
// kinder.
var gzipMagic = []byte{0x1f, 0x8b}

func refuseGzip(path string) *protoError {
	head, err := readHead(path, len(gzipMagic))
	if err != nil {
		return protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	if len(head) < len(gzipMagic) || head[0] != gzipMagic[0] || head[1] != gzipMagic[1] {
		return nil
	}
	return protoErr("unsupported_source", false,
		"backup source is gzip-compressed; this adapter restores plain RDB files — "+
			"decompress the artifact first, or point the drill at an uncompressed copy")
}

// rdbCandidate is one directory entry considered for restore.
type rdbCandidate struct {
	path  string
	ctime int64 // the artifact's own save instant, 0 when unstated
	mtime time.Time
}

// latestRDBIn picks the directory's newest RDB by what each artifact
// records about itself. Files without either RDB magic are skipped as
// non-candidates (checksum sidecars, README files); a Valkey artifact is
// a candidate, so that when it is the newest it is refused by name rather
// than silently passed over for an older neighbour — the same
// not-a-filter principle as settle.go. If nothing qualifies, the skipped
// names are counted so the refusal says what was passed over.
func latestRDBIn(ctx context.Context, dir string) (string, *protoError) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return "", protoErr("source_not_found", false, "backup directory does not exist: %s", dir)
	case err != nil:
		return "", protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	var best *rdbCandidate
	skipped := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		meta := readRDBMeta(path)
		if !meta.valid {
			skipped++
			continue
		}
		info, err := e.Info()
		if err != nil {
			return "", protoErr("source_unreadable", false, "stat %s: %v", e.Name(), err)
		}
		candidate := rdbCandidate{path: path, ctime: meta.ctime, mtime: info.ModTime()}
		if best == nil || candidate.beats(*best) {
			c := candidate
			best = &c
		}
	}
	if best == nil {
		if skipped > 0 {
			return "", protoErr("source_not_found", false,
				"backup directory %s holds no RDB files (%d files without the REDIS header were passed over)",
				dir, skipped)
		}
		return "", protoErr("source_not_found", false, "backup directory %s contains no files", dir)
	}
	// The adapter chose this file, not the operator: make sure a backup
	// job is not still writing it (see settle.go).
	if perr := assertSettled(ctx, best.path, settleWindow); perr != nil {
		return "", perr
	}
	return best.path, nil
}

// beats orders candidates: a dated artifact outranks every undated one, a
// newer save instant outranks an older, undated artifacts fall back to
// file time, and remaining ties break toward the lexicographically larger
// name so the choice is deterministic.
func (c rdbCandidate) beats(o rdbCandidate) bool {
	switch {
	case (c.ctime != 0) != (o.ctime != 0):
		return c.ctime != 0
	case c.ctime != o.ctime:
		return c.ctime > o.ctime
	case !c.mtime.Equal(o.mtime):
		return c.mtime.After(o.mtime)
	default:
		return filepath.Base(c.path) > filepath.Base(o.path)
	}
}

// fileChecksum streams the artifact once. The hash feeds the evidence
// record's backup identity, so it must be a real measurement of the bytes
// that will be restored.
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
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(h.Sum(nil))), nil
}

// createdAtLayout renders source_identity.created_at; the instant is
// already UTC (epoch seconds), so the offset is literal Z.
const createdAtLayout = "2006-01-02T15:04:05.000Z07:00"

// formatCreatedAt renders an instant for source_identity.created_at.
func formatCreatedAt(t time.Time) *string {
	s := t.Format(createdAtLayout)
	return &s
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// wall-clock formats the sibling adapters read need it; an RDB records
// its save instant as epoch seconds, which carry no zone question at
// all, and an append-only directory is deliberately not dated. A
// declaration is refused rather than ignored: silence would leave the
// operator believing it did something.
const backupTimezoneParam = "backup_timezone"

// rejectBackupTimezone refuses a declaration these formats make redundant.
func rejectBackupTimezone(params map[string]string) *protoError {
	if params[backupTimezoneParam] == "" {
		return nil
	}
	return protoErr("invalid_request", false,
		"source.params.%s has no effect for this adapter: an RDB dates itself in epoch seconds and "+
			"an append-only directory is deliberately not dated — remove the parameter",
		backupTimezoneParam)
}
