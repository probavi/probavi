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

func execWith(exit int, stdout, stderr string) any {
	return execValue{
		ExitCode:  exit,
		StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout)),
		StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr)),
	}
}

// summaryLine is what asrestore prints when it finishes. Its skipped
// counter is not a case this adapter distinguishes: a skipped record is
// one an --ignore flag passed over, and the adapter passes no such flag.
func summaryLine(expired, errIgnored, inserted, failed int) string {
	return fmt.Sprintf(
		"2026-09-05 10:00:07 GMT [INF] [389] Expired %d : skipped 0 : err_ignored %d : inserted %d: failed %d (existed 0 , fresher 0)\n",
		expired, errIgnored, inserted, failed)
}

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
	return args.Argv
}

// stage names which step of the provision an exec call is, read from the
// program it runs rather than from its position, so a reordering does not
// silently answer the wrong call.
func stage(argv []string) string {
	if len(argv) == 0 {
		return "empty"
	}
	if argv[0] == "asrestore" {
		return "restore"
	}
	if len(argv) < 3 || argv[0] != "sh" || argv[1] != "-c" {
		return "unknown"
	}
	switch {
	case strings.Contains(argv[2], "no %s in the sandbox image"):
		return "preflight"
	case strings.HasPrefix(argv[2], `cat > "$1"`):
		return "write-config"
	case strings.Contains(argv[2], "asd --config-file"):
		return "start"
	case strings.Contains(argv[2], "sets/$ns"):
		return "readable"
	case strings.Contains(argv[2], "aql -h 127.0.0.1 -o json"):
		return "check"
	default:
		return "unknown"
	}
}

// provisionHandler answers a whole provision the way a healthy sandbox
// would, recording the stages in order.
func provisionHandler(t *testing.T, sequence *[]string, restoreValue any) func(verbCall) (any, *protoError) {
	t.Helper()
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 42, DurationSeconds: 0.01}, nil
		}
		s := stage(argvOf(t, call))
		*sequence = append(*sequence, s)
		switch s {
		case "restore":
			return restoreValue, nil
		case "readable":
			return execWith(0, "orders\n", ""), nil
		case "unknown", "empty":
			t.Fatalf("unrecognised exec: %v", argvOf(t, call))
		}
		return okExec(), nil
	}
}

// fixtureNamespace is the namespace every fixture backup names, and the
// one a restored drill therefore has to serve.
const fixtureNamespace = "orders"

// writeBackup lays out a backup directory holding one .asb file, and
// returns the directory.
func writeBackup(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "nightly")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if files == nil {
		files = map[string]string{"test_00000.asb": asbFile(fixtureNamespace, true, 3)}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// asbFile renders what asbackup writes: the version, the namespace, the
// first-file marker on exactly one of them, and records.
func asbFile(namespace string, first bool, records int) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "Version 3.1\n# namespace %s\n", namespace)
	if first {
		b.WriteString("# first-file\n")
	}
	for i := range records {
		fmt.Fprintf(b, "+ k S 2 k%d\n+ n %s\n+ s orders\n+ g 1\n+ t 0\n+ b 1\n- I id %d\n",
			i, namespace, i)
	}
	return b.String()
}

func TestProbeGolden(t *testing.T) {
	line, calls, exit := driveOp(t, "probe", "{}", func(verbCall) (any, *protoError) {
		t.Fatal("probe must issue no sandbox calls")
		return nil, nil
	})
	if exit != 0 || len(calls) != 0 {
		t.Fatalf("exit=%d calls=%d", exit, len(calls))
	}
	golden := filepath.Join("testdata", "probe_response.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -args -update once): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(line)) {
		t.Errorf("probe response changed:\n got %s\nwant %s", line, want)
	}
}

// TestRunnerTemplateShape holds the runner to conformance check 2: {{sql}}
// is its own argv element, and {{password}} appears nowhere — this engine
// has no authentication in the edition the adapter drives.
func TestRunnerTemplateShape(t *testing.T) {
	payload, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatalf("probe payload is %T, want an object", probePayload())
	}
	runner, ok := payload["sql_runner"].(map[string]any)
	if !ok {
		t.Fatalf("sql_runner is %T, want an object", payload["sql_runner"])
	}
	argv, ok := runner["argv"].([]string)
	if !ok {
		t.Fatalf("sql_runner.argv is %T, want a string list", runner["argv"])
	}
	sqlElements := 0
	for _, a := range argv {
		if a == "{{sql}}" {
			sqlElements++
		}
		if a != "{{sql}}" && strings.Contains(a, "{{sql}}") {
			t.Errorf("argv element %q embeds {{sql}} rather than being it", a)
		}
		if strings.Contains(a, "{{password}}") {
			t.Errorf("argv element %q carries {{password}}, which may appear only in env", a)
		}
	}
	if sqlElements != 1 {
		t.Errorf("argv carries {{sql}} %d times, want exactly once: %v", sqlElements, argv)
	}
}

