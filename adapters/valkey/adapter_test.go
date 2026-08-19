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

// engineVersionOut is what an 8.0 server prints for --version (measured;
// 7.2 prints the same line without the leading engine name).
const engineVersionOut = "Valkey server v=8.0.10 sha=00000000:0 malloc=jemalloc-5.3.0 bits=64 build=abc"

// writeRDB writes an RDB-shaped fixture dated and versioned by its own
// header, in the pre-9 layout an 8.x server saves.
func writeRDB(t *testing.T, dir, name, valkeyVer string, ctime string) string {
	t.Helper()
	aux := [][2]string{}
	if valkeyVer != "" {
		aux = append(aux, [2]string{"valkey-ver", valkeyVer})
	}
	if ctime != "" {
		aux = append(aux, [2]string{"ctime", ctime})
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, rdbFixture(aux...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	return provisionHandlerCensus(t, sequence, healthyCensus)
}

// healthyCensus is what a restored server answers INFO with when the
// artifact's keys are all still alive — the real thing's field names and
// CRLF line endings (measured).
const healthyCensus = "# Persistence\r\nrdb_last_load_keys_expired:0\r\n" +
	"rdb_last_load_keys_loaded:500\r\n# Keyspace\r\ndb0:keys=500,expires=0,avg_ttl=0\r\n"

// provisionHandlerCensus is provisionHandler with the restored server's
// account of its own load under the test's control.
func provisionHandlerCensus(t *testing.T, sequence *[]string, census string) func(verbCall) (any, *protoError) {
	started := false
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return handlePut(t, call), nil
		}
		argv := argvOf(t, call)
		label, value := classifyExec(argv, &started, census)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argv)
		}
		if label == "ping" && !started {
			t.Error("readiness polled before the server was started")
		}
		*sequence = append(*sequence, label)
		return value, nil
	}
}

// handlePut asserts the transfer destination — the RDB's fixed path, or
// a member of the adapter's append-only directory — and answers it.
func handlePut(t *testing.T, call verbCall) any {
	t.Helper()
	args := putFileArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("put_file args: %v", err)
	}
	if args.DestPath != rdbInSandbox && !strings.HasPrefix(args.DestPath, aofDirInSandbox+"/") {
		t.Errorf("put_file dest = %q", args.DestPath)
	}
	return putFileValue{BytesCopied: 20, DurationSeconds: 0.4}
}

