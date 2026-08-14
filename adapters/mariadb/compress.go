package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
)

// compress.go accepts dumps that are stored compressed.
//
// Piping mysqldump through gzip is what dump pipelines do once the dumps
// grow, and the artifact the operator retains is then the compressed one.
// Decompressing it outside Probavi to make a drill possible would break
// the property the evidence record exists for: backup.checksum would cover
// a temporary file nobody keeps, and the link between the record and the
// stored backup would rest on whatever script produced that file. So the
// adapter takes the artifact as stored — the checksum covers the retained
// bytes, and the decompression happens where the bytes are consumed.
//
// Compression is recognised from the magic bytes, never from the file
// name: renaming a backup must not change what a drill does, and a
// mislabeled but otherwise valid backup must not fail one. The mongodb
// adapter settled that rule first.
//
// Only gzip is recognised. It is what the dump pipelines in the field
// produce, and every recognised format is one this adapter must keep
// working across every engine image it is pointed at — so adding one is a
// deliberate act, not a convenience.

// gzipMagic opens every gzip member (RFC 1952 §2.3.1).
var gzipMagic = [2]byte{0x1f, 0x8b}

// inflateBufferBytes is the read buffer the decompressor pulls through.
// Measured: 400 MiB of dump inflates in under a second at this size, and a
// directory source pays that per candidate (see newestBackupIn).
const inflateBufferBytes = 1 << 20

// sniffCompressed reports whether an artifact is gzip-compressed. A file
// shorter than the magic is simply not compressed; anything that cannot be
// read at all is an error, because the caller is about to restore it.
func sniffCompressed(path string) (bool, *protoError) {
	f, err := os.Open(path)
	if err != nil {
		return false, protoErr("source_unreadable", false, "open backup source: %v", err)
	}
	var magic [2]byte
	n, rerr := io.ReadFull(f, magic[:])
	if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
		rerr = nil
	}
	if cerr := f.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return false, protoErr("source_unreadable", false, "read backup source: %v", rerr)
	}
	return n == len(magic) && magic == gzipMagic, nil
}

// readDumpTail returns the last n bytes of a dump's SQL text, which is where
// mysqldump signs its output off (see timestamp.go). A stored-plain dump
// is seeked to its end; a compressed one has no other route to its own
// trailer than the whole stream, because a gzip member carries no index
// and its header records no usable date (measured: gzip zeroes the header
// timestamp whenever it compresses a pipe, which is exactly the
// `mysqldump | gzip -c` shape).
//
// The scan is therefore proportional to the artifact, and a directory
// source pays it once per candidate. That is deliberate: the alternative
// is ranking compressed backups by file modification time, which is the
// claim newestBackupIn exists to stop making.
func readDumpTail(ctx context.Context, path string) (string, bool) {
	compressed, perr := sniffCompressed(path)
	if perr != nil {
		return "", false
	}
	if !compressed {
		return readTail(path, dumpTrailerBytes)
	}
	return inflateTail(ctx, path)
}

// readDumpHead returns the leading bytes of a dump's SQL text, which is
// where mysqldump announces itself (see complete.go). Unlike the trailer,
// the head costs the same whatever the artifact's size: a compressed member
// is inflated only far enough to reach it.
func readDumpHead(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	head, rerr := readHeadFrom(f)
	if cerr := f.Close(); cerr != nil || rerr != nil {
		return "", false
	}
	return head, true
}

func readHeadFrom(f io.Reader) (string, error) {
	buf := bufio.NewReaderSize(f, inflateBufferBytes)
	magic, perr := buf.Peek(len(gzipMagic))
	// A file shorter than the magic is simply not compressed, which Peek
	// reports as EOF.
	if perr != nil && !errors.Is(perr, io.EOF) {
		return "", perr
	}
	var r io.Reader = buf
	if len(magic) == len(gzipMagic) && [2]byte(magic) == gzipMagic {
		zr, err := gzip.NewReader(buf)
		if err != nil {
			return "", err
		}
		defer func() { _ = zr.Close() }() //nolint:errcheck // a bounded head read closes nothing that can fail meaningfully
		r = zr
	}
	head := make([]byte, dumpHeadBytes)
	// A dump shorter than the head is normal, and so is a stream that ends
	// early: the restore refuses that one separately, and with a better
	// diagnosis than this function could give.
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	return string(head[:n]), nil
}

// inflateTail streams the whole member through the decompressor, keeping
// only its last n bytes. A member that does not decompress cleanly —
// truncated, or with a checksum its trailer disagrees with — yields no
// tail rather than the prefix that did decompress: half an archive cannot
// date a backup, and the restore refuses it separately.
func inflateTail(ctx context.Context, path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	tail, rerr := inflateTailFrom(ctx, f)
	if cerr := f.Close(); cerr != nil || rerr != nil {
		return "", false
	}
	return tail, true
}

func inflateTailFrom(ctx context.Context, r io.Reader) (string, error) {
	zr, err := gzip.NewReader(bufio.NewReaderSize(r, inflateBufferBytes))
	if err != nil {
		return "", err
	}
	// gzip.Reader spans concatenated members by default, so a file built by
	// appending one dump's archive to another reads as one stream — the
	// same shape the plain case handles, and the trailer rule (last match
	// wins) already covers it.
	tail := &tailBuffer{limit: dumpTrailerBytes}
	_, cerr := io.Copy(tail, &cancellable{ctx: ctx, r: zr})
	if err := zr.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", cerr
	}
	return string(tail.buf), nil
}

// tailBuffer keeps the last limit bytes written to it and drops the rest,
// so a stream of any size can be scanned for its trailer in bounded
// memory — the property that makes inflating an operator's largest backup
// safe on the drill host.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	if len(p) >= t.limit {
		t.buf = append(t.buf[:0], p[len(p)-t.limit:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if excess := len(t.buf) - t.limit; excess > 0 {
		t.buf = append(t.buf[:0], t.buf[excess:]...)
	}
	return len(p), nil
}

// cancellable makes a long read honor cancellation. A compressed artifact
// says nothing about how much it expands to, so the scan is unbounded in
// principle; the drill's deadline has to be able to end it.
type cancellable struct {
	ctx context.Context
	r   io.Reader
}

func (c *cancellable) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
