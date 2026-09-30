// Package redact masks known secret values in text on its way somewhere a
// secret must never arrive: a signed evidence record, the drill host's
// log, a notification.
//
// It is defence in depth, not permission. Adapters must keep secrets out
// of protocol messages (adapter-protocol.md §2.5) and checks record
// aggregates rather than engine diagnostics (evidence-schema.md §8). But
// an evidence log is append-only and signed, so a credential that reaches
// one cannot be taken back — only rotated — and the adapters are external
// processes anybody may write. evidence-schema.md §8 therefore requires
// the core to mask what it holds before writing, rather than trusting
// every adapter in the catalogue to have got it right.
package redact

import (
	"slices"
	"strings"
)

// Placeholder stands in for a secret wherever one is found. It is the
// spelling internal/checks has used for the ephemeral sandbox password
// since before this package existed, so a reader of an older record and a
// reader of a newer one see the same word.
const Placeholder = "[redacted]"

// Redactor holds the secret values of one drill. A nil *Redactor passes
// text through unchanged, which is what a caller with nothing to hide
// wants and what keeps every call site a single expression.
//
// What counts as a secret is the operator's declaration, not a guess:
// the values named in source.credential_env, plus the ephemeral password
// the core generated. A variable declared there whose value is not
// actually secret is masked too. That is the safe direction of the two,
// and a reason to declare in source.credential_env only what an adapter
// needs in order to read the backup.
type Redactor struct {
	// secrets is ordered longest first. Order matters when one secret
	// contains another: masking the shorter first would leave the
	// remainder of the longer one in the text, so a record could state
	// the last characters of a passphrase it was meant to hide.
	secrets []string
}

// New builds a redactor over the values given. Empty values are dropped —
// a variable the drill declared but the environment does not set masks
// nothing and would otherwise match everywhere — and duplicates collapse.
func New(values ...string) *Redactor {
	secrets := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || slices.Contains(secrets, v) {
			continue
		}
		secrets = append(secrets, v)
	}
	slices.SortFunc(secrets, func(a, b string) int {
		if n := len(b) - len(a); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	return &Redactor{secrets: secrets}
}

// String returns s with every secret this redactor holds replaced by
// Placeholder.
//
// It is deliberately a plain substring replacement over bytes. A secret
// split across a line wrap, hex-encoded or quoted by an engine survives
// it — this masks what is recognisable, and is not a filter that makes
// leaking safe.
func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, Placeholder)
	}
	return s
}
