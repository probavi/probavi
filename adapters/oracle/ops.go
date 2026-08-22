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
	adapterName    = "oracle"
	adapterVersion = "0.1.0"

	// workDirName is created under the provider's scratch directory —
	// the official image runs as the oracle user (uid 54321, measured),
	// and scratch is the directory the provider guarantees writable.
	workDirName = "probavi-oracle"
	// dumpName is the file name the dump is placed under; Data Pump
	// names files relative to a directory object, never by path.
	dumpName = "import.dmp"
	// importLogName is the log Data Pump writes beside the dump.
	importLogName = "probavi-import.log"
	// directoryName is the directory object the drill creates in the
	// pluggable database for the dump — upper case, because Data Pump
	// matches the name against the dictionary exactly (measured: the
	// lower-case spelling is "ORA-39087: Directory name ... is invalid").
	directoryName = "PROBAVI_DUMP"
	// jobName is the import job's name, which is also its master table's
	// — so it must differ from the directory object's: both live in the
	// SYS namespace, and a shared name breaks the job's own lookup
	// (measured: ORA-31632 with ORA-01422 "exact fetch returned more than
	// the requested number of rows").
	jobName = "PROBAVI_IMPORT"
	// defaultPDB is the pluggable database the verified image ships; it
	// is used only when the engine did not name one (the simulated
	// sandbox), every real drill reads the name from the instance.
	defaultPDB = "FREEPDB1"

	// importGrace is how long the import job may stay out of its running
	// states while its client waits before the client is declared hung
	// (importScript). Connecting takes a few seconds at the start, leaving
	// takes under one at the end; the measured hang is forever.
	importGrace = 60 * time.Second
)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "oracle"},
		"sources": []map[string]any{
			{"kind": "oracle_datapump", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Checks are SQL through SQL*Plus over the bequeath adapter in
			// the restored pluggable database (see runnerScript). The
			// core's generating built-ins apply: the dialect takes
			// SQL-standard quoted identifiers, and the session's NLS
			// formats render max() of a timestamp in a form the core
			// parses (measured) — the README records the consequences.
			// NLS_LANG names the client character set; without it the
			// client renders every non-ASCII character as '?' (measured).
			"argv": []string{"bash", "-c", runnerScript, "bash", "{{sql}}"},
			"env": map[string]string{
				"ORACLE_PDB_SID": "{{database}}",
				"NLS_LANG":       ".AL32UTF8",
			},
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

// opProvision imports the dump into the image's pluggable database:
// preflight, an instance started from the drill's own parameter file
// (no listener, no dispatchers, scheduler and queue time manager
// suspended), the pins read back, the dump transferred, its own header
// vetted through the engine, and an import whose verdict is the client's
// exit code under a watchdog that knows the one way it never returns.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req, scratch, loc, perr := parseProvisionRequest(payload)
	if perr != nil {
		return nil, perr
	}

	src, perr := resolveSource(req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes)

	if perr := checkEngine(ctx, c); perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	readySeconds, engine, perr := startEngine(ctx, c, workDir)
	if perr != nil {
		return nil, perr
	}
	pdb, perr := choosePDB(engine)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine ready", "seconds", readySeconds, "version", engine.version, "pdb", pdb)

	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: path.Join(workDir, dumpName), Mode: "0600"})
	if perr != nil {
		return nil, perr
	}

	header, headerSeconds, perr := readHeader(ctx, c, pdb, workDir)
	if perr != nil {
		return nil, perr
	}
	if perr := vetHeader(header, engine.version); perr != nil {
		return nil, perr
	}

	importSeconds, perr := runImport(ctx, c, pdb, workDir)
	if perr != nil {
		return nil, perr
	}
	logger.Info("import complete", "seconds", importSeconds)

	return map[string]any{
		"connection": map[string]any{
			// Checks reach the instance over the bequeath adapter inside
			// the sandbox; there is no TCP endpoint to name.
			"scheme": "oracle", "host": "127.0.0.1", "port": 0,
			"database": pdb, "user": "sys",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			"created_at": createdAt(header.item(itemCreationDate), loc),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			"restore_seconds":      headerSeconds + importSeconds,
		},
		"state": map[string]any{
			"work_dir": workDir, "pdb": pdb,
		},
	}, nil
}

