package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"testing"
)

// gzipOf compresses one payload, for seeding the readers with the stored
// form they must handle.
func gzipOf(t testing.TB, payload []byte) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := gzip.NewWriter(buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FuzzReadHeadFrom drives the dump head reader over arbitrary bytes.
//
// It runs on the drill host over a file nothing has vetted, and it
// decompresses — the one input where a few kilobytes can ask for
// gigabytes. What it hands back has to stay a head whatever the artifact
// claims to expand to, because that bound is the whole reason a
// compressed backup can be classified without inflating it.
func FuzzReadHeadFrom(f *testing.F) {
	f.Add([]byte("-- MySQL dump 10.13  Distrib 8.4.6\n"))
	f.Add(gzipOf(f, []byte("-- MySQL dump 10.13  Distrib 8.4.6\n")))
	f.Add(gzipOf(f, bytes.Repeat([]byte("A"), 1<<20)))
	f.Add([]byte("\x1f\x8b not actually gzip"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, artifact []byte) {
		head, err := readHeadFrom(bytes.NewReader(artifact))
		if err != nil {
			if head != "" {
				t.Fatalf("read reported %v and still returned %d bytes", err, len(head))
			}
			return
		}
		if len(head) > dumpHeadBytes {
			t.Fatalf("head is %d bytes, over the %d this read is bounded to — "+
				"a compressed artifact chooses how much that is", len(head), dumpHeadBytes)
		}
	})
}

// FuzzInflateTailFrom drives the trailer reader over arbitrary bytes.
//
// This one inflates the whole member: a dump's completion marker is its
// last line, so there is no bound on what it reads, only on what it
// keeps. That bound is what makes inflating an operator's largest backup
// safe on the drill host — and what an artifact built to expand without
// limit is aimed at.
func FuzzInflateTailFrom(f *testing.F) {
	f.Add(gzipOf(f, []byte("-- Dump completed on 2026-08-14 14:37:45\n")))
	f.Add(gzipOf(f, bytes.Repeat([]byte("A"), 4<<20)))
	f.Add(gzipOf(f, nil))
	f.Add([]byte("not compressed at all"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, artifact []byte) {
		tail, err := inflateTailFrom(context.Background(), bytes.NewReader(artifact))
		if err != nil {
			if tail != "" {
				t.Fatalf("inflate reported %v and still returned %d bytes", err, len(tail))
			}
			return
		}
		if len(tail) > dumpTrailerBytes {
			t.Fatalf("tail is %d bytes, over the %d this read keeps — a stream of any size must "+
				"cost the drill host the same", len(tail), dumpTrailerBytes)
		}
	})
}
