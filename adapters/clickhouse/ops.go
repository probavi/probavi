package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	adapterName    = "clickhouse"
	adapterVersion = "0.3.0"

	// defaultDatabase is ClickHouse's own default: it always exists, so the
	// healthcheck and the sql_runner have a valid target before anything is
	// restored. Checks against restored data set options.database.
	defaultDatabase = "default"
	// defaultUser is the account the official image serves without a
	// password. Probavi sandboxes are zero-ingress (--network none, no
	// published ports), which is the only reason an unauthenticated engine
	// holding production data is acceptable — see the adapter README.
	defaultUser = "default"
	// defaultPort is the native protocol port the client uses.
	defaultPort = 9000

	readinessBudget = 2 * time.Minute
	readinessPoll   = 500 * time.Millisecond

	// restoreArchiveName is what the transferred archive is called inside
	// the sandbox. The adapter composes it, so the RESTORE statement never
	// interpolates operator input.
	restoreArchiveName = "probavi-restore.zip"

	// defaultBackupDir is where the official image keeps backups: the data
	// path the server reports, plus the default backups.allowed_path. It is
	// a fallback for a server that will not say where its data lives, never
	// the first answer — an image is free to move its data path, and the
	// postgres image did exactly that in 18.
	defaultBackupDir = "/var/lib/clickhouse/backups"
)

// databasePattern is deliberately strict: the name reaches the engine as a
// distinct argv element, so injection is impossible by construction, but a
// permissive name is still worth refusing early with a clear message.
var databasePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "clickhouse"},
		"sources": []map[string]any{
			{"kind": "clickhouse_backup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "clickhouse_backup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// ClickHouse speaks SQL, so the core's built-in checks reach it
			// unchanged — measured: the generated `SELECT count(*) FROM
			// "db"."table"` and `SELECT max("col") FROM …` both run, rows
			// print tab-separated with no decoration, and an unknown table
			// exits non-zero, which is exactly the §6.1 contract.
			//
			// --host 127.0.0.1 is not decoration: a zero-ingress sandbox has
			// no DNS, and the client resolves its own hostname on startup.
			// Without the flag it has nowhere to connect; with it, the
			// failed self-lookup is a warning on stderr and the query runs.
			"argv": []string{"clickhouse-client", "--host", "127.0.0.1",
				"--user", "{{user}}", "--database", "{{database}}", "--query", "{{sql}}"},
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

// opProvision restores the backup into the already-running sandbox: wait
// for engine readiness, find where the server accepts backup files,
// transfer the archive there, and replay it.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	database, loc, perr := parseProvisionTarget(req)
	if perr != nil {
		return nil, perr
	}
	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"backup_wall_clock", src.wallClock)

	readySeconds, perr := awaitEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine ready", "seconds", readySeconds)

	backupsDir, perr := backupDir(ctx, c)
	if perr != nil {
		return nil, perr
	}
	if perr := prepareBackupDir(ctx, c, backupsDir); perr != nil {
		return nil, perr
	}

	// 0644, not the protocol's 0600 default: the server runs as its own
	// account (measured: `clickhouse`) while sandbox commands run as root,
	// and a 0600 artifact it cannot open fails the restore with errno 13
	// while blaming the backup.
	put, perr := c.putFile(ctx, putFileArgs{
		SourcePath: src.path,
		DestPath:   path.Join(backupsDir, restoreArchiveName),
		Mode:       "0644",
	})
	if perr != nil {
		return nil, perr
	}

	restore, stderr, perr := execRestore(ctx, c)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, mapRestoreFailure(restore.ExitCode, stderr)
	}
	logger.Info("restore complete", "seconds", restore.DurationSeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "clickhouse", "host": "127.0.0.1", "port": defaultPort,
			"database": database, "user": defaultUser,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			"created_at": createdAt(src.wallClock, loc),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			"restore_seconds":      restore.DurationSeconds,
		},
		"state": map[string]any{"database": database, "backups_dir": backupsDir},
	}, nil
}

