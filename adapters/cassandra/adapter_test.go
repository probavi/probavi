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
	schema   any
	load     any
	probe    any
	discover any
	unpack   any
}

func defaultSimulated() simulated {
	return simulated{
		schema:   execValue{ExitCode: 0, DurationSeconds: 0.1},
		load:     execValue{ExitCode: 0, DurationSeconds: 0.5},
		probe:    execValue{ExitCode: 0, DurationSeconds: 0.05},
		discover: outExec(""),
		unpack:   okExec(),
	}
}

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string, sim simulated) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.25}, nil
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
	case "bash":
		return classifyShellExec(argv[2], sim)
	case "mkdir":
		return "mkdir", okExec()
	case "tar":
		return "unpack", sim.unpack
	case "sstableloader":
		return "load", sim.load
	case "cqlsh":
		return classifyCqlshExec(argv, sim)
	}
	return "", nil
}

func classifyShellExec(script string, sim simulated) (string, any) {
	switch {
	case strings.Contains(script, "command -v cassandra"):
		return "engine", okExec()
	case strings.Contains(script, "/etc/hosts"):
		return "prepare", okExec()
	case script == rootScript:
		return "locate", outExec("/scratch/probavi-cassandra/extract\n")
	case strings.Contains(script, "ls -d"):
		return "discover", sim.discover
	case strings.Contains(script, "cassandra -R"):
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.2}
	}
	return "", nil
}

func classifyCqlshExec(argv []string, sim simulated) (string, any) {
	joined := strings.Join(argv, " ")
	switch {
	case strings.Contains(joined, "release_version"):
		return "ready", outExec(" release_version\n----\n 5.0.9\n")
	case strings.Contains(joined, "CREATE KEYSPACE"):
		return "keyspace", okExec()
	case argv[len(argv)-2] == "-f":
		return "schema", sim.schema
	case strings.Contains(joined, "LIMIT 1"):
		return "probe", sim.probe
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

// TestRunnerTemplateShape pins the declared dialect absorption: the CQL
// travels as one argv element into a bash pipeline whose awk filter turns
// cqlsh's decorated table into the undecorated tab-separated rows the
// protocol requires (measured shapes in runnerScript's comment).
func TestRunnerTemplateShape(t *testing.T) {
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("sql_runner is not an object")
	}
	want := []string{"bash", "-c", runnerScript, "bash", "{{database}}", "{{sql}}"}
	if got, ok := runner["argv"].([]string); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("sql_runner argv = %v, want %v", runner["argv"], want)
	}
}

// sequenceCounts tallies the labels of a recorded sequence.
func sequenceCounts(sequence []string) map[string]int {
	counts := map[string]int{}
	for _, label := range sequence {
		counts[label]++
	}
	return counts
}

