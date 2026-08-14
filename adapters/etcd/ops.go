package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	adapterName    = "etcd"
	adapterVersion = "0.1.0"

	// clientEndpoint is where the restored server serves inside the
	// sandbox. No TLS and no auth: a Probavi sandbox is zero-ingress
	// (--network none, no ports expressible), which is the only reason a
	// bare client port on restored production data is acceptable.
	clientEndpoint = "http://127.0.0.1:2379"
	defaultPort    = 2379

	// dataDir is where the snapshot is restored. It is adapter-composed
	// under the sandbox's own filesystem, never operator input, and it
	// must not exist before `etcdutl snapshot restore` creates it.
	dataDir = "/probavi-etcd/data"
	// serverLog is where the backgrounded server writes; the readiness
	// timeout path reads it so a start failure names the engine's own
	// reason instead of "never became ready".
	serverLog = "/probavi-etcd/etcd.log"

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
		"engine":            map[string]string{"name": "etcd"},
		"sources": []map[string]any{
			{"kind": "etcd_snapshot", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "etcd_snapshot_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// etcd has no SQL: the check text the core passes through
			// {{sql}} is a line of etcdctl arguments (documented in the
			// adapter README), expanded by the shell into words. This is
			// word splitting only, not shell parsing: a POSIX shell does
			// not re-read expansions as syntax, so operators like ; and |
			// in the text stay literal arguments, and set -f keeps glob
			// characters literal too. The engine dialect is absorbed here,
			// declaratively — the core never learns it (§6.1).
			"argv": []string{"sh", "-c",
				"set -f; exec etcdctl --endpoints=" + clientEndpoint + " --dial-timeout=5s $0", "{{sql}}"},
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

// opProvision restores the snapshot into the idle sandbox and starts the
// server on the restored data: preflight (shell present, engine idle),
// transfer, integrity check, restore, background start, readiness.
//
// The whole lifecycle belongs to the adapter because it has to: a
// snapshot restores into a data directory the server then starts from, so
// the sequence "restore, then serve" cannot be expressed by an image's
// own entrypoint. The drill config starts the sandbox idle
// (docker: command: sleep infinity).
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

	if perr := checkSandbox(ctx, c); perr != nil {
		return nil, perr
	}

	snapInSandbox := scratch + "/probavi-snapshot.db"
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: snapInSandbox, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}

	if perr := checkSnapshot(ctx, c, snapInSandbox); perr != nil {
		return nil, perr
	}

	restore, stderr, perr := execChecked(ctx, c,
		"etcdutl", "snapshot", "restore", snapInSandbox, "--data-dir", dataDir)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, mapRestoreFailure(stderr)
	}
	logger.Info("snapshot restored", "seconds", restore.DurationSeconds)

	readySeconds, perr := startEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine serving restored data", "seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "etcd", "host": "127.0.0.1", "port": defaultPort,
			// etcd has no databases; the field is a protocol requirement
			// and the declared sql_runner never references it.
			"database": "", "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// A snapshot records revisions, not wall clocks: nothing in
			// the artifact can honestly date it (see source.go).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			"restore_seconds":      restore.DurationSeconds,
		},
		"state": map[string]any{"data_dir": dataDir, "snapshot_path": snapInSandbox},
	}, nil
}

// checkSandbox verifies the one precondition this flow cannot survive
// without: the image must carry a POSIX shell (the official etcd images
// are distroless and do not — the README ships the two-line wrapper
// recipe). A server already bound to the client port is deliberately not
// probed for here: the sibling adapters that refuse a running engine can
// afford to, because their conformance runs drive a logical kind, while
// every kind of this adapter owns the engine lifecycle — an occupied port
// surfaces at start as the engine's own address-in-use error instead.
func checkSandbox(ctx context.Context, c *core) *protoError {
	// Providers report a missing executable in different shapes — some as
	// a failed verb, some as a non-zero exit with the runtime's message on
	// stderr (measured: the docker provider does the latter) — so both
	// shapes get the same answer.
	sh, stderr, perr := execChecked(ctx, c, "sh", "-c", "true")
	detail := ""
	switch {
	case perr != nil:
		detail = perr.Message
	case sh.ExitCode != 0:
		detail = firstLine(stderr)
	default:
		return nil
	}
	return protoErr("invalid_request", false,
		"the sandbox image has no POSIX shell, which starting the restored server requires: "+
			"the official etcd images are distroless — build the wrapper image the adapter README "+
			"documents (%s)", detail)
}

