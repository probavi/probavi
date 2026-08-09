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
	adapterName    = "mssql"
	adapterVersion = "0.7.0"

	defaultUser     = "sa"
	defaultDatabase = "probavi"
	defaultPort     = 1433

	sqlcmdPath = "/opt/mssql-tools18/bin/sqlcmd"

	// sandboxPassword is the sa password of the drill engine. It is a
	// documented CONSTANT, not a secret: SQL Server refuses to run without
	// an authenticated superuser, and the ephemeral core-generated secret
	// cannot be used — its value must never cross the protocol (§2.5), yet
	// setting an engine password requires exactly that. So the adapter
	// starts the engine itself with this fixed value, the SQL Server
	// equivalent of the postgres adapter's pg_hba trust overwrite and the
	// mysql adapter's empty root password: publicly known access, confined
	// to a sandbox with zero ingress (--network none, no ports
	// expressible). The credential never protects anything reachable.
	sandboxPassword = "Probavi!DrillSandbox0" //nolint:gosec // deliberately public constant, not a credential — see above

	// initFilePath is written during provision and referenced by the
	// declared sql_runner's SQLCMDINI: the startup script sets NOCOUNT so
	// check output stays undecorated rows (§6.1) — sqlcmd's equivalent of
	// the mysql adapter's --init-command bridge. The path is static
	// because the probe template is static; /tmp exists in every image.
	initFilePath = "/tmp/probavi-sqlcmd-init.sql"

	readinessBudget = 3 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// Target-side shell fragments (arguments travel as positional parameters,
// never interpolated into the scripts).
const (
	// startScript launches sqlservr in the background; the exec returns
	// once the shell exits, the engine keeps running inside the sandbox.
	// ACCEPT_EULA and MSSQL_SA_PASSWORD arrive via the exec env.
	startScript = `nohup /opt/mssql/bin/sqlservr >/tmp/probavi-sqlservr.log 2>&1 &`

	// initFileScript writes the sql_runner startup script (see
	// initFilePath).
	initFileScript = `printf 'SET NOCOUNT ON;\n' > /tmp/probavi-sqlcmd-init.sql`

	// restoreScript restores backup set $3 of $1 (backup file) as database
	// $2: the file list is read from that same set and every logical file
	// is MOVEd to a fresh path — the original paths inside the .bak belong
	// to the production server, not this sandbox. Logical names come from
	// the operator's artifact, so embedded single quotes are doubled
	// (\047) before they enter the T-SQL string; the set number is an
	// integer the adapter parsed from the backup header. All parsing
	// happens inside the sandbox; the adapter only sees the exit code.
	// chainRestoreScript restores one member of a backup chain: $1 file,
	// $2 database, $3 backup set, $4 DATABASE or LOG, $5 NORECOVERY or
	// RECOVERY, $6 whether to relocate the data files. Only the full
	// backup that opens a chain creates files and needs the MOVEs; every
	// later member lands on the ones it made. Arguments travel as
	// positional parameters, and each was validated before it could reach
	// T-SQL text.
	chainRestoreScript = `set -e
sqlcmd=/opt/mssql-tools18/bin/sqlcmd
moves=""
if [ "$6" = "1" ]; then
  moves=$("$sqlcmd" -S 127.0.0.1,1433 -U sa -C -b -l 5 -h -1 -W -s "|" -r 1 -Q "SET NOCOUNT ON; RESTORE FILELISTONLY FROM DISK = N'$1' WITH FILE = $3" | awk -F"|" 'NF >= 3 { name = $1; gsub("\047", "\047\047", name); printf ", MOVE N\047%s\047 TO N\047/var/opt/mssql/data/probavi_restore_%d.dat\047", name, NR }')
fi
"$sqlcmd" -S 127.0.0.1,1433 -U sa -C -b -l 5 -r 1 -Q "RESTORE $4 [$2] FROM DISK = N'$1' WITH FILE = $3, $5$moves"`

	restoreScript = `set -e
sqlcmd=/opt/mssql-tools18/bin/sqlcmd
moves=$("$sqlcmd" -S 127.0.0.1,1433 -U sa -C -b -l 5 -h -1 -W -s "|" -r 1 -Q "SET NOCOUNT ON; RESTORE FILELISTONLY FROM DISK = N'$1' WITH FILE = $3" | awk -F"|" 'NF >= 3 { name = $1; gsub("\047", "\047\047", name); printf ", MOVE N\047%s\047 TO N\047/var/opt/mssql/data/probavi_restore_%d.dat\047", name, NR }')
"$sqlcmd" -S 127.0.0.1,1433 -U sa -C -b -l 5 -r 1 -Q "RESTORE DATABASE [$2] FROM DISK = N'$1' WITH FILE = $3, RECOVERY$moves"`
)

