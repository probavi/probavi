package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// verbCall records one sandbox call the adapter issued.
type verbCall struct {
	Verb string
	Args json.RawMessage
}

// driveOp runs one full operation through run() with an in-process core
// simulator. handler returns the verb's value (or an error) for each
// sandbox call.
func driveOp(t *testing.T, op, payload string, handler func(call verbCall) (any, *protoError)) (finalLine []byte, calls []verbCall, exit int) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &bytes.Buffer{}

	exitCh := make(chan int, 1)
	go func() { exitCh <- run(stdinR, stdoutW, stderr) }()

	request := fmt.Sprintf(`{"protocol":"probavi-adapter/0","request_id":"r-test","op":%q,"payload":%s}`, op, payload)
	if _, err := io.WriteString(stdinW, request+"\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	sc := bufio.NewScanner(stdoutR)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		msg := struct {
			RequestID   string `json:"request_id"`
			SandboxCall *struct {
				CallID string          `json:"call_id"`
				Verb   string          `json:"verb"`
				Args   json.RawMessage `json:"args"`
			} `json:"sandbox_call"`
			OK *bool `json:"ok"`
		}{}
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("adapter emitted non-JSON: %s", line)
		}
		if msg.RequestID != "r-test" {
			t.Fatalf("adapter did not echo request_id: %s", line)
		}
		if msg.OK != nil {
			finalLine = line
			break
		}
		if msg.SandboxCall == nil {
			t.Fatalf("message is neither call nor final: %s", line)
		}
		call := verbCall{Verb: msg.SandboxCall.Verb, Args: msg.SandboxCall.Args}
		calls = append(calls, call)
		value, verr := handler(call)
		result := map[string]any{"call_id": msg.SandboxCall.CallID}
		if verr != nil {
			result["ok"] = false
			result["error"] = verr
		} else {
			result["ok"] = true
			result["value"] = value
		}
		reply, err := json.Marshal(map[string]any{
			"protocol": "probavi-adapter/0", "request_id": "r-test", "sandbox_result": result,
		})
		if err != nil {
			t.Fatalf("marshal sandbox_result: %v", err)
		}
		if _, err := stdinW.Write(append(reply, '\n')); err != nil {
			t.Fatalf("write sandbox_result: %v", err)
		}
	}
	if finalLine == nil {
		t.Fatal("adapter closed stdout without a final response")
	}
	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	exit = <-exitCh
	return finalLine, calls, exit
}

// finalResponse unpacks a final response line.
type finalResponse struct {
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   *protoError     `json:"error"`
}

func parseFinal(t *testing.T, line []byte) finalResponse {
	t.Helper()
	f := finalResponse{}
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatalf("parse final %s: %v", line, err)
	}
	return f
}

func okExec(exit int) any {
	return execValue{ExitCode: exit}
}

func outExec(stdout string) any {
	return execValue{StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

func errExec(exit int, stderr string) any {
	return execValue{
		ExitCode:  exit,
		StdoutB64: base64.StdEncoding.EncodeToString(nil),
		StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr)),
	}
}

// lastArg extracts the final argv element — for the mysql client that is
// always the -e statement, which identifies the call's purpose.
func lastArg(t *testing.T, call verbCall) (execArgs, string) {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) == 0 {
		t.Fatal("exec with empty argv")
	}
	return args, args.Argv[len(args.Argv)-1]
}

func TestProbeGolden(t *testing.T) {
	line, calls, exit := driveOp(t, "probe", "{}", func(verbCall) (any, *protoError) {
		t.Fatal("probe must not touch the sandbox")
		return nil, nil
	})
	if exit != 0 || len(calls) != 0 {
		t.Fatalf("exit=%d calls=%d", exit, len(calls))
	}
	golden := filepath.Join("testdata", "probe_response.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(golden, append(line, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -args -update once): %v", err)
	}
	if !bytes.Equal(append(line, '\n'), want) {
		t.Errorf("probe response deviates from golden:\n got: %s\nwant: %s", line, bytes.TrimSpace(want))
	}
}

