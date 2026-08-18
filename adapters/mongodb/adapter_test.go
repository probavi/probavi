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

func errExec(exit int, stderr string) any {
	return execValue{
		ExitCode:  exit,
		StdoutB64: base64.StdEncoding.EncodeToString(nil),
		StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr)),
	}
}

func stdoutExec(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

// parseExec decodes exec args and classifies the call by its argv head:
// mongosh calls are pings, mongorestore calls are the restore.
func parseExec(t *testing.T, call verbCall) execArgs {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) == 0 {
		t.Fatal("exec with empty argv")
	}
	return args
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
	path := filepath.Join(t.TempDir(), "fixture.archive")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func provisionPayload(path, options string) string {
	return fmt.Sprintf(`{"source":{"kind":"mongodump","path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":%s}`, path, options)
}

// happyProvisionHandler simulates a sandbox where the engine needs one
// readiness retry and every verb succeeds, asserting argv shapes en route.
func happyProvisionHandler(t *testing.T, fixture string, readyCalls *int, wantGzip bool) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "exec":
			return happyExecResponse(t, call, readyCalls, wantGzip), nil
		case "put_file":
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.SourcePath != fixture || args.DestPath != "/scratch/probavi-restore.archive" || args.Mode != "0600" {
				t.Errorf("put_file args = %+v", args)
			}
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.2}, nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

func happyExecResponse(t *testing.T, call verbCall, readyCalls *int, wantGzip bool) any {
	args := parseExec(t, call)
	switch args.Argv[0] {
	case "mongosh":
		if args.Argv[len(args.Argv)-1] == ttlPinEval {
			if strings.Join(args.Argv, " ") != strings.Join(ttlPinArgv, " ") {
				t.Errorf("ttl pin argv = %v, want %v", args.Argv, ttlPinArgv)
			}
			return stdoutExec("1\n")
		}
		want := []string{"mongosh", "--quiet", "--norc", pingURI, "--eval", pingEval}
		if strings.Join(args.Argv, " ") != strings.Join(want, " ") {
			t.Errorf("readiness argv = %v", args.Argv)
		}
		*readyCalls++
		if *readyCalls == 1 {
			return okExec(1) // not ready yet: adapter must poll
		}
		return stdoutExec("1\n")
	case "mongorestore":
		want := []string{"mongorestore", "--host", "127.0.0.1", "--port", "27017",
			"--stopOnError", "--archive=/scratch/probavi-restore.archive"}
		if wantGzip {
			want = append(want, "--gzip")
		}
		if strings.Join(args.Argv, " ") != strings.Join(want, " ") {
			t.Errorf("restore argv = %v, want %v", args.Argv, want)
		}
		return execValue{ExitCode: 0, DurationSeconds: 1.5}
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

func TestProvisionHappyPath(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MONGODUMP-BYTES")
	readyCalls := 0
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(fixture, `{"database":"orders"}`),
		happyProvisionHandler(t, fixture, &readyCalls, false))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if len(calls) != 5 { // ready fail, ready ok, ttl pin, put_file, mongorestore
		t.Errorf("calls = %d, want 5", len(calls))
	}
	assertProvisionResult(t, f.Payload)
}

func assertProvisionResult(t *testing.T, payload json.RawMessage) {
	t.Helper()
	res := provisionWire{}
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	sum := sha256.Sum256([]byte("FAKE-MONGODUMP-BYTES"))
	if res.SourceIdentity.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %s — must be a real measurement of the fixture bytes", res.SourceIdentity.Checksum)
	}
	// A mongodump archive records no backup timestamp (measured), and a
	// file's mtime dates a copy rather than a backup — so this adapter
	// reports no creation time at all rather than one it cannot stand
	// behind.
	if res.SourceIdentity.SizeBytes != 20 || res.SourceIdentity.CreatedAt != nil {
		t.Errorf("source_identity = %+v", res.SourceIdentity)
	}
	if res.Connection.Database != "orders" || res.Connection.User != "" ||
		res.Connection.Scheme != "mongodb" || res.Connection.Port != 27017 {
		t.Errorf("connection = %+v — options must override the default database", res.Connection)
	}
	if res.Timings.Transfer != 0.2 || res.Timings.Restore != 1.5 || res.Timings.EngineReady <= 0 {
		t.Errorf("timings = %+v — must carry the measured values", res.Timings)
	}
	if res.State["database"] != "orders" || res.State["archive_path"] != "/scratch/probavi-restore.archive" {
		t.Errorf("state = %+v", res.State)
	}
}

