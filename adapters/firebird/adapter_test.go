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
// shell one-liners are told apart by their argument counts: the preflight
// carries none, the serving query one path, the restore two.
func classifyExec(argv []string) (string, any) {
	switch {
	case argv[0] == "sh" && len(argv) == 3:
		return "preflight", execValue{ExitCode: 0, DurationSeconds: 0.05}
	case argv[0] == "mkdir":
		return "mkdir", okExec()
	case argv[0] == "sh" && len(argv) == 6:
		return "restore", execValue{ExitCode: 0, DurationSeconds: 0.3}
	case argv[0] == "sh" && len(argv) == 5:
		return "serving", execValue{ExitCode: 0, DurationSeconds: 0.02,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("           1 \n"))}
	}
	return "", nil
}

func provisionHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 128, DurationSeconds: 0.4}, nil
		}
		label, value := classifyExec(argvOf(t, call))
		if label == "" {
			t.Fatalf("unexpected exec: %v", argvOf(t, call))
		}
		*sequence = append(*sequence, label)
		return value, nil
	}
}

func fixtureBackup(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(headerFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "backup.fbk")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
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
		if err := os.WriteFile(golden, append(line, '\n'), 0o644); err != nil { //#nosec G306 -- test golden.
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -args -update once): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), line) {
		t.Errorf("probe response deviates from golden:\n got: %s\nwant: %s", line, bytes.TrimSpace(want))
	}
}

// TestTheCheckRunnerSuspendsDatabaseTriggers is the load-bearing test of
// this adapter.
//
// An ON CONNECT database trigger travels inside a gbak backup and is
// restored with it. Measured: a clean restore holds every row, and the
// first ordinary connection fires the trigger and deletes what it deletes,
// irreversibly. Every connection the drill makes therefore carries
// -nodbtriggers — the check runner above all, because it is the one the
// core opens, and by the time a row_count check has run, a dropped flag
// has already destroyed the thing it was counting.
func TestTheCheckRunnerSuspendsDatabaseTriggers(t *testing.T) {
	if !strings.Contains(checkScript, "-nodbtriggers") {
		t.Error("the check runner does not suspend database triggers")
	}
	if !strings.Contains(checkScript, "SET HEADING OFF") {
		t.Error("the check runner does not turn off column titles, so every value arrives decorated")
	}
	payload, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := payload["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("probe declares no sql_runner")
	}
	argv, ok := runner["argv"].([]string)
	if !ok {
		t.Fatal("sql_runner declares no argv")
	}
	var hasSQL bool
	for _, a := range argv {
		if a == "{{sql}}" {
			hasSQL = true
		}
	}
	if !hasSQL {
		t.Error("{{sql}} is not its own argv element (§10 check 2)")
	}
}

// TestEveryConnectionSuspendsTriggers: the runner is not the only place
// this adapter opens the restored database. A serving probe or a
// healthcheck without the flag would fire the trigger before any check
// ran, and the drill would then measure a database the drill itself
// emptied.
func TestEveryConnectionSuspendsTriggers(t *testing.T) {
	backup := fixtureBackup(t)
	var argvs [][]string
	_, _, _ = driveOp(t, "provision", provisionPayload(t, "firebird_gbak", backup, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			argvs = append(argvs, argv)
			_, value := classifyExec(argv)
			return value, nil
		})
	// A call opens the database when it names the restored file, not when
	// it merely mentions isql — the preflight asks whether the binary
	// exists and connects to nothing.
	var opened int
	for _, argv := range argvs {
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, dbFileName) || !strings.Contains(joined, "isql") {
			continue
		}
		opened++
		if !strings.Contains(joined, "-nodbtriggers") {
			t.Errorf("this call opens the database without suspending triggers: %v", argv)
		}
	}
	if opened == 0 {
		t.Error("provision never opened the restored database, so nothing proved it serves")
	}
}

