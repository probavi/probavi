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
	adapterName    = "weaviate"
	adapterVersion = "0.1.0"

	// engineBinary is started directly. The image's entrypoint is pinned
	// to this same binary — and the binary ignores unknown positional
	// arguments (measured), so `command: sleep infinity` under the stock
	// image starts a serving engine on defaults instead of failing. That
	// is why the sandbox runs the two-line wrapper image the README
	// documents: the entrypoint pin lifted, everything else the official
	// image's.
	engineBinary = "/bin/weaviate"

	// httpPort is the REST port the adapter serves the engine on, bound
	// to loopback. Nothing listens outside the sandbox: the drill
	// publishes no ports and runs under --network none (measured — with
	// CLUSTER_ADVERTISE_ADDR pinned to loopback, without which the
	// memberlist layer refuses to start on a host with no private IP).
	httpPort = "8080"

	// workDirName is created under the provider's scratch directory; the
	// backup tree lands under backups/ (the engine's filesystem-backend
	// root) and the restored data under data/.
	workDirName    = "probavi-weaviate"
	backupsDir     = "backups"
	dataDir        = "data"
	tarName        = "restore.tar"
	extractDirName = "extract"

	// fallbackNode and fallbackID stand in when an archive's own metadata
	// cannot be parsed. A real backup always parses — the engine wrote it
	// — so these are reached only on a damaged manifest inside a valid
	// archive, and the engine then refuses the restore with its own words
	// (measured: HTTP 422 on a corrupt backup_config.json).
	fallbackNode = "node1"
	fallbackID   = "restore"
	// fallbackClass labels the verdict query in the same unreachable-on-
	// healthy-artifacts situation.
	fallbackClass = "restored"
)

// checkScript is the sql_runner's body. Weaviate speaks HTTP rather than
// SQL, so a check is what the glossary allows where there is no SQL: the
// engine's own client arguments. Here that is either a **path, optionally
// followed by a space and a JSON body** — a body turns the request into a
// POST —, a **GraphQL query** for text beginning with "{", POSTed to
// /v1/graphql (how Weaviate asks its most useful question: an exact,
// optionally filtered object count via Aggregate) — and a path that does
// not begin with "/" is taken relative to /v1.
//
// The script speaks HTTP with busybox wget because that is what the
// official image carries: sh, wget and nc, no curl and no bash (measured).
// wget exits non-zero on any non-2xx answer and names the status line on
// stderr, so the status is the verdict — and because Weaviate answers
// GraphQL errors as HTTP 200 with an "errors" array (measured), a body
// carrying one fails the check too. The body is reduced to the one number
// Weaviate states where it states one ("count" from Aggregate), passed
// through otherwise.
const checkScript = `set -u
class=$1; text=$2
out=$(mktemp); errf=$(mktemp)
trap 'rm -f "$out" "$errf"' EXIT
case "$text" in
'{'*)
  p=/v1/graphql
  q=$(printf '%s' "$text" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr '\n' ' ')
  body='{"query":"'$q'"}'
  ;;
*)
  p=${text%% *}
  body=''
  case "$text" in *' '*) body=${text#* } ;; esac
  case "$p" in /*) ;; *) p=/v1/$p ;; esac
  ;;
esac
if [ -n "${body:-}" ]; then
  wget -q -O "$out" --header 'Content-Type: application/json' --post-data "$body" "http://127.0.0.1:` + httpPort + `$p" 2>"$errf"
else
  wget -q -O "$out" "http://127.0.0.1:` + httpPort + `$p" 2>"$errf"
fi
rc=$?
if [ $rc -ne 0 ]; then
  echo "weaviate answered: $(head -1 "$errf")" >&2
  head -c 300 "$out" >&2
  exit 1
fi
if grep -q '"errors":' "$out"; then
  echo 'weaviate answered 200 with a GraphQL error' >&2
  head -c 300 "$out" >&2
  exit 1
fi
n=$(grep -o '"count":[0-9][0-9]*' "$out" | head -1 | sed 's/.*://')
if [ -n "$n" ]; then printf '%s\n' "$n"; else cat "$out"; echo; fi`

