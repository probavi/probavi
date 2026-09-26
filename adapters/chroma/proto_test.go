package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// proto_test.go tests proto.go, the protocol harness every adapter carries
// as the same file, and it is carried the same way: a change to one copy
// is a change to all of them. It speaks to the harness directly rather
// than through an operation, because the paths it covers are the ones a
// well-behaved core never takes — a torn stream, a result for another
// call, a value that does not decode — and an operation meets them only
// once something upstream has already gone wrong.

// harnessCore is a core that reads input as the lines the core sends and
// writes the adapter's messages to out.
func harnessCore(input string, out io.Writer) *core {
	sc := bufio.NewScanner(strings.NewReader(input))
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	// The floor, because that is what the core probes at and what these
	// tests frame their fixtures in; every message back echoes it.
	return &core{in: sc, out: out, requestID: "r-test", protocol: protocolFloor}
}

// sandboxResultLine is one sandbox_result message carrying result.
func sandboxResultLine(result string) string {
	return `{"protocol":"probavi-adapter/0","request_id":"r-test","sandbox_result":` + result + "}\n"
}

// brokenPipe is a stdout nobody reads any more.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestProtoAcceptReadsOneRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		input, code, message string
	}{
		"a request":               {input: `{"protocol":"probavi-adapter/0","request_id":"r-test","op":"probe","payload":{}}` + "\n"},
		"nothing on stdin":        {code: "invalid_request", message: "no request on stdin"},
		"a line that is not JSON": {input: "probe\n", code: "invalid_request", message: "not valid JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			c, req, perr := accept(strings.NewReader(tc.input), io.Discard)
			if tc.code == "" {
				if perr != nil || req == nil || req.Op != "probe" || c == nil || c.requestID != "r-test" {
					t.Errorf("accept = %+v, %+v, %+v, want the request and a core echoing its id", c, req, perr)
				}
				return
			}
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) || c != nil || req != nil {
				t.Errorf("accept = %+v, %+v, %+v, want only %s mentioning %q", c, req, perr, tc.code, tc.message)
			}
		})
	}
}

// TestProtoAnotherVersionIsRefusedByName pins §3.1: the refusal lists the
// versions spoken, and still comes with a core, so it can be written
// echoing the request it refuses.
func TestProtoAnotherVersionIsRefusedByName(t *testing.T) {
	c, req, perr := accept(strings.NewReader(`{"protocol":"probavi-adapter/9","request_id":"r-9","op":"probe"}`+"\n"), io.Discard)
	if c == nil || c.requestID != "r-9" || req != nil {
		t.Fatalf("core = %+v, request = %+v, want a core for the refusal and no request", c, req)
	}
	if perr == nil || perr.Code != "unsupported_protocol" {
		t.Fatalf("perr = %+v, want unsupported_protocol", perr)
	}
	supported, ok := perr.Detail["supported"].([]string)
	if !ok || !slices.Equal(supported, protocolVersions) {
		t.Errorf("detail.supported = %v, want every version this adapter speaks %v",
			perr.Detail["supported"], protocolVersions)
	}
	// The refusal itself must be readable: it is framed at the floor,
	// never at the version that was asked for and refused.
	if c.protocol != protocolFloor {
		t.Errorf("refusal framed at %q, want the floor %q", c.protocol, protocolFloor)
	}
}

// TestProtoVerbsCallAndDecode pins the happy exchange: one call at a time,
// each numbered, each answered by its own result, the verb's value decoded.
func TestProtoVerbsCallAndDecode(t *testing.T) {
	var out bytes.Buffer
	c := harnessCore(
		sandboxResultLine(`{"call_id":"c1","ok":true,"value":{"exit_code":3,"stdout_b64":"b3V0","stderr_b64":"ZXJy","duration_seconds":0.5}}`)+
			sandboxResultLine(`{"call_id":"c2","ok":true,"value":{"bytes_copied":7,"duration_seconds":0.25}}`), &out)
	val, stdout, stderr, perr := c.exec(context.Background(), execArgs{Argv: []string{"true"}})
	if perr != nil || val.ExitCode != 3 || val.DurationSeconds != 0.5 || string(stdout) != "out" || string(stderr) != "err" {
		t.Errorf("exec = %+v, %q, %q, %+v", val, stdout, stderr, perr)
	}
	put, perr := c.putFile(context.Background(), putFileArgs{SourcePath: "/backup", DestPath: "/scratch/backup"})
	if perr != nil || put.BytesCopied != 7 || put.DurationSeconds != 0.25 {
		t.Errorf("put_file = %+v, %+v", put, perr)
	}
	assertCalls(t, out.String(), "exec", "put_file")
}

