package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	adapterName    = "aerospike"
	adapterVersion = "0.1.0"

	// defaultPort is where the drill engine listens inside the sandbox.
	// Nothing is published: the adapter binds loopback and the drill never
	// exposes a port (measured under --network none).
	defaultPort = 3000

	// workDirName is created under the provider's scratch directory for
	// the artifact, the generated configuration and the engine's log.
	workDirName = "probavi-aerospike"
	// configName is the configuration the adapter writes and starts the
	// engine with. The image ships one, but it asks for a cluster this
	// drill is not (see startEngineScript).
	configName = "aerospike.conf"
	// artifactName is where the backup lands inside the sandbox.
	artifactName = "backup"
	// logName is where the engine's own output goes. It is read back only
	// to quote a startup failure.
	logName = "asd.log"

	// dataSizeOption caps the namespace's data. Aerospike refuses a
	// data-size below 512 MiB (measured), and the cap is logical rather
	// than an allocation — a 4 GiB namespace starts in a 384 MiB
	// container — so the default is generous and the container's own
	// memory limit is the real bound.
	dataSizeOption  = "data_size"
	defaultDataSize = "4G"
)

// namespacePattern is what Aerospike accepts as a namespace name. The name
// comes out of the artifact's header and is written into a configuration
// file the engine parses, so it is checked rather than trusted: a name
// carrying a brace or a newline would rewrite the configuration around it.
var namespacePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,31}$`)

// dataSizePattern is what the generated configuration accepts for the
// namespace cap, for the same reason.
var dataSizePattern = regexp.MustCompile(`^[0-9]+[KMGT]?$`)

// checkScript is the sql_runner's body. Aerospike has no SQL, so a check
// is what the glossary allows where there is none: the engine's own client
// arguments. Two clients answer different questions, and the statement
// says which by an explicit prefix rather than by a guess at its shape:
//
//	info:sets/<ns>/<set> objects   asinfo, printing one named field
//	SELECT ...                     aql, printing the rows it returns
//
// The prefix exists because aql cannot count: `SELECT count(*)` is not a
// statement it parses (measured), so the generating built-in checks have
// no aql form and the count lives on the info side.
//
// The wrapper is not decoration. aql exits 0 for an invalid namespace and
// for a statement it cannot parse at all (both measured), so the verdict
// has to be read from what it printed: the last Status it reports, or the
// absence of one.
const checkScript = `set -u
stmt=$1
case "$stmt" in
info:*)
  req=${stmt#info:}
  cmd=${req%% *}
  field=""
  case "$req" in *" "*) field=${req##* } ;; esac
  out=$(asinfo -h 127.0.0.1 -v "$cmd" 2>&1) || { printf '%s\n' "$out" >&2; exit 1; }
  if [ -z "$out" ]; then
    printf 'the engine answered nothing for %s\n' "$cmd" >&2
    exit 1
  fi
  if [ -z "$field" ]; then printf '%s\n' "$out"; exit 0; fi
  val=$(printf '%s\n' "$out" | tr ':;' '\n\n' | sed -n "s/^$field=//p" | head -1)
  if [ -z "$val" ]; then
    printf '%s reports no %s\n' "$cmd" "$field" >&2
    exit 1
  fi
  printf '%s\n' "$val"
  ;;
*)
  out=$(aql -h 127.0.0.1 -o json -c "$stmt" 2>&1)
  status=$(printf '%s\n' "$out" | sed -n 's/.*"Status": *\([0-9][0-9]*\).*/\1/p' | tail -1)
  if [ -z "$status" ]; then
    printf '%s\n' "$out" | grep -v '^$' | tail -2 >&2
    exit 1
  fi
  if [ "$status" != 0 ]; then
    printf '%s\n' "$out" | sed -n 's/.*"Message": *"\(.*\)".*/\1/p' | tail -1 >&2
    exit 1
  fi
  printf '%s\n' "$out" | awk '
    { line=$0; gsub(/^[ \t]+|[ \t]+$/, "", line) }
    line == "[" { depth++; if (depth == 2) arr++; next }
    line == "]" || line == "]," { depth--; next }
    depth != 2 || arr != 1 { next }
    line == "{" { n=0; next }
    line == "}" || line == "}," {
      row=""
      for (i=1; i<=n; i++) row = (i==1 ? v[i] : row "\t" v[i])
      print row
      next
    }
    {
      sub(/,$/, "", line)
      idx = index(line, "\": ")
      if (idx == 0) next
      val = substr(line, idx+3)
      if (substr(val,1,1) == "\"" && substr(val,length(val),1) == "\"") val = substr(val, 2, length(val)-2)
      v[++n] = val
    }'
  ;;
