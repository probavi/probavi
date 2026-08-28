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
	adapterName    = "firebird"
	adapterVersion = "0.1.0"

	// workDirName is created under the provider's scratch directory.
	// Firebird refuses nothing about where a database file lives, but
	// scratch is the one directory the provider guarantees writable.
	workDirName = "probavi-firebird"
	// dbFileName is the restored database inside the work directory: the
	// file every check opens, reached through connection.database.
	dbFileName = "restored.fdb"
	// backupFileName is where the artifact is placed before gbak reads it.
	backupFileName = "backup.fbk"
)

// checkScript runs one check statement through isql.
//
// Two measured facts shape it. isql takes its statements on stdin or from
// a file and has no "run this one string" flag, so the statement is fed in
// rather than passed as an argument — the protocol still requires {{sql}}
// to be its own argv element (§10 check 2), which is what the trailing
// arguments provide. And `SET HEADING OFF` is what removes the column
// titles and rule line; isql has no delimiter setting at all, so a
// single-value result — every generating built-in the core composes —
// arrives exact after the core trims it, while a multi-column result
// arrives in isql's own column layout rather than tab-separated. The
// adapter README states that limit rather than parsing fixed-width columns
// back into fields, which cannot be done without guessing where a padded
// value ends and the padding begins.
//
// -nodbtriggers is the load-bearing flag, not a nicety: see the trigger
// note in opProvision.
const checkScript = `set -u
set -o pipefail
s=$2
s=${s%"${s##*[![:space:]]}"}
s=${s%;}
printf 'SET HEADING OFF;\n%s;\n' "$s" | isql -q -b -nodbtriggers "$1"`

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "firebird"},
		"sources": []map[string]any{
			{"kind": "firebird_gbak", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "firebird_gbak_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			"argv": []string{"bash", "-c", checkScript, "bash", "{{database}}", "{{sql}}"},
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

// opProvision places the artifact in the sandbox and has gbak rebuild a
// database from it. There is no server to start: isql against a plain file
// path uses the embedded engine, so a drill needs no listener, no port and
// no credentials (measured on the official image under --network none).
//
// Every connection this adapter makes, and the check runner it declares,
// carries -nodbtriggers. That is not tidiness. An ON CONNECT database
// trigger travels inside the backup and is restored with it; measured, a
// clean restore holds every row and the first ordinary connection fires
// the trigger and deletes what it is written to delete, irreversibly. The
// trigger is suspended for the drill's own connections and never altered,
// so a check that reads RDB$TRIGGERS still sees exactly what the operator
// declared.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, protoErr("invalid_request", false,
			"this adapter does not support pitr: a gbak backup is a snapshot of one instant, and the "+
				"engine offers nothing to recover between two of them")
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path, req.Source.Params)
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

	dbPath := path.Join(workDir, dbFileName)
	backupPath := path.Join(workDir, backupFileName)
	transferSeconds, restoreSeconds, perr := restoreBackup(ctx, c, src.path, backupPath, dbPath)
	if perr != nil {
		return nil, perr
	}
	logger.Info("database restored", "seconds", restoreSeconds)

	if perr := assertServing(ctx, c, dbPath); perr != nil {
		return nil, perr
	}

	return map[string]any{
		"connection": map[string]any{
			// There is nothing to dial: checks reach the restored data as a
			// file, through the path in database. scheme, host and port are
			// §6.2 requirements — the scheme Firebird connection strings
			// use, and a port no client will ever open.
			"scheme": "firebird", "host": "127.0.0.1", "port": 0,
			"database": dbPath, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			"created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"db_path": dbPath, "work_dir": workDir},
	}, nil
}

// checkEngine verifies the two binaries every later step runs: gbak, which
// performs the restore, and isql, which every check opens the result with.
// Its measured duration is the engine_ready wait — an embedded engine is
// ready the moment its tools answer.
func checkEngine(ctx context.Context, c *core) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", `command -v gbak >/dev/null && command -v isql >/dev/null && echo 1`}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("invalid_request", false,
			"the sandbox image does not carry gbak and isql, and this adapter needs both — use the "+
				"Firebird project's own image, which the adapter README names (%s)", firstLine(stderr))
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

