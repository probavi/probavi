package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

const (
	protocolVersion = "probavi-adapter/0"
	maxLineBytes    = 4 << 20
)

// protoError is the §5 error object an adapter sends in a final response.
type protoError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Detail    map[string]any `json:"detail,omitempty"`
}

func protoErr(code string, retryable bool, format string, a ...any) *protoError {
	return &protoError{Code: code, Message: fmt.Sprintf(format, a...), Retryable: retryable}
}

// inbound is any message the core sends: the initial request or a
// sandbox_result.
type inbound struct {
	Protocol      string          `json:"protocol"`
	RequestID     string          `json:"request_id"`
	Op            string          `json:"op,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	SandboxResult *sandboxResult  `json:"sandbox_result,omitempty"`
}

type sandboxResult struct {
	CallID string          `json:"call_id"`
	OK     bool            `json:"ok"`
	Value  json.RawMessage `json:"value,omitempty"`
	Error  *protoError     `json:"error,omitempty"`
}

// core is the adapter's channel back to the Probavi core.
type core struct {
	in        *bufio.Scanner
	out       io.Writer
	requestID string
	callSeq   int
}

// accept reads and validates the single request message (§3.1).
func accept(stdin io.Reader, stdout io.Writer) (*core, *inbound, *protoError) {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	if !sc.Scan() {
		return nil, nil, protoErr("invalid_request", false, "no request on stdin")
	}
	req := &inbound{}
	if err := json.Unmarshal(sc.Bytes(), req); err != nil {
		return nil, nil, protoErr("invalid_request", false, "request is not valid JSON")
	}
	c := &core{in: sc, out: stdout, requestID: req.RequestID}
	if req.Protocol != protocolVersion {
		// §3.1: the spoken versions MUST be listed in detail.supported.
		return c, nil, &protoError{Code: "unsupported_protocol",
			Message: "this adapter speaks " + protocolVersion + " only",
			Detail:  map[string]any{"supported": []string{protocolVersion}}}
	}
	return c, req, nil
}

// call issues one sandbox verb and blocks for its result (§3.2, §3.3). A
// canceled context refuses new calls per §2.4.
func (c *core) call(ctx context.Context, verb string, args any) (json.RawMessage, *protoError) {
	if ctx.Err() != nil {
		return nil, protoErr("cancelled", true, "operation cancelled before %s call", verb)
	}
	c.callSeq++
	callID := "c" + strconv.Itoa(c.callSeq)
	msg := map[string]any{
		"protocol":   protocolVersion,
		"request_id": c.requestID,
		"sandbox_call": map[string]any{
			"call_id": callID, "verb": verb, "args": args,
		},
	}
	if err := c.writeLine(msg); err != nil {
		return nil, protoErr("internal", false, "write sandbox_call: %v", err)
	}
	if !c.in.Scan() {
		return nil, protoErr("internal", false, "core closed the stream while a %s call was outstanding", verb)
	}
	res := &inbound{}
	if err := json.Unmarshal(c.in.Bytes(), res); err != nil || res.SandboxResult == nil {
		return nil, protoErr("internal", false, "malformed sandbox_result")
	}
	sr := res.SandboxResult
	if sr.CallID != callID {
		return nil, protoErr("internal", false, "sandbox_result call_id %s does not match %s", sr.CallID, callID)
	}
	if !sr.OK {
		if sr.Error != nil {
			return nil, sr.Error
		}
		return nil, protoErr("internal", false, "sandbox call failed without error object")
	}
	return sr.Value, nil
}

func (c *core) finishOK(payload any) int {
	if err := c.writeLine(map[string]any{
		"protocol": protocolVersion, "request_id": c.requestID, "ok": true, "payload": payload,
	}); err != nil {
		return 1
	}
	return 0
}

func (c *core) finishError(perr *protoError) int {
	if err := c.writeLine(map[string]any{
		"protocol": protocolVersion, "request_id": c.requestID, "ok": false, "error": perr,
	}); err != nil {
		return 1
	}
	return 0
}

func (c *core) writeLine(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.out.Write(append(line, '\n'))
	return err
}

// Wire forms of the sandbox verbs (§4).

type execArgs struct {
	Argv           []string          `json:"argv"`
	Env            map[string]string `json:"env,omitempty"`
	StdinB64       string            `json:"stdin_b64,omitempty"`
	TimeoutSeconds float64           `json:"timeout_seconds,omitempty"`
}

type execValue struct {
	ExitCode        int     `json:"exit_code"`
	StdoutB64       string  `json:"stdout_b64"`
	StderrB64       string  `json:"stderr_b64"`
	Truncated       bool    `json:"truncated"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type putFileArgs struct {
	SourcePath string `json:"source_path"`
	DestPath   string `json:"dest_path"`
	Mode       string `json:"mode,omitempty"`
}

type putFileValue struct {
	BytesCopied     int64   `json:"bytes_copied"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// exec runs one command in the sandbox and decodes the captured streams.
func (c *core) exec(ctx context.Context, args execArgs) (*execValue, []byte, []byte, *protoError) {
	raw, perr := c.call(ctx, "exec", args)
	if perr != nil {
		return nil, nil, nil, perr
	}
	val := &execValue{}
	if err := json.Unmarshal(raw, val); err != nil {
		return nil, nil, nil, protoErr("internal", false, "malformed exec value")
	}
	stdout, err := base64.StdEncoding.DecodeString(val.StdoutB64)
	if err != nil {
		return nil, nil, nil, protoErr("internal", false, "malformed exec stdout_b64")
	}
	stderr, err := base64.StdEncoding.DecodeString(val.StderrB64)
	if err != nil {
		return nil, nil, nil, protoErr("internal", false, "malformed exec stderr_b64")
	}
	return val, stdout, stderr, nil
}

func (c *core) putFile(ctx context.Context, args putFileArgs) (*putFileValue, *protoError) {
	raw, perr := c.call(ctx, "put_file", args)
	if perr != nil {
		return nil, perr
	}
	val := &putFileValue{}
	if err := json.Unmarshal(raw, val); err != nil {
		return nil, protoErr("internal", false, "malformed put_file value")
	}
	return val, nil
}
