package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	adapterName    = "neo4j"
	adapterVersion = "0.1.0"

	// defaultDatabase is the database a Neo4j server serves unless its
	// image configures another one, and the only one Community Edition
	// can mount. It is both the restore target and the connection
	// database the checks run against.
	defaultDatabase = "neo4j"
	defaultPort     = 7687

	// connectionUser is the built-in administrative user every Neo4j
	// installation starts with; Community Edition has no other.
	connectionUser = "neo4j"

	// sandboxPassword is the password of the drill engine's neo4j user.
	// It is a documented CONSTANT, not a secret: Neo4j has no way to
	// authenticate a client without one, and the core's ephemeral
	// per-drill secret cannot be used — its value would have to cross the
	// protocol to reach the engine, which §2.5 forbids. So the adapter
	// sets this fixed value, the Neo4j equivalent of the mssql adapter's
	// sa constant and the postgres adapter's pg_hba trust overwrite:
	// publicly known access, confined to a sandbox with zero ingress
	// (--network none, no ports expressible). The credential never
	// protects anything reachable.
	sandboxPassword = "Probavi-DrillSandbox-0" //nolint:gosec // deliberately public constant, not a credential — see above

	// workDirName is the per-drill directory inside the sandbox's scratch
	// space that holds the artifact under the name the engine expects.
	workDirName = "probavi-neo4j"

	// readinessBudget bounds the wait for a started server to answer
	// queries. `neo4j start` returns once the bootloader is up and says
	// so ("There may be a short delay until the server is ready");
	// measured on the verified image, the first query succeeds about two
	// seconds later.
	readinessBudget = 3 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// databasePattern is Neo4j's own naming rule, applied before anything is
