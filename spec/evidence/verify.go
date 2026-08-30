package evidence

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Status is the verdict over a whole log (evidence-schema.md §9).
type Status string

const (
	// StatusValid means every record is authentic, complete and in order.
	StatusValid Status = "VALID"
	// StatusValidDamage means the above holds for every parseable record,
	// but the file also contains unparseable fragments — a crash artifact,
	// not a forgery (§9 security note).
	StatusValidDamage Status = "VALID_WITH_DAMAGE"
	// StatusInvalid means a record failed authenticity, ordering or
	// continuity. The log cannot be trusted.
	StatusInvalid Status = "INVALID"
)

// genesisPrevHash is the prev_hash of the first record in a file (§5).
const genesisPrevHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// supportedSchemas lists every published schema version. §10 requires a
// verifier to support all of them for the lifetime of the format: records
// already written under an old version must stay verifiable forever.
var supportedSchemas = map[string]bool{
	"probavi-evidence/0": true,
	"probavi-evidence/1": true,
	"probavi-evidence/2": true,
}

// Result is the outcome of verifying one log.
type Result struct {
	Status Status `json:"status"`
	// Records counts the verified records (damaged fragments excluded).
	Records int `json:"records"`
	// DamagedLines lists 1-based line numbers of unparseable fragments.
	DamagedLines []int `json:"damaged_lines"`
	// Reason and Line are set only when Status is INVALID.
	Reason string `json:"reason,omitempty"`
	Line   int    `json:"line,omitempty"`
	// Head is the chain head after the last record that verified (§9.1).
	// It is reported on every run, with or without an anchor, so that each
	// verification yields the anchor for the next one; after INVALID it
	// describes only the prefix that verified.
	Head Head `json:"head"`
}

// invalid finishes a Result with the §9 INVALID verdict. Line 0 means no
// line is at fault, which is how §9.1 reports a log that simply stops
// before its anchor.
func invalid(res Result, line int, reason string) Result {
	res.Status = StatusInvalid
	res.Reason = reason
	res.Line = line
	return res
}

// Keyring maps a key_id (§6) to the public key that bears it. Verification
// accepts several keys so that a log spanning a key rotation still verifies
// end to end.
type Keyring map[string]ed25519.PublicKey

// NewKeyring indexes the given public keys by their key_id.
func NewKeyring(keys ...ed25519.PublicKey) Keyring {
	kr := make(Keyring, len(keys))
	for _, k := range keys {
		kr[KeyID(k)] = k
	}
	return kr
}

// KeyID derives the §6 key identifier: the first 16 hex characters of the
// SHA-256 of the 32 raw public-key bytes.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

// ParsePublicKey reads the §6 public key file format: 64 lowercase hex
// characters, surrounding whitespace ignored.
func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	s := strings.TrimSpace(string(data))
	if len(s) != 2*ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d hex characters, got %d", 2*ed25519.PublicKeySize, len(s))
	}
	if s != strings.ToLower(s) {
		return nil, errors.New("public key must be lowercase hex")
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	return ed25519.PublicKey(raw), nil
}

// readCappedLine reads one line, including its terminator, and stops as
// soon as it is longer than a record may be (§4). It reports that as
// oversized rather than returning the bytes.
//
// This tool's whole purpose is to be run by someone who did not produce the
// log: an auditor, an insurer, anyone handed a file. Buffering a line
// before applying the size rule would let a file containing no newline at
// all exhaust the machine's memory before any rule ran, which is a poor
// property for the program whose job is to survive hostile input.
func readCappedLine(br *bufio.Reader) (line []byte, oversized bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		if len(line)+len(chunk) > maxCanonicalBytes+1 { // +1 for the newline
			return nil, true, nil
		}
		line = append(line, chunk...) // the slice is only valid until the next read
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		return line, false, rerr
	}
}

// Verify runs the §9 algorithm over a log and reports the verdict. The error
// return covers only I/O failures reading r; a log that fails verification
// is a Result, not an error.
func Verify(r io.Reader, keys Keyring) (Result, error) {
	return VerifyAnchored(r, keys, nil)
}