func decodeProvision(t *testing.T, f finalResponse) (database string, createdAt *string, timings map[string]float64) {
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
	if got.Connection.Scheme != "cassandra" || got.Connection.Port != defaultPort {
		t.Errorf("connection = %+v", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	return got.Connection.Database, got.SourceIdentity.CreatedAt, got.Timings
}

func TestProvisionRestoresTree(t *testing.T) {
	root := writeTree(t, t.TempDir(), "probavi.orders", "probavi.meta")
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "cassandra_snapshot", root, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	counts := sequenceCounts(sequence)
	// Two tables of ten files each transfer after one mkdir of the whole
	// skeleton; one keyspace, then per table its own schema and stream,
	// then a first read of each.
	want := map[string]int{"engine": 1, "prepare": 1, "mkdir": 1, "put_file": 20,
		"start": 1, "ready": 1, "keyspace": 1, "schema": 2, "load": 2, "probe": 2}
	for label, n := range want {
		if counts[label] != n {
			t.Errorf("sequence has %d %s calls, want %d (sequence: %v)", counts[label], label, n, sequence)
		}
	}
	database, createdAt, timings := decodeProvision(t, f)
	if database != "probavi" {
		t.Errorf("database = %q, want the restored keyspace", database)
	}
	if createdAt == nil || *createdAt != fixtureCreatedAt {
		t.Errorf("created_at = %v, want the manifest's own instant", createdAt)
	}
	if timings["transfer_seconds"] != 5.0 || timings["engine_ready_seconds"] <= 0 ||
		timings["restore_seconds"] <= 0 {
		t.Errorf("timings = %+v, want measured phases", timings)
	}
}

func TestProvisionUnpacksArchive(t *testing.T) {
	tree := writeTree(t, t.TempDir(), "probavi.orders")
	path := treeToTar(t, tree, filepath.Join(t.TempDir(), "snap.tar.gz"), "snapname", true)
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "cassandra_snapshot_tar", path, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	counts := sequenceCounts(sequence)
	// The host walked the archive: one put of the tar, unpack and locate,
	// and no in-sandbox discovery.
	want := map[string]int{"put_file": 1, "unpack": 1, "locate": 1, "discover": 0,
		"schema": 1, "load": 1, "probe": 1}
	for label, n := range want {
		if counts[label] != n {
			t.Errorf("sequence has %d %s calls, want %d (sequence: %v)", counts[label], label, n, sequence)
		}
	}
	for _, call := range calls {
		if call.Verb != "put_file" {
			continue
		}
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
		if args.DestPath != "/scratch/probavi-cassandra/snapshot.tar" {
			t.Errorf("put_file dest = %q", args.DestPath)
		}
	}
	database, createdAt, _ := decodeProvision(t, f)
	if database != "probavi" || createdAt == nil {
		t.Errorf("database = %q created_at = %v, want the archive's own claims", database, createdAt)
	}
}

func TestProvisionDiscoversFromOpaqueArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	sim := defaultSimulated()
	sim.discover = outExec("probavi/meta/\nprobavi/orders/\n")
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "cassandra_snapshot_tar", path, nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v — the sandbox extraction is the authority on an opaque archive", f)
	}
	counts := sequenceCounts(sequence)
	want := map[string]int{"discover": 1, "keyspace": 1, "schema": 2, "load": 2, "probe": 2}
	for label, n := range want {
		if counts[label] != n {
			t.Errorf("sequence has %d %s calls, want %d (sequence: %v)", counts[label], label, n, sequence)
		}
	}
	database, createdAt, _ := decodeProvision(t, f)
	if database != "probavi" || createdAt != nil {
		t.Errorf("database = %q created_at = %v, want the discovered keyspace and no dating claim", database, createdAt)
	}
}

func TestProvisionRefusals(t *testing.T) {
	base := t.TempDir()
	tree := writeTree(t, filepath.Join(base, "snap"), "probavi.orders")
	liveTree := filepath.Join(base, "live")
	writeTable(t, liveTree, "probavi", "orders", tableFixture{liveMarker: "snapshots"})
	corruptTree := filepath.Join(base, "corrupt")
	writeTable(t, corruptTree, "probavi", "orders", tableFixture{corruptData: true})
	incompleteTree := filepath.Join(base, "incomplete")
	writeTable(t, incompleteTree, "probavi", "orders", tableFixture{dropComponent: "Index.db"})
	systemTree := filepath.Join(base, "system")
	writeTable(t, systemTree, "system_auth", "roles", tableFixture{})
	badNameTree := filepath.Join(base, "badname")
	writeTable(t, badNameTree, "probavi", "Orders", tableFixture{})

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "cassandra_backup", tree, nil), "unsupported_source"},
		{"missing archive", provisionPayload(t, "cassandra_snapshot_tar", filepath.Join(base, "gone.tar"), nil), "source_not_found"},
		{"a directory for the tar kind", provisionPayload(t, "cassandra_snapshot_tar", tree, nil), "invalid_request"},
		{"a raw data-directory copy", provisionPayload(t, "cassandra_snapshot", liveTree, nil), "unsupported_source"},
		{"a digest mismatch", provisionPayload(t, "cassandra_snapshot", corruptTree, nil), "source_corrupt"},
		{"a missing component the TOC lists", provisionPayload(t, "cassandra_snapshot", incompleteTree, nil), "source_corrupt"},
		{"a system keyspace", provisionPayload(t, "cassandra_snapshot", systemTree, nil), "invalid_request"},
		{"a name no unquoted identifier allows", provisionPayload(t, "cassandra_snapshot", badNameTree, nil), "invalid_request"},
		{"backup_timezone has nothing to add to a UTC manifest",
			provisionPayload(t, "cassandra_snapshot", tree, map[string]string{"backup_timezone": "UTC"}), "invalid_request"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"cassandra_snapshot","path":"` + tree + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
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
	root := writeTree(t, t.TempDir(), "probavi.orders")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "cassandra_snapshot", root, nil),
		func(call verbCall) (any, *protoError) {
			return errExec(127, "bash: cqlsh: command not found"), nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "toolchain") {
		t.Errorf("final = %+v, want invalid_request naming the missing toolchain", f)
	}
}