esac`

// probePayload reports identity and capabilities (§6.1).
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "aerospike"},
		"sources": []map[string]any{
			{"kind": "asbackup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "asbackup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			"argv": []string{"sh", "-c", checkScript, "sh", "{{sql}}"},
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

// opProvision restores the backup into the sandbox. The sandbox must start
// idle (docker: command: sleep infinity): the engine has to be started with
// a configuration this adapter writes, because the image's own asks for a
// cluster whose heartbeat seed does not exist and for more file descriptors
// than a container is given, and because the namespace it serves has to be
// the one the artifact names.
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
	dataSize := option(req.Options, dataSizeOption, defaultDataSize)
	if !dataSizePattern.MatchString(dataSize) {
		return nil, protoErr("invalid_request", false,
			"options.%s is %q; it must be a number with an optional K, M, G or T suffix",
			dataSizeOption, dataSize)
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	if !namespacePattern.MatchString(src.namespace) {
		return nil, protoErr("source_corrupt", false,
			"the artifact names namespace %q, which is not a name Aerospike accepts (letters, "+
				"digits, - and _, at most 31 of them)", src.namespace)
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes, "namespace", src.namespace)

	workDir := path.Join(scratch, workDirName)
	if perr := prepareSandbox(ctx, c, workDir); perr != nil {
		return nil, perr
	}
	readySeconds, perr := startEngine(ctx, c, workDir, src.namespace, dataSize)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine ready", "seconds", readySeconds)

	dest := path.Join(workDir, artifactName)
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: dest, Mode: "0600"})
	if perr != nil {
		return nil, perr
	}
	restoreSeconds, perr := restore(ctx, c, dest, src)
	if perr != nil {
		return nil, perr
	}
	logger.Info("restore complete", "seconds", restoreSeconds)
	if perr := assertReadable(ctx, c, src.namespace); perr != nil {
		return nil, perr
	}

	return map[string]any{
		"connection": map[string]any{
			"scheme": "aerospike", "host": "127.0.0.1", "port": defaultPort,
			"database": src.namespace,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// An .asb header carries no clock (see source.go).
			"created_at": nil,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			"restore_seconds":      restoreSeconds,
		},
		"state": map[string]any{"work_dir": workDir, "namespace": src.namespace},
	}, nil
}

func option(options map[string]string, key, fallback string) string {
	if v := strings.TrimSpace(options[key]); v != "" {
		return v
	}
	return fallback
}

// prepareSandboxScript verifies the sandbox is the idle one this adapter
// needs — the engine and its tools present, and nothing serving yet — and
// makes the working directory.
//
// An engine already running would be serving the image's own namespace on
// its own configuration, and the restore would write into whatever that
// happens to be rather than into the namespace the artifact names.
const prepareSandboxScript = `set -u
for t in asd asrestore asinfo aql; do
  command -v "$t" >/dev/null || { printf 'no %s in the sandbox image\n' "$t" >&2; exit 1; }
done
if asinfo -h 127.0.0.1 -v status >/dev/null 2>&1; then
  echo "an engine is already serving in this sandbox" >&2; exit 1
fi
mkdir -p "$1"`

func prepareSandbox(ctx context.Context, c *core, workDir string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", prepareSandboxScript, "sh", workDir},
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox is not an idle Aerospike image, and this adapter needs one — the "+
				"adapter README names the image and the idle command it must start with (%s)",
			firstLine(stderr))
	}
	return nil
}

// drillConfig is the configuration the adapter writes and starts the engine
// with. Every line of it was measured against the alternative:
//
//   - node-id, because a node derives one from a network interface's MAC
//     and there is none under --network none (`could not get node id`).
//   - a loopback address in the fabric and heartbeat stanzas, because both
//     otherwise look for a routable IPv4 and find none (`no IPv4 addresses
//     configured for fabric`).
//   - proto-fd-max at its floor of 1024, because the image's own
//     configuration asks for 15000 and a container is given 1024 by
//     default, which stops the engine before it starts.
//   - the namespace named by the artifact, because asrestore writes each
//     record into the namespace the record names.
//   - nsup-period 0, so the engine's expiration and eviction thread removes
//     nothing for the drill's duration, with allow-ttl-without-nsup beside
//     it because the server otherwise refuses every write carrying a time
//     to live once its reaper is off (measured: "Error while storing
//     record - code 22", nothing inserted, and a restore of any backup
//     holding a TTL fails). The pair is a suspension and not a rewrite:
//     each record keeps the expiry the operator gave it, and a check
//     reading it sees exactly that. It does not make an expired record
//     readable — nothing does, see assertReadableScript.
//
// There is deliberately no admin (8.x) or info (7.x) stanza: the two
// versions renamed it, and leaving it out is what lets one configuration
// serve both (measured on 8.1.2.4 and 7.2.0.21).
const drillConfig = `service {
	node-id a1
	proto-fd-max 1024
	cluster-name probavi
}

logging {
	console {
		context any info
	}
}

network {
	service {
		address 127.0.0.1
		port %d
	}
	heartbeat {
		mode mesh
		address 127.0.0.1
		port 3002
		interval 150
		timeout 10
	}
	fabric {
		address 127.0.0.1
		port 3001
	}
}

namespace %s {
	replication-factor 1
	nsup-period 0
	allow-ttl-without-nsup true
	storage-engine memory {
		data-size %s
	}
}
`

