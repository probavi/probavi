package evidence

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// verifyAnchored runs the anchored check of §9.1 over log text.
func verifyAnchored(t *testing.T, log string, anchor *Head) *Result {
	t.Helper()
	res, err := Verify(strings.NewReader(log), testKeyring(), anchor)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return res
}

// headOf returns the chain head of a log, which is what an operator records
// after a verification and hands back as the anchor of the next one.
func headOf(t *testing.T, log string) Head {
	t.Helper()
	return verifyAnchored(t, log, nil).Head
}

// TestTheHeadIsReportedOnEveryRun covers the first half of §9.1: a
// verification always yields the anchor for the next one, whatever its
// verdict. After INVALID the head describes only the prefix that verified —
// stated here so that the rule "never take an anchor from an INVALID run"
// has something concrete behind it.
func TestTheHeadIsReportedOnEveryRun(t *testing.T) {
	lines := logLines(t, buildLog(t))
	full := strings.Join(lines, "\n") + "\n"
	firstTwo := strings.Join(lines[:2], "\n") + "\n"

	tests := []struct {
		name       string
		log        string
		wantStatus Status
		wantHead   Head
	}{
		{"empty log carries the genesis head", "", StatusValid, Head{Seq: 0, Hash: GenesisPrevHash}},
		{"whole log", full, StatusValid, headOf(t, full)},
		{"torn tail does not move the head", full + `{"partial":`, StatusValidWithDamage, headOf(t, full)},
		{"unparseable line does not move the head", firstTwo + "not json at all", StatusValidWithDamage, headOf(t, firstTwo)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := verifyAnchored(t, tc.log, nil)
			if res.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (%s)", res.Status, tc.wantStatus, res.Reason)
			}
			if res.Head != tc.wantHead {
				t.Errorf("head = %s, want %s", res.Head, tc.wantHead)
			}
		})
	}
}

// TestTheHeadAfterAnInvalidRecordIsThePrefixThatVerified pins the rule
// separately from damage: an INVALID verdict still reports a head, and it is
// the last record checked before the failing line — not the log's last line.
func TestTheHeadAfterAnInvalidRecordIsThePrefixThatVerified(t *testing.T) {
	lines := logLines(t, buildLog(t))
	// Remove the middle record: the chain breaks at what is now line 2,
	// after exactly one record has verified.
	broken := strings.Join([]string{lines[0], lines[2]}, "\n") + "\n"
	res := verifyAnchored(t, broken, nil)
	if res.Status != StatusInvalid || res.FailedLine != 2 {
		t.Fatalf("Verify = %s at line %d, want INVALID at line 2", res.Status, res.FailedLine)
	}
	if want := headOf(t, lines[0]+"\n"); res.Head != want {
		t.Errorf("head = %s, want the head of the verified prefix %s", res.Head, want)
	}
}

// TestAnAnchorThatHoldsChangesNothing covers the first outcome of §9.1: the
// walk reached the anchor's seq carrying the same head, so the verdict is
// the one the same file produces with no anchor at all — including when the
// log has grown well past the anchor, which is the ordinary case.
func TestAnAnchorThatHoldsChangesNothing(t *testing.T) {
	lines := logLines(t, buildLog(t))
	full := strings.Join(lines, "\n") + "\n"
	unanchored := verifyAnchored(t, full, nil)

	for _, anchor := range []Head{
		{Seq: 0, Hash: GenesisPrevHash},               // the log has grown from nothing
		headOf(t, lines[0]+"\n"),                      // one record old
		headOf(t, strings.Join(lines[:2], "\n")+"\n"), // one drill behind
		unanchored.Head,                               // taken from the log as it stands
	} {
		res := verifyAnchored(t, full, &anchor)
		if res.Status != StatusValid || res.Records != unanchored.Records {
			t.Fatalf("anchor %s: %s with %d records, want VALID with %d (%s)",
				anchor, res.Status, res.Records, unanchored.Records, res.Reason)
		}
		if res.Head != unanchored.Head {
			t.Errorf("anchor %s: head = %s, want %s", anchor, res.Head, unanchored.Head)
		}
	}
}

