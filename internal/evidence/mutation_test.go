package evidence

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// TestATimestampOutsideTheRecordedFormIsRefused: the schema states RFC
// 3339 UTC at exactly millisecond precision, and every other rendering of
// the same instant is refused — an offset, a different fraction width, a
// lower-case zone. Two readers must write a record's instants the same
// way, or the canonical bytes they hash differ.
//
// The guard behind this has a second half — an instant that parses and
// then formats differently — which the strict layout leaves no input for.
// It is defensive depth rather than a reachable branch: a mutation run
// finds no test for it because none can exist while the layout stays
// literal about its zone and its fraction.
func TestATimestampOutsideTheRecordedFormIsRefused(t *testing.T) {
	for name, ts := range map[string]string{
		"a positive offset":                "2026-07-30T16:32:00.000+02:00",
		"a negative offset":                "2026-07-30T10:32:00.000-04:00",
		"zulu spelled as an offset":        "2026-07-30T14:32:00.000+00:00",
		"a lower-case zone":                "2026-07-30T14:32:00.000z",
		"microsecond precision":            "2026-07-30T14:32:00.482123Z",
		"a two-digit fraction":             "2026-07-30T14:32:00.48Z",
		"no fraction at all":               "2026-07-30T14:32:00Z",
		"a trailing space":                 "2026-07-30T14:32:00.000Z ",
		"a day the calendar does not have": "2026-02-30T14:32:00.000Z",
	} {
		t.Run(name, func(t *testing.T) {
			for field, mutate := range map[string]func(r *Record){
				"backup.created_at": func(r *Record) { r.Backup.CreatedAt = strPtr(ts) },
				"drill.pitr_target": func(r *Record) { r.Drill.PITRTarget = strPtr(ts) },
				"ts":                func(r *Record) { r.TS = ts },
			} {
				rec := sampleRecordPass()
				mutate(rec)
				if err := rec.Validate(); !errors.Is(err, ErrInvalidRecord) {
					t.Errorf("%s = %q: Validate = %v, want ErrInvalidRecord", field, ts, err)
				}
			}
		})
	}
}

// TestEveryEnvFieldIsRequiredOnItsOwn: the environment fingerprint is what
// ties a record to the machine that wrote it, so each of its three
// required fields has to be refused by itself rather than only together.
func TestEveryEnvFieldIsRequiredOnItsOwn(t *testing.T) {
	for field, blank := range map[string]func(e *Env){
		"probavi_version": func(e *Env) { e.ProbaviVersion = "" },
		"os":              func(e *Env) { e.OS = "" },
		"arch":            func(e *Env) { e.Arch = "" },
	} {
		t.Run(field, func(t *testing.T) {
			rec := sampleRecordPass()
			blank(&rec.Env)
			if err := rec.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("env.%s empty: Validate = %v, want ErrInvalidRecord", field, err)
			}
		})
	}
}

