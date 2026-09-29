package evidence

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// TestTruncateLineWalksOutOfAStringOfContinuationBytes: the walk back to a
// rune boundary is bounded by `cut > 0`, and that bound is load-bearing.
// Every byte of a Go string can be a UTF-8 continuation byte, and the walk
// then reaches offset 0 with nothing else to stop it — without the bound it
// indexes s[-1] and panics inside the helper two record fields are built
// with.
//
// No shipped caller can deliver such a string today, and every mechanism
// that prevents it belongs to someone else: internal/checks composes its
// details from ASCII and keeps engine output out of them by the redaction
// rule, adapter-supplied text arrives through encoding/json, and
// sanitizeMessage runs strings.Map — and both of those coerce an invalid
// byte to U+FFFD rather than passing it on (measured, not assumed). So the
// bound is a promise to the next caller rather than a live defect, and it
// is worth keeping asserted: a panic is the one outcome that leaves no
// record at all, where a field the record layer rejects at least leaves a
// drill that failed for a stated reason.
//
// What the helper does not promise is repair. It refuses to split a rune;
// an input that was never valid UTF-8 stays invalid, and the last case
// below says so rather than leaving it to be assumed.
func TestTruncateLineWalksOutOfAStringOfContinuationBytes(t *testing.T) {
	const cont = "\x80" // 10xxxxxx: never the first byte of a rune
	for name, tc := range map[string]struct {
		in       string
		maxBytes int
		want     string
	}{
		// The walk starts at maxBytes-3 and finds no rune start below it,
		// so it arrives at 0 and keeps nothing. A budget of 4 is the
		// smallest that walks at all: below that the ellipsis path returns
		// first.
		"nothing but continuation bytes": {strings.Repeat(cont, 5), 4, ellipsis},
		"a longer run, a larger budget":  {strings.Repeat(cont, 40), 20, ellipsis},
		// A lead byte at offset 0 stops the walk by being a rune start, not
		// by the bound — the same result reached the other way.
		"a lead byte under the run": {"\xf0" + strings.Repeat(cont, 5), 4, ellipsis},
		// Repair is not on offer: the cut lands on a rune start, and a lone
		// lead byte is one.
		"a lone lead byte is kept": {"a\xc3" + "bcdefgh", 5, "a\xc3" + ellipsis},
	} {
		t.Run(name, func(t *testing.T) {
			got := TruncateLine(tc.in, tc.maxBytes)
			if got != tc.want {
				t.Errorf("TruncateLine(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
			}
		})
	}
}

// TestTruncateLineSurvivesEveryBudgetOnInvalidUTF8 is the property behind
// the cases above: whatever the bytes are, the helper returns and stays
// inside its budget. Validity is deliberately not asserted — that is the
// one promise invalid input does not get, and asserting it here would be
// asserting a repair the helper does not perform.
func TestTruncateLineSurvivesEveryBudgetOnInvalidUTF8(t *testing.T) {
	for _, s := range []string{
		"\x80\x80\x80\x80\x80\x80\x80\x80",
		"\xf0\x80\x80\x80\x80\x80\x80\x80",
		"a\x80b\x80c\x80d\x80",
		"\xc3\xc3\xc3\xc3\xc3\xc3",
		"\xff\xfe\xff\xfe\xff\xfe",
	} {
		for maxBytes := range len(s) + 4 {
			got := TruncateLine(s, maxBytes)
			if len(got) > maxBytes {
				t.Errorf("TruncateLine(%q, %d) returned %d bytes", s, maxBytes, len(got))
			}
		}
	}
}

// reopenWithLog opens an existing log again and returns what the store told
// its logger while resuming.
func reopenWithLog(t *testing.T, path string) string {
	t.Helper()
	buf := &bytes.Buffer{}
	st, err := Open(path, testSigner(), slog.New(slog.NewTextHandler(buf, nil)))
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.String()
}

// TestTheDamagedLineWarningFiresExactlyWhenThereIsDamage: a log carrying
// crash artifacts reopens successfully on purpose — the chain continues from
// the last valid record rather than refusing to append ever again — so this
// warning is the only thing that tells an operator the file is not what it
// was, and which lines to go and look at.
//
// Both directions matter, and neither was asserted. Warning about a log with
// nothing wrong teaches an operator to ignore the line, which costs the
// warning its meaning for the log that does have damage. Staying quiet about
// a single damaged line hides the commonest case there is: one interrupted
// append, which is exactly what a crash leaves behind.
func TestTheDamagedLineWarningFiresExactlyWhenThereIsDamage(t *testing.T) {
	t.Run("an intact log says nothing", func(t *testing.T) {
		if out := reopenWithLog(t, buildLog(t)); out != "" {
			t.Errorf("reopening an intact log logged %q, want silence", out)
		}
	})
	t.Run("one damaged line is named by its line number", func(t *testing.T) {
		path := buildLog(t)
		// Appending is how damage arrives: bytes after the last good record
		// that are not a record. Three were written, so this is line 4.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open log for append: %v", err)
		}
		if _, err := f.WriteString("this line is not a record\n"); err != nil {
			t.Fatalf("append damage: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close log: %v", err)
		}
		out := reopenWithLog(t, path)
		// The line number is asserted as the rendered attribute rather than
		// as a bare "4": every line of this output carries digits, and a
		// substring that the timestamp satisfies asserts nothing.
		for _, want := range []string{
			"level=WARN",
			"evidence log contains damaged lines",
			"chain continues from last valid record",
			"damaged_lines=[4]",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the store logged %q, want it to carry %q", out, want)
			}
		}
	})
}

// TestOnlyTheFilesystemThatCannotBeAskedIsPassedOver: a directory sync is
// how this package promises that the *name* pointing at a fsynced file
// survives a crash, and exactly one failure is forgiven — the filesystem
// that does not implement the operation. Everything else is a promise it
// cannot make.
//
// The condition had no test and could not have had one while it lived
// inside syncDir: no ordinary filesystem answers a directory fsync with
// anything but success or EINVAL, so the tolerance was reachable only by
// the errors themselves. Naming the decision is what makes it reachable,
// and the mutation run is what asked for the name.
//
// The wrapped cases are the ones that matter. os.File.Sync reports through
// a *fs.PathError, so a comparison against the bare errno would pass over
// nothing at all and every host without directory fsync would fail its
// drills.
func TestOnlyTheFilesystemThatCannotBeAskedIsPassedOver(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nothing failed":                  {nil, false},
		"the bare errno":                  {syscall.EINVAL, false},
		"the errno Sync reports":          {&fs.PathError{Op: "sync", Path: "/d", Err: syscall.EINVAL}, false},
		"a wrapped errno":                 {fmt.Errorf("sync: %w", syscall.EINVAL), false},
		"an I/O error":                    {syscall.EIO, true},
		"an I/O error as Sync reports it": {&fs.PathError{Op: "sync", Path: "/d", Err: syscall.EIO}, true},
		"a permission error":              {fs.ErrPermission, true},
		"something with no errno":         {errors.New("the disk went away"), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := durabilityUnknown(tc.err); got != tc.want {
				t.Errorf("durabilityUnknown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
