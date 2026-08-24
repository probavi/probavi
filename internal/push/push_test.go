package push

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/notify"
)

// logBody is a two-record evidence log — the bytes a push must reproduce
// exactly. Its content is opaque to this package on purpose: push parses
// nothing.
const logBody = `{"schema":"probavi-evidence/2","seq":1,"outcome":"pass"}
{"schema":"probavi-evidence/2","seq":2,"outcome":"pass"}
`

// capture records what a receiver saw, so a test can assert on the exact
// request rather than on the client's own view of it.
type capture struct {
	mu       sync.Mutex
	requests int
	method   string
	path     string
	query    string
	header   http.Header
	body     string
	length   int64
}

func (c *capture) record(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.method = r.Method
	c.path = r.URL.Path
	c.query = r.URL.RawQuery
	c.header = r.Header.Clone()
	c.body = string(body)
	c.length = r.ContentLength
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

// server answers with the given statuses in order, repeating the last one,
// and records every request it receives.
func server(t *testing.T, recv *capture, statuses ...int) *httptest.Server {
	t.Helper()
	if len(statuses) == 0 {
		statuses = []int{http.StatusOK}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		recv.record(r, body)
		i := min(recv.count()-1, len(statuses)-1)
		w.WriteHeader(statuses[i])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// client builds a Client with negligible backoff: the retry policy is
// asserted by request counts, not by making the suite wait for it.
func client(t *testing.T, o Options) *Client {
	t.Helper()
	if o.Path == "" {
		o.Path = "prod.jsonl"
	}
	if o.Version == "" {
		o.Version = "0.0.0-test"
	}
	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	return c
}

// TestDeliveryConstantsMatchTheNotificationRules pins what
// docs/evidence-push.md §7 states: the retry budget is the notification
// budget, deliberately. The two packages are separate so they can diverge
// on a spec change — this makes divergence a failing test rather than a
// silent difference between two things the docs call identical.
func TestDeliveryConstantsMatchTheNotificationRules(t *testing.T) {
	if Attempts != notify.Attempts {
		t.Errorf("Attempts = %d, notify.Attempts = %d", Attempts, notify.Attempts)
	}
	if AttemptTimeout != notify.AttemptTimeout {
		t.Errorf("AttemptTimeout = %s, notify.AttemptTimeout = %s", AttemptTimeout, notify.AttemptTimeout)
	}
	if Budget != notify.Budget {
		t.Errorf("Budget = %s, notify.Budget = %s", Budget, notify.Budget)
	}
	if HeaderSignature != notify.HeaderSignature || SignatureAlgorithm != notify.SignatureAlgorithm {
		t.Errorf("signing differs from notifications: %s/%s vs %s/%s",
			HeaderSignature, SignatureAlgorithm, notify.HeaderSignature, notify.SignatureAlgorithm)
	}
}

// TestProtocolVocabulary pins the two strings a receiver reads, because
// both are wire contract and because one thing gets one word: the header
// name, the identifier it carries, and the specification's title all say
// evidence push.
func TestProtocolVocabulary(t *testing.T) {
	if SchemaID != "probavi-evidence-push/1" {
		t.Errorf("SchemaID = %q, want %q", SchemaID, "probavi-evidence-push/1")
	}
	if HeaderVersion != "X-Probavi-Evidence-Push-Version" {
		t.Errorf("HeaderVersion = %q, want %q", HeaderVersion, "X-Probavi-Evidence-Push-Version")
	}
}

// TestPushSendsTheWholeLogUnchanged is the core guarantee: the bytes on
// the wire are the log file, with the length and media type that describe
// them.
func TestPushSendsTheWholeLogUnchanged(t *testing.T) {
	recv := &capture{}
	srv := server(t, recv)
	c := client(t, Options{URL: srv.URL, Token: "s3cret-token", Version: "1.2.3"})

	res, err := c.Push(context.Background(), []byte(logBody))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Status != http.StatusOK || res.Bytes != len(logBody) {
		t.Errorf("result = %+v, want status 200 and %d bytes", res, len(logBody))
	}
	if recv.body != logBody {
		t.Errorf("body on the wire = %q, want %q", recv.body, logBody)
	}
	if recv.length != int64(len(logBody)) {
		t.Errorf("Content-Length = %d, want %d", recv.length, len(logBody))
	}
	if recv.method != http.MethodPost {
		t.Errorf("method = %s, want POST", recv.method)
	}
	for header, want := range map[string]string{
		"Content-Type":  ContentType,
		"User-Agent":    "probavi/1.2.3",
		HeaderVersion:   SchemaID,
		"Authorization": "Bearer s3cret-token",
	} {
		if got := recv.header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := recv.header.Get(HeaderSignature); got != "" {
		t.Errorf("unsigned push carries %s = %q", HeaderSignature, got)
	}
	if recv.header.Get("Content-Encoding") != "" {
		t.Error("the sender compressed the body")
	}
}

// TestSignedPushCarriesTheHMACOfTheBody proves a receiver that already
// verifies notification payloads verifies a push with the same code.
func TestSignedPushCarriesTheHMACOfTheBody(t *testing.T) {
	recv := &capture{}
	srv := server(t, recv)
	secret := []byte("shared-secret")
	c := client(t, Options{URL: srv.URL, Token: "t", Secret: secret})

	if _, err := c.Push(context.Background(), []byte(logBody)); err != nil {
		t.Fatalf("Push: %v", err)
	}
	mac := hmac.New(sha256.New, secret)
	if _, err := mac.Write([]byte(logBody)); err != nil {
		t.Fatalf("write mac: %v", err)
	}
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := recv.header.Get(HeaderSignature); got != want {
		t.Errorf("%s = %q, want %q", HeaderSignature, got, want)
	}
}

// TestUnauthenticatedPushSendsNoAuthorization covers the deliberate
// --allow-unauthenticated path: no header at all, not an empty one.
func TestUnauthenticatedPushSendsNoAuthorization(t *testing.T) {
	recv := &capture{}
	srv := server(t, recv)
	c := client(t, Options{URL: srv.URL})

	if _, err := c.Push(context.Background(), []byte(logBody)); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if _, ok := recv.header["Authorization"]; ok {
		t.Errorf("unauthenticated push sent an Authorization header: %q", recv.header.Get("Authorization"))
	}
}

// TestPathIsAppendedToTheDestination pins §5: exactly one slash joins the
// two, whatever the destination looks like, and a query string survives.
func TestPathIsAppendedToTheDestination(t *testing.T) {
	cases := []struct {
		name           string
		suffix, path   string
		wantPath       string
		wantQueryParam string
	}{
		{"bare destination", "", "prod.jsonl", "/prod.jsonl", ""},
		{"sub path", "/ingest", "prod.jsonl", "/ingest/prod.jsonl", ""},
		{"trailing slash", "/ingest/", "prod.jsonl", "/ingest/prod.jsonl", ""},
		{"nested path", "/ingest", "db01/orders.jsonl", "/ingest/db01/orders.jsonl", ""},
		{"query preserved", "/ingest?tenant=7", "prod.jsonl", "/ingest/prod.jsonl", "tenant=7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recv := &capture{}
			srv := server(t, recv)
			c := client(t, Options{URL: srv.URL + tc.suffix, Path: tc.path})
			if _, err := c.Push(context.Background(), []byte(logBody)); err != nil {
				t.Fatalf("Push: %v", err)
			}
			if recv.path != tc.wantPath {
				t.Errorf("request path = %q, want %q", recv.path, tc.wantPath)
			}
			if recv.query != tc.wantQueryParam {
				t.Errorf("query = %q, want %q", recv.query, tc.wantQueryParam)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{"base name", "prod.jsonl", true},
		{"hierarchy", "db01/orders.jsonl", true},
		{"dashes and underscores", "host-01_orders.jsonl", true},
		{"eight segments", "a/b/c/d/e/f/g/prod.jsonl", true},
		{"nine segments", "a/b/c/d/e/f/g/h/prod.jsonl", false},
		{"hidden name", ".prod.jsonl", false},
		{"hidden name deeper in", "db01/.prod.jsonl", false},
		{"empty", "", false},
		{"leading slash", "/prod.jsonl", false},
		{"trailing slash", "prod.jsonl/", false},
		{"empty segment", "db01//orders.jsonl", false},
		{"traversal", "../../etc/passwd", false},
		{"single dot segment", "./prod.jsonl", false},
		{"space", "prod log.jsonl", false},
		{"query", "prod.jsonl?tenant=7", false},
		{"fragment", "prod.jsonl#tail", false},
		{"percent", "prod%2e.jsonl", false},
		{"non-ascii", "napló.jsonl", false},
		{"newline", "prod.jsonl\nX-Injected: 1", false},
		{"long segment", strings.Repeat("a", 65) + ".jsonl", false},
		{"too long overall", strings.Repeat("ab/", 43) + "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePath(tc.path)
			if tc.ok && err != nil {
				t.Errorf("ValidatePath(%q) = %v, want nil", tc.path, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ValidatePath(%q) = nil, want an error", tc.path)
			}
		})
	}
}

func TestDefaultPathIsTheLogsBaseName(t *testing.T) {
	if got := DefaultPath(filepath.Join("/var", "lib", "probavi", "prod.jsonl")); got != "prod.jsonl" {
		t.Errorf("DefaultPath = %q, want %q", got, "prod.jsonl")
	}
}

// TestNewRejectsUnusableDestinations keeps configuration errors where they
// belong: before anything is read or sent.
func TestNewRejectsUnusableDestinations(t *testing.T) {
	cases := []struct {
		name string
		o    Options
	}{
		{"empty url", Options{URL: "", Path: "p.jsonl"}},
		{"relative url", Options{URL: "collector/ingest", Path: "p.jsonl"}},
		{"no host", Options{URL: "https:///ingest", Path: "p.jsonl"}},
		{"wrong scheme", Options{URL: "ftp://collector.example/ingest", Path: "p.jsonl"}},
		{"empty path", Options{URL: "https://collector.example", Path: ""}},
		{"traversal path", Options{URL: "https://collector.example", Path: "../secrets"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.o); err == nil {
				t.Errorf("New(%+v) accepted an unusable destination", tc.o)
			}
		})
	}
}

// TestRetriesServerErrors: 5xx is a receiver that may recover, so the
// attempt loop runs; the successful attempt is the result.
func TestRetriesServerErrors(t *testing.T) {
	recv := &capture{}
	srv := server(t, recv, http.StatusBadGateway, http.StatusOK)
	c := client(t, Options{URL: srv.URL})

	res, err := c.Push(context.Background(), []byte(logBody))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", res.Status)
	}
	if recv.count() != 2 {
		t.Errorf("%d requests, want 2 (one refused, one accepted)", recv.count())
	}
}

func TestGivesUpAfterTheAttemptBudget(t *testing.T) {
	recv := &capture{}
	srv := server(t, recv, http.StatusServiceUnavailable)
	c := client(t, Options{URL: srv.URL})

	if _, err := c.Push(context.Background(), []byte(logBody)); err == nil {
		t.Fatal("a permanently failing receiver was reported as success")
	} else if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q does not name the status", err)
	}
	if recv.count() != Attempts {
		t.Errorf("%d requests, want %d", recv.count(), Attempts)
	}
}

// TestDoesNotRetryRefusals: a 4xx is a configuration answer a retry cannot
// change, and the reason the receiver gave reaches the caller.
func TestDoesNotRetryRefusals(t *testing.T) {
	recv := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv.record(r, nil)
		w.WriteHeader(http.StatusPaymentRequired)
		if _, err := w.Write([]byte("licence expired on 2026-08-01")); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	c := client(t, Options{URL: srv.URL})

	_, err := c.Push(context.Background(), []byte(logBody))
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a *Refusal", err)
	}
	if refusal.Status != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", refusal.Status)
	}
	if refusal.Reason != "licence expired on 2026-08-01" {
		t.Errorf("reason = %q, want the receiver's own words", refusal.Reason)
	}
	if recv.count() != 1 {
		t.Errorf("%d requests, want 1 — a refusal is not retried", recv.count())
	}
}

// TestRefusalReasonIsSanitized: the receiver is untrusted, and its answer
// lands in a terminal or in cron mail.
func TestRefusalReasonIsSanitized(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"control characters removed", "out of\x00 licence\x07", "out of licence"},
		{"whitespace collapsed", "log too large\n\n  for this plan\t\t", "log too large for this plan"},
		{"empty stays empty", "   \n\t ", ""},
		{"bounded", strings.Repeat("a", 5000), strings.Repeat("a", maxReason) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				if _, err := w.Write([]byte(tc.body)); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			c := client(t, Options{URL: srv.URL})

			_, err := c.Push(context.Background(), []byte(logBody))
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("error %v is not a *Refusal", err)
			}
			if refusal.Reason != tc.want {
				t.Errorf("reason = %q, want %q", refusal.Reason, tc.want)
			}
		})
	}
}

