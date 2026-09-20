package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"
)

const (
	adapterName    = "scylladb"
	adapterVersion = "0.2.0"

	// workDirName is created under the provider's scratch directory.
	workDirName = "probavi-scylladb"
	// tarName is where the archive kind places the artifact before
	// unpacking it.
	tarName = "snapshot.tar"

	// dataRoot is where the engine keeps its keyspace directories, and
	// therefore where a restore has to land: refresh reads an sstable
	// only from the table's own upload/ directory, never from a staging
	// path of the adapter's choosing.
	engineDataRoot = "/var/lib/scylla/data"

	// defaultHost is the loopback address the node actually serves on.
	// Not 127.0.0.1: the image's entrypoint pins listen_address,
	// rpc_address and the seed to 127.0.0.2, and a connection to
	// 127.0.0.1 is refused (measured). The address reaches the evidence
	// record, so it states where the drill really talked.
	defaultHost = "127.0.0.2"
	defaultPort = 9042

	readinessBudget = 5 * time.Minute
	readinessPoll   = 2 * time.Second
)

// runnerScript absorbs the cqlsh dialect declaratively: cqlsh prints a
// decorated table — blank line, header, dash separator, padded rows, a
// "(N rows)" footer, sometimes a Warnings block — while the protocol
// requires undecorated tab-separated rows. The awk filter keeps exactly
// the value rows between the separator and the first blank line, trims
// the padding, and turns column pipes into tabs; pipefail carries cqlsh's
// own exit code through the pipe.
//
// The script also rewrites one statement the core generates, for the same
// reason the Cassandra adapter does: table_exists probes with
// `SELECT count(*) FROM <table> WHERE 1=0`, and CQL has no such predicate
// — a WHERE clause must name a column. `DESCRIBE TABLE` is the engine's
// own way to ask the question, exits 0 for a table that exists and
// non-zero for one that does not, and answers from the schema, so nothing
// is scanned and no row of restored production data reaches stdout. The
// rewrite is guarded by the whole statement, so a check of the operator's
// own can never be caught by it.
//
// cqlsh is invoked without a host because the image's own cqlsh defaults
// to the address the node serves on; the integration suite proves the
// runner end to end rather than trusting that.
const runnerScript = `set -o pipefail
probe='^SELECT count\(\*\) FROM "[A-Za-z_][A-Za-z0-9_]*"(\."[A-Za-z_][A-Za-z0-9_]*")? WHERE 1=0$'
stmt=$2
if printf '%s' "$stmt" | grep -Eq "$probe"; then
  stmt=$(printf '%s' "$stmt" | sed -E 's/^SELECT count\(\*\) FROM (.*) WHERE 1=0$/DESCRIBE TABLE \1/')
fi
cqlsh --no-color -k "$1" -e "$stmt" | awk '/^-+[-+]*$/{d=1;next} d&&NF==0{exit} d{gsub(/^ +| +$/,""); gsub(/ *\| */,"\t"); print}'`

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "scylladb"},
		"sources": []map[string]any{
			{"kind": "scylladb_snapshot_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "scylladb_snapshot", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "scylladb_snapshot_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
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

// opProvision restores the snapshot into the sandbox.
//
// The engine starts itself here, which is the one structural difference
// from the Cassandra adapter and is forced by the image: its entrypoint
// appends the container command to the server's own argv, so the
// `command: sleep infinity` that idles a cassandra image makes this one
// exit with "too many positional options" before it serves anything
// (measured). The sandbox therefore runs the engine, the drill config
// passes the engine's own flags as the command, and this operation waits
// for readiness rather than creating it.
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
	readySeconds, perr := awaitReady(ctx, c)
	if perr != nil {
		return nil, perr
	}
	quietenHousekeeping(ctx, c, logger)

	workDir := path.Join(scratch, workDirName)
	transferSeconds, unpackSeconds, stageRoot, perr := transferArtifact(ctx, c, src, workDir)
	if perr != nil {
		return nil, perr
	}

	tables := src.census.tables
	discoverSeconds := 0.0
	if len(tables) == 0 && src.tarball {
		tables, discoverSeconds, perr = discoverTables(ctx, c, stageRoot)
		if perr != nil {
			return nil, perr
		}
	}

	restoreSeconds, perr := restoreTables(ctx, c, stageRoot, tables)
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
			"scheme": "scylla", "host": defaultHost, "port": defaultPort,
			// The restored keyspace the declared runner's -k consumes;
			// with several keyspaces, the alphabetically first — checks
			// against the others use qualified names (README).
			"database": database, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// The instant the snapshot's own manifests state, in epoch
			// seconds — exact, with no timezone to declare.
			"created_at": formatCreatedAt(src.census.maxCreatedMs),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      unpackSeconds + discoverSeconds + restoreSeconds + probeSeconds,
		},
		"state": map[string]any{"work_dir": workDir, "stage_root": stageRoot},
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
// runs on, and names an image that does not.
func checkEngine(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c",
		"command -v scylla >/dev/null && command -v nodetool >/dev/null && exec cqlsh --version"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image lacks the ScyllaDB toolchain (scylla, nodetool, cqlsh over bash): "+
				"use an official scylladb/scylla image and pass the engine's own flags as the "+
				"sandbox command, never `sleep infinity` (%s)", firstLine(stderr))
	}
	return nil
}

