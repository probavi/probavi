package main

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// sniff.go reads what an artifact's own first and last bytes state about
// it. Every rule built on these readers fires on positive evidence only —
// bytes this file does not recognise stay silent, and the authority on
// whether the artifact restores is sqlite3 inside the sandbox (ops.go).

const (
	// sqliteMagic opens every SQLite database file: the sixteen bytes
	// "SQLite format 3\x00", unchanged since SQLite 3.0.0 and measured
	// across the verified images.
	sqliteMagic = "SQLite format 3\x00"

	// dumpSignature is the exact opening `sqlite3 .dump` writes before
	// the first schema statement (measured on 3.46.1 and 3.53.4). SQL
	// text that does not open this way is not treated as a .dump: the
	// gates that key on the signature skip rather than guess.
	dumpSignature = "PRAGMA foreign_keys=OFF;\nBEGIN TRANSACTION;\n"

	// headMax bounds every head read; both signatures decide within it.
	headMax = 64
	// tailMax bounds the completeness gate's tail read (lastNonEmptyLine).
	tailMax = 4096
)

// gzipMagic is the two-byte gzip header. Compressing a backup is a common
// job habit, and handing the compressed bytes to sqlite3 would end in a
// bewildering refusal minutes later — refusing by name is kinder.
var gzipMagic = []byte{0x1f, 0x8b}

func hasSQLiteMagic(head []byte) bool {
	return len(head) >= len(sqliteMagic) && string(head[:len(sqliteMagic)]) == sqliteMagic
}

func hasDumpSignature(head []byte) bool {
	return bytes.HasPrefix(head, []byte(dumpSignature))
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

// lastNonEmptyLine returns the last non-empty line within the file's final
// tailMax bytes. A closing line longer than the window comes back as a
// fragment, which is what the completeness gate needs: a fragment is never
// exactly the short trailer it looks for.
func lastNonEmptyLine(path string) (string, error) {
	tail, err := readTail(path, tailMax)
	if err != nil {
		return "", err
	}
	tail = bytes.TrimRight(tail, " \t\r\n")
	if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return string(bytes.TrimRight(tail, " \t\r")), nil
}

// readTail reads up to max bytes from the end of the file.
func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	tail, rerr := tailBytes(f, max)
	if cerr := f.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return nil, rerr
	}
	return tail, nil
}

func tailBytes(f *os.File, max int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := info.Size() - max
	if off < 0 {
		off = 0
	}
	tail := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(tail, off); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return tail, nil
}
