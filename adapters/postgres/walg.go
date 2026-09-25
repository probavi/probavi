package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// walg.go restores a wal-g repository, and replays WAL to a point in time.
//
// This is the other half of the Phase 2 promise the `pgbackrest` kind
// delivered alone: point-in-time recovery from a WAL archive, through the
// other tool operators actually run. `barman` came first because its
// layout needs nothing the official postgres image does not ship; wal-g
// needs its own binary, which is why it waited and why the verified image
// pins one (README).
//
// # Only the filesystem backend, and why that is not a limitation here
//
// wal-g's home is object storage. A drill's sandbox starts with no network
// at all — that is the isolation default, not a setting — so the backends
// that reach S3, GCS or Azure cannot serve one, and pretending otherwise
// would mean opening the sandbox to the internet to prove a backup.
// WALG_FILE_PREFIX, the filesystem backend, is the one that works on the
// terms a drill already holds: the operator's archive is staged on the
// drill host, the adapter moves it into the sandbox, and nothing reaches
// out. Backups that live in a bucket wait on the object-storage question
// the ROADMAP keeps open, which is a core decision rather than this
// adapter's.
//
// # The layout, measured
//
// Against wal-g v3.0.9 and PostgreSQL 16:
//
//	<prefix>/basebackups_005/base_<name>/                        the backup
//	<prefix>/basebackups_005/base_<name>_backup_stop_sentinel.json
//	<prefix>/wal_005/<segment>.lz4                               the archive
//
// The sentinel is the catalogue: it carries FinishTime, which orders the
// backups and answers a point-in-time target, and PgVersion, which is
// server_version_num and is what makes the major-version pre-check
// possible before a byte moves.
const (
	walgBaseDir         = "basebackups_005"
	walgWALDir          = "wal_005"
	walgSentinelSuffix  = "_backup_stop_sentinel.json"
	walgBackupPrefix    = "base_"
	walgFilePrefixEnv   = "WALG_FILE_PREFIX"
	walgRestoreFailHint = "wal-g backup-fetch failed"
)

// walgBackup is one base backup, as its own sentinel describes it.
type walgBackup struct {
	name       string // base_000000010000000000000002
	finishTime time.Time
	pgVersion  int // server_version_num, 0 when the sentinel does not say
}

// walgSentinel is the subset of the sentinel this adapter reads. The file
// carries more — sizes, LSNs, the system identifier, the hostname of the
// machine that took it — and none of it changes which backup a drill
// restores, so none of it is read.
type walgSentinel struct {
	FinishTime string `json:"FinishTime"`
	PgVersion  int    `json:"PgVersion"`
}

// readWalgBackups reads every sentinel in the repository. A sentinel that
// cannot be parsed is passed over rather than refused: wal-g writes it
// last, so a half-written one is a backup still in progress, and the
// catalogue is allowed to contain one. What is refused is a *choice* with
// nothing behind it, which the caller reports in its own words.
func readWalgBackups(dir string) []walgBackup {
	entries, err := os.ReadDir(filepath.Join(dir, walgBaseDir))
	if err != nil {
		return nil
	}
	backups := make([]walgBackup, 0, len(entries))
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), walgSentinelSuffix)
		if !ok || !strings.HasPrefix(name, walgBackupPrefix) {
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, walgBaseDir, e.Name())))
		if err != nil {
			continue
		}
		s := walgSentinel{}
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		finish, err := time.Parse(time.RFC3339Nano, s.FinishTime)
		if err != nil {
			continue
		}
		backups = append(backups, walgBackup{name: name, finishTime: finish.UTC(), pgVersion: s.PgVersion})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].finishTime.Before(backups[j].finishTime) })
	return backups
}

// walgSeries turns server_version_num into the series checkEngineVersion
// compares: 160015 is PostgreSQL 16, and 90624 is 9.6, because the
// numbering changed shape at 10 and a backup taken before it still has to
// be refused against the right sandbox.
func walgSeries(num int) string {
	switch {
	case num <= 0:
		return ""
	case num >= 100000:
		return fmt.Sprintf("%d", num/10000)
	default:
		return fmt.Sprintf("%d.%d", num/10000, (num/100)%100)
	}
}

