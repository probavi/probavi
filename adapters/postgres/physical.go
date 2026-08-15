package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// Physical restore (pgbackrest source kind): unlike pg_restore, a pgBackRest
// restore replaces the data directory, so the engine must NOT be running.
// The drill config must start the sandbox idle (docker provider:
// `command: sleep infinity`) with an image that contains postgres,
// pgbackrest, and gosu. The adapter then owns the whole lifecycle:
// transfer repo → write config → restore → open sandbox-local auth →
// start the server → wait for recovery to finish.
const pgdataDir = "/var/lib/postgresql/data"

// pgbackrestTimeFormat is the recovery-target form both pgbackrest
// (--target) and postgresql (recovery_target_time) parse: an explicit UTC
// offset, no RFC 3339 "T"/"Z" shorthand.
const pgbackrestTimeFormat = "2006-01-02 15:04:05.000000+00"

var stanzaPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// resolvePITRTarget converts the protocol's RFC 3339 pitr.target_time into
// pgbackrest's --target form; empty when the drill did not request PITR.
func resolvePITRTarget(req *provisionRequest) (string, *protoError) {
	if req.PITR == nil {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339, req.PITR.TargetTime)
	if err != nil {
		return "", protoErr("invalid_request", false,
			"pitr.target_time %q is not an RFC 3339 timestamp", req.PITR.TargetTime)
	}
	return ts.UTC().Format(pgbackrestTimeFormat), nil
}

// provisionPhysical runs the pgbackrest provision flow and returns the
// §6.2 response payload.
func provisionPhysical(ctx context.Context, c *core, req *provisionRequest, src *resolvedSource, logger *slog.Logger) (any, *protoError) {
	stanza := req.Source.Params["stanza"]
	if !stanzaPattern.MatchString(stanza) {
		return nil, protoErr("invalid_request", false,
			"pgbackrest source requires source.params.stanza (letters, digits, - and _)")
	}
	pitrTarget, perr := resolvePITRTarget(req)
	if perr != nil {
		return nil, perr
	}
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	repoInSandbox := scratch + "/probavi-pgbackrest-repo"

	if perr := checkIdleSandbox(ctx, c); perr != nil {
		return nil, perr
	}
	if perr := checkEngineVersion(ctx, c, repoDBVersion(src.path, stanza)); perr != nil {
		return nil, perr
	}

	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: repoInSandbox, Mode: "0755"})
	if perr != nil {
		return nil, perr
	}

	if perr := prepareRestore(ctx, c, repoInSandbox, stanza); perr != nil {
		return nil, perr
	}

	restoreArgv := []string{"gosu", "postgres", "pgbackrest", "--stanza=" + stanza, "restore"}
	if pitrTarget != "" {
		// --target-action=promote: recovery stops at the target and the
		// instance opens read-write; the postgres default (pause) would
		// leave the drill hanging at the target point forever.
		restoreArgv = append(restoreArgv,
			"--type=time", "--target="+pitrTarget, "--target-action=promote")
		logger.Info("point-in-time recovery requested", "target", pitrTarget)
	}
	restore, stderr, perr := execChecked(ctx, c, restoreArgv...)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, protoErr("restore_failed", false, "pgbackrest restore failed: %s", firstLine(stderr))
	}
	logger.Info("pgbackrest restore complete", "seconds", restore.DurationSeconds)

	readySeconds, perr := startEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine recovered and ready", "seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "postgresql", "host": "127.0.0.1", "port": defaultPort,
			"database": defaultDatabase, "user": defaultUser,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			"restore_seconds":      restore.DurationSeconds,
		},
		"state": map[string]any{
			"database": defaultDatabase, "user": defaultUser, "mode": "physical", "stanza": stanza,
		},
	}, nil
}

// checkIdleSandbox verifies the preconditions of a physical restore: no
// engine running, pgbackrest present.
func checkIdleSandbox(ctx context.Context, c *core) *protoError {
	ready, _, perr := execChecked(ctx, c, "pg_isready", "-h", "127.0.0.1", "-U", defaultUser, "-q")
	if perr != nil {
		return perr
	}
	if ready.ExitCode == 0 {
		return protoErr("invalid_request", false,
			"pgbackrest restore needs an idle sandbox: set sandbox params command to keep the engine stopped (docker: command: sleep infinity)")
	}
	version, stderr, perr := execChecked(ctx, c, "pgbackrest", "version")
	if perr != nil {
		return perr
	}
	if version.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"sandbox image lacks pgbackrest (%s): use an image with postgres, pgbackrest, and gosu", firstLine(stderr))
	}
	return nil
}

// engineVersionPattern finds the server version in `postgres --version`
// output ("postgres (PostgreSQL) 16.9 (Debian 16.9-1.pgdg120+1)").
var engineVersionPattern = regexp.MustCompile(`\d+(?:\.\d+)*`)

