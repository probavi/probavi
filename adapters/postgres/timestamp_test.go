package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The headers below are the first bytes of real archives taken from
// PostgreSQL 13, 14, 16 and 17 servers running in Asia/Tokyo, together
// with the wall clock pg_restore -l printed for each. Two archive layouts
// are represented on purpose: through archive 1.14 the compression level
// is written as an int, from 1.15 it is a single byte, and reading the
// wrong one shifts every field after it.
var archiveHeaders = []struct {
	name  string
	head  []byte
	clock string // what pg_restore -l reported
}{
	{"archive 1.14 (server 13)", []byte{
		0x50, 0x47, 0x44, 0x4d, 0x50, 0x01, 0x0e, 0x00, 0x04, 0x08, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00,
		0x00, 0x1a, 0x00, 0x00, 0x00, 0x00, 0x1a, 0x00, 0x00, 0x00, 0x00, 0x15, 0x00, 0x00, 0x00, 0x00,
		0x09, 0x00, 0x00, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x7e, 0x00, 0x00, 0x00, 0x00, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}, "2026-08-09 21:26:26"},
	{"archive 1.14 (server 14)", []byte{
		0x50, 0x47, 0x44, 0x4d, 0x50, 0x01, 0x0e, 0x00, 0x04, 0x08, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00,
		0x00, 0x2a, 0x00, 0x00, 0x00, 0x00, 0x1a, 0x00, 0x00, 0x00, 0x00, 0x15, 0x00, 0x00, 0x00, 0x00,
		0x09, 0x00, 0x00, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x7e, 0x00, 0x00, 0x00, 0x00, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}, "2026-08-09 21:26:42"},
	{"archive 1.15 (server 16)", []byte{
		0x50, 0x47, 0x44, 0x4d, 0x50, 0x01, 0x0f, 0x00, 0x04, 0x08, 0x01, 0x01, 0x00, 0x2d, 0x00, 0x00,
		0x00, 0x00, 0x1a, 0x00, 0x00, 0x00, 0x00, 0x15, 0x00, 0x00, 0x00, 0x00, 0x09, 0x00, 0x00, 0x00,
		0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x7e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}, "2026-08-09 21:26:45"},
	{"archive 1.16 (server 17)", []byte{
		0x50, 0x47, 0x44, 0x4d, 0x50, 0x01, 0x10, 0x00, 0x04, 0x08, 0x01, 0x01, 0x00, 0x02, 0x00, 0x00,
		0x00, 0x00, 0x1b, 0x00, 0x00, 0x00, 0x00, 0x15, 0x00, 0x00, 0x00, 0x00, 0x09, 0x00, 0x00, 0x00,
		0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x7e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}, "2026-08-09 21:27:02"},
}

func TestParseArchiveHeaderTime(t *testing.T) {
	for _, tt := range archiveHeaders {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseArchiveHeaderTime(tt.head)
			if !ok {
				t.Fatalf("parseArchiveHeaderTime refused a real %s header", tt.name)
			}
			clock := time.Date(got.year, time.Month(got.month), got.day,
				got.hour, got.minute, got.second, 0, time.UTC).Format("2006-01-02 15:04:05")
			if clock != tt.clock {
				t.Errorf("decoded %s, want %s — the wall clock pg_restore reports", clock, tt.clock)
			}
		})
	}
}

func TestParseArchiveHeaderTimeRefusals(t *testing.T) {
	valid := archiveHeaders[2].head
	mutate := func(index int, value byte) []byte {
		head := append([]byte(nil), valid...)
		head[index] = value
		return head
	}
	tests := []struct {
		name string
		head []byte
	}{
		{"not an archive at all", make([]byte, 64)},
		// A file whose every other structural field looks right but whose
		// magic does not: the magic is what says "this is a pg_dump
		// archive" before any offset is trusted.
		{"a foreign format wearing a plausible header", mutate(0, 'X')},
		{"a plain SQL dump", []byte("-- PostgreSQL database dump\n" + string(make([]byte, 36)))},
		{"a directory-format archive", mutate(pgdumpFormatOffset, 3)},
		{"an integer width no pg_dump writes", mutate(pgdumpIntSizeOffset, 7)},
		{"a future major archive version", mutate(pgdumpVersionMajorOffset, 2)},
		// The layout guard: reading a 1.15 header as a 1.14 one lands on
		// sizes instead of a date, and the field ranges must catch it.
		{"the wrong layout for the version", mutate(pgdumpVersionMinorOffset, 14)},
		{"truncated below the header", valid[:20]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := parseArchiveHeaderTime(tt.head); ok {
				t.Errorf("parseArchiveHeaderTime accepted %s and produced %+v", tt.name, got)
			}
		})
	}
}

