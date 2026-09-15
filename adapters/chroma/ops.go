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
	adapterVersion = "0.2.0"

	// readinessBudget bounds waiting for the engine to answer. Chroma
	// rebuilds any HNSW segment the write queue still covers at startup,
	// so a large collection takes longer than an empty one.
	readinessBudget = 4 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// sandboxPaths is everything this adapter puts inside the sandbox,
// composed from sandbox.scratch_dir — the writable directory the provider
// guarantees (§6.2) and the only one an adapter may rely on.
//
// These were absolute paths under / until issue #287, and the docker
// provider hid it: its commands run as root on a disposable filesystem, so
// a directory at the root costs nothing. On the bare-host provider every
// payload runs as the drill user in a workspace it owns, and / is not
// writable. The log moves with them for a second reason: /tmp is writable
// there but shared, so two drills on one host wrote the same file.
type sandboxPaths struct {
	// root holds everything below, and is what the prepare step clears.
	root string
	// data is the restored persistence directory. The engine is pointed at
	// it and never at the staging copy, so a failed extraction cannot be
	// served as a database.
	data string
	// staging is where a transferred persistence directory lands. It is
	// created by the transfer, never before it.
	staging string
	// archive is where a transferred archive lands, and unpack is where it
	// is extracted before the persistence directory inside it is located.
	archive string
	unpack  string
	// log is where the detached server's own words go.
	log string
}

// newSandboxPaths derives the set from the scratch directory the provision
// request carried. An empty scratch_dir falls back to /tmp, which every
// sandbox has and every user may write.
func newSandboxPaths(scratch string) sandboxPaths {
	if scratch == "" {
		scratch = "/tmp"
	}
	root := scratch + "/probavi-chroma"
	return sandboxPaths{
		root:    root,
		data:    root + "/data",
		staging: root + "/staging",
		archive: root + "/backup.tar",
		unpack:  root + "/unpack",
		log:     root + "/chroma.log",
	}
}

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

	paths := newSandboxPaths(req.Sandbox.ScratchDir)
	if perr := prepareSandbox(ctx, c, paths); perr != nil {
		return nil, perr
	}

	transferStart := time.Now()
	if _, perr := c.putFile(ctx, putFileArgs{
		SourcePath: src.path, DestPath: stagingPath(src, paths), Mode: "0700",
	}); perr != nil {
		return nil, perr
	}
	transferSeconds := time.Since(transferStart).Seconds()

	restoreStart := time.Now()
	if perr := placeData(ctx, c, src, paths); perr != nil {
		return nil, perr
	}
	restoreSeconds := time.Since(restoreStart).Seconds()

	readySeconds, perr := startEngine(ctx, c, paths)
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
		"state": map[string]any{"data_dir": paths.data, "collections": collections},
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
func stagingPath(src *source, paths sandboxPaths) string {
	if src.kind == kindDataTar {
		return paths.archive
	}
	return paths.staging
}

// prepareSandbox makes the directory the transfer copies into.
func prepareSandbox(ctx context.Context, c *core, paths sandboxPaths) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", prepareScript, "bash", paths.root}, TimeoutSeconds: 60})
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
func placeData(ctx context.Context, c *core, src *source, paths sandboxPaths) *protoError {
	script, args := placeDirScript, []string{"bash", paths.data, paths.staging}
	if src.kind == kindDataTar {
		script, args = placeTarScript(src.gzip), []string{"bash", paths.data, paths.unpack, paths.archive}
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: append([]string{"bash", "-c", script}, args...), TimeoutSeconds: 900})
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
func startEngine(ctx context.Context, c *core, paths sandboxPaths) (float64, *protoError) {
	start := time.Now()
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", startScript, "bash", paths.data, paths.log}})
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
				readinessBudget, startupDiagnosis(ctx, c, paths.log))
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
func startupDiagnosis(ctx context.Context, c *core, engineLog string) string {
	_, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", startupErrorScript, "bash", engineLog}, TimeoutSeconds: 15,
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
