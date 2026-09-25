package conformance

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	protocolVersion = "probavi-adapter/0"
	// protocolV1 is not what the harness drives at: the probe request goes
	// out at the floor, because it is the message that discovers the
	// version. v1 only names the fields whose presence the suite checks.
	protocolV1   = "probavi-adapter/1"
	maxLineBytes = 4 << 20
)

// envelope is any message an adapter may emit (§3).
type envelope struct {
	Protocol    string          `json:"protocol"`
	RequestID   string          `json:"request_id"`
	SandboxCall *sandboxCall    `json:"sandbox_call"`
	OK          *bool           `json:"ok"`
	Payload     json.RawMessage `json:"payload"`
	Error       *wireError      `json:"error"`
}

type sandboxCall struct {
	CallID string          `json:"call_id"`
	Verb   string          `json:"verb"`
	Args   json.RawMessage `json:"args"`
}

type wireError struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Retryable *bool           `json:"retryable"`
	Detail    json.RawMessage `json:"detail"`
}

// driveSpec configures one adapter operation run.
type driveSpec struct {
	// request is the complete first message, written verbatim — checks may
	// deliberately send wrong protocol versions or unknown ops.
	request string
	// sigterm, when true, delivers SIGTERM right after the first sandbox
	// call arrives, waits sigtermSettle, and only then answers the call —
	// the §2.4 scenario of check 14.
	sigterm bool
	// budget bounds the whole operation; on expiry the process is killed.
	budget time.Duration
	// grace replaces the remaining budget once SIGTERM was delivered.
	grace time.Duration
}

const sigtermSettle = 200 * time.Millisecond

// opResult is everything observable about one driven operation.
type opResult struct {
	calls           []sandboxCall
	postSignalCalls int
	final           *envelope
	finalCount      int
	postFinalLines  int
	violations      []string // framing violations (§2.2, §3), aggregated by check 15
	exitCode        int
	killed          bool  // harness had to SIGKILL: the budget or grace expired
	harnessErr      error // suite-side failure; not an adapter verdict
	wall            time.Duration
}

func (r *opResult) violate(format string, a ...any) {
	r.violations = append(r.violations, fmt.Sprintf(format, a...))
}

// finalError returns the final response's error code, or "" when the
// operation succeeded or never produced a final response.
func (r *opResult) finalError() string {
	if r.final == nil || r.final.Error == nil {
		return ""
	}
	return r.final.Error.Code
}

func (r *opResult) finalOK() bool {
	return r.final != nil && r.final.OK != nil && *r.final.OK
}

// drive runs one adapter process through one operation, acting as the core
// with a simulated sandbox: every exec succeeds (exit 0, stdout "1", empty
// stderr), every put_file succeeds (§10).
func drive(ctx context.Context, adapterPath, requestID string, spec driveSpec) (*opResult, error) {
	cmd := exec.Command(adapterPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard // §2.1: stderr is drill-log material, not protocol
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start adapter: %w", err)
	}

	res := &opResult{}
	var killed, killFailed atomic.Bool
	kill := func() {
		killed.Store(true)
		if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			killFailed.Store(true)
		}
	}
	if spec.budget <= 0 {
		spec.budget = 60 * time.Second
	}
	killTimer := time.AfterFunc(spec.budget, kill)
	defer killTimer.Stop()
	stop := context.AfterFunc(ctx, kill)
	defer stop()

	start := time.Now()
	if _, err := io.WriteString(stdin, spec.request+"\n"); err != nil && !closedPipe(err) {
		kill()
		return nil, errors.Join(fmt.Errorf("write request: %w", err), exitless(cmd.Wait()))
	}

	readLoop(res, stdin, stdout, cmd, requestID, spec, killTimer, start)
	return finish(res, stdin, cmd, &killed, &killFailed)
}

// finish reaps the adapter and separates the two kinds of bad news: a
// harness-side failure, which aborts the suite, and anything the adapter
// itself did, which belongs in the result as a verdict.
func finish(res *opResult, stdin io.WriteCloser, cmd *exec.Cmd, killed, killFailed *atomic.Bool) (*opResult, error) {
	cerr := stdin.Close()
	werr := cmd.Wait()
	res.killed = killed.Load()
	if res.harnessErr != nil {
		return nil, res.harnessErr
	}
	if killFailed.Load() {
		return nil, errors.New("adapter outlived its budget and could not be killed")
	}
	if cerr != nil && !errors.Is(cerr, os.ErrClosed) {
		return nil, fmt.Errorf("close adapter stdin: %w", cerr)
	}
	var exitErr *exec.ExitError
	if errors.As(werr, &exitErr) {
		res.exitCode = exitErr.ExitCode()
	} else if werr != nil && !res.killed {
		return nil, fmt.Errorf("wait for adapter: %w", werr)
	}
	return res, nil
}

// exitless filters the adapter's own exit status out of harness cleanup
// errors: on abort paths the exit code is expected and meaningless.
func exitless(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}

