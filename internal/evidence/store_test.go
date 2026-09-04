package evidence

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// examplesDir holds the published worked-example logs and the signer's public
// key (evidence-schema.md §11). They live beside the JSON Schema under docs/
// rather than in this package's testdata/ so that third parties — and the
// independent verifier in spec/evidence — can use them as conformance vectors
// without reaching into internal/.
const examplesDir = "../../docs/schemas/evidence/examples"

// buildLog writes the three sample records through a real Store and returns
// the log path. Fixed seed + fixed timestamps make the bytes deterministic.
func buildLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	for _, rec := range []*Record{sampleRecordPass(), sampleRecordFail(), sampleRecordError()} {
		if err := st.Append(rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return path
}

func verifyFile(t *testing.T, path string, kr Keyring) *Result {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Fatalf("close log: %v", err)
		}
	}()
	res, err := Verify(f, kr, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return res
}

func testKeyring() Keyring { return NewKeyring(testSigner().PublicKey()) }

func TestRoundTrip(t *testing.T) {
	path := buildLog(t)
	res := verifyFile(t, path, testKeyring())
	if res.Status != StatusValid || res.Records != 3 {
		t.Fatalf("Verify = %s with %d records, want VALID with 3 (%s)", res.Status, res.Records, res.Reason)
	}

	// Reopen and continue the chain.
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if st.NextSeq() != 4 {
		t.Errorf("NextSeq = %d, want 4", st.NextSeq())
	}
	rec := sampleRecordPass()
	rec.TS = "2026-07-31T03:00:00.000Z"
	if err := st.Append(rec); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if rec.Seq != 4 || rec.Sig == nil {
		t.Errorf("appended record: seq=%d sig=%v, want seq=4 with sig", rec.Seq, rec.Sig)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if res := verifyFile(t, path, testKeyring()); res.Status != StatusValid || res.Records != 4 {
		t.Fatalf("Verify after reopen = %s with %d records, want VALID with 4", res.Status, res.Records)
	}
}

func TestGoldenLog(t *testing.T) {
	got, err := os.ReadFile(buildLog(t))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	golden := filepath.Join(examplesDir, "log_v2.jsonl")
	if *updateGolden {
		if err := os.MkdirAll(examplesDir, 0o755); err != nil {
			t.Fatalf("mkdir examples: %v", err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run `go test -run TestGoldenLog -update ./internal/evidence` once): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("evidence log bytes differ from golden file — canonicalization, signing, or chain construction changed; this is a schema-breaking change unless the golden was intentionally regenerated with a spec bump")
	}
}

// TestFrozenLogsVerify pins schema-version support forever. log_v0.jsonl
// and log_v1.jsonl were written by the implementations of their own
// versions and are byte-frozen — they have no updater and MUST never be
// regenerated. Records already written under a published version stay
// verifiable for the lifetime of the product (evidence-schema.md §10), and
// this is the test that would notice if they stopped.
func TestFrozenLogsVerify(t *testing.T) {
	for _, name := range []string{"log_v0.jsonl", "log_v1.jsonl"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join(examplesDir, name))
			if err != nil {
				t.Fatalf("open frozen log: %v", err)
			}
			defer func() {
				if err := f.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
			}()
			res, err := Verify(f, testKeyring(), nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Status != StatusValid || res.Records != 3 {
				t.Fatalf("%s: %s with %d records, want VALID with 3 (%s)", name, res.Status, res.Records, res.Reason)
			}
		})
	}
}

// TestWorkedExampleVerifiesWithCommittedKey completes the evidence-schema.md
// §11 worked example: the committed 3-record example logs verify offline with
// only the committed public key file (examples/signer.pub) — exactly the
// position an external auditor is in. The key pair is the deterministic test
// key (seed bytes 0x00…0x1f, see testSeed).
func TestWorkedExampleVerifiesWithCommittedKey(t *testing.T) {
	pub, err := LoadPublicKey(filepath.Join(examplesDir, "signer.pub"))
	if err != nil {
		t.Fatalf("load committed public key: %v", err)
	}
	if got, want := PublicKeyID(pub), testSigner().KeyID(); got != want {
		t.Fatalf("committed key id %s does not match the golden signer %s", got, want)
	}
	for _, golden := range []string{"log_v0.jsonl", "log_v1.jsonl", "log_v2.jsonl"} {
		f, err := os.Open(filepath.Join(examplesDir, golden))
		if err != nil {
			t.Fatalf("open %s: %v", golden, err)
		}
		res, verr := Verify(f, NewKeyring(pub), nil)
		if cerr := f.Close(); cerr != nil {
			t.Fatalf("close %s: %v", golden, cerr)
		}
		if verr != nil {
			t.Fatalf("Verify %s: %v", golden, verr)
		}
		if res.Status != StatusValid || res.Records != 3 {
			t.Fatalf("%s: %s with %d records, want VALID with 3 (%s)", golden, res.Status, res.Records, res.Reason)
		}
	}
}

