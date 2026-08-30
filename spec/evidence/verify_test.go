package evidence

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// examplesDir holds the published worked example of evidence-schema.md §11:
// byte-frozen 3-record logs for both schema versions plus the signer's
// public key. These files are the contract surface between the Probavi core
// and this package. The core pins that it writes exactly these bytes; this
// package pins that an implementation which has never seen the core's code
// accepts exactly these bytes. Neither imports the other — if the two ever
// drift, one of the two test suites goes red.
const examplesDir = "../../docs/schemas/evidence/examples"

func exampleKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(examplesDir, "signer.pub"))
	if err != nil {
		t.Fatalf("read committed public key: %v", err)
	}
	pub, err := ParsePublicKey(data)
	if err != nil {
		t.Fatalf("parse committed public key: %v", err)
	}
	return pub
}

func exampleLog(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(examplesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func verifyBytes(t *testing.T, log []byte, kr Keyring) Result {
	t.Helper()
	res, err := Verify(bytes.NewReader(log), kr)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return res
}

// TestPublishedExamplesVerify is the headline claim of this package: a
// verifier written from the specification alone, sharing no code with the
// producer, accepts the published logs of every schema version using nothing
// but the committed public key — exactly the position an external auditor
// is in.
func TestPublishedExamplesVerify(t *testing.T) {
	kr := NewKeyring(exampleKey(t))
	for _, name := range []string{"log_v0.jsonl", "log_v1.jsonl", "log_v2.jsonl"} {
		t.Run(name, func(t *testing.T) {
			res := verifyBytes(t, exampleLog(t, name), kr)
			if res.Status != StatusValid {
				t.Fatalf("status = %s (%s at line %d), want VALID", res.Status, res.Reason, res.Line)
			}
			if res.Records != 3 {
				t.Errorf("records = %d, want 3", res.Records)
			}
			if len(res.DamagedLines) != 0 {
				t.Errorf("damaged lines = %v, want none", res.DamagedLines)
			}
		})
	}
}

// TestCommittedKeyIDMatchesRecords derives the §6 key identifier here and
// compares it with the key_id the producer wrote into every record. A
// mismatch would mean the two implementations disagree about how a key is
// named, which would silently break keyring lookup after a rotation.
func TestCommittedKeyIDMatchesRecords(t *testing.T) {
	want := KeyID(exampleKey(t))
	for _, name := range []string{"log_v0.jsonl", "log_v1.jsonl", "log_v2.jsonl"} {
		for i, line := range splitLines(t, exampleLog(t, name)) {
			var rec struct {
				Sig struct {
					KeyID string `json:"key_id"`
				} `json:"sig"`
			}
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatalf("%s line %d: %v", name, i+1, err)
			}
			if rec.Sig.KeyID != want {
				t.Errorf("%s line %d: key_id %s, derived %s", name, i+1, rec.Sig.KeyID, want)
			}
		}
	}
}

func splitLines(t *testing.T, log []byte) [][]byte {
	t.Helper()
	trimmed := bytes.TrimSuffix(log, []byte("\n"))
	if len(trimmed) == 0 {
		return nil
	}
	return bytes.Split(trimmed, []byte("\n"))
}

// joinLines rebuilds a log from lines, restoring the terminator the format
// requires on every line.
func joinLines(lines [][]byte) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.Write(l)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// TestTamperDetection walks the attacker model of §1: someone with write
// access to the log who wants to forge "everything was fine". Every mutation
// below must be rejected, and the reported reason must name the specific
// invariant that caught it — a verifier that rejects for the wrong reason is
// one refactor away from not rejecting at all.
func TestTamperDetection(t *testing.T) {
	kr := NewKeyring(exampleKey(t))
	original := exampleLog(t, "log_v1.jsonl")

	cases := []struct {
		name       string
		mutate     func(t *testing.T, log []byte) []byte
		wantReason string
	}{
		{
			name: "altered outcome",
			// Equal-length substitution: the record stays canonical JSON,
			// so only the signature can catch it.
			mutate: func(t *testing.T, log []byte) []byte {
				return mustReplace(t, log, `"outcome":"pass"`, `"outcome":"fail"`)
			},
			wantReason: "signature does not verify",
		},
		{
			name: "forged signature",
			mutate: func(t *testing.T, log []byte) []byte {
				lines := splitLines(t, log)
				lines[0] = flipBase64Char(t, lines[0])
				return joinLines(lines)
			},
			wantReason: "signature does not verify",
		},
		{
			name: "deleted middle record",
			mutate: func(t *testing.T, log []byte) []byte {
				lines := splitLines(t, log)
				return joinLines([][]byte{lines[0], lines[2]})
			},
			wantReason: "seq 3, want 2",
		},
		{
			name: "reordered records",
			mutate: func(t *testing.T, log []byte) []byte {
				lines := splitLines(t, log)
				return joinLines([][]byte{lines[1], lines[0], lines[2]})
			},
			wantReason: "seq 2, want 1",
		},
		{
			name: "duplicated record",
			mutate: func(t *testing.T, log []byte) []byte {
				lines := splitLines(t, log)
				return joinLines([][]byte{lines[0], lines[0], lines[1], lines[2]})
			},
			wantReason: "seq 1, want 2",
		},
		{
			name: "rewritten prev_hash",
			mutate: func(t *testing.T, log []byte) []byte {
				lines := splitLines(t, log)
				lines[1] = flipPrevHashChar(t, lines[1])
				return joinLines(lines)
			},
			wantReason: "prev_hash",
		},
		{
			// Re-encoding a record with encoding/json is deliberately NOT
			// used as a mutation here: for this fixture Go's marshaller
			// happens to emit the canonical bytes exactly (ASCII keys,
			// integral numbers, nothing it would HTML-escape), so it is a
			// no-op. The two cases below break canonical form for real.
			name: "keys reordered within an object",
			mutate: func(t *testing.T, log []byte) []byte {
				return mustReplace(t, log,
					`{"name":"postgres","protocol":"probavi-adapter/0","version":"0.1.0"}`,
					`{"protocol":"probavi-adapter/0","name":"postgres","version":"0.1.0"}`)
			},
			wantReason: "stored bytes are not the canonical serialization of the record",
		},
		{
			name: "insignificant whitespace inserted",
			mutate: func(t *testing.T, log []byte) []byte {
				return mustReplace(t, log, `{"adapter":`, `{ "adapter":`)
			},
			wantReason: "stored bytes are not the canonical serialization of the record",
		},
		{
			name: "unsupported schema version",
			mutate: func(t *testing.T, log []byte) []byte {
				return mustReplace(t, log, `"probavi-evidence/1"`, `"probavi-evidence/9"`)
			},
			wantReason: `unsupported schema "probavi-evidence/9"`,
		},
		{
			name: "fractional number",
			mutate: func(t *testing.T, log []byte) []byte {
				return mustReplace(t, log, `"size_bytes":565248`, `"size_bytes":56524.8`)
			},
			wantReason: "is not an integer literal",
		},
		{
			name: "seq tampered",
			mutate: func(t *testing.T, log []byte) []byte {
				return mustReplace(t, log, `"seq":1,`, `"seq":7,`)
			},
			wantReason: "seq 7, want 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := verifyBytes(t, tc.mutate(t, append([]byte(nil), original...)), kr)
			if res.Status != StatusInvalid {
				t.Fatalf("status = %s, want INVALID", res.Status)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
		})
	}
}

// TestUnknownSigningKeyRejected covers the keyring miss of §9: a log signed
// by a key the verifier was not given is INVALID, never "probably fine".
func TestUnknownSigningKeyRejected(t *testing.T) {
	stranger, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	res := verifyBytes(t, exampleLog(t, "log_v1.jsonl"), NewKeyring(stranger))
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", res.Status)
	}
	if !strings.Contains(res.Reason, "no public key in the keyring") {
		t.Errorf("reason = %q, want a keyring miss", res.Reason)
	}
}