// loopCtl bundles what the read loop needs to answer calls and run the
// §2.4 SIGTERM scenario.
type loopCtl struct {
	stdin     io.WriteCloser
	cmd       *exec.Cmd
	requestID string
	spec      driveSpec
	killTimer *time.Timer
	start     time.Time
	signaled  bool
	callSeq   int
}

// readLoop consumes adapter stdout until EOF, answering sandbox calls and
// recording every observation. It never returns early on violations:
// post-final frames are protocol breaches the harness must see, not skip.
func readLoop(res *opResult, stdin io.WriteCloser, stdout io.Reader, cmd *exec.Cmd, requestID string, spec driveSpec, killTimer *time.Timer, start time.Time) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	ctl := &loopCtl{stdin: stdin, cmd: cmd, requestID: requestID, spec: spec, killTimer: killTimer, start: start}
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if !handleLine(res, ctl, line) {
			return
		}
	}
	if err := sc.Err(); err != nil {
		res.violate("stdout stream: %v (frames must stay within 4 MiB)", err)
	}
	if res.final == nil {
		res.wall = time.Since(start)
	}
}

// closedPipe reports a write failure that means the adapter closed its end
// of stdin — normally because it had already exited, racing our write. That
// is no verdict by itself: §2.1 lets an adapter stop the moment it sees EOF,
// and §2.3 makes "exited without a final response" a crash. What actually
// happened therefore comes from the read loop and the exit status, exactly
// as when the write lands before the exit. Treating the write error as a
// harness failure instead would abort the whole suite — telling an adapter
// author that the tool broke, for behaviour their adapter got right.
func closedPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed)
}

// handleLine classifies one stdout line; it reports false when the loop
// must stop (harness error or the adapter died mid-answer).
func handleLine(res *opResult, ctl *loopCtl, line []byte) bool {
	if res.finalCount > 0 {
		res.postFinalLines++
		res.violate("frame after the final response: %.120s", line)
		return true
	}
	msg := &envelope{}
	if err := json.Unmarshal(line, msg); err != nil {
		res.violate("stdout line is not a protocol message: %.120s", line)
		return true
	}
	if msg.RequestID != ctl.requestID {
		res.violate("request_id %q not echoed (got %q)", ctl.requestID, msg.RequestID)
	}
	switch {
	case msg.OK != nil:
		res.finalCount++
		res.final = msg
		res.wall = time.Since(ctl.start)
		return true
	case msg.SandboxCall != nil:
		return handleCall(res, ctl, msg.SandboxCall)
	default:
		res.violate("message is neither sandbox_call nor final: %.120s", line)
		return true
	}
}

func handleCall(res *opResult, ctl *loopCtl, call *sandboxCall) bool {
	ctl.callSeq++
	res.calls = append(res.calls, *call)
	if ctl.signaled {
		res.postSignalCalls++
	}
	if ctl.spec.sigterm && ctl.callSeq == 1 {
		if err := ctl.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			res.harnessErr = fmt.Errorf("deliver SIGTERM: %w", err)
			return false
		}
		time.Sleep(sigtermSettle)
		ctl.signaled = true
		ctl.killTimer.Reset(ctl.spec.grace)
	}
	// A write failure means the process died mid-answer; Wait sorts it out.
	return answer(ctl.stdin, ctl.requestID, call) == nil
}

// answer fulfills one sandbox call with the simulated sandbox's fixed
// responses (§10): exec exits 0 with stdout "1", put_file succeeds.
func answer(stdin io.Writer, requestID string, call *sandboxCall) error {
	var value any
	switch call.Verb {
	case "exec":
		value = map[string]any{
			"exit_code":        0,
			"stdout_b64":       base64.StdEncoding.EncodeToString([]byte("1\n")),
			"stderr_b64":       "",
			"truncated":        false,
			"duration_seconds": 0.001,
		}
	case "put_file":
		value = map[string]any{"bytes_copied": 1, "duration_seconds": 0.001}
	default:
		retryable := false
		reply, err := json.Marshal(map[string]any{
			"protocol": protocolVersion, "request_id": requestID,
			"sandbox_result": map[string]any{
				"call_id": call.CallID, "ok": false,
				"error": map[string]any{
					"code": "invalid_request", "retryable": retryable,
					"message": "verb " + strconv.Quote(call.Verb) + " is not defined in protocol v0",
				},
			},
		})
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(reply, '\n'))
		return err
	}
	reply, err := json.Marshal(map[string]any{
		"protocol": protocolVersion, "request_id": requestID,
		"sandbox_result": map[string]any{"call_id": call.CallID, "ok": true, "value": value},
	})
	if err != nil {
		return err
	}
	_, err = stdin.Write(append(reply, '\n'))
	return err
}

// request builds a well-formed §3.1 request line.
func request(requestID, op string, payload any) string {
	raw, err := json.Marshal(map[string]any{
		"protocol": protocolVersion, "request_id": requestID, "op": op, "payload": payload,
	})
	if err != nil {
		// Payloads are harness-built literals; failing to encode them is a
		// conformance-suite bug, not an adapter verdict.
		panic("conformance: encode request: " + err.Error())
	}
	return string(raw)
}
