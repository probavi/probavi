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
	valkeyVer string  // the origin server's version, "" when unstated
	// aof is set when the artifact is an append-only directory rather
	// than one RDB file; the flow in ops.go branches on it.
	aof *aofArtifact
}

// resolveSource maps a source kind to one restorable artifact.
//
//	valkey_rdb     — path is one RDB file (a copied dump.rdb, or the
//	                 output of `valkey-cli --rdb`)
//	valkey_rdb_dir — path is a directory of them; the newest by the
//	                 artifact's own save instant is restored
//	valkey_aof     — path is a copy of the append-only directory Valkey
//	                 kept from Redis 7 (manifest, base, incremental
//	                 segments — see aof.go)
//
// An RDB records when it was saved (the ctime aux field, epoch seconds —
// see rdbmeta.go), so both the reported created_at and the directory
// ranking come from what the artifact states about itself. A file whose
// head carries no readable ctime ranks below every file that does, by
// file time among its like — an undatable artifact never outranks a dated
// one.
func resolveSource(ctx context.Context, kind, path string) (*resolvedSource, *protoError) {
	switch kind {
	case "valkey_rdb":
		if perr := refuseDirectory(path); perr != nil {
			return nil, perr
		}
		return resolveFile(path)
	case "valkey_rdb_dir":
		latest, perr := latestRDBIn(ctx, path)
		if perr != nil {
			return nil, perr
		}
		return resolveFile(latest)
	case "valkey_aof":
		return resolveAOF(path)
	default:
		return nil, protoErr("unsupported_source", false,
			"unsupported source kind: %s (supported: valkey_rdb, valkey_rdb_dir, valkey_aof)", kind)
	}
}

// resolveAOF vets an append-only directory and reads what its base
// states. The base RDB's header carries the same facts the rdb kinds
// read — the origin version for the pre-check, the Redis-dialect
// markers for the fence — while its ctime stays unreported: it dates
// the last rewrite, not the backup, so created_at is deliberately null
// (see aof.go).
func resolveAOF(path string) (*resolvedSource, *protoError) {
	art, perr := resolveAOFDir(path)
	if perr != nil {
		return nil, perr
	}
	src := &resolvedSource{path: path, aof: art}
	if strings.HasSuffix(art.baseName, ".rdb") {
		meta := readRDBMeta(filepath.Join(art.dir, art.baseName))
		if perr := refuseRedisDialect(meta); perr != nil {
			return nil, perr
		}
		src.valkeyVer = meta.valkeyVer
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
			"source path %s is a directory; use kind valkey_rdb_dir for directories", path)
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
	// Metadata is a bonus — an unparseable head resolves with nothing
	// dated and nothing versioned, and valkey-check-rdb inside the
	// sandbox stays the authority on whether the file restores — except
	// where the head is positive evidence of the other dialect, which is
	// the one verdict only this side can deliver in time (see
	// refuseRedisDialect).
	meta := readRDBMeta(path)
	if perr := refuseRedisDialect(meta); perr != nil {
		return nil, perr
	}
	checksum, perr := fileChecksum(path)
	if perr != nil {
		return nil, perr
	}
	src := &resolvedSource{path: path, checksum: checksum, sizeBytes: info.Size()}
	src.valkeyVer = meta.valkeyVer
	if meta.ctime != 0 {
		src.createdAt = formatCreatedAt(time.Unix(meta.ctime, 0).UTC())
	}
	return src, nil
}

// refuseRedisDialect refuses an artifact whose head is positive evidence
// of a Redis save. Two grounds, both measured:
//
// A redis-ver aux field: Valkey has written valkey-ver and never
// redis-ver since the fork, so the field can only come from Redis. A
// pre-divergence Redis RDB would in fact load — the formats are identical
// through version 11 — but a drill that restores a Redis backup into
// Valkey proves recovery into an engine the operator does not run, which
// is the false green ROADMAP.md names; the redis adapter exists for that
// artifact.
//
// A REDIS-magic format version of 12 or above: only post-fork Redis
// writes those, no Valkey engine loads them, and valkey-check-rdb passes
// them anyway — the server would then die at startup ("Can't handle RDB
// format version 12") minutes after a clean integrity check. Refusing
// here, before a byte is transferred, is the only timely place.
//
// Absence stays silent: an artifact with neither field is refused by
// nothing — positive evidence only.
func refuseRedisDialect(meta rdbMeta) *protoError {
	if meta.redisVer != "" && meta.valkeyVer == "" {
		return protoErr("unsupported_source", false,
			"the RDB was saved by Redis %s: restoring a Redis backup into Valkey would prove "+
				"recovery into an engine the backup does not belong to — use the redis adapter "+
				"for this artifact", meta.redisVer)
	}
	if meta.valid && !meta.valkeyMagic && meta.formatVersion >= redisDialectFloor {
		return protoErr("unsupported_source", false,
			"the RDB carries format version %d, a post-fork Redis dialect no Valkey engine "+
				"loads — use the redis adapter for this artifact", meta.formatVersion)
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
// non-candidates (checksum sidecars, README files); a Redis-dialect
// artifact is a candidate, so that when it is the newest it is refused by
// name rather than silently passed over for an older neighbour — the same
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
				"backup directory %s holds no RDB files (%d files without an RDB header were passed over)",
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
