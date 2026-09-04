package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"
)

const (
	adapterName    = "h2"
	adapterVersion = "0.1.1"

	// jarPath is where the sandbox image keeps the H2 jar. There is no
	// official H2 image, so the drill runs a wrapper the operator builds —
	// a JRE plus this one file — and this constant is that wrapper's
	// contract. The adapter README carries the two lines that build it.
	jarPath = "/opt/h2/h2.jar"

	// workDirName is created under the provider's scratch directory:
	// scratch is the one place the provider guarantees writable, and H2
	// writes beside its database file.
	workDirName = "probavi-h2"
	// dbBaseName is the restored database's base name. H2 addresses a
	// database by the path without its extension and appends .mv.db
	// itself, so this is what connection.database carries and what the
	// runner's {{database}} delivers.
	dbBaseName = "restored"
	// archiveFileName is where the backup archive lands before H2's own
	// Restore tool unpacks it.
	archiveFileName = "backup.zip"

	// defaultUser is H2's own default account.
	defaultUser = "sa"
	// runnerEnvKey is the env entry the check runner reads the database
	// password from. The core substitutes its value from the connection's
	// password_env (protocol §6.1), so no secret is ever in the argv —
	// which is the whole reason this indirection exists.
	runnerEnvKey = "PROBAVI_H2_PASSWORD"
)

// urlSuffix is appended to every JDBC URL this adapter builds.
//
// IFEXISTS=TRUE is not a preference. Pointed at a path that holds no
// database, H2 creates one and answers queries against it (measured), so
// without this a drill whose restore silently produced nothing would open
// a fresh empty database and report every check against it. The flag turns
// that into a refusal the connection cannot survive.
const urlSuffix = ";IFEXISTS=TRUE"

// checkScript is the sql_runner's body, and it exists because H2's Shell
// meets neither half of the runner contract (§6.1) on its own.
//
// It does not exit non-zero on SQL error: it prints "Error: ..." on stdout
// and returns 0, mid-stream, after any output the statements before it
// produced (measured). A runner built on the bare tool would report every
// failing check as passing. And it decorates: a result arrives as a column
// header, the rows, then a "(N rows, M ms)" trailer.
//
// So the script takes the tool's own words as the verdict — any Error line
// fails the check, with the whole message on stderr where a diagnostic
// belongs — and strips the two decoration lines, leaving the undecorated
// rows the contract asks for. A zero-row result correctly leaves nothing.
const checkScript = `out=$(java -cp ` + jarPath + ` org.h2.tools.Shell ` +
	`-url "jdbc:h2:file:$1` + urlSuffix + `" -user "$2" -password "${` + runnerEnvKey + `-}" -sql "$3" 2>&1)
status=$?
if [ "$status" -ne 0 ]; then printf '%s\n' "$out" >&2; exit "$status"; fi
if printf '%s\n' "$out" | grep -q '^Error: '; then printf '%s\n' "$out" >&2; exit 1; fi
printf '%s\n' "$out" | sed -e '1d' -e '$d'`

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "h2"},
		"sources": []map[string]any{
			{"kind": "h2_db", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "h2_db_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "h2_backup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "h2_backup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// H2 speaks SQL, so the core's generating built-ins apply
			// unchanged. There is no server: each check is one JVM opening
			// the restored file, whose in-sandbox base path {{database}}
			// delivers — provision decides it under the provider's scratch
			// directory and returns it as connection.database, which is
			// how a path the probe cannot know reaches a template declared
			// here (§6.1).
			"argv": []string{"sh", "-c", checkScript, "sh", "{{database}}", "{{user}}", "{{sql}}"},
			"env":  map[string]string{runnerEnvKey: "{{password}}"},
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

// opProvision places the artifact in the idle sandbox and proves the
// engine opens it. There is no server to start — H2 runs embedded, and
// the engine is the JVM each later check starts — so engine_ready is the
// preflight's measured wait, and the restore this drill measures is the
// archive's extraction, where there is one, plus the engine's first open
// of the restored database.
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
	user := option(req.Options, "user", defaultUser)
	passwordEnv, perr := resolvePasswordEnv(req.Options, req.Source.CredentialEnv)
	if perr != nil {
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

	readySeconds, perr := checkEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	if perr := prepareWorkDir(ctx, c, workDir); perr != nil {
		return nil, perr
	}

	dbBase := path.Join(workDir, dbBaseName)
	transferSeconds, unpackSeconds, perr := place(ctx, c, src, workDir, dbBase)
	if perr != nil {
		return nil, perr
	}
	openSeconds, perr := assertRestored(ctx, c, dbBase, user)
	if perr != nil {
		return nil, perr
	}
	restoreSeconds := unpackSeconds + openSeconds
	logger.Info("database restored", "seconds", restoreSeconds)

	return map[string]any{
		"connection": map[string]any{
			// There is nothing to dial: checks reach the restored data as
			// a file, through the base path in database. scheme, host and
			// port are §6.2 requirements — the scheme every H2 JDBC URL
			// uses, and a port no client will ever open.
			"scheme": "h2", "host": "127.0.0.1", "port": 0,
			"database": dbBase, "user": user, "password_env": passwordEnv,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// Nothing in either artifact form dates the backup
			// (see source.go).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"db_base": dbBase, "work_dir": workDir},
	}, nil
}

// option reads a drill config option, falling back to a default.
func option(options map[string]string, key, fallback string) string {
	if v := strings.TrimSpace(options[key]); v != "" {
		return v
	}
	return fallback
}

// resolvePasswordEnv names the variable the core resolves {{password}}
// from. It is a variable name, never a value, so it crosses the protocol
// safely (§2.5); the core only honours a name the drill also declared in
// source.credential_env, so refusing an undeclared one here turns a silent
// "password ignored" into a message at the point the mistake was made.
func resolvePasswordEnv(options map[string]string, declared []string) (string, *protoError) {
	name := option(options, "password_env", "")
	if name == "" {
		return "", nil
	}
	for _, d := range declared {
		if d == name {
			return name, nil
		}
	}
	return "", protoErr("invalid_request", false,
		"options.password_env names %s, but the drill's source.credential_env does not list it: "+
			"the core only passes through variables the drill declares, so the password would be "+
			"silently empty", name)
}

// checkEngine verifies the two things every later step runs on: a POSIX
// shell — the check runner and the restore are shell one-liners, because
// the sandbox is where H2's exit-code quirks are absorbed (see
// checkScript) — and a JVM with the H2 jar where the wrapper contract puts
// it. One probe covers both, and its measured duration is the
// engine_ready wait: an embedded engine is ready the moment its runtime
// answers.
func checkEngine(ctx context.Context, c *core) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"sh", "-c", `[ -r "$1" ] || { echo "no H2 jar at $1" >&2; exit 1; }; exec java -version`, "sh", jarPath}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("invalid_request", false,
			"the sandbox image cannot run java with the H2 jar at %s, and this adapter needs "+
				"both — there is no official H2 image, so the drill runs a wrapper you build; "+
				"the adapter README carries the two lines that build it (%s)",
			jarPath, firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// prepareWorkDir creates the directory the restored database lives in.
func prepareWorkDir(ctx context.Context, c *core, workDir string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mkdir", "-p", workDir}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare work directory: %s", firstLine(stderr))
	}
	return nil
}

// place transfers the artifact and, for an archive, has H2 unpack it with
// its own Restore tool — the tool that reads what BACKUP TO writes, so no
// unzip utility has to exist in the sandbox. A database file needs no
// unpacking and goes straight to its restored name.
func place(ctx context.Context, c *core, src *resolvedSource, workDir, dbBase string) (transferSeconds, unpackSeconds float64, perr *protoError) {
	if !src.archive {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: src.path, DestPath: dbBase + mvStoreSuffix, Mode: "0600"})
		if perr != nil {
			return 0, 0, perr
		}
		return put.DurationSeconds, 0, nil
	}
	archivePath := path.Join(workDir, archiveFileName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: archivePath, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"java", "-cp", jarPath, "org.h2.tools.Restore",
		"-file", archivePath, "-dir", workDir, "-db", dbBaseName, "-quiet"}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, 0, protoErr("source_corrupt", false,
			"H2 could not unpack the backup archive: %s", firstLine(stderr))
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// tableCountSQL counts the restored database's own tables. It is the
// restore's verdict, and the query is chosen from what H2 does with a
// damaged file rather than from what would read best.
const tableCountSQL = "SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = 'PUBLIC'"

