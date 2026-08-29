package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	adapterName    = "influxdb"
	adapterVersion = "0.3.0"

	// Where the restored instance serves inside the sandbox. No TLS and
	// no operator credentials: a Probavi sandbox is zero-ingress
	// (--network none, no ports expressible), which is the only reason
	// the fixed sandbox-local token below is acceptable — it is a
	// documented public constant, never a secret, the influxdb analog of
	// the postgres adapter's trust auth and the mssql adapter's published
	// sa password.
	listenAddr = "127.0.0.1:8086"
	serverURL  = "http://" + listenAddr

	// The initial-setup values the adapter starts the drill instance
	// with. They are documented CONSTANTS, not secrets: InfluxDB 2.x
	// refuses to serve before `influx setup` creates an operator token,
	// and the ephemeral core-generated secret cannot be used — its value
	// must never cross the protocol (§2.5), yet setting one requires
	// exactly that. So the adapter sets the instance up itself with
	// these fixed values, the InfluxDB equivalent of the postgres
	// adapter's pg_hba trust overwrite and the mssql adapter's published
	// sa password: publicly known access, confined to a sandbox with
	// zero ingress (--network none, no ports expressible). The token
	// never protects anything reachable, and the restore uses it rather
	// than any credential out of the backup (see README).
	sandboxUser   = "probavi"
	sandboxPass   = "probavi-sandbox"
	sandboxToken  = "probavi-sandbox-token" //nolint:gosec // deliberately public constant, not a credential — see above
	sandboxOrg    = "probavi-drill"
	sandboxBucket = "probavi-init"

	// workDirName is created under the provider's scratch directory —
	// the one directory the provider guarantees writable.
	workDirName = "probavi-influxdb"

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
		"engine":            map[string]string{"name": "influxdb"},
		"sources": []map[string]any{
			{"kind": "influx_backup_tar", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "influx_backup", "capabilities": map[string]bool{"pitr": false}},
			{"kind": "influx_backup_dir", "capabilities": map[string]bool{"pitr": false}},
		},
		"sql_runner": map[string]any{
			// InfluxDB 2.x has no SQL: the check text the core passes
			// through {{sql}} is one Flux query, delivered as a single
			// argv element — no shell anywhere. {{database}} carries the
			// organization provision returned; the token is the documented
			// sandbox constant above. The engine dialect is absorbed here,
			// declaratively — the core never learns it (§6.1).
			"argv": []string{"influx", "query", "--host", serverURL,
				"-t", sandboxToken, "-o", "{{database}}", "{{sql}}"},
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

// opProvision restores the backup into the idle sandbox: preflight
// (influxd present and 2.x), transfer the manifest's members, start the
// instance on adapter-owned paths, initialize it with the sandbox
// constants, `influx restore` with the sandbox token — a plain restore
// creates the backup's own organizations and buckets, so no credential
// from the backup is ever needed (measured) — then the bucket census.
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
	// A directory kind's manifest was read on the host; an archive the
	// host could walk carries the same facts. Only an opaque archive
	// leaves the organizations unknown here — recovered from the
	// unpacked tree below.
	if perr := checkKnownOrgs(src, req.Options["database"]); perr != nil {
		return nil, perr
	}
	logger.Info("source resolved", "path", src.dir, "size_bytes", src.sizeBytes,
		"organizations", len(src.orgs))

	if perr := checkEngine(ctx, c); perr != nil {
		return nil, perr
	}

	workDir := path.Join(scratch, workDirName)
	transferSeconds, unpackSeconds, recoverSeconds, backupDir, perr := stageArtifact(ctx, c, src, workDir)
	if perr != nil {
		return nil, perr
	}
	org, perr := connectionOrg(src, req.Options["database"])
	if perr != nil {
		return nil, perr
	}

	readySeconds, perr := startEngine(ctx, c, workDir)
	if perr != nil {
		return nil, perr
	}
	setup, perr := setupInstance(ctx, c)
	if perr != nil {
		return nil, perr
	}

	restore, perr := execRestore(ctx, c, backupDir)
	if perr != nil {
		return nil, perr
	}
	censusSeconds, perr := checkBucketCensus(ctx, c, org, src.orgs[org])
	if perr != nil {
		return nil, perr
	}
	logger.Info("backup restored and verified", "organization", org,
		"buckets", len(src.orgs[org]), "restore_seconds", restore.DurationSeconds)

	return map[string]any{
		"connection": map[string]any{
			// Checks reach the instance over HTTP; database carries the
			// organization the declared runner's -o consumes.
			"scheme": "http", "host": "127.0.0.1", "port": 8086,
			"database": org, "user": "",
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds + setup,
			"transfer_seconds":     transferSeconds,
			"restore_seconds":      unpackSeconds + recoverSeconds + restore.DurationSeconds + censusSeconds,
		},
		"state": map[string]any{
			"work_dir": workDir, "backup_dir": backupDir, "org": org,
		},
	}, nil
}

