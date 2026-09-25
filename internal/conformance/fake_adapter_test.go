package conformance

// The fake adapter the suite's own tests run against: a minimal but fully
// conformant probavi-adapter/0 implementation whose PROBAVI_FAKE_ADAPTER
// modes switch on one protocol violation each, so every §10 check is
// proven to fail when it should. It is written against the protocol doc
// only — like a third-party adapter would be.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type fakeAdapter struct {
	mode string
	rid  string
	in   *bufio.Scanner
	out  *os.File
	sig  chan os.Signal
}

func fakeMain(mode string) int {
	f := &fakeAdapter{mode: mode, out: os.Stdout, sig: make(chan os.Signal, 1)}
	if mode == "ignore-sigterm" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(f.sig, syscall.SIGTERM)
	}
	if mode == "exit-before-read" {
		// Never touches stdin: the driver's write has nowhere to go.
		return 0
	}
	f.in = bufio.NewScanner(os.Stdin)
	f.in.Buffer(make([]byte, 64*1024), maxLineBytes)
	if !f.in.Scan() {
		return 1
	}
	req := struct {
		Protocol  string          `json:"protocol"`
		RequestID string          `json:"request_id"`
		Op        string          `json:"op"`
		Payload   json.RawMessage `json:"payload"`
	}{}
	if err := json.Unmarshal(f.in.Bytes(), &req); err != nil {
		return 1
	}
	f.rid = req.RequestID
	if mode == "wrong-echo" {
		f.rid = "wrong-" + req.RequestID
	}
	if req.Protocol != protocolVersion {
		detail := map[string]any{"supported": []string{protocolVersion}}
		if mode == "no-detail" {
			detail = nil
		}
		return f.finalError("unsupported_protocol", detail)
	}
	switch req.Op {
	case "probe":
		return f.probe()
	case "provision":
		return f.provision(req.Payload)
	case "healthcheck":
		return f.healthcheck()
	case "teardown":
		return f.teardown()
	default:
		return f.finalError("invalid_request", nil)
	}
}

func (f *fakeAdapter) probe() int {
	switch f.mode {
	case "stdout-noise":
		fmt.Fprintln(f.out, "log: starting up") // the violation: logs belong on stderr
	case "huge-frame":
		fmt.Fprintln(f.out, strings.Repeat("x", maxLineBytes+1))
	case "crash-on-probe":
		return 3
	case "hang-on-probe":
		time.Sleep(time.Hour)
	case "probe-sandbox-call":
		f.call("exec", map[string]any{"argv": []string{"true"}})
	}
	if f.mode == "bare-message" {
		f.write(map[string]any{"protocol": protocolVersion, "request_id": f.rid})
	}
	if f.mode == "probe-payload-not-object" {
		// §6.1 says the payload is an object; this one is a string, and
		// the suite has to say so rather than fail decoding it.
		f.write(map[string]any{
			"protocol": protocolVersion, "request_id": f.rid, "ok": true, "payload": "not an object",
		})
		return 0
	}
	code := f.finalOK(f.probePayload())
	if f.mode == "double-final" {
		f.finalOK(map[string]any{"name": "fake-again"})
	}
	if f.mode == "self-destruct" {
		// The probe op itself succeeds; every later operation finds no
		// executable — the mid-suite harness-error path.
		if exe, err := os.Executable(); err == nil {
			if err := os.Remove(exe); err != nil {
				return 1
			}
		}
	}
	return code
}

// probePayload shapes the §6.1 payload, bending one field per violation
// mode.
func (f *fakeAdapter) probePayload() map[string]any {
	name, versions := "fake", []string{protocolVersion}
	sources := []map[string]any{{"kind": "fakedump", "capabilities": map[string]bool{"pitr": false}}}
	verbs := []string{"exec"}
	argv := []string{"fakesql", "-c", "{{sql}}"}
	switch f.mode {
	case "no-sql-placeholder":
		argv = []string{"fakesql", "-c", "literal"}
	case "password-in-argv":
		argv = []string{"fakesql", "-p", "{{password}}", "-c", "{{sql}}"}
	case "bad-name":
		name = "Fake Adapter!"
	case "no-protocol-version":
		versions = []string{"probavi-adapter/99"}
	case "empty-sources":
		sources = nil
	case "empty-kind-name":
		sources = []map[string]any{{"kind": ""}}
	case "bad-verb":
		verbs = []string{"teleport"}
	}
	payload := map[string]any{
		"name":              name,
		"adapter_version":   "0.0.1",
		"protocol_versions": versions,
		"engine":            map[string]string{"name": "fakedb"},
		"sources":           sources,
		"sql_runner":        map[string]any{"argv": argv, "env": map[string]string{}},
		"verbs_required":    verbs,
	}
	f.bendV1(payload)
	return payload
}

