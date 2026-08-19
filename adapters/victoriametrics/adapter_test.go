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

func outExec(stdout string) any {
	return execValue{StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

func errExec(stderr string) any {
	return execValue{ExitCode: 1, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

// verdictSeconds is the duration the simulated verdict read reports, so
// the timing assertions have something measured to add up.
const verdictSeconds = 0.05

// timedOut is outExec with a measured duration, for the calls whose
// durations feed the reported restore phase.
func timedOut(stdout string) any {
	return execValue{DurationSeconds: verdictSeconds,
		StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
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

// simulated answers per flow step; tests override single entries.
type simulated struct {
	engine   any // the wrapper-image probe
	restore  any // vmrestore
	ready    any // the health poll
	census   any // the status/tsdb read
	recover  any // the metadata recovery for opaque archives
	fatalLog any // the grep for the server's own fatal line
}

// statusBody is the answer /api/v1/status/tsdb gives for a restored
// server holding series (measured shape).
func statusBody(series int) string {
	return fmt.Sprintf(`{"status":"success","data":{"totalSeries":%d,"totalLabelValuePairs":9}}`, series)
}

// fixtureSeries is what a healthy restore reports through the status
// endpoint; the verdict tests override the answer directly.
const fixtureSeries = 3

func defaultSimulated() simulated {
	return simulated{
		engine:   okExec(),
		restore:  execValue{ExitCode: 0, DurationSeconds: 1.5},
		ready:    okExec(),
		census:   timedOut(statusBody(fixtureSeries)),
		recover:  outExec(`{"created_at":"2026-08-18T18:23:25Z"}`),
		fatalLog: errExec(""),
	}
}

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string, sim simulated) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.5}, nil
		}
		label, value := classifyExec(argvOf(t, call), sim)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argvOf(t, call))
		}
		*sequence = append(*sequence, label)
		return value, nil
	}
}

// classifyExec labels one exec call of the happy path and returns its
// simulated result; an empty label means the call was not expected.
func classifyExec(argv []string, sim simulated) (string, any) {
	switch argv[0] {
	case "sh":
		return classifyShellExec(argv[2], sim)
	case "mkdir":
		return "mkdir", okExec()
	case "tar":
		return "unpack", okExec()
	case "cat":
		return "recover", sim.recover
	case "vmrestore":
		return "restore", sim.restore
	case "grep":
		return "fatal", sim.fatalLog
	case "wget":
		if strings.Contains(strings.Join(argv, " "), "/health") {
			return "ready", sim.ready
		}
		return "census", sim.census
	case "promtool":
		return "query", outExec("{} => 3 @[1787077405]\n")
	}
	return "", nil
}