func TestProvisionGzipArchive(t *testing.T) {
	// A gzip magic number in the artifact must add --gzip to the restore —
	// sniffed from the bytes, never from the file name.
	fixture := writeFixture(t, "\x1f\x8bGZIP-FAKE-BYTES")
	readyCalls := 0
	line, _, exit := driveOp(t, "provision",
		provisionPayload(fixture, `{}`),
		happyProvisionHandler(t, fixture, &readyCalls, true))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.Connection.Database != "admin" {
		t.Errorf("database = %s, want the admin default", res.Connection.Database)
	}
}

// ttlPinArgv is the one command that stops the sandbox from expiring the
// artifact. It is spelled out here rather than built from the adapter's
// pieces so that changing the command has to change this line too.
var ttlPinArgv = []string{"mongosh", "--quiet", "--norc",
	"--host", "127.0.0.1", "--port", "27017", "admin", "--eval", ttlPinEval}

// TestProvisionPinsTheTTLMonitorBeforeRestoring pins the order the fix
// depends on. MongoDB's TTL thread runs on the server's clock, not the
// drill's, so a pin issued after the restore leaves exactly the window
// this change exists to close — the longer the restore, the more of the
// artifact is gone before anything reads it.
func TestProvisionPinsTheTTLMonitorBeforeRestoring(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MONGODUMP-BYTES")
	readyCalls := 0
	_, calls, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"),
		happyProvisionHandler(t, fixture, &readyCalls, false))
	if exit != 0 {
		t.Fatalf("provision exit = %d", exit)
	}
	pin, restore := -1, -1
	for i, call := range calls {
		if call.Verb != "exec" {
			continue
		}
		argv := parseExec(t, call).Argv
		switch {
		case argv[len(argv)-1] == ttlPinEval:
			pin = i
		case argv[0] == "mongorestore":
			restore = i
		}
	}
	if pin < 0 {
		t.Fatal("provision never disabled the TTL monitor")
	}
	if restore < 0 {
		t.Fatal("provision never restored")
	}
	if pin > restore {
		t.Errorf("TTL monitor pinned at call %d, after the restore at call %d", pin, restore)
	}
}

// TestProvisionRefusesAnUnpinnableTTLMonitor holds the deliberate choice
// behind the pin: a drill that cannot stop the engine expiring the
// artifact fails loudly instead of recording whatever survived the clock.
// The message names the parameter, so a server that renamed it says which
// one rather than leaving an operator to guess.
func TestProvisionRefusesAnUnpinnableTTLMonitor(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MONGODUMP-BYTES")
	line, calls, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"),
		func(call verbCall) (any, *protoError) {
			if call.Verb != "exec" {
				t.Fatalf("unexpected verb %s — the artifact must not move before the pin", call.Verb)
			}
			args := parseExec(t, call)
			if args.Argv[len(args.Argv)-1] == ttlPinEval {
				return errExec(1, "MongoServerError: attempted to set unrecognized "+
					"parameter [ttlMonitorEnabled], use help:true to see options"), nil
			}
			return stdoutExec("1\n"), nil
		})
	f := parseFinal(t, line)
	if exit != 0 || f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if f.Error.Code != "invalid_request" {
		t.Errorf("code = %s (%s), want invalid_request", f.Error.Code, f.Error.Message)
	}
	if !strings.Contains(f.Error.Message, "ttlMonitorEnabled") {
		t.Errorf("message = %q, want it to name the parameter", f.Error.Message)
	}
	for _, call := range calls {
		if call.Verb == "exec" && parseExec(t, call).Argv[0] == "mongorestore" {
			t.Error("the drill restored anyway — an unpinnable engine must stop it first")
		}
	}
}

