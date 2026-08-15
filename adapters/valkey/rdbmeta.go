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
// Valkey writes two RDB headers, both measured against the official
// images: through 8.x the magic is "REDIS" plus a four-digit format
// version (11 — the pre-fork layout), and from 9.0 it is "VALKEY" plus a
// three-digit format version in Valkey's own numbering (80). Every Valkey
// since the fork writes a `valkey-ver` auxiliary field naming the server
// that saved the file, and never writes `redis-ver` — while Redis writes
// `redis-ver` and never `valkey-ver`. Those aux fields are therefore
// positive evidence of the artifact's dialect, which source.go turns into
// refusals; `ctime`, the save instant as epoch seconds, dates the backup
// with no timezone question, exactly as in the redis adapter.
//
// The parser reads only the head of the file and only the encodings those
// fields use. Anything it does not recognise ends the parse with what was
// collected so far: metadata is a bonus, never a verdict — except where a
// field is positive evidence of a dialect this engine does not load, and
// the authority on everything else is valkey-check-rdb, inside the
// sandbox.

const (
	// rdbMagicRedis opens the pre-fork layout Valkey kept through 8.x;
	// four format-version digits follow.
	rdbMagicRedis = "REDIS"
	// rdbMagicValkey opens Valkey's own layout from 9.0; three
	// format-version digits follow. Both headers are nine bytes.
	rdbMagicValkey = "VALKEY"
	// rdbHeadMax bounds the read: the aux fields sit immediately after
	// the nine-byte header, well inside this.
	rdbHeadMax = 4096
	// rdbOpcodeAux introduces one key/value auxiliary field.
	rdbOpcodeAux = 0xFA

	auxValkeyVer = "valkey-ver"
	auxRedisVer  = "redis-ver"
	auxCtime     = "ctime"

	// redisDialectFloor is the first REDIS-magic format version no Valkey
	// engine loads: Redis moved to 12 with 7.4, after the fork, while
	// Valkey stayed on 11 until it switched to its own magic. Measured:
	// Valkey 9 refuses such a file at startup ("Can't handle RDB format
	// version 12") even though valkey-check-rdb passes it.
	redisDialectFloor = 12
)

// rdbVersionShape accepts only version-shaped aux values, so an aux
// oddity cannot reach a refusal message.
var rdbVersionShape = regexp.MustCompile(`^\d+(?:\.\d+)*$`)

// rdbMeta is what the head of an RDB file states about itself.
type rdbMeta struct {
	// valid reports either magic plus its format-version digits — the
	// artifact is RDB-shaped. It says nothing about restorability.
	valid bool
	// formatVersion is the number after the magic: Redis-numbered under
	// the REDIS magic (11, 12, …), Valkey-numbered under VALKEY (80, …).
	formatVersion int
	// valkeyMagic reports the VALKEY magic — the layout only Valkey 9+
	// writes.
	valkeyMagic bool
	// valkeyVer is the origin Valkey server's version ("8.0.10"), ""
	// when the aux field is absent or not version-shaped. Unstable
	// builds write 255.255.255 (a convention Valkey kept); the shape
	// passes here and the version pre-check's plausibility bounds
	// discard it.
	valkeyVer string
	// redisVer is the origin Redis server's version — positive evidence
	// that the artifact is the other dialect's, which source.go refuses.
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
	version, isValkey, ok := parseRDBHeader(head)
	if !ok {
		return m
	}
	m.valid = true
	m.formatVersion = version
	m.valkeyMagic = isValkey
	pos := headerLen
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

// headerLen is the nine bytes both magics occupy: "REDIS" plus four
// digits, or "VALKEY" plus three.
const headerLen = 9

// parseRDBHeader recognises either magic and returns its format version.
func parseRDBHeader(head []byte) (version int, isValkey, ok bool) {
	if len(head) < headerLen {
		return 0, false, false
	}
	var digits []byte
	switch {
	case string(head[:len(rdbMagicValkey)]) == rdbMagicValkey:
		digits, isValkey = head[len(rdbMagicValkey):headerLen], true
	case string(head[:len(rdbMagicRedis)]) == rdbMagicRedis:
		digits = head[len(rdbMagicRedis):headerLen]
	default:
		return 0, false, false
	}
	for _, d := range digits {
		if d < '0' || d > '9' {
			return 0, false, false
		}
		version = version*10 + int(d-'0')
	}
	return version, isValkey, true
}

// recordAux keeps the aux fields this adapter reads, when plausible.
func recordAux(m *rdbMeta, key, value string) {
	switch key {
	case auxValkeyVer:
		if rdbVersionShape.MatchString(value) {
			m.valkeyVer = value
		}
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