// TestRefusalError pins the two shapes an operator sees: a receiver that
// explained itself, and one that only answered with a status.
func TestRefusalError(t *testing.T) {
	cases := []struct {
		name    string
		refusal Refusal
		want    string
	}{
		{"with reason", Refusal{Status: 402, Reason: "out of licence"}, "response status 402: out of licence"},
		{"without reason", Refusal{Status: 404}, "response status 404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.refusal.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTruncatedRefusalBodyContributesNoReason: the status is the verdict,
// and half a sentence from a receiver is worse than none.
func TestTruncatedRefusalBodyContributesNoReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hijack the connection")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// A body that promises more than it delivers, then the connection
		// ends: the client's read of it fails.
		if _, err := io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 100\r\n\r\nshort"); err != nil {
			t.Errorf("write raw response: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close hijacked connection: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	c := client(t, Options{URL: srv.URL})

	_, err := c.Push(context.Background(), []byte(logBody))
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a *Refusal", err)
	}
	if refusal.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", refusal.Status)
	}
	if refusal.Reason != "" {
		t.Errorf("reason = %q, want none from a truncated body", refusal.Reason)
	}
}

// TestRedirectsAreNeverFollowed: a redirect could hand a token-bearing URL
// and a signed body to a host the operator never named.
func TestRedirectsAreNeverFollowed(t *testing.T) {
	elsewhere := &capture{}
	target := server(t, elsewhere)
	recv := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv.record(r, nil)
		http.Redirect(w, r, target.URL+"/prod.jsonl", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c := client(t, Options{URL: srv.URL, Token: "s3cret-token"})

	_, err := c.Push(context.Background(), []byte(logBody))
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a *Refusal", err)
	}
	if refusal.Status != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want 307", refusal.Status)
	}
	if elsewhere.count() != 0 {
		t.Errorf("the redirect target received %d requests", elsewhere.count())
	}
	if recv.count() != 1 {
		t.Errorf("%d requests to the destination, want 1 — a redirect is not retried", recv.count())
	}
}

