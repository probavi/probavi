package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "arangodb"
	adapterVersion = "0.1.0"

	// workDirName is created under the provider's scratch directory.
	workDirName = "probavi-arangodb"
	// tarName is where the archive kind places the artifact before
	// unpacking it.
	tarName = "dump.tar"
	// runnerFile is the check runner the declared sql_runner executes.
	// Its path is fixed rather than derived from the scratch directory:
	// the probe declares the runner and must not touch the sandbox, so it
	// cannot know where the scratch directory will be. Provision writes
	// the file here, and the provider destroys it with the sandbox.
	runnerFile = "/tmp/probavi-arangodb-runner.js"

	// The endpoint the adapter starts the server on and every later step
	// addresses. Loopback and unauthenticated, which is acceptable only
	// because a Probavi sandbox is zero-ingress.
	endpoint    = "tcp://127.0.0.1:8529"
	defaultHost = "127.0.0.1"
	defaultPort = 8529

	dataDirPath = "/var/lib/arangodb3"
	appsDirPath = "/var/lib/arangodb3-apps"

	readinessBudget = 3 * time.Minute
	readinessPoll   = 2 * time.Second
)

// runnerJS is the check runner, written into the sandbox during
// provision and executed by the declared sql_runner.
//
// It is a file rather than a command-line string, and the statement
// reaches it through the environment rather than as an option value, for
// a measured reason: **this engine's option parser collapses `@@` to `@`
// in an option value**, because `@file` is its own syntax for reading a
// value from a file. An AQL collection bind parameter is written `@@coll`,
// so a query carrying one cannot survive being passed as
// `--javascript.execute-string` — it arrives mangled. The failure
// observed was a syntax error, which is the lucky case; a query mangled
// into something that still parses is what this arrangement rules out.
//
// The statement is still its own argv element, as §6.1 requires: the
// shell wrapper below takes it as a positional parameter and exports it,
// so it never becomes an option value on the way.
//
// What it does is absorb the dialect declaratively, so the core's
// generating built-ins apply unchanged. The three statements the core
// composes are recognised whole and answered in the engine's own terms:
//
//	SELECT count(*) FROM "c" WHERE 1=0  →  the collection exists, or throw
//	SELECT count(*) FROM "c"            →  its document count
//	SELECT max("f") FROM "c"            →  the largest value of that field
//
// table_exists prints nothing at all, because a probe for a collection's
// existence should not put a document of restored production data on
// stdout. Anything that is not one of the three reaches the engine as
// written — the operator's own AQL, which is what a check is.
const runnerJS = `var stmt = (require("internal").env.PROBAVI_AQL || "").trim();
var ident = /^[A-Za-z_][A-Za-z0-9_-]*$/;
var m;
if ((m = stmt.match(/^SELECT count\(\*\) FROM "([^"]+)"( WHERE 1=0)?$/))) {
  var c = db._collection(m[1]);
  if (c === null) { throw new Error("no such collection: " + m[1]); }
  if (!m[2]) { print(c.count()); }
} else if ((m = stmt.match(/^SELECT max\("([^"]+)"\) FROM "([^"]+)"$/))) {
  if (!ident.test(m[2])) { throw new Error("collection name not usable: " + m[2]); }
  if (db._collection(m[2]) === null) { throw new Error("no such collection: " + m[2]); }
  var r = db._query("RETURN MAX(FOR d IN " + m[2] + " RETURN d[@f])", {f: m[1]}).toArray();
  if (r.length && r[0] !== null && r[0] !== undefined) { print(r[0]); }
} else {
  var rows = db._query(stmt).toArray();
  for (var i = 0; i < rows.length; i++) {
    print(typeof rows[i] === "object" && rows[i] !== null ? JSON.stringify(rows[i]) : rows[i]);
  }
}
`

