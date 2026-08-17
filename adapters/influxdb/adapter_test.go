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

func errExec(exit int, stderr string) any {
	return execValue{ExitCode: exit, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

// engineVersionOut is what a 2.7 instance prints for `influxd version`
// (measured).
const engineVersionOut = "InfluxDB v2.7.12 (git: ec9dcde5d6) build_date: 2025-05-20T22:48:39Z"

func provisionPayload(t *testing.T, kind, path string, params, options map[string]string) string {
	t.Helper()
	req := map[string]any{
		"source":  map[string]any{"kind": kind, "path": path, "params": params},
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

// simulated answers per flow step; tests override single entries.
type simulated struct {
	version any // influxd version
	setup   any
	restore any
	census  any // influx bucket list --json
	locate  any // the manifest-directory lookup after an unpack
	recover any // the manifest read for an opaque archive
}

func defaultSimulated(bucketsJSON string) simulated {
	return simulated{
		version: outExec(engineVersionOut + "\n"),
		setup:   execValue{ExitCode: 0, DurationSeconds: 0.3},
		restore: execValue{ExitCode: 0, DurationSeconds: 1.5},
		census:  execValue{ExitCode: 0, DurationSeconds: 0.1, StdoutB64: base64.StdEncoding.EncodeToString([]byte(bucketsJSON))},
		locate:  outExec("/scratch/probavi-influxdb/extract\n"),
		recover: outExec(""),
	}
}

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string, sim simulated) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if !strings.HasPrefix(args.DestPath, "/scratch/probavi-influxdb/") {
				t.Errorf("put_file dest = %q", args.DestPath)
			}
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.2}, nil
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
	case "influxd":
		return "version", sim.version
	case "mkdir":
		return "mkdir", okExec()
	case "tar":
		return "untar", execValue{ExitCode: 0, DurationSeconds: 0.4}
	case "sh":
		switch {
		case strings.Contains(argv[2], "dirname"):
			return "locate", sim.locate
		case strings.Contains(argv[2], "cat"):
			return "recover", sim.recover
		}
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.1}
	case "influx":
		switch argv[1] {
		case "version":
			return "cli", outExec("Influx CLI dev\n")
		case "ping":
			return "ping", okExec()
		case "setup":
			return "setup", sim.setup
		case "restore":
			return "restore", sim.restore
		case "bucket":
			return "census", sim.census
		}
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

// restoredBucketsJSON is what `influx bucket list --json` answers for
// the fixture's organization after a full restore.
const restoredBucketsJSON = `[{"name":"metrics"},{"name":"events"}]`

// TestProvisionRestoresBackup pins the whole flow: preflight, transfer
// of every manifest member, engine start, sandbox setup, the restore
// with the sandbox token, and the bucket census — in that order — with
// the organization in the connection and the backup's own instant in
// created_at.
func TestProvisionRestoresBackup(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, singleOrg())
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "influx_backup", dir, nil, nil),
		provisionHandler(t, &sequence, defaultSimulated(restoredBucketsJSON)))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	// Five members: manifest, bolt, sqlite, two shards.
	want := "version|cli|mkdir|put_file|put_file|put_file|put_file|put_file|start|ping|setup|restore|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}

	assertDirectoryProvisionPayload(t, f)
}

// provisionWire mirrors the §6.2 response payload for assertions.
type provisionWire struct {
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
	State   map[string]string  `json:"state"`
}