func TestProvisionRestoresADirectory(t *testing.T) {
	dir := writeBackup(t, nil)
	var seq []string
	line, _, exit := driveOp(t, "provision",
		provisionPayload(t, "asbackup_dir", dir, nil, nil),
		provisionHandler(t, &seq, execWith(0, "", summaryLine(0, 0, 3, 0))))
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	final := parseFinal(t, line)
	if !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	want := []string{"preflight", "write-config", "start", "put_file", "restore", "readable"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", seq, want)
	}
	payload := struct {
		Connection struct {
			Scheme   string `json:"scheme"`
			Port     int    `json:"port"`
			Database string `json:"database"`
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		State map[string]string `json:"state"`
	}{}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Connection.Database != "orders" {
		t.Errorf("the namespace must come from the artifact: %+v", payload.Connection)
	}
	if payload.Connection.Scheme != "aerospike" || payload.Connection.Port != defaultPort {
		t.Errorf("connection = %+v", payload.Connection)
	}
	if payload.SourceIdentity.CreatedAt != nil {
		t.Errorf("no .asb header carries a clock; created_at = %v", *payload.SourceIdentity.CreatedAt)
	}
	if !strings.HasPrefix(payload.SourceIdentity.Checksum, "sha256:") {
		t.Errorf("checksum = %q", payload.SourceIdentity.Checksum)
	}
}

// TestTheGeneratedConfigurationIsWhatTheEngineNeeds pins every line of the
// configuration the adapter writes, because each one was measured against
// an engine that would not start or would not restore without it.
func TestTheGeneratedConfigurationIsWhatTheEngineNeeds(t *testing.T) {
	dir := writeBackup(t, nil)
	var seq []string
	_, calls, _ := driveOp(t, "provision",
		provisionPayload(t, "asbackup_dir", dir, nil, nil),
		provisionHandler(t, &seq, execWith(0, "", summaryLine(0, 0, 3, 0))))
	conf := configWritten(t, calls)

	// The namespace has to be the artifact's, or asrestore writes into one
	// nobody asked for and the batch fails.
	if !strings.Contains(conf, "namespace "+fixtureNamespace+" {") {
		t.Errorf("generated configuration does not serve the artifact's namespace:\n%s", conf)
	}
	// Required under --network none: there is no MAC to derive a node id
	// from, and fabric and heartbeat find no routable address.
	for _, want := range []string{"node-id", "proto-fd-max 1024", "address 127.0.0.1"} {
		if !strings.Contains(conf, want) {
			t.Errorf("generated configuration lacks %q", want)
		}
	}
	// Issue #166: the engine removes nothing for the drill's duration, and
	// the switch beside it is what lets a backup carrying a time to live be
	// restored at all — without it the server refuses every such write once
	// its reaper is off (measured).
	for _, want := range []string{"nsup-period 0", "allow-ttl-without-nsup true"} {
		if !strings.Contains(conf, want) {
			t.Errorf("generated configuration lacks %q", want)
		}
	}
	if strings.Contains(conf, "admin {") || strings.Contains(conf, "info {") {
		t.Error("the configuration must carry neither stanza: 7.2 and 8.1 disagree about its name")
	}
}

// configWritten returns the configuration body the adapter sent to the
// sandbox on stdin.
func configWritten(t *testing.T, calls []verbCall) string {
	t.Helper()
	for _, call := range calls {
		if call.Verb != "exec" {
			continue
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatal(err)
		}
		if stage(args.Argv) != "write-config" {
			continue
		}
		body, err := base64.StdEncoding.DecodeString(args.StdinB64)
		if err != nil {
			t.Fatalf("decode configuration: %v", err)
		}
		return string(body)
	}
	t.Fatal("the adapter never wrote a configuration")
	return ""
}

func TestProvisionRestoresASingleFile(t *testing.T) {
	dir := writeBackup(t, nil)
	file := filepath.Join(dir, "test_00000.asb")
	var seq []string
	line, calls, _ := driveOp(t, "provision",
		provisionPayload(t, "asbackup", file, nil, nil),
		provisionHandler(t, &seq, execWith(0, "", summaryLine(0, 0, 3, 0))))
	if final := parseFinal(t, line); !final.OK {
		t.Fatalf("provision failed: %+v", final.Error)
	}
	// A single file is restored with -i; a directory with -d. Restoring a
	// file with -d makes asrestore read the directory holding it, which is
	// a different backup than the drill named.
	for _, call := range calls {
		argv := argvOf(t, call)
		if stage(argv) == "restore" {
			if len(argv) < 5 || argv[3] != "-i" {
				t.Errorf("restore argv = %v, want -i for a single file", argv)
			}
		}
	}
}

// TestTheExpiryFence is the issue #166 verdict for this engine. A record's
// expiry is an absolute instant inside the artifact, and asrestore drops
// every record whose instant has passed while exiting 0.
func TestTheExpiryFence(t *testing.T) {
	dir := writeBackup(t, nil)
	var seq []string
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "asbackup_dir", dir, nil, nil),
		provisionHandler(t, &seq, execWith(0, "", summaryLine(3, 0, 0, 0))))
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("a restore that dropped every expired record must not be reported green")
	}
	if final.Error.Code != "restore_failed" {
		t.Errorf("code = %s, want restore_failed", final.Error.Code)
	}
	if !strings.Contains(final.Error.Message, "expired") {
		t.Errorf("the refusal must say why: %s", final.Error.Message)
	}
	// The readable gate must never be reached: the drill is refused on the
	// counter, before anything is read back.
	if strings.Contains(strings.Join(seq, ","), "readable") {
		t.Errorf("stages = %v, want the refusal before the readback", seq)
	}
}

