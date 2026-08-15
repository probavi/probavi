package main

import (
	"os"
	"path/filepath"
	"regexp"
)

// backupversion.go reads which server release series an XtraBackup backup
// was taken from, out of the backup itself.
//
// A physical backup restores only into its own release series
// (docs/engine-versions.md §5): XtraBackup 8.0 output belongs on a MySQL
// 8.0 server, 8.4 on 8.4. xtrabackup_info — the same metadata file
// backuptime.go dates the backup from — records the origin server as
// `server_version`. Reading it host-side costs nothing and lets the drill
// refuse an impossible pairing before a byte is transferred, with a
// message that names both sides, instead of surfacing whatever the
// prepare or the restored server prints when the data files belong to a
// different series.

// serverVersionKey is the xtrabackup_info line naming the server the
// backup was taken from ("8.4.5", or "8.0.36-28" from a Percona Server).
const serverVersionKey = "server_version"

// seriesPattern extracts the leading major.minor release series from a
// server version string; a value it does not match is not a version this
// check should reason about.
var seriesPattern = regexp.MustCompile(`^\d+\.\d+`)

// backupSeries returns the major.minor release series the backup was
// taken from, or "" when that cannot be read: no metadata file, no
// server_version line, or a value that is not version-shaped. Nothing is
// guessed — "" skips the version pre-check rather than weakening it.
func backupSeries(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, xtrabackupInfoName))
	if err != nil || len(raw) > xtrabackupInfoMaxBytes {
		return ""
	}
	version, ok := backupInfoValue(string(raw), serverVersionKey)
	if !ok {
		return ""
	}
	return seriesPattern.FindString(version)
}
