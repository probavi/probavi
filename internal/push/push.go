// Package push sends an evidence log to an operator-chosen URL
// (docs/evidence-push.md). A push is a copy, never a move: the log is
// opened read-only, the whole file is sent every time, and nothing about
// the transfer is recorded — a delivery is not evidence.
//
// The delivery loop deliberately mirrors internal/notify rather than
// sharing code with it. The two carry different bodies, answer a refusal
// differently (a receiver's reason is printed here, discarded there), and
// are versioned independently — probavi-push/N against
// probavi-notification/N — so they must be free to diverge on a spec
// change. What must not diverge silently is the retry budget, which
// docs/evidence-push.md §7 declares identical to the notification rules; a
// test pins these constants to notify's.
package push

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SchemaID is this protocol's version, carried in HeaderVersion and
// published by the capabilities manifest. It is versioned independently of
// the binary, exactly like probavi-adapter/N, probavi-evidence/N, and
// probavi-notification/N.
const SchemaID = "probavi-push/1"

// Request constants (docs/evidence-push.md §4).
const (
	// Method is the HTTP method every push uses.
	Method = http.MethodPost
	// ContentType is the body's media type: the evidence log is
	// newline-delimited JSON, sent verbatim.
	ContentType = "application/x-ndjson"
	// HeaderVersion carries SchemaID, so a receiver can tell this protocol
	// version from a future one without inspecting the body.
	HeaderVersion = "X-Probavi-Push-Version"
	// HeaderSignature carries the optional HMAC-SHA256 of the body,
	// GitHub-style as "sha256=<hex>" — the same header and scheme a
	// notification uses, so one receiver verifies both with one function.
	HeaderSignature = "X-Probavi-Signature-256"
	// SignatureAlgorithm names the MAC in the capabilities manifest.
	SignatureAlgorithm = "HMAC-SHA256"
	// DefaultTokenEnv is the environment variable the bearer token is read
	// from unless --token-env names another one.
	DefaultTokenEnv = "PROBAVI_PUSH_TOKEN" //nolint:gosec // G101 false positive: the name of an environment variable, not a credential
)

// Delivery constants (docs/evidence-push.md §7), identical to the
// notification rules by decision, not by accident.
const (
	// Budget bounds total delivery time across all attempts.
	Budget = 60 * time.Second
	// AttemptTimeout bounds one attempt.
	AttemptTimeout = 10 * time.Second
	// Attempts is how many times the destination is tried before giving up.
	Attempts = 3
)

// Limits on what is sent and what is read back.
const (
	// MaxLogBytes is the largest log this command sends. At 1–2 KB per
	// record it is tens of millions of drills — far outside what the format
	// is for — and refusing is preferable to an out-of-memory kill.
	MaxLogBytes = 64 << 20
	// maxDrain caps how much of a response body is read; receivers are
	// untrusted.
	maxDrain = 4 << 10
	// maxReason caps the printable characters of a refusal reason.
	maxReason = 500
	// maxPath caps the total length of a destination path.
	maxPath = 128
)

// pathSegment is one segment of a destination path (§5): narrow enough to
// need no percent-encoding and to carry no query, fragment, or traversal.
var pathSegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Refusal is a receiver's non-2xx answer together with the reason it gave.
// The reason is sanitized (printable characters only, whitespace
// collapsed, truncated) because the receiver is untrusted and the text
// lands in an operator's terminal or cron mail.
type Refusal struct {
	// Status is the HTTP status code of the answer.
	Status int
	// Reason is the sanitized response body, empty when there was none.
	Reason string
}

func (r *Refusal) Error() string {
	if r.Reason == "" {
		return fmt.Sprintf("response status %d", r.Status)
	}
	return fmt.Sprintf("response status %d: %s", r.Status, r.Reason)
}

// Options configures one push. Values, not environment variable names:
// resolving the environment is the caller's job, so that a missing
// variable is diagnosed in the operator's language before anything here
// runs (docs/i18n.md §1).
type Options struct {
	// URL is the resolved destination, an absolute http(s) URL. It is
	// treated as a credential throughout: it never appears in an error.
	URL string
	// Path is the destination path this log is sent under; empty is a
	// configuration error, since a receiver names the log from it.
	Path string
	// Token is the bearer token. Empty means the operator asked for an
	// unauthenticated push explicitly (§6.1).
	Token string
	// Secret is the HMAC signing secret; nil means unsigned.
	Secret []byte
	// Version is the probavi version, sent as the User-Agent.
	Version string
}

// Client delivers one evidence log to one destination.
type Client struct {
	url     string
	token   string
	secret  []byte
	version string
	client  *http.Client
	backoff []time.Duration
}

// Result reports an accepted push.
type Result struct {
	// Status is the 2xx status the receiver answered with.
	Status int
	// Bytes is the number of body bytes sent.
	Bytes int
}

