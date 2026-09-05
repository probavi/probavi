package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzDecodeBlockMeta drives one block's meta.json reader over arbitrary
// bytes, and asks the census what it makes of the result.
//
// A TSDB snapshot states its own contents in these files, read on the
// drill host before anything is transferred. The census derived from
// them is what the restored server's prometheus_tsdb_blocks_loaded is
// compared against, so a meta.json read wrongly turns into a drill that
// reports green for a snapshot the server silently loaded less of.
//
// The census promises to account for every block it was given — each one
// either counts or is a superseded compaction source, never both and
// never neither — and the newest instant it derives may only be one a
// block could carry.
func FuzzDecodeBlockMeta(f *testing.F) {
	f.Add([]byte(`{"ulid":"01JABCDEF0123456789ABCDEFG","maxTime":1786000000000,"compaction":{"parents":[{"ulid":"01JPARENT0123456789ABCDEF"}]}}`))
	f.Add([]byte(`{"ulid":"01JABCDEF0123456789ABCDEFG","maxTime":1786000000000,"compaction":{"parents":["01JPARENT0123456789ABCDEF"]}}`))
	f.Add([]byte(`{"ulid":"x","maxTime":0}`))
	f.Add([]byte(`{"maxTime":1786000000000}`))
	f.Add([]byte(`{"ulid":"x","maxTime":-1}`))
	f.Add([]byte(`{}`))
	f.Add([]byte("not json"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		meta, ok := decodeBlockMeta(bytes.NewReader(raw))

		if !ok {
			if meta.ULID != "" || meta.MaxTime != 0 || len(meta.Compaction.Parents) != 0 {
				t.Fatalf("an unreadable meta.json still produced %+v", meta)
			}
			return
		}
		if !plausibleEpochMs(meta.MaxTime) {
			t.Fatalf("maxTime %d is not an instant a block covers, yet it dates the snapshot",
				meta.MaxTime)
		}
		metas := []blockMeta{meta, {ULID: "01JOTHER0123456789ABCDEFGH", MaxTime: meta.MaxTime}}
		info := censusOf(metas)
		if info.blocks+info.sourcesSkipped != len(metas) {
			t.Fatalf("census counted %d blocks and %d skipped sources out of %d — "+
				"the count the restored server is judged against must account for every block",
				info.blocks, info.sourcesSkipped, len(metas))
		}
		if info.maxTimeMs != 0 && !plausibleEpochMs(info.maxTimeMs) {
			t.Fatalf("census dated the snapshot %d", info.maxTimeMs)
		}
	})
}

// FuzzParentULID drives the compaction parent reader over arbitrary
// JSON.
//
// What it returns subtracts a block from the census — the count the
// drill's green verdict rests on — and the reader is tolerant on
// purpose, because the entry's shape has varied across server versions.
// Tolerant must still mean "reads what is there": a shape carrying no
// ulid has to subtract nothing rather than something the decoder
// improvised.
func FuzzParentULID(f *testing.F) {
	f.Add([]byte(`"01JPARENT0123456789ABCDEF"`))
	f.Add([]byte(`{"ulid":"01JPARENT0123456789ABCDEF"}`))
	f.Add([]byte(`{"ulid":123}`))
	f.Add([]byte(`{"ulid":""}`))
	// Two decoder behaviours this reader inherits, kept as cases rather
	// than as opaque corpus files: Go matches object keys without regard
	// to case, and it replaces invalid UTF-8 in a string with U+FFFD.
	f.Add([]byte(`{"uliD":"01JPARENT0123456789ABCDEF"}`))
	f.Add([]byte("\"\x94\x94\""))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, raw []byte) {
		u := parentULID(json.RawMessage(raw))
		if u == "" {
			return
		}
		var entry any
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("parentULID(%q) = %q out of an entry that is not JSON", raw, u)
		}
		switch v := entry.(type) {
		case string:
			if v != u {
				t.Fatalf("parentULID(%q) = %q, not the string the entry holds", raw, u)
			}
		case map[string]any:
			named := false
			for k, val := range v {
				// Go matches object keys without regard to case, so the
				// entry names a parent under any spelling of the key.
				if strings.EqualFold(k, "ulid") && val == any(u) {
					named = true
				}
			}
			if !named {
				t.Fatalf("parentULID(%q) = %q, not the ulid the entry names", raw, u)
			}
		default:
			t.Fatalf("parentULID(%q) = %q out of an entry naming no parent", raw, u)
		}
	})
}