// assertCalls checks that written holds one sandbox_call line per verb, in
// order, each numbered from c1 and echoing the request.
func assertCalls(t *testing.T, written string, verbs ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(written, "\n"), "\n")
	if len(lines) != len(verbs) {
		t.Fatalf("wrote %d lines, want one per call: %q", len(lines), written)
	}
	for i, verb := range verbs {
		msg := struct {
			Protocol    string `json:"protocol"`
			RequestID   string `json:"request_id"`
			SandboxCall struct {
				CallID string          `json:"call_id"`
				Verb   string          `json:"verb"`
				Args   json.RawMessage `json:"args"`
			} `json:"sandbox_call"`
		}{}
		if err := json.Unmarshal([]byte(lines[i]), &msg); err != nil {
			t.Fatalf("line %d is not JSON: %s", i+1, lines[i])
		}
		id := "c" + strconv.Itoa(i+1)
		// Echoing, not asserting a constant: the fixture core was handed
		// the floor, and every message back must carry what it was sent.
		if msg.Protocol != protocolFloor || msg.RequestID != "r-test" ||
			msg.SandboxCall.CallID != id || msg.SandboxCall.Verb != verb || len(msg.SandboxCall.Args) == 0 {
			t.Errorf("line %d = %s, want call %s of %s echoing the request", i+1, lines[i], id, verb)
		}
	}
}

// TestProtoCallRefusesWhatIsNotItsAnswer covers every result a call cannot
// take as its answer. A harness that guessed here would act on another
// call's outcome, or on none.
func TestProtoCallRefusesWhatIsNotItsAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		input, code, message string
		retryable            bool
	}{
		"the stream closed":       {"", "internal", "closed the stream", false},
		"a line that is not JSON": {"sandbox result\n", "internal", "malformed sandbox_result", false},
		"a message that is not a result": {
			`{"protocol":"probavi-adapter/0","request_id":"r-test","op":"probe"}` + "\n", "internal", "malformed sandbox_result", false,
		},
		"the result of another call": {
			sandboxResultLine(`{"call_id":"c9","ok":true,"value":{}}`), "internal", "c9 does not match c1", false,
		},
		"a failure without an error": {
			sandboxResultLine(`{"call_id":"c1","ok":false}`), "internal", "without error object", false,
		},
		// §3.3: a failed call is the adapter's to judge, so the core's own
		// account of it arrives unchanged.
		"a failure the core explains": {
			sandboxResultLine(`{"call_id":"c1","ok":false,"error":{"code":"sandbox_error","message":"container exited","retryable":true}}`),
			"sandbox_error", "container exited", true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, perr := harnessCore(tc.input, io.Discard).call(context.Background(), "exec", execArgs{Argv: []string{"true"}})
			if perr == nil || perr.Code != tc.code || perr.Retryable != tc.retryable || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want %s (retryable %v) mentioning %q", perr, tc.code, tc.retryable, tc.message)
			}
		})
	}
}

// TestProtoVerbValuesThatDoNotDecode covers the verb wrappers: a failed
// call passes through, and a value that is not the verb's shape is the
// core's fault, not the sandbox's.
func TestProtoVerbValuesThatDoNotDecode(t *testing.T) {
	answer := func(value string) string {
		return sandboxResultLine(`{"call_id":"c1","ok":true,"value":` + value + `}`)
	}
	for name, tc := range map[string]struct {
		verb, input, message string
	}{
		"an exec call that failed":         {"exec", "", "closed the stream"},
		"an exec value that is not one":    {"exec", answer(`"done"`), "malformed exec value"},
		"stdout that is not base64":        {"exec", answer(`{"stdout_b64":"!!!"}`), "malformed exec stdout_b64"},
		"stderr that is not base64":        {"exec", answer(`{"stderr_b64":"!!!"}`), "malformed exec stderr_b64"},
		"a put_file call that failed":      {"put_file", "", "closed the stream"},
		"a put_file value that is not one": {"put_file", answer(`"done"`), "malformed put_file value"},
	} {
		t.Run(name, func(t *testing.T) {
			c := harnessCore(tc.input, io.Discard)
			var perr *protoError
			if tc.verb == "exec" {
				_, _, _, perr = c.exec(context.Background(), execArgs{Argv: []string{"true"}})
			} else {
				_, perr = c.putFile(context.Background(), putFileArgs{SourcePath: "/backup", DestPath: "/scratch/backup"})
			}
			if perr == nil || perr.Code != "internal" || !strings.Contains(perr.Message, tc.message) {
				t.Errorf("perr = %+v, want internal mentioning %q", perr, tc.message)
			}
		})
	}
}

// TestProtoACancelledCallIsNeverWritten pins §2.4: once the operation is
// cancelled no new call leaves the adapter, not even onto the stream.
func TestProtoACancelledCallIsNeverWritten(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	_, perr := harnessCore("", &out).call(ctx, "exec", execArgs{Argv: []string{"true"}})
	if perr == nil || perr.Code != "cancelled" || !perr.Retryable {
		t.Errorf("perr = %+v, want a retryable cancelled", perr)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q after the operation was cancelled", out.String())
	}
}

