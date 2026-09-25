package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets this test binary double as the fake adapter: when the mode
// variable is set (inherited by the child processes the suite spawns), the
// process behaves as an adapter instead of running tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("PROBAVI_FAKE_ADAPTER"); mode != "" {
		os.Exit(fakeMain(mode))
	}
	// The race detector sleeps atexit_sleep_ms (default 1000) at every
	// process exit to widen the window for catching late races from
	// still-running goroutines. This suite spawns hundreds of short-lived
	// fake-adapter children (this binary, re-executed), and each child
	// inheriting the default turns ~400×1s of pure sleeping into the
	// package's entire runtime. Disabling the exit sleep does not weaken
	// race detection of memory accesses in any way — it only narrows the
	// at-exit observation window, which buys nothing for a ~10 ms child.
	// Any operator-provided GORACE options are preserved; ours wins on
	// conflict by coming last.
	gorace := "atexit_sleep_ms=0"
	if prev := os.Getenv("GORACE"); prev != "" {
		gorace = prev + " " + gorace
	}
	if err := os.Setenv("GORACE", gorace); err != nil {
		fmt.Fprintf(os.Stderr, "set GORACE: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func runSuite(t *testing.T, mode string, opts Options) *Report {
	t.Helper()
	t.Setenv("PROBAVI_FAKE_ADAPTER", mode)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := Run(ctx, self, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

func check(t *testing.T, report *Report, name string) Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q missing from report %+v", name, report.Checks)
	return Check{}
}

// frozenList is the exact §10 order; drifting from it is a spec violation.
var frozenList = []string{
	"probe.shape", "probe.sql_runner", "probe.no_sandbox_calls",
	"handshake.unsupported_protocol", "handshake.unknown_op",
	"provision.malformed_payload", "provision.unsupported_source",
	"provision.missing_source", "provision.happy_path", "provision.timings",
	"healthcheck.shape", "teardown.empty_state", "teardown.idempotent",
	"sigterm.cancels", "framing.discipline",
	// v1's, appended: §10 freezes the list per version, so v0's fifteen
	// keep their order and their numbers.
	"probe.checks_keys", "probe.identifier",
}

func TestConformantAdapterPassesEverything(t *testing.T) {
	report := runSuite(t, "conformant", Options{})
	if report.Failed != 0 || report.Passed != len(frozenList) {
		t.Fatalf("report = %d passed / %d failed: %+v", report.Passed, report.Failed, report.Checks)
	}
	names := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, frozenList) {
		t.Errorf("check list = %v\nwant the frozen §10 order %v", names, frozenList)
	}
}

func TestViolationsAreDetected(t *testing.T) {
	tests := []struct {
		mode      string
		check     string
		detailSub string
		opts      Options
	}{
		{"no-detail", "handshake.unsupported_protocol", "detail", Options{}},
		{"stdout-noise", "framing.discipline", "not a protocol message", Options{}},
		{"double-final", "framing.discipline", "after the final response", Options{}},
		{"wrong-echo", "framing.discipline", "not echoed", Options{}},
		{"huge-frame", "framing.discipline", "4 MiB", Options{}},
		{"crash-on-probe", "probe.shape", "ok final response", Options{}},
		{"bad-name", "probe.shape", "must match", Options{}},
		{"no-protocol-version", "probe.shape", "protocol_versions", Options{}},
		{"empty-sources", "probe.shape", "source kind", Options{}},
		{"empty-kind-name", "probe.shape", "kind is empty", Options{}},
		{"bare-message", "framing.discipline", "neither sandbox_call nor final", Options{}},
		{"bad-verb", "probe.shape", "exec and put_file", Options{}},
		{"probe-sandbox-call", "probe.no_sandbox_calls", "forbids", Options{}},
		{"no-sql-placeholder", "probe.sql_runner", "{{sql}}", Options{}},
		{"password-in-argv", "probe.sql_runner", "{{password}}", Options{}},
		{"bad-timings", "provision.timings", "exceeds", Options{}},
		{"negative-timings", "provision.timings", "negative", Options{}},
		{"bad-checksum", "provision.happy_path", "checksum", Options{}},
		{"empty-scheme", "provision.happy_path", "scheme", Options{}},
		{"bad-state", "provision.happy_path", "state", Options{}},
		{"bad-created-at", "provision.happy_path", "RFC 3339", Options{}},
		{"crash-on-malformed", "provision.malformed_payload", "invalid_request", Options{}},
		{"wrong-missing-code", "provision.missing_source", "source_not_found", Options{}},
		{"unhealthy-error", "healthcheck.shape", "ok:true", Options{}},
		{"negative-latency", "healthcheck.shape", "negative", Options{}},
		{"no-healthy-field", "healthcheck.shape", "boolean healthy", Options{}},
		{"teardown-empty-fails", "teardown.empty_state", "crash case", Options{}},
		{"teardown-third-fails", "teardown.idempotent", "both succeed", Options{}},
		{"ignore-sigterm", "sigterm.cancels", "after SIGTERM", Options{}},
		{"sigterm-wrong-code", "sigterm.cancels", "want cancelled", Options{}},
		{"hang-on-sigterm", "sigterm.cancels", "grace", Options{Grace: 700 * time.Millisecond}},
		// §6.1 and §6.2 both say "object": a payload that decodes into
		// something else is named as such rather than met with a decoder
		// error nobody can act on.
		{"probe-payload-not-object", "probe.shape", "not a §6.1 object", Options{}},
		{"provision-payload-not-object", "provision.happy_path", "not a §6.2 object", Options{}},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			if tt.mode == "teardown-third-fails" {
				t.Setenv("PROBAVI_FAKE_COUNTER", tempCounterPath(t))
			}
			report := runSuite(t, tt.mode, tt.opts)
			c := check(t, report, tt.check)
			if c.OK {
				t.Fatalf("check %s passed for violation mode %s: %+v", tt.check, tt.mode, report.Checks)
			}
			if !strings.Contains(c.Detail, tt.detailSub) {
				t.Errorf("detail = %q, want it to mention %q", c.Detail, tt.detailSub)
			}
		})
	}
}

