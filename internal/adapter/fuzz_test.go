package adapter

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

// fuzz_test.go fuzzes the core's side of the adapter protocol: the bytes an
// adapter writes to stdout, which reach this package before anything has
// established that the adapter is well-behaved. A third party may write an
// adapter in any language (that is the point of the protocol being a wire
// format), so malformed framing is an ordinary event rather than an attack,
// and it has to end in a verdict rather than in a crash of the core.

// fuzzSession builds the minimum a message read needs: a scanner over the
// bytes under test, buffered exactly as the real session buffers it so the
// frame limit behaves identically.
func fuzzSession(data []byte) *session {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	return &session{stdout: sc}
}

// FuzzReadMessage drives the framing layer. Two properties beyond survival:
// a message that is accepted must be one that satisfies the checks §3 makes
// mandatory — the protocol identifier and the echoed request_id — and a
// message that is rejected must carry the crash classification, because
// anything else would leave the core with an error it cannot map to a drill
// outcome.
func FuzzReadMessage(f *testing.F) {
	const rid = "r-8f2c"
	f.Add([]byte(`{"protocol":"probavi-adapter/0","request_id":"r-8f2c","ok":true,"payload":{}}`))
	f.Add([]byte(`{"protocol":"probavi-adapter/0","request_id":"r-8f2c","sandbox_call":{"call_id":"c1","verb":"exec","args":{}}}`))
	f.Add([]byte(`{"protocol":"probavi-adapter/0","request_id":"wrong","ok":true}`))
	f.Add([]byte(`{"protocol":"probavi-adapter/999","request_id":"r-8f2c","ok":true}`))
	f.Add([]byte("not json\n"))
	f.Add([]byte(""))
	f.Add([]byte("\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		env, aerr := fuzzSession(data).readMessage("provision", rid)
		if aerr != nil {
			if aerr.Code != CodeAdapterCrash {
				t.Fatalf("rejected message classified as %q, want %q", aerr.Code, CodeAdapterCrash)
			}
			if aerr.Message == "" {
				t.Fatal("rejection with no message")
			}
			if env != nil {
				t.Fatalf("both an envelope and an error: %+v / %+v", env, aerr)
			}
			return
		}
		if env == nil {
			t.Fatal("neither an envelope nor an error")
		}
		if env.Protocol != ProtocolVersion {
			t.Fatalf("accepted protocol %q, want %q", env.Protocol, ProtocolVersion)
		}
		if env.RequestID != rid {
			t.Fatalf("accepted request_id %q, which does not echo %q", env.RequestID, rid)
		}
	})
}

// FuzzParseRFC3339 covers the value that decides whether a completed drill
// gets an evidence record at all. source_identity.created_at arrives as text
// from a foreign process; the core normalizes it to the schema's millisecond
// UTC form, and a value it cannot parse is an adapter_crash verdict
// (adapter protocol §6.2). The property is that acceptance is stable: what
// this parser takes, it must take again after the instant has been rendered
// back out — otherwise a record could be written that no verifier could
// re-derive.
func FuzzParseRFC3339(f *testing.F) {
	f.Add("2026-07-30T01:58:02.000Z")
	f.Add("2026-07-30t01:58:02z")
	f.Add("2026-07-30T01:58:02+02:00")
	f.Add("2026-07-30T01:58:02.123456789-07:00")
	f.Add("")
	f.Add("yesterday")
	f.Add("2026-13-45T99:99:99Z")

	f.Fuzz(func(t *testing.T, s string) {
		ts, err := parseRFC3339(s)
		if err != nil {
			return
		}
		rendered := ts.UTC().Format("2006-01-02T15:04:05.000Z")
		again, err := parseRFC3339(rendered)
		if err != nil {
			t.Fatalf("parsed %q into %v, which renders to %q and no longer parses: %v",
				s, ts, rendered, err)
		}
		// Truncation to milliseconds is the documented normalization, so the
		// round trip may lose sub-millisecond digits and nothing else.
		if diff := ts.Sub(again); diff < 0 || diff >= time.Millisecond {
			t.Fatalf("round trip of %q moved the instant by %v", s, diff)
		}
		// A parser that accepted a value containing a newline would let a
		// record field carry a line break into a JSONL log.
		if strings.ContainsAny(s, "\n\r") {
			t.Fatalf("accepted a timestamp containing a line break: %q", s)
		}
	})
}
