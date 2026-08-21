package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
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

func outExec(stdout string) any { return outExecDur(stdout, 0) }

func outExecDur(stdout string, dur float64) any {
	return execValue{StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout)), DurationSeconds: dur}
}

// failExec is a non-zero exit whose words are on stdout — SQL*Plus and
// impdp print their ORA- lines there (measured).
func failExec(exit int, stdout string) any {
	return execValue{ExitCode: exit, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
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

// writeDump writes a stand-in artifact: the host vets nothing about the
// bytes, so random ones are as good as a dump for the simulated drill.
func writeDump(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "orders.dmp")
	buf := make([]byte, 8192)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	return p
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

// Fixtures in the shapes the verified image answered (measured).
const (
	fixtureIdentity = "version=23.26.3.0.0\npdb=FREEPDB1\naq_tm_processes=0\njob_queue_processes=0\n"
	fixtureHeader   = "filetype=1\nitem1=6.1\nitem15=23.06.00.00.00\nitem2=1\nitem7=49666\nitem25=0\n" +
		"item3=598E6E466AB80430E063030015ACEC99\nitem5=873\nitem4=1\nitem8=\"SYSTEM\".\"SYS_EXPORT_SCHEMA_01\"\n" +
		"item9=x86_64/Linux 2.4.xx\nitem10=012cb46a7cb7:FREE\nitem11=AL32UTF8\nitem6=Fri Aug 21 12:04:02 2026\n" +
		"item12=4096\nitem14=1\nitem18=0\nitem23=3\nitem26=0\nitem27=1\nitem19=0\nitem20=0\nitem21=0\nitem22=2\n" +
		"item16=1\nitem17=1\n"
	fixtureImport = `. . imported "PROBAVI_APP"."ORDERS"                        6.1 KB       5 rows
Job "SYS"."SYS_IMPORT_FULL_01" successfully completed at Fri Aug 21 12:05:55 2026 elapsed 0 00:00:19
`
)

// simulated answers per flow step; tests override single entries.
type simulated struct {
	tool     any
	start    any
	identity any
	header   any
	imp      any
}

func defaultSimulated() simulated {
	return simulated{
		tool:     okExec(),
		start:    execValue{ExitCode: 0, DurationSeconds: 12.3},
		identity: outExecDur(fixtureIdentity, 0.4),
		header:   outExecDur(fixtureHeader, 0.5),
		imp:      outExecDur(fixtureImport, 24.0),
	}
}

// provisionHandler simulates the idle sandbox through the whole flow,
// recording a label per call.
func provisionHandler(t *testing.T, sequence *[]string, sim simulated) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 8192, DurationSeconds: 0.25}, nil
		}
		argv := argvOf(t, call)
		label, value := classifyExec(argv, sim)
		if label == "" {
			t.Fatalf("unexpected exec: %v", argv)
		}
		*sequence = append(*sequence, label)
		return value, nil
	}
}

// classifyExec labels one exec call of the happy path by the script it
// runs and returns its simulated result; an empty label means the call
// was not expected.
func classifyExec(argv []string, sim simulated) (string, any) {
	if argv[0] != "bash" || len(argv) < 3 {
		return "", nil
	}
	switch argv[2] {
	case toolScript:
		return "tool", sim.tool
	case startScript:
		return "start", sim.start
	case identityScript:
		return "identity", sim.identity
	case headerScript:
		return "header", sim.header
	case importScript:
		return "import", sim.imp
	case healthScript:
		return "health", outExec("1\n")
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

// TestRunnerTemplateShape pins the declared dialect absorption: the SQL
// travels as one argv element into a bash script that runs it through
// SQL*Plus in the pluggable database the env names, and the env carries
// the client character set without which non-ASCII data renders as '?'
// (measured).
func TestRunnerTemplateShape(t *testing.T) {
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("sql_runner is not an object")
	}
	want := []string{"bash", "-c", runnerScript, "bash", "{{sql}}"}
	if got, ok := runner["argv"].([]string); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("sql_runner argv = %v, want %v", runner["argv"], want)
	}
	env, ok := runner["env"].(map[string]string)
	if !ok || env["ORACLE_PDB_SID"] != "{{database}}" || env["NLS_LANG"] != ".AL32UTF8" {
		t.Errorf("sql_runner env = %v, want the pluggable database placeholder and the client charset", runner["env"])
	}
	for _, fragment := range []string{"set markup csv on quote off", "define off", "whenever sqlerror exit 1",
		"nls_timestamp_tz_format='YYYY-MM-DD HH24:MI:SS.FF6TZH:TZM'"} {
		if !strings.Contains(runnerScript, fragment) {
			t.Errorf("runnerScript lacks %q", fragment)
		}
	}
}

