package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
)

// storage.go decides what an artifact is before anything acts on it: which
// pg_dump output format wrote it, and whether it is stored compressed.
//
// Both answers come from the bytes, never from the file name. Renaming a
// backup must not change what a drill does, and a mislabeled but otherwise
// valid backup must not fail one; the mongodb adapter settled that rule for
// compression first, and format is the same kind of claim.
//
// Reading a dump as stored is what keeps the evidence record meaningful.
// Decompressing an artifact outside Probavi to make a drill possible would
// break the property backup.checksum exists for: the checksum would cover a
// temporary file nobody keeps, and the link between the record and the
// retained backup would rest on whatever script produced that file. So the
// adapter takes the artifact as it is stored and decompresses where the
// bytes are consumed.
//
// Only gzip is recognised. It is what dump pipelines in the field produce,
// and every recognised format is one this adapter must keep working across
// every engine image it is pointed at — so adding one is a deliberate act,
// not a convenience.

// gzipMagic opens every gzip member (RFC 1952 §2.3.1).
var gzipMagic = [2]byte{0x1f, 0x8b}

// tarMagic sits at a fixed offset in every POSIX tar header. pg_dump's tar
// output (-Ft) is not a supported source, and recognising it is what turns
// a baffling client failure deep in a restore into one honest sentence.
const (
	tarMagic       = "ustar"
	tarMagicOffset = 257
)

const (
	// dumpHeadBytes is how much of a dump's logical content is read to
	// classify and date it. Everything both answers need sits in the
	// leading comment block or the archive header, so the read is bounded
	// whatever the artifact's size — which is what lets a directory source
	// rank compressed candidates without inflating them whole.
	dumpHeadBytes = 8192
	// inflateBufferBytes is the read buffer the decompressor pulls through.
	inflateBufferBytes = 64 << 10
)

// dumpStorage is what an artifact turned out to be.
type dumpStorage struct {
	// plain distinguishes pg_dump's text output from a custom-format
	// archive. The two are restored by different clients — psql replays a
	// script, pg_restore reads an archive — and neither accepts the other.
	plain bool
	// compressed reports whether the artifact is stored gzip-compressed.
	compressed bool
}

// readDumpHead returns the leading bytes of a dump's logical content —
// decompressing on the way if the artifact is stored compressed — together
// with what the artifact turned out to be.
func readDumpHead(path string) ([]byte, dumpStorage, *protoError) {
	f, err := os.Open(path)
	if err != nil {
		return nil, dumpStorage{}, protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	head, storage, perr := readHeadFrom(f)
	if cerr := f.Close(); cerr != nil && perr == nil {
		return nil, dumpStorage{}, protoErr("source_unreadable", false, "read backup source: %v", cerr)
	}
	if perr != nil {
		return nil, dumpStorage{}, perr
	}
	return head, storage, nil
}

func readHeadFrom(f io.Reader) ([]byte, dumpStorage, *protoError) {
	buf := bufio.NewReaderSize(f, inflateBufferBytes)
	// A file shorter than the magic simply is not compressed, which Peek
	// reports as EOF; anything else means the artifact cannot be read.
	magic, err := buf.Peek(len(gzipMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, dumpStorage{}, protoErr("source_unreadable", false, "read backup source: %v", err)
	}
	storage := dumpStorage{compressed: len(magic) == len(gzipMagic) && [2]byte(magic) == gzipMagic}

	var (
		r  io.Reader = buf
		zr *gzip.Reader
	)
	if storage.compressed {
		if zr, err = gzip.NewReader(buf); err != nil {
			return nil, storage, protoErr("source_corrupt", false,
				"backup source is gzip-compressed but does not decompress: %v", err)
		}
		r = zr
	}

	head := make([]byte, dumpHeadBytes)
	n, rerr := io.ReadFull(r, head)
	if zr != nil {
		if cerr := zr.Close(); cerr != nil && rerr == nil {
			rerr = cerr
		}
	}
	// A dump shorter than the head is normal; a stream that ends early is
	// left to the restore to refuse, which it does with the client's own
	// diagnosis rather than this function's guess.
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return nil, storage, protoErr("source_unreadable", false, "read backup source: %v", rerr)
	}
	head = head[:n]

	if perr := refuseTar(head); perr != nil {
		return nil, storage, perr
	}
	storage.plain = !bytes.HasPrefix(head, []byte(pgdumpMagic))
	return head, storage, nil
}

// refuseTar names the one format that would otherwise be mistaken for a
// plain-SQL script and handed to psql, which would report a syntax error in
// binary data and leave an operator no wiser.
func refuseTar(head []byte) *protoError {
	if len(head) < tarMagicOffset+len(tarMagic) {
		return nil
	}
	if !bytes.HasPrefix(head[tarMagicOffset:], []byte(tarMagic)) {
		return nil
	}
	return protoErr("unsupported_source", false,
		"backup source is a tar archive; this adapter restores pg_dump custom-format (-Fc) "+
			"and plain-SQL (-Fp) dumps, either of them optionally gzip-compressed")
}
