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
	adapterName    = "chroma"
	adapterVersion = "0.1.0"

	// dataDir is where the restored persistence directory lands. The
	// engine is pointed at it and never at the staging copy, so a failed
	// extraction cannot be served as a database.
	dataDir = "/probavi-chroma/data"
	// rootDir holds everything this adapter puts in the sandbox.
	rootDir = "/probavi-chroma"
	// stagingDir is where a transferred persistence directory lands. It is
	// created by the transfer, never before it.
	stagingDir = "/probavi-chroma/staging"
	// archivePath is where a transferred archive lands.
	archivePath = "/probavi-chroma/backup.tar"
	// engineLog is where the detached server's own words go.
	engineLog = "/tmp/probavi-chroma.log"
	// unpackDir is where an archive is extracted before the persistence
	// directory inside it is located.
	unpackDir = "/probavi-chroma/unpack"

	// readinessBudget bounds waiting for the engine to answer. Chroma
	// rebuilds any HNSW segment the write queue still covers at startup,
	// so a large collection takes longer than an empty one.
	readinessBudget = 4 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// probePayload reports identity and capabilities (§6.1).
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "chroma"},
		"sources": []map[string]any{
			// The archive kind leads because the §10 conformance suite
			// provisions its first declared kind from 64 KiB of random
			// bytes: a kind that reads magic host-side would refuse them
			// and fail a check that is asking whether the adapter can
			// restore at all. An archive defers to the engine by design —
			// what is inside it is tar's verdict and then Chroma's — so
			// accepting the bytes here and failing in the sandbox is the
			// honest order for it anyway. The directory kind is the one
			// an operator reaches for most (the qdrant precedent).
			{"kind": kindDataTar, "capabilities": map[string]bool{"pitr": false}},
			{"kind": kindData, "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Chroma has no SQL, so the check text is an API path with an
			// optional JSON body and the dialect is absorbed here,
			// declaratively — the core never learns it. {{database}} is
			// passed but unused: a restored persistence directory serves
			// one tenant and one database, and the path names the
			// collection.
			"argv": []string{"bash", "-c", checkScript, "bash", "{{database}}", "{{sql}}"},
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
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection map[string]any `json:"connection"`
	State      map[string]any `json:"state"`
}

// opProvision restores the artifact and refuses anything it cannot prove
// serves (§6.2).
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "provision payload is not valid JSON")
	}
	if perr := refuseUnknownParams(req.Source.Params); perr != nil {
		return nil, perr
	}
	src, perr := inspect(req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "kind", src.kind, "path", src.path,
		"size_bytes", src.sizeBytes, "gzip", src.gzip)

	if perr := prepareSandbox(ctx, c); perr != nil {
		return nil, perr
	}

	transferStart := time.Now()
	if _, perr := c.putFile(ctx, putFileArgs{
		SourcePath: src.path, DestPath: stagingPath(src), Mode: "0700",
	}); perr != nil {
		return nil, perr
	}
	transferSeconds := time.Since(transferStart).Seconds()

	restoreStart := time.Now()
	if perr := placeData(ctx, c, src); perr != nil {
		return nil, perr
	}
	restoreSeconds := time.Since(restoreStart).Seconds()

	readySeconds, perr := startEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	collections, perr := verifyRestore(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore verified", "collections", collections)

	return map[string]any{
		"connection": map[string]any{
			"scheme":   "http",
			"host":     "127.0.0.1",
			"port":     8000,
			"database": "default_database",
			"user":     "default_tenant",
		},
		"source_identity": map[string]any{
			"checksum":   src.checksum,
			"size_bytes": src.sizeBytes,
			// Nothing in a Chroma persistence directory dates the backup.
			// The engine writes no manifest, the artifact is a copy rather
			// than an export, and a file's mtime dates the copy rather
			// than the data in it — so this is null rather than a number
			// that would read as a fact (the redis AOF precedent).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"data_dir": dataDir, "collections": collections},
	}, nil
}

// opHealthcheck reports whether the engine still serves (§6.3).
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "healthcheck payload is not valid JSON")
	}
	start := time.Now()
	val, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", readyScript}, TimeoutSeconds: 30})
	if perr != nil {
		return nil, perr
	}
	latency := time.Since(start).Seconds()
	if val.ExitCode != 0 {
		return map[string]any{
			"healthy": false, "latency_seconds": latency,
			"detail": "chroma did not answer its heartbeat",
		}, nil
	}
	return map[string]any{
		"healthy": true, "latency_seconds": latency,
		"detail": "heartbeat answered",
	}, nil
}

// stagingPath is where put_file drops the artifact: the directory itself
// for a persistence directory, a file for an archive.
func stagingPath(src *source) string {
	if src.kind == kindDataTar {
		return archivePath
	}
	return stagingDir
}

// prepareSandbox makes the directory the transfer copies into.
func prepareSandbox(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", prepareScript}, TimeoutSeconds: 60})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("sandbox_error", true, "prepare the sandbox: %s", firstLine(stderr))
	}
	return nil
}