// probePayload reports identity and capabilities (§6.1).
//
// weaviate_backup_tar is declared first deliberately: the conformance
// suite drives checks 8–10 against the first declared kind with a
// generated single file, so the first kind must be one whose host-side
// pass lets an unreadable archive through to the sandbox, where tar and
// then the engine judge it.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "weaviate"},
		"sources": []map[string]any{
			{"kind": "weaviate_backup_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "weaviate_backup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "weaviate_backup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			"argv": []string{"sh", "-c", checkScript, "sh", "{{database}}", "{{sql}}"},
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

// opProvision restores the backup into the sandbox. The sandbox must
// start idle (the wrapper image, command: sleep infinity), because the
// backup tree must sit under the filesystem backend's root and the
// module, telemetry and cluster environment must be set before the engine
// starts — so the adapter owns the engine's whole lifetime, then drives
// the restore over the engine's own backup API and lets the engine judge
// the artifact.
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

	src, perr := resolveSource(req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes,
		"archive", src.tarball)

	if perr := checkSandbox(ctx, c); perr != nil {
		return nil, perr
	}
	workDir := path.Join(scratch, workDirName)
	backupsRoot := path.Join(workDir, backupsDir)
	dataRoot := path.Join(workDir, dataDir)
	if perr := mkdirAll(ctx, c, backupsRoot, dataRoot); perr != nil {
		return nil, perr
	}

	transferSeconds, perr := transferArtifact(ctx, c, src, workDir, backupsRoot)
	if perr != nil {
		return nil, perr
	}
	class, perr := chooseClass(req.Options, src)
	if perr != nil {
		return nil, perr
	}
	logger.Info("artifact in place", "backup_id", backupID(src), "node", nodeName(src),
		"class", class, "seconds", transferSeconds)

	readySeconds, perr := startEngine(ctx, c, nodeName(src), backupsRoot, dataRoot)
	if perr != nil {
		return nil, perr
	}
	restoreSeconds, perr := restoreBackup(ctx, c, backupID(src))
	if perr != nil {
		return nil, perr
	}
	objects, perr := assertRestored(ctx, c, class)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "seconds", restoreSeconds, "objects", objects)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": 8080,
			"database": class,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// The completion instant the backup states about itself,
			// RFC 3339 UTC with the zone attached (measured), so no
			// timezone declaration is ever needed; null when the
			// metadata could not be read.
			"created_at": createdAtValue(src),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{
			"work_dir": workDir, "class": class, "backup_id": backupID(src),
		},
	}, nil
}

func backupID(src *resolvedSource) string {
	if src.meta != nil && src.meta.ID != "" {
		return src.meta.ID
	}
	return fallbackID
}

func nodeName(src *resolvedSource) string {
	if src.node != "" {
		return src.node
	}
	return fallbackNode
}

func createdAtValue(src *resolvedSource) any {
	if src.createdAt == nil {
		return nil
	}
	return *src.createdAt
}

// chooseClass names the class whose object count is the restore's
// verdict: the drill config's `class` option, or the backup's own single
// class when it holds exactly one.
func chooseClass(options map[string]string, src *resolvedSource) (string, *protoError) {
	chosen := strings.TrimSpace(options["class"])
	if chosen != "" {
		for _, c := range src.classes {
			if c == chosen {
				return chosen, nil
			}
		}
		if len(src.classes) == 0 {
			// The archive's metadata was unreadable; the engine will
			// judge the restore and the count will judge the class.
			return chosen, nil
		}
		return "", protoErr("invalid_request", false,
			"options.class names %s but the backup holds %s", chosen,
			strings.Join(src.classes, ", "))
	}
	switch len(src.classes) {
	case 0:
		return fallbackClass, nil
	case 1:
		return src.classes[0], nil
	default:
		return "", protoErr("invalid_request", false,
			"the backup holds %d classes (%s): set options.class to the one whose object "+
				"count is this drill's verdict", len(src.classes), strings.Join(src.classes, ", "))
	}
}

// checkSandbox verifies the sandbox is the idle wrapper image this
// adapter needs: busybox wget (the only HTTP client the image has), the
// engine binary, and an engine that is NOT already running — a sandbox
// whose image started Weaviate for us is serving defaults with no backup
// module and would have to be restarted to restore anything.
// preflightScript verifies the sandbox before anything moves: busybox
// wget (the only HTTP client the image has), the engine binary, and no
// listener on the engine's port.
const preflightScript = `command -v wget >/dev/null || { echo "no wget in the sandbox image" >&2; exit 1; }
[ -x ` + engineBinary + ` ] || { echo "no ` + engineBinary + ` — this is not a weaviate image" >&2; exit 1; }
if nc -w 1 127.0.0.1 ` + httpPort + ` </dev/null 2>/dev/null; then
  echo "something is already serving on ` + httpPort + ` — the sandbox must start idle" >&2; exit 1
fi`

func checkSandbox(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", preflightScript}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox is not an idle Weaviate wrapper image, and this adapter needs one — "+
				"the adapter README names the two-line wrapper and the idle command it must "+
				"start with (%s)", firstLine(stderr))
	}
	return nil
}

func mkdirAll(ctx context.Context, c *core, dirs ...string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: append([]string{"mkdir", "-p"}, dirs...)})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare work directories: %s", firstLine(stderr))
	}
	return nil
}

