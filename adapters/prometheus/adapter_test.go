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

// simulated answers per flow step; tests override single entries.
type simulated struct {
	census  any // the /metrics grep
	probe   any // the promtool read
	recover any // the meta.json recovery for opaque archives
	vetTar  any // the tar extraction
}

// timedOut is outExec with a measured duration, for the calls whose
// durations feed the reported restore phase.
func timedOut(stdout string, seconds float64) any {
	return execValue{DurationSeconds: seconds,
		StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

func defaultSimulated(blocks int) simulated {
	return simulated{
		census:  timedOut(fmt.Sprintf("prometheus_tsdb_blocks_loaded %d\n", blocks), 0.05),
		probe:   timedOut("{} => 680 @[1786876374]\n", 0.1),
		recover: outExec(""),
		vetTar:  okExec(),
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
		return "unpack", sim.vetTar
	case "wget":
		return "ready", okExec()
	case "promtool":
		return "probe", sim.probe
	}
	return "", nil
}

// classifyShellExec labels the shell one-liners by their scripts.
func classifyShellExec(script string, sim simulated) (string, any) {
	switch {
	case strings.Contains(script, "--version"):
		return "engine", okExec()
	case script == dataDirScript:
		return "locate", outExec("/scratch/probavi-prometheus/extract\n")
	case strings.Contains(script, "meta.json"):
		return "recover", sim.recover
	case strings.Contains(script, "printf"):
		return "config", okExec()
	case strings.Contains(script, "--config.file"):
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.1}
	case strings.Contains(script, "/metrics"):
		return "census", sim.census
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

// TestRunnerTemplateShape pins the property the check path rests on: no
// shell in the runner, the PromQL travelling as one argv element, and the
// evaluation instant delivered through {{database}} so checks read the
// backup's data instead of an empty now.
func TestRunnerTemplateShape(t *testing.T) {
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("sql_runner is not an object")
	}
	want := []string{"promtool", "query", "instant", "--time", "{{database}}", serverURL, "{{sql}}"}
	if got, ok := runner["argv"].([]string); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("sql_runner argv = %v, want %v", runner["argv"], want)
	}
}

func decodeProvision(t *testing.T, f finalResponse) (connection struct {
	Scheme   string `json:"scheme"`
	Port     int    `json:"port"`
	Database string `json:"database"`
}, createdAt *string, timings map[string]float64) {
	t.Helper()
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
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	return got.Connection, got.SourceIdentity.CreatedAt, got.Timings
}

func TestProvisionRestoresSnapshotDir(t *testing.T) {
	dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026-60000, maxAug2026)
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot", dir, nil),
		provisionHandler(t, &sequence, defaultSimulated(2)))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	// Two blocks of four files each transfer after one mkdir of the whole
	// skeleton; the server then starts and the two verdict reads run.
	want := "engine|mkdir|put_file|put_file|put_file|put_file|put_file|put_file|put_file|put_file|config|start|ready|census|probe"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	conn, createdAt, timings := decodeProvision(t, f)
	if conn.Scheme != "http" || conn.Port != 9090 || conn.Database != "1786876374" {
		t.Errorf("connection = %+v, want the backup's own instant in database", conn)
	}
	if createdAt == nil || *createdAt != "2026-08-16T10:32:54.046Z" {
		t.Errorf("created_at = %v, want the newest block's own claim", createdAt)
	}
	if timings["transfer_seconds"] != 4.0 || timings["engine_ready_seconds"] <= 0 ||
		timings["restore_seconds"] <= 0 {
		t.Errorf("timings = %+v, want measured phases", timings)
	}
}

func TestProvisionUnpacksArchive(t *testing.T) {
	path := buildTar(t, filepath.Join(t.TempDir(), "snap.tar.gz"), true,
		snapshotTarEntries("snapname", maxAug2026))
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot_tar", path, nil),
		provisionHandler(t, &sequence, defaultSimulated(1)))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	// The host walked the archive, so no in-sandbox census recovery runs.
	want := "engine|mkdir|put_file|unpack|locate|config|start|ready|census|probe"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	for _, call := range calls {
		if call.Verb != "put_file" {
			continue
		}
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
		if args.DestPath != "/scratch/probavi-prometheus/snapshot.tar" {
			t.Errorf("put_file dest = %q", args.DestPath)
		}
	}
	conn, createdAt, _ := decodeProvision(t, f)
	if conn.Database != "1786876374" || createdAt == nil {
		t.Errorf("connection = %+v created_at = %v, want the archive's own claim", conn, createdAt)
	}
}

func TestProvisionRecoversCensusFromOpaqueArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	sim := defaultSimulated(2)
	sim.recover = outExec(fmt.Sprintf(`{"ulid":"a","maxTime":%d}`+"\n"+`{"ulid":"b","maxTime":%d}`+"\n",
		maxAug2026-60000, maxAug2026))
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot_tar", path, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v — the sandbox extraction is the authority on an opaque archive", f)
	}
	want := "engine|mkdir|put_file|unpack|locate|recover|config|start|ready|census|probe"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	conn, createdAt, _ := decodeProvision(t, f)
	if conn.Database != "1786876374" || createdAt == nil || *createdAt != "2026-08-16T10:32:54.046Z" {
		t.Errorf("connection = %+v created_at = %v, want the recovered claim", conn, createdAt)
	}
}