// databasePattern is deliberately strict: the restore target name is the
// only operator-supplied string that reaches T-SQL text (bracketed), so
// nothing that could close a bracket or quote gets past validation.
var databasePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "sqlserver"},
		"sources": []map[string]any{
			{"kind": "bak", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "bak_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "bak_with_logins", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "bak_chain", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// -I turns on QUOTED_IDENTIFIER, so the SQL-standard
			// double-quoted identifiers the core emits are accepted; the
			// SQLCMDINI startup script sets NOCOUNT, so stdout carries
			// undecorated rows only. The engine dialect is absorbed here,
			// declaratively (§6.1). SQLCMDPASSWORD carries the documented
			// sandbox constant — see the sandboxPassword comment.
			"argv": []string{sqlcmdPath, "-S", "127.0.0.1,1433", "-U", "{{user}}", "-d", "{{database}}",
				"-C", "-b", "-I", "-h", "-1", "-W", "-s", "\t", "-Q", "{{sql}}"},
			"env": map[string]string{
				"SQLCMDPASSWORD": sandboxPassword,
				"SQLCMDINI":      initFilePath,
			},
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

// opProvision restores the backup into the sandbox. The sandbox must start
// idle (docker: command: sleep infinity): SQL Server cannot run without a
// superuser password, and a password in sandbox params would enter the
// signed evidence record verbatim — so the adapter starts the engine
// itself with the documented sandbox constant, then restores the .bak
// under the target name with server-side MOVEs.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	database, scratch, perr := parseProvisionTarget(req)
	if perr != nil {
		return nil, perr
	}

	plan, perr := resolveSource(req.Source.Kind, req.Source.Path, req.Source.Params)
	if perr != nil {
		return nil, perr
	}

	if perr := writeInitFile(ctx, c); perr != nil {
		return nil, perr
	}
	readySeconds, perr := ensureEngine(ctx, c, logger)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine ready", "seconds", readySeconds)

	if req.Source.Kind == "bak_chain" {
		return provisionChain(ctx, c, plan, database, scratch, readySeconds, logger)
	}

	// Which file is a full backup — and which set inside it — is a question
	// only the engine can answer, so the choice happens here rather than on
	// the host (see backupset.go).
	bakInSandbox := scratch + "/probavi-restore.bak"
	chosen, perr := selectBackup(ctx, c, plan, bakInSandbox)
	if perr != nil {
		return nil, perr
	}
	src, perr := plan.identity(chosen.hostPath, chosen.createdAt)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"backup_set", chosen.position)

	state := map[string]any{"database": database}
	// Server logins go in before the restore: that is the order of the
	// recovery run-book this drill stands for, and their load is part of
	// the restore, not a phase of its own — the measured restore duration
	// is the drill's RTO figure.
	var loginsTransfer, loginsLoad float64
	if src.loginsPath != "" {
		loginsInSandbox := scratch + "/probavi-logins.sql"
		loginsTransfer, loginsLoad, perr = loadLogins(ctx, c, src.loginsPath, loginsInSandbox)
		if perr != nil {
			return nil, perr
		}
		logger.Info("server logins loaded", "seconds", loginsLoad)
		state["logins_path"] = loginsInSandbox
	}

	// The chosen artifact is already in the sandbox: selection transferred
	// it there to ask the engine what it was.
	state["bak_path"] = bakInSandbox
	state["backup_set"] = strconv.Itoa(chosen.position)

	restore, stderr, perr := execRestore(ctx, c, bakInSandbox, database, chosen.position)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, mapRestoreFailure(stderr)
	}
	logger.Info("restore complete", "seconds", restore.DurationSeconds)

	if src.loginsPath != "" {
		if perr := verifyLoginsCoverRestoredUsers(ctx, c, database); perr != nil {
			return nil, perr
		}
	}

	return provisionResult(database, src, state, timings{
		engineReady: readySeconds,
		transfer:    loginsTransfer + chosen.transfer,
		restore:     loginsLoad + restore.DurationSeconds,
	}), nil
}

