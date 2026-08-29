package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"
)

// do runs one operation: fresh process, one request line in, sandbox calls
// mediated, exactly one final response out (§2, §3). guard is non-nil only
// for provision (put_file source allow-listing).
func (r *Runner) do(ctx context.Context, op string, payload any, verbs SandboxVerbs, guard func(string) (string, error)) (json.RawMessage, error) {
	requestID := newRequestID()
	request, err := json.Marshal(envelope{
		Protocol: ProtocolVersion, RequestID: requestID, Op: op, Payload: mustMarshal(payload),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", op, err)
	}

	s, err := r.start(ctx)
	if err != nil {
		return nil, err
	}
	defer s.finish()

	if _, err := s.stdin.Write(append(request, '\n')); err != nil && !closedPipe(err) {
		return nil, crashf("%s: write request: %v", op, err)
	}

	final, ferr := s.readLoop(ctx, op, requestID, verbs, guard)
	// The adapter had its final say (or violated the protocol); closing
	// stdin signals it to exit (§2.1).
	s.closeStdin()
	waitErr := s.wait()

	if ferr != nil {
		if waitErr != nil {
			return nil, crashf("%s (process: %v)", ferr.Message, waitErr)
		}
		return nil, ferr
	}
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, context.DeadlineExceeded) {
		// §2.3: a non-zero exit is a crash even when a final response was
		// written — an adapter that fails after answering is not trusted.
		// A wait error caused purely by our own cancellation is not held
		// against an adapter that already answered cleanly (§2.4).
		return nil, crashf("%s: %v", op, waitErr)
	}
	if !*final.OK {
		if final.Error == nil {
			return nil, crashf("%s: ok=false without error object", op)
		}
		return nil, final.Error
	}
	return final.Payload, nil
}

// closedPipe reports a write failure meaning the adapter closed its end of
// the pipe — usually because it already exited, racing our request write.
// That alone is no verdict: the truth (a crash, a clean exit without a
// response, or a protocol violation) comes from the read loop and the exit
// status, exactly as when the write lands before the exit.
func closedPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)
}

// session is one adapter process with its pipes.
type session struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       *bufio.Scanner
	rawStdout    io.ReadCloser
	stderrDone   chan struct{}
	stopWatchdog chan struct{}
	logger       *slog.Logger
	grace        time.Duration
	waited       bool
	stdinClosed  bool
}