// checkKnownOrgs applies connectionOrg's refusals as early as the facts
// allow: before any sandbox call when the organizations are known
// host-side, so a misconfigured drill never spends a restore finding
// out.
func checkKnownOrgs(src *resolvedSource, requested string) *protoError {
	if len(src.orgs) == 0 {
		return nil
	}
	_, perr := connectionOrg(src, requested)
	return perr
}

// connectionOrg picks the organization checks run against: the
// manifest's single organization, or the one options.database names
// when the backup holds several — a guess between organizations would
// decide silently what the drill proves. When the organizations could
// not be read at all (an opaque archive whose recovered manifest was
// unreadable too), the choice falls to options.database or stays empty:
// `influx restore` is then the only authority left, and the census has
// nothing to compare.
func connectionOrg(src *resolvedSource, requested string) (string, *protoError) {
	if len(src.orgs) == 0 {
		return requested, nil
	}
	names := make([]string, 0, len(src.orgs))
	for name := range src.orgs {
		names = append(names, name)
	}
	sort.Strings(names)
	if requested != "" {
		if _, ok := src.orgs[requested]; !ok {
			return "", protoErr("invalid_request", false,
				"options.database names organization %q, which the backup does not hold "+
					"(it holds: %s)", requested, strings.Join(names, ", "))
		}
		return requested, nil
	}
	if len(names) == 1 {
		return names[0], nil
	}
	return "", protoErr("invalid_request", false,
		"the backup holds %d organizations (%s): set options.database to the one this drill's "+
			"checks should run against — all of them are restored either way",
		len(names), strings.Join(names, ", "))
}

// stageTarball places the archive and unpacks it in the sandbox — GNU
// tar detects gzip on its own — then locates the directory the manifest
// unpacked into: the archive's members sit at its root or under one
// wrapping directory.
func stageTarball(ctx context.Context, c *core, hostPath, workDir string) (transfer, unpack float64, backupDir string, perr *protoError) {
	extractDir := workDir + "/extract"
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mkdir", "-p", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	if val.ExitCode != 0 {
		return 0, 0, "", protoErr("internal", false, "prepare extract directory: %s", firstLine(stderr))
	}
	tarPath := workDir + "/backup.tar"
	put, perr := c.putFile(ctx, putFileArgs{SourcePath: hostPath, DestPath: tarPath, Mode: "0600"})
	if perr != nil {
		return 0, 0, "", perr
	}
	untar, _, stderr2, perr := c.exec(ctx, execArgs{Argv: []string{"tar", "-xf", tarPath, "-C", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	if untar.ExitCode != 0 {
		return 0, 0, "", protoErr("source_corrupt", false,
			"the archive could not be unpacked: %s", firstLine(stderr2))
	}
	locate, stdout, stderr3, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", locateManifestScript, "sh", extractDir}})
	if perr != nil {
		return 0, 0, "", perr
	}
	if locate.ExitCode != 0 {
		return 0, 0, "", protoErr("source_corrupt", false,
			"the unpacked archive holds no backup manifest — not a tar of an `influx backup` "+
				"directory: %s", firstLine(stderr3))
	}
	return put.DurationSeconds, untar.DurationSeconds + locate.DurationSeconds,
		firstLine(stdout), nil
}

// locateManifestScript prints the directory holding the unpacked
// manifest: the extract root, or the single wrapping directory a tar of
// a backup directory naturally produces.
const locateManifestScript = `
for f in "$1"/*.manifest "$1"/*/*.manifest; do
  [ -e "$f" ] && { dirname "$f"; exit 0; }
done
exit 1
`

// recoverOrgs reads the unpacked manifest when the host could not walk
// the archive: the facts must come from somewhere real before the
// census can mean anything, and the fences must fire here too — a
// tarred 1.x portable backup is the same migration whichever side reads
// its manifest. Content that is not one readable manifest stays a
// bonus-style skip: `influx restore` is then the authority.
func recoverOrgs(ctx context.Context, c *core, backupDir string) (map[string][]string, float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", `exec cat "$1"/*.manifest`, "sh", backupDir}})
	if perr != nil {
		return nil, 0, perr
	}
	if val.ExitCode != 0 {
		return nil, val.DurationSeconds, nil
	}
	m, mperr := parseManifestBytes(stdout, "the recovered manifest")
	if mperr != nil {
		if mperr.Code == "unsupported_source" {
			return nil, 0, mperr
		}
		return nil, val.DurationSeconds, nil
	}
	return orgsOf(m), val.DurationSeconds, nil
}

// checkEngine verifies the image carries a 2.x influxd and the influx
// CLI. A 1.x image is refused by name — its engine cannot read a 2.x
// backup, and the operator pointing one here is one migration
// misunderstanding away from the fence in parseManifest — and so is
// anything else that is not the 2.x line this adapter is verified
// against.
func checkEngine(ctx context.Context, c *core) *protoError {
	val, stdout, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"influxd", "version"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"sandbox image lacks influxd (%s): use an official influxdb:2.x image with "+
				"command: sleep infinity", firstLine(stderr))
	}
	// Positive evidence only (§10: the simulated sandbox answers every
	// exec with "1"): a version line NAMING another InfluxDB line is
	// refused; output naming nothing is left for the flow to judge.
	out := firstLine(stdout)
	if strings.Contains(out, "InfluxDB") && !strings.Contains(out, "InfluxDB v2.") {
		return protoErr("invalid_request", false,
			"the sandbox engine is not the InfluxDB 2.x line this adapter restores into (%s) — "+
				"use an official influxdb:2.x image", out)
	}
	cli, _, stderr2, perr := c.exec(ctx, execArgs{Argv: []string{"influx", "version"}})
	if perr != nil {
		return perr
	}
	if cli.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"sandbox image lacks the influx CLI, which the restore is driven with: %s", firstLine(stderr2))
	}
	return nil
}

