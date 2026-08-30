package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

func okExec() any { return execValue{ExitCode: 0} }

func errExec(exit int, stderr string) any {
	return execValue{ExitCode: exit, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

func provisionPayload(t *testing.T, kind, path string, params map[string]string) string {
	t.Helper()
	req := map[string]any{
		"source":  map[string]any{"kind": kind, "path": path, "params": params},
		"sandbox": map[string]any{"scratch_dir": "/scratch"},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

// argvOf decodes an exec call's argv.
func argvOf(t *testing.T, call verbCall) []string {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) == 0 {
		t.Fatal("exec with empty argv")
	}
	return args.Argv
}

// classifyExec labels one exec call of the happy path and returns its
// simulated result; an empty label means the call was not expected. The
// calls are told apart by shape, which is why each script takes a
// different number of arguments.
func classifyExec(argv []string) (string, any) {
	switch {
	case argv[0] == "mkdir":
		return "mkdir", okExec()
	case argv[0] != "sh":
		return "", nil
	}
	switch len(argv) {
	case 3:
		return "preflight", execValue{ExitCode: 0, DurationSeconds: 0.02}
	case 5:
		return "start", execValue{ExitCode: 0, DurationSeconds: 1.2}
	case 6:
		return "place", execValue{ExitCode: 0, DurationSeconds: 0.3}
	case 7:
		return "count", execValue{ExitCode: 0, DurationSeconds: 0.05,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("500"))}
	case 8:
		return "replay", execValue{ExitCode: 0, DurationSeconds: 0.6}
	}
	return "", nil
}

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.4}, nil
		}
		label, value := classifyExec(argvOf(t, call))
		if label == "" {
			t.Fatalf("unexpected exec: %v", argvOf(t, call))
		}
		*sequence = append(*sequence, label)
		return value, nil
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
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update once): %v", err)
	}
	if !bytes.Equal(append(line, '\n'), want) {
		t.Errorf("probe response deviates from golden:\n got: %s\nwant: %s", line, bytes.TrimSpace(want))
	}
}

// TestRunnerTemplateShape pins the runner contract of §6.1. CouchDB speaks
// HTTP rather than SQL, so {{sql}} carries a path and query string
// relative to the restored database — the engine's own client arguments,
// which is what the glossary allows where there is no SQL.
func TestRunnerTemplateShape(t *testing.T) {
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("probe declares no sql_runner")
	}
	argv, ok := runner["argv"].([]string)
	if !ok {
		t.Fatal("sql_runner declares no argv")
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"{{user}}", "{{database}}", "{{sql}}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("runner argv lacks %s: %v", want, argv)
		}
	}
	if strings.Contains(joined, sandboxPassword) {
		t.Error("the sandbox password sits in the runner argv, where a process table would show it")
	}
	env, ok := runner["env"].(map[string]string)
	if !ok || env[passwordVarName] != sandboxPassword {
		t.Errorf("runner env = %v, want %s to carry the documented sandbox constant", env, passwordVarName)
	}
}

// TestEveryScriptAuthenticates pins that no request this adapter makes
// goes out unauthenticated: CouchDB 3.x answers 401 without credentials
// (measured), so an unauthenticated call would fail late and confusingly.
func TestEveryScriptAuthenticates(t *testing.T) {
	for name, script := range map[string]string{
		"checkScript":       checkScript,
		"startEngineScript": startEngineScript,
		"replayScript":      replayScript,
	} {
		if strings.Count(script, "curl") != strings.Count(script, "--user") {
			t.Errorf("%s: %d curl calls but %d --user flags", name,
				strings.Count(script, "curl"), strings.Count(script, "--user"))
		}
	}
}

// TestTheCompactionDaemonIsSuspendedOnEveryStart is this engine's line of
// issue #166. CouchDB's compactor runs unbidden, and compaction is exactly
// the operation that drops old revisions and the bodies of deleted
// documents — it can only subtract from what the backup holds. Emptying
// smoosh's channels is the switch, and it belongs to starting the engine
// rather than to one source kind, because every kind starts it.
func TestTheCompactionDaemonIsSuspendedOnEveryStart(t *testing.T) {
	if !strings.Contains(startEngineScript, "smoosh") {
		t.Fatal("the engine is started without suspending the compaction daemon")
	}
	for _, ch := range []string{"db_channels", "view_channels"} {
		if !strings.Contains(startEngineScript, ch) {
			t.Errorf("smoosh channel %s is left running", ch)
		}
	}
	// Suspension, not a rewrite: an explicit compaction must still be
	// possible, so nothing here may touch the _compact endpoint.
	if strings.Contains(startEngineScript, "_compact\"") {
		t.Error("the adapter interferes with explicit compaction; only the daemon is suspended")
	}
}

