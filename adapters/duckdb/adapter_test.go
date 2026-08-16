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

// engineVersionOut is what duckdb --version prints (measured on 1.5.5).
const engineVersionOut = "v1.5.5 (Variegata) d8cdaa33fd"

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.5}, nil
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
// simulated result; an empty label means the call was not expected.
func classifyExec(argv []string) (string, any) {
	last := argv[len(argv)-1]
	switch {
	case argv[0] == "duckdb" && len(argv) == 2 && argv[1] == "--version":
		return "engine", execValue{ExitCode: 0, DurationSeconds: 0.05,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte(engineVersionOut))}
	case argv[0] == "mkdir":
		return "mkdir", okExec()
	case argv[0] == "duckdb" && last == vetQuery:
		return "vet", execValue{ExitCode: 0, DurationSeconds: 0.1,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("2\n"))}
	case argv[0] == "duckdb" && strings.HasPrefix(last, "IMPORT DATABASE"):
		return "import", execValue{ExitCode: 0, DurationSeconds: 0.3}
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
// check can become anything but arguments to duckdb. The mode flags are
// load-bearing: without -list the output stays a decorated box even
// piped (measured).
func TestRunnerTemplateShape(t *testing.T) {
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("sql_runner is not an object")
	}
	want := []string{"duckdb", "-batch", "-list", "-noheader", "-bail",
		"-separator", "\t", "{{database}}", "{{sql}}"}
	if got, ok := runner["argv"].([]string); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("sql_runner argv = %v, want %v", runner["argv"], want)
	}
}

// assertPutDest asserts every put_file call targeted a path with the
// given prefix.
func assertPutDest(t *testing.T, calls []verbCall, wantPrefix string) {
	t.Helper()
	for _, call := range calls {
		if call.Verb != "put_file" {
			continue
		}
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
		if !strings.HasPrefix(args.DestPath, wantPrefix) {
			t.Errorf("put_file dest = %q, want prefix %q", args.DestPath, wantPrefix)
		}
	}
}

func TestProvisionPlacesDatabase(t *testing.T) {
	db := writeArtifact(t, t.TempDir(), "nightly.duckdb", dbFixture())
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "duckdb_db", db, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if want := "engine|mkdir|put_file|vet"; strings.Join(sequence, "|") != want {
		t.Errorf("sequence = %s, want %s", strings.Join(sequence, "|"), want)
	}
	assertPutDest(t, calls, "/scratch/probavi-duckdb/restored.duckdb")

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
	if got.Connection.Scheme != "duckdb" || got.Connection.Port != 0 ||
		got.Connection.Database != "/scratch/probavi-duckdb/restored.duckdb" {
		t.Errorf("connection = %+v", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	if got.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null — nothing in the artifact dates it", *got.SourceIdentity.CreatedAt)
	}
	if got.Timings["transfer_seconds"] != 0.5 || got.Timings["restore_seconds"] != 0.1 ||
		got.Timings["engine_ready_seconds"] != 0.05 {
		t.Errorf("timings = %+v, want the simulator's measured phases", got.Timings)
	}
}

func TestProvisionImportsExport(t *testing.T) {
	dir := writeExport(t, filepath.Join(t.TempDir(), "nightly"),
		"schema.sql", "load.sql", "t.csv")
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "duckdb_export", dir, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if want := "engine|mkdir|put_file|put_file|put_file|import"; strings.Join(sequence, "|") != want {
		t.Errorf("sequence = %s, want %s", strings.Join(sequence, "|"), want)
	}
	assertPutDest(t, calls, "/scratch/probavi-duckdb/export/")

	got := struct {
		Connection struct {
			Database string `json:"database"`
		} `json:"connection"`
		Timings map[string]float64 `json:"timings"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.Connection.Database != "/scratch/probavi-duckdb/restored.duckdb" {
		t.Errorf("connection.database = %q — checks must open the built database, not the export", got.Connection.Database)
	}
	if got.Timings["transfer_seconds"] != 1.5 || got.Timings["restore_seconds"] != 0.3 {
		t.Errorf("timings = %+v, want the summed transfers and the import as the restore", got.Timings)
	}
}

func TestProvisionRefusals(t *testing.T) {
	dir := t.TempDir()
	db := writeArtifact(t, dir, "nightly.duckdb", dbFixture())
	gz := writeArtifact(t, dir, "nightly.duckdb.gz", []byte{0x1f, 0x8b, 0x08, 0x00})
	live := writeArtifact(t, dir, "live.duckdb", dbFixture())
	writeArtifact(t, dir, "live.duckdb.wal", []byte("wal frames"))
	notExport := writeExport(t, filepath.Join(dir, "not-export"), "schema.sql", "t.csv")

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "duckdb_backup", db, nil), "unsupported_source"},
		{"missing file", provisionPayload(t, "duckdb_db", filepath.Join(dir, "gone.duckdb"), nil), "source_not_found"},
		{"a directory for the file kind", provisionPayload(t, "duckdb_db", dir, nil), "invalid_request"},
		{"gzip-compressed artifact named for what it is",
			provisionPayload(t, "duckdb_db", gz, nil), "unsupported_source"},
		{"a live copy is refused before transfer",
			provisionPayload(t, "duckdb_db", live, nil), "unsupported_source"},
		{"a directory without load.sql is not an export",
			provisionPayload(t, "duckdb_export", notExport, nil), "source_corrupt"},
		{"a database file for the export kind",
			provisionPayload(t, "duckdb_export", db, nil), "invalid_request"},
		{"backup_timezone has nothing to date",
			provisionPayload(t, "duckdb_db", db, map[string]string{"backup_timezone": "UTC"}), "invalid_request"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"duckdb_db","path":"` + db + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
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
	db := writeArtifact(t, t.TempDir(), "nightly.duckdb", dbFixture())
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "duckdb_db", db, nil),
		func(call verbCall) (any, *protoError) {
			return errExec(127, "exec: duckdb: not found"), nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "wrapper") {
		t.Errorf("final = %+v, want invalid_request pointing at the wrapper recipe", f)
	}
}

// vetFails answers the happy path until the opening read, which fails
// with the given stderr.
func vetFails(t *testing.T, stderr string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		argv := argvOf(t, call)
		if argv[0] == "duckdb" && argv[len(argv)-1] == vetQuery {
			return errExec(1, stderr), nil
		}
		label, value := classifyExec(argv)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argv)
		}
		return value, nil
	}
}