// TestCensusRefusesPartialLoad is the fence the server's own forgiveness
// demands: it skips an unloadable block and stays up (measured), and the
// drill must never call that green.
func TestCensusRefusesPartialLoad(t *testing.T) {
	dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026-60000, maxAug2026)
	sim := defaultSimulated(2)
	sim.census = outExec("prometheus_tsdb_blocks_loaded 1\n")
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot", dir, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" ||
		!strings.Contains(f.Error.Message, "1 of the 2 blocks") {
		t.Errorf("final = %+v, want source_corrupt naming the partial load", f)
	}
}

// TestProbeRefusesAnEmptyServe pins the last verdict read: a well-formed
// zero at the instant the backup claims to cover means the promised data
// is not there.
func TestProbeRefusesAnEmptyServe(t *testing.T) {
	dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026)
	sim := defaultSimulated(1)
	sim.probe = outExec("{} => 0 @[1786876374]\n")
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot", dir, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "no series") {
		t.Errorf("final = %+v, want source_corrupt for the empty serve", f)
	}
}

// TestProbeSurfacesAReadFailure proves a failing read carries the
// engine's own words — a chunk checksum mismatch surfaces exactly here
// (measured).
func TestProbeSurfacesAReadFailure(t *testing.T) {
	dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026)
	sim := defaultSimulated(1)
	sim.probe = errExec(1, `query error: execution: cannot populate chunk 472 from block 00000000000000000000000000: checksum mismatch expected:ae2e9b12, actual:23d12c3e`)
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot", dir, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "checksum mismatch") {
		t.Errorf("final = %+v, want source_corrupt carrying the engine's words", f)
	}
}

func TestTarUnpackVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	sim := defaultSimulated(1)
	sim.vetTar = errExec(1, "tar: invalid magic")
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot_tar", path, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "unpack") {
		t.Errorf("final = %+v, want source_corrupt for an archive tar cannot read", f)
	}
}

func TestRecoverCensusRefusesNoMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	sim := defaultSimulated(1)
	sim.recover = errExec(1, "cat: can't open '/scratch/probavi-prometheus/extract/*/meta.json': No such file or directory")
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "prometheus_snapshot_tar", path, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "no block metadata") {
		t.Errorf("final = %+v, want source_corrupt for an archive holding no blocks", f)
	}
}

func TestProvisionRefusals(t *testing.T) {
	base := t.TempDir()
	snap := writeSnapshot(t, filepath.Join(base, "snap"), maxAug2026)
	liveDir := writeSnapshot(t, filepath.Join(base, "data"), maxAug2026)
	if err := os.Mkdir(filepath.Join(liveDir, "wal"), 0o755); err != nil {
		t.Fatal(err)
	}
	liveTar := buildTar(t, filepath.Join(base, "datadir.tar"), false,
		append(snapshotTarEntries("", maxAug2026), tarEntry{name: "wal/", dir: true},
			tarEntry{name: "wal/00000001", content: "segment"}))
	blockless := buildTar(t, filepath.Join(base, "other.tar"), false,
		[]tarEntry{{name: "README.md", content: "hello"}})

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "prometheus_backup", snap, nil), "unsupported_source"},
		{"missing archive", provisionPayload(t, "prometheus_snapshot_tar", filepath.Join(base, "gone.tar"), nil), "source_not_found"},
		{"a directory for the tar kind", provisionPayload(t, "prometheus_snapshot_tar", snap, nil), "invalid_request"},
		{"a file for the snapshot kind", provisionPayload(t, "prometheus_snapshot", blockless, nil), "invalid_request"},
		{"a raw data directory", provisionPayload(t, "prometheus_snapshot", liveDir, nil), "unsupported_source"},
		{"a tar of a raw data directory", provisionPayload(t, "prometheus_snapshot_tar", liveTar, nil), "unsupported_source"},
		{"a walkable archive without blocks", provisionPayload(t, "prometheus_snapshot_tar", blockless, nil), "source_corrupt"},
		{"backup_timezone has nothing to add to epoch milliseconds",
			provisionPayload(t, "prometheus_snapshot", snap, map[string]string{"backup_timezone": "UTC"}), "invalid_request"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"prometheus_snapshot","path":"` + snap + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
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
	dir := writeSnapshot(t, filepath.Join(t.TempDir(), "snap"), maxAug2026)
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "prometheus_snapshot", dir, nil),
		func(call verbCall) (any, *protoError) {
			return errExec(127, "sh: prometheus: not found"), nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "wrapper") {
		t.Errorf("final = %+v, want invalid_request pointing at the wrapper recipe", f)
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
	payload := `{"connection":{"database":"1786876374"},"state":{}}`

	t.Run("a serving restore", func(t *testing.T) {
		line, calls, exit := driveOp(t, "healthcheck", payload,
			func(verbCall) (any, *protoError) { return outExec("{} => 680 @[1786876374]\n"), nil })
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
		// The read must evaluate at the instant provision returned.
		argv := argvOf(t, calls[0])
		if argv[0] != "promtool" || !strings.Contains(strings.Join(argv, " "), "--time 1786876374") {
			t.Errorf("healthcheck argv = %v", argv)
		}
	})

	t.Run("a failing read is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", payload,
			func(verbCall) (any, *protoError) {
				return errExec(1, "query error: connection refused"), nil
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
			t.Error("healthy = true for a server that answered nothing")
		}
	})

	t.Run("a payload without the instant is refused", func(t *testing.T) {
		line, calls, _ := driveOp(t, "healthcheck", `{"connection":{},"state":{}}`, nil)
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || len(calls) != 0 {
			t.Errorf("final = %+v calls=%d, want invalid_request with no sandbox calls", f, len(calls))
		}
	})
}