// classifyExec labels one exec call of the happy path and returns its
// simulated result; an empty label means the call was not expected.
func classifyExec(argv []string, started *bool, census string) (string, any) {
	switch {
	case argv[0] == "valkey-server" && argv[1] == "--version":
		return "version", outExec(engineVersionOut)
	case argv[0] == "mkdir":
		return "mkdir", okExec()
	case argv[0] == "valkey-check-rdb":
		return "check", okExec()
	case argv[0] == "valkey-check-aof":
		return "checkaof", okExec()
	case argv[0] == "valkey-server":
		*started = true
		return "start", execValue{ExitCode: 0, DurationSeconds: 0.2}
	case argv[0] == "valkey-cli" && argv[len(argv)-1] == "info":
		return "census", outExec(census)
	case argv[0] == "valkey-cli":
		return "ping", outExec("PONG\n")
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

// TestRunnerExpansionIsNotShellParsing pins the property the sql_runner
// template rests on: the check text is word-split, never re-parsed as
// shell syntax, so operators like ; | && $() in a check cannot become
// commands.
func TestRunnerExpansionIsNotShellParsing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	for _, hostile := range []string{
		"get k; touch " + marker,
		"get k && touch " + marker,
		"get $(touch " + marker + ")",
		"get `touch " + marker + "`",
		"get k | touch " + marker,
	} {
		// Run the template's argv with a stand-in for valkey-cli that
		// ignores its arguments: only the shell's treatment of $0 is
		// under test.
		script := "set -f; exec true $0"
		cmd := []string{"sh", "-c", script, hostile}
		if err := execCommand(cmd); err != nil {
			t.Fatalf("runner invocation failed for %q: %v", hostile, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("check text %q executed as shell — the runner must only word-split", hostile)
		}
	}
}

func TestProvisionRestoresRDB(t *testing.T) {
	rdb := writeRDB(t, t.TempDir(), "dump.rdb", "8.0.10", "1786289869")
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "valkey_rdb", rdb, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := "version|mkdir|put_file|check|start|ping|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}

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
	if got.Connection.Scheme != "redis" || got.Connection.Port != defaultPort {
		t.Errorf("connection = %+v", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	if got.SourceIdentity.CreatedAt == nil || *got.SourceIdentity.CreatedAt != "2026-08-09T15:37:49.000Z" {
		t.Errorf("created_at = %v, want the RDB's own save instant", got.SourceIdentity.CreatedAt)
	}
	if got.Timings["transfer_seconds"] != 0.4 {
		t.Errorf("timings = %+v, want the simulator's measured transfer", got.Timings)
	}
	if got.Timings["restore_seconds"] < 0 || got.Timings["engine_ready_seconds"] <= 0 {
		t.Errorf("timings = %+v, want measured non-negative phases", got.Timings)
	}
}

// TestProvisionWithoutMetadata pins the bonus-only nature of the header
// read: a file the parser cannot date or version still provisions, with
// created_at null and every pre-check silent.
func TestProvisionWithoutMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque.rdb")
	if err := os.WriteFile(path, []byte("not an rdb at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "valkey_rdb", path, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v — valkey-check-rdb in the sandbox is the authority, not the host parser", f)
	}
	got := struct {
		SourceIdentity struct {
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null — nothing dated this artifact", *got.SourceIdentity.CreatedAt)
	}
}

// TestProvisionRestoresAOF is the append-only half end to end at the
// protocol level: the manifest plus every member staged into the
// adapter's own append-only directory, vetted by valkey-check-aof, and
// the server started reading exactly that set — with created_at null,
// because an append-only directory does not date itself.
func TestProvisionRestoresAOF(t *testing.T) {
	dir := writeAOFDir(t, filepath.Join(t.TempDir(), "appendonlydir"), healthyManifest, healthyAOFFiles())
	var sequence []string
	var startArgv []string
	inner := provisionHandler(t, &sequence)
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "valkey_aof", dir, nil), func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if argv := argvOf(t, call); argv[0] == "valkey-server" && argv[1] != "--version" {
					startArgv = argv
				}
			}
			return inner(call)
		})
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	// The set is vetted member by member: the RDB base by
	// valkey-check-rdb (its manifest mode misreads a 9.x VALKEY-magic
	// base, measured), the incremental segment by valkey-check-aof.
	want := "version|mkdir|put_file|put_file|put_file|check|checkaof|start|ping|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	assertAOFStartArgv(t, startArgv)
	assertAOFProvisionPayload(t, f)
}

// TestProvisionAOFPlainTextBase pins the other base shape: a plain-text
// base (aof-use-rdb-preamble off) is RESP and goes to valkey-check-aof
// like the segments.
func TestProvisionAOFPlainTextBase(t *testing.T) {
	manifest := "file appendonly.aof.1.base.aof seq 1 type b\n" +
		"file appendonly.aof.1.incr.aof seq 1 type i\n"
	files := map[string]string{
		"appendonly.aof.1.base.aof": "*1\r\n$6\r\nSELECT\r\n",
		"appendonly.aof.1.incr.aof": "*1\r\n$4\r\nPING\r\n",
	}
	dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), manifest, files)
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "valkey_aof", dir, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := "version|mkdir|put_file|put_file|put_file|checkaof|checkaof|start|ping|census"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
}

// TestAOFBaseVerdict proves the base's own tool delivers the verdict on
// a damaged base, named as the member it is.
func TestAOFBaseVerdict(t *testing.T) {
	dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), healthyManifest, healthyAOFFiles())
	var sequence []string
	inner := provisionHandler(t, &sequence)
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "valkey_aof", dir, nil), func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if argv := argvOf(t, call); argv[0] == "valkey-check-rdb" {
					sequence = append(sequence, "check")
					return errExec(1, "RDB CRC error"), nil
				}
			}
			return inner(call)
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" ||
		!strings.Contains(f.Error.Message, "valkey-check-rdb rejected the append-only set member appendonly.aof.1.base.rdb") {
		t.Errorf("final = %+v, want source_corrupt naming the base and its tool", f)
	}
}

// assertAOFStartArgv pins the flags that make the restored server read
// the staged set instead of silently starting an empty one.
func assertAOFStartArgv(t *testing.T, startArgv []string) {
	t.Helper()
	joined := strings.Join(startArgv, " ")
	for _, flag := range []string{
		"--appendonly yes", "--appenddirname " + aofDirName, "--appendfilename appendonly.aof",
	} {
		if !strings.Contains(joined, flag) {
			t.Errorf("start argv %q missing %q", joined, flag)
		}
	}
	if strings.Contains(joined, "--appendonly no") || !strings.Contains(joined, "--save") {
		t.Errorf("start argv %q must keep AOF on and RDB saves off", joined)
	}
}