// TestOpenVerdicts proves a database the engine rejects surfaces as the
// right claim: corruption is the backup's fault, and a newer storage
// format is a drill config pairing a backup with a sandbox that cannot
// read it — named with both sides' versions, which only the host-read
// header and the engine probe together can supply.
func TestOpenVerdicts(t *testing.T) {
	t.Run("an invalid file is the backup's fault", func(t *testing.T) {
		db := writeArtifact(t, t.TempDir(), "nightly.duckdb", dbFixture())
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "duckdb_db", db, nil),
			vetFails(t, `Error: unable to open database '/x': IO Error: The file '/x' exists, but it is not a valid DuckDB database file!`))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "not a valid DuckDB database") {
			t.Errorf("final = %+v, want source_corrupt carrying the engine's verdict", f)
		}
	})

	t.Run("a newer storage format names both sides", func(t *testing.T) {
		db := writeArtifact(t, t.TempDir(), "future.duckdb", duckFixture(68, "v1.5.5"))
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "duckdb_db", db, nil),
			vetFails(t, `Error: unable to open database '/x': IO Error: Trying to read a database file with version number 68, but we can only read versions between 64 and 67.`))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" {
			t.Fatalf("final = %+v, want invalid_request", f)
		}
		for _, want := range []string{"storage format version 68", "written by DuckDB v1.5.5", engineVersionOut} {
			if !strings.Contains(f.Error.Message, want) {
				t.Errorf("message %q missing %q", f.Error.Message, want)
			}
		}
	})
}

func TestImportVerdicts(t *testing.T) {
	dir := writeExport(t, filepath.Join(t.TempDir(), "nightly"),
		"schema.sql", "load.sql", "t.csv")
	importFails := func(stderr string) func(verbCall) (any, *protoError) {
		return func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			if argv[0] == "duckdb" && strings.HasPrefix(argv[len(argv)-1], "IMPORT DATABASE") {
				return errExec(1, stderr), nil
			}
			label, value := classifyExec(argv)
			if label == "" {
				t.Fatalf("unexpected exec: %v", argv)
			}
			return value, nil
		}
	}

	t.Run("a missing data file is the backup's fault", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "duckdb_export", dir, nil),
			importFails(`IO Error: No files found that match the pattern '/scratch/probavi-duckdb/export/t.csv'`))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "incomplete") {
			t.Errorf("final = %+v, want source_corrupt naming the incomplete export", f)
		}
	})

	t.Run("an engine failure is a failed restore", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "duckdb_export", dir, nil),
			importFails(`Conversion Error: Could not convert string 'x' to INT32`))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "Conversion Error") {
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

const healthcheckPayload = `{"connection":{"database":"/scratch/probavi-duckdb/restored.duckdb"},"state":{}}`

func TestHealthcheckServing(t *testing.T) {
	line, calls, exit := driveOp(t, "healthcheck", healthcheckPayload,
		func(verbCall) (any, *protoError) { return outExec("3\n"), nil })
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	got := struct {
		Healthy bool    `json:"healthy"`
		Latency float64 `json:"latency_seconds"`
		Detail  string  `json:"detail"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !got.Healthy || got.Latency < 0 || !strings.Contains(got.Detail, "3 tables") {
		t.Errorf("payload = %+v", got)
	}
	argv := argvOf(t, calls[0])
	if argv[0] != "duckdb" || argv[len(argv)-2] != "/scratch/probavi-duckdb/restored.duckdb" ||
		argv[len(argv)-1] != vetQuery {
		t.Errorf("healthcheck argv = %v", argv)
	}
}

func TestHealthcheck(t *testing.T) {
	payload := healthcheckPayload

	t.Run("a failing database is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", payload,
			func(verbCall) (any, *protoError) {
				return errExec(1, "Error: unable to open database: IO Error: not a valid DuckDB database file!"), nil
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
			t.Error("healthy = true for a database duckdb refused to read")
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
