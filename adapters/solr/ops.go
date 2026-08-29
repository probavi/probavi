package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "solr"
	adapterVersion = "0.2.0"
	// defaultPort is where Solr listens inside the sandbox. Nothing is
	// published: checks run in-sandbox through the runner below.
	defaultPort = 8983
	// transferDir is the name the artifact is given inside SOLR_HOME. The
	// Collections API restores from `location=<parent>&name=<this>`, so
	// the artifact's own directory name never has to be trusted.
	transferDir = "probavi-restore"
	// readinessBudget bounds the wait for a sandbox that never serves.
	// Solr answered in about two seconds on both verified versions with
	// no network at all; the budget is for a host under load, not for a
	// server that is not coming.
	readinessBudget = 3 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// collectionPattern is what Solr accepts as a collection name, and what
// this adapter will hand to the Collections API.
var collectionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "solr"},
		"sources": []map[string]any{
			{"kind": "solr_backup_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "solr_backup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "solr_backup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Solr has no SQL: the check text the core passes through
			// {{sql}} is one Solr query string — everything an operator
			// would write after `select?` — travelling as a single argv
			// element, so no shell sees it. {{database}} delivers the
			// collection provision restored into. The dialect, and the
			// CSV writer's silence about counts, are absorbed in
			// runnerScript; the core never learns either.
			"argv": []string{"bash", "-c", runnerScript, "bash", "{{database}}", "{{sql}}"},
			"env":  map[string]string{},
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

// timings accumulates the §7 measurements this adapter owns.
type timings struct {
	engineReady float64
	transfer    float64
	restore     float64
}

// opProvision restores one Collections API backup into the running
// sandbox.
//
// The official image starts Solr itself, in SolrCloud mode with an
// embedded ZooKeeper (measured: `mode: solrcloud`, `zkHost
// 127.0.0.1:9983`), so unlike the engines that need an idle sandbox this
// adapter waits for the server rather than starting it. The ROADMAP
// entry that scheduled this adapter expected standalone cores and the
// replication handler; the image says otherwise, and the image is what
// operators run.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if perr := checkRequest(req); perr != nil {
		return nil, perr
	}
	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	// Before a byte moves: a backup whose own configuration deletes what
	// it restores cannot be drilled honestly (see fence.go).
	if perr := rejectExpiringBackup(src.path); perr != nil {
		return nil, perr
	}
	// A name the operator supplied is checked here, before anything is
	// done to the sandbox: a drill that cannot be honest should cost
	// nothing. The name discovered from the artifact is checked below,
	// because an archive only gives it up once unpacked.
	if requested := option(req.Options, "collection", ""); requested != "" &&
		!collectionPattern.MatchString(requested) {
		return nil, protoErr("invalid_request", false,
			"collection %q is not a name Solr accepts (letters, digits, - and _)", requested)
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"backup_collection", src.collection)

	var t timings
	home, ready, perr := prepareEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	t.engineReady = ready

	held, transfer, extract, perr := placeArtifact(ctx, c, src, home)
	if perr != nil {
		return nil, perr
	}
	t.transfer = transfer
	t.restore += extract

	collection := option(req.Options, "collection", held)
	if !collectionPattern.MatchString(collection) {
		return nil, protoErr("source_corrupt", false,
			"the backup names its collection %q, which Solr will not accept as one", collection)
	}
	logger.Info("artifact in place", "collection", held, "restore_target", collection)

	restored, perr := restoreCollection(ctx, c, home, collection)
	if perr != nil {
		return nil, perr
	}
	t.restore = restored

	if perr := assertServing(ctx, c, collection); perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "restore_seconds", t.restore, "collection", collection)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": defaultPort,
			"database": collection,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": t.engineReady,
			"transfer_seconds":     t.transfer,
			"restore_seconds":      t.restore,
		},
		"state": map[string]any{"collection": collection},
	}, nil
}