func decodeProvision(t *testing.T, f finalResponse) provisionWire {
	t.Helper()
	got := provisionWire{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return got
}

// assertDirectoryProvisionPayload pins what the final payload states
// about a directory restore: the connection checks reach, the identity
// of the chosen set, and every phase as a measurement.
func assertDirectoryProvisionPayload(t *testing.T, f finalResponse) {
	t.Helper()
	got := decodeProvision(t, f)
	if got.Connection.Scheme != "http" || got.Connection.Port != 8086 || got.Connection.Database != "probavi-org" {
		t.Errorf("connection = %+v, want the backup's organization in database", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	if got.SourceIdentity.CreatedAt == nil || *got.SourceIdentity.CreatedAt != "2026-08-17T10:00:00.000Z" {
		t.Errorf("created_at = %v, want the stem's own instant", got.SourceIdentity.CreatedAt)
	}
	if diff := got.Timings["transfer_seconds"] - 1.0; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("transfer = %v, want the five members' measured transfers summed", got.Timings)
	}
	if diff := got.Timings["restore_seconds"] - 1.6; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("restore = %v, want restore plus census", got.Timings)
	}
	if got.State["org"] != "probavi-org" || got.State["backup_dir"] != "/scratch/probavi-influxdb/backup" {
		t.Errorf("state = %+v", got.State)
	}
}

// TestMultiOrgNeedsTheDatabaseOption pins the no-guessing rule: a
// backup holding several organizations restores whole either way, but
// which one the checks run against is the operator's call.
func TestMultiOrgNeedsTheDatabaseOption(t *testing.T) {
	orgs := map[string][]string{"alpha": {"a1"}, "beta": {"b1"}}
	dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, orgs)

	var sequence []string
	line, calls, _ := driveOp(t, "provision",
		provisionPayload(t, "influx_backup", dir, nil, nil),
		provisionHandler(t, &sequence, defaultSimulated(`[]`)))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" ||
		!strings.Contains(f.Error.Message, "alpha, beta") {
		t.Errorf("final = %+v, want invalid_request listing the organizations", f)
	}
	if len(calls) != 0 {
		t.Errorf("%d sandbox calls before the refusal", len(calls))
	}

	line, _, _ = driveOp(t, "provision",
		provisionPayload(t, "influx_backup", dir, nil, map[string]string{"database": "beta"}),
		provisionHandler(t, &sequence, defaultSimulated(`[{"name":"b1"}]`)))
	f = parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v, want the chosen organization to provision", f)
	}
	got := decodeProvision(t, f)
	if got.Connection.Database != "beta" {
		t.Errorf("database = %q, want the option's choice", got.Connection.Database)
	}
}

// TestCensusRefusesAPartialRestore is the fence the drill's verdict
// rests on: the restore created the organization, so a bucket missing
// from it can only mean part of the backup did not come back.
func TestCensusRefusesAPartialRestore(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, singleOrg())
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "influx_backup", dir, nil, nil),
		provisionHandler(t, &sequence, defaultSimulated(`[{"name":"metrics"}]`)))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" ||
		!strings.Contains(f.Error.Message, "1 of the 2 buckets") ||
		!strings.Contains(f.Error.Message, "events") {
		t.Errorf("final = %+v, want source_corrupt naming the missing bucket", f)
	}
}

// TestEngineFences pins the sandbox-side refusals: a 1.x image cannot
// read a 2.x backup, and an image without the binaries cannot drill.
func TestEngineFences(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, singleOrg())
	tests := []struct {
		name    string
		version any
		wantMsg string
	}{
		{"a 1.x engine is refused by name",
			outExec("InfluxDB 1.12.4 (git: unknown f9befe3459b689dc6dd1dd11e1e58e2943434ab1)\n"),
			"not the InfluxDB 2.x line"},
		{"an image without influxd is named",
			errExec(127, "influxd: not found"), "lacks influxd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := defaultSimulated(restoredBucketsJSON)
			sim.version = tt.version
			var sequence []string
			line, _, _ := driveOp(t, "provision",
				provisionPayload(t, "influx_backup", dir, nil, nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("final = %+v, want invalid_request containing %q", f, tt.wantMsg)
			}
		})
	}
}