func TestRejectsWrongProtocolAndOp(t *testing.T) {
	t.Run("unsupported protocol", func(t *testing.T) {
		stdout := &bytes.Buffer{}
		exit := run(strings.NewReader(`{"protocol":"probavi-adapter/9","request_id":"r-1","op":"probe"}`+"\n"), stdout, io.Discard)
		f := parseFinal(t, stdout.Bytes())
		if exit != 0 || f.OK || f.Error.Code != "unsupported_protocol" {
			t.Errorf("exit=%d final=%+v", exit, f)
		}
		supported, ok := f.Error.Detail["supported"].([]any)
		if !ok || len(supported) == 0 || supported[0] != "probavi-adapter/0" {
			t.Errorf("detail.supported = %v, want the spoken versions (§3.1)", f.Error.Detail)
		}
	})
	t.Run("unknown op", func(t *testing.T) {
		line, _, exit := driveOp(t, "backup", "{}", nil)
		f := parseFinal(t, line)
		if exit != 0 || f.OK || f.Error.Code != "invalid_request" {
			t.Errorf("exit=%d final=%+v", exit, f)
		}
	})
	t.Run("garbage stdin is a crash", func(t *testing.T) {
		// A malformed request carries no request_id to echo, so no valid
		// final response is possible: exit non-zero and let the core
		// classify it as adapter_crash.
		stdout := &bytes.Buffer{}
		if exit := run(strings.NewReader("not json\n"), stdout, io.Discard); exit != 1 {
			t.Errorf("exit=%d, want 1", exit)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want nothing — never emit an unechoable response", stdout)
		}
	})
	t.Run("empty stdin is a crash", func(t *testing.T) {
		if exit := run(strings.NewReader(""), io.Discard, io.Discard); exit != 1 {
			t.Errorf("exit=%d, want 1 — no request means nothing to respond to", exit)
		}
	})
}

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.sql")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func provisionPayload(path, options string) string {
	return fmt.Sprintf(`{"source":{"kind":"mariadb_dump","path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":%s}`, path, options)
}

// happyProvisionHandler simulates a sandbox where the engine needs one
// readiness retry and every verb succeeds, asserting argv shapes en route.
func happyProvisionHandler(t *testing.T, fixture string, readyCalls *int) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "exec":
			return happyExecResponse(t, call, readyCalls), nil
		case "put_file":
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.SourcePath != fixture || args.DestPath != "/scratch/probavi-restore.sql" || args.Mode != "0600" {
				t.Errorf("put_file args = %+v", args)
			}
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.2}, nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

// replayCall is one load the adapter asked the sandbox to run, with the
// shell wrapper unpacked into the parts a test asserts on.
type replayCall struct {
	script string
	args   []string
}

func parseReplay(argv []string) (replayCall, bool) {
	if len(argv) < 5 || argv[0] != "sh" || argv[1] != "-c" || argv[3] != "sh" {
		return replayCall{}, false
	}
	return replayCall{script: argv[2], args: argv[4:]}, true
}

// replayArgs is the argv a load script is expected to run under.
func replayArgs(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

func happyExecResponse(t *testing.T, call verbCall, readyCalls *int) any {
	args, stmt := lastArg(t, call)
	if replay, ok := parseReplay(args.Argv); ok && replay.script == restoreScript {
		want := replayArgs(restoreScript, "/scratch/probavi-restore.sql",
			"orders_admin", "orders", "", strconv.Itoa(markerTailBytes))
		if strings.Join(args.Argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("restore argv = %v", args.Argv)
		}
		return execValue{ExitCode: 0, DurationSeconds: 1.5}
	}
	switch {
	case stmt == "SELECT 1":
		want := []string{"mariadb", "-h", "127.0.0.1", "-u", "orders_admin", "-N", "-B", "-e", "SELECT 1"}
		if strings.Join(args.Argv, " ") != strings.Join(want, " ") {
			t.Errorf("readiness argv = %v", args.Argv)
		}
		*readyCalls++
		if *readyCalls == 1 {
			return okExec(1) // not ready yet: adapter must poll
		}
		return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}
	case stmt == "CREATE DATABASE IF NOT EXISTS `orders`":
		return okExec(0)
	default:
		t.Fatalf("unexpected exec: %v", args.Argv)
		return nil
	}
}

// provisionWire mirrors the §6.2 response payload for assertions.
type provisionWire struct {
	Connection struct {
		Database string `json:"database"`
		User     string `json:"user"`
		Scheme   string `json:"scheme"`
		Port     int    `json:"port"`
	} `json:"connection"`
	SourceIdentity struct {
		Checksum  string  `json:"checksum"`
		SizeBytes int64   `json:"size_bytes"`
		CreatedAt *string `json:"created_at"`
	} `json:"source_identity"`
	Timings struct {
		EngineReady float64 `json:"engine_ready_seconds"`
		Transfer    float64 `json:"transfer_seconds"`
		Restore     float64 `json:"restore_seconds"`
	} `json:"timings"`
	State map[string]string `json:"state"`
}

