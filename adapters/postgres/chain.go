package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// chain.go refuses a pgBackRest repository whose restore chain has a hole
// in it, host-side, before a byte is transferred.
//
// A physical restore is a chain: a differential or incremental backup is
// meaningless without the full it was taken against, and no backup reaches
// consistency without the WAL written while it ran. Both halves can be
// absent from a repository that looks complete, and measured against
// pgbackrest 2.59.1 and PostgreSQL 16 neither says so usefully:
//
//   - With the restored backup's stop segment deleted from the archive,
//     `pgbackrest restore` exits **0**. The failure appears later, when
//     the server will not start, and the log says only "startup process
//     exited with exit code 1". The adapter reports "restored cluster
//     failed to start", which names nothing an operator can act on.
//   - With the prior full backup's directory deleted, the restore fails
//     with `unable to open missing file .../base/5/1255.gz` — a relation
//     file, not "the backup this one builds on is missing".
//
// Neither passes a broken backup, which matters more than anything here:
// the drill fails either way. What this adds is failing **before** the
// transfer and naming what is absent, the way `adapters/mssql` already
// refuses a log-sequence gap.
//
// What it deliberately does not do is guess. Where the repository cannot
// be read — an encrypted manifest, a layout this parser does not
// recognise, an archive directory that is not there — the check says
// nothing and the restore proceeds exactly as before. A pre-check that
// declines to answer costs a late failure; one that answers wrongly
// refuses a good backup, which is the mistake issue #327 was.
const (
	// The two trees a pgBackRest repository keeps: manifests and backups
	// under one, archived WAL under the other.
	archiveDirName = "archive"
	backupDirName  = "backup"
	// walNameLen is the length of a WAL segment name: timeline, logical
	// id and segment number, eight hex digits each.
	walNameLen = 24
	// walPrefixLen is the part of that name pgBackRest uses as the
	// directory holding the segment — timeline and logical id.
	walPrefixLen = 16
)

// repoBackup is one entry of backup.info's [backup:current]. pgBackRest
// writes many more fields; naming only these keeps the parser indifferent
// to the rest, as repotime.go's does.
type repoBackup struct {
	label        string
	Prior        string `json:"backup-prior"`
	ArchiveStart string `json:"backup-archive-start"`
	ArchiveStop  string `json:"backup-archive-stop"`
	Start        int64  `json:"backup-timestamp-start"`
	Stop         int64  `json:"backup-timestamp-stop"`
}

// stoppedAt is the instant the backup finished, falling back to its start
// the way repotime.go does for a manifest that records only one.
func (b repoBackup) stoppedAt() int64 {
	if b.Stop > 0 {
		return b.Stop
	}
	return b.Start
}

// currentBackups reads the manifest's current-backup section. Expired
// backups have moved out of it, so this is the set a restore can use.
func currentBackups(manifest string) []repoBackup {
	var out []repoBackup
	inSection := false
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inSection = line == currentBackupSection
			continue
		}
		if !inSection || line == "" {
			continue
		}
		label, payload, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		entry := repoBackup{}
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue
		}
		entry.label = label
		out = append(out, entry)
	}
	return out
}

// readBackupInfo returns the current backups of a repository, or nothing
// at all when the manifest cannot be read — the same silence repotime.go
// keeps, and for the same reason.
func readBackupInfo(dir, stanza string) []repoBackup {
	raw, err := os.ReadFile(filepath.Join(dir, backupDirName, stanza, backupInfoName))
	if err != nil || len(raw) > backupInfoMaxBytes {
		return nil
	}
	return currentBackups(string(raw))
}

// checkRestoreChain refuses a repository that cannot serve the restore
// about to be attempted. target is the resolved point-in-time instant, or
// the zero time when the drill asked for none.
func checkRestoreChain(dir, stanza string, target time.Time) *protoError {
	backups := readBackupInfo(dir, stanza)
	if len(backups) == 0 {
		// Unreadable or empty: repoCreatedAt is silent here too, and a
		// repository with no current backup fails loudly in pgbackrest's
		// own words a moment later.
		return nil
	}

	chosen, ok, perr := selectBackup(backups, target)
	if perr != nil {
		return perr
	}
	if !ok {
		return nil
	}
	if perr := checkAncestors(dir, stanza, chosen, backups); perr != nil {
		return perr
	}
	return checkArchiveEndpoints(dir, stanza, chosen)
}