// assertAOFProvisionPayload pins what the final payload states about an
// append-only restore.
func assertAOFProvisionPayload(t *testing.T, f finalResponse) {
	t.Helper()
	got := struct {
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
		State   map[string]any     `json:"state"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	if got.SourceIdentity.CreatedAt != nil {
		t.Errorf("created_at = %v, want null — the base ctime dates the rewrite, not the backup",
			*got.SourceIdentity.CreatedAt)
	}
	if diff := got.Timings["transfer_seconds"] - 1.2; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("timings = %+v, want the three members' measured transfers summed", got.Timings)
	}
	if got.State["aof_dir"] != aofDirInSandbox {
		t.Errorf("state = %+v, want the staged append-only directory", got.State)
	}
}

// TestAOFVersionPrecheckThroughProvision proves the base RDB feeds the
// same asymmetric pre-check the rdb kinds have: a base written by a
// newer server than the sandbox runs is refused after the version
// probe, before anything is transferred.
func TestAOFVersionPrecheckThroughProvision(t *testing.T) {
	files := map[string]string{
		"appendonly.aof.1.base.rdb": string(rdbFixture([2]string{"valkey-ver", "99.9.0"})),
		"appendonly.aof.1.incr.aof": "*1\r\n$4\r\nPING\r\n",
	}
	dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), healthyManifest, files)
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "valkey_aof", dir, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "Valkey 99.9") {
		t.Fatalf("final = %+v, want invalid_request naming the origin version", f)
	}
	if got := strings.Join(sequence, "|"); got != "version" {
		t.Errorf("sequence = %s, want the version probe only — nothing may be transferred", got)
	}
}

// TestAOFCheckVerdict proves valkey-check-aof inside the sandbox stays
// the authority on loadability, exactly like its RDB sibling.
func TestAOFCheckVerdict(t *testing.T) {
	dir := writeAOFDir(t, filepath.Join(t.TempDir(), "aof"), healthyManifest, healthyAOFFiles())
	var sequence []string
	inner := provisionHandler(t, &sequence)
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "valkey_aof", dir, nil), func(call verbCall) (any, *protoError) {
			if call.Verb == "exec" {
				if argv := argvOf(t, call); argv[0] == "valkey-check-aof" {
					sequence = append(sequence, "checkaof")
					return errExec(1, "Bad file format reading the append only file appendonly.aof.1.incr.aof"), nil
				}
			}
			return inner(call)
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" ||
		!strings.Contains(f.Error.Message, "append-only set member appendonly.aof.1.incr.aof") {
		t.Errorf("final = %+v, want source_corrupt naming the failing segment", f)
	}
}

func TestProvisionRefusals(t *testing.T) {
	dir := t.TempDir()
	rdb := writeRDB(t, dir, "dump.rdb", "8.0.10", "")
	gz := filepath.Join(dir, "dump.rdb.gz")
	if err := os.WriteFile(gz, []byte{0x1f, 0x8b, 0x08, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "redis.rdb")
	if err := os.WriteFile(foreign, rdbFixtureMagic("REDIS0012",
		[2]string{"redis-ver", "7.4.2"}), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyAOF := filepath.Join(dir, "appendonly.aof")
	if err := os.WriteFile(legacyAOF, []byte("*1\r\n$4\r\nPING\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	incomplete := writeAOFDir(t, filepath.Join(dir, "incomplete"), healthyManifest,
		map[string]string{"appendonly.aof.1.base.rdb": "x"})
	redisAOF := writeAOFDir(t, filepath.Join(dir, "redis-aof"), healthyManifest, map[string]string{
		"appendonly.aof.1.base.rdb": string(rdbFixture([2]string{"redis-ver", "7.4.2"})),
		"appendonly.aof.1.incr.aof": "*1\r\n$4\r\nPING\r\n",
	})

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown source kind", provisionPayload(t, "valkey_backup", rdb, nil), "unsupported_source"},
		{"missing file", provisionPayload(t, "valkey_rdb", filepath.Join(dir, "gone.rdb"), nil), "source_not_found"},
		{"a directory for the file kind", provisionPayload(t, "valkey_rdb", dir, nil), "invalid_request"},
		{"gzip-compressed artifact named for what it is",
			provisionPayload(t, "valkey_rdb", gz, nil), "unsupported_source"},
		{"a Redis artifact is the other dialect's",
			provisionPayload(t, "valkey_rdb", foreign, nil), "unsupported_source"},
		{"backup_timezone has nothing to add to epoch seconds",
			provisionPayload(t, "valkey_rdb", rdb, map[string]string{"backup_timezone": "UTC"}), "invalid_request"},
		{"a single legacy AOF file for the directory kind",
			provisionPayload(t, "valkey_aof", legacyAOF, nil), "invalid_request"},
		{"an incomplete append-only copy is refused by the manifest",
			provisionPayload(t, "valkey_aof", incomplete, nil), "source_corrupt"},
		{"a Redis append-only set is the other dialect's",
			provisionPayload(t, "valkey_aof", redisAOF, nil), "unsupported_source"},
		{"malformed payload", `"not an object"`, "invalid_request"},
		{"pitr is not supported",
			`{"source":{"kind":"valkey_rdb","path":"` + rdb + `"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
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

// TestVersionPrecheckThroughProvision drives the refusal end to end: an
// RDB whose header names a newer server than the sandbox runs must be
// refused after the version probe, before anything is transferred.
func TestVersionPrecheckThroughProvision(t *testing.T) {
	rdb := writeRDB(t, t.TempDir(), "dump.rdb", "9.0.5", "")
	var sequence []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "valkey_rdb", rdb, nil), provisionHandler(t, &sequence))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request", f)
	}
	for _, want := range []string{"Valkey 9.0", "Valkey 8.0"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message %q missing %q", f.Error.Message, want)
		}
	}
	if got := strings.Join(sequence, "|"); got != "version" {
		t.Errorf("sequence = %s, want the version probe only — nothing may be transferred", got)
	}
}