func assertProvisionResult(t *testing.T, payload json.RawMessage) {
	t.Helper()
	res := provisionWire{}
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	sum := sha256.Sum256([]byte("FAKE-MYSQLDUMP-BYTES"))
	if res.SourceIdentity.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %s — must be a real measurement of the fixture bytes", res.SourceIdentity.Checksum)
	}
	// The fixture carries no mysqldump trailer and the drill declares no
	// backup zone, so there is no creation time to report — and none is
	// invented from the file's mtime.
	if res.SourceIdentity.SizeBytes != 20 || res.SourceIdentity.CreatedAt != nil {
		t.Errorf("source_identity = %+v", res.SourceIdentity)
	}
	if res.Connection.Database != "orders" || res.Connection.User != "orders_admin" ||
		res.Connection.Scheme != "mariadb" || res.Connection.Port != 3306 {
		t.Errorf("connection = %+v — options must override the defaults", res.Connection)
	}
	if res.Timings.Transfer != 0.2 || res.Timings.Restore != 1.5 || res.Timings.EngineReady <= 0 {
		t.Errorf("timings = %+v — must carry the measured values", res.Timings)
	}
	if res.State["database"] != "orders" || res.State["dump_path"] != "/scratch/probavi-restore.sql" {
		t.Errorf("state = %+v", res.State)
	}
}

func TestProvisionHappyPath(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MYSQLDUMP-BYTES")
	readyCalls := 0
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(fixture, `{"user":"orders_admin","database":"orders"}`),
		happyProvisionHandler(t, fixture, &readyCalls))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if len(calls) != 5 { // ready fail, ready ok, put_file, create db, load
		t.Errorf("calls = %d, want 5", len(calls))
	}
	assertProvisionResult(t, f.Payload)
}

func TestProvisionFailures(t *testing.T) {
	fixture := writeFixture(t, "X")
	readyThen := func(rest func(call verbCall) (any, *protoError)) func(verbCall) (any, *protoError) {
		return func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				switch _, stmt := lastArg(t, call); {
				case stmt == "SELECT 1":
					return okExec(0), nil
				case strings.HasPrefix(stmt, "CREATE DATABASE"):
					return okExec(0), nil
				}
			}
			return rest(call)
		}
	}
	loadFails := func(stderr string) func(verbCall) (any, *protoError) {
		return readyThen(func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			return errExec(1, stderr), nil
		})
	}

	tests := []struct {
		name     string
		payload  string
		handler  func(call verbCall) (any, *protoError)
		wantCode string
		maxCalls int
	}{
		{"missing source refused before any verb",
			provisionPayload(filepath.Join(t.TempDir(), "nope.sql"), "{}"),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"source_not_found", 0},
		{"pitr not supported",
			`{"source":{"kind":"mariadb_dump","path":"/x"},"sandbox":{},"options":{},"pitr":{"target_time":"t"}}`,
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"invalid_request", 0},
		{"database name injection refused before any verb",
			provisionPayload(fixture, "{\"database\":\"x`; DROP DATABASE mysql\"}"),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"invalid_request", 0},
		{"not a sql dump",
			provisionPayload(fixture, "{}"),
			loadFails("ERROR 1064 (42000) at line 1 in file: '/scratch/probavi-restore.sql': You have an error in your SQL syntax"),
			"source_corrupt", 5},
		{"binary garbage",
			provisionPayload(fixture, "{}"),
			loadFails(`ERROR at line 1 in file: '/scratch/probavi-restore.sql': ASCII '\0' appeared in the statement`),
			"source_corrupt", 5},
		{"engine failure during load",
			provisionPayload(fixture, "{}"),
			loadFails("ERROR 1114 (HY000) at line 12 in file: '/scratch/probavi-restore.sql': The table 'orders' is full"),
			"restore_failed", 5},
		{"sandbox verb failure propagates",
			provisionPayload(fixture, "{}"),
			func(verbCall) (any, *protoError) {
				return nil, protoErr("sandbox_error", true, "container gone")
			},
			"sandbox_error", 1},
		{"malformed payload",
			`"not an object"`,
			func(verbCall) (any, *protoError) { return nil, nil },
			"invalid_request", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertProvisionFailure(t, tt.payload, tt.handler, tt.wantCode, tt.maxCalls)
		})
	}
}

func assertProvisionFailure(t *testing.T, payload string, handler func(verbCall) (any, *protoError), wantCode string, maxCalls int) {
	t.Helper()
	line, calls, exit := driveOp(t, "provision", payload, handler)
	f := parseFinal(t, line)
	if exit != 0 || f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if f.Error.Code != wantCode {
		t.Errorf("code = %s (%s), want %s", f.Error.Code, f.Error.Message, wantCode)
	}
	if len(calls) > maxCalls {
		t.Errorf("calls = %d, want at most %d", len(calls), maxCalls)
	}
	if strings.Contains(f.Error.Message, `"`) {
		t.Errorf("error message %q must stay quote-free for protocol embedding", f.Error.Message)
	}
}

