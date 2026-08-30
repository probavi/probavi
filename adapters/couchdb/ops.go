package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
)

const (
	adapterName    = "couchdb"
	adapterVersion = "0.1.0"

	// dataDir is where CouchDB keeps its databases inside the official
	// image, and where a data-directory restore must land.
	dataDir = "/opt/couchdb/data"
	// registryFile is the database registry CouchDB reads at startup to
	// learn which databases exist. A data-directory copy without it serves
	// nothing (measured), which is why source.go refuses one.
	registryFile = "_dbs.couch"

	// workDirName is created under the provider's scratch directory for
	// the artifact and the restore's working files.
	workDirName = "probavi-couchdb"
	// backupFileName is where a couchbackup artifact lands.
	backupFileName = "backup.jsonl"
	// dataTarName is where a data-directory tar lands.
	dataTarName = "data.tar"

	// endpoint is the loopback address the engine serves on. Nothing
	// listens outside the sandbox: the image binds 5984 and the drill
	// never publishes it (measured under --network none).
	endpoint = "http://127.0.0.1:5984"
	// defaultUser is the admin account the sandbox creates. CouchDB 3.x
	// has no admin party, so a drill always authenticates.
	defaultUser = "admin"
	// defaultDatabase is where the backup kinds replay, and what the
	// data-directory kinds require to be present afterwards.
	defaultDatabase = "restored"
	// sandboxPassword is the admin password of the drill engine. It is a
	// documented CONSTANT, not a secret: CouchDB 3.x refuses to run
	// without an authenticated administrator, and the ephemeral
	// core-generated secret cannot be used — its value must never cross
	// the protocol (§2.5), yet setting an engine password requires exactly
	// that. So the adapter starts the engine itself with this fixed value,
	// the CouchDB equivalent of the postgres adapter's pg_hba trust
	// overwrite and the mssql adapter's sa constant: publicly known
	// access, confined to a sandbox with zero ingress (--network none, no
	// ports expressible). The credential never protects anything
	// reachable.
	sandboxPassword = "probavi-drill-sandbox" //nolint:gosec // deliberately public constant, not a credential — see above

	// passwordVarName is the env entry the check runner and the restore
	// scripts read the constant from, so it never sits in an argument
	// list where the sandbox's process table would show it.
	passwordVarName = "COUCHDB_PASSWORD"
	// userVarName carries the admin account name to the image's own
	// entrypoint, which writes both into CouchDB's configuration.
	userVarName = "COUCHDB_USER"
)

// checkScript is the sql_runner's body. CouchDB speaks HTTP rather than
// SQL, so a check is what the glossary allows where there is no SQL: the
// engine's own client arguments. Here that is a path with a query string,
// relative to the restored database — `_all_docs?limit=0` for a document
// count, a view or a `_find` for anything else.
//
// The script exists because the runner contract (§6.1) wants an exit code
// and undecorated rows, and curl gives neither on its own: it exits 0 for
// a 404 as readily as for a 200. So the status code is the verdict, and
// the body is reduced to the one number a check asks for where CouchDB
// states one — `total_rows` or `doc_count` — and passed through otherwise.
const checkScript = `set -u
code=$(curl -s -o /tmp/probavi-check.out -w '%{http_code}' \
  --user "$1:${` + passwordVarName + `}" "` + endpoint + `/$2/$3")
case "$code" in
  2*) ;;
  *) printf 'couchdb answered %s\n' "$code" >&2; cat /tmp/probavi-check.out >&2; exit 1 ;;
esac
n=$(sed -n 's/.*"\(total_rows\|doc_count\)":\([0-9][0-9]*\).*/\2/p' /tmp/probavi-check.out | head -1)
if [ -n "$n" ]; then printf '%s\n' "$n"; else cat /tmp/probavi-check.out; fi`

// probePayload reports identity and capabilities (§6.1).
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "couchdb"},
		"sources": []map[string]any{
			{"kind": "couchdb_data_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "couchdb_data", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "couchbackup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "couchbackup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			"argv": []string{"sh", "-c", checkScript, "sh", "{{user}}", "{{database}}", "{{sql}}"},
			"env":  map[string]string{passwordVarName: sandboxPassword},
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

// opProvision restores the backup into the sandbox. The sandbox must start
// idle (docker: command: sleep infinity): a data-directory restore has to
// put files in place before the engine reads them, and CouchDB caches its
// database registry at startup, so files placed under a running server are
// invisible to it (measured: HTTP 404 for a database whose shards are on
// disk). The adapter therefore owns the engine's whole lifetime.
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
	database := option(req.Options, "database", defaultDatabase)
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes, "batches", src.batches)

	readySeconds, perr := checkSandbox(ctx, c)
	if perr != nil {
		return nil, perr
	}
	workDir := path.Join(scratch, workDirName)
	if perr := prepareWorkDir(ctx, c, workDir); perr != nil {
		return nil, perr
	}

	transferSeconds, restoreSeconds, perr := restore(ctx, c, src, workDir, user, database, logger)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "seconds", restoreSeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": 5984,
			"database": database, "user": user,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// Nothing in any CouchDB artifact dates the backup
			// (see source.go).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"work_dir": workDir, "database": database},
	}, nil
}

