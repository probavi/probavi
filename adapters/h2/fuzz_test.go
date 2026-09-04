package main

import (
	"testing"
)

// FuzzStorageFormat drives the MVStore header field reader over
// arbitrary bytes.
//
// The value it returns is quoted verbatim into a refusal that tells the
// operator their database "states MVStore format %s" — a sentence about
// the artifact, read on the drill host from a file nothing has vetted.
// The header is text and the reader is a string scan, so what it finds
// after the key is whatever the file put there: the field has to be the
// number the message claims it is, or the refusal is the artifact
// writing into the operator's terminal.
func FuzzStorageFormat(f *testing.F) {
	f.Add([]byte(mvStoreMagic + "\nformat:3,blockSize:4096"))
	f.Add([]byte(mvStoreMagic + "\nformat:1,"))
	f.Add([]byte(mvStoreMagic + "\nformat:"))
	f.Add([]byte(mvStoreMagic + "\nformat:\x1b[31mred\x07,"))
	f.Add([]byte("no header here"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, head []byte) {
		format := storageFormat(head)
		if format == "" {
			return
		}
		for _, r := range format {
			if r < '0' || r > '9' {
				t.Fatalf("storageFormat = %q, which is not a format number — "+
					"it is quoted verbatim into the refusal the operator reads", format)
			}
		}
	})
}