// selectWalgBackup picks the base backup a restore starts from: the newest
// one that finished at or before the target, because recovery only rolls
// forward. With no target it is simply the newest, which is what
// `wal-g backup-fetch LATEST` would have chosen — the name is resolved
// here anyway so the record can say which backup it proved rather than a
// word that means something different tomorrow.
func selectWalgBackup(backups []walgBackup, target time.Time) (walgBackup, *protoError) {
	var chosen walgBackup
	var found bool
	for _, b := range backups {
		if !target.IsZero() && b.finishTime.After(target) {
			continue
		}
		if !found || b.finishTime.After(chosen.finishTime) {
			chosen, found = b, true
		}
	}
	if found {
		return chosen, nil
	}
	if len(backups) == 0 {
		return walgBackup{}, protoErr("source_corrupt", false,
			"the wal-g repository describes no backup: %s holds no readable sentinel", walgBaseDir)
	}
	return walgBackup{}, protoErr("source_not_found", false,
		"no wal-g backup finished before the requested point in time: the target is %s and the "+
			"oldest backup finished at %s, and recovery only rolls forward",
		target.Format(time.RFC3339), backups[0].finishTime.Format(time.RFC3339))
}

// walgCreatedAt dates the source by the newest backup the repository
// holds, which is the one a drill without a target restores.
func walgCreatedAt(dir string) *string {
	backups := readWalgBackups(dir)
	if len(backups) == 0 {
		return nil
	}
	newest := backups[len(backups)-1].finishTime.UTC().Format("2006-01-02T15:04:05.000Z")
	return &newest
}

// resolveWalg vets the repository shape before anything moves. Both
// directories have to be there: a prefix with backups and no WAL archive
// restores to the instant of a base backup and cannot recover past it,
// which is not the kind this declares.
func resolveWalg(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "wal-g repository does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat wal-g repository: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the walg kind expects the directory WALG_FILE_PREFIX names "+
				"(the one holding %s and %s)", dir, walgBaseDir, walgWALDir)
	}
	for _, want := range []string{walgBaseDir, walgWALDir} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			return nil, protoErr("invalid_request", false,
				"source path %s holds no %s directory; the walg kind expects the prefix "+
					"WALG_FILE_PREFIX names, not a directory above or inside it", dir, want)
		}
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path:      dir,
		checksum:  checksum,
		sizeBytes: size,
		createdAt: walgCreatedAt(dir),
	}, nil
}

// provisionWalg runs the wal-g flow: choose the backup host-side, refuse
// the pairings that cannot work before a byte moves, fetch, then let
// PostgreSQL replay the archive.
func provisionWalg(ctx context.Context, c *core, req *provisionRequest, src *resolvedSource,
	user, database string, logger *slog.Logger) (any, *protoError) {
	_, pitrAt, perr := resolvePITRTarget(req)
	if perr != nil {
		return nil, perr
	}
	chosen, perr := selectWalgBackup(readWalgBackups(src.path), pitrAt)
	if perr != nil {
		return nil, perr
	}
	logger.Info("selected the wal-g backup", "backup", chosen.name,
		"finished", chosen.finishTime.Format(time.RFC3339))

	if perr := checkEngineStopped(ctx, c, "a wal-g restore"); perr != nil {
		return nil, perr
	}
	// The sentinel states the server that took it, so the pairing a
	// physical restore can never survive is refused here rather than
	// discovered by a server that will not start (docs/engine-versions.md
	// §5).
	if perr := checkEngineVersion(ctx, c, walgSeries(chosen.pgVersion)); perr != nil {
		return nil, perr
	}
	if perr := checkWalgTool(ctx, c); perr != nil {
		return nil, perr
	}

	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	repo := scratch + "/probavi-walg-repo"

	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: repo, Mode: "0755"})
	if perr != nil {
		return nil, perr
	}

	pgdata, perr := resolvePGData(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("resolved the sandbox data directory", "pgdata", pgdata)

	restore, stderr, perr := execChecked(ctx, c, "sh", "-c",
		walgRestoreScript(repo, pgdata, chosen.name, req.PITR != nil, pitrAt))
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, protoErr("restore_failed", false, "%s: %s", walgRestoreFailHint, firstLine(stderr))
	}
	logger.Info("wal-g backup fetched", "seconds", restore.DurationSeconds)

	readySeconds, perr := startEngine(ctx, c, pgdata, user, database)
	if perr != nil {
		return nil, perr
	}
	logger.Info("engine recovered and ready", "seconds", readySeconds)

	return map[string]any{
		"connection": map[string]any{
			"scheme": "postgresql", "host": "127.0.0.1", "port": defaultPort,
			"database": database, "user": user,
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
			"database": database, "user": user, "mode": "walg", "backup_id": chosen.name,
		},
	}, nil
}