// VerifyAnchored runs §9 and, given a non-nil anchor, the §9.1 check on top
// of it: the log must once have carried that head, or records were removed
// from its end. Verify is this function with no anchor, which is the whole
// of §9 and remains sufficient for everything the chain alone can prove.
func VerifyAnchored(r io.Reader, keys Keyring, anchor *Head) (Result, error) {
	// res.Head carries what §9 calls expected_prev and expected_seq: the
	// hash is expected_prev, and expected_seq is one past its seq. Keeping
	// them there rather than in separate locals is what makes every return
	// path report the head §9.1 requires, without repeating an assignment.
	res := Result{Status: StatusValid, DamagedLines: []int{}, Head: Head{Seq: 0, Hash: genesisPrevHash}}
	// The genesis head is one the walk carries before it has read anything,
	// so an anchor at seq 0 is compared here.
	check := anchorCheck{want: anchor}
	if reason := check.at(res.Head); reason != "" {
		return invalid(res, 0, reason), nil
	}

	br := bufio.NewReader(r)
	for lineNo := 1; ; lineNo++ {
		raw, oversized, err := readCappedLine(br)
		if err != nil && !errors.Is(err, io.EOF) {
			return Result{}, fmt.Errorf("read log: %w", err)
		}
		// §4 caps a record; a line past that cap is not one, and saying so
		// costs nothing because the bytes were never gathered.
		if oversized {
			return invalid(res, lineNo, fmt.Sprintf("line exceeds the %d byte limit of §4", maxCanonicalBytes)), nil
		}
		if len(raw) == 0 {
			break
		}
		// A final fragment with no terminator is the torn tail of §2: the
		// writer crashed mid-line. Signed content cannot be altered or
		// removed this way, so it is damage, never tampering.
		if !bytes.HasSuffix(raw, []byte("\n")) {
			res.DamagedLines = append(res.DamagedLines, lineNo)
			break
		}
		line := raw[:len(raw)-1]

		rec, perr := parseRecord(line)
		if perr != nil {
			res.DamagedLines = append(res.DamagedLines, lineNo)
			continue // the chain does not advance across damage
		}
		if reason := verifyRecord(rec, line, keys, res.Head.Hash, res.Head.Seq+1); reason != "" {
			return invalid(res, lineNo, reason), nil
		}

		sum := sha256.Sum256(line)
		res.Head = Head{Seq: res.Head.Seq + 1, Hash: "sha256:" + hex.EncodeToString(sum[:])}
		res.Records++
		if reason := check.at(res.Head); reason != "" {
			return invalid(res, lineNo, reason), nil
		}
	}

	// No line is at fault when the log simply stops short of its anchor:
	// every line the file still holds verified.
	if reason := check.missed(res.Head); reason != "" {
		return invalid(res, 0, reason), nil
	}
	if len(res.DamagedLines) > 0 {
		res.Status = StatusValidDamage
	}
	return res, nil
}

// verifyRecord applies the per-record assertions of §9 in the order the
// specification lists them, returning "" when the record is sound and a
// human-readable reason otherwise.
func verifyRecord(rec map[string]any, line []byte, keys Keyring, expectedPrev string, expectedSeq int64) string {
	schema, ok := rec["schema"].(string)
	if !ok || !supportedSchemas[schema] {
		return fmt.Sprintf("unsupported schema %q", schema)
	}

	// canonicalize also enforces the §4 integer restriction, so a
	// fractional or oversized number is reported here rather than
	// surfacing later as an opaque byte mismatch.
	canon, err := canonicalize(rec)
	if err != nil {
		return err.Error()
	}
	// §4's size ceiling is enforced by readCappedLine, before a line is
	// ever gathered: a stored line that reaches this point is already
	// within it, and no canonical form is longer than the bytes it was
	// parsed from. Re-checking here would be an untestable branch.
	if !bytes.Equal(canon, line) {
		return "stored bytes are not the canonical serialization of the record"
	}

	// canonicalize above already proved every number in the record obeys
	// §4, so the only way seq can fail here is by being absent or not a
	// number at all; both fold into one branch rather than leaving an
	// untestable one behind.
	seq, ok := integerField(rec, "seq")
	if !ok {
		return "seq is missing or not an integer"
	}
	if seq != expectedSeq {
		return fmt.Sprintf("seq %d, want %d", seq, expectedSeq)
	}

	prev, ok := rec["prev_hash"].(string)
	if !ok || prev != expectedPrev {
		return fmt.Sprintf("prev_hash %s, want %s", prev, expectedPrev)
	}

	return verifySignature(rec, keys)
}

// verifySignature performs the last two assertions of §9: select the public
// key by key_id, then check the ed25519 signature over the canonical form of
// the record with sig removed.
func verifySignature(rec map[string]any, keys Keyring) string {
	sig, ok := rec["sig"].(map[string]any)
	if !ok {
		return "sig is missing or not an object"
	}
	alg, ok := sig["alg"].(string)
	if !ok || alg != "ed25519" {
		return fmt.Sprintf("unsupported signature algorithm %q", alg)
	}
	keyID, ok := sig["key_id"].(string)
	if !ok {
		return "sig.key_id is missing or not a string"
	}
	pub, ok := keys[keyID]
	if !ok {
		return fmt.Sprintf("no public key in the keyring for key_id %s", keyID)
	}
	sigB64, ok := sig["sig_b64"].(string)
	if !ok {
		return "sig.sig_b64 is missing or not a string"
	}
	rawSig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return "sig_b64 is not valid base64"
	}
	if len(rawSig) != ed25519.SignatureSize {
		return fmt.Sprintf("signature is %d bytes, want %d", len(rawSig), ed25519.SignatureSize)
	}

	// The signed message is the canonical form with sig removed entirely —
	// absent, not null (§6).
	unsigned := make(map[string]any, len(rec))
	for k, v := range rec {
		if k != "sig" {
			unsigned[k] = v
		}
	}
	// unsigned holds a subset of values the earlier canonicalize call
	// already accepted, so its error is unreachable; it shares the failure
	// branch instead of forming a path no test could ever exercise.
	msg, err := canonicalize(unsigned)
	if err != nil || !ed25519.Verify(pub, msg, rawSig) {
		return "signature does not verify"
	}
	return ""
}

// integerField reads a §4 integer out of a decoded record. It reports false
// when the field is missing, is not a number, or is not a legal integer.
func integerField(rec map[string]any, name string) (int64, bool) {
	num, ok := rec[name].(json.Number)
	if !ok {
		return 0, false
	}
	v, err := integerValue(num)
	if err != nil {
		return 0, false
	}
	return v, true
}
