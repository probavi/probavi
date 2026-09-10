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

func outExec(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout)), DurationSeconds: 0.1}
}

func parseExec(t *testing.T, call verbCall) execArgs {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if len(args.Argv) == 0 {
		t.Fatal("exec with empty argv")
	}
	return args
}

func okExec(exit int) any {
	return execValue{ExitCode: exit, DurationSeconds: 0.1, StdoutB64: "", StderrB64: ""}
}

// step names the provision step an exec call belongs to, by the script it
// carries. The adapter's steps are shell fragments, so the fragment is the
// only honest identifier — a step renamed in scripts.go renames itself
// here.
func step(t *testing.T, call verbCall) (string, execArgs) {
	t.Helper()
	args := parseExec(t, call)
	script := ""
	if len(args.Argv) > 2 {
		script = args.Argv[2]
	}
	switch {
	case strings.Contains(script, "echo 0 || echo 1"):
		return "idle", args
	case strings.Contains(script, "rm -rf"):
		return "place", args
	case strings.Contains(script, "nohup"):
		return "start", args
	case strings.Contains(script, "from tables()"):
		return "tables", args
	case strings.Contains(script, "grep -iE"):
		return "startup-error", args
	case strings.Contains(script, "curl -sf -o /dev/null"):
		return "ready", args
	default:
		return "unknown:" + script, args
	}
}

// dataRootOptions shapes a fixture data root.
type dataRootOptions struct {
	// checkpointed fills .checkpoint/db the way CHECKPOINT CREATE does.
	checkpointed bool
	// tables are user table directory names; the engine's own are added
	// regardless, because a real data root always carries them.
	tables []string
	// noDB leaves out the db directory, which is what makes a directory
	// something other than a QuestDB data root.
	noDB bool
}

// writeDataRoot lays out a data root the way the engine does.
func writeDataRoot(t *testing.T, opts dataRootOptions) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "questdb")
	files := map[string]string{
		"conf/server.conf":  "# probavi fixture\n",
		"public/index.html": "<html></html>",
	}
	if !opts.noDB {
		files["db/tables.d.0"] = "table registry"
		files["db/sys.text_import_log/_meta"] = "engine's own"
		files["db/telemetry_config/_meta"] = "engine's own"
		files["db/_query_trace/_meta"] = "engine's own"
		tables := opts.tables
		if tables == nil {
			tables = []string{"orders~9"}
		}
		for _, name := range tables {
			files["db/"+name+"/_meta"] = "meta"
			files["db/"+name+"/2026-09-01/id.d"] = "column bytes"
		}
	}
	if opts.checkpointed {
		files[".checkpoint/db/_checkpoint_meta.d"] = "\x00\x00\x00\x00"
		files[".checkpoint/db/tables.d.0"] = "registry copy"
	} else {
		// The directory survives CHECKPOINT RELEASE; its contents do not.
		if err := os.MkdirAll(filepath.Join(root, ".checkpoint"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func provisionPayload(path, kind string) string {
	if kind == "" {
		kind = "questdb_checkpoint"
	}
	return fmt.Sprintf(`{"source":{"kind":%q,"path":%q},"sandbox":{"scratch_dir":"/tmp"},"options":{}}`, kind, path)
}

// happyHandler answers every step the way a healthy sandbox does.
func happyHandler(t *testing.T, seen *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*seen = append(*seen, "put_file")
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.DestPath != stagingDir {
				t.Errorf("put_file dest = %q, want the staging directory outside the data root", args.DestPath)
			}
			return putFileValue{BytesCopied: 4096, DurationSeconds: 0.4}, nil
		}
		name, args := step(t, call)
		*seen = append(*seen, name)
		switch name {
		case "idle":
			return outExec("1"), nil
		case "place":
			wantArgs(t, name, args, dataRoot, stagingDir)
			return outExec("db"), nil
		case "start":
			return outExec("started"), nil
		case "ready":
			return okExec(0), nil
		case "tables":
			return outExec("1"), nil
		default:
			return okExec(0), nil
		}
	}
}

// wantArgs pins the positional parameters a step is given. bash -c
// <script> bash a b puts the first argument at argv[4].
func wantArgs(t *testing.T, name string, args execArgs, want ...string) {
	t.Helper()
	got := args.Argv[min(4, len(args.Argv)):]
	if len(got) != len(want) {
		t.Fatalf("%s: argv tail = %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: argv[%d] = %q, want %q", name, i+4, got[i], want[i])
		}
	}
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
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
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

// TestProvisionHappyPath pins the order the steps run in. The order is the
// design: the artifact is read and fenced host-side, the sandbox is proven
// idle before anything is written, and the engine starts only once the
// backup is its data root.
func TestProvisionHappyPath(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true})
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(root, ""), happyHandler(t, &seen))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	want := []string{"idle", "put_file", "place", "start", "ready", "tables"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", seen, want)
	}

	payload := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Port     int    `json:"port"`
			Database string `json:"database"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			SizeBytes int64   `json:"size_bytes"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings struct {
			EngineReady float64 `json:"engine_ready_seconds"`
			Transfer    float64 `json:"transfer_seconds"`
			Restore     float64 `json:"restore_seconds"`
		} `json:"timings"`
		State struct {
			DataRoot string `json:"data_root"`
		} `json:"state"`
	}{}
	if err := json.Unmarshal(f.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Connection.Port != defaultPort || payload.Connection.Scheme != "http" {
		t.Errorf("connection = %+v, want the engine's own HTTP endpoint", payload.Connection)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") || payload.SourceIdentity.SizeBytes == 0 {
		t.Errorf("source_identity = %+v, want a checksum over the tree and its size", payload.SourceIdentity)
	}
	if payload.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null — nothing in a QuestDB backup dates it", *payload.SourceIdentity.CreatedAt)
	}
	if payload.Timings.Transfer == 0 || payload.Timings.EngineReady < 0 {
		t.Errorf("timings = %+v, want measurements", payload.Timings)
	}
	if payload.State.DataRoot != dataRoot {
		t.Errorf("state.data_root = %q, want %q", payload.State.DataRoot, dataRoot)
	}
}

