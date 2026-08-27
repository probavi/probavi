package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "postgres"
	adapterVersion = "0.13.0"

	defaultUser     = "postgres"
	defaultDatabase = "postgres"
	defaultPort     = 5432

	readinessBudget = 2 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "postgresql"},
		"sources": []map[string]any{
			{"kind": "pgdump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "pgdump_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "pgdump_with_globals", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "timescaledb_dump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "timescaledb_dump_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "pgbackrest", "capabilities": map[string]bool{"pitr": true}},
		},
		"sql_runner": map[string]any{
			"argv": []string{"psql", "-U", "{{user}}", "-d", "{{database}}",
				"-tA", "-v", "ON_ERROR_STOP=1", "-c", "{{sql}}"},
			"env": map[string]string{},
		},
		"verbs_required": []string{"exec", "put_file"},
	}
}

// provisionRequest is the §6.2 request payload.
type provisionRequest struct {
	Source struct {
		Kind          string            `json:"kind"`
		Path          string            `json:"path"`
		Params        map[string]string `json:"params"`
		CredentialEnv []string          `json:"credential_env"`
	} `json:"source"`
	Sandbox struct {
		ScratchDir string `json:"scratch_dir"`
	} `json:"sandbox"`
	Options map[string]string `json:"options"`
	PITR    *struct {
		TargetTime string `json:"target_time"`
	} `json:"pitr"`
}

// opProvision restores the backup into the already-running sandbox:
// wait for engine readiness (TCP, not socket — the initdb temporary server
// answers socket probes), transfer the dump, pg_restore it.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil && req.Source.Kind != "pgbackrest" {
		return nil, protoErr("invalid_request", false, "pitr is only supported by the pgbackrest source kind")
	}
	user := option(req.Options, "user", defaultUser)
	database := option(req.Options, "database", defaultDatabase)
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path, req.Source.Params)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes)

	if req.Source.Kind == "pgbackrest" {
		return provisionPhysical(ctx, c, req, src, logger)
	}

	readySeconds, perr := awaitEngine(ctx, c, user)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine ready", "seconds", readySeconds)

	state := map[string]any{"database": database, "user": user}
	// Cluster globals go in before the dump: a restored GRANT names roles
	// that must already exist. Their load is part of the restore, not a
	// phase of its own — the measured restore duration is the drill's RTO
	// figure, and the real recovery path includes this step.
	var globalsTransfer, globalsRestore float64
	if src.globalsPath != "" {
		globals := sandboxMember(scratch+"/probavi-globals.sql", src.globalsStorage)
		globalsTransfer, globalsRestore, perr = loadGlobals(ctx, c, user, src.globalsPath, globals)
		if perr != nil {
			return nil, perr
		}
		logger.Info("cluster globals loaded", "seconds", globalsRestore)
		state["globals_path"] = globals.path
	}

	dump := sandboxDump(scratch, src.storage)
	state["dump_path"] = dump.path
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dump.path, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}

	restoreSeconds, perr := restoreDump(ctx, c, user, database, dump, src.timescale)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "seconds", restoreSeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "postgresql", "host": "127.0.0.1", "port": defaultPort,
			"database": database, "user": user,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     globalsTransfer + put.DurationSeconds,
			"restore_seconds":      globalsRestore + restoreSeconds,
		},
		"state": state,
	}, nil
}

// restoreDump runs the restore — framed with the timescale procedure
// when the source demands it — and returns the measured seconds of
// everything the real recovery path cannot skip.
func restoreDump(ctx context.Context, c *core, user, database string, dump sandboxFile, framed bool) (float64, *protoError) {
	var framingSeconds float64
	if framed {
		var perr *protoError
		framingSeconds, perr = beginTimescaleFrame(ctx, c, user, database)
		if perr != nil {
			return 0, perr
		}
	}
	restore, stderr, perr := execRestore(ctx, c, user, database, dump, framed)
	if perr != nil {
		return 0, perr
	}
	if restore.ExitCode != 0 {
		return 0, mapRestoreFailure(restore.ExitCode, stderr, dump.storage)
	}
	if framed {
		pin, perr := pinPolicyJobs(ctx, c, user, database)
		if perr != nil {
			return 0, perr
		}
		post, perr := endTimescaleFrame(ctx, c, user, database)
		if perr != nil {
			return 0, perr
		}
		framingSeconds += pin + post
	}
	return restore.DurationSeconds + framingSeconds, nil
}

