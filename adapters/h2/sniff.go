package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
)

// sniff.go reads what an artifact's own bytes state about it. Every rule
// built on these readers fires on positive evidence only — bytes this file
// does not recognise stay silent, and the authority on whether the
// artifact restores is H2 inside the sandbox (ops.go).

const (
	// mvStoreMagic opens every MVStore file, which is what H2 2.x writes
	// as <database>.mv.db. The header is plain text: a comma-separated
	// key:value list in the file's first block, of which this is the
	// opening key (measured on 1.4.200, 2.2.224, 2.3.232 and 2.4.240 —
	// the same across all four, which is why it identifies the file
	// without dating it).
	mvStoreMagic = "H:2,"

	// storageFormatKey is the header field that does move between major
	// lines: format:1 is what H2 1.4 writes, format:3 what every 2.x
	// version writes (measured). It is the only version coordinate an
	// artifact carries, and it is what this adapter fences on.
	storageFormatKey = "format:"

	// supportedStorageFormat is the MVStore format the verified engine
	// versions read. A file declaring anything else is refused by name
	// rather than left to the engine's "Unsupported database file version
	// or invalid file header", which names neither side.
	supportedStorageFormat = "3"

	// headMax bounds every head read. The MVStore header lives in the
	// file's first 4096-byte block, and the fields this file reads sit at
	// its very start.
	headMax = 256
)

// gzipMagic is the two-byte gzip header. Compressing a backup is a common
// job habit, and handing the compressed bytes to H2 would end in a
// bewildering refusal minutes later — refusing by name is kinder.
var gzipMagic = []byte{0x1f, 0x8b}

// zipMagic opens a zip local file header, which is what BACKUP TO writes.
var zipMagic = []byte("PK\x03\x04")

func hasMVStoreMagic(head []byte) bool {
	return bytes.HasPrefix(head, []byte(mvStoreMagic))
}

func isZip(head []byte) bool {
	return bytes.HasPrefix(head, zipMagic)
}

func isGzip(head []byte) bool {
	return bytes.HasPrefix(head, gzipMagic)
}

// formatNumber is what the header's format field may be: the version
// number this reader claims to have found. Anything else is not one, and
// is dropped rather than quoted — the refusal built from this value tells
// the operator their file "states MVStore format %s", and the file is
// unvetted input at that point.
var formatNumber = regexp.MustCompile(`^\d+$`)

// storageFormat reads the MVStore header's format field, or "" when the
// head does not carry one. The header is text, so this is a string scan
// rather than a struct: H2 writes the fields in a fixed order but the set
// grows between versions, and a reader that insisted on the whole shape
// would refuse files the engine reads perfectly well.
func storageFormat(head []byte) string {
	i := bytes.Index(head, []byte(storageFormatKey))
	if i < 0 {
		return ""
	}
	rest := string(head[i+len(storageFormatKey):])
	if j := strings.IndexByte(rest, ','); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	if !formatNumber.MatchString(rest) {
		return ""
	}
	return rest
}

// zipHoldsDatabase reports whether the archive carries an MVStore file,
// which is the one thing BACKUP TO ever writes into it (measured: exactly
// one entry, <database>.mv.db). The check opens the central directory, so
// it also proves the archive is whole — a truncated zip has none.
func zipHoldsDatabase(path string) (bool, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return false, err
	}
	holds := false
	for _, f := range r.File {
		if strings.HasSuffix(f.Name, mvStoreSuffix) {
			holds = true
			break
		}
	}
	if err := r.Close(); err != nil {
		return false, err
	}
	return holds, nil
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
