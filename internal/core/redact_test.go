package core

import (
	"context"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/evidence"
)

// The secret an encrypted backup needs. It is declared in the drill's
// source.credential_env, so the core holds its value — and an engine that
// quotes its own configuration back at a failure puts it somewhere no
// deletion reaches, because an evidence log is append-only and signed.
const cipherPass = "correct-horse-battery-staple"

func TestRecordErrorMessageMasksADeclaredCredential(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provErr: &adapter.Error{
		Code:    "source_corrupt",
		Message: `pgbackrest: unable to decrypt with repo1-cipher-pass=` + cipherPass,
	}}
	d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})
	d.CredentialValues = []string{cipherPass}

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("a drill that ran must leave a record: %v", err)
	}
	if strings.Contains(rec.Error.Message, cipherPass) {
		t.Fatalf("error.message = %q — a signed record cannot carry a backup's passphrase", rec.Error.Message)
	}
	if !strings.Contains(rec.Error.Message, "[redacted]") {
		t.Errorf("error.message = %q, want the masked value in place", rec.Error.Message)
	}
	if !strings.Contains(rec.Error.Message, "repo1-cipher-pass=") {
		t.Errorf("error.message = %q, want the diagnostic still readable around the mask", rec.Error.Message)
	}
}

// The ephemeral password is the one secret the core generated itself, so
// no caller declares it and none can forget to.
func TestRecordErrorMessageMasksTheSandboxPassword(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provErr: &adapter.Error{
		Code:    "restore_failed",
		Message: `psql: FATAL: password authentication failed for "ephemeral-secret"`,
	}}
	d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(rec.Error.Message, "ephemeral-secret") {
		t.Errorf("error.message = %q, want the generated password masked without being declared",
			rec.Error.Message)
	}
}

// A healthcheck detail is adapter text that reaches checks[].detail. The
// cap on that field is applied after masking, inside internal/checks.
func TestCheckDetailMasksADeclaredCredential(t *testing.T) {
	fa := &fakeAdapter{
		probe: testProbe(), provRes: testProvision(), healthy: true,
		healthDetail: "ready; opened repo with " + cipherPass,
	}
	d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})
	d.CredentialValues = []string{cipherPass}

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var detail string
	for _, c := range rec.Checks {
		if c.Name == "service_healthy" && c.Detail != nil {
			detail = *c.Detail
		}
	}
	if detail == "" {
		t.Fatalf("no service_healthy detail in %+v", rec.Checks)
	}
	if strings.Contains(detail, cipherPass) || !strings.Contains(detail, "[redacted]") {
		t.Errorf("detail = %q, want the passphrase masked", detail)
	}
}

// Masking before truncation is the specified order (evidence-schema.md
// §8) and the only one that holds. Cutting first leaves the head of a
// secret in the record, which no later replacement can match — so this
// message is sized to be cut exactly through the passphrase.
func TestRecordMessageMasksBeforeTruncating(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	// Twenty bytes short of the cap, so a message ending in the secret is
	// cut straight through it: truncating first would leave seventeen of
	// its characters in the record, and the mask would never match again.
	filler := strings.Repeat("f", evidence.MaxErrorMessageBytes-20)
	d := &Drill{CredentialValues: []string{secret}}
	d.defaults()

	got := d.recordMessage(filler + secret)
	if len(got) > evidence.MaxErrorMessageBytes {
		t.Fatalf("message is %d bytes, over the %d-byte cap", len(got), evidence.MaxErrorMessageBytes)
	}
	if strings.Contains(got, secret[:16]) {
		t.Errorf("message ends %q — it carries the head of the secret, so the cut came before the mask",
			got[len(filler):])
	}
	if !strings.HasSuffix(got, "[redacted]") {
		t.Errorf("message ends %q, want the whole secret replaced", got[len(filler):])
	}
}

// A drill that declares no credentials must read exactly as it did
// before: no secrets means no substitutions, not an empty one matching
// everywhere.
func TestRecordMessageWithoutSecretsIsUnchanged(t *testing.T) {
	d := &Drill{}
	d.defaults()

	const in = "unable to read /backups/orders.dump: no such file"
	if got := d.recordMessage(in); got != in {
		t.Errorf("recordMessage(%q) = %q, want it unchanged", in, got)
	}
}