// startEngineScript starts the engine and waits for the signal that a
// client can actually work.
//
// The wait is `cluster-stable:` and not `status`, and the difference was
// measured: `asinfo -v status` answers ok 0.04 s after launch while a
// client is still refused with "not yet fully initialized", and
// unavailable_partitions reads 0 just as early. cluster-stable returns a
// cluster key at the instant the client becomes usable, 1.47 s in.
const startEngineScript = `set -u
conf=$1; log=$2
asd --config-file "$conf" > "$log" 2>&1
i=0
while [ $i -lt 120 ]; do
  key=$(asinfo -h 127.0.0.1 -v 'cluster-stable:' 2>/dev/null | tr -d '\r')
  case "$key" in
    ''|*ERROR*|*error*) ;;
    *) printf '%s' "$key" | grep -qE '^[0-9A-F]+$' && exit 0 ;;
  esac
  i=$((i+1)); sleep 1
done
echo "the engine did not become usable within 120s" >&2
tail -5 "$log" >&2
exit 1`

// startEngine writes the configuration and brings the engine up on it.
func startEngine(ctx context.Context, c *core, workDir, namespace, dataSize string) (float64, *protoError) {
	conf := fmt.Sprintf(drillConfig, defaultPort, namespace, dataSize)
	confPath := path.Join(workDir, configName)
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv:     []string{"sh", "-c", `cat > "$1"`, "sh", confPath},
		StdinB64: base64.StdEncoding.EncodeToString([]byte(conf)),
	})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("internal", false, "write the engine configuration: %s", firstLine(stderr))
	}
	start, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", startEngineScript, "sh", confPath, path.Join(workDir, logName)},
	})
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("engine_not_ready", true, "starting the engine: %s", firstLine(stderr))
	}
	return val.DurationSeconds + start.DurationSeconds, nil
}

// restore replays the artifact and holds the result to what asrestore
// itself reports about it.
func restore(ctx context.Context, c *core, dest string, src *resolvedSource) (float64, *protoError) {
	flag := "-i"
	if src.dir {
		flag = "-d"
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"asrestore", "-h", "127.0.0.1", flag, dest},
	})
	if perr != nil {
		return 0, perr
	}
	// asrestore writes its summary to stderr; stdout is read too so a
	// build that moves it is not silently unread.
	summary, ok := parseRestoreSummary(append(append([]byte{}, stderr...), stdout...))
	if val.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"asrestore refused the backup: %s", lastError(stderr))
	}
	if !ok {
		return 0, protoErr("restore_failed", false,
			"asrestore exited 0 without reporting what it restored, so nothing about this "+
				"restore can be relied on")
	}
	if perr := summary.verdict(); perr != nil {
		return 0, perr
	}
	return val.DurationSeconds, nil
}

// restoreSummary is the line asrestore prints when it finishes:
//
//	Expired 3 : skipped 0 : err_ignored 0 : inserted 0: failed 0 (existed 0 , fresher 0)
type restoreSummary struct {
	expired    int
	skipped    int
	errIgnored int
	inserted   int
	failed     int
}

var summaryPattern = regexp.MustCompile(
	`Expired (\d+) : skipped (\d+) : err_ignored (\d+) : inserted (\d+): failed (\d+)`)