func (r *Runner) start(ctx context.Context) (*session, error) {
	cmd := exec.CommandContext(ctx, r.path)
	cmd.Env = r.buildEnv()
	// §2.4: SIGTERM on cancellation, SIGKILL after the grace period.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = r.opts.Grace

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("adapter stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("adapter stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("adapter stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start adapter %s: %w", r.path, err)
	}

	// §1: stderr is captured verbatim into logs, never parsed.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), maxLineBytes)
		for sc.Scan() {
			r.logger.Info("adapter stderr", "line", sc.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	s := &session{
		cmd: cmd, stdin: stdin, stdout: scanner, rawStdout: stdout,
		stderrDone: done, stopWatchdog: make(chan struct{}),
		logger: r.logger, grace: r.opts.Grace,
	}
	// Watchdog: cmd.Cancel SIGTERMs on context cancellation, but a stubborn
	// adapter — or a grandchild holding the pipe open — would leave the
	// read loop blocked forever. After the grace period, kill the process
	// AND close our read end of stdout; the latter unblocks the scanner
	// even when orphaned grandchildren keep the write end alive.
	go func() {
		select {
		case <-s.stopWatchdog:
		case <-ctx.Done():
			t := time.NewTimer(s.grace)
			defer t.Stop()
			select {
			case <-s.stopWatchdog:
			case <-t.C:
				if err := cmd.Process.Kill(); err != nil {
					s.logger.Debug("watchdog kill", "err", err)
				}
				if err := stdout.Close(); err != nil {
					s.logger.Debug("watchdog close stdout", "err", err)
				}
			}
		}
	}()
	return s, nil
}

// readLoop consumes adapter stdout until the final response (§3.4),
// dispatching sandbox calls along the way (§3.2, §3.3).
func (s *session) readLoop(ctx context.Context, op, requestID string, verbs SandboxVerbs, guard func(string) (string, error)) (*envelope, *Error) {
	for {
		env, verr := s.readMessage(op, requestID)
		if verr != nil {
			return nil, verr
		}
		switch {
		case env.SandboxCall != nil:
			if verbs == nil {
				return nil, crashf("%s: sandbox_call is a protocol violation in this operation", op)
			}
			result := dispatchVerb(ctx, verbs, guard, env.SandboxCall)
			if err := s.writeSandboxResult(requestID, result); err != nil && !closedPipe(err) {
				return nil, crashf("%s: write sandbox_result: %v", op, err)
			}
			// A closed pipe is not the verdict (same rule as the request
			// write): the adapter stopped listening mid-conversation, and
			// the loop's next read — EOF, a late final response, or a
			// protocol violation — classifies what actually happened.
		case env.OK != nil:
			return env, nil
		default:
			return nil, crashf("%s: message is neither sandbox_call nor final response", op)
		}
	}
}

func (s *session) readMessage(op, requestID string) (*envelope, *Error) {
	if !s.stdout.Scan() {
		if err := s.stdout.Err(); errors.Is(err, bufio.ErrTooLong) {
			return nil, crashf("%s: message exceeds the %d byte frame limit", op, maxLineBytes)
		} else if err != nil {
			return nil, crashf("%s: read adapter stdout: %v", op, err)
		}
		return nil, crashf("%s: adapter exited without a final response", op)
	}
	env := &envelope{}
	if err := json.Unmarshal(s.stdout.Bytes(), env); err != nil {
		return nil, crashf("%s: stdout is not a protocol message: %v", op, err)
	}
	if env.Protocol != ProtocolVersion {
		return nil, crashf("%s: message protocol %q, want %q", op, env.Protocol, ProtocolVersion)
	}
	if env.RequestID != requestID {
		return nil, crashf("%s: message request_id %q does not echo %q", op, env.RequestID, requestID)
	}
	return env, nil
}

func (s *session) writeSandboxResult(requestID string, result sandboxResult) error {
	line, err := json.Marshal(sandboxResultEnvelope{
		Protocol: ProtocolVersion, RequestID: requestID, SandboxResult: result,
	})
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(line, '\n'))
	return err
}

func (s *session) closeStdin() {
	if s.stdinClosed {
		return
	}
	s.stdinClosed = true
	if err := s.stdin.Close(); err != nil {
		// The pipe is often already gone with a dead process; record it,
		// nothing more to do.
		s.logger.Debug("close adapter stdin", "err", err)
	}
}

// wait reaps the process with a hard bound: an adapter that lingers after
// its final response (or after SIGTERM) is killed rather than waited on
// forever.
func (s *session) wait() error {
	s.waited = true
	close(s.stopWatchdog)
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		<-s.stderrDone
		return err
	case <-time.After(s.grace):
		if kerr := s.cmd.Process.Kill(); kerr != nil {
			s.logger.Debug("kill lingering adapter", "err", kerr)
		}
		err := <-done
		<-s.stderrDone
		if err == nil {
			err = errors.New("adapter lingered past the grace period and was killed")
		}
		return err
	}
}

// finish guarantees the process is reaped on every path, including early
// returns before wait().
func (s *session) finish() {
	if s.waited {
		return
	}
	s.closeStdin()
	if err := s.rawStdout.Close(); err != nil {
		s.logger.Debug("close adapter stdout", "err", err)
	}
	if err := s.cmd.Process.Kill(); err != nil {
		s.logger.Debug("kill adapter on error path", "err", err)
	}
	if err := s.wait(); err != nil {
		// The primary error is already on its way to the caller; the kill
		// fallout is expected noise.
		s.logger.Debug("reap adapter on error path", "err", err)
	}
}

// buildEnv assembles the §2.5 allowlisted environment: baseline variables,
// declared credential passthrough, and explicit extras. Nothing else leaks
// into the adapter process.
func (r *Runner) buildEnv() []string {
	names := append([]string{"PATH", "HOME", "LANG", "TZ"}, r.opts.CredentialEnv...)
	env := make([]string, 0, len(names)+len(r.opts.Env))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		// A credential named PATH (or any other baseline variable) would
		// otherwise appear twice, leaving which one wins to exec's
		// last-wins rule rather than to this allow-list.
		if seen[name] {
			continue
		}
		seen[name] = true
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	extra := make([]string, 0, len(r.opts.Env))
	for k, v := range r.opts.Env {
		extra = append(extra, k+"="+v)
	}
	sort.Strings(extra)
	return append(env, extra...)
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// All payload types are plain structs and maps; failure here is a
		// programming error, not a runtime condition.
		panic("adapter: marshal payload: " + err.Error())
	}
	return b
}