func TestProvisionHappyPath(t *testing.T) {
	backup := fixtureBackup(t)
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "firebird_gbak", backup, map[string]string{"backup_timezone": "UTC"}),
		provisionHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	want := []string{"preflight", "mkdir", "put_file", "restore", "serving"}
	if strings.Join(sequence, ",") != strings.Join(want, ",") {
		t.Errorf("call sequence = %v, want %v", sequence, want)
	}
	payload := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Database string `json:"database"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
	}{}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Connection.Scheme != "firebird" {
		t.Errorf("scheme = %q", payload.Connection.Scheme)
	}
	if !strings.HasSuffix(payload.Connection.Database, dbFileName) {
		t.Errorf("database = %q, want the restored file", payload.Connection.Database)
	}
	if payload.SourceIdentity.CreatedAt == nil {
		t.Error("created_at is nil although the header carries a clock and a zone was declared")
	} else if *payload.SourceIdentity.CreatedAt != "2026-08-28T17:59:41.000Z" {
		t.Errorf("created_at = %q", *payload.SourceIdentity.CreatedAt)
	}
	for _, key := range []string{"engine_ready_seconds", "transfer_seconds", "restore_seconds"} {
		if payload.Timings[key] <= 0 {
			t.Errorf("timing %s = %v, want a real measurement", key, payload.Timings[key])
		}
	}
}

// TestCreatedAtIsNilWithoutADeclaredZone at the protocol boundary: the
// same backup, no backup_timezone, and the record carries no creation time
// rather than a wrong one.
func TestCreatedAtIsNilWithoutADeclaredZone(t *testing.T) {
	backup := fixtureBackup(t)
	var sequence []string
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "firebird_gbak", backup, nil),
		provisionHandler(t, &sequence))
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	payload := struct {
		SourceIdentity struct {
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
	}{}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %q, want nil without a declared zone", *payload.SourceIdentity.CreatedAt)
	}
}

func TestProvisionRefusals(t *testing.T) {
	backup := fixtureBackup(t)
	tests := []struct {
		name     string
		payload  string
		wantCode string
	}{
		{"a malformed payload", `"not an object"`, "invalid_request"},
		{"a kind nobody declared", provisionPayload(t, "firebird_dump", backup, nil), "unsupported_source"},
		{"a path that is not there", provisionPayload(t, "firebird_gbak", backup+".gone", nil), "source_not_found"},
		{"an unknown time zone",
			provisionPayload(t, "firebird_gbak", backup, map[string]string{"backup_timezone": "Mars/Olympus"}),
			"invalid_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "provision", tc.payload, func(verbCall) (any, *protoError) {
				t.Fatal("a refusal must be decided before the sandbox is touched")
				return nil, nil
			})
			if exit != 0 {
				t.Fatalf("exit = %d, want a clean final response", exit)
			}
			if len(calls) != 0 {
				t.Errorf("issued %d sandbox calls before refusing", len(calls))
			}
			final := parseFinal(t, line)
			if final.OK {
				t.Fatal("provision succeeded, want a refusal")
			}
			if final.Error.Code != tc.wantCode {
				t.Errorf("code = %s (%s), want %s", final.Error.Code, final.Error.Message, tc.wantCode)
			}
		})
	}
}

// TestProvisionRefusesPITR: a gbak backup is a snapshot of one instant,
// and the probe declares pitr false for both kinds — a request for it is
// the core's mistake, and saying so beats restoring the snapshot and
// letting the record imply a target it never hit.
func TestProvisionRefusesPITR(t *testing.T) {
	backup := fixtureBackup(t)
	payload := fmt.Sprintf(
		`{"source":{"kind":"firebird_gbak","path":%q},"sandbox":{"scratch_dir":"/scratch"},`+
			`"pitr":{"target_time":"2026-08-28T00:00:00Z"}}`, backup)
	line, _, _ := driveOp(t, "provision", payload, func(verbCall) (any, *protoError) {
		t.Fatal("pitr must be refused before the sandbox is touched")
		return nil, nil
	})
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request", final)
	}
}

// TestRestoreFailureIsClassifiedFromGbaksOwnWords, table-driven over the
// three damage forms measured on Firebird 5.0.4 plus the cross-version
// refusal. The "do not recognize" line deliberately does not decide: it
// appears both when the artifact is newer than the sandbox and when its
// bytes are simply damaged, so the message names both.
func TestRestoreFailureIsClassifiedFromGbaksOwnWords(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		wantCode string
		wantIn   string
	}{
		{
			name:     "truncated part-way",
			output:   "Done with volume #0\n\tPress return to reopen that file, or type a new\n\nERROR: Backup incomplete",
			wantCode: "source_corrupt",
			wantIn:   "Backup incomplete",
		},
		{
			name:     "bytes overwritten mid-file",
			output:   "gbak:do not recognize privilege attribute 65 -- continuing\ngbak: ERROR:string truncated",
			wantCode: "source_corrupt",
			wantIn:   "either the artifact is damaged, or it was written by a newer Firebird",
		},
		{
			name:     "not a backup at all",
			output:   "gbak: ERROR:expected backup description record\ngbak:Exiting before completion due to errors",
			wantCode: "source_corrupt",
			wantIn:   "expected backup description record",
		},
		{
			name:     "something else entirely",
			output:   "gbak: ERROR:cannot open the output file",
			wantCode: "restore_failed",
			wantIn:   "cannot open the output file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			perr := mapRestoreFailure([]byte(tc.output))
			if perr.Code != tc.wantCode {
				t.Errorf("code = %s, want %s", perr.Code, tc.wantCode)
			}
			if !strings.Contains(perr.Message, tc.wantIn) {
				t.Errorf("message %q does not carry %q", perr.Message, tc.wantIn)
			}
		})
	}
}

// TestErrorLineSkipsTheInteractivePrompt: a truncated artifact makes gbak
// ask the operator a question first, so the first line of its output is
// not the diagnosis. Reporting the prompt instead of the error would tell
// an operator nothing about what went wrong.
func TestErrorLineSkipsTheInteractivePrompt(t *testing.T) {
	out := "Done with volume #0, \"a.fbk\"\tPress return to reopen that file, or type a new\n" +
		"\tname followed by return to open a different file.  Name: \n\nERROR: Backup incomplete"
	if got := errorLine([]byte(out)); !strings.Contains(got, "Backup incomplete") {
		t.Errorf("errorLine = %q, want the ERROR line", got)
	}
}

// TestRestoreScriptCannotHangOrLeaveADatabaseBehind pins the three things
// the restore script exists to guarantee, each measured: gbak writes to
// stdout and would otherwise be silent to the operator; it prompts on a
// damaged volume and would wait forever for an answer; and it leaves a
// queryable database behind when it fails, which anything downstream would
// read as a successful restore.
func TestRestoreScriptCannotHangOrLeaveADatabaseBehind(t *testing.T) {
	for _, want := range []string{">&2", "</dev/null", "rm -f"} {
		if !strings.Contains(restoreScript, want) {
			t.Errorf("the restore script is missing %q", want)
		}
	}
}

// TestProvisionRefusesASilentRestoreFailure: gbak exiting 0 is not proof
// that a database serves. If the serving probe does not come back with the
// answer it asked for, the drill fails here rather than letting every
// later check run against nothing.
func TestProvisionRefusesASilentRestoreFailure(t *testing.T) {
	backup := fixtureBackup(t)
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "firebird_gbak", backup, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{BytesCopied: 128, DurationSeconds: 0.1}, nil
			}
			argv := argvOf(t, call)
			if label, _ := classifyExec(argv); label == "serving" {
				return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(""))}, nil
			}
			_, value := classifyExec(argv)
			return value, nil
		})
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("provision reported success although the restored database answered nothing")
	}
	if final.Error.Code != "restore_failed" {
		t.Errorf("code = %s, want restore_failed", final.Error.Code)
	}
}

func TestHealthcheckShape(t *testing.T) {
	line, _, exit := driveOp(t, "healthcheck",
		`{"connection":{"database":"/scratch/probavi-firebird/restored.fdb"},"state":{}}`,
		func(call verbCall) (any, *protoError) {
			return execValue{ExitCode: 0, DurationSeconds: 0.02,
				StdoutB64: base64.StdEncoding.EncodeToString([]byte("          57 \n"))}, nil
		})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("healthcheck failed: %+v", final.Error)
	}
	payload := struct {
		Healthy bool    `json:"healthy"`
		Latency float64 `json:"latency_seconds"`
		Detail  string  `json:"detail"`
	}{}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !payload.Healthy || payload.Latency < 0 {
		t.Errorf("payload = %+v", payload)
	}
	if !strings.Contains(payload.Detail, "57") {
		t.Errorf("detail = %q, want the relation count", payload.Detail)
	}
}

func TestTeardownIsIdempotentAndNeedsNoState(t *testing.T) {
	for _, payload := range []string{`{"state":{},"reason":"failed"}`, `{"state":{},"reason":"completed"}`} {
		line, calls, exit := driveOp(t, "teardown", payload, func(verbCall) (any, *protoError) {
			t.Fatal("teardown has nothing outside the sandbox to release")
			return nil, nil
		})
		if exit != 0 || len(calls) != 0 {
			t.Fatalf("exit=%d calls=%d", exit, len(calls))
		}
		if final := parseFinal(t, line); !final.OK {
			t.Fatalf("teardown failed: %+v", final.Error)
		}
	}
}

func TestUnknownOpIsRefused(t *testing.T) {
	line, _, exit := driveOp(t, "restore", "{}", func(verbCall) (any, *protoError) {
		t.Fatal("an unknown op must not touch the sandbox")
		return nil, nil
	})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request", final)
	}
}

func TestUnsupportedProtocolListsWhatIsSpoken(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	in := strings.NewReader(`{"protocol":"probavi-adapter/999","request_id":"r-test","op":"probe"}` + "\n")
	if exit := run(in, stdout, stderr); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	final := parseFinal(t, bytes.TrimSpace(stdout.Bytes()))
	if final.OK || final.Error.Code != "unsupported_protocol" {
		t.Fatalf("final = %+v", final)
	}
	if final.Error.Detail["supported"] == nil {
		t.Error("detail.supported is missing (§3.1)")
	}
}