// awaitReady waits until the node answers CQL — readiness in the §7
// sense, and honest here: a create succeeds at the same instant the first
// query answers (measured), so there is no gap between "answers" and
// "usable" of the kind other engines have.
func awaitReady(ctx context.Context, c *core) (float64, *protoError) {
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
		if time.Since(begin) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"the node did not answer CQL within %s — check that the sandbox command carries "+
					"the engine's own flags (for example --smp 1) and that the sandbox has more "+
					"than 2 GiB, below which the server aborts on startup", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// quietenHousekeeping stops the image's version-check service, which
// wakes daily and posts an installation UUID to the vendor. A drill
// sandbox is zero-ingress so it cannot reach anything, and the adapter
// stops it anyway rather than relying on that: a restore of production
// data should not carry a reporting process at all. Best effort by
// design — an image without supervisord is not an error, and this is not
// a reason to fail a drill.
func quietenHousekeeping(ctx context.Context, c *core, logger *slog.Logger) {
	val, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c",
		"command -v supervisorctl >/dev/null && exec supervisorctl stop scylla-housekeeping"}})
	if perr != nil || val.ExitCode != 0 {
		logger.Info("housekeeping service not stopped; the sandbox has no egress in any case")
	}
}

// transferArtifact moves the artifact into the sandbox: an archive is
// placed and unpacked, a tree is recreated file by file. It returns the
// directory whose children are the keyspace directories.
func transferArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir string) (transferSeconds, unpackSeconds float64, stageRoot string, perr *protoError) {
	if src.tarball {
		return unpackArchive(ctx, c, src.path, workDir)
	}
	stageRoot = path.Join(workDir, "snapshot")
	transferSeconds, perr = transferTree(ctx, c, src.path, stageRoot)
	return transferSeconds, 0, stageRoot, perr
}

// rootScript locates the keyspace directories after unpacking: a
// collected snapshot tars either the keyspaces at its root or one
// wrapping directory above them, so the script descends exactly one
// level when the root shows no <keyspace>/<table>/schema.cql and exactly
// one subdirectory.
// noExtractorExit is the status unpackScript reserves for "this image
// has nothing that can read a tar archive". It is distinct from every
// exit an extractor that ran could produce, so the adapter can tell a
// sandbox that cannot do the work from a backup that is damaged.
const noExtractorExit = 127

// unpackScript extracts the archive with whatever the image has.
//
// `tar` is the obvious tool and seven of this repository's eight
// archive-reading adapters find it in their own verified image. This one
// does not: `scylladb/scylla:2026.3.1` has no tar anywhere on its
// filesystem (measured — no tar, no bsdtar, no busybox), which made the
// archive kind unusable on the only image the manifest verifies, and
// reported it as `source_corrupt` — a verdict about the operator's
// backup — because a missing program exits non-zero like a refused
// archive does (issue #327).
//
// What the image does have is Python, so that is the fallback. The
// extraction is filtered: a backup file is attacker-controlled input
// (SECURITY.md), and `data` is the filter that refuses absolute paths,
// parent-directory escapes and device nodes. A Python too old to offer
// it is not used at all rather than used unfiltered — the point of the
// fallback is to extract safely, not merely to extract.
const unpackScript = `set -u
archive=$1; dest=$2
if command -v tar >/dev/null 2>&1; then
  tar -xf "$archive" -C "$dest"
  exit $?
fi
for py in python3 python; do
  command -v "$py" >/dev/null 2>&1 || continue
  "$py" -c 'import tarfile,sys; sys.exit(0 if hasattr(tarfile,"data_filter") else 1)' >/dev/null 2>&1 || continue
  "$py" -c 'import sys,tarfile
with tarfile.open(sys.argv[1]) as t:
    t.extractall(sys.argv[2], filter="data")' "$archive" "$dest"
  exit $?
done
echo "no usable extractor: neither tar nor a python with tarfile filtering is on PATH" >&2
exit 127`

