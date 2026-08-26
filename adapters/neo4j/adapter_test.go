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

func outExec(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout)), DurationSeconds: 0.1}
}

func errExec(exit int, stderr string) any {
	return execValue{
		ExitCode:        exit,
		StderrB64:       base64.StdEncoding.EncodeToString([]byte(stderr)),
		DurationSeconds: 0.1,
	}
}

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

// step names the provision step an exec call belongs to, by the script it
// carries — the adapter's steps are bash fragments, so the fragment is
// the identity.
func step(t *testing.T, call verbCall) (string, execArgs) {
	t.Helper()
	args := parseExec(t, call)
	if args.Argv[0] == "mkdir" {
		return "mkdir", args
	}
	if len(args.Argv) < 3 || args.Argv[0] != "bash" || args.Argv[1] != "-c" {
		t.Fatalf("unexpected exec argv %v", args.Argv)
	}
	switch args.Argv[2] {
	case hostsScript:
		return "hosts", args
	case toolScript:
		return "tools", args
	case passwordScript:
		return "password", args
	case infoScript:
		return "info", args
	case loadScript:
		return "load", args
	case startScript:
		return "start", args
	case healthScript:
		return "health", args
	case onlineScript:
		return "online", args
	case servedScript:
		return "served", args
	}
	t.Fatalf("unexpected script: %s", args.Argv[2])
	return "", args
}

const (
	goodInfo   = "Database: orders\nFormat: Neo4j ZSTD Dump.\nFiles: 37\nBytes: 270532608\n"
	goodStatus = "neo4j, online\nsystem, online\n"
)

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "whatever-the-job-called-it")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func provisionPayload(path, options string) string {
	return fmt.Sprintf(
		`{"source":{"kind":"neo4j_dump","path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":%s}`,
		path, options)
}

// happyHandler simulates a sandbox where the engine needs one readiness
// retry and every step succeeds.
func happyHandler(t *testing.T, fixture, database string, seen *[]string) func(verbCall) (any, *protoError) {
	t.Helper()
	onlineCalls := 0
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*seen = append(*seen, "put_file")
			return happyPutFile(t, call, fixture, database), nil
		}
		name, args := step(t, call)
		*seen = append(*seen, name)
		if name == "online" {
			onlineCalls++
			if onlineCalls == 1 {
				// The server is not answering yet: the gate's query fails
				// outright, which is what the readiness wait is made of.
				return errExec(1, "Connection refused"), nil
			}
		}
		return happyStep(t, name, args, database), nil
	}
}

func happyPutFile(t *testing.T, call verbCall, fixture, database string) any {
	t.Helper()
	args := putFileArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("put_file args: %v", err)
	}
	if args.SourcePath != fixture {
		t.Errorf("put_file source = %s, want %s", args.SourcePath, fixture)
	}
	// The engine derives the file name from the database name, so the
	// operator's file name must not survive the transfer.
	if args.DestPath != "/scratch/probavi-neo4j/"+database+".dump" || args.Mode != "0600" {
		t.Errorf("put_file args = %+v", args)
	}
	return putFileValue{BytesCopied: 4, DurationSeconds: 0.2}
}

// happyStep answers one provision step the way the verified image does,
// asserting on the way that the step was handed what it needs.
func happyStep(t *testing.T, name string, args execArgs, database string) any {
	t.Helper()
	switch name {
	case "info":
		wantArgs(t, name, args, database, "/scratch/probavi-neo4j")
		return outExec(goodInfo)
	case "load":
		wantArgs(t, name, args, database, "/scratch/probavi-neo4j")
		return errExec(0, "Files: 37/37, data: 100.0%\nDone: 37 files, 258.0MiB processed in 0.189 seconds.\n")
	case "online":
		wantArgs(t, name, args, database)
		return outExec("1\n")
	case "password":
		wantArgs(t, name, args, sandboxPassword)
		return outExec("")
	}
	return outExec("")
}