// bendV1 adds the §6.1.1 declarations, correctly for the v1 mode and
// wrongly for each violation the suite must catch.
func (f *fakeAdapter) bendV1(payload map[string]any) {
	switch f.mode {
	case "v1":
		payload["protocol_versions"] = []string{protocolVersion, protocolV1}
		payload["identifier"] = map[string]string{"open": "`", "close": "`", "separator": "."}
		payload["checks"] = map[string]any{
			"row_count": map[string]string{"statement": "SELECT COUNT(*) FROM {{table}}"},
		}
	case "v1-unknown-check-kind":
		payload["protocol_versions"] = []string{protocolVersion, protocolV1}
		payload["checks"] = map[string]any{
			"row_counts": map[string]string{"statement": "SELECT COUNT(*) FROM {{table}}"},
		}
	case "v1-stray-placeholder":
		payload["protocol_versions"] = []string{protocolVersion, protocolV1}
		payload["checks"] = map[string]any{
			"row_count": map[string]string{"statement": "SELECT COUNT(*) FROM {{table}} WHERE x = {{password}}"},
		}
	case "v1-undeclared-version":
		// The fields without the version that defines them.
		payload["identifier"] = map[string]string{"open": "`", "close": "`", "separator": "."}
	case "v1-half-quoted":
		payload["protocol_versions"] = []string{protocolVersion, protocolV1}
		payload["identifier"] = map[string]string{"open": "[", "close": "", "separator": "."}
	case "v1-long-separator":
		payload["protocol_versions"] = []string{protocolVersion, protocolV1}
		payload["identifier"] = map[string]string{"open": "", "close": "", "separator": "::"}
	}
}

func (f *fakeAdapter) provision(payload json.RawMessage) int {
	req := struct {
		Source struct {
			Kind string `json:"kind"`
			Path string `json:"path"`
		} `json:"source"`
	}{}
	if err := json.Unmarshal(payload, &req); err != nil {
		if f.mode == "crash-on-malformed" {
			return 2
		}
		return f.finalError("invalid_request", nil)
	}
	if req.Source.Kind != "fakedump" {
		return f.finalError("unsupported_source", nil)
	}
	raw, err := os.ReadFile(req.Source.Path)
	if err != nil {
		if f.mode == "wrong-missing-code" {
			return f.finalError("internal", nil)
		}
		return f.finalError("source_not_found", nil)
	}

	if f.mode != "no-calls-provision" {
		f.call("exec", map[string]any{"argv": []string{"fake_isready"}}) // readiness
	}
	if f.cancelled() {
		return f.finalError(f.cancelCode(), nil)
	}
	if f.mode == "weird-verb" {
		// The simulated sandbox must answer an undefined verb with an
		// error result, not a crash; this fake shrugs and proceeds.
		f.call("teleport", map[string]any{})
	}
	if f.mode == "put-file-provision" {
		// The other half of §4: the simulated sandbox has to answer a
		// put_file as readily as an exec.
		f.call("put_file", map[string]any{
			"source_path": req.Source.Path, "dest_path": "/tmp/probavi-fake", "mode": "0600",
		})
	}
	if f.mode == "die-after-first-call" {
		// Issues a call and leaves without reading its answer: the harness
		// meets a dead adapter mid-answer, which is a verdict about the
		// adapter rather than a failure of the suite. Stdin is closed
		// first, so the harness's answer always meets a closed pipe
		// rather than racing the exit.
		if err := os.Stdin.Close(); err != nil {
			return 1
		}
		f.write(map[string]any{
			"protocol": protocolVersion, "request_id": f.rid,
			"sandbox_call": map[string]any{"call_id": "c9", "verb": "exec",
				"args": map[string]any{"argv": []string{"true"}}},
		})
		return 0
	}
	if f.mode != "no-calls-provision" {
		f.call("exec", map[string]any{"argv": []string{"fake_restore"}}) // restore
	}
	if f.cancelled() {
		return f.finalError(f.cancelCode(), nil)
	}

	if f.mode == "provision-payload-not-object" {
		f.write(map[string]any{
			"protocol": protocolVersion, "request_id": f.rid, "ok": true, "payload": "not an object",
		})
		return 0
	}
	return f.finalOK(f.provisionResponse(raw))
}

