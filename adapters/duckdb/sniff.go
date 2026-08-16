package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"regexp"
)

// sniff.go reads what a DuckDB database file's own header states about it.
// Every rule built on this reader fires on positive evidence only — bytes
// it does not recognise stay silent, and the authority on whether the
// artifact restores is duckdb inside the sandbox (ops.go).
//
// The main header opens with an 8-byte block checksum, then the four
// magic bytes "DUCK" at offset 8, the storage format version as a 64-bit
// little-endian integer at offset 12, and — since the 1.x line — the
// version string of the library that wrote the file at offset 52
// (measured: a default 1.5.5 file reads DUCK, 64, "v1.5.5"; a file
// written with STORAGE_VERSION 'v1.5.0' reads 68). The header carries no
// wall clock of any kind.

const (
	// duckMagic sits at duckMagicOffset in every DuckDB database file.
	duckMagic       = "DUCK"
	duckMagicOffset = 8
	// storageVersionOffset is where the storage format version lives.
	storageVersionOffset = 12
	// libraryVersionOffset is where the writing library's version string
	// starts; NUL-terminated (measured).
	libraryVersionOffset = 52
	// headMax bounds every head read; every field above decides within it.
	headMax = 96
)

// gzipMagic is the two-byte gzip header. Compressing a backup is a common
// job habit, and handing the compressed bytes to duckdb would end in a
// bewildering refusal minutes later — refusing by name is kinder.
var gzipMagic = []byte{0x1f, 0x8b}

// versionShape accepts only version-shaped library strings, so header
// noise cannot reach a refusal message.
var versionShape = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// duckHeader is what the head of a DuckDB database file states about
// itself.
type duckHeader struct {
	// valid reports the DUCK magic — the artifact is DuckDB-shaped. It
	// says nothing about restorability.
	valid bool
	// storageVersion is the storage format version the file carries; the
	// engine refuses versions newer than it can read (measured), and the
	// refusal in ops.go names this number.
	storageVersion uint64
	// libraryVersion is the version of the library that wrote the file
	// ("v1.5.5"), "" when absent or not version-shaped.
	libraryVersion string
}

// parseDuckHeader reads the header fields out of a head slice; bytes it
// does not recognise yield the zero value, because metadata is a bonus,
// never worth an error of its own.
func parseDuckHeader(head []byte) duckHeader {
	h := duckHeader{}
	if !hasDuckMagic(head) {
		return h
	}
	h.valid = true
	if len(head) >= storageVersionOffset+8 {
		h.storageVersion = binary.LittleEndian.Uint64(head[storageVersionOffset:])
	}
	if len(head) > libraryVersionOffset {
		raw := head[libraryVersionOffset:]
		if i := bytes.IndexByte(raw, 0); i >= 0 {
			raw = raw[:i]
		}
		if versionShape.Match(raw) {
			h.libraryVersion = string(raw)
		}
	}
	return h
}

func hasDuckMagic(head []byte) bool {
	return len(head) >= duckMagicOffset+len(duckMagic) &&
		string(head[duckMagicOffset:duckMagicOffset+len(duckMagic)]) == duckMagic
}

func isGzip(head []byte) bool {
	return bytes.HasPrefix(head, gzipMagic)
}

// readHead reads up to max bytes from the start of the file.
func readHead(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	head := make([]byte, max)
	n, rerr := io.ReadFull(f, head)
	if errors.Is(rerr, io.ErrUnexpectedEOF) || errors.Is(rerr, io.EOF) {
		rerr = nil
	}
	if cerr := f.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return nil, rerr
	}
	return head[:n], nil
}
