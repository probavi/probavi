package redact

import (
	"strings"
	"testing"
)

func TestRedactorString(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		in      string
		want    string
	}{
		{
			name: "no secrets leaves the text alone",
			in:   "restore failed: exit 1",
			want: "restore failed: exit 1",
		},
		{
			name:    "one secret, one occurrence",
			secrets: []string{"hunter2"},
			in:      `pgbackrest: cipher pass "hunter2" rejected`,
			want:    `pgbackrest: cipher pass "[redacted]" rejected`,
		},
		{
			name:    "every occurrence goes, not just the first",
			secrets: []string{"s3cr3t"},
			in:      "tried s3cr3t, retried s3cr3t",
			want:    "tried [redacted], retried [redacted]",
		},
		{
			name:    "a secret that is the whole message",
			secrets: []string{"only"},
			in:      "only",
			want:    "[redacted]",
		},
		{
			name:    "several secrets in one message",
			secrets: []string{"alpha", "beta"},
			in:      "alpha then beta",
			want:    "[redacted] then [redacted]",
		},
		{
			// The point of ordering by length. Masking "pass" first would
			// leave "[redacted]phrase" — a record stating the tail of the
			// passphrase it was written to hide.
			name:    "a secret containing another is masked whole",
			secrets: []string{"pass", "passphrase"},
			in:      "read of passphrase failed",
			want:    "read of [redacted] failed",
		},
		{
			// Equal lengths cannot be separated by length, and the choice
			// is observable: masking "babx" first would answer
			// "a[redacted]". The tie-break is lexicographic so the answer
			// is the same on every run and on every machine.
			name:    "equal-length secrets resolve in a fixed order",
			secrets: []string{"babx", "abab"},
			in:      "ababx",
			want:    "[redacted]x",
		},
		{
			name:    "an unset variable contributes no secret",
			secrets: []string{""},
			in:      "nothing to hide",
			want:    "nothing to hide",
		},
		{
			name:    "the same value declared twice masks once",
			secrets: []string{"dup", "dup"},
			in:      "dup",
			want:    "[redacted]",
		},
		{
			name:    "a non-ASCII secret is matched by its bytes",
			secrets: []string{"jelszó"},
			in:      "a jelszó nem jó",
			want:    "a [redacted] nem jó",
		},
		{
			name:    "an empty message stays empty",
			secrets: []string{"x"},
			in:      "",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := New(tt.secrets...).String(tt.in); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNilRedactorPassesThrough is what keeps every call site a single
// expression: a package holding no secrets wires nil rather than a
// sentinel, and text still arrives intact.
func TestNilRedactorPassesThrough(t *testing.T) {
	var r *Redactor
	const in = "exit 1: could not read /backups/orders.dump"
	if got := r.String(in); got != in {
		t.Errorf("nil redactor String(%q) = %q, want it unchanged", in, got)
	}
}

// TestNewDropsUnsetAndDuplicates checks what New refuses to hold, since
// an empty secret would otherwise match between every pair of characters.
func TestNewDropsUnsetAndDuplicates(t *testing.T) {
	r := New("", "a", "a", "", "b")
	if len(r.secrets) != 2 {
		t.Fatalf("secrets = %q, want the two distinct non-empty values", r.secrets)
	}
	if got := r.String("ab"); got != "[redacted][redacted]" {
		t.Errorf("String(\"ab\") = %q, want both masked", got)
	}
}

// TestNewOrdersLongestFirst states the invariant directly, so a change to
// the comparator fails here by name rather than only through the one
// message in the table above that happens to depend on it.
func TestNewOrdersLongestFirst(t *testing.T) {
	r := New("bb", "a", "cccc", "dd")
	want := []string{"cccc", "bb", "dd", "a"}
	if len(r.secrets) != len(want) {
		t.Fatalf("secrets = %q, want %q", r.secrets, want)
	}
	for i := range want {
		if r.secrets[i] != want[i] {
			t.Fatalf("secrets = %q, want %q", r.secrets, want)
		}
	}
}

// TestPlaceholderIsTheWordChecksAlreadyUsed pins the spelling. It reaches
// records that are signed and never rewritten, so two readers of the same
// log must not meet two different words for the same thing.
func TestPlaceholderIsTheWordChecksAlreadyUsed(t *testing.T) {
	if Placeholder != "[redacted]" {
		t.Errorf("Placeholder = %q, want [redacted]", Placeholder)
	}
	if strings.Contains(Placeholder, "\n") {
		t.Errorf("Placeholder %q carries a newline; record messages are single-line", Placeholder)
	}
}
