package evidence

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Status is the overall verdict of verifying an evidence log.
type Status int

// Verification verdicts (evidence-schema.md §9). Damage means unparseable
// crash artifacts were found; signed content was still fully verified.
const (
	StatusValid Status = iota
	StatusValidWithDamage
	StatusInvalid
)

// String returns the verdict name as printed by `probavi evidence verify`.
func (s Status) String() string {
	switch s {
	case StatusValid:
		return "VALID"
	case StatusValidWithDamage:
		return "VALID_WITH_DAMAGE"
	case StatusInvalid:
		return "INVALID"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// Result reports the outcome of verifying an evidence log.
type Result struct {
	Status       Status
	Records      int   // valid records verified
	DamagedLines []int // 1-based line numbers of unparseable fragments
	FailedLine   int   // 1-based line of the first invalid record, 0 if none
	Reason       string
	// Head is the chain head after the last record that verified (§9.1).
	// It is reported on every run so that each verification yields the
	// anchor for the next one; after INVALID it describes the prefix that
	// verified, which is why an anchor is never taken from such a run.
	Head Head
}

// Verify checks an evidence log against evidence-schema.md §9 using the
// given keyring. A non-nil anchor adds the §9.1 check: the log must once
// have carried that head, or records were removed from its end. The
// returned error reports I/O problems only; integrity verdicts are in the
// Result.
func Verify(r io.Reader, keyring Keyring, anchor *Head) (*Result, error) {
	if keyring == nil {
		keyring = Keyring{}
	}
	w, err := walk(r, keyring, anchor)
	if err != nil {
		return nil, err
	}
	res := &Result{
		Records:      w.records,
		DamagedLines: w.damaged,
		FailedLine:   w.failedLine,
		Reason:       w.reason,
		Head:         w.head(),
	}
	switch {
	case w.failed:
		res.Status = StatusInvalid
	case len(w.damaged) > 0:
		res.Status = StatusValidWithDamage
	default:
		res.Status = StatusValid
	}
	return res, nil
}

// walkState carries chain verification state across lines. With a nil
// keyring, signature checks are skipped (writer-side chain scan).
type walkState struct {
	keyring Keyring
	// anchor is the §9.1 head to check against, nil when none was given;
	// anchorSeen records whether the walk ever reached its seq.
	anchor     *Head
	anchorSeen bool
	nextSeq    int64
	prevHash   string
	records    int
	damaged    []int
	failed     bool
	failedLine int
	reason     string
}

// head returns the chain head the walk currently carries: the last seq that
// verified and the hash of its stored line (§9.1).
func (w *walkState) head() Head {
	return Head{Seq: w.nextSeq - 1, Hash: w.prevHash}
}

// walk runs the §9 algorithm over every line of the log. It stops at the
// first invalid record; damage (unparseable lines) is collected and
// skipped without advancing the chain.
func walk(r io.Reader, keyring Keyring, anchor *Head) (*walkState, error) {
	w := &walkState{keyring: keyring, anchor: anchor, nextSeq: 1, prevHash: GenesisPrevHash}
	// An anchor at seq 0 names the genesis head, which the walk carries
	// before it has read anything.
	if !w.checkAnchor(0) {
		return w, nil
	}
	br := bufio.NewReaderSize(r, readChunkBytes)
	for lineNo := 1; ; lineNo++ {
		line, terminated, tooLong, err := readLine(br, MaxRecordBytes)
		switch {
		case tooLong:
			w.invalid(lineNo, ErrRecordTooLarge.Error())
			return w, nil
		case err != nil && !errors.Is(err, io.EOF):
			return nil, fmt.Errorf("read evidence log: %w", err)
		case !terminated:
			if len(line) > 0 {
				w.damaged = append(w.damaged, lineNo) // torn tail, no newline
			}
			w.checkTruncated()
			return w, nil
		}
		if !w.step(line, lineNo) {
			return w, nil
		}
	}
}

// readChunkBytes is how much of a line the reader holds at once; a line may
// span several chunks up to the caller's cap.
const readChunkBytes = 64 * 1024

// readLine reads one newline-terminated line while refusing to buffer more
// than max bytes, and reports whether a terminator was seen and whether the
// cap was exceeded.
//
// The cap is the point. A log is untrusted input by design — `probavi
// evidence verify` and the independent verifier exist so that someone can
// check a log they were handed — and reading a line into memory before
// applying §4's size rule would let a file with no newline in it exhaust
// the machine's memory long before the rule ran. The rule now bounds the
// read itself.
func readLine(br *bufio.Reader, max int) (line []byte, terminated, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			// Over the cap: the verdict is already decided, so the rest of
			// the line is never read.
			return nil, false, true, nil
		}
		buf = append(buf, chunk...) // ReadSlice's slice dies at the next read
		switch {
		case rerr == nil:
			return bytes.TrimSuffix(buf, []byte("\n")), true, false, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			return buf, false, false, io.EOF
		default:
			return nil, false, false, rerr
		}
	}
}

