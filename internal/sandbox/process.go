package sandbox

import (
	"errors"
	"math"
	"os"
	"syscall"
)

// ProcessAlive reports whether the local process that created a sandbox is
// still running. Providers use it to tell an orphan (owner gone, safe to
// reclaim) from a sandbox a concurrent drill is still using.
//
// It asks the kernel with signal 0, which delivers nothing and only
// performs the permission and existence checks. The obvious alternative —
// stat /proc/<pid> — is Linux-only, and Probavi ships macOS binaries: with
// no /proc the check fails for every pid, every labeled sandbox looks
// orphaned, and a starting drill destroys the running sandbox of a
// concurrent one. This function has no such platform hole.
//
// EPERM means the process exists and belongs to another user, which is
// still "alive" for our purpose: reclaiming a sandbox whose owner is
// running would be the destructive answer.
//
// Caveat, unchanged from the previous implementation: a recycled pid looks
// alive, so a sandbox whose owner died can survive until its pid is free
// again. That errs toward leaving a sandbox behind rather than destroying
// a live one, which is the right direction for a sweep to be wrong in.
//
// What it refuses before asking is representableAsPid's business, below.
func ProcessAlive(pid int) bool {
	if !representableAsPid(pid) {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// representableAsPid reports whether a number is one kill(2) could be asked
// about at all.
//
// It refuses two things, and they are the same thing twice. Zero and the
// negatives are kill's wildcards — signalling every process in the group,
// or every process the caller may signal, is not a question this function
// may ever ask. A number too large is that same question through a
// different door: kill takes a pid_t, which is 32 bits wide, so a wider
// number arrives truncated and MaxInt64 arrives as -1. Measured before this
// existed, ProcessAlive answered alive for it.
//
// MaxInt32 is that width rather than a line somebody drew. The more direct
// way to say it — whether the number survives int32 — is what gosec reads
// as the overflow itself rather than as the check for one, and a //nolint
// on correct code is what AGENTS.md §3 forbids in the same breath as
// weakening a linter. So the width is named instead.
//
// It is a function rather than a condition because the kernel hides its
// answer: a pid this refuses would be refused by the kernel too, for its
// own reasons, so the only place the rule can be read is here.
func representableAsPid(pid int) bool {
	return pid > 0 && pid <= math.MaxInt32
}