// TestStartScriptPins pins the launch parameters the lifecycle rule and
// the zero-ingress claim rest on: each one lands in the parameter file
// before the instance exists, and the spfile is referenced, never
// rewritten.
func TestStartScriptPins(t *testing.T) {
	for _, fragment := range []string{`dispatchers=""`, "shared_servers=0", "job_queue_processes=0",
		"aq_tm_processes=0", "spfile=%s/dbs/spfile%s.ora", "startup pfile="} {
		if !strings.Contains(startScript, fragment) {
			t.Errorf("startScript lacks %q", fragment)
		}
	}
	for _, forbidden := range []string{"lsnrctl", "alter system set", "create spfile"} {
		if strings.Contains(startScript, forbidden) {
			t.Errorf("startScript must not %s", forbidden)
		}
	}
}

func indexOf(sequence []string, label string) int {
	for i, l := range sequence {
		if l == label {
			return i
		}
	}
	return -1
}

func decodeProvision(t *testing.T, f finalResponse) (database string, createdAt *string, timings map[string]float64) {
	t.Helper()
	got := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Host     string `json:"host"`
			Database string `json:"database"`
			User     string `json:"user"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			SizeBytes int64   `json:"size_bytes"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
		State   struct {
			PDB     string `json:"pdb"`
			WorkDir string `json:"work_dir"`
		} `json:"state"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.Connection.Scheme != "oracle" || got.Connection.Host != "127.0.0.1" || got.Connection.User != "sys" {
		t.Errorf("connection = %+v", got.Connection)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") || got.SourceIdentity.SizeBytes != 8192 {
		t.Errorf("source identity = %+v", got.SourceIdentity)
	}
	if got.State.PDB != got.Connection.Database || got.State.WorkDir != "/scratch/probavi-oracle" {
		t.Errorf("state = %+v, connection.database = %q", got.State, got.Connection.Database)
	}
	return got.Connection.Database, got.SourceIdentity.CreatedAt, got.Timings
}

func TestProvisionImportsTheDump(t *testing.T) {
	dump := writeDump(t)
	var sequence []string
	line, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "oracle_datapump", dump, map[string]string{"backup_timezone": "Europe/Budapest"}),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	want := []string{"tool", "start", "identity", "put_file", "header", "import"}
	if !reflect.DeepEqual(sequence, want) {
		t.Errorf("sequence = %v, want %v", sequence, want)
	}
	// The engine starts before a byte moves, and the pins are read back
	// before the artifact reaches the instance.
	if indexOf(sequence, "identity") > indexOf(sequence, "put_file") {
		t.Error("the pins must be verified before the dump is transferred")
	}
	assertPutFile(t, calls, dump)
	database, createdAt, timings := decodeProvision(t, f)
	if database != "FREEPDB1" {
		t.Errorf("connection.database = %q, want the pluggable database the instance named", database)
	}
	// The header's wall clock (Fri Aug 21 12:04:02 2026) in the declared
	// zone: Budapest is +02:00 in August.
	if createdAt == nil || *createdAt != "2026-08-21T12:04:02.000+02:00" {
		t.Errorf("created_at = %v, want the header's clock in the declared zone", createdAt)
	}
	for key, want := range map[string]float64{"engine_ready_seconds": 12.7, "transfer_seconds": 0.25, "restore_seconds": 24.5} {
		if got := timings[key]; math.Abs(got-want) > 1e-9 {
			t.Errorf("timings[%s] = %v, want %v (measured start+identity, transfer, header+import)", key, got, want)
		}
	}
}

// assertPutFile checks the one transfer: the artifact, under the file
// name Data Pump is told, owner-only.
func assertPutFile(t *testing.T, calls []verbCall, dump string) {
	t.Helper()
	for _, call := range calls {
		if call.Verb != "put_file" {
			continue
		}
		args := putFileArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
		if args.SourcePath != dump || args.DestPath != "/scratch/probavi-oracle/import.dmp" || args.Mode != "0600" {
			t.Errorf("put_file = %+v", args)
		}
	}
}

// TestCreatedAtNeedsTheDeclaredZone pins the rule that nothing is
// guessed: the header's clock carries no offset, so without
// backup_timezone there is no created_at.
func TestCreatedAtNeedsTheDeclaredZone(t *testing.T) {
	var sequence []string
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
		provisionHandler(t, &sequence, defaultSimulated()))
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final=%+v", f)
	}
	if _, createdAt, _ := decodeProvision(t, f); createdAt != nil {
		t.Errorf("created_at = %q without a declared zone, want null", *createdAt)
	}
}

// TestProvisionSurvivesTheConformanceSandbox is the §10 happy path: a
// random-bytes file, every exec answering exit 0 and stdout "1". Every
// gate must stay silent — each fires on positive evidence only — and
// the pluggable database falls back to the image's own name.
func TestProvisionSurvivesTheConformanceSandbox(t *testing.T) {
	line, _, exit := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{BytesCopied: 1, DurationSeconds: 0.001}, nil
			}
			return outExecDur("1\n", 0.001), nil
		})
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	database, createdAt, _ := decodeProvision(t, f)
	if database != defaultPDB || createdAt != nil {
		t.Errorf("database=%q created_at=%v", database, createdAt)
	}
}

func TestProvisionRefusals(t *testing.T) {
	dump := writeDump(t)
	tests := []struct {
		name     string
		payload  string
		wantCode string
		wantMsg  string
	}{
		{"unknown kind", provisionPayload(t, "rman", dump, nil), "unsupported_source", "oracle_datapump"},
		{"missing file", provisionPayload(t, "oracle_datapump", filepath.Join(t.TempDir(), "nope.dmp"), nil),
			"source_not_found", "does not exist"},
		{"directory", provisionPayload(t, "oracle_datapump", t.TempDir(), nil), "invalid_request", "one dump file"},
		{"unknown zone", provisionPayload(t, "oracle_datapump", dump, map[string]string{"backup_timezone": "Mars/Olympus"}),
			"invalid_request", "IANA time zone"},
		{"pitr", `{"source":{"kind":"oracle_datapump","path":"` + dump + `"},"sandbox":{"scratch_dir":"/scratch"},"pitr":{"target_time":"2026-01-01T00:00:00Z"}}`,
			"invalid_request", "pitr"},
		{"malformed", `[]`, "invalid_request", "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, calls, exit := driveOp(t, "provision", tt.payload, func(verbCall) (any, *protoError) {
				t.Fatal("a refused request must not touch the sandbox")
				return nil, nil
			})
			f := parseFinal(t, line)
			if exit != 0 || f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantMsg) || len(calls) != 0 {
				t.Errorf("exit=%d calls=%d final=%+v", exit, len(calls), f)
			}
		})
	}
	t.Run("empty file", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.dmp")
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", empty, nil), nil)
		if f := parseFinal(t, line); f.OK || f.Error.Code != "source_corrupt" {
			t.Errorf("final=%+v", f)
		}
	})
}

// TestStartFailuresCarryTheEnginesOwnWords maps every measured way the
// instance does not start to the code and the instruction the operator
// needs.
func TestStartFailuresCarryTheEnginesOwnWords(t *testing.T) {
	tests := []struct {
		name, stdout, wantCode, wantMsg string
	}{
		{"already running", "ORA-01081: cannot start already-running ORACLE - shut it down first",
			"invalid_request", "sleep infinity"},
		{"loopback only", "ORA-00600: internal error code, arguments: [ksipc: no private ips avail for use], [], []",
			"invalid_request", "docker network create --internal"},
		{"too little memory", "ORACLE instance started.\nDatabase mounted.\nORA-03113: end-of-file on communication channel",
			"invalid_request", "3 GiB"},
		{"anything else", "ORA-01078: failure in processing system parameters",
			"restore_failed", "ORA-01078"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := defaultSimulated()
			sim.start = failExec(1, tt.stdout)
			var sequence []string
			line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("final=%+v", f)
			}
			if indexOf(sequence, "put_file") >= 0 {
				t.Error("nothing may be transferred into an instance that did not start")
			}
		})
	}
}

// TestPinsNotTakenAreRefused: the pins are read back through the engine,
// and an instance that reports a scheduler or a queue time manager
// running is refused before the artifact reaches it.
func TestPinsNotTakenAreRefused(t *testing.T) {
	for _, pin := range []string{"job_queue_processes", "aq_tm_processes"} {
		t.Run(pin, func(t *testing.T) {
			sim := defaultSimulated()
			sim.identity = outExec(strings.ReplaceAll(fixtureIdentity, pin+"=0", pin+"=4"))
			var sequence []string
			line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, pin+"=4") {
				t.Errorf("final=%+v", f)
			}
			if indexOf(sequence, "put_file") >= 0 {
				t.Error("the dump must not be transferred into an instance whose pins did not take")
			}
		})
	}
}

func TestPluggableDatabaseChoice(t *testing.T) {
	tests := []struct {
		name, identity, wantCode, wantMsg string
	}{
		{"none open", "version=23.26.3.0.0\njob_queue_processes=0\naq_tm_processes=0\n", "restore_failed", "no pluggable database"},
		{"several open", "version=23.26.3.0.0\npdb=APP1\npdb=APP2\njob_queue_processes=0\naq_tm_processes=0\n",
			"invalid_request", "APP1, APP2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := defaultSimulated()
			sim.identity = outExec(tt.identity)
			var sequence []string
			line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("final=%+v", f)
			}
		})
	}
}

// TestHeaderVerdicts: what the dump's own header says decides before the
// import is attempted, on positive evidence only.
func TestHeaderVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		header   any
		wantCode string
		wantMsg  string
	}{
		{"unreadable file", failExec(1, "ORA-39211: unable to retrieve dumpfile information as specified"),
			"source_corrupt", "ORA-39211"},
		{"original export", outExec("filetype=2\n"), "unsupported_source", "original Export"},
		{"unknown type", outExec("filetype=0\n"), "source_corrupt", "file type 0"},
		{"encrypted data", outExec(strings.ReplaceAll(fixtureHeader, "item20=0", "item20=1")),
			"unsupported_source", "table data is encrypted"},
		{"newer origin", outExec(strings.ReplaceAll(fixtureHeader, "item15=23.06.00.00.00", "item15=26.01.00.00.00")),
			"invalid_request", "26.01.00.00.00, and the sandbox engine is 23.26.3.0.0"},
		{"other failure", failExec(1, "ORA-01031: insufficient privileges"), "restore_failed", "ORA-01031"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := defaultSimulated()
			sim.header = tt.header
			var sequence []string
			line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("final=%+v", f)
			}
			if indexOf(sequence, "import") >= 0 {
				t.Error("a refused header must stop the drill before the import")
			}
		})
	}
}

// TestImportVerdicts maps impdp's exit codes and the engine's lines
// (every one measured) to protocol codes.
func TestImportVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		imp      any
		wantCode string
		wantMsg  string
	}{
		{"completed with errors", failExec(5, "ORA-31684: Object type USER:\"PROBAVI_APP\" already exists\n"+
			"ORA-39151: Table \"PROBAVI_APP\".\"ORDERS\" exists.\nJob \"SYS\".\"SYS_IMPORT_FULL_01\" completed with 2 error(s)\n"),
			"restore_failed", "completed with 2 error lines"},
		{"watchdog", failExec(125, "ORA-39776: fatal Direct Path API error loading table \"SYS\".\"SYS_IMPORT_FULL_01\"\n"+
			"ORA-39376: invalid data encountered\n"), "source_corrupt", "never returned within 1m0s"},
		{"header checksum", failExec(1, "ORA-39001: invalid argument value\nORA-39000: bad dump file specification\n"+
			"ORA-39411: header checksum error in dump file \"/scratch/probavi-oracle/import.dmp\"\n"),
			"source_corrupt", "ORA-39001"},
		{"truncated", failExec(1, "ORA-39001: invalid argument value\nORA-39000: bad dump file specification\n"+
			"ORA-31640: unable to open dump file for read\nORA-27046: file size is not a multiple of logical block size\n"),
			"source_corrupt", "rejected the dump file"},
		{"newer origin", failExec(1, "ORA-39142: incompatible version number 26.1 in dump file"),
			"invalid_request", "version pairing"},
		{"anything else", failExec(1, "UDI-01017: operation generated ORACLE error 1017\nORA-01017: invalid credential"),
			"restore_failed", "impdp exited 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := defaultSimulated()
			sim.imp = tt.imp
			var sequence []string
			line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
				provisionHandler(t, &sequence, sim))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("final=%+v", f)
			}
		})
	}
}

// TestImportScriptWatchdog pins the shape of the one failure that never
// returns: the job's own state is what is polled, and the client is
// killed with the distinct exit the adapter classifies.
func TestImportScriptWatchdog(t *testing.T) {
	for _, fragment := range []string{"dba_datapump_jobs", "EXECUTING", "DEFINING", "exit 125",
		`impdp \"/ as sysdba\"`, "job_name=PROBAVI_IMPORT", "directory=PROBAVI_DUMP"} {
		if !strings.Contains(importScript, fragment) {
			t.Errorf("importScript lacks %q", fragment)
		}
	}
}