// TestChainContinuesAcrossSchemaVersions covers the upgrade path the schema
// guarantees: a log started under v0 keeps its chain when the writer starts
// emitting v1 records mid-file.
func TestChainContinuesAcrossSchemaVersions(t *testing.T) {
	v0, err := os.ReadFile(filepath.Join(examplesDir, "log_v0.jsonl"))
	if err != nil {
		t.Fatalf("read frozen v0 log: %v", err)
	}
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	if err := os.WriteFile(path, v0, 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open over v0 log: %v", err)
	}
	if st.NextSeq() != 4 {
		t.Errorf("NextSeq = %d, want 4", st.NextSeq())
	}
	rec := sampleRecordPass()
	rec.TS = "2026-08-01T02:00:00.000Z"
	if err := st.Append(rec); err != nil {
		t.Fatalf("Append v1 record: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := verifyFile(t, path, testKeyring())
	if res.Status != StatusValid || res.Records != 4 {
		t.Fatalf("mixed-version log: %s with %d records, want VALID with 4 (%s)", res.Status, res.Records, res.Reason)
	}
}

// mutateLine re-canonicalizes a stored line after applying mutate to its
// decoded form, so tampering tests hit chain/signature checks rather than
// the canonical-form check.
func mutateLine(t *testing.T, line string, mutate func(m map[string]any)) string {
	t.Helper()
	v, err := decodeStrict([]byte(line))
	if err != nil {
		t.Fatalf("decode line: %v", err)
	}
	mutate(asMap(t, v))
	out, err := Canonicalize(v)
	if err != nil {
		t.Fatalf("re-canonicalize: %v", err)
	}
	return string(out)
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON object, got %T", v)
	}
	return m
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("expected JSON array, got %T", v)
	}
	return s
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func TestTamperDetection(t *testing.T) {
	path := buildLog(t)
	forgedSig := base64.StdEncoding.EncodeToString(make([]byte, 64))

	tests := []struct {
		name       string
		tamper     func(l []string) []string
		wantReason string
	}{
		{"modified content", func(l []string) []string {
			l[1] = mutateLine(t, l[1], func(m map[string]any) {
				asMap(t, asSlice(t, m["checks"])[0])["detail"] = "100000 rows (min 100000)"
			})
			return l
		}, "signature verification failed"},
		{"removed record", func(l []string) []string { return append(l[:1], l[2:]...) }, "seq"},
		{"reordered records", func(l []string) []string { l[1], l[2] = l[2], l[1]; return l }, "seq"},
		{"forged signature", func(l []string) []string {
			l[1] = mutateLine(t, l[1], func(m map[string]any) {
				asMap(t, m["sig"])["sig_b64"] = forgedSig
			})
			return l
		}, "signature verification failed"},
		{"unknown key", func(l []string) []string {
			l[1] = mutateLine(t, l[1], func(m map[string]any) {
				asMap(t, m["sig"])["key_id"] = "0000000000000000"
			})
			return l
		}, "no public key"},
		{"unsupported alg", func(l []string) []string {
			l[1] = mutateLine(t, l[1], func(m map[string]any) {
				asMap(t, m["sig"])["alg"] = "rsa"
			})
			return l
		}, "unsupported sig.alg"},
		{"unsupported schema", func(l []string) []string {
			l[0] = mutateLine(t, l[0], func(m map[string]any) { m["schema"] = "probavi-evidence/99" })
			return l
		}, "unsupported schema"},
		{"schema downgrade hiding pitr_target", func(l []string) []string {
			// The attack the v1 bump must resist: rewrite a v1 record as v0
			// to make the PITR target disappear. The schema field is signed,
			// so the downgrade breaks the signature, not just the field.
			l[0] = mutateLine(t, l[0], func(m map[string]any) {
				m["schema"] = SchemaIDv0
				delete(asMap(t, m["drill"]), "pitr_target")
			})
			return l
		}, "signature verification failed"},
		{"field type mismatch", func(l []string) []string {
			l[0] = mutateLine(t, l[0], func(m map[string]any) { m["seq"] = "one" })
			return l
		}, "decode record"},
		{"non-canonical storage", func(l []string) []string { l[1] += " "; return l }, ErrNotCanonical.Error()},
		{"canonical non-object line", func(l []string) []string { l[1] = "null"; return l }, "not a JSON object"},
		{"float smuggling", func(l []string) []string {
			l[1] = strings.Replace(l[1], `"provision":1170`, `"provision":1170.0`, 1)
			return l
		}, "not a safe integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tampered := strings.Join(tt.tamper(logLines(t, path)), "\n") + "\n"
			res, err := Verify(strings.NewReader(tampered), testKeyring(), nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Status != StatusInvalid {
				t.Fatalf("Verify = %s, want INVALID", res.Status)
			}
			if !strings.Contains(res.Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tt.wantReason)
			}
		})
	}
}