// stageArtifact places the artifact in the sandbox and returns the
// directory the restore reads. For an opaque archive it also recovers
// the organizations from the unpacked tree, so the census and the
// fences run on facts read from somewhere real.
func stageArtifact(ctx context.Context, c *core, src *resolvedSource, workDir string) (transfer, unpack, recover float64, backupDir string, perr *protoError) {
	backupDir = workDir + "/backup"
	if src.tarball {
		transfer, unpack, backupDir, perr = stageTarball(ctx, c, src.dir, workDir)
	} else {
		transfer, perr = stageBackup(ctx, c, src, backupDir)
	}
	if perr != nil {
		return 0, 0, 0, "", perr
	}
	if src.tarball && len(src.orgs) == 0 {
		src.orgs, recover, perr = recoverOrgs(ctx, c, backupDir)
		if perr != nil {
			return 0, 0, 0, "", perr
		}
	}
	return transfer, unpack, recover, backupDir, nil
}

// stageBackup places the manifest and every member it names into the
// sandbox.
func stageBackup(ctx context.Context, c *core, src *resolvedSource, backupDir string) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"mkdir", "-p", backupDir}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("internal", false, "prepare backup directory: %s", firstLine(stderr))
	}
	total := 0.0
	for _, name := range src.files {
		put, perr := c.putFile(ctx, putFileArgs{
			SourcePath: path.Join(src.dir, name),
			DestPath:   backupDir + "/" + name, Mode: "0600",
		})
		if perr != nil {
			return 0, perr
		}
		total += put.DurationSeconds
	}
	return total, nil
}

// startEngine launches influxd in the background on adapter-owned paths
// and waits until it answers ping. The engine paths live under the
// scratch directory, so nothing of the image's own data layout is
// touched.
func startEngine(ctx context.Context, c *core, workDir string) (float64, *protoError) {
	logPath := workDir + "/influxd.log"
	script := fmt.Sprintf(
		"influxd --bolt-path %s/influxd.bolt --engine-path %s/engine --sqlite-path %s/influxd.sqlite "+
			"--http-bind-address %s --storage-retention-check-interval %s >%s 2>&1 </dev/null &",
		workDir, workDir, workDir, listenAddr, retentionCheckInterval, logPath)
	start, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"sh", "-c", script}})
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"the instance failed to launch: %s", firstLine(stderr))
	}
	readySeconds, perr := awaitEngine(ctx, c, logPath)
	if perr != nil {
		return 0, perr
	}
	return start.DurationSeconds + readySeconds, nil
}

// awaitEngine polls `influx ping` until the instance answers, and on a
// timeout surfaces the engine's own last error line rather than "never
// became ready".
func awaitEngine(ctx context.Context, c *core, logPath string) (float64, *protoError) {
	begin := time.Now()
	for {
		val, _, _, perr := c.exec(ctx, execArgs{
			Argv:           []string{"influx", "ping", "--host", serverURL},
			TimeoutSeconds: 5,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 {
			return time.Since(begin).Seconds(), nil
		}
		if time.Since(begin) > readinessBudget {
			return 0, describeStartFailure(ctx, c, logPath)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for engine readiness")
		case <-time.After(readinessPoll):
		}
	}
}

func describeStartFailure(ctx context.Context, c *core, logPath string) *protoError {
	timeout := protoErr("engine_not_ready", true,
		"the instance did not answer ping within %s", readinessBudget)
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"tail", "-n", "20", logPath}})
	if perr != nil || val.ExitCode != 0 {
		return timeout
	}
	if line := lastErrorLine(stdout); line != "" {
		return protoErr("restore_failed", false, "the instance failed to start: %s", line)
	}
	return timeout
}

