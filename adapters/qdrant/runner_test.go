package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// parseBash asks bash to read a script without running it, which is the
// only compiler these two shell programs have.
func parseBash(t *testing.T, script string) error {
	t.Helper()
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runCheck runs the declared check runner against a stand-in engine on
// loopback. The script is the shipped one with its port rewritten, which
// is the only thing a test can parameterise: the runner's argv is fixed by
// the probe response, so the endpoint is a constant in production.
func runCheck(t *testing.T, addr, collection, text string) (stdout, stderr string, exitCode int) {
	t.Helper()
	script := strings.ReplaceAll(checkScript,
		"/dev/tcp/127.0.0.1/"+httpPort, "/dev/tcp/"+addr)
	cmd := exec.Command("bash", "-c", script, "bash", collection, text)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec.ExitError is not wrapped here
		exitCode = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run check script: %v", err)
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// recorded is what the stand-in engine saw.
type recorded struct {
	method, path, body string
}

// engineStub answers on loopback exactly as Qdrant does: content-length
// rather than chunked encoding, and a JSON body the runner has to reduce.
func engineStub(t *testing.T, status int, body string, seen *recorded) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body) //nolint:errcheck // test stub
		*seen = recorded{method: r.Method, path: r.URL.RequestURI(), body: string(raw)}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", srv.URL, err)
	}
	return host + "/" + port
}

// TestTheRunnerSpeaksHTTPWithoutAnHTTPClient is the check runner's whole
// contract, exercised against a real socket. The official Qdrant image
// carries no curl, wget, nc or python3 (measured), so the runner talks to
// the engine through bash's /dev/tcp — and that is a shell program, which
// only a shell can test.
func TestTheRunnerSpeaksHTTPWithoutAnHTTPClient(t *testing.T) {
	for _, tc := range []struct {
		name, collection, text string
		status                 int
		body                   string
		wantMethod, wantPath   string
		wantBody, wantStdout   string
		wantExit               int
	}{
		{
			name:       "an absolute path is taken as written",
			collection: "orders", text: "/collections/orders",
			status: 200, body: `{"result":{"points_count":1000,"segments_count":8},"status":"ok"}`,
			wantMethod: "GET", wantPath: "/collections/orders", wantStdout: "1000\n",
		},
		{
			name:       "a relative path hangs off the restored collection",
			collection: "orders", text: "points/count",
			status: 200, body: `{"result":{"count":42},"status":"ok"}`,
			wantMethod: "GET", wantPath: "/collections/orders/points/count", wantStdout: "42\n",
		},
		{
			name:       "a body turns the check into a POST",
			collection: "orders",
			text:       `points/count {"exact":true,"filter":{"must":[{"key":"region","match":{"value":"eu"}}]}}`,
			status:     200, body: `{"result":{"count":500},"status":"ok"}`,
			wantMethod: "POST", wantPath: "/collections/orders/points/count",
			wantBody:   `{"exact":true,"filter":{"must":[{"key":"region","match":{"value":"eu"}}]}}`,
			wantStdout: "500\n",
		},
		{
			name:       "a query string survives",
			collection: "orders", text: "/collections/orders/exists?x=1",
			status: 200, body: `{"result":{"exists":true},"status":"ok"}`,
			wantMethod: "GET", wantPath: "/collections/orders/exists?x=1",
			wantStdout: `{"result":{"exists":true},"status":"ok"}` + "\n",
		},
		{
			name:       "the status line is the verdict",
			collection: "orders", text: "/collections/gone",
			status: 404, body: `{"status":{"error":"Not found"}}`,
			wantMethod: "GET", wantPath: "/collections/gone", wantExit: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen recorded
			addr := engineStub(t, tc.status, tc.body, &seen)
			stdout, stderr, exit := runCheck(t, addr, tc.collection, tc.text)
			if exit != tc.wantExit {
				t.Fatalf("exit %d, want %d (stderr: %s)", exit, tc.wantExit, strings.TrimSpace(stderr))
			}
			if seen.method != tc.wantMethod || seen.path != tc.wantPath {
				t.Errorf("engine saw %s %s, want %s %s", seen.method, seen.path, tc.wantMethod, tc.wantPath)
			}
			if seen.body != tc.wantBody {
				t.Errorf("engine saw body %q, want %q", seen.body, tc.wantBody)
			}
			if tc.wantExit == 0 && stdout != tc.wantStdout {
				t.Errorf("runner printed %q, want %q", stdout, tc.wantStdout)
			}
			if tc.wantExit != 0 && !strings.Contains(stderr, "qdrant answered 404") {
				t.Errorf("a refused check must say what the engine answered, got %q", stderr)
			}
		})
	}
}

// TestTheRunnerRefusesADeadEngineRatherThanPassing covers the case a
// status-code verdict cannot see: nothing listening at all. curl would
// exit non-zero here; /dev/tcp fails at the redirection, which is a
// different shape and needs its own guard.
func TestTheRunnerRefusesADeadEngineRatherThanPassing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	stdout, stderr, exit := runCheck(t, host+"/"+port, "orders", "/collections/orders")
	if exit == 0 {
		t.Fatalf("a check against a dead engine passed, printing %q", stdout)
	}
	if !strings.Contains(stderr, "not listening") {
		t.Errorf("stderr %q does not say the engine is not listening", strings.TrimSpace(stderr))
	}
}