// TestTransportErrorsAreRetriedAndRedacted covers both halves of a failed
// dial: it is worth retrying, and the message may not carry the URL — Go's
// *url.Error embeds the full destination, credentials included.
func TestTransportErrorsAreRetriedAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()
	c := client(t, Options{URL: dead + "/ingest?token=super-secret", Path: "prod.jsonl"})

	_, err := c.Push(context.Background(), []byte(logBody))
	if err == nil {
		t.Fatal("a closed destination was reported as success")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "prod.jsonl") {
		t.Errorf("error leaks the destination: %v", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error %q does not report the exhausted attempts", err)
	}
}

// TestPushHonorsCancellation proves a push is killable: the context is
// checked before every retry, and Ctrl-C reaches it through the command.
func TestPushHonorsCancellation(t *testing.T) {
	recv := &capture{}
	srv := server(t, recv, http.StatusServiceUnavailable)
	c := client(t, Options{URL: srv.URL})
	c.backoff = []time.Duration{time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for recv.count() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := c.Push(ctx, []byte(logBody))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want a cancellation", err)
	}
}

func TestReadLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "prod.jsonl")
	if err := os.WriteFile(logPath, []byte(logBody), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	body, err := ReadLog(logPath)
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if string(body) != logBody {
		t.Errorf("ReadLog = %q, want %q", body, logBody)
	}

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty log: %v", err)
	}
	if body, err = ReadLog(empty); err != nil || len(body) != 0 {
		t.Errorf("ReadLog(empty) = %q, %v; want an empty body and no error", body, err)
	}

	if _, err = ReadLog(dir); err == nil {
		t.Error("ReadLog accepted a directory")
	}
	if _, err = ReadLog(filepath.Join(dir, "absent.jsonl")); err == nil {
		t.Error("ReadLog accepted a missing file")
	}
}

