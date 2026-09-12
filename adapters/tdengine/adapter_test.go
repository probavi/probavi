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
	"time"
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
func step(t *testing.T, call verbCall) string {
	t.Helper()
	args := parseExec(t, call)
	script := ""
	if len(args.Argv) > 2 {
		script = args.Argv[2]
	}
	switch {
	case strings.Contains(script, "taosdump -i"):
		return "restore"
	case strings.Contains(script, "nohup taosd"):
		return "start"
	case strings.Contains(script, "tar -xf"):
		return "extract"
	case strings.Contains(script, "ins_tables"):
		return "tables"
	case strings.Contains(script, "ins_dnodes"):
		return "ready"
	default:
		return "unknown:" + script
	}
}

// dumpOptions shapes a fixture taosdump output.
type dumpOptions struct {
	// database is the name the schema creates; empty means "drill".
	database string
	// keepDays is the retention the schema declares; 0 leaves the clause
	// out, which is what a dump of a database without one looks like.
	keepDays int
	// startedAgo dates the dump's own record of itself.
	startedAgo time.Duration
	// rows is what its accounting claims it holds; -1 leaves the line out.
	rows int
	// nested puts the payload one level down, the way `taosdump -o DIR`
	// actually writes it.
	nested bool
	// noSchema writes a dbs.sql with only the header taosdump puts beside
	// the payload — the thing that makes the outer directory not the one
	// the restore tool can be pointed at.
	noSchema bool
}

// writeDump lays out a taosdump output the way the tool does.
func writeDump(t *testing.T, opts dumpOptions) string {
	t.Helper()
	if opts.database == "" {
		opts.database = "drill"
	}
	if opts.rows == 0 {
		opts.rows = 250
	}
	root := filepath.Join(t.TempDir(), "dump")
	payload := root
	if opts.nested {
		payload = filepath.Join(root, "taosdump.3578289329408")
	}
	header := "#!server_ver: ver:3.3.6.13\n#!taosdump_ver: 3.3.6.13\n#!escape_char: true\n"
	schema := header
	if !opts.noSchema {
		keep := ""
		if opts.keepDays > 0 {
			keep = fmt.Sprintf(" KEEP %dd,%dd,%dd", opts.keepDays, opts.keepDays, opts.keepDays)
		}
		schema += fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s REPLICA 1   DURATION 10d%s     PRECISION 'ms' ;\n\n",
			opts.database, keep)
		schema += "CREATE TABLE IF NOT EXISTS `" + opts.database + "`.`meters` (`ts` TIMESTAMP, `v` INT) TAGS (`loc` VARCHAR(32))\n"
	}
	files := map[string]string{
		filepath.Join(payload, "dbs.sql"):                    schema,
		filepath.Join(payload, "data0-13E2E57E", "x.avro"):   "avro bytes",
		filepath.Join(payload, "drill.13E2E57E.avro-tbtags"): "tag bytes",
	}
	if opts.nested {
		files[filepath.Join(root, "dbs.sql")] = header + "#!dumpdb: " + opts.database + ": \n"
	}
	result := "========== DUMP OUT ========== \n"
	if opts.startedAgo != 0 {
		at := time.Now().UTC().Add(-opts.startedAgo).Format("2006-01-02 15:04:05")
		result += "# DumpOut start time: " + at + "\n"
	}
	if opts.rows > 0 {
		result += fmt.Sprintf("# total row count:          %d\n", opts.rows)
	}
	files[filepath.Join(root, "dump_result.txt")] = result

	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func provisionPayload(path, kind string) string {
	if kind == "" {
		kind = "taosdump"
	}
	return fmt.Sprintf(`{"source":{"kind":%q,"path":%q},"sandbox":{"scratch_dir":"/tmp"},"options":{}}`, kind, path)
}

