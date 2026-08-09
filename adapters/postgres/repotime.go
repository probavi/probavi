package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// repotime.go dates a pgBackRest repository from the repository itself.
//
// This is the one backup format this adapter reads that records an
// absolute instant rather than a wall clock: backup.info stores
// backup-timestamp-start and backup-timestamp-stop as epoch seconds
// (measured — a backup taken on a host in Asia/Tokyo recorded 1786289869,
// which is 15:37:49 UTC, and pgbackrest's own info output renders it as
// 00:37:49+09). Epoch seconds carry no zone question at all, so unlike
// every other kind here, a pgbackrest source needs no
// source.params.backup_timezone to report an exact creation time — the
// declaration is simply unnecessary, not ignored.

const (
	// backupInfoName is the repository's backup manifest, at
	// <repo>/backup/<stanza>/backup.info.
	backupInfoName = "backup.info"
	// currentBackupSection lists the backups the repository still holds;
	// expired ones move out of it, so it is the set a restore can use.
	currentBackupSection = "[backup:current]"
	// backupInfoMaxBytes bounds the read: the file is a manifest, not data,
	// and a repository that hands back something enormous is not one this
	// parser should try to make sense of.
	backupInfoMaxBytes = 8 << 20
)

// backupInfoEntry is the part of one backup's manifest line this adapter
// reads. pgBackRest writes many more fields; naming only these two keeps
// the parser indifferent to the rest.
type backupInfoEntry struct {
	Start int64 `json:"backup-timestamp-start"`
	Stop  int64 `json:"backup-timestamp-stop"`
}

// repoCreatedAt returns when the repository's newest backup finished, or
// nil when that cannot be read: a repository with no current backup, an
// encrypted manifest (repo1-cipher-type makes backup.info unreadable), or
// a layout this parser does not recognise. Nothing is guessed.
//
// The newest backup dates the repository because that is what a restore
// without a target uses, and because it answers the question the record
// exists to answer: how current is the backup this drill proved.
func repoCreatedAt(dir, stanza string) *string {
	raw, err := os.ReadFile(filepath.Join(dir, "backup", stanza, backupInfoName))
	if err != nil || len(raw) > backupInfoMaxBytes {
		return nil
	}
	newest, ok := newestBackupStop(string(raw))
	if !ok {
		return nil
	}
	return formatCreatedAt(time.Unix(newest, 0).UTC())
}

// newestBackupStop reads the completion instants from the manifest's
// current-backup section and returns the latest.
func newestBackupStop(manifest string) (int64, bool) {
	var newest int64
	found := false
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
		// Each line is LABEL={json}: the label is pgBackRest's own and the
		// value is the backup's manifest.
		_, payload, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		entry := backupInfoEntry{}
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue
		}
		stop := entry.Stop
		if stop == 0 {
			stop = entry.Start
		}
		if stop <= 0 || !plausibleEpoch(stop) {
			continue
		}
		if !found || stop > newest {
			newest, found = stop, true
		}
	}
	return newest, found
}

// plausibleEpoch rejects values no backup timestamp produces, so a field
// that happens to parse as a number cannot date a backup absurdly.
func plausibleEpoch(seconds int64) bool {
	const (
		year2000 = 946684800  // no pgBackRest repository predates this
		year2200 = 7258118400 // and none is written this far ahead
	)
	return seconds >= year2000 && seconds <= year2200
}
