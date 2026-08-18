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
	adapterName    = "victoriametrics"
	adapterVersion = "0.1.0"

	// workDirName is created under the provider's scratch directory — the
	// one directory the provider guarantees writable.
	workDirName = "probavi-victoriametrics"
	// tarName is where the archive kind places the artifact before
	// unpacking it.
	tarName = "backup.tar"

	// listenAddr is where the restored server serves inside the sandbox.
	// No TLS and no auth: a Probavi sandbox is zero-ingress (--network
	// none, no ports expressible), which is the only reason a bare port
	// on restored production data is acceptable.
	listenAddr = "127.0.0.1:8428"
	serverURL  = "http://" + listenAddr

	readinessBudget = 2 * time.Minute
	readinessPoll   = 500 * time.Millisecond

	// The sandbox server must not apply a retention policy to the
	// artifact it was handed. VictoriaMetrics keeps one month by default
	// (measured: flag retentionPeriod=1, is_set=false), and a restored
	// 90-day history then serves 48 of its 89 samples — the drill would
	// prove whatever the default admitted rather than what the backup
	// holds. Retention states what a running server should keep; the
	// operator's real policy is already expressed in which samples the
	// backup contains.
	//
	// The value is the largest the server accepts: 100y starts, 1000y is
	// refused, and 0 is refused outright rather than silently meaning
	// "unlimited" ("-retentionPeriod cannot be smaller than a day", all
	// measured).
	retentionPeriod = "100y"
)

// censusQuery counts every series visible at one instant — the check a
// drill runs to refuse a well-formed zero. It travels as a single argv
// element, so the regex reaches the engine intact; a form-encoded body
// would decode the `+` as a space and answer 0 on a populated server
// (measured), which is why this adapter declares a real query client
// rather than an HTTP one-liner.
const censusQuery = `count({__name__=~".+"})`

// probePayload reports identity and capabilities (§6.1). Probe must not
// touch the sandbox and needs no credentials.
func probePayload() any {
	return map[string]any{
		"name":              adapterName,
		"adapter_version":   adapterVersion,
		"protocol_versions": []string{protocolVersion},
		"engine":            map[string]string{"name": "victoriametrics"},
		"sources": []map[string]any{
			{"kind": "victoriametrics_backup_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "victoriametrics_backup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "victoriametrics_backup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// VictoriaMetrics has no SQL: the check text the core passes
			// through {{sql}} is one MetricsQL expression, travelling as
			// a single argv element — no shell anywhere. {{database}}
			// delivers the evaluation instant: provision returns the
			// instant the backup's own metadata claims as
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

// opProvision restores the backup into the idle sandbox and starts the
// server on it: preflight, transfer (unpacking the archive kind),
// vmrestore, a background start with retention pinned off, readiness,
// and then the read that decides the verdict. Every step exists because
// something quieter would pass: vmrestore restores a truncated backup
// without a word (measured), and a server with no data is still a server
// that answers.
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
		"declared_parts", src.info.parts)

	if perr := checkEngine(ctx, c); perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	dataDir := path.Join(workDir, "data")
	transferSeconds, unpackSeconds, backupDir, perr := transferArtifact(ctx, c, src, workDir)
	if perr != nil {
		return nil, perr
	}

	info := src.info
	recoverSeconds := 0.0
	if src.tarball && info.createdAtMs == 0 {
		info, recoverSeconds, perr = recoverMetadata(ctx, c, backupDir)
		if perr != nil {
			return nil, perr
		}
	}

	restoreSeconds, perr := execRestore(ctx, c, backupDir, dataDir)
	if perr != nil {
		return nil, perr
	}
	readySeconds, perr := startEngine(ctx, c, workDir, dataDir)
	if perr != nil {
		return nil, perr
	}
	censusSeconds, perr := checkSeries(ctx, c, info.createdAtMs)
	if perr != nil {
		return nil, perr
	}
	logger.Info("backup restored and verified", "declared_parts", info.parts,
		"ready_seconds", readySeconds)

	evalTime := strconv.FormatInt(info.createdAtMs/1000, 10)
	return map[string]any{
		"connection": map[string]any{
			// Checks reach the server over HTTP; database carries the
			// evaluation instant the declared runner's --time consumes.
			"scheme": "http", "host": "127.0.0.1", "port": 8428,
			"database": evalTime, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes,
			// The instant the snapshot froze the data, stated by the
			// backup itself (see source.go).
			"created_at": formatCreatedAt(info.createdAtMs),
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      unpackSeconds + recoverSeconds + restoreSeconds + censusSeconds,
		},
		"state": map[string]any{
			"work_dir": workDir, "data_dir": dataDir, "eval_time": evalTime,
		},
	}, nil
}

// engineProbeScript names the first tool the image is missing, so the
// refusal can say which half of the wrapper recipe was skipped.
const engineProbeScript = `for b in victoria-metrics vmrestore promtool; do
  command -v "$b" >/dev/null 2>&1 || { printf '%s\n' "$b"; exit 1; }
done`

// checkEngine verifies the sandbox image carries the three binaries a
// drill needs. They ship in three different images — the server, the
// vmrestore tool, and a query client VictoriaMetrics does not ship at
// all — so the drill sandbox runs the small wrapper the README
// documents, which also keeps tool and server versions identical.
func checkEngine(ctx context.Context, c *core) *protoError {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", engineProbeScript}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image provides no %s: a drill needs the server, vmrestore and a query "+
				"client together, and they ship in separate images — build the wrapper image the "+
				"adapter README documents", firstLine(stdout))
	}
	return nil
}

