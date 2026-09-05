package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
)

// sniff.go reads what an .asb file's own bytes state about it. Every rule
// built on this reader fires on positive evidence only — bytes it does not
// recognise stay silent, and the authority on whether the artifact
// restores is asrestore inside the sandbox (ops.go).

const (
	// headMax bounds every head read. Everything read here lives in the
	// first three lines, and a namespace name is at most 31 bytes.
	headMax = 4096

	// versionPrefix opens every file asbackup writes. Measured on
	// Community Edition 8.1.2.4 and 7.2.0.21, both write "Version 3.1";
	// the minor is not pinned because the format is read by asrestore,
	// not by this adapter.
	versionPrefix = "Version 3."
	// namespacePrefix introduces the namespace the backup was taken from,
	// on the second line of every file.
	namespacePrefix = "# namespace "
	// firstFileMarker is on the third line of exactly one file in a
	// backup — the one asbackup wrote first. A directory backup's other
	// files carry the two lines above and then records (measured with
	// --file-limit against a six-file backup), so this marker is what
	// tells a whole backup from a fragment of one.
	firstFileMarker = "# first-file"
)

// asbHeader is what the first lines of an .asb file state.
type asbHeader struct {
	namespace string
	firstFile bool
}

// gzipMagic is the two-byte gzip header. Compressing a backup is a common
// job habit, and handing the compressed bytes to asrestore would end in a
// parse error about line 1 — refusing by name is kinder.
var gzipMagic = []byte{0x1f, 0x8b}

func isGzip(head []byte) bool { return bytes.HasPrefix(head, gzipMagic) }

// parseHeader reads an .asb file's header lines. ok is false for anything
// that does not open the way asbackup opens its files.
func parseHeader(head []byte) (h asbHeader, ok bool) {
	lines := strings.SplitN(string(head), "\n", 4)
	if len(lines) < 2 || !strings.HasPrefix(lines[0], versionPrefix) {
		return asbHeader{}, false
	}
	if !strings.HasPrefix(lines[1], namespacePrefix) {
		return asbHeader{}, false
	}
	h.namespace = strings.TrimSpace(strings.TrimPrefix(lines[1], namespacePrefix))
	if h.namespace == "" {
		return asbHeader{}, false
	}
	h.firstFile = len(lines) > 2 && strings.TrimSpace(lines[2]) == firstFileMarker
	return h, true
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
