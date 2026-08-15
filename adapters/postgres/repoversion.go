package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// repoversion.go reads which PostgreSQL version a pgBackRest repository
// was taken from, out of the repository itself.
//
// A physical backup restores only into its own major version
// (docs/engine-versions.md §5), and the repository states that major in
// backup.info's [db] section — the same manifest repotime.go dates the
// repository from. Reading it host-side costs nothing and lets the drill
// refuse an impossible pairing before a byte is transferred, with a
// message that names both sides, instead of surfacing whatever pgbackrest
// prints once the restored data directory meets the wrong server.

const (
	// dbSection is the manifest section describing the cluster the
	// repository backs up. [db:history] repeats the same fields per
	// upgrade generation; only the current section states what a restore
	// today produces.
	dbSection = "[db]"
	// dbVersionKey names the PostgreSQL version line inside [db]. The
	// value is quoted: "16" on modern servers, "9.6" in the
	// two-component era.
	dbVersionKey = "db-version"
)

// dbVersionPattern accepts only version-shaped values, so a manifest
// oddity cannot become a refusal message.
var dbVersionPattern = regexp.MustCompile(`^\d+(?:\.\d+)*$`)

// repoDBVersion returns the PostgreSQL version the repository's backups
// were taken from, or "" when that cannot be read: no manifest, an
// encrypted one (repo1-cipher-type makes backup.info unreadable), or a
// layout this parser does not recognise. Nothing is guessed — "" skips
// the version pre-check rather than weakening it.
func repoDBVersion(dir, stanza string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "backup", stanza, backupInfoName))
	if err != nil || len(raw) > backupInfoMaxBytes {
		return ""
	}
	inSection := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inSection = line == dbSection
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != dbVersionKey {
			continue
		}
		version := strings.Trim(value, `"`)
		if !dbVersionPattern.MatchString(version) {
			return ""
		}
		return version
	}
	return ""
}
