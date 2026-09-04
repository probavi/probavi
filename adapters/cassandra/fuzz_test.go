package main

import (
	"testing"
)

// FuzzParseTOC drives the component list reader over arbitrary bytes.
//
// An SSTable's TOC.txt is the table's own statement of which components
// it consists of, and the census judges the artifact against it: a
// component named there but absent is a truncated copy, refused before
// anything is transferred. The names it produces are compared and
// quoted, so an entry that is not a name would either weaken that
// judgement or carry the artifact's bytes into the operator's terminal.
func FuzzParseTOC(f *testing.F) {
	f.Add([]byte("Data.db\nIndex.db\nStatistics.db\n"))
	f.Add([]byte("Data.db\r\nIndex.db\r\n"))
	f.Add([]byte("\n\n   \n"))
	f.Add([]byte("Data.db"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		for _, c := range parseTOC(raw) {
			if c == "" {
				t.Fatal("TOC produced an empty component name, which names every file and none")
			}
			if len(c) > len(raw) {
				t.Fatalf("component %q is longer than the TOC it was read from", c)
			}
		}
	})
}

// FuzzRecordManifest drives the snapshot manifest reader over arbitrary
// bytes.
//
// manifest.json is what a snapshot says about itself, including when it
// was taken — an instant that dates the record. The reader is
// deliberately tolerant (a manifest it cannot read leaves the table
// judged by its files alone), so what it must never do is record a claim
// it did not read: a date outside any snapshot's lifetime, or facts from
// a document that never parsed.
func FuzzRecordManifest(f *testing.F) {
	f.Add([]byte(`{"files":["md-1-big-Data.db"],"created_at":"2026-08-14T14:37:45.123Z"}`))
	f.Add([]byte(`{"files":[],"created_at":"1900-01-01T00:00:00Z"}`))
	f.Add([]byte(`{"created_at":"yesterday"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte("not json"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		facts := newTableFacts()
		recordManifest(facts, raw)

		if !facts.manifestOK {
			if facts.createdMs != 0 || len(facts.manifest.Files) != 0 {
				t.Fatalf("an unread manifest still recorded %+v", facts.manifest)
			}
			return
		}
		if facts.createdMs != 0 && !plausibleEpochMs(facts.createdMs) {
			t.Fatalf("created_at %d is not an instant a snapshot was taken at, yet it dates the record",
				facts.createdMs)
		}
	})
}
