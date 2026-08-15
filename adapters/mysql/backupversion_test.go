package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBackupInfoFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, xtrabackupInfoName), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestBackupSeries(t *testing.T) {
	tests := []struct {
		name string
		info string
		want string
	}{
		{"mysql community", "tool_name = xtrabackup\nserver_version = 8.4.5\n", "8.4"},
		{"percona server suffix", "server_version = 8.0.36-28\n", "8.0"},
		{"mariadb-origin backup still states its series", "server_version = 10.11.8-MariaDB\n", "10.11"},
		{"empty value is not a value", "server_version = \n", ""},
		{"no server_version line", "tool_name = xtrabackup\n", ""},
		{"non-version value", "server_version = unknown\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBackupInfoFile(t, dir, tt.info)
			if got := backupSeries(dir); got != tt.want {
				t.Errorf("backupSeries = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBackupSeriesMissingOrOversized(t *testing.T) {
	if got := backupSeries(t.TempDir()); got != "" {
		t.Errorf("missing file: %q, want empty", got)
	}
	dir := t.TempDir()
	writeBackupInfoFile(t, dir, "server_version = 8.4.5\n"+strings.Repeat("#", xtrabackupInfoMaxBytes))
	if got := backupSeries(dir); got != "" {
		t.Errorf("oversized file: %q, want empty — bounded read, not data", got)
	}
}
