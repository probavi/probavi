package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// barman.go restores a Barman backup, with point-in-time recovery.
//
// Barman's own restore is server-side: `barman recover` runs where the
// catalogue lives and ships the files to the target over rsync or ssh.
// That shape does not fit a drill, which has the artifact and a sandbox
// and no Barman server. What does fit is the catalogue's layout, measured
// against Barman 3.20.0 and PostgreSQL 16:
//
//	<server>/meta/<id>-backup.info   key=value: begin_wal, end_wal,
//	                                 timeline, end_time, status, parent
//	<server>/base/<id>/data/         the cluster itself, as pg_basebackup
//	                                 wrote it
//	<server>/wals/<16 hex>/<segment> plain segment files, uncompressed,
//	                                 named exactly as the segment
//
// So a restore is: place `data`, point `restore_command` at the WAL tree,
// and let PostgreSQL recover. **Measured: this needs nothing in the
// sandbox that the official postgres image does not already have** — the
// probe restored a real Barman backup in stock `postgres:16`, promoted it,
// and found both the 500 rows inside the base backup and the 100 written
// after it and recovered from archived WAL. That is the difference from
// the pgbackrest kind, which needs its tool in the image.
const (
	// barmanMetaDir holds one file per backup, whatever the backup's own
	// directory contains.
	barmanMetaDir = "meta"
	// barmanBaseDir holds the backups themselves, one directory each.
	barmanBaseDir = "base"
	// barmanWALDir holds the archived WAL, in directories named for the
	// first sixteen characters of the segments inside them.
	barmanWALDir = "wals"
	// barmanDataDir is the cluster inside a backup's directory.
	barmanDataDir = "data"
	// barmanDone is the only status a backup can be restored from.
	// WAITING_FOR_WALS means Barman has not yet received the WAL that
	// makes the backup consistent, which is exactly what a drill must not
	// assume it will find.
	barmanDone = "DONE"
	// barmanInfoMaxBytes bounds a metadata read: these are manifests of a
	// kilobyte or so, and one that is enormous is not one to parse.
	barmanInfoMaxBytes = 1 << 20
)

// barmanTimeLayout is how a backup.info renders an instant:
// "2026-09-23 18:34:46.592775+00:00".
const barmanTimeLayout = "2006-01-02 15:04:05.999999-07:00"

