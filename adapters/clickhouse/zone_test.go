package main

import (
	"testing"
	"time"
)

func TestBackupLocation(t *testing.T) {
	tests := []struct {
		name, param string
		wantErr     bool
		wantNil     bool
	}{
		{name: "no declaration means no creation time", param: "", wantNil: true},
		{name: "a named zone resolves", param: "Europe/Budapest"},
		{name: "UTC resolves", param: "UTC"},
		{name: "an unknown name fails the drill", param: "Mars/Olympus", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loc, perr := backupLocation(map[string]string{backupTimezoneParam: tc.param})
			switch {
			case tc.wantErr:
				if perr == nil || perr.Code != "invalid_request" {
					t.Fatalf("perr = %+v, want invalid_request", perr)
				}
				return
			case perr != nil:
				t.Fatalf("perr = %+v", perr)
			}
			if (loc == nil) != tc.wantNil {
				t.Errorf("loc = %v, wantNil = %v", loc, tc.wantNil)
			}
		})
	}
}

// TestCreatedAtPlacesTheWallClock covers the whole point of the zone
// parameter: the same recorded wall clock is a different instant depending
// on where the server stood, and the offset depends on the date — which is
// why the config names a zone rather than a number.
func TestCreatedAtPlacesTheWallClock(t *testing.T) {
	budapest, err := time.LoadLocation("Europe/Budapest")
	if err != nil {
		t.Skipf("zone database unavailable: %v", err)
	}
	winter := time.Date(2026, 1, 15, 2, 30, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 15, 2, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		wallClock time.Time
		loc       *time.Location
		want      string
	}{
		{"UTC keeps the clock", summer, time.UTC, "2026-07-15T02:30:00.000Z"},
		{"winter is +01:00", winter, budapest, "2026-01-15T02:30:00.000+01:00"},
		{"summer is +02:00", summer, budapest, "2026-07-15T02:30:00.000+02:00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := createdAt(tc.wallClock, tc.loc)
			if got == nil || *got != tc.want {
				t.Errorf("createdAt = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestCreatedAtStaysNullWhenNothingIsKnown(t *testing.T) {
	known := time.Date(2026, 8, 14, 14, 37, 45, 0, time.UTC)
	if got := createdAt(known, nil); got != nil {
		t.Errorf("createdAt = %v with no declared zone, want nil: a wall clock is not an instant", *got)
	}
	if got := createdAt(time.Time{}, time.UTC); got != nil {
		t.Errorf("createdAt = %v with no backup timestamp, want nil", *got)
	}
}
