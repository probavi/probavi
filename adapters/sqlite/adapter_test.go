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

func outExec(stdout string) any {
	return execValue{StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

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

// engineVersionOut is what sqlite3 -version prints (measured on 3.53.4).
const engineVersionOut = "3.53.4 2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc (64-bit)"

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

// classifyExec labels one exec call of the happy path and returns its
// simulated result; an empty label means the call was not expected. The
// shell one-liners are told apart by their argument counts: the preflight
// carries none, the integrity script one path, the replay two.
func classifyExec(argv []string) (string, any) {
	switch {
	case argv[0] == "sh" && len(argv) == 3:
		return "engine", execValue{ExitCode: 0, DurationSeconds: 0.05,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte(engineVersionOut))}
	case argv[0] == "mkdir":
		return "mkdir", okExec()
	case argv[0] == "sh" && len(argv) == 5:
		return "integrity", execValue{ExitCode: 0, DurationSeconds: 0.1}
	case argv[0] == "sh" && len(argv) == 6:
		return "replay", execValue{ExitCode: 0, DurationSeconds: 0.3}
	}
	return "", nil
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

// TestRunnerTemplateShape pins the property the check path rests on:
// there is no shell in the runner at all — the SQL and the database path
// each travel as one argv element, so nothing an operator writes in a
// check can become anything but arguments to sqlite3.
func TestRunnerTemplateShape(t *testing.T) {
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("sql_runner is not an object")
	}
	want := []string{"sqlite3", "-batch", "-noheader", "-bail", "-separator", "\t", "{{database}}", "{{sql}}"}
	if got, ok := runner["argv"].([]string); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("sql_runner argv = %v, want %v", runner["argv"], want)
	}
}

// assertPutDest asserts every put_file call targeted the given sandbox
// path.
func assertPutDest(t *testing.T, calls []verbCall, want string) {
	t.Helper()
	for _, call := range calls {
		if call.Verb != "put_file" {
			continue
		}
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
		if args.DestPath != want {
			t.Errorf("put_file dest = %q, want %q", args.DestPath, want)
		}
	}
}

func TestProvisionPlacesDatabase(t *testing.T) {
	db := writeArtifact(t, t.TempDir(), "nightly.db", dbFixture())
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "sqlite_db", db, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if want := "engine|mkdir|put_file|integrity"; strings.Join(sequence, "|") != want {
		t.Errorf("sequence = %s, want %s", strings.Join(sequence, "|"), want)
	}
	assertPutDest(t, calls, "/scratch/probavi-sqlite/restored.db")

	got := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Port     int    `json:"port"`
			Database string `json:"database"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.Connection.Scheme != "sqlite" || got.Connection.Port != 0 ||
		got.Connection.Database != "/scratch/probavi-sqlite/restored.db" {
		t.Errorf("connection = %+v", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	if got.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null — nothing in the artifact dates it", *got.SourceIdentity.CreatedAt)
	}
	if got.Timings["transfer_seconds"] != 0.4 || got.Timings["restore_seconds"] != 0.1 ||
		got.Timings["engine_ready_seconds"] != 0.05 {
		t.Errorf("timings = %+v, want the simulator's measured phases", got.Timings)
	}
}

func TestProvisionReplaysDump(t *testing.T) {
	dump := writeArtifact(t, t.TempDir(), "nightly.sql", dumpFixture())
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "sqlite_dump", dump, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if want := "engine|mkdir|put_file|replay"; strings.Join(sequence, "|") != want {
		t.Errorf("sequence = %s, want %s", strings.Join(sequence, "|"), want)
	}
	assertPutDest(t, calls, "/scratch/probavi-sqlite/dump.sql")
	got := struct {
		Connection struct {
			Database string `json:"database"`
		} `json:"connection"`
		Timings map[string]float64 `json:"timings"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.Connection.Database != "/scratch/probavi-sqlite/restored.db" {
		t.Errorf("connection.database = %q — checks must open the built database, not the dump", got.Connection.Database)
	}
	if got.Timings["restore_seconds"] != 0.3 {
		t.Errorf("timings = %+v, want the replay as the measured restore", got.Timings)
	}
}

func TestProvisionRefusals(t *testing.T) {
	dir := t.TempDir()
	db := writeArtifact(t, dir, "nightly.db", dbFixture())
	gz := writeArtifact(t, dir, "nightly.db.gz", []byte{0x1f, 0x8b, 0x08, 0x00})
	dumpAsDB := writeArtifact(t, dir, "mislabeled.db", dumpFixture())
	truncated := writeArtifact(t, dir, "truncated.sql",
		[]byte(dumpSignature+"CREATE TABLE t(id INTEGER);\n"))
	live := writeArtifact(t, dir, "live.db", dbFixture())
	writeArtifact(t, dir, "live.db-wal", []byte("wal frames"))

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "sqlite_backup", db, nil), "unsupported_source"},
		{"missing file", provisionPayload(t, "sqlite_db", filepath.Join(dir, "gone.db"), nil), "source_not_found"},
		{"a directory for the file kind", provisionPayload(t, "sqlite_db", dir, nil), "invalid_request"},
		{"gzip-compressed artifact named for what it is",
			provisionPayload(t, "sqlite_db", gz, nil), "unsupported_source"},
		{"dump text handed to the db kind", provisionPayload(t, "sqlite_db", dumpAsDB, nil), "invalid_request"},
		{"a database handed to the dump kind", provisionPayload(t, "sqlite_dump", db, nil), "invalid_request"},
		{"a truncated dump is refused before transfer",
			provisionPayload(t, "sqlite_dump", truncated, nil), "source_corrupt"},
		{"a live copy is refused before transfer",
			provisionPayload(t, "sqlite_db", live, nil), "unsupported_source"},
		{"backup_timezone has nothing to date",
			provisionPayload(t, "sqlite_db", db, map[string]string{"backup_timezone": "UTC"}), "invalid_request"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"sqlite_db","path":"` + db + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
			"invalid_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "provision", tc.payload, func(verbCall) (any, *protoError) {
				t.Fatal("a refusal must be decided before any sandbox call")
				return nil, nil
			})
			f := parseFinal(t, line)
			if exit != 0 || f.OK || f.Error.Code != tc.want {
				t.Errorf("final = %+v, want code %s", f, tc.want)
			}
			if len(calls) != 0 {
				t.Errorf("%d sandbox calls issued before the refusal", len(calls))
			}
		})
	}
}

