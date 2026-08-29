package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// prelude reads the request and extracts the request_id — the shared
// header of every fake adapter script.
const prelude = `read -r REQ
RID=$(printf '%s' "$REQ" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`

const probePayload = `{"name":"fake","adapter_version":"0.0.1","protocol_versions":["probavi-adapter/0"],"engine":{"name":"fakedb"},"sources":[{"kind":"file","capabilities":{"pitr":true}}],"sql_runner":{"argv":["cat"],"env":{}},"verbs_required":["exec","put_file"]}`

func probeFinal(payload string) string {
	return `printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":true,"payload":` + payload + `}\n' "$RID"` + "\n"
}

// fakeRunner writes an executable sh script and returns a Runner for it.
func fakeRunner(t *testing.T, script string, logger *slog.Logger, opts *Options) *Runner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probavi-adapter-fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake adapter: %v", err)
	}
	if opts == nil {
		opts = &Options{Grace: 500 * time.Millisecond}
	}
	return newRunner(path, logger, opts)
}

type putCall struct{ hostPath, destPath, mode string }

// fakeVerbs implements SandboxVerbs without Docker.
type fakeVerbs struct {
	execReqs []sandbox.ExecRequest
	execRes  sandbox.ExecResult
	execErr  error
	putCalls []putCall
	putRes   sandbox.PutFileResult
	putErr   error
}

func (f *fakeVerbs) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	f.execReqs = append(f.execReqs, req)
	if f.execErr != nil {
		return nil, f.execErr
	}
	res := f.execRes
	return &res, nil
}

func (f *fakeVerbs) PutFile(_ context.Context, hostPath, destPath, mode string) (*sandbox.PutFileResult, error) {
	f.putCalls = append(f.putCalls, putCall{hostPath, destPath, mode})
	if f.putErr != nil {
		return nil, f.putErr
	}
	res := f.putRes
	return &res, nil
}

func asAdapterError(t *testing.T, err error) *Error {
	t.Helper()
	aerr := &Error{}
	if !errors.As(err, &aerr) {
		t.Fatalf("error %v (%T) is not an adapter *Error", err, err)
	}
	return aerr
}

func TestProbeOK(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	script := prelude +
		`echo "starting up" >&2` + "\n" +
		probeFinal(probePayload)
	r := fakeRunner(t, script, logger, nil)

	res, err := r.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Name != "fake" || res.Engine.Name != "fakedb" || !res.Sources[0].Capabilities.PITR {
		t.Errorf("Probe result = %+v", res)
	}
	if len(res.SQLRunner.Argv) != 1 || res.SQLRunner.Argv[0] != "cat" {
		t.Errorf("sql_runner = %+v", res.SQLRunner)
	}
	if !strings.Contains(logBuf.String(), "starting up") {
		t.Error("adapter stderr must be captured into the log")
	}
}