func TestRestoreVerdicts(t *testing.T) {
	tests := map[string]struct {
		restore  any
		code     string
		contains string
	}{
		"a well-formed zero": {
			execWith(0, "", summaryLine(0, 0, 0, 0)), "source_corrupt", "inserted no records"},
		"records the engine refused": {
			execWith(0, "", summaryLine(0, 2, 5, 0)), "restore_failed", "did not write every record"},
		"records that failed": {
			execWith(0, "", summaryLine(0, 0, 5, 1)), "restore_failed", "did not write every record"},
		"a summary that never came": {
			execWith(0, "", "no counters here\n"), "restore_failed", "without reporting what it restored"},
		"asrestore refusing the artifact": {
			execWith(1, "", "2026-09-05 [ERR] [395] Unexpected end of file in backup block (line 417, col 12)\n"),
			"source_corrupt", "Unexpected end of file"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := writeBackup(t, nil)
			var seq []string
			line, _, _ := driveOp(t, "provision",
				provisionPayload(t, "asbackup_dir", dir, nil, nil),
				provisionHandler(t, &seq, tt.restore))
			final := parseFinal(t, line)
			if final.OK {
				t.Fatal("want a refusal")
			}
			if final.Error.Code != tt.code {
				t.Errorf("code = %s, want %s (%s)", final.Error.Code, tt.code, final.Error.Message)
			}
			if !strings.Contains(final.Error.Message, tt.contains) {
				t.Errorf("message = %q, want it to contain %q", final.Error.Message, tt.contains)
			}
		})
	}
}

// TestAnUnreadableRestoreIsRefused covers the divergence measured on this
// engine: a namespace can report objects that no reader can see, because
// expiry is applied when a record is read.
func TestAnUnreadableRestoreIsRefused(t *testing.T) {
	dir := writeBackup(t, nil)
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "asbackup_dir", dir, nil, nil),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			switch stage(argvOf(t, call)) {
			case "restore":
				return execWith(0, "", summaryLine(0, 0, 3, 0)), nil
			case "readable":
				return execWith(1, "", "no set in the restored namespace returns a record to a reader\n"), nil
			}
			return okExec(), nil
		})
	final := parseFinal(t, line)
	if final.OK {
		t.Fatal("a restore nothing can read must not be reported green")
	}
	if final.Error.Code != "restore_failed" {
		t.Errorf("code = %s, want restore_failed", final.Error.Code)
	}
	if !strings.Contains(final.Error.Message, "expiry") {
		t.Errorf("the refusal must name the cause: %s", final.Error.Message)
	}
}

