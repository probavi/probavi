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

func okStdout(s string) any {
	return execValue{ExitCode: 0, DurationSeconds: 0.05,
		StdoutB64: base64.StdEncoding.EncodeToString([]byte(s))}
}

func provisionPayload(t *testing.T, kind, path string) string {
	t.Helper()
	req := map[string]any{
		"source":  map[string]any{"kind": kind, "path": path},
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
// shell calls are told apart by the script they carry, which is more
// honest than a shape puzzle: the scripts are constants.
func classifyExec(argv []string) (string, any) {
	switch argv[0] {
	case "mkdir":
		return "mkdir", execValue{ExitCode: 0}
	case "tar":
		return "tar", execValue{ExitCode: 0, DurationSeconds: 0.2}
	case "mv":
		return "mv", execValue{ExitCode: 0}
	case "cat":
		return "cat", okStdout("1")
	case "sh":
	default:
		return "", nil
	}
	if len(argv) < 3 {
		return "", nil
	}
	switch argv[2] {
	case preflightScript:
		return "preflight", execValue{ExitCode: 0, DurationSeconds: 0.02}
	case locateScript:
		return "locate", okStdout("/scratch/probavi-weaviate/extract/unpacked")
	case startEngineScript:
		return "start", execValue{ExitCode: 0, DurationSeconds: 1.1}
	case restoreScript:
		return "restore", execValue{ExitCode: 0, DurationSeconds: 0.8}
	case checkScript:
		return "count", okStdout("1000")
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
	if argv[0] != "sh" {
		t.Errorf("runner argv[0] is %q; the image carries busybox sh and wget, nothing else "+
			"that speaks HTTP", argv[0])
	}
	if got, want := argv[len(argv)-2:], []string{"{{database}}", "{{sql}}"}; !reflect.DeepEqual(got, want) {
		t.Errorf("runner argv ends %v, want %v", got, want)
	}
	if _, has := runner["env"]; has {
		t.Error("the runner declares env: the drill runs with anonymous access enabled, and " +
			"an adapter that invents a credential puts a value on the wire that protects nothing")
	}
}

// TestNoTelemetryEverLeavesADrill is ADR 0018 in a test. Without
// DISABLE_TELEMETRY=true the engine POSTs usage data to
// telemetry.weaviate.io at startup (measured), so an engine this project
// starts must be told not to — every time, on every path.
func TestNoTelemetryEverLeavesADrill(t *testing.T) {
	if !strings.Contains(startEngineScript, "DISABLE_TELEMETRY=true") {
		t.Fatal("the engine is started without DISABLE_TELEMETRY=true")
	}
	if got := strings.Count(startEngineScript, "nohup "+engineBinary); got != 1 {
		t.Fatalf("%d engine invocations in the start script, want exactly 1 — a second path "+
			"could start the engine without the telemetry environment", got)
	}
}

// TestTheClusterEnvironmentIsPinned guards the two environment values a
// restore cannot work without: the advertise address that lets the
// memberlist layer start under --network none, and the node name the
// backup demands (both measured).
func TestTheClusterEnvironmentIsPinned(t *testing.T) {
	if !strings.Contains(startEngineScript, "CLUSTER_ADVERTISE_ADDR=127.0.0.1") {
		t.Error("CLUSTER_ADVERTISE_ADDR is not pinned to loopback: under --network none the " +
			"memberlist layer finds no private IP and the engine refuses to start")
	}
	if !strings.Contains(startEngineScript, `CLUSTER_HOSTNAME="$node"`) {
		t.Error("CLUSTER_HOSTNAME is not taken from the backup: the engine refuses to " +
			"restore another node's backup")
	}
}

func TestProvisionRestoresABackupDirectory(t *testing.T) {
	parent := t.TempDir()
	src := writeBackupFixture(t, parent, "nightly", backupSpec{})

	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "weaviate_backup", src),
		provisionHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("provision exit %d", exit)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	// The artifact must be in place before the engine starts — the backup
	// tree has to sit under the filesystem backend's root when the module
	// reads it — and the restore call comes only after readiness.
	want := []string{"preflight", "mkdir", "mkdir",
		"put_file", "put_file", "put_file", "start", "restore", "count"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("call sequence %v, want %v", sequence, want)
	}
	var payload struct {
		Connection     struct{ Database string }
		SourceIdentity struct {
			Checksum  string
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings struct {
			EngineReadySeconds float64 `json:"engine_ready_seconds"`
			TransferSeconds    float64 `json:"transfer_seconds"`
			RestoreSeconds     float64 `json:"restore_seconds"`
		}
		State struct {
			Class    string
			BackupID string `json:"backup_id"`
		}
	}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Connection.Database != "Books" || payload.State.Class != "Books" {
		t.Errorf("provision reports class %q/%q, want Books",
			payload.Connection.Database, payload.State.Class)
	}
	if payload.State.BackupID != "nightly" {
		t.Errorf("backup_id %q, want nightly", payload.State.BackupID)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("source checksum %q is not a sha256 digest", payload.SourceIdentity.Checksum)
	}
	if payload.SourceIdentity.CreatedAt == nil || *payload.SourceIdentity.CreatedAt != fixtureCompletedAt {
		t.Errorf("created_at = %v, want the backup's own completion instant %s",
			payload.SourceIdentity.CreatedAt, fixtureCompletedAt)
	}
	if payload.Timings.EngineReadySeconds <= 0 || payload.Timings.RestoreSeconds <= 0 {
		t.Errorf("timings = %+v, want the simulated measurements", payload.Timings)
	}
}

// archiveRecorder simulates the sandbox through the tar flow, answering
// the metadata read with the fixture's own bytes and recording what the
// adapter told the engine.
type archiveRecorder struct {
	t         *testing.T
	meta      []byte
	sequence  []string
	startNode string
	restoreID string
	mvDest    string
}

func (r *archiveRecorder) handle(call verbCall) (any, *protoError) {
	if call.Verb == "put_file" {
		r.sequence = append(r.sequence, "put_file")
		return putFileValue{BytesCopied: 4096, DurationSeconds: 0.4}, nil
	}
	argv := argvOf(r.t, call)
	label, value := classifyExec(argv)
	if label == "" {
		r.t.Fatalf("unexpected exec: %v", argv)
	}
	r.sequence = append(r.sequence, label)
	switch label {
	case "cat":
		return okStdout(string(r.meta)), nil
	case "start":
		r.startNode = argv[4]
	case "restore":
		r.restoreID = argv[4]
	case "mv":
		r.mvDest = argv[2]
	}
	return value, nil
}

// TestProvisionRestoresAnArchive walks the tar flow: unpack, read the
// backup's own metadata where the files now are, and pin the engine to
// the node the backup names.
func TestProvisionRestoresAnArchive(t *testing.T) {
	parent := t.TempDir()
	fixture := writeBackupFixture(t, parent, "arch1", backupSpec{node: "prod-7"})
	src := tarOf(t, fixture)
	meta, err := os.ReadFile(filepath.Join(fixture, metaFileName))
	if err != nil {
		t.Fatalf("read fixture meta: %v", err)
	}

	rec := &archiveRecorder{t: t, meta: meta}
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "weaviate_backup_tar", src), rec.handle)
	if exit != 0 {
		t.Fatalf("provision exit %d", exit)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	want := []string{"preflight", "mkdir", "mkdir",
		"put_file", "tar", "locate", "cat", "mv", "start", "restore", "count"}
	if !reflect.DeepEqual(rec.sequence, want) {
		t.Errorf("call sequence %v, want %v", rec.sequence, want)
	}
	if rec.startNode != "prod-7" {
		t.Errorf("engine started as node %q, want the backup's own prod-7", rec.startNode)
	}
	if rec.restoreID != "arch1" {
		t.Errorf("restore asked for id %q, want the backup's own arch1", rec.restoreID)
	}
	if !strings.HasSuffix(rec.mvDest, "/"+backupsDir+"/arch1") {
		t.Errorf("the backup was placed at %q, want …/%s/arch1 — the directory name must "+
			"equal the backup's own id for the restore call to find it", rec.mvDest, backupsDir)
	}
	var payload struct {
		SourceIdentity struct {
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
	}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.SourceIdentity.CreatedAt == nil || *payload.SourceIdentity.CreatedAt != fixtureCompletedAt {
		t.Errorf("created_at = %v, want %s from the archive's own metadata",
			payload.SourceIdentity.CreatedAt, fixtureCompletedAt)
	}
}

// TestAWellFormedZeroIsRefused is the qdrant, h2 and victoriametrics
// precedent. A backup of an empty class restores green with count 0
// (measured), so "the engine answered" cannot be the verdict.
func TestAWellFormedZeroIsRefused(t *testing.T) {
	parent := t.TempDir()
	src := writeBackupFixture(t, parent, "empty", backupSpec{})
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "weaviate_backup", src),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			label, value := classifyExec(argv)
			if label == "count" {
				return okStdout("0"), nil
			}
			return value, nil
		})
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("a restore that produced an empty class was reported as a success")
	}
	if final.Error.Code != "source_corrupt" {
		t.Errorf("error code %q, want source_corrupt", final.Error.Code)
	}
}

