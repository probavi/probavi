package main

import (
	"io"
	"os"
	"strings"
	"time"
)

// timestamp.go dates the backup from the backup itself.
//
// The obvious candidate — the file's modification time — is not the
// backup's creation time and cannot be made into one: copying a backup
// without preserving timestamps (cp without -p, most object-store
// downloads) resets it, and a month-old artifact then looks like last
// night's. A drill that restores it would record a fresh-looking
// created_at for a stale backup, which is a claim the evidence should
// never make.
//
// mysqldump signs its output off with the time it finished. What it does
// not write is an offset: the value is the wall clock of the host that
// ran mysqldump (measured — the same dump taken under TZ=Asia/Tokyo and
// TZ=UTC differs by nine hours with nothing in the file to say which).
// The `SET TIME_ZONE='+00:00'` line a dump carries governs its TIMESTAMP
// data, not this comment. A wall clock is not an instant, so the zone
// comes from the drill config (see zone.go) or the adapter reports no
// creation time at all.

const (
	// dumpCompletedPrefix is what mysqldump writes as its last line. With
	// --skip-dump-date the prefix stays and the date does not, and
	// --compact drops the comments entirely: in both cases there is
	// simply nothing to read, which is not the same as a defect.
	dumpCompletedPrefix = "-- Dump completed on"
	dumpClockLayout     = "2006-01-02 15:04:05"
	// dumpTrailerBytes is how much of the tail is searched. The trailer is
	// the last line of the file; this is slack for the newline
	// conventions and any tool that appends its own footer.
	dumpTrailerBytes = 4096
)

// dumpCompletedAt reads the dump's own completion time and places it in
// the operator-declared zone. It returns nil whenever the answer would be
// a guess: no zone declared, or no dated trailer in the file.
func dumpCompletedAt(path string, loc *time.Location) *string {
	if loc == nil {
		return nil
	}
	clock, ok := dumpClock(path)
	if !ok {
		return nil
	}
	return formatCreatedAt(time.Date(clock.Year(), clock.Month(), clock.Day(),
		clock.Hour(), clock.Minute(), clock.Second(), 0, loc))
}

// dumpClock reads the wall clock a dump records about itself, carried in a
// time.Time labelled UTC that is a wall clock and not an instant.
//
// It exists so two backups can be ranked against each other (see
// newestBackupIn): both came off the same backup host, so whatever zone
// that host was in cancels out of the comparison, and ranking therefore
// works whether or not the operator declared one. Reporting a creation
// time is the other job, and that one does need the zone.
func dumpClock(path string) (time.Time, bool) {
	tail, ok := readTail(path, dumpTrailerBytes)
	if !ok {
		return time.Time{}, false
	}
	clock, ok := lastDumpClock(tail)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(dumpClockLayout, clock, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// readTail returns the last n bytes of a file.
func readTail(path string, n int64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	tail, rerr := readTailFrom(f, n)
	if cerr := f.Close(); cerr != nil || rerr != nil {
		return "", false
	}
	return tail, true
}

func readTailFrom(f *os.File, n int64) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	offset := max(info.Size()-n, 0)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// lastDumpClock finds the wall clock in the trailer. The last match wins:
// concatenated dumps carry one trailer each, and the drill restores all of
// them, so the set is only as current as the last one written.
func lastDumpClock(tail string) (string, bool) {
	clock, found := "", false
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, dumpCompletedPrefix)
		if !ok {
			continue
		}
		// The sentence without a date is not a date: mysqldump writes one
		// under --skip-dump-date, and a drill must read that as "unknown"
		// rather than as an unparseable value.
		if rest = strings.TrimSpace(rest); rest != "" {
			clock, found = rest, true
		}
	}
	return clock, found
}
