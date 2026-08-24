package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/probavi/probavi/internal/push"
)

// push_test.go covers `probavi push` end to end through run(): the exit
// codes of docs/evidence-push.md §8 and the diagnostics an operator reads
// in a cron job's mail.

const pushLogBody = `{"schema":"probavi-evidence/2","seq":1,"outcome":"pass"}
`

// receiver is a stand-in collector: it records one request and answers
// with a fixed status and body.
type receiver struct {
	mu     sync.Mutex
	hits   int
	auth   string
	target string
	body   string
}

func newReceiver(t *testing.T, status int, answer string) (*receiver, string) {
	t.Helper()
	r := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		r.mu.Lock()
		r.hits++
		r.auth = req.Header.Get("Authorization")
		r.target = req.URL.Path
		r.body = string(body)
		r.mu.Unlock()
		w.WriteHeader(status)
		if answer != "" {
			if _, err := w.Write([]byte(answer)); err != nil {
				t.Errorf("write response: %v", err)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return r, srv.URL
}

func (r *receiver) seen() (hits int, auth, target, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits, r.auth, r.target, r.body
}

func writeLog(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

func TestPushDeliversTheLogAndReportsIt(t *testing.T) {
	logPath := writeLog(t, "prod.jsonl", pushLogBody)
	recv, url := newReceiver(t, http.StatusAccepted, "")
	t.Setenv(push.DefaultTokenEnv, "operator-token")

	code, stdout, stderr := runCLI(t, "push", "--log", logPath, "--to", url+"/ingest")
	if code != exitPass {
		t.Fatalf("push exit %d, want 0 (stderr: %s)", code, stderr)
	}
	var out pushOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("push output is not JSON: %v (%q)", err, stdout)
	}
	if out.Status != http.StatusAccepted || out.Bytes != len(pushLogBody) || out.Path != "prod.jsonl" {
		t.Errorf("push output = %+v, want 202 / %d bytes / prod.jsonl", out, len(pushLogBody))
	}
	if strings.Contains(stdout, url) {
		t.Errorf("the summary carries the destination, which may be a credential: %s", stdout)
	}
	hits, auth, target, body := recv.seen()
	if hits != 1 {
		t.Errorf("%d requests, want 1", hits)
	}
	if auth != "Bearer operator-token" {
		t.Errorf("Authorization = %q, want the token from the environment", auth)
	}
	if target != "/ingest/prod.jsonl" {
		t.Errorf("request path = %q, want /ingest/prod.jsonl", target)
	}
	if body != pushLogBody {
		t.Errorf("body = %q, want the log verbatim", body)
	}
}

// TestPushKeepsTheLogUntouched is the guarantee stated in every layer:
// the command copies, it never moves or truncates.
func TestPushKeepsTheLogUntouched(t *testing.T) {
	logPath := writeLog(t, "prod.jsonl", pushLogBody)
	before, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	_, url := newReceiver(t, http.StatusOK, "")
	t.Setenv(push.DefaultTokenEnv, "operator-token")

	if code, _, stderr := runCLI(t, "push", "--log", logPath, "--to", url); code != exitPass {
		t.Fatalf("push exit %d, want 0 (stderr: %s)", code, stderr)
	}
	after, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat after push: %v", err)
	}
	kept, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log after push: %v", err)
	}
	if string(kept) != pushLogBody || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the log changed: %q, %d bytes at %s", kept, after.Size(), after.ModTime())
	}
}

// TestPushRefusalIsPrintedAsItArrives: a receiver that names its reason —
// out of licence, log too large, path not accepted — is quoted, because
// that line is the whole of what the operator gets by mail.
func TestPushRefusalIsPrintedAsItArrives(t *testing.T) {
	logPath := writeLog(t, "prod.jsonl", pushLogBody)
	recv, url := newReceiver(t, http.StatusPaymentRequired, "licence expired on 2026-08-01")
	t.Setenv(push.DefaultTokenEnv, "operator-token")

	code, stdout, stderr := runCLI(t, "push", "--log", logPath, "--to", url)
	if code != exitPushFailed {
		t.Fatalf("push exit %d, want %d", code, exitPushFailed)
	}
	if stdout != "" {
		t.Errorf("a failed push printed a summary: %q", stdout)
	}
	for _, want := range []string{"402", "licence expired on 2026-08-01"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not contain %q", stderr, want)
		}
	}
	if hits, _, _, _ := recv.seen(); hits != 1 {
		t.Errorf("%d requests, want 1 — a refusal is not retried", hits)
	}
}