// provisionResponse shapes the §6.2 payload, bending one field per
// violation mode.
func (f *fakeAdapter) provisionResponse(raw []byte) map[string]any {
	sum := sha256.Sum256(raw)
	checksum := "sha256:" + hex.EncodeToString(sum[:])
	if f.mode == "bad-checksum" {
		checksum = "md5:0000"
	}
	scheme := "fakedb"
	if f.mode == "empty-scheme" {
		scheme = ""
	}
	createdAt := "2026-08-01T00:00:00Z"
	if f.mode == "bad-created-at" {
		createdAt = "yesterday around noon"
	}
	var state any = map[string]any{"marker": "fake"}
	if f.mode == "bad-state" {
		state = []string{"not", "an", "object"}
	}
	restore := 0.001
	switch f.mode {
	case "bad-timings":
		restore = 9999
	case "negative-timings":
		restore = -1
	}
	return map[string]any{
		"connection": map[string]any{
			"scheme": scheme, "host": "127.0.0.1", "port": 1, "database": "fake", "user": "fake",
		},
		"source_identity": map[string]any{
			"checksum": checksum, "size_bytes": len(raw), "created_at": createdAt,
		},
		"timings": map[string]any{
			"engine_ready_seconds": 0.001, "transfer_seconds": 0.001, "restore_seconds": restore,
		},
		"state": state,
	}
}

func (f *fakeAdapter) healthcheck() int {
	f.call("exec", map[string]any{"argv": []string{"fake_ping"}})
	switch f.mode {
	case "unhealthy-error":
		return f.finalError("internal", nil) // the violation: unhealthy is ok:true
	case "negative-latency":
		return f.finalOK(map[string]any{"healthy": true, "latency_seconds": -0.5, "detail": "ok"})
	case "no-healthy-field":
		return f.finalOK(map[string]any{"latency_seconds": 0.001, "detail": "ok"})
	}
	return f.finalOK(map[string]any{"healthy": true, "latency_seconds": 0.001, "detail": "ok"})
}

// teardown succeeds statelessly; the teardown-third-fails mode counts
// invocations in PROBAVI_FAKE_COUNTER across processes and fails the third
// one — the second teardown of the idempotence pair.
func (f *fakeAdapter) teardown() int {
	if f.mode == "teardown-empty-fails" {
		return f.finalError("internal", nil)
	}
	if f.mode == "teardown-third-fails" {
		if bumpCounter(os.Getenv("PROBAVI_FAKE_COUNTER")) >= 3 {
			return f.finalError("internal", nil)
		}
	}
	return f.finalOK(map[string]any{"released": true})
}

func bumpCounter(path string) int {
	if path == "" {
		return 0
	}
	raw, _ := os.ReadFile(path)                              //nolint:errcheck // empty file on first use
	n, _ := strconv.Atoi(string(raw))                        //nolint:errcheck // empty parses as 0
	_ = os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o600) //nolint:errcheck // test fixture
	return n + 1
}

// cancelCode is "cancelled" per §2.4; the sigterm-wrong-code mode
// misreports the abandonment as an internal error.
func (f *fakeAdapter) cancelCode() string {
	if f.mode == "sigterm-wrong-code" {
		return "internal"
	}
	return "cancelled"
}

// cancelled reports (without blocking) whether SIGTERM arrived. The
// hang-on-sigterm mode "handles" the signal by hanging, so the harness has
// to SIGKILL after grace; the ignore-sigterm mode never even notices it.
func (f *fakeAdapter) cancelled() bool {
	select {
	case <-f.sig:
		if f.mode == "hang-on-sigterm" {
			time.Sleep(time.Hour)
		}
		return true
	default:
		return false
	}
}

// call issues one sandbox verb and waits for its result.
func (f *fakeAdapter) call(verb string, args map[string]any) {
	f.write(map[string]any{
		"protocol": protocolVersion, "request_id": f.rid,
		"sandbox_call": map[string]any{"call_id": "c1", "verb": verb, "args": args},
	})
	if !f.in.Scan() {
		os.Exit(1)
	}
}

func (f *fakeAdapter) finalOK(payload map[string]any) int {
	f.write(map[string]any{"protocol": protocolVersion, "request_id": f.rid, "ok": true, "payload": payload})
	return 0
}

func (f *fakeAdapter) finalError(code string, detail map[string]any) int {
	errObj := map[string]any{"code": code, "message": "fake adapter error", "retryable": false}
	if detail != nil {
		errObj["detail"] = detail
	}
	f.write(map[string]any{"protocol": protocolVersion, "request_id": f.rid, "ok": false, "error": errObj})
	return 0
}

func (f *fakeAdapter) write(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		os.Exit(1)
	}
	if _, err := f.out.Write(append(raw, '\n')); err != nil {
		os.Exit(1)
	}
}
