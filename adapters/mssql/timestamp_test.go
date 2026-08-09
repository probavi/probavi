package main

import (
	"testing"
	"time"
)

// headerRowWithDate extends the test header row to the column
// BackupFinishDate occupies on a real server (measured: 19th of 59).
// headerRowWithDate is a full backup's header row carrying a completion
// date, which is what dates a backup and what ranks two of them. The set
// is the first one on the media, as a single-set backup file has.
func headerRowWithDate(finished string) string {
	row := headerRow(backupTypeFull, 1)
	for range 7 {
		row += "|x"
	}
	return row + "|" + finished + "|" + finished + "|52"
}

func TestParseBackupSetsReadsTheFinishDate(t *testing.T) {
	sets, perr := parseBackupSets([]byte(headerRowWithDate("2026-08-09 21:08:21.000") + "\n"))
	if perr != nil {
		t.Fatalf("parseBackupSets: %+v", perr)
	}
	if len(sets) != 1 || sets[0].finishedAt != "2026-08-09 21:08:21.000" {
		t.Fatalf("sets = %+v, want the completion wall clock", sets)
	}
}

func TestParseBackupSetsToleratesAShortRow(t *testing.T) {
	// A row that stops before the date column still selects normally: the
	// backup type and position are what the choice needs.
	sets, perr := parseBackupSets([]byte(headerRow(backupTypeFull, 1) + "\n"))
	if perr != nil {
		t.Fatalf("parseBackupSets: %+v", perr)
	}
	if len(sets) != 1 || sets[0].finishedAt != "" {
		t.Errorf("sets = %+v, want no date and no complaint", sets)
	}
}

func TestFinishedAtOf(t *testing.T) {
	sets := []backupSet{
		{position: 1, backupType: backupTypeFull, finishedAt: "2026-08-08 03:00:00.000"},
		{position: 2, backupType: backupTypeLog, finishedAt: "2026-08-09 03:00:00.000"},
		{position: 3, backupType: backupTypeFull, finishedAt: "2026-08-09 21:08:21.000"},
	}
	if got := finishedAtOf(sets, 3); got != "2026-08-09 21:08:21.000" {
		t.Errorf("finishedAtOf(3) = %q, want the chosen set's own clock", got)
	}
	if got := finishedAtOf(sets, 9); got != "" {
		t.Errorf("finishedAtOf(9) = %q, want empty for a set that is not there", got)
	}
}

func TestBackupFinishedAt(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load zone: %v — the adapter embeds the zone database", err)
	}
	t.Run("no zone declared means no claim", func(t *testing.T) {
		if got := backupFinishedAt("2026-08-09 21:08:21.000", nil); got != nil {
			t.Errorf("createdAt = %v, want nil — the instant is not derivable without the zone", *got)
		}
	})
	t.Run("no date in the header means no claim", func(t *testing.T) {
		if got := backupFinishedAt("", tokyo); got != nil {
			t.Errorf("createdAt = %v, want nil", *got)
		}
	})
	t.Run("the declared zone makes it an instant", func(t *testing.T) {
		// Measured: a server in Asia/Tokyo records 21:08:21 for a backup
		// taken at 12:08:21 UTC, with no offset in the header.
		got := backupFinishedAt("2026-08-09 21:08:21.000", tokyo)
		if got == nil || *got != "2026-08-09T21:08:21.000+09:00" {
			t.Fatalf("createdAt = %v, want the header's wall clock with Tokyo's offset", got)
		}
		parsed, err := time.Parse(time.RFC3339, *got)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if utc := parsed.UTC().Format("2006-01-02T15:04:05Z"); utc != "2026-08-09T12:08:21Z" {
			t.Errorf("in UTC = %s, want 2026-08-09T12:08:21Z", utc)
		}
	})
	t.Run("an unreadable clock yields no claim", func(t *testing.T) {
		if got := backupFinishedAt("not a date", tokyo); got != nil {
			t.Errorf("createdAt = %v, want nil", *got)
		}
	})
}

// TestDaylightSavingIsTakenFromTheBackupDate is why the config names a
// zone instead of an offset: the same host is +01:00 in January and
// +02:00 in July, so a number written once in a config file would be
// wrong for half of every year.
func TestDaylightSavingIsTakenFromTheBackupDate(t *testing.T) {
	budapest, err := time.LoadLocation("Europe/Budapest")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	if got := backupFinishedAt("2026-07-09 03:00:00.000", budapest); got == nil || *got != "2026-07-09T03:00:00.000+02:00" {
		t.Errorf("summer createdAt = %v, want +02:00", got)
	}
	if got := backupFinishedAt("2026-01-09 03:00:00.000", budapest); got == nil || *got != "2026-01-09T03:00:00.000+01:00" {
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