const rootScript = `d="$1"
set -- "$d"/*/*/schema.cql
if [ ! -e "$1" ]; then
  set -- "$d"/*/
  if [ "$#" -eq 1 ] && [ -d "${1%/}" ]; then d="${1%/}"; fi
fi
printf '%s\n' "$d"`

func unpackArchive(ctx context.Context, c *core, hostPath, workDir string) (transferSeconds, unpackSeconds float64, stageRoot string, perr *protoError) {
	extractDir := path.Join(workDir, "extract")
	if perr := mkdirAll(ctx, c, extractDir); perr != nil {
		return 0, 0, "", perr
	}
	tarPath := path.Join(workDir, tarName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: tarPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, "", perr
	}
	unpack, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", unpackScript, "bash", tarPath, extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	if unpack.ExitCode == noExtractorExit {
		// The sandbox could not run an extractor at all. That is not a
		// verdict on the backup, and saying it was would send an
		// operator looking for damage in a good one.
		return 0, 0, "", protoErr("invalid_request", false,
			"this sandbox image cannot unpack an archive: %s. The stock ScyllaDB image ships no "+
				"tar (measured on 2026.3.1), so either use kind scylladb_snapshot with the "+
				"collected tree, or an image carrying tar or a python with tarfile filtering",
			firstLine(stderr))
	}
	if unpack.ExitCode != 0 {
		return 0, 0, "", protoErr("source_corrupt", false,
			"the archive could not be unpacked: %s", firstLine(stderr))
	}
	locate, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"bash", "-c", rootScript, "bash", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	stageRoot = strings.TrimSpace(firstLine(stdout))
	if locate.ExitCode != 0 || stageRoot == "" {
		stageRoot = extractDir
	}
	return put.DurationSeconds, unpack.DurationSeconds + locate.DurationSeconds, stageRoot, nil
}

// transferTree recreates the snapshot tree inside the sandbox: one mkdir
// for the directory skeleton, one put_file per file.
func transferTree(ctx context.Context, c *core, hostDir, stageRoot string) (float64, *protoError) {
	dirs, files, perr := walkTree(hostDir, stageRoot)
	if perr != nil {
		return 0, perr
	}
	if perr := mkdirAll(ctx, c, dirs...); perr != nil {
		return 0, perr
	}
	total := 0.0
	for _, f := range files {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: f.host, DestPath: f.dest, Mode: "0600",
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
// the host could not walk the archive. Every discovered name still passes
// the same gate host-collected names do, because these strings reach
// composed CQL.
func discoverTables(ctx context.Context, c *core, stageRoot string) ([]tableRef, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", `cd "$1" && exec ls -d */*/`, "bash", stageRoot}})
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

// restoreTables recreates each keyspace and table from the backup's own
// schema, then loads every table's sstables.
//
// The keyspace is created with NetworkTopologyStrategy rather than
// SimpleStrategy, which is not a style choice: this engine places data in
// tablets by default, and a tablet-enabled keyspace refuses SimpleStrategy
// outright — `ConfigurationException: SimpleStrategy doesn't support
// tablet replication` (measured). Replication factor 1 is the honest
// setting for a single-node drill, and the backup does not state one:
// schema.cql carries the table DDL alone, never the keyspace (measured),
// so the drill's replication is the sandbox's own and the README says so.
func restoreTables(ctx context.Context, c *core, stageRoot string, tables []tableRef) (float64, *protoError) {
	total := 0.0
	for _, keyspace := range keyspacesOf(tables) {
		val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"cqlsh", "--no-color", "-e",
			"CREATE KEYSPACE IF NOT EXISTS " + keyspace +
				" WITH replication = {'class': 'NetworkTopologyStrategy', 'replication_factor': 1};"}})
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
		seconds, perr := restoreTable(ctx, c, stageRoot, ref)
		if perr != nil {
			return 0, perr
		}
		total += seconds
	}
	return total, nil
}