// selectBackup picks the backup a restore will build on. Without a target
// that is the newest; with one it is the newest that had finished by then,
// because recovery rolls forward and never back.
func selectBackup(backups []repoBackup, target time.Time) (repoBackup, bool, *protoError) {
	var chosen repoBackup
	var found bool
	var oldest int64
	for _, b := range backups {
		stop := b.stoppedAt()
		if stop <= 0 {
			continue
		}
		if oldest == 0 || stop < oldest {
			oldest = stop
		}
		if !target.IsZero() && stop > target.Unix() {
			continue
		}
		if !found || stop > chosen.stoppedAt() {
			chosen, found = b, true
		}
	}
	if found {
		return chosen, true, nil
	}
	if target.IsZero() || oldest == 0 {
		// Nothing readable to judge: stay silent rather than guess.
		return repoBackup{}, false, nil
	}
	return repoBackup{}, false, protoErr("source_not_found", false,
		"no backup in stanza finished before the requested point in time: the target is %s and the "+
			"oldest backup in the repository finished at %s, and recovery only rolls forward",
		target.UTC().Format(time.RFC3339), time.Unix(oldest, 0).UTC().Format(time.RFC3339))
}

// checkAncestors walks backup-prior to the full the chain rests on. A
// differential or incremental whose ancestor has left the repository
// cannot be restored, and pgbackrest reports that as a missing relation
// file deep inside the absent backup.
func checkAncestors(dir, stanza string, b repoBackup, backups []repoBackup) *protoError {
	byLabel := make(map[string]repoBackup, len(backups))
	for _, x := range backups {
		byLabel[x.label] = x
	}
	seen := make(map[string]bool, len(backups))
	for cur := b; cur.Prior != ""; {
		if seen[cur.label] {
			// A cycle is not a repository pgbackrest writes; refusing to
			// loop is cheaper than trusting it not to.
			return nil
		}
		seen[cur.label] = true
		prior, ok := byLabel[cur.Prior]
		if !ok {
			return protoErr("source_not_found", false,
				"the restore chain is broken: backup %s builds on %s, which the repository no longer lists",
				cur.label, cur.Prior)
		}
		if _, err := os.Stat(filepath.Join(dir, backupDirName, stanza, prior.label)); err != nil {
			return protoErr("source_not_found", false,
				"the restore chain is broken: backup %s builds on %s, which the manifest lists but the "+
					"repository does not hold", cur.label, prior.label)
		}
		cur = prior
	}
	return nil
}

// checkArchiveEndpoints requires the WAL a backup names as its own to be
// in the archive. Those two segments are what carries the cluster to a
// consistent state; without them the restore still exits 0 and the server
// refuses to start.
//
// Only the chosen backup's own range is checked, which was measured
// rather than assumed: deleting the *full's* archive segment left a
// differential restore working end to end, so requiring the whole chain's
// WAL would refuse repositories whose older segments have legitimately
// expired.
//
// The endpoints, not the range between them. A repository records no WAL
// segment size, and the number of segments in a logical file follows from
// it, so the names between two endpoints cannot be enumerated from the
// repository alone. An interior hole in a long range is therefore not
// detected here — it stays the late failure it is today.
func checkArchiveEndpoints(dir, stanza string, b repoBackup) *protoError {
	archive := filepath.Join(dir, archiveDirName, stanza)
	if !archiveIsReadable(archive) {
		return nil
	}
	for _, segment := range []string{b.ArchiveStart, b.ArchiveStop} {
		if !validWALName(segment) {
			continue
		}
		ok, err := archiveHolds(archive, segment)
		if err != nil || ok {
			continue
		}
		return protoErr("source_not_found", false,
			"the WAL archive is missing %s, which backup %s names as its own %s: the restore would "+
				"reach no consistent state", segment, b.label, endpointName(b, segment))
	}
	return nil
}

// endpointName says which end of the range is missing, because the two
// mean different things to someone reading the message.
func endpointName(b repoBackup, segment string) string {
	if segment == b.ArchiveStart {
		return "first segment"
	}
	return "last segment"
}

// archiveIsReadable reports whether the stanza's archive is present and
// holds at least one segment. An archive that is absent or shaped in a way
// this code has not measured is left alone rather than judged.
func archiveIsReadable(archive string) bool {
	matches, err := filepath.Glob(filepath.Join(archive, "*", "????????????????", "????????????????????????*"))
	return err == nil && len(matches) > 0
}

// archiveHolds looks for one segment. The stored name carries a checksum
// and, with compression, a suffix, so the segment name is a prefix rather
// than the whole filename; the directory above it is the segment's first
// sixteen characters, which is pgBackRest's own layout.
func archiveHolds(archive, segment string) (bool, error) {
	if len(segment) < walPrefixLen {
		return false, nil
	}
	matches, err := filepath.Glob(filepath.Join(archive, "*", segment[:walPrefixLen], segment+"*"))
	if err != nil {
		return false, err
	}
	for _, m := range matches {
		// A ".backup" label file sits beside the segment it marks and is
		// not the segment: it would answer this question wrongly.
		if !strings.HasSuffix(m, ".backup") {
			return true, nil
		}
	}
	return false, nil
}

// validWALName reports whether a string is a WAL segment name this code
// can look up. A manifest field that is empty, truncated, or not
// hexadecimal is left to pgbackrest rather than guessed at.
func validWALName(s string) bool {
	if len(s) != walNameLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'F', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