// placeData turns whatever arrived in staging into the directory the
// engine will be pointed at.
//
// The engine is never pointed at the staging copy: an extraction that
// failed half way would otherwise be served as a database.
func placeData(ctx context.Context, c *core, src *source) *protoError {
	script := placeDirScript
	if src.kind == kindDataTar {
		script = placeTarScript(src.gzip)
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", script}, TimeoutSeconds: 900})
	if perr != nil {
		return perr
	}
	switch val.ExitCode {
	case 0:
		return nil
	case exitNoDatabase:
		if src.kind == kindDataTar {
			return protoErr("source_corrupt", false,
				"the archive holds no %s, at its root or under a single wrapping directory, so it is not "+
					"an archive of a Chroma persistence directory", sqliteFile)
		}
		return protoErr("source_corrupt", false,
			"the transferred directory holds no %s inside the sandbox, though it did on the drill host — "+
				"the copy did not arrive whole", sqliteFile)
	default:
		return protoErr("source_corrupt", false, "unpack the backup: %s", firstLine(stderr))
	}
}

// refuseUnknownParams rejects a source.params key this adapter does not
// act on, so a drill config never reads as configuring something it is
// not. The core passes params through untouched and cannot do this.
func refuseUnknownParams(params map[string]string) *protoError {
	for name := range params {
		return protoErr("invalid_request", false,
			"source.params.%s has no effect for this adapter: a Chroma persistence directory "+
				"carries everything the restore needs, and this adapter declares no parameters", name)
	}
	return nil
}

// startEngine starts the server on the restored directory and waits until
// it answers, or says what the engine said while failing to.
func startEngine(ctx context.Context, c *core) (float64, *protoError) {
	start := time.Now()
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", startScript}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("engine_not_ready", false, "start Chroma: %s", firstLine(stderr))
	}
	for {
		ready, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", readyScript}, TimeoutSeconds: 15})
		if perr != nil {
			return 0, perr
		}
		if ready.ExitCode == 0 {
			return time.Since(start).Seconds(), nil
		}
		if time.Since(start) > readinessBudget {
			return 0, protoErr("engine_not_ready", false,
				"Chroma did not answer within %s of starting on the restored directory%s",
				readinessBudget, startupDiagnosis(ctx, c))
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for the engine to start")
		case <-time.After(readinessPoll):
		}
	}
}

// Exit codes the verdict script uses to say which verdict it reached.
const (
	exitNoCollections = 3
	exitIndexLost     = 4
)

// verifyRestore is the verdict, and it is deliberately not a count.
//
// Measured on chromadb/chroma:1.5.9: with the write queue already purged,
// deleting a collection's whole HNSW segment directory leaves `count`
// answering exactly right — 2200 of 2200 — and every document readable,
// while the nearest-neighbour search the store exists for returns nothing.
// No error, no warning, no line in the engine's log. A drill that asked for
// a record count would have reported a green restore of a database that can
// no longer answer the one question it is for.
//
// The script therefore queries each collection with a vector taken out of
// that same collection and requires as many neighbours back as it asked
// for, and it answers in its exit code so that the count it prints on
// success stays a plain number.
func verifyRestore(ctx context.Context, c *core) (int, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", verdictScript}, TimeoutSeconds: 120,
	})
	if perr != nil {
		return 0, perr
	}
	switch val.ExitCode {
	case 0:
		return atoi(string(stdout)), nil
	case exitNoCollections:
		return 0, protoErr("restore_failed", false,
			"the restored directory serves no collections: a Chroma backup that restores to nothing is a "+
				"backup of nothing, whatever the engine reports")
	case exitIndexLost:
		return 0, protoErr("source_corrupt", false,
			"%s — its vector index did not survive the backup. Chroma reports the record count correctly "+
				"in this state and logs nothing, so the count is not the proof and the query is. Copy the "+
				"persistence directory with the server stopped, and keep every segment directory beside %s",
			firstLine(stderr), sqliteFile)
	default:
		return 0, protoErr("restore_failed", false,
			"reading the restored database failed: %s", firstLine(stderr))
	}
}

// startupDiagnosis returns what the engine said while failing to start, as
// a parenthesised suffix, or nothing when it said nothing useful.
func startupDiagnosis(ctx context.Context, c *core) string {
	_, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", startupErrorScript}, TimeoutSeconds: 15,
	})
	if perr != nil {
		return ""
	}
	said := strings.TrimSpace(string(stdout))
	if said == "" {
		return ""
	}
	return " (engine said: " + firstLine([]byte(said)) + ")"
}

// firstLine reduces captured output to one readable line.
func firstLine(b []byte) string {
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "no output"
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

// atoi reads a non-negative integer, returning 0 for anything else — the
// verdict script already normalises its fields, and a malformed one must
// fail the comparison rather than the parse.
func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