func TestProbeProtocolViolations(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		wantCode   string
		wantSubstr string
	}{
		{"crash without output", `exit 1`, CodeAdapterCrash, "exit status 1"},
		{"exit zero without response", `exit 0`, CodeAdapterCrash, "without a final response"},
		{"garbage stdout", `echo this-is-not-json` + "\n" + `exit 0`, CodeAdapterCrash, "not a protocol message"},
		{"wrong protocol id", prelude + `printf '{"protocol":"probavi-adapter/9","request_id":"%s","ok":true,"payload":{}}\n' "$RID"`, CodeAdapterCrash, "message protocol"},
		{"request_id not echoed", prelude + `printf '{"protocol":"probavi-adapter/0","request_id":"bogus","ok":true,"payload":{}}\n'`, CodeAdapterCrash, "does not echo"},
		{"neither call nor final", prelude + `printf '{"protocol":"probavi-adapter/0","request_id":"%s"}\n' "$RID"`, CodeAdapterCrash, "neither sandbox_call nor final"},
		{"ok false without error", prelude + `printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":false}\n' "$RID"`, CodeAdapterCrash, "without error object"},
		{"nonzero exit after final", prelude + probeFinal(probePayload) + `exit 3`, CodeAdapterCrash, "exit status 3"},
		{"oversized frame", `dd if=/dev/zero bs=1048576 count=5 2>/dev/null | tr '\0' 'x'; echo`, CodeAdapterCrash, "frame limit"},
		{"sandbox_call in probe", prelude + `printf '{"protocol":"probavi-adapter/0","request_id":"%s","sandbox_call":{"call_id":"c1","verb":"exec","args":{"argv":["x"]}}}\n' "$RID"` + "\n" + `read -r IGNORED`, CodeAdapterCrash, "protocol violation"},
		{"empty probe name", prelude + probeFinal(`{"name":"","protocol_versions":["probavi-adapter/0"]}`), CodeAdapterCrash, "name is empty"},
		{"mistyped payload", prelude + probeFinal(`{"name":"x","sources":"not-a-list"}`), CodeAdapterCrash, "probe payload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fakeRunner(t, tt.script, nil, nil)
			_, err := r.Probe(context.Background())
			aerr := asAdapterError(t, err)
			if aerr.Code != tt.wantCode || !strings.Contains(aerr.Message, tt.wantSubstr) {
				t.Errorf("Probe error = %+v, want code %s containing %q", aerr, tt.wantCode, tt.wantSubstr)
			}
		})
	}
}

// TestClosedPipe pins the classification that lets do() survive the race
// where the adapter exits before the request write lands: a write into a
// pipe whose read end is gone must be recognized, anything else must not.
func TestClosedPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read end: %v", err)
	}
	_, werr := w.Write([]byte("x"))
	if !closedPipe(werr) {
		t.Errorf("write into closed pipe returned %v, want a closedPipe match", werr)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}
	if closedPipe(nil) || closedPipe(errors.New("boom")) {
		t.Error("closedPipe must not match nil or unrelated errors")
	}
}

func TestProbeAdapterSentError(t *testing.T) {
	script := prelude + `printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":false,"error":{"code":"unsupported_source","message":"no such kind","retryable":false}}\n' "$RID"`
	r := fakeRunner(t, script, nil, nil)
	_, err := r.Probe(context.Background())
	aerr := asAdapterError(t, err)
	if aerr.Code != "unsupported_source" || aerr.Message != "no such kind" {
		t.Errorf("error = %+v, want the adapter's own error passed through", aerr)
	}
}

func TestProbeUnsupportedProtocolVersion(t *testing.T) {
	r := fakeRunner(t, prelude+probeFinal(`{"name":"x","protocol_versions":["probavi-adapter/7"]}`), nil, nil)
	_, err := r.Probe(context.Background())
	if aerr := asAdapterError(t, err); aerr.Code != "unsupported_protocol" {
		t.Errorf("error = %+v, want unsupported_protocol", aerr)
	}
}

func TestEnvAllowlist(t *testing.T) {
	t.Setenv("PROBAVI_TEST_CRED", "cred-value")
	t.Setenv("PROBAVI_TEST_SECRET", "leak-me-not")
	script := prelude + `printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":true,"payload":{"name":"n","adapter_version":"%s|%s|%s","protocol_versions":["probavi-adapter/0"]}}\n' "$RID" "$PROBAVI_TEST_CRED" "$PROBAVI_TEST_SECRET" "$PROBAVI_EXPLICIT"`
	r := fakeRunner(t, script, nil, &Options{
		Grace:         500 * time.Millisecond,
		CredentialEnv: []string{"PROBAVI_TEST_CRED"},
		Env:           map[string]string{"PROBAVI_EXPLICIT": "explicit-value"},
	})
	res, err := r.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.AdapterVersion != "cred-value||explicit-value" {
		t.Errorf("adapter saw %q, want credential and explicit vars present and the secret scrubbed", res.AdapterVersion)
	}
}