func TestSandboxPreconditions(t *testing.T) {
	rdb := writeRDB(t, t.TempDir(), "dump.rdb", "", "")

	t.Run("an image without valkey-server is named", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "valkey_rdb", rdb, nil),
			func(call verbCall) (any, *protoError) {
				return errExec(127, "valkey-server: not found"), nil
			})
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "valkey-server") {
			t.Errorf("final = %+v, want invalid_request naming the missing binary", f)
		}
	})

	t.Run("an engine reporting itself as Redis is refused", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "valkey_rdb", rdb, nil),
			func(call verbCall) (any, *protoError) {
				return outExec("Redis server v=7.4.2 sha=00000000:0 malloc=jemalloc-5.3.0 bits=64 build=abc"), nil
			})
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "Redis") {
			t.Errorf("final = %+v, want invalid_request naming the wrong engine", f)
		}
	})
}

func TestRDBVerdicts(t *testing.T) {
	rdb := writeRDB(t, t.TempDir(), "dump.rdb", "", "")

	t.Run("a file valkey-check-rdb rejects is the backup's fault", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "valkey_rdb", rdb, nil),
			func(call verbCall) (any, *protoError) {
				if call.Verb == "put_file" {
					return putFileValue{}, nil
				}
				argv := argvOf(t, call)
				switch argv[0] {
				case "valkey-server":
					return outExec(engineVersionOut), nil
				case "mkdir":
					return okExec(), nil
				case "valkey-check-rdb":
					return execValue{ExitCode: 1, StdoutB64: base64.StdEncoding.EncodeToString(
						[]byte("[offset 9] Unexpected EOF reading RDB file\nRDB CRC error"))}, nil
				}
				t.Fatalf("unexpected exec: %v", argv)
				return nil, nil
			})
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "source_corrupt" || !strings.Contains(f.Error.Message, "RDB CRC error") {
			t.Errorf("final = %+v, want source_corrupt carrying the verdict line", f)
		}
	})
}

func TestLastErrorLine(t *testing.T) {
	log := []byte(`1:M 15 Aug 2026 12:00:00.000 * Ready to accept connections
1:M 15 Aug 2026 12:00:01.000 # Can't handle RDB format version 80
1:M 15 Aug 2026 12:00:01.000 # Fatal error loading the DB, check server logs. Exiting.`)
	got := lastErrorLine(log)
	if !strings.Contains(got, "Fatal error loading the DB") {
		t.Errorf("lastErrorLine = %q, want the final failure report", got)
	}
	if lastErrorLine([]byte("1:M * Ready to accept connections\n")) != "" {
		t.Error("a healthy log must yield nothing")
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
	t.Run("an answering server", func(t *testing.T) {
		line, _, exit := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return outExec("PONG\n"), nil })
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
	t.Run("an unanswering server is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) { return errExec(1, "Could not connect to Valkey"), nil })
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
			t.Error("healthy = true for a server that exited non-zero")
		}
	})
}
