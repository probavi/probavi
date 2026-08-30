package evidence

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// anchoredResult verifies log bytes with the §9.1 anchor applied.
func anchoredResult(t *testing.T, log []byte, anchor *Head) Result {
	t.Helper()
	res, err := VerifyAnchored(bytes.NewReader(log), NewKeyring(exampleKey(t)), anchor)
	if err != nil {
		t.Fatalf("VerifyAnchored: %v", err)
	}
	return res
}

// mustParseAnchor is how an operator's recorded value reaches the verifier:
// as the written form of §9.1, not as a struct.
func mustParseAnchor(t *testing.T, text string) *Head {
	t.Helper()
	head, err := ParseAnchor(text)
	if err != nil {
		t.Fatalf("ParseAnchor(%q): %v", text, err)
	}
	return &head
}

// The three conformance vectors §9.1 publishes, as the section states them.
// They are values of the already-frozen example logs, so an implementation
// that has never seen this repository can reproduce every one of them from
// bytes it can fetch.
const (
	v2Seq2 = "2:sha256:1fab7db153a3276cb7081e1bf6d16a9b31689b9f3d4b950b5a50748e3ae3032d"
	v2Seq3 = "3:sha256:6b3e356a9444cf3d7ca6bfdf7ee6bdf35d88928b2d66fcc28a5ceb033308b62d"
	v1Seq2 = "2:sha256:28e87aa7b1a896e908fb1be0a8b8e9ad3c5a42d4d1e821e8a7ac82d8ec3ba241"
)

// TestAnchorConformanceVectors runs the three vectors of §9.1 exactly as the
// specification writes them. They are the contract between this verifier and
// the core's: both check themselves against the same published bytes, and a
// divergence turns one of the two suites red.
func TestAnchorConformanceVectors(t *testing.T) {
	full := exampleLog(t, "log_v2.jsonl")
	firstTwo := joinLines(splitLines(t, full)[:2])

	tests := []struct {
		name       string
		log        []byte
		anchor     string
		wantStatus Status
		wantReason string
		wantLine   int
		wantHead   string
	}{
		{
			name:       "the anchor holds and the log has grown",
			log:        full,
			anchor:     v2Seq2,
			wantStatus: StatusValid,
			wantHead:   v2Seq3,
		},
		{
			name:       "the log ends before the anchor",
			log:        firstTwo,
			anchor:     v2Seq3,
			wantStatus: StatusInvalid,
			wantReason: "truncated",
			wantLine:   0,
			wantHead:   v2Seq2,
		},
		{
			name:       "the line at the anchor's seq is a different line",
			log:        full,
			anchor:     v1Seq2,
			wantStatus: StatusInvalid,
			wantReason: "anchor mismatch at seq 2",
			wantLine:   2,
			wantHead:   v2Seq2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := anchoredResult(t, tc.log, mustParseAnchor(t, tc.anchor))
			if res.Status != tc.wantStatus {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Reason, tc.wantStatus)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			if res.Line != tc.wantLine {
				t.Errorf("line = %d, want %d", res.Line, tc.wantLine)
			}
			if res.Head.String() != tc.wantHead {
				t.Errorf("head = %s, want %s", res.Head, tc.wantHead)
			}
		})
	}
}

// TestTheTruncatedVectorIsValidWithoutItsAnchor states the other half of the
// second vector, and the reason §9.1 exists at all: the same bytes that the
// anchor refuses are a perfectly valid log on their own.
func TestTheTruncatedVectorIsValidWithoutItsAnchor(t *testing.T) {
	firstTwo := joinLines(splitLines(t, exampleLog(t, "log_v2.jsonl"))[:2])
	res := anchoredResult(t, firstTwo, nil)
	if res.Status != StatusValid || res.Records != 2 {
		t.Fatalf("status = %s with %d records, want VALID with 2", res.Status, res.Records)
	}
	if res.Head.String() != v2Seq2 {
		t.Errorf("head = %s, want %s", res.Head, v2Seq2)
	}
}

// TestTheHeadIsReportedOnEveryRun covers the first half of §9.1: every
// verification yields the anchor for the next one, whatever its verdict, and
// after a failure the head describes only the prefix that verified.
func TestTheHeadIsReportedOnEveryRun(t *testing.T) {
	full := exampleLog(t, "log_v2.jsonl")
	lines := splitLines(t, full)

	tests := []struct {
		name     string
		log      []byte
		want     string
		wantStat Status
	}{
		{"empty log", nil, "0:" + genesisPrevHash, StatusValid},
		{"whole log", full, v2Seq3, StatusValid},
		{"torn tail", append(append([]byte(nil), full...), []byte(`{"schema":"probavi-ev`)...), v2Seq3, StatusValidDamage},
		{"damage mid-file", joinLines([][]byte{lines[0], []byte("not json at all"), lines[1], lines[2]}), v2Seq3, StatusValidDamage},
		{"invalid at line 3", joinLines([][]byte{lines[0], lines[1], flipPrevHashChar(t, lines[2])}), v2Seq2, StatusInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := anchoredResult(t, tc.log, nil)
			if res.Status != tc.wantStat {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Reason, tc.wantStat)
			}
			if res.Head.String() != tc.want {
				t.Errorf("head = %s, want %s", res.Head, tc.want)
			}
		})
	}
}

