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

// parseSh asks sh to read a script without running it, which is the only
// compiler these shell programs have. The scripts run under busybox ash
// in the sandbox; POSIX sh syntax is the common ground both speak.
func parseSh(t *testing.T, script string) error {
	t.Helper()
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// TestScriptsAreValidSh catches a quoting or redirection mistake in the
// shell programs this adapter ships without needing an engine: they are
// the parts a Go compiler cannot check.
func TestScriptsAreValidSh(t *testing.T) {
	for name, script := range map[string]string{
		"checkScript":       checkScript,
		"preflightScript":   preflightScript,
		"locateScript":      locateScript,
		"startEngineScript": startEngineScript,
		"restoreScript":     restoreScript,
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseSh(t, script); err != nil {
				t.Errorf("%s is not valid sh: %v", name, err)
			}
		})
	}
}

// runCheck runs the declared check runner against a stand-in engine on
// loopback. The script is the shipped one with its port rewritten, which
// is the only thing a test can parameterise: the runner's argv is fixed
// by the probe response, so the endpoint is a constant in production.
// The host's wget stands in for busybox wget; the flags the script uses
// (-q, -O, -T, --header, --post-data) mean the same in both.
func runCheck(t *testing.T, addr, class, text string) (stdout, stderr string, exitCode int) {
	t.Helper()
	script := strings.ReplaceAll(checkScript, "127.0.0.1:"+httpPort, addr)
	cmd := exec.Command("sh", "-c", script, "sh", class, text)
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

// engineStub answers on loopback with a fixed status and body.
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
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestTheRunnerSpeaksTheEnginesOwnLanguage is the check runner's whole
// contract, exercised against a real socket: paths as written, a body
// making the POST, bare text becoming the GraphQL query, the count
// reduction, and the two refusal shapes — a non-2xx status and the
// HTTP-200 GraphQL error Weaviate answers with (measured).
func TestTheRunnerSpeaksTheEnginesOwnLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, class, text    string
		status               int
		body                 string
		wantMethod, wantPath string
		wantBody, wantStdout string
		wantExit             int
	}{
		{
			name:  "an absolute path is taken as written",
			class: "Books", text: "/v1/schema",
			status: 200, body: `{"classes":[{"class":"Books"}]}`,
			wantMethod: "GET", wantPath: "/v1/schema",
			wantStdout: `{"classes":[{"class":"Books"}]}` + "\n",
		},
		{
			name:  "a relative path hangs off /v1",
			class: "Books", text: "objects?class=Books&limit=1",
			status: 200, body: `{"objects":[{"id":"x"}]}`,
			wantMethod: "GET", wantPath: "/v1/objects?class=Books&limit=1",
			wantStdout: `{"objects":[{"id":"x"}]}` + "\n",
		},
		{
			name:  "a body turns the check into a POST",
			class: "Books", text: `/v1/graphql {"query":"{Aggregate{Books{meta{count}}}}"}`,
			status: 200, body: `{"data":{"Aggregate":{"Books":[{"meta":{"count":1000}}]}}}`,
			wantMethod: "POST", wantPath: "/v1/graphql",
			wantBody:   `{"query":"{Aggregate{Books{meta{count}}}}"}`,
			wantStdout: "1000\n",
		},
		{
			name:  "bare text is the GraphQL query",
			class: "Books", text: `{Aggregate{Books(where:{path:["region"],operator:Equal,valueText:"eu"}){meta{count}}}}`,
			status: 200, body: `{"data":{"Aggregate":{"Books":[{"meta":{"count":500}}]}}}`,
			wantMethod: "POST", wantPath: "/v1/graphql",
			wantBody:   `{"query":"{Aggregate{Books(where:{path:[\"region\"],operator:Equal,valueText:\"eu\"}){meta{count}}}}"}`,
			wantStdout: "500\n",
		},
		{
			name:  "the status line is the verdict",
			class: "Books", text: "/v1/backups/filesystem/gone",
			status: 404, body: `{"error":[{"message":"not found"}]}`,
			wantMethod: "GET", wantPath: "/v1/backups/filesystem/gone", wantExit: 1,
		},
		{
			name:  "a 200 carrying a GraphQL error is a refusal",
			class: "Books", text: "{Aggregate{Ghost{meta{count}}}}",
			status: 200, body: `{"data":{"Aggregate":null},"errors":[{"message":"Cannot query field"}]}`,
			wantMethod: "POST", wantPath: "/v1/graphql",
			wantBody: `{"query":"{Aggregate{Ghost{meta{count}}}}"}`, wantExit: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen recorded
			addr := engineStub(t, tc.status, tc.body, &seen)
			stdout, stderr, exit := runCheck(t, addr, tc.class, tc.text)
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
			if tc.wantExit != 0 && !strings.Contains(stderr, "weaviate answered") {
				t.Errorf("a refused check must say what the engine answered, got %q", stderr)
			}
		})
	}
}

// TestTheRunnerRefusesADeadEngineRatherThanPassing covers the case a
// status-code verdict cannot see: nothing listening at all.
func TestTheRunnerRefusesADeadEngineRatherThanPassing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	stdout, _, exit := runCheck(t, addr, "Books", "/v1/schema")
	if exit == 0 {
		t.Fatalf("a check against a dead engine passed, printing %q", stdout)
	}
}