func assertPutDest(t *testing.T, calls []verbCall, want string) {
	t.Helper()
	for _, c := range calls {
		if c.Verb != "put_file" {
			continue
		}
		args := putFileArgs{}
		if err := json.Unmarshal(c.Args, &args); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
		if args.DestPath == want {
			return
		}
		t.Fatalf("put_file dest = %s, want %s", args.DestPath, want)
	}
	t.Fatal("no put_file call")
}

func TestProvisionReplaysABackup(t *testing.T) {
	path := writeArtifact(t, t.TempDir(), "nightly.jsonl", backupFixture(3))

	var sequence []string
	line, calls, exit := driveOp(t, "provision", provisionPayload(t, "couchbackup", path, nil),
		provisionHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit %d: %s", exit, line)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %s", line)
	}
	// The engine starts before the replay, and the count is the verdict
	// after it.
	want := []string{"preflight", "mkdir", "start", "put_file", "replay", "count"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("call sequence = %v, want %v", sequence, want)
	}
	assertPutDest(t, calls, "/scratch/probavi-couchdb/backup.jsonl")

	res := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Database string `json:"database"`
			User     string `json:"user"`
			Port     int    `json:"port"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
	}{}
	if err := json.Unmarshal(final.Payload, &res); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if res.Connection.Database != defaultDatabase || res.Connection.User != defaultUser ||
		res.Connection.Scheme != "http" || res.Connection.Port != 5984 {
		t.Errorf("connection = %+v", res.Connection)
	}
	if res.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null: no CouchDB artifact dates the backup", *res.SourceIdentity.CreatedAt)
	}
}

func TestProvisionPlacesADataDirectory(t *testing.T) {
	dir := dataDirFixture(t)

	var sequence []string
	line, calls, exit := driveOp(t, "provision", provisionPayload(t, "couchdb_data", dir, nil),
		provisionHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit %d: %s", exit, line)
	}
	if !parseFinal(t, line).OK {
		t.Fatalf("provision failed: %s", line)
	}
	// The data goes in BEFORE the engine starts: CouchDB caches its
	// database registry at startup, so a tree placed under a running
	// server is invisible to it (measured).
	want := []string{"preflight", "mkdir", "put_file", "place", "start", "count"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("call sequence = %v, want %v", sequence, want)
	}
	assertPutDest(t, calls, "/scratch/probavi-couchdb/data")
}

// TestAWellFormedZeroIsRefused pins the verdict a measurement forced. A
// shard file truncated at its tail leaves a database CouchDB opens without
// complaint and serves with HTTP 200 while holding 280 documents of 500,
// so "the engine answered" cannot be the verdict; the document count is,
// and zero is a refusal.
func TestAWellFormedZeroIsRefused(t *testing.T) {
	path := writeArtifact(t, t.TempDir(), "nightly.jsonl", backupFixture(2))

	line, _, exit := driveOp(t, "provision", provisionPayload(t, "couchbackup", path, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			if len(argv) == 7 {
				return execValue{ExitCode: 0, DurationSeconds: 0.05,
					StdoutB64: base64.StdEncoding.EncodeToString([]byte("0"))}, nil
			}
			label, value := classifyExec(argv)
			if label == "" {
				t.Fatalf("unexpected exec: %v", argv)
			}
			return value, nil
		})
	if exit != 0 {
		t.Fatalf("adapter exited %d; a refusal is a final response", exit)
	}
	final := parseFinal(t, line)
	if final.OK || final.Error == nil {
		t.Fatalf("provision accepted a restore that produced no documents: %s", line)
	}
	if final.Error.Code != "source_corrupt" || !strings.Contains(final.Error.Message, "holds no documents") {
		t.Errorf("error = %s/%q, want source_corrupt naming the empty restore",
			final.Error.Code, final.Error.Message)
	}
}

func TestProvisionRefusals(t *testing.T) {
	good := writeArtifact(t, t.TempDir(), "nightly.jsonl", backupFixture(2))

	tests := []struct {
		name     string
		payload  string
		handler  func(verbCall) (any, *protoError)
		wantCode string
		wantText string
	}{
		{
			name:     "pitr is not offered",
			payload:  `{"source":{"kind":"couchbackup","path":"` + good + `"},"pitr":{"target_time":"2026-01-01T00:00:00Z"}}`,
			wantCode: "invalid_request", wantText: "does not support pitr",
		},
		{
			name:     "backup_timezone has nothing to apply to",
			payload:  provisionPayload(t, "couchbackup", good, map[string]string{"backup_timezone": "UTC"}),
			wantCode: "invalid_request", wantText: "has no effect",
		},
		{
			name:     "a sandbox that is not a couchdb image",
			payload:  provisionPayload(t, "couchbackup", good, nil),
			handler:  failingPreflight,
			wantCode: "invalid_request", wantText: "not a CouchDB image",
		},
		{
			name:     "an engine that will not start",
			payload:  provisionPayload(t, "couchbackup", good, nil),
			handler:  failingAt(t, 5, "couchdb did not answer within 120s"),
			wantCode: "restore_failed", wantText: "starting couchdb",
		},
		{
			name:     "a batch the engine refuses",
			payload:  provisionPayload(t, "couchbackup", good, nil),
			handler:  failingAt(t, 8, "batch 2 refused with 400"),
			wantCode: "restore_failed", wantText: "replaying the backup",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.handler
			if handler == nil {
				handler = func(verbCall) (any, *protoError) { return okExec(), nil }
			}
			line, _, exit := driveOp(t, "provision", tc.payload, handler)
			if exit != 0 {
				t.Fatalf("adapter exited %d; a refusal is a final response, not a crash", exit)
			}
			final := parseFinal(t, line)
			if final.OK || final.Error == nil {
				t.Fatalf("provision accepted what it must refuse: %s", line)
			}
			if final.Error.Code != tc.wantCode {
				t.Errorf("code = %s, want %s (%s)", final.Error.Code, tc.wantCode, final.Error.Message)
			}
			if !strings.Contains(final.Error.Message, tc.wantText) {
				t.Errorf("message = %q, want it to contain %q", final.Error.Message, tc.wantText)
			}
		})
	}
}

// failingPreflight simulates a sandbox that is not a CouchDB image.
func failingPreflight(verbCall) (any, *protoError) {
	return errExec(1, "no /opt/couchdb/data — this is not a couchdb image"), nil
}

// failingAt simulates a sandbox where everything works until the call with
// the given argv length, which is how the scripts are told apart.
func failingAt(t *testing.T, argc int, stderr string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		argv := argvOf(t, call)
		if len(argv) == argc {
			return errExec(1, stderr), nil
		}
		label, value := classifyExec(argv)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argv)
		}
		return value, nil
	}
}

func TestRejectsWrongProtocolAndOp(t *testing.T) {
	line, _, exit := driveOp(t, "sing", "{}", func(verbCall) (any, *protoError) {
		t.Fatal("an unknown op must touch nothing")
		return nil, nil
	})
	if exit != 0 {
		t.Fatalf("exit %d, want a final response", exit)
	}
	final := parseFinal(t, line)
	if final.OK || final.Error == nil || final.Error.Code != "invalid_request" {
		t.Fatalf("unknown op = %s, want invalid_request", line)
	}
}

func TestHealthcheck(t *testing.T) {
	payload := `{"connection":{"database":"restored","user":"admin"}}`
	line, calls, exit := driveOp(t, "healthcheck", payload, func(call verbCall) (any, *protoError) {
		argv := argvOf(t, call)
		if argv[0] != "sh" || len(argv) != 7 {
			t.Fatalf("healthcheck ran %v, want the check script", argv)
		}
		if !strings.Contains(argv[6], "_all_docs") {
			t.Errorf("healthcheck query = %q, want one that counts documents", argv[6])
		}
		return execValue{ExitCode: 0, DurationSeconds: 0.02,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("42"))}, nil
	})
	if exit != 0 || len(calls) != 1 {
		t.Fatalf("exit=%d calls=%d", exit, len(calls))
	}
	res := struct {
		Healthy bool   `json:"healthy"`
		Detail  string `json:"detail"`
	}{}
	if err := json.Unmarshal(parseFinal(t, line).Payload, &res); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if !res.Healthy || !strings.Contains(res.Detail, "42 documents") {
		t.Errorf("healthcheck = %+v, want healthy with the document count", res)
	}
}

// TestHealthcheckReportsUnhealthyRatherThanFailing pins §6.3: a database
// that no longer answers is a result, not an operation error.
func TestHealthcheckReportsUnhealthyRatherThanFailing(t *testing.T) {
	line, _, exit := driveOp(t, "healthcheck", `{"connection":{"database":"restored","user":"admin"}}`,
		func(verbCall) (any, *protoError) { return errExec(1, "couchdb answered 500"), nil })
	if exit != 0 {
		t.Fatalf("exit %d, want a final response", exit)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("an unhealthy database must be a result, not an error: %s", line)
	}
	res := struct {
		Healthy bool   `json:"healthy"`
		Detail  string `json:"detail"`
	}{}
	if err := json.Unmarshal(final.Payload, &res); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if res.Healthy || !strings.Contains(res.Detail, "couchdb answered 500") {
		t.Errorf("healthcheck = %+v, want unhealthy naming the engine's own answer", res)
	}
}