// placeArtifact gets the backup into the layout the Collections API
// restores from, and reports the collection it holds.
//
// A directory is transferred as it stands. An archive is transferred as
// one file and unpacked by the sandbox, which is also what settles the
// collection name when the host-side pass could not read the stream.
func placeArtifact(ctx context.Context, c *core, src *resolvedSource, home string) (
	collection string, transfer, extract float64, perr *protoError,
) {
	if !src.tarball {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: src.path, DestPath: home + "/" + transferDir, Mode: "0755",
		})
		if perr != nil {
			return "", 0, 0, perr
		}
		return src.collection, put.DurationSeconds, 0, nil
	}
	archive := home + "/probavi-backup.tar"
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: archive, Mode: "0600"})
	if perr != nil {
		return "", 0, 0, perr
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", extractScript, "bash", home, transferDir, archive},
	})
	if perr != nil {
		return "", put.DurationSeconds, 0, perr
	}
	if val.ExitCode != 0 {
		return "", put.DurationSeconds, val.DurationSeconds, protoErr("source_corrupt", false,
			"the archive does not unpack into a Solr backup: %s", firstLine(stderr))
	}
	held := strings.TrimSpace(string(stdout))
	if src.collection != "" {
		held = src.collection
	}
	return held, put.DurationSeconds, val.DurationSeconds, nil
}

// checkRequest validates everything the request supplies before any
// sandbox call.
func checkRequest(req *provisionRequest) *protoError {
	if req.PITR != nil {
		return protoErr("invalid_request", false,
			"point-in-time recovery is not supported: a Solr backup is a snapshot of an index at one "+
				"instant, and the engine offers nothing to recover between two of them")
	}
	// A Collections API backup records its own start time, in UTC, so a
	// declared zone has nothing to correct.
	if req.Source.Params[backupTimezoneParam] != "" {
		return protoErr("invalid_request", false,
			"source.params.%s has no effect for this adapter: a Solr backup records its own start "+
				"time in UTC (backup_N.properties), and that is what reaches backup.created_at",
			backupTimezoneParam)
	}
	return nil
}

// backupTimezoneParam names the IANA zone the backup host was in. The
// adapters whose engines record no timestamp read it; this one refuses it.
const backupTimezoneParam = "backup_timezone"

// prepareEngine resolves SOLR_HOME and waits until Solr answers,
// returning the home and the measured wait.
func prepareEngine(ctx context.Context, c *core) (string, float64, *protoError) {
	start := time.Now()
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", homeScript}})
	if perr != nil {
		return "", 0, perr
	}
	if val.ExitCode != 0 {
		return "", 0, protoErr("internal", false, "resolve SOLR_HOME: %s", firstLine(stderr))
	}
	home := strings.TrimSpace(string(stdout))
	if !usablePath(home) {
		// An image that cannot tell us gets the documented default. It is
		// not a guess between equals: writing there and letting Solr
		// refuse the path in its own words beats picking a different one
		// quietly, and Solr's refusal names the setting.
		home = defaultSolrHome
	}
	if perr := awaitReady(ctx, c, start); perr != nil {
		return "", 0, perr
	}
	if perr := assertCloudMode(ctx, c); perr != nil {
		return "", 0, perr
	}
	return home, time.Since(start).Seconds(), nil
}

// assertCloudMode refuses a standalone server with the reason, rather
// than letting the Collections API answer 400 and leaving an operator to
// work out why.
func assertCloudMode(ctx context.Context, c *core) *protoError {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", cloudScript}})
	if perr != nil {
		return perr
	}
	if val.ExitCode == 0 && strings.TrimSpace(string(stdout)) != "0" {
		return nil
	}
	return protoErr("invalid_request", false,
		"this sandbox runs Solr in standalone mode, and this adapter restores through the Collections "+
			"API, which such a server refuses. The official solr:10 image starts in SolrCloud mode with "+
			"an embedded ZooKeeper and needs no configuration; solr:9.10 starts standalone (both "+
			"measured), so point the drill at a 10.x image or start the server with -c")
}