// TestProvisionRefusesBeforeTouchingTheSandbox covers every refusal a
// drill can earn from the artifact alone. A drill that cannot be honest
// should cost nothing.
func TestProvisionRefusesBeforeTouchingTheSandbox(t *testing.T) {
	tests := map[string]struct {
		path, kind, code, message string
	}{
		"an unknown source kind": {
			writeDataRoot(t, dataRootOptions{checkpointed: true}), "questdb_dump",
			"unsupported_source", "questdb_checkpoint",
		},
		"a path that does not exist": {
			filepath.Join(t.TempDir(), "nope"), "questdb_checkpoint",
			"source_not_found", "does not exist",
		},
		"a directory that is not a data root": {
			writeDataRoot(t, dataRootOptions{noDB: true, checkpointed: true}), "questdb_checkpoint",
			"source_corrupt", "no db directory",
		},
		"a copy taken with no checkpoint held": {
			writeDataRoot(t, dataRootOptions{}), "questdb_checkpoint",
			"invalid_request", "questdb_data",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "provision", provisionPayload(tc.path, tc.kind),
				func(verbCall) (any, *protoError) {
					t.Error("the sandbox was touched for a drill that cannot run")
					return okExec(0), nil
				})
			if exit != 0 {
				t.Fatalf("exit = %d", exit)
			}
			if len(calls) != 0 {
				t.Errorf("sandbox calls = %d, want 0", len(calls))
			}
			f := parseFinal(t, line)
			if f.OK || f.Error == nil {
				t.Fatalf("final = %+v, want a refusal", f)
			}
			if f.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", f.Error.Code, tc.code)
			}
			if !strings.Contains(f.Error.Message, tc.message) {
				t.Errorf("message = %q, want it to contain %q", f.Error.Message, tc.message)
			}
		})
	}
}

// TestProvisionAcceptsAnUncheckpointedCopyAsItsOwnKind is the other half
// of the fence: the copy is a legitimate artifact, it just makes no
// consistency claim, so it drills under a kind that claims nothing.
func TestProvisionAcceptsAnUncheckpointedCopyAsItsOwnKind(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{})
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(root, "questdb_data"), happyHandler(t, &seen))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v, want questdb_data to restore a copy with no checkpoint", f)
	}
}

// TestProvisionRefusesPITR keeps the probe and the behaviour honest: no
// source kind declares the capability, so a drill asking for it is told
// why rather than quietly restored to the wrong instant.
func TestProvisionRefusesPITR(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true})
	payload := fmt.Sprintf(
		`{"source":{"kind":"questdb_checkpoint","path":%q},"sandbox":{"scratch_dir":"/tmp"},`+
			`"pitr":{"target_time":"2026-09-01T00:00:00Z"}}`, root)
	line, calls, exit := driveOp(t, "provision", payload, func(verbCall) (any, *protoError) {
		t.Error("the sandbox was touched for a drill that cannot run")
		return okExec(0), nil
	})
	if exit != 0 || len(calls) != 0 {
		t.Fatalf("exit=%d calls=%d", exit, len(calls))
	}
	f := parseFinal(t, line)
	if f.OK || f.Error == nil || f.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request", f)
	}
}