// TestRestoreAndSetupFailures pins the error mapping of the two steps
// that talk to the live instance.
func TestRestoreAndSetupFailures(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, singleOrg())
	tests := []struct {
		name     string
		mutate   func(*simulated)
		wantCode string
		wantMsg  string
	}{
		{"a restore refusing the artifact is the backup's fault",
			func(s *simulated) {
				s.restore = errExec(1, "Error: failed to decode manifest: unexpected EOF")
			}, "source_corrupt", "rejected the backup"},
		{"any other restore failure carries the CLI's words",
			func(s *simulated) {
				s.restore = errExec(1, "Error: 503 Service Unavailable")
			}, "restore_failed", "503"},
		{"a failing setup is a failed restore",
			func(s *simulated) { s.setup = errExec(1, "Error: instance has already been set up") },
			"restore_failed", "initializing the sandbox instance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := defaultSimulated(restoredBucketsJSON)
			tt.mutate(&sim)
			var sequence []string
			line, _, _ := driveOp(t, "provision",
				provisionPayload(t, "influx_backup", dir, nil, nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("final = %+v, want %s containing %q", f, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// TestProvisionRefusals proves the host-side refusals are decided before
// any sandbox call.
func TestProvisionRefusals(t *testing.T) {
	base := t.TempDir()
	healthy := writeBackup(t, filepath.Join(base, "bak"), stemA, singleOrg())
	incomplete := writeBackup(t, filepath.Join(base, "incomplete"), stemA, singleOrg(), stemA+".1.tar.gz")
	portable := filepath.Join(base, "portable")
	if err := os.MkdirAll(portable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(portable, stemA+".manifest"),
		[]byte(`{"meta":{"fileName":"`+stemA+`.meta","size":66},"limited":false,"files":null}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "influx_snapshot", healthy, nil, nil), "unsupported_source"},
		{"missing source", provisionPayload(t, "influx_backup", filepath.Join(base, "gone"), nil, nil), "source_not_found"},
		{"an incomplete copy", provisionPayload(t, "influx_backup", incomplete, nil, nil), "source_corrupt"},
		{"a 1.x portable backup", provisionPayload(t, "influx_backup", portable, nil, nil), "unsupported_source"},
		{"backup_timezone has nothing to add",
			provisionPayload(t, "influx_backup", healthy, map[string]string{"backup_timezone": "UTC"}, nil),
			"invalid_request"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"influx_backup","path":"` + healthy + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
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
	t.Run("a serving restore", func(t *testing.T) {
		line, calls, exit := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return okExec(), nil })
		f := parseFinal(t, line)
		if exit != 0 || !f.OK || len(calls) != 1 {
			t.Fatalf("exit=%d final=%+v calls=%d", exit, f, len(calls))
		}
		got := struct {
			Healthy bool `json:"healthy"`
		}{}
		if err := json.Unmarshal(f.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !got.Healthy {
			t.Error("healthy = false for an answering instance")
		}
	})
	t.Run("a failing ping is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return errExec(1, "connection refused"), nil })
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
			t.Error("healthy = true for an instance that answered nothing")
		}
	})
}

// TestProvisionRestoresTarball pins the archive flow with a host-walkable
// tar: the organizations come from the archive's own table of contents,
// so no in-sandbox recovery runs, and the located directory is what the
// restore reads.
func TestProvisionRestoresTarball(t *testing.T) {
	path := buildTar(t, filepath.Join(t.TempDir(), "bak.tar"), false, backupTarEntries(""))
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "influx_backup_tar", path, nil, nil),
		provisionHandler(t, &sequence, defaultSimulated(restoredBucketsJSON)))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := "version|cli|mkdir|put_file|untar|locate|start|ping|setup|restore|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	got := decodeProvision(t, f)
	if got.Connection.Database != "probavi-org" || got.State["backup_dir"] != "/scratch/probavi-influxdb/extract" {
		t.Errorf("connection/state = %+v, want the walked org and the located directory", got)
	}
}

// TestProvisionRecoversOrgsFromOpaqueArchive pins the fallback: an
// archive the host cannot walk still drills — the manifest is recovered
// from the unpacked tree, and the census runs against what it states.
func TestProvisionRecoversOrgsFromOpaqueArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	sim := defaultSimulated(restoredBucketsJSON)
	sim.recover = outExec(manifestJSON(stemA, nil, singleOrg()))
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "influx_backup_tar", path, nil, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v — the sandbox extraction is the authority on an opaque archive", f)
	}
	want := "version|cli|mkdir|put_file|untar|locate|recover|start|ping|setup|restore|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	got := decodeProvision(t, f)
	if got.Connection.Database != "probavi-org" {
		t.Errorf("database = %q, want the recovered organization", got.Connection.Database)
	}
}

// TestOpaqueRecoveryFailureSkipsTheCensus pins the bonus-only nature of
// the recovery: content that is not one readable manifest leaves the
// organizations unknown, `influx restore` is the remaining authority,
// and the drill still reaches a verdict.
func TestOpaqueRecoveryFailureSkipsTheCensus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "influx_backup_tar", path, nil, nil),
		provisionHandler(t, &sequence, defaultSimulated(`[]`)))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	got := decodeProvision(t, f)
	if got.Connection.Database != "" {
		t.Errorf("database = %q, want empty when nothing stated the organization", got.Connection.Database)
	}
}

// TestTarredPortableBackupIsRefusedInSandbox pins the fence's second
// firing position: an opaque archive that unpacks into a 1.x portable
// backup is the same migration, and the recovered manifest is where the
// evidence appears.
func TestTarredPortableBackupIsRefusedInSandbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	sim := defaultSimulated(restoredBucketsJSON)
	sim.recover = outExec(`{"meta":{"fileName":"` + stemA + `.meta","size":66},"limited":false,"files":null}`)
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "influx_backup_tar", path, nil, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "unsupported_source" || !strings.Contains(f.Error.Message, "migration") {
		t.Errorf("final = %+v, want the migration fence from the recovered manifest", f)
	}
}