func TestProvisionFailures(t *testing.T) {
	fixture := writeFixture(t, "X")
	restoreFails := func(stderr string) func(verbCall) (any, *protoError) {
		return func(call verbCall) (any, *protoError) {
			switch call.Verb {
			case "put_file":
				return putFileValue{}, nil
			case "exec":
				if args := parseExec(t, call); args.Argv[0] == "mongosh" {
					return stdoutExec("1\n"), nil
				}
				return errExec(1, stderr), nil
			}
			return nil, protoErr("internal", false, "unexpected verb")
		}
	}

	tests := []struct {
		name     string
		payload  string
		handler  func(call verbCall) (any, *protoError)
		wantCode string
		maxCalls int
	}{
		{"missing source refused before any verb",
			provisionPayload(filepath.Join(t.TempDir(), "nope.archive"), "{}"),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"source_not_found", 0},
		{"pitr not supported",
			`{"source":{"kind":"mongodump","path":"/x"},"sandbox":{},"options":{},"pitr":{"target_time":"t"}}`,
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"invalid_request", 0},
		{"connection-string-shaped database refused before any verb",
			provisionPayload(fixture, `{"database":"mongodb://evil/x"}`),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"invalid_request", 0},
		// The fake stderr mirrors the real tool's shape: timestamp-prefixed
		// lines with the Failed verdict FOLLOWED by a summary line — the
		// classifier must find the verdict, not the last line.
		{"not an archive",
			provisionPayload(fixture, "{}"),
			restoreFails("2026-08-01T00:00:00.000+0000\tFailed: stream or file does not appear to be a mongodump archive\n2026-08-01T00:00:00.000+0000\t0 document(s) restored successfully. 0 document(s) failed to restore."),
			"source_corrupt", 4},
		{"wrong magic number",
			provisionPayload(fixture, "{}"),
			restoreFails("2026-08-01T00:00:00.000+0000\tFailed: restore error: error reading archive header: checking magic number: unexpected EOF"),
			"source_corrupt", 4},
		{"gzip bytes lie mid-stream",
			provisionPayload(fixture, "{}"),
			restoreFails("2026-08-01T00:00:00.000+0000\tFailed: gzip: invalid header"),
			"source_corrupt", 4},
		{"engine failure during restore",
			provisionPayload(fixture, "{}"),
			restoreFails("2026-08-01T00:00:00.000+0000\tFailed: orders.orders: error restoring from archive: insertion error: connection closed\n2026-08-01T00:00:00.000+0000\t42 document(s) restored successfully. 1 document(s) failed to restore."),
			"restore_failed", 4},
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
	payload := `{"connection":{"database":"orders","user":""},"state":{}}`

	t.Run("healthy", func(t *testing.T) {
		line, calls, _ := driveOp(t, "healthcheck", payload, func(call verbCall) (any, *protoError) {
			args := parseExec(t, call)
			want := []string{"mongosh", "--quiet", "--norc",
				"--host", "127.0.0.1", "--port", "27017", "orders", "--eval", pingEval}
			if strings.Join(args.Argv, " ") != strings.Join(want, " ") {
				t.Errorf("healthcheck argv = %v", args.Argv)
			}
			return execValue{ExitCode: 0, DurationSeconds: 0.01,
				StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}, nil
		})
		f := parseFinal(t, line)
		res := healthcheckWire{}
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !f.OK || !res.Healthy || len(calls) != 1 {
			t.Errorf("final=%+v res=%+v calls=%d", f, res, len(calls))
		}
	})

	t.Run("empty database pings the default", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"connection":{},"state":{}}`, func(call verbCall) (any, *protoError) {
			args := parseExec(t, call)
			if args.Argv[len(args.Argv)-3] != "admin" {
				t.Errorf("argv = %v, want the admin default as the ping target", args.Argv)
			}
			return stdoutExec("1\n"), nil
		})
		if f := parseFinal(t, line); !f.OK {
			t.Errorf("final = %+v", f)
		}
	})

	t.Run("unhealthy is a result, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", payload, func(verbCall) (any, *protoError) {
			return errExec(1, "MongoNetworkError: connect ECONNREFUSED 127.0.0.1:27017"), nil
		})
		f := parseFinal(t, line)
		res := healthcheckWire{}
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !f.OK || res.Healthy {
			t.Errorf("final=%+v res=%+v — unhealthy must come back as ok:true healthy:false", f, res)
		}
	})
}

// healthcheckWire mirrors the §6.3 response for assertions.
type healthcheckWire struct {
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
