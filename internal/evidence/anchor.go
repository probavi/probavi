package evidence

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMalformedAnchor reports an anchor that is not in the written form of
// evidence-schema.md §9.1. It is a usage error, never a verdict: a value
// that cannot be read says nothing about the log.
var ErrMalformedAnchor = errors.New("malformed anchor")

// hashPrefix is the reference form every hash in the schema carries (§5).
const hashPrefix = "sha256:"

// hashHexLen is the number of hex digits in a SHA-256 reference.
const hashHexLen = 64

// Head is a chain head: the highest verified seq together with the SHA-256
// reference of that record's stored line (evidence-schema.md §9.1). The head
// of an empty log is seq 0 with the genesis hash, and the head at seq n is
// the value record n+1 carries as its prev_hash.
//
// A Head is also an anchor. Kept where the log's writer cannot rewrite it
// and handed back to Verify, it answers the one question the file cannot:
// whether records were removed from the end. The type is one rather than
// two because §9.1's check is exactly the question "did the walk ever carry
// this head", which a struct comparison asks directly.
type Head struct {
	Seq  int64
	Hash string
}

// String renders the head in the written anchor form, "<seq>:sha256:<hex>".
func (h Head) String() string {
	return strconv.FormatInt(h.Seq, 10) + ":" + h.Hash
}

// ParseAnchor reads the written form of §9.1, "<seq>:sha256:<hex>". The
// sequence number is decimal digits with no sign and no leading zeros, so
// that one head has exactly one spelling and two implementations cannot
// disagree about a value either of them printed.
func ParseAnchor(s string) (Head, error) {
	seqText, hash, ok := strings.Cut(s, ":")
	if !ok {
		return Head{}, fmt.Errorf("%w %q: want <seq>:sha256:<hex>", ErrMalformedAnchor, s)
	}
	seq, err := parseAnchorSeq(seqText)
	if err != nil {
		return Head{}, err
	}
	if err := checkAnchorHash(hash); err != nil {
		return Head{}, err
	}
	return Head{Seq: seq, Hash: hash}, nil
}

// parseAnchorSeq reads the sequence number half of an anchor.
func parseAnchorSeq(s string) (int64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, fmt.Errorf("%w: sequence number %q must be decimal digits without leading zeros", ErrMalformedAnchor, s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: sequence number %q must be decimal digits without leading zeros", ErrMalformedAnchor, s)
		}
	}
	seq, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: sequence number %q is out of range", ErrMalformedAnchor, s)
	}
	return seq, nil
}

// checkAnchorHash accepts the same "sha256:<64 lowercase hex>" reference a
// record carries as its prev_hash (§5).
func checkAnchorHash(hash string) error {
	hex, ok := strings.CutPrefix(hash, hashPrefix)
	if !ok || len(hex) != hashHexLen {
		return fmt.Errorf("%w: hash %q must be %q followed by %d hex digits", ErrMalformedAnchor, hash, hashPrefix, hashHexLen)
	}
	for _, r := range hex {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: hash %q must be lowercase hex", ErrMalformedAnchor, hash)
		}
	}
	return nil
}