// transferArtifact moves the artifact into place under the filesystem
// backend's root: a directory tree is recreated file by file at the name
// the backup's own id states; an archive is placed, unpacked, its
// metadata read where the files now are, and moved to that name.
func transferArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir, backupsRoot string) (float64, *protoError) {
	if !src.tarball {
		seconds, perr := transferTree(ctx, c, src.path, path.Join(backupsRoot, src.meta.ID))
		return seconds, perr
	}
	return transferArchive(ctx, c, src, workDir, backupsRoot)
}

// locateScript finds the backup after unpacking: an archive holds either
// backup_config.json at its root or one wrapping directory above it.
const locateScript = `d="$1"
if [ ! -e "$d/` + metaFileName + `" ]; then
  set -- "$d"/*/
  if [ "$#" -eq 1 ] && [ -d "${1%/}" ]; then d="${1%/}"; fi
fi
printf '%s\n' "$d"`

func transferArchive(ctx context.Context, c *core, src *resolvedSource,
	workDir, backupsRoot string) (float64, *protoError) {
	extractDir := path.Join(workDir, extractDirName)
	if perr := mkdirAll(ctx, c, extractDir); perr != nil {
		return 0, perr
	}
	tarPath := path.Join(workDir, tarName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: tarPath, Mode: "0600"})
	if perr != nil {
		return 0, perr
	}
	unpack, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"tar", "-xf", tarPath, "-C", extractDir}})
	if perr != nil {
		return 0, perr
	}
	if unpack.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"tar could not unpack the archive: %s", firstLine(stderr))
	}
	locate, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", locateScript, "sh", extractDir}})
	if perr != nil {
		return 0, perr
	}
	located := strings.TrimSpace(firstLine(stdout))
	if locate.ExitCode != 0 || located == "" {
		located = extractDir
	}
	readMeta, perr := readArchiveMeta(ctx, c, src, located)
	if perr != nil {
		return 0, perr
	}
	move, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mv", located,
		path.Join(backupsRoot, backupID(src))}})
	if perr != nil {
		return 0, perr
	}
	if move.ExitCode != 0 {
		return 0, protoErr("internal", false, "place the backup: %s", firstLine(stderr))
	}
	return put.DurationSeconds + unpack.DurationSeconds + locate.DurationSeconds +
		readMeta + move.DurationSeconds, nil
}

// readArchiveMeta reads the unpacked backup's own metadata where the
// files now are. A parse failure with the file present is not refused
// here: a real backup's metadata always parses — the engine wrote it —
// and on a damaged one the engine refuses the restore with its own words
// (measured: HTTP 422), which is a better verdict than a guess.
func readArchiveMeta(ctx context.Context, c *core, src *resolvedSource, located string) (float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"cat",
		path.Join(located, metaFileName)}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"the archive holds no %s: every backup the filesystem backend writes has one, so "+
				"this is not a Weaviate backup — a copy of the persistence directory is not an "+
				"artifact, POST /v1/backups/filesystem is", metaFileName)
	}
	meta := &backupMeta{}
	if err := json.Unmarshal(stdout, meta); err != nil || meta.ID == "" {
		return val.DurationSeconds, nil
	}
	if perr := requireCompleted(meta, meta.ID); perr != nil {
		return 0, perr
	}
	node, perr := singleNode(meta, meta.ID)
	if perr != nil {
		return 0, perr
	}
	src.meta = meta
	src.node = node
	src.classes = meta.Nodes[node].Classes
	src.createdAt = createdAtFrom(meta)
	return val.DurationSeconds, nil
}

