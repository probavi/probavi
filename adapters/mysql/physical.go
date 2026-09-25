package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// Physical restore (xtrabackup source kind): an XtraBackup restore replaces
// the data directory, so the engine must NOT be running. The drill config
// must start the sandbox idle (docker provider: `command: sleep infinity`)
// with an image that contains mysqld, xtrabackup, and gosu. The adapter
// then owns the whole lifecycle: transfer backup → prepare + copy-back →
// open sandbox-local auth → start the server → wait for readiness.
const (
	datadir      = "/var/lib/mysql"
	initFilePath = "/tmp/probavi-init.sql"

	// physicalDatabase is the connection database for physical restores:
	// the system schema is the only database guaranteed to exist in an
	// arbitrary restored server. Checks against restored data should use
	// schema-qualified table names.
	physicalDatabase = "mysql"
)

// physicalRestoreScript prepares the backup in place and copies it back.
//
// $1 is the backup directory inside the sandbox and $2 the engine's data
// directory. They are positional parameters rather than text folded into
// the script because $1 comes from the sandbox's scratch directory, which
// the bare-host provider derives from the operator's workspace_root: a
// space there would break the restore and a shell metacharacter would be
// read as script. The sandbox providers pass paths this way for the same
// reason.
const physicalRestoreScript = `set -e
xtrabackup --prepare --target-dir="$1"
xtrabackup --copy-back --target-dir="$1" --datadir="$2"
chown -R mysql:mysql "$2"`

// provisionPhysical runs the xtrabackup provision flow and returns the
// §6.2 response payload. Options are ignored: the restored server is
// served as root with the connection database fixed to the system schema,
// mirroring the postgres adapter's physical mode.
func provisionPhysical(ctx context.Context, c *core, req *provisionRequest, src *resolvedSource, logger *slog.Logger) (any, *protoError) {
	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	backupInSandbox := scratch + "/probavi-xtrabackup"

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
	restore, stderr, perr := execChecked(ctx, c, "sh", "-c", physicalRestoreScript, "sh", backupInSandbox, datadir)
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, protoErr("restore_failed", false, "xtrabackup restore failed: %s", firstLine(stderr))
	}
	logger.Info("xtrabackup restore complete", "seconds", restore.DurationSeconds)

	readySeconds, perr := startEngine(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine recovered and ready", "seconds", readySeconds)

	replaySeconds, perr := replayBinlogs(ctx, c, req, src, backupInSandbox, logger)
	if perr != nil {
		return nil, perr
	}

	return map[string]any{
		"connection": map[string]any{
			"scheme": "mysql", "host": "127.0.0.1", "port": defaultPort,
			"database": physicalDatabase, "user": defaultUser,
		},
		"source_identity": map[string]any{
			"checksum": src.checksum, "size_bytes": src.sizeBytes, "created_at": src.createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": readySeconds,
			"transfer_seconds":     put.DurationSeconds,
			// The replay is recovery, not discovery: every second of it is
			// time an operator would spend before the database is usable,
			// so it counts toward restore_seconds and the RTO trend rather
			// than disappearing into a phase of its own.
			"restore_seconds": restore.DurationSeconds + replaySeconds,
		},
		"state": map[string]any{
			"database": physicalDatabase, "user": defaultUser, "mode": mode(src),
		},
	}, nil
}

// mode is what the record's state says this restore was. The binlog replay
// is a different proof from a full-only restore — it reaches a later
// instant — so it says so rather than sharing one word with the kind that
// stops at the backup.
func mode(src *resolvedSource) string {
	if src.binlogsPath != "" {
		return "physical+binlog"
	}
	return "physical"
}