// lastErrorLine picks the last log line that reads like the engine's own
// failure report.
func lastErrorLine(log []byte) string {
	found := ""
	for _, line := range strings.Split(string(log), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "lvl=error") || strings.Contains(lower, "fatal") {
			found = strings.TrimSpace(line)
		}
	}
	return strings.ReplaceAll(found, `"`, "'")
}

// setupInstance initializes the fresh instance with the documented
// sandbox constants. The restore that follows runs with this token and
// creates the backup's own organizations beside the sandbox one
// (measured), so the drill never needs a credential from the backup.
func setupInstance(ctx context.Context, c *core) (float64, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"influx", "setup", "-f", "--host", serverURL,
		"--username", sandboxUser, "--password", sandboxPass,
		"--org", sandboxOrg, "--bucket", sandboxBucket, "--token", sandboxToken,
	}})
	if perr != nil {
		return 0, perr
	}
	if val.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"initializing the sandbox instance failed: %s", firstLine(stderr))
	}
	return val.DurationSeconds, nil
}

// execRestore drives `influx restore` — the plain form, deliberately:
// --full would replace the KV store and with it every credential,
// locking the drill out behind the backup's own tokens (see README).
func execRestore(ctx context.Context, c *core, backupDir string) (*execValue, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{
		"influx", "restore", backupDir, "--host", serverURL, "-t", sandboxToken,
	}})
	if perr != nil {
		return nil, perr
	}
	if val.ExitCode != 0 {
		line := firstLine(stderr)
		if strings.Contains(line, "manifest") || strings.Contains(line, "gzip") ||
			strings.Contains(line, "EOF") {
			return nil, protoErr("source_corrupt", false, "influx restore rejected the backup: %s", line)
		}
		return nil, protoErr("restore_failed", false, "influx restore failed: %s", line)
	}
	return val, nil
}

// checkBucketCensus compares the buckets the restored organization
// actually holds — the instance's own listing — with the ones the
// manifest names for it. The restore creates the organization itself,
// so every bucket in it must have come from the backup; one missing is
// a partial restore, which no drill may report as green.
func checkBucketCensus(ctx context.Context, c *core, org string, wantBuckets []string) (float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{
		"influx", "bucket", "list", "--host", serverURL, "-t", sandboxToken,
		"-o", org, "--json",
	}})
	if perr != nil {
		return 0, perr
	}
	// The comparison fires on positive evidence only: a listing that
	// failed, or output that is not the CLI's JSON, states no count to
	// compare against, and `influx restore`'s own exit already spoke.
	if val.ExitCode != 0 {
		return val.DurationSeconds, nil
	}
	var listed []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(stdout, &listed); err != nil {
		return val.DurationSeconds, nil
	}
	have := map[string]bool{}
	for _, b := range listed {
		have[b.Name] = true
	}
	missing := []string{}
	for _, name := range wantBuckets {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return 0, protoErr("source_corrupt", false,
			"the restored organization %s holds %d of the %d buckets the backup names for it "+
				"(missing: %s) — the drill refuses to prove a partial restore",
			org, len(wantBuckets)-len(missing), len(wantBuckets), strings.Join(missing, ", "))
	}
	return val.DurationSeconds, nil
}

// healthcheckRequest is the §6.3 request payload.
type healthcheckRequest struct {
	State json.RawMessage `json:"state"`
}

// opHealthcheck verifies the restored instance still answers (§6.3). An
// unhealthy engine is a valid result, not an operation error.
func opHealthcheck(ctx context.Context, c *core, payload json.RawMessage) (any, *protoError) {
	req := &healthcheckRequest{}
	if err := json.Unmarshal(payload, req); err != nil {
		return nil, protoErr("invalid_request", false, "malformed healthcheck payload")
	}
	val, _, _, perr := c.exec(ctx, execArgs{Argv: []string{"influx", "ping", "--host", serverURL}})
	if perr != nil {
		return nil, perr
	}
	healthy := val.ExitCode == 0
	detail := "ping answered"
	if !healthy {
		detail = fmt.Sprintf("influx ping exited %d", val.ExitCode)
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
