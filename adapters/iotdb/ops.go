package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "iotdb"
	adapterVersion = "0.1.0"

	// defaultUser is the user checks connect as when the drill names none:
	// IoTDB's own administrator, the one user that can read everything.
	defaultUser = "root"

	// readinessBudget bounds waiting for the restored engine to serve. Both
	// nodes start in about six seconds on the fixtures (measured on 2.0.11
	// and 1.3.7); a copy with many regions recovers each one first.
	readinessBudget = 5 * time.Minute
	readinessPoll   = 500 * time.Millisecond

	// verdictTimeout bounds the full read, which decodes every value.
	verdictTimeout = 3600
)

// knownOptions are the drill config options this adapter reads.
var knownOptions = map[string]bool{"user": true, "password_env": true, "database": true}

// sandboxPaths is everything this adapter puts inside the sandbox, composed
// from sandbox.scratch_dir — the only writable directory an adapter may rely
// on (§6.2). The engine's own install directory is read, never written: the
// configuration it starts on is a copy under root.
type sandboxPaths struct {
	// root holds everything below, and is what the prepare step clears.
	root string
	// data is the restored data directory the engine is pointed at, never
	// the staging copy, so a half-finished extraction is never served.
	data string
	// staging is where a transferred data directory lands; the transfer
	// creates it.
	staging string
	// archive is where a transferred archive lands, and unpack where it is
	// extracted.
	archive string
	unpack  string
}

// newSandboxPaths derives the set from the scratch directory. An empty
// scratch_dir falls back to /tmp, which every sandbox has.
func newSandboxPaths(scratch string) sandboxPaths {
	if scratch == "" {
		scratch = "/tmp"
	}
	root := scratch + "/probavi-iotdb"
	return sandboxPaths{
		root:    root,
		data:    root + "/home/data",
		staging: root + "/staging",
		archive: root + "/backup.tar",
		unpack:  root + "/unpack",
	}
}

// probePayload reports identity and capabilities (§6.1).
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "iotdb"},
		"sources": []map[string]any{
			// The archive kind leads because the §10 conformance suite
			// provisions its first declared kind from random bytes: an
			// archive defers to tar and then to the engine, so accepting the
			// bytes host-side and failing in the sandbox is the honest order
			// for it anyway (the chroma precedent).
			{"kind": kindDataTar, "capabilities": map[string]bool{"pitr": false}},
			{"kind": kindData, "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// IoTDB speaks two dialects on one server. {{database}} says
			// which: empty is the tree dialect, which has no current
			// database, and a name is the table dialect run in that
			// database. The password reaches the script in its environment
			// and curl on its standard input, never an argument list.
			"argv": []string{"bash", "-c", runnerScript, "bash", "{{database}}", "{{user}}", "{{sql}}"},
			"env":  map[string]string{passwordEnv: "{{password}}"},
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
	PITR    json.RawMessage   `json:"pitr"`
}

// login is who the restored engine is read as.
type login struct {
	user string
	// passwordEnv names the variable holding the password, "" for IoTDB's
	// default; password is its value.
	passwordEnv string
	password    string
	// database is the table-model database checks run in, "" for the tree
	// dialect.
	database string
}

// env is the environment every script that reads the engine runs with.
func (l login) env() map[string]string {
	env := map[string]string{"IOTDB_USER": l.user}
	if l.password != "" {
		env[passwordEnv] = l.password
	}
	if l.database != "" {
		env["IOTDB_DATABASE"] = l.database
	}
	return env
}

// opProvision restores the copy and refuses anything it cannot prove
// serves (§6.2).
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "provision payload is not valid JSON")
	}
	if len(req.PITR) > 0 && string(req.PITR) != "null" {
		return nil, protoErr("invalid_request", false,
			"this adapter declares no point-in-time recovery: an offline copy is one instant")
	}
	who, perr := resolveLogin(req.Options, req.Source.Params, req.Source.CredentialEnv)
	if perr != nil {
		return nil, perr
	}
	src, perr := inspect(req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	if perr := assertSettled(ctx, src.path, settleWindow); perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "kind", src.kind, "path", src.path,
		"size_bytes", src.sizeBytes, "gzip", src.gzip)

	paths := newSandboxPaths(req.Sandbox.ScratchDir)
	if perr := prepareStep.run(ctx, c, 60, paths.root); perr != nil {
		return nil, perr
	}
	transferStart := time.Now()
	dest := paths.staging
	if src.kind == kindDataTar {
		dest = paths.archive
	}
	if _, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dest, Mode: "0700"}); perr != nil {
		return nil, perr
	}
	transferSeconds := time.Since(transferStart).Seconds()

	restoreStart := time.Now()
	if perr := placeData(ctx, c, src, paths); perr != nil {
		return nil, perr
	}
	if perr := configure(ctx, c, paths); perr != nil {
		return nil, perr
	}
	restoreSeconds := time.Since(restoreStart).Seconds()

	readySeconds, perr := startEngine(ctx, c, paths, who)
	if perr != nil {
		return nil, perr
	}
	values, perr := verifyRestore(ctx, c, who)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore verified", "values_read", values)

	connection := map[string]any{
		"scheme": "http", "host": "127.0.0.1", "port": atoi(restPort),
		"database": who.database, "user": who.user,
	}
	if who.passwordEnv != "" {
		connection["password_env"] = who.passwordEnv
	}
	return map[string]any{
		"connection": connection,
		"source_identity": map[string]any{
			"checksum":   src.checksum,
			"size_bytes": src.sizeBytes,
			// Nothing in an offline copy dates the backup. A data file's name
			// records when that file was created, which is not when the copy
			// was taken, and a file's mtime dates the copy rather than the
			// data — so this is null rather than a number that would read as
			// a fact (the chroma precedent).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"data_dir": paths.data, "values_read": values},
	}, nil
}

