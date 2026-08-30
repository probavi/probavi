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
	adapterName    = "qdrant"
	adapterVersion = "0.1.0"

	// engineDir is the image's working directory: the binary, its config
	// and the storage tree all live here.
	engineDir = "/qdrant"
	// engineBinary is started directly rather than through the image's
	// entrypoint.sh. That wrapper exists to restart the engine in
	// *recovery mode* after an OOM, which is an engine deciding on its
	// own to serve something other than what the backup held — the one
	// thing a drill must not allow. Recovery mode is off by default, and
	// starting the binary means it cannot be switched on behind the
	// drill's back either.
	engineBinary = engineDir + "/qdrant"

	// httpPort is the REST port Qdrant serves on. Nothing listens outside
	// the sandbox: the drill publishes no ports and runs under
	// --network none (measured).
	httpPort = "6333"

	// workDirName is created under the provider's scratch directory to
	// hold the artifact.
	workDirName = "probavi-qdrant"
	// snapshotFileName is where a snapshot artifact lands in the sandbox.
	snapshotFileName = "restore.snapshot"

	// defaultCollection is the collection a collection snapshot is
	// restored into, and the one the storage kinds require to be present
	// afterwards. A collection snapshot's file name does begin with the
	// collection it came from, but a renamed file would then silently
	// restore under the wrong name, so the adapter takes the name from
	// the drill config and never from the artifact.
	defaultCollection = "restored"
)

// checkScript is the sql_runner's body. Qdrant speaks HTTP rather than
// SQL, so a check is what the glossary allows where there is no SQL: the
// engine's own client arguments. Here that is a **path, optionally
// followed by a space and a JSON body** — a body turns the request into a
// POST, which is how Qdrant asks its most useful question, an exact and
// optionally filtered point count. A path that does not begin with "/" is
// relative to the restored collection.
//
// The script speaks HTTP with bash's /dev/tcp because the official image
// carries no HTTP client at all: no curl, no wget, no nc, no python3
// (measured). It carries bash, and Qdrant answers with content-length
// rather than chunked encoding, so one read is the whole body.
//
// The status line is the verdict — an engine that answers 404 has answered
// — and the body is reduced to the one number Qdrant states where it
// states one ("count" or "points_count"), passed through otherwise.
const checkScript = `set -u
coll=$1; text=$2
path=${text%% *}
body=''
case "$text" in *' '*) body=${text#* } ;; esac
case "$path" in /*) ;; *) path=/collections/$coll/$path ;; esac
exec 3<>/dev/tcp/127.0.0.1/` + httpPort + ` || { echo 'qdrant is not listening on ` + httpPort + `' >&2; exit 1; }
{
  if [ -n "$body" ]; then
    printf 'POST %s HTTP/1.1\r\n' "$path"
    printf 'Host: localhost\r\nConnection: close\r\nContent-Type: application/json\r\n'
    printf 'Content-Length: %s\r\n\r\n' "${#body}"
    printf '%s' "$body"
  else
    printf 'GET %s HTTP/1.1\r\n' "$path"
    printf 'Host: localhost\r\nConnection: close\r\n\r\n'
  fi
} >&3
resp=$(cat <&3)
exec 3>&-
status=$(printf '%s' "$resp" | head -1 | cut -d' ' -f2)
payload=$(printf '%s' "$resp" | awk 'BEGIN{h=1} h && /^\r?$/ {h=0; next} !h {print}')
case "$status" in
  2*) ;;
  *) printf 'qdrant answered %s\n' "${status:-nothing}" >&2
     printf '%s' "$payload" | head -c 300 >&2; exit 1 ;;
esac
n=$(printf '%s' "$payload" | grep -o '"\(points_count\|count\)":[0-9][0-9]*' | head -1 | sed 's/.*://')
if [ -n "$n" ]; then printf '%s\n' "$n"; else printf '%s\n' "$payload"; fi`