// transferArtifact moves the artifact into the sandbox: an archive is
// placed and unpacked, a directory tree is recreated file by file. It
// returns the directory vmrestore will read.
func transferArtifact(ctx context.Context, c *core, src *resolvedSource,
	workDir string) (transferSeconds, unpackSeconds float64, backupDir string, perr *protoError) {
	if src.tarball {
		return unpackArchive(ctx, c, src.path, workDir)
	}
	backupDir = path.Join(workDir, "backup")
	transferSeconds, perr = transferTree(ctx, c, src.path, backupDir)
	return transferSeconds, 0, backupDir, perr
}

// backupDirScript locates the backup after unpacking: an archive holds
// either the backup's own files at its root or one wrapping directory
// above them, so the script descends exactly one level when the root
// carries no completion marker and holds exactly one subdirectory.
const backupDirScript = `d="$1"
if [ ! -e "$d/` + completeMarker + `" ]; then
  set -- "$d"/*/
  if [ "$#" -eq 1 ] && [ -d "${1%/}" ]; then d="${1%/}"; fi
fi
printf '%s\n' "$d"`

// unpackArchive places the tar and unpacks it in the sandbox — busybox
// tar reads both the plain and the gzip form seamlessly.
func unpackArchive(ctx context.Context, c *core, hostPath, workDir string) (transferSeconds, unpackSeconds float64, backupDir string, perr *protoError) {
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
	locate, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", backupDirScript, "sh", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	backupDir = strings.TrimSpace(firstLine(stdout))
	if locate.ExitCode != 0 || backupDir == "" {
		backupDir = extractDir
	}
	return put.DurationSeconds, unpack.DurationSeconds + locate.DurationSeconds, backupDir, nil
}

// transferTree recreates the backup directory inside the sandbox: one
// mkdir for the directory skeleton, one put_file per file.
func transferTree(ctx context.Context, c *core, hostDir, backupDir string) (float64, *protoError) {
	dirs := []string{backupDir}
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
			dirs = append(dirs, path.Join(backupDir, filepath.ToSlash(rel)))
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
			DestPath:   path.Join(backupDir, rel),
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

// recoverMetadata reads the unpacked backup's own marker when the host
// could not walk the archive: the instant the checks evaluate at must
// come from somewhere real. The exec's exit code carries the only
// refusal — an unpacked tree with no backup metadata is not a backup —
// while unparseable content leaves created_at null, exactly as a
// directory artifact would.
func recoverMetadata(ctx context.Context, c *core, backupDir string) (backupInfo, float64, *protoError) {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{
		Argv: []string{"cat", path.Join(backupDir, metadataMarker)}})
	if perr != nil {
		return backupInfo{}, 0, perr
	}
	if val.ExitCode != 0 {
		return backupInfo{}, 0, protoErr("source_corrupt", false,
			"the unpacked archive carries no %s — not a vmbackup output: %s",
			metadataMarker, firstLine(stderr))
	}
	info := backupInfo{}
	if ms, ok := parseBackupMetadata(stdout); ok {
		info.createdAtMs = ms
	}
	return info, val.DurationSeconds, nil
}

// execRestore replays the backup into the sandbox's storage path.
// -skipBackupCompleteCheck is deliberately absent: the tool's own
// completeness refusal is one of this adapter's fences, not an obstacle
// to route around.
func execRestore(ctx context.Context, c *core, backupDir, dataDir string) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"vmrestore", "-src=fs://" + backupDir, "-storageDataPath=" + dataDir}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, mapRestoreFailure(stderr)
	}
	return val.DurationSeconds, nil
}

