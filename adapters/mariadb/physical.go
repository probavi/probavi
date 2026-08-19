package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// Physical restore (mariadb_backup source kind): a mariadb-backup restore
// replaces the data directory, so the engine must NOT be running. The
// drill config must start the sandbox idle (docker provider: `command:
// sleep infinity`) with an image that contains mariadbd, mariadb-backup,
// and gosu — the official mariadb images carry all three, so unlike the
// sibling adapter's XtraBackup flow no separate tool image is involved.
// The adapter then owns the whole lifecycle: transfer backup → prepare +
// copy-back → open sandbox-local auth → start the server → wait for
// readiness.
const (
	// datadir is where the official mariadb images keep the data
	// directory. It is the same bet on an image layout the sibling
	// adapters make, recorded as such: the server is not running when the
	// restore needs the answer, so there is nothing to ask.
	datadir      = "/var/lib/mysql"
	initFilePath = "/tmp/probavi-init.sql"

	// physicalDatabase is the connection database for physical restores:
	// the system schema is the only database guaranteed to exist in an
	// arbitrary restored server. Checks against restored data should use
	// schema-qualified table names.
	physicalDatabase = "mysql"
)

// provisionPhysical runs the xtrabackup provision flow and returns the
// §6.2 response payload. Options are ignored: the restored server is
// served as root with the connection database fixed to the system schema,
// mirroring the postgres adapter's physical mode.
func provisionPhysical(ctx context.Context, c *core, req *provisionRequest, src *resolvedSource, logger *slog.Logger) (any, *protoError) {
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	backupInSandbox := scratch + "/probavi-mariadb-backup"

	if perr := checkIdleSandbox(ctx, c); perr != nil {
		return nil, perr
	}
	if perr := checkEngineVersion(ctx, c, backupSeries(src.path)); perr != nil {
		return nil, perr
	}

	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: backupInSandbox, Mode: "0755"})
	if perr != nil {
		return nil, perr
	}

	if perr := prepareRestore(ctx, c); perr != nil {
		return nil, perr
	}

	// prepare (redo apply) and copy-back are both restore work: one timed
	// execution so restore_seconds is a single honest measurement.
	restoreScript := fmt.Sprintf(`set -e
mariadb-backup --prepare --target-dir=%s
mariadb-backup --copy-back --target-dir=%s --datadir=%s
chown -R mysql:mysql %s`, backupInSandbox, backupInSandbox, datadir, datadir)
	restore, stderr, perr := execChecked(ctx, c, "sh", "-c", restoreScript)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, protoErr("restore_failed", false, "mariadb-backup restore failed: %s", firstLine(stderr))
	}
	logger.Info("xtrabackup restore complete", "seconds", restore.DurationSeconds)

	readySeconds, perr := startEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine recovered and ready", "seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "mariadb", "host": "127.0.0.1", "port": defaultPort,
			"database": physicalDatabase, "user": defaultUser,
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
			"database": physicalDatabase, "user": defaultUser, "mode": "physical",
		},
	}, nil
}

// engineSeriesPattern finds the release series in `mariadbd --version`
// output ("mariadbd  Ver 11.4.7-MariaDB-ubu2404-log for debian-linux-gnu
// on x86_64 (mariadb.org binary distribution)"). The binary name carries
// no digits, so the first match is the version.
var engineSeriesPattern = regexp.MustCompile(`\d+\.\d+`)

// checkEngineVersion refuses the one pairing a physical restore can never
// survive: a backup from one release series handed to a sandbox running
// another (docs/engine-versions.md §5). It refuses only on positive
// evidence — a backup without a readable server_version arrives here as
// "", an unanswerable or unparseable version query skips the check — and
// the restore then speaks for itself.
func checkEngineVersion(ctx context.Context, c *core, series string) *protoError {
	if series == "" {
		return nil
	}
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"mariadbd", "--version"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return nil
	}
	engine := engineSeriesPattern.FindString(string(stdout))
	if engine == "" || engine == series {
		return nil
	}
	return protoErr("invalid_request", false,
		"the mariadb-backup backup was taken from server release series %s, but the sandbox engine is %s: a physical backup restores only into its own release series — use a mariadb:%s sandbox image",
		series, engine, series)
}

