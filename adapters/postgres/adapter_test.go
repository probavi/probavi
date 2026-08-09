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

// final unpacks a final response line.
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
	path := filepath.Join(t.TempDir(), "fixture.dump")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func provisionPayload(path, options string) string {
	return fmt.Sprintf(`{"source":{"kind":"pgdump","path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":%s}`, path, options)
}

// happyProvisionHandler simulates a sandbox where the engine needs one
// readiness retry and every verb succeeds, asserting argv shapes en route.
func happyProvisionHandler(t *testing.T, fixture string, isreadyCalls *int) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "exec":
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			return happyExecResponse(t, args, isreadyCalls), nil
		case "put_file":
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.SourcePath != fixture || args.DestPath != "/scratch/probavi-restore.dump" || args.Mode != "0600" {
				t.Errorf("put_file args = %+v", args)
			}
			return putFileValue{BytesCopied: 17, DurationSeconds: 0.2}, nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

func happyExecResponse(t *testing.T, args execArgs, isreadyCalls *int) any {
	switch args.Argv[0] {
	case "pg_isready":
		*isreadyCalls++
		if *isreadyCalls == 1 {
			return okExec(2) // not ready yet: adapter must poll
		}
		return okExec(0)
	case "pg_restore":
		want := []string{"pg_restore", "-h", "127.0.0.1", "-U", "orders_admin", "-d", "orders",
			"--no-owner", "--exit-on-error", "/scratch/probavi-restore.dump"}
		if strings.Join(args.Argv, " ") != strings.Join(want, " ") {
			t.Errorf("pg_restore argv = %v", args.Argv)
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
	sum := sha256.Sum256([]byte("FAKE-PGDUMP-BYTES"))
	if res.SourceIdentity.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %s — must be a real measurement of the fixture bytes", res.SourceIdentity.Checksum)
	}
	// The fixture is not a real archive and the drill declares no backup
	// zone, so there is no creation time to report — and none is invented.
	if res.SourceIdentity.SizeBytes != 17 || res.SourceIdentity.CreatedAt != nil {
		t.Errorf("source_identity = %+v", res.SourceIdentity)
	}
	if res.Connection.Database != "orders" || res.Connection.User != "orders_admin" || res.Connection.Scheme != "postgresql" {
		t.Errorf("connection = %+v — options must override the defaults", res.Connection)
	}
	if res.Timings.Transfer != 0.2 || res.Timings.Restore != 1.5 || res.Timings.EngineReady <= 0 {
		t.Errorf("timings = %+v — must carry the measured values", res.Timings)
	}
	if res.State["database"] != "orders" || res.State["dump_path"] != "/scratch/probavi-restore.dump" {
		t.Errorf("state = %+v", res.State)
	}
}

func TestProvisionHappyPath(t *testing.T) {
	fixture := writeFixture(t, "FAKE-PGDUMP-BYTES")
	isreadyCalls := 0
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(fixture, `{"user":"orders_admin","database":"orders"}`),
		happyProvisionHandler(t, fixture, &isreadyCalls))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if len(calls) != 4 { // isready fail, isready ok, put_file, pg_restore
		t.Errorf("calls = %d, want 4", len(calls))
	}
	assertProvisionResult(t, f.Payload)
}

