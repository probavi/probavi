package main

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
)

// rdbmeta.go reads what an RDB file states about itself, out of the file.
//
// An RDB begins with the magic "REDIS" plus a four-digit format version,
// followed by auxiliary fields the server writes before any data: among
// them `ctime`, the save instant as epoch seconds, and `redis-ver`, the
// version of the server that saved it (both measured). Epoch seconds
// carry no zone question, so — like the postgres adapter's pgBackRest
// kind, and unlike every wall-clock format — an RDB dates itself with no
// declared timezone, and `redis-ver` gives the version pre-check
// (docs/engine-versions.md §5) its backup side.
//
// The parser reads only the head of the file and only the encodings those
// two fields use. Anything it does not recognise ends the parse with what
// was collected so far: metadata is a bonus, never a verdict — the
// authority on whether the file restores is redis-check-rdb, inside the
// sandbox.

const (
	rdbMagic = "REDIS"
	// rdbHeadMax bounds the read: the aux fields sit immediately after
	// the nine-byte header, well inside this.
	rdbHeadMax = 4096
	// rdbOpcodeAux introduces one key/value auxiliary field.
	rdbOpcodeAux = 0xFA

	auxRedisVer = "redis-ver"
	auxCtime    = "ctime"
)

// rdbVersionShape accepts only version-shaped redis-ver values, so an aux
// oddity cannot reach a refusal message.
var rdbVersionShape = regexp.MustCompile(`^\d+(?:\.\d+)*$`)

// rdbMeta is what the head of an RDB file states about itself.
type rdbMeta struct {
	// valid reports the REDIS magic plus a four-digit format version —
	// the artifact is RDB-shaped. It says nothing about restorability.
	valid bool
	// redisVer is the origin server's version ("7.2.5"), "" when the aux
	// field is absent or not version-shaped. Unstable builds write
	// 255.255.255 (measured convention); the shape passes here and the
	// version pre-check's plausibility bounds discard it.
	redisVer string
	// ctime is the save instant in epoch seconds, 0 when absent or
	// implausible.
	ctime int64
}

// readRDBMeta parses the head of the file at path; an unreadable file
// yields the zero value, because metadata is a bonus, never worth an
// error of its own.
func readRDBMeta(path string) rdbMeta {
	head, err := readHead(path, rdbHeadMax)
	if err != nil {
		return rdbMeta{}
	}
	return parseRDBMeta(head)
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

// parseRDBMeta walks the auxiliary fields at the head of an RDB.
func parseRDBMeta(head []byte) rdbMeta {
	m := rdbMeta{}
	if !rdbHeaderValid(head) {
		return m
	}
	m.valid = true
	pos := len(rdbMagic) + 4
	for pos < len(head) && head[pos] == rdbOpcodeAux {
		key, next, ok := rdbString(head, pos+1)
		if !ok {
			return m
		}
		value, after, ok := rdbString(head, next)
		if !ok {
			return m
		}
		pos = after
		recordAux(&m, key, value)
	}
	return m
}

// rdbHeaderValid reports the REDIS magic plus a four-digit format version.
func rdbHeaderValid(head []byte) bool {
	if len(head) < len(rdbMagic)+4 || string(head[:len(rdbMagic)]) != rdbMagic {
		return false
	}
	for _, d := range head[len(rdbMagic) : len(rdbMagic)+4] {
		if d < '0' || d > '9' {
			return false
		}
	}
	return true
}

// recordAux keeps the two aux fields this adapter reads, when plausible.
func recordAux(m *rdbMeta, key, value string) {
	switch key {
	case auxRedisVer:
		if rdbVersionShape.MatchString(value) {
			m.redisVer = value
		}
	case auxCtime:
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && plausibleEpoch(n) {
			m.ctime = n
		}
	}
}

// rdbString decodes one length-prefixed RDB string starting at pos and
// returns it with the position after it. Only the encodings the header
// aux fields use are handled — 6-bit and 14-bit lengths, and the 8/16/32
// bit little-endian integer forms, rendered as the decimal strings the
// server encoded — anything else ends the parse.
func rdbString(head []byte, pos int) (string, int, bool) {
	if pos >= len(head) {
		return "", 0, false
	}
	b := head[pos]
	pos++
	switch b >> 6 {
	case 0: // 6-bit length in place
		return takeBytes(head, pos, int(b&0x3F))
	case 1: // 14-bit length: high bits here, low byte next
		if pos >= len(head) {
			return "", 0, false
		}
		return takeBytes(head, pos+1, int(b&0x3F)<<8|int(head[pos]))
	case 3: // integer-encoded string: signed little-endian, sign-extended
		switch b & 0x3F {
		case 0:
			if pos+1 > len(head) {
				return "", 0, false
			}
			return strconv.FormatInt(signExtend(int64(head[pos]), 8), 10), pos + 1, true
		case 1:
			if pos+2 > len(head) {
				return "", 0, false
			}
			return strconv.FormatInt(signExtend(int64(binary.LittleEndian.Uint16(head[pos:])), 16), 10), pos + 2, true
		case 2:
			if pos+4 > len(head) {
				return "", 0, false
			}
			return strconv.FormatInt(signExtend(int64(binary.LittleEndian.Uint32(head[pos:])), 32), 10), pos + 4, true
		}
	}
	// 32/64-bit lengths and LZF compression never carry the short header
	// values this parser is after.
	return "", 0, false
}

// signExtend interprets the low bits of an unsigned load as a signed
// integer of the given width.
func signExtend(v int64, bits uint) int64 {
	if v >= 1<<(bits-1) {
		v -= 1 << bits
	}
	return v
}

func takeBytes(head []byte, pos, n int) (string, int, bool) {
	if n < 0 || pos+n > len(head) {
		return "", 0, false
	}
	return string(head[pos : pos+n]), pos + n, true
}

// plausibleEpoch rejects values no save timestamp produces, so a field
// that happens to parse as a number cannot date a backup absurdly.
func plausibleEpoch(seconds int64) bool {
	const (
		year2000 = 946684800  // no restorable RDB predates this
		year2200 = 7258118400 // and none is written this far ahead
	)
	return seconds >= year2000 && seconds <= year2200
}