// checkIdleSandbox verifies the preconditions of a physical restore: no
// engine running, mariadb-backup present.
func checkIdleSandbox(ctx context.Context, c *core) *protoError {
	running, _, perr := execChecked(ctx, c,
		"mariadb", "-h", "127.0.0.1", "-u", defaultUser, "-N", "-B", "-e", "SELECT 1")
	if perr != nil {
		return perr
	}
	if running.ExitCode == 0 {
		return protoErr("invalid_request", false,
			"a mariadb_backup restore needs an idle sandbox: set sandbox params command to keep the engine stopped (docker: command: sleep infinity)")
	}
	version, stderr, perr := execChecked(ctx, c, "mariadb-backup", "--version")
	if perr != nil {
		return perr
	}
	if version.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"sandbox image lacks mariadb-backup (%s): the official mariadb images carry it", firstLine(stderr))
	}
	return nil
}

// prepareRestore empties the data directory and writes the startup init
// file that resets sandbox-local root auth: the restored grant tables carry
// production credentials this drill does not have — the sandbox has no
// network exposure, so the empty password is confined to the disposable
// container (same rationale as the postgres adapter's pg_hba overwrite).
// All paths are adapter-controlled constants.
func prepareRestore(ctx context.Context, c *core) *protoError {
	script := fmt.Sprintf(`set -e
rm -rf %s/* %s/.[!.]* 2>/dev/null || true
printf "CREATE USER IF NOT EXISTS 'root'@'%%%%' IDENTIFIED BY '';\nALTER USER 'root'@'%%%%' IDENTIFIED BY '';\nGRANT ALL PRIVILEGES ON *.* TO 'root'@'%%%%' WITH GRANT OPTION;\nFLUSH PRIVILEGES;\n" > %s
chown mysql:mysql %s`,
		datadir, datadir, initFilePath, initFilePath)
	res, stderr, perr := execChecked(ctx, c, "sh", "-c", script)
	if perr != nil {
		return perr
	}
	if res.ExitCode != 0 {
		return protoErr("internal", false, "prepare restore environment: %s", firstLine(stderr))
	}
	return nil
}

// startEngine starts the restored server with the auth-reset init file and
// waits until it serves queries. Returns the measured wait in seconds.
//
// mariadbd has no --daemonize (measured — the option mysqld has simply
// does not exist here), so the server is backgrounded by the shell with
// its streams detached; once the exec's own process exits, the server is
// reparented to the sandbox's init and lives on. The consequence is that
// launch failures cannot surface in the launch script's exit code: they
// surface as the readiness wait timing out, so that path reads the
// server's own error log before blaming the engine.
func startEngine(ctx context.Context, c *core) (float64, *protoError) {
	script := fmt.Sprintf(`set -e
mkdir -p /var/run/mysqld && chown mysql:mysql /var/run/mysqld
gosu mysql mariadbd %s --init-file=%s --log-error=%s >/dev/null 2>&1 </dev/null &`,
		eventSchedulerFlag, initFilePath, serverErrLog)
	start, stderr, perr := execChecked(ctx, c, "sh", "-c", script)
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false, "restored server failed to launch: %s", firstLine(stderr))
	}
	readySeconds, perr := awaitEngine(ctx, c, defaultUser)
	if perr != nil {
		return 0, describeStartFailure(ctx, c, perr)
	}
	// The flag above is the pin; this is the server confirming it, which
	// is what the drill is entitled to rest on (retention.go).
	pinSeconds, perr := pinEventScheduler(ctx, c, defaultUser)
	if perr != nil {
		return 0, perr
	}
	return start.DurationSeconds + readySeconds + pinSeconds, nil
}

// serverErrLog is where the restored server writes its error log; the
// timeout path below reads it so a start failure names the engine's own
// reason instead of "never became ready".
const serverErrLog = "/tmp/probavi-mariadbd.err"

// describeStartFailure enriches a readiness timeout with the tail of the
// server's error log. A restored data directory from a different major
// version, for instance, makes mariadbd exit immediately — without this,
// the drill would report a timeout when the engine said precisely what was
// wrong.
func describeStartFailure(ctx context.Context, c *core, perr *protoError) *protoError {
	if perr.Code != "engine_not_ready" {
		return perr
	}
	val, stdout, _, eperr := c.exec(ctx, execArgs{
		Argv: []string{"sh", "-c", `tail -n 5 ` + serverErrLog + ` 2>/dev/null | grep -iE "error|aborting" | tail -n 1`},
	})
	if eperr != nil || val.ExitCode != 0 || len(stdout) == 0 {
		return perr
	}
	return protoErr("restore_failed", false,
		"restored server failed to start: %s", firstLine(stdout))
}

// execChecked wraps core.exec returning the value and raw stderr.
func execChecked(ctx context.Context, c *core, argv ...string) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}
