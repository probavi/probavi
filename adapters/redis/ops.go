package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "redis"
	adapterVersion = "0.3.0"

	// Where the restored server serves inside the sandbox. No TLS and no
	// auth: a Probavi sandbox is zero-ingress (--network none, no ports
	// expressible), which is the only reason a bare port on restored
	// production data is acceptable. The RDB itself carries no
	// credentials — requirepass and ACLs live in server config, not in
	// the data — so there is nothing to reset either.
	defaultPort = 6379

	// dataDir is where the RDB is placed and the server started from. It
	// is adapter-composed under the sandbox's own filesystem, never
	// operator input.
	dataDir      = "/probavi-redis/data"
	rdbName      = "dump.rdb"
	rdbInSandbox = dataDir + "/" + rdbName
	// aofDirName is where the append-only set is placed under dataDir;
	// the server is started with --appenddirname naming it, so the
	// artifact's original directory name never matters.
	aofDirName      = "appendonlydir"
	aofDirInSandbox = dataDir + "/" + aofDirName
	// serverLog is where the daemonized server writes; the readiness
	// timeout path reads it so a start failure names the engine's own
	// reason instead of "never became ready".
	serverLog = "/probavi-redis/redis.log"

	readinessBudget = 2 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "redis"},
		"sources": []map[string]any{
			{"kind": "redis_rdb", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "redis_rdb_dir", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "redis_aof", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Redis has no SQL: the check text the core passes through
			// {{sql}} is a line of redis-cli arguments (documented in the
			// adapter README), expanded by the shell into words. This is
			// word splitting only, not shell parsing: a POSIX shell does
			// not re-read expansions as syntax, so operators like ; and |
			// in the text stay literal arguments, and set -f keeps glob
			// characters literal too. -e makes a command-level error exit
			// non-zero, which is what the runner contract requires. The
			// engine dialect is absorbed here, declaratively — the core
			// never learns it (§6.1).
			"argv": []string{"sh", "-c",
				"set -f; exec redis-cli -e -h 127.0.0.1 -p " + strconv.Itoa(defaultPort) + " $0", "{{sql}}"},
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

// opProvision restores the artifact — one RDB file, or a Redis 7+
// append-only directory — into the idle sandbox and starts the server
// on it: preflight (redis-server present, versions compatible), stage
// the file or set, integrity check by the matching redis-check-* tool,
// daemonized start, readiness.
//
// The whole lifecycle belongs to the adapter because it has to: both
// artifact shapes are loaded by the server at startup from its
// configured data directory, so the sequence "place, then serve" cannot
// be expressed by an image's own entrypoint. The drill config starts the
// sandbox idle (docker: command: sleep infinity) — the official images
// otherwise boot an empty server on the port the restored one needs.
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

	src, perr := resolveSource(ctx, req.Source.Kind, req.Source.Path)
	if perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.path, "size_bytes", src.sizeBytes)

	engine, perr := checkEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	if perr := checkEngineVersion(src.redisVer, engine); perr != nil {
		return nil, perr
	}

	transferSeconds, perr := stageArtifact(ctx, c, src)
	if perr != nil {
		return nil, perr
	}

	restoreSeconds, readySeconds, perr := startEngine(ctx, c, src.serverArgs())
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine serving restored data",
		"load_seconds", restoreSeconds, "ready_seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "redis", "host": "127.0.0.1", "port": defaultPort,
			// Redis numbers its databases; the server starts on 0 and the
			// declared runner does not select — a check may SELECT itself.
			"database": "0", "user": "",
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
		"state": src.state(),
	}, nil
}

// stageArtifact places the artifact in the sandbox and has the matching
// redis-check-* tool vet it before the server is pointed at it. For the
// append-only kind that is the manifest plus every file it names, into
// the adapter's own append-only directory.
func stageArtifact(ctx context.Context, c *core, src *resolvedSource) (float64, *protoError) {
	if src.aof == nil {
		if perr := prepareDir(ctx, c, dataDir); perr != nil {
			return 0, perr
		}
		put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: rdbInSandbox, Mode: "0600"})
		if perr != nil {
			return 0, perr
		}
		return put.DurationSeconds, checkRDB(ctx, c)
	}
	if perr := prepareDir(ctx, c, aofDirInSandbox); perr != nil {
		return 0, perr
	}
	total := 0.0
	for _, name := range src.aof.transferNames() {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: filepath.Join(src.aof.dir, name),
			DestPath:   aofDirInSandbox + "/" + name, Mode: "0600",
		})
		if perr != nil {
			return 0, perr
		}
		total += put.DurationSeconds
	}
	return total, checkAOF(ctx, c, src.aof.manifestName)
}

