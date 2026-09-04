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
// act on it. `valid` gates everything else, a version string that reaches
// a version pre-check must be text rather than raw bytes, and a parser
// that reported nothing must not have filled a field anyway.
func FuzzParseRDBMeta(f *testing.F) {
	f.Add([]byte("REDIS0011"))
	f.Add([]byte("VALKEY001"))
	f.Add([]byte("REDIS0011\xfa\x09redis-ver\x057.2.5"))
	f.Add([]byte("REDIS0011\xfa\x0avalkey-ver\x057.2.5"))
	f.Add([]byte("REDIS0011\xfa\x05ctime\xc2\x00\x00\x00\x00"))
	f.Add([]byte("REDIS0011\xfa\xff"))
	f.Add([]byte("REDIS999"))
	f.Add([]byte(""))
	f.Add([]byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00\xfa"))

	f.Fuzz(func(t *testing.T, head []byte) {
		m := parseRDBMeta(head)

		if !m.valid {
			// Nothing was recognised, so nothing may be claimed: the
			// source fences read these fields as positive evidence.
			if m.redisVer != "" || m.valkeyVer != "" || m.ctime != 0 || m.valkeyMagic {
				t.Fatalf("invalid header still produced %+v", m)
			}
			return
		}
		for name, v := range map[string]string{"redis-ver": m.redisVer, "valkey-ver": m.valkeyVer} {
			if v == "" {
				continue
			}
			if !utf8.ValidString(v) {
				t.Fatalf("%s = %q is not text — it reaches a refusal message", name, v)
			}
			if strings.ContainsAny(v, "\x00\n\r") {
				t.Fatalf("%s = %q carries a control byte", name, v)
			}
		}
		if m.ctime < 0 {
			t.Fatalf("ctime = %d — a backup instant is never negative", m.ctime)
		}
	})
}