// TestPushUnauthenticatedIsDeliberate: the flag has to be typed in full,
// and it is the only way a push goes out without a token.
func TestPushUnauthenticatedIsDeliberate(t *testing.T) {
	logPath := writeLog(t, "prod.jsonl", pushLogBody)
	recv, url := newReceiver(t, http.StatusOK, "")
	t.Setenv(push.DefaultTokenEnv, "")

	code, _, stderr := runCLI(t, "push", "--log", logPath, "--to", url, "--allow-unauthenticated")
	if code != exitPass {
		t.Fatalf("push exit %d, want 0 (stderr: %s)", code, stderr)
	}
	if _, auth, _, _ := recv.seen(); auth != "" {
		t.Errorf("Authorization = %q, want none", auth)
	}
}

// TestPushEmptyLogSaysSo: an empty log is a truthful state and exits 0
// exactly as a log of proven drills does, so the difference is stated.
func TestPushEmptyLogSaysSo(t *testing.T) {
	logPath := writeLog(t, "prod.jsonl", "")
	recv, url := newReceiver(t, http.StatusOK, "")
	t.Setenv(push.DefaultTokenEnv, "operator-token")

	code, _, stderr := runCLI(t, "push", "--log", logPath, "--to", url)
	if code != exitPass {
		t.Fatalf("push exit %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "holds no records") {
		t.Errorf("stderr does not report the empty log: %q", stderr)
	}
	if _, _, _, body := recv.seen(); body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

// TestPushUsageErrors covers everything that must fail before a byte is
// read or sent, all of it exit 3 — distinct from a delivery failure, so a
// timer can tell "misconfigured" from "receiver unreachable".
func TestPushUsageErrors(t *testing.T) {
	logPath := writeLog(t, "prod.jsonl", pushLogBody)
	accented := writeLog(t, "napló.jsonl", pushLogBody)
	recv, url := newReceiver(t, http.StatusOK, "")

	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no log", []string{"push", "--to", url}, nil, "--log is required"},
		{"no destination", []string{"push", "--log", logPath}, nil, "exactly one of --to or --to-env"},
		{"both destinations", []string{"push", "--log", logPath, "--to", url, "--to-env", "PROBAVI_TEST_URL"}, nil, "not both"},
		{"token and anonymous", []string{"push", "--log", logPath, "--to", url, "--allow-unauthenticated", "--token-env", "PROBAVI_TEST_TOKEN"}, nil, "cannot both be given"},
		{"unset token variable", []string{"push", "--log", logPath, "--to", url}, map[string]string{push.DefaultTokenEnv: ""}, push.DefaultTokenEnv},
		{"unset destination variable", []string{"push", "--log", logPath, "--to-env", "PROBAVI_TEST_URL"}, map[string]string{"PROBAVI_TEST_URL": ""}, "PROBAVI_TEST_URL"},
		{"unset secret variable", []string{"push", "--log", logPath, "--to", url, "--secret-env", "PROBAVI_TEST_SECRET"}, map[string]string{"PROBAVI_TEST_SECRET": ""}, "PROBAVI_TEST_SECRET"},
		{"bad path", []string{"push", "--log", logPath, "--to", url, "--path", "../secrets"}, nil, "--path"},
		{"unusable derived path", []string{"push", "--log", accented, "--to", url}, nil, "pass --path"},
		{"missing log file", []string{"push", "--log", filepath.Join(t.TempDir(), "absent.jsonl"), "--to", url}, nil, "absent.jsonl"},
		{"destination is not a URL", []string{"push", "--log", logPath, "--to", "collector.example/ingest"}, nil, "absolute http(s) URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(push.DefaultTokenEnv, "operator-token")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			code, stdout, stderr := runCLI(t, tc.args...)
			if code != exitUsage {
				t.Fatalf("exit %d, want %d (stderr: %s)", code, exitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("a rejected push printed a summary: %q", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not contain %q", stderr, tc.want)
			}
		})
	}
	if hits, _, _, _ := recv.seen(); hits != 0 {
		t.Errorf("%d requests reached the receiver, want 0 — nothing is sent on a usage error", hits)
	}
}

// TestPushDiagnosticsNeverEchoCredentials: the token and the destination
// are credentials, and a diagnostic is the easiest place to leak one.
func TestPushDiagnosticsNeverEchoCredentials(t *testing.T) {
	logPath := writeLog(t, "prod.jsonl", pushLogBody)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()
	t.Setenv("PROBAVI_TEST_URL", dead+"/ingest?token=url-secret")
	t.Setenv(push.DefaultTokenEnv, "bearer-secret")

	code, _, stderr := runCLI(t, "push", "--log", logPath, "--to-env", "PROBAVI_TEST_URL")
	if code != exitPushFailed {
		t.Fatalf("push exit %d, want %d (stderr: %s)", code, exitPushFailed, stderr)
	}
	for _, secret := range []string{"url-secret", "bearer-secret"} {
		if strings.Contains(stderr, secret) {
			t.Errorf("stderr leaks %q: %s", secret, stderr)
		}
	}
}
