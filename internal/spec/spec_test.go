package spec_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	schemasDir = "../../docs/schemas"
	idBase     = "https://probavi.dev/schemas/"

	// schemaFileCount guards against a schema file silently dropping out of
	// the walk (12 adapter shapes + the evidence record + the notification
	// payload + the capabilities manifest + the backup manifest).
	schemaFileCount = 16
)

// newCompiler registers every schema file under its canonical $id so that
// cross-file $refs resolve offline, and enables format assertions.
func newCompiler(t *testing.T) (*jsonschema.Compiler, []string) {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()

	var ids []string
	err := filepath.WalkDir(schemasDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".json" {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := f.Close(); cerr != nil {
				t.Errorf("close %s: %v", path, cerr)
			}
		}()
		doc, err := jsonschema.UnmarshalJSON(f)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		obj, ok := doc.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: schema file is not a JSON object", path)
		}
		id, ok := obj["$id"].(string)
		if !ok || !strings.HasPrefix(id, idBase) {
			return fmt.Errorf("%s: $id %v does not start with %s", path, obj["$id"], idBase)
		}
		ids = append(ids, id)
		return c.AddResource(id, doc)
	})
	if err != nil {
		t.Fatalf("register schemas: %v", err)
	}
	if len(ids) != schemaFileCount {
		t.Fatalf("found %d schema files, want %d — update schemaFileCount alongside docs/schemas changes", len(ids), schemaFileCount)
	}
	return c, ids
}

func compile(t *testing.T, c *jsonschema.Compiler, rel string) *jsonschema.Schema {
	t.Helper()
	sch, err := c.Compile(idBase + rel)
	if err != nil {
		t.Fatalf("compile %s: %v", rel, err)
	}
	return sch
}

func parseJSON(t *testing.T, data []byte) any {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse sample: %v", err)
	}
	return doc
}

func goldenLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("golden %s is empty", path)
	}
	return lines
}

// TestSchemasCompile proves every committed schema file is itself valid
// draft 2020-12 with resolvable references.
func TestSchemasCompile(t *testing.T) {
	c, ids := newCompiler(t)
	for _, id := range ids {
		if _, err := c.Compile(id); err != nil {
			t.Errorf("compile %s: %v", id, err)
		}
	}
}

// TestEvidenceGoldenLogsValidate holds the evidence record schema to the
// byte-frozen golden logs of both published schema versions.
func TestEvidenceGoldenLogsValidate(t *testing.T) {
	c, _ := newCompiler(t)
	record := compile(t, c, "evidence/record.json")
	for _, golden := range []string{
		"../../docs/schemas/evidence/examples/log_v0.jsonl",
		"../../docs/schemas/evidence/examples/log_v1.jsonl",
		"../../docs/schemas/evidence/examples/log_v2.jsonl",
	} {
		for i, line := range goldenLines(t, golden) {
			if err := record.Validate(parseJSON(t, line)); err != nil {
				t.Errorf("%s line %d does not validate: %v", golden, i+1, err)
			}
		}
	}
}

// child returns the named sub-object of a decoded record with a checked
// type assertion, so mutation cases stay one-liners.
func child(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	c, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object", key)
	}
	return c
}

func firstCheck(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	arr, ok := m["checks"].([]any)
	if !ok || len(arr) == 0 {
		t.Fatal("checks is not a non-empty array")
	}
	c, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatal("checks[0] is not an object")
	}
	return c
}

