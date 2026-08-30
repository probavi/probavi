package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/sandbox"
)

// --- fakes -----------------------------------------------------------------

type fakeAdapter struct {
	probe        *adapter.ProbeResult
	probeErr     error
	provRes      *adapter.ProvisionResult
	provErr      error
	healthy      bool
	healthDetail string
	healthEr     error
	teardownErr  error

	provReq         *adapter.ProvisionRequest
	teardownReasons []string
	teardownStates  []string

	// path is the executable the core would hash into adapter.digest.
	// Empty by default, so a record carries a null digest unless a test
	// says otherwise — the shape a drill produces when the file cannot be
	// read (evidence-schema.md §3).
	path string
}

func (f *fakeAdapter) Path() string { return f.path }

func (f *fakeAdapter) Probe(context.Context) (*adapter.ProbeResult, error) {
	return f.probe, f.probeErr
}

func (f *fakeAdapter) Provision(_ context.Context, req *adapter.ProvisionRequest, _ adapter.SandboxVerbs) (*adapter.ProvisionResult, error) {
	f.provReq = req
	return f.provRes, f.provErr
}

func (f *fakeAdapter) Healthcheck(context.Context, *adapter.Connection, json.RawMessage, adapter.SandboxVerbs) (*adapter.HealthcheckResult, error) {
	if f.healthEr != nil {
		return nil, f.healthEr
	}
	detail := f.healthDetail
	if detail == "" {
		detail = "checked"
	}
	return &adapter.HealthcheckResult{Healthy: f.healthy, Detail: detail}, nil
}

func (f *fakeAdapter) Teardown(_ context.Context, state json.RawMessage, reason string, _ adapter.SandboxVerbs) (*adapter.TeardownResult, error) {
	f.teardownReasons = append(f.teardownReasons, reason)
	f.teardownStates = append(f.teardownStates, string(state))
	if f.teardownErr != nil {
		return nil, f.teardownErr
	}
	return &adapter.TeardownResult{Released: true}, nil
}

type fakeSandbox struct {
	execRequests []sandbox.ExecRequest
	execValue    string
	destroyed    int
	destroyErr   error
}

func (f *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	f.execRequests = append(f.execRequests, req)
	return &sandbox.ExecResult{ExitCode: 0, Stdout: []byte(f.execValue + "\n")}, nil
}

func (f *fakeSandbox) PutFile(context.Context, string, string, string) (*sandbox.PutFileResult, error) {
	return &sandbox.PutFileResult{}, nil
}

func (f *fakeSandbox) ID() string                    { return "sbx-1" }
func (f *fakeSandbox) ScratchDir() string            { return "/tmp" }
func (f *fakeSandbox) Destroy(context.Context) error { f.destroyed++; return f.destroyErr }

type fakeProvider struct {
	sbx       *fakeSandbox
	createErr error
	created   int
	swept     int
}

func (f *fakeProvider) Create(context.Context, map[string]string) (Sandbox, error) {
	f.created++
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.sbx, nil
}

func (f *fakeProvider) SweepOrphans(context.Context) ([]string, error) {
	f.swept++
	return nil, nil
}

type failingStore struct{ err error }

func (s failingStore) Append(*evidence.Record) error { return s.err }

// countingStore fails every append and counts the attempts, so tests can
// tell "the core tried once" from "the core tried to rescue the record".
type countingStore struct {
	err   error
	calls int
}

func (s *countingStore) Append(*evidence.Record) error {
	s.calls++
	return s.err
}

// --- fixtures ----------------------------------------------------------------

func testConfig() *config.Config {
	return &config.Config{
		Target: config.Target{
			Name:    "prod-orders-db",
			Adapter: "postgres",
			Source:  config.Source{Kind: "pgdump", Path: "/backups/orders.dump"},
		},
		Sandbox: config.Sandbox{Provider: "docker", Params: map[string]string{"image": "postgres:16"}},
		Checks: []config.Check{
			{Builtin: "service_healthy"},
			{SQL: "SELECT 1", Expect: config.ScalarFromString("1")},
		},
		Hash: "sha256:" + strings.Repeat("7d", 32),
	}
}