// provisionScript exercises the full verb dance: put_file (source taken
// from the request), then exec with stdin, then a final response embedding
// what the core returned for the exec call.
func provisionScript(checksum string) string {
	return prelude +
		`SRC=$(printf '%s' "$REQ" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')` + "\n" +
		`printf '{"protocol":"probavi-adapter/0","request_id":"%s","sandbox_call":{"call_id":"c1","verb":"put_file","args":{"source_path":"%s","dest_path":"/tmp/x.dump","mode":"0644"}}}\n' "$RID" "$SRC"` + "\n" +
		`read -r R1` + "\n" +
		`printf '{"protocol":"probavi-adapter/0","request_id":"%s","sandbox_call":{"call_id":"c2","verb":"exec","args":{"argv":["restore","--fast"],"env":{"MODE":"quick"},"stdin_b64":"aGVsbG8=","timeout_seconds":5}}}\n' "$RID"` + "\n" +
		`read -r R2` + "\n" +
		`OUT=$(printf '%s' "$R2" | sed -n 's/.*"stdout_b64":"\([^"]*\)".*/\1/p')` + "\n" +
		`printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":true,"payload":{"connection":{"scheme":"fakedb","host":"127.0.0.1","port":1,"database":"d","user":"u"},"source_identity":{"checksum":"sha256:` + checksum + `","size_bytes":7,"created_at":null},"timings":{"engine_ready_seconds":0.1,"transfer_seconds":0.2,"restore_seconds":0.3},"state":{"echo":"%s"}}}\n' "$RID" "$OUT"` + "\n"
}

func TestProvisionVerbDance(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	r := fakeRunner(t, provisionScript(checksum), nil, nil)
	verbs := &fakeVerbs{
		execRes: sandbox.ExecResult{ExitCode: 0, Stdout: []byte("world"), Duration: 10 * time.Millisecond},
		putRes:  sandbox.PutFileResult{BytesCopied: 7, Duration: time.Millisecond},
	}
	req := &ProvisionRequest{Source: ProvisionSource{Kind: "file", Path: "/backups/orders.dump"}}
	res, err := r.Provision(context.Background(), req, verbs)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if len(verbs.putCalls) != 1 || verbs.putCalls[0] != (putCall{"/backups/orders.dump", "/tmp/x.dump", "0644"}) {
		t.Errorf("put_file calls = %+v", verbs.putCalls)
	}
	if len(verbs.execReqs) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(verbs.execReqs))
	}
	execReq := verbs.execReqs[0]
	if strings.Join(execReq.Argv, " ") != "restore --fast" || execReq.Env["MODE"] != "quick" ||
		string(execReq.Stdin) != "hello" || execReq.Timeout != 5*time.Second {
		t.Errorf("exec request = %+v — wire decoding is broken", execReq)
	}

	if res.SourceIdentity.Checksum != "sha256:"+checksum || res.Timings.RestoreSeconds != 0.3 {
		t.Errorf("result = %+v", res)
	}
	state := map[string]string{}
	if err := json.Unmarshal(res.State, &state); err != nil || state["echo"] != "d29ybGQ=" {
		t.Errorf("state = %s — the exec result did not round-trip to the adapter", res.State)
	}
}

// relayScript sends one scripted sandbox_call, then relays the core's
// response for it into a final error, letting tests assert verb outcomes.
func relayScript(call string) string {
	return prelude +
		`printf '{"protocol":"probavi-adapter/0","request_id":"%s","sandbox_call":` + call + `}\n' "$RID"` + "\n" +
		`read -r R1` + "\n" +
		`CODE=$(printf '%s' "$R1" | sed -n 's/.*"code":"\([^"]*\)".*/\1/p')` + "\n" +
		`MSG=$(printf '%s' "$R1" | sed -n 's/.*"message":"\([^"]*\)".*/\1/p')` + "\n" +
		`printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":false,"error":{"code":"%s","message":"%s","retryable":false}}\n' "$RID" "${CODE:-verb-succeeded}" "$MSG"` + "\n"
}

