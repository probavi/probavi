package main

import (
	"archive/zip"
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

func stdoutExec(stdout string) any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout))}
}

func errExec(exit int, stderr string) any {
	return execValue{ExitCode: exit, StderrB64: base64.StdEncoding.EncodeToString([]byte(stderr))}
}

// writeArchive builds a backup archive whose manifest carries wallClock,
// in the shape ClickHouse writes: a `.backup` XML member opening with the
// header fields, followed by the file list.
func writeArchive(t *testing.T, path, wallClock string) {
	t.Helper()
	manifest := "<config><version>1</version><deduplicate_files>1</deduplicate_files>" +
		"<timestamp>" + wallClock + "</timestamp><uuid>3b7daaa5-e7bf-4f51-afa0-cb1168306227</uuid>" +
		"<contents><file><name>data/shop/orders/all_1_1_0/data.bin</name><size>4081</size>" +
		"<checksum>62512e2b88e0fe44a474a95fc4d89d99</checksum></file></contents></config>"
	if wallClock == "" {
		manifest = "<config><version>1</version><contents></contents></config>"
	}
	writeArchiveMembers(t, path, []string{backupManifest, manifest})
}

// writeArchiveMembers writes a zip of name/content pairs.
func writeArchiveMembers(t *testing.T, path string, members []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	zw := zip.NewWriter(f)
	for i := 0; i+1 < len(members); i += 2 {
		w, err := zw.Create(members[i])
		if err != nil {
			t.Fatalf("create member: %v", err)
		}
		if _, err := io.WriteString(w, members[i+1]); err != nil {
			t.Fatalf("write member: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
}

// restoreSandbox answers the call sequence a successful provision makes:
// readiness, data-path lookup, directory preparation, transfer, restore.
func restoreSandbox(t *testing.T, restoreStdout string, restoreExit int, restoreStderr string) func(verbCall) (any, *protoError) {
	t.Helper()
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{BytesCopied: 128, DurationSeconds: 0.01}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("exec args: %v", err)
		}
		joined := strings.Join(args.Argv, " ")
		switch {
		case strings.Contains(joined, "system.server_settings"):
			return stdoutExec("/var/lib/clickhouse/\n"), nil
		case strings.Contains(joined, "mkdir"):
			return okExec(), nil
		case strings.Contains(joined, "RESTORE ALL"):
			if restoreExit != 0 {
				return errExec(restoreExit, restoreStderr), nil
			}
			return stdoutExec(restoreStdout), nil
		case strings.Contains(joined, "SELECT 1"):
			return stdoutExec("1\n"), nil
		}
		t.Fatalf("unexpected exec: %s", joined)
		return nil, nil
	}
}

func okExec() any { return execValue{ExitCode: 0} }

func provisionPayload(t *testing.T, kind, path string, params map[string]string) string {
	t.Helper()
	req := map[string]any{
		"source":  map[string]any{"kind": kind, "path": path, "params": params},
		"sandbox": map[string]any{"scratch_dir": "/tmp"},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
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

// TestProbeSQLRunnerIsUsableByTheCore pins the two properties the core's
// check layer depends on and the conformance suite asserts (§6.1, §10.2).
func TestProbeSQLRunnerIsUsableByTheCore(t *testing.T) {
	payload, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := payload["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("probe declares no sql_runner")
	}
	argv, ok := runner["argv"].([]string)
	if !ok {
		t.Fatal("sql_runner.argv is not a string list")
	}
	var sawSQL, sawHost bool
	for _, a := range argv {
		if a == "{{sql}}" {
			sawSQL = true
		}
		if a == "127.0.0.1" {
			sawHost = true
		}
		if strings.Contains(a, "{{password}}") {
			t.Errorf("argv element %q carries {{password}}; secrets belong in env only", a)
		}
	}
	if !sawSQL {
		t.Error("sql_runner.argv must contain {{sql}} as its own element")
	}
	// A zero-ingress sandbox has no DNS, so a runner that lets the client
	// fall back to its own hostname cannot connect at all.
	if !sawHost {
		t.Error("sql_runner.argv must pin the loopback host: the sandbox has no DNS")
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

func TestProvisionRestoresArchive(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "shop.zip")
	writeArchive(t, archive, "2026-08-14 14:37:45")

	payload := provisionPayload(t, "clickhouse_backup", archive, map[string]string{"backup_timezone": "UTC"})
	line, calls, exit := driveOp(t, "provision", payload,
		restoreSandbox(t, "3b7daaa5-e7bf-4f51-afa0-cb1168306227\tRESTORED\n", 0, ""))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}

	got := struct {
		Connection struct {
			Scheme, Database, User string
			Port                   int
		} `json:"connection"`
		SourceIdentity struct {
			Checksum  string  `json:"checksum"`
			SizeBytes int64   `json:"size_bytes"`
			CreatedAt *string `json:"created_at"`
		} `json:"source_identity"`
		Timings map[string]float64 `json:"timings"`
		State   map[string]any     `json:"state"`
	}{}
	if err := json.Unmarshal(f.Payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if !strings.HasPrefix(got.SourceIdentity.Checksum, "sha256:") || len(got.SourceIdentity.Checksum) != 71 {
		t.Errorf("checksum = %q", got.SourceIdentity.Checksum)
	}
	if got.SourceIdentity.CreatedAt == nil || *got.SourceIdentity.CreatedAt != "2026-08-14T14:37:45.000Z" {
		t.Errorf("created_at = %v, want the manifest timestamp in the declared zone", got.SourceIdentity.CreatedAt)
	}
	if got.Connection.Scheme != "clickhouse" || got.Connection.Database != defaultDatabase {
		t.Errorf("connection = %+v", got.Connection)
	}
	for _, k := range []string{"engine_ready_seconds", "transfer_seconds", "restore_seconds"} {
		if _, ok := got.Timings[k]; !ok {
			t.Errorf("timings missing %s", k)
		}
	}
	if got.State["backups_dir"] != "/var/lib/clickhouse/backups" {
		t.Errorf("state.backups_dir = %v, want the path derived from the server's own setting", got.State["backups_dir"])
	}

	assertTransfer(t, calls)
}

// assertTransfer pins where the archive lands and at what mode: the server
// runs as its own account while sandbox commands run as root, so the
// protocol's 0600 default would produce a file the engine cannot open.
func assertTransfer(t *testing.T, calls []verbCall) {
	t.Helper()
	var put putFileArgs
	for _, c := range calls {
		if c.Verb != "put_file" {
			continue
		}
		if err := json.Unmarshal(c.Args, &put); err != nil {
			t.Fatalf("put_file args: %v", err)
		}
	}
	if put.DestPath != "/var/lib/clickhouse/backups/"+restoreArchiveName {
		t.Errorf("put_file dest = %q", put.DestPath)
	}
	if put.Mode != "0644" {
		t.Errorf("put_file mode = %q, want 0644 so the engine account can open it", put.Mode)
	}
}

// TestProvisionRequiresTheEnginesOwnConfirmation covers the failure the
// client's exit code alone would miss: the statement was accepted, the
// engine never said it restored anything. The verdict is reached inside
// the sandbox, so what the adapter sees is the script's exit code.
func TestProvisionRequiresTheEnginesOwnConfirmation(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "shop.zip")
	writeArchive(t, archive, "2026-08-14 14:37:45")

	payload := provisionPayload(t, "clickhouse_backup", archive, nil)
	line, _, _ := driveOp(t, "provision", payload, restoreSandbox(t, "", notRestoredExit, ""))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" {
		t.Errorf("final = %+v, want restore_failed when no RESTORED status is printed", f)
	}
	if !strings.Contains(f.Error.Message, "no RESTORED status") {
		t.Errorf("message = %q, want it to name what was missing", f.Error.Message)
	}
}

func TestProvisionRefusals(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "shop.zip")
	writeArchive(t, archive, "2026-08-14 14:37:45")

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "unknown source kind",
			payload: provisionPayload(t, "clickhouse_dump", archive, nil),
			want:    "unsupported_source",
		},
		{
			name:    "missing archive",
			payload: provisionPayload(t, "clickhouse_backup", filepath.Join(dir, "absent.zip"), nil),
			want:    "source_not_found",
		},
		{
			name:    "a directory handed to the single-archive kind",
			payload: provisionPayload(t, "clickhouse_backup", dir, nil),
			want:    "invalid_request",
		},
		{
			name:    "an unknown time zone",
			payload: provisionPayload(t, "clickhouse_backup", archive, map[string]string{"backup_timezone": "Mars/Olympus"}),
			want:    "invalid_request",
		},
		{
			name:    "malformed payload",
			payload: `"not an object"`,
			want:    "invalid_request",
		},
		{
			name: "pitr is not supported",
			payload: `{"source":{"kind":"clickhouse_backup","path":"` + archive +
				`"},"pitr":{"target_time":"2026-08-01T00:00:00Z"}}`,
			want: "invalid_request",
		},
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

func TestMapRestoreFailure(t *testing.T) {
	tests := []struct {
		name, stderr, want string
	}{
		{
			name:   "an archive the engine cannot unpack is the backup's fault",
			stderr: "Code: 643. DB::Exception: Couldn't open zip archive 'probavi-restore.zip'. (CANNOT_UNPACK_ARCHIVE)",
			want:   "source_corrupt",
		},
		{
			name:   "an image that moved allowed_path is not the backup's fault",
			stderr: "Code: 36. DB::Exception: Path '/x' is not allowed for backups, see the 'backups.allowed_path' configuration parameter. (BAD_ARGUMENTS)",
			want:   "restore_failed",
		},
		{
			name:   "an unreadable transferred file is not the backup's fault",
			stderr: "Code: 76. DB::ErrnoException: Cannot open file /var/lib/clickhouse/backups/x.zip: , errno: 13 (CANNOT_OPEN_FILE)",
			want:   "restore_failed",
		},
		{
			name:   "anything else is a restore failure",
			stderr: "Code: 57. DB::Exception: Table shop.orders already exists. (TABLE_ALREADY_EXISTS)",
			want:   "restore_failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapRestoreFailure(1, []byte(tc.stderr))
			if got.Code != tc.want {
				t.Errorf("code = %s, want %s (message: %s)", got.Code, tc.want, got.Message)
			}
			if strings.Contains(got.Message, `"`) {
				t.Errorf("message carries double quotes: %s", got.Message)
			}
		})
	}
}