// wantArgs asserts the positional parameters a script fragment received.
func wantArgs(t *testing.T, name string, args execArgs, want ...string) {
	t.Helper()
	got := args.Argv[min(4, len(args.Argv)):]
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("%s args = %v, want %v", name, got, want)
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

// TestProbeRunnerObeysTheRunnerContract pins the two §6.1 rules a probe
// can break silently: the check text must be its own argv element, and a
// password may never appear in argv.
func TestProbeRunnerObeysTheRunnerContract(t *testing.T) {
	payload, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatalf("probe payload = %T", probePayload())
	}
	runner, ok := payload["sql_runner"].(map[string]any)
	if !ok {
		t.Fatalf("sql_runner = %T", payload["sql_runner"])
	}
	argv, ok := runner["argv"].([]string)
	if !ok {
		t.Fatalf("argv = %T", runner["argv"])
	}
	found := false
	for _, a := range argv {
		if a == "{{sql}}" {
			found = true
		}
		if strings.Contains(a, "{{password}}") {
			t.Errorf("argv element %q carries {{password}} — secrets belong in env values only", a)
		}
	}
	if !found {
		t.Errorf("argv = %v, want {{sql}} as its own element", argv)
	}
	env, ok := runner["env"].(map[string]string)
	if !ok {
		t.Fatalf("env = %T", runner["env"])
	}
	if env["NEO4J_PASSWORD"] != sandboxPassword || env["NEO4J_USERNAME"] != "{{user}}" {
		t.Errorf("env = %v, want the sandbox constant and the connection user", env)
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

// TestProvisionHappyPath pins the order of the whole restore: the store
// is prepared while no server is running, and the engine starts only
// after the dump is in it.
func TestProvisionHappyPath(t *testing.T) {
	fixture := writeFixture(t, "DZV1 pretend dump")
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"), happyHandler(t, fixture, "neo4j", &seen))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := []string{"hosts", "tools", "mkdir", "password", "put_file", "info", "load", "start", "online", "online"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v,\n want %v", seen, want)
	}
	got := parseProvisionPayload(t, f.Payload)
	assertConnection(t, got)
	assertIdentity(t, got, "DZV1 pretend dump")

	if got.Timings.Restore <= 0 || got.Timings.EngineReady <= 0 || got.Timings.Transfer <= 0 {
		t.Errorf("timings = %+v, want real measurements", got.Timings)
	}
	if got.State.Database != "neo4j" || got.State.WorkDir != "/scratch/probavi-neo4j" {
		t.Errorf("state = %+v", got.State)
	}
}

// provisionPayloadShape is the §6.2 response, as a test reads it.
type provisionPayloadShape struct {
	Connection struct {
		Scheme, Host, Database, User string
		Port                         int
		PasswordEnv                  string `json:"password_env"`
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
	State struct {
		Database string `json:"database"`
		WorkDir  string `json:"work_dir"`
	} `json:"state"`
}

func parseProvisionPayload(t *testing.T, payload []byte) provisionPayloadShape {
	t.Helper()
	got := provisionPayloadShape{}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return got
}

func assertConnection(t *testing.T, got provisionPayloadShape) {
	t.Helper()
	if got.Connection.Scheme != "neo4j" || got.Connection.Port != 7687 ||
		got.Connection.Database != "neo4j" || got.Connection.User != "neo4j" {
		t.Errorf("connection = %+v", got.Connection)
	}
	// The engine password is a documented constant, not an env var the
	// core resolves: naming one would ask the core for a secret that does
	// not exist.
	if got.Connection.PasswordEnv != "" {
		t.Errorf("password_env = %q, want empty", got.Connection.PasswordEnv)
	}
}

func assertIdentity(t *testing.T, got provisionPayloadShape, body string) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	if got.SourceIdentity.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %s, want the hash of the artifact bytes", got.SourceIdentity.Checksum)
	}
	// A Neo4j dump records no backup timestamp, so the record carries none.
	if got.SourceIdentity.SizeBytes != int64(len(body)) || got.SourceIdentity.CreatedAt != nil {
		t.Errorf("source identity = %+v, want the artifact size and no creation time", got.SourceIdentity)
	}
}

