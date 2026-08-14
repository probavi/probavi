package evidence

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// fuzz_test.go fuzzes the two places where hostile bytes meet trust-critical
// code: the log parser an auditor runs, and the canonicalizer whose output is
// what a signature actually covers.
//
// The targets assert properties, not merely the absence of panics. A parser
// that crashes on a malformed log is a denial of verification; a parser that
// returns a *self-contradicting verdict* is worse, because the contradiction
// is invisible to whoever reads the answer.

// publishedExamples returns the worked-example logs the schema publishes as
// conformance vectors, plus the public key that signed them. Seeding from
// them starts the fuzzer at valid input and lets it mutate outward, which is
// the only way it reaches the interesting states inside a signed chain.
func publishedExamples(t testing.TB) (logs [][]byte, keyring Keyring) {
	t.Helper()
	dir := filepath.Join("..", "..", "docs", "schemas", "evidence", "examples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		logs = append(logs, raw)
	}
	if len(logs) == 0 {
		t.Fatal("no published example logs — the seed corpus would be empty")
	}
	pub, err := LoadPublicKey(filepath.Join(dir, "signer.pub"))
	if err != nil {
		t.Fatalf("load published key: %v", err)
	}
	return logs, NewKeyring(pub)
}

// FuzzVerify drives the log parser over arbitrary bytes.
//
// The invariant is that the verdict has to agree with itself: VALID means
// every line parsed and verified, so it cannot coexist with a damaged line
// or a failing one. INVALID has to name the line it rejected, or an auditor
// is told "no" with nowhere to look.
//
// What VALID does *not* imply is that anything was verified. An empty log
// verifies (schema §9: "VALID otherwise" — no lines, no damage, no failed
// assertion), and that is the right answer to the question the verifier
// asks, which is whether the file was tampered with rather than whether
// drills were run. The record count is what separates the two, and this
// target asserts the boundary rather than a stricter rule the specification
// does not make.
func FuzzVerify(f *testing.F) {
	logs, keyring := publishedExamples(f)
	for _, raw := range logs {
		f.Add(raw)
	}
	f.Add([]byte(""))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"schema":"probavi-evidence/1"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := Verify(bytes.NewReader(data), keyring)
		if err != nil {
			// Verify reports I/O problems only; a bytes.Reader has none.
			t.Fatalf("Verify returned an I/O error over an in-memory reader: %v", err)
		}
		assertVerdictIsSelfConsistent(t, res)
	})
}

// assertVerdictIsSelfConsistent checks a Result against the meaning of its
// own status. It is deliberately separate from the fuzz body so the same
// rules can be applied to any verdict, however it was produced.
func assertVerdictIsSelfConsistent(t *testing.T, res *Result) {
	t.Helper()
	switch res.Status {
	case StatusValid:
		if len(res.DamagedLines) != 0 || res.FailedLine != 0 {
			t.Fatalf("VALID carrying damage %v or a failing line %d: %+v",
				res.DamagedLines, res.FailedLine, res)
		}
	case StatusValidWithDamage:
		if len(res.DamagedLines) == 0 {
			t.Fatalf("VALID_WITH_DAMAGE naming no damaged line: %+v", res)
		}
		if res.FailedLine != 0 {
			t.Fatalf("VALID_WITH_DAMAGE with a failing line %d: %+v", res.FailedLine, res)
		}
	case StatusInvalid:
		if res.FailedLine == 0 {
			t.Fatalf("INVALID naming no line — an auditor is told no with nowhere to look: %+v", res)
		}
		if res.Reason == "" {
			t.Fatalf("INVALID with no reason: %+v", res)
		}
	default:
		t.Fatalf("unknown status %d", res.Status)
	}
	for _, line := range res.DamagedLines {
		if line <= 0 {
			t.Fatalf("damaged line number %d is not 1-based: %+v", line, res)
		}
	}
}

// FuzzVerifyWithoutKeys pins the verdict when no key can vouch for the log.
// The empty keyring is what a third party has before they are given one, and
// the answer must never be "valid" — an unsigned chain that verified would
// make every signature in the format decorative.
func FuzzVerifyWithoutKeys(f *testing.F) {
	logs, _ := publishedExamples(f)
	for _, raw := range logs {
		f.Add(raw)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := Verify(bytes.NewReader(data), Keyring{})
		if err != nil {
			t.Fatalf("Verify returned an I/O error over an in-memory reader: %v", err)
		}
		if res.Status == StatusValid && res.Records > 0 {
			t.Fatalf("records verified with an empty keyring: %+v", res)
		}
	})
}

// FuzzCanonicalize asserts the property the signature layer rests on: the
// canonical form is a fixed point. Signing happens over these bytes, so if
// canonicalizing them again could produce something else, two parties
// holding the same record could compute two different digests and disagree
// about a signature neither of them altered.
func FuzzCanonicalize(f *testing.F) {
	f.Add([]byte(`{"b":1,"a":2}`))
	f.Add([]byte(`{"a":{"z":[1,2,3],"y":null},"b":"é"}`))
	f.Add([]byte(`[{"k":"v"},{"k":"w"}]`))
	f.Add([]byte(`"plain string"`))
	f.Add([]byte(`{"unicode":"😀 é ő"}`))
	f.Add([]byte(`{"nested":{"deep":{"deeper":{"deepest":0}}}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := decodeStrict(data)
		if err != nil {
			return // not a JSON value; nothing to canonicalize
		}
		once, err := Canonicalize(v)
		if err != nil {
			return // outside the schema's restriction (non-integer numbers)
		}

		reparsed, err := decodeStrict(once)
		if err != nil {
			t.Fatalf("canonical output does not parse as JSON: %v (%q)", err, once)
		}
		twice, err := Canonicalize(reparsed)
		if err != nil {
			t.Fatalf("canonical output is not itself canonicalizable: %v (%q)", err, once)
		}
		if !bytes.Equal(once, twice) {
			t.Fatalf("canonicalization is not a fixed point:\n once: %q\ntwice: %q", once, twice)
		}
	})
}