// TestTailTruncationIsNotDetectable pins a documented limitation rather than
// a defect: §1 keeps truncation at the end out of what the file alone
// proves, and §9 says why no file-only algorithm can do better — a valid
// prefix of a log is itself a valid log. What closes it is an input, not a
// better algorithm: hand the verifier the anchor of §9.1 and these same
// bytes are INVALID (TestAnchorConformanceVectors). This test must keep
// passing unchanged, because the anchor is only worth having for as long as
// the file alone still cannot tell.
func TestTailTruncationIsNotDetectable(t *testing.T) {
	lines := splitLines(t, exampleLog(t, "log_v1.jsonl"))
	res := verifyBytes(t, joinLines(lines[:2]), NewKeyring(exampleKey(t)))
	if res.Status != StatusValid || res.Records != 2 {
		t.Fatalf("truncated log = %s with %d records, want VALID with 2", res.Status, res.Records)
	}
}

// TestTornTailIsDamageNotTampering covers §2: a crash mid-write leaves an
// unterminated fragment. It must be reported, and it must not be confused
// with forgery — signed content cannot be altered this way.
func TestTornTailIsDamageNotTampering(t *testing.T) {
	log := append(exampleLog(t, "log_v1.jsonl"), []byte(`{"schema":"probavi-ev`)...)
	res := verifyBytes(t, log, NewKeyring(exampleKey(t)))
	if res.Status != StatusValidDamage {
		t.Fatalf("status = %s, want VALID_WITH_DAMAGE", res.Status)
	}
	if res.Records != 3 {
		t.Errorf("records = %d, want 3", res.Records)
	}
	if len(res.DamagedLines) != 1 || res.DamagedLines[0] != 4 {
		t.Errorf("damaged lines = %v, want [4]", res.DamagedLines)
	}
}

