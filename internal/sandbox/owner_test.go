package sandbox

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// k8sLabelValue is the shape Kubernetes accepts for a label value. Owner
// ids become one, so a separator that fails this would break the k8s
// provider in the cluster rather than in a test.
var k8sLabelValue = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)

func TestOwnerIDIsAValidLabelValue(t *testing.T) {
	id := OwnerID(os.Getpid())
	if !k8sLabelValue.MatchString(id) || len(id) > 63 {
		t.Errorf("owner id %q is not a valid Kubernetes label value", id)
	}
	if !strings.HasPrefix(id, strconv.Itoa(os.Getpid())) {
		t.Errorf("owner id %q does not start with the pid", id)
	}
}

func TestOwnerAlive(t *testing.T) {
	self := os.Getpid()

	t.Run("this process by its own id", func(t *testing.T) {
		if !OwnerAlive(OwnerID(self)) {
			t.Error("the sweep would reclaim this drill's own sandbox")
		}
	})

	t.Run("this process by pid alone", func(t *testing.T) {
		// A sandbox labelled before this change carries no token; the pid
		// rule must still answer for it.
		if !OwnerAlive(strconv.Itoa(self)) {
			t.Error("a token-less id must fall back to the pid rule")
		}
	})

	t.Run("a recycled pid is not the owner", func(t *testing.T) {
		// Same pid, a token that cannot be this process's: exactly what an
		// unrelated process inheriting the pid looks like.
		//
		// Whether this platform offers a token is established first. Reading
		// the answer out of OwnerAlive instead — skipping when it says true —
		// cannot tell "there is no token here" from "the comparison was
		// skipped", and a mutation run found exactly that: dropping the
		// negation on the cannot-compare branch returns alive without
		// comparing, and this case skipped rather than failing.
		if _, ok := processStartToken(self); !ok {
			t.Skip("no start token on this platform; the pid rule is all there is")
		}
		if OwnerAlive(strconv.Itoa(self) + ownerSep + "1") {
			t.Error("a token that is not this process's was accepted; a recycled pid owns everything")
		}
	})

	t.Run("a reaped process", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper: %v", err)
		}
		id := OwnerID(cmd.Process.Pid)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("wait helper: %v", err)
		}
		if OwnerAlive(id) {
			t.Errorf("OwnerAlive(%q) = true for a reaped process — orphans would never be swept", id)
		}
	})

	t.Run("malformed ids own nothing", func(t *testing.T) {
		for _, id := range []string{"", "-", "abc", "0", "-1", "0" + ownerSep + "1", "x" + ownerSep + "1"} {
			if OwnerAlive(id) {
				t.Errorf("OwnerAlive(%q) = true, want false", id)
			}
		}
	})
}

// TestProcessStartTokenIsStable pins the property the whole scheme rests
// on: the token identifies a process, not a moment, so reading it twice
// gives the same answer while the process lives.
func TestProcessStartTokenIsStable(t *testing.T) {
	first, ok := processStartToken(os.Getpid())
	if !ok {
		t.Skip("no start token on this platform")
	}
	second, _ := processStartToken(os.Getpid())
	if first != second {
		t.Errorf("token changed under a live process: %q then %q", first, second)
	}
	if _, ok := processStartToken(-1); ok {
		t.Error("a negative pid has no start token")
	}
}