func TestHealthcheckOp(t *testing.T) {
	payload := `{"connection":{"database":"orders","user":"u"},"state":{}}`

	t.Run("healthy", func(t *testing.T) {
		line, calls, _ := driveOp(t, "healthcheck", payload, func(call verbCall) (any, *protoError) {
			args, stmt := lastArg(t, call)
			if args.Argv[0] != "mariadb" || stmt != "SELECT 1" ||
				!strings.Contains(strings.Join(args.Argv, " "), "-D orders") {
				t.Errorf("healthcheck argv = %v", args.Argv)
			}
			return execValue{ExitCode: 0, DurationSeconds: 0.01,
				StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n")), StderrB64: ""}, nil
		})
		f := parseFinal(t, line)
		res := HealthcheckWire{}
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !f.OK || !res.Healthy || len(calls) != 1 {
			t.Errorf("final=%+v res=%+v calls=%d", f, res, len(calls))
		}
	})

	t.Run("unhealthy is a result, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", payload, func(verbCall) (any, *protoError) {
			return errExec(1, "ERROR 2003 (HY000): Can't connect to MySQL server"), nil
		})
		f := parseFinal(t, line)
		res := HealthcheckWire{}
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !f.OK || res.Healthy {
			t.Errorf("final=%+v res=%+v — unhealthy must come back as ok:true healthy:false", f, res)
		}
	})
}

// HealthcheckWire mirrors the §6.3 response for assertions.
type HealthcheckWire struct {
	Healthy        bool    `json:"healthy"`
	LatencySeconds float64 `json:"latency_seconds"`
	Detail         string  `json:"detail"`
}

func TestTeardownOp(t *testing.T) {
	line, calls, exit := driveOp(t, "teardown", `{"state":{},"reason":"failed"}`, nil)
	f := parseFinal(t, line)
	if exit != 0 || !f.OK || len(calls) != 0 {
		t.Fatalf("exit=%d final=%+v calls=%d", exit, f, len(calls))
	}
	if !strings.Contains(string(f.Payload), `"released":true`) {
		t.Errorf("payload = %s", f.Payload)
	}
}

func TestCoreCallEdgeCases(t *testing.T) {
	newCore := func(input string) *core {
		sc := bufio.NewScanner(strings.NewReader(input))
		sc.Buffer(make([]byte, 64*1024), maxLineBytes)
		return &core{in: sc, out: io.Discard, requestID: "r-test"}
	}

	t.Run("cancelled context refuses new calls", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, perr := newCore("").call(ctx, "exec", execArgs{}); perr == nil || perr.Code != "cancelled" {
			t.Errorf("perr = %+v, want cancelled — §2.4 forbids new calls after SIGTERM", perr)
		}
	})
	t.Run("stream closed mid-call", func(t *testing.T) {
		if _, perr := newCore("").call(context.Background(), "exec", execArgs{}); perr == nil || perr.Code != "internal" {
			t.Errorf("perr = %+v", perr)
		}
	})
	t.Run("call_id mismatch", func(t *testing.T) {
		input := `{"protocol":"probavi-adapter/0","request_id":"r-test","sandbox_result":{"call_id":"c99","ok":true,"value":{}}}` + "\n"
		if _, perr := newCore(input).call(context.Background(), "exec", execArgs{}); perr == nil || !strings.Contains(perr.Message, "does not match") {
			t.Errorf("perr = %+v", perr)
		}
	})
	t.Run("failure without error object", func(t *testing.T) {
		input := `{"protocol":"probavi-adapter/0","request_id":"r-test","sandbox_result":{"call_id":"c1","ok":false}}` + "\n"
		if _, perr := newCore(input).call(context.Background(), "exec", execArgs{}); perr == nil || !strings.Contains(perr.Message, "without error object") {
			t.Errorf("perr = %+v", perr)
		}
	})
	t.Run("malformed exec value", func(t *testing.T) {
		input := `{"protocol":"probavi-adapter/0","request_id":"r-test","sandbox_result":{"call_id":"c1","ok":true,"value":{"stdout_b64":"!!!"}}}` + "\n"
		if _, _, _, perr := newCore(input).exec(context.Background(), execArgs{Argv: []string{"x"}}); perr == nil || !strings.Contains(perr.Message, "stdout_b64") {
			t.Errorf("perr = %+v", perr)
		}
	})
}
