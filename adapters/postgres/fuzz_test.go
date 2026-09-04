package main

import (
	"bytes"
	"compress/gzip"
	"testing"
	"time"
)

// gzipOf compresses one payload, for seeding the reader with the stored
// form it must handle.
func gzipOf(t testing.TB, payload string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := gzip.NewWriter(buf)
	if _, err := zw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FuzzReadDumpHead drives the artifact classifier over arbitrary bytes.
//
// It runs on the drill host, on a file the operator points at and nothing
// has vetted, and it decompresses: a gzip stream is the one input where a
// few kilobytes can ask for gigabytes. The bound on what it hands back is
// therefore the property that matters most — the head is read to classify
// and date the dump, and it must stay a head whatever the artifact
// claims.
func FuzzReadDumpHead(f *testing.F) {
	f.Add([]byte("PGDMP\x01\x0f\x00\x08\x08\x01"))
	f.Add([]byte("-- Started on 2026-03-01 12:00:00 CET\nCREATE TABLE t();"))
	f.Add(gzipOf(f, "PGDMP\x01\x0f\x00\x08\x08\x01"))
	f.Add(gzipOf(f, "-- Started on 2026-03-01 12:00:00 CET"))
	f.Add([]byte("\x1f\x8b not actually gzip"))
	f.Add(append(bytes.Repeat([]byte{0}, tarMagicOffset), []byte(tarMagic+"\x0000")...))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, artifact []byte) {
		head, storage, perr := readHeadFrom(bytes.NewReader(artifact))

		if perr != nil {
			if head != nil {
				t.Fatalf("refusal %q still returned %d bytes of head", perr.Code, len(head))
			}
			return
		}
		if len(head) > dumpHeadBytes {
			t.Fatalf("head is %d bytes, over the %d this read is bounded to — "+
				"a compressed artifact chooses how much that is", len(head), dumpHeadBytes)
		}
		if plain := !bytes.HasPrefix(head, []byte(pgdumpMagic)); plain != storage.plain {
			t.Fatalf("storage.plain = %v for a head beginning %q", storage.plain, headPrefix(head))
		}
		if compressed := bytes.HasPrefix(artifact, gzipMagic[:]); compressed != storage.compressed {
			t.Fatalf("storage.compressed = %v for an artifact beginning %q",
				storage.compressed, headPrefix(artifact))
		}
	})
}

func headPrefix(b []byte) []byte {
	if len(b) > 8 {
		return b[:8]
	}
	return b
}

// FuzzDumpClock drives the dump dating over arbitrary heads, in both
// storage forms.
//
// The custom-format path walks pg_dump's own integer encoding at computed
// offsets — a sign byte and intSize magnitude bytes per field, with the
// start of the timestamp depending on the archive version — which is the
// shape that reads past the end of a buffer when the header is not what it
// claims. The plain path scans text.
//
// What both must guarantee is that a false answer is never a confident
// one. This clock ranks the candidates in a backup directory
// (newestBackupIn) and, with a declared zone, becomes the record's
// backup.created_at: a clock read out of the wrong bytes would pick the
// wrong artifact to restore and sign a date for it. So a success has to
// mean a date a calendar produces — which is what the custom-format path's
// own plausibility gate is for.
func FuzzDumpClock(f *testing.F) {
	header := func(minor byte, intSize byte, fields ...int32) []byte {
		h := make([]byte, 0, pgdumpHeaderBytes)
		h = append(h, pgdumpMagic...)
		h = append(h, 1, minor, 0, intSize, 8, pgdumpFormatCustom, 0)
		for _, v := range fields {
			h = append(h, 0, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		}
		for len(h) < pgdumpHeaderBytes {
			h = append(h, 0)
		}
		return h
	}
	f.Add(header(15, 4, 45, 37, 14, 14, 7, 126), false)
	f.Add(header(14, 4, 45, 37, 14, 14, 7, 126), false)
	f.Add(header(15, 8), false)
	f.Add([]byte("PGDMP"), false)
	f.Add([]byte("-- Started on 2026-08-14 14:37:45 CEST\n"), true)
	f.Add([]byte("-- Started on 9999-12-31 23:59:59 UTC\n"), true)
	f.Add([]byte("-- Started on \n"), true)
	f.Add([]byte(""), true)

	f.Fuzz(func(t *testing.T, head []byte, plain bool) {
		clock, ok := dumpClock(head, dumpStorage{plain: plain})

		if !ok {
			if !clock.IsZero() {
				t.Fatalf("dumpClock reported no clock and returned %v", clock)
			}
			return
		}
		if loc := clock.Location(); loc != time.UTC {
			t.Fatalf("clock carries location %v — it is a wall clock the config gives a zone to", loc)
		}
		if clock.Nanosecond() != 0 {
			t.Fatalf("clock %v has precision neither form records", clock)
		}
		fields := headerTime{
			second: clock.Second(), minute: clock.Minute(), hour: clock.Hour(),
			day: clock.Day(), month: int(clock.Month()), year: clock.Year(),
		}
		if !plausible(fields) {
			t.Fatalf("clock %v is not a date a backup was taken at, yet it ranks the "+
				"candidates in a backup directory and can reach a signed created_at", clock)
		}
	})
}
