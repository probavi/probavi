package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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
// calls are told apart by shape: the preflight is a shell one-liner with
// one argument, every query is the check script with three, and the
// archive is unpacked by H2's own tool.
func classifyExec(argv []string) (string, any) {
	switch {
	case argv[0] == "sh" && len(argv) == 5:
		return "engine", execValue{ExitCode: 0, DurationSeconds: 0.05}
	case argv[0] == "mkdir":
		return "mkdir", okExec()
	case argv[0] == "java":
		return "unpack", execValue{ExitCode: 0, DurationSeconds: 0.3}
	case argv[0] == "sh" && len(argv) == 7:
		return "query", execValue{ExitCode: 0, DurationSeconds: 0.1,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("1"))}
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

// TestRunnerTemplateShape pins the runner contract of §6.1: the template
// carries every placeholder the core substitutes, and the password never
// appears in argv — a secret in an argument list is visible in the
// sandbox's process table.
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
	for _, want := range []string{"{{database}}", "{{user}}", "{{sql}}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("runner argv lacks %s: %v", want, argv)
		}
	}
	if strings.Contains(joined, "{{password}}") {
		t.Error("runner argv carries {{password}}; a secret belongs in env values only (§6.1)")
	}
	env, ok := runner["env"].(map[string]string)
	if !ok {
		t.Fatal("sql_runner declares no env")
	}
	if env[runnerEnvKey] != "{{password}}" {
		t.Errorf("runner env = %v, want %s to carry {{password}}", env, runnerEnvKey)
	}
}

// fakeJava puts an executable named java on PATH that replays a recorded
// H2 Shell run: the given stdout, then the given exit code. The check
// script is a contract with the tool's actual behaviour, so the tests
// below drive the real script against real recorded output.
func fakeJava(t *testing.T, stdout string, exit int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'H2EOF'\n%s\nH2EOF\nexit %d\n", stdout, exit)
	if err := os.WriteFile(filepath.Join(dir, "java"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake java: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// runCheckScript runs the sql_runner's body exactly as the sandbox would.
func runCheckScript(t *testing.T, sql string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", checkScript, "sh", "/w/restored", "sa", sql)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run check script: %v", err)
	}
	return out.String(), errBuf.String(), code
}

// TestTheRunnerTurnsH2sOwnWordsIntoAnExitCode is the reason the runner is
// a script rather than the tool. H2's Shell prints "Error: ..." on stdout
// and exits 0 — measured, and mid-stream, after whatever the statements
// before it printed — so a runner built on the bare tool would report
// every failing check as passing. Each case below is a recorded run of
// the real tool.
func TestTheRunnerTurnsH2sOwnWordsIntoAnExitCode(t *testing.T) {
	tests := []struct {
		name       string
		toolStdout string
		toolExit   int
		wantStdout string
		wantExit   int
	}{
		{
			name:       "a scalar answer arrives undecorated",
			toolStdout: "COUNT(*)\n1000\n(1 row, 10 ms)",
			wantStdout: "1000\n",
		},
		{
			name:       "no rows is no output, not an error",
			toolStdout: "ID\n(0 rows, 9 ms)",
			wantStdout: "",
		},
		{
			name: "a SQL error the tool reports with exit 0 still fails the check",
			toolStdout: "Error: org.h2.jdbc.JdbcSQLSyntaxErrorException: Table \"NOPE\" not found; " +
				"SQL statement:\n SELECT * FROM nope [42102-240]",
			wantExit: 1,
		},
		{
			name: "an error after successful output fails too",
			toolStdout: "COUNT(*)\n1000\n(1 row, 8 ms)\nError: org.h2.jdbc.JdbcSQLSyntaxErrorException: " +
				"Table \"NOPE\" not found [42102-240]",
			wantExit: 1,
		},
		{
			name:       "a connection the engine refuses keeps its own exit code",
			toolStdout: "Exception in thread \"main\" org.h2.jdbc.JdbcSQLNonTransientConnectionException",
			toolExit:   1,
			wantExit:   1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeJava(t, tc.toolStdout, tc.toolExit)
			stdout, stderr, exit := runCheckScript(t, "SELECT COUNT(*) FROM orders")
			if exit != tc.wantExit {
				t.Fatalf("exit = %d, want %d (stderr %q)", exit, tc.wantExit, stderr)
			}
			if tc.wantExit == 0 && stdout != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tc.wantStdout)
			}
			if tc.wantExit != 0 {
				if stdout != "" {
					t.Errorf("a failed check printed %q on stdout; the diagnostic belongs on stderr", stdout)
				}
				if !strings.Contains(stderr, "org.h2") {
					t.Errorf("stderr = %q, want the tool's own diagnostic", stderr)
				}
			}
		})
	}
}