func option(options map[string]string, key, fallback string) string {
	if v := strings.TrimSpace(options[key]); v != "" {
		return v
	}
	return fallback
}

// checkSandbox verifies the sandbox is the idle one this adapter needs:
// a POSIX shell, curl, and an engine that is NOT already running. A
// sandbox whose image started CouchDB for us would have read an empty data
// directory, and a data-directory restore placed underneath it would be
// invisible (measured).
func checkSandbox(ctx context.Context, c *core) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c",
		`command -v curl >/dev/null || { echo "no curl in the sandbox image" >&2; exit 1; }
[ -d ` + dataDir + ` ] || { echo "no ` + dataDir + ` — this is not a couchdb image" >&2; exit 1; }`}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("invalid_request", false,
			"the sandbox is not a CouchDB image with a POSIX shell and curl, and this adapter "+
				"needs all three — the adapter README names the image and the idle command it "+
				"must start with (%s)", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

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

// restore drives one of the two shapes and returns the measured transfer
// and restore durations.
func restore(ctx context.Context, c *core, src *resolvedSource, workDir, user, database string,
	logger *slog.Logger) (transferSeconds, restoreSeconds float64, perr *protoError) {
	if src.form == formBackup {
		return restoreBackup(ctx, c, src, workDir, user, database)
	}
	return restoreDataDir(ctx, c, src, workDir, user, database, logger)
}

// startEngineScript launches CouchDB through the image's own entrypoint
// and waits for it to answer, then suspends the compaction daemon.
//
// Suspending smoosh is the issue #166 answer for this engine: CouchDB's
// compactor runs unbidden, and compaction is precisely the operation that
// drops old revisions and the bodies of deleted documents — it can only
// subtract from what the backup holds. Emptying its channels stops it
// enqueueing anything (measured). It is a suspension and not a rewrite:
// an explicit _compact still works, because what a drill must not do is
// let the engine decide, not stop the operator from asking.
const startEngineScript = `set -u
nohup /docker-entrypoint.sh /opt/couchdb/bin/couchdb > /tmp/probavi-couchdb.log 2>&1 &
i=0
while [ $i -lt 120 ]; do
  curl -sf --user "$1:${` + passwordVarName + `}" ` + endpoint + `/ >/dev/null 2>&1 && break
  i=$((i+1)); sleep 1
done
[ $i -lt 120 ] || { echo "couchdb did not answer within 120s" >&2; tail -20 /tmp/probavi-couchdb.log >&2; exit 1; }
for ch in db_channels view_channels; do
  curl -sf -X PUT -H 'Content-Type: application/json' -d '""' \
    --user "$1:${` + passwordVarName + `}" "` + endpoint + `/_node/_local/_config/smoosh/$ch" >/dev/null || {
      echo "could not suspend the compaction daemon ($ch)" >&2; exit 1; }
done`

// startEngine starts CouchDB and suspends compaction.
func startEngine(ctx context.Context, c *core, user string) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", startEngineScript, "sh", user},
		Env:  map[string]string{userVarName: user, passwordVarName: sandboxPassword},
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("restore_failed", false, "starting couchdb: %s", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// replayScript posts every batch of a couchbackup file to _bulk_docs and
// stops at the first batch the engine does not accept.
//
// One POST per line is all the format needs: couchbackup writes a header
// line and then one JSON array of documents per line, which is already the
// shape _bulk_docs takes (measured on 2.11.19). new_edits=false preserves
// the revisions the backup recorded rather than minting new ones — a
// restore that renumbered every revision would not be the database that
// was backed up.
//
// The batch count is passed in and checked: the format writes nothing at
// its end, so the only way to know the loop consumed the whole file is to
// have counted the lines before the transfer.
const replayScript = `set -u
user=$1; db=$2; file=$3; want=$4
curl -sf -X PUT --user "$user:${` + passwordVarName + `}" "` + endpoint + `/$db" >/dev/null || {
  echo "could not create database $db" >&2; exit 1; }
n=0
first=1
while IFS= read -r line; do
  [ -z "$line" ] && continue
  if [ "$first" = 1 ]; then first=0; continue; fi
  printf '{"new_edits":false,"docs":%s}' "$line" > /tmp/probavi-batch.json
  code=$(curl -s -o /tmp/probavi-batch.out -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' -d @/tmp/probavi-batch.json \
    --user "$user:${` + passwordVarName + `}" "` + endpoint + `/$db/_bulk_docs")
  case "$code" in
    2*) ;;
    *) printf 'batch %s refused with %s\n' "$((n+1))" "$code" >&2; head -c 300 /tmp/probavi-batch.out >&2; exit 1 ;;
  esac
  n=$((n+1))
done < "$file"
[ "$n" = "$want" ] || { printf 'restored %s batches of %s\n' "$n" "$want" >&2; exit 1; }`

// restoreBackup replays a couchbackup file into a fresh database.
func restoreBackup(ctx context.Context, c *core, src *resolvedSource, workDir, user, database string) (float64, float64, *protoError) {
	ready, perr := startEngine(ctx, c, user)
	if perr != nil {
		return 0, 0, perr
	}
	dest := path.Join(workDir, backupFileName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dest, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", replayScript, "sh", user, database, dest, strconv.Itoa(src.batches)},
		Env:  map[string]string{passwordVarName: sandboxPassword},
	})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, 0, protoErr("restore_failed", false, "replaying the backup: %s", firstLine(stderr))
	}
	if perr := assertRestored(ctx, c, user, database); perr != nil {
		return 0, 0, perr
	}
	return put.DurationSeconds, ready + val.DurationSeconds, nil
}