func writeArchive(t *testing.T, head []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orders.dump")
	if err := os.WriteFile(path, head, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArchiveCreatedAt(t *testing.T) {
	// The archive was written at 21:26:45 on a host in Tokyo, which is
	// 12:26:45 UTC. Only the declared zone can turn one into the other.
	path := writeArchive(t, archiveHeaders[2].head)

	t.Run("no zone declared means no claim", func(t *testing.T) {
		if got := archiveCreatedAt(path, nil); got != nil {
			t.Errorf("createdAt = %v, want nil — the instant is not derivable without the zone", *got)
		}
	})
	t.Run("the declared zone makes it an instant", func(t *testing.T) {
		tokyo := mustLoad(t, "Asia/Tokyo")
		got := archiveCreatedAt(path, tokyo)
		if got == nil {
			t.Fatal("createdAt = nil, want the archive's own creation time")
		}
		if *got != "2026-08-09T21:26:45.000+09:00" {
			t.Errorf("createdAt = %s, want the wall clock with Tokyo's offset", *got)
		}
		parsed, err := time.Parse(time.RFC3339, *got)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if utc := parsed.UTC().Format("2006-01-02T15:04:05Z"); utc != "2026-08-09T12:26:45Z" {
			t.Errorf("in UTC = %s, want 2026-08-09T12:26:45Z", utc)
		}
	})
	t.Run("the same wall clock in another zone is another instant", func(t *testing.T) {
		got := archiveCreatedAt(path, mustLoad(t, "UTC"))
		if got == nil || *got != "2026-08-09T21:26:45.000Z" {
			t.Errorf("createdAt = %v, want the wall clock read as UTC", got)
		}
	})
}

func TestArchiveCreatedAtRefusals(t *testing.T) {
	t.Run("a file that is not an archive yields no claim", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "orders.sql")
		if err := os.WriteFile(plain, []byte("-- PostgreSQL database dump\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		tokyo := mustLoad(t, "Asia/Tokyo")
		if got := archiveCreatedAt(plain, tokyo); got != nil {
			t.Errorf("createdAt = %v, want nil", *got)
		}
	})
	t.Run("a missing file yields no claim", func(t *testing.T) {
		tokyo := mustLoad(t, "Asia/Tokyo")
		if got := archiveCreatedAt(filepath.Join(t.TempDir(), "gone"), tokyo); got != nil {
			t.Errorf("createdAt = %v, want nil", *got)
		}
	})
}

// TestDaylightSavingIsTakenFromTheBackupDate is why the config names a
// zone instead of an offset: the same host is +01:00 in January and
// +02:00 in July, so a number written once in a config file would be
// wrong for half of every year.
func TestDaylightSavingIsTakenFromTheBackupDate(t *testing.T) {
	budapest := mustLoad(t, "Europe/Budapest")
	summer := archiveHeaders[2].head // 2026-08-09 21:26:45
	winter := append([]byte(nil), summer...)
	winter[33] = 0 // month field: August (7) -> January (0)

	got := archiveCreatedAt(writeArchive(t, summer), budapest)
	if got == nil || *got != "2026-08-09T21:26:45.000+02:00" {
		t.Errorf("summer createdAt = %v, want +02:00", got)
	}
	got = archiveCreatedAt(writeArchive(t, winter), budapest)
	if got == nil || *got != "2026-01-09T21:26:45.000+01:00" {
		t.Errorf("winter createdAt = %v, want +01:00", got)
	}
}

func TestBackupLocation(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		loc, perr := backupLocation(nil)
		if loc != nil || perr != nil {
			t.Errorf("backupLocation = %v, %+v, want no zone and no error", loc, perr)
		}
	})
	t.Run("valid", func(t *testing.T) {
		loc, perr := backupLocation(map[string]string{backupTimezoneParam: "Europe/Budapest"})
		if perr != nil || loc == nil || loc.String() != "Europe/Budapest" {
			t.Errorf("backupLocation = %v, %+v", loc, perr)
		}
	})
	t.Run("an offset is not a zone name", func(t *testing.T) {
		_, perr := backupLocation(map[string]string{backupTimezoneParam: "+02:00"})
		if perr == nil || perr.Code != "invalid_request" {
			t.Errorf("perr = %+v, want invalid_request — an offset cannot express daylight saving", perr)
		}
	})
	t.Run("a typo fails the drill rather than dropping the timestamp", func(t *testing.T) {
		_, perr := backupLocation(map[string]string{backupTimezoneParam: "Europe/Budapesst"})
		if perr == nil || perr.Code != "invalid_request" {
			t.Errorf("perr = %+v, want invalid_request", perr)
		}
	})
}

// mustLoad resolves a zone the adapter is expected to know; a failure here
// means the embedded zone database is missing, not that the test is wrong.
func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone %s: %v — the adapter embeds the zone database", name, err)
	}
	return loc
}
