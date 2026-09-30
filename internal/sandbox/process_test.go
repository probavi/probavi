package sandbox

import (
	"math"
	"os"
	"os/exec"
	"testing"
)

func TestProcessAliveSelf(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive(self) = false — the sweep would reclaim this drill's own sandbox")
	}
}

// TestProcessAliveAfterExit uses a process this test owns and has already
// reaped: its pid is genuinely gone, which is the case the sweep exists
// for.
func TestProcessAliveAfterExit(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait helper: %v", err)
	}
	if ProcessAlive(pid) {
		t.Errorf("ProcessAlive(%d) = true for a reaped process — orphans would never be swept", pid)
	}
}

// TestProcessAliveForeignProcess covers the EPERM path: pid 1 exists on
// every Unix and is almost never ours, so signalling it is refused. Refused
// is not gone, and treating it as gone would mean destroying a live
// sandbox.
func TestProcessAliveForeignProcess(t *testing.T) {
	if !ProcessAlive(1) {
		t.Error("ProcessAlive(1) = false — a process we may not signal is still running")
	}
}

func TestProcessAliveRejectsNonPositive(t *testing.T) {
	// 0 and -1 are wildcards for kill(2): signalling every process in the
	// group is not a question this function may ever ask.
	for _, pid := range []int{0, -1, -1000} {
		if ProcessAlive(pid) {
			t.Errorf("ProcessAlive(%d) = true, want false", pid)
		}
	}
}

// TestProcessAliveRejectsANumberTooLargeToBeAPid: the same wildcard, reached
// through a different door. kill(2) takes a pid_t, which is 32 bits wide, so
// a wider number arrives truncated — math.MaxInt64 arrives as -1, which is
// "every process the caller may signal", and the call succeeds.
//
// Measured before this guard existed: ProcessAlive(math.MaxInt64) answered
// true. It was unreachable, because OwnerAlive refuses such an id before the
// pid is ever used and TestAnOwnerIdNoPidCouldHaveOwnsNothing says so — but
// this function is exported, states the rule in its own comment, and is the
// place the rule belongs.
func TestProcessAliveRejectsANumberTooLargeToBeAPid(t *testing.T) {
	for _, pid := range []int{math.MaxInt32 + 1, math.MaxInt64, math.MinInt64} {
		if ProcessAlive(pid) {
			t.Errorf("ProcessAlive(%d) = true; no pid is that wide, and truncated into a "+
				"pid_t it asks kill(2) a question this function may never ask", pid)
		}
	}
}

// TestRepresentableAsPidIsAskedOfTheNumberNotTheKernel: both edges of the
// rule are invisible through ProcessAlive, because the kernel refuses the
// same numbers for its own reasons — ESRCH for one too large to name a
// process, "not initialized" for zero. Asking the rule directly is the only
// way to see which of the two answered, and a rule nobody can see is a rule
// nobody can keep.
func TestRepresentableAsPidIsAskedOfTheNumberNotTheKernel(t *testing.T) {
	for pid, want := range map[int]bool{
		1:                 true,  // the smallest pid there is
		4194304:           true,  // Linux's default pid_max, comfortably inside
		math.MaxInt32:     true,  // the widest a pid_t holds: this rule's business ends here
		math.MaxInt32 + 1: false, // one past it, and truncation begins
		math.MaxInt64:     false, // arrives as -1: every process the caller may signal
		0:                 false, // the process group
		-1:                false, // the other wildcard
		math.MinInt64:     false,
	} {
		if got := representableAsPid(pid); got != want {
			t.Errorf("representableAsPid(%d) = %v, want %v", pid, got, want)
		}
	}
}
