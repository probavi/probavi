package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gzipBytes compresses body the way a dump pipeline does.
func gzipBytes(t *testing.T, body string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := gzip.NewWriter(buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close compressor: %v", err)
	}
	return buf.Bytes()
}

// writeGzipDump writes a dump stored the way `mysqldump | gzip -c` stores
// one: compressed bytes, and a name that says so.
func writeGzipDump(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, gzipBytes(t, body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestSniffCompressed(t *testing.T) {
	tests := []struct {
		name  string
		bytes []byte
		want  bool
	}{
		{"a plain dump", []byte("-- MySQL dump 10.13\n"), false},
		{"a gzip member", gzipBytes(t, "-- MySQL dump 10.13\n"), true},
		{"an empty file", nil, false},
		{"a file shorter than the magic", []byte{0x1f}, false},
		{"half the magic followed by text", []byte("\x1fnot gzip"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "artifact")
			if err := os.WriteFile(path, tt.bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			got, perr := sniffCompressed(path)
			if perr != nil {
				t.Fatalf("sniffCompressed: %+v", perr)
			}
			if got != tt.want {
				t.Errorf("compressed = %v, want %v — the bytes decide, never the name", got, tt.want)
			}
		})
	}

	t.Run("an unreadable artifact is an error, not a verdict", func(t *testing.T) {
		_, perr := sniffCompressed(filepath.Join(t.TempDir(), "gone"))
		if perr == nil || perr.Code != "source_unreadable" {
			t.Errorf("perr = %+v, want source_unreadable — the caller is about to restore this", perr)
		}
	})

	t.Run("the name never decides", func(t *testing.T) {
		dir := t.TempDir()
		lying := filepath.Join(dir, "orders.sql.gz")
		if err := os.WriteFile(lying, []byte("SELECT 1;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _ := sniffCompressed(lying); got {
			t.Error("a plain dump named .gz was called compressed")
		}
		unnamed := writeGzipDump(t, dir, "orders.sql", "SELECT 1;\n")
		if got, _ := sniffCompressed(unnamed); !got {
			t.Error("a compressed dump without the extension was called plain")
		}
	})
}

// datedBody is a dump that signs itself off the way mysqldump does.
const datedBody = "INSERT INTO `orders` VALUES (1);\n-- Dump completed on 2026-08-09 21:08:17\n"

// TestReadDumpTailIsIndifferentToStorage is what lets the two storage
// forms rank against each other: both are dated from the same sentence.
func TestReadDumpTailIsIndifferentToStorage(t *testing.T) {
	dir := t.TempDir()
	plain := writeDumpAs(t, dir, "plain.sql", "2026-08-09 21:08:17",
		time.Date(2026, 8, 9, 21, 8, 17, 0, time.UTC))
	compressed := writeGzipDump(t, dir, "compressed.sql.gz", datedBody)

	plainTail, ok := readDumpTail(context.Background(), plain)
	if !ok {
		t.Fatal("no tail read from the plain dump")
	}
	gzTail, ok := readDumpTail(context.Background(), compressed)
	if !ok {
		t.Fatal("no tail read from the compressed dump")
	}
	plainClock, _ := lastDumpClock(plainTail)
	gzClock, _ := lastDumpClock(gzTail)
	if plainClock != gzClock || gzClock != "2026-08-09 21:08:17" {
		t.Errorf("plain %q vs compressed %q — the same dump must date the same either way",
			plainClock, gzClock)
	}
}

// TestReadDumpTailSpansTheWholeMember: the dump is larger than the read
// buffer and than any single copy, so finding the trailer proves the tail
// is carried across reads rather than taken from one of them.
func TestReadDumpTailSpansTheWholeMember(t *testing.T) {
	big := strings.Repeat("INSERT INTO `orders` VALUES (1);\n", 200_000)
	path := writeGzipDump(t, t.TempDir(), "big.sql.gz", big+datedBody)
	tail, ok := readDumpTail(context.Background(), path)
	if !ok {
		t.Fatal("no tail read")
	}
	if clock, found := lastDumpClock(tail); !found || clock != "2026-08-09 21:08:17" {
		t.Errorf("clock = %q, found = %v — the trailer must survive a multi-read stream", clock, found)
	}
	if len(tail) > dumpTrailerBytes {
		t.Errorf("tail is %d bytes, want at most %d — the scan must stay bounded", len(tail), dumpTrailerBytes)
	}
}

// TestReadDumpTailRefusesWhatItCannotRead: every one of these could have
// produced a tail of sorts, and each would have dated a backup from
// something that is not a whole backup.
func TestReadDumpTailRefusesWhatItCannotRead(t *testing.T) {
	const body = datedBody

	t.Run("a truncated member dates nothing", func(t *testing.T) {
		// Half an archive can decompress to a prefix that still holds an
		// earlier trailer; dating a backup from it would claim a backup that
		// was never completely stored.
		whole := gzipBytes(t, body+"-- Dump completed on 2026-08-10 21:08:17\n"+
			strings.Repeat("INSERT INTO `orders` VALUES (2);\n", 5_000))
		path := filepath.Join(t.TempDir(), "truncated.sql.gz")
		if err := os.WriteFile(path, whole[:len(whole)/2], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readDumpTail(context.Background(), path); ok {
			t.Error("a truncated member yielded a tail — half an archive must date nothing")
		}
	})

	t.Run("bytes that only start like gzip date nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lying.sql.gz")
		if err := os.WriteFile(path, append([]byte{0x1f, 0x8b}, body...), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readDumpTail(context.Background(), path); ok {
			t.Error("a bogus gzip header yielded a tail")
		}
	})

	t.Run("cancellation ends the scan", func(t *testing.T) {
		path := writeGzipDump(t, t.TempDir(), "orders.sql.gz", body)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := readDumpTail(ctx, path); ok {
			t.Error("a cancelled scan still returned a tail — a compressed artifact expands unboundedly")
		}
	})

	t.Run("a missing artifact dates nothing", func(t *testing.T) {
		if _, ok := readDumpTail(context.Background(), filepath.Join(t.TempDir(), "gone")); ok {
			t.Error("a missing file yielded a tail")
		}
	})
}

// TestTailBufferKeepsTheLastBytes is the property the scan rests on: no
// matter how the stream is chopped into writes, what remains is exactly
// its last limit bytes.
func TestTailBufferKeepsTheLastBytes(t *testing.T) {
	stream := []byte(strings.Repeat("abcdefghij", 40))
	splits := [][]int{
		{400},
		{1, 399},
		{399, 1},
		{200, 200},
		{10, 90, 1, 299},
		{50, 50, 50, 50, 50, 50, 50, 50},
	}
	for _, limit := range []int{1, 7, 64, 399, 400, 401, 4096} {
		for _, split := range splits {
			tail := &tailBuffer{limit: limit}
			offset := 0
			for _, n := range split {
				if _, err := tail.Write(stream[offset : offset+n]); err != nil {
					t.Fatalf("write: %v", err)
				}
				offset += n
			}
			want := stream
			if len(want) > limit {
				want = want[len(want)-limit:]
			}
			if !bytes.Equal(tail.buf, want) {
				t.Fatalf("limit %d, split %v: tail = %q, want %q", limit, split, tail.buf, want)
			}
		}
	}
}