func TestSourceKindOverride(t *testing.T) {
	t.Setenv("PROBAVI_FAKE_ADAPTER", "conformant")
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	// The fake declares fakedump only: forcing another kind must turn the
	// happy path into unsupported_source failures.
	report, err := Run(context.Background(), self, Options{SourceKind: "not-a-kind"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c := check(t, report, "provision.happy_path"); c.OK {
		t.Error("happy path with a forced unknown kind must fail")
	}
	if c := check(t, report, "provision.missing_source"); c.OK {
		t.Error("missing-source with a forced unknown kind must fail — the adapter reports unsupported_source instead")
	}
}

func TestSigtermWithNothingToInterrupt(t *testing.T) {
	// A provision that never touches the sandbox leaves the §2.4 scenario
	// nothing to interrupt; the check passes vacuously instead of hanging.
	report := runSuite(t, "no-calls-provision", Options{})
	if c := check(t, report, "sigterm.cancels"); !c.OK {
		t.Errorf("sigterm.cancels = %+v, want vacuous pass", c)
	}
}

func TestSupportedListed(t *testing.T) {
	ok := func(detail string) *envelope {
		return &envelope{Error: &wireError{Detail: []byte(detail)}}
	}
	for name, tt := range map[string]struct {
		final *envelope
		want  bool
	}{
		"nil final":      {nil, false},
		"no error":       {&envelope{}, false},
		"no detail":      {&envelope{Error: &wireError{}}, false},
		"not json":       {ok(`"supported"`), false},
		"empty list":     {ok(`{"supported":[]}`), false},
		"versions given": {ok(`{"supported":["probavi-adapter/0"]}`), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := supportedListed(tt.final); got != tt.want {
				t.Errorf("supportedListed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExitless(t *testing.T) {
	if err := exitless(nil); err != nil {
		t.Errorf("exitless(nil) = %v", err)
	}
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := exitless(cmd.Run()); err != nil {
		t.Errorf("exit statuses must be filtered, got %v", err)
	}
	sentinel := errors.New("harness trouble")
	if err := exitless(sentinel); !errors.Is(err, sentinel) {
		t.Errorf("real errors must pass through, got %v", err)
	}
}

func TestUndefinedVerbIsAnsweredNotCrashed(t *testing.T) {
	// The simulated sandbox answers an undefined verb with an error result;
	// the fake ignores it and completes, so the drill-shaped checks stay
	// green — the incident is between the adapter and its sandbox result.
	report := runSuite(t, "weird-verb", Options{})
	if c := check(t, report, "provision.happy_path"); !c.OK {
		t.Errorf("happy path = %+v, want ok despite the refused verb", c)
	}
}

func TestRunUnstartableAdapter(t *testing.T) {
	_, err := Run(context.Background(), "/nonexistent/probavi-adapter-void", Options{})
	if err == nil {
		t.Fatal("Run must report an unstartable adapter as a harness error, not a verdict")
	}
}

func TestAdapterVanishingMidSuiteIsAHarnessError(t *testing.T) {
	t.Setenv("PROBAVI_FAKE_ADAPTER", "self-destruct")
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read self: %v", err)
	}
	copyPath := filepath.Join(t.TempDir(), "adapter-copy")
	if err := os.WriteFile(copyPath, raw, 0o755); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	if _, err := Run(context.Background(), copyPath, Options{}); err == nil {
		t.Fatal("an adapter that vanishes mid-suite must surface as a harness error")
	}
}

func TestRunTempSourceFailure(t *testing.T) {
	t.Setenv("PROBAVI_FAKE_ADAPTER", "conformant")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "gone"))
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	if _, err := Run(context.Background(), self, Options{}); err == nil {
		t.Fatal("an uncreatable temp source is a harness error, not an adapter verdict")
	}
}

func TestRunHonorsContext(t *testing.T) {
	t.Setenv("PROBAVI_FAKE_ADAPTER", "hang-on-probe")
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	report, err := Run(ctx, self, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("Run ignored the cancelled context")
	}
	if c := check(t, report, "probe.shape"); c.OK {
		t.Error("a probe killed by the context cannot pass")
	}
}

func tempCounterPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "counter")
	if err != nil {
		t.Fatalf("temp counter: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close counter: %v", err)
	}
	return f.Name()
}

// TestIsRFC3339 pins the acceptance set of the created_at check to RFC
// 3339 itself rather than to Go's narrower time.RFC3339 layout: the
// published provision-response schema declares lowercase designators
// valid, so an adapter that validates against it must pass this suite.
func TestIsRFC3339(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"2026-07-30T01:58:02Z", true},
		{"2026-07-30T01:58:02.000Z", true},
		{"2026-07-30T01:58:02.789999999Z", true},
		{"2026-07-30t01:58:02z", true},
		{"2026-07-30T03:58:02+02:00", true},
		{"2026-07-30", false},
		{"2026-07-30T01:58:02", false},
		{"yesterday", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isRFC3339(tt.in); got != tt.want {
			t.Errorf("isRFC3339(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestDriveToleratesClosedStdin covers the race that made the suite fail
// with a harness error instead of a verdict: the adapter exits before the
// request lands, our write gets EPIPE, and there is nothing wrong with the
// adapter that the read loop and the exit status cannot describe. The
// request is deliberately larger than a pipe buffer so the write cannot be
// absorbed and the failure is the one under test rather than a coin flip.
func TestDriveToleratesClosedStdin(t *testing.T) {
	t.Setenv("PROBAVI_FAKE_ADAPTER", "exit-before-read")
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	huge := `{"protocol":"` + protocolVersion + `","request_id":"r1","op":"probe","payload":{"pad":"` +
		strings.Repeat("x", 512*1024) + `"}}`

	res, err := drive(context.Background(), self, "r1", driveSpec{request: huge})
	if err != nil {
		t.Fatalf("a closed stdin is not a harness error: %v", err)
	}
	if res.final != nil {
		t.Errorf("final = %+v, want none — the adapter never answered", res.final)
	}
}

func TestClosedPipe(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EPIPE", syscall.EPIPE, true},
		{"wrapped EPIPE", fmt.Errorf("write: %w", syscall.EPIPE), true},
		{"closed pipe", io.ErrClosedPipe, true},
		{"closed file", os.ErrClosed, true},
		{"unrelated", errors.New("disk on fire"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := closedPipe(tt.err); got != tt.want {
				t.Errorf("closedPipe(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestPutFileIsAnsweredLikeAnExec: §4 defines two verbs, and the
// simulated sandbox of §10 answers both — an adapter that moves its
// artifact with put_file must be able to pass the suite.
func TestPutFileIsAnsweredLikeAnExec(t *testing.T) {
	report := runSuite(t, "put-file-provision", Options{})
	if c := check(t, report, "provision.happy_path"); !c.OK {
		t.Errorf("provision.happy_path = %+v, want a pass: a put_file must be answered", c)
	}
	if report.Failed != 0 {
		t.Errorf("report = %d failed, want none: %+v", report.Failed, report.Checks)
	}
}

// TestAnAdapterThatDiesMidAnswerIsAVerdict: the suite writes a sandbox
// result to an adapter that has already gone. That is the adapter's
// behaviour to report — it exited without a final response (§2.3) — and
// never a failure of the harness itself.
func TestAnAdapterThatDiesMidAnswerIsAVerdict(t *testing.T) {
	report := runSuite(t, "die-after-first-call", Options{})
	c := check(t, report, "provision.happy_path")
	if c.OK {
		t.Errorf("provision.happy_path = %+v, want a failure for an adapter that left mid-operation", c)
	}
	if framing := check(t, report, "framing.discipline"); !framing.OK {
		t.Errorf("framing.discipline = %+v, want no framing violation: the adapter framed what it sent", framing)
	}
}

// TestTheOperatorsOwnArtifactIsUsedWhenNamed: Options.SourcePath is what a
// drill would restore, so the suite provisions from it rather than from a
// temporary file of random bytes — and creates nothing of its own.
func TestTheOperatorsOwnArtifactIsUsedWhenNamed(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "fake.dump")
	if err := os.WriteFile(artifact, []byte("the operator's own backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := runSuite(t, "conformant", Options{SourcePath: artifact})
	if report.Failed != 0 {
		t.Fatalf("report = %d failed: %+v", report.Failed, report.Checks)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Errorf("the named artifact is the operator's: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the artifact itself", len(entries))
	}
}

// TestV1DeclarationsAreChecked covers §10's checks 16–17. An adapter that
// declares nothing passes both, which is every v0 adapter; an adapter that
// declares something wrong is refused here, where the cost is a failing
// conformance run rather than a declaration that silently never takes
// effect at 3am.
func TestV1DeclarationsAreChecked(t *testing.T) {
	t.Run("a correct v1 adapter passes", func(t *testing.T) {
		report := runSuite(t, "v1", Options{})
		if report.Failed != 0 {
			t.Fatalf("report = %d passed / %d failed: %+v", report.Passed, report.Failed, report.Checks)
		}
	})

	tests := []struct {
		mode, check, wantIn string
	}{
		{"v1-unknown-check-kind", "probe.checks_keys", "not a built-in check kind"},
		{"v1-stray-placeholder", "probe.checks_keys", "{{password}}"},
		{"v1-undeclared-version", "probe.identifier", "probavi-adapter/1"},
		{"v1-half-quoted", "probe.identifier", "both be empty or both be set"},
		{"v1-long-separator", "probe.identifier", "exactly one character"},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			got := check(t, runSuite(t, tc.mode, Options{}), tc.check)
			if got.OK {
				t.Fatalf("%s passed against %s, want a refusal", tc.check, tc.mode)
			}
			if !strings.Contains(got.Detail, tc.wantIn) {
				t.Errorf("detail = %q, want it to name %q", got.Detail, tc.wantIn)
			}
		})
	}
}