func testProbe() *adapter.ProbeResult {
	return &adapter.ProbeResult{
		Name:             "postgres",
		AdapterVersion:   "0.1.0",
		ProtocolVersions: []string{adapter.ProtocolVersion},
		Sources:          []adapter.SourceKind{{Kind: "pgdump"}},
		SQLRunner: adapter.SQLRunner{
			Argv: []string{"psql", "-U", "{{user}}", "-c", "{{sql}}"},
			Env:  map[string]string{"PGPASSWORD": "{{password}}"},
		},
	}
}

func testProvision() *adapter.ProvisionResult {
	created := "2026-07-30T01:58:02.000Z"
	return &adapter.ProvisionResult{
		Connection: adapter.Connection{
			Scheme: "postgresql", Host: "127.0.0.1", Port: 5432,
			Database: "postgres", User: "postgres", PasswordEnv: "PROBAVI_SANDBOX_PASSWORD",
		},
		SourceIdentity: adapter.SourceIdentity{
			Checksum: "sha256:" + strings.Repeat("9f", 32), SizeBytes: 565248, CreatedAt: &created,
		},
		Timings: adapter.Timings{EngineReadySeconds: 1.1665, TransferSeconds: 0.11, RestoreSeconds: 0.19},
		State:   json.RawMessage(`{"database":"postgres"}`),
	}
}

// newDrill wires a Drill against fakes and a REAL evidence store, so every
// record the core produces passes full schema validation and signing.
func newDrill(t *testing.T, fa *fakeAdapter, fp *fakeProvider) (*Drill, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "evidence.jsonl")
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	store, err := evidence.Open(logPath, evidence.NewSignerFromSeed(seed), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	base := time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC)
	tick := 0
	return &Drill{
		Config:          testConfig(),
		Adapter:         fa,
		Provider:        fp,
		Store:           store,
		Version:         "test",
		SandboxPassword: "ephemeral-secret",
		Now: func() time.Time {
			tick++
			return base.Add(time.Duration(tick) * 50 * time.Millisecond)
		},
		Hostname: func() (string, error) { return "drill-host", nil },
	}, logPath
}

func i64v(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// --- tests -------------------------------------------------------------------

func TestRunPass(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	d, logPath := newDrill(t, fa, fp)

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Outcome != evidence.OutcomePass || rec.Error != nil || rec.Seq != 1 || rec.Sig == nil {
		t.Fatalf("record = outcome %s seq %d err %+v", rec.Outcome, rec.Seq, rec.Error)
	}
	assertPassRecord(t, rec)
	assertPassCleanup(t, fa, fp)
	assertLogVerifies(t, logPath)
}

func assertPassRecord(t *testing.T, rec *evidence.Record) {
	t.Helper()
	if *rec.Adapter.Version != "0.1.0" || *rec.Backup.Checksum != "sha256:"+strings.Repeat("9f", 32) {
		t.Errorf("identity fields: adapter=%v backup=%v", rec.Adapter, rec.Backup)
	}
	// Rounding: 1.1665 s → 1167 ms (round half away from zero), 0.19 → 190.
	if i64v(rec.Timings.EngineReady) != 1167 || i64v(rec.Timings.Restore) != 190 || i64v(rec.Timings.Transfer) != 110 {
		t.Errorf("timings = ready %d transfer %d restore %d", i64v(rec.Timings.EngineReady), i64v(rec.Timings.Transfer), i64v(rec.Timings.Restore))
	}
	if i64v(rec.Timings.Provision) < 0 || i64v(rec.Timings.Validate) < 0 || i64v(rec.Timings.Total) <= 0 {
		t.Errorf("core-measured timings missing: %+v", rec.Timings)
	}
	if len(rec.Checks) != 2 || !rec.Checks[0].OK || !rec.Checks[1].OK {
		t.Errorf("checks = %+v", rec.Checks)
	}
}

func assertPassCleanup(t *testing.T, fa *fakeAdapter, fp *fakeProvider) {
	t.Helper()
	if fa.teardownReasons[0] != "completed" || fp.sbx.destroyed != 1 || fp.swept != 1 {
		t.Errorf("cleanup: teardowns=%v destroyed=%d swept=%d", fa.teardownReasons, fp.sbx.destroyed, fp.swept)
	}
	// The ephemeral password must reach the sql_runner env, and only there.
	last := fp.sbx.execRequests[len(fp.sbx.execRequests)-1]
	if last.Env["PGPASSWORD"] != "ephemeral-secret" {
		t.Errorf("sql_runner env = %v — password_env resolution broken", last.Env)
	}
	if strings.Contains(strings.Join(last.Argv, " "), "ephemeral-secret") {
		t.Error("password leaked into argv")
	}
}

func assertLogVerifies(t *testing.T, logPath string) {
	t.Helper()
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close log: %v", err)
		}
	}()
	res, err := evidence.Verify(f, evidence.NewKeyring(evidence.NewSignerFromSeed(seed32()).PublicKey()), nil)
	if err != nil || res.Status != evidence.StatusValid || res.Records != 1 {
		t.Errorf("evidence verify = %+v err=%v", res, err)
	}
}

