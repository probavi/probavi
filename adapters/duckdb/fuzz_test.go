package main

import (
	"testing"
)

// FuzzParseDuckHeader drives the database header reader over arbitrary
// bytes.
//
// A DuckDB artifact is a single file the operator points at, read on the
// drill host before anything is transferred. The header is read at fixed
// offsets, which is the shape that reads past the end of a short file,
// and both fields it reports reach the operator: the storage version
// appears in a refusal naming what the engine cannot open, and the
// library version is quoted back as the writer of the file. So a header
// that was not recognised must claim nothing, and what is claimed must
// be what the file says rather than what the offsets landed on.
func FuzzParseDuckHeader(f *testing.F) {
	valid := make([]byte, storageVersionOffset+8)
	copy(valid[duckMagicOffset:], duckMagic)
	f.Add(valid)
	f.Add(append(append([]byte{}, valid...), []byte("v1.5.5\x00")...))
	f.Add([]byte(duckMagic))
	f.Add([]byte("not a duckdb file"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, head []byte) {
		h := parseDuckHeader(head)

		if !h.valid {
			if h.storageVersion != 0 || h.libraryVersion != "" {
				t.Fatalf("unrecognised header still produced %+v", h)
			}
			return
		}
		if !hasDuckMagic(head) {
			t.Fatal("valid header without the magic that defines one")
		}
		if v := h.libraryVersion; v != "" && !versionShape.MatchString(v) {
			t.Fatalf("libraryVersion = %q — header noise must not reach a refusal message", v)
		}
	})
}