// transferTree recreates the backup directory inside the sandbox: one
// mkdir for the directory skeleton, one put_file per file.
func transferTree(ctx context.Context, c *core, hostDir, destDir string) (float64, *protoError) {
	dirs, files, perr := treeEntries(hostDir, destDir)
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

// startEngineScript starts Weaviate on the placed backup tree and waits
// for it to answer.
//
// DISABLE_TELEMETRY=true is not optional and not configurable. Without
// it the engine POSTs usage data to telemetry.weaviate.io at startup
// (measured); Probavi does not phone home, from any process it starts,
// ever. The drill runs under --network none so nothing could leave
// anyway, but an environment that states it is worth more than a network
// that happens to prevent it.
//
// CLUSTER_HOSTNAME is pinned to the node the backup was taken on: the
// engine refuses to restore another node's backup (measured, HTTP 500).
// CLUSTER_ADVERTISE_ADDR is pinned to loopback: without it the
// memberlist layer looks for a private IP and, under --network none,
// finds none and refuses to start (measured).
//
// The wait loop watches the pid as well as the readiness endpoint, so an
// engine that dies reports its own last words instead of a timeout.
const startEngineScript = `set -u
node=$1; backups=$2; data=$3
ENABLE_MODULES=backup-filesystem BACKUP_FILESYSTEM_PATH="$backups" \
PERSISTENCE_DATA_PATH="$data" \
AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true DISABLE_TELEMETRY=true \
CLUSTER_HOSTNAME="$node" CLUSTER_ADVERTISE_ADDR=127.0.0.1 \
nohup ` + engineBinary + ` --host 127.0.0.1 --port ` + httpPort + ` --scheme http \
  > /tmp/probavi-weaviate.log 2>&1 &
pid=$!
i=0
while [ $i -lt 240 ]; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "weaviate exited while starting" >&2
    tail -5 /tmp/probavi-weaviate.log >&2
    exit 1
  fi
  if wget -q -O /dev/null -T 2 http://127.0.0.1:` + httpPort + `/v1/.well-known/ready 2>/dev/null; then
    exit 0
  fi
  i=$((i+1)); sleep 0.5
done
echo "weaviate did not answer within 120s" >&2
tail -5 /tmp/probavi-weaviate.log >&2
exit 1`

func startEngine(ctx context.Context, c *core, node, backupsRoot, dataRoot string) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", startEngineScript, "sh", node, backupsRoot, dataRoot},
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		// The artifact has not been judged yet — the engine starts on an
		// empty data directory — so a failed start is the drill's
		// environment, not the backup.
		return 0, protoErr("restore_failed", false,
			"weaviate did not start: %s", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// restoreScript drives the engine's own restore: one POST, then the
// status endpoint until a terminal answer. A FAILED restore reports the
// engine's own error field — the engine is the judge of its artifact,
// and it judges well: a truncated chunk, a flipped byte and a missing
// file all fail here with their own words, and the class is never
// created (measured).
const restoreScript = `set -u
id=$1
base="http://127.0.0.1:` + httpPort + `/v1/backups/filesystem/$id/restore"
if ! wget -q -O /tmp/probavi-restore.json --header 'Content-Type: application/json' \
    --post-data '{}' "$base" 2>/tmp/probavi-restore.err; then
  echo "weaviate refused the restore request: $(head -1 /tmp/probavi-restore.err)" >&2
  exit 1
fi
i=0
while [ $i -lt 600 ]; do
  s=$(wget -q -O - "$base" 2>&1) || {
    echo "restore status unreadable: $(printf '%s' "$s" | head -1)" >&2; exit 1; }
  case "$s" in
    *'"status":"SUCCESS"'*) exit 0 ;;
    *'"status":"FAILED"'*)
      printf '%s' "$s" | grep -o '"error":"[^"]*"' | head -c 400 >&2
      echo >&2; exit 1 ;;
  esac
  i=$((i+1)); sleep 0.5
done
echo "the restore did not finish within 300s" >&2
exit 1`

func restoreBackup(ctx context.Context, c *core, id string) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", restoreScript, "sh", id},
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"weaviate refused to restore the backup: %s", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// assertRestored is the restore's verdict, and it is the restored class's
// object count rather than the fact that the engine answered.
//
// Weaviate fences a damaged artifact hard — a failed restore leaves the
// class absent rather than short (measured) — so this count is not
// covering for a forgiving engine. What it catches is the other shape: a
// backup of an empty class restores green with count 0 (measured), which
// is a drill that proved nothing while reporting success. A well-formed
// zero is refused for the same reason it is in the qdrant, h2 and
// victoriametrics adapters.
func assertRestored(ctx context.Context, c *core, class string) (string, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", checkScript, "sh", class, aggregateCount(class)},
	})
	if perr != nil {
		return "", perr
	}
	if val.ExitCode != 0 {
		return "", protoErr("restore_failed", false,
			"the restored class %s does not answer: %s", class, firstLine(stderr))
	}
	count := firstLine(stdout)
	switch count {
	case "0":
		return "", protoErr("source_corrupt", false,
			"the restored class %s holds no objects: an empty class has a valid backup, so a "+
				"restore that proves nothing looks exactly like a restore that worked", class)
	case "":
		return "", protoErr("restore_failed", false,
			"the restored class %s did not report how many objects it holds, so nothing about "+
				"this restore can be relied on", class)
	}
	return count, nil
}

// aggregateCount is the GraphQL question the verdict asks.
func aggregateCount(class string) string {
	return fmt.Sprintf("{Aggregate{%s{meta{count}}}}", class)
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored class still answers (§6.3). An
// unhealthy class is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no restored class")
	}
	class := req.Connection.Database
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", checkScript, "sh", class, aggregateCount(class)},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("class serves queries; %s objects", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("weaviate check exited %d: %s", val.ExitCode, firstLine(stderr))
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
