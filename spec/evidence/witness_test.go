package evidence

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// The two committed witnesses of §9.2.5. They are scripts rather than
// fakes written by each test because the contract under test is a process
// boundary: argv, stdin, stdout, stderr and the exit status. A fake in Go
// would test a different thing.
const (
	witnessExpect = "testdata/witness-expect.sh"
	witnessFail   = "testdata/witness-fail.sh"
	witnessAbsent = "testdata/witness-does-not-exist.sh"
)

// expectHead arms the attesting witness for one head, which is how the
// vectors assert that the verifier passed the right argv: the script
// refuses anything else.
func expectHead(t *testing.T, head, at string) {
	t.Helper()
	t.Setenv("PROBAVI_TEST_HEAD", head)
	t.Setenv("PROBAVI_TEST_AT", at)
}

// witnessed verifies the published example log and then applies §9.2.
func witnessed(t *testing.T, log []byte, anchor *Head, command string) (Result, error) {
	t.Helper()
	res, err := VerifyAnchored(bytes.NewReader(log), NewKeyring(exampleKey(t)), anchor)
	if err != nil {
		t.Fatalf("VerifyAnchored: %v", err)
	}
	return Witnessed(context.Background(), res, command)
}

// The five vectors of §9.2.5, one test each, run against the published
// log whose head that section names. No timestamp authority and no
// network: the exit status carries the answer, which is what makes the
// contract checkable by an implementation that has neither.

// TestWitnessAttests is vector 1. The script refuses any head but the one
// it was armed with, so a pass here is also the assertion that the
// verifier passed the right argv.
func TestWitnessAttests(t *testing.T) {
	const at = "2026-09-27T09:00:00Z"
	expectHead(t, v2Seq3, at)
	res, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, witnessExpect)
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusValid {
		t.Fatalf("status = %s (%s), want %s", res.Status, res.Reason, StatusValid)
	}
	if res.Witness == nil {
		t.Fatal("witness is null after a witness was asked")
	}
	if res.Witness.Command != witnessExpect || !res.Witness.Attested {
		t.Errorf("witness = %+v, want the command attesting", res.Witness)
	}
	if res.Witness.AttestedAt == nil || *res.Witness.AttestedAt != at {
		t.Errorf("attested_at = %v, want %q", res.Witness.AttestedAt, at)
	}
}

// TestWitnessAttestsWithoutATime is vector 2.
func TestWitnessAttestsWithoutATime(t *testing.T) {
	expectHead(t, v2Seq3, "")
	res, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, witnessExpect)
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusValid {
		t.Fatalf("status = %s (%s), want %s", res.Status, res.Reason, StatusValid)
	}
	if res.Witness == nil || !res.Witness.Attested || res.Witness.AttestedAt != nil {
		t.Errorf("witness = %+v, want attested with no time", res.Witness)
	}
}

// TestWitnessRefuses is vector 3. The witness is armed for a head this log
// does not have, which is what a log rewritten after it was attested looks
// like from the outside.
func TestWitnessRefuses(t *testing.T) {
	expectHead(t, v2Seq2, "")
	res, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, witnessExpect)
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want %s — a head no witness attests is INVALID", res.Status, StatusInvalid)
	}
	if res.Witness == nil || res.Witness.Attested {
		t.Fatalf("witness = %+v, want a refusal recorded", res.Witness)
	}
	for _, want := range []string{witnessExpect, v2Seq3, "no attestation"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason %q does not name %q", res.Reason, want)
		}
	}
	if res.Line != 0 {
		t.Errorf("line = %d, want 0 — no line of the log is at fault", res.Line)
	}
}

// TestWitnessCannotAnswer is vector 4, in both of its shapes: a command
// that runs and exits with something other than 0 or 1, and one that
// cannot be run at all. Neither is a verdict about the log.
func TestWitnessCannotAnswer(t *testing.T) {
	for _, command := range []string{witnessFail, witnessAbsent} {
		t.Run(command, func(t *testing.T) {
			_, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, command)
			if !errors.Is(err, ErrWitnessFailed) {
				t.Fatalf("err = %v, want %v", err, ErrWitnessFailed)
			}
			if !strings.Contains(err.Error(), command) {
				t.Errorf("error %q does not name the command", err)
			}
		})
	}
}