// probePayload reports identity and capabilities (§6.1).
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "qdrant"},
		"sources": []map[string]any{
			{"kind": "qdrant_snapshot", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "qdrant_snapshot_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "qdrant_full_snapshot", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "qdrant_full_snapshot_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			"argv": []string{"bash", "-c", checkScript, "bash", "{{database}}", "{{sql}}"},
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
// idle (docker: command: sleep infinity), because Qdrant restores a
// snapshot as a *startup* argument: the binary takes --snapshot or
// --storage-snapshot, reads the artifact before it serves anything, and
// there is no way to hand a snapshot to a server that is already running
// without an HTTP client the image does not have. The adapter therefore
// owns the engine's whole lifetime.
func opProvision(ctx context.Context, c *core, payload json.RawMessage, logger *slog.Logger) (any, *protoError) {
	req := &provisionRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed provision payload")
	}
	if req.PITR != nil {
		return nil, protoErr("invalid_request", false, "this adapter does not support pitr")
	}
	collection := option(req.Options, "collection", defaultCollection)
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"form", src.form.String(), "checksum_declared", src.declaredChecksum != "")

	if perr := checkSandbox(ctx, c); perr != nil {
		return nil, perr
	}
	workDir := path.Join(scratch, workDirName)
	if perr := prepareWorkDir(ctx, c, workDir); perr != nil {
		return nil, perr
	}

	dest := path.Join(workDir, snapshotFileName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dest, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}
	logger.Info("artifact transferred", "seconds", put.DurationSeconds)

	ready, perr := startEngine(ctx, c, dest, src.form, collection)
	if perr != nil {
		return nil, perr
	}
	points, perr := assertRestored(ctx, c, collection)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "seconds", ready, "points", points)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": 6333,
			"database": collection,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// Nothing inside a Qdrant snapshot dates the backup: the
			// creation time lives in the API response that made it and in
			// the file name, neither of which survives a copy.
			"created_at": nil,
		},
		"timings": map[string]any{
			"transfer_seconds": put.DurationSeconds,
			"restore_seconds":  ready,
		},
		"state": map[string]any{"work_dir": workDir, "collection": collection},
	}, nil
}

func option(options map[string]string, key, fallback string) string {
	if v := strings.TrimSpace(options[key]); v != "" {
		return v
	}
	return fallback
}

// checkSandbox verifies the sandbox is the idle Qdrant image this adapter
// needs: bash (the only HTTP client the image has, through /dev/tcp), the
// engine binary, and an engine that is NOT already running. A sandbox
// whose image started Qdrant for us would have read an empty storage
// directory, and the snapshot flags are startup arguments — there would be
// no way to restore into it.
func checkSandbox(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c",
		`command -v bash >/dev/null || { echo "no bash in the sandbox image" >&2; exit 1; }
[ -x ` + engineBinary + ` ] || { echo "no ` + engineBinary + ` — this is not a qdrant image" >&2; exit 1; }
if (exec 3<>/dev/tcp/127.0.0.1/` + httpPort + `) 2>/dev/null; then
  echo "qdrant is already serving on ` + httpPort + ` — the sandbox must start idle" >&2; exit 1
fi`}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox is not an idle Qdrant image with bash, and this adapter needs all of "+
				"that — the adapter README names the image and the idle command it must start "+
				"with (%s)", firstLine(stderr))
	}
	return nil
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