// resolveLogin reads who the restored engine is read as.
//
// An offline copy keeps the users and passwords of the node it was taken
// from: root/root is refused by a copy whose root password was changed
// (801, measured), so the drill names the password the copy holds. It
// travels as a variable name through the protocol and is resolved by the
// core (§2.5, §6.1).
func resolveLogin(options, params map[string]string, declared []string) (login, *protoError) {
	for name := range params {
		return login{}, protoErr("invalid_request", false,
			"source.params.%s has no effect for this adapter: an offline copy carries everything the "+
				"restore needs, and this adapter declares no source parameters", name)
	}
	for name := range options {
		if !knownOptions[name] {
			return login{}, protoErr("invalid_request", false,
				"options.%s has no effect for this adapter; it reads user, password_env and database", name)
		}
	}
	who := login{user: option(options, "user", defaultUser), database: option(options, "database", "")}
	name := option(options, "password_env", "")
	if name == "" {
		return who, nil
	}
	listed := false
	for _, d := range declared {
		listed = listed || d == name
	}
	if !listed {
		return login{}, protoErr("invalid_request", false,
			"options.password_env names %s, but the drill's source.credential_env does not list it: the "+
				"core passes through only the variables a drill declares, so the password would be "+
				"silently empty", name)
	}
	who.passwordEnv, who.password = name, os.Getenv(name)
	if who.password == "" {
		return login{}, protoErr("invalid_request", false,
			"options.password_env names %s, which is empty in the drill's environment", name)
	}
	return who, nil
}

// option reads a drill config option, falling back to a default.
func option(options map[string]string, key, fallback string) string {
	if v := strings.TrimSpace(options[key]); v != "" {
		return v
	}
	return fallback
}

// step is one script that has no verdict beyond succeeding, and the error
// its failure is.
type step struct {
	what      string
	script    string
	code      string
	retryable bool
}

var (
	// prepareStep failing is the sandbox's fault, not the backup's.
	prepareStep = step{"prepare the sandbox", prepareScript, "sandbox_error", true}
	// hostsStep failing means the sandbox cannot name the copy's node.
	hostsStep = step{"map the node's host names to loopback", hostsScript, "restore_failed", false}
	// configureStep failing means the engine's configuration could not be
	// copied or written.
	configureStep = step{"write the sandbox's configuration", configureScript, "sandbox_error", true}
)

// run runs the step's script.
func (s step) run(ctx context.Context, c *core, timeout float64, args ...string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: append([]string{"bash", "-c", s.script, "bash"}, args...), TimeoutSeconds: timeout,
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr(s.code, s.retryable, "%s: %s", s.what, firstLine(stderr))
	}
	return nil
}

