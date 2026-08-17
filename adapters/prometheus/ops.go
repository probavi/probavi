package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	adapterName    = "prometheus"
	adapterVersion = "0.2.0"

	// workDirName is created under the provider's scratch directory — the
	// one directory the provider guarantees writable (the official images
	// run as nobody, measured).
	workDirName = "probavi-prometheus"
	// tarName is where the archive kind places the artifact before
	// unpacking it.
	tarName = "snapshot.tar"

	// listenAddr is where the restored server serves inside the sandbox.
	// No TLS and no auth: a Probavi sandbox is zero-ingress (--network
	// none, no ports expressible), which is the only reason a bare port
	// on restored production data is acceptable.
	listenAddr = "127.0.0.1:9090"
	serverURL  = "http://" + listenAddr

	readinessBudget = 2 * time.Minute
	readinessPoll   = 500 * time.Millisecond
)

// censusQuery counts every series visible at one instant. The instant
// selector keeps metric names, so it never trips PromQL's duplicate-
// labelset refusal the way range functions over everything do (measured)
// — and evaluated at the backup's own newest claimed instant it answers
// the only question that matters here: does the restored server serve
// the data the backup promises?
const censusQuery = `count({__name__=~".+"})`

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "prometheus"},
		"sources": []map[string]any{
			{"kind": "prometheus_snapshot_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "prometheus_snapshot", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "prometheus_snapshot_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// Prometheus has no SQL: the check text the core passes
			// through {{sql}} is one PromQL expression, travelling as a
			// single argv element — no shell anywhere. {{database}}
			// delivers the evaluation instant: provision returns the
			// newest instant the backup's own blocks claim as
			// connection.database, so every check is evaluated against
			// the restored data rather than an empty now (§6.1). The
			// engine dialect is absorbed here, declaratively — the core
			// never learns it.
			"argv": []string{"promtool", "query", "instant",
				"--time", "{{database}}", serverURL, "{{sql}}"},
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

// opProvision restores the snapshot into the idle sandbox and starts the
// server on it: preflight, transfer (unpacking the archive kind), a
// background start, readiness, and then the two reads that decide the
// verdict — the block census against the artifact's own count, and a
// series count at the newest instant the backup claims. Both exist
// because the server alone is too forgiving: it skips an unloadable
// block and stays up (measured), which no drill may report as green.
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
		"blocks", src.info.blocks, "compaction_sources_skipped", src.info.sourcesSkipped)

	if perr := checkEngine(ctx, c); perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	transferSeconds, unpackSeconds, dataDir, perr := transferArtifact(ctx, c, src, workDir)
	if perr != nil {
		return nil, perr
	}

	census := src.info
	recoverSeconds := 0.0
	if src.tarball && census.blocks == 0 {
		census, recoverSeconds, perr = recoverCensus(ctx, c, dataDir)
		if perr != nil {
			return nil, perr
		}
	}

	readySeconds, perr := startEngine(ctx, c, workDir, dataDir)
	if perr != nil {
		return nil, perr
	}
	censusSeconds, perr := checkCensus(ctx, c, census)
	if perr != nil {
		return nil, perr
	}
	probeSeconds, perr := probeData(ctx, c, census.maxTimeMs)
	if perr != nil {
		return nil, perr
	}
	logger.Info("snapshot restored and verified", "blocks", census.blocks,
		"compaction_sources_skipped", census.sourcesSkipped, "ready_seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			// Checks reach the server over HTTP; database carries the
			// evaluation instant the declared runner's --time consumes
			// (see probePayload).
			"scheme": "http", "host": "127.0.0.1", "port": 9090,
			"database": strconv.FormatInt(census.maxTimeMs/1000, 10), "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// The newest instant the backup's own blocks claim to cover
			// (see source.go); nil when the artifact stated nothing.
			"created_at": formatCreatedAt(census.maxTimeMs),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      unpackSeconds + recoverSeconds + censusSeconds + probeSeconds,
		},
		"state": map[string]any{
			"work_dir": workDir, "data_dir": dataDir,
			"eval_time": strconv.FormatInt(census.maxTimeMs/1000, 10),
		},
	}, nil
}

// checkEngine verifies the image can run the server through a POSIX
// shell — the background start requires one, and the official
// prom/prometheus images cannot even sit idle as a drill sandbox needs,
// because they pin the server binary as their entrypoint (measured). The
// two-line wrapper the README documents fixes both.
func checkEngine(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", "exec prometheus --version"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image cannot run prometheus through a POSIX shell: the official "+
				"prom/prometheus images pin the server as their entrypoint and cannot idle as a "+
				"drill sandbox — build the wrapper image the adapter README documents (%s)",
			firstLine(stderr))
	}
	return nil
}

// transferArtifact moves the artifact into the sandbox: an archive is
// placed and unpacked, a directory tree is recreated file by file. It
// returns the directory the server will serve from.
func transferArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir string) (transferSeconds, unpackSeconds float64, dataDir string, perr *protoError) {
	if src.tarball {
		return unpackArchive(ctx, c, src.path, workDir)
	}
	dataDir = path.Join(workDir, "data")
	transferSeconds, perr = transferTree(ctx, c, src.path, dataDir)
	return transferSeconds, 0, dataDir, perr
}