// placeDataScript empties the engine's data directory and unpacks the
// artifact into it, before the engine has read anything.
const placeDataScript = `set -u
src=$1
rm -rf ` + dataDir + `/* ` + dataDir + `/.[!.]* 2>/dev/null || true
case "$2" in
  tar) tar xf "$src" -C ` + dataDir + ` ;;
  dir) cp -a "$src"/. ` + dataDir + `/ ;;
esac
[ -f ` + dataDir + `/` + registryFile + ` ] || { echo "no ` + registryFile + ` after placing the data" >&2; exit 1; }
chown -R couchdb:couchdb ` + dataDir + ` 2>/dev/null || true`

// restoreDataDir places a data directory and then starts the engine on it.
func restoreDataDir(ctx context.Context, c *core, src *resolvedSource, workDir, user, database string,
	logger *slog.Logger) (float64, float64, *protoError) {
	mode, dest := "dir", path.Join(workDir, "data")
	if src.form == formDataTar {
		mode, dest = "tar", path.Join(workDir, dataTarName)
	}
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dest, Mode: "0600"})
	if perr != nil {
		return 0, 0, perr
	}
	logger.Info("artifact transferred", "seconds", put.DurationSeconds)

	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", placeDataScript, "sh", dest, mode}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, 0, protoErr("source_corrupt", false, "placing the data directory: %s", firstLine(stderr))
	}
	ready, perr := startEngine(ctx, c, user)
	if perr != nil {
		return 0, 0, perr
	}
	if perr := assertRestored(ctx, c, user, database); perr != nil {
		return 0, 0, perr
	}
	return put.DurationSeconds, val.DurationSeconds + ready, nil
}

// assertRestored is the restore's verdict, and it is the database's own
// document count rather than the fact that the engine answered.
//
// That distinction was measured. A shard file truncated at its tail leaves
// a database CouchDB opens without complaint — the file's header sits at
// its end, so the engine falls back to the last valid one — and serves
// with HTTP 200 while holding 280 documents of 500. "The server started"
// and "the database answers" are both true of a restore that lost nearly
// half the backup, so neither can be the verdict.
//
// What the count catches is a restore that produced nothing: an empty or
// absent database. What it cannot catch is a truncation that leaves the
// database present and shorter, because no CouchDB artifact states how
// much it should hold. That is the drill's own row-count check's job, and
// the adapter README says so rather than implying a fence it lacks.
func assertRestored(ctx context.Context, c *core, user, database string) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", checkScript, "sh", user, database, "_all_docs?limit=0"},
		Env:  map[string]string{passwordVarName: sandboxPassword},
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("restore_failed", false,
			"the restored database %s does not answer: %s", database, firstLine(stderr))
	}
	count := firstLine(stdout)
	if count == "0" {
		return protoErr("source_corrupt", false,
			"the restored database %s holds no documents: CouchDB opens a damaged artifact as "+
				"the smaller database its remaining bytes describe rather than refusing it, so "+
				"an empty restore is what a broken backup looks like here", database)
	}
	if count == "" {
		return protoErr("restore_failed", false,
			"the restored database %s did not report how many documents it holds, so nothing "+
				"about this restore can be relied on", database)
	}
	return nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
		User     string `json:"user"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored database still serves queries (§6.3).
// An unhealthy database is a valid result, not an operation error.
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
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", checkScript, "sh", user, req.Connection.Database, "_all_docs?limit=0"},
		Env:  map[string]string{passwordVarName: sandboxPassword},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("database serves queries; %s documents", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("couchdb check exited %d: %s", val.ExitCode, firstLine(stderr))
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