// TestProvisionRefusesABusySandbox pins the idle requirement. Replacing
// the data root under a running server is what this refusal prevents, and
// it names the sandbox parameter that fixes the drill.
func TestProvisionRefusesABusySandbox(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true})
	var seen []string
	handler := happyHandler(t, &seen)
	line, _, exit := driveOp(t, "provision", provisionPayload(root, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if name, _ := step(t, call); name == "idle" {
					return outExec("0"), nil
				}
			}
			return handler(call)
		})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if f.OK || f.Error == nil || f.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request", f)
	}
	if !strings.Contains(f.Error.Message, "sleep infinity") {
		t.Errorf("message = %q, want it to name the sandbox parameter", f.Error.Message)
	}
	for _, s := range seen {
		if s == "put_file" || s == "place" {
			t.Errorf("the artifact was moved into a sandbox already serving: steps %v", seen)
		}
	}
}

// TestProvisionRefusesAWellFormedZero is the restore's verdict: a server
// that came up on a data root holding tables and serves none of them has
// restored nothing, and green is the one answer that must not be given.
func TestProvisionRefusesAWellFormedZero(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true, tables: []string{"orders~9", "trades~11"}})
	var seen []string
	handler := happyHandler(t, &seen)
	line, _, exit := driveOp(t, "provision", provisionPayload(root, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if name, _ := step(t, call); name == "tables" {
					return outExec("0"), nil
				}
			}
			return handler(call)
		})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if f.OK || f.Error == nil || f.Error.Code != "source_corrupt" {
		t.Fatalf("final = %+v, want source_corrupt", f)
	}
	if !strings.Contains(f.Error.Message, "2 tables") {
		t.Errorf("message = %q, want it to name what the backup held", f.Error.Message)
	}
}

// TestProvisionWaitsForAnEngineThatIsStillStarting pins the readiness
// wait. QuestDB answers about two seconds after start (measured), and a
// loaded host takes longer; returning at the first refused query would
// hand the drill's first check a server that is not up.
func TestProvisionWaitsForAnEngineThatIsStillStarting(t *testing.T) {
	root := writeDataRoot(t, dataRootOptions{checkpointed: true})
	var seen []string
	handler := happyHandler(t, &seen)
	polls := 0
	line, _, exit := driveOp(t, "provision", provisionPayload(root, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if name, _ := step(t, call); name == "ready" {
					polls++
					if polls < 3 {
						// curl against a port nothing listens on yet.
						return okExec(7), nil
					}
					return okExec(0), nil
				}
			}
			return handler(call)
		})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v, want a successful provision once the engine answers", f)
	}
	if polls != 3 {
		t.Errorf("readiness polls = %d, want 3 — the wait must outlast a slow start", polls)
	}
}

func TestHealthcheck(t *testing.T) {
	tests := map[string]struct {
		exit    int
		healthy bool
	}{
		"the engine answers": {0, true},
		"the engine stopped": {7, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			line, _, exit := driveOp(t, "healthcheck", `{"state":{"data_root":"/var/lib/questdb"}}`,
				func(call verbCall) (any, *protoError) {
					if name, _ := step(t, call); name != "ready" {
						t.Errorf("healthcheck ran %q, want the query the checks run", name)
					}
					return okExec(tc.exit), nil
				})
			if exit != 0 {
				t.Fatalf("exit = %d", exit)
			}
			f := parseFinal(t, line)
			if !f.OK {
				t.Fatalf("final = %+v", f)
			}
			got := struct {
				Healthy bool   `json:"healthy"`
				Detail  string `json:"detail"`
			}{}
			if err := json.Unmarshal(f.Payload, &got); err != nil {
				t.Fatal(err)
			}
			if got.Healthy != tc.healthy {
				t.Errorf("healthy = %v, want %v (detail %q)", got.Healthy, tc.healthy, got.Detail)
			}
		})
	}
}

// TestTeardownIsIdempotent covers §6.4: everything this adapter creates
// lives inside the sandbox, so teardown releases nothing and must say so
// twice with the same words.
func TestTeardownIsIdempotent(t *testing.T) {
	for i := range 2 {
		line, calls, exit := driveOp(t, "teardown", `{"state":{"data_root":"/var/lib/questdb"}}`,
			func(verbCall) (any, *protoError) {
				t.Error("teardown must not touch the sandbox")
				return okExec(0), nil
			})
		if exit != 0 || len(calls) != 0 {
			t.Fatalf("run %d: exit=%d calls=%d", i, exit, len(calls))
		}
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("run %d: final = %+v", i, f)
		}
	}
}
