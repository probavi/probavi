package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
)

// sniff.go reads what an artifact's own bytes state about it. Every rule
// built on these readers fires on positive evidence only — bytes this file
// does not recognise stay silent, and the authority on whether the
// artifact restores is CouchDB inside the sandbox (ops.go).

const (
	// backupToolName is what couchbackup writes into the first line of
	// every file it produces (measured on 2.11.19). The line is a JSON
	// object naming the tool, its version and the backup mode; the lines
	// after it are the document batches. Nothing else this adapter
	// restores opens that way, which is what makes it a signature.
	backupToolName = `"@cloudant/couchbackup"`

	// headMax bounds every head read. The signature decides within the
	// first line, and a couchbackup header line is far shorter than this.
	headMax = 4096

	// maxLineBytesScan bounds the completeness scan's per-line read. A
	// batch line is one JSON array of documents and can be large, so this
	// is generous; a file whose first line alone exceeds it is not a
	// couchbackup file.
	maxLineBytesScan = 64 * 1024 * 1024
)

// gzipMagic is the two-byte gzip header. Compressing a backup is a common
// job habit, and handing the compressed bytes to the restore loop would
// end in a bewildering refusal — refusing by name is kinder.
var gzipMagic = []byte{0x1f, 0x8b}

func isGzip(head []byte) bool { return bytes.HasPrefix(head, gzipMagic) }

// hasBackupSignature reports whether the file opens with couchbackup's own
// header line.
func hasBackupSignature(head []byte) bool {
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) &&
		bytes.Contains(line, []byte(backupToolName))
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

// batchLines counts the document batches in a couchbackup file and reports
// whether every one of them is a complete line.
//
// The count is what the restore loop is held to afterwards: couchbackup
// writes one JSON array per line and nothing at the end, so a file
// truncated between two lines is indistinguishable from a shorter backup
// (measured) — but a file truncated *within* a line ends without a
// newline, and that this adapter can see before a byte moves.
func batchLines(path string) (batches int, tornTail bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	br := bufio.NewReaderSize(f, 1<<20)
	first := true
	for {
		line, rerr := readCappedLine(br, maxLineBytesScan)
		counts, torn := classifyLine(line)
		if torn {
			return batches, true, nil
		}
		if counts && !first {
			batches++
		}
		if len(line) > 0 {
			first = false
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return batches, false, nil
			}
			return 0, false, rerr
		}
	}
}

// classifyLine says what one line read from a couchbackup file is: a
// complete line that counts towards the batch total, or the torn tail of a
// file cut inside a batch. An empty line counts as neither.
func classifyLine(line []byte) (counts, torn bool) {
	if len(line) == 0 {
		return false, false
	}
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return false, false
	}
	if !bytes.HasSuffix(line, []byte("\n")) {
		return false, true
	}
	return true, false
}

// readCappedLine reads one line including its terminator, refusing to
// buffer more than max bytes.
func readCappedLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, errors.New("a line exceeds the size a couchbackup batch may have")
		}
		line = append(line, chunk...)
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		return line, rerr
	}
}
