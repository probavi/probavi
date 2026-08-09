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
	"slices"
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

// servingExec is a successful SELECT 1: exit 0, "1" on stdout.
func servingExec() any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}
}

// classify decodes exec args and names the call by its argv shape.
func classify(t *testing.T, call verbCall) (execArgs, string) {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) == 0 {
		t.Fatal("exec with empty argv")
	}
	if args.Argv[0] == sqlcmdPath {
		return args, classifySqlcmd(args)
	}
	if len(args.Argv) >= 3 {
		switch args.Argv[2] {
		case initFileScript:
			return args, "initfile"
		case startScript:
			return args, "start"
		case restoreScript:
			return args, "restore"
		case chainRestoreScript:
			return args, "chain"
		}
	}
	t.Fatalf("unexpected exec: %v", args.Argv)
	return args, ""
}

func classifySqlcmd(args execArgs) string {
	switch {
	case slices.Contains(args.Argv, "-i"):
		return "logins"
	case slices.Contains(args.Argv, orphanQuery):
		return "orphans"
	case strings.Contains(args.Argv[len(args.Argv)-1], "RESTORE HEADERONLY"):
		return "headeronly"
	default:
		return "probe"
	}
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
	path := filepath.Join(t.TempDir(), "fixture.bak")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// headerRow is one RESTORE HEADERONLY result row in the pipe-separated
// shape sqlcmd prints, trimmed to the columns this adapter reads plus
// enough neighbours to keep the layout honest: BackupName,
// BackupDescription, BackupType, ExpirationDate, Compressed, Position.
func headerRow(backupType, position int) string {
	return fmt.Sprintf("NULL|NULL|%d|NULL|0|%d|2|sa|host|shop|957", backupType, position)
}

// fullHeaderExec answers a HEADERONLY probe with a single full backup set.
func fullHeaderExec() any {
	return execValue{ExitCode: 0, DurationSeconds: 0.05,
		StdoutB64: base64.StdEncoding.EncodeToString([]byte(headerRow(backupTypeFull, 1) + "\n"))}
}

// writeMedia writes a file that starts like SQL Server backup media, so
// directory scanning treats it as a candidate.
func writeMedia(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("TAPE"+body), 0o600); err != nil {
		t.Fatalf("write media %s: %v", name, err)
	}
	return path
}

func provisionPayload(path, options string) string {
	return fmt.Sprintf(`{"source":{"kind":"bak","path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":%s}`, path, options)
}

// idleProvisionHandler simulates an idle sandbox: the first probe finds no
// engine (connection refused), the adapter starts sqlservr, one poll
// retries, then everything succeeds — asserting argv and env shapes en
// route.
func idleProvisionHandler(t *testing.T, fixture string, probes *int) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "exec":
			return idleExecResponse(t, call, probes), nil
		case "put_file":
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.SourcePath != fixture || args.DestPath != "/scratch/probavi-restore.bak" || args.Mode != "0600" {
				t.Errorf("put_file args = %+v", args)
			}
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.2}, nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