func TestProvisionRefusals(t *testing.T) {
	good := writeBackup(t, nil)
	fragment := writeBackup(t, map[string]string{
		"test_00001.asb": asbFile("orders", false, 2),
	})
	mixed := writeBackup(t, map[string]string{
		"a_00000.asb": asbFile("orders", true, 1),
		"b_00000.asb": asbFile("customers", false, 1),
	})
	twoFirsts := writeBackup(t, map[string]string{
		"a_00000.asb": asbFile("orders", true, 1),
		"b_00000.asb": asbFile("orders", true, 1),
	})
	notABackup := writeBackup(t, map[string]string{"notes.txt": "hello\n"})
	gzipped := writeBackup(t, map[string]string{
		"test_00000.asb": "\x1f\x8b\x08 and then compressed bytes",
	})

	tests := map[string]struct{ kind, path, code, contains string }{
		"a kind the probe never declared": {
			"aerospike_rmw", good, "unsupported_source", "supported: asbackup"},
		"a source that does not exist": {
			"asbackup_dir", filepath.Join(t.TempDir(), "gone"), "source_not_found", "does not exist"},
		"a directory given to the file kind": {
			"asbackup", good, "invalid_request", "asbackup_dir"},
		"a file given to the directory kind": {
			"asbackup_dir", filepath.Join(good, "test_00000.asb"), "invalid_request", "asbackup"},
		"one part of a split backup": {
			"asbackup", filepath.Join(fragment, "test_00001.asb"), "source_corrupt", "first-file"},
		"a directory holding only the tail": {
			"asbackup_dir", fragment, "source_corrupt", "wrote first"},
		"two backups in one directory": {
			"asbackup_dir", mixed, "source_corrupt", "two backups"},
		"two first files": {
			"asbackup_dir", twoFirsts, "source_corrupt", "more than one backup"},
		"a directory of something else": {
			"asbackup_dir", notABackup, "source_not_found", "no asbackup files"},
		"a compressed artifact": {
			"asbackup", filepath.Join(gzipped, "test_00000.asb"), "unsupported_source", "gzip"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			line, calls, _ := driveOp(t, "provision",
				provisionPayload(t, tt.kind, tt.path, nil, nil),
				func(verbCall) (any, *protoError) {
					t.Fatal("the sandbox must not be touched")
					return nil, nil
				})
			if len(calls) != 0 {
				t.Fatalf("%d sandbox calls before the refusal", len(calls))
			}
			final := parseFinal(t, line)
			if final.OK {
				t.Fatal("want a refusal")
			}
			if final.Error.Code != tt.code {
				t.Errorf("code = %s, want %s (%s)", final.Error.Code, tt.code, final.Error.Message)
			}
			if !strings.Contains(final.Error.Message, tt.contains) {
				t.Errorf("message = %q, want it to contain %q", final.Error.Message, tt.contains)
			}
		})
	}
}

func TestRequestRefusalsBeforeTheSource(t *testing.T) {
	good := writeBackup(t, nil)
	tests := map[string]struct{ payload, code, contains string }{
		"a point-in-time request": {
			`{"source":{"kind":"asbackup_dir","path":"` + good + `"},"sandbox":{"scratch_dir":"/scratch"},` +
				`"pitr":{"target_time":"2026-07-30T14:32:00Z"}}`, "invalid_request", "pitr"},
		"a declared backup zone": {
			provisionPayload(t, "asbackup_dir", good, map[string]string{"backup_timezone": "Europe/Budapest"}, nil),
			"invalid_request", "no effect"},
		"a data size the configuration would not accept": {
			provisionPayload(t, "asbackup_dir", good, nil, map[string]string{"data_size": "512M; evil"}),
			"invalid_request", "data_size"},
		"a malformed payload": {`[]`, "invalid_request", "malformed"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			line, calls, _ := driveOp(t, "provision", tt.payload, func(verbCall) (any, *protoError) {
				t.Fatal("the sandbox must not be touched")
				return nil, nil
			})
			if len(calls) != 0 {
				t.Fatalf("%d sandbox calls before the refusal", len(calls))
			}
			final := parseFinal(t, line)
			if final.OK {
				t.Fatal("want a refusal")
			}
			if final.Error.Code != tt.code {
				t.Errorf("code = %s, want %s (%s)", final.Error.Code, tt.code, final.Error.Message)
			}
			if !strings.Contains(final.Error.Message, tt.contains) {
				t.Errorf("message = %q, want it to contain %q", final.Error.Message, tt.contains)
			}
		})
	}
}

