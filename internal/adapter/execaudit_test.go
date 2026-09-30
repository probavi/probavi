package adapter

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/sandbox"
)

// discardWriter accepts the sandbox_result the read loop writes back. What
// this file tests is what the audit logged on the way past, not the wire.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) Close() error                { return nil }

func newScanner(s string) *bufio.Scanner { return bufio.NewScanner(strings.NewReader(s)) }

// cipherPass is the shape this audit exists for: a passphrase the operator
// declared in source.credential_env so an adapter could read an encrypted
// backup. The adapter is given the value (§2.5 passes the named variables
// into its environment) and may not write it into a message.
const cipherPass = "correct-horse-battery-staple"

// driveExec runs one exec sandbox call through a session and returns what
// the logger saw. The verbs are fake; what is under test is the audit the
// read loop applies on the way past.
func driveExec(t *testing.T, envJSON string, secrets []string) (string, *fakeVerbs) {
	t.Helper()
	const requestID = "req-audit"
	call := `{"protocol":"` + ProtocolVersion + `","request_id":"` + requestID +
		`","sandbox_call":{"call_id":"c1","verb":"exec","args":{"argv":["true"],"env":` + envJSON + `}}}`
	final := `{"protocol":"` + ProtocolVersion + `","request_id":"` + requestID +
		`","ok":true,"payload":{}}`

	log := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(log, nil))
	s := &session{
		stdin:    discardWriter{},
		stdout:   newScanner(call + "\n" + final + "\n"),
		logger:   logger,
		protocol: ProtocolVersion,
		audit:    &execAudit{logger: logger, secrets: secrets},
	}
	verbs := &fakeVerbs{execRes: sandbox.ExecResult{ExitCode: 0}}
	if _, verr := s.readLoop(context.Background(), "provision", requestID, verbs, nil); verr != nil {
		t.Fatalf("readLoop: %v", verr)
	}
	return log.String(), verbs
}

// TestExecAuditWarnsOnADeclaredCredential is the gate. An adapter that
// forwards a passphrase into the sandbox's environment through exec.env
// has written a secret into a protocol message, which §2.5 and §4.1
// forbid — and nothing said so before this audit existed.
func TestExecAuditWarnsOnADeclaredCredential(t *testing.T) {
	logged, verbs := driveExec(t,
		`{"PGBACKREST_REPO1_CIPHER_PASS":"`+cipherPass+`"}`, []string{cipherPass})

	if !strings.Contains(logged, "exec.env") {
		t.Errorf("the log does not report the violation: %s", logged)
	}
	if !strings.Contains(logged, "PGBACKREST_REPO1_CIPHER_PASS") {
		t.Errorf("the warning does not name the variable, which is what its author needs: %s", logged)
	}
	if strings.Contains(logged, cipherPass) {
		t.Errorf("the warning carries the value — a message written because a secret was "+
			"mishandled must not be where it leaks: %s", logged)
	}
	// Warn, not refuse: the operator cannot fix someone else's adapter, and
	// the value reaches no record and no log by this route.
	if len(verbs.execReqs) != 1 {
		t.Fatalf("exec ran %d times, want the call to have been fulfilled anyway", len(verbs.execReqs))
	}
	if verbs.execReqs[0].Env["PGBACKREST_REPO1_CIPHER_PASS"] != cipherPass {
		t.Error("the sandbox did not receive what the adapter asked for")
	}
}

// TestExecAuditIsSilentOnWhatAdaptersActuallySend keeps the audit from
// being noise. Three adapters put a publicly documented constant into
// exec.env — that is the workaround this rule leaves them, not a
// violation of it.
func TestExecAuditIsSilentOnWhatAdaptersActuallySend(t *testing.T) {
	logged, _ := driveExec(t,
		`{"COUCHDB_PASSWORD":"probavi-drill-sandbox","NEO4J_USERNAME":"neo4j"}`,
		[]string{cipherPass})

	if strings.Contains(logged, "exec.env") {
		t.Errorf("a constant an adapter chose itself was reported as a leak: %s", logged)
	}
}

// TestExecAuditWithNothingToHide covers the two silent shapes: a drill
// that declared no credentials, and an operation with no audit at all.
func TestExecAuditWithNothingToHide(t *testing.T) {
	if logged, _ := driveExec(t, `{"TZ":"UTC"}`, nil); strings.Contains(logged, "exec.env") {
		t.Errorf("a drill with no secrets reported one: %s", logged)
	}
	var none *execAudit
	none.check(map[string]string{"ANY": "value"}) // must not panic
}

// TestSecretValuesResolvesBothHalves pins what the audit is given: the
// declared pass-through variables the environment actually sets, and the
// extras the core generated. An unset or empty variable is no secret, and
// would otherwise match every empty exec.env value.
func TestSecretValuesResolvesBothHalves(t *testing.T) {
	t.Setenv("PROBAVI_TEST_CIPHER_PASS", cipherPass)
	t.Setenv("PROBAVI_TEST_EMPTY", "")

	r := &Runner{opts: Options{
		CredentialEnv: []string{"PROBAVI_TEST_CIPHER_PASS", "PROBAVI_TEST_EMPTY", "PROBAVI_TEST_UNSET"},
		Env:           map[string]string{SandboxPasswordEnv: "ephemeral", "BLANK": ""},
	}}
	got := r.secretValues()

	want := map[string]bool{cipherPass: true, "ephemeral": true}
	if len(got) != len(want) {
		t.Fatalf("secretValues = %q, want the two non-empty values", got)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("secretValues = %q, want only %v", got, want)
		}
	}
}