func idleExecResponse(t *testing.T, call verbCall, probes *int) any {
	args, kind := classify(t, call)
	switch kind {
	case "initfile":
		return okExec(0)
	case "probe":
		if args.Env["SQLCMDPASSWORD"] != sandboxPassword {
			t.Errorf("probe env = %v, want the sandbox constant", args.Env)
		}
		*probes++
		switch *probes {
		case 1:
			return errExec(1, "Sqlcmd: Error: TCP Provider: connection refused")
		case 2:
			return okExec(1) // started, not accepting yet: adapter must poll
		default:
			return servingExec()
		}
	case "start":
		if args.Env["ACCEPT_EULA"] != "Y" || args.Env["MSSQL_SA_PASSWORD"] != sandboxPassword {
			t.Errorf("start env = %v, want EULA acceptance and the sandbox constant", args.Env)
		}
		return okExec(0)
	case "headeronly":
		return fullHeaderExec()
	case "restore":
		want := []string{"sh", "-c", restoreScript, "sh", "/scratch/probavi-restore.bak", "orders", "1"}
		if strings.Join(args.Argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("restore argv = %v", args.Argv)
		}
		if args.Env["SQLCMDPASSWORD"] != sandboxPassword {
			t.Errorf("restore env = %v", args.Env)
		}
		return execValue{ExitCode: 0, DurationSeconds: 1.5}
	default:
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

func TestProvisionIdleSandbox(t *testing.T) {
	fixture := writeFixture(t, "FAKE-BAK-BYTES-HERE!")
	probes := 0
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(fixture, `{"database":"orders"}`),
		idleProvisionHandler(t, fixture, &probes))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	// initfile, probe(refused), start, poll(not ready), poll(ready),
	// put_file, headeronly, restore
	if len(calls) != 8 {
		t.Errorf("calls = %d, want 8", len(calls))
	}
	assertProvisionResult(t, f.Payload)
}

func assertProvisionResult(t *testing.T, payload json.RawMessage) {
	t.Helper()
	res := provisionWire{}
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	sum := sha256.Sum256([]byte("FAKE-BAK-BYTES-HERE!"))
	if res.SourceIdentity.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %s — must be a real measurement of the fixture bytes", res.SourceIdentity.Checksum)
	}
	// The scripted header carries no completion date and the drill
	// declares no backup zone, so there is no creation time to report —
	// and none is invented from the file's mtime.
	if res.SourceIdentity.SizeBytes != 20 || res.SourceIdentity.CreatedAt != nil {
		t.Errorf("source_identity = %+v", res.SourceIdentity)
	}
	if res.Connection.Database != "orders" || res.Connection.User != "sa" ||
		res.Connection.Scheme != "mssql" || res.Connection.Port != 1433 {
		t.Errorf("connection = %+v — options must override the default database", res.Connection)
	}
	if res.Timings.Transfer != 0.2 || res.Timings.Restore != 1.5 || res.Timings.EngineReady <= 0 {
		t.Errorf("timings = %+v — must carry the measured values", res.Timings)
	}
	if res.State["database"] != "orders" || res.State["bak_path"] != "/scratch/probavi-restore.bak" ||
		res.State["backup_set"] != "1" {
		t.Errorf("state = %+v", res.State)
	}
}

// TestProvisionAdoptsRunningEngine covers the conformance-suite shape too:
// when the very first probe answers, no start happens.
func TestProvisionAdoptsRunningEngine(t *testing.T) {
	fixture := writeFixture(t, "X")
	line, calls, exit := driveOp(t, "provision", provisionPayload(fixture, `{}`),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			args, kind := classify(t, call)
			if kind == "start" {
				t.Errorf("a serving engine must be adopted, not restarted: %v", args.Argv)
			}
			if kind == "headeronly" {
				return fullHeaderExec(), nil
			}
			return servingExec(), nil
		})
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	// initfile, probe(ok), put_file, headeronly, restore
	if len(calls) != 5 {
		t.Errorf("calls = %d, want 5", len(calls))
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.Connection.Database != defaultDatabase {
		t.Errorf("database = %s, want the %s default", res.Connection.Database, defaultDatabase)
	}
}