// TestEveryConnectionRefusesToInventADatabase pins the flag that stands
// between a failed restore and a green drill: pointed at a path holding
// no database, H2 creates one and answers queries against it (measured),
// so a drill whose restore produced nothing would check a fresh empty
// database. Every URL this adapter builds carries IFEXISTS=TRUE.
func TestEveryConnectionRefusesToInventADatabase(t *testing.T) {
	if !strings.Contains(checkScript, urlSuffix) {
		t.Errorf("the check script builds a URL without %s", urlSuffix)
	}
	for _, url := range regexp.MustCompile(`jdbc:h2:[^"]*`).FindAllString(checkScript, -1) {
		if !strings.Contains(url, urlSuffix) {
			t.Errorf("URL %q does not carry %s", url, urlSuffix)
		}
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

func TestProvisionPlacesDatabase(t *testing.T) {
	dir := t.TempDir()
	path := writeArtifact(t, dir, "prod.mv.db", dbFixture())

	var sequence []string
	line, calls, exit := driveOp(t, "provision", provisionPayload(t, "h2_db", path, nil),
		provisionHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit %d: %s", exit, line)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %s", line)
	}
	want := []string{"engine", "mkdir", "put_file", "query"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("call sequence = %v, want %v", sequence, want)
	}
	assertPutDest(t, calls, "/scratch/probavi-h2/restored.mv.db")

	res := struct {
		Connection struct {
			Scheme      string `json:"scheme"`
			Database    string `json:"database"`
			User        string `json:"user"`
			PasswordEnv string `json:"password_env"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
	}{}
	if err := json.Unmarshal(final.Payload, &res); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if res.Connection.Database != "/scratch/probavi-h2/restored" {
		t.Errorf("database = %s, want the base path H2 addresses (no extension)", res.Connection.Database)
	}
	if res.Connection.User != defaultUser || res.Connection.Scheme != "h2" {
		t.Errorf("connection = %+v, want scheme h2 and user %s", res.Connection, defaultUser)
	}
	if res.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null: no H2 artifact dates the backup", *res.SourceIdentity.CreatedAt)
	}
}

func TestProvisionUnpacksArchive(t *testing.T) {
	dir := t.TempDir()
	path := writeArtifact(t, dir, "prod.zip", archiveFixture(t, "prod.mv.db"))

	var sequence []string
	line, calls, exit := driveOp(t, "provision", provisionPayload(t, "h2_backup", path, nil),
		provisionHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit %d: %s", exit, line)
	}
	want := []string{"engine", "mkdir", "put_file", "unpack", "query"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("call sequence = %v, want %v", sequence, want)
	}
	assertPutDest(t, calls, "/scratch/probavi-h2/backup.zip")
	// The archive is unpacked by H2's own tool, so the sandbox image needs
	// no unzip utility beyond the jar the wrapper already carries.
	for _, c := range calls {
		if c.Verb != "exec" {
			continue
		}
		argv := argvOf(t, c)
		if argv[0] == "unzip" {
			t.Error("the adapter reached for an unzip utility; H2 unpacks its own archives")
		}
	}
}

func TestProvisionRefusals(t *testing.T) {
	dir := t.TempDir()
	good := writeArtifact(t, dir, "prod.mv.db", dbFixture())

	tests := []struct {
		name     string
		payload  string
		handler  func(verbCall) (any, *protoError)
		wantCode string
		wantText string
	}{
		{
			name:     "pitr is not offered",
			payload:  `{"source":{"kind":"h2_db","path":"` + good + `"},"pitr":{"target_time":"2026-01-01T00:00:00Z"}}`,
			wantCode: "invalid_request", wantText: "does not support pitr",
		},
		{
			name:     "backup_timezone has nothing to apply to",
			payload:  provisionPayload(t, "h2_db", good, map[string]string{"backup_timezone": "UTC"}),
			wantCode: "invalid_request", wantText: "has no effect",
		},
		{
			name:     "a sandbox without the jar",
			payload:  provisionPayload(t, "h2_db", good, nil),
			handler:  failingPreflight,
			wantCode: "invalid_request", wantText: "there is no official H2 image",
		},
		{
			name:     "a restored database H2 refuses",
			payload:  provisionPayload(t, "h2_db", good, nil),
			handler:  refusingOpen(t),
			wantCode: "source_corrupt", wantText: "File corrupted",
		},
		{
			name: "a password_env the drill never declared",
			payload: `{"source":{"kind":"h2_db","path":"` + good + `","credential_env":["OTHER"]},` +
				`"sandbox":{"scratch_dir":"/scratch"},"options":{"password_env":"H2_PASSWORD"}}`,
			wantCode: "invalid_request", wantText: "source.credential_env does not list it",
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

// failingPreflight simulates an image without the H2 jar.
func failingPreflight(verbCall) (any, *protoError) {
	return errExec(1, "no H2 jar at /opt/h2/h2.jar"), nil
}

// refusingOpen simulates a sandbox where everything works until H2 is
// asked to open the restored database, which is where a corrupt artifact
// is caught (measured: "File corrupted while reading record").
func refusingOpen(t *testing.T) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		if len(argvOf(t, call)) == 7 {
			return errExec(1, "File corrupted while reading record: '/scratch/probavi-h2/restored.mv.db'"), nil
		}
		return okExec(), nil
	}
}

// TestAWellFormedZeroIsRefused pins the verdict a measurement forced.
// MVStore opens a truncated database as the older one its remaining bytes
// describe rather than refusing it, and at every truncation measured that
// older database had no tables while the engine reported success — so the
// restore's verdict is the table count, and zero is a refusal.
func TestAWellFormedZeroIsRefused(t *testing.T) {
	dir := t.TempDir()
	good := writeArtifact(t, dir, "prod.mv.db", dbFixture())

	line, _, exit := driveOp(t, "provision", provisionPayload(t, "h2_db", good, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			if len(argvOf(t, call)) == 7 {
				// The engine opened the file and answered, cleanly, that
				// it holds nothing.
				return execValue{ExitCode: 0, DurationSeconds: 0.1,
					StdoutB64: base64.StdEncoding.EncodeToString([]byte("0"))}, nil
			}
			return okExec(), nil
		})
	if exit != 0 {
		t.Fatalf("adapter exited %d; a refusal is a final response", exit)
	}
	final := parseFinal(t, line)
	if final.OK || final.Error == nil {
		t.Fatalf("provision accepted a restore that produced no tables: %s", line)
	}
	if final.Error.Code != "source_corrupt" || !strings.Contains(final.Error.Message, "holds no tables") {
		t.Errorf("error = %s/%q, want source_corrupt naming the empty restore",
			final.Error.Code, final.Error.Message)
	}
}

// TestPasswordEnvTravelsAsAName pins §2.5: the adapter passes the
// variable's name, never its value, and the core resolves it.
func TestPasswordEnvTravelsAsAName(t *testing.T) {
	dir := t.TempDir()
	good := writeArtifact(t, dir, "prod.mv.db", dbFixture())
	payload := `{"source":{"kind":"h2_db","path":"` + good + `","credential_env":["H2_PASSWORD"]},` +
		`"sandbox":{"scratch_dir":"/scratch"},"options":{"password_env":"H2_PASSWORD","user":"drill"}}`

	var sequence []string
	line, _, _ := driveOp(t, "provision", payload, provisionHandler(t, &sequence))
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %s", line)
	}
	if strings.Contains(string(final.Payload), "secret") {
		t.Error("the provision payload carries something that looks like a secret value")
	}
	res := struct {
		Connection struct {
			User        string `json:"user"`
			PasswordEnv string `json:"password_env"`
		} `json:"connection"`
	}{}
	if err := json.Unmarshal(final.Payload, &res); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if res.Connection.PasswordEnv != "H2_PASSWORD" || res.Connection.User != "drill" {
		t.Errorf("connection = %+v, want the declared user and password_env", res.Connection)
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
	payload := `{"connection":{"database":"/scratch/probavi-h2/restored","user":"sa"}}`
	line, calls, exit := driveOp(t, "healthcheck", payload, func(call verbCall) (any, *protoError) {
		argv := argvOf(t, call)
		if argv[0] != "sh" || len(argv) != 7 {
			t.Fatalf("healthcheck ran %v, want the check script", argv)
		}
		if !strings.Contains(argv[6], "INFORMATION_SCHEMA.TABLES") {
			t.Errorf("healthcheck query = %q, want one that reads the schema", argv[6])
		}
		return execValue{ExitCode: 0, DurationSeconds: 0.02,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("7"))}, nil
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
	if !res.Healthy || !strings.Contains(res.Detail, "7 tables") {
		t.Errorf("healthcheck = %+v, want healthy with the table count", res)
	}
}

// TestHealthcheckReportsUnhealthyRatherThanFailing pins §6.3: a database
// that no longer answers is a result, not an operation error.
func TestHealthcheckReportsUnhealthyRatherThanFailing(t *testing.T) {
	payload := `{"connection":{"database":"/scratch/probavi-h2/restored","user":"sa"}}`
	line, _, exit := driveOp(t, "healthcheck", payload, func(verbCall) (any, *protoError) {
		return errExec(1, "File corrupted while reading record"), nil
	})
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
	if res.Healthy || !strings.Contains(res.Detail, "File corrupted") {
		t.Errorf("healthcheck = %+v, want unhealthy naming the engine's own words", res)
	}
}
