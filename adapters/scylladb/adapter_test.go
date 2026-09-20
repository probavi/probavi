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
	stage    any
	refresh  any
	probe    any
	ttl      any
	discover any
	unpack   any
}

// populatedRead and emptyRead are cqlsh's own output for a table that
// holds a row and one that does not. The footer is what the drill reads
// the answer from.
const (
	populatedRead = " id | v\n----+---\n  1 | x\n\n(1 rows)\n"
	emptyRead     = " id | v\n----+---\n\n\n(0 rows)\n"
)

func defaultSimulated() simulated {
	return simulated{
		schema:  execValue{ExitCode: 0, DurationSeconds: 0.1},
		stage:   execValue{ExitCode: 0, DurationSeconds: 0.2, StdoutB64: base64.StdEncoding.EncodeToString([]byte("16\n"))},
		refresh: execValue{ExitCode: 0, DurationSeconds: 0.5},
		probe: execValue{ExitCode: 0, DurationSeconds: 0.05,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte(populatedRead))},
		ttl:      outExec("0\n"),
		discover: outExec(""),
		unpack:   okExec(),
	}
}

// provisionHandler simulates the sandbox through the whole flow,
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
	case "nodetool":
		return "refresh", sim.refresh
	case "cqlsh":
		return classifyCqlshExec(argv, sim)
	}
	return "", nil
}

// classifyShellExec identifies a bash step. The exact scripts are
// compared first and the substring tests come last, because several of
// these scripts share phrases: the TTL probe runs its own `command -v
// scylla` and its own `ls -d`, so a loose test would label it as the
// toolchain check or as discovery and quietly disarm the fence it exists
// to exercise.
func classifyShellExec(script string, sim simulated) (string, any) {
	switch script {
	case rootScript:
		return "locate", outExec("/scratch/probavi-scylladb/extract\n")
	case stageScript:
		return "stage", sim.stage
	case ttlProbeScript:
		return "ttl", sim.ttl
	case unpackScript:
		return "unpack", sim.unpack
	}
	switch {
	case strings.Contains(script, "command -v scylla >"):
		return "engine", okExec()
	case strings.Contains(script, "supervisorctl stop"):
		return "housekeeping", okExec()
	case strings.Contains(script, "ls -d"):
		return "discover", sim.discover
	}
	return "", nil
}

func classifyCqlshExec(argv []string, sim simulated) (string, any) {
	joined := strings.Join(argv, " ")
	switch {
	case strings.Contains(joined, "release_version"):
		return "ready", outExec(" release_version\n----\n 3.0.8\n")
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

// TestTheRunnerCarriesTheDialect pins the two things the declared runner
// must do: hand cqlsh the keyspace and the statement, and rewrite the
// core's table_exists probe, which CQL cannot answer as written.
func TestTheRunnerCarriesTheDialect(t *testing.T) {
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
		t.Fatalf("sql_runner argv = %v, want bash -c script bash {{database}} {{sql}}", runner["argv"])
	}
	if argv[len(argv)-2] != "{{database}}" || argv[len(argv)-1] != "{{sql}}" {
		t.Errorf("argv tail = %v, want the two templates last", argv[len(argv)-2:])
	}
	if !strings.Contains(argv[2], "DESCRIBE TABLE") {
		t.Error("the runner does not rewrite the core's table_exists probe, which CQL cannot answer")
	}
	if !strings.Contains(argv[2], "set -o pipefail") {
		t.Error("without pipefail the engine's exit code is lost through the filter")
	}
}

// snapshotTable writes one table directory of a collected snapshot: the
// schema, the manifest, and the sstable component files the manifest
// names.
type snapshotTable struct {
	keyspace, table string
	createdAt       int64
	sstables        int
	// omitData drops the Data.db of the first sstable, which is how a
	// half-copied snapshot looks on disk.
	omitData bool
	// omitManifest and omitSchema drop the file entirely.
	omitManifest, omitSchema bool
	// live adds the subdirectory only a live data directory has.
	live string
}

func writeSnapshot(t *testing.T, root string, tables ...snapshotTable) {
	t.Helper()
	for _, tb := range tables {
		dir := filepath.Join(root, tb.keyspace, tb.table)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if tb.live != "" {
			if err := os.MkdirAll(filepath.Join(dir, tb.live), 0o755); err != nil {
				t.Fatalf("mkdir live marker: %v", err)
			}
		}
		if !tb.omitSchema {
			writeFile(t, filepath.Join(dir, schemaName),
				"CREATE TABLE "+tb.keyspace+"."+tb.table+" (id int, PRIMARY KEY (id));\n")
		}
		type sst struct {
			TOCName  string `json:"toc_name"`
			DataSize int64  `json:"data_size"`
			TabletID *int   `json:"tablet_id"`
		}
		list := []sst{}
		for i := range tb.sstables {
			prefix := fmt.Sprintf("mt-3h3z_0t8d_%05d286uuzsfbc6pv-big", i)
			id := i
			list = append(list, sst{TOCName: prefix + tocSuffix, DataSize: 1024, TabletID: &id})
			writeFile(t, filepath.Join(dir, prefix+tocSuffix), "Data.db\nTOC.txt\n")
			if tb.omitData && i == 0 {
				continue
			}
			writeFile(t, filepath.Join(dir, prefix+"-Data.db"), fmt.Sprintf("data-%d", i))
		}
		if tb.omitManifest {
			continue
		}
		m := map[string]any{
			"snapshot": map[string]any{"name": "pvtest", "created_at": tb.createdAt},
			"table": map[string]any{
				"keyspace_name": tb.keyspace, "table_name": tb.table, "tablet_count": tb.sstables,
			},
			"sstables": list,
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		writeFile(t, filepath.Join(dir, manifestName), string(raw))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// oneTable is the ordinary fixture: a single two-sstable table taken at a
// known instant.
func oneTable() snapshotTable {
	return snapshotTable{keyspace: "shop", table: "orders", createdAt: 1789900285, sstables: 2}
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
	if payload.Connection.Scheme != "scylla" || payload.Connection.Port != defaultPort {
		t.Errorf("connection = %+v, want scheme scylla on %d", payload.Connection, defaultPort)
	}
	// The address is not 127.0.0.1: the image pins the node to 127.0.0.2
	// and refuses the other, and this field reaches the evidence record.
	if payload.Connection.Host != "127.0.0.2" {
		t.Errorf("connection.host = %q, want 127.0.0.2 — where the node actually serves",
			payload.Connection.Host)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q, want a sha256 of the artifact", payload.SourceIdentity.Checksum)
	}
	return payload.Connection.Database, payload.SourceIdentity.CreatedAt, payload.Timings
}

func TestProvisionRestoresTree(t *testing.T) {
	root := t.TempDir()
	writeSnapshot(t, root, oneTable())

	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "scylladb_snapshot", root, nil),
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
		t.Errorf("connection.database = %q, want the restored keyspace", database)
	}
	if createdAt == nil || !strings.HasPrefix(*createdAt, "2026-09-20T") {
		t.Errorf("created_at = %v, want the instant the manifest states", createdAt)
	}
	if timings["restore_seconds"] <= 0 || timings["engine_ready_seconds"] < 0 {
		t.Errorf("timings = %v, want measured values", timings)
	}
	for _, want := range []string{"engine", "ready", "housekeeping", "keyspace", "schema", "stage", "refresh", "probe"} {
		if !contains(sequence, want) {
			t.Errorf("the flow never did %q; it did %v", want, sequence)
		}
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
