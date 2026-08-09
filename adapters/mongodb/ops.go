package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

const (
	adapterName    = "mongodb"
	adapterVersion = "0.3.0"

	// defaultDatabase is the connection database when the drill config
	// does not name one: admin always exists, so healthchecks and the
	// sql_runner have a valid target even before any data is restored.
	// Checks against restored data should set options.database.
	defaultDatabase = "admin"
	defaultPort     = 27017

	// connectionUser is empty on purpose: Probavi sandboxes run mongod
	// without access control (see README — zero-ingress rationale), so no
	// user identity exists to report. The declared sql_runner references
	// no {{user}} either.
	connectionUser = ""

	readinessBudget = 2 * time.Minute
	readinessPoll   = 500 * time.Millisecond

	// pingURI bounds server selection so an unready engine answers the
	// readiness poll quickly instead of riding mongosh's default 30 s
	// selection timeout into the per-command limit.
	pingURI  = "mongodb://127.0.0.1:27017/admin?serverSelectionTimeoutMS=2000&connectTimeoutMS=2000"
	pingEval = "db.runCommand({ping:1}).ok"
)

// databasePattern is deliberately strict. The database name only ever
// crosses process boundaries as a distinct argv element (injection-proof
// by construction), but a permissive name could still be mistaken by
// mongosh for a connection string — refuse anything but the boring shape.
var databasePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "mongodb"},
		"sources": []map[string]any{
			{"kind": "mongodump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "mongodump_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "mongodump_with_users", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "mongodump_with_oplog", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// MongoDB has no SQL: the check text the core passes through
			// {{sql}} is a mongosh --eval expression (documented in the
			// adapter README). The engine dialect is absorbed here,
			// declaratively — the core never learns it (§6.1).
			"argv": []string{"mongosh", "--quiet", "--norc",
				"--host", "127.0.0.1", "--port", "27017", "{{database}}", "--eval", "{{sql}}"},
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
// for engine readiness, transfer the archive, replay it with mongorestore.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	database := option(req.Options, "database", defaultDatabase)
	if !databasePattern.MatchString(database) {
		return nil, protoErr("invalid_request", false,
			"database name %s must contain only letters, digits, underscores, and hyphens", database)
	}
	plan, perr := planRestore(req.Source.Kind, database, req.Options["database"] != "")
	if perr != nil {
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
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes, "gzip", src.gzip)

	readySeconds, perr := awaitEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine ready", "seconds", readySeconds)

	archiveInSandbox := scratch + "/probavi-restore.archive"
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: archiveInSandbox, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}

	restore, stderr, perr := execRestore(ctx, c, archiveInSandbox, src.gzip, plan)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, mapRestoreFailure(stderr)
	}
	// The gates run on a restore the tool called successful: what they
	// check is whether it proved what the kind claims.
	if plan.replayOplog {
		if perr := verifyOplogReplayed(stderr); perr != nil {
			return nil, perr
		}
	}
	if plan.restoreAccounts {
		if perr := verifyAccountLayer(ctx, c, database); perr != nil {
			return nil, perr
		}
	}
	logger.Info("restore complete", "seconds", restore.DurationSeconds,
		"accounts", plan.restoreAccounts, "oplog", plan.replayOplog)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "mongodb", "host": "127.0.0.1", "port": defaultPort,
			"database": database, "user": connectionUser,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			"restore_seconds":      restore.DurationSeconds,
		},
		"state": map[string]any{"database": database, "archive_path": archiveInSandbox},
	}, nil
}

// restorePlan says how the archive must be replayed, derived from the
// source kind alone — the kind is the claim, so it is what decides.
type restorePlan struct {
	restoreAccounts bool
	replayOplog     bool
	database        string
}