// TestEvidenceRecordViolations proves the record schema actually
// constrains: each mutation of a valid golden record must be rejected.
func TestEvidenceRecordViolations(t *testing.T) {
	c, _ := newCompiler(t)
	record := compile(t, c, "evidence/record.json")
	// Line 1 of the v1 golden is a pass record (error null); line 2 is a
	// fail record with an error object.
	lines := goldenLines(t, "../../docs/schemas/evidence/examples/log_v1.jsonl")

	cases := []struct {
		name   string
		line   int
		mutate func(t *testing.T, m map[string]any)
	}{
		{"unpublished schema version", 0, func(_ *testing.T, m map[string]any) { m["schema"] = "probavi-evidence/3" }},
		{"unknown top-level field", 0, func(_ *testing.T, m map[string]any) { m["comment"] = "forged" }},
		{"missing env", 0, func(_ *testing.T, m map[string]any) { delete(m, "env") }},
		{"v1 drill without pitr_target", 0, func(t *testing.T, m map[string]any) {
			delete(child(t, m, "drill"), "pitr_target")
		}},
		{"v0 downgrade keeping pitr_target", 0, func(_ *testing.T, m map[string]any) { m["schema"] = "probavi-evidence/0" }},
		{"fractional timing", 0, func(t *testing.T, m map[string]any) {
			child(t, m, "timings_ms")["restore"] = 190.5
		}},
		{"timing beyond 2^53-1", 0, func(t *testing.T, m map[string]any) {
			child(t, m, "timings_ms")["restore"] = json.Number("9007199254740992")
		}},
		{"seq zero", 0, func(_ *testing.T, m map[string]any) { m["seq"] = 0 }},
		{"ts without millisecond precision", 0, func(_ *testing.T, m map[string]any) { m["ts"] = "2026-07-31T02:00:11Z" }},
		{"prev_hash not hex", 0, func(_ *testing.T, m map[string]any) { m["prev_hash"] = "sha256:zz" }},
		{"unknown outcome", 0, func(_ *testing.T, m map[string]any) { m["outcome"] = "ok" }},
		{"pass with error object", 1, func(_ *testing.T, m map[string]any) { m["outcome"] = "pass" }},
		{"fail with null error", 1, func(_ *testing.T, m map[string]any) { m["error"] = nil }},
		{"error code outside registry", 1, func(t *testing.T, m map[string]any) {
			child(t, m, "error")["code"] = "mystery"
		}},
		{"error with extra field", 1, func(t *testing.T, m map[string]any) {
			child(t, m, "error")["hint"] = "x"
		}},
		{"check detail over 256 chars", 0, func(t *testing.T, m map[string]any) {
			firstCheck(t, m)["detail"] = strings.Repeat("a", 257)
		}},
		{"sandbox param non-string value", 0, func(t *testing.T, m map[string]any) {
			child(t, child(t, m, "sandbox"), "params")["image"] = 16
		}},
		{"host_id uppercase", 0, func(t *testing.T, m map[string]any) {
			child(t, m, "env")["host_id"] = "3F7A9C2E5B1D8E04"
		}},
		{"sig_b64 wrong length", 0, func(t *testing.T, m map[string]any) {
			child(t, m, "sig")["sig_b64"] = "c2hvcnQ="
		}},
		{"sig alg not ed25519", 0, func(t *testing.T, m map[string]any) {
			child(t, m, "sig")["alg"] = "rsa"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := parseJSON(t, lines[tc.line])
			m, ok := doc.(map[string]any)
			if !ok {
				t.Fatal("golden line is not an object")
			}
			tc.mutate(t, m)
			if err := record.Validate(doc); err == nil {
				t.Error("mutated record validates, want rejection")
			}
		})
	}
}

// TestNotificationGoldenValidates holds the notification payload schema to
// the byte-frozen golden payloads of internal/notify.
func TestNotificationGoldenValidates(t *testing.T) {
	c, _ := newCompiler(t)
	payload := compile(t, c, "notification/payload.json")
	for i, line := range goldenLines(t, "../notify/testdata/payload.golden") {
		if err := payload.Validate(parseJSON(t, line)); err != nil {
			t.Errorf("payload.golden line %d does not validate: %v", i+1, err)
		}
	}
}

// TestNotificationPayloadViolations proves the payload schema constrains:
// each mutation of a valid golden payload must be rejected.
func TestNotificationPayloadViolations(t *testing.T) {
	c, _ := newCompiler(t)
	payload := compile(t, c, "notification/payload.json")
	// Line 1 of the golden is a pass payload (error null); line 2 is a fail
	// payload with an error object.
	lines := goldenLines(t, "../notify/testdata/payload.golden")

	cases := []struct {
		name   string
		line   int
		mutate func(t *testing.T, m map[string]any)
	}{
		{"unpublished schema version", 0, func(_ *testing.T, m map[string]any) { m["schema"] = "probavi-notification/2" }},
		{"unknown event", 0, func(_ *testing.T, m map[string]any) { m["event"] = "drill.started" }},
		{"unknown top-level field", 0, func(_ *testing.T, m map[string]any) { m["comment"] = "forged" }},
		{"missing probavi_version", 0, func(_ *testing.T, m map[string]any) { delete(m, "probavi_version") }},
		{"seq zero", 0, func(_ *testing.T, m map[string]any) { m["seq"] = 0 }},
		{"ts without millisecond precision", 0, func(_ *testing.T, m map[string]any) { m["ts"] = "2026-08-01T03:10:02Z" }},
		{"config_hash not sha256", 0, func(t *testing.T, m map[string]any) {
			child(t, m, "drill")["config_hash"] = "md5:abc"
		}},
		{"uppercase adapter name", 0, func(_ *testing.T, m map[string]any) { m["adapter"] = "Postgres" }},
		{"unknown outcome", 0, func(_ *testing.T, m map[string]any) { m["outcome"] = "ok" }},
		{"fractional timing", 0, func(t *testing.T, m map[string]any) {
			child(t, m, "timings_ms")["restore"] = 190.5
		}},
		{"pass with error object", 1, func(_ *testing.T, m map[string]any) { m["outcome"] = "pass" }},
		{"fail with null error", 1, func(_ *testing.T, m map[string]any) { m["error"] = nil }},
		{"error code outside registry", 1, func(t *testing.T, m map[string]any) {
			child(t, m, "error")["code"] = "mystery"
		}},
		{"multi-line error message", 1, func(t *testing.T, m map[string]any) {
			child(t, m, "error")["message"] = "line one\nline two"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := parseJSON(t, lines[tc.line])
			m, ok := doc.(map[string]any)
			if !ok {
				t.Fatal("golden line is not an object")
			}
			tc.mutate(t, m)
			if err := payload.Validate(doc); err == nil {
				t.Error("mutated payload validates, want rejection")
			}
		})
	}
}

