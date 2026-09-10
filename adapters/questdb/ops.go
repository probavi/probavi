package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "questdb"
	adapterVersion = "0.1.0"
	// defaultPort is where QuestDB serves HTTP inside the sandbox. Nothing
	// is published: checks run in-sandbox through the runner below.
	defaultPort = 9000
	// defaultDatabase is the database name QuestDB's PostgreSQL wire
	// protocol answers to. The HTTP endpoint the checks use has no
	// database selector at all, so this names the engine's own default
	// rather than anything the drill chose.
	defaultDatabase = "qdb"
	// readinessBudget bounds the wait for an engine that never serves.
	// QuestDB answered 2.0-2.1 seconds after start on both verified
	// versions with no network at all (measured); the budget is for a host
	// under load, not for a server that is not coming.
	readinessBudget = 3 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "questdb"},
		"sources": []map[string]any{
			{"kind": "questdb_checkpoint", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "questdb_checkpoint_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "questdb_data", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// QuestDB speaks SQL over its HTTP endpoint, so a check is an
			// ordinary statement and the core's generating built-ins apply
			// unchanged. The dialect work — dropping the CSV header and the
			// quoting around text values — is absorbed by the script, so
			// the core never learns an engine concept.
			"argv": []string{"bash", "-c", runnerScript, "bash", "{{sql}}"},
			"env":  map[string]string{},
		},
		"verbs_required": []string{"exec", "put_file"},
	}
}

// provisionRequest is the §6.2 payload this adapter reads.
type provisionRequest struct {
	Source struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	} `json:"source"`
	Sandbox struct {
		ScratchDir string `json:"scratch_dir"`
	} `json:"sandbox"`
	Options map[string]string `json:"options"`
	PITR    *struct {
		TargetTime string `json:"target_time"`
	} `json:"pitr"`
}

// opProvision restores one QuestDB data root into the sandbox and starts
// the engine on it.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, protoErr("invalid_request", false,
			"this adapter declares no point-in-time recovery capability: a QuestDB backup is one "+
				"checkpoint, and the artifact carries nothing to recover forward from")
	}
	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	// Before a byte moves: an artifact a copy job is still writing would
	// restore as a torn data root (see settle.go).
	if perr := assertSettled(ctx, src.path, settleWindow); perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"checkpointed", src.checkpointed, "user_tables", src.userTables)

	if perr := assertIdle(ctx, c); perr != nil {
		return nil, perr
	}

	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: stagingDir, Mode: "0700"})
	if perr != nil {
		return nil, perr
	}
	restoreSeconds, perr := placeDataRoot(ctx, c)
	if perr != nil {
		return nil, perr
	}
	readySeconds, perr := startEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	if perr := assertRestored(ctx, c, src); perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "restore_seconds", restoreSeconds, "engine_ready_seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": defaultPort,
			"database": defaultDatabase,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// Nothing inside a QuestDB backup records when it was taken
			// (see source.go).
			"created_at": nil,
		},
		"timings": map[string]any{
			// The engine can only start once the artifact is its data
			// root, so recovery is these two in sequence: restore is the
			// placement, engine_ready is the server coming up on it. Both
			// are things a real recovery does, and neither is padded with
			// work that one does not.
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"data_root": dataRoot},
	}, nil
}

// assertIdle refuses a sandbox whose engine is already running.
//
// This adapter replaces the data root, which cannot be done under a server
// holding those files open. The official image starts QuestDB itself, so
// the drill has to ask for an idle sandbox — and an operator who did not
// gets the parameter to add rather than a restore that fights the running
// server.
func assertIdle(ctx context.Context, c *core) *protoError {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", idleScript}})
	if perr != nil {
		return perr
	}
	// Only a positive answer refuses: the script says 0 when it reached a
	// server. Anything else means the question could not be asked — a
	// sandbox without curl, say — and a drill is not stopped by a probe
	// that did not run.
	if val.ExitCode != 0 || strings.TrimSpace(string(stdout)) != "0" {
		return nil
	}
	return protoErr("invalid_request", false,
		"QuestDB is already serving in this sandbox: this adapter replaces the data root, which needs "+
			"the engine stopped. Start the sandbox idle with the docker or k8s parameter "+
			`command: "sleep infinity" — the adapter starts the engine itself once the backup is in place`)
}