// planRestore turns a source kind into a restore plan, refusing the
// combinations the engine cannot honour before anything is transferred.
func planRestore(kind, database string, databaseNamed bool) (*restorePlan, *protoError) {
	plan := &restorePlan{database: database}
	switch kind {
	case "mongodump_with_users":
		// mongorestore restores an archive's account layer only for a named
		// database ("cannot use --restoreDbUsersAndRoles without a specified
		// database"), and the accounts belong to the database the archive was
		// dumped from. Defaulting here would restore them into admin and
		// prove the wrong thing, so the drill has to say it.
		if !databaseNamed {
			return nil, protoErr("invalid_request", false,
				"the mongodump_with_users kind requires target.options.database: "+
					"the name of the database the archive's accounts belong to")
		}
		plan.restoreAccounts = true
	case "mongodump_with_oplog":
		plan.replayOplog = true
	}
	return plan, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the provisioned instance answers commands (§6.3).
// An unhealthy engine is a valid result, not an operation error. The ping
// runs against the connection database: MongoDB creates databases lazily,
// so any name is a valid ping target.
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
		Argv: []string{"mongosh", "--quiet", "--norc",
			"--host", "127.0.0.1", "--port", "27017", database, "--eval", pingEval},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1"
	detail := "accepting commands"
	if !healthy {
		detail = fmt.Sprintf("mongosh exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// awaitEngine polls a ping until mongod answers commands. The official
// image runs a temporary localhost-only server during first-boot
// initialization when init scripts or root-user variables are present —
// the same trap as PostgreSQL's initdb-phase server. Probavi sandboxes are
// documented to run the image bare (no MONGO_INITDB_*), which skips that
// phase entirely; the poll still tolerates the engine restarting once by
// requiring nothing beyond an eventually stable answer within the budget.
func awaitEngine(ctx context.Context, c *core) (float64, *protoError) {
	start := time.Now()
	for {
		val, stdout, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"mongosh", "--quiet", "--norc", pingURI, "--eval", pingEval},
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
				"engine did not answer ping within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// execRestore replays the archive with mongorestore. --stopOnError makes
// partial restores fail loudly (§5: never report success past ignored
// errors); the archive path is adapter-composed, never operator input, and
// the plan's flags come from the source kind, never from the request.
func execRestore(ctx context.Context, c *core, archivePath string, gzip bool, plan *restorePlan) (*execValue, []byte, *protoError) {
	argv := []string{"mongorestore", "--host", "127.0.0.1", "--port", "27017",
		"--stopOnError", "--archive=" + archivePath}
	if gzip {
		argv = append(argv, "--gzip")
	}
	if plan.restoreAccounts {
		argv = append(argv, usersRestoreFlags(plan.database)...)
	}
	if plan.replayOplog {
		argv = append(argv, oplogRestoreFlags()...)
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// mapRestoreFailure classifies mongorestore failures into protocol error
// codes. Input the tool rejects as an archive ("does not appear to be a
// mongodump archive", a wrong magic number, a lying gzip header) means
// "this is not a usable dump archive"; everything else ran and failed for
// engine reasons.
func mapRestoreFailure(stderr []byte) *protoError {
	line := verdictLine(stderr)
	for _, marker := range []string{
		"does not appear to be a mongodump archive", "magic number", "gzip: invalid header",
	} {
		if strings.Contains(line, marker) {
			return protoErr("source_corrupt", false, "mongorestore rejected the archive: %s", line)
		}
	}
	return protoErr("restore_failed", false, "mongorestore failed: %s", line)
}

func option(opts map[string]string, key, fallback string) string {
	if v, ok := opts[key]; ok && v != "" {
		return v
	}
	return fallback
}

// verdictLine extracts the failure verdict from mongorestore's stderr: the
// fatal error is the last line containing "Failed:" — a restored/failed
// summary line follows it, so the last line alone is not the verdict. Each
// line starts with a timestamp separated by a tab; the prefix is stripped.
// The result crosses the protocol as a JSON string and lands in evidence
// error fields: keep it single-line and quote-free.
func verdictLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	fallback := ""
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" {
			continue
		}
		if _, rest, found := strings.Cut(s, "\t"); found {
			s = rest
		}
		if fallback == "" {
			fallback = s
		}
		if strings.Contains(s, "Failed:") {
			return strings.ReplaceAll(scrubSecrets(s), `"`, "'")
		}
	}
	return strings.ReplaceAll(scrubSecrets(fallback), `"`, "'")
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

// credentialField matches the SCRAM material an account layer carries.
// admin.system.users documents embed each account's salt and derived keys
// (measured: credentials.SCRAM-SHA-1.{salt,storedKey,serverKey}), and a
// failed write echoes document content back in its error — so engine
// diagnostics are a live path from a backup's credentials into a signed
// evidence record, which the schema forbids from carrying any (evidence
// schema §8).
var credentialField = regexp.MustCompile(`(?i)(storedKey|serverKey|salt)("?\s*:\s*)"[^"]*"`)

// scrubSecrets removes credential material from text bound for a protocol
// message.
func scrubSecrets(s string) string {
	return credentialField.ReplaceAllString(s, `${1}${2}"[redacted]"`)
}