func seed32() []byte {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	return seed
}

func TestRunOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(fa *fakeAdapter, fp *fakeProvider)
		wantOutcome  evidence.Outcome
		wantCode     string
		wantTeardown string // "" = teardown must not run
	}{
		{"provision source corrupt is a fail verdict",
			func(fa *fakeAdapter, fp *fakeProvider) {
				fa.provRes = nil
				fa.provErr = &adapter.Error{Code: "source_corrupt", Message: "bad archive"}
			}, evidence.OutcomeFail, "source_corrupt", "failed"},
		{"check verdict failure",
			func(fa *fakeAdapter, fp *fakeProvider) { fp.sbx.execValue = "2" },
			evidence.OutcomeFail, "check_failed", "failed"},
		{"check infrastructure failure",
			func(fa *fakeAdapter, fp *fakeProvider) {
				fa.healthEr = &adapter.Error{Code: "adapter_crash", Message: "boom"}
			}, evidence.OutcomeError, "adapter_crash", "failed"},
		{"probe failure",
			func(fa *fakeAdapter, fp *fakeProvider) {
				fa.probe = nil
				fa.probeErr = &adapter.Error{Code: "adapter_crash", Message: "no binary"}
			}, evidence.OutcomeError, "adapter_crash", ""},
		{"unsupported source kind",
			func(fa *fakeAdapter, fp *fakeProvider) { fa.probe.Sources = []adapter.SourceKind{{Kind: "walg"}} },
			evidence.OutcomeError, "unsupported_source", ""},
		{"sandbox create failure",
			func(fa *fakeAdapter, fp *fakeProvider) { fp.createErr = errors.New("docker daemon down") },
			evidence.OutcomeError, "sandbox_error", ""},
		{"cancelled drill",
			func(fa *fakeAdapter, fp *fakeProvider) {
				fa.provRes = nil
				fa.provErr = &adapter.Error{Code: "cancelled", Message: "stopping"}
			}, evidence.OutcomeCancelled, "cancelled", "cancelled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
			fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
			tt.mutate(fa, fp)
			assertOutcome(t, fa, fp, tt.wantOutcome, tt.wantCode, tt.wantTeardown)
		})
	}
}

func assertOutcome(t *testing.T, fa *fakeAdapter, fp *fakeProvider, wantOutcome evidence.Outcome, wantCode, wantTeardown string) {
	t.Helper()
	d, _ := newDrill(t, fa, fp)
	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Outcome != wantOutcome || rec.Error == nil || rec.Error.Code != wantCode {
		t.Errorf("record = %s / %+v, want %s / %s", rec.Outcome, rec.Error, wantOutcome, wantCode)
	}
	if wantTeardown == "" {
		if len(fa.teardownReasons) != 0 {
			t.Errorf("teardown ran (%v) although provision was never attempted", fa.teardownReasons)
		}
	} else if len(fa.teardownReasons) != 1 || fa.teardownReasons[0] != wantTeardown {
		t.Errorf("teardown reasons = %v, want [%s]", fa.teardownReasons, wantTeardown)
	}
	if fp.created > 0 && fp.createErr == nil && fp.sbx.destroyed != 1 {
		t.Error("created sandbox was not destroyed")
	}
}

