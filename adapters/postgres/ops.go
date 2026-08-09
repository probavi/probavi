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
	adapterName    = "postgres"
	adapterVersion = "0.8.0"

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
		globalsInSandbox := scratch + "/probavi-globals.sql"
		globalsTransfer, globalsRestore, perr = loadGlobals(ctx, c, user, src.globalsPath, globalsInSandbox)
		if perr != nil {
			return nil, perr
		}
		logger.Info("cluster globals loaded", "seconds", globalsRestore)
		state["globals_path"] = globalsInSandbox
	}

	dumpInSandbox := scratch + "/probavi-restore.dump"
	state["dump_path"] = dumpInSandbox
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dumpInSandbox, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}

	restore, stderr, perr := execRestore(ctx, c, user, database, dumpInSandbox)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, mapRestoreFailure(stderr)
	}
	logger.Info("restore complete", "seconds", restore.DurationSeconds)

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
			"restore_seconds":      globalsRestore + restore.DurationSeconds,
		},
		"state": state,
	}, nil
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

func execRestore(ctx context.Context, c *core, user, database, dumpPath string) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"pg_restore", "-h", "127.0.0.1", "-U", user, "-d", database,
			"--no-owner", "--exit-on-error", dumpPath},
	})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// mapRestoreFailure classifies pg_restore failures into protocol error
// codes. Partial restores must never look like success (§5).
func mapRestoreFailure(stderr []byte) *protoError {
	line := firstLine(stderr)
	if strings.Contains(line, "not appear to be a valid archive") {
		return protoErr("source_corrupt", false, "pg_restore rejected the archive: %s", line)
	}
	return protoErr("restore_failed", false, "pg_restore failed: %s", line)
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
