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

// errExec is a sandbox command that failed with something to say. Every
// failure this adapter classifies is a non-zero exit; which non-zero it
// was never changes the verdict.
func errExec(stderr string) any {
	return execValue{ExitCode: 1, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

func provisionPayload(t *testing.T, kind, path string, options map[string]string) string {
	t.Helper()
	req := map[string]any{
		"source":  map[string]any{"kind": kind, "path": path},
		"sandbox": map[string]any{"scratch_dir": "/scratch"},
		"options": options,
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
		return "mkdir", execValue{ExitCode: 0}
	case argv[0] == "sh" && len(argv) == 3:
		return "preflight", execValue{ExitCode: 0, DurationSeconds: 0.02}
	case argv[0] != "bash":
		return "", nil
	}
	switch len(argv) {
	case 6:
		return "count", execValue{ExitCode: 0, DurationSeconds: 0.05,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte("1000"))}
	case 7:
		return "start", execValue{ExitCode: 0, DurationSeconds: 1.1}
	}
	return "", nil
}

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 4096, DurationSeconds: 0.4}, nil
		}
		label, value := classifyExec(argvOf(t, call))
		if label == "" {
			t.Fatalf("unexpected exec: %v", argvOf(t, call))
		}
		*sequence = append(*sequence, label)
		return value, nil
	}
}

// snapshotFile writes a stand-in artifact. Nothing in this adapter reads
// the bytes for anything but the checksum, which is the point: the engine
// is the judge of a snapshot (source.go).
func snapshotFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := bytes.Repeat([]byte("q"), 4096)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestProbeGolden(t *testing.T) {
	line, calls, exit := driveOp(t, "probe", "{}", func(verbCall) (any, *protoError) {
		t.Fatal("probe must not touch the sandbox")
		return nil, nil
	})
	if len(calls) != 0 || exit != 0 {
		t.Fatalf("probe issued %d sandbox calls, exit %d", len(calls), exit)
	}
	golden := filepath.Join("testdata", "probe_response.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update once): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), line) {
		t.Errorf("probe response deviates from golden:\n got: %s\nwant: %s", line, bytes.TrimSpace(want))
	}
}

// TestRunnerTemplateShape holds the declared check runner to the contract
// internal/checks renders: the core substitutes {{database}} and {{sql}}
// and knows nothing else about the engine.
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
		t.Fatalf("sql_runner argv is %T, not []string", runner["argv"])
	}
	if argv[0] != "bash" {
		t.Errorf("runner argv[0] is %q; the script speaks HTTP through /dev/tcp, which dash "+
			"does not have, and the image has no other HTTP client", argv[0])
	}
	if got, want := argv[len(argv)-2:], []string{"{{database}}", "{{sql}}"}; !reflect.DeepEqual(got, want) {
		t.Errorf("runner argv ends %v, want %v", got, want)
	}
	if _, has := runner["env"]; has {
		t.Error("the runner declares env: Qdrant needs no credential, and an adapter that " +
			"invents one puts a value on the wire that protects nothing")
	}
}

// TestNoTelemetryEverLeavesADrill is ADR 0018 in a test. Qdrant ships with
// telemetry_disabled: false and sends usage data to its developers, so an
// engine this project starts must be told not to — every time, on every
// path. --network none makes it unreachable in practice; this makes it
// stated.
func TestNoTelemetryEverLeavesADrill(t *testing.T) {
	if !strings.Contains(startEngineScript, "--disable-telemetry") {
		t.Fatal("the engine is started without --disable-telemetry")
	}
	starts := strings.Count(startEngineScript, engineBinary+" --disable-telemetry")
	if got := strings.Count(startEngineScript, engineBinary); got != starts {
		t.Errorf("%d of the %d engine invocations carry --disable-telemetry: a path that "+
			"starts Qdrant without it phones home", starts, got)
	}
}

// TestTheEngineIsStartedDirectlyRatherThanThroughItsWrapper guards a
// deliberate choice. The image's entrypoint.sh restarts the engine in
// recovery mode after an OOM, which is the engine deciding on its own to
// serve something other than what the backup held.
func TestTheEngineIsStartedDirectlyRatherThanThroughItsWrapper(t *testing.T) {
	if strings.Contains(startEngineScript, "entrypoint.sh") {
		t.Error("the adapter starts Qdrant through the image's entrypoint wrapper, which can " +
			"restart it in recovery mode behind the drill's back")
	}
}