// mapRestoreFailure classifies vmrestore's own refusals. The completion
// marker is the one it names itself, and the host-side fence already
// refuses that artifact — this is the archive kind's backstop, where the
// host could not walk the tar.
func mapRestoreFailure(stderr []byte) *protoError {
	line := firstLine(stderr)
	if strings.Contains(line, completeMarker) {
		return refuseIncomplete()
	}
	return protoErr("restore_failed", false, "vmrestore failed: %s", line)
}

// startEngine launches the server in the background on the restored
// storage and waits for readiness. The server serves the backup and must
// not collect: no scrape configuration exists at all, and retention is
// pinned off (see retentionPeriod) so it cannot expire the artifact
// either.
func startEngine(ctx context.Context, c *core, workDir, dataDir string) (float64, *protoError) {
	logPath := path.Join(workDir, "victoria-metrics.log")
	script := fmt.Sprintf(
		`victoria-metrics -storageDataPath=%s -retentionPeriod=%s -httpListenAddr=%s `+
			`>%s 2>&1 </dev/null &`,
		dataDir, retentionPeriod, listenAddr, logPath)
	start, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", script}})
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"restored server failed to launch: %s", firstLine(stderr))
	}
	readySeconds, perr := awaitReady(ctx, c, logPath)
	if perr != nil {
		return 0, describeStartFailure(ctx, c, logPath, perr)
	}
	return start.DurationSeconds + readySeconds, nil
}

// awaitReady polls the health endpoint until the server answers. Between
// polls it also looks for the server's own fatal line, so a start that
// died instantly — a truncated backup does exactly that (measured) —
// fails the drill in seconds with the engine's reason instead of burning
// the whole readiness budget first.
func awaitReady(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	begin := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"wget", "-q", "-O", "/dev/null", serverURL + "/health"},
			TimeoutSeconds: 5,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(begin).Seconds(), nil
		}
		if fatal, _, _, perr := c.exec(ctx, execArgs{
			Argv: []string{"grep", "-q", "FATAL", logPath}}); perr == nil && fatal.ExitCode == 0 {
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
// last fatal line. A backup missing part of itself makes the server exit
// immediately with a precise message — `part "…" is listed in
// "…/parts.json", but is missing on disk`, or `cannot stat "…"` for a
// part missing only some of its files (both measured) — the backup's
// fault, named as such.
func describeStartFailure(ctx context.Context, c *core, logPath string, perr *protoError) *protoError {
	if perr.Code != "engine_not_ready" {
		return perr
	}
	val, stdout, _, eperr := c.exec(ctx, execArgs{Argv: []string{"grep", "-m", "1", "FATAL", logPath}})
	if eperr != nil || val.ExitCode != 0 {
		return perr
	}
	line := firstLine(stdout)
	if line == "" {
		return perr
	}
	if strings.Contains(line, "missing on disk") || strings.Contains(line, "cannot stat") {
		return protoErr("source_corrupt", false,
			"the restored storage is incomplete: %s", line)
	}
	return protoErr("restore_failed", false, "restored server failed to start: %s", line)
}

// tsdbStatus is the shape /api/v1/status/tsdb answers with.
type tsdbStatus struct {
	Data struct {
		TotalSeries int64 `json:"totalSeries"`
	} `json:"data"`
}

// checkSeries refuses a well-formed zero: a server that is up but holds
// none of the promised data is exactly the false green a metrics backup
// invites. The count comes from the server's own status endpoint rather
// than a query, so no lookback window stands between the artifact and
// the verdict — a backup of an idle instance is still a backup. The
// refusal fires on positive evidence only: an answer that does not parse
// is skipped rather than treated as zero.
func checkSeries(ctx context.Context, c *core, createdAtMs int64) (float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"wget", "-q", "-O-", serverURL + "/api/v1/status/tsdb"}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return val.DurationSeconds, nil
	}
	status := tsdbStatus{}
	if err := json.Unmarshal(stdout, &status); err != nil {
		return val.DurationSeconds, nil
	}
	if status.Data.TotalSeries == 0 {
		return 0, protoErr("source_corrupt", false,
			"the restored server holds no series at all, though the backup claims to have been "+
				"taken at %s — the data the backup promises is not there",
			time.UnixMilli(createdAtMs).UTC().Format(createdAtLayout))
	}
	return val.DurationSeconds, nil
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
// data (§6.3), through the same client and at the same instant the
// checks evaluate at. An unhealthy server is a valid result, not an
// operation error.
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
