package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	adapterName    = "cassandra"
	adapterVersion = "0.1.0"

	// workDirName is created under the provider's scratch directory.
	workDirName = "probavi-cassandra"
	// tarName is where the archive kind places the artifact before
	// unpacking it.
	tarName = "snapshot.tar"

	// defaultPort is the CQL native port the restored node serves on,
	// bound to loopback. No TLS and no auth: a Probavi sandbox is
	// zero-ingress (--network none, no ports expressible), which is the
	// only reason a bare port on restored production data is acceptable.
	defaultPort = 9042

	readinessBudget = 5 * time.Minute
	readinessPoll   = 2 * time.Second
)

// runnerScript absorbs the cqlsh dialect declaratively: cqlsh prints a
// decorated table — blank line, header, dash separator, padded rows, a
// "(N rows)" footer, sometimes a Warnings block (measured) — while the
// protocol requires undecorated tab-separated rows. The awk filter keeps
// exactly the value rows between the separator and the first blank line,
// trims the padding, and turns column pipes into tabs; pipefail carries
// cqlsh's own exit code through the pipe. Measured end to end: a count
// yields the bare number, a two-column row yields tab-separated values,
// and a CQL error exits 2.
const runnerScript = `set -o pipefail
cqlsh --no-color -k "$1" -e "$2" | awk '/^-+[-+]*$/{d=1;next} d&&NF==0{exit} d{gsub(/^ +| +$/,""); gsub(/ *\| */,"\t"); print}'`

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "cassandra"},
		"sources": []map[string]any{
			{"kind": "cassandra_snapshot_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "cassandra_snapshot", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "cassandra_snapshot_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Checks are CQL. The dialect — including cqlsh's decorated
			// output — is absorbed here declaratively (see runnerScript),
			// so the core's generating built-ins apply unchanged;
			// {{database}} resolves to the restored keyspace provision
			// returned as connection.database (§6.1).
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

// opProvision restores the snapshot into the idle sandbox: preflight,
// node preparation (the loopback fixes a zero-ingress sandbox needs,
// measured), transfer (unpacking the archive kind), a background start,
// readiness, then per table the backup's own schema and an sstableloader
// stream — and finally a first read of every restored table, because the
// loader streams corrupted data without a word and the damage only
// surfaces when read (measured).
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
		"tables", len(src.census.tables))

	if perr := checkEngine(ctx, c); perr != nil {
		return nil, perr
	}
	if perr := prepareNode(ctx, c); perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	logPath := path.Join(workDir, "cassandra.log")
	transferSeconds, unpackSeconds, dataRoot, perr := transferArtifact(ctx, c, src, workDir)
	if perr != nil {
		return nil, perr
	}

	tables := src.census.tables
	discoverSeconds := 0.0
	if len(tables) == 0 && src.tarball {
		tables, discoverSeconds, perr = discoverTables(ctx, c, dataRoot)
		if perr != nil {
			return nil, perr
		}
	}

	readySeconds, perr := startEngine(ctx, c, logPath)
	if perr != nil {
		return nil, perr
	}
	restoreSeconds, perr := restoreTables(ctx, c, dataRoot, tables)
	if perr != nil {
		return nil, perr
	}
	probeSeconds, perr := probeTables(ctx, c, tables)
	if perr != nil {
		return nil, perr
	}
	logger.Info("snapshot restored and verified",
		"tables", len(tables), "ready_seconds", readySeconds)

	database := ""
	if len(tables) > 0 {
		database = tables[0].keyspace
	}
	return map[string]any{
		"connection": map[string]any{
			"scheme": "cassandra", "host": "127.0.0.1", "port": defaultPort,
			// The restored keyspace the declared runner's -k consumes;
			// with several keyspaces, the alphabetically first — checks
			// against the others use qualified names (README).
			"database": database, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// The newest instant the snapshot's own manifests claim
			// (see source.go); nil when the artifact stated nothing.
			"created_at": formatCreatedAt(src.census.maxCreatedMs),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      unpackSeconds + discoverSeconds + restoreSeconds + probeSeconds,
		},
		"state": map[string]any{"work_dir": workDir, "data_root": dataRoot},
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
// runs on. The official cassandra images pass a non-cassandra command
// through their entrypoint, so `command: sleep infinity` idles them with
// no wrapper (measured) — an image that lacks the tools is named.
func checkEngine(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c",
		"command -v cassandra >/dev/null && command -v sstableloader >/dev/null && exec cqlsh --version"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image lacks the Cassandra toolchain (cassandra, sstableloader, cqlsh over "+
				"bash): use an official cassandra image with command: sleep infinity (%s)",
			firstLine(stderr))
	}
	return nil
}

// prepareNode makes a single node startable in a zero-ingress sandbox:
// the daemon refuses to start when the container hostname does not
// resolve, and the image entrypoint's own address defaults break without
// a network (both measured) — so the hostname is mapped to loopback and
// every address in cassandra.yaml pinned to 127.0.0.1.
const prepareScript = `grep -q "$(hostname)" /etc/hosts || echo "127.0.0.1 $(hostname)" >> /etc/hosts
exec sed -i -e "s/^listen_address:.*/listen_address: 127.0.0.1/" \
  -e "s/^rpc_address:.*/rpc_address: 127.0.0.1/" \
  -e "s/- seeds:.*/- seeds: 127.0.0.1/" /etc/cassandra/cassandra.yaml`

func prepareNode(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", prepareScript}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare node: %s", firstLine(stderr))
	}
	return nil
}