// checkEngineVersion refuses the one pairing a physical restore can never
// survive: a repository from one PostgreSQL major handed to a sandbox
// running another (docs/engine-versions.md §5). It refuses only on
// positive evidence — an unreadable manifest arrives here as "", an
// unanswerable or unparseable version query skips the check — and the
// restore then speaks for itself.
func checkEngineVersion(ctx context.Context, c *core, backupVersion string) *protoError {
	if backupVersion == "" {
		return nil
	}
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"postgres", "--version"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return nil
	}
	engine := engineVersionPattern.FindString(string(stdout))
	if engine == "" {
		return nil
	}
	// Compare at the granularity the backup states: "16" against the
	// engine's major, "9.6" against its first two components.
	series := seriesOf(engine, 1+strings.Count(backupVersion, "."))
	if series == backupVersion {
		return nil
	}
	return protoErr("invalid_request", false,
		"the pgBackRest repository holds a PostgreSQL %s backup, but the sandbox engine is PostgreSQL %s: a physical backup restores only into its own major version — use a PostgreSQL %s sandbox image",
		backupVersion, series, backupVersion)
}

// seriesOf truncates a version to its first n dot-separated components.
func seriesOf(version string, n int) string {
	parts := strings.Split(version, ".")
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, ".")
}

// prepareRestore writes the pgbackrest config, empties the data directory,
// hands the repo to the postgres user, and opens sandbox-local trust auth
// for after the restore. All paths are adapter-controlled constants; the
// stanza is pattern-validated.
func prepareRestore(ctx context.Context, c *core, repo, stanza string) *protoError {
	script := fmt.Sprintf(
		`set -e
mkdir -p /etc/pgbackrest
printf '[global]\nrepo1-path=%s\n\n[%s]\npg1-path=%s\n' > /etc/pgbackrest/pgbackrest.conf
rm -rf %s/* %s/.[!.]* 2>/dev/null || true
chown -R postgres:postgres %s %s /etc/pgbackrest`,
		repo, stanza, pgdataDir, pgdataDir, pgdataDir, repo, pgdataDir)
	res, stderr, perr := execChecked(ctx, c, "sh", "-c", script)
	if perr != nil {
		return perr
	}
	if res.ExitCode != 0 {
		return protoErr("internal", false, "prepare restore environment: %s", firstLine(stderr))
	}
	return nil
}

// startEngine overwrites pg_hba with sandbox-local trust (the restored
// cluster's auth config expects credentials this drill does not have — the
// sandbox has no network exposure, so trust is confined to the container),
// starts the server, and waits until recovery finishes and queries are
// accepted. Returns the measured wait in seconds.
func startEngine(ctx context.Context, c *core) (float64, *protoError) {
	script := fmt.Sprintf(
		`set -e
printf 'local all all trust\nhost all all 127.0.0.1/32 trust\nhost all all ::1/128 trust\n' > %s/pg_hba.conf
chown postgres:postgres %s/pg_hba.conf`, pgdataDir, pgdataDir)
	if res, stderr, perr := execChecked(ctx, c, "sh", "-c", script); perr != nil {
		return 0, perr
	} else if res.ExitCode != 0 {
		return 0, protoErr("internal", false, "write sandbox auth config: %s", firstLine(stderr))
	}

	// Start via sh so a failed start surfaces the server log's FATAL lines
	// — for pitr that is where "recovery ended before configured recovery
	// target" is reported.
	startScript := fmt.Sprintf(
		`gosu postgres pg_ctl -D %s -w -t 600 -l /tmp/probavi-pg.log start || { tail -n 20 /tmp/probavi-pg.log >&2; exit 1; }`,
		pgdataDir)
	start, stderr, perr := execChecked(ctx, c, "sh", "-c", startScript)
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, mapStartFailure(stderr)
	}
	readySeconds, perr := awaitEngine(ctx, c, defaultUser)
	if perr != nil {
		return 0, perr
	}
	promoteSeconds, perr := awaitPromotion(ctx, c)
	if perr != nil {
		return 0, perr
	}
	return start.DurationSeconds + readySeconds + promoteSeconds, nil
}

// mapStartFailure classifies a failed server start. Recovery ending before
// the requested target is a verdict about the backup and its WAL archive —
// they do not cover the target time — not an infrastructure error.
func mapStartFailure(stderr []byte) *protoError {
	if strings.Contains(string(stderr), "recovery ended before configured recovery target was reached") {
		return protoErr("restore_failed", false,
			"pitr target not reachable: recovery ended before the requested target time — the backup and its WAL archive do not cover it")
	}
	return protoErr("restore_failed", false, "restored cluster failed to start: %s", firstLine(stderr))
}

// awaitPromotion waits until recovery has finished and the instance is
// writable. Hot standby answers pg_isready during WAL replay, so readiness
// alone would let checks run against a still-recovering — for pitr,
// possibly pre-target — instance.
func awaitPromotion(ctx context.Context, c *core) (float64, *protoError) {
	start := time.Now()
	for {
		val, stdout, _, perr := c.exec(ctx, execArgs{
			Argv: []string{"psql", "-h", "127.0.0.1", "-U", defaultUser, "-d", defaultDatabase,
				"-tA", "-c", "SELECT pg_is_in_recovery()"},
			TimeoutSeconds: 5,
		})
		if perr != nil {
			return 0, perr
		}
		if val.ExitCode == 0 && strings.TrimSpace(string(stdout)) == "f" {
			return time.Since(start).Seconds(), nil
		}
		if time.Since(start) > readinessBudget {
			return 0, protoErr("engine_not_ready", true, "recovery did not finish within %s", readinessBudget)
		}
		select {
		case <-ctx.Done():
			return 0, protoErr("cancelled", true, "cancelled while waiting for recovery to finish")
		case <-time.After(readinessPoll):
		}
	}
}

// execChecked wraps core.exec returning the value and raw stderr.
func execChecked(ctx context.Context, c *core, argv ...string) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}