func TestRunPITRResolvedTargetReachesRecordAndAdapter(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	fa.probe.Sources = []adapter.SourceKind{{Kind: "pgdump", Capabilities: adapter.Capabilities{PITR: true}}}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	d, _ := newDrill(t, fa, fp)
	d.Config.Target.PITR = &config.PITR{TargetAge: config.Duration(24 * time.Hour)}

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Outcome != evidence.OutcomePass {
		t.Fatalf("outcome = %s (%+v)", rec.Outcome, rec.Error)
	}
	if rec.Drill.PITRTarget == nil {
		t.Fatal("record must carry the resolved pitr target")
	}
	// Run's fake clock ticks 50 ms per Now(): drill start, then the
	// resolve inside baseRecord — so the target is base+100ms − 24h.
	want := "2026-07-30T02:00:00.100Z"
	if *rec.Drill.PITRTarget != want {
		t.Errorf("drill.pitr_target = %q, want %q", *rec.Drill.PITRTarget, want)
	}
	if fa.provReq == nil || fa.provReq.PITR == nil || fa.provReq.PITR.TargetTime != want {
		t.Errorf("provision request pitr = %+v, want the record's target %q — one resolution, two consumers", fa.provReq.PITR, want)
	}
}

func TestRunPITRRefusedWithoutCapability(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	d, _ := newDrill(t, fa, fp)
	d.Config.Target.PITR = &config.PITR{TargetAge: config.Duration(time.Hour)}

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Outcome != evidence.OutcomeError || rec.Error == nil ||
		rec.Error.Code != "unsupported_source" || !strings.Contains(rec.Error.Message, "point-in-time") {
		t.Errorf("outcome = %s error = %+v, want unsupported_source about point-in-time recovery", rec.Outcome, rec.Error)
	}
	if fp.created != 0 {
		t.Error("the capability gate must fire before a sandbox is created")
	}
	if rec.Drill.PITRTarget == nil {
		t.Error("even a refused pitr drill must record what it asked for")
	}
}

func TestRunWithoutPITRRecordsNull(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	d, _ := newDrill(t, fa, fp)
	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Drill.PITRTarget != nil {
		t.Errorf("drill.pitr_target = %q, want nil", *rec.Drill.PITRTarget)
	}
	if fa.provReq.PITR != nil {
		t.Errorf("provision request pitr = %+v, want absent (§6.2)", fa.provReq.PITR)
	}
}

func TestSupportsPITRUnknownKind(t *testing.T) {
	if supportsPITR(testProbe(), "kind-nobody-declared") {
		t.Error("a kind absent from probe.sources must not claim the pitr capability")
	}
}

func TestRunTimeoutClassification(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(),
		provErr: &adapter.Error{Code: "adapter_crash", Message: "killed"}}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	d, _ := newDrill(t, fa, fp)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	rec, err := d.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Outcome != evidence.OutcomeError || rec.Error.Code != "timeout" {
		t.Errorf("record = %s / %+v, want error/timeout — wall-clock death must be identifiable", rec.Outcome, rec.Error)
	}
	if fa.teardownReasons[0] != "timeout" {
		t.Errorf("teardown reason = %v, want timeout", fa.teardownReasons)
	}
}

func TestRunCrashBeforeProvisionStateIsEmptyObject(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(),
		provErr: &adapter.Error{Code: "adapter_crash", Message: "died mid-provision"}}
	fp := &fakeProvider{sbx: &fakeSandbox{}}
	d, _ := newDrill(t, fa, fp)

	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The protocol client normalizes nil state to {} (§6.4); the core must
	// still call teardown after a provision crash.
	if len(fa.teardownStates) != 1 {
		t.Fatalf("teardown did not run after provision crash")
	}
}

func TestRunAppendFailureIsFatal(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	d, _ := newDrill(t, fa, fp)
	d.Store = failingStore{err: errors.New("disk full")}

	_, err := d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "left no evidence") {
		t.Errorf("err = %v — a lost record is the highest-severity failure and must surface", err)
	}
}

func TestRunWithDefaultsAndCleanupFailures(t *testing.T) {
	// Nil Logger/Now/Hostname take defaults; teardown and destroy failures
	// are logged, never overriding the drill verdict; nil sandbox params
	// normalize to an empty object so the record still validates.
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true,
		teardownErr: errors.New("adapter teardown broke")}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1", destroyErr: errors.New("rm failed")}}
	d, _ := newDrill(t, fa, fp)
	d.Logger, d.Now, d.Hostname = nil, nil, nil
	d.Config.Sandbox.Params = nil

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Outcome != evidence.OutcomePass {
		t.Errorf("outcome = %s — cleanup failures must not change the verdict", rec.Outcome)
	}
	if rec.Sandbox.Params == nil {
		t.Error("nil sandbox params must normalize to an empty object")
	}
}

