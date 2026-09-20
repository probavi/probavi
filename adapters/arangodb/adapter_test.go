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
	restore  any
	count    any
	ready    any
	unpack   any
	database any
}

func defaultSimulated() simulated {
	return simulated{
		restore:  execValue{ExitCode: 0, DurationSeconds: 0.4},
		count:    execValue{ExitCode: 0, DurationSeconds: 0.02, StdoutB64: base64.StdEncoding.EncodeToString([]byte("2\n"))},
		ready:    execValue{ExitCode: 0, DurationSeconds: 0.01},
		unpack:   okExec(),
		database: outExec("shop\n"),
	}
}

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

// classifyExec labels one exec call of the happy path. The exact scripts
// are compared before any substring test: several of them share phrases,
// and a loose test would mislabel one and quietly change what a case
// exercises.
func classifyExec(argv []string, sim simulated) (string, any) {
	switch argv[0] {
	case "mkdir":
		return "mkdir", okExec()
	case "tar":
		return "unpack", sim.unpack
	case "arangorestore":
		return "restore", sim.restore
	case "arangosh":
		if strings.Contains(strings.Join(argv, " "), "_collections") {
			return "count", sim.count
		}
		return "ready", sim.ready
	case "tail":
		return "tail", outExec("")
	case "grep":
		// No fatal line in the log unless a test says otherwise.
		return "fatal?", execValue{ExitCode: 1}
	case "sh":
		return classifyShellExec(argv[2], sim)
	}
	return "", nil
}

func classifyShellExec(script string, sim simulated) (string, any) {
	if script == rootScript {
		return "locate", outExec("/scratch/probavi-arangodb/extract\n")
	}
	switch {
	case strings.Contains(script, "command -v arangod"):
		return "engine", okExec()
	case strings.HasPrefix(script, "cat > "):
		return "runner", okExec()
	case strings.Contains(script, "nohup"):
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.05}
	case strings.Contains(script, `"database"`):
		return "database", sim.database
	}
	return "", nil
}

func TestProbeGolden(t *testing.T) {
	line, calls, exit := driveOp(t, "probe", `{}`, func(verbCall) (any, *protoError) {
		t.Fatal("probe must not touch the sandbox")
		return nil, nil
	})
	if exit != 0 {
		t.Fatalf("probe exit = %d, want 0", exit)
	}
	if len(calls) != 0 {
		t.Fatalf("probe issued %d sandbox calls, want none", len(calls))
	}
	golden := filepath.Join("testdata", "probe_response.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(line)) {
		t.Errorf("probe response drifted from the golden file\n got: %s\nwant: %s", line, want)
	}
}

// TestTheRunnerKeepsTheStatementOutOfOptionValues pins the arrangement a
// measurement forced: this engine's option parser collapses `@@` into `@`
// in an option value, and an AQL collection bind is written `@@coll`. The
// statement must therefore reach the engine through the environment, and
// must still be its own argv element (§6.1).
func TestTheRunnerKeepsTheStatementOutOfOptionValues(t *testing.T) {
	payload, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probePayload did not return an object")
	}
	runner, ok := payload["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("probe declares no sql_runner")
	}
	argv, ok := runner["argv"].([]string)
	if !ok || len(argv) < 5 {
		t.Fatalf("sql_runner argv = %v, want a shell wrapper", runner["argv"])
	}
	if argv[len(argv)-1] != "{{sql}}" || argv[len(argv)-2] != "{{database}}" {
		t.Errorf("argv tail = %v, want the two templates last", argv[len(argv)-2:])
	}
	script := argv[2]
	if !strings.Contains(script, "export PROBAVI_AQL") {
		t.Error("the wrapper does not export the statement; it would become an option value")
	}
	if strings.Contains(script, "{{sql}}") {
		t.Error("the statement is interpolated into the script rather than passed as an argument")
	}
	// The JavaScript must read it back from the environment, and must
	// answer all three statements the core composes.
	for _, want := range []string{"env.PROBAVI_AQL", "SELECT count", "SELECT max", "_query(stmt)"} {
		if !strings.Contains(runnerJS, want) {
			t.Errorf("the runner script does not carry %q", want)
		}
	}
}

// dumpFixture writes an arangodump output directory.
type dumpFixture struct {
	database    string
	createdAt   string
	collections []string
	// encryption overrides the ENCRYPTION marker.
	encryption string
	// omitData and omitStructure drop one half of the first collection.
	omitData, omitStructure bool
	// omitManifest drops dump.json.
	omitManifest bool
}

func writeDump(t *testing.T, dir string, f dumpFixture) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	enc := f.encryption
	if enc == "" {
		enc = "none"
	}
	writeFile(t, filepath.Join(dir, encryptionName), enc)
	if !f.omitManifest {
		raw, err := json.Marshal(map[string]any{
			"database": f.database, "createdAt": f.createdAt, "useEnvelope": false,
		})
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		writeFile(t, filepath.Join(dir, manifestName), string(raw))
	}
	for i, name := range f.collections {
		hash := strings.Repeat(fmt.Sprintf("%x", i%16), 32)
		if !(f.omitStructure && i == 0) {
			writeFile(t, filepath.Join(dir, name+"_"+hash+".structure.json"),
				`{"parameters":{"name":"`+name+`"},"indexes":[]}`)
		}
		if !(f.omitData && i == 0) {
			writeFile(t, filepath.Join(dir, name+"_"+hash+".data.json.gz"), "gzipped-documents")
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// oneDump is the ordinary fixture: two collections taken at a known
// instant.
func oneDump() dumpFixture {
	return dumpFixture{
		database: "shop", createdAt: "2026-09-20T14:21:39Z",
		collections: []string{"orders", "meta"},
	}
}

func decodeProvision(t *testing.T, f finalResponse) (database string, createdAt *string, timings map[string]float64) {
	t.Helper()
	payload := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Database string `json:"database"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
	}{}
	if err := json.Unmarshal(f.Payload, &payload); err != nil {
		t.Fatalf("decode provision payload: %v", err)
	}
	if payload.Connection.Scheme != "arangodb" || payload.Connection.Port != defaultPort {
		t.Errorf("connection = %+v, want scheme arangodb on %d", payload.Connection, defaultPort)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q, want a sha256 of the artifact", payload.SourceIdentity.Checksum)
	}
	return payload.Connection.Database, payload.SourceIdentity.CreatedAt, payload.Timings
}

func TestProvisionRestoresADump(t *testing.T) {
	dir := t.TempDir()
	writeDump(t, dir, oneDump())

	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "arangodb_dump", dir, nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	if exit != 0 {
		t.Fatalf("provision exit = %d, want 0", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("provision failed: %+v", f.Error)
	}
	database, createdAt, timings := decodeProvision(t, f)
	if database != "shop" {
		t.Errorf("connection.database = %q, want the database the dump names", database)
	}
	if createdAt == nil || !strings.HasPrefix(*createdAt, "2026-09-20T14:21:39") {
		t.Errorf("created_at = %v, want the instant dump.json states", createdAt)
	}
	if timings["restore_seconds"] <= 0 {
		t.Errorf("timings = %v, want measured values", timings)
	}
	for _, want := range []string{"engine", "mkdir", "put_file", "runner", "start", "ready", "restore", "count"} {
		if !contains(sequence, want) {
			t.Errorf("the flow never did %q; it did %v", want, sequence)
		}
	}
	// The database came from the artifact, so the sandbox was never asked.
	if contains(sequence, "database") {
		t.Error("the flow asked the sandbox for a database name the dump already stated")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