func TestVerbFailures(t *testing.T) {
	execCall := `{"call_id":"c1","verb":"exec","args":{"argv":["x"]}}`
	tests := []struct {
		name       string
		call       string
		verbs      *fakeVerbs
		wantCode   string
		wantSubstr string
	}{
		{"put_file outside source", `{"call_id":"c1","verb":"put_file","args":{"source_path":"/etc/passwd","dest_path":"/tmp/x"}}`,
			&fakeVerbs{}, CodeInvalidRequest, "outside the drill"},
		{"put_file traversal escape", `{"call_id":"c1","verb":"put_file","args":{"source_path":"/backups/orders.dump/../../etc/shadow","dest_path":"/tmp/x"}}`,
			&fakeVerbs{}, CodeInvalidRequest, "outside the drill"},
		{"unknown verb", `{"call_id":"c1","verb":"stream","args":{}}`, &fakeVerbs{}, CodeInvalidRequest, "unknown sandbox verb"},
		{"malformed exec args", `{"call_id":"c1","verb":"exec","args":{"argv":[]}}`, &fakeVerbs{}, CodeInvalidRequest, "malformed exec args"},
		{"bad stdin base64", `{"call_id":"c1","verb":"exec","args":{"argv":["x"],"stdin_b64":"!!!"}}`, &fakeVerbs{}, CodeInvalidRequest, "not valid base64"},
		{"malformed put_file args", `{"call_id":"c1","verb":"put_file","args":{"dest_path":""}}`, &fakeVerbs{}, CodeInvalidRequest, "malformed put_file args"},
		{"exec sandbox failure", execCall, &fakeVerbs{execErr: errors.New("container gone")}, CodeSandboxError, "container gone"},
		{"put_file sandbox failure", `{"call_id":"c1","verb":"put_file","args":{"source_path":"/backups/orders.dump","dest_path":"/tmp/x"}}`,
			&fakeVerbs{putErr: errors.New("cp failed")}, CodeSandboxError, "cp failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fakeRunner(t, relayScript(tt.call), nil, nil)
			req := &ProvisionRequest{Source: ProvisionSource{Kind: "file", Path: "/backups/orders.dump"}}
			_, err := r.Provision(context.Background(), req, tt.verbs)
			aerr := asAdapterError(t, err)
			if aerr.Code != tt.wantCode || !strings.Contains(aerr.Message, tt.wantSubstr) {
				t.Errorf("relayed verb outcome = %+v, want %s containing %q", aerr, tt.wantCode, tt.wantSubstr)
			}
			if tt.wantCode == CodeInvalidRequest && (len(tt.verbs.execReqs) > 0 || len(tt.verbs.putCalls) > 0) {
				t.Error("rejected verb must not reach the sandbox")
			}
		})
	}
}

func TestProvisionResultValidation(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"bad checksum", `{"connection":{"scheme":"s"},"source_identity":{"checksum":"md5:zz"},"timings":{},"state":{}}`, "sha256 reference"},
		{"negative timing", `{"connection":{"scheme":"s"},"source_identity":{"checksum":"sha256:` + strings.Repeat("a", 64) + `"},"timings":{"restore_seconds":-1},"state":{}}`, "negative timings"},
		{"empty scheme", `{"connection":{},"source_identity":{"checksum":"sha256:` + strings.Repeat("a", 64) + `"},"timings":{},"state":{}}`, "connection.scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fakeRunner(t, prelude+probeFinal(tt.payload), nil, nil)
			req := &ProvisionRequest{Source: ProvisionSource{Kind: "file", Path: "/b.dump"}}
			_, err := r.Provision(context.Background(), req, &fakeVerbs{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Provision: got %v, want %q", err, tt.want)
			}
		})
	}

	if _, err := fakeRunner(t, "exit 0", nil, nil).Provision(context.Background(), &ProvisionRequest{}, nil); err == nil {
		t.Error("Provision without verbs must be refused")
	}
}

