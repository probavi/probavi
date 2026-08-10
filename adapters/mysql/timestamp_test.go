package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The trailer below is what mysqldump 8.4 writes as the last line of a
// dump. Measured: the same dump taken under TZ=Asia/Tokyo and TZ=UTC
// differs by nine hours with nothing in the file to say which — the
// SET TIME_ZONE line a dump carries governs its TIMESTAMP data, not this
// comment.
const dumpTail = "INSERT INTO `orders` VALUES (1);\n" +
	"-- Dump completed on 2026-08-09 21:08:17\n"

func writeDump(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orders.sql")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLastDumpClock(t *testing.T) {
	tests := []struct {
		name  string
		tail  string
		want  string
		found bool
	}{
		{"an ordinary dump", dumpTail, "2026-08-09 21:08:17", true},
		{"trailing whitespace", "-- Dump completed on 2026-08-09 21:08:17  \n", "2026-08-09 21:08:17", true},
		// Concatenated dumps carry one trailer each and the drill restores
		// all of them, so the set is only as current as the last written.
		{"concatenated dumps",
			"-- Dump completed on 2026-08-08 03:00:00\n-- Dump completed on 2026-08-09 03:00:00\n",
			"2026-08-09 03:00:00", true},
		// Measured: --skip-dump-date keeps the sentence and drops the date.
		{"skip-dump-date", "-- Dump completed\n", "", false},
		// And the defensive shape of the same absence: the prefix present
		// with nothing after it must not count as a date.
		{"the sentence with no date after it", "-- Dump completed on\n", "", false},
		{"the sentence with only spaces after it", "-- Dump completed on    \n", "", false},
		// Measured: --compact writes no comments at all.
		{"compact output", "INSERT INTO `orders` VALUES (1);\n", "", false},
		{"empty file", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := lastDumpClock(tt.tail)
			if found != tt.found || got != tt.want {
				t.Errorf("lastDumpClock = %q, %v; want %q, %v", got, found, tt.want, tt.found)
			}
		})
	}
}

func TestDumpCompletedAt(t *testing.T) {
	path := writeDump(t, dumpTail)

	t.Run("no zone declared means no claim", func(t *testing.T) {
		if got := dumpCompletedAt(context.Background(), path, nil); got != nil {
			t.Errorf("createdAt = %v, want nil — the instant is not derivable without the zone", *got)
		}
	})
	t.Run("the declared zone makes it an instant", func(t *testing.T) {
		tokyo := mustLoad(t, "Asia/Tokyo")
		got := dumpCompletedAt(context.Background(), path, tokyo)
		if got == nil || *got != "2026-08-09T21:08:17.000+09:00" {
			t.Fatalf("createdAt = %v, want the trailer's wall clock with Tokyo's offset", got)
		}
		parsed, err := time.Parse(time.RFC3339, *got)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if utc := parsed.UTC().Format("2006-01-02T15:04:05Z"); utc != "2026-08-09T12:08:17Z" {
			t.Errorf("in UTC = %s, want 2026-08-09T12:08:17Z", utc)
		}
	})
	t.Run("an undated dump yields no claim", func(t *testing.T) {
		tokyo := mustLoad(t, "Asia/Tokyo")
		if got := dumpCompletedAt(context.Background(), writeDump(t, "-- Dump completed\n"), tokyo); got != nil {
			t.Errorf("createdAt = %v, want nil", *got)
		}
	})
	t.Run("a missing file yields no claim", func(t *testing.T) {
		tokyo := mustLoad(t, "Asia/Tokyo")
		if got := dumpCompletedAt(context.Background(), filepath.Join(t.TempDir(), "gone"), tokyo); got != nil {
			t.Errorf("createdAt = %v, want nil", *got)
		}
	})
	t.Run("the trailer is found in a large dump", func(t *testing.T) {
		big := make([]byte, 200*1024)
		for i := range big {
			big[i] = 'x'
		}
		path := writeDump(t, string(big)+"\n"+dumpTail)
		tokyo := mustLoad(t, "Asia/Tokyo")
		if got := dumpCompletedAt(context.Background(), path, tokyo); got == nil {
			t.Error("createdAt = nil — the trailer must be read from the tail, not the head")
		}
	})
}

// TestDaylightSavingIsTakenFromTheBackupDate is why the config names a
// zone instead of an offset: the same host is +01:00 in January and
// +02:00 in July, so a number written once in a config file would be
// wrong for half of every year.
func TestDaylightSavingIsTakenFromTheBackupDate(t *testing.T) {
	budapest := mustLoad(t, "Europe/Budapest")
	summer := writeDump(t, "-- Dump completed on 2026-07-09 03:00:00\n")
	winter := writeDump(t, "-- Dump completed on 2026-01-09 03:00:00\n")
	if got := dumpCompletedAt(context.Background(), summer, budapest); got == nil || *got != "2026-07-09T03:00:00.000+02:00" {
		t.Errorf("summer createdAt = %v, want +02:00", got)
	}
	if got := dumpCompletedAt(context.Background(), winter, budapest); got == nil || *got != "2026-01-09T03:00:00.000+01:00" {
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