// TestProvisionRejectsBeforeTouchingTheSandbox covers everything the
// adapter can refuse from the request alone — none of it may reach the
// engine.
func TestProvisionRejectsBeforeTouchingTheSandbox(t *testing.T) {
	fixture := writeFixture(t, "DZV1")
	tests := map[string]struct {
		payload string
		code    string
	}{
		"pitr": {
			fmt.Sprintf(`{"source":{"kind":"neo4j_dump","path":%q},"sandbox":{},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`, fixture),
			"invalid_request"},
		"backup timezone": {
			fmt.Sprintf(`{"source":{"kind":"neo4j_dump","path":%q,"params":{"backup_timezone":"Europe/Budapest"}},"sandbox":{}}`, fixture),
			"invalid_request"},
		"uppercase database": {provisionPayload(fixture, `{"database":"Orders"}`), "invalid_request"},
		"short database":     {provisionPayload(fixture, `{"database":"ab"}`), "invalid_request"},
		"database with a slash": {
			provisionPayload(fixture, `{"database":"../etc"}`), "invalid_request"},
		"unsupported kind": {
			fmt.Sprintf(`{"source":{"kind":"neo4j_backup","path":%q},"sandbox":{}}`, fixture), "unsupported_source"},
		"missing source": {
			`{"source":{"kind":"neo4j_dump","path":"/nonexistent/orders.dump"},"sandbox":{}}`, "source_not_found"},
		"malformed payload": {`"not an object"`, "invalid_request"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "provision", tt.payload, func(verbCall) (any, *protoError) {
				t.Error("the adapter reached the sandbox for a request it could refuse on its own")
				return outExec(""), nil
			})
			f := parseFinal(t, line)
			if exit != 0 || f.OK || f.Error.Code != tt.code {
				t.Errorf("exit=%d final=%+v, want %s", exit, f, tt.code)
			}
			if len(calls) != 0 {
				t.Errorf("calls = %d, want none", len(calls))
			}
		})
	}
}

func TestProvisionRefusesADirectoryForTheFileKind(t *testing.T) {
	line, _, _ := driveOp(t, "provision", provisionPayload(t.TempDir(), "{}"), func(verbCall) (any, *protoError) {
		t.Error("the adapter reached the sandbox")
		return outExec(""), nil
	})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "neo4j_dump_dir") {
		t.Errorf("final = %+v, want the kind to be named", f)
	}
}

// TestProvisionFailurePaths uses the engine's own words, measured on the
// verified image, so a message the tool changes fails here rather than in
// an operator's evidence record.
func TestProvisionFailurePaths(t *testing.T) {
	tests := map[string]struct {
		failAt  string
		exit    int
		stream  string
		code    string
		message string
	}{
		"hostname cannot be made to resolve": {
			"hosts", 1, "hostname: Name or service not known", "engine_not_ready", "resolve its own hostname"},
		"image is not a neo4j image": {
			"tools", 1, "", "invalid_request", "must be a Neo4j image"},
		"password refused because the server already ran": {
			"password", 1, "The initial password cannot be set", "engine_not_ready", "start idle"},
		"artifact is not an archive": {
			"info", 1, "Print metadata failed for databases: 'neo4j'\nRun with '--verbose' for a more detailed error message.",
			"source_corrupt", "does not recognize"},
		"dump is truncated": {
			"load", 1, "Files: 1/37, data: 0.0%\nFailed to load database 'neo4j': Unable to load database: ZstdIOException: Truncated source\nLoad failed for databases: 'neo4j'",
			"source_corrupt", "Truncated source"},
		"dump is random bytes": {
			"load", 1, "Failed to load database 'neo4j': Not a valid Neo4j archive: /scratch/probavi-neo4j/neo4j.dump\nLoad failed for databases: 'neo4j'",
			"source_corrupt", "Not a valid Neo4j archive"},
		"database is already mounted": {
			"load", 1, "Failed to load database 'neo4j': The database is in use. Stop database 'neo4j' and try again.\nLoad failed for databases: 'neo4j'",
			"restore_failed", "The database is in use"},
		"engine will not start": {
			"start", 1, "Configuration is invalid. See log for more info.", "engine_not_ready", "did not start"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := writeFixture(t, "DZV1")
			var seen []string
			happy := happyHandler(t, fixture, "neo4j", &seen)
			line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"),
				func(call verbCall) (any, *protoError) {
					if call.Verb == "exec" {
						if name, _ := step(t, call); name == tt.failAt {
							return errExec(tt.exit, tt.stream), nil
						}
					}
					return happy(call)
				})
			f := parseFinal(t, line)
			if exit != 0 || f.OK || f.Error.Code != tt.code {
				t.Fatalf("exit=%d final=%+v, want %s", exit, f, tt.code)
			}
			if !strings.Contains(f.Error.Message, tt.message) {
				t.Errorf("message = %q, want it to carry %q", f.Error.Message, tt.message)
			}
			if strings.Contains(f.Error.Message, `"`) {
				t.Errorf("message %q must stay quote-free for protocol embedding", f.Error.Message)
			}
		})
	}
}