// parseProvisionTarget validates the request's operator-supplied values
// before anything is transferred: the restore target name is the only one
// that reaches T-SQL text, and PITR is not a capability this adapter
// declares.
func parseProvisionTarget(req *provisionRequest) (database, scratch string, perr *protoError) {
	if req.PITR != nil {
		return "", "", protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	database = option(req.Options, "database", defaultDatabase)
	if !databasePattern.MatchString(database) {
		return "", "", protoErr("invalid_request", false,
			"database name %s must contain only letters, digits, underscores, and hyphens", database)
	}
	scratch = req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	return database, scratch, nil
}

// timings are the measured phases of one provision. Only the chosen
// artifact's transfer is counted: probing rejected candidates is how the
// drill finds the backup, not part of the recovery it measures.
type timings struct {
	engineReady, transfer, restore float64
}

func provisionResult(database string, src *resolvedSource, state map[string]any, t timings) any {
	return map[string]any{
		"connection": map[string]any{
			"scheme": "mssql", "host": "127.0.0.1", "port": defaultPort,
			"database": database, "user": defaultUser,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": t.engineReady,
			"transfer_seconds":     t.transfer,
			"restore_seconds":      t.restore,
		},
		"state": state,
	}
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
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
	database := req.Connection.Database
	if database == "" || !databasePattern.MatchString(database) {
		database = defaultDatabase
	}
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{sqlcmdPath, "-S", "127.0.0.1,1433", "-U", defaultUser, "-d", database,
			"-C", "-b", "-l", "2", "-h", "-1", "-W", "-Q", "SELECT 1"},
		Env: sqlcmdEnv(),
	})
	if perr != nil {
		return nil, perr
	}
	// Without the NOCOUNT startup script a "(1 rows affected)" trailer
	// follows the row; only the first line is the answer.
	healthy := val.ExitCode == 0 && firstLine(stdout) == "1"
	detail := "accepting queries"
	if !healthy {
		detail = fmt.Sprintf("sqlcmd exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// ensureEngine makes the sandbox serve queries and returns the measured
// wait: if an engine already answers with the sandbox credentials it is
// adopted; otherwise sqlservr is started (idle-sandbox pattern) and polled
// until ready.
func ensureEngine(ctx context.Context, c *core, logger *slog.Logger) (float64, *protoError) {
	start := time.Now()
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: probeArgv(), Env: sqlcmdEnv(), TimeoutSeconds: 10})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode == 0 {
		return time.Since(start).Seconds(), nil
	}
	// An engine that answers but refuses the sandbox credentials was
	// started by the image with its own password — which would have had to
	// come from sandbox params, where it would enter the evidence record.
	if strings.Contains(string(stderr), "Login failed") {
		return 0, protoErr("invalid_request", false,
			"the sandbox engine runs with its own credentials; start the image idle (docker: command: sleep infinity) and let the adapter own it")
	}
	res, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", startScript},
		Env: map[string]string{
			// ACCEPT_EULA carries the operator's acceptance of the
			// Microsoft EULA for the image they configured.
			"ACCEPT_EULA":       "Y",
			"MSSQL_SA_PASSWORD": sandboxPassword,
		},
	})
	if perr != nil {
		return 0, perr
	}
	if res.ExitCode != 0 {
		return 0, protoErr("invalid_request", false,
			"cannot start sqlservr (%s): use a Microsoft SQL Server image, started idle", firstLine(stderr))
	}
	logger.Info("sqlservr started, waiting for readiness")
	return awaitEngine(ctx, c, start)
}