// withGlobalsPayload is a provision request for the two-member kind: one
// source directory, both members named in params.
func withGlobalsPayload(dir string) string {
	return fmt.Sprintf(`{"source":{"kind":"pgdump_with_globals","path":%q,`+
		`"params":{"globals":"globals.sql","dump":"orders.dump"},`+
		`"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, dir)
}

// globalsFixtureBody is the globals script every unit-level fixture uses;
// its bytes only have to be stable, never valid SQL — no engine runs here.
const globalsFixtureBody = "CREATE ROLE app_ro;\n"

// writeGlobalsSet builds a source directory and returns it with the two
// member paths the adapter is expected to transfer.
func writeGlobalsSet(t *testing.T, dumpBody string) (dir, globals, dump string) {
	t.Helper()
	dir = t.TempDir()
	globals = filepath.Join(dir, "globals.sql")
	dump = filepath.Join(dir, "orders.dump")
	for path, body := range map[string]string{globals: globalsFixtureBody, dump: dumpBody} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir, globals, dump
}

// TestProvisionWithGlobals pins the order and the shape of the added step.
//
// The globals must be in before the dump — a restored GRANT names roles
// that have to exist already — and the psql invocation is load-bearing in
// two directions: ON_ERROR_STOP must stay off (the bootstrap-role
// collision sits in the middle of every globals script, and stopping there
// would silently skip the roles after it), while --echo-errors must stay
// absent (it echoes statements, and those carry password verifiers).
func TestProvisionWithGlobals(t *testing.T) {
	dir, globals, dump := writeGlobalsSet(t, "FAKE-PGDUMP-BYTES")

	var order []string
	line, _, exit := driveOp(t, "provision", withGlobalsPayload(dir),
		withGlobalsHandler(t, globals, dump, &order))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	want := []string{"pg_isready", "put:/scratch/probavi-globals.sql", "psql",
		"put:/scratch/probavi-restore.dump", "pg_restore"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v — globals load before the dump", order, want)
	}

	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// Both transfers and both loads are accounted for. The restore figure
	// is the drill's measured RTO, and a real recovery loads the globals.
	// Binary-exact fractions: the sums are asserted, not approximated.
	if res.Timings.Transfer != 0.25+0.5 {
		t.Errorf("transfer_seconds = %v, want both transfers", res.Timings.Transfer)
	}
	if res.Timings.Restore != 0.5+1.5 {
		t.Errorf("restore_seconds = %v, want the globals load counted into the restore", res.Timings.Restore)
	}
	if res.SourceIdentity.SizeBytes != int64(len("FAKE-PGDUMP-BYTES")+len(globalsFixtureBody)) {
		t.Errorf("size_bytes = %d, want both members", res.SourceIdentity.SizeBytes)
	}
	if res.State["globals_path"] != "/scratch/probavi-globals.sql" {
		t.Errorf("state = %+v, want the staged globals recorded", res.State)
	}
}

// withGlobalsHandler simulates a sandbox where every verb succeeds,
// recording the call order and asserting each call's shape.
func withGlobalsHandler(t *testing.T, globals, dump string, order *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "put_file":
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			*order = append(*order, "put:"+args.DestPath)
			return withGlobalsPutFile(t, args, globals, dump), nil
		case "exec":
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			*order = append(*order, args.Argv[0])
			return withGlobalsExec(t, args), nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

func withGlobalsPutFile(t *testing.T, args putFileArgs, globals, dump string) any {
	t.Helper()
	if args.Mode != "0600" {
		t.Errorf("put_file mode = %q, want 0600", args.Mode)
	}
	switch args.DestPath {
	case "/scratch/probavi-globals.sql":
		if args.SourcePath != globals {
			t.Errorf("globals put_file source = %s, want %s", args.SourcePath, globals)
		}
		return putFileValue{DurationSeconds: 0.25}
	case "/scratch/probavi-restore.dump":
		if args.SourcePath != dump {
			t.Errorf("dump put_file source = %s, want %s", args.SourcePath, dump)
		}
		return putFileValue{DurationSeconds: 0.5}
	}
	t.Fatalf("unexpected put_file dest %s", args.DestPath)
	return nil
}

func withGlobalsExec(t *testing.T, args execArgs) any {
	t.Helper()
	switch args.Argv[0] {
	case "pg_isready":
		return okExec(0)
	case "psql":
		assertGlobalsArgv(t, args.Argv)
		return execValue{ExitCode: 0, DurationSeconds: 0.5}
	case "pg_restore":
		return execValue{ExitCode: 0, DurationSeconds: 1.5}
	}
	t.Fatalf("unexpected exec %v", args.Argv)
	return nil
}

func assertGlobalsArgv(t *testing.T, argv []string) {
	t.Helper()
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-f /scratch/probavi-globals.sql") {
		t.Errorf("globals argv = %v, want psql -f over the staged script — pg_dumpall wraps its "+
			"output in \\restrict meta-commands that only a file-reading session executes", argv)
	}
	if !strings.Contains(joined, "ON_ERROR_STOP=0") {
		t.Errorf("globals argv = %v, want ON_ERROR_STOP explicitly off: the bootstrap-role "+
			"collision sits mid-script and stopping there skips the roles after it", argv)
	}
	if strings.Contains(joined, "--echo-errors") {
		t.Errorf("globals argv = %v, must not echo statements — they carry password verifiers", argv)
	}
}

// TestProvisionWithGlobalsFailures proves a bad globals load never reaches
// the dump: a partial cluster must not be restored into and reported as a
// pass.
func TestProvisionWithGlobalsFailures(t *testing.T) {
	dir, _, _ := writeGlobalsSet(t, "X")

	tests := []struct {
		name    string
		psql    any
		wantMsg string
	}{
		{"an unrelated error fails the drill",
			errExec(0, `psql:g.sql:31: ERROR:  permission denied to create role`),
			"permission denied"},
		{"psql itself failing fails the drill",
			errExec(2, `psql: error: could not open file: No such file or directory`),
			"could not open file"},
		{"a client failure with no classified error still fails",
			errExec(2, ""),
			"psql exited 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, calls, _ := driveOp(t, "provision", withGlobalsPayload(dir),
				globalsLoadHandler(t, tt.psql, false))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != "restore_failed" {
				t.Fatalf("final = %+v, want restore_failed", f)
			}
			if !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to name %q", f.Error.Message, tt.wantMsg)
			}
			// isready, put globals, psql — and nothing after.
			if len(calls) != 3 {
				t.Errorf("calls = %d, want 3: the dump must not be restored on top of a "+
					"half-loaded cluster", len(calls))
			}
		})
	}
}

// globalsLoadHandler answers the readiness poll and the globals load with
// psql. Unless allowRestore is set, anything the adapter attempts after a
// rejected globals load fails the test.
func globalsLoadHandler(t *testing.T, psql any, allowRestore bool) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("exec args: %v", err)
		}
		switch {
		case args.Argv[0] == "pg_isready":
			return okExec(0), nil
		case args.Argv[0] == "psql":
			return psql, nil
		case allowRestore:
			return execValue{ExitCode: 0, DurationSeconds: 1}, nil
		}
		t.Errorf("reached %v after a rejected globals load", args.Argv)
		return okExec(0), nil
	}
}

// TestProvisionWithGlobalsSandboxFailure keeps the outcome classification
// honest across the added step.
//
// A sandbox that dies while the globals are being staged says nothing
// about the backup. The evidence schema separates the two outcomes for
// exactly this reason — `fail` is a verdict on the backup, `error` is
// infrastructure — and reporting a lost container as `restore_failed`
// would put a false negative into an append-only log.
func TestProvisionWithGlobalsSandboxFailure(t *testing.T) {
	dir, _, _ := writeGlobalsSet(t, "X")
	for _, tt := range []struct {
		name string
		fail string // verb that fails
	}{
		{"while transferring the globals", "put_file"},
		{"while replaying the globals", "exec"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			line, _, _ := driveOp(t, "provision", withGlobalsPayload(dir), failingVerbHandler(t, tt.fail))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != "sandbox_error" {
				t.Errorf("final = %+v, want sandbox_error — a lost container is not a verdict "+
					"about the backup", f)
			}
		})
	}
}

// failingVerbHandler answers the readiness poll, then fails the named verb
// the way a dying sandbox would.
func failingVerbHandler(t *testing.T, fail string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "exec" {
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			if args.Argv[0] == "pg_isready" {
				return okExec(0), nil
			}
		}
		if call.Verb == fail {
			return nil, protoErr("sandbox_error", true, "container gone")
		}
		return putFileValue{}, nil
	}
}

// TestProvisionToleratesBootstrapRoleCollision covers the one diagnostic
// every real globals script produces: pg_dumpall emits CREATE ROLE for the
// bootstrap superuser, which initdb created before the drill started.
// Treating it as a failure would make the kind useless in every setup.
func TestProvisionToleratesBootstrapRoleCollision(t *testing.T) {
	dir, _, _ := writeGlobalsSet(t, "X")
	stderr := `psql:/scratch/probavi-globals.sql:20: ERROR:  role "postgres" already exists`

	line, calls, _ := driveOp(t, "provision", withGlobalsPayload(dir),
		globalsLoadHandler(t, errExec(0, stderr), true))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v — every globals script collides with the bootstrap superuser "+
			"initdb already created", f)
	}
	if len(calls) != 5 {
		t.Errorf("calls = %d, want the restore to proceed", len(calls))
	}
}

// TestProvisionWithGlobalsRefusesPITR keeps the logical kinds honest: a
// dump is a single frozen snapshot, whatever the globals add.
func TestProvisionWithGlobalsRefusesPITR(t *testing.T) {
	dir, _, _ := writeGlobalsSet(t, "X")
	payload := fmt.Sprintf(`{"source":{"kind":"pgdump_with_globals","path":%q,`+
		`"params":{"globals":"globals.sql"}},`+
		`"sandbox":{},"options":{},"pitr":{"target_time":"2026-07-30T14:32:00Z"}}`, dir)
	assertProvisionFailure(t, payload,
		func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
		"invalid_request", 0)
}

func TestProvisionFailures(t *testing.T) {
	fixture := writeFixture(t, "X")
	readyThen := func(rest func(call verbCall) (any, *protoError)) func(verbCall) (any, *protoError) {
		return func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				args := execArgs{}
				_ = json.Unmarshal(call.Args, &args) //nolint:errcheck // args validated by the flows under test
				if args.Argv[0] == "pg_isready" {
					return okExec(0), nil
				}
			}
			return rest(call)
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
			provisionPayload(filepath.Join(t.TempDir(), "nope.dump"), "{}"),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"source_not_found", 0},
		{"pitr not supported",
			`{"source":{"kind":"pgdump","path":"/x"},"sandbox":{},"options":{},"pitr":{"target_time":"t"}}`,
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") },
			"invalid_request", 0},
		{"corrupt archive",
			provisionPayload(fixture, "{}"),
			readyThen(func(call verbCall) (any, *protoError) {
				if call.Verb == "put_file" {
					return putFileValue{}, nil
				}
				return errExec(1, `pg_restore: error: input file does not appear to be a valid archive`), nil
			}),
			"source_corrupt", 4},
		{"engine failure during restore",
			provisionPayload(fixture, "{}"),
			readyThen(func(call verbCall) (any, *protoError) {
				if call.Verb == "put_file" {
					return putFileValue{}, nil
				}
				return errExec(1, `pg_restore: error: could not execute query: ERROR: out of memory`), nil
			}),
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
	payload := `{"connection":{"database":"orders","user":"u"},"state":{}}`

	t.Run("healthy", func(t *testing.T) {
		line, calls, _ := driveOp(t, "healthcheck", payload, func(call verbCall) (any, *protoError) {
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("args: %v", err)
			}
			if args.Argv[0] != "psql" || !strings.Contains(strings.Join(args.Argv, " "), "-d orders") {
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
			return errExec(2, "connection refused"), nil
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