// serverArgs are the redis-server flags that point the engine at the
// staged artifact. Persistence is pinned in both flows so the server
// never rewrites what the drill measures: the RDB flow disables AOF
// outright, and the AOF flow disables RDB saves while --appendfilename
// derives from the staged manifest's own name — an unmatched name would
// make the server silently start a fresh, empty append-only set, the
// exact false green this adapter exists to refuse.
func (src *resolvedSource) serverArgs() []string {
	if src.aof == nil {
		return []string{"--dir", dataDir, "--dbfilename", rdbName, "--appendonly", "no", "--save", ""}
	}
	return []string{"--dir", dataDir, "--appendonly", "yes", "--appenddirname", aofDirName,
		"--appendfilename", src.aof.appendFilename(), "--save", ""}
}

// state is what healthcheck and teardown are handed back.
func (src *resolvedSource) state() map[string]any {
	if src.aof == nil {
		return map[string]any{"data_dir": dataDir, "rdb_path": rdbInSandbox}
	}
	return map[string]any{"data_dir": dataDir, "aof_dir": aofDirInSandbox}
}

// engineVersionPattern finds the release series in `redis-server
// --version` output ("Redis server v=7.2.5 sha=00000000:0
// malloc=jemalloc-5.3.0 bits=64 build=…").
var engineVersionPattern = regexp.MustCompile(`v=(\d+)\.(\d+)`)

// checkEngine verifies the image carries redis-server — one probe, three
// duties: presence gates the flow, the reported version feeds the
// pre-check below, and a version line naming Valkey is refused outright —
// positive evidence that the sandbox runs the other engine (the Valkey
// images ship redis-* compatibility symlinks, so the presence check alone
// would pass there), which would green a drill against an engine the
// backup does not belong to. The 7.2-line Valkey names no engine in that
// line, so the refusal fires only where the evidence is positive.
func checkEngine(ctx context.Context, c *core) (string, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"redis-server", "--version"}})
	if perr != nil {
		return "", perr
	}
	if val.ExitCode != 0 {
		return "", protoErr("invalid_request", false,
			"sandbox image lacks redis-server (%s): use an official redis image with "+
				"command: sleep infinity", firstLine(stderr))
	}
	out := string(stdout)
	if strings.Contains(out, "Valkey") {
		return "", protoErr("invalid_request", false,
			"the sandbox engine reports itself as Valkey (%s): restoring a Redis backup there "+
				"would prove recovery into an engine the backup does not belong to — use an "+
				"official redis image", firstLine(stdout))
	}
	return out, nil
}

// originSeriesPattern extracts major.minor from a redis-ver aux value.
var originSeriesPattern = regexp.MustCompile(`^(\d+)\.(\d+)`)

// seriesInts converts a two-group version match to integers.
func seriesInts(m []string) (major, minor int, ok bool) {
	if m == nil {
		return 0, 0, false
	}
	major, majErr := strconv.Atoi(m[1])
	minor, minErr := strconv.Atoi(m[2])
	if majErr != nil || minErr != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// originSeries extracts major.minor from the RDB's redis-ver aux value.
// The bounds discard what real servers never state about a restorable
// backup — notably 255.255.255, which unstable builds write.
func originSeries(redisVer string) (major, minor int, ok bool) {
	major, minor, ok = seriesInts(originSeriesPattern.FindStringSubmatch(redisVer))
	if !ok || major < 2 || major > 99 || minor > 99 {
		return 0, 0, false
	}
	return major, minor, true
}

// checkEngineVersion refuses the restore direction Redis does not have: a
// newer server's RDB handed to an older engine, which refuses the format
// version at load ("Can't handle RDB format version …") minutes later —
// the reverse, an older RDB on a newer server, is the supported path and
// passes (docs/engine-versions.md §5). It refuses only on positive
// evidence: an RDB without a readable redis-ver, or an engine whose
// version does not parse, skips the check and the load speaks for itself.
func checkEngineVersion(redisVer, engineOut string) *protoError {
	oMajor, oMinor, ok := originSeries(redisVer)
	if !ok {
		return nil
	}
	eMajor, eMinor, ok := seriesInts(engineVersionPattern.FindStringSubmatch(engineOut))
	if !ok {
		return nil
	}
	if oMajor < eMajor || (oMajor == eMajor && oMinor <= eMinor) {
		return nil
	}
	return protoErr("invalid_request", false,
		"the RDB was saved by Redis %d.%d, but the sandbox engine is Redis %d.%d: a newer "+
			"server's RDB does not load on an older one — use a redis image at least as new as %d.%d",
		oMajor, oMinor, eMajor, eMinor, oMajor, oMinor)
}

// prepareDir creates the directory the server will load from. No
// shell: every step of this flow is direct argv, so the adapter works on
// any image that carries the redis binaries and mkdir.
func prepareDir(ctx context.Context, c *core, dir string) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mkdir", "-p", dir}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("internal", false, "prepare data directory: %s", firstLine(stderr))
	}
	return nil
}