// parseProvisionRequest validates the §6.2 payload and resolves the
// scratch directory and the declared backup zone.
func parseProvisionRequest(payload json.RawMessage) (*provisionRequest, string, *time.Location, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, "", nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, "", nil, protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	loc, perr := backupLocation(req.Source.Params)
	if perr != nil {
		return nil, "", nil, perr
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	return req, scratch, loc, nil
}

// checkEngine verifies the toolchain every later step runs on: SQL*Plus
// and impdp on the PATH, ORACLE_HOME and ORACLE_SID in the environment —
// what the official image provides (measured; its lite variant ships
// neither impdp nor the XML component Data Pump runs on, and is refused
// here by the first of those).
func checkEngine(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", toolScript}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image lacks the Oracle toolchain (sqlplus and impdp on the PATH, ORACLE_HOME "+
				"and ORACLE_SID set): use the official container-registry.oracle.com/database/free "+
				"image — not its lite variant — with command: sleep infinity (%s)", firstLine(stderr))
	}
	return nil
}

// startEngine starts the instance and reads what it is.
func startEngine(ctx context.Context, c *core, workDir string) (float64, engineIdentity, *protoError) {
	start, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", startScript, "bash", workDir}})
	if perr != nil {
		return 0, engineIdentity{}, perr
	}
	if start.ExitCode != 0 {
		return 0, engineIdentity{}, classifyStartFailure(stdout, stderr)
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", identityScript}})
	if perr != nil {
		return 0, engineIdentity{}, perr
	}
	if val.ExitCode != 0 {
		return 0, engineIdentity{}, protoErr("restore_failed", false,
			"the instance started but does not answer: %s", firstLine(append(stdout, stderr...)))
	}
	engine := parseIdentity(stdout)
	if perr := verifyPins(engine); perr != nil {
		return 0, engineIdentity{}, perr
	}
	return start.DurationSeconds + val.DurationSeconds, engine, nil
}

// classifyStartFailure reads the engine's own words for why it did not
// start: every marker below was measured on the verified image.
func classifyStartFailure(stdout, stderr []byte) *protoError {
	line := verdictLine(append(stdout, stderr...))
	switch {
	case strings.Contains(line, "ORA-01081"):
		return protoErr("invalid_request", false,
			"an instance is already running in the sandbox: start the image idle (docker: command: "+
				"sleep infinity) and let the adapter own it, so the listener, the dispatchers and the "+
				"scheduler stay off")
	case strings.Contains(line, "ksipc: no private ips"):
		return protoErr("invalid_request", false,
			"the instance refuses a loopback-only host (%s): the sandbox must join a network — "+
				"for the docker provider an internal one (docker network create --internal probavi; "+
				"sandbox.params.network: probavi) — nothing listens on it, the instance only needs an "+
				"interface to exist", line)
	case strings.Contains(line, "ORA-03113"), strings.Contains(line, "ORA-27102"),
		strings.Contains(line, "ORA-00845"):
		return protoErr("invalid_request", false,
			"the instance died while starting (%s) — most often the sandbox's memory: the measured "+
				"floor is 3 GiB (sandbox.params.memory: 3g); at 2 GiB the instance mounts and is "+
				"killed while opening", line)
	}
	return protoErr("restore_failed", false, "the instance failed to start: %s", line)
}

// verifyPins refuses an instance whose pins did not take: a policy that
// may run in the drill is a policy that may subtract from what the backup
// holds before a check reads it.
func verifyPins(engine engineIdentity) *protoError {
	for _, name := range []string{"job_queue_processes", "aq_tm_processes"} {
		if v, ok := engine.pins[name]; ok && v != "0" {
			return protoErr("invalid_request", false,
				"the instance reports %s=%s after a launch that pinned it to 0 — the sandbox engine "+
					"would run the backup's own scheduled work against the restored data; this "+
					"adapter refuses to drill under that", name, v)
		}
	}
	return nil
}

// choosePDB names the pluggable database the import lands in: the one the
// instance has open for writing. The verified image ships exactly one;
// an instance that names none cannot take an import, one that names
// several leaves the target undecidable.
func choosePDB(engine engineIdentity) (string, *protoError) {
	switch len(engine.pdbs) {
	case 0:
		if engine.version == "" {
			// The engine answered in no readable shape at all (the
			// simulated sandbox): the image's own name stands in.
			return defaultPDB, nil
		}
		return "", protoErr("restore_failed", false,
			"the instance has no pluggable database open read write to import into")
	case 1:
		return engine.pdbs[0], nil
	}
	return "", protoErr("invalid_request", false,
		"the instance has %d pluggable databases open read write (%s); this adapter imports "+
			"into the image's single one", len(engine.pdbs), strings.Join(engine.pdbs, ", "))
}

// readHeader asks the engine what the transferred file is.
func readHeader(ctx context.Context, c *core, pdb, workDir string) (dumpHeader, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", headerScript, "bash", pdb, workDir, dumpName}})
	if perr != nil {
		return dumpHeader{}, 0, perr
	}
	if val.ExitCode != 0 {
		line := verdictLine(append(stdout, stderr...))
		if strings.Contains(line, "ORA-39211") {
			return dumpHeader{}, 0, protoErr("source_corrupt", false,
				"the engine cannot read the file as a Data Pump dump: %s", line)
		}
		return dumpHeader{}, 0, protoErr("restore_failed", false,
			"reading the dump header failed: %s", line)
	}
	return parseHeader(stdout), val.DurationSeconds, nil
}

// runImport runs impdp under the watchdog and reads its verdict.
func runImport(ctx context.Context, c *core, pdb, workDir string) (float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", importScript, "bash",
		pdb, dumpName, path.Join(workDir, "impdp.out"), strconv.Itoa(int(importGrace.Seconds()))}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode == 0 {
		return val.DurationSeconds, nil
	}
	return 0, classifyImportFailure(val.ExitCode, append(stdout, stderr...))
}

