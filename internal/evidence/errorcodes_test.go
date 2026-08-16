package evidence

import "testing"

// TestErrorCodesVocabulary pins the public accessors of the §7 vocabulary:
// every published code answers true, anything else false, and the returned
// slice is a copy — a caller mutating it must not be able to alter what
// producers normalize against before signing.
func TestErrorCodesVocabulary(t *testing.T) {
	codes := ErrorCodes()
	if len(codes) == 0 {
		t.Fatal("ErrorCodes returned an empty vocabulary")
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Errorf("code %q appears twice", c)
		}
		seen[c] = true
		if !IsErrorCode(c) {
			t.Errorf("IsErrorCode(%q) = false for a published code", c)
		}
	}
	for _, c := range []string{"", "not_a_code", "INVALID_REQUEST"} {
		if IsErrorCode(c) {
			t.Errorf("IsErrorCode(%q) = true, want false", c)
		}
	}

	codes[0] = "tampered"
	if IsErrorCode("tampered") || !IsErrorCode(CodeInvalidRequest) {
		t.Error("mutating the returned slice altered the vocabulary — ErrorCodes must return a copy")
	}
}