// New validates the destination and returns a ready Client. Errors here
// are configuration errors: nothing has been read or sent yet.
func New(o Options) (*Client, error) {
	if err := ValidatePath(o.Path); err != nil {
		return nil, err
	}
	// The error never echoes the URL — it may be a credential.
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("destination is not an absolute http(s) URL")
	}
	return &Client{
		url:     u.JoinPath(o.Path).String(),
		token:   o.Token,
		secret:  o.Secret,
		version: o.Version,
		client: &http.Client{
			Timeout: AttemptTimeout,
			// Redirects are never followed: a redirect could hand a
			// token-bearing URL or a signed body to an unintended host
			// (docs/evidence-push.md §7).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		backoff: []time.Duration{time.Second, 2 * time.Second},
	}, nil
}

// DefaultPath is the destination path for a log the operator did not name
// one for: the log file's base name, which is the same on every run.
func DefaultPath(logPath string) string {
	return filepath.Base(logPath)
}

// ValidatePath enforces the §5 grammar. It is exported so the command can
// check a derived default before anything else happens.
func ValidatePath(p string) error {
	if p == "" {
		return errors.New("destination path is empty")
	}
	if len(p) > maxPath {
		return fmt.Errorf("destination path is %d characters, more than the %d allowed", len(p), maxPath)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || !pathSegment.MatchString(seg) {
			return fmt.Errorf("path segment %q is not 1-64 characters of A-Z a-z 0-9 . _ -", seg)
		}
	}
	return nil
}

// ReadLog reads the evidence log to send. The descriptor is read-only by
// construction: a push copies a log, and no code path here may be able to
// write to one.
func ReadLog(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not an evidence log", path)
	}
	if info.Size() > MaxLogBytes {
		return nil, oversize(path)
	}
	// LimitReader as well as the size above: a log being appended to can
	// grow between the two, and the cap must hold regardless.
	body, err := io.ReadAll(io.LimitReader(f, MaxLogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(body) > MaxLogBytes {
		return nil, oversize(path)
	}
	return body, nil
}

func oversize(path string) error {
	return fmt.Errorf("%s is larger than the %d-byte push limit", path, MaxLogBytes)
}

// Push delivers the body, retrying transport errors and 5xx answers only.
// The bytes are sent exactly as given: they are what Content-Length counts
// and what the signature covers, so a drill appending to the log during a
// push cannot make the three disagree.
func (c *Client) Push(ctx context.Context, body []byte) (*Result, error) {
	var last error
	for attempt := range Attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("delivery aborted: %w", ctx.Err())
			case <-time.After(c.backoff[min(attempt-1, len(c.backoff)-1)]):
			}
		}
		status, retryable, err := c.post(ctx, body)
		if err == nil {
			return &Result{Status: status, Bytes: len(body)}, nil
		}
		last = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", Attempts, last)
}

// post makes one attempt. Errors are redacted before they leave: Go's
// *url.Error embeds the full URL, which may be a credential, so only its
// inner transport error survives.
func (c *Client) post(ctx context.Context, body []byte) (status int, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, Method, c.url, bytes.NewReader(body))
	if err != nil {
		// Unreachable for a URL New validated; keep the message value-free
		// regardless.
		return 0, false, errors.New("build request: invalid destination")
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("User-Agent", "probavi/"+c.version)
	req.Header.Set(HeaderVersion, SchemaID)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.secret != nil {
		mac := hmac.New(sha256.New, c.secret)
		if _, werr := mac.Write(body); werr != nil {
			return 0, false, fmt.Errorf("sign body: %w", werr)
		}
		req.Header.Set(HeaderSignature, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return 0, true, fmt.Errorf("post: %w", err)
	}
	drained, rerr := io.ReadAll(io.LimitReader(resp.Body, maxDrain))
	if cerr := resp.Body.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		// The status is the verdict; a body that could not be read in full
		// contributes no reason rather than a torn one.
		drained = nil
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, false, nil
	case resp.StatusCode >= 500:
		return 0, true, &Refusal{Status: resp.StatusCode, Reason: reason(drained)}
	default:
		// 3xx (redirects are never followed) and 4xx are configuration
		// problems a retry cannot fix.
		return 0, false, &Refusal{Status: resp.StatusCode, Reason: reason(drained)}
	}
}

// reason turns a receiver's answer into one printable line. A refusal that
// names a reason — out of licence, log too large, path not accepted — is
// what an operator reads in a cron job's mail, so it is passed through;
// the receiver is untrusted, so control characters (terminal escapes among
// them) are dropped and the length is bounded.
func reason(body []byte) string {
	var b strings.Builder
	space := false
	for _, r := range string(body) {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case r == utf8.RuneError || !unicode.IsPrint(r):
			continue
		default:
			if space {
				b.WriteRune(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	out := []rune(b.String())
	if len(out) > maxReason {
		return string(out[:maxReason]) + "…"
	}
	return string(out)
}