// step processes one complete line; it reports false when walking must stop
// because the chain is invalid.
func (w *walkState) step(line []byte, lineNo int) bool {
	rec, obj, verdict := w.parseCanonical(line, lineNo)
	if rec == nil {
		return verdict
	}
	if rec.Seq != w.nextSeq {
		return w.invalid(lineNo, fmt.Sprintf("seq %d, want %d", rec.Seq, w.nextSeq))
	}
	if rec.PrevHash != w.prevHash {
		return w.invalid(lineNo, fmt.Sprintf("prev_hash mismatch, want %s", w.prevHash))
	}
	if w.keyring != nil {
		if err := w.verifySignature(rec, obj); err != nil {
			return w.invalid(lineNo, err.Error())
		}
	}
	w.prevHash = lineHash(line)
	w.nextSeq++
	w.records++
	return w.checkAnchor(lineNo)
}

// checkAnchor compares the walk's head against the anchor at the moment the
// walk reaches the anchor's seq — before the first line when that seq is 0,
// immediately after record A.seq otherwise (§9.1). Because the head's seq
// advances by exactly one per valid record, the walk passes through every
// head the log has ever had, so one equality asked once decides the
// anchored outcome. It reports false when walking must stop.
func (w *walkState) checkAnchor(lineNo int) bool {
	if w.anchor == nil || w.anchorSeen || w.head().Seq != w.anchor.Seq {
		return true
	}
	w.anchorSeen = true
	if w.head() == *w.anchor {
		return true
	}
	return w.invalid(lineNo, fmt.Sprintf("anchor mismatch at seq %d: log has %s", w.anchor.Seq, w.prevHash))
}

// checkTruncated reports the other §9.1 outcome: the walk ended without ever
// carrying the anchor's head, so the log stops before the anchor's seq and
// records are missing from its end. No line is at fault — every line the
// file still holds verified — so the failing line stays 0.
func (w *walkState) checkTruncated() {
	if w.anchor == nil || w.anchorSeen || w.failed {
		return
	}
	w.invalid(0, fmt.Sprintf("log truncated: it ends at seq %d, before the anchor's seq %d", w.head().Seq, w.anchor.Seq))
}

// parseCanonical classifies a line: damage (nil record, keep walking),
// invalid (nil record, stop), or a parsed record in canonical form, returned
// both as a struct and as the decoded generic object.
func (w *walkState) parseCanonical(line []byte, lineNo int) (*Record, map[string]any, bool) {
	v, err := decodeStrict(line)
	if err != nil {
		w.damaged = append(w.damaged, lineNo)
		return nil, nil, true
	}
	canonical, err := Canonicalize(v)
	if err != nil {
		return nil, nil, w.invalid(lineNo, err.Error())
	}
	if !bytes.Equal(canonical, line) {
		return nil, nil, w.invalid(lineNo, ErrNotCanonical.Error())
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, nil, w.invalid(lineNo, "decode record: not a JSON object")
	}
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, nil, w.invalid(lineNo, fmt.Sprintf("decode record: %v", err))
	}
	if !supportedSchema(rec.Schema) {
		return nil, nil, w.invalid(lineNo, fmt.Sprintf("unsupported schema %q", rec.Schema))
	}
	return &rec, obj, true
}

// verifySignature checks rec's signature. The signed message is rebuilt from
// the stored object itself — drop sig, re-canonicalize — never from the
// Record struct: a struct round-trip would impose the current schema
// version's shape on stored records of every version (e.g. inject a v1-only
// field as null into a v0 record and falsely fail its signature).
func (w *walkState) verifySignature(rec *Record, obj map[string]any) error {
	if rec.Sig == nil {
		return errors.New("record has no sig")
	}
	if rec.Sig.Alg != "ed25519" {
		return fmt.Errorf("unsupported sig.alg %q", rec.Sig.Alg)
	}
	pub, ok := w.keyring[rec.Sig.KeyID]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownKey, rec.Sig.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(rec.Sig.SigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("malformed sig.sig_b64")
	}
	delete(obj, "sig")
	message, err := Canonicalize(obj)
	if err != nil {
		return fmt.Errorf("rebuild signed bytes: %w", err)
	}
	if !ed25519.Verify(pub, message, sig) {
		return errors.New("signature verification failed")
	}
	return nil
}

func (w *walkState) invalid(lineNo int, reason string) bool {
	w.failed = true
	w.failedLine = lineNo
	w.reason = reason
	return false
}
