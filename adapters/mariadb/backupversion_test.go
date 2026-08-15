package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInfoFileAs(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestBackupSeries(t *testing.T) {
	tests := []struct {
		name string
		file string
		info string
		want string
	}{
		{"11.x metadata name", backupInfoName, "server_version = 11.4.7-MariaDB\n", "11.4"},
		{"10.x inherited name", xtrabackupInfoName, "server_version = 10.11.8-MariaDB-1:10.11.8+maria~ubu2204-log\n", "10.11"},
		{"12.x series", backupInfoName, "server_version = 12.3.1-MariaDB\n", "12.3"},
		{"empty value is not a value", backupInfoName, "server_version = \n", ""},
		{"no server_version line", backupInfoName, "tool_name = mariadb-backup\n", ""},
		{"non-version value", backupInfoName, "server_version = unknown\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeInfoFileAs(t, dir, tt.file, tt.info)
			if got := backupSeries(dir); got != tt.want {
				t.Errorf("backupSeries = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBackupSeriesPrefersTheCurrentName pins the resolution order: when a
// backup carries both metadata names, the 11.0+ name wins — it is the one
// the tool that wrote the backup maintains.
func TestBackupSeriesPrefersTheCurrentName(t *testing.T) {
	dir := t.TempDir()
	writeInfoFileAs(t, dir, backupInfoName, "server_version = 11.4.7-MariaDB\n")
	writeInfoFileAs(t, dir, xtrabackupInfoName, "server_version = 10.11.8-MariaDB\n")
	if got := backupSeries(dir); got != "11.4" {
		t.Errorf("backupSeries = %q, want the mariadb_backup_info answer", got)
	}
}

func TestBackupSeriesMissingOrOversized(t *testing.T) {
	if got := backupSeries(t.TempDir()); got != "" {
		t.Errorf("missing file: %q, want empty", got)
	}
	dir := t.TempDir()
	writeInfoFileAs(t, dir, backupInfoName, "server_version = 11.4.7-MariaDB\n"+strings.Repeat("#", xtrabackupInfoMaxBytes))
	if got := backupSeries(dir); got != "" {
		t.Errorf("oversized file: %q, want empty — bounded read, not data", got)
	}
}
