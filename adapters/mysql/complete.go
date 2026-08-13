package main

import (
	"strings"
)

// complete.go answers a question the mysql client cannot: was this the
// whole dump?
//
// The client reports that no statement it ran failed. It does not report
// that it reached the end of a complete dump, and a dump that stops on a
// statement boundary is entirely valid SQL as far as it goes. Measured
// against a real server: a dump of three tables cut where mysqldump would
// have died after the first restores that one table, the client exits 0,
// the decompressor exits 0, and the drill passes — having proved a third of
// the backup. For the accounts script the hole is wider still, because that
// replay runs with --force, so the client cannot abort at all.
//
// The failure that produces such a file is ordinary. A backup job running
// `mysqldump | gzip` whose mysqldump dies of a full disk, a killed session,
// or a lost connection leaves a perfectly valid gzip member behind holding
// an unfinished dump, and every byte in it restores. Reporting that as a
// pass is what the protocol forbids (§5), and it is the one failure an
// evidence product must not make.
//
// mysqldump signs its output off with a line of its own, and the check
// rests on that. What makes it usable is that the sign-off and the banner
// travel together: measured across the flags a backup job plausibly uses,
// the default and --skip-dump-date write both, while --compact and
// --skip-comments write neither. So a dump that announces itself as a
// mysqldump must also carry its ending, and a comment-free dump — which has
// no ending to carry — is exempt rather than failed.

const (
	// dumpBannerPrefix opens the output of every mysqldump that writes
	// comments at all.
	dumpBannerPrefix = "-- MySQL dump"
	// dumpCompleteMarker matches the line mysqldump closes with, dated or
	// not: --skip-dump-date drops the date and keeps the line.
	dumpCompleteMarker = `^-- Dump completed`
	// markerTailBytes is how much of a member's end is searched for that
	// line. It is the last line of the file; this is slack for the newline
	// conventions and for any tool that appends a footer of its own.
	markerTailBytes = 4096
	// dumpHeadBytes is how much of a member's beginning is read to see
	// whether it announces itself. The banner is the first line, and a
	// bounded read keeps the cost the same whatever the artifact's size —
	// including a compressed one, which is inflated only this far.
	dumpHeadBytes = 4096
)

// completenessMarker returns the pattern a member's end has to match, or ""
// when the member carries no ending to check.
//
// An unreadable member yields "" as well: it is not this function's job to
// fail a drill, and the restore that opens the same file next reports what
// is wrong with it in the client's own words.
func completenessMarker(path string) string {
	head, ok := readDumpHead(path)
	if !ok || !announcesItself(head) {
		return ""
	}
	return dumpCompleteMarker
}

// announcesItself reports whether a member opens with mysqldump's banner.
// The banner is looked for anywhere in the head rather than at byte zero, so
// a backup job that prepends a note of its own does not turn a checkable
// dump into an uncheckable one.
func announcesItself(head string) bool {
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), dumpBannerPrefix) {
			return true
		}
	}
	return false
}
