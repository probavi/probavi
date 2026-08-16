package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"
)

const (
	adapterName    = "sqlite"
	adapterVersion = "0.1.0"

	// workDirName is created under the provider's scratch directory. The
	// sibling adapters own root-level paths, but images that carry sqlite3
	// commonly run as an unprivileged user (measured: the community image
	// CI verifies against runs as "sqlite"), and scratch is the one
	// directory the provider guarantees writable.
	workDirName = "probavi-sqlite"
	// dbFileName is the restored database inside the work directory: the
	// file every check opens, reached through connection.database.
	dbFileName = "restored.db"
	// dumpFileName is where the dump kinds place the SQL text before
	// replaying it into a fresh database.
	dumpFileName = "dump.sql"
)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "sqlite"},
		"sources": []map[string]any{
			{"kind": "sqlite_db", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "sqlite_db_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "sqlite_dump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "sqlite_dump_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// SQLite speaks SQL, so the core's generating built-ins apply
			// unchanged. There is no server: each check is one sqlite3
			// invocation opening the restored file, whose in-sandbox path
			// {{database}} delivers — provision decides it under the
			// provider's scratch directory and returns it as
			// connection.database, which is how a path the probe cannot
			// know reaches a template declared here (§6.1). -bail makes
			// the exit code report the first SQL error, -batch -noheader
			// and the literal tab separator produce the undecorated
			// tab-separated rows the runner contract requires (measured).
			"argv": []string{"sqlite3", "-batch", "-noheader", "-bail",
				"-separator", "\t", "{{database}}", "{{sql}}"},
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

// opProvision places the artifact in the idle sandbox and proves the
// engine reads it: preflight (shell and sqlite3 present), transfer, then
// either an integrity check of the placed database or a replay of the
// dump into a fresh one. There is no server to start — SQLite is a
// library, and the engine is the sqlite3 process each later check runs —
// so engine_ready is the preflight's measured wait, and the restore this
// drill measures is the engine's first full read of the restored data.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	if perr := rejectBackupTimezone(req.Source.Params); perr != nil {
		return nil, perr
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes)

	readySeconds, perr := checkEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	if perr := prepareWorkDir(ctx, c, workDir); perr != nil {
		return nil, perr
	}

	dbPath := path.Join(workDir, dbFileName)
	restore := placeDatabase
	if src.sql {
		restore = replayDump
	}
	transferSeconds, restoreSeconds, perr := restore(ctx, c, src.path, workDir, dbPath)
	if perr != nil {
		return nil, perr
	}
	logger.Info("database restored", "seconds", restoreSeconds)

	return map[string]any{
		"connection": map[string]any{
			// There is nothing to dial: checks reach the restored data as
			// a file, through the path in database. scheme, host and port
			// are §6.2 requirements — the scheme every SQLite connection
			// string uses, and a port no client will ever open.
			"scheme": "sqlite", "host": "127.0.0.1", "port": 0,
			"database": dbPath, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// Nothing in either artifact form dates it (see source.go).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"db_path": dbPath, "work_dir": workDir},
	}, nil
}

// checkEngine verifies the two things every later step runs on: a POSIX
// shell — the integrity check and the dump replay are shell one-liners,
// because the sandbox is where sqlite3's exit-code quirks are absorbed
// (see integrityScript) — and the sqlite3 CLI itself. One probe covers
// both, and its measured duration is the engine_ready wait: an embedded
// engine is ready the moment its binary answers.
func checkEngine(ctx context.Context, c *core) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", "exec sqlite3 -version"}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("invalid_request", false,
			"the sandbox image cannot run sqlite3 through a POSIX shell, and this adapter needs "+
				"both — there is no official SQLite image, so use any image that ships them; the "+
				"adapter README names the community image CI verifies against (%s)", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// prepareWorkDir creates the directory the restored database lives in.
func prepareWorkDir(ctx context.Context, c *core, workDir string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mkdir", "-p", workDir}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare work directory: %s", firstLine(stderr))
	}
	return nil
}

// integrityScript runs PRAGMA integrity_check and converts its awkward
// contract into an exit code, inside the sandbox: the pragma prints
// problem rows to stdout while exiting 0 — only failing to open the file
// moves the exit code, and even that code differs between versions
// (measured: 1 on 3.53, 26 on 3.46). The shell folds all of it into
// "exit 0 with the single row ok, or exit 1 with the tool's own words on
// stderr", so the adapter decides on the exit code alone — the same
// declarative absorption the sql_runner template performs for dialects.
const integrityScript = `out=$(sqlite3 "$1" "PRAGMA integrity_check;" 2>&1); status=$?
if [ "$status" -eq 0 ] && [ "$out" = "ok" ]; then exit 0; fi
printf '%s\n' "$out" >&2
exit 1`

// placeDatabase transfers the artifact and has the engine read all of it.
// Placing a file is not yet a restore: the restore this adapter measures
// is sqlite3 walking every page of the placed database — PRAGMA
// integrity_check — which is also the sandbox-side verdict on the backup.
func placeDatabase(ctx context.Context, c *core, hostPath, _, dbPath string) (transferSeconds, restoreSeconds float64, perr *protoError) {
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: dbPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", integrityScript, "sh", dbPath}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, 0, protoErr("source_corrupt", false,
			"sqlite3 rejected the restored database: %s", firstLine(stderr))
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// replayDump transfers the SQL text and builds a fresh database from it.
// -bail stops at the first error with a non-zero exit — without it
// sqlite3 replays straight past errors (measured) — and a parse failure
// is the backup's fault: SQL that stops mid-token is a truncated
// artifact. The truncation sqlite3 cannot see, a tail lost cleanly
// between statements, is refused host-side before the transfer
// (see refuseTruncatedDump).
func replayDump(ctx context.Context, c *core, hostPath, workDir, dbPath string) (transferSeconds, restoreSeconds float64, perr *protoError) {
	dumpPath := path.Join(workDir, dumpFileName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: dumpPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", `exec sqlite3 -bail "$1" < "$2"`, "sh", dbPath, dumpPath}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		line := firstLine(stderr)
		if strings.Contains(line, "Parse error") {
			return 0, 0, protoErr("source_corrupt", false,
				"the dump stopped parsing mid-statement — a truncated backup: %s", line)
		}
		return 0, 0, protoErr("restore_failed", false, "replaying the dump failed: %s", line)
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// healthcheckRequest is the §6.3 request payload. The restored database
// is reached through the connection provision returned; there is no
// server-side state to consult.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored database still serves queries
// (§6.3). The query counts sqlite_schema rather than SELECT 1: a bare
// constant SELECT reads no page, and older CLIs open lazily enough to
// answer it against a file that is not a database at all (measured on
// 3.46) — counting the schema forces the read that makes the verdict mean
// something. An unhealthy database is a valid result, not an operation
// error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no restored database")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"sqlite3", "-batch", "-noheader", req.Connection.Database,
		"SELECT count(*) FROM sqlite_schema;"}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("database file serves queries; %s schema objects", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("sqlite3 exited %d: %s", val.ExitCode, firstLine(stderr))
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// The message crosses the protocol as a JSON string and lands in
	// evidence error fields: keep it single-line and quote-free.
	return strings.ReplaceAll(s, `"`, "'")
}