// TestDamageDoesNotAdvanceTheChain covers the "continue" of §9: an
// unparseable line is skipped without consuming a sequence number, so the
// records after it still chain to the last valid one.
func TestDamageDoesNotAdvanceTheChain(t *testing.T) {
	lines := splitLines(t, exampleLog(t, "log_v1.jsonl"))
	withGarbage := joinLines([][]byte{lines[0], []byte("not json at all"), lines[1], lines[2]})
	res := verifyBytes(t, withGarbage, NewKeyring(exampleKey(t)))
	if res.Status != StatusValidDamage {
		t.Fatalf("status = %s (%s), want VALID_WITH_DAMAGE", res.Status, res.Reason)
	}
	if res.Records != 3 {
		t.Errorf("records = %d, want 3", res.Records)
	}
	if len(res.DamagedLines) != 1 || res.DamagedLines[0] != 2 {
		t.Errorf("damaged lines = %v, want [2]", res.DamagedLines)
	}
}

func TestEmptyLogIsValid(t *testing.T) {
	res := verifyBytes(t, nil, NewKeyring(exampleKey(t)))
	if res.Status != StatusValid || res.Records != 0 {
		t.Fatalf("empty log = %s with %d records, want VALID with 0", res.Status, res.Records)
	}
}

// TestOversizedRecordRejected covers the 64 KiB canonical ceiling of §4.
func TestOversizedRecordRejected(t *testing.T) {
	huge := map[string]any{
		"schema": "probavi-evidence/1",
		"pad":    strings.Repeat("x", maxCanonicalBytes+1),
	}
	line, err := json.Marshal(huge)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res := verifyBytes(t, append(line, '\n'), NewKeyring(exampleKey(t)))
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", res.Status)
	}
	if !strings.Contains(res.Reason, "exceeds the 65536 byte limit") {
		t.Errorf("reason = %q, want the size limit", res.Reason)
	}
}

func TestGenesisPrevHashEnforced(t *testing.T) {
	lines := splitLines(t, exampleLog(t, "log_v1.jsonl"))
	// Starting a log at its second record leaves prev_hash pointing at a
	// predecessor that is not there.
	res := verifyBytes(t, joinLines(lines[1:]), NewKeyring(exampleKey(t)))
	if res.Status != StatusInvalid {
		t.Fatalf("status = %s, want INVALID", res.Status)
	}
	if !strings.Contains(res.Reason, "seq 2, want 1") {
		t.Errorf("reason = %q, want the sequence check to fire", res.Reason)
	}
}

// mustReplace performs an exact substitution and fails the test if the
// needle was absent, so a fixture change can never silently turn a mutation
// case into a no-op that "passes".
func mustReplace(t *testing.T, log []byte, old, replacement string) []byte {
	t.Helper()
	if !bytes.Contains(log, []byte(old)) {
		t.Fatalf("fixture does not contain %q — the mutation would be a no-op", old)
	}
	return bytes.Replace(log, []byte(old), []byte(replacement), 1)
}

// flipBase64Char changes one character of sig_b64 to a different valid
// base64 character, forging a signature that is well-formed but wrong.
func flipBase64Char(t *testing.T, line []byte) []byte {
	t.Helper()
	marker := []byte(`"sig_b64":"`)
	i := bytes.Index(line, marker)
	if i < 0 {
		t.Fatal("fixture has no sig_b64 field")
	}
	pos := i + len(marker)
	out := append([]byte(nil), line...)
	if out[pos] == 'A' {
		out[pos] = 'B'
	} else {
		out[pos] = 'A'
	}
	return out
}

// flipPrevHashChar rewrites one hex digit of prev_hash, the mutation an
// attacker would need to make a deletion or reordering chain up again.
func flipPrevHashChar(t *testing.T, line []byte) []byte {
	t.Helper()
	marker := []byte(`"prev_hash":"sha256:`)
	i := bytes.Index(line, marker)
	if i < 0 {
		t.Fatal("fixture has no prev_hash field")
	}
	pos := i + len(marker)
	out := append([]byte(nil), line...)
	if out[pos] == '0' {
		out[pos] = '1'
	} else {
		out[pos] = '0'
	}
	return out
}
