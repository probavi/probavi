package main

import (
	"slices"
	"testing"
)

// TestCredentialValuesResolvesOnlyWhatIsSet covers the one place the
// process environment is read for masking. A variable the drill declares
// but the environment does not set must contribute nothing: an empty
// secret would otherwise be looked for between every pair of characters
// in every message a record carries.
func TestCredentialValuesResolvesOnlyWhatIsSet(t *testing.T) {
	t.Setenv("PROBAVI_TEST_CIPHER_PASS", "correct-horse-battery-staple")
	t.Setenv("PROBAVI_TEST_EMPTY", "")

	got := credentialValues([]string{
		"PROBAVI_TEST_CIPHER_PASS",
		"PROBAVI_TEST_EMPTY",
		"PROBAVI_TEST_NEVER_SET",
	})
	want := []string{"correct-horse-battery-staple", ""}
	if !slices.Equal(got, want) {
		t.Errorf("credentialValues = %q, want %q — a set-but-empty variable is still declared, "+
			"an unset one is not resolvable at all", got, want)
	}
}

// A drill declaring no credentials resolves none, and the redactor built
// from the result masks nothing.
func TestCredentialValuesOfNothingIsEmpty(t *testing.T) {
	if got := credentialValues(nil); len(got) != 0 {
		t.Errorf("credentialValues(nil) = %q, want no values", got)
	}
}