// placeData turns whatever arrived in staging into the data directory the
// engine will be pointed at.
func placeData(ctx context.Context, c *core, src *source, paths sandboxPaths) *protoError {
	script, args := placeDirScript, []string{paths.data, paths.staging}
	if src.kind == kindDataTar {
		script, args = placeTarScript(src.gzip), []string{paths.data, paths.unpack, paths.archive}
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: append([]string{"bash", "-c", script, "bash"}, args...), TimeoutSeconds: 3600,
	})
	if perr != nil {
		return perr
	}
	switch val.ExitCode {
	case 0:
		return nil
	case exitNoData:
		if src.kind == kindDataTar {
			return protoErr("source_corrupt", false,
				"the archive holds no IoTDB data directory: nothing inside it carries both %s and %s",
				confignodeProperties, datanodeProperties)
		}
		return protoErr("source_corrupt", false,
			"the transferred directory holds no IoTDB data directory inside the sandbox, though it did on "+
				"the drill host — the copy did not arrive whole")
	case exitAmbiguous:
		return protoErr("source_corrupt", false,
			"the archive holds more than one IoTDB data directory, and a drill restores one node")
	case exitUnpack:
		return protoErr("source_corrupt", false, "the archive could not be unpacked: %s", firstLine(stderr))
	default:
		return protoErr("restore_failed", false, "place the restored data: %s", firstLine(stderr))
	}
}

// configure reads what the copy records about its node, refuses the copies
// a sandbox cannot start, and writes the sandbox's configuration.
func configure(ctx context.Context, c *core, paths sandboxPaths) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", propertiesScript, "bash", paths.data}, TimeoutSeconds: 60,
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("source_corrupt", false, "read the copy's node properties: %s", firstLine(stderr))
	}
	props := parseProperties(stdout)
	var settings, hosts []string
	if props.spoke() {
		if perr := refuseNewerCopy(props); perr != nil {
			return perr
		}
		settings, hosts, perr = nodeSettings(props)
		if perr != nil {
			return perr
		}
	}
	if len(hosts) > 0 {
		if perr := hostsStep.run(ctx, c, 30, hosts...); perr != nil {
			return perr
		}
	}
	home := props["engine.home"]
	if home == "" {
		home = "/iotdb"
	}
	args := append([]string{paths.root, home, paths.data}, settings...)
	return configureStep.run(ctx, c, 120, args...)
}

// startEngine starts both nodes on the restored copy and waits until every
// region serves.
func startEngine(ctx context.Context, c *core, paths sandboxPaths, who login) (float64, *protoError) {
	start := time.Now()
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", startScript, "bash", paths.root}, TimeoutSeconds: 60,
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("engine_not_ready", false, "start IoTDB: %s", firstLine(stderr))
	}
	for {
		ready, _, _, perr := c.exec(ctx, execArgs{
			Argv: []string{"bash", "-c", readyScript, "bash"}, Env: who.env(), TimeoutSeconds: 60,
		})
		if perr != nil {
			return 0, perr
		}
		switch ready.ExitCode {
		case 0:
			return time.Since(start).Seconds(), nil
		case exitLoginRefused:
			return 0, loginRefused(who)
		}
		if time.Since(start) > readinessBudget {
			return 0, protoErr("engine_not_ready", false,
				"IoTDB did not serve every region within %s of starting on the restored copy%s",
				readinessBudget, startupDiagnosis(ctx, c, paths.root))
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for the engine to start")
		case <-time.After(readinessPoll):
		}
	}
}

// loginRefused names the refusal and what to do about it.
func loginRefused(who login) *protoError {
	if who.passwordEnv == "" {
		return protoErr("invalid_request", false,
			"the restored engine refused user %s with IoTDB's default password: an offline copy keeps "+
				"the users and passwords of the node it was taken from, so name that password with "+
				"options.password_env", who.user)
	}
	return protoErr("invalid_request", false,
		"the restored engine refused user %s with the password in %s: an offline copy keeps the users and "+
			"passwords of the node it was taken from, and that is not the one this copy holds",
		who.user, who.passwordEnv)
}