// transferArtifact moves the artifact into the sandbox: an archive is
// placed and unpacked, a tree is recreated file by file. It returns the
// directory whose children are the keyspace directories.
func transferArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir string) (transferSeconds, unpackSeconds float64, dataRoot string, perr *protoError) {
	if src.tarball {
		return unpackArchive(ctx, c, src.path, workDir)
	}
	dataRoot = path.Join(workDir, "snapshot")
	transferSeconds, perr = transferTree(ctx, c, src.path, dataRoot)
	return transferSeconds, 0, dataRoot, perr
}

// rootScript locates the keyspace directories after unpacking: a
// collected snapshot tars either the keyspaces at its root or one
// wrapping directory above them, so the script descends exactly one
// level when the root shows no <keyspace>/<table>/schema.cql and exactly
// one subdirectory.
const rootScript = `d="$1"
set -- "$d"/*/*/schema.cql
if [ ! -e "$1" ]; then
  set -- "$d"/*/
  if [ "$#" -eq 1 ] && [ -d "${1%/}" ]; then d="${1%/}"; fi
fi
printf '%s\n' "$d"`

func unpackArchive(ctx context.Context, c *core, hostPath, workDir string) (transferSeconds, unpackSeconds float64, dataRoot string, perr *protoError) {
	extractDir := path.Join(workDir, "extract")
	if perr := mkdirAll(ctx, c, extractDir); perr != nil {
		return 0, 0, "", perr
	}
	tarPath := path.Join(workDir, tarName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: tarPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, "", perr
	}
	unpack, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"tar", "-xf", tarPath, "-C", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	if unpack.ExitCode != 0 {
		return 0, 0, "", protoErr("source_corrupt", false,
			"tar could not unpack the archive: %s", firstLine(stderr))
	}
	locate, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", rootScript, "bash", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	dataRoot = strings.TrimSpace(firstLine(stdout))
	if locate.ExitCode != 0 || dataRoot == "" {
		dataRoot = extractDir
	}
	return put.DurationSeconds, unpack.DurationSeconds + locate.DurationSeconds, dataRoot, nil
}