func TestHealthcheck(t *testing.T) {
	// The script asserts the non-empty state arrives verbatim; on mismatch
	// it exits 1 and the test fails as adapter_crash.
	script := prelude + `case "$REQ" in *'"state":{"k":"v"}'*) ;; *) exit 1 ;; esac` + "\n" +
		probeFinal(`{"healthy":true,"latency_seconds":0.02,"detail":"1 database"}`)
	r := fakeRunner(t, script, nil, nil)
	res, err := r.Healthcheck(context.Background(), &Connection{Scheme: "fakedb"}, json.RawMessage(`{"k":"v"}`), &fakeVerbs{})
	if err != nil {
		t.Fatalf("Healthcheck: %v", err)
	}
	if !res.Healthy || res.Detail != "1 database" {
		t.Errorf("result = %+v", res)
	}
}

func TestPayloadDecodeErrors(t *testing.T) {
	mistyped := prelude + probeFinal(`{"healthy":"yes","released":"maybe","connection":7}`)
	r := fakeRunner(t, mistyped, nil, nil)

	if _, err := r.Healthcheck(context.Background(), &Connection{}, nil, &fakeVerbs{}); err == nil ||
		!strings.Contains(err.Error(), "healthcheck payload") {
		t.Errorf("Healthcheck mistyped payload: got %v", err)
	}
	r = fakeRunner(t, mistyped, nil, nil)
	if _, err := r.Teardown(context.Background(), nil, "completed", &fakeVerbs{}); err == nil ||
		!strings.Contains(err.Error(), "teardown payload") {
		t.Errorf("Teardown mistyped payload: got %v", err)
	}
	r = fakeRunner(t, mistyped, nil, nil)
	req := &ProvisionRequest{Source: ProvisionSource{Kind: "f", Path: "/b"}}
	if _, err := r.Provision(context.Background(), req, &fakeVerbs{}); err == nil ||
		!strings.Contains(err.Error(), "provision payload") {
		t.Errorf("Provision mistyped payload: got %v", err)
	}
}

func TestPutFileForbiddenOutsideProvision(t *testing.T) {
	// Healthcheck runs with verbs but without a source guard: put_file has
	// no legitimate use there and must be refused per §4.2.
	call := `{"call_id":"c1","verb":"put_file","args":{"source_path":"/b","dest_path":"/tmp/x"}}`
	r := fakeRunner(t, relayScript(call), nil, nil)
	verbs := &fakeVerbs{}
	_, err := r.Healthcheck(context.Background(), &Connection{}, nil, verbs)
	aerr := asAdapterError(t, err)
	if aerr.Code != CodeInvalidRequest || !strings.Contains(aerr.Message, "not permitted") {
		t.Errorf("relayed outcome = %+v, want invalid_request not-permitted", aerr)
	}
	if len(verbs.putCalls) != 0 {
		t.Error("forbidden put_file must not reach the sandbox")
	}
}

func TestSandboxResultWriteRaces(t *testing.T) {
	// The adapter asks for a verb and stops listening. Whether our result
	// write lands in the pipe buffer or hits EPIPE depends on scheduling
	// (it flapped on loaded CI runners), so a closed pipe must never be
	// the verdict: both orderings converge on the read loop's
	// classification of the adapter's exit.
	call := `printf '{"protocol":"probavi-adapter/0","request_id":"%s","sandbox_call":{"call_id":"c1","verb":"exec","args":{"argv":["x"]}}}\n' "$RID"`
	scripts := map[string]string{
		"stdin closes after the call":  prelude + call + "\n" + `exec 0<&-` + "\n" + `sleep 0.3` + "\n" + `exit 0`,
		"stdin closes before the call": prelude + `exec 0<&-` + "\n" + call + "\n" + `sleep 0.3` + "\n" + `exit 0`,
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			r := fakeRunner(t, script, nil, nil)
			req := &ProvisionRequest{Source: ProvisionSource{Kind: "f", Path: "/b"}}
			_, err := r.Provision(context.Background(), req, &fakeVerbs{})
			aerr := asAdapterError(t, err)
			if aerr.Code != CodeAdapterCrash || !strings.Contains(aerr.Message, "without a final response") {
				t.Errorf("error = %+v, want the read loop's deterministic classification", aerr)
			}
		})
	}
}