// TestInvalidLogIsNeverWitnessed is vector 5. The witness is armed to
// attest, so a verifier that asked anyway would record one and fail here
// rather than pass by accident.
func TestInvalidLogIsNeverWitnessed(t *testing.T) {
	expectHead(t, v2Seq3, "2026-09-27T09:00:00Z")
	res, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), mustParseAnchor(t, v1Seq2), witnessExpect)
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want %s from the anchor", res.Status, StatusInvalid)
	}
	if res.Witness != nil {
		t.Errorf("witness = %+v, want null — a run that returned INVALID is never a source of an anchor", res.Witness)
	}
	if !strings.Contains(res.Reason, "anchor mismatch") {
		t.Errorf("reason = %q, want the anchor's, not the witness's", res.Reason)
	}
}

// TestNoWitnessLeavesTheResultAlone is the ordinary case: §9 stands exactly
// as written when no witness is asked, and the field says so rather than
// being absent.
func TestNoWitnessLeavesTheResultAlone(t *testing.T) {
	res, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, "")
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusValid || res.Witness != nil {
		t.Errorf("result = %+v, want VALID with a null witness", res)
	}
}

// TestWitnessDiagnosticsAreOptional covers the quiet halves of both
// scripts: a witness that says nothing on stderr must still produce a
// readable reason, and a failure with no diagnostic must not grow an empty
// parenthesis.
func TestWitnessDiagnosticsAreOptional(t *testing.T) {
	t.Setenv("PROBAVI_TEST_QUIET", "1")

	t.Run("a silent refusal", func(t *testing.T) {
		expectHead(t, v2Seq2, "")
		res, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, witnessExpect)
		if err != nil {
			t.Fatalf("Witnessed: %v", err)
		}
		if res.Status != StatusInvalid {
			t.Fatalf("status = %s, want %s", res.Status, StatusInvalid)
		}
		if strings.HasSuffix(res.Reason, ": ") {
			t.Errorf("reason %q ends in an empty diagnostic", res.Reason)
		}
	})

	t.Run("a silent failure", func(t *testing.T) {
		_, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, witnessFail)
		if !errors.Is(err, ErrWitnessFailed) {
			t.Fatalf("err = %v, want %v", err, ErrWitnessFailed)
		}
		if strings.Contains(err.Error(), "()") {
			t.Errorf("error %q carries an empty diagnostic", err)
		}
	})
}

// TestAttestedAtIsWhatTheWitnessSaid: the field carries the witness's own
// words, and a line that is not an RFC 3339 instant is read as attested
// with the time unstated rather than as a failure. The exit status already
// carried the answer.
func TestAttestedAtIsWhatTheWitnessSaid(t *testing.T) {
	tests := []struct {
		name, printed string
		want          *string
	}{
		{"an RFC 3339 instant", "2026-09-27T09:00:00Z", strptr("2026-09-27T09:00:00Z")},
		{"an instant with an offset", "2026-09-27T11:00:00+02:00", strptr("2026-09-27T11:00:00+02:00")},
		{"nothing at all", "", nil},
		{"prose", "attested yesterday", nil},
		{"a date with no time", "2026-09-27", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expectHead(t, v2Seq3, tc.printed)
			res, err := witnessed(t, exampleLog(t, "log_v2.jsonl"), nil, witnessExpect)
			if err != nil {
				t.Fatalf("Witnessed: %v", err)
			}
			if res.Witness == nil || !res.Witness.Attested {
				t.Fatalf("witness = %+v, want attested", res.Witness)
			}
			switch {
			case tc.want == nil && res.Witness.AttestedAt != nil:
				t.Errorf("attested_at = %q, want null", *res.Witness.AttestedAt)
			case tc.want != nil && (res.Witness.AttestedAt == nil || *res.Witness.AttestedAt != *tc.want):
				t.Errorf("attested_at = %v, want %q", res.Witness.AttestedAt, *tc.want)
			}
		})
	}
}

func strptr(s string) *string { return &s }
