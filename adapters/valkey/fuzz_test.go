package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParseRDBMeta drives the RDB header parser over arbitrary bytes.
//
// It reads an artifact the operator supplies and the drill has not yet
// vetted, on the host, before anything is transferred — which by this
// repository's threat model is attacker-shaped input. The parser walks
// length-prefixed strings inside a byte slice, so an offset that runs
// past the end is the failure it must not have.
//
// Beyond survival: what comes back has to be usable by the callers that
// act on it. `valid` gates everything else, a version string that
// reaches a version pre-check or a dialect refusal must be text rather
// than raw bytes, and a parser that reported nothing must not have
// filled a field anyway.
func FuzzParseRDBMeta(f *testing.F) {
	f.Add([]byte("VALKEY0080"))
	f.Add([]byte("REDIS0011"))
	f.Add([]byte("VALKEY0080\xfa\x0avalkey-ver\x068.0.10"))
	f.Add([]byte("REDIS0011\xfa\x09redis-ver\x057.2.5"))
	f.Add([]byte("VALKEY0080\xfa\x05ctime\xc2\x00\x00\x00\x00"))
	f.Add([]byte("VALKEY0080\xfa\xff"))
	f.Add([]byte("VALKEY99"))
	f.Add([]byte(""))
	f.Add([]byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00\xfa"))

	f.Fuzz(func(t *testing.T, head []byte) {
		m := parseRDBMeta(head)

		if !m.valid {
			// Nothing was recognised, so nothing may be claimed: the
			// source fences read these fields as positive evidence.
			if m.redisVer != "" || m.valkeyVer != "" || m.ctime != 0 ||
				m.valkeyMagic || m.formatVersion != 0 {
				t.Fatalf("invalid header still produced %+v", m)
			}
			return
		}
		assertVersionText(t, "valkey-ver", m.valkeyVer)
		assertVersionText(t, "redis-ver", m.redisVer)
		if m.ctime < 0 {
			t.Fatalf("ctime = %d — a backup instant is never negative", m.ctime)
		}
		if m.formatVersion < 0 {
			t.Fatalf("formatVersion = %d, which no magic numbers", m.formatVersion)
		}
	})
}

// assertVersionText holds a reported version string to what a refusal
// message may carry: an aux field is copied out of the artifact, and
// both the version pre-check and the dialect refusal quote it.
func assertVersionText(t *testing.T, name, v string) {
	t.Helper()
	if v == "" {
		return
	}
	if !utf8.ValidString(v) {
		t.Fatalf("%s = %q is not text — it reaches a refusal message", name, v)
	}
	if strings.ContainsAny(v, "\x00\n\r") {
		t.Fatalf("%s = %q carries a control byte", name, v)
	}
}