// beginTimescaleFrame prepares the target database the way the
// extension's own restore procedure demands: the extension first — the
// dump repeats CREATE EXTENSION IF NOT EXISTS and skips with a NOTICE
// (measured), so --exit-on-error survives — then timescaledb_pre_restore(),
// which stops background workers from racing the catalog copy. Without
// the frame a production-shaped dump restores partially (measured, see
// the fence above).
func beginTimescaleFrame(ctx context.Context, c *core, user, database string) (float64, *protoError) {
	create, stderr, perr := psqlStatement(ctx, c, user, database, "CREATE EXTENSION IF NOT EXISTS timescaledb")
	if perr != nil {
		return 0, perr
	}
	if create.ExitCode != 0 {
		line := firstLine(stderr)
		if strings.Contains(line, "is not available") || strings.Contains(line, "extension control file") {
			return 0, protoErr("invalid_request", false,
				"the sandbox image provides no timescaledb extension, which the timescaledb_dump "+
					"kind restores with: use a timescale/timescaledb image (or one carrying the "+
					"extension at the backup's version): %s", line)
		}
		return 0, protoErr("restore_failed", false, "creating the timescaledb extension failed: %s", line)
	}
	pre, stderr, perr := psqlStatement(ctx, c, user, database, "SELECT timescaledb_pre_restore()")
	if perr != nil {
		return 0, perr
	}
	if pre.ExitCode != 0 {
		return 0, protoErr("restore_failed", false, "timescaledb_pre_restore() failed: %s", firstLine(stderr))
	}
	return create.DurationSeconds + pre.DurationSeconds, nil
}

// endTimescaleFrame completes the procedure. A restore left in the
// restoring state is not a recovered database — background jobs stay
// down and the catalog write protection stays on — so a failure here is
// a failed restore, not a warning.
func endTimescaleFrame(ctx context.Context, c *core, user, database string) (float64, *protoError) {
	post, stderr, perr := psqlStatement(ctx, c, user, database, "SELECT timescaledb_post_restore()")
	if perr != nil {
		return 0, perr
	}
	if post.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the restore completed but timescaledb_post_restore() failed, which leaves the "+
				"database in the restoring state: %s", firstLine(stderr))
	}
	return post.DurationSeconds, nil
}

// pinPolicyJobsSQL holds every job in the restored catalog out of reach
// for the life of the sandbox.
//
// next_start is the deliberate lever rather than the job's scheduled
// flag. The dump does not carry bgw_job_stat — measured: straight after
// pg_restore a restored job exists with scheduled set and no statistics
// at all — so writing next_start fills a field the restore left empty and
// overwrites nothing the backup contained, while the scheduled flag the
// backup did carry stays exactly as it was. A drill that reads the
// restored policies still sees what the backup held.
const pinPolicyJobsSQL = `WITH pinned AS (` +
	`SELECT alter_job(job_id, next_start => 'infinity') FROM timescaledb_information.jobs) ` +
	`SELECT count(*) FROM pinned`

// pinPolicyJobs stops the restored database's own automation from acting
// on the artifact, inside the window the frame already owns: after the
// restore, while timescaledb_pre_restore() still holds the background
// workers down.
//
// It has to be here, because timescaledb_post_restore() does not merely
// release the workers — the retention policy runs in the same second it
// returns (measured: 15 of 29 chunks and 52% of the rows gone before
// post_restore's own call finished, on a hypertable holding 200 days
// under a 90-day policy). Nothing is racing: bgw_job_stat is absent from
// the dump, so a restored job has no next_start and the scheduler treats
// it as due immediately. A policy is a statement about what a running
// database should keep; a drill proves what the backup holds, and the
// operator's real policy is already expressed in which chunks the dump
// contains.
//
// A failure fails the restore. Carrying on would hand back a record of a
// database that deleted part of itself between the restore and the first
// check, with the restore reported successful — which is precisely the
// evidence this product must never produce.
func pinPolicyJobs(ctx context.Context, c *core, user, database string) (float64, *protoError) {
	pin, stderr, perr := psqlStatement(ctx, c, user, database, pinPolicyJobsSQL)
	if perr != nil {
		return 0, perr
	}
	if pin.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the restored policy jobs could not be held back: %s — releasing the background "+
				"workers would let them act on the artifact (measured: a retention policy drops "+
				"its chunks in the same second the restore frame closes), so the drill would "+
				"prove a database that deleted part of itself", firstLine(stderr))
	}
	return pin.DurationSeconds, nil
}