// TestABusySandboxIsRefused covers the preflight: an engine already
// serving would be serving the image's own namespace, and the restore
// would land somewhere nobody asked for.
func TestABusySandboxIsRefused(t *testing.T) {
	dir := writeBackup(t, nil)
	line, _, _ := driveOp(t, "provision",
		provisionPayload(t, "asbackup_dir", dir, nil, nil),
		func(call verbCall) (any, *protoError) {
			if stage(argvOf(t, call)) == "preflight" {
				return execWith(1, "", "an engine is already serving in this sandbox\n"), nil
			}
			t.Fatal("nothing may follow a failed preflight")
			return nil, nil
		})
	final := parseFinal(t, line)
	if final.OK || final.Error.Code != "invalid_request" {
		t.Fatalf("want invalid_request, got %+v", final)
	}
	if !strings.Contains(final.Error.Message, "idle") {
		t.Errorf("the refusal must name what the sandbox has to be: %s", final.Error.Message)
	}
}

func TestRejectsWrongProtocolAndOp(t *testing.T) {
	stdout := &bytes.Buffer{}
	in := strings.NewReader(`{"protocol":"probavi-adapter/999","request_id":"r-1","op":"probe"}` + "\n")
	if exit := run(in, stdout, io.Discard); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout.String(), "unsupported_protocol") ||
		!strings.Contains(stdout.String(), protocolVersion) {
		t.Errorf("response = %s", stdout)
	}
	line, _, _ := driveOp(t, "nonsense", "{}", func(verbCall) (any, *protoError) { return okExec(), nil })
	if final := parseFinal(t, line); final.OK || final.Error.Code != "invalid_request" {
		t.Errorf("unknown op = %+v", final)
	}
}

func TestTeardownIsIdempotentAndTakesEmptyState(t *testing.T) {
	for range 2 {
		line, calls, exit := driveOp(t, "teardown", `{"state":{},"reason":"failed"}`,
			func(verbCall) (any, *protoError) {
				t.Fatal("teardown creates nothing outside the sandbox and releases nothing")
				return nil, nil
			})
		if exit != 0 || len(calls) != 0 {
			t.Fatalf("exit=%d calls=%d", exit, len(calls))
		}
		if final := parseFinal(t, line); !final.OK {
			t.Fatalf("teardown failed: %+v", final.Error)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	tests := map[string]struct {
		value   any
		healthy bool
		detail  string
	}{
		"a namespace that answers": {execWith(0, "3\n", ""), true, "3 objects"},
		"an engine that stopped":   {execWith(1, "", "the engine answered nothing for namespace/orders\n"), false, "did not answer"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			line, _, _ := driveOp(t, "healthcheck",
				`{"connection":{"database":"orders"},"state":{"namespace":"orders"}}`,
				func(verbCall) (any, *protoError) { return tt.value, nil })
			final := parseFinal(t, line)
			if !final.OK {
				t.Fatalf("healthcheck must report a verdict, not fail: %+v", final.Error)
			}
			got := struct {
				Healthy bool    `json:"healthy"`
				Latency float64 `json:"latency_seconds"`
				Detail  string  `json:"detail"`
			}{}
			if err := json.Unmarshal(final.Payload, &got); err != nil {
				t.Fatal(err)
			}
			if got.Healthy != tt.healthy {
				t.Errorf("healthy = %v, want %v", got.Healthy, tt.healthy)
			}
			if !strings.Contains(got.Detail, tt.detail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tt.detail)
			}
		})
	}
}

func TestHealthcheckNeedsANamespace(t *testing.T) {
	line, _, _ := driveOp(t, "healthcheck", `{"connection":{},"state":{}}`,
		func(verbCall) (any, *protoError) {
			t.Fatal("nothing to ask without a namespace")
			return nil, nil
		})
	if final := parseFinal(t, line); final.OK || final.Error.Code != "invalid_request" {
		t.Errorf("final = %+v", final)
	}
}
