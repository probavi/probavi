package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// backuptime.go dates a mariadb-backup backup from the backup itself.
//
// mariadb-backup keeps its XtraBackup ancestry's metadata file names: it
// writes xtrabackup_info beside the data files (measured on 10.11), and it records
// when the backup started and finished. What it does not record is an
// offset: the values are the wall clock of the host that ran the backup
// (measured — a backup taken at 15:50:23 UTC on a host in Asia/Tokyo is
// written as `end_time = 2026-08-10 00:50:25`). So this kind reads the
// same declaration the logical kinds read, and reports nothing without it.
//
// The pgbackrest kind in the postgres adapter is the exception in this
// project: its manifest records epoch seconds, which need no zone at all.

const (
	// backupInfoName is the metadata file every mariadb-backup backup
	// carries since 11.0; xtrabackupInfoName is the name its 10.x
	// releases inherited from XtraBackup (both measured). The adapter
	// already refuses a directory without a checkpoints file, so one of
	// these sitting beside it is the norm.
	backupInfoName     = "mariadb_backup_info"
	xtrabackupInfoName = "xtrabackup_info"
	// endTimeKey is the completion time, which is what dates the backup:
	// that is the point at which the artifact exists and is consistent.
	endTimeKey        = "end_time"
	backupClockLayout = "2006-01-02 15:04:05"
	// xtrabackupInfoMaxBytes bounds the read: this is a short metadata
	// file, not data.
	xtrabackupInfoMaxBytes = 1 << 20
)

// backupCreatedAt reads the backup's completion time and places it in the
// operator-declared zone. It returns nil whenever the answer would be a
// guess: no zone declared, no metadata file, or no readable end_time.
func backupCreatedAt(dir string, loc *time.Location) *string {
	if loc == nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, backupInfoName))
	if err != nil {
		raw, err = os.ReadFile(filepath.Join(dir, xtrabackupInfoName))
	}
	if err != nil || len(raw) > xtrabackupInfoMaxBytes {
		return nil
	}
	clock, ok := backupInfoValue(string(raw), endTimeKey)
	if !ok {
		return nil
	}
	t, err := time.ParseInLocation(backupClockLayout, clock, loc)
	if err != nil {
		return nil
	}
	return formatCreatedAt(t)
}

// backupInfoValue reads one `key = value` line from xtrabackup_info. An
// empty value is not a value: XtraBackup writes `name = ` with nothing
// after it, so the same shape appears for fields it simply did not fill.
func backupInfoValue(info, key string) (string, bool) {
	for _, line := range strings.Split(info, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			return value, true
		}
	}
	return "", false
}