// parseProvisionTarget validates everything the request supplies before any
// sandbox call.
func parseProvisionTarget(req *provisionRequest) (database string, loc *time.Location, perr *protoError) {
	if req.PITR != nil {
		return "", nil, protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	database = option(req.Options, "database", defaultDatabase)
	if !databasePattern.MatchString(database) {
		return "", nil, protoErr("invalid_request", false,
			"database name %s must contain only letters, digits, underscores, and hyphens", database)
	}
	loc, perr = backupLocation(req.Source.Params)
	if perr != nil {
		return "", nil, perr
	}
	return database, loc, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the provisioned instance answers queries (§6.3).
// An unhealthy engine is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	database := req.Connection.Database
	if database == "" || !databasePattern.MatchString(database) {
		database = defaultDatabase
	}
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: clientArgv(database, "SELECT 1")})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1"
	detail := "accepting queries"
	if !healthy {
		detail = fmt.Sprintf("clickhouse-client exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// awaitEngine polls until the server answers a query. The sandbox runtime
// being up says nothing about the engine: the official image starts the
// server through an entrypoint that first fixes ownership and applies
// configuration, and the client refuses connections for a second or two
// after the container is running.
func awaitEngine(ctx context.Context, c *core) (float64, *protoError) {
	start := time.Now()
	for {
		val, stdout, _, perr := c.exec(ctx, execArgs{
			Argv:           clientArgv(defaultDatabase, "SELECT 1"),
			TimeoutSeconds: 5,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1" {
			return time.Since(start).Seconds(), nil
		}
		if time.Since(start) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"engine did not answer a query within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// backupDir asks the server where its data lives and composes the backup
// directory from the answer.
//
// The path is deliberately not a constant. The official image serves
// /var/lib/clickhouse today, and an adapter that assumed as much would
// break the day an image moves it — which is not hypothetical: the
// postgres image moved PGDATA in 18 and broke exactly that assumption in
// the sibling adapter. `backups` is the default `backups.allowed_path`,
// which the server does not expose; an image that overrides it fails the
// restore with the engine's own message, mapped below.
func backupDir(ctx context.Context, c *core) (string, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv:           clientArgv(defaultDatabase, "SELECT value FROM system.server_settings WHERE name = 'path'"),
		TimeoutSeconds: 10,
	})
	if perr != nil {
		return "", perr
	}
	dataPath := strings.TrimSpace(string(stdout))
	if val.ExitCode != 0 || !strings.HasPrefix(dataPath, "/") {
		// Asking is the point; not getting an answer is not fatal. The
		// default is what the official image serves, and if this server
		// disagrees the RESTORE fails with the engine's own message about
		// backups.allowed_path rather than with a guess dressed up as a
		// diagnosis (mapped below).
		return defaultBackupDir, nil
	}
	return path.Join(dataPath, "backups"), nil
}

// prepareBackupDir makes sure the directory exists and the server can read
// what lands in it. A server that has never run a BACKUP has no such
// directory: it is created on first use (measured).
func prepareBackupDir(ctx context.Context, c *core, dir string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv:           []string{"sh", "-c", `mkdir -p "$1" && chmod 0755 "$1"`, "sh", dir},
		TimeoutSeconds: 30,
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("restore_failed", false,
			"could not prepare the sandbox backup directory %s: %s", dir, firstLine(stderr))
	}
	return nil
}

// notRestoredExit is what the restore script exits with when the client
// succeeded but the engine never said it restored anything. No
// clickhouse-client produces it: the client's own codes are engine error
// codes, and none of them is 91.
const notRestoredExit = 91

// execRestore replays the archive in the two passes retention.go
// explains, with the engine's own retention policy pinned off between
// them.
func execRestore(ctx context.Context, c *core) (*execValue, []byte, *protoError) {
	structure, pin, data, census := restoreStatements()
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", restoreScript, "sh",
			defaultUser, defaultDatabase, structure, pin, data, census},
	})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// mapRestoreFailure classifies a failed RESTORE into a protocol error code.
// The markers are the engine's own error names, measured against
// ClickHouse 26.3 — an archive it cannot unpack is a statement about the
// backup, everything else is a statement about the restore.
func mapRestoreFailure(exitCode int, stderr []byte) *protoError {
	if exitCode == notRestoredExit {
		return protoErr("restore_failed", false,
			"clickhouse accepted the RESTORE statement but reported no RESTORED status for the archive")
	}
	if exitCode == pinRefusedExit {
		return refusedPin(stderr)
	}
	if exitCode == emptyRestoreExit {
		return protoErr("restore_failed", false,
			"the restore reported RESTORED and produced no table — the archive holds nothing to "+
				"drill, so a check would run against an empty server and prove nothing (%s)",
			firstLine(stderr))
	}
	line := verdictLine(stderr)
	switch {
	case strings.Contains(line, "CANNOT_UNPACK_ARCHIVE"), strings.Contains(line, "Couldn't open zip archive"):
		return protoErr("source_corrupt", false, "clickhouse could not unpack the archive: %s", line)
	case strings.Contains(line, "backups.allowed_path"):
		return protoErr("restore_failed", false,
			"the sandbox server does not accept backups from its own data path — this image overrides "+
				"backups.allowed_path: %s", line)
	case strings.Contains(line, "CANNOT_OPEN_FILE"):
		return protoErr("restore_failed", false,
			"the sandbox server could not open the transferred archive: %s", line)
	}
	return protoErr("restore_failed", false, "clickhouse restore failed: %s", line)
}

// clientArgv builds a client invocation. --host 127.0.0.1 is required in a
// zero-ingress sandbox: see the note in probePayload.
func clientArgv(database, query string) []string {
	return []string{"clickhouse-client", "--host", "127.0.0.1",
		"--user", defaultUser, "--database", database, "--query", query}
}

func option(opts map[string]string, key, fallback string) string {
	if v, ok := opts[key]; ok && v != "" {
		return v
	}
	return fallback
}

// verdictLine extracts the failure verdict from the client's stderr.
//
// A zero-ingress sandbox has no DNS, so every invocation opens with a
// multi-line "Cannot resolve host" warning and a stack trace (measured,
// harmless — the query still runs). The real diagnostic is the line
// carrying the engine's error code, so that is what is looked for; the
// first line is the fallback only when nothing matches. The result crosses
// the protocol as a JSON string and lands in evidence error fields: keep
// it single-line and quote-free.
func verdictLine(b []byte) string {
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		// The engine's own diagnostic opens the line with its error code.
		// The DNS warning carries one too, but buried mid-line behind
		// "DB::DNSResolver::Impl::Impl():", and stack frames open with a
		// frame number — so the prefix is what separates them.
		if s := strings.TrimSpace(l); strings.HasPrefix(s, "Code:") {
			return clean(s)
		}
	}
	return firstLine(b)
}

// firstLine reduces captured output to its first line, quote-free.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clean(s)
}

func clean(s string) string {
	return strings.ReplaceAll(s, `"`, "'")
}