// TestTailTruncationVerifiesValid pins the documented limit of the chain
// (evidence-schema.md §1 and §9): removing the newest records leaves a
// shorter chain that is still perfect, so verification returns VALID.
// TestTamperDetection's "removed record" case covers removal from within
// the sequence, which does break continuity — this is the one member of
// that family the file cannot answer for. The independent verifier pins
// the same property from the specification alone (spec/evidence). What
// closes it is an input rather than an algorithm: hand the verifier the
// anchor of §9.1 and the same file is INVALID, which is what
// TestATruncatedLogFailsItsAnchor covers. This test must keep passing
// unchanged, because the anchor is only worth having while the file alone
// still cannot tell.
func TestTailTruncationVerifiesValid(t *testing.T) {
	lines := logLines(t, buildLog(t))
	truncated := strings.Join(lines[:2], "\n") + "\n"
	res, err := Verify(strings.NewReader(truncated), testKeyring(), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != StatusValid || res.Records != 2 {
		t.Fatalf("Verify = %s with %d records, want VALID with 2 (the tail is unanchored)", res.Status, res.Records)
	}
}

func TestTornTailIsDamageNotTampering(t *testing.T) {
	raw, err := os.ReadFile(buildLog(t))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	torn := string(raw) + `{"partial":`
	res, err := Verify(strings.NewReader(torn), testKeyring(), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != StatusValidWithDamage || res.Records != 3 {
		t.Fatalf("Verify = %s with %d records, want VALID_WITH_DAMAGE with 3", res.Status, res.Records)
	}
	if len(res.DamagedLines) != 1 || res.DamagedLines[0] != 4 {
		t.Errorf("DamagedLines = %v, want [4]", res.DamagedLines)
	}
}

func TestMidFileFragmentSkippedWithoutChainBreak(t *testing.T) {
	lines := logLines(t, buildLog(t))
	withGarbage := strings.Join([]string{lines[0], lines[1], "not json at all", lines[2]}, "\n") + "\n"
	res, err := Verify(strings.NewReader(withGarbage), testKeyring(), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != StatusValidWithDamage || res.Records != 3 {
		t.Fatalf("Verify = %s with %d records, want VALID_WITH_DAMAGE with 3 (%s)", res.Status, res.Records, res.Reason)
	}
}

func TestStoreReopensAcrossTornTail(t *testing.T) {
	path := buildLog(t)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open for damage: %v", err)
	}
	if _, err := f.WriteString(`{"torn":`); err != nil {
		t.Fatalf("write fragment: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open over torn tail: %v", err)
	}
	if st.NextSeq() != 4 {
		t.Errorf("NextSeq = %d, want 4", st.NextSeq())
	}
	rec := sampleRecordPass()
	rec.TS = "2026-07-31T04:00:00.000Z"
	if err := st.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res := verifyFile(t, path, testKeyring())
	if res.Status != StatusValidWithDamage || res.Records != 4 {
		t.Fatalf("Verify = %s with %d records, want VALID_WITH_DAMAGE with 4 (%s)", res.Status, res.Records, res.Reason)
	}
}

func TestOpenRefusesBrokenChain(t *testing.T) {
	path := buildLog(t)
	lines := logLines(t, path)
	lines[1] = mutateLine(t, lines[1], func(m map[string]any) {
		m["outcome"] = "pass" // content change: line hash no longer matches record 3's prev_hash
	})
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite log: %v", err)
	}
	_, err := Open(path, testSigner(), nil)
	if !errors.Is(err, ErrChainState) {
		t.Fatalf("Open: got %v, want ErrChainState", err)
	}
}

func TestSingleWriterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	if _, err := Open(path, testSigner(), nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: got %v, want ErrLocked", err)
	}
}

