package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

const (
	adapterName    = "mysql"
	adapterVersion = "0.8.0"

	defaultUser     = "root"
	defaultDatabase = "probavi"
	defaultPort     = 3306

	readinessBudget = 2 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// databasePattern is deliberately strict: the database name is the only
// operator-supplied string this adapter must embed into SQL text (CREATE
// DATABASE); everything else crosses process boundaries as distinct argv
// elements, injection-proof by construction.
var databasePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "mysql"},
		"sources": []map[string]any{
			{"kind": "mysqldump", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "mysqldump_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "mysqldump_with_users", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "xtrabackup", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Appending ANSI_QUOTES to the session sql_mode makes the server
			// accept the SQL-standard double-quoted identifiers the core
			// emits. The engine dialect is absorbed here, declaratively —
			// exactly what the sql_runner template exists for (§6.1).
			"argv": []string{"mysql", "-h", "127.0.0.1", "-u", "{{user}}", "-D", "{{database}}",
				"--init-command=SET SESSION sql_mode = CONCAT(@@sql_mode, ',ANSI_QUOTES')",
				"-N", "-B", "-e", "{{sql}}"},
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

// opProvision restores the backup into the already-running sandbox: wait
// for engine readiness (TCP, not socket — the first-boot temporary server
// runs with --skip-networking), transfer the dump, load it with the mysql
// client.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	tgt, perr := parseProvisionTarget(req)
	if perr != nil {
		return nil, perr
	}
	user, database, scratch := tgt.user, tgt.database, tgt.scratch

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path, req.Source.Params)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"compressed", src.compressed)

	if req.Source.Kind == "xtrabackup" {
		return provisionPhysical(ctx, c, req, src, logger)
	}

	readySeconds, perr := awaitEngine(ctx, c, user)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine ready", "seconds", readySeconds)

	state := map[string]any{"database": database, "user": user}
	// The account layer goes in before the dump: that is the order of the
	// recovery run-book this drill stands for (principals, then data), and
	// its load is part of the restore, not a phase of its own — the
	// measured restore duration is the drill's RTO figure.
	var usersTransfer, usersLoad float64
	if src.usersPath != "" {
		users := sandboxMember(scratch+"/probavi-users.sql", src.usersCompressed)
		usersTransfer, usersLoad, perr = loadUsers(ctx, c, user, src.usersPath, users)
		if perr != nil {
			return nil, perr
		}
		logger.Info("user accounts loaded", "seconds", usersLoad)
		state["users_path"] = users.path
	}

	dump := sandboxMember(scratch+"/probavi-restore.sql", src.compressed)
	state["dump_path"] = dump.path
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dump.path, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}

	if perr := ensureDatabase(ctx, c, user, database, tgt.charset, tgt.collation); perr != nil {
		return nil, perr
	}
	restore, stderr, perr := execRestore(ctx, c, user, database, dump)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, mapRestoreFailure(restore.ExitCode, stderr)
	}
	logger.Info("restore complete", "seconds", restore.DurationSeconds)

	if src.usersPath != "" {
		if perr := verifyPrincipalChain(ctx, c, user, database); perr != nil {
			return nil, perr
		}
	}

	return map[string]any{
		"connection": map[string]any{
			"scheme": "mysql", "host": "127.0.0.1", "port": defaultPort,
			"database": database, "user": user,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     usersTransfer + put.DurationSeconds,
			"restore_seconds":      usersLoad + restore.DurationSeconds,
		},
		"state": state,
	}, nil
}

// namePattern validates charset and collation option values: identifier
// characters only, so nothing can escape the CREATE DATABASE statement
// they are embedded in.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// provisionTarget is the validated per-drill target configuration.
type provisionTarget struct {
	user, database, charset, collation, scratch string
}