// TestScriptsAreValidBash catches a quoting or redirection mistake in the
// two shell programs this adapter ships without needing an engine: they
// are the parts a Go compiler cannot check.
func TestScriptsAreValidBash(t *testing.T) {
	for name, script := range map[string]string{
		"checkScript": checkScript, "startEngineScript": startEngineScript,
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseBash(t, script); err != nil {
				t.Errorf("%s is not valid bash: %v", name, err)
			}
		})
	}
}

func TestProvisionRestoresACollectionSnapshot(t *testing.T) {
	dir := t.TempDir()
	src := snapshotFile(t, dir, "orders.snapshot")

	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "qdrant_snapshot", src, map[string]string{"collection": "orders"}),
		provisionHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("provision exit %d", exit)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	// The artifact must be in place before the engine starts: Qdrant
	// restores a snapshot as a startup argument, so a transfer after the
	// start would have nothing to read.
	want := []string{"preflight", "mkdir", "put_file", "start", "count"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("call sequence %v, want %v", sequence, want)
	}
	var payload struct {
		Connection     struct{ Database string }
		SourceIdentity struct {
			Checksum  string
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		State struct{ Collection string }
	}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Connection.Database != "orders" || payload.State.Collection != "orders" {
		t.Errorf("provision reports collection %q/%q, want orders",
			payload.Connection.Database, payload.State.Collection)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("source checksum %q is not a sha256 digest", payload.SourceIdentity.Checksum)
	}
	if payload.SourceIdentity.CreatedAt != nil {
		t.Error("created_at must be null: no Qdrant snapshot records when it was taken")
	}
}

// TestTheSnapshotFlagFollowsTheSourceKind is the difference between the
// two artifact shapes, and getting it backwards would restore nothing:
// --snapshot takes one collection, --storage-snapshot takes the whole
// storage tree.
func TestTheSnapshotFlagFollowsTheSourceKind(t *testing.T) {
	for _, tc := range []struct {
		kind, wantMode string
	}{
		{"qdrant_snapshot", "collection"},
		{"qdrant_full_snapshot", "full"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			src := snapshotFile(t, dir, "a.snapshot")
			var mode string
			_, _, exit := driveOp(t, "provision", provisionPayload(t, tc.kind, src, nil),
				func(call verbCall) (any, *protoError) {
					if call.Verb == "put_file" {
						return putFileValue{}, nil
					}
					argv := argvOf(t, call)
					if label, _ := classifyExec(argv); label == "start" {
						mode = argv[5]
					}
					_, value := classifyExec(argv)
					return value, nil
				})
			if exit != 0 {
				t.Fatalf("provision exit %d", exit)
			}
			if mode != tc.wantMode {
				t.Errorf("%s starts the engine in %q mode, want %q", tc.kind, mode, tc.wantMode)
			}
		})
	}
}

// TestAWellFormedZeroIsRefused is the h2 and victoriametrics precedent. A
// snapshot of an empty collection restores green with points_count 0
// (measured), so "the engine answered" cannot be the verdict.
func TestAWellFormedZeroIsRefused(t *testing.T) {
	dir := t.TempDir()
	src := snapshotFile(t, dir, "empty.snapshot")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "qdrant_snapshot", src, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			label, value := classifyExec(argv)
			if label == "count" {
				return execValue{ExitCode: 0,
					StdoutB64: base64.StdEncoding.EncodeToString([]byte("0"))}, nil
			}
			return value, nil
		})
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("a restore that produced an empty collection was reported as a success")
	}
	if final.Error.Code != "source_corrupt" {
		t.Errorf("error code %q, want source_corrupt", final.Error.Code)
	}
}

// TestARefusedSnapshotIsTheArtifactsVerdict keeps the engine's refusal
// classified as a bad backup rather than a broken drill. Qdrant exits 101
// and never listens on a damaged snapshot, which is the artifact failing,
// not the sandbox.
func TestARefusedSnapshotIsTheArtifactsVerdict(t *testing.T) {
	dir := t.TempDir()
	src := snapshotFile(t, dir, "torn.snapshot")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "qdrant_snapshot", src, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			label, value := classifyExec(argv)
			if label == "start" {
				return errExec("qdrant exited while restoring the snapshot"), nil
			}
			return value, nil
		})
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("a snapshot the engine refused was reported as restored")
	}
	if final.Error.Code != "source_corrupt" {
		t.Errorf("error code %q, want source_corrupt", final.Error.Code)
	}
}