// TestProvisionRefusesADatabaseTheEngineDoesNotServe is the measured
// trap: Community Edition mounts only the database its configuration
// names, and a dump loaded under any other name lands on disk without
// ever being mounted. The server serves, every check runs, and the drill
// would prove nothing.
func TestProvisionRefusesADatabaseTheEngineDoesNotServe(t *testing.T) {
	fixture := writeFixture(t, "DZV1")
	var seen []string
	happy := happyHandler(t, fixture, "orders", &seen)
	asked := false
	line, _, _ := driveOp(t, "provision", provisionPayload(fixture, `{"database":"orders"}`),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				switch name, _ := step(t, call); name {
				case "online":
					return outExec("0\n"), nil
				case "served":
					asked = true
					return outExec(goodStatus), nil
				}
			}
			return happy(call)
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" {
		t.Fatalf("final = %+v, want restore_failed", f)
	}
	for _, want := range []string{"orders", "neo4j (online)", "system (online)"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
		}
	}
	if !asked {
		t.Error("the refusal did not ask what the engine does serve — the message cannot be acted on")
	}
}

func TestProvisionRefusesADatabaseThatIsNotOnline(t *testing.T) {
	fixture := writeFixture(t, "DZV1")
	var seen []string
	happy := happyHandler(t, fixture, "neo4j", &seen)
	line, _, _ := driveOp(t, "provision", provisionPayload(fixture, "{}"),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				switch name, _ := step(t, call); name {
				case "online":
					return outExec("0\n"), nil
				case "served":
					return outExec("neo4j, offline\nsystem, online\n"), nil
				}
			}
			return happy(call)
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "is offline rather than online") {
		t.Errorf("final = %+v, want the state the engine reported", f)
	}
}

// TestProvisionSaysSoWhenTheEngineWillNotListItsDatabases covers the
// third answer the gate can get: the engine is up, says the database is
// not online, and then will not say what it does have.
func TestProvisionSaysSoWhenTheEngineWillNotListItsDatabases(t *testing.T) {
	fixture := writeFixture(t, "DZV1")
	var seen []string
	happy := happyHandler(t, fixture, "neo4j", &seen)
	line, _, _ := driveOp(t, "provision", provisionPayload(fixture, "{}"),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				switch name, _ := step(t, call); name {
				case "online":
					return outExec("0\n"), nil
				case "served":
					return errExec(1, "Connection refused"), nil
				}
			}
			return happy(call)
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "would not say which databases") {
		t.Errorf("final = %+v, want the engine's refusal reported", f)
	}
}

// TestProvisionSurvivesASandboxThatDies covers the §3.3 path: a failed
// sandbox call is the core's error, and the adapter must pass it on
// rather than crash.
func TestProvisionSurvivesASandboxThatDies(t *testing.T) {
	fixture := writeFixture(t, "DZV1")
	line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"),
		func(verbCall) (any, *protoError) {
			return nil, protoErr("sandbox_error", true, "runtime died")
		})
	f := parseFinal(t, line)
	if exit != 0 || f.OK || f.Error.Code != "sandbox_error" {
		t.Errorf("exit=%d final=%+v, want the sandbox error passed through", exit, f)
	}
}

func TestHealthcheck(t *testing.T) {
	tests := map[string]struct {
		payload     string
		exec        any
		wantHealthy bool
		wantDB      string
	}{
		"serving":       {`{"connection":{"database":"neo4j"},"state":{}}`, outExec("1\n"), true, "neo4j"},
		"not serving":   {`{"connection":{"database":"neo4j"},"state":{}}`, errExec(1, "Connection refused"), false, "neo4j"},
		"wrong output":  {`{"connection":{"database":"neo4j"},"state":{}}`, outExec("something else\n"), false, "neo4j"},
		"no connection": {`{"connection":{},"state":{}}`, outExec("1\n"), true, defaultDatabase},
		"unusable name": {`{"connection":{"database":"../etc"},"state":{}}`, outExec("1\n"), true, defaultDatabase},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "healthcheck", tt.payload, func(call verbCall) (any, *protoError) {
				args := parseExec(t, call)
				wantArgs(t, "healthcheck", args, tt.wantDB)
				if args.Env["NEO4J_PASSWORD"] != sandboxPassword {
					t.Errorf("healthcheck env = %v, want the sandbox constant", args.Env)
				}
				return tt.exec, nil
			})
			f := parseFinal(t, line)
			if exit != 0 || !f.OK || len(calls) != 1 {
				t.Fatalf("exit=%d final=%+v calls=%d", exit, f, len(calls))
			}
			assertHealth(t, f.Payload, tt.wantHealthy)
		})
	}
	t.Run("malformed payload", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `"nope"`, nil)
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" {
			t.Errorf("final = %+v", f)
		}
	})
}