// checkRDB asks redis-check-rdb whether the transferred file is a loadable
// RDB before the server is pointed at it. The tool prints its findings to
// stdout and keeps stderr for usage errors, so the verdict line is taken
// from whichever spoke.
func checkRDB(ctx context.Context, c *core) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"redis-check-rdb", rdbInSandbox}})
	if perr != nil {
		return perr
	}
	if val.ExitCode == 0 {
		return nil
	}
	line := firstLine(stderr)
	if line == "" {
		line = lastLine(stdout)
	}
	return protoErr("source_corrupt", false, "redis-check-rdb rejected the RDB: %s", line)
}

// checkAOF asks redis-check-aof whether the staged set is loadable
// before the server is pointed at it: handed the manifest, the tool
// walks the base and every incremental segment (measured). Like its RDB
// sibling it prints findings to stdout and keeps stderr for usage
// errors.
func checkAOF(ctx context.Context, c *core, manifestName string) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"redis-check-aof", aofDirInSandbox + "/" + manifestName}})
	if perr != nil {
		return perr
	}
	if val.ExitCode == 0 {
		return nil
	}
	line := firstLine(stderr)
	if line == "" {
		line = lastLine(stdout)
	}
	return protoErr("source_corrupt", false, "redis-check-aof rejected the append-only set: %s", line)
}

// startEngine launches the daemonized server on the staged artifact —
// args come from serverArgs, which pins persistence for both flows —
// and waits until it answers PING.
//
// Redis answers clients while it loads (-LOADING, RDB and AOF alike),
// which is what splits the wait into the protocol's phases: engine_ready
// ends at the server's first answer of any kind, and the load that
// follows is the restore this drill measures. A dataset small enough to
// load between two polls measures as zero restore — a real measurement
// at the poll's resolution, not an estimate.
func startEngine(ctx context.Context, c *core, args []string) (restoreSeconds, readySeconds float64, perr *protoError) {
	argv := append([]string{"redis-server"}, args...)
	argv = append(argv, "--port", strconv.Itoa(defaultPort), "--daemonize", "yes", "--logfile", serverLog)
	start, stderr, perr := execChecked(ctx, c, argv...)
	if perr != nil {
		return 0, 0, perr
	}
	if start.ExitCode != 0 {
		return 0, 0, protoErr("restore_failed", false,
			"restored server failed to launch: %s", firstLine(stderr))
	}
	upSeconds, totalSeconds, perr := awaitEngine(ctx, c)
	if perr != nil {
		return 0, 0, describeStartFailure(ctx, c, perr)
	}
	return totalSeconds - upSeconds, start.DurationSeconds + upSeconds, nil
}

func pingArgv() []string {
	return []string{"redis-cli", "-e", "-h", "127.0.0.1", "-p", strconv.Itoa(defaultPort), "ping"}
}

// awaitEngine polls PING until it succeeds. upSeconds is when the server
// first answered anything — a -LOADING error counts, the process is up
// and the restore is running — totalSeconds when PING returned PONG.
func awaitEngine(ctx context.Context, c *core) (upSeconds, totalSeconds float64, perr *protoError) {
	begin := time.Now()
	up := -1.0
	for {
		val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: pingArgv(), TimeoutSeconds: 5})
		if perr != nil {
			return 0, 0, perr
		}
		if val.ExitCode == 0 {
			total := time.Since(begin).Seconds()
			if up < 0 {
				up = total
			}
			return up, total, nil
		}
		if up < 0 && (strings.Contains(string(stdout), "LOADING") ||
			strings.Contains(string(stderr), "LOADING")) {
			up = time.Since(begin).Seconds()
		}
		if time.Since(begin) > readinessBudget {
			return 0, 0, protoErr("engine_not_ready", true,
				"engine did not answer PING within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// describeStartFailure enriches a readiness timeout with the server log's
// own last error line: an RDB from a newer format version, for instance,
// makes redis exit immediately with a precise message.
func describeStartFailure(ctx context.Context, c *core, perr *protoError) *protoError {
	if perr.Code != "engine_not_ready" {
		return perr
	}
	val, stdout, _, eperr := c.exec(ctx, execArgs{Argv: []string{"tail", "-n", "20", serverLog}})
	if eperr != nil || val.ExitCode != 0 {
		return perr
	}
	line := lastErrorLine(stdout)
	if line == "" {
		return perr
	}
	return protoErr("restore_failed", false, "restored server failed to start: %s", line)
}

// lastErrorLine picks the last log line that reads like the server's own
// failure report.
func lastErrorLine(log []byte) string {
	found := ""
	for _, line := range strings.Split(string(log), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "can't handle") {
			found = strings.TrimSpace(line)
		}
	}
	return strings.ReplaceAll(found, `"`, "'")
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored server still answers (§6.3). An
// unhealthy engine is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, _, _, perr := c.exec(ctx, execArgs{Argv: pingArgv()})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := "PING answered"
	if !healthy {
		detail = fmt.Sprintf("redis-cli ping exited %d", val.ExitCode)
	}
	return map[string]any{
		"healthy": healthy, "latency_seconds": val.DurationSeconds, "detail": detail,
	}, nil
}

// execChecked wraps core.exec returning the value and raw stderr.
func execChecked(ctx context.Context, c *core, argv ...string) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
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