func TestAdapterLingersAfterFinal(t *testing.T) {
	script := prelude + probeFinal(probePayload) + `sleep 30`
	r := fakeRunner(t, script, nil, &Options{Grace: 200 * time.Millisecond})
	start := time.Now()
	_, err := r.Probe(context.Background())
	aerr := asAdapterError(t, err)
	if aerr.Code != CodeAdapterCrash {
		t.Errorf("error = %+v, want crash — an adapter must exit after its final response", aerr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("reaping took %v, the lingering bound is not enforced", elapsed)
	}
}

func TestTransportErrorPropagation(t *testing.T) {
	if _, err := fakeRunner(t, "exit 1", nil, nil).Healthcheck(context.Background(), &Connection{}, nil, &fakeVerbs{}); err == nil {
		t.Error("Healthcheck must propagate transport failures")
	}
	if _, err := fakeRunner(t, "exit 1", nil, nil).Teardown(context.Background(), nil, "completed", &fakeVerbs{}); err == nil {
		t.Error("Teardown must propagate transport failures")
	}
}

func TestWriteRequestClosedPipe(t *testing.T) {
	// The adapter closes its stdin immediately; the oversized request
	// cannot fit the pipe buffer, so the core's write deterministically
	// hits a closed pipe. That is not a verdict — the adapter's own
	// conduct decides: with the small requests of real operations the same
	// exits race the write (CI flake, 2026-07-31), and both orders must
	// classify identically.
	tests := []struct {
		name       string
		exit       string
		wantSubstr string
	}{
		{"clean exit without response", "exit 0", "without a final response"},
		{"crash exit carries status", "exit 3", "exit status 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := `exec 0<&-` + "\n" + `sleep 0.3` + "\n" + tt.exit
			r := fakeRunner(t, script, nil, nil)
			req := &ProvisionRequest{
				Source:  ProvisionSource{Kind: "f", Path: "/b"},
				Options: map[string]string{"pad": strings.Repeat("x", 256*1024)},
			}
			_, err := r.Provision(context.Background(), req, &fakeVerbs{})
			aerr := asAdapterError(t, err)
			if aerr.Code != CodeAdapterCrash || !strings.Contains(aerr.Message, tt.wantSubstr) {
				t.Errorf("error = %+v, want %s containing %q", aerr, CodeAdapterCrash, tt.wantSubstr)
			}
		})
	}
}

func TestFinishReapsWithoutWait(t *testing.T) {
	// finish() is the cleanup of early-error paths that return before the
	// normal wait; it must kill and reap a lingering adapter promptly, not
	// sit out its runtime.
	r := fakeRunner(t, "sleep 5", nil, nil)
	s, err := r.start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	start := time.Now()
	s.finish()
	if !s.waited {
		t.Error("finish must reap the process")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("finish took %v, must kill rather than wait out the adapter", elapsed)
	}
}

func TestTeardown(t *testing.T) {
	// The script itself asserts that empty state normalizes to {} and the
	// reason arrives verbatim; on mismatch it exits 1 → adapter_crash.
	script := prelude + `case "$REQ" in *'"state":{},"reason":"failed"'*) ;; *) exit 1 ;; esac` + "\n" +
		probeFinal(`{"released":true}`)
	r := fakeRunner(t, script, nil, nil)
	res, err := r.Teardown(context.Background(), nil, "failed", &fakeVerbs{})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !res.Released {
		t.Errorf("result = %+v", res)
	}

	if _, err := r.Teardown(context.Background(), nil, "because", &fakeVerbs{}); err == nil || !strings.Contains(err.Error(), "invalid reason") {
		t.Errorf("invalid reason: got %v", err)
	}
}

