package main

import (
	"encoding/binary"
	"os"
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
// A pg_dump custom-format archive carries its own creation time in the
// header. What it does not carry is an offset: the timestamp is the
// backup host's wall clock, broken into fields (measured — the same
// archive read back under TZ=UTC has pg_restore print the stored wall
// clock with a UTC label on it). A wall clock is not an instant, and
// backup.created_at is an instant, so the missing piece has to come from
// the drill config: source.params.backup_timezone names the zone the
// backup host was in. Without it, this adapter reports no creation time
// at all rather than guessing one.

// pgdumpMagic opens every pg_dump custom-format archive.
const pgdumpMagic = "PGDMP"

// The header is fixed-width up to the creation time: magic (5), version
// triple (3), intSize, offSize, format. What follows depends on the
// archive version, and this was measured across servers rather than
// assumed: through archive 1.14 the compression level is written as an
// int, from 1.15 it is a single byte naming the algorithm. Reading the
// wrong one shifts every field after it.
const (
	pgdumpVersionMajorOffset = 5
	pgdumpVersionMinorOffset = 6
	pgdumpIntSizeOffset      = 8
	pgdumpFormatOffset       = 10
	pgdumpFormatCustom       = 1
	pgdumpCompressionOffset  = 11
	pgdumpCompressionByteMin = 15 // archive minor version that made it a byte
	pgdumpHeaderBytes        = 64
)

// archiveCreatedAt reads a custom-format archive's own creation time and
// places it in the operator-declared zone. It returns nil whenever the
// answer would be a guess: no zone declared, a file that is not a custom
// -format archive, or a header this parser does not recognise.
func archiveCreatedAt(path string, loc *time.Location) *string {
	if loc == nil {
		return nil
	}
	clock, ok := archiveClock(path)
	if !ok {
		return nil
	}
	return formatCreatedAt(time.Date(clock.Year(), clock.Month(), clock.Day(),
		clock.Hour(), clock.Minute(), clock.Second(), 0, loc))
}

// archiveClock reads the wall clock a custom-format archive records about
// itself, carried in a time.Time labelled UTC that is a wall clock and not
// an instant.
//
// It exists so two backups can be ranked against each other (see
// newestBackupIn): both came off the same backup host, so whatever zone
// that host was in cancels out of the comparison, and ranking therefore
// works whether or not the operator declared one. Reporting a creation
// time is the other job, and that one does need the zone.
func archiveClock(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	head := make([]byte, pgdumpHeaderBytes)
	n, rerr := f.Read(head)
	if cerr := f.Close(); cerr != nil || rerr != nil || n < pgdumpHeaderBytes {
		return time.Time{}, false
	}
	fields, ok := parseArchiveHeaderTime(head)
	if !ok {
		return time.Time{}, false
	}
	return time.Date(fields.year, time.Month(fields.month), fields.day,
		fields.hour, fields.minute, fields.second, 0, time.UTC), true
}

// headerTime is the broken-down wall clock a custom-format header stores.
type headerTime struct {
	second, minute, hour, day, month, year int
}

// parseArchiveHeaderTime decodes the header's creation time. Every field
// it reads is range-checked, so a header laid out differently from what
// this parser expects is refused rather than misread — a wrong offset
// lands on sizes and version numbers, which do not look like a date.
func parseArchiveHeaderTime(head []byte) (headerTime, bool) {
	if string(head[:len(pgdumpMagic)]) != pgdumpMagic {
		return headerTime{}, false
	}
	if head[pgdumpFormatOffset] != pgdumpFormatCustom {
		return headerTime{}, false
	}
	intSize := int(head[pgdumpIntSizeOffset])
	if intSize != 4 && intSize != 8 {
		return headerTime{}, false
	}
	if head[pgdumpVersionMajorOffset] != 1 {
		return headerTime{}, false
	}

	// Where the timestamp starts depends on how compression was written.
	offset := pgdumpCompressionOffset + 1
	if head[pgdumpVersionMinorOffset] < pgdumpCompressionByteMin {
		offset = pgdumpCompressionOffset + 1 + intSize
	}

	const fieldCount = 6
	values := make([]int, 0, fieldCount)
	for range fieldCount {
		v, ok := readArchiveInt(head, offset, intSize)
		if !ok {
			return headerTime{}, false
		}
		values = append(values, v)
		offset += 1 + intSize
	}
	t := headerTime{
		second: values[0], minute: values[1], hour: values[2],
		day: values[3], month: values[4] + 1, year: values[5] + 1900,
	}
	if !plausible(t) {
		return headerTime{}, false
	}
	return t, true
}

// readArchiveInt reads one of pg_dump's integers: a sign byte followed by
// intSize little-endian magnitude bytes.
func readArchiveInt(head []byte, offset, intSize int) (int, bool) {
	if offset+1+intSize > len(head) {
		return 0, false
	}
	var magnitude uint64
	switch intSize {
	case 4:
		magnitude = uint64(binary.LittleEndian.Uint32(head[offset+1 : offset+5]))
	case 8:
		magnitude = binary.LittleEndian.Uint64(head[offset+1 : offset+9])
	}
	if magnitude > 1<<31 {
		return 0, false
	}
	if head[offset] != 0 {
		return -int(magnitude), true
	}
	return int(magnitude), true
}

// plausible rejects field values no calendar produces, which is what makes
// a misread header fail closed instead of dating a backup wrongly.
func plausible(t headerTime) bool {
	return t.second >= 0 && t.second <= 60 &&
		t.minute >= 0 && t.minute <= 59 &&
		t.hour >= 0 && t.hour <= 23 &&
		t.day >= 1 && t.day <= 31 &&
		t.month >= 1 && t.month <= 12 &&
		t.year >= 1970 && t.year <= 2150
}
