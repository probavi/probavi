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
	adapterName    = "duckdb"
	adapterVersion = "0.1.0"

	// workDirName is created under the provider's scratch directory —
	// the one directory the provider guarantees writable on any image.
	workDirName = "probavi-duckdb"
	// dbFileName is the restored database inside the work directory: the
	// file every check opens, reached through connection.database.
	dbFileName = "restored.duckdb"
	// exportDirName is where the export kind places the EXPORT DATABASE
	// files before importing them into a fresh database.
	exportDirName = "export"

	// newerStorageMarker is the engine's own words for a database file
	// whose storage format is newer than the engine reads (measured on
	// 1.4.5 opening a v1.5.0-format file); ops.go maps it to the
	// invalid_request refusal that names both sides.
	newerStorageMarker = "version number"
)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "duckdb"},
		"sources": []map[string]any{
			{"kind": "duckdb_db", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "duckdb_db_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "duckdb_export", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// DuckDB speaks SQL, so the core's generating built-ins apply
			// unchanged. There is no server: each check is one duckdb
			// invocation opening the restored file, whose in-sandbox path
			// {{database}} delivers — provision decides it under the
			// provider's scratch directory and returns it as
			// connection.database (§6.1). No shell anywhere: the SQL and
			// the path each travel as one argv element. -bail makes the
			// exit code report the first SQL error, and -list -noheader
			// with the literal tab separator produce the undecorated
			// tab-separated rows the runner contract requires — the
			// default output stays a decorated box even piped (measured),
			// so the mode flags are load-bearing, not cosmetic.
			"argv": []string{"duckdb", "-batch", "-list", "-noheader", "-bail",
				"-separator", "\t", "{{database}}", "{{sql}}"},
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

// opProvision places the artifact in the idle sandbox and proves the
// engine reads it: preflight (the duckdb CLI answers), transfer, then
// either an opening read of the placed database or an IMPORT DATABASE of
// the export into a fresh one. There is no server to start — DuckDB is a
// library, and the engine is the duckdb process each later check runs —
// so engine_ready is the preflight's measured wait, and the restore this
// drill measures is the engine's first read of the restored data.
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
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes)

	engine, readySeconds, perr := checkEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	dbPath := path.Join(workDir, dbFileName)
	transferSeconds, restoreSeconds, perr := restoreArtifact(ctx, c, src, workDir, dbPath, engine)
	if perr != nil {
		return nil, perr
	}
	logger.Info("database restored", "seconds", restoreSeconds)

	return map[string]any{
		"connection": map[string]any{
			// There is nothing to dial: checks reach the restored data as
			// a file, through the path in database. scheme, host and port
			// are §6.2 requirements — the scheme DuckDB connection
			// strings use, and a port no client will ever open.
			"scheme": "duckdb", "host": "127.0.0.1", "port": 0,
			"database": dbPath, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// Nothing in either artifact form dates it (see source.go).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"db_path": dbPath, "work_dir": workDir},
	}, nil
}

// checkEngine verifies the duckdb CLI answers, which is the whole
// readiness question for an embedded engine. The official duckdb/duckdb
// images carry the binary and nothing else — no shell, no coreutils, not
// even a way to idle (measured) — so a drill sandbox runs the two-line
// wrapper the README documents, and an image without the CLI is refused
// up front with a message that names it. The reported version feeds the
// storage-format refusal below.
func checkEngine(ctx context.Context, c *core) (engineVersion string, readySeconds float64, perr *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"duckdb", "--version"}})
	if perr != nil {
		return "", 0, perr
	}
	if val.ExitCode != 0 {
		return "", 0, protoErr("invalid_request", false,
			"the sandbox image cannot run the duckdb CLI: the official duckdb/duckdb images "+
				"carry only the binary and cannot host a drill sandbox on their own — build the "+
				"wrapper image the adapter README documents (%s)", firstLine(stderr))
	}
	return firstLine(stdout), val.DurationSeconds, nil
}

// prepareWorkDir creates the directory the restored database lives in.
func prepareWorkDir(ctx context.Context, c *core, dir string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mkdir", "-p", dir}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare work directory: %s", firstLine(stderr))
	}
	return nil
}

// restoreArtifact drives the branch the source kind decided: place a
// database file and open it, or transfer an export and import it.
func restoreArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir, dbPath, engine string) (transferSeconds, restoreSeconds float64, perr *protoError) {
	if src.export {
		return importExport(ctx, c, src, workDir, dbPath)
	}
	return placeDatabase(ctx, c, src, workDir, dbPath, engine)
}