// awaitReady polls until Solr serves its system information.
func awaitReady(ctx context.Context, c *core, start time.Time) *protoError {
	for {
		val, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", readyScript}})
		if perr != nil {
			return perr
		}
		if val.ExitCode == 0 {
			return nil
		}
		if time.Since(start) > readinessBudget {
			return protoErr("engine_not_ready", false,
				"Solr did not answer within %s; the sandbox image must start the server, which the "+
					"official image does by default", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return protoErr("cancelled", true, "cancelled while waiting for Solr")
		case <-time.After(readinessPoll):
		}
	}
}

// restoreCollection asks the Collections API to restore, and classifies
// what comes back. The API answers 200 with a status inside the body for
// several failures, so the body is read rather than the HTTP code alone.
func restoreCollection(ctx context.Context, c *core, home, collection string) (float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", restoreScript, "bash", home, transferDir, collection},
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1" {
		return val.DurationSeconds, nil
	}
	return val.DurationSeconds, mapRestoreFailure(string(stderr))
}

// usablePath reports whether a value read out of the sandbox is a plain
// absolute path this adapter is willing to build a shell argument from.
func usablePath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.ContainsAny(p, " \t\n$`\"'\\")
}

// errorMessage returns the Collections API's own reason, if the body
// carries one.
func errorMessage(body string) string {
	const key = `"msg":"`
	i := strings.Index(body, key)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return rest
}

// restoreCorruptMarkers are what a damaged or incomplete artifact
// produces, as opposed to a drill that asked for something impossible.
// Measured against Solr 10 with a damaged index and with the shard
// metadata removed: both answer HTTP 500 with "Could not restore core".
// The restore is the act of reading the artifact, so a core the engine
// cannot restore from it is a statement about the backup. Both verdicts
// are a drill failure either way (evidence-schema.md §fail); what this
// buys is a diagnosis an operator can act on.
var restoreCorruptMarkers = []string{
	"Could not restore core",
	"Index 0 out of bounds",
	"Could not find a valid backup",
	"Couldn't restore since doesn't exist",
	"CorruptIndexException",
	"EOFException",
}

// mapRestoreFailure turns the engine's own words — which the script puts
// on stderr — into a verdict.
func mapRestoreFailure(diagnosis string) *protoError {
	msg := errorMessage(diagnosis)
	if msg == "" {
		msg = firstLine([]byte(diagnosis))
	}
	if msg == "" {
		msg = "the engine said nothing"
	}
	for _, marker := range restoreCorruptMarkers {
		if strings.Contains(diagnosis, marker) {
			return protoErr("source_corrupt", false, "Solr rejected the backup: %s", msg)
		}
	}
	return protoErr("restore_failed", false, "restore failed: %s", msg)
}

// assertServing is the gate between "the API said 0" and "the drill has
// something to check". A restore can report success and leave a
// collection that never came up, and every check would then run against
// nothing.
func assertServing(ctx context.Context, c *core, collection string) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", liveScript, "bash", collection},
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1" {
		return nil
	}
	if val.ExitCode != 0 && len(stderr) > 0 {
		return protoErr("restore_failed", false, "ask Solr what it serves: %s", firstLine(stderr))
	}
	return protoErr("restore_failed", false,
		"the restore reported success but Solr does not serve collection %q%s",
		collection, servedSuffix(ctx, c))
}

// servedSuffix names what the server does serve, so the refusal above is
// actionable rather than merely negative.
func servedSuffix(ctx context.Context, c *core) string {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", servedScript}})
	if perr != nil || val.ExitCode != 0 {
		return ""
	}
	names := strings.Fields(strings.TrimSpace(string(stdout)))
	if len(names) == 0 {
		return " — it serves no collections at all"
	}
	return " — it serves: " + strings.Join(names, ", ")
}

// opHealthcheck proves the restored collection still answers a query.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := struct {
		State struct {
			Collection string `json:"collection"`
		} `json:"state"`
	}{}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	collection := req.State.Collection
	if !collectionPattern.MatchString(collection) {
		return nil, protoErr("invalid_request", false, "healthcheck state carries no collection")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", healthScript, "bash", collection},
	})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		return map[string]any{"healthy": false, "detail": firstLine(stderr)}, nil
	}
	if _, err := strconv.Atoi(strings.TrimSpace(string(stdout))); err != nil {
		return map[string]any{"healthy": false, "detail": "the collection did not answer a count"}, nil
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

// option reads an optional drill-config value.
func option(opts map[string]string, key, fallback string) string {
	if v, ok := opts[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}
