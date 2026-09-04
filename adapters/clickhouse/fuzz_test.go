package main

import (
	"bytes"
	"testing"
	"time"
)

// FuzzScanManifestTimestamp drives the manifest header reader over
// arbitrary bytes.
//
// The `.backup` member is read on the drill host, out of an archive the
// operator points at and nothing has vetted, before a byte is
// transferred — attacker-shaped input by this repository's threat model
// (SECURITY.md). An XML decoder over such a stream is the classic place
// for an unbounded read or a decoder panic.
//
// Beyond survival, the property that matters is that the two returns
// stay distinguishable. Both callers of readBackupWallClock read a zero
// time as "this archive says nothing about when it was taken"
// (source.go's candidate, zone.go's createdAt), so a success that hands
// back a zero time would make a dated backup and an undated one the same
// value. The rest pins what a caller may assume of a success: a wall
// clock with no zone attached, at the manifest's own precision, that
// survives a round trip through the layout it was written in.
func FuzzScanManifestTimestamp(f *testing.F) {
	f.Add([]byte(`<backup><version>4</version><timestamp>2026-03-01 12:00:00</timestamp></backup>`))
	f.Add([]byte(`<backup><timestamp>2026-03-01 12:00:00</timestamp><contents></contents></backup>`))
	f.Add([]byte(`<backup><version>4</version></backup>`))
	f.Add([]byte(`<backup><timestamp>not a time</timestamp></backup>`))
	f.Add([]byte(`<backup><timestamp></timestamp></backup>`))
	f.Add([]byte(`<backup><timestamp><nested/></timestamp></backup>`))
	f.Add([]byte(`<backup><timestamp`))
	f.Add([]byte(`<!DOCTYPE backup [<!ENTITY x "2026-03-01 12:00:00">]><timestamp>&x;</timestamp>`))
	f.Add([]byte(`<timestamp>0001-01-01 00:00:00</timestamp>`))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, manifest []byte) {
		ts, err := scanManifestTimestamp(bytes.NewReader(manifest))

		if err != nil {
			if !ts.IsZero() {
				t.Fatalf("scanManifestTimestamp reported %v and still returned %v", err, ts)
			}
			return
		}
		if ts.IsZero() {
			t.Fatal("a success returned the zero time, which every caller reads as 'no timestamp'")
		}
		if loc := ts.Location(); loc != time.UTC {
			t.Fatalf("timestamp carries location %v — the manifest states no offset, so zone.go must supply it", loc)
		}
		if ts.Nanosecond() != 0 {
			t.Fatalf("timestamp %v has sub-second precision the manifest layout cannot hold", ts)
		}
		again, perr := time.Parse(manifestTimeLayout, ts.Format(manifestTimeLayout))
		if perr != nil || !again.Equal(ts) {
			t.Fatalf("timestamp %v does not survive its own layout: %v, %v", ts, again, perr)
		}
	})
}
