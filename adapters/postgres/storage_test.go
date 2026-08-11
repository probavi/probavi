package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plainDumpBody is what pg_dump -Fp writes around the data, kept to the
// parts this adapter reads: the leading comment block that can date it and
// the closing line that says the dump finished.
const plainDumpBody = "--\n-- PostgreSQL database dump\n--\n\n" +
	"-- Dumped from database version 16.14\n" +
	"-- Started on 2026-08-09 21:26:45 JST\n\n" +
	"CREATE TABLE public.orders (id integer);\n\n" +
	"--\n-- PostgreSQL database dump complete\n--\n\n"

// undatedPlainDumpBody is the same dump taken without --verbose, which is
// the ordinary case: nothing in it records when it was taken.
const undatedPlainDumpBody = "--\n-- PostgreSQL database dump\n--\n\n" +
	"-- Dumped from database version 16.14\n\n" +
	"CREATE TABLE public.orders (id integer);\n\n" +
	"--\n-- PostgreSQL database dump complete\n--\n\n"

func writePlain(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// writeGzip stores a body the way a dump pipeline stores one.
func writeGzip(t *testing.T, dir, name, body string) string {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := gzip.NewWriter(buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatalf("compress %s: %v", name, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return writePlain(t, dir, name, buf.String())
}

// archiveBytes builds a custom-format archive out of a real header, padded
// past the length the header parser needs.
func archiveBytes() string {
	return string(archiveHeaders[2].head) + strings.Repeat("\x00", 128)
}

// TestReadDumpHeadClassifiesWhatItIsGiven pins the sniff both restore paths
// hang off. The name is never consulted: every fixture here is called
// "backup", because renaming a backup must not change what a drill does.
func TestReadDumpHeadClassifiesWhatItIsGiven(t *testing.T) {
	tests := []struct {
		name string
		body string
		gzip bool
		want dumpStorage
	}{
		{"a custom-format archive", archiveBytes(), false, dumpStorage{}},
		{"a custom-format archive stored compressed", archiveBytes(), true,
			dumpStorage{compressed: true}},
		{"a plain-SQL dump", plainDumpBody, false, dumpStorage{plain: true}},
		{"a plain-SQL dump stored compressed", plainDumpBody, true,
			dumpStorage{plain: true, compressed: true}},
		// Nothing forbids an empty or tiny file in a backup directory; it is
		// simply not compressed and not an archive, and the restore says so.
		{"a file too short to carry any magic", "P", false, dumpStorage{plain: true}},
		{"an empty file", "", false, dumpStorage{plain: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePlain(t, dir, "backup", tt.body)
			if tt.gzip {
				path = writeGzip(t, dir, "backup", tt.body)
			}
			head, got, perr := readDumpHead(path)
			if perr != nil {
				t.Fatalf("readDumpHead: %+v", perr)
			}
			if got != tt.want {
				t.Errorf("storage = %+v, want %+v", got, tt.want)
			}
			if !strings.HasPrefix(tt.body, string(head)) {
				t.Errorf("head = %q, want the artifact's own leading bytes", head)
			}
		})
	}
}

// TestReadDumpHeadSeesThroughTheDecompressor is the property that keeps a
// compressed dump rankable: what dates it sits in the head, so only a few
// kilobytes have to be inflated however large the artifact is.
func TestReadDumpHeadSeesThroughTheDecompressor(t *testing.T) {
	// A dump whose payload dwarfs the head, so a head that came back right
	// cannot have been the whole stream.
	body := plainDumpBody + strings.Repeat("INSERT INTO public.orders VALUES (1);\n", 5000)
	head, storage, perr := readDumpHead(writeGzip(t, t.TempDir(), "backup", body))
	if perr != nil {
		t.Fatalf("readDumpHead: %+v", perr)
	}
	if !storage.compressed || !storage.plain {
		t.Fatalf("storage = %+v, want a compressed plain-SQL dump", storage)
	}
	if len(head) != dumpHeadBytes {
		t.Errorf("head = %d bytes, want the bounded %d", len(head), dumpHeadBytes)
	}
	if !strings.Contains(string(head), plainStartedPrefix) {
		t.Error("head does not carry the line that dates the dump")
	}
}

func TestReadDumpHeadRefusals(t *testing.T) {
	dir := t.TempDir()
	// The gzip magic over a header no decompressor accepts (compression
	// method 9 is not one RFC 1952 defines): the artifact claims to be
	// compressed and cannot be read as such, which no restore will fix.
	brokenGzip := writePlain(t, dir, "broken.gz", "\x1f\x8b\x09\x00\x00\x00\x00\x00\x00\x03rest")
	// A tar archive: pg_dump writes one under -Ft, and handing it to psql
	// would report a syntax error in binary data.
	tarHeader := make([]byte, 512)
	copy(tarHeader, "toc.dat")
	copy(tarHeader[tarMagicOffset:], tarMagic)

	tests := []struct {
		name     string
		path     string
		wantCode string
		wantIn   string
	}{
		{"a file that is not there", filepath.Join(dir, "gone"), "source_unreadable", "open backup source"},
		{"a gzip member that does not decompress", brokenGzip, "source_corrupt", "does not decompress"},
		{"a tar archive", writePlain(t, dir, "backup.tar", string(tarHeader)),
			"unsupported_source", "tar archive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, perr := readDumpHead(tt.path)
			if perr == nil {
				t.Fatal("readDumpHead accepted it")
			}
			if perr.Code != tt.wantCode || !strings.Contains(perr.Message, tt.wantIn) {
				t.Errorf("error = %s/%q, want %s mentioning %q",
					perr.Code, perr.Message, tt.wantCode, tt.wantIn)
			}
		})
	}
}

// TestPlainDumpClock covers the half of dating that a plain-SQL dump can
// answer, and the much commoner half it cannot.
func TestPlainDumpClock(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  string
		dated bool
	}{
		{"a dump taken with --verbose", plainDumpBody, "2026-08-09 21:26:45", true},
		{"a dump taken without it says nothing", undatedPlainDumpBody, "", false},
		// The zone abbreviation is deliberately unread: CST is three
		// different zones, so believing one would put a guess into a record.
		{"the abbreviation does not move the clock",
			strings.Replace(plainDumpBody, "JST", "CST", 1), "2026-08-09 21:26:45", true},
		{"a sentence without a date is not a date",
			strings.Replace(plainDumpBody, "2026-08-09 21:26:45 JST", "", 1), "", false},
		{"an unparseable clock is refused rather than guessed",
			strings.Replace(plainDumpBody, "2026-08-09 21:26:45", "yesterday afternoon", 1), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dated := plainDumpClock([]byte(tt.body))
			if dated != tt.dated {
				t.Fatalf("dated = %v, want %v", dated, tt.dated)
			}
			if dated && got.Format(plainClockLayout) != tt.want {
				t.Errorf("clock = %s, want %s", got.Format(plainClockLayout), tt.want)
			}
		})
	}
}

// TestDumpClockReadsBothFormatsOnOneScale is what lets a directory holding
// an archive and a plain dump rank them against each other: both values are
// the backup host's wall clock at the moment the dump began.
func TestDumpClockReadsBothFormatsOnOneScale(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		name string
		path string
	}{
		{"custom-format archive", writePlain(t, dir, "a.dump", archiveBytes())},
		{"plain-SQL dump", writePlain(t, dir, "b.sql", plainDumpBody)},
		{"plain-SQL dump stored compressed", writeGzip(t, dir, "c.sql.gz", plainDumpBody)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			head, storage, perr := readDumpHead(tt.path)
			if perr != nil {
				t.Fatalf("readDumpHead: %+v", perr)
			}
			clock, ok := dumpClock(head, storage)
			if !ok {
				t.Fatal("dumpClock could not date it")
			}
			if got := clock.Format(plainClockLayout); got != "2026-08-09 21:26:45" {
				t.Errorf("clock = %s, want the same wall clock both formats record", got)
			}
		})
	}
}
