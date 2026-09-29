package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// TestTheChildGetsTheEnvironmentEntriesItWasGiven: the env argument exists
// for one reason, and the doc comment on Runner states it — a secret
// reaches a CLI this way rather than through argv, where every local user
// could read it out of the process list. Every case before this passed nil,
// so nothing asserted that a non-nil one arrives.
//
// One entry is the case worth pinning rather than several: a drill passes
// exactly one secret, and the guard that decides whether the environment is
// built at all accepted being moved past that.
func TestTheChildGetsTheEnvironmentEntriesItWasGiven(t *testing.T) {
	r := ExecRunner{}
	const script = `printf %s "$PROBAVI_TEST_SECRET-$PROBAVI_TEST_INHERITED"`

	t.Run("one entry, which is what a secret looks like", func(t *testing.T) {
		stdout, _, _, exit, err := r.Run(context.Background(), nil,
			[]string{"PROBAVI_TEST_SECRET=shhh"}, "sh", "-c", script)
		if err != nil || exit != 0 {
			t.Fatalf("Run: exit=%d err=%v", exit, err)
		}
		if got := string(stdout); got != "shhh-" {
			t.Errorf("the child saw %q, want the entry it was given", got)
		}
	})

	t.Run("nil means inherit and nothing more", func(t *testing.T) {
		t.Setenv("PROBAVI_TEST_INHERITED", "from-the-parent")
		stdout, _, _, exit, err := r.Run(context.Background(), nil, nil, "sh", "-c", script)
		if err != nil || exit != 0 {
			t.Fatalf("Run: exit=%d err=%v", exit, err)
		}
		if got := string(stdout); got != "-from-the-parent" {
			t.Errorf("the child saw %q, want the parent's environment and no addition", got)
		}
	})
}

// TestACancelledRunNamesTheContextRatherThanTheCommand: three things can go
// wrong in one call and they are not the same answer. A non-zero exit is a
// result, not an error — the suite already says so. What it did not say is
// that a run the context ended must be identifiable as one, and that a
// command that never started must not be.
//
// The caller is a sandbox provider deciding whether a drill timed out or
// its tooling is missing, and an error that only reports "something failed"
// leaves that to a substring match on a message.
func TestACancelledRunNamesTheContextRatherThanTheCommand(t *testing.T) {
	r := ExecRunner{}

	t.Run("a cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, _, _, err := r.Run(ctx, nil, nil, "sleep", "1")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to carry context.Canceled", err)
		}
	})
	// A deadline that expires while the command is running is a different
	// shape, and the sharp edge is worth writing down: the command is
	// killed, so cmd.Run reports an ExitError, and Runner's documented
	// contract turns that into an exit code rather than an error. The
	// caller is left to consult its own context — which is what
	// internal/core does to classify a drill as a timeout.
	t.Run("a deadline that passed mid-run", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, _, _, exit, err := r.Run(ctx, nil, nil, "sleep", "5")
		if err != nil {
			t.Errorf("err = %v, want a killed command reported as an exit code", err)
		}
		if exit == 0 {
			t.Error("exit = 0 for a command the deadline killed")
		}
		if ctx.Err() == nil {
			t.Error("the context did not expire; this case tested nothing")
		}
	})
	t.Run("a command that never started", func(t *testing.T) {
		_, _, _, _, err := r.Run(context.Background(), nil, nil, "/no/such/binary-probavi")
		if err == nil {
			t.Fatal("a missing binary must be an error")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want it not to blame the context", err)
		}
		// The cause matters more than the wording: an error naming no cause
		// is one a caller can only match on a substring.
		if errors.Unwrap(err) == nil {
			t.Errorf("err = %v wraps nothing, so nothing downstream can tell why", err)
		}
		if !strings.Contains(err.Error(), "run /no/such/binary-probavi") {
			t.Errorf("err = %v, want it to name the command it could not run", err)
		}
	})
}

// TestTheCaptureCapIsCountedToTheByte: the cap exists so a command dumping
// gigabytes cannot exhaust memory, and the number is a byte count rather
// than roughly a byte count. Both of its edges were unasserted, because
// every case before this crossed the cap in a single oversized write — the
// shape that never leaves one byte of room and never fills it exactly.
//
// Neither edge loses much on its own. Together they cost the ability to
// read a captured stream and know whether anything is missing, which is the
// only thing the truncated flag is for.
func TestTheCaptureCapIsCountedToTheByte(t *testing.T) {
	t.Run("a write that exactly fills the cap lost nothing", func(t *testing.T) {
		var w limitedWriter
		if _, err := w.Write(make([]byte, sandbox.MaxCaptureBytes)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if w.truncated {
			t.Error("a stream that exactly filled the cap was reported truncated")
		}
		if w.buf.Len() != sandbox.MaxCaptureBytes {
			t.Errorf("kept %d bytes, want the whole %d", w.buf.Len(), sandbox.MaxCaptureBytes)
		}
	})
	t.Run("the last byte of room is still used", func(t *testing.T) {
		var w limitedWriter
		if _, err := w.Write(make([]byte, sandbox.MaxCaptureBytes-1)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := w.Write([]byte("ab")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !w.truncated {
			t.Error("a write that did not fit was not reported truncated")
		}
		if w.buf.Len() != sandbox.MaxCaptureBytes {
			t.Errorf("kept %d bytes of a %d-byte cap — the cap is how many bytes are kept, "+
				"not roughly how many", w.buf.Len(), sandbox.MaxCaptureBytes)
		}
	})
}