// checkSnapshot asks etcdutl whether the transferred file is a readable
// snapshot before anything is restored from it.
func checkSnapshot(ctx context.Context, c *core, path string) *protoError {
	status, stderr, perr := execChecked(ctx, c, "etcdutl", "snapshot", "status", path)
	if perr != nil {
		return perr
	}
	if status.ExitCode != 0 {
		return protoErr("source_corrupt", false,
			"etcdutl rejected the snapshot: %s", firstLine(stderr))
	}
	return nil
}

// startEngine launches the restored server in the background and waits for
// it to serve. etcd has no daemonize mode, so the shell detaches it — the
// wrapper-image requirement exists exactly for this line — and launch
// failures surface as the readiness wait timing out, at which point the
// server's own log is read before blaming the engine.
func startEngine(ctx context.Context, c *core) (float64, *protoError) {
	script := fmt.Sprintf(
		`etcd --data-dir=%s --listen-client-urls=%s --advertise-client-urls=%s >%s 2>&1 </dev/null &`,
		dataDir, clientEndpoint, clientEndpoint, serverLog)
	start, stderr, perr := execChecked(ctx, c, "sh", "-c", script)
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false, "restored server failed to launch: %s", firstLine(stderr))
	}
	readySeconds, perr := awaitEngine(ctx, c)
	if perr != nil {
		return 0, describeStartFailure(ctx, c, perr)
	}
	return start.DurationSeconds + readySeconds, nil
}

// awaitEngine polls endpoint health until the server answers.
func awaitEngine(ctx context.Context, c *core) (float64, *protoError) {
	start := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv: []string{"etcdctl", "--endpoints=" + clientEndpoint, "--dial-timeout=2s",
				"endpoint", "health"},
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
				"engine did not answer endpoint health within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// describeStartFailure enriches a readiness timeout with the tail of the
// server's log: a data directory from an incompatible version, for
// instance, makes etcd exit immediately with a precise message.
func describeStartFailure(ctx context.Context, c *core, perr *protoError) *protoError {
	if perr.Code != "engine_not_ready" {
		return perr
	}
	val, stdout, _, eperr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c",
			`tail -n 10 ` + serverLog + ` 2>/dev/null | grep -iE "fatal|panic|error" | tail -n 1`},
	})
	if eperr != nil || val.ExitCode != 0 || len(stdout) == 0 {
		return perr
	}
	return protoErr("restore_failed", false, "restored server failed to start: %s", firstLine(stdout))
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored server still answers (§6.3). An
// unhealthy engine is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, _, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"etcdctl", "--endpoints=" + clientEndpoint, "--dial-timeout=2s",
			"endpoint", "health"},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := "endpoint healthy"
	if !healthy {
		detail = fmt.Sprintf("etcdctl endpoint health exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// mapRestoreFailure classifies a failed etcdutl snapshot restore. The
// hash-check refusal deserves its own words: a db file copied out of a
// live data directory lacks the integrity hash `etcdctl snapshot save`
// appends, and telling the operator to change how the backup is taken is
// more useful than "corrupt".
func mapRestoreFailure(stderr []byte) *protoError {
	line := firstLine(stderr)
	if strings.Contains(line, "hash") {
		return protoErr("source_corrupt", false,
			"the snapshot carries no integrity hash — it was likely copied out of a data directory "+
				"rather than taken with `etcdctl snapshot save`, which is the format this adapter "+
				"restores: %s", line)
	}
	return protoErr("restore_failed", false, "etcdutl snapshot restore failed: %s", line)
}

// execChecked wraps core.exec returning the value and raw stderr.
func execChecked(ctx context.Context, c *core, argv ...string) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
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