// transferred: 3 to 63 characters, beginning with a letter, then letters,
// digits, dots and dashes. Uppercase is refused rather than folded — the
// engine normalizes names to lowercase, and a drill whose evidence record
// names a database the server never had is worse than one that stops.
// The name reaches the engine as an argv element and as a file name, so
// nothing here is a quoting guard; it is there so an operator learns the
// rule from Probavi instead of from a failed restore.
var databasePattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{2,62}$`)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "neo4j"},
		"sources": []map[string]any{
			{"kind": "neo4j_dump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "neo4j_dump_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Neo4j has no SQL: the check text the core passes through
			// {{sql}} is one Cypher query (documented in the adapter
			// README), travelling as a single positional parameter. The
			// dialect — including cypher-shell's decorated output — is
			// absorbed here declaratively (see runnerScript), so the core
			// never learns it (§6.1). NEO4J_PASSWORD carries the
			// documented sandbox constant; see the sandboxPassword
			// comment.
			"argv": []string{"bash", "-c", runnerScript, "bash", "{{database}}", "{{sql}}"},
			"env": map[string]string{
				"NEO4J_USERNAME": "{{user}}",
				"NEO4J_PASSWORD": sandboxPassword,
			},
		},
		"verbs_required": []string{"exec", "put_file"},
	}
}

// clientEnv is the environment every cypher-shell call the adapter makes
// for itself runs with — the same identity the declared sql_runner uses.
func clientEnv() map[string]string {
	return map[string]string{
		"NEO4J_USERNAME": connectionUser,
		"NEO4J_PASSWORD": sandboxPassword,
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

// opProvision restores the dump into the already-running sandbox. The
// sandbox must start idle (docker: command: sleep infinity): a dump can
// only be loaded into a database no server has mounted, and the initial
// password only takes effect before the first start — so the adapter
// prepares the store and then starts the engine itself.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	database, workDir, perr := parseProvisionTarget(req)
	if perr != nil {
		return nil, perr
	}
	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes)

	var t timings
	prepared, perr := prepareEngine(ctx, c, workDir)
	if perr != nil {
		return nil, perr
	}
	t.engineReady += prepared

	dumpPath := workDir + "/" + database + ".dump"
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dumpPath, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}
	t.transfer = put.DurationSeconds

	infoSeconds, archive, perr := readArchive(ctx, c, database, workDir)
	if perr != nil {
		return nil, perr
	}
	t.restore += infoSeconds
	logger.Info("archive accepted", "database", archive.database, "format", archive.format, "restore_target", database)

	loadSeconds, perr := loadArchive(ctx, c, database, workDir)
	if perr != nil {
		return nil, perr
	}
	t.restore += loadSeconds

	started, perr := startEngine(ctx, c, database)
	if perr != nil {
		return nil, perr
	}
	t.engineReady += started
	logger.Info("restore complete", "restore_seconds", t.restore, "database", database)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "neo4j", "host": "127.0.0.1", "port": defaultPort,
			"database": database, "user": connectionUser,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": t.engineReady,
			"transfer_seconds":     t.transfer,
			"restore_seconds":      t.restore,
		},
		"state": map[string]any{"database": database, "work_dir": workDir},
	}, nil
}

// parseProvisionTarget validates everything the request supplies before
// any sandbox call: the target database, the declared backup zone (which
// this adapter cannot honour at all), and the work directory.
func parseProvisionTarget(req *provisionRequest) (database, workDir string, perr *protoError) {
	if req.PITR != nil {
		return "", "", protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	if perr := rejectBackupTimezone(req.Source.Params); perr != nil {
		return "", "", perr
	}
	database = option(req.Options, "database", defaultDatabase)
	if !databasePattern.MatchString(database) {
		return "", "", protoErr("invalid_request", false,
			"database name %s is not a Neo4j database name: 3 to 63 characters, "+
				"beginning with a lowercase letter, then lowercase letters, digits, dots and dashes",
			database)
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	return database, strings.TrimRight(scratch, "/") + "/" + workDirName, nil
}

// prepareEngine makes the sandbox a host Neo4j will start in, and leaves
// the server stopped: name resolution, the toolchain, the work
// directory, and the initial password — which only takes effect before
// the first start. Everything here is engine readiness work, so its cost
// belongs to engine_ready_seconds (§7).
func prepareEngine(ctx context.Context, c *core, workDir string) (float64, *protoError) {
	seconds := 0.0

	hosts, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", hostsScript}})
	if perr != nil {
		return 0, perr
	}
	if hosts.ExitCode != 0 {
		return 0, protoErr("engine_not_ready", false,
			"the sandbox could not be made to resolve its own hostname, which Neo4j requires to start: %s",
			firstLine(stderr))
	}
	seconds += hosts.DurationSeconds

	tools, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", toolScript}})
	if perr != nil {
		return 0, perr
	}
	if tools.ExitCode != 0 {
		return 0, protoErr("invalid_request", false,
			"the sandbox image does not carry neo4j, neo4j-admin and cypher-shell: "+
				"this adapter drives the engine's own tools inside the sandbox, so the image must be a Neo4j image")
	}
	seconds += tools.DurationSeconds

	mkdir, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mkdir", "-p", workDir}})
	if perr != nil {
		return 0, perr
	}
	if mkdir.ExitCode != 0 {
		return 0, protoErr("sandbox_error", true, "create work directory %s: %s", workDir, firstLine(stderr))
	}
	seconds += mkdir.DurationSeconds

	pw, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", passwordScript, "bash", sandboxPassword}})
	if perr != nil {
		return 0, perr
	}
	if pw.ExitCode != 0 {
		return 0, protoErr("engine_not_ready", false,
			"could not set the sandbox engine's initial password: %s — "+
				"the sandbox must start idle (docker: command: sleep infinity), "+
				"because the password only takes effect before the server's first start",
			firstLine(stderr))
	}
	seconds += pw.DurationSeconds

	return seconds, nil
}

// archiveInfo is what the engine says the artifact is, read before the
// restore rather than assumed from its file name.
type archiveInfo struct {
	database string
	format   string
}

// readArchive asks neo4j-admin to describe the artifact without loading
// it. A file the engine cannot read as an archive fails here, which is
// what distinguishes a corrupt backup from a restore that went wrong.
func readArchive(ctx context.Context, c *core, database, workDir string) (float64, *archiveInfo, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", infoScript, "bash", database, workDir}})
	if perr != nil {
		return 0, nil, perr
	}
	if val.ExitCode != 0 {
		return 0, nil, protoErr("source_corrupt", false,
			"the engine does not recognize the backup as a Neo4j dump: %s", verdictLine(stderr))
	}
	return val.DurationSeconds, parseArchiveInfo(stdout), nil
}

// parseArchiveInfo reads the `Key: value` lines neo4j-admin prints for an
// archive. Missing keys stay empty: the information is for the drill log,
// never a gate.
func parseArchiveInfo(stdout []byte) *archiveInfo {
	info := &archiveInfo{}
	for _, line := range strings.Split(string(stdout), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "."))
		switch strings.ToLower(key) {
		case "database":
			info.database = value
		case "format":
			info.format = value
		}
	}
	return info
}

// loadArchive replays the dump into the unmounted database.
func loadArchive(ctx context.Context, c *core, database, workDir string) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", loadScript, "bash", database, workDir}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, mapLoadFailure(stderr)
	}
	return val.DurationSeconds, nil
}

// startEngine starts the server and waits until it serves the restored
// database.
//
// The wait and the gate are one question, asked repeatedly: while the
// server is not answering the query fails, and once it answers the query
// says whether the database is online. That is why a slow mount and a
// missing database do not look alike here — the first keeps polling, the
// second is recognized the moment the engine can be asked (see
// awaitServing).
func startEngine(ctx context.Context, c *core, database string) (float64, *protoError) {
	start := time.Now()
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", startScript}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("engine_not_ready", true, "the engine did not start: %s", firstLine(stderr))
	}
	if perr := awaitServing(ctx, c, database, start); perr != nil {
		return 0, perr
	}
	return time.Since(start).Seconds(), nil
}

// awaitServing polls until the engine serves the restored database, and
// refuses to call the restore successful otherwise.
//
// Without this a drill can be green and prove nothing: a dump loaded
// under a name Community Edition cannot mount lands on disk, the server
// starts, and every check runs against whatever database the connection
// resolves to instead (measured — see onlineScript). So the first
// negative answer is not simply retried: the engine is asked what it
// does serve, and only a database on its way up is waited for. A
// database it has never heard of, or one it has given up on, fails the
// drill then and there rather than at the end of the readiness budget.
func awaitServing(ctx context.Context, c *core, database string, start time.Time) *protoError {
	for {
		answered, online, perr := databaseOnline(ctx, c, database)
		if perr != nil {
			return perr
		}
		switch {
		case online:
			return nil
		case answered:
			// The engine is up and says no. Whether that is a verdict or
			// a moment in a startup depends on what it says next.
			served, perr := servedDatabases(ctx, c)
			if perr != nil {
				return perr
			}
			state, mounted := served[database]
			if !mounted {
				return protoErr("restore_failed", false,
					"the engine does not serve the restored database %s%s: a dump is only proven once "+
						"the server mounts it, and Community Edition mounts only the database its "+
						"configuration names", database, servedSuffix(served))
			}
			if !transientState(state) {
				return protoErr("restore_failed", false,
					"the restored database %s is %s rather than online: the server mounted it and "+
						"stopped there, so nothing that ran against it would prove the backup",
					database, state)
			}
		}
		if time.Since(start) > readinessBudget {
			return protoErr("engine_not_ready", true,
				"the engine did not serve the restored database %s within %s of starting",
				database, readinessBudget)
		}
		select {
		case <-ctx.Done():
			return protoErr("cancelled", true, "cancelled while waiting for the engine to serve the restored database")
		case <-time.After(readinessPoll):
		}
	}
}

// transientState reports whether a database's reported state is one it is
// expected to leave on its own. Everything else — offline, quarantined, a
// state a later engine invents — is a verdict, because waiting out the
// readiness budget for it would replace an accurate message with a
// timeout.
func transientState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "starting", "initial", "store copying":
		return true
	}
	return false
}

// databaseOnline asks the gate's question. answered separates "the server
// is not up yet" from "the server says no".
func databaseOnline(ctx context.Context, c *core, database string) (answered, online bool, perr *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv:           []string{"bash", "-c", onlineScript, "bash", database},
		Env:            clientEnv(),
		TimeoutSeconds: 30,
	})
	if perr != nil {
		return false, false, perr
	}
	if val.ExitCode != 0 {
		return false, false, nil
	}
	return true, strings.TrimSpace(string(stdout)) == "1", nil
}

// servedDatabases reads what the engine does serve, for a refusal that
// has to be actionable.
func servedDatabases(ctx context.Context, c *core) (map[string]string, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", servedScript}, Env: clientEnv(), TimeoutSeconds: 30})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		return nil, protoErr("restore_failed", false,
			"the engine would not say which databases it serves: %s", firstLine(stderr))
	}
	return parseDatabaseStatuses(stdout), nil
}

// servedSuffix renders the listing for a refusal message.
func servedSuffix(served map[string]string) string {
	if len(served) == 0 {
		return ""
	}
	parts := make([]string, 0, len(served))
	for _, name := range sortedNames(served) {
		parts = append(parts, name+" ("+served[name]+")")
	}
	return " (it serves " + strings.Join(parts, ", ") + ")"
}

// parseDatabaseStatuses reads the `name, status` lines servedScript
// prints.
func parseDatabaseStatuses(stdout []byte) map[string]string {
	served := map[string]string{}
	for _, line := range strings.Split(string(stdout), "\n") {
		name, status, found := strings.Cut(strings.TrimSpace(line), ",")
		if !found {
			continue
		}
		served[strings.TrimSpace(name)] = strings.TrimSpace(status)
	}
	return served
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the provisioned instance serves queries (§6.3).
// An unhealthy engine is a valid result, not an operation error. The
// query runs against the restored database itself, so a server that came
// up without mounting it answers unhealthy rather than fine.
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
		Argv:           []string{"bash", "-c", healthScript, "bash", database},
		Env:            clientEnv(),
		TimeoutSeconds: 30,
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1"
	detail := "answering queries on " + database
	if !healthy {
		detail = fmt.Sprintf("cypher-shell exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// mapLoadFailure classifies a failed load into a protocol error code.
// Input the engine rejects as an archive is a corrupt source; anything
// else ran and failed for engine reasons (§5).
func mapLoadFailure(stderr []byte) *protoError {
	line := verdictLine(stderr)
	for _, marker := range []string{
		"Not a valid Neo4j archive", "Not a valid archive", "ZstdIOException", "Truncated source",
	} {
		if strings.Contains(line, marker) {
			return protoErr("source_corrupt", false, "the engine rejected the dump: %s", line)
		}
	}
	return protoErr("restore_failed", false, "neo4j-admin could not load the dump: %s", line)
}

// verdictLine extracts the failure verdict from neo4j-admin's stderr.
// The stream carries the restore's progress as well — a line per file and
// per percent — so the verdict is the first line that reports a failure,
// and the last non-progress line when the tool phrased it some other way.
// The result crosses the protocol as a JSON string and lands in evidence
// error fields: keep it single-line and quote-free.
func verdictLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	fallback := ""
	for _, raw := range lines {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "Files:") || strings.HasPrefix(s, "Done:") {
			continue
		}
		if strings.HasPrefix(s, "Failed to") {
			return sanitize(s)
		}
		fallback = s
	}
	return sanitize(fallback)
}

// firstLine reduces captured output to its first line, quote-free.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return sanitize(s)
}

// sanitize keeps engine text safe for a JSON protocol message and for an
// evidence error field, and removes the sandbox constant should the
// engine ever echo it back.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, sandboxPassword, "[redacted]")
	return strings.ReplaceAll(s, `"`, "'")
}

func option(opts map[string]string, key, fallback string) string {
	if v, ok := opts[key]; ok && v != "" {
		return v
	}
	return fallback
}

// sortedNames lists the databases the engine reported, for a diagnostic
// that has to read the same on every run.
func sortedNames(served map[string]string) []string {
	names := make([]string, 0, len(served))
	for name := range served {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