// Exit codes of impdp and of the watchdog around it.
const (
	impdpExitErrors = 5   // the job completed, with errors
	watchdogExit    = 125 // the client hung on a dead job
)

// classifyImportFailure maps the import's exit code and the engine's own
// lines to protocol codes. Every marker was measured on the verified
// image (importScript's comment).
func classifyImportFailure(exit int, output []byte) *protoError {
	line := verdictLine(output)
	errors := countLines(output, "ORA-")
	switch {
	case exit == watchdogExit:
		return protoErr("source_corrupt", false,
			"the Data Pump job stopped executing and its client never returned within %s — the "+
				"measured signature of a dump damaged mid-file, where the worker dies loading the "+
				"dump's master table (%s)", importGrace, line)
	case exit == impdpExitErrors:
		return protoErr("restore_failed", false,
			"the import completed with %d error lines — a restore with errors proves nothing about "+
				"the backup; first: %s", errors, line)
	case strings.Contains(line, "ORA-39142"):
		return protoErr("invalid_request", false,
			"the engine refused the version pairing: %s — use an image at least as new as the "+
				"backup's origin", line)
	case containsAny(string(output), "ORA-39411", "ORA-27046", "ORA-39000"):
		return protoErr("source_corrupt", false, "the engine rejected the dump file: %s", line)
	}
	return protoErr("restore_failed", false, "import failed (impdp exited %d): %s", exit, line)
}

func containsAny(s string, markers ...string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func countLines(b []byte, marker string) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, marker) {
			n++
		}
	}
	return n
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the pluggable database still answers (§6.3). An
// unhealthy instance is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	pdb := req.Connection.Database
	if pdb == "" {
		pdb = defaultPDB
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", healthScript, "bash", pdb}, TimeoutSeconds: 30})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0 && firstLine(stdout) == "1"
	detail := "accepting queries"
	if !healthy {
		detail = fmt.Sprintf("sqlplus exited %d: %s", val.ExitCode, verdictLine(append(stdout, stderr...)))
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// verdictLine extracts the engine's primary message from SQL*Plus or
// impdp output: the first ORA-/SP2-/PLS-/RMAN- line, or the first
// non-empty line when there is none. The result crosses the protocol as
// a JSON string and lands in evidence error fields: keep it single-line
// and quote-free.
func verdictLine(b []byte) string {
	first := ""
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if first == "" {
			first = s
		}
		if containsAny(s, "ORA-", "SP2-", "PLS-", "UDI-") {
			return strings.ReplaceAll(s, `"`, "'")
		}
	}
	return strings.ReplaceAll(first, `"`, "'")
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(s, `"`, "'")
}