// TestProtoFinalResponses pins §3.4 and §2.3: each final response is one
// line echoing the request, and the exit code says whether it was written
// — the only account left once stdout will not take one.
func TestProtoFinalResponses(t *testing.T) {
	var out bytes.Buffer
	c := harnessCore("", &out)
	if exit := c.finishOK(map[string]any{"healthy": true}); exit != 0 {
		t.Errorf("finishOK exit = %d", exit)
	}
	if exit := c.finishError(protoErr("source_corrupt", false, "the backup is %s", "damaged")); exit != 0 {
		t.Errorf("finishError exit = %d", exit)
	}
	if exit := c.finishOK(math.Inf(1)); exit != 1 {
		t.Errorf("finishOK of a payload JSON cannot carry exit = %d, want 1", exit)
	}
	want := []string{
		`{"ok":true,"payload":{"healthy":true},"protocol":"probavi-adapter/0","request_id":"r-test"}`,
		`{"error":{"code":"source_corrupt","message":"the backup is damaged","retryable":false},"ok":false,"protocol":"probavi-adapter/0","request_id":"r-test"}`,
	}
	if got := strings.TrimSuffix(out.String(), "\n"); got != strings.Join(want, "\n") {
		t.Errorf("wrote\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}

	broken := harnessCore("", brokenPipe{})
	if _, perr := broken.call(context.Background(), "exec", execArgs{Argv: []string{"true"}}); perr == nil ||
		perr.Code != "internal" || !strings.Contains(perr.Message, "broken pipe") {
		t.Errorf("call on a broken stdout = %+v, want internal naming the write", perr)
	}
	if exit := broken.finishOK(map[string]any{}); exit != 1 {
		t.Errorf("finishOK on a broken stdout exit = %d, want 1", exit)
	}
	if exit := broken.finishError(protoErr("internal", false, "unwritable")); exit != 1 {
		t.Errorf("finishError on a broken stdout exit = %d, want 1", exit)
	}
}

// TestProtoSpeaksBothVersionsAndEchoesWhatItWasSent: the migration every
// adapter makes. The core probes at the floor and drives everything after
// it at the highest version both sides declare, comparing the protocol on
// every line it reads.
func TestProtoSpeaksBothVersionsAndEchoesWhatItWasSent(t *testing.T) {
	for _, version := range protocolVersions {
		t.Run(version, func(t *testing.T) {
			in := strings.NewReader(`{"protocol":"` + version +
				`","request_id":"r-1","op":"probe","payload":{}}` + "\n")
			var out bytes.Buffer
			c, req, perr := accept(in, &out)
			if perr != nil {
				t.Fatalf("accept refused %s: %+v", version, perr)
			}
			if req.Op != "probe" || c.protocol != version {
				t.Fatalf("core protocol = %q, request = %+v; want the version it was sent", c.protocol, req)
			}
			c.finishOK(map[string]any{"ok": true})
			var msg struct {
				Protocol string `json:"protocol"`
			}
			if err := json.Unmarshal(out.Bytes(), &msg); err != nil {
				t.Fatalf("response is not JSON: %s", out.String())
			}
			if msg.Protocol != version {
				t.Errorf("response protocol = %q, want the %q it was sent", msg.Protocol, version)
			}
		})
	}
}

// TestProbeDeclaresTheTwoBuiltinsChromaCanAnswer. The engine answers both
// from one request, and the third is left undeclared because Chroma has
// no aggregation over a dated field to answer it with.
func TestProbeDeclaresTheTwoBuiltinsChromaCanAnswer(t *testing.T) {
	payload, err := json.Marshal(probePayload())
	if err != nil {
		t.Fatalf("marshal probe: %v", err)
	}
	var got struct {
		ProtocolVersions []string                     `json:"protocol_versions"`
		Identifier       map[string]string            `json:"identifier"`
		Checks           map[string]map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("probe payload: %v", err)
	}
	if !slices.Equal(got.ProtocolVersions, protocolVersions) {
		t.Errorf("protocol_versions = %v, want %v", got.ProtocolVersions, protocolVersions)
	}
	if got.Identifier["open"] != "" || got.Identifier["close"] != "" || got.Identifier["separator"] != "." {
		t.Errorf("identifier = %v, want no quoting: a collection is a path segment", got.Identifier)
	}
	if got.Checks["table_exists"]["statement"] != "{{table}}/count" {
		t.Errorf("table_exists = %q, want the count request", got.Checks["table_exists"]["statement"])
	}
	// The same request on purpose: the engine answers both from it, with a
	// number for a collection that is there and a failed name resolution
	// for one that is not.
	if got.Checks["table_exists"]["statement"] != got.Checks["row_count"]["statement"] {
		t.Error("table_exists and row_count diverged; the engine answers both from one request")
	}
	if _, declared := got.Checks["freshness"]; declared {
		t.Error("freshness is declared, but Chroma has no aggregation over a dated field to answer it")
	}
}