func parseRestoreSummary(out []byte) (restoreSummary, bool) {
	m := summaryPattern.FindAllStringSubmatch(string(out), -1)
	if len(m) == 0 {
		return restoreSummary{}, false
	}
	last := m[len(m)-1]
	n := make([]int, 5)
	for i := range n {
		v, err := strconv.Atoi(last[i+1])
		if err != nil {
			return restoreSummary{}, false
		}
		n[i] = v
	}
	return restoreSummary{
		expired: n[0], skipped: n[1], errIgnored: n[2], inserted: n[3], failed: n[4],
	}, true
}

// verdict decides whether the restore restored the backup. asrestore's exit
// code does not: it exits 0 having inserted nothing at all.
func (s restoreSummary) verdict() *protoError {
	// The fence. A record's expiry travels inside the artifact as an
	// absolute instant, and asrestore drops every record whose instant has
	// passed — measured: three records backed up with a 20-second TTL and
	// restored after it gave "Expired 3 : inserted 0" and exit 0, on an
	// empty namespace. Nothing on the engine did it, so there is nothing
	// to suspend; the one switch that would restore them, --extra-ttl,
	// moves the operator's own recorded expiry forward, and a check
	// reading a record's TTL must still see what the operator declared.
	// So the drill is refused rather than reported green.
	if s.expired > 0 {
		return protoErr("restore_failed", false,
			"%d record(s) in this backup had already expired when it was restored, and "+
				"asrestore dropped every one of them (%d were inserted): a record's expiry is "+
				"an absolute instant written into the backup, so a backup of data with a TTL "+
				"stops being restorable once that instant passes. The drill is refused rather "+
				"than reported green against a restore the backup did not survive",
			s.expired, s.inserted)
	}
	if s.failed > 0 || s.errIgnored > 0 {
		return protoErr("restore_failed", false,
			"asrestore did not write every record it read: %d failed and %d were ignored after "+
				"an error, so the restore is partial", s.failed, s.errIgnored)
	}
	if s.inserted == 0 {
		return protoErr("source_corrupt", false,
			"the restore inserted no records: an empty namespace backs up to a structurally "+
				"valid artifact that restores with a zero exit code, and a drill that proved "+
				"nothing is not a green one")
	}
	return nil
}

// assertReadableScript asks the engine for one record a client can read.
//
// The count Aerospike reports is not that. A record whose expiry has passed
// is still counted — measured: objects=1 with data_used_bytes=64 — while a
// scan returns nothing and a read by key answers
// AEROSPIKE_ERR_RECORD_NOT_FOUND. Expiry is applied when a record is read,
// not when it is reaped, so "the namespace holds N records" and "a check
// can see one" are different statements, and only the second is worth a
// verdict.
const assertReadableScript = `set -u
ns=$1
sets=$(asinfo -h 127.0.0.1 -v "sets/$ns" 2>/dev/null | tr ';' '\n' | sed -n 's/.*set=\([^:]*\).*/\1/p')
[ -n "$sets" ] || { echo "the restored namespace reports no sets" >&2; exit 1; }
for s in $sets; do
  rows=$(aql -h 127.0.0.1 -o json -c "SELECT * FROM $ns.$s LIMIT 1" 2>/dev/null | grep -c '"PK"' || true)
  [ "$rows" -gt 0 ] && { printf '%s\n' "$s"; exit 0; }
done
echo "no set in the restored namespace returns a record to a reader" >&2
exit 1`

// assertReadable is the restore's second gate: something a check could
// read has to come back.
func assertReadable(ctx context.Context, c *core, namespace string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", assertReadableScript, "sh", namespace},
	})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("restore_failed", false,
			"the restore reported records written, but nothing in namespace %s comes back to a "+
				"reader (%s). Aerospike applies a record's expiry when the record is read, so a "+
				"namespace can report objects that no check can see",
			namespace, firstLine(stderr))
	}
	return nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored namespace still serves queries
// (§6.3). An unhealthy engine is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no restored namespace")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", checkScript, "sh",
			"info:namespace/" + req.Connection.Database + " objects"},
	})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("namespace %s answers; %s objects", req.Connection.Database, firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("the engine did not answer for namespace %s: %s",
			req.Connection.Database, firstLine(stderr))
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// lastError returns the last line asrestore wrote that names an error, so
// a refusal quotes the engine tooling rather than paraphrasing it.
func lastError(stderr []byte) string {
	var last string
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.Contains(line, "[ERR]") {
			last = line
		}
	}
	if last == "" {
		return firstLine(stderr)
	}
	if i := strings.Index(last, "] "); i >= 0 {
		last = last[i+2:]
	}
	return strings.ReplaceAll(strings.TrimSpace(last), `"`, "'")
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