// TestATruncatedLogFailsItsAnchor is the fifth chain attack: records deleted
// from the end of the log. TestTailTruncationVerifiesValid pins that the
// file alone cannot see this; here the anchor does, and it reports no
// failing line, because every line the file still holds verified.
func TestATruncatedLogFailsItsAnchor(t *testing.T) {
	lines := logLines(t, buildLog(t))
	full := strings.Join(lines, "\n") + "\n"
	anchor := headOf(t, full)

	for _, keep := range []int{0, 1, 2} {
		truncated := strings.Join(lines[:keep], "\n")
		if keep > 0 {
			truncated += "\n"
		}
		if unanchored := verifyAnchored(t, truncated, nil); unanchored.Status != StatusValid {
			t.Fatalf("keeping %d records: unanchored = %s, want VALID — the attack must be invisible without the anchor", keep, unanchored.Status)
		}
		res := verifyAnchored(t, truncated, &anchor)
		if res.Status != StatusInvalid {
			t.Fatalf("keeping %d records: %s, want INVALID", keep, res.Status)
		}
		if !strings.Contains(res.Reason, "truncated") {
			t.Errorf("keeping %d records: reason = %q, want it to name truncation", keep, res.Reason)
		}
		if res.FailedLine != 0 {
			t.Errorf("keeping %d records: failed line = %d, want 0 — no line in the file is at fault", keep, res.FailedLine)
		}
		if res.Head.Seq != int64(keep) {
			t.Errorf("keeping %d records: head = %s, want seq %d", keep, res.Head, keep)
		}
	}
}