// assertRestored is the restore's verdict: the engine opening the restored
// database and finding something in it.
//
// Opening alone proves almost nothing here, which was measured the hard
// way. MVStore reconstructs whatever consistent state the bytes present
// can support, so a truncated database does not fail to open — it opens as
// an older one, and at every truncation of the suite's own fixture that
// older state had no tables at all while the engine reported exit 0. A
// SELECT 1 passes against that. So the verdict is the table count, and a
// well-formed zero is refused: a restore that produced no tables has
// proven nothing, and reporting it green is the failure this project
// exists to prevent.
//
// It is still not a full read, deliberately. H2 offers no whole-file
// verification pass, and dumping the database to force one would put work
// into restore_seconds that no real recovery does, in a number an operator
// reads as their RTO. What a tail truncation costs beyond this — rows lost
// from a database whose tables survive — is the same property a copy of a
// running database has, and the adapter README states both rather than
// implying a fence that does not exist.
func assertRestored(ctx context.Context, c *core, dbBase, user string) (float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", checkScript, "sh", dbBase, user, tableCountSQL}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"H2 refused the restored database: %s", firstLine(stderr))
	}
	tables := firstLine(stdout)
	if tables == "0" {
		return 0, protoErr("source_corrupt", false,
			"the restored database holds no tables: H2 opens a damaged file as the older database "+
				"its bytes still describe rather than refusing it, so an empty restore is what a "+
				"truncated backup looks like here — take the backup again, with BACKUP TO")
	}
	if tables == "" {
		return 0, protoErr("source_corrupt", false,
			"the restored database did not answer how many tables it holds, so nothing about "+
				"this restore can be relied on")
	}
	return val.DurationSeconds, nil
}

// healthcheckRequest is the §6.3 request payload. The restored database is
// reached through the connection provision returned; there is no
// server-side state to consult.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
		User     string `json:"user"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored database still serves queries
// (§6.3). It counts INFORMATION_SCHEMA.TABLES rather than selecting a
// constant: a bare SELECT 1 answers from the session without reading the
// store, so counting the schema forces the read that makes the verdict
// mean something. An unhealthy database is a valid result, not an
// operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no restored database")
	}
	user := req.Connection.User
	if user == "" {
		user = defaultUser
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"sh", "-c", checkScript, "sh", req.Connection.Database, user,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES"}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("database file serves queries; %s tables", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("h2 exited %d: %s", val.ExitCode, firstLine(stderr))
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