// TestAdapterProbeGoldensValidate holds the wire and payload schemas to the
// in-repo adapters' golden probe responses.
func TestAdapterProbeGoldensValidate(t *testing.T) {
	c, _ := newCompiler(t)
	response := compile(t, c, "adapter/response.json")
	probe := compile(t, c, "adapter/probe-response.json")
	for _, golden := range []string{
		"../../adapters/postgres/testdata/probe_response.golden",
		"../../adapters/mysql/testdata/probe_response.golden",
		"../../adapters/mongodb/testdata/probe_response.golden",
		"../../adapters/mssql/testdata/probe_response.golden",
	} {
		doc := parseJSON(t, goldenLines(t, golden)[0])
		if err := response.Validate(doc); err != nil {
			t.Errorf("%s: response envelope: %v", golden, err)
		}
		m, ok := doc.(map[string]any)
		if !ok {
			t.Fatalf("%s: golden is not an object", golden)
		}
		if err := probe.Validate(m["payload"]); err != nil {
			t.Errorf("%s: probe payload: %v", golden, err)
		}
	}
}

// wireSamples are complete, valid protocol messages for every wire and
// payload shape — the positive half of the schema contract.
var wireSamples = []struct {
	name   string
	schema string
	doc    string
}{
	{"probe request", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","op":"probe","payload":{}}`},
	{"provision request with pitr", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","op":"provision","payload":{
			"source":{"kind":"pgdump","path":"/backups/orders/latest.dump","params":{"a":"b"},"credential_env":["ORDERS_BACKUP_PASSPHRASE"]},
			"sandbox":{"scratch_dir":"/tmp"},"options":{},
			"pitr":{"target_time":"2026-07-30T14:32:00Z"}}}`},
	{"healthcheck request", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-3","op":"healthcheck","payload":{
			"connection":{"scheme":"postgresql","host":"127.0.0.1","port":5432,"database":"postgres","user":"postgres","password_env":"PROBAVI_SANDBOX_PASSWORD"},
			"state":{"restored_database":"postgres"}}}`},
	{"teardown request", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-4","op":"teardown","payload":{"state":{},"reason":"completed"}}`},
	{"exec call", "adapter/sandbox-call.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_call":{"call_id":"c1","verb":"exec",
			"args":{"argv":["pg_isready"],"env":{"PGUSER":"postgres"},"stdin_b64":"AQ==","timeout_seconds":30}}}`},
	{"put_file call", "adapter/sandbox-call.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_call":{"call_id":"c2","verb":"put_file",
			"args":{"source_path":"/backups/orders/latest.dump","dest_path":"/tmp/src.dump","mode":"0600"}}}`},
	{"exec result", "adapter/sandbox-result.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_result":{"call_id":"c1","ok":true,
			"value":{"exit_code":0,"stdout_b64":"MQ==","stderr_b64":"","truncated":false,"duration_seconds":0.04}}}`},
	{"put_file result", "adapter/sandbox-result.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_result":{"call_id":"c2","ok":true,
			"value":{"bytes_copied":565248,"duration_seconds":0.11}}}`},
	{"failed sandbox result", "adapter/sandbox-result.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_result":{"call_id":"c1","ok":false,
			"error":{"code":"sandbox_error","message":"runtime died","retryable":true}}}`},
	{"ok final response", "adapter/response.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-4","ok":true,"payload":{"released":true}}`},
	{"error final response with detail", "adapter/response.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","ok":false,
			"error":{"code":"unsupported_protocol","message":"only probavi-adapter/0","retryable":false,"detail":{"supported":["probavi-adapter/0"]}}}`},
	{"provision response payload", "adapter/provision-response.json",
		`{"connection":{"scheme":"postgresql","host":"127.0.0.1","port":5432,"database":"postgres","user":"postgres","password_env":"PROBAVI_SANDBOX_PASSWORD"},
			"source_identity":{"checksum":"sha256:9f2a11a6a9e1a76f7e4c62b9b2b0a3f2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6","size_bytes":565248,"created_at":"2026-07-30T01:58:02Z"},
			"timings":{"engine_ready_seconds":1.17,"transfer_seconds":0.11,"restore_seconds":0.19},
			"state":{"restored_database":"postgres"}}`},
	{"provision response with null created_at", "adapter/provision-response.json",
		`{"connection":{"scheme":"nulldb","host":"127.0.0.1","port":0,"database":"null","user":"null"},
			"source_identity":{"checksum":"sha256:9f2a11a6a9e1a76f7e4c62b9b2b0a3f2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6","size_bytes":0,"created_at":null},
			"timings":{"engine_ready_seconds":0,"transfer_seconds":0,"restore_seconds":0},
			"state":{}}`},
	{"healthcheck response payload", "adapter/healthcheck-response.json",
		`{"healthy":true,"latency_seconds":0.02,"detail":"accepting connections; 1 database"}`},
	{"unhealthy response payload", "adapter/healthcheck-response.json",
		`{"healthy":false,"latency_seconds":0,"detail":"connection refused"}`},
	{"teardown response payload", "adapter/teardown-response.json",
		`{"released":true}`},
}

func TestAdapterMessageSamplesValidate(t *testing.T) {
	c, _ := newCompiler(t)
	for _, tc := range wireSamples {
		t.Run(tc.name, func(t *testing.T) {
			if err := compile(t, c, tc.schema).Validate(parseJSON(t, []byte(tc.doc))); err != nil {
				t.Errorf("valid sample rejected: %v", err)
			}
		})
	}
}

// wireViolations are malformed messages the schemas must reject — the
// negative half of the schema contract.
var wireViolations = []struct {
	name   string
	schema string
	doc    string
}{
	{"unknown op", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","op":"backup","payload":{}}`},
	{"missing request_id", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","op":"probe","payload":{}}`},
	{"future protocol version", "adapter/request.json",
		`{"protocol":"probavi-adapter/999","request_id":"r-1","op":"probe","payload":{}}`},
	{"probe payload not empty", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","op":"probe","payload":{"verbose":true}}`},
	{"provision payload missing sandbox", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","op":"provision","payload":{
			"source":{"kind":"pgdump","path":"/b/x.dump","params":{},"credential_env":[]},"options":{}}}`},
	{"pitr without target_time", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","op":"provision","payload":{
			"source":{"kind":"pgdump","path":"/b/x.dump","params":{},"credential_env":[]},
			"sandbox":{"scratch_dir":"/tmp"},"options":{},"pitr":{}}}`},
	{"pitr target not a timestamp", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","op":"provision","payload":{
			"source":{"kind":"pgdump","path":"/b/x.dump","params":{},"credential_env":[]},
			"sandbox":{"scratch_dir":"/tmp"},"options":{},"pitr":{"target_time":"yesterday"}}}`},
	{"teardown reason outside enum", "adapter/request.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-4","op":"teardown","payload":{"state":{},"reason":"because"}}`},
	{"unknown verb", "adapter/sandbox-call.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_call":{"call_id":"c1","verb":"download","args":{}}}`},
	{"exec with empty argv", "adapter/sandbox-call.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_call":{"call_id":"c1","verb":"exec","args":{"argv":[]}}}`},
	{"exec with undefined arg field", "adapter/sandbox-call.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_call":{"call_id":"c1","verb":"exec","args":{"argv":["ls"],"cwd":"/"}}}`},
	{"put_file mode not octal", "adapter/sandbox-call.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_call":{"call_id":"c2","verb":"put_file",
			"args":{"source_path":"/b/x","dest_path":"/tmp/x","mode":"rw-r--r--"}}}`},
	{"ok result without value", "adapter/sandbox-result.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_result":{"call_id":"c1","ok":true}}`},
	{"result value matching no verb", "adapter/sandbox-result.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_result":{"call_id":"c1","ok":true,"value":{"foo":1}}}`},
	{"failed result without error", "adapter/sandbox-result.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-2","sandbox_result":{"call_id":"c1","ok":false}}`},
	{"ok response without payload", "adapter/response.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","ok":true}`},
	{"ok response with error", "adapter/response.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","ok":true,"payload":{},
			"error":{"code":"internal","message":"x","retryable":false}}`},
	{"error response with payload", "adapter/response.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","ok":false,"payload":{},
			"error":{"code":"internal","message":"x","retryable":false}}`},
	{"error code outside registry", "adapter/response.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","ok":false,
			"error":{"code":"oops","message":"x","retryable":false}}`},
	{"error missing retryable", "adapter/response.json",
		`{"protocol":"probavi-adapter/0","request_id":"r-1","ok":false,
			"error":{"code":"internal","message":"x"}}`},
	{"probe name uppercase", "adapter/probe-response.json",
		`{"name":"Postgres","adapter_version":"0.1.0","protocol_versions":["probavi-adapter/0"],
			"engine":{"name":"postgresql"},"sources":[{"kind":"pgdump","capabilities":{"pitr":false}}],
			"sql_runner":{"argv":["psql","-c","{{sql}}"],"env":{}},"verbs_required":["exec"]}`},
	{"probe without v0 in protocol_versions", "adapter/probe-response.json",
		`{"name":"postgres","adapter_version":"0.1.0","protocol_versions":["probavi-adapter/1"],
			"engine":{"name":"postgresql"},"sources":[{"kind":"pgdump","capabilities":{"pitr":false}}],
			"sql_runner":{"argv":["psql","-c","{{sql}}"],"env":{}},"verbs_required":["exec"]}`},
	{"probe without source kinds", "adapter/probe-response.json",
		`{"name":"postgres","adapter_version":"0.1.0","protocol_versions":["probavi-adapter/0"],
			"engine":{"name":"postgresql"},"sources":[],
			"sql_runner":{"argv":["psql","-c","{{sql}}"],"env":{}},"verbs_required":["exec"]}`},
	{"sql_runner without {{sql}} element", "adapter/probe-response.json",
		`{"name":"postgres","adapter_version":"0.1.0","protocol_versions":["probavi-adapter/0"],
			"engine":{"name":"postgresql"},"sources":[{"kind":"pgdump","capabilities":{"pitr":false}}],
			"sql_runner":{"argv":["psql","-c","select 1"],"env":{}},"verbs_required":["exec"]}`},
	{"verbs_required outside v0 set", "adapter/probe-response.json",
		`{"name":"postgres","adapter_version":"0.1.0","protocol_versions":["probavi-adapter/0"],
			"engine":{"name":"postgresql"},"sources":[{"kind":"pgdump","capabilities":{"pitr":false}}],
			"sql_runner":{"argv":["psql","-c","{{sql}}"],"env":{}},"verbs_required":["network"]}`},
	{"checksum not sha256-prefixed", "adapter/provision-response.json",
		`{"connection":{"scheme":"postgresql","host":"h","port":5432,"database":"d","user":"u"},
			"source_identity":{"checksum":"md5:abc","size_bytes":1,"created_at":null},
			"timings":{"engine_ready_seconds":0,"transfer_seconds":0,"restore_seconds":0},"state":{}}`},
	{"port out of range", "adapter/provision-response.json",
		`{"connection":{"scheme":"postgresql","host":"h","port":70000,"database":"d","user":"u"},
			"source_identity":{"checksum":"sha256:9f2a11a6a9e1a76f7e4c62b9b2b0a3f2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6","size_bytes":1,"created_at":null},
			"timings":{"engine_ready_seconds":0,"transfer_seconds":0,"restore_seconds":0},"state":{}}`},
	{"negative restore timing", "adapter/provision-response.json",
		`{"connection":{"scheme":"postgresql","host":"h","port":5432,"database":"d","user":"u"},
			"source_identity":{"checksum":"sha256:9f2a11a6a9e1a76f7e4c62b9b2b0a3f2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6","size_bytes":1,"created_at":null},
			"timings":{"engine_ready_seconds":0,"transfer_seconds":0,"restore_seconds":-1},"state":{}}`},
	{"healthcheck missing latency", "adapter/healthcheck-response.json",
		`{"healthy":true,"detail":"ok"}`},
	{"teardown released not boolean", "adapter/teardown-response.json",
		`{"released":"yes"}`},
}

func TestAdapterMessageViolations(t *testing.T) {
	c, _ := newCompiler(t)
	for _, tc := range wireViolations {
		t.Run(tc.name, func(t *testing.T) {
			if err := compile(t, c, tc.schema).Validate(parseJSON(t, []byte(tc.doc))); err == nil {
				t.Error("malformed sample validates, want rejection")
			}
		})
	}
}