// placeDataRoot makes the transferred artifact the server's data root and
// reports how long that took: the act this engine calls a restore.
func placeDataRoot(ctx context.Context, c *core) (float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", placeScript, "bash", dataRoot, stagingDir},
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 || strings.TrimSpace(string(stdout)) == "" {
		return val.DurationSeconds, protoErr("source_corrupt", false,
			"the backup could not be made the server's data root: %s", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// startEngine starts the server on the restored data root and waits until
// it answers a query, or says what it said on the way down.
func startEngine(ctx context.Context, c *core) (float64, *protoError) {
	start := time.Now()
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", startScript}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("engine_not_ready", false, "start QuestDB: %s", firstLine(stderr))
	}
	for {
		ready, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", readyScript}})
		if perr != nil {
			return 0, perr
		}
		if ready.ExitCode == 0 {
			return time.Since(start).Seconds(), nil
		}
		if time.Since(start) > readinessBudget {
			return 0, protoErr("engine_not_ready", false,
				"QuestDB did not answer a query within %s of starting on the restored data root%s",
				readinessBudget, startupDiagnosis(ctx, c))
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for the engine to start")
		case <-time.After(readinessPoll):
		}
	}
}

// startupDiagnosis returns what the engine said while failing to start, as
// a suffix for the refusal above.
func startupDiagnosis(ctx context.Context, c *core) string {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", startupErrorScript}})
	if perr != nil || val.ExitCode != 0 {
		return ""
	}
	if line := firstLine(stdout); line != "" {
		return " — it said: " + line
	}
	return ""
}

// assertRestored refuses a well-formed zero: a server that came up on a
// data root holding tables and serves none of them has restored nothing,
// and reporting that green is the failure this project exists to prevent.
//
// The comparison is deliberately "any" rather than "all". A table
// directory the artifact still carries may belong to a table that was
// dropped before the backup was taken — QuestDB purges those lazily — so
// requiring every directory to reappear would refuse healthy backups.
//
// What this cannot catch is stated rather than implied: QuestDB serves a
// truncated column file without complaint. Measured on 10.0.1, a column
// cut from 16 MiB to 64 bytes left count(*) answering 250 while the column
// held 8 real values, with no error anywhere — so a row count proves the
// table exists, not that its data survived. The adapter README says so,
// and says what to check instead.
func assertRestored(ctx context.Context, c *core, src *resolvedSource) *protoError {
	if src.userTables == 0 {
		return nil
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", tablesScript}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("restore_failed", false,
			"the engine started but would not say what it serves: %s", firstLine(stderr))
	}
	served, err := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if err != nil {
		return protoErr("restore_failed", false,
			"the engine started but did not answer a table count")
	}
	if served == 0 {
		return protoErr("source_corrupt", false,
			"the backup holds %d table%s and the restored server serves none: the data root came up "+
				"empty", src.userTables, plural(src.userTables))
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// healthcheckRequest is the §6.3 payload.
type healthcheckRequest struct {
	State struct {
		DataRoot string `json:"data_root"`
	} `json:"state"`
}

// opHealthcheck proves the restored server still answers a query.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", readyScript}})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		detail := firstLine(stderr)
		if detail == "" {
			detail = fmt.Sprintf("the engine did not answer a query (curl exit %d)", val.ExitCode)
		}
		return map[string]any{"healthy": false, "detail": detail}, nil
	}
	return map[string]any{"healthy": true}, nil
}

// firstLine is the first non-empty line of engine output, trimmed.
func firstLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}