// psqlStatement runs one SQL statement the frame needs, with the same
// client shape the healthcheck uses.
func psqlStatement(ctx context.Context, c *core, user, database, sql string) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"psql", "-h", "127.0.0.1", "-U", user, "-d", database,
			"-tA", "-v", "ON_ERROR_STOP=1", "-c", sql},
	})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
		User     string `json:"user"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the provisioned instance serves queries (§6.3).
// An unhealthy engine is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	user := req.Connection.User
	if user == "" {
		user = defaultUser
	}
	database := req.Connection.Database
	if database == "" {
		database = defaultDatabase
	}
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"psql", "-h", "127.0.0.1", "-U", user, "-d", database,
			"-tA", "-v", "ON_ERROR_STOP=1", "-c", "SELECT 1"},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1"
	detail := "accepting queries"
	if !healthy {
		detail = fmt.Sprintf("psql exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// awaitEngine polls pg_isready over TCP until the engine accepts
// connections. The initdb-phase temporary server only listens on the unix
// socket, so a TCP probe cannot report ready too early (PoC finding 1).
func awaitEngine(ctx context.Context, c *core, user string) (float64, *protoError) {
	start := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"pg_isready", "-h", "127.0.0.1", "-U", user, "-q"},
			TimeoutSeconds: 5,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(start).Seconds(), nil
		}
		if time.Since(start) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"engine did not accept TCP connections within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// sandboxFile is one file placed inside the sandbox, and how it is stored
// there. A compressed member keeps the extension its bytes call for, so a
// person reading the adapter log or an exec argv sees what the file is.
type sandboxFile struct {
	path    string
	storage dumpStorage
}

func sandboxMember(path string, storage dumpStorage) sandboxFile {
	if storage.compressed {
		path += ".gz"
	}
	return sandboxFile{path: path, storage: storage}
}

// sandboxDump names the dump inside the sandbox after what it actually is,
// which the two clients disagree about: pg_restore reads an archive, psql
// replays a script, and neither accepts the other's file.
func sandboxDump(scratch string, storage dumpStorage) sandboxFile {
	name := scratch + "/probavi-restore.dump"
	if storage.plain {
		name = scratch + "/probavi-restore.sql"
	}
	return sandboxMember(name, storage)
}

func execRestore(ctx context.Context, c *core, user, database string, dump sandboxFile, framed bool) (*execValue, []byte, *protoError) {
	argv := []string{"sh", "-c", archiveRestoreScript, "sh",
		dump.path, user, database, archiveFence(framed)}
	switch {
	case dump.storage.plain:
		// A dump's own statements are the drill: the first one that fails
		// ends the restore rather than leaving a half-restored database
		// looking healthy (§5).
		argv = psqlReplayArgv(dump, user, database, errorStopOn, scriptFence(framed))
	case dump.storage.compressed:
		argv = []string{"sh", "-c", compressedArchiveRestoreScript, "sh",
			dump.path, user, database}
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// The timescale fence: a dump that creates the timescaledb extension must
// not be restored unframed. Measured on 2.29.1: a dump with compressed
// chunks restores partially under the plain flow — pg_restore aborts with
// "could not find hypertable with id 1" after a fraction of the rows —
// while a trivial hypertable happens to restore, so whether the plain
// flow breaks depends on the backup's shape, which is exactly what an
// operator must not have to know. The fence reads positive evidence only:
// the archive's own table of contents, or the extension statement in a
// script's head (pg_dump writes extensions before any data). A compressed
// custom-format archive offers no exact probe without inflating it, so
// that one form goes unfenced — its unframed restore still fails loudly,
// never silently.
const (
	// timescaleTOCMark is the pg_restore -l line naming the extension —
	// exact, and distinct from the COMMENT entry ("COMMENT - EXTENSION").
	timescaleTOCMark = " EXTENSION - timescaledb"
	// timescaleScriptPattern matches the statement pg_dump writes; the
	// bounded head is enough because extensions precede all data.
	timescaleScriptPattern = `^CREATE EXTENSION (IF NOT EXISTS )?"?timescaledb"?`
	// timescaleFenceHeadBytes bounds the script scan.
	timescaleFenceHeadBytes = "65536"
)

// archiveFence arms the custom-format fence; the framed kinds pass the
// empty string, which the scripts read as "do not fence".
func archiveFence(framed bool) string {
	if framed {
		return ""
	}
	return timescaleTOCMark
}

// scriptFence is archiveFence for the plain-SQL replay scripts.
func scriptFence(framed bool) string {
	if framed {
		return ""
	}
	return timescaleScriptPattern
}

// archiveRestoreScript restores a custom-format archive, fencing a
// TimescaleDB dump first when $4 is armed: the archive's own table of
// contents is the exact witness, and an archive pg_restore -l cannot read
// is left for the restore itself to refuse with its own words.
const archiveRestoreScript = `
[ -n "$4" ] && pg_restore -l -- "$1" 2>/dev/null | grep -qF -- "$4" && exit 93
exec pg_restore -h 127.0.0.1 -U "$2" -d "$3" --no-owner --exit-on-error "$1"
`

// dumpCompleteMarker matches the sentence a dump signs itself off with —
// pg_dump writes the database form, pg_dumpall the cluster one. Measured on
// PostgreSQL 16: every flag combination a backup job plausibly uses
// (--no-comments, --no-owner, --data-only, --schema-only, --inserts,
// --clean) still writes it.
//
// It is the only thing that can tell a complete plain-SQL dump from a
// truncated one, because psql cannot. Measured: fed a dump cut in half,
// psql restores the half, treats the stream's end as the end of the data,
// and exits 0 — a silent partial restore, which §5 forbids reporting as
// success. The failure that produces one is ordinary: a backup job running
// `pg_dump | gzip` whose pg_dump dies of a full disk still leaves a
// perfectly valid gzip file behind, and every byte in it restores.
const dumpCompleteMarker = `^-- PostgreSQL database (cluster )?dump complete$`

// markerTailBytes is how much of a dump's end is searched for that
// sentence. The trailer is the last line but one — pg_dumpall closes with a
// \unrestrict meta-command after it — and this is slack for that and for
// any tool that appends a footer of its own.
const markerTailBytes = 4096

// compressedArchiveRestoreScript restores a compressed custom-format
// archive without ever writing the archive down plain: the sandbox needs
// room for the stored artifact only, while materialising it would ask for
// the uncompressed size on top of the data directory the restore is about
// to fill.
//
// Both ends of the pipeline are judged, in this order. A pipeline's status
// is its last command's, so $? carries pg_restore's verdict and the
// decompressor's is left in a file. pg_restore is judged first
// deliberately: when an archive holds something it rejects it aborts, the
// decompressor then dies of a broken pipe, and blaming the archive's
// compression would name the wrong culprit.
//
// No completeness witness is needed here. Measured: a custom-format archive
// truncated before it was compressed makes pg_restore exit non-zero on its
// own — the format carries a table of contents, so it knows what is
// missing. A plain-SQL script knows nothing about itself, which is why the
// other two scripts have to.
const compressedArchiveRestoreScript = `
{ gzip -dc -- "$1"; echo $? > "$1.status"; } | pg_restore -h 127.0.0.1 -U "$2" -d "$3" --no-owner --exit-on-error || exit 1
[ "$(cat "$1.status")" = 0 ] || exit 90
`

// psqlReplayArgv builds the command that replays a plain-SQL member —
// either half of a pgdump_with_globals pair, or a dump on its own. What
// varies between them is the database and whether the first error ends the
// replay; the completeness rule does not, which is why one pair of scripts
// serves both.
func psqlReplayArgv(member sandboxFile, user, database, errorStop, fence string) []string {
	script := scriptReplayScript
	if member.storage.compressed {
		script = compressedScriptReplayScript
	}
	return []string{"sh", "-c", script, "sh", member.path, user, database,
		dumpCompleteMarker, strconv.Itoa(markerTailBytes), errorStop,
		fence, timescaleFenceHeadBytes}
}

// psql's own ON_ERROR_STOP values, named where they are chosen rather than
// spelled at the call sites.
const (
	errorStopOn  = "1"
	errorStopOff = "0"
)

// scriptReplayScript replays a plain-SQL dump and then proves the dump was
// whole, in that order: psql's exit code says only that nothing it executed
// failed, never that it reached the end of a complete dump.
//
// psql's verdict is taken first throughout, and passed through rather than
// normalised: it is the one that can name a real defect in the dump's
// contents, and no psql exit code collides with the codes these scripts add
// (psql uses 0 through 3).
const scriptReplayScript = `
[ -n "$7" ] && head -c "$8" -- "$1" | grep -qE -- "$7" && exit 93
psql -h 127.0.0.1 -U "$2" -d "$3" -v ON_ERROR_STOP="$6" -q -f "$1" >/dev/null
rc=$?
[ "$rc" = 0 ] || exit "$rc"
tail -c "$5" -- "$1" | grep -qE "$4" || exit 91
`

// compressedScriptRestoreScript replays a compressed plain-SQL dump and
// witnesses its end as it goes by. The dump's last bytes are only knowable
// by decompressing everything before them — a gzip member carries no index
// — so the stream is tapped while psql consumes it and its tail kept in
// bounded memory, rather than inflating the artifact a second time. Measured
// on a 218 MiB dump: the tap costs 2% of the restore, a second inflate pass
// would have cost 70%, and the drill's restore duration is an RTO figure
// somebody reads.
//
// The three verdicts are separated because they send an operator to three
// different places: psql failed (the dump's contents), the decompressor
// failed (the stored artifact), or the dump simply had no end (the backup
// job that wrote it). psql is judged first for the reason the archive
// script gives — once psql aborts, the decompressor dies of a broken pipe,
// and blaming the compression would name the wrong culprit.
const compressedScriptReplayScript = `
[ -n "$7" ] && { gzip -dc -- "$1" 2>/dev/null | head -c "$8" | grep -qE -- "$7"; } && exit 93
rm -f "$1.fifo"
mkfifo "$1.fifo" || exit 92
tail -c "$5" <"$1.fifo" >"$1.tail" &
{ gzip -dc -- "$1"; echo $? > "$1.status"; } | tee "$1.fifo" | psql -h 127.0.0.1 -U "$2" -d "$3" -v ON_ERROR_STOP="$6" -q >/dev/null
rc=$?
wait
[ "$rc" = 0 ] || exit "$rc"
[ "$(cat "$1.status")" = 0 ] || exit 90
grep -qE "$4" "$1.tail" || exit 91
`

// What the restore scripts exit with when they, and not the client, decided
// the restore failed. No psql or pg_restore produces these: their own codes
// stop at 3.
const (
	decompressFailedExit = 90
	incompleteDumpExit   = 91
	witnessSetupExit     = 92
	timescaleFencedExit  = 93
)

// mapScriptExit classifies the verdicts a replay script reaches on its own,
// as opposed to the client's; what names the member in diagnostics. It
// returns nil for every other exit code, which means the client failed and
// its own diagnostics decide.
func mapScriptExit(exitCode int, stderr []byte, what string) *protoError {
	switch exitCode {
	case decompressFailedExit:
		return mapDecompressFailure(stderr)
	case incompleteDumpExit:
		return protoErr("source_corrupt", false,
			"%s is not a complete dump: it ends without the line pg_dump writes when it finishes, "+
				"so whatever wrote it stopped early — restoring it would have proved only the "+
				"part that survived", what)
	case witnessSetupExit:
		return protoErr("restore_failed", false,
			"the sandbox image provides no mkfifo, which replaying a compressed plain-SQL dump "+
				"needs in order to prove the dump was whole: %s", firstLine(stderr))
	case timescaleFencedExit:
		return protoErr("unsupported_source", false,
			"%s creates the timescaledb extension, and restoring it without the "+
				"timescaledb_pre_restore()/timescaledb_post_restore() frame breaks hypertable state — "+
				"measured: a dump with compressed chunks restores partially ('could not find "+
				"hypertable') — use the timescaledb_dump source kind, which frames the restore", what)
	}
	return nil
}

// mapRestoreFailure classifies a failed restore into protocol error codes.
// Partial restores must never look like success (§5).
func mapRestoreFailure(exitCode int, stderr []byte, storage dumpStorage) *protoError {
	if perr := mapScriptExit(exitCode, stderr, "the backup"); perr != nil {
		return perr
	}
	if storage.plain {
		return mapScriptRestoreFailure(stderr)
	}
	line := firstLine(stderr)
	if strings.Contains(line, "not appear to be a valid archive") {
		return protoErr("source_corrupt", false, "pg_restore rejected the archive: %s", line)
	}
	return protoErr("restore_failed", false, "pg_restore failed: %s", line)
}

// mapScriptRestoreFailure classifies a psql failure. The missing-role case
// is named because it is the one a plain-SQL dump produces by construction:
// the script carries its ownership and grants inline, and unlike
// pg_restore's --no-owner there is no flag that drops them.
func mapScriptRestoreFailure(stderr []byte) *protoError {
	line := restoreDiagnostic(stderr)
	// A dump the server cannot parse is not a dump. pg_restore says so
	// about an archive in as many words ("does not appear to be a valid
	// archive"); for a script the server's own syntax error is the same
	// verdict, and it belongs to the artifact rather than to the restore.
	if strings.Contains(line, "syntax error") {
		return protoErr("source_corrupt", false, "psql rejected the dump: %s", line)
	}
	if strings.Contains(line, "role") && strings.Contains(line, "does not exist") {
		return protoErr("restore_failed", false,
			"the dump assigns ownership or grants to a role the restored cluster does not have; "+
				"a plain-SQL dump carries those inline, so the cluster globals belong in the drill "+
				"with it (source kind pgdump_with_globals): %s", line)
	}
	return protoErr("restore_failed", false, "psql failed replaying the dump: %s", line)
}

// mapDecompressFailure separates the two ways decompression fails inside a
// sandbox. A missing tool is the operator's image, not their backup, and
// calling that a corrupt source would send them to inspect a file that is
// fine.
func mapDecompressFailure(stderr []byte) *protoError {
	line := firstLine(stderr)
	if strings.Contains(line, "not found") {
		return protoErr("restore_failed", false,
			"the sandbox image provides no gzip, so a compressed dump cannot be restored in it: %s", line)
	}
	return protoErr("source_corrupt", false, "the dump could not be decompressed: %s", line)
}

// restoreDiagnostic returns psql's first failure line, falling back to the
// first line of all. A decompressor shares the pipeline's stderr and adds a
// broken-pipe note of its own once psql aborts; that note must not become
// the drill's explanation.
func restoreDiagnostic(stderr []byte) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		if _, isFailure := diagnosticMessage(line); isFailure {
			return firstLine([]byte(line))
		}
	}
	return firstLine(stderr)
}

func option(opts map[string]string, key, fallback string) string {
	if v, ok := opts[key]; ok && v != "" {
		return v
	}
	return fallback
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// The message crosses the protocol as a JSON string and lands in
	// evidence error fields: keep it single-line, quote-free, and free of
	// credentials.
	return strings.ReplaceAll(scrubSecrets(s), `"`, "'")
}

// passwordLiteral matches a SQL password literal. A cluster-globals script
// carries every role's password verifier (ALTER ROLE … PASSWORD
// 'SCRAM-SHA-256$…'), and PostgreSQL quotes the offending source text back
// in syntax errors — so engine diagnostics are a live path from a backup's
// credentials into a signed evidence record, which the schema forbids from
// carrying any (evidence schema §8).
var passwordLiteral = regexp.MustCompile(`(?i)password\s+'[^']*'`)

// scrubSecrets removes credential material from text bound for a protocol
// message.
func scrubSecrets(s string) string {
	return passwordLiteral.ReplaceAllString(s, "PASSWORD '[redacted]'")
}