// vetQuery is the opening read: counting the catalog forces the header
// and catalog blocks through the engine's checksum verification, and an
// invalid or truncated file fails it loudly (measured — a zero-byte file
// included, which is why no host-side gate duplicates that verdict).
const vetQuery = "SELECT count(*) FROM duckdb_tables();"

// placeDatabase transfers the artifact and has the engine open it.
// Placing a file is not yet a restore: the restore this adapter measures
// is duckdb's checksummed first read of the placed database, which is
// also the sandbox-side verdict on the backup.
func placeDatabase(ctx context.Context, c *core, src *resolvedSource,
	workDir, dbPath, engine string) (transferSeconds, restoreSeconds float64, perr *protoError) {
	if perr := prepareWorkDir(ctx, c, workDir); perr != nil {
		return 0, 0, perr
	}
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dbPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"duckdb", "-batch", "-list", "-noheader", dbPath, vetQuery}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, 0, mapOpenFailure(src, engine, verdictLine(stdout, stderr))
	}
	return put.DurationSeconds, val.DurationSeconds, nil
}

// mapOpenFailure classifies a failed opening read. A storage format newer
// than the engine reads deserves its own words: it is a drill config
// pairing a backup with a sandbox image that cannot restore it
// (docs/engine-versions.md §5), and the header the host already read
// names the exact versions the engine's message cannot.
func mapOpenFailure(src *resolvedSource, engine, line string) *protoError {
	if strings.Contains(line, newerStorageMarker) {
		written := ""
		if src.header.libraryVersion != "" {
			written = fmt.Sprintf(", written by DuckDB %s", src.header.libraryVersion)
		}
		return protoErr("invalid_request", false,
			"the database file carries storage format version %d%s, and the sandbox engine (%s) "+
				"cannot read it: %s — use a duckdb image at least as new as the one that wrote "+
				"the backup", src.header.storageVersion, written, engine, line)
	}
	return protoErr("source_corrupt", false, "duckdb rejected the restored database: %s", line)
}

// importExport transfers the export's files one by one — exports are flat
// (measured) — and imports them into a fresh database. IMPORT DATABASE
// resolves the data-file paths against the directory it is handed rather
// than the paths load.sql recorded at export time (measured against a
// moved export), so the sandbox placement owes nothing to where the
// export was taken.
func importExport(ctx context.Context, c *core, src *resolvedSource,
	workDir, dbPath string) (transferSeconds, restoreSeconds float64, perr *protoError) {
	exportDir := path.Join(workDir, exportDirName)
	if perr := prepareWorkDir(ctx, c, exportDir); perr != nil {
		return 0, 0, perr
	}
	names, perr := exportFiles(src.path)
	if perr != nil {
		return 0, 0, perr
	}
	for _, name := range names {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: path.Join(src.path, name),
			DestPath:   path.Join(exportDir, name),
			Mode:       "0600",
		})
		if perr != nil {
			return 0, 0, perr
		}
		transferSeconds += put.DurationSeconds
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"duckdb", "-batch", dbPath, "IMPORT DATABASE '" + exportDir + "';"}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		line := verdictLine(stdout, stderr)
		if strings.Contains(line, "No files found") || strings.Contains(line, "Cannot open file") {
			return 0, 0, protoErr("source_corrupt", false,
				"the export directory is incomplete — a file every restore needs is missing: %s", line)
		}
		return 0, 0, protoErr("restore_failed", false, "importing the export failed: %s", line)
	}
	return transferSeconds, val.DurationSeconds, nil
}

// healthcheckRequest is the §6.3 request payload. The restored database
// is reached through the connection provision returned; there is no
// server-side state to consult.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored database still serves queries
// (§6.3). The query counts the catalog, which forces a checksummed read
// through the engine — DuckDB refuses invalid files at open (measured),
// so the verdict is trustworthy without the sibling engines' deeper
// probes. An unhealthy database is a valid result, not an operation
// error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no restored database")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"duckdb", "-batch", "-list", "-noheader", req.Connection.Database, vetQuery}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("database file serves queries; %s tables", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("duckdb exited %d: %s", val.ExitCode, firstLine(stderr))
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// verdictLine picks the engine's own words for a refusal message: the
// first stderr line, or the last stdout line when stderr stayed silent.
func verdictLine(stdout, stderr []byte) string {
	if line := firstLine(stderr); line != "" {
		return line
	}
	return lastLine(stdout)
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

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ReplaceAll(strings.TrimSpace(s), `"`, "'")
}