func assertHealth(t *testing.T, payload []byte, wantHealthy bool) {
	t.Helper()
	got := struct {
		Healthy bool    `json:"healthy"`
		Latency float64 `json:"latency_seconds"`
		Detail  string  `json:"detail"`
	}{}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.Healthy != wantHealthy {
		t.Errorf("healthy = %v, want %v (detail %q)", got.Healthy, wantHealthy, got.Detail)
	}
	if got.Latency < 0 {
		t.Errorf("latency = %v", got.Latency)
	}
}

// TestTeardown covers §6.4: everything this adapter creates lives inside
// the sandbox, so teardown releases nothing — including after a
// provision that never returned state.
func TestTeardown(t *testing.T) {
	for _, payload := range []string{
		`{"state":{},"reason":"failed"}`,
		`{"state":{"database":"neo4j","work_dir":"/scratch/probavi-neo4j"},"reason":"completed"}`,
	} {
		for range 2 { // idempotent: any number of invocations, same result
			line, calls, exit := driveOp(t, "teardown", payload, func(verbCall) (any, *protoError) {
				t.Error("teardown touched the sandbox")
				return outExec(""), nil
			})
			f := parseFinal(t, line)
			if exit != 0 || !f.OK || len(calls) != 0 {
				t.Fatalf("exit=%d final=%+v calls=%d", exit, f, len(calls))
			}
			if !strings.Contains(string(f.Payload), `"released":true`) {
				t.Errorf("payload = %s", f.Payload)
			}
		}
	}
}

func TestParseArchiveInfo(t *testing.T) {
	got := parseArchiveInfo([]byte(goodInfo))
	if got.database != "orders" || got.format != "Neo4j ZSTD Dump" {
		t.Errorf("archive info = %+v", got)
	}
	if empty := parseArchiveInfo([]byte("nothing here\n")); empty.database != "" || empty.format != "" {
		t.Errorf("archive info = %+v, want empty rather than a guess", empty)
	}
}

func TestParseDatabaseStatuses(t *testing.T) {
	got := parseDatabaseStatuses([]byte("neo4j, online\nsystem, online\n\ngarbage\n"))
	if len(got) != 2 || got["neo4j"] != "online" || got["system"] != "online" {
		t.Errorf("statuses = %v", got)
	}
}

func TestVerdictLine(t *testing.T) {
	tests := map[string]struct {
		stderr string
		want   string
	}{
		"progress before the verdict": {
			"Files: 1/37, data: 0.0%\nFiles: 2/37, data: 0.1%\n" +
				"Failed to load database 'neo4j': Not a valid Neo4j archive: /scratch/x.dump\n" +
				"Load failed for databases: 'neo4j'\nRun with '--verbose' for a more detailed error message.",
			"Failed to load database 'neo4j': Not a valid Neo4j archive: /scratch/x.dump"},
		"no verdict line at all": {
			"Load failed for databases: 'neo4j'\nRun with '--verbose' for a more detailed error message.",
			"Run with '--verbose' for a more detailed error message."},
		"progress only": {"Files: 37/37, data: 100.0%\nDone: 37 files, 258.0MiB processed in 0.2 seconds.", ""},
		"empty":         {"", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := verdictLine([]byte(tt.stderr)); got != tt.want {
				t.Errorf("verdictLine = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSanitizeRedactsTheSandboxConstant keeps a documented constant out
// of evidence anyway: publishing it in the adapter's source is a
// deliberate decision, echoing it into every operator's records is not.
func TestSanitizeRedactsTheSandboxConstant(t *testing.T) {
	got := sanitize(`auth failed for "neo4j" with password ` + sandboxPassword)
	if strings.Contains(got, sandboxPassword) || strings.Contains(got, `"`) {
		t.Errorf("sanitize = %q", got)
	}
}

func TestSortedNames(t *testing.T) {
	got := sortedNames(map[string]string{"system": "online", "neo4j": "online", "a": "offline"})
	if strings.Join(got, ",") != "a,neo4j,system" {
		t.Errorf("sortedNames = %v, want a stable order", got)
	}
}

// TestSIGTERMStopsSandboxCalls covers §2.4 at the level a unit test can:
// a cancelled context refuses to issue the next verb.
func TestCancelledContextRefusesFurtherCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &core{}
	if _, perr := c.call(ctx, "exec", execArgs{Argv: []string{"true"}}); perr == nil || perr.Code != "cancelled" {
		t.Errorf("call = %+v, want cancelled", perr)
	}
}