// TestARewoundAndRegrownLogFailsItsAnchor is the attack the truncation is
// really for: delete the drill that failed, then let the next drill append
// onto the shortened chain. The result is a log of full length that verifies
// VALID on its own — §9's own point that the removal leaves no trace — and
// the anchor still refuses it, because the line at that seq is no longer the
// line the anchor was taken of.
func TestARewoundAndRegrownLogFailsItsAnchor(t *testing.T) {
	path := buildLog(t)
	lines := logLines(t, path)
	// Two anchors an operator could be holding: the log's head, and one
	// drill older. Each must catch the rewind at its own seq.
	atSeq2 := headOf(t, strings.Join(lines[:2], "\n")+"\n")
	atSeq3 := headOf(t, strings.Join(lines, "\n")+"\n")

	// Rewind to the first record, then let the writer grow the log again.
	// The store resumes from the last valid record, so the new records are
	// genuinely signed and genuinely chained: nothing here is hand-forged.
	if err := os.WriteFile(path, []byte(lines[0]+"\n"), 0o600); err != nil {
		t.Fatalf("rewind log: %v", err)
	}
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, rec := range []*Record{sampleRecordPass(), sampleRecordPass()} {
		if err := st.Append(rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	regrown := strings.Join(logLines(t, path), "\n") + "\n"
	unanchored := verifyAnchored(t, regrown, nil)
	if unanchored.Status != StatusValid || unanchored.Records != 3 {
		t.Fatalf("regrown log = %s with %d records, want VALID with 3 — the attack must be invisible without the anchor",
			unanchored.Status, unanchored.Records)
	}

	for _, anchor := range []Head{atSeq2, atSeq3} {
		res := verifyAnchored(t, regrown, &anchor)
		if res.Status != StatusInvalid {
			t.Fatalf("anchor %s: status = %s, want INVALID", anchor, res.Status)
		}
		want := fmt.Sprintf("anchor mismatch at seq %d", anchor.Seq)
		if !strings.Contains(res.Reason, want) {
			t.Errorf("anchor %s: reason = %q, want it to contain %q", anchor, res.Reason, want)
		}
		if int64(res.FailedLine) != anchor.Seq {
			t.Errorf("anchor %s: failed line = %d, want %d — the line the anchor disagrees with", anchor, res.FailedLine, anchor.Seq)
		}
	}
}

// TestAnAnchorNoLogEverCarriedIsAMismatch covers the same outcome from the
// other side: an anchor whose hash belongs to no line of this log — the
// shape an anchor taken from a different log has — is refused at its own
// seq, even though the log itself is sound end to end.
func TestAnAnchorNoLogEverCarriedIsAMismatch(t *testing.T) {
	log := strings.Join(logLines(t, buildLog(t)), "\n") + "\n"
	real := headOf(t, log)
	stranger := Head{Seq: real.Seq, Hash: flipHashDigit(real.Hash)}

	res := verifyAnchored(t, log, &stranger)
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", res.Status)
	}
	if !strings.Contains(res.Reason, "anchor mismatch") || !strings.Contains(res.Reason, real.Hash) {
		t.Errorf("reason = %q, want the mismatch and the hash the log actually has", res.Reason)
	}
	if res.Head != real {
		t.Errorf("head = %s, want the log's own head %s", res.Head, real)
	}
}

// flipHashDigit rewrites the last hex digit of a "sha256:" reference, giving
// a well-formed hash that no record line hashes to.
func flipHashDigit(hash string) string {
	last := hash[len(hash)-1]
	if last == '0' {
		return hash[:len(hash)-1] + "1"
	}
	return hash[:len(hash)-1] + "0"
}

// TestTheGenesisAnchorIsAcceptedBackAsInput covers the seq-0 case of §9.1:
// the head a verifier prints for an empty log must be usable as an anchor,
// and a genesis anchor with any other hash is a mismatch like any other.
func TestTheGenesisAnchorIsAcceptedBackAsInput(t *testing.T) {
	genesis := headOf(t, "")
	if genesis != (Head{Seq: 0, Hash: GenesisPrevHash}) {
		t.Fatalf("head of an empty log = %s, want the genesis value at seq 0", genesis)
	}
	if res := verifyAnchored(t, "", &genesis); res.Status != StatusValid {
		t.Errorf("empty log anchored at its own head = %s (%s), want VALID", res.Status, res.Reason)
	}

	wrong := Head{Seq: 0, Hash: "sha256:" + strings.Repeat("1", 64)}
	res := verifyAnchored(t, strings.Join(logLines(t, buildLog(t)), "\n")+"\n", &wrong)
	if res.Status != StatusInvalid || !strings.Contains(res.Reason, "anchor mismatch at seq 0") {
		t.Fatalf("Verify = %s (%s), want INVALID naming the mismatch at seq 0", res.Status, res.Reason)
	}
	if res.FailedLine != 0 {
		t.Errorf("failed line = %d, want 0 — the genesis head precedes every line", res.FailedLine)
	}
}

// TestParseAnchor covers the written form of §9.1. One head has exactly one
// spelling: a value either verifier printed must parse, and anything else
// must be refused as a usage error rather than quietly reinterpreted.
func TestParseAnchor(t *testing.T) {
	const hash = "sha256:1fab7db153a3276cb7081e1bf6d16a9b31689b9f3d4b950b5a50748e3ae3032d"

	valid := []struct {
		text string
		want Head
	}{
		{"0:" + GenesisPrevHash, Head{Seq: 0, Hash: GenesisPrevHash}},
		{"1:" + hash, Head{Seq: 1, Hash: hash}},
		{"9007199254740991:" + hash, Head{Seq: MaxSafeInteger, Hash: hash}},
	}
	for _, tc := range valid {
		got, err := ParseAnchor(tc.text)
		if err != nil {
			t.Errorf("ParseAnchor(%q): %v", tc.text, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAnchor(%q) = %s, want %s", tc.text, got, tc.want)
		}
		if got.String() != tc.text {
			t.Errorf("round trip: %q printed back as %q", tc.text, got.String())
		}
	}

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