// TestZeroIsAMeasurementAndNegativeIsNot: a backup of no bytes and a phase
// that took no measurable time are both things a drill records; only a
// negative number is impossible. A guard that refused zero would refuse
// honest records.
func TestZeroIsAMeasurementAndNegativeIsNot(t *testing.T) {
	zero := int64(0)
	for name, tc := range map[string]struct {
		mutate func(r *Record)
		valid  bool
	}{
		"a backup of no bytes":      {func(r *Record) { r.Backup.SizeBytes = &zero }, true},
		"a phase that took no time": {func(r *Record) { r.Timings.Transfer = &zero }, true},
		"every phase at zero": {func(r *Record) {
			r.Timings = Timings{
				Provision: &zero, EngineReady: &zero, Transfer: &zero,
				Restore: &zero, Validate: &zero, Total: &zero,
			}
		}, true},
		"a backup of negative bytes": {func(r *Record) { r.Backup.SizeBytes = i64Ptr(-1) }, false},
		"a phase of negative time":   {func(r *Record) { r.Timings.Transfer = i64Ptr(-1) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			rec := sampleRecordPass()
			tc.mutate(rec)
			err := rec.Validate()
			if tc.valid && err != nil {
				t.Errorf("Validate = %v, want the record accepted", err)
			}
			if !tc.valid && !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("Validate = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

// TestARecordAtTheSizeLimitIsStillWritten: the limit is the largest record
// the store accepts, not the first one it refuses. A record exactly at it
// must be written, because an operator who sized their checks against the
// documented number would otherwise lose a drill's evidence.
func TestARecordAtTheSizeLimitIsStillWritten(t *testing.T) {
	// The padding lands in a sandbox parameter, where one added byte is
	// one canonical byte: no escaping, no re-encoding. The limit governs
	// the stored line, which also carries what the store itself adds —
	// the sequence number, the previous hash and the signature — so the
	// fixture is sized against a line the store actually wrote.
	sized := func(pad int) *Record {
		rec := sampleRecordPass()
		rec.Sandbox.Params = map[string]string{"image": "postgres:16", "probavi_pad": strings.Repeat("p", pad)}
		return rec
	}
	path := filepath.Join(t.TempDir(), "evidence.jsonl")
	st, err := Open(path, testSigner(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Append(sized(0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	pad := MaxRecordBytes - len(lastLine(t, path))

	if err := st.Append(sized(pad)); err != nil {
		t.Errorf("Append of a record exactly at the limit: %v, want it written", err)
	}
	if got := len(lastLine(t, path)); got != MaxRecordBytes {
		t.Errorf("the stored line is %d bytes, want exactly the %d-byte limit", got, MaxRecordBytes)
	}
	if err := st.Append(sized(pad + 1)); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("Append of a record one byte over: %v, want ErrRecordTooLarge", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// lastLine returns the log's final stored line without its terminator.
func lastLine(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	return lines[len(lines)-1]
}

// TestASignatureOfTheWrongLengthIsRefused: sig_b64 that decodes cleanly is
// not a signature. Ed25519 verification would refuse it anyway, but the
// verifier says what is wrong with the record rather than reporting a
// failed signature check over bytes that were never one.
func TestASignatureOfTheWrongLengthIsRefused(t *testing.T) {
	path := buildLog(t)
	for name, sig := range map[string][]byte{
		"one byte short":  make([]byte, ed25519.SignatureSize-1),
		"one byte long":   make([]byte, ed25519.SignatureSize+1),
		"no bytes at all": {},
	} {
		t.Run(name, func(t *testing.T) {
			lines := logLines(t, path)
			lines[0] = mutateLine(t, lines[0], func(m map[string]any) {
				asMap(t, m["sig"])["sig_b64"] = base64.StdEncoding.EncodeToString(sig)
			})
			res, err := Verify(strings.NewReader(strings.Join(lines, "\n")+"\n"), testKeyring(), nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Status != StatusInvalid || !strings.Contains(res.Reason, "malformed sig.sig_b64") {
				t.Errorf("Verify = %s (%q), want INVALID naming the signature", res.Status, res.Reason)
			}
		})
	}
}

// TestATornTailOfOneByteIsDamage: a crash can leave any number of bytes
// behind, one included. The verdict is the same at every length — the log
// is valid with damage — because the alternative is a single stray byte
// reading as a clean log.
func TestATornTailOfOneByteIsDamage(t *testing.T) {
	raw, err := os.ReadFile(buildLog(t))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for name, tail := range map[string]string{
		"one byte":   "{",
		"two bytes":  `{"`,
		"a fragment": `{"partial":`,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Verify(strings.NewReader(string(raw)+tail), testKeyring(), nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Status != StatusValidWithDamage {
				t.Fatalf("Verify = %s, want VALID_WITH_DAMAGE", res.Status)
			}
			if len(res.DamagedLines) != 1 || res.DamagedLines[0] != 4 {
				t.Errorf("DamagedLines = %v, want [4]", res.DamagedLines)
			}
		})
	}
}

// TestTruncateLineAtABudgetOfOne: the smallest budget that is not zero
// still has to answer within it, and the answer is as much of the ellipsis
// as fits.
func TestTruncateLineAtABudgetOfOne(t *testing.T) {
	if got := TruncateLine("abcd", 1); got != "." {
		t.Errorf("TruncateLine(%q, 1) = %q, want %q", "abcd", got, ".")
	}
}

// TestCanonicalOrderHoldsWhereOneKeyIsAPrefixOfAnother: RFC 8785 orders by
// UTF-16 code units, and where one name runs out first the shorter one
// comes first. The pair is worth its own case because it is the only shape
// that walks one name past its end — the boundary a comparison loop gets
// wrong without ever being noticed by a log whose keys all differ early.
func TestCanonicalOrderHoldsWhereOneKeyIsAPrefixOfAnother(t *testing.T) {
	got, err := Canonicalize(map[string]any{
		"ab": json.Number("2"), "a": json.Number("1"), "abc": json.Number("3"), "b": json.Number("4"),
	})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if want := `{"a":1,"ab":2,"abc":3,"b":4}`; string(got) != want {
		t.Errorf("Canonicalize = %s, want %s", got, want)
	}
}

// TestLessUTF16AtItsBoundaries: the comparison behind the ordering above,
// called directly, because through Canonicalize its boundaries cannot be
// reached on purpose.
//
// The sort decides which name is the comparison's first argument and which
// its second, and the order the names reach the sort in is a map's
// iteration order, which Go randomises. So a case that walks one name past
// its end is reached, or not, by whatever permutation a run happened to
// get. Measured with the loop's second bound mutated and the whole package
// run 25 times: caught 21 times, survived 4 — and the scheduled Mutation
// workflow reported it as a survivor on a tree where a local run of the
// same sweep had caught it.
//
// Getting this bound wrong is not a misordering. It indexes one past the
// end of a slice, which panics inside the canonicalizer — the signing path
// — so a record whose `sandbox.params` happen to name two keys where one
// is a prefix of the other would intermittently produce no record at all.
// Those keys come from user configuration, which is why the schema says
// they MUST be compared by the RFC rule rather than assumed to be ASCII
// identifiers (docs/evidence-schema.md §4).
func TestLessUTF16AtItsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		// Either name running out first is a bound of its own, and the
		// argument order is what picks which one the loop reaches.
		{"", "a", true},
		{"a", "", false},
		{"a", "ab", true},
		{"ab", "a", false},
		// Neither runs out and no unit differs. A JSON object cannot hold
		// the pair, but the comparison still has to be a strict order or
		// the sort above is not well defined.
		{"a", "a", false},
		// Why the rule is UTF-16 code units and not bytes: U+10000 encodes
		// as the surrogate 0xd800, below U+FFFF, while its UTF-8 bytes
		// (f0 90 80 80) sort above U+FFFF's (ef bf bf).
		{"\U00010000", "￿", true},
	} {
		if got := lessUTF16(tc.a, tc.b); got != tc.want {
			t.Errorf("lessUTF16(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