// overrideStep answers the happy path except one labelled step.
func overrideStep(t *testing.T, label string, value any) func(verbCall) (any, *protoError) {
	sim := defaultSimulated()
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		got, v := classifyExec(argvOf(t, call), sim)
		if got == "" {
			t.Fatalf("unexpected exec: %v", argvOf(t, call))
		}
		if got == label {
			return value, nil
		}
		return v, nil
	}
}

// TestSchemaFailureNamesBothSides proves the measured cross-version
// refusal: 5.0's table options do not parse on 4.1, and the drill states
// the pairing rather than a bare parse error.
func TestSchemaFailureNamesBothSides(t *testing.T) {
	root := writeTree(t, t.TempDir(), "probavi.orders")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "cassandra_snapshot", root, nil),
		overrideStep(t, "schema",
			errExec(2, "<stdin>:24:SyntaxException: Unknown property 'allow_auto_snapshot'")))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" ||
		!strings.Contains(f.Error.Message, "newer Cassandra") ||
		!strings.Contains(f.Error.Message, "allow_auto_snapshot") {
		t.Errorf("final = %+v, want invalid_request naming both sides", f)
	}
}

func TestLoaderFailureIsAFailedRestore(t *testing.T) {
	root := writeTree(t, t.TempDir(), "probavi.orders")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "cassandra_snapshot", root, nil),
		overrideStep(t, "load", errExec(1, "Error: Could not stream to any hosts")))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "sstableloader") {
		t.Errorf("final = %+v, want restore_failed carrying the loader's reason", f)
	}
}

// TestReadProbeSurfacesCorruption pins the last verdict read: the loader
// streams corrupted data without a word, and the damage surfaces at the
// first read (measured) — as a claim about the backup.
func TestReadProbeSurfacesCorruption(t *testing.T) {
	root := writeTree(t, t.TempDir(), "probavi.orders")
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "cassandra_snapshot", root, nil),
		overrideStep(t, "probe",
			errExec(2, "ReadFailure: Error from server: code=1300 [Replica(s) failed to execute read]")))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "ReadFailure") {
		t.Errorf("final = %+v, want source_corrupt carrying the engine's refusal", f)
	}
}

func TestUnpackAndDiscoveryVerdicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("an archive tar cannot read is the backup's fault", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "cassandra_snapshot_tar", path, nil),
			overrideStep(t, "unpack", errExec(2, "tar: This does not look like a tar archive")))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "unpack") {
			t.Errorf("final = %+v, want source_corrupt", f)
		}
	})

	t.Run("an unpacked tree without tables is not a snapshot", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "cassandra_snapshot_tar", path, nil),
			overrideStep(t, "discover", errExec(2, "ls: cannot access '*/*/': No such file or directory")))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "collected snapshot") {
			t.Errorf("final = %+v, want source_corrupt", f)
		}
	})

	t.Run("a discovered system keyspace is still refused", func(t *testing.T) {
		sim := defaultSimulated()
		sim.discover = outExec("system_auth/roles/\n")
		var sequence []string
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "cassandra_snapshot_tar", path, nil),
			provisionHandler(t, &sequence, sim))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "system_auth") {
			t.Errorf("final = %+v, want invalid_request — sandbox-derived names pass the same gate", f)
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
	t.Run("an answering node", func(t *testing.T) {
		line, _, exit := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return outExec(" release_version\n----\n 5.0.9\n"), nil })
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

	t.Run("an unanswering node is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) {
				return errExec(1, "Connection error: Could not connect to 127.0.0.1:9042"), nil
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
			t.Error("healthy = true for a node that answered nothing")
		}
	})
}

func TestLastErrorLine(t *testing.T) {
	log := []byte(`INFO  [main] 2026-08-16 Starting Messaging Service
ERROR [main] 2026-08-16 CassandraDaemon.java:887 - Unable to resolve hostname or get valid IP address`)
	got := lastErrorLine(log)
	if !strings.Contains(got, "Unable to resolve hostname") {
		t.Errorf("lastErrorLine = %q, want the node's own failure report", got)
	}
	if lastErrorLine([]byte("INFO ready\n")) != "" {
		t.Error("a healthy log must yield nothing")
	}
}