func TestProvisionRefusals(t *testing.T) {
	dir := t.TempDir()
	src := snapshotFile(t, dir, "a.snapshot")

	for _, tc := range []struct {
		name, kind, path, wantCode string
		handler                    func(verbCall) (any, *protoError)
	}{
		{
			name: "unknown kind", kind: "qdrant_dump", path: src,
			wantCode: "unsupported_source",
		},
		{
			name: "missing file", kind: "qdrant_snapshot",
			path: filepath.Join(dir, "nope.snapshot"), wantCode: "source_not_found",
		},
		{
			name: "directory given to the file kind", kind: "qdrant_snapshot", path: dir,
			wantCode: "invalid_request",
		},
		{
			name: "sandbox is not an idle qdrant image", kind: "qdrant_snapshot", path: src,
			wantCode: "invalid_request",
			handler: func(call verbCall) (any, *protoError) {
				return errExec("qdrant is already serving on 6333"), nil
			},
		},
		{
			name: "the restored collection never answers", kind: "qdrant_snapshot", path: src,
			wantCode: "restore_failed",
			handler: func(call verbCall) (any, *protoError) {
				if call.Verb == "put_file" {
					return putFileValue{}, nil
				}
				argv := argvOf(t, call)
				label, value := classifyExec(argv)
				if label == "count" {
					return errExec("qdrant answered 404"), nil
				}
				return value, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.handler
			if handler == nil {
				var seq []string
				handler = provisionHandler(t, &seq)
			}
			line, _, exit := driveOp(t, "provision", provisionPayload(t, tc.kind, tc.path, nil), handler)
			if exit != 0 {
				t.Fatalf("a refusal must still be a well-formed response; exit %d", exit)
			}
			final := parseFinal(t, line)
			if final.OK {
				t.Fatal("expected a refusal")
			}
			if final.Error.Code != tc.wantCode {
				t.Errorf("error code %q, want %q (%s)", final.Error.Code, tc.wantCode, final.Error.Message)
			}
		})
	}
}

func TestPITRIsRefused(t *testing.T) {
	dir := t.TempDir()
	src := snapshotFile(t, dir, "a.snapshot")
	payload := fmt.Sprintf(
		`{"source":{"kind":"qdrant_snapshot","path":%q},"pitr":{"target_time":"2026-08-30T00:00:00Z"}}`, src)
	line, calls, _ := driveOp(t, "provision", payload, func(verbCall) (any, *protoError) {
		t.Fatal("a refused pitr request must not touch the sandbox")
		return nil, nil
	})
	if len(calls) != 0 {
		t.Fatalf("pitr refusal issued %d sandbox calls", len(calls))
	}
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "invalid_request" {
		t.Errorf("pitr must be refused with invalid_request, got %+v", final)
	}
}

func TestRejectsWrongProtocolAndOp(t *testing.T) {
	stderr := &bytes.Buffer{}
	out := &bytes.Buffer{}
	in := strings.NewReader(`{"protocol":"probavi-adapter/9","request_id":"r","op":"probe"}` + "\n")
	if exit := run(in, out, stderr); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if !strings.Contains(out.String(), "unsupported_protocol") {
		t.Errorf("wrong protocol not refused: %s", out)
	}

	line, _, _ := driveOp(t, "dance", "{}", func(verbCall) (any, *protoError) { return nil, nil })
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "invalid_request" {
		t.Errorf("unknown op must be invalid_request, got %+v", final)
	}
}

func TestHealthcheck(t *testing.T) {
	line, _, _ := driveOp(t, "healthcheck", `{"connection":{"database":"orders"}}`,
		func(call verbCall) (any, *protoError) {
			return execValue{ExitCode: 0, DurationSeconds: 0.01,
				StdoutB64: base64.StdEncoding.EncodeToString([]byte("1000"))}, nil
		})
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("healthcheck failed: %+v", final.Error)
	}
	var payload struct {
		Healthy bool
		Detail  string
	}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !payload.Healthy || !strings.Contains(payload.Detail, "1000 points") {
		t.Errorf("healthcheck reported %+v", payload)
	}
}

// TestHealthcheckReportsUnhealthyRatherThanFailing keeps §6.3's
// distinction: a collection that does not answer is a result, not an
// operation error, so the drill still writes a signed record.
func TestHealthcheckReportsUnhealthyRatherThanFailing(t *testing.T) {
	line, _, _ := driveOp(t, "healthcheck", `{"connection":{"database":"orders"}}`,
		func(call verbCall) (any, *protoError) { return errExec("qdrant answered 404"), nil })
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("an unhealthy collection must not fail the operation: %+v", final.Error)
	}
	var payload struct{ Healthy bool }
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Healthy {
		t.Error("healthcheck reported healthy for a collection that answered 404")
	}
}
