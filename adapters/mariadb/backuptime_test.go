package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// realXtraBackupInfo is what XtraBackup 8.0.35 wrote beside a backup taken
// on a host in Asia/Tokyo at 15:50:23 UTC. The times are bare wall clock:
// nothing in the file says the host was nine hours ahead.
const realXtraBackupInfo = `uuid = 0963b8d2-940a-11f1-acff-a586b1e1570e
name = 
tool_name = xtrabackup
tool_command = --backup --target-dir=/tmp/xb --user=root
tool_version = 8.0.35-36
ibbackup_version = 8.0.35-36
server_version = 8.0.46
start_time = 2026-08-10 00:50:23
end_time = 2026-08-10 00:50:25
lock_time = 0
binlog_pos = filename 'binlog.000003', position '157'
innodb_from_lsn = 0
innodb_to_lsn = 29367758
partial = N
incremental = N
format = file
compressed = N
encrypted = N
`

func writeBackupInfo(t *testing.T, info string) string {
	t.Helper()
	dir := t.TempDir()
	if info == "" {
		return dir
	}
	if err := os.WriteFile(filepath.Join(dir, xtrabackupInfoName), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBackupInfoValue(t *testing.T) {
	tests := []struct {
		name, key, want string
		found           bool
	}{
		{"the completion time", endTimeKey, "2026-08-10 00:50:25", true},
		{"a value containing its own separator", "binlog_pos", "filename 'binlog.000003', position '157'", true},
		// XtraBackup writes `name = ` with nothing after it; an empty
		// value is an absence, not a value.
		{"an empty value", "name", "", false},
		{"a key that is not there", "backup_timezone", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := backupInfoValue(realXtraBackupInfo, tt.key)
			if found != tt.found || got != tt.want {
				t.Errorf("backupInfoValue(%s) = %q, %v; want %q, %v", tt.key, got, found, tt.want, tt.found)
			}
		})
	}
}

func TestBackupCreatedAt(t *testing.T) {
	dir := writeBackupInfo(t, realXtraBackupInfo)

	t.Run("no zone declared means no claim", func(t *testing.T) {
		if got := backupCreatedAt(dir, nil); got != nil {
			t.Errorf("createdAt = %v, want nil — the instant is not derivable without the zone", *got)
		}
	})
	t.Run("the declared zone makes it an instant", func(t *testing.T) {
		got := backupCreatedAt(dir, mustLoad(t, "Asia/Tokyo"))
		if got == nil || *got != "2026-08-10T00:50:25.000+09:00" {
			t.Fatalf("createdAt = %v, want the completion wall clock with Tokyo's offset", got)
		}
		parsed, err := time.Parse(time.RFC3339, *got)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if utc := parsed.UTC().Format("2006-01-02T15:04:05Z"); utc != "2026-08-09T15:50:25Z" {
			t.Errorf("in UTC = %s, want 2026-08-09T15:50:25Z — the instant the backup finished", utc)
		}
	})
	t.Run("the completion time dates it, not the start", func(t *testing.T) {
		got := backupCreatedAt(dir, mustLoad(t, "UTC"))
		if got == nil || *got != "2026-08-10T00:50:25.000Z" {
			t.Errorf("createdAt = %v, want end_time (…:25), not start_time (…:23)", got)
		}
	})
}

func TestBackupCreatedAtRefusals(t *testing.T) {
	tokyo := mustLoad(t, "Asia/Tokyo")
	tests := []struct{ name, info string }{
		{"no metadata file", ""},
		{"metadata without a completion time", "uuid = x\ntool_name = xtrabackup\n"},
		{"an empty completion time", "end_time = \n"},
		{"a completion time that is not one", "end_time = shortly after lunch\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backupCreatedAt(writeBackupInfo(t, tt.info), tokyo); got != nil {
				t.Errorf("createdAt = %s, want none", *got)
			}
		})
	}
}
