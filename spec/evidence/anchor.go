package evidence

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMalformedAnchor reports a value that is not in the written anchor form
// of §9.1. It is a usage error rather than a verdict: an anchor that cannot
// be read says nothing at all about the log.
var ErrMalformedAnchor = errors.New("malformed anchor")

// Head is a chain head: the highest verified seq with the SHA-256 reference
// of that record's stored line (§9.1). The head of an empty log is seq 0
// with the genesis hash of §5, and the head at seq n is the value record
// n+1 carries as its prev_hash.
//
// The same type is the anchor. §9.1's check is the question "did this log
// ever carry this head", so the value compared and the value reported are
// one kind of thing, and comparing them is a struct equality.
type Head struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// String renders a head in the written anchor form, "<seq>:sha256:<hex>".
func (h Head) String() string {
	return strconv.FormatInt(h.Seq, 10) + ":" + h.Hash
}

// ParseAnchor reads "<seq>:sha256:<hex>" as §9.1 writes it: a decimal
// sequence number with no sign and no leading zeros, then the same hash
// reference a record carries as its prev_hash.
func ParseAnchor(s string) (Head, error) {
	seqText, hash, ok := strings.Cut(s, ":")
	if !ok {
		return Head{}, fmt.Errorf("%w %q: want <seq>:sha256:<hex>", ErrMalformedAnchor, s)
	}
	seq, err := parseSeq(seqText)
	if err != nil {
		return Head{}, err
	}
	if err := checkHashReference(hash); err != nil {
		return Head{}, err
	}
	return Head{Seq: seq, Hash: hash}, nil
}

// parseSeq reads the sequence-number half. One head has exactly one
// spelling, so that two implementations cannot disagree about a value
// either of them printed.
func parseSeq(s string) (int64, error) {
	malformed := fmt.Errorf("%w: sequence number %q must be decimal digits without leading zeros", ErrMalformedAnchor, s)
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, malformed
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, malformed
		}
	}
	seq, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: sequence number %q is out of range", ErrMalformedAnchor, s)
	}
	return seq, nil
}

// checkHashReference accepts the "sha256:" reference form of §5: the prefix
// followed by exactly 64 lowercase hex digits.
func checkHashReference(hash string) error {
	const prefix, digits = "sha256:", 64
	hex, ok := strings.CutPrefix(hash, prefix)
	if !ok || len(hex) != digits {
		return fmt.Errorf("%w: hash %q must be %q followed by %d hex digits", ErrMalformedAnchor, hash, prefix, digits)
	}
	for _, r := range hex {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: hash %q must be lowercase hex", ErrMalformedAnchor, hash)
		}
	}
	return nil
}

// anchorCheck applies §9.1's single equality as the walk passes each head it
// carries. It lives beside Head rather than inside the verification loop
// because it is state — "has the walk reached the anchor's seq yet" — and
// the loop reads better without it.
type anchorCheck struct {
	want *Head
	seen bool
}

// at reports the INVALID reason when the walk's head has reached the
// anchor's seq and differs from it, and "" when there is nothing to say.
// Called once at the genesis head and once after every record that
// verifies, it fires exactly where §9.1 says the comparison happens.
func (a *anchorCheck) at(head Head) string {
	if a.want == nil || a.seen || head.Seq != a.want.Seq {
		return ""
	}
	a.seen = true
	if head == *a.want {
		return ""
	}
	return fmt.Sprintf("anchor mismatch at seq %d: log has %s", a.want.Seq, head.Hash)
}

// missed reports the other §9.1 outcome: the walk ended without ever
// carrying the anchor's head, so the log stops before that seq and records
// are missing from its end.
func (a *anchorCheck) missed(head Head) string {
	if a.want == nil || a.seen {
		return ""
	}
	return fmt.Sprintf("log truncated: it ends at seq %d, before the anchor's seq %d", head.Seq, a.want.Seq)
}