func TestCancellationCleanExit(t *testing.T) {
	script := prelude +
		`trap 'printf "{\"protocol\":\"probavi-adapter/0\",\"request_id\":\"%s\",\"ok\":false,\"error\":{\"code\":\"cancelled\",\"message\":\"stopping\",\"retryable\":true}}\n" "$RID"; exit 0' TERM` + "\n" +
		`sleep 30 &` + "\n" + `wait $!` + "\n"
	r := fakeRunner(t, script, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_, err := r.Probe(ctx)
	if aerr := asAdapterError(t, err); aerr.Code != CodeCancelled {
		t.Errorf("error = %+v, want cancelled — a SIGTERM-aware adapter is not a crash", aerr)
	}
}

func TestStubbornAdapterKilled(t *testing.T) {
	script := `trap '' TERM` + "\n" + prelude + `sleep 30`
	r := fakeRunner(t, script, nil, &Options{Grace: 200 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := r.Probe(ctx)
	if aerr := asAdapterError(t, err); aerr.Code != CodeAdapterCrash {
		t.Errorf("error = %+v, want adapter_crash after SIGKILL", aerr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("kill took %v — the grace period is not being enforced", elapsed)
	}
}

func TestOutputAfterFinalIsIgnored(t *testing.T) {
	script := prelude + probeFinal(probePayload) + `echo trailing-garbage` + "\n" + `exit 0`
	r := fakeRunner(t, script, nil, nil)
	if _, err := r.Probe(context.Background()); err != nil {
		t.Errorf("Probe: %v — output after the final response is logged and ignored, not fatal", err)
	}
}

func TestNewResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probavi-adapter-real")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write adapter: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r, err := New("real", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.opts.Grace != defaultGrace {
		t.Errorf("grace = %v, want default %v", r.opts.Grace, defaultGrace)
	}
	// The core hashes this path into adapter.digest, so a runner must
	// report the file it actually resolved — not the name it was asked for
	// (evidence-schema.md §3).
	if r.Path() != path {
		t.Errorf("Path() = %q, want the resolved executable %q", r.Path(), path)
	}
	if _, err := New("definitely-not-installed", nil, nil); err == nil {
		t.Error("New must fail for unresolvable adapters")
	}
}

// guardTree builds a backup source with one escape route of every kind
// beside the legitimate files, and returns the source root.
func guardTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "backups", "orders")
	if err := os.MkdirAll(filepath.Join(root, "2026"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, p := range []string{
		filepath.Join(root, "full.dump"),
		filepath.Join(root, "2026", "full.dump"),
		filepath.Join(base, "secrets"),
	} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	for target, link := range map[string]string{
		"/etc":                           filepath.Join(root, "etc-link"),
		"../../secrets":                  filepath.Join(root, "escape-link"),
		"full.dump":                      filepath.Join(root, "inside-link"),
		filepath.Join(root, "full.dump"): filepath.Join(root, "abs-link"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
	}
	return root
}

// TestSourceGuard exercises containment against a real tree. The §4.2
// guarantee is about the file the provider ends up opening, and a lexical
// prefix test cannot see a symlink: it accepts <source>/etc-link/hostname
// and the provider reads /etc/hostname.
func TestSourceGuard(t *testing.T) {
	root := guardTree(t)
	guard := sourceGuard(root)
	for name, tt := range map[string]struct {
		path   string
		wantOK bool
	}{
		"the source itself":             {root, true},
		"a file beneath it":             {filepath.Join(root, "full.dump"), true},
		"a nested file":                 {filepath.Join(root, "2026", "full.dump"), true},
		"a path that cleans to inside":  {filepath.Join(root, "..", "orders", "full.dump"), true},
		"a symlink staying inside":      {filepath.Join(root, "inside-link"), true},
		"a symlink to a directory out":  {filepath.Join(root, "etc-link", "hostname"), false},
		"a symlink to a file out":       {filepath.Join(root, "escape-link"), false},
		"an absolute symlink":           {filepath.Join(root, "abs-link"), false},
		"the sibling prefix trick":      {root + "-evil/x", false},
		"a path that cleans to outside": {filepath.Join(root, "..", "secrets"), false},
		"an unrelated path":             {"/etc/passwd", false},
		"a file that does not exist":    {filepath.Join(root, "missing.dump"), false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := guard(tt.path)
			if (err == nil) != tt.wantOK {
				t.Fatalf("guard(%s) = %q, %v; want ok=%v", tt.path, got, err, tt.wantOK)
			}
			if tt.wantOK && got != filepath.Clean(tt.path) {
				t.Errorf("guard returned %q, want the cleaned request %q", got, filepath.Clean(tt.path))
			}
			if err != nil && strings.ContainsAny(err.Error(), "\"\n") {
				t.Errorf("refusal %q must stay a single quote-free line: it crosses the protocol as a JSON string", err)
			}
		})
	}
}

// TestSourceGuardAcceptsASymlinkedSource: the configured source may
// itself be a symlink — /backups/latest -> /mnt/disk2/2026-08-29 is an
// ordinary layout, and the operator chose it. Containment starts at what
// that path resolves to; only symlinks found *inside* it are escapes.
func TestSourceGuardAcceptsASymlinkedSource(t *testing.T) {
	root := guardTree(t)
	alias := filepath.Join(t.TempDir(), "latest")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	guard := sourceGuard(alias)
	if _, err := guard(filepath.Join(alias, "full.dump")); err != nil {
		t.Errorf("guard refused a file under a symlinked source: %v", err)
	}
	if _, err := guard(filepath.Join(alias, "etc-link", "hostname")); err == nil {
		t.Error("guard accepted an escape from a symlinked source")
	}
}

// TestSourceGuardRefusesAnUnusableSource covers a source that cannot
// contain anything: nothing lives beneath a regular file, and the guard
// must not fall back to the lexical answer for it.
func TestSourceGuardRefusesAnUnusableSource(t *testing.T) {
	root := guardTree(t)
	file := filepath.Join(root, "full.dump")
	guard := sourceGuard(file)
	if _, err := guard(file); err != nil {
		t.Errorf("guard refused the configured source itself: %v", err)
	}
	if _, err := guard(filepath.Join(file, "anything")); err == nil {
		t.Error("guard accepted a path beneath a regular file")
	}
	if _, err := sourceGuard(filepath.Join(root, "nonexistent"))(filepath.Join(root, "nonexistent", "x")); err == nil {
		t.Error("guard accepted a path beneath a source that does not exist")
	}
}

func TestErrorString(t *testing.T) {
	e := &Error{Code: "timeout", Message: "too slow"}
	if got := e.Error(); got != "adapter error timeout: too slow" {
		t.Errorf("Error() = %q", got)
	}
	if c := crashf("x %d", 1); c.Code != CodeAdapterCrash || c.Message != "x 1" || c.Retryable {
		t.Errorf("crashf = %+v", c)
	}
}

// TestBuildEnvDeduplicates covers a credential named like a baseline
// variable. Passing it twice would leave which one the adapter sees to
// exec's last-wins rule instead of to the §2.5 allow-list.
func TestBuildEnvDeduplicates(t *testing.T) {
	t.Setenv("PATH", "/probavi-test-bin")
	t.Setenv("PROBAVI_TEST_CRED", "secret")
	r := &Runner{opts: Options{CredentialEnv: []string{"PATH", "PROBAVI_TEST_CRED", "PATH"}}}

	env := r.buildEnv()
	seen := map[string]int{}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		seen[name]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times in the adapter environment", name, n)
		}
	}
	if !slices.Contains(env, "PATH=/probavi-test-bin") || !slices.Contains(env, "PROBAVI_TEST_CRED=secret") {
		t.Errorf("env = %v, want both variables present once", env)
	}
}