// TestPushNeverModifiesTheLog is the guarantee the command exists to keep:
// a push is a copy. The file's bytes, size, and modification time are the
// same afterwards, and the log is still there.
func TestPushNeverModifiesTheLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "prod.jsonl")
	if err := os.WriteFile(logPath, []byte(logBody), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	before, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	recv := &capture{}
	srv := server(t, recv)
	c := client(t, Options{URL: srv.URL})

	body, err := ReadLog(logPath)
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if _, err = c.Push(context.Background(), body); err != nil {
		t.Fatalf("Push: %v", err)
	}
	after, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat after push: %v", err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the log changed: %d bytes at %s, was %d bytes at %s",
			after.Size(), after.ModTime(), before.Size(), before.ModTime())
	}
	kept, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log after push: %v", err)
	}
	if string(kept) != logBody {
		t.Errorf("log content after push = %q, want %q", kept, logBody)
	}
}

// TestReadLogRefusesAnOversizeLog: the recv is what keeps a pathological
// file from becoming an out-of-memory kill rather than a diagnostic.
func TestReadLogRefusesAnOversizeLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "huge.jsonl")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A sparse file: the recv is about length, and the test must not write
	// 64 MiB to do its job.
	if err := f.Truncate(MaxLogBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := ReadLog(logPath); err == nil {
		t.Error("ReadLog accepted a log past the size limit")
	}
}