// transferTree recreates the snapshot tree inside the sandbox: one mkdir
// for the directory skeleton, one put_file per file.
func transferTree(ctx context.Context, c *core, hostDir, dataRoot string) (float64, *protoError) {
	dirs := []string{dataRoot}
	files := []string{}
	err := filepath.WalkDir(hostDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostDir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path.Join(dataRoot, filepath.ToSlash(rel)))
		} else if d.Type().IsRegular() {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return 0, protoErr("source_unreadable", false, "read backup directory: %v", err)
	}
	if perr := mkdirAll(ctx, c, dirs...); perr != nil {
		return 0, perr
	}
	total := 0.0
	for _, rel := range files {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: filepath.Join(hostDir, filepath.FromSlash(rel)),
			DestPath:   path.Join(dataRoot, rel),
			Mode:       "0600",
		})
		if perr != nil {
			return 0, perr
		}
		total += put.DurationSeconds
	}
	return total, nil
}

func mkdirAll(ctx context.Context, c *core, dirs ...string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: append([]string{"mkdir", "-p"}, dirs...)})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare work directory: %s", firstLine(stderr))
	}
	return nil
}

// discoverTables enumerates the unpacked keyspace/table directories when
// the host could not walk the archive. The exec's exit code carries the
// only refusal — an unpacked tree with no such directories is not a
// collected snapshot — and every discovered name still passes the same
// gate host-collected names do, because these strings reach composed CQL.
func discoverTables(ctx context.Context, c *core, dataRoot string) ([]tableRef, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", `cd "$1" && exec ls -d */*/`, "bash", dataRoot}})
	if perr != nil {
		return nil, 0, perr
	}
	if val.ExitCode != 0 {
		return nil, 0, protoErr("source_corrupt", false,
			"the unpacked archive holds no keyspace/table directories — not a collected snapshot: %s",
			firstLine(stderr))
	}
	lines := strings.Split(string(stdout), "\n")
	tables := make([]tableRef, 0, len(lines))
	for _, line := range lines {
		parts := strings.Split(strings.Trim(strings.TrimSpace(line), "/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		ref := tableRef{keyspace: parts[0], table: parts[1]}
		if perr := judgeName("keyspace", ref.keyspace); perr != nil {
			return nil, 0, perr
		}
		if perr := judgeName("table", ref.table); perr != nil {
			return nil, 0, perr
		}
		tables = append(tables, ref)
	}
	sortTables(tables)
	return tables, val.DurationSeconds, nil
}

// startEngine launches the node and waits until it answers CQL —
// readiness in the §7 sense. Between polls it watches the server's own
// log for a startup failure, so a node that died instantly fails the
// drill in seconds with its own reason instead of burning the budget.
func startEngine(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	start, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", "exec cassandra -R > " + logPath + " 2>&1"}})
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the node failed to launch: %s", firstLine(stderr))
	}
	readySeconds, perr := awaitReady(ctx, c, logPath)
	if perr != nil {
		return 0, describeStartFailure(ctx, c, logPath, perr)
	}
	return start.DurationSeconds + readySeconds, nil
}

func awaitReady(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	begin := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"cqlsh", "--no-color", "-e", "SELECT release_version FROM system.local;"},
			TimeoutSeconds: 15,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(begin).Seconds(), nil
		}
		if fatal, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"grep", "-qE",
			"Exiting|Fatal|Exception encountered during startup", logPath}}); perr == nil && fatal.ExitCode == 0 {
			return 0, protoErr("engine_not_ready", true, "the node exited during startup")
		}
		if time.Since(begin) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"the node did not answer CQL within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// describeStartFailure enriches a readiness timeout with the node's own
// last error line.
func describeStartFailure(ctx context.Context, c *core, logPath string, perr *protoError) *protoError {
	if perr.Code != "engine_not_ready" {
		return perr
	}
	val, stdout, _, eperr := c.exec(ctx, execArgs{Argv: []string{"tail", "-n", "30", logPath}})
	if eperr != nil || val.ExitCode != 0 {
		return perr
	}
	line := lastErrorLine(stdout)
	if line == "" {
		return perr
	}
	return protoErr("restore_failed", false, "the node failed to start: %s", line)
}

// lastErrorLine picks the last log line that reads like the node's own
// failure report.
func lastErrorLine(log []byte) string {
	found := ""
	for _, line := range strings.Split(string(log), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "exception") {
			found = strings.TrimSpace(line)
		}
	}
	return strings.ReplaceAll(found, `"`, "'")
}