// replayBinlogs carries the restored server forward from the instant the
// full backup ended to the target the drill asked for, or to the end of
// the archive when it asked for none (binlog.go). Returns the measured
// seconds; zero for a kind that has no logs.
//
// The order matters and is not arbitrary: the logs go in *after* the
// server is serving, because mysqlbinlog produces SQL and a server has to
// be up to accept it. That is the same order an operator follows, which is
// the point — a drill that proved a recovery no run-book describes would
// prove the wrong thing.
func replayBinlogs(ctx context.Context, c *core, req *provisionRequest, src *resolvedSource,
	backupInSandbox string, logger *slog.Logger) (float64, *protoError) {
	if src.binlogsPath == "" {
		return 0, nil
	}
	start, perr := readBinlogStart(src.path)
	if perr != nil {
		return 0, perr
	}
	files, perr := binlogFilesFrom(src.binlogsPath, start)
	if perr != nil {
		return 0, perr
	}
	stop, perr := binlogStopDatetime(req)
	if perr != nil {
		return 0, perr
	}

	// The logs travel as their own directory beside the backup, so the
	// replay reads them where they land rather than from the host.
	logsInSandbox := backupInSandbox + "-binlogs"
	if _, perr := c.putFile(ctx, putFileArgs{
		SourcePath: src.binlogsPath, DestPath: logsInSandbox, Mode: "0755",
	}); perr != nil {
		return 0, perr
	}

	replay, stderr, perr := execChecked(ctx, c,
		binlogReplayArgv(logsInSandbox, start, stop, files)...)
	if perr != nil {
		return 0, perr
	}
	if replay.ExitCode != 0 {
		return 0, protoErr("restore_failed", false,
			"binary log replay failed (%s): %s", binlogSummary(files, stop), firstLine(stderr))
	}
	logger.Info("binary logs replayed", "from", start.file, "position", start.position,
		"chain", binlogSummary(files, stop), "seconds", replay.DurationSeconds)
	return replay.DurationSeconds, nil
}

// checkIdleSandbox verifies the preconditions of a physical restore: no
// engine running, xtrabackup present.
func checkIdleSandbox(ctx context.Context, c *core) *protoError {
	running, _, perr := execChecked(ctx, c,
		"mysql", "-h", "127.0.0.1", "-u", defaultUser, "-N", "-B", "-e", "SELECT 1")
	if perr != nil {
		return perr
	}
	if running.ExitCode == 0 {
		return protoErr("invalid_request", false,
			"xtrabackup restore needs an idle sandbox: set sandbox params command to keep the engine stopped (docker: command: sleep infinity)")
	}
	version, stderr, perr := execChecked(ctx, c, "xtrabackup", "--version")
	if perr != nil {
		return perr
	}
	if version.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"sandbox image lacks xtrabackup (%s): use an image with mysqld, xtrabackup, and gosu", firstLine(stderr))
	}
	return nil
}

// engineSeriesPattern finds the release series in `mysqld --version`
// output ("/usr/sbin/mysqld  Ver 8.4.5 for Linux on x86_64 (MySQL
// Community Server - GPL)"). The binary path carries no digits, so the
// first match is the version.
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
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: []string{"mysqld", "--version"}})
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
		"the XtraBackup backup was taken from server release series %s, but the sandbox engine is %s: a physical backup restores only into its own release series — use a sandbox image running a %s server",
		series, engine, series)
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
func startEngine(ctx context.Context, c *core) (float64, *protoError) {
	script := fmt.Sprintf(`set -e
mkdir -p /var/run/mysqld && chown mysql:mysql /var/run/mysqld
gosu mysql mysqld --daemonize %s --init-file=%s --pid-file=/tmp/probavi-mysqld.pid --log-error=/tmp/probavi-mysqld.err || { tail -n 20 /tmp/probavi-mysqld.err >&2; exit 1; }`,
		eventSchedulerFlag, initFilePath)
	start, stderr, perr := execChecked(ctx, c, "sh", "-c", script)
	if perr != nil {
		return 0, perr
	}
	if start.ExitCode != 0 {
		return 0, protoErr("restore_failed", false, "restored server failed to start: %s", firstLine(stderr))
	}
	readySeconds, perr := awaitEngine(ctx, c, defaultUser)
	if perr != nil {
		return 0, perr
	}
	// The flag above is the pin; this is the server confirming it, which
	// is what the drill is entitled to rest on (retention.go).
	pinSeconds, perr := pinEventScheduler(ctx, c, defaultUser)
	if perr != nil {
		return 0, perr
	}
	readySeconds += pinSeconds
	return start.DurationSeconds + readySeconds, nil
}

// execChecked wraps core.exec returning the value and raw stderr.
func execChecked(ctx context.Context, c *core, argv ...string) (*execValue, []byte, *protoError) {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: argv})
	if perr != nil {
		return nil, nil, perr
	}
	return val, stderr, nil
}