// dataDirScript locates the block directories after unpacking: a
// snapshot tars either the blocks at its root or one wrapping directory
// above them (both measured), so the script descends exactly one level
// when the root holds no block and exactly one subdirectory.
const dataDirScript = `d="$1"
set -- "$d"/*/meta.json
if [ ! -e "$1" ]; then
  set -- "$d"/*/
  if [ "$#" -eq 1 ] && [ -d "${1%/}" ]; then d="${1%/}"; fi
fi
printf '%s\n' "$d"`

// unpackArchive places the tar and unpacks it in the sandbox — busybox
// tar reads both the plain and the gzip form seamlessly (measured).
func unpackArchive(ctx context.Context, c *core, hostPath, workDir string) (transferSeconds, unpackSeconds float64, dataDir string, perr *protoError) {
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
	locate, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", dataDirScript, "sh", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	dataDir = strings.TrimSpace(firstLine(stdout))
	if locate.ExitCode != 0 || dataDir == "" {
		dataDir = extractDir
	}
	return put.DurationSeconds, unpack.DurationSeconds + locate.DurationSeconds, dataDir, nil
}

// transferTree recreates the snapshot directory inside the sandbox: one
// mkdir for the directory skeleton, one put_file per file.
func transferTree(ctx context.Context, c *core, hostDir, dataDir string) (float64, *protoError) {
	dirs := []string{dataDir}
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
			dirs = append(dirs, path.Join(dataDir, filepath.ToSlash(rel)))
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
			DestPath:   path.Join(dataDir, rel),
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

// recoverCensus reads the unpacked blocks' own meta.json files when the
// host could not walk the archive: the facts must come from somewhere
// real before the census can mean anything. The exec's exit code carries
// the only refusal — an unpacked tree with no block metadata at all is
// not a snapshot — while unparseable content stays a bonus-style skip.
func recoverCensus(ctx context.Context, c *core, dataDir string) (snapshotInfo, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", `exec cat "$1"/*/meta.json`, "sh", dataDir}})
	if perr != nil {
		return snapshotInfo{}, 0, perr
	}
	if val.ExitCode != 0 {
		return snapshotInfo{}, 0, protoErr("source_corrupt", false,
			"the unpacked archive holds no block metadata — not a snapshot archive: %s",
			firstLine(stderr))
	}
	metas := []blockMeta{}
	dec := json.NewDecoder(strings.NewReader(string(stdout)))
	for {
		meta := blockMeta{}
		if err := dec.Decode(&meta); err != nil {
			break
		}
		if !plausibleEpochMs(meta.MaxTime) {
			continue
		}
		metas = append(metas, meta)
	}
	info := censusOf(metas)
	if perr := refuseSupersededOnly(info); perr != nil {
		return snapshotInfo{}, 0, perr
	}
	return info, val.DurationSeconds, nil
}

// startEngine writes the server's empty config, launches it in the
// background on the restored data, and waits for readiness — which flips
// only after the TSDB has opened, so this wait is the engine accepting
// connections in the §7 sense. The config scrapes nothing: the restored
// server serves the backup, it must not collect.
func startEngine(ctx context.Context, c *core, workDir, dataDir string) (float64, *protoError) {
	cfgPath := path.Join(workDir, "prometheus.yml")
	logPath := path.Join(workDir, "prometheus.log")
	cfg, _, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", "printf 'global: {}\\n' > " + cfgPath}})
	if perr != nil {
		return 0, perr
	}
	if cfg.ExitCode != 0 {
		return 0, protoErr("internal", false, "write server config: %s", firstLine(stderr))
	}
	script := fmt.Sprintf(
		`prometheus --config.file=%s --storage.tsdb.path=%s --web.listen-address=%s >%s 2>&1 </dev/null &`,
		cfgPath, dataDir, listenAddr, logPath)
	start, _, stderr2, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", script}})
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"restored server failed to launch: %s", firstLine(stderr2))
	}
	readySeconds, perr := awaitReady(ctx, c, logPath)
	if perr != nil {
		return 0, describeStartFailure(ctx, c, logPath, perr)
	}
	return start.DurationSeconds + readySeconds, nil
}