// restoreTables recreates each table from the backup's own schema.cql
// and streams its sstables into the node. The keyspace itself is not in
// the backup's claims (schema.cql carries the table DDL only, measured),
// so it is created drill-locally with replication factor 1 — the honest
// setting for a single-node drill, stated in the README.
func restoreTables(ctx context.Context, c *core, dataRoot string, tables []tableRef) (float64, *protoError) {
	total := 0.0
	for _, keyspace := range keyspacesOf(tables) {
		val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"cqlsh", "--no-color", "-e",
			"CREATE KEYSPACE IF NOT EXISTS " + keyspace +
				" WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1};"}})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode != 0 {
			return 0, protoErr("restore_failed", false,
				"creating keyspace %s failed: %s", keyspace, firstLine(stderr))
		}
		total += val.DurationSeconds
	}
	for _, ref := range tables {
		seconds, perr := restoreTable(ctx, c, dataRoot, ref)
		if perr != nil {
			return 0, perr
		}
		total += seconds
	}
	return total, nil
}

func restoreTable(ctx context.Context, c *core, dataRoot string, ref tableRef) (float64, *protoError) {
	tableDir := path.Join(dataRoot, ref.keyspace, ref.table)
	schema, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"cqlsh", "--no-color", "-f", path.Join(tableDir, schemaName)}})
	if perr != nil {
		return 0, perr
	}
	if schema.ExitCode != 0 {
		return 0, mapSchemaFailure(ref, stderr)
	}
	load, _, loadErr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sstableloader", "-d", "127.0.0.1", tableDir}})
	if perr != nil {
		return 0, perr
	}
	if load.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"sstableloader failed for %s: %s", ref, firstLine(loadErr))
	}
	return schema.DurationSeconds + load.DurationSeconds, nil
}

// mapSchemaFailure classifies a schema the engine refused. A snapshot
// from a newer Cassandra states table options an older engine does not
// know (measured: 5.0's allow_auto_snapshot on 4.1 — "Unknown property")
// — a drill config pairing a backup with a sandbox image that cannot
// restore it.
func mapSchemaFailure(ref tableRef, stderr []byte) *protoError {
	line := firstLine(stderr)
	if strings.Contains(line, "Unknown property") || strings.Contains(line, "SyntaxException") {
		return protoErr("invalid_request", false,
			"the backup's own schema for %s does not parse on this engine (%s): a snapshot from "+
				"a newer Cassandra can state options an older engine does not know — use a "+
				"cassandra image at least as new as the backup's origin", ref, line)
	}
	return protoErr("restore_failed", false, "applying the schema for %s failed: %s", ref, line)
}

// probeTables reads one row of every restored table. The loader streams
// corrupted data without a word, and the damage surfaces only at first
// read (measured) — so the drill reads before it reports.
func probeTables(ctx context.Context, c *core, tables []tableRef) (float64, *protoError) {
	total := 0.0
	for _, ref := range tables {
		val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"cqlsh", "--no-color", "-e",
			"SELECT * FROM " + ref.String() + " LIMIT 1;"}})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode != 0 {
			return 0, protoErr("source_corrupt", false,
				"reading restored table %s failed: %s — the engine refused what the loader "+
					"streamed, which is how corrupted backup data surfaces (measured)",
				ref, firstLine(stderr))
		}
		total += val.DurationSeconds
	}
	return total, nil
}

func keyspacesOf(tables []tableRef) []string {
	seen := map[string]bool{}
	var keyspaces []string
	for _, ref := range tables {
		if !seen[ref.keyspace] {
			seen[ref.keyspace] = true
			keyspaces = append(keyspaces, ref.keyspace)
		}
	}
	sort.Strings(keyspaces)
	return keyspaces
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored node still answers CQL (§6.3). An
// unhealthy node is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"cqlsh", "--no-color", "-e",
		"SELECT release_version FROM system.local;"}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := "node answers CQL"
	if !healthy {
		detail = fmt.Sprintf("cqlsh exited %d: %s", val.ExitCode, firstLine(stderr))
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
