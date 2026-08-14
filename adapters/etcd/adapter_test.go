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

func errExec(exit int, stderr string) any {
	return execValue{ExitCode: exit, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

// writeSnapshot writes a fixture standing in for an etcdctl snapshot: the
// adapter never parses the bytes on the host (etcdutl inside the sandbox
// is the authority), so any content exercises the host-side paths.
func writeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(path, []byte("bbolt-snapshot-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	started := false
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return handlePut(t, call), nil
		}
		argv := argvOf(t, call)
		label, value := classifyExec(argv, &started)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argv)
		}
		if label == "health" && !started {
			t.Error("health polled before the server was started")
		}
		*sequence = append(*sequence, label)
		return value, nil
	}
}

// handlePut asserts the transfer destination and answers it.
func handlePut(t *testing.T, call verbCall) any {
	t.Helper()
	args := putFileArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("put_file args: %v", err)
	}
	if args.DestPath != "/scratch/probavi-snapshot.db" {
		t.Errorf("put_file dest = %q", args.DestPath)
	}
	return putFileValue{BytesCopied: 20, DurationSeconds: 0.4}
}

// classifyExec labels one exec call of the happy path and returns its
// simulated result; an empty label means the call was not expected.
func classifyExec(argv []string, started *bool) (string, any) {
	joined := strings.Join(argv, " ")
	switch {
	case argv[0] == "sh" && strings.Contains(joined, "true"):
		return "shell-probe", okExec()
	case argv[0] == "etcdctl":
		return "health", okExec()
	case argv[0] == "etcdutl" && argv[1] == "snapshot" && argv[2] == "status":
		return "status", okExec()
	case argv[0] == "etcdutl" && argv[1] == "snapshot" && argv[2] == "restore":
		return "restore", execValue{ExitCode: 0, DurationSeconds: 1.5}
	case argv[0] == "sh" && strings.Contains(joined, "etcd --data-dir"):
		*started = true
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.2}
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

// TestRunnerExpansionIsNotShellParsing pins the property the sql_runner
// template rests on: the check text is word-split, never re-parsed as
// shell syntax, so operators like ; | && $() in a check cannot become
// commands.
func TestRunnerExpansionIsNotShellParsing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	// If expansion were shell parsing, either of these would create marker.
	for _, hostile := range []string{
		"get /k; touch " + marker,
		"get /k && touch " + marker,
		"get $(touch " + marker + ")",
		"get `touch " + marker + "`",
		"get /k | touch " + marker,
	} {
		// Run the template's argv with a stand-in for etcdctl that ignores
		// its arguments: only the shell's treatment of $0 is under test.
		script := "set -f; exec true $0"
		cmd := []string{"sh", "-c", script, hostile}
		if err := runHost(t, cmd); err != nil {
			t.Fatalf("runner invocation failed for %q: %v", hostile, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("check text %q executed as shell — the runner must only word-split", hostile)
		}
	}
}

// runHost executes argv on the host, mirroring how the sandbox exec verb
// launches it: directly, no shell wrapper of the harness's own.
func runHost(t *testing.T, argv []string) error {
	t.Helper()
	return execCommand(argv)
}

func TestProvisionRestoresSnapshot(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir())
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "etcd_snapshot", snap, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := "shell-probe|put_file|status|restore|start|health"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}

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
	if got.Connection.Scheme != "etcd" || got.Connection.Port != defaultPort {
		t.Errorf("connection = %+v", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	if got.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null — a snapshot records no wall clock", *got.SourceIdentity.CreatedAt)
	}
	if got.Timings["restore_seconds"] != 1.5 || got.Timings["transfer_seconds"] != 0.4 {
		t.Errorf("timings = %+v, want the simulator's measured values", got.Timings)
	}
}

func TestProvisionRefusals(t *testing.T) {
	dir := t.TempDir()
	snap := writeSnapshot(t, dir)

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "etcd_backup", snap, nil), "unsupported_source"},
		{"missing snapshot", provisionPayload(t, "etcd_snapshot", filepath.Join(dir, "gone.db"), nil), "source_not_found"},
		{"a directory for the file kind", provisionPayload(t, "etcd_snapshot", dir, nil), "invalid_request"},
		{"backup_timezone has nothing to act on",
			provisionPayload(t, "etcd_snapshot", snap, map[string]string{"backup_timezone": "UTC"}), "invalid_request"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"etcd_snapshot","path":"` + snap + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
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
	snap := writeSnapshot(t, t.TempDir())

	t.Run("a shell-less image is named, with the fix", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "etcd_snapshot", snap, nil),
			func(call verbCall) (any, *protoError) {
				return nil, protoErr("sandbox_error", true, `exec: "sh": executable file not found in $PATH`)
			})
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "distroless") {
			t.Errorf("final = %+v, want invalid_request naming the distroless images and the wrapper", f)
		}
	})

}

func TestSnapshotVerdicts(t *testing.T) {
	snap := writeSnapshot(t, t.TempDir())

	t.Run("a file etcdutl rejects is the backup's fault", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "etcd_snapshot", snap, nil),
			func(call verbCall) (any, *protoError) {
				if call.Verb == "put_file" {
					return putFileValue{}, nil
				}
				argv := argvOf(t, call)
				switch {
				case argv[0] == "sh":
					return okExec(), nil
				case argv[0] == "etcdutl" && argv[2] == "status":
					return errExec(1, "Error: snapshot file corrupt"), nil
				}
				t.Fatalf("unexpected exec: %v", argv)
				return nil, nil
			})
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_corrupt" {
			t.Errorf("final = %+v, want source_corrupt", f)
		}
	})

	t.Run("a hashless data-dir copy gets the actionable message", func(t *testing.T) {
		perr := mapRestoreFailure([]byte("Error: expected sha256 hash, but no hash found in snapshot"))
		if perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "etcdctl snapshot save") {
			t.Errorf("perr = %+v, want source_corrupt telling the operator how to take the snapshot", perr)
		}
	})

	t.Run("other restore failures blame the restore", func(t *testing.T) {
		perr := mapRestoreFailure([]byte("Error: data-dir already exists"))
		if perr.Code != "restore_failed" {
			t.Errorf("perr = %+v, want restore_failed", perr)
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
	t.Run("a healthy endpoint", func(t *testing.T) {
		line, _, exit := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return okExec(), nil })
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
	})
	t.Run("an unanswering endpoint is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return errExec(1, "context deadline exceeded"), nil })
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
			t.Error("healthy = true for an endpoint that exited non-zero")
		}
	})
}