func TestSandboxPreconditions(t *testing.T) {
	db := writeArtifact(t, t.TempDir(), "nightly.db", dbFixture())
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "sqlite_db", db, nil),
		func(call verbCall) (any, *protoError) {
			return errExec(127, "sh: sqlite3: not found"), nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "sqlite3") {
		t.Errorf("final = %+v, want invalid_request naming what the image lacks", f)
	}
}

// TestIntegrityVerdict proves a database the engine rejects surfaces as a
// claim about the backup, carrying sqlite3's own words.
func TestIntegrityVerdict(t *testing.T) {
	db := writeArtifact(t, t.TempDir(), "nightly.db", dbFixture())
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "sqlite_db", db, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			if argv[0] == "sh" && len(argv) == 5 {
				return errExec(1, "Parse error in 2nd command line argument: database disk image is malformed (11)"), nil
			}
			label, value := classifyExec(argv)
			if label == "" {
				t.Fatalf("unexpected exec: %v", argv)
			}
			return value, nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "malformed") {
		t.Errorf("final = %+v, want source_corrupt carrying the engine's verdict", f)
	}
}

func TestReplayVerdicts(t *testing.T) {
	dump := writeArtifact(t, t.TempDir(), "nightly.sql", dumpFixture())
	replayFails := func(stderr string) func(verbCall) (any, *protoError) {
		return func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			if argv[0] == "sh" && len(argv) == 6 {
				return errExec(1, stderr), nil
			}
			label, value := classifyExec(argv)
			if label == "" {
				t.Fatalf("unexpected exec: %v", argv)
			}
			return value, nil
		}
	}

	t.Run("a parse failure is the backup's fault", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "sqlite_dump", dump, nil),
			replayFails(`Parse error near line 255: unrecognized token: "'row2"`))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "truncated") {
			t.Errorf("final = %+v, want source_corrupt for SQL that stops mid-token", f)
		}
	})

	t.Run("a runtime failure is a failed restore", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "sqlite_dump", dump, nil),
			replayFails("Runtime error near line 3: UNIQUE constraint failed: t.id (19)"))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "Runtime error") {
			t.Errorf("final = %+v, want restore_failed carrying the engine's reason", f)
		}
	})
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
			t.Error("garbage stdin must exit non-zero so the core records adapter_crash")
		}
	})
	t.Run("teardown succeeds on empty state", func(t *testing.T) {
		line, calls, exit := driveOp(t, "teardown", `{"state":{},"reason":"failed"}`, nil)
		f := parseFinal(t, line)
		if exit != 0 || !f.OK || len(calls) != 0 {
			t.Errorf("exit=%d ok=%v calls=%d", exit, f.OK, len(calls))
		}
	})
}

func TestHealthcheck(t *testing.T) {
	payload := `{"connection":{"database":"/scratch/probavi-sqlite/restored.db"},"state":{}}`

	t.Run("a serving database", func(t *testing.T) {
		line, calls, exit := driveOp(t, "healthcheck", payload,
			func(verbCall) (any, *protoError) { return outExec("3\n"), nil })
		f := parseFinal(t, line)
		if exit != 0 || !f.OK {
			t.Fatalf("exit=%d final=%+v", exit, f)
		}
		got := struct {
			Healthy bool    `json:"healthy"`
			Latency float64 `json:"latency_seconds"`
		}{}
		if err := json.Unmarshal(f.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !got.Healthy || got.Latency < 0 {
			t.Errorf("payload = %+v", got)
		}
		// The query must open the provisioned file and force a real page
		// read — SELECT 1 answers even against garbage (measured on 3.46).
		argv := argvOf(t, calls[0])
		if argv[0] != "sqlite3" || argv[len(argv)-2] != "/scratch/probavi-sqlite/restored.db" ||
			!strings.Contains(argv[len(argv)-1], "sqlite_schema") {
			t.Errorf("healthcheck argv = %v", argv)
		}
	})

	t.Run("a failing database is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", payload,
			func(verbCall) (any, *protoError) {
				return errExec(26, "Error: in prepare, file is not a database (26)"), nil
			})
		f := parseFinal(t, line)
		if !f.OK {
			t.Fatalf("an unhealthy verdict must still be ok:true (§6.3): %+v", f)
		}
		got := struct {
			Healthy bool `json:"healthy"`
		}{}
		if err := json.Unmarshal(f.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if got.Healthy {
			t.Error("healthy = true for a database sqlite3 refused to read")
		}
	})

	t.Run("a payload without the database path is refused", func(t *testing.T) {
		line, calls, _ := driveOp(t, "healthcheck", `{"connection":{},"state":{}}`, nil)
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || len(calls) != 0 {
			t.Errorf("final = %+v calls=%d, want invalid_request with no sandbox calls", f, len(calls))
		}
	})
}
