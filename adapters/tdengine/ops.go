package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "tdengine"
	adapterVersion = "0.1.0"
	// defaultPort is where taosAdapter serves HTTP inside the sandbox.
	// Nothing is published: checks run in-sandbox through the runner.
	defaultPort = 6041
	// readinessBudget bounds the wait for a server that never answers.
	// The image's own entrypoint starts taosd, which answered 0.6 s after
	// the container started, with the REST endpoint 1.9 s behind it
	// (measured on both verified lines); the budget is for a host under
	// load, not for a server that is not coming.
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
		"engine":            map[string]string{"name": "tdengine"},
		// The directory kind is declared first because it is what the
		// tool produces and what a drill usually names; the conformance
		// suite provisions from the first kind, and this adapter's
		// committed fixture is a dump directory.
		"sources": []map[string]any{
			{"kind": "taosdump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "taosdump_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "taosdump_tar", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// TDengine speaks SQL over its HTTP endpoint, so a check is an
			// ordinary statement and the core's generating built-ins apply
			// unchanged. The dialect work — unwrapping the JSON answer and
			// its quoting — is absorbed by the script, so the core never
			// learns an engine concept.
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

// opProvision restores one taosdump backup into the sandbox's server.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, protoErr("invalid_request", false,
			"this adapter declares no point-in-time recovery capability: a taosdump backup is one "+
				"logical export, and the artifact carries nothing to recover forward from")
	}
	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	// Before a byte moves: an artifact still being written, and one the
	// engine would empty rather than restore (fence.go).
	if perr := assertSettled(ctx, src.path, settleWindow); perr != nil {
		return nil, perr
	}
	if perr := rejectAgedBackup(src, time.Now()); perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"database", src.database, "keep_days", src.keepDays, "rows", src.rows)

	readySeconds, perr := awaitReady(ctx, c)
	if perr != nil {
		return nil, perr
	}
	dest := stagingDir
	if src.tarball {
		dest = archivePath
	}
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dest, Mode: "0700"})
	if perr != nil {
		return nil, perr
	}
	dumpDir := stagingDir
	transfer := put.DurationSeconds
	if src.tarball {
		unpacked, extract, perr := extractArchive(ctx, c)
		if perr != nil {
			return nil, perr
		}
		dumpDir = unpacked
		transfer += extract
	}
	restoreSeconds, perr := runRestore(ctx, c, dumpDir, src)
	if perr != nil {
		return nil, perr
	}
	database, perr := assertRestored(ctx, c, src)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "restore_seconds", restoreSeconds, "database", database)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": defaultPort,
			"database": database,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// taosdump records when it started; an artifact that lost that
			// file dates to nothing (see source.go).
			"created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transfer,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"database": database},
	}, nil
}

// awaitReady waits for the endpoint the checks will use.
func awaitReady(ctx context.Context, c *core) (float64, *protoError) {
	begin := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", readyScript}, TimeoutSeconds: 10})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(begin).Seconds(), nil
		}
		if time.Since(begin) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"the server did not answer a query within %s: this adapter restores into the server the "+
					"image starts, so the sandbox needs no command override and the engine has to come "+
					"up on its own", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for the server")
		case <-time.After(readinessPoll):
		}
	}
}

// extractArchive unpacks a tar artifact and reports the directory the
// restore tool has to be pointed at.
func extractArchive(ctx context.Context, c *core) (string, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", extractScript, "bash", stagingDir, archivePath},
	})
	if perr != nil {
		return "", 0, perr
	}
	if val.ExitCode == 90 {
		return "", 0, protoErr("source_corrupt", false, "the archive could not be unpacked: %s", firstLine(stderr))
	}
	dir := strings.TrimSpace(string(stdout))
	if val.ExitCode != 0 || dir == "" {
		return "", 0, protoErr("source_corrupt", false,
			"the archive holds no taosdump backup: nothing inside it carries a dbs.sql")
	}
	return dir, val.DurationSeconds, nil
}

// runRestore drives the vendor's tool and judges what it said.
//
// Never the exit code: taosdump answers 0 when it restored everything,
// when it restored half of a damaged backup, and when it never reached
// the server at all (all measured). What it prints is the only account
// there is, and the artifact's own row count is what it has to match.
func runRestore(ctx context.Context, c *core, dumpDir string, src *resolvedSource) (float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", restoreScript, "bash", dumpDir}})
	if perr != nil {
		return 0, perr
	}
	outcome := readRestoreOutcome(stdout)
	if perr := outcome.verdict(src.rows, src.rowsKnown); perr != nil {
		return val.DurationSeconds, perr
	}
	return val.DurationSeconds, nil
}

// assertRestored refuses a well-formed zero: a server that answers while
// serving none of the backup's tables has restored nothing, and reporting
// that green is the failure this project exists to prevent.
func assertRestored(ctx context.Context, c *core, src *resolvedSource) (string, *protoError) {
	database := src.database
	if database == "" {
		// The archive kinds learn the name only inside the sandbox; the
		// restore recreated whatever the dump held, and the checks need a
		// name to talk to.
		return "", protoErr("source_corrupt", false, "the backup does not name the database it holds")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", databaseScript, "bash", database},
	})
	if perr != nil {
		return "", perr
	}
	if val.ExitCode != 0 {
		return "", protoErr("restore_failed", false,
			"the server would not say what it serves after the restore: %s", firstLine(stderr))
	}
	tables, err := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if err != nil {
		return "", protoErr("restore_failed", false,
			"the server did not answer a table count for the restored database")
	}
	if tables == 0 {
		return "", protoErr("source_corrupt", false,
			"the restore reported success and database %q serves no table at all", database)
	}
	return database, nil
}

// healthcheckRequest is the §6.3 payload.
type healthcheckRequest struct {
	State struct {
		Database string `json:"database"`
	} `json:"state"`
}

// opHealthcheck proves the restored server still answers a query.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", readyScript}, TimeoutSeconds: 10})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		detail := firstLine(stderr)
		if detail == "" {
			detail = "the server did not answer a query"
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