func TestProvisionFailures(t *testing.T) {
	fixture := writeFixture(t, "X")
	restoreFails := func(stderr string) func(verbCall) (any, *protoError) {
		return func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			switch _, kind := classify(t, call); kind {
			case "restore":
				return errExec(1, stderr), nil
			case "headeronly":
				return fullHeaderExec(), nil
			}
			return servingExec(), nil
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
			provisionPayload(filepath.Join(t.TempDir(), "nope.bak"), "{}"),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"source_not_found", 0},
		{"pitr not supported",
			`{"source":{"kind":"bak","path":"/x"},"sandbox":{},"options":{},"pitr":{"target_time":"t"}}`,
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"invalid_request", 0},
		{"tsql-shaped database refused before any verb",
			provisionPayload(fixture, `{"database":"x]; DROP DATABASE [master"}`),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"invalid_request", 0},
		// Fake stderr mirrors the real server's shape: "Msg N, Level …"
		// header lines with the message text after each — the classifier
		// must read the first message, not the headers or the trailing
		// "terminating abnormally".
		{"random garbage",
			provisionPayload(fixture, "{}"),
			restoreFails("Msg 3241, Level 16, State 1, Server x, Line 1\nThe media family on device '/scratch/probavi-restore.bak' is incorrectly formed. SQL Server cannot process this media family.\nMsg 3013, Level 16, State 1, Server x, Line 1\nRESTORE DATABASE is terminating abnormally."),
			"source_corrupt", 5},
		{"text garbage",
			provisionPayload(fixture, "{}"),
			restoreFails("Msg 3254, Level 16, State 1, Server x, Line 1\nThe volume on device '/scratch/probavi-restore.bak' is empty.\nMsg 3013, Level 16, State 1, Server x, Line 1\nRESTORE DATABASE is terminating abnormally."),
			"source_corrupt", 5},
		{"engine failure during restore",
			provisionPayload(fixture, "{}"),
			restoreFails("Msg 3257, Level 16, State 1, Server x, Line 1\nThere is insufficient free space on disk volume '/var/opt/mssql/data'.\nMsg 3013, Level 16, State 1, Server x, Line 1\nRESTORE DATABASE is terminating abnormally."),
			"restore_failed", 5},
		{"engine with foreign credentials",
			provisionPayload(fixture, "{}"),
			func(call verbCall) (any, *protoError) {
				if _, kind := classify(t, call); kind == "probe" {
					return errExec(1, "Sqlcmd: Error: Microsoft ODBC Driver 18 for SQL Server : Login failed for user 'sa'.."), nil
				}
				return okExec(0), nil
			},
			"invalid_request", 2},
		{"unstartable engine",
			provisionPayload(fixture, "{}"),
			func(call verbCall) (any, *protoError) {
				switch _, kind := classify(t, call); kind {
				case "probe":
					return errExec(1, "connection refused"), nil
				case "start":
					return errExec(127, "sh: /opt/mssql/bin/sqlservr: not found"), nil
				default:
					return okExec(0), nil
				}
			},
			"invalid_request", 3},
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

func TestHealthcheckHealthy(t *testing.T) {
	payload := `{"connection":{"database":"orders","user":"sa"},"state":{}}`
	line, calls, _ := driveOp(t, "healthcheck", payload, func(call verbCall) (any, *protoError) {
		args, kind := classify(t, call)
		if kind != "probe" {
			t.Fatalf("healthcheck ran %v", args.Argv)
		}
		joined := strings.Join(args.Argv, " ")
		if !strings.Contains(joined, "-d orders") {
			t.Errorf("healthcheck argv = %v, want the connection database", args.Argv)
		}
		// The adapter's own execs run without the NOCOUNT init file: the
		// answer is the first line, the trailer must not matter.
		return execValue{ExitCode: 0, DurationSeconds: 0.01,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n\n(1 rows affected)\n"))}, nil
	})
	f := parseFinal(t, line)
	res := healthcheckWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !f.OK || !res.Healthy || len(calls) != 1 {
		t.Errorf("final=%+v res=%+v calls=%d", f, res, len(calls))
	}
}

func TestHealthcheckSuspiciousDatabaseFallsBack(t *testing.T) {
	line, _, _ := driveOp(t, "healthcheck", `{"connection":{"database":"x]; DROP"},"state":{}}`, func(call verbCall) (any, *protoError) {
		args, _ := classify(t, call)
		if !strings.Contains(strings.Join(args.Argv, " "), "-d "+defaultDatabase) {
			t.Errorf("argv = %v, want the %s default", args.Argv, defaultDatabase)
		}
		return servingExec(), nil
	})
	if f := parseFinal(t, line); !f.OK {
		t.Errorf("final = %+v", f)
	}
}

func TestHealthcheckUnhealthyIsAResult(t *testing.T) {
	payload := `{"connection":{"database":"orders","user":"sa"},"state":{}}`
	line, _, _ := driveOp(t, "healthcheck", payload, func(verbCall) (any, *protoError) {
		return errExec(1, "Sqlcmd: Error: connection refused"), nil
	})
	f := parseFinal(t, line)
	res := healthcheckWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !f.OK || res.Healthy {
		t.Errorf("final=%+v res=%+v — unhealthy must come back as ok:true healthy:false", f, res)
	}
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

func TestVerdictLine(t *testing.T) {
	stderr := "Msg 3241, Level 16, State 1, Server x, Line 1\nThe media family on device 'x' is \"incorrectly\" formed.\nMsg 3013, Level 16, State 1, Server x, Line 1\nRESTORE DATABASE is terminating abnormally."
	want := "The media family on device 'x' is 'incorrectly' formed."
	if got := verdictLine([]byte(stderr)); got != want {
		t.Errorf("verdictLine = %q, want %q (first message line, quotes stripped)", got, want)
	}
	if got := verdictLine([]byte("")); got != "" {
		t.Errorf("verdictLine(empty) = %q", got)
	}
	if got := verdictLine([]byte("Msg 1, Level 1\nMsg 2, Level 2")); got != "" {
		t.Errorf("verdictLine(headers only) = %q", got)
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