// awaitReady polls the readiness endpoint until the server answers.
// Between polls it also looks for the server's own fatal line, so a
// start that died instantly — a corrupted block index does exactly that
// (measured) — fails the drill in seconds with the engine's reason
// instead of burning the whole readiness budget first.
func awaitReady(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	begin := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"wget", "-q", "-O", "/dev/null", serverURL + "/-/ready"},
			TimeoutSeconds: 5,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(begin).Seconds(), nil
		}
		if fatal, _, _, perr := c.exec(ctx, execArgs{
			Argv: []string{"grep", "-q", "Fatal error", logPath}}); perr == nil && fatal.ExitCode == 0 {
			return 0, protoErr("engine_not_ready", true, "the restored server exited during startup")
		}
		if time.Since(begin) > readinessBudget {
			return 0, protoErr("engine_not_ready", true,
				"engine did not answer readiness within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

// describeStartFailure enriches a readiness timeout with the server's own
// last error line: a corrupted block index makes the server exit
// immediately with a precise message — "corrupted block …: invalid
// checksum" on the current line, "opening storage failed: read symbols:
// invalid checksum" on the LTS line (both measured) — the backup's
// fault, named as such.
func describeStartFailure(ctx context.Context, c *core, logPath string, perr *protoError) *protoError {
	if perr.Code != "engine_not_ready" {
		return perr
	}
	val, stdout, _, eperr := c.exec(ctx, execArgs{Argv: []string{"tail", "-n", "20", logPath}})
	if eperr != nil || val.ExitCode != 0 {
		return perr
	}
	line := lastErrorLine(stdout)
	if line == "" {
		return perr
	}
	if strings.Contains(line, "corrupted") || strings.Contains(line, "invalid checksum") {
		return protoErr("source_corrupt", false, "the restored TSDB refused to open: %s", line)
	}
	return protoErr("restore_failed", false, "restored server failed to start: %s", line)
}

// lastErrorLine picks the last log line that reads like the server's own
// failure report.
func lastErrorLine(log []byte) string {
	found := ""
	for _, line := range strings.Split(string(log), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "level=error") || strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "corrupted") {
			found = strings.TrimSpace(line)
		}
	}
	return strings.ReplaceAll(found, `"`, "'")
}

// checkCensus compares the number of blocks the restored server actually
// loaded — its own /metrics states it — with the number the artifact
// requires: present blocks minus the compaction sources another present
// block supersedes, which the server deliberately skips (censusOf). The
// comparison fires on positive evidence only: output that is not the
// metric's exposition line is skipped, and the count the artifact could
// not state (an archive nothing could walk) reduces the check to "not
// zero".
func checkCensus(ctx context.Context, c *core, census snapshotInfo) (float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c",
		"wget -q -O- " + serverURL + "/metrics | grep '^prometheus_tsdb_blocks_loaded'"}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return val.DurationSeconds, nil
	}
	fields := strings.Fields(firstLine(stdout))
	if len(fields) != 2 || fields[0] != "prometheus_tsdb_blocks_loaded" {
		return val.DurationSeconds, nil
	}
	loaded, err := strconv.Atoi(fields[1])
	if err != nil {
		return val.DurationSeconds, nil
	}
	switch {
	case census.blocks > 0 && loaded != census.blocks:
		return 0, protoErr("source_corrupt", false,
			"the restored server loaded %d of the %d blocks the snapshot requires (compaction "+
				"sources excluded from the count: %d): a block failed to load, and the server "+
				"stays up without it (measured) — the drill refuses to prove a partial restore",
			loaded, census.blocks, census.sourcesSkipped)
	case census.blocks == 0 && loaded == 0:
		return 0, protoErr("source_corrupt", false,
			"the restored server loaded no blocks at all — the unpacked archive holds no "+
				"restorable snapshot")
	}
	return val.DurationSeconds, nil
}

// probeData counts every series at the newest instant the backup claims
// to cover. It refuses on positive evidence only — a zero count in a
// well-formed answer means the promised data is not there; a read error
// carries the engine's own words (a chunk checksum mismatch surfaces
// here, measured).
func probeData(ctx context.Context, c *core, maxTimeMs int64) (float64, *protoError) {
	if maxTimeMs == 0 {
		return 0, nil
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"promtool", "query", "instant",
		"--time", strconv.FormatInt(maxTimeMs/1000, 10), serverURL, censusQuery}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("source_corrupt", false,
			"reading the restored data failed: %s", firstLine(stderr))
	}
	if n, ok := parseInstantCount(firstLine(stdout)); ok && n == 0 {
		return 0, protoErr("source_corrupt", false,
			"the restored server serves no series at the newest instant the backup claims to "+
				"cover (%s) — the data the snapshot promises is not there",
			time.UnixMilli(maxTimeMs).UTC().Format(createdAtLayout))
	}
	return val.DurationSeconds, nil
}

// parseInstantCount reads the value out of promtool's instant-vector
// line, `{} => 680 @[...]` (measured shape).
func parseInstantCount(line string) (int64, bool) {
	_, rest, found := strings.Cut(line, "=> ")
	if !found {
		return 0, false
	}
	value, _, found := strings.Cut(rest, " @")
	if !found {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// healthcheckRequest is the §6.3 request payload. The evaluation instant
// travels in connection.database, exactly as provision returned it.
type healthcheckRequest struct {
	Connection struct {
		Database string `json:"database"`
	} `json:"connection"`
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored server still serves the backup's
// data (§6.3): the same series count, at the same instant the checks
// evaluate at. An unhealthy server is a valid result, not an operation
// error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	if req.Connection.Database == "" {
		return nil, protoErr("invalid_request", false, "healthcheck payload names no evaluation instant")
	}
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"promtool", "query", "instant",
		"--time", req.Connection.Database, serverURL, censusQuery}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := fmt.Sprintf("serving the restored data: %s", firstLine(stdout))
	if !healthy {
		detail = fmt.Sprintf("promtool exited %d: %s", val.ExitCode, firstLine(stderr))
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
