package checks

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/redact"
)

// cipherPass is a secret this package never resolves itself: the drill
// declared it in source.credential_env so an adapter could decrypt the
// backup, and the core hands the value down purely so it can be masked.
// Target.Password would not cover it — that is the engine's login.
const cipherPass = "correct-horse-battery-staple"

// TestDeclaredCredentialIsMaskedInTheLog is the reason Deps.Redact exists.
// An engine quoting its own configuration back at a failure is ordinary
// behaviour, and the drill host's log is the one place this package
// deliberately writes engine text in full.
func TestDeclaredCredentialIsMaskedInTheLog(t *testing.T) {
	stderr := "ERROR: could not open repository\nrepo1-cipher-pass=" + cipherPass

	log := &bytes.Buffer{}
	deps := testDeps(&fakeExec{t: t, respond: queryFailure(stderr)})
	deps.Logger = slog.New(slog.NewTextHandler(log, nil))
	deps.Redact = redact.New(cipherPass)

	if _, err := Run(context.Background(), []config.Check{
		{SQL: "SELECT 1", Expect: config.ScalarFromString("1")},
	}, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logged := log.String()
	if strings.Contains(logged, cipherPass) {
		t.Errorf("the log carries the backup's passphrase: %s", logged)
	}
	if !strings.Contains(logged, "could not open repository") {
		t.Errorf("the diagnostic did not survive masking, and it is the operator's only copy: %s", logged)
	}
	// Target.Password is masked whether or not a redactor was wired, so
	// the two must not be able to shadow each other.
	if strings.Contains(logged, deps.Target.Password) {
		t.Errorf("the log carries the connection password: %s", logged)
	}
}

// TestDeclaredCredentialIsMaskedInADetail covers the string that reaches
// a signed record rather than a log: the adapter's healthcheck text,
// which service_healthy passes through verbatim.
func TestDeclaredCredentialIsMaskedInADetail(t *testing.T) {
	deps := testDeps(&fakeExec{t: t, respond: value("1")})
	deps.Healthcheck = func(context.Context) (bool, string, error) {
		return true, "ready; opened repo with " + cipherPass, nil
	}
	deps.Redact = redact.New(cipherPass)

	res := runSingleWithDeps(t, config.Check{Builtin: config.CheckServiceHealthy}, deps)
	if strings.Contains(res.Detail, cipherPass) {
		t.Fatalf("detail = %q — a signed record cannot carry a backup's passphrase", res.Detail)
	}
	if !strings.Contains(res.Detail, redact.Placeholder) {
		t.Errorf("detail = %q, want the masked value in place", res.Detail)
	}
}

// TestDetailIsMaskedBeforeItIsCut pins the order. A cap applied first can
// slice a passphrase in half, leaving a head no replacement will ever
// match; masking first can also lengthen the text, so the cap must be the
// last thing applied or the record becomes unwritable.
func TestDetailIsMaskedBeforeItIsCut(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	filler := strings.Repeat("h", evidence.MaxDetailBytes-20)

	deps := testDeps(&fakeExec{t: t, respond: value("1")})
	deps.Healthcheck = func(context.Context) (bool, string, error) {
		return true, filler + secret, nil
	}
	deps.Redact = redact.New(secret)

	res := runSingleWithDeps(t, config.Check{Builtin: config.CheckServiceHealthy}, deps)
	if len(res.Detail) > evidence.MaxDetailBytes {
		t.Fatalf("detail is %d bytes, over the %d-byte cap", len(res.Detail), evidence.MaxDetailBytes)
	}
	if strings.Contains(res.Detail, secret[:16]) {
		t.Errorf("detail ends %q — it carries the head of the secret, so the cut came before the mask",
			res.Detail[len(filler):])
	}
	if !strings.HasSuffix(res.Detail, redact.Placeholder) {
		t.Errorf("detail ends %q, want the whole secret replaced", res.Detail[len(filler):])
	}
}

func runSingleWithDeps(t *testing.T, c config.Check, deps Deps) Result {
	t.Helper()
	results, err := Run(context.Background(), []config.Check{c}, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	return results[0]
}