// parseProvisionTarget validates every operator-supplied value that can
// reach SQL text. The charset and collation options exist because without
// them the restore target is created with the sandbox server's defaults —
// which silently differ from the source database's (a dump without
// --databases carries no CREATE DATABASE, and the collation governs
// comparisons, ordering, and uniqueness). The options let a drill pin the
// source values; the README documents the trade.
func parseProvisionTarget(req *provisionRequest) (*provisionTarget, *protoError) {
	tgt := &provisionTarget{
		user:      option(req.Options, "user", defaultUser),
		database:  option(req.Options, "database", defaultDatabase),
		charset:   option(req.Options, "charset", ""),
		collation: option(req.Options, "collation", ""),
		scratch:   req.Sandbox.ScratchDir,
	}
	if !databasePattern.MatchString(tgt.database) {
		return nil, protoErr("invalid_request", false,
			"database name %s must contain only letters, digits, and underscores", tgt.database)
	}
	for name, v := range map[string]string{"charset": tgt.charset, "collation": tgt.collation} {
		if v != "" && !namePattern.MatchString(v) {
			return nil, protoErr("invalid_request", false,
				"%s %s must contain only letters, digits, and underscores", name, v)
		}
	}
	if tgt.scratch == "" {
		tgt.scratch = "/tmp"
	}
	return tgt, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
		User     string `json:"user"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the provisioned instance serves queries (§6.3).
// An unhealthy engine is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	user := req.Connection.User
	if user == "" {
		user = defaultUser
	}
	database := req.Connection.Database
	if database == "" {
		database = defaultDatabase
	}
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", user, "-D", database,
			"-N", "-B", "-e", "SELECT 1"},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "1"
	detail := "accepting queries"
	if !healthy {
		detail = fmt.Sprintf("mysql exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// awaitEngine polls a TCP SELECT 1 until the server serves queries. The
// official image's first-boot initialization runs a temporary server with
// --skip-networking (socket only), so a TCP probe cannot report ready
// during init — the same trap as PostgreSQL's initdb-phase server. The
// probe runs without -D: the target database may not exist yet.
func awaitEngine(ctx context.Context, c *core, user string) (float64, *protoError) {
	start := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"mysql", "-h", "127.0.0.1", "-u", user, "-N", "-B", "-e", "SELECT 1"},
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
				"engine did not accept TCP connections within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// ensureDatabase creates the restore target if missing. Plain mysqldump
// output (without --databases) carries no CREATE DATABASE statement, so
// the target must exist before the load; for --databases dumps this is a
// no-op. Without explicit charset/collation options the target gets the
// sandbox server's defaults, not the source database's. All three values
// are pattern-validated before they can reach this statement, and the
// options only apply when the adapter actually creates the database.
func ensureDatabase(ctx context.Context, c *core, user, database, charset, collation string) *protoError {
	stmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", database)
	if charset != "" {
		stmt += " CHARACTER SET " + charset
	}
	if collation != "" {
		stmt += " COLLATE " + collation
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"mysql", "-h", "127.0.0.1", "-u", user, "-N", "-B", "-e", stmt},
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("restore_failed", false, "create target database: %s", firstLine(stderr))
	}
	return nil
}

// execRestore loads the dump with the mysql client's source command. The
// client stops at the first error (no --force): partial restores fail
// loudly (§5). The dump path is adapter-composed, never operator input.
func execRestore(ctx context.Context, c *core, user, database string, dump sqlMember) (*execValue, []byte, *protoError) {
	argv := []string{"mysql", "-h", "127.0.0.1", "-u", user, "--database", database,
		"-e", "source " + dump.path}
	if dump.compressed {
		argv = []string{"sh", "-c", compressedRestoreScript, "sh",
			dump.path, dump.statusPath(), user, database}
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}

// compressedRestoreScript loads a compressed dump without ever writing the
// plain SQL down: the sandbox needs room for the stored artifact only,
// while materialising it would ask for the uncompressed size on top of the
// data directory the restore is about to fill.
//
// Both ends of the pipeline are judged, in this order. A pipeline's status
// is its last command's, so $? carries the client's verdict and the
// decompressor's is left in a file. The client is judged first
// deliberately: when a dump holds bad SQL the client aborts, the
// decompressor then dies of a broken pipe, and blaming the archive would
// name the wrong culprit. A decompressor that failed while the client was
// content is the case that matters — a truncated archive whose surviving
// prefix happens to end on a statement boundary would otherwise pass as a
// complete restore, which the protocol forbids (§5).
//
// A client failure exits 1 rather than passing the client's own status
// through, so decompressFailedExit stays unambiguous.
const compressedRestoreScript = `
{ gzip -dc -- "$1"; echo $? > "$2"; } | mysql -h 127.0.0.1 -u "$3" --database "$4" || exit 1
[ "$(cat "$2")" = 0 ] || exit 90
`

// decompressFailedExit is what the load scripts exit with when the
// decompressor failed and the client did not notice. No mysql client
// produces it: the scripts normalise every client failure to 1.
const decompressFailedExit = 90

// sqlMember is one SQL file placed inside the sandbox, and how it is
// stored there.
type sqlMember struct {
	path       string
	compressed bool
}

// sandboxMember names a member inside the sandbox. A compressed member
// keeps the extension its bytes call for, so a person reading the adapter
// log or an exec argv sees what the file actually is.
func sandboxMember(path string, compressed bool) sqlMember {
	if compressed {
		path += ".gz"
	}
	return sqlMember{path: path, compressed: compressed}
}

// statusPath is where the load script leaves the decompressor's exit
// status. It sits beside the member, inside the scratch directory the core
// gave this drill, and dies with the sandbox.
func (m sqlMember) statusPath() string { return m.path + ".status" }

// mapRestoreFailure classifies mysql client load failures into protocol
// error codes. ERROR 1064 is the parser rejecting the input as SQL; the
// ASCII '\0' message is the client refusing binary garbage — both mean
// "this is not a usable SQL dump".
func mapRestoreFailure(exitCode int, stderr []byte) *protoError {
	if exitCode == decompressFailedExit {
		return mapDecompressFailure(stderr)
	}
	line := firstDiagnostic(stderr)
	if strings.Contains(line, "ERROR 1064") || strings.Contains(line, `ASCII '\0'`) {
		return protoErr("source_corrupt", false, "mysql rejected the dump: %s", line)
	}
	return protoErr("restore_failed", false, "mysql load failed: %s", line)
}

// mapDecompressFailure separates the two ways decompression fails inside a
// sandbox. A missing tool is the operator's image, not their backup, and
// calling that a corrupt source would send them to inspect a file that is
// fine.
func mapDecompressFailure(stderr []byte) *protoError {
	line := firstLine(stderr)
	if strings.Contains(line, "not found") {
		return protoErr("restore_failed", false,
			"the sandbox image provides no gzip, so a compressed dump cannot be restored in it: %s", line)
	}
	return protoErr("source_corrupt", false, "the dump could not be decompressed: %s", line)
}

func option(opts map[string]string, key, fallback string) string {
	if v, ok := opts[key]; ok && v != "" {
		return v
	}
	return fallback
}

// firstDiagnostic returns the client's first ERROR line, falling back to
// the first line of all. A decompressor shares the pipeline's stderr and
// can add a broken-pipe note of its own once the client aborts; that note
// must not become the drill's explanation.
func firstDiagnostic(stderr []byte) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ERROR") {
			return firstLine([]byte(line))
		}
	}
	return firstLine(stderr)
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// The message crosses the protocol as a JSON string and lands in
	// evidence error fields: keep it single-line, quote-free, and free of
	// credentials.
	return strings.ReplaceAll(scrubSecrets(s), `"`, "'")
}

// A users script carries every account's credentials — password hashes
// (IDENTIFIED WITH ... AS 0x... or '$A$...'), possibly plaintext
// (IDENTIFIED BY '...') — and the server quotes the offending source token
// back in syntax errors ("near '...'"). Engine diagnostics are therefore a
// live path from a backup's credentials into a signed evidence record,
// which the schema forbids from carrying any (evidence schema §8).
var (
	identifiedLiteral = regexp.MustCompile(`(?i)identified\s+(?:with\s+\S+\s+)?(?:by|as)\s+(?:password\s+)?('[^']*'|0x[0-9A-Fa-f]+)`)
	hashLiteral       = regexp.MustCompile(`'\$A\$[0-9]{3}\$[^']*'`)
	hexLiteral        = regexp.MustCompile(`(?i)0x[0-9a-f]{12,}`)
)

// scrubSecrets removes credential material from text bound for a protocol
// message.
func scrubSecrets(s string) string {
	s = identifiedLiteral.ReplaceAllString(s, "IDENTIFIED [redacted]")
	s = hashLiteral.ReplaceAllString(s, "'[redacted]'")
	return hexLiteral.ReplaceAllString(s, "0x[redacted]")
}