// restoreScript runs gbak, and two of its three lines exist because of
// what gbak does rather than what it is for.
//
// It writes everything to stdout and nothing to stderr — measured — so its
// words are redirected to stderr, where the sandbox verb's stderr capture
// carries them to the operator and this adapter's own stdout stays what
// the protocol reserves it for (§2.2).
//
// It also prompts. A backup whose volume ends early makes gbak ask the
// operator to "press return to reopen that file, or type a new name",
// measured on a truncated artifact — and a restore that waits forever for
// an answer nobody is there to give is worse than one that fails. stdin is
// closed so the question is answered by EOF.
//
// The removal on failure is the third line and the important one.
// Measured: a truncated backup and one corrupted mid-file both exit 1
// while leaving a database that opens and answers queries holding every
// row, and a refused cross-version restore leaves an openable empty one.
// Anything downstream that judged by whether the restored database
// responds would call a broken backup proven, so the file does not survive
// the failure that produced it.
const restoreScript = `gbak -c "$1" "$2" >&2 </dev/null || { rm -f "$2"; exit 1; }`

// restoreBackup transfers the artifact and rebuilds a database from it.
//
// The removal on failure is the point. Measured: a truncated backup and one
// corrupted mid-file both exit 1 while leaving a database that opens and
// answers queries holding every row, and a refused cross-version restore
// leaves an openable empty one. Anything downstream that judged by whether
// the restored database responds would call a broken backup proven, so the
// file does not survive the failure that produced it.
func restoreBackup(ctx context.Context, c *core, hostPath, backupPath, dbPath string) (transferSeconds, restoreSeconds float64, perr *protoError) {
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: backupPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", restoreScript, "sh", backupPath, dbPath}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, 0, mapRestoreFailure(stderr)
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// mapRestoreFailure reads gbak's own words and says which failure it was.
//
// The classification rests on the ERROR line and nothing else. gbak also
// emits "do not recognize <something> attribute N -- continuing" for
// anything in the stream it has no meaning for, and that line is
// deliberately not used to decide: measured, a Firebird 5 partial index
// restored by Firebird 4 draws it (an artifact newer than the sandbox),
// and so does a backup with random bytes written over its middle (an
// artifact that is simply damaged). Reporting one cause when the evidence
// admits two would send an operator looking for the wrong problem, so the
// message names both and lets gbak's own words separate them.
func mapRestoreFailure(stderr []byte) *protoError {
	words := string(stderr)
	unrecognised := strings.Contains(words, "do not recognize")
	for _, marker := range restoreCorruptMarkers {
		if strings.Contains(words, marker) {
			if unrecognised {
				return protoErr("source_corrupt", false,
					"gbak met parts of this backup it has no meaning for and then failed: either the "+
						"artifact is damaged, or it was written by a newer Firebird than this sandbox "+
						"runs — its own words say which: %s", firstLine(stderr))
			}
			return protoErr("source_corrupt", false, "gbak rejected the backup: %s", errorLine(stderr))
		}
	}
	return protoErr("restore_failed", false, "gbak could not restore the backup: %s", errorLine(stderr))
}

// restoreCorruptMarkers are the phrases gbak uses for an artifact it read
// and rejected. Measured against three damage forms: a backup truncated
// part-way, one whose bytes were overwritten mid-file, and a file that is
// not a backup at all.
var restoreCorruptMarkers = []string{
	"Backup incomplete",
	"string truncated",
	"expected backup description record",
}

// errorLine picks gbak's ERROR line out of its output rather than the
// first line. A truncated artifact makes gbak print an interactive prompt
// first — "press return to reopen that file" — so the first line is a
// question nobody answered, and the diagnosis is further down.
func errorLine(stderr []byte) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.Contains(line, "ERROR") {
			return firstLine([]byte(line))
		}
	}
	return firstLine(stderr)
}

// assertServing proves the restored database answers before the drill
// calls anything green. A restore that exited 0 is not yet a database that
// serves, and every check after this one runs against whatever this query
// reaches.
func assertServing(ctx context.Context, c *core, dbPath string) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c",
			`printf 'SET HEADING OFF;\nSELECT 1 FROM RDB$DATABASE;\n' | isql -q -b -nodbtriggers "$1"`,
			"sh", dbPath}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 || strings.TrimSpace(string(stdout)) != "1" {
		return protoErr("restore_failed", false,
			"gbak reported success but the restored database does not answer: %s", firstLine(stderr))
	}
	return nil
}

// healthcheckRequest is the §6.3 request payload. The restored database is
// reached through the connection provision returned; there is no
// server-side state to consult.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored database still serves queries
// (§6.3). An unhealthy database is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no restored database")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c",
			`printf 'SET HEADING OFF;\nSELECT count(*) FROM RDB$RELATIONS;\n' | isql -q -b -nodbtriggers "$1"`,
			"sh", req.Connection.Database}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("database file serves queries; %s relations", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("isql exited %d: %s", val.ExitCode, firstLine(stderr))
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