// checkWalgTool refuses an image without the binary by name. Unlike
// pgbackrest and Barman, wal-g has no distribution package: an image
// carries it because someone put it there, and "command not found" from
// inside a restore script is a worse way to learn that.
func checkWalgTool(ctx context.Context, c *core) *protoError {
	val, _, stderr, perr := c.exec(ctx, execArgs{Argv: []string{"wal-g", "--version"}})
	if perr != nil {
		return perr
	}
	if val.ExitCode != 0 {
		return protoErr("invalid_request", false,
			"the sandbox image lacks wal-g (%s): unlike pgbackrest and Barman it ships in no "+
				"distribution, so the image has to carry a pinned release binary — the adapter "+
				"README gives the Dockerfile", firstLine(stderr))
	}
	return nil
}

// walgRestoreScript fetches the base backup and arms recovery.
//
// The repository is handed to the postgres user first, the way the
// pgbackrest kind hands over its own. put_file lands a tree owned by the
// identity the sandbox execs as and sets a mode on the top directory
// only, so wal-g's own 0700 directories inside it stay closed to anyone
// else — and the process that needs them most is the one PostgreSQL
// spawns for restore_command, which runs as postgres. Measured: without
// this the base backup fetches (that runs as postgres through gosu, but
// reads a path root could still traverse) and then recovery cannot stat a
// single WAL segment, and the server exits with "could not locate
// required checkpoint record" — a message about the cluster, for a
// permission problem.
//
// The environment carries the repository rather than a config file,
// because that is wal-g's own interface and a file would be one more thing
// to keep in step. It appears twice on purpose: once for the fetch, and
// once inside restore_command, which PostgreSQL runs through its own
// shell and which therefore inherits nothing from this one.
//
// recovery_target_action = promote for the same reason the pgbackrest kind
// sets it: the default is pause, which would leave a drill waiting at the
// target point until its wall-clock deadline killed it.
func walgRestoreScript(repo, pgdata, backup string, pitr bool, target time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, `set -e
chown -R postgres:postgres %[3]s
mkdir -p %[1]s
find %[1]s -mindepth 1 -delete 2>/dev/null || true
chown postgres:postgres %[1]s
chmod 0700 %[1]s
%[2]s=%[3]s gosu postgres wal-g backup-fetch %[1]s %[4]s
cat >> %[1]s/postgresql.auto.conf <<'PROBAVI_EOF'
restore_command = '%[2]s=%[3]s wal-g wal-fetch %%f %%p'
archive_mode = off
recovery_target_action = 'promote'
`, pgdata, walgFilePrefixEnv, repo, backup)
	if pitr {
		fmt.Fprintf(&b, "recovery_target_time = '%s'\n", target.Format(pgbackrestTimeFormat))
	}
	fmt.Fprintf(&b, `PROBAVI_EOF
touch %[1]s/recovery.signal
chown -R postgres:postgres %[1]s
chmod 0700 %[1]s`, pgdata)
	return b.String()
}