// restoredOK is what taosdump prints when it put everything back.
func restoredOK(rows int) any {
	return outExec(fmt.Sprintf("INFO: [0]: Restoring from drill.0.avro ...\nOK: %d row(s) dumped in!\nend time: 2026-09-11 16:38:34\n", rows))
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
				t.Errorf("put_file dest = %q, want the staging directory", args.DestPath)
			}
			return putFileValue{BytesCopied: 4096, DurationSeconds: 0.4}, nil
		}
		name := step(t, call)
		*seen = append(*seen, name)
		switch name {
		case "start":
			return outExec("started"), nil
		case "ready":
			return outExec("1"), nil
		case "extract":
			return outExec(stagingDir + "/unpacked/taosdump.1"), nil
		case "restore":
			return restoredOK(250), nil
		case "tables":
			return outExec("2"), nil
		default:
			return okExec(0), nil
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

// TestProvisionHappyPath pins the order the steps run in: the artifact is
// read and fenced host-side, the server the image starts is waited for,
// and only then does anything move.
func TestProvisionHappyPath(t *testing.T) {
	dump := writeDump(t, dumpOptions{nested: true, startedAgo: time.Hour, keepDays: 3650})
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(dump, ""), happyHandler(t, &seen))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	want := []string{"ready", "put_file", "restore", "tables"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", seen, want)
	}
	payload := struct {
		Connection struct {
			Port     int    `json:"port"`
			Database string `json:"database"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		State struct {
			Database string `json:"database"`
		} `json:"state"`
	}{}
	if err := json.Unmarshal(f.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Connection.Port != defaultPort || payload.Connection.Database != "drill" {
		t.Errorf("connection = %+v, want the engine's endpoint and the dump's own database", payload.Connection)
	}
	if payload.SourceIdentity.CreatedAt == nil {
		t.Error("created_at is nil — taosdump records when it started")
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q, want a tree hash", payload.SourceIdentity.Checksum)
	}
	if payload.State.Database != "drill" {
		t.Errorf("state.database = %q", payload.State.Database)
	}
}

// TestAnEngineThatIsAlreadyServingIsLeftAlone pins one half of the start:
// a sandbox whose engine answers is not restarted, because readiness is
// what the drill needs and who provided it does not matter.
func TestAnEngineThatIsAlreadyServingIsLeftAlone(t *testing.T) {
	dump := writeDump(t, dumpOptions{nested: true, startedAgo: time.Hour})
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(dump, ""), happyHandler(t, &seen))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	for _, s := range seen {
		if s == "start" {
			t.Errorf("the adapter restarted an engine that was already serving: %v", seen)
		}
	}
}

// TestAnEngineThatNeverCameUpIsStarted pins the other half, which is the
// normal path: the image's entrypoint did not leave a working engine, so
// after a short grace the adapter clears whatever is there and starts one
// itself. A process check would have stood back here — the entrypoint's
// own taosd can be alive and already dying (scripts.go).
func TestAnEngineThatNeverCameUpIsStarted(t *testing.T) {
	dump := writeDump(t, dumpOptions{nested: true, startedAgo: time.Hour})
	var seen []string
	handler := happyHandler(t, &seen)
	polls := 0
	line, _, exit := driveOp(t, "provision", provisionPayload(dump, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" && step(t, call) == "ready" {
				polls++
				if polls < 12 {
					// The engine answers, and no node is ready: what a
					// doomed taosd looks like from the outside.
					return outExec("0"), nil
				}
			}
			return handler(call)
		})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	started := false
	for _, s := range seen {
		started = started || s == "start"
	}
	if !started {
		t.Errorf("steps = %v, want the adapter to start the engine", seen)
	}
}

// TestOuterDirectoryResolvesToThePayload is the first of the three ways
// this engine's tool reports success having restored nothing: pointed at
// the directory `-o` was given, taosdump exits 0 and creates no database
// (measured). The adapter resolves the inner directory itself, so the
// drill may name either level.
func TestOuterDirectoryResolvesToThePayload(t *testing.T) {
	dump := writeDump(t, dumpOptions{nested: true, startedAgo: time.Hour})
	var seen []string
	handler := happyHandler(t, &seen)
	var restorePath string
	line, _, exit := driveOp(t, "provision", provisionPayload(dump, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				args := putFileArgs{}
				if err := json.Unmarshal(call.Args, &args); err != nil {
					t.Fatal(err)
				}
				restorePath = args.SourcePath
			}
			return handler(call)
		})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if filepath.Base(restorePath) != "taosdump.3578289329408" {
		t.Errorf("transferred %q, want the inner payload directory — the outer one restores nothing",
			restorePath)
	}
}

// TestProvisionRefusesBeforeTouchingTheSandbox covers every refusal the
// artifact alone can earn. A drill that cannot be honest should cost
// nothing.
func TestProvisionRefusesBeforeTouchingTheSandbox(t *testing.T) {
	tests := map[string]struct {
		path, kind, code, message string
	}{
		"an unknown source kind": {
			writeDump(t, dumpOptions{nested: true}), "taosdump_avro", "unsupported_source", "taosdump_dir",
		},
		"a path that does not exist": {
			filepath.Join(t.TempDir(), "nope"), "taosdump", "source_not_found", "does not exist",
		},
		"a directory that holds no dump": {
			writeDump(t, dumpOptions{nested: true, noSchema: true}), "taosdump",
			"source_corrupt", "no dbs.sql with a CREATE DATABASE line",
		},
		"a backup older than its own retention": {
			writeDump(t, dumpOptions{nested: true, keepDays: 3, startedAgo: 10 * 24 * time.Hour}), "taosdump",
			"restore_failed", "KEEP 3d",
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
				t.Errorf("code = %q, want %q (%s)", f.Error.Code, tc.code, f.Error.Message)
			}
			if !strings.Contains(f.Error.Message, tc.message) {
				t.Errorf("message = %q, want it to contain %q", f.Error.Message, tc.message)
			}
		})
	}
}

// TestRestoreVerdictReadsWhatTheToolSaid pins the second and third ways
// the tool reports success: a damaged file leaves a failure line beside
// the summary, and a server it never reached leaves retry lines and
// nothing else. Both exit 0 (measured), so the exit code is never the
// verdict here.
func TestRestoreVerdictReadsWhatTheToolSaid(t *testing.T) {
	tests := map[string]struct {
		output, code, message string
	}{
		"a damaged file": {
			"OK: 125 row(s) dumped in!\nERROR: 1 failures occurred to dump in!\n",
			"source_corrupt", "1 failure",
		},
		"a server it never reached": {
			"INFO: Retry to connect for 3 after sleep 1000ms ...\nend time: 2026-09-11 16:38:23\n",
			"restore_failed", "never reached the server",
		},
		"fewer rows than the backup records": {
			"OK: 200 row(s) dumped in!\n", "source_corrupt", "records 250 row(s) and the restore put back 200",
		},
		"nothing at all": {
			"INFO: [0]:100%\nend time: 2026-09-11 16:38:23\n", "restore_failed", "said nothing about restoring",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dump := writeDump(t, dumpOptions{nested: true, startedAgo: time.Hour, rows: 250})
			var seen []string
			handler := happyHandler(t, &seen)
			line, _, exit := driveOp(t, "provision", provisionPayload(dump, ""),
				func(call verbCall) (any, *protoError) {
					if call.Verb == "exec" {
						if step(t, call) == "restore" {
							return outExec(tc.output), nil
						}
					}
					return handler(call)
				})
			if exit != 0 {
				t.Fatalf("exit = %d", exit)
			}
			f := parseFinal(t, line)
			if f.OK || f.Error == nil || f.Error.Code != tc.code {
				t.Fatalf("final = %+v, want %s", f, tc.code)
			}
			if !strings.Contains(f.Error.Message, tc.message) {
				t.Errorf("message = %q, want it to contain %q", f.Error.Message, tc.message)
			}
		})
	}
}

// TestOutputThatIsNotTheToolsIsNotAVerdict pins the one hole in the
// restore verdict, deliberately: every real taosdump run prints its "end
// time:" line, the failures included, so output carrying none of the
// tool's markers is a sandbox that did not run it — the conformance
// suite's simulator answers every command with a stand-in — and judging
// it would refuse a drill on the strength of nothing. The engine-facing
// gate after it still has to find tables.
func TestOutputThatIsNotTheToolsIsNotAVerdict(t *testing.T) {
	dump := writeDump(t, dumpOptions{nested: true, startedAgo: time.Hour})
	var seen []string
	handler := happyHandler(t, &seen)
	line, _, exit := driveOp(t, "provision", provisionPayload(dump, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if step(t, call) == "restore" {
					return outExec("1\n"), nil
				}
			}
			return handler(call)
		})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v, want the engine-facing gate to decide", f)
	}
}

// TestWellFormedZeroIsRefused: a server that answers while serving none of
// the backup's tables has restored nothing, whatever the tool said.
func TestWellFormedZeroIsRefused(t *testing.T) {
	dump := writeDump(t, dumpOptions{nested: true, startedAgo: time.Hour})
	var seen []string
	handler := happyHandler(t, &seen)
	line, _, exit := driveOp(t, "provision", provisionPayload(dump, ""),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if step(t, call) == "tables" {
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
	if !strings.Contains(f.Error.Message, "serves no table") {
		t.Errorf("message = %q", f.Error.Message)
	}
}

func TestHealthcheck(t *testing.T) {
	for name, tc := range map[string]struct {
		answer  any
		healthy bool
	}{
		"a node the cluster calls ready": {outExec("1"), true},
		"the engine stopped":             {okExec(7), false},
		// The engine answers, and says no node is ready: the state the
		// restore would fail in, and the one a healthcheck must not call
		// healthy (measured: "Out of dnodes" while queries still answer).
		"no ready node": {outExec("0"), false},
	} {
		t.Run(name, func(t *testing.T) {
			line, _, exit := driveOp(t, "healthcheck", `{"state":{"database":"drill"}}`,
				func(call verbCall) (any, *protoError) {
					if step(t, call) != "ready" {
						t.Errorf("healthcheck ran %q, want the readiness the drill waited for", name)
					}
					return tc.answer, nil
				})
			if exit != 0 {
				t.Fatalf("exit = %d", exit)
			}
			f := parseFinal(t, line)
			if !f.OK {
				t.Fatalf("final = %+v", f)
			}
			got := struct {
				Healthy bool `json:"healthy"`
			}{}
			if err := json.Unmarshal(f.Payload, &got); err != nil {
				t.Fatal(err)
			}
			if got.Healthy != tc.healthy {
				t.Errorf("healthy = %v, want %v", got.Healthy, tc.healthy)
			}
		})
	}
}

// TestTeardownIsIdempotent covers §6.4: everything this adapter creates
// lives inside the sandbox, so teardown releases nothing and must say so
// twice with the same words.
func TestTeardownIsIdempotent(t *testing.T) {
	for i := range 2 {
		line, calls, exit := driveOp(t, "teardown", `{"state":{"database":"drill"}}`,
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