// runnerScript is the declared runner's argv: it takes the database and
// the statement as positional parameters, puts the statement in the
// environment, and hands the work to the JavaScript above.
const runnerScript = `set -u
PROBAVI_AQL=$2
export PROBAVI_AQL
exec arangosh --quiet --server.endpoint ` + endpoint + ` --server.authentication false \
  --server.database "$1" --javascript.execute ` + runnerFile

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "arangodb"},
		"sources": []map[string]any{
			{"kind": "arangodb_dump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "arangodb_dump_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "arangodb_dump_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// {{database}} resolves to the database the dump names, which
			// provision returns as connection.database; {{sql}} is its own
			// argv element (§6.1) and the wrapper exports it rather than
			// letting it become an option value — see runnerJS.
			"argv": []string{"sh", "-c", runnerScript, "sh", "{{database}}", "{{sql}}"},
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

// opProvision restores the dump into the idle sandbox: preflight,
// transfer, a background start with the engine's own expiry thread turned
// off (see retention.go), readiness, the restore, and then a count of
// every collection — because the restore tool reports success for a dump
// that restored nothing (measured).
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req, scratch, perr := parseProvisionRequest(payload)
	if perr != nil {
		return nil, perr
	}

	src, perr := resolveSource(req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"database", src.census.database, "collections", len(src.census.collections))

	if perr := checkEngine(ctx, c); perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	logPath := path.Join(workDir, "arangod.log")
	transferSeconds, unpackSeconds, dumpDir, perr := transferArtifact(ctx, c, src, workDir)
	if perr != nil {
		return nil, perr
	}
	if perr := writeRunner(ctx, c); perr != nil {
		return nil, perr
	}

	readySeconds, perr := startEngine(ctx, c, logPath)
	if perr != nil {
		return nil, perr
	}

	database := src.census.database
	if database == "" {
		database, perr = databaseFromDump(ctx, c, dumpDir)
		if perr != nil {
			return nil, perr
		}
	}
	restoreSeconds, perr := restoreDump(ctx, c, dumpDir, database)
	if perr != nil {
		return nil, perr
	}
	countSeconds, collections, perr := countCollections(ctx, c, database)
	if perr != nil {
		return nil, perr
	}
	if collections == 0 {
		return nil, protoErr("source_corrupt", false,
			"the restore reported success and the database holds no collection: the dump "+
				"carried nothing to restore, and the tool reports that as success (measured)")
	}
	logger.Info("dump restored and read back",
		"database", database, "collections", collections, "ready_seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "arangodb", "host": defaultHost, "port": defaultPort,
			// The database the dump names, which the declared runner's
			// --server.database consumes.
			"database": database, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// The instant the dump's own dump.json states, already UTC.
			"created_at": formatCreatedAt(src.census.createdMs),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      unpackSeconds + restoreSeconds + countSeconds,
		},
		"state": map[string]any{"work_dir": workDir, "dump_dir": dumpDir, "database": database},
	}, nil
}

// parseProvisionRequest validates the §6.2 payload and resolves the
// scratch directory.
func parseProvisionRequest(payload json.RawMessage) (*provisionRequest, string, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, "", protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, "", protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	if perr := rejectBackupTimezone(req.Source.Params); perr != nil {
		return nil, "", perr
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	return req, scratch, nil
}

// checkEngine verifies the image carries the toolchain every later step
// runs on. The scripts are `sh` rather than `bash` throughout, because
// this image is Alpine-based and ships no bash (measured).
func checkEngine(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c",
		"command -v arangod >/dev/null && command -v arangorestore >/dev/null && " +
			"exec arangosh --version >/dev/null"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image lacks the ArangoDB toolchain (arangod, arangorestore, arangosh "+
				"over sh): use an official arangodb/arangodb image with command: sleep infinity (%s)",
			firstLine(stderr))
	}
	return nil
}

// writeRunner places the check runner in the sandbox. It is written with
// a here-document rather than put_file: the script is the adapter's own
// text, not a file on the drill host, and put_file may only carry paths
// belonging to the drill's backup source (§4.2).
func writeRunner(ctx context.Context, c *core) *protoError {
	script := "cat > " + runnerFile + " <<'PROBAVI_EOF'\n" + runnerJS + "PROBAVI_EOF\n"
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", script}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "write check runner: %s", firstLine(stderr))
	}
	return nil
}

// startEngine launches the server and waits until it answers — readiness
// in the §7 sense.
func startEngine(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	argv := append([]string{"arangod"}, engineArgs()...)
	script := "exec " + strings.Join(argv, " ") + " > " + logPath + " 2>&1"
	start, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c",
		"nohup sh -c " + shellQuote(script) + " >/dev/null 2>&1 &"}})
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the server failed to launch: %s", firstLine(stderr))
	}
	readySeconds, perr := awaitReady(ctx, c, logPath)
	if perr != nil {
		return 0, perr
	}
	return start.DurationSeconds + readySeconds, nil
}