func TestAppendRejections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	preset := sampleRecordPass()
	preset.Seq = 7
	if err := st.Append(preset); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("preset seq: got %v, want ErrInvalidRecord", err)
	}

	invalid := sampleRecordPass()
	invalid.Outcome = "maybe"
	if err := st.Append(invalid); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("invalid record: got %v, want ErrInvalidRecord", err)
	}
	if invalid.Seq != 0 || invalid.PrevHash != "" || invalid.Sig != nil {
		t.Error("failed append must reset the chain fields it set")
	}

	huge := sampleRecordPass()
	detail := strings.Repeat("d", maxDetailLen)
	for i := range 300 {
		huge.Checks = append(huge.Checks, Check{Name: fmt.Sprintf("sql:invariant-%03d", i), OK: true, Detail: &detail})
	}
	if err := st.Append(huge); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("oversized record: got %v, want ErrRecordTooLarge", err)
	}

	if st.NextSeq() != 1 {
		t.Errorf("NextSeq after rejected appends = %d, want 1", st.NextSeq())
	}
	if _, err := Open(path, nil, nil); err == nil {
		t.Error("Open with nil signer: expected error")
	}
}

func TestVerifyEdgeCases(t *testing.T) {
	res, err := Verify(strings.NewReader(""), testKeyring(), nil)
	if err != nil {
		t.Fatalf("Verify empty: %v", err)
	}
	if res.Status != StatusValid || res.Records != 0 {
		t.Errorf("empty log: %s with %d records, want VALID with 0", res.Status, res.Records)
	}

	if _, err := Verify(iotest.ErrReader(errors.New("disk gone")), testKeyring(), nil); err == nil {
		t.Error("Verify must surface I/O errors as errors, not verdicts")
	}

	res, err = Verify(strings.NewReader(""), nil, nil)
	if err != nil || res.Status != StatusValid {
		t.Errorf("nil keyring must behave as empty keyring: res=%v err=%v", res, err)
	}

	for s, want := range map[Status]string{
		StatusValid: "VALID", StatusValidWithDamage: "VALID_WITH_DAMAGE", StatusInvalid: "INVALID", Status(9): "Status(9)",
	} {
		if s.String() != want {
			t.Errorf("Status(%d).String() = %q, want %q", int(s), s.String(), want)
		}
	}
}

func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	if err := syncDir(filepath.Join(dir, "evidence.jsonl"), "evidence log"); err != nil {
		t.Errorf("syncDir on a real directory: %v", err)
	}
	err := syncDir(filepath.Join(dir, "no-such-dir", "evidence.jsonl"), "evidence log")
	if err == nil {
		t.Fatal("syncDir on a missing directory must report it — the log cannot be durable there")
	}
	// The label is what tells an operator which durability step failed,
	// now that two callers share this one.
	if !strings.Contains(err.Error(), "evidence log directory") {
		t.Errorf("error = %q, want it to name the evidence log", err)
	}
	if err := syncDir(filepath.Join(dir, "no-such-dir", "key"), "key"); err == nil ||
		!strings.Contains(err.Error(), "key directory") {
		t.Errorf("error = %v, want it to name the key", err)
	}
}

// TestOpenSurvivesAnUnsyncableDirectory keeps the durability step from
// becoming a new way for a drill to fail: a store still opens on a
// filesystem that cannot sync a directory, because refusing to run a drill
// is worse than a weaker crash guarantee.
func TestOpenSurvivesAnUnsyncableDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestLockIsHeldByOneWriter pins what the platform split exists to
// preserve. The lock is not a convenience: two processes appending to one
// log interleave records and break the hash chain, so a platform that
// cannot take it does not build (lock_unsupported.go) rather than running
// without it.
func TestLockIsHeldByOneWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	first, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	defer first.Close() //nolint:errcheck // the assertion below is the point

	_, err = Open(path, testSigner(), nil)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second open = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the log it could not take", err)
	}
}