// TestAnAnchorThatHoldsChangesNothing pins that the anchored run and the
// plain run agree whenever the anchor holds — including the genesis anchor,
// which constrains nothing, and the head of the log as it stands.
func TestAnAnchorThatHoldsChangesNothing(t *testing.T) {
	full := exampleLog(t, "log_v2.jsonl")
	plain := anchoredResult(t, full, nil)

	for _, text := range []string{"0:" + genesisPrevHash, v2Seq2, v2Seq3} {
		res := anchoredResult(t, full, mustParseAnchor(t, text))
		if res.Status != plain.Status || res.Records != plain.Records || res.Head != plain.Head {
			t.Errorf("anchor %s: %s with %d records and head %s, want %s with %d and %s",
				text, res.Status, res.Records, res.Head, plain.Status, plain.Records, plain.Head)
		}
	}
}

// TestAGenesisAnchorWithAnotherHashIsAMismatch covers the seq-0 edge of
// §9.1: the head an empty log yields must come back as an anchor, and any
// other hash at seq 0 is refused before a line is read.
func TestAGenesisAnchorWithAnotherHashIsAMismatch(t *testing.T) {
	wrong := mustParseAnchor(t, "0:sha256:"+strings.Repeat("1", 64))
	res := anchoredResult(t, exampleLog(t, "log_v2.jsonl"), wrong)
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", res.Status)
	}
	if !strings.Contains(res.Reason, "anchor mismatch at seq 0") {
		t.Errorf("reason = %q, want it to name the mismatch at seq 0", res.Reason)
	}
	if res.Line != 0 {
		t.Errorf("line = %d, want 0 — the genesis head precedes every line", res.Line)
	}
}

// TestAnAnchorBeyondADamagedLogIsStillTruncation pins that damage and
// truncation stay distinct: unparseable fragments do not advance the chain
// (§2), so a log whose records stop short of its anchor is truncated no
// matter what else the file holds.
func TestAnAnchorBeyondADamagedLogIsStillTruncation(t *testing.T) {
	lines := splitLines(t, exampleLog(t, "log_v2.jsonl"))
	damaged := append(joinLines(lines[:2]), []byte("not json at all\n")...)
	res := anchoredResult(t, damaged, mustParseAnchor(t, v2Seq3))
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", res.Status)
	}
	if !strings.Contains(res.Reason, "truncated") {
		t.Errorf("reason = %q, want it to name truncation", res.Reason)
	}
}

// TestParseAnchor covers the written form. One head has exactly one
// spelling: whatever a verifier printed must parse back, and everything else
// is a usage error rather than a value quietly reinterpreted.
func TestParseAnchor(t *testing.T) {
	for _, text := range []string{"0:" + genesisPrevHash, v2Seq2, v2Seq3, v1Seq2} {
		head, err := ParseAnchor(text)
		if err != nil {
			t.Errorf("ParseAnchor(%q): %v", text, err)
			continue
		}
		if head.String() != text {
			t.Errorf("round trip: %q printed back as %q", text, head.String())
		}
	}

	const hash = "sha256:1fab7db153a3276cb7081e1bf6d16a9b31689b9f3d4b950b5a50748e3ae3032d"
	invalid := []struct{ name, text string }{
		{"empty", ""},
		{"no separator", "2sha256:" + strings.Repeat("a", 64)},
		{"no sequence number", ":" + hash},
		{"leading zero", "02:" + hash},
		{"negative sequence number", "-1:" + hash},
		{"non-numeric sequence number", "two:" + hash},
		{"sequence number out of range", "99999999999999999999:" + hash},
		{"wrong hash prefix", "2:sha512:" + strings.Repeat("a", 64)},
		{"hash too short", "2:sha256:beef"},
		{"hash too long", "2:sha256:" + strings.Repeat("a", 65)},
		{"uppercase hash", "2:sha256:" + strings.Repeat("A", 64)},
		{"non-hex hash", "2:sha256:" + strings.Repeat("g", 64)},
		{"whole hash missing", "2:"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseAnchor(tc.text); !errors.Is(err, ErrMalformedAnchor) {
				t.Errorf("ParseAnchor(%q) = %s, %v; want ErrMalformedAnchor", tc.text, got, err)
			}
		})
	}
}
