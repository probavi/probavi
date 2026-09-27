package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The two committed witnesses of §9.2.5. They are scripts rather than Go
// fakes because the contract under test is a process boundary — argv,
// stdin, stdout, stderr and the exit status — and a fake would test
// something else.
const (
	witnessExpect = "testdata/witness-expect.sh"
	witnessFail   = "testdata/witness-fail.sh"
	witnessAbsent = "testdata/witness-does-not-exist.sh"
)

// armWitness points the attesting script at one head. It is how these
// tests assert that the verifier passed the right argv: the script refuses
// anything else, so a verifier that passed the wrong head fails the
// attesting vector rather than passing it quietly.
func armWitness(t *testing.T, head Head, at string) {
	t.Helper()
	t.Setenv("PROBAVI_TEST_HEAD", head.String())
	t.Setenv("PROBAVI_TEST_AT", at)
}

// witnessLog is the three-record log these vectors run against, read back
// as text the way §9's algorithm reads one.
func witnessLog(t *testing.T) string {
	t.Helper()
	return strings.Join(logLines(t, buildLog(t)), "\n") + "\n"
}

// witnessed verifies a log and then applies §9.2 to the result.
func witnessed(t *testing.T, log string, anchor *Head, command string) (*Result, error) {
	t.Helper()
	res := verifyAnchored(t, log, anchor)
	return res, Witnessed(context.Background(), res, command)
}

// The five vectors of §9.2.5, one test each. They need no timestamp
// authority and no network — the exit status carries the answer — which is
// what makes the contract checkable by an implementation that has neither,
// and by this one, which shares no code with the independent verifier
// running the same five.

// TestWitnessAttests is vector 1. The script refuses any head but the one
// it was armed with, so a pass here is also the assertion that the
// verifier passed the right argv.
func TestWitnessAttests(t *testing.T) {
	const at = "2026-09-27T09:00:00Z"
	log := witnessLog(t)
	armWitness(t, headOf(t, log), at)
	res, err := witnessed(t, log, nil, witnessExpect)
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusValid {
		t.Fatalf("status = %s (%s), want %s", res.Status, res.Reason, StatusValid)
	}
	if res.Witness == nil {
		t.Fatal("witness is nil after a witness was asked")
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
	log := witnessLog(t)
	armWitness(t, headOf(t, log), "")
	res, err := witnessed(t, log, nil, witnessExpect)
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
// does not carry, which is what a log rewritten after it was attested
// looks like from the outside.
func TestWitnessRefuses(t *testing.T) {
	log := witnessLog(t)
	head := headOf(t, log)
	armWitness(t, Head{Seq: head.Seq, Hash: flipHashDigit(head.Hash)}, "")
	res, err := witnessed(t, log, nil, witnessExpect)
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want %s — a head no witness attests is INVALID", res.Status, StatusInvalid)
	}
	if res.Witness == nil || res.Witness.Attested {
		t.Fatalf("witness = %+v, want a refusal recorded", res.Witness)
	}
	for _, want := range []string{witnessExpect, head.String(), "no attestation"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason %q does not name %q", res.Reason, want)
		}
	}
	if res.FailedLine != 0 {
		t.Errorf("failed line = %d, want 0 — no line of the log is at fault", res.FailedLine)
	}
}

// TestWitnessCannotAnswer is vector 4, in both of its shapes: a command
// that runs and exits with something other than 0 or 1, and one that
// cannot be run at all. Neither is a verdict about the log.
func TestWitnessCannotAnswer(t *testing.T) {
	log := witnessLog(t)
	for _, command := range []string{witnessFail, witnessAbsent} {
		t.Run(command, func(t *testing.T) {
			_, err := witnessed(t, log, nil, command)
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
	log := witnessLog(t)
	head := headOf(t, log)
	armWitness(t, head, "2026-09-27T09:00:00Z")
	stranger := Head{Seq: head.Seq, Hash: flipHashDigit(head.Hash)}
	res, err := witnessed(t, log, &stranger, witnessExpect)
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want %s from the anchor", res.Status, StatusInvalid)
	}
	if res.Witness != nil {
		t.Errorf("witness = %+v, want nil — a run that returned INVALID is never a source of an anchor", res.Witness)
	}
	if strings.Contains(res.Reason, "witness") {
		t.Errorf("reason = %q, want the anchor's finding rather than the witness's", res.Reason)
	}
}

// TestNoWitnessLeavesTheResultAlone is the ordinary case: §9 stands exactly
// as written when no witness is asked.
func TestNoWitnessLeavesTheResultAlone(t *testing.T) {
	res, err := witnessed(t, witnessLog(t), nil, "")
	if err != nil {
		t.Fatalf("Witnessed: %v", err)
	}
	if res.Status != StatusValid || res.Witness != nil {
		t.Errorf("result = %+v, want VALID with no witness", res)
	}
}

// TestWitnessDiagnosticsAreOptional covers the quiet halves of both
// scripts: a witness that says nothing on stderr must still produce a
// readable reason, and a failure with no diagnostic must not grow an empty
// parenthesis.
func TestWitnessDiagnosticsAreOptional(t *testing.T) {
	t.Setenv("PROBAVI_TEST_QUIET", "1")
	log := witnessLog(t)

	t.Run("a silent refusal", func(t *testing.T) {
		armWitness(t, Head{Seq: 9, Hash: GenesisPrevHash}, "")
		res, err := witnessed(t, log, nil, witnessExpect)
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
		_, err := witnessed(t, log, nil, witnessFail)
		if !errors.Is(err, ErrWitnessFailed) {
			t.Fatalf("err = %v, want %v", err, ErrWitnessFailed)
		}
		if strings.Contains(err.Error(), "()") {
			t.Errorf("error %q carries an empty diagnostic", err)
		}
	})
}

// TestAttestedAtIsWhatTheWitnessSaid: the field carries the witness's own
// words, and a line that is not an RFC 3339 instant reads as attested with
// the time unstated rather than as a failure — the exit status already
// carried the answer.
func TestAttestedAtIsWhatTheWitnessSaid(t *testing.T) {
	log := witnessLog(t)
	head := headOf(t, log)
	tests := []struct {
		name, printed, want string
	}{
		{"an RFC 3339 instant", "2026-09-27T09:00:00Z", "2026-09-27T09:00:00Z"},
		{"an instant with an offset", "2026-09-27T11:00:00+02:00", "2026-09-27T11:00:00+02:00"},
		{"nothing at all", "", ""},
		{"prose", "attested yesterday", ""},
		{"a date with no time", "2026-09-27", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			armWitness(t, head, tc.printed)
			res, err := witnessed(t, log, nil, witnessExpect)
			if err != nil {
				t.Fatalf("Witnessed: %v", err)
			}
			if res.Witness == nil || !res.Witness.Attested {
				t.Fatalf("witness = %+v, want attested", res.Witness)
			}
			switch {
			case tc.want == "" && res.Witness.AttestedAt != nil:
				t.Errorf("attested_at = %q, want nil", *res.Witness.AttestedAt)
			case tc.want != "" && (res.Witness.AttestedAt == nil || *res.Witness.AttestedAt != tc.want):
				t.Errorf("attested_at = %v, want %q", res.Witness.AttestedAt, tc.want)
			}
		})
	}
}