// barmanIDPattern is a backup id as Barman mints it, and as this adapter
// will paste it into a shell script: digits and a T, nothing else.
var barmanIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`)

// barmanBackup is the part of a backup.info this adapter reads.
type barmanBackup struct {
	id       string
	parent   string
	beginWAL string
	endWAL   string
	status   string
	endTime  time.Time
}

// readBarmanBackups reads every backup the catalogue describes. A file
// that cannot be read or understood is skipped rather than guessed at.
func readBarmanBackups(dir string) []barmanBackup {
	entries, err := os.ReadDir(filepath.Join(dir, barmanMetaDir))
	if err != nil {
		return nil
	}
	var out []barmanBackup
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), "-backup.info")
		if !ok || !barmanIDPattern.MatchString(id) {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, barmanMetaDir, e.Name()))
		if err != nil || info.Size() > barmanInfoMaxBytes {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, barmanMetaDir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, parseBarmanInfo(id, string(raw)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// parseBarmanInfo reads the key=value manifest. Barman writes the literal
// "None" where a field is unset, which is not a value.
func parseBarmanInfo(id, raw string) barmanBackup {
	b := barmanBackup{id: id}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || value == "None" {
			continue
		}
		switch key {
		case "parent_backup_id":
			b.parent = value
		case "begin_wal":
			b.beginWAL = value
		case "end_wal":
			b.endWAL = value
		case "status":
			b.status = value
		case "end_time":
			if ts, err := time.Parse(barmanTimeLayout, value); err == nil {
				b.endTime = ts.UTC()
			}
		}
	}
	return b
}

// barmanCreatedAt dates the catalogue from its newest restorable backup,
// the way repotime.go dates a pgBackRest repository: a backup.info records
// an instant with an explicit offset, so no declared zone is needed.
func barmanCreatedAt(dir string) *string {
	var newest time.Time
	for _, b := range readBarmanBackups(dir) {
		if b.status != barmanDone || b.endTime.IsZero() {
			continue
		}
		if b.endTime.After(newest) {
			newest = b.endTime
		}
	}
	if newest.IsZero() {
		return nil
	}
	return formatCreatedAt(newest)
}

// selectBarmanBackup picks the backup a restore will build on: the newest
// that finished, and with a target, the newest that finished before it,
// because recovery rolls forward and never back.
func selectBarmanBackup(backups []barmanBackup, target time.Time) (barmanBackup, bool, *protoError) {
	var chosen barmanBackup
	var found bool
	var oldest time.Time
	for _, b := range backups {
		if b.status != barmanDone || b.endTime.IsZero() {
			continue
		}
		if oldest.IsZero() || b.endTime.Before(oldest) {
			oldest = b.endTime
		}
		if !target.IsZero() && b.endTime.After(target) {
			continue
		}
		if !found || b.endTime.After(chosen.endTime) {
			chosen, found = b, true
		}
	}
	switch {
	case found:
		return chosen, true, nil
	case oldest.IsZero():
		return barmanBackup{}, false, protoErr("source_not_found", false,
			"the Barman catalogue holds no backup in status %s: a backup still waiting for its WAL "+
				"is not one a drill can prove", barmanDone)
	case target.IsZero():
		return barmanBackup{}, false, nil
	default:
		return barmanBackup{}, false, protoErr("source_not_found", false,
			"no Barman backup finished before the requested point in time: the target is %s and the "+
				"oldest completed backup finished at %s, and recovery only rolls forward",
			target.Format(time.RFC3339), oldest.Format(time.RFC3339))
	}
}

// checkBarmanChain refuses a catalogue that cannot serve the restore, the
// way chain.go does for pgBackRest and for the same reasons: the ancestor
// a backup rests on, and the WAL endpoints that carry it to a consistent
// state. Only the chosen backup's own range, and only its endpoints — a
// catalogue records no WAL segment size either.
func checkBarmanChain(dir string, chosen barmanBackup, backups []barmanBackup) *protoError {
	byID := make(map[string]barmanBackup, len(backups))
	for _, b := range backups {
		byID[b.id] = b
	}
	seen := make(map[string]bool, len(backups))
	for cur := chosen; cur.parent != ""; {
		if seen[cur.id] {
			return nil
		}
		seen[cur.id] = true
		parent, ok := byID[cur.parent]
		if !ok {
			return protoErr("source_not_found", false,
				"the backup chain is broken: %s builds on %s, which the catalogue no longer describes",
				cur.id, cur.parent)
		}
		if _, err := os.Stat(filepath.Join(dir, barmanBaseDir, parent.id, barmanDataDir)); err != nil {
			return protoErr("source_not_found", false,
				"the backup chain is broken: %s builds on %s, which the catalogue describes but the "+
					"repository does not hold", cur.id, parent.id)
		}
		cur = parent
	}

	wals := filepath.Join(dir, barmanWALDir)
	if entries, err := os.ReadDir(wals); err != nil || len(entries) == 0 {
		// No archive to judge: say nothing rather than guess.
		return nil
	}
	for _, segment := range []string{chosen.beginWAL, chosen.endWAL} {
		if !validWALName(segment) {
			continue
		}
		if _, err := os.Stat(filepath.Join(wals, segment[:walPrefixLen], segment)); err == nil {
			continue
		}
		end := "last segment"
		if segment == chosen.beginWAL {
			end = "first segment"
		}
		return protoErr("source_not_found", false,
			"the WAL archive is missing %s, which backup %s names as its own %s: the restore would "+
				"reach no consistent state", segment, chosen.id, end)
	}
	return nil
}

// resolveBarman identifies a Barman server directory as a source.
func resolveBarman(dir string) (*resolvedSource, *protoError) {
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return nil, protoErr("source_not_found", false, "Barman server directory does not exist: %s", dir)
	case err != nil:
		return nil, protoErr("source_unreadable", false, "stat Barman server directory: %v", err)
	case !info.IsDir():
		return nil, protoErr("invalid_request", false,
			"source path %s is a file; the barman kind expects a Barman server directory "+
				"(the one holding base/, wals/ and meta/)", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, barmanBaseDir)); err != nil {
		return nil, protoErr("invalid_request", false,
			"source path %s holds no %s directory; the barman kind expects a Barman server "+
				"directory, not the Barman home above it", dir, barmanBaseDir)
	}
	checksum, size, perr := dirChecksum(dir)
	if perr != nil {
		return nil, perr
	}
	return &resolvedSource{
		path:      dir,
		checksum:  checksum,
		sizeBytes: size,
		createdAt: barmanCreatedAt(dir),
	}, nil
}

// provisionBarman runs the Barman provision flow and returns the §6.2
// response payload. Like the pgbackrest kind it replaces the data
// directory, so the sandbox must start idle.
func provisionBarman(ctx context.Context, c *core, req *provisionRequest, src *resolvedSource, user, database string, logger *slog.Logger) (any, *protoError) {
	_, pitrAt, perr := resolvePITRTarget(req)
	if perr != nil {
		return nil, perr
	}

	backups := readBarmanBackups(src.path)
	if len(backups) == 0 {
		return nil, protoErr("source_corrupt", false,
			"the Barman server directory describes no backup: %s holds no readable %s",
			src.path, barmanMetaDir)
	}
	chosen, ok, perr := selectBarmanBackup(backups, pitrAt)
	if perr != nil {
		return nil, perr
	}
	if !ok {
		return nil, protoErr("source_not_found", false,
			"the Barman catalogue holds no backup this drill can restore")
	}
	// Host-side, before a byte moves, for the reasons chain.go states.
	if perr := checkBarmanChain(src.path, chosen, backups); perr != nil {
		return nil, perr
	}
	logger.Info("selected the Barman backup", "backup_id", chosen.id, "timeline_wal", chosen.endWAL)

	if perr := checkEngineStopped(ctx, c, "a Barman restore"); perr != nil {
		return nil, perr
	}

	scratch := req.Sandbox.ScratchDir
	if scratch == "" {
		scratch = "/tmp"
	}
	catalogue := scratch + "/probavi-barman"

	put, perr := c.putFile(ctx, putFileArgs{SourcePath: src.path, DestPath: catalogue, Mode: "0755"})
	if perr != nil {
		return nil, perr
	}

	pgdata, perr := resolvePGData(ctx, c)
	if perr != nil {
		return nil, perr
	}
	logger.Info("resolved the sandbox data directory", "pgdata", pgdata)

	restore, stderr, perr := execChecked(ctx, c, "sh", "-c",
		barmanRestoreScript(catalogue, chosen.id, pgdata, req.PITR != nil, pitrAt))
	if perr != nil {
		return nil, perr
	}
	if restore.ExitCode != 0 {
		return nil, protoErr("restore_failed", false, "place the Barman backup: %s", firstLine(stderr))
	}
	logger.Info("Barman backup placed", "seconds", restore.DurationSeconds)

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
			"database": database, "user": user, "mode": "barman", "backup_id": chosen.id,
		},
	}, nil
}

// barmanRestoreScript places the cluster and writes the recovery settings.
//
// Three of its lines are not obvious. The WAL directory is derived inside
// restore_command rather than listed, because Barman files a segment under
// the first sixteen characters of its own name; `cut` is used instead of a
// printf width because PostgreSQL rejects a restore_command containing any
// % escape it does not know. archive_mode is forced off: the restored
// configuration is the source host's, whose archive_command addresses a
// Barman server this sandbox has no business reaching, and a drill must
// not run the engine's own policies against the artifact it is proving.
// And the data directory is emptied with `find -delete` rather than
// `rm -rf` on itself, because the sandbox may have it mounted.
func barmanRestoreScript(catalogue, backupID, pgdata string, pitr bool, target time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, `set -e
mkdir -p %[1]s
find %[1]s -mindepth 1 -delete 2>/dev/null || true
cp -a %[2]s/%[3]s/%[4]s/. %[1]s/
rm -f %[1]s/postmaster.pid
cat >> %[1]s/postgresql.auto.conf <<'PROBAVI_EOF'
restore_command = 'cp %[2]s/%[5]s/$(echo %%f | cut -c1-16)/%%f %%p'
archive_mode = off
recovery_target_action = 'promote'
`, pgdata, catalogue, barmanBaseDir+"/"+backupID, barmanDataDir, barmanWALDir)
	if pitr {
		fmt.Fprintf(&b, "recovery_target_time = '%s'\n", target.Format(pgbackrestTimeFormat))
	}
	fmt.Fprintf(&b, `PROBAVI_EOF
touch %[1]s/recovery.signal
chown -R postgres:postgres %[1]s
chmod 0700 %[1]s`, pgdata)
	return b.String()
}