// TestVerdictLineSkipsTheDNSNoise covers the stderr every invocation
// carries in a zero-ingress sandbox: the client fails to resolve its own
// hostname and prints a stack trace before running the query perfectly.
func TestVerdictLineSkipsTheDNSNoise(t *testing.T) {
	stderr := "Cannot resolve host (bc5ffb289baf), error 0: DNS error.\n" +
		"DB::DNSResolver::Impl::Impl(): Code: 198. DB::NetException: Not found address of host: bc5ffb289baf. (DNS_ERROR)\n" +
		"0. DB::Exception::Exception(DB::Exception::MessageMasked&&, int, bool) @ 0x17b16c2a\n" +
		"Received exception from server (version 26.3.17):\n" +
		"Code: 643. DB::Exception: Couldn't open zip archive 'x.zip'. (CANNOT_UNPACK_ARCHIVE)\n"
	got := verdictLine([]byte(stderr))
	if !strings.Contains(got, "CANNOT_UNPACK_ARCHIVE") {
		t.Errorf("verdict = %q, want the engine's diagnostic rather than the DNS warning", got)
	}
}

func TestHealthcheck(t *testing.T) {
	t.Run("a serving engine is healthy", func(t *testing.T) {
		line, _, exit := driveOp(t, "healthcheck", `{"connection":{"database":"shop"},"state":{}}`,
			func(verbCall) (any, *protoError) { return stdoutExec("1\n"), nil })
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
	t.Run("an engine that does not answer is unhealthy, not an error", func(t *testing.T) {
		line, _, _ := driveOp(t, "healthcheck", `{"connection":{"database":"shop"},"state":{}}`,
			func(verbCall) (any, *protoError) { return errExec(210, "connection refused"), nil })
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
			t.Error("healthy = true for an engine that exited non-zero")
		}
	})
}