// awaitEngine polls SELECT 1 until the server authenticates and serves.
func awaitEngine(ctx context.Context, c *core, start time.Time) (float64, *protoError) {
	for {
		val, _, _, perr := c.exec(ctx, execArgs{Argv: probeArgv(), Env: sqlcmdEnv(), TimeoutSeconds: 10})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(start).Seconds(), nil
		}
		if time.Since(start) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"engine did not accept connections within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

func probeArgv() []string {
	return []string{sqlcmdPath, "-S", "127.0.0.1,1433", "-U", defaultUser,
		"-C", "-b", "-l", "2", "-h", "-1", "-Q", "SELECT 1"}
}

func sqlcmdEnv() map[string]string {
	return map[string]string{"SQLCMDPASSWORD": sandboxPassword}
}

// writeInitFile creates the sql_runner startup script (NOCOUNT) inside the
// sandbox; the probe's static template references it via SQLCMDINI.
func writeInitFile(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", initFileScript}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "write sql_runner init file: %s", firstLine(stderr))
	}
	return nil
}

// execRestore runs the in-sandbox restore script; the .bak path is
// adapter-composed, the database name is validated before it can reach
// T-SQL text, and the backup set number came from the header the adapter
// parsed.
func execRestore(ctx context.Context, c *core, bakPath, database string, position int) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", restoreScript, "sh", bakPath, database, strconv.Itoa(position)},
		Env:  sqlcmdEnv(),
	})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// mapRestoreFailure classifies restore failures into protocol error codes.
// The markers are the engine's own verdicts for unusable backup media
// (Msg 3241 "incorrectly formed", Msg 3254 "is empty", Msg 3242 "not a
// valid ... backup set"), captured from a real server.
func mapRestoreFailure(stderr []byte) *protoError {
	line := verdictLine(stderr)
	for _, marker := range []string{"incorrectly formed", "is empty", "not a valid"} {
		if strings.Contains(line, marker) {
			return protoErr("source_corrupt", false, "sql server rejected the backup: %s", line)
		}
	}
	return protoErr("restore_failed", false, "restore failed: %s", line)
}

func option(opts map[string]string, key, fallback string) string {
	if v, ok := opts[key]; ok && v != "" {
		return v
	}
	return fallback
}

// verdictLine extracts the primary engine message from sqlcmd stderr:
// every server error is a "Msg N, Level …" header line followed by the
// message text — the first text line is the root cause ("RESTORE … is
// terminating abnormally" trails it). The result crosses the protocol as
// a JSON string and lands in evidence error fields: keep it single-line,
// quote-free, and free of credentials.
func verdictLine(b []byte) string {
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "Msg ") {
			continue
		}
		return strings.ReplaceAll(scrubSecrets(s), `"`, "'")
	}
	return ""
}

// firstLine reduces captured output to its first line, quote-free and free
// of credentials.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(scrubSecrets(s), `"`, "'")
}

// A logins script carries every login's password verifier (CREATE LOGIN …
// WITH PASSWORD = 0x0200… HASHED) and possibly plaintext password
// literals, and the engine quotes the offending source token back in
// syntax errors — measured: a malformed statement produced "Incorrect
// syntax near '0x0200feedface…'", the full hash, lowercased. Engine
// diagnostics are therefore a live path from a backup's credentials into
// a signed evidence record, which the schema forbids from carrying any
// (evidence schema §8). Long hex literals also cover SIDs; redacting
// those costs nothing.
var (
	passwordLiteral = regexp.MustCompile(`(?i)password\s*=\s*N?'[^']*'`)
	hexLiteral      = regexp.MustCompile(`(?i)0x[0-9a-f]{12,}`)
)

// scrubSecrets removes credential material from text bound for a protocol
// message.
func scrubSecrets(s string) string {
	s = passwordLiteral.ReplaceAllString(s, "PASSWORD = '[redacted]'")
	return hexLiteral.ReplaceAllString(s, "0x[redacted]")
}