// shellQuote renders one argument for sh -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// awaitReady polls until the server answers a real query.
//
// The probe is a server round-trip rather than "arangosh started", and
// that distinction is measured rather than assumed: `arangosh
// --javascript.execute-string "print(1)"` exits 0 with no server running
// at all, because the statement needs no connection. A readiness check
// written that way reports ready before anything is.
func awaitReady(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	begin := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           versionArgv(),
			TimeoutSeconds: 15,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(begin).Seconds(), nil
		}
		// Between polls, watch the server's own log for a startup
		// failure, so a server that died instantly fails the drill in
		// seconds with its own reason instead of burning the budget.
		if fatal, _, _, ferr := c.exec(ctx, execArgs{Argv: []string{
			"grep", "-qE", "FATAL|cannot open|unable to start", logPath}}); ferr == nil &&
			fatal.ExitCode == 0 {
			return 0, describeStartFailure(ctx, c, logPath)
		}
		if time.Since(begin) > readinessBudget {
			return 0, describeStartFailure(ctx, c, logPath)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// versionArgv asks the server for its version — a query that needs a
// connection, which is the point.
func versionArgv() []string {
	return []string{"arangosh", "--quiet", "--server.endpoint", endpoint,
		"--server.authentication", "false",
		"--javascript.execute-string", "db._version()"}
}

// describeStartFailure enriches a readiness timeout with the server's own
// last error line.
func describeStartFailure(ctx context.Context, c *core, logPath string) *protoError {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"tail", "-n", "30", logPath}})
	if perr != nil || val.ExitCode != 0 {
		return protoErr("engine_not_ready", true,
			"the server did not answer within %s", readinessBudget)
	}
	line := lastErrorLine(stdout)
	if line == "" {
		return protoErr("engine_not_ready", true,
			"the server did not answer within %s", readinessBudget)
	}
	return protoErr("restore_failed", false, "the server failed to start: %s", line)
}

// lastErrorLine picks the last log line that reads like the server's own
// failure report.
func lastErrorLine(log []byte) string {
	found := ""
	for _, line := range strings.Split(string(log), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") {
			found = strings.TrimSpace(line)
		}
	}
	return strings.ReplaceAll(found, `"`, "'")
}

// databaseFromDump reads the database name out of an unpacked dump when
// the host could not walk the archive.
func databaseFromDump(ctx context.Context, c *core, dumpDir string) (string, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c",
		`sed -n 's/.*"database"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1/` + manifestName + `" | head -1`,
		"sh", dumpDir}})
	if perr != nil {
		return "", perr
	}
	name := strings.TrimSpace(firstLine(stdout))
	if val.ExitCode != 0 || name == "" {
		return "", protoErr("source_corrupt", false,
			"the unpacked dump states no database in %s — not an arangodump output", manifestName)
	}
	if perr := judgeName("database", name); perr != nil {
		return "", perr
	}
	return name, nil
}

// restoreDump runs the restore. The tool's own stderr is deliberately not
// carried into the error message: on a damaged dump it quotes the request
// payload, which is hundreds of restored production documents (measured),
// and an evidence record must be shareable as it stands (§8 of the
// evidence schema). What the drill reports is the exit code and the
// adapter's own words.
func restoreDump(ctx context.Context, c *core, dumpDir, database string) (float64, *protoError) {
	val, _, _, perr := c.exec(ctx, execArgs{Argv: []string{
		"arangorestore",
		"--server.endpoint", endpoint,
		"--server.authentication", "false",
		"--server.database", database,
		"--create-database", "true",
		"--input-directory", dumpDir,
	}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"the engine refused the dump for database %s (arangorestore exited %d): a truncated "+
				"or altered collection file fails here. The tool's own message names the document "+
				"it stopped at and is kept out of this record deliberately — it quotes restored "+
				"rows; it is in the drill host's log",
			database, val.ExitCode)
	}
	return val.DurationSeconds, nil
}

// countCollections reads back what the restore produced. The count is the
// verdict, because arangorestore reports success for an empty dump
// directory: "Processed 0 collection(s)", exit 0 (measured).
func countCollections(ctx context.Context, c *core, database string) (float64, int, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"arangosh", "--quiet", "--server.endpoint", endpoint,
		"--server.authentication", "false", "--server.database", database,
		"--javascript.execute-string",
		`print(db._collections().filter(function (c) { return c.name()[0] !== "_"; }).length)`,
	}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, 0, protoErr("restore_failed", false,
			"reading back database %s failed: %s", database, firstLine(stderr))
	}
	n, err := strconv.Atoi(strings.TrimSpace(firstLine(stdout)))
	if err != nil || n < 0 {
		return 0, 0, protoErr("restore_failed", false,
			"the restored database did not report a collection count")
	}
	return val.DurationSeconds, n, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored server still answers (§6.3). An
// unhealthy server is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: versionArgv()})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := "server answers"
	if !healthy {
		detail = fmt.Sprintf("arangosh exited %d: %s", val.ExitCode, firstLine(stderr))
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
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
