package main

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"io"
	"time"
)

// backupmeta.go dates the backup from the backup itself.
//
// A ClickHouse backup archive carries a `.backup` member — an XML manifest
// naming every file in the backup, opened by a small header. That header
// holds a `<timestamp>` (measured against ClickHouse 26.3), which is the
// moment the BACKUP statement ran. This adapter therefore reports a real
// backup.created_at rather than the null the mongodb adapter must report,
// and ranks a directory of archives by what each one says about itself
// rather than by an mtime that dates a copy.
//
// What the timestamp does not carry is an offset: it is the *server's*
// wall clock, and a wall clock is not an instant (measured: a container
// running Etc/UTC writes UTC, and nothing in the file says so). The
// missing piece comes from the drill config — see zone.go.
//
// The manifest is read as a token stream and abandoned at the closing
// </timestamp> tag. A backup of a large table lists one <file> element per
// part file, so the manifest can reach many megabytes; the header is
// always its first few hundred bytes, and there is no reason to decode the
// rest to learn one field.

// backupManifest is the archive member every ClickHouse backup carries.
const backupManifest = ".backup"

// manifestTimeLayout is how ClickHouse writes the timestamp: the server's
// wall clock, seconds precision, no offset.
const manifestTimeLayout = "2006-01-02 15:04:05"

// errNoManifest reports an archive with no `.backup` member — either not a
// ClickHouse backup at all, or one truncated before its manifest.
var errNoManifest = errors.New("archive carries no .backup manifest")

// errNoTimestamp reports a manifest whose header holds no timestamp. It is
// separate from errNoManifest because the archive is recognisably a
// ClickHouse backup: the drill may proceed with no creation time.
var errNoTimestamp = errors.New(".backup manifest declares no timestamp")

// readBackupWallClock returns the wall clock the archive records, with no
// zone attached. The caller decides what to do with a naive time.
func readBackupWallClock(path string) (time.Time, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return time.Time{}, err
	}
	ts, rerr := scanArchive(&zr.Reader)
	if cerr := zr.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	return ts, rerr
}

// scanArchive finds the manifest member and reads its header.
func scanArchive(r *zip.Reader) (time.Time, error) {
	for _, f := range r.File {
		if f.Name != backupManifest {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return time.Time{}, err
		}
		ts, serr := scanManifestTimestamp(rc)
		if cerr := rc.Close(); cerr != nil && serr == nil {
			serr = cerr
		}
		return ts, serr
	}
	return time.Time{}, errNoManifest
}

// scanManifestTimestamp pulls the first <timestamp> out of the manifest and
// stops there.
func scanManifestTimestamp(r io.Reader) (time.Time, error) {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return time.Time{}, errNoTimestamp
		}
		if err != nil {
			return time.Time{}, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "timestamp" {
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			return time.Time{}, err
		}
		ts, err := time.Parse(manifestTimeLayout, text)
		// A zero time is how every caller spells "this archive says
		// nothing about when it was taken" (the candidate in source.go,
		// createdAt in zone.go), so returning one as a success would make
		// a dated backup and an undated one the same value. No ClickHouse
		// server writes year 1 — a manifest that does is corrupt, and
		// corrupt is what errNoTimestamp already means here.
		if err != nil || ts.IsZero() {
			return time.Time{}, errNoTimestamp
		}
		return ts, nil
	}
}