// verifyRestore is the verdict (verdictScript says why it reads every value
// rather than counting).
func verifyRestore(ctx context.Context, c *core, who login) (int64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", verdictScript, "bash"}, Env: who.env(), TimeoutSeconds: verdictTimeout,
	})
	if perr != nil {
		return 0, perr
	}
	said := firstLine(stderr)
	switch val.ExitCode {
	case 0:
		return atoi(string(stdout)), nil
	case exitRestoredNothing:
		return 0, protoErr("restore_failed", false,
			"the restored copy holds no value in any series or table: a backup that restores to nothing is "+
				"a backup of nothing, whatever the engine reports")
	case exitUndecodable:
		return 0, protoErr("source_corrupt", false,
			"the engine could not read every value of the restored copy (%s). Its counts are answered from "+
				"statistics and still look whole in this state, so the full read is the proof", said)
	case exitTTLHidesAll:
		return 0, ttlHidesAll(said)
	case exitReadDisagrees:
		return 0, protoErr("source_corrupt", false,
			"reading every value of %s counted other than the engine's own statistics for it", said)
	case exitNoSuchDatabase:
		held := strings.TrimSpace(string(stderr))
		if held == "" {
			held = "none — a copy written before IoTDB 2.0 has no table model"
		}
		return 0, protoErr("invalid_request", false,
			"options.database names %s, which the restored copy does not hold as a table-model database "+
				"(it holds: %s)", who.database, held)
	case exitLoginRefused:
		return 0, loginRefused(who)
	default:
		return 0, protoErr("restore_failed", false, "reading the restored copy failed: %s", said)
	}
}

// ttlHidesAll refuses a copy whose TTL hides every row of a scope.
//
// TTL travels in the copy and is enforced when a query runs, and nothing
// suspends it: ttl_check_interval schedules only the physical deletion, and
// unsetting a TTL would rewrite a policy a check is entitled to read. So a
// drill of a copy older than a TTL it carries proves nothing about that
// scope — every row is hidden before the first read — and it is refused
// with both numbers rather than reported green on the scopes that remain.
func ttlHidesAll(said string) *protoError {
	fields := strings.Split(said, "\t")
	if len(fields) != 3 {
		return protoErr("restore_failed", false,
			"a TTL the restored copy carries hides every row it covers: %s", said)
	}
	ms, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return protoErr("restore_failed", false,
			"a TTL the restored copy carries hides every row it covers: %s", said)
	}
	return protoErr("restore_failed", false,
		"%s %s holds data and reads no row: the copy carries a TTL of %s, which IoTDB enforces when a "+
			"query runs, so every row it covers is hidden before a check can read it. Drill a backup "+
			"younger than the TTL, or widen it on the source",
		fields[0], fields[1], time.Duration(ms)*time.Millisecond)
}

// startupDiagnosis returns the engine's last logged error as a suffix, or
// nothing when it logged none.
func startupDiagnosis(ctx context.Context, c *core, root string) string {
	_, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", startupErrorScript, "bash", root}, TimeoutSeconds: 15,
	})
	if perr != nil {
		return ""
	}
	said := strings.TrimSpace(string(stdout))
	if said == "" {
		return ""
	}
	return " (the engine said: " + strings.ReplaceAll(said, "\n", " | ") + ")"
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		User        string `json:"user"`
		PasswordEnv string `json:"password_env"`
		Database    string `json:"database"`
	} `json:"connection"`
}

// opHealthcheck reports whether the engine still answers an authenticated
// query (§6.3).
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "healthcheck payload is not valid JSON")
	}
	who := login{user: req.Connection.User, passwordEnv: req.Connection.PasswordEnv}
	if who.user == "" {
		who.user = defaultUser
	}
	if who.passwordEnv != "" {
		who.password = os.Getenv(who.passwordEnv)
	}
	start := time.Now()
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", healthScript, "bash"}, Env: who.env(), TimeoutSeconds: 30,
	})
	if perr != nil {
		return nil, perr
	}
	latency := time.Since(start).Seconds()
	if val.ExitCode != 0 {
		return map[string]any{"healthy": false, "latency_seconds": latency, "detail": firstLine(stderr)}, nil
	}
	return map[string]any{"healthy": true, "latency_seconds": latency, "detail": "answered an authenticated query"}, nil
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

// atoi reads a non-negative integer, returning 0 for anything else.
func atoi(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
