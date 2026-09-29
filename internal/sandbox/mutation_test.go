package sandbox

import (
	"os"
	"strconv"
	"testing"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// TestPidOneOwnsItsSandbox: the guard that refuses an id whose pid is not a
// pid accepted being moved one step further, and one step further excludes
// pid 1. That is not a corner: a drill running inside a container is pid 1,
// which is how Probavi is packaged, so the sandboxes such a drill creates
// would every one of them look orphaned — and the next sweep would destroy
// a sandbox whose owner is still restoring into it.
func TestPidOneOwnsItsSandbox(t *testing.T) {
	if !ProcessAlive(1) {
		t.Skip("pid 1 is not visible from here")
	}
	if !OwnerAlive("1") {
		t.Error("OwnerAlive(\"1\") = false; a containerised drill owns nothing it creates")
	}
}

// TestTheHostNameIsTheHostsAndTheFallbackIsOnlyAFallback: the fallback
// exists for a host that cannot be asked its name, and the guard accepted
// being reversed — which gives every host the fallback and none of them
// their own name. The sandbox host id is how a sweep tells its own
// machine's leftovers from another's, and it is a hash, so a wrong one
// looks exactly like a right one.
func TestTheHostNameIsTheHostsAndTheFallbackIsOnlyAFallback(t *testing.T) {
	want, err := os.Hostname()
	if err != nil {
		t.Skipf("this host cannot be asked its name: %v", err)
	}
	if want == "unknown-host" {
		t.Skip("this host is actually named unknown-host, so the two cases cannot be told apart")
	}
	if got := hostname(); got != want {
		t.Errorf("hostname() = %q, want %q — the fallback is not the answer for a host that answered", got, want)
	}
}

// TestTheStartTokenDistinguishesRatherThanExists: the token's whole job is
// to differ between a process and a later one that inherits its pid, so a
// parse landing on the wrong field is not a smaller version of working —
// field 21 of that line is itrealvalue, which is zero for every process
// alive, and a token every process shares is the pid rule with extra steps.
func TestTheStartTokenDistinguishesRatherThanExists(t *testing.T) {
	token, ok := processStartToken(os.Getpid())
	if !ok {
		t.Skip("no start token on this platform")
	}
	if token == "" || token == "0" {
		t.Errorf("start token = %q; a constant token distinguishes nothing", token)
	}
	// The same statement from the other side: a token this process does not
	// have must be refused, and "0" is the one a misparse would produce.
	if OwnerAlive(strconv.Itoa(os.Getpid()) + ownerSep + "0") {
		t.Error("a token of 0 was accepted as this process's own")
	}
}

// TestAnOwnerIdNoPidCouldHaveOwnsNothing: the guard reads "unparseable or
// not positive", and the first half looks redundant beside the second —
// strconv.Atoi returns zero for a name, so a name is caught either way.
// It is not redundant, and the case that shows it is a number too large to
// be a pid.
//
// Atoi clamps on overflow: it returns the largest int alongside its error,
// so the value is positive and the second half of the guard lets it past.
// What follows is worse than a wrong answer. A pid that wide truncates to
// -1 on its way into kill(2), which is the wildcard meaning "every process
// the caller may signal" — measured here, ProcessAlive answers true for it.
// Without the first half of this guard, a sandbox labelled with a number
// like that has a living owner forever and no sweep ever reclaims it.
func TestAnOwnerIdNoPidCouldHaveOwnsNothing(t *testing.T) {
	for _, id := range []string{
		"99999999999999999999",
		"99999999999999999999" + ownerSep + "1",
		"-99999999999999999999",
	} {
		if OwnerAlive(id) {
			t.Errorf("OwnerAlive(%q) = true; a sandbox labelled with it would never be swept", id)
		}
	}
}