// stageScript copies a table's sstables into the live table's own upload
// directory, which is the only place refresh reads from. The table
// directory carries an id the engine generated when the schema was
// applied, so it is found rather than composed. A staged set with no
// sstable at all exits distinctly: refresh would accept it and report
// success (see restoreTable).
// tableDirScript resolves the live table directory. Its id is the one
// the engine generated when the schema was applied, so the directory is
// found rather than composed — and the path lives here once rather than
// in each script that needs it.
const tableDirScript = `d=$(ls -d "` + engineDataRoot + `/$ks/$tbl-"*/ 2>/dev/null | head -1)`

const stageScript = `set -u
ks=$1; tbl=$2; srcdir=$3
` + tableDirScript + `
if [ -z "$d" ]; then echo "no data directory for $ks.$tbl" >&2; exit 3; fi
mkdir -p "$d/upload"
n=0
for f in "$srcdir"/*-big-*; do
  [ -e "$f" ] || continue
  cp -- "$f" "$d/upload/" || exit 5
  n=$((n+1))
done
if [ "$n" -eq 0 ]; then echo "no sstable files staged for $ks.$tbl" >&2; exit 4; fi
chown -R scylla:scylla "$d/upload" 2>/dev/null || true
printf '%s\n' "$n"`

// restoreTable applies the backup's own schema for one table, stages its
// sstables, and asks the engine to take them.
//
// The verdict is deliberately not the refresh exit code. Pointed at an
// empty upload directory, refresh loads nothing and exits 0 (measured) —
// the same shape as the Cassandra loader's silent no-op — so an empty
// stage is refused here, before refresh is asked, and what the table
// actually serves is read back afterwards (probeTables).
func restoreTable(ctx context.Context, c *core, stageRoot string, ref tableRef) (float64, *protoError) {
	tableDir := path.Join(stageRoot, ref.keyspace, ref.table)
	schema, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"cqlsh", "--no-color", "-f", path.Join(tableDir, schemaName)}})
	if perr != nil {
		return 0, perr
	}
	if schema.ExitCode != 0 {
		return 0, mapSchemaFailure(ref, stderr)
	}
	stage, _, stageErr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", stageScript, "bash", ref.keyspace, ref.table, tableDir}})
	if perr != nil {
		return 0, perr
	}
	if stage.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"staging %s for refresh failed: %s", ref, firstLine(stageErr))
	}
	load, _, loadErr, perr := c.exec(ctx, execArgs{
		Argv: []string{"nodetool", "refresh", ref.keyspace, ref.table}})
	if perr != nil {
		return 0, perr
	}
	if load.ExitCode != 0 {
		// A damaged sstable is loud here: the engine validates the
		// compressed chunks as it reads them and names the file and the
		// offset (measured).
		return 0, protoErr("source_corrupt", false,
			"the engine refused the sstables for %s: %s", ref, firstLine(loadErr))
	}
	return schema.DurationSeconds + stage.DurationSeconds + load.DurationSeconds, nil
}

// mapSchemaFailure classifies a schema the engine refused: a snapshot
// from a newer engine can state table options an older one does not know,
// which is a drill config pairing a backup with a sandbox image that
// cannot restore it.
func mapSchemaFailure(ref tableRef, stderr []byte) *protoError {
	line := firstLine(stderr)
	if strings.Contains(line, "Unknown property") || strings.Contains(line, "SyntaxException") ||
		strings.Contains(line, "ConfigurationException") {
		return protoErr("invalid_request", false,
			"the backup's own schema for %s does not parse on this engine (%s): a snapshot from "+
				"a newer ScyllaDB can state options an older engine does not know — use an image "+
				"at least as new as the backup's origin", ref, line)
	}
	return protoErr("restore_failed", false, "applying the schema for %s failed: %s", ref, line)
}

// probeTables reads one row of every restored table, because refresh's
// exit code says only that the engine accepted the files.
func probeTables(ctx context.Context, c *core, tables []tableRef) (float64, *protoError) {
	total := 0.0
	for _, ref := range tables {
		val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"cqlsh", "--no-color", "-e",
			"SELECT * FROM " + ref.String() + " LIMIT 1;"}})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode != 0 {
			return 0, protoErr("source_corrupt", false,
				"reading restored table %s failed: %s", ref, firstLine(stderr))
		}
		total += val.DurationSeconds
		if !strings.Contains(string(stdout), emptyResultFooter) {
			continue
		}
		// A table that reads nothing is only news when the artifact says
		// it held rows that expire — see retention.go.
		ttl, seconds, perr := declaredTTL(ctx, c, ref)
		if perr != nil {
			return 0, perr
		}
		total += seconds
		if ttl > 0 {
			return 0, refusedExpiredTable(ref, ttl)
		}
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
	sortStrings(keyspaces)
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
