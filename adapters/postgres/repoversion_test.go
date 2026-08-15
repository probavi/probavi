package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBackupInfo(t *testing.T, stanza, content string) string {
	t.Helper()
	repo := t.TempDir()
	dir := filepath.Join(repo, "backup", stanza)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupInfoName), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return repo
}

func TestRepoDBVersion(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{"modern single-component major", "[db]\ndb-catalog-version=202307071\ndb-version=\"16\"\n", "16"},
		{"two-component era", "[db]\ndb-version=\"9.6\"\n", "9.6"},
		{"history section is not the current cluster",
			"[db:history]\n1={\"db-version\":\"14\"}\n\n[db]\ndb-version=\"16\"\n", "16"},
		{"no db section", "[backup:current]\nx={\"backup-timestamp-stop\":1}\n", ""},
		{"version key only outside db section", "[cipher]\ndb-version=\"16\"\n", ""},
		{"non-version value is not a version", "[db]\ndb-version=\"whatever\"\n", ""},
		{"encrypted manifest reads as garbage", "\x00\x8f\x1bnot-ini-at-all", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := writeBackupInfo(t, "demo", tt.manifest)
			if got := repoDBVersion(repo, "demo"); got != tt.want {
				t.Errorf("repoDBVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepoDBVersionMissingOrOversized(t *testing.T) {
	if got := repoDBVersion(t.TempDir(), "demo"); got != "" {
		t.Errorf("missing manifest: %q, want empty", got)
	}
	huge := "[db]\ndb-version=\"16\"\n" + strings.Repeat("#", backupInfoMaxBytes)
	repo := writeBackupInfo(t, "demo", huge)
	if got := repoDBVersion(repo, "demo"); got != "" {
		t.Errorf("oversized manifest: %q, want empty — bounded read, not data", got)
	}
}

func TestSeriesOf(t *testing.T) {
	tests := []struct {
		version string
		n       int
		want    string
	}{
		{"16.9", 1, "16"},
		{"16.9", 2, "16.9"},
		{"9.6.24", 2, "9.6"},
		{"16", 2, "16"},
	}
	for _, tt := range tests {
		if got := seriesOf(tt.version, tt.n); got != tt.want {
			t.Errorf("seriesOf(%q, %d) = %q, want %q", tt.version, tt.n, got, tt.want)
		}
	}
}