func TestSandboxPreconditions(t *testing.T) {
	sim := defaultSimulated()
	sim.tool = errExec(1, "")
	var sequence []string
	line, _, _ := driveOp(t, "provision", provisionPayload(t, "oracle_datapump", writeDump(t), nil),
		provisionHandler(t, &sequence, sim))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "not its lite variant") {
		t.Errorf("final=%+v", f)
	}
	if len(sequence) != 1 {
		t.Errorf("sequence = %v, want the toolchain check alone", sequence)
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
	t.Run("an answering pluggable database", func(t *testing.T) {
		var argv []string
		line, _, exit := driveOp(t, "healthcheck", `{"connection":{"database":"APP1"},"state":{}}`,
			func(call verbCall) (any, *protoError) {
				argv = argvOf(t, call)
				return outExecDur("1\n", 0.08), nil
			})
		f := parseFinal(t, line)
		if exit != 0 || !f.OK {
			t.Fatalf("exit=%d final=%+v", exit, f)
		}
		if len(argv) != 5 || argv[2] != healthScript || argv[4] != "APP1" {
			t.Errorf("argv = %v, want the health script against the connection's pluggable database", argv)
		}
		got := struct {
			Healthy bool    `json:"healthy"`
			Latency float64 `json:"latency_seconds"`
			Detail  string  `json:"detail"`
		}{}
		if err := json.Unmarshal(f.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if !got.Healthy || got.Latency != 0.08 || got.Detail != "accepting queries" {
			t.Errorf("payload = %+v", got)
		}
	})
	t.Run("an instance that is down is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"state":{}}`,
			func(verbCall) (any, *protoError) {
				return failExec(1, "ORA-01034: The Oracle instance is not available for use. Start the instance."), nil
			})
		f := parseFinal(t, line)
		if !f.OK {
			t.Fatalf("an unhealthy verdict must still be ok:true (§6.3): %+v", f)
		}
		got := struct {
			Healthy bool   `json:"healthy"`
			Detail  string `json:"detail"`
		}{}
		if err := json.Unmarshal(f.Payload, &got); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if got.Healthy || !strings.Contains(got.Detail, "ORA-01034") {
			t.Errorf("payload = %+v", got)
		}
	})
}

func TestVerdictLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"ORACLE instance started.\n\nORA-03113: end-of-file on communication channel\n", "ORA-03113: end-of-file on communication channel"},
		{"Connected.\nnothing wrong\n", "Connected."},
		{`select * from "NOPE"` + "\nERROR at line 1:\nORA-00942: table or view \"SYS\".\"NOPE\" does not exist\n",
			"ORA-00942: table or view 'SYS'.'NOPE' does not exist"},
	}
	for _, tt := range tests {
		if got := verdictLine([]byte(tt.in)); got != tt.want {
			t.Errorf("verdictLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