// classifyShellExec labels the shell one-liners by their scripts.
func classifyShellExec(script string, sim simulated) (string, any) {
	switch {
	case script == engineProbeScript:
		return "engine", sim.engine
	case script == backupDirScript:
		return "locate", outExec("/scratch/probavi-victoriametrics/extract\n")
	case strings.Contains(script, "victoria-metrics -storageDataPath"):
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.1}
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

// decodeProvision unpacks the parts of a provision payload the tests
// assert on.
func decodeProvision(t *testing.T, f finalResponse) (connection struct {
	Scheme   string `json:"scheme"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
}, createdAt *string, timings map[string]float64) {
	t.Helper()
	res := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Port     int    `json:"port"`
			Database string `json:"database"`
			User     string `json:"user"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			SizeBytes int64   `json:"size_bytes"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
	}{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return res.Connection, res.SourceIdentity.CreatedAt, res.Timings
}

func TestProvisionRestoresBackupDir(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "victoriametrics_backup", dir, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	// The wrapper-image probe, the skeleton, one put_file per file — the
	// two markers, the partition's parts.json and its one part — then the
	// restore, the background start, readiness, and the verdict read.
	want := "engine|mkdir|put_file|put_file|put_file|put_file|restore|start|ready|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	conn, createdAt, timings := decodeProvision(t, f)
	if conn.Scheme != "http" || conn.Port != 8428 || conn.Database != "1787077405" {
		t.Errorf("connection = %+v, want the backup's own instant in database", conn)
	}
	if createdAt == nil || *createdAt != "2026-08-18T18:23:25.000Z" {
		t.Errorf("created_at = %v, want the instant the backup states", createdAt)
	}
	if timings["transfer_seconds"] != 2.0 || timings["engine_ready_seconds"] <= 0 ||
		timings["restore_seconds"] <= 0 {
		t.Errorf("timings = %+v, want measured phases", timings)
	}
}

// TestLaunchLinePinsRetentionOff pins the flag that stops the sandbox
// server from expiring the artifact. Measured: with the default one-month
// retention a restored 90-day history serves 48 of its 89 samples, and
// nothing anywhere reports the loss.
func TestLaunchLinePinsRetentionOff(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	var sequence []string
	_, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "victoriametrics_backup", dir, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	if exit != 0 {
		t.Fatalf("provision exit = %d", exit)
	}
	script := ""
	for _, call := range calls {
		if call.Verb != "exec" {
			continue
		}
		if argv := argvOf(t, call); argv[0] == "sh" && len(argv) == 3 &&
			strings.Contains(argv[2], "victoria-metrics -storageDataPath") {
			script = argv[2]
		}
	}
	if script == "" {
		t.Fatal("no server launch among the exec calls")
	}
	if !strings.Contains(script, "-retentionPeriod=100y") {
		t.Errorf("launch line does not pin retention off: %s", script)
	}
}

func TestProvisionUnpacksArchive(t *testing.T) {
	backup := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	archive := buildTar(t, filepath.Join(t.TempDir(), "b.tar.gz"), backup, "backup", true)
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "victoriametrics_backup_tar", archive, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := "engine|mkdir|put_file|unpack|locate|restore|start|ready|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	_, createdAt, _ := decodeProvision(t, f)
	if createdAt == nil || *createdAt != "2026-08-18T18:23:25.000Z" {
		t.Errorf("created_at = %v, want the instant the archive states", createdAt)
	}
}

// TestProvisionRecoversMetadataFromOpaqueArchive proves the archive
// contract: what the host could not read, the sandbox reads after
// unpacking, so the drill still evaluates its checks at the instant the
// backup states.
func TestProvisionRecoversMetadataFromOpaqueArchive(t *testing.T) {
	opaque := filepath.Join(t.TempDir(), "opaque.tar")
	writeAt(t, opaque, "\x1f\x8bnot really a gzip stream")
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "victoriametrics_backup_tar", opaque, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := "engine|mkdir|put_file|unpack|locate|recover|restore|start|ready|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	_, createdAt, _ := decodeProvision(t, f)
	if createdAt == nil || *createdAt != "2026-08-18T18:23:25.000Z" {
		t.Errorf("created_at = %v, want the instant recovered in the sandbox", createdAt)
	}
}

func TestSeriesVerdict(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	tests := []struct {
		name     string
		census   any
		wantOK   bool
		wantCode string
	}{
		{"a server holding the backup's series", timedOut(statusBody(3)), true, ""},
		{"a server that is up and holds nothing", timedOut(statusBody(0)),
			false, "source_corrupt"},
		// Positive evidence only: an answer the adapter cannot read is
		// not evidence of an empty restore.
		{"an answer that does not parse", timedOut("<html>nope</html>"), true, ""},
		{"a read that fails outright", errExec("connection refused"), true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sim := defaultSimulated()
			sim.census = tc.census
			var sequence []string
			line, _, _ := driveOp(t, "provision",
				provisionPayload(t, "victoriametrics_backup", dir, nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (%+v)", f.OK, tc.wantOK, f.Error)
			}
			if !tc.wantOK && f.Error.Code != tc.wantCode {
				t.Errorf("code = %s, want %s", f.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestProvisionSandboxRefusals(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	tests := []struct {
		name     string
		sim      func(s *simulated)
		wantCode string
		wantMsg  string
	}{
		{
			name:     "an image without the tools names the missing one",
			sim:      func(s *simulated) { s.engine = execValue{ExitCode: 1, StdoutB64: b64("vmrestore\n")} },
			wantCode: "invalid_request", wantMsg: "vmrestore",
		},
		{
			name: "vmrestore's own completeness refusal",
			sim: func(s *simulated) {
				s.restore = errExec(`cannot restore from backup: cannot find ` + completeMarker +
					` file in fsremote '/x'; this means either incomplete backup or old backup`)
			},
			wantCode: "source_corrupt", wantMsg: completeMarker,
		},
		{
			name:     "any other vmrestore failure",
			sim:      func(s *simulated) { s.restore = errExec("cannot read from fsremote: permission denied") },
			wantCode: "restore_failed", wantMsg: "vmrestore failed",
		},
		{
			name: "a server that dies on an incomplete storage",
			sim: func(s *simulated) {
				s.ready = errExec("")
				s.fatalLog = outExec(`FATAL: part "/d/data/small/2026_05/18CC" is listed in ` +
					`"/d/data/small/2026_05/parts.json", but is missing on disk`)
			},
			wantCode: "source_corrupt", wantMsg: "missing on disk",
		},
		{
			name: "a server that dies for a reason of its own",
			sim: func(s *simulated) {
				s.ready = errExec("")
				s.fatalLog = outExec("FATAL: cannot open storage: too many open files")
			},
			wantCode: "restore_failed", wantMsg: "failed to start",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sim := defaultSimulated()
			tc.sim(&sim)
			var sequence []string
			line, _, _ := driveOp(t, "provision",
				provisionPayload(t, "victoriametrics_backup", dir, nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK {
				t.Fatalf("provision succeeded, want %s", tc.wantCode)
			}
			if f.Error.Code != tc.wantCode || !strings.Contains(f.Error.Message, tc.wantMsg) {
				t.Errorf("refusal = %s %q, want %s containing %q",
					f.Error.Code, f.Error.Message, tc.wantCode, tc.wantMsg)
			}
		})
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestProvisionRequestRefusals(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "backup"), "2026-08-18T18:23:25Z", onePartition())
	never := func(verbCall) (any, *protoError) {
		return nil, protoErr("internal", false, "the sandbox must not be touched")
	}
	tests := []struct {
		name, payload, wantCode string
	}{
		{"a pitr drill", fmt.Sprintf(
			`{"source":{"kind":"victoriametrics_backup","path":%q},"sandbox":{},"pitr":{"target_time":"t"}}`,
			dir), "invalid_request"},
		{"a declared backup timezone", provisionPayload(t, "victoriametrics_backup", dir,
			map[string]string{backupTimezoneParam: "Europe/Budapest"}), "invalid_request"},
		{"a malformed payload", `"not an object"`, "invalid_request"},
		{"an unknown kind", provisionPayload(t, "victoriametrics_dump", dir, nil), "unsupported_source"},
		{"a source that does not exist", provisionPayload(t, "victoriametrics_backup",
			filepath.Join(t.TempDir(), "absent"), nil), "source_not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "provision", tc.payload, never)
			f := parseFinal(t, line)
			if exit != 0 || f.OK || f.Error.Code != tc.wantCode {
				t.Fatalf("exit=%d final=%+v, want %s", exit, f, tc.wantCode)
			}
			if len(calls) != 0 {
				t.Errorf("calls = %d, want the refusal before any sandbox call", len(calls))
			}
		})
	}
}

func TestHealthcheck(t *testing.T) {
	t.Run("serving", func(t *testing.T) {
		line, calls, _ := driveOp(t, "healthcheck",
			`{"connection":{"database":"1787077405"},"state":{}}`,
			func(call verbCall) (any, *protoError) {
				argv := argvOf(t, call)
				if argv[0] != "promtool" {
					t.Errorf("healthcheck argv = %v, want the declared client", argv)
				}
				return outExec("{} => 3 @[1787077405]\n"), nil
			})
		f := parseFinal(t, line)
		res := struct {
			Healthy bool   `json:"healthy"`
			Detail  string `json:"detail"`
		}{}
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			t.Fatal(err)
		}
		if !f.OK || !res.Healthy || len(calls) != 1 {
			t.Errorf("final=%+v res=%+v calls=%d", f, res, len(calls))
		}
	})

	t.Run("not serving is a result, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck",
			`{"connection":{"database":"1787077405"},"state":{}}`,
			func(verbCall) (any, *protoError) { return errExec("connection refused"), nil })
		f := parseFinal(t, line)
		res := struct {
			Healthy bool `json:"healthy"`
		}{}
		if err := json.Unmarshal(f.Payload, &res); err != nil {
			t.Fatal(err)
		}
		if !f.OK || res.Healthy {
			t.Errorf("final=%+v res=%+v", f, res)
		}
	})

	t.Run("without an instant", func(t *testing.T) {
		line, calls, _ := driveOp(t, "healthcheck", `{"connection":{},"state":{}}`,
			func(verbCall) (any, *protoError) {
				t.Fatal("must not touch the sandbox")
				return nil, nil
			})
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || len(calls) != 0 {
			t.Errorf("final=%+v calls=%d", f, len(calls))
		}
	})
}

func TestTeardownReleasesNothingExternal(t *testing.T) {
	line, calls, exit := driveOp(t, "teardown", `{"state":{},"outcome":"completed"}`,
		func(verbCall) (any, *protoError) {
			t.Fatal("teardown must not touch the sandbox")
			return nil, nil
		})
	f := parseFinal(t, line)
	res := struct {
		Released bool `json:"released"`
	}{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatal(err)
	}
	if exit != 0 || !f.OK || !res.Released || len(calls) != 0 {
		t.Errorf("exit=%d final=%+v res=%+v calls=%d", exit, f, res, len(calls))
	}
}

func TestRejectsWrongProtocolAndOp(t *testing.T) {
	t.Run("an unknown op", func(t *testing.T) {
		line, _, exit := driveOp(t, "dance", "{}", func(verbCall) (any, *protoError) {
			t.Fatal("must not touch the sandbox")
			return nil, nil
		})
		f := parseFinal(t, line)
		if exit != 0 || f.OK || f.Error.Code != "invalid_request" {
			t.Errorf("exit=%d final=%+v", exit, f)
		}
	})

	t.Run("a protocol version this adapter does not speak", func(t *testing.T) {
		out := &bytes.Buffer{}
		in := strings.NewReader(`{"protocol":"probavi-adapter/9","request_id":"r","op":"probe"}` + "\n")
		if exit := run(in, out, io.Discard); exit != 0 {
			t.Errorf("exit = %d, want a well-formed refusal", exit)
		}
		f := finalResponse{}
		if err := json.Unmarshal(out.Bytes(), &f); err != nil {
			t.Fatalf("parse final: %v", err)
		}
		if f.OK || f.Error.Code != "unsupported_protocol" {
			t.Errorf("final = %+v", f)
		}
	})

	t.Run("nothing on stdin", func(t *testing.T) {
		if exit := run(strings.NewReader(""), io.Discard, io.Discard); exit == 0 {
			t.Error("an empty stdin must not report success")
		}
	})
}