// TestARefusedRestoreIsTheArtifactsVerdict keeps the engine's refusal
// classified as a bad backup rather than a broken drill: a truncated
// chunk, a flipped byte and a missing file all fail the restore with the
// engine's own words, and the class is never created (measured).
func TestARefusedRestoreIsTheArtifactsVerdict(t *testing.T) {
	parent := t.TempDir()
	src := writeBackupFixture(t, parent, "torn", backupSpec{})
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "weaviate_backup", src),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			label, value := classifyExec(argv)
			if label == "restore" {
				return errExec(`'error':'restore class Books: unzip chunk Books/chunk-1: unexpected EOF'`), nil
			}
			return value, nil
		})
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("a backup the engine refused was reported as restored")
	}
	if final.Error.Code != "source_corrupt" {
		t.Errorf("error code %q, want source_corrupt", final.Error.Code)
	}
}

// TestAnEngineThatDoesNotStartIsNotTheArtifactsFault: the engine starts
// on an empty data directory before the artifact is judged, so a failed
// start is the drill's environment.
func TestAnEngineThatDoesNotStartIsNotTheArtifactsFault(t *testing.T) {
	parent := t.TempDir()
	src := writeBackupFixture(t, parent, "fine", backupSpec{})
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "weaviate_backup", src),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			argv := argvOf(t, call)
			label, value := classifyExec(argv)
			if label == "start" {
				return errExec("weaviate exited while starting"), nil
			}
			return value, nil
		})
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("a drill whose engine never started was reported as restored")
	}
	if final.Error.Code != "restore_failed" {
		t.Errorf("error code %q, want restore_failed", final.Error.Code)
	}
}

func TestChooseClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		options  map[string]string
		classes  []string
		want     string
		wantCode string
	}{
		{name: "the single class is the default", classes: []string{"Books"}, want: "Books"},
		{name: "the option picks among several",
			options: map[string]string{"class": "Users"},
			classes: []string{"Books", "Users"}, want: "Users"},
		{name: "several classes need the option",
			classes: []string{"Books", "Users"}, wantCode: "invalid_request"},
		{name: "an option outside the backup is refused",
			options: map[string]string{"class": "Ghost"},
			classes: []string{"Books"}, wantCode: "invalid_request"},
		{name: "unknown classes defer to the engine",
			options: map[string]string{"class": "Books"}, want: "Books"},
		{name: "no metadata falls back to the constant", want: fallbackClass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, perr := chooseClass(tc.options, &resolvedSource{classes: tc.classes})
			switch {
			case tc.wantCode != "":
				if perr == nil || perr.Code != tc.wantCode {
					t.Errorf("chooseClass = %q, %+v; want code %s", got, perr, tc.wantCode)
				}
			case perr != nil:
				t.Errorf("chooseClass: %+v", perr)
			case got != tc.want:
				t.Errorf("chooseClass = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProvisionRefusals(t *testing.T) {
	parent := t.TempDir()
	src := writeBackupFixture(t, parent, "fine", backupSpec{})

	for _, tc := range []struct {
		name, kind, path, wantCode string
		handler                    func(verbCall) (any, *protoError)
	}{
		{
			name: "unknown kind", kind: "weaviate_dump", path: src,
			wantCode: "unsupported_source",
		},
		{
			name: "missing directory", kind: "weaviate_backup",
			path: filepath.Join(parent, "nope"), wantCode: "source_not_found",
		},
		{
			name: "file given to the directory kind", kind: "weaviate_backup",
			path: filepath.Join(src, metaFileName), wantCode: "invalid_request",
		},
		{
			name: "directory given to the archive kind", kind: "weaviate_backup_tar",
			path: src, wantCode: "invalid_request",
		},
		{
			name: "sandbox is not an idle weaviate image", kind: "weaviate_backup", path: src,
			wantCode: "invalid_request",
			handler: func(call verbCall) (any, *protoError) {
				return errExec("something is already serving on 8080"), nil
			},
		},
		{
			name: "the restored class never answers", kind: "weaviate_backup", path: src,
			wantCode: "restore_failed",
			handler: func(call verbCall) (any, *protoError) {
				if call.Verb == "put_file" {
					return putFileValue{}, nil
				}
				argv := execArgs{}
				_ = json.Unmarshal(call.Args, &argv) //nolint:errcheck // shaped above
				label, value := classifyExec(argv.Argv)
				if label == "count" {
					return errExec("weaviate answered: HTTP/1.1 422"), nil
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
			line, _, exit := driveOp(t, "provision", provisionPayload(t, tc.kind, tc.path), handler)
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

// TestABackupTimezoneParamIsRefused: the one declaration other adapters
// accept that would be a silent no-op here — Weaviate's own timestamps
// carry their zone.
func TestABackupTimezoneParamIsRefused(t *testing.T) {
	parent := t.TempDir()
	src := writeBackupFixture(t, parent, "fine", backupSpec{})
	payload := fmt.Sprintf(
		`{"source":{"kind":"weaviate_backup","path":%q,"params":{"backup_timezone":"Europe/Budapest"}}}`, src)
	line, calls, _ := driveOp(t, "provision", payload, func(verbCall) (any, *protoError) {
		t.Fatal("a refused parameter must not touch the sandbox")
		return nil, nil
	})
	if len(calls) != 0 {
		t.Fatalf("the refusal issued %d sandbox calls", len(calls))
	}
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "invalid_request" {
		t.Errorf("backup_timezone must be refused with invalid_request, got %+v", final)
	}
}

func TestPITRIsRefused(t *testing.T) {
	parent := t.TempDir()
	src := writeBackupFixture(t, parent, "fine", backupSpec{})
	payload := fmt.Sprintf(
		`{"source":{"kind":"weaviate_backup","path":%q},"pitr":{"target_time":"2026-09-03T00:00:00Z"}}`, src)
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
	line, _, _ := driveOp(t, "healthcheck", `{"connection":{"database":"Books"}}`,
		func(call verbCall) (any, *protoError) {
			return okStdout("1000"), nil
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
	if !payload.Healthy || !strings.Contains(payload.Detail, "1000 objects") {
		t.Errorf("healthcheck reported %+v", payload)
	}
}

// TestHealthcheckReportsUnhealthyRatherThanFailing keeps §6.3's
// distinction: a class that does not answer is a result, not an operation
// error, so the drill still writes a signed record.
func TestHealthcheckReportsUnhealthyRatherThanFailing(t *testing.T) {
	line, _, _ := driveOp(t, "healthcheck", `{"connection":{"database":"Books"}}`,
		func(call verbCall) (any, *protoError) { return errExec("weaviate answered: HTTP/1.1 422"), nil })
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("an unhealthy class must not fail the operation: %+v", final.Error)
	}
	var payload struct{ Healthy bool }
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Healthy {
		t.Error("healthcheck reported healthy for a class that answered 422")
	}
}