func TestResolvePassword(t *testing.T) {
	// The name comes from the adapter, so what it can reach must be the
	// drill's declared allow-list and nothing else: password_env is a field
	// for a database password, not a read primitive for the core's
	// environment.
	t.Setenv("PROBAVI_TEST_DB_PW", "inherited")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-for-the-adapter")

	cfg := testConfig()
	cfg.Target.Source.CredentialEnv = []string{"PROBAVI_TEST_DB_PW"}
	d := &Drill{Config: cfg, SandboxPassword: "generated", Logger: slog.New(slog.DiscardHandler)}

	tests := []struct {
		name        string
		passwordEnv string
		want        string
	}{
		{"empty asks for nothing", "", ""},
		{"the core's own ephemeral secret", adapter.SandboxPasswordEnv, "generated"},
		{"a declared credential", "PROBAVI_TEST_DB_PW", "inherited"},
		{"an undeclared variable is refused", "AWS_SECRET_ACCESS_KEY", ""},
		{"an unset undeclared variable is refused too", "NOT_SET_ANYWHERE", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := d.resolvePassword(tt.passwordEnv); got != tt.want {
				t.Errorf("resolvePassword(%q) = %q, want %q", tt.passwordEnv, got, tt.want)
			}
		})
	}
}

func TestHostIDFallback(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	fp := &fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	d, _ := newDrill(t, fa, fp)
	d.Hostname = func() (string, error) { return "", errors.New("no hostname") }

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.Env.HostID) != 16 {
		t.Errorf("host_id = %q, want 16 hex chars even without a hostname", rec.Env.HostID)
	}
}