// startEngineScript starts Qdrant on the artifact and waits for it to
// answer.
//
// --disable-telemetry is not optional and not configurable. Qdrant ships
// with telemetry_disabled: false and sends usage data to its developers;
// Probavi does not phone home, from any process it starts, ever. The
// drill runs under --network none so nothing could leave anyway, but a
// flag that states it is worth more than a network that happens to
// prevent it.
//
// --force-snapshot lets the restore replace a collection of the same name.
// The storage directory is empty at this point, so nothing is being
// overwritten; the flag is what makes a re-run of the same drill behave
// identically to its first run.
//
// A failed restore is a dead process rather than a bad answer: Qdrant
// exits 101 on a damaged snapshot and never listens (measured at
// truncations from 25% to 99% and on a bit flip inside a structurally
// valid archive). So the wait loop watches the pid as well as the port,
// and reports the engine's own last words.
const startEngineScript = `set -u
art=$1; mode=$2; coll=$3
cd ` + engineDir + ` || exit 1
if [ "$mode" = full ]; then
  nohup ` + engineBinary + ` --disable-telemetry --force-snapshot --storage-snapshot "$art" \
    > /tmp/probavi-qdrant.log 2>&1 &
else
  nohup ` + engineBinary + ` --disable-telemetry --force-snapshot --snapshot "$art:$coll" \
    > /tmp/probavi-qdrant.log 2>&1 &
fi
pid=$!
i=0
while [ $i -lt 120 ]; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "qdrant exited while restoring the snapshot" >&2
    tail -5 /tmp/probavi-qdrant.log >&2
    exit 1
  fi
  if (exec 3<>/dev/tcp/127.0.0.1/` + httpPort + `) 2>/dev/null; then
    exec 3<>/dev/tcp/127.0.0.1/` + httpPort + `
    printf 'GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n' >&3
    line=$(head -1 <&3)
    exec 3>&-
    case "$line" in *' 200 '*) exit 0 ;; esac
  fi
  i=$((i+1)); sleep 1
done
echo "qdrant did not answer within 120s" >&2
tail -5 /tmp/probavi-qdrant.log >&2
exit 1`

// startEngine starts Qdrant with the snapshot flag the source form needs.
func startEngine(ctx context.Context, c *core, artifact string, form sourceForm, collection string) (float64, *protoError) {
	mode := "collection"
	if form == formFullSnapshot {
		mode = "full"
	}
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", startEngineScript, "bash", artifact, mode, collection},
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		// The engine refusing a snapshot is the artifact's verdict, not
		// the adapter's failure: Qdrant validates what it reads and dies
		// rather than serving a partial collection.
		return 0, protoErr("source_corrupt", false,
			"qdrant refused to restore the snapshot: %s", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// assertRestored is the restore's verdict, and it is the restored
// collection's point count rather than the fact that the engine answered.
//
// Qdrant fences a damaged artifact hard — it exits rather than serving
// less than the snapshot held — so unlike the h2 and couchdb adapters this
// count is not covering for an engine that opens a broken file. What it
// catches is the other shape: a snapshot of an empty collection restores
// green with points_count 0 (measured), which is a drill that proved
// nothing while reporting success. A well-formed zero is refused for the
// same reason it is in the h2 and victoriametrics adapters.
func assertRestored(ctx context.Context, c *core, collection string) (string, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", checkScript, "bash", collection, "/collections/" + collection},
	})
	if perr != nil {
		return "", perr
	}
	if val.ExitCode != 0 {
		return "", protoErr("restore_failed", false,
			"the restored collection %s does not answer: %s", collection, firstLine(stderr))
	}
	count := firstLine(stdout)
	switch count {
	case "0":
		return "", protoErr("source_corrupt", false,
			"the restored collection %s holds no points: an empty collection has a valid "+
				"snapshot, so a restore that proves nothing looks exactly like a restore that "+
				"worked", collection)
	case "":
		return "", protoErr("restore_failed", false,
			"the restored collection %s did not report how many points it holds, so nothing "+
				"about this restore can be relied on", collection)
	}
	return count, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored collection still answers (§6.3). An
// unhealthy collection is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no restored collection")
	}
	collection := req.Connection.Database
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", checkScript, "bash", collection, "/collections/" + collection},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("collection serves queries; %s points", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("qdrant check exited %d: %s", val.ExitCode, firstLine(stderr))
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
