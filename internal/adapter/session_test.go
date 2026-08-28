package adapter

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStartFailureIsAnError pins the race where the adapter executable
// disappears between resolution and exec: the operation must fail with a
// diagnosis naming the start, not hang or panic.
func TestStartFailureIsAnError(t *testing.T) {
	r := newRunner(filepath.Join(t.TempDir(), "gone"), nil, &Options{Grace: 500 * time.Millisecond})
	_, err := r.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "start adapter") {
		t.Fatalf("Probe = %v, want the start failure", err)
	}
}

// TestCloseStdinIsIdempotent: closeStdin runs on both the normal path and
// the finish() error path, so a second call must be a no-op and a failing
// Close (the pipe is often already gone with a dead process) must be
// absorbed, not escalated.
func TestCloseStdinIsIdempotent(t *testing.T) {
	_, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("pre-close: %v", err)
	}

	s := &session{stdin: w, logger: slog.New(slog.DiscardHandler)}
	s.closeStdin() // Close fails on the already-closed pipe; absorbed.
	if !s.stdinClosed {
		t.Error("closeStdin did not record the close")
	}
	s.closeStdin() // second call must return on the guard
}

// The tests below reach the error branches this package can trigger without
// cutting a failure seam into the process lifecycle for a test's benefit.
// Ten statements stay uncovered, deliberately, and every one of them is the
// body of a guard on a path that is itself exercised:
//
//   - newRequestID: crypto/rand failing.
//   - start(): the three cmd.Std{in,out,err}Pipe failures, which occur only
//     when a pipe is requested twice or after Start.
//   - do(): marshalling the request envelope, unreachable because mustMarshal
//     panics first on the same value and every other field is a string.
//   - do(): the request write. A pipe's real write errors are EPIPE and
//     EBADF, and closedPipe declines to treat either as a verdict — the read
//     loop and the exit status classify what actually happened.
//   - the watchdog and wait(): Kill or Close reporting an error, which needs
//     the process reaped between the timer firing and the signal landing, and
//     wait() seeing a nil Wait result despite having sent SIGKILL.
//
// Reaching any of them means injecting a failure seam into production code,
// and a seam that exists only for a test costs more than the statement it
// would cover. AGENTS.md §3.1 sets the near-total target for this package;
// this is where it stops, and why.

// errWriter fails every write with an error that is not a closed pipe, so
// the session must read it as a crash rather than as an adapter that simply
// stopped listening mid-conversation.
type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }
func (w errWriter) Close() error              { return nil }

// TestReadLoopCrashesWhenTheSandboxResultCannotBeWritten: a write failure
// that is not a closed pipe leaves the adapter waiting for an answer that
// will never arrive, so the operation must end with a diagnosis naming the
// write rather than block or report the adapter's own words.
func TestReadLoopCrashesWhenTheSandboxResultCannotBeWritten(t *testing.T) {
	const requestID = "req-1"
	call := `{"protocol":"` + ProtocolVersion + `","request_id":"` + requestID +
		`","sandbox_call":{"call_id":"c1","verb":"exec","args":{"argv":["true"]}}}`
	s := &session{
		stdin:  errWriter{err: errors.New("stdin is gone")},
		stdout: bufio.NewScanner(strings.NewReader(call + "\n")),
		logger: slog.New(slog.DiscardHandler),
	}

	final, verr := s.readLoop(context.Background(), "provision", requestID, &fakeVerbs{}, nil)
	if final != nil {
		t.Errorf("readLoop returned a final response %v, want none", final)
	}
	if verr == nil {
		t.Fatal("readLoop = nil error, want a crash")
	}
	if !strings.Contains(verr.Message, "write sandbox_result") {
		t.Errorf("message = %q, want it to name the failed write", verr.Message)
	}
	if !strings.Contains(verr.Message, "stdin is gone") {
		t.Errorf("message = %q, want it to carry the underlying cause", verr.Message)
	}
}

// TestWriteSandboxResultReportsAMarshalFailure pins the split between the
// two encode paths: the request side panics on a payload that cannot be
// marshalled because only a programming error produces one, while a sandbox
// result carries a verb's value and is reported instead. The write must not
// be attempted with a half-encoded line.
func TestWriteSandboxResultReportsAMarshalFailure(t *testing.T) {
	s := &session{
		stdin:  errWriter{err: errors.New("the write was attempted")},
		logger: slog.New(slog.DiscardHandler),
	}

	err := s.writeSandboxResult("req-1", sandboxResult{CallID: "c1", OK: true, Value: make(chan int)})
	if err == nil {
		t.Fatal("writeSandboxResult = nil, want the marshal failure")
	}
	if strings.Contains(err.Error(), "the write was attempted") {
		t.Errorf("error = %v, want the encode to fail before anything reaches stdin", err)
	}
}

// TestMustMarshalPanicsOnAnUnmarshalablePayload holds the helper to the
// contract its comment states: payload types are plain structs and maps, so
// a failure here is a programming error and must be loud rather than
// silently producing a request the adapter cannot read.
func TestMustMarshalPanicsOnAnUnmarshalablePayload(t *testing.T) {
	defer func() {
		raised := recover()
		if raised == nil {
			t.Fatal("mustMarshal did not panic")
		}
		msg, ok := raised.(string)
		if !ok || !strings.Contains(msg, "adapter: marshal payload") {
			t.Errorf("panic = %v, want the marshal diagnosis", raised)
		}
	}()

	mustMarshal(make(chan int))
}

// TestFinishAbsorbsCleanupFailures: finish() runs on the error paths, where
// the primary failure is already on its way to the caller. Every step it
// takes can fail against an adapter that is already gone, and none of them
// may escalate — cleanup that panics or returns early would leave the
// process unreaped, which is the one thing this function exists to prevent.
func TestFinishAbsorbsCleanupFailures(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Reaping it here is what makes every cleanup step below fail: the pipes
	// are closed and the process is no longer signalable.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stderrDone := make(chan struct{})
	close(stderrDone)
	s := &session{
		cmd: cmd, stdin: stdin, rawStdout: stdout,
		stderrDone: stderrDone, stopWatchdog: make(chan struct{}),
		logger: logger, grace: time.Second,
	}

	s.finish()

	if !s.waited {
		t.Error("finish did not reap the session")
	}
	for _, want := range []string{"close adapter stdout", "kill adapter on error path"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("logs do not record %q; got:\n%s", want, logBuf.String())
		}
	}
}