// TestUnregisteredErrorCodeIsNormalized covers the vocabulary rule: an
// adapter chooses its own error code, and a record carrying one outside
// the published enum would verify as VALID while failing the schema every
// consumer validates against. The code becomes internal; the original
// survives in the message, so nothing is lost for whoever debugs it.
func TestUnregisteredErrorCodeIsNormalized(t *testing.T) {
	tests := []struct {
		name        string
		code        string
		wantCode    string
		wantOutcome evidence.Outcome
		wantInMsg   string
	}{
		{"registry code passes through", "restore_failed", "restore_failed", evidence.OutcomeFail, "disk full"},
		{"schema-only code passes through", "check_failed", "check_failed", evidence.OutcomeFail, "disk full"},
		{"invented code becomes internal", "banana_peel", "internal", evidence.OutcomeError, `"banana_peel"`},
		{"gameday summary code is not a record code", "evidence_lost", "internal", evidence.OutcomeError, `"evidence_lost"`},
		{"empty code becomes internal", "", "internal", evidence.OutcomeError, `""`},
		{"case matters", "INTERNAL", "internal", evidence.OutcomeError, `"INTERNAL"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeAdapter{probe: testProbe(),
				provErr: &adapter.Error{Code: tt.code, Message: "disk full"}}
			d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})

			rec, err := d.Run(context.Background())
			if err != nil {
				t.Fatalf("a drill that ran must leave a record: %v", err)
			}
			if rec.Error.Code != tt.wantCode {
				t.Errorf("error.code = %q, want %q", rec.Error.Code, tt.wantCode)
			}
			if rec.Outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", rec.Outcome, tt.wantOutcome)
			}
			if !strings.Contains(rec.Error.Message, tt.wantInMsg) {
				t.Errorf("message %q does not carry %s", rec.Error.Message, tt.wantInMsg)
			}
			if !evidence.IsErrorCode(rec.Error.Code) {
				t.Errorf("signed record carries %q, which is outside the published vocabulary", rec.Error.Code)
			}
		})
	}
}

// TestNonASCIIFailureTextStillLeavesARecord is the regression test for the
// class of bug where a drill ran and then lost its proof. Both strings are
// non-ASCII and longer than their evidence caps: an adapter reporting an
// engine error in its own language, and a healthcheck detail carrying
// accented output. Counting characters instead of bytes made the first
// exceed the 512-byte limit, and slicing at a byte offset made the second
// invalid UTF-8; either one made Append reject the record.
func TestNonASCIIFailureTextStillLeavesARecord(t *testing.T) {
	t.Run("adapter error message", func(t *testing.T) {
		fa := &fakeAdapter{
			probe:   testProbe(),
			provErr: &adapter.Error{Code: "restore_failed", Message: strings.Repeat("á", 400)},
		}
		d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})

		rec, err := d.Run(context.Background())
		if err != nil {
			t.Fatalf("a drill that ran must leave a record: %v", err)
		}
		if len(rec.Error.Message) > 512 {
			t.Errorf("error.message is %d bytes, over the schema cap", len(rec.Error.Message))
		}
		if !utf8.ValidString(rec.Error.Message) {
			t.Error("error.message is not valid UTF-8")
		}
		if rec.Outcome != evidence.OutcomeFail {
			t.Errorf("outcome = %q, want fail", rec.Outcome)
		}
	})

	t.Run("check detail", func(t *testing.T) {
		fa := &fakeAdapter{
			probe: testProbe(), provRes: testProvision(), healthy: true,
			healthDetail: strings.Repeat("é", 300),
		}
		d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})

		rec, err := d.Run(context.Background())
		if err != nil {
			t.Fatalf("a drill that ran must leave a record: %v", err)
		}
		if len(rec.Checks) == 0 || rec.Checks[0].Detail == nil {
			t.Fatal("expected a detail on the first check")
		}
		detail := *rec.Checks[0].Detail
		if len(detail) > 256 {
			t.Errorf("detail is %d bytes, over the schema cap", len(detail))
		}
		if !utf8.ValidString(detail) {
			t.Error("detail is not valid UTF-8")
		}
	})
}

func TestSanitizeMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"passes short text through", "restore failed", "restore failed"},
		{"folds newlines to spaces", "line one\nline two\r\nthree", "line one line two  three"},
		{"keeps non-ASCII intact when it fits", "hiba: nem sikerült", "hiba: nem sikerült"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeMessage(tt.in); got != tt.want {
				t.Errorf("sanitizeMessage(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	for _, filler := range []string{"a", "á", "€", "𝄞"} {
		got := sanitizeMessage(strings.Repeat(filler, 600))
		if len(got) > 512 {
			t.Errorf("%q filler: %d bytes, over the 512-byte cap", filler, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("%q filler: result is not valid UTF-8", filler)
		}
	}
}

// TestDegradedRecordReplacesAnUnrepresentableOne exercises the §7 backstop
// end to end against the real store: the adapter reports a created_at the
// evidence schema cannot hold, the composed record is refused, and the
// drill still leaves a signed, verifiable record instead of vanishing from
// the log.
func TestDegradedRecordReplacesAnUnrepresentableOne(t *testing.T) {
	created := "2026-07-30T01:58:02Z" // valid RFC 3339, but not millisecond UTC
	prov := testProvision()
	prov.SourceIdentity.CreatedAt = &created
	fa := &fakeAdapter{probe: testProbe(), provRes: prov, healthy: true}
	d, logPath := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})

	rec, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("a drill that ran must leave a record: %v", err)
	}
	if rec.Outcome != evidence.OutcomeError || rec.Error == nil || rec.Error.Code != "internal" {
		t.Fatalf("degraded record = outcome %q error %+v, want error/internal", rec.Outcome, rec.Error)
	}
	// The message must say what was lost, or the record is a mystery.
	for _, want := range []string{"replaced", "created_at", `"pass"`} {
		if !strings.Contains(rec.Error.Message, want) {
			t.Errorf("message %q does not mention %s", rec.Error.Message, want)
		}
	}
	// Identity the operator needs to find the drill survives; adapter- and
	// operator-supplied payload does not.
	if rec.Drill.Name != "prod-orders-db" || rec.Drill.ConfigHash != d.Config.Hash {
		t.Errorf("degraded record lost its identity: %+v", rec.Drill)
	}
	if len(rec.Sandbox.Params) != 0 || len(rec.Checks) != 0 || rec.Backup.Checksum != nil {
		t.Errorf("degraded record kept payload it should have dropped: %+v", rec)
	}
	if rec.Seq != 1 || rec.Sig == nil {
		t.Errorf("degraded record is not chained and signed: seq=%d sig=%v", rec.Seq, rec.Sig)
	}
	assertLogVerifies(t, logPath)
}

// TestDegradedRecordNotAttemptedOnWriteFailure keeps the backstop narrow: a
// store that cannot write is not a record the core can fix by rewriting it,
// and a second attempt would only bury the real error.
func TestDegradedRecordNotAttemptedOnWriteFailure(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})
	counting := &countingStore{err: errors.New("disk full")}
	d.Store = counting

	if _, err := d.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "left no evidence") {
		t.Errorf("err = %v — a lost record must surface", err)
	}
	if counting.calls != 1 {
		t.Errorf("Append called %d times, want 1: an I/O failure must not be retried", counting.calls)
	}
}

// TestDegradedRecordFailureStillSurfaces covers the case where even the
// degraded record cannot be stored: the original error must reach the
// caller rather than being swallowed by the rescue attempt.
func TestDegradedRecordFailureStillSurfaces(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
	d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})
	counting := &countingStore{err: fmt.Errorf("%w: nope", evidence.ErrInvalidRecord)}
	d.Store = counting

	if _, err := d.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "left no evidence") {
		t.Errorf("err = %v — a lost record must surface", err)
	}
	if counting.calls != 2 {
		t.Errorf("Append called %d times, want 2: one composed, one degraded", counting.calls)
	}
}

func TestSafeText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"usable value survives", "prod-orders-db", "prod-orders-db"},
		{"empty is substituted", "", "sub"},
		{"invalid utf8 is substituted", "bad\xff", "sub"},
		{"newline is substituted", "two\nlines", "sub"},
		{"carriage return is substituted", "two\rlines", "sub"},
		{"over-long is substituted", strings.Repeat("x", 257), "sub"},
		{"at the limit survives", strings.Repeat("x", 256), strings.Repeat("x", 256)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeText(tt.in, "sub"); got != tt.want {
				t.Errorf("safeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCancelledContextIsRecordedAsCancelled covers the outcome the schema
// defines for an operator stopping a drill. Before this, only an adapter
// that managed to answer with code "cancelled" produced it: a SIGTERM that
// killed the adapter outright was recorded as adapter_crash, so the signed
// record blamed a third party's adapter for the operator's own interrupt.
func TestCancelledContextIsRecordedAsCancelled(t *testing.T) {
	tests := []struct {
		name    string
		adapter *adapter.Error
	}{
		{"adapter died without answering", &adapter.Error{Code: "adapter_crash", Message: "adapter exited without a final response"}},
		{"adapter answered cancelled itself", &adapter.Error{Code: "cancelled", Message: "stopping"}},
		{"adapter blamed the restore", &adapter.Error{Code: "restore_failed", Message: "half-written"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := &fakeAdapter{probe: testProbe(), provErr: tt.adapter}
			d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			rec, err := d.Run(ctx)
			if err != nil {
				t.Fatalf("a cancelled drill still leaves a record: %v", err)
			}
			if rec.Outcome != evidence.OutcomeCancelled {
				t.Errorf("outcome = %q, want cancelled", rec.Outcome)
			}
			if rec.Error.Code != evidence.CodeCancelled {
				t.Errorf("error.code = %q, want cancelled", rec.Error.Code)
			}
			// The adapter's own words survive; only the verdict changes.
			if !strings.Contains(rec.Error.Message, tt.adapter.Message) {
				t.Errorf("message %q dropped what the adapter said", rec.Error.Message)
			}
			if fa.teardownReasons[0] != "cancelled" {
				t.Errorf("teardown reason = %q, want cancelled", fa.teardownReasons[0])
			}
		})
	}
}

// TestDeadlineOutranksCancellation pins the order of the two context
// verdicts: a drill that ran out of its wall-clock limit is a timeout, not
// a cancellation, even though the context is done either way.
func TestDeadlineOutranksCancellation(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe(), provErr: &adapter.Error{Code: "adapter_crash", Message: "killed"}}
	d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	rec, err := d.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Outcome != evidence.OutcomeError || rec.Error.Code != evidence.CodeTimeout {
		t.Errorf("outcome = %q code = %q, want error/timeout", rec.Outcome, rec.Error.Code)
	}
	if fa.teardownReasons[0] != "timeout" {
		t.Errorf("teardown reason = %q, want timeout", fa.teardownReasons[0])
	}
}
