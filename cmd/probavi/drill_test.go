package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/gameday"
	"github.com/probavi/probavi/internal/i18n"
)

// fakeProbePayload is a conformant §6.1 probe response for the fake
// adapter the tests below place on PATH.
const fakeProbePayload = `{"name":"covfake","adapter_version":"0.0.1","protocol_versions":["probavi-adapter/0"],"engine":{"name":"fakedb"},"sources":[{"kind":"pgdump","capabilities":{"pitr":false}}],"sql_runner":{"argv":["cat"],"env":{}},"verbs_required":["exec","put_file"]}`

// fakeAdapterScript answers any operation with an ok response; only probe
// carries a payload worth reading.
const fakeAdapterScript = `#!/bin/sh
read -r REQ
RID=$(printf '%s' "$REQ" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
OP=$(printf '%s' "$REQ" | sed -n 's/.*"op":"\([^"]*\)".*/\1/p')
if [ "$OP" = probe ]; then
  printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":true,"payload":` + fakeProbePayload + `}\n' "$RID"
else
  printf '{"protocol":"probavi-adapter/0","request_id":"%s","ok":true,"payload":{}}\n' "$RID"
fi
`

// installFakeToolchain puts a working fake adapter, a broken one, and a
// docker CLI that always fails onto PATH, so drills can be driven end to
// end without a container runtime.
func installFakeToolchain(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("probavi-adapter-covfake", fakeAdapterScript)
	write("probavi-adapter-broken", "#!/bin/sh\nexit 0\n")
	write("docker", "#!/bin/sh\necho 'docker: no container runtime in unit tests' >&2\nexit 1\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// failingDrill writes a complete drill fixture whose sandbox can never be
// created (the fake docker fails), returning the config and the paths the
// assertions need.
func failingDrill(t *testing.T) (cfgPath, evidencePath, pubPath, metricsPath string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ed25519.key")
	if code, _, stderr := runCLI(t, "evidence", "keygen", "--out", keyPath); code != 0 {
		t.Fatalf("keygen exit %d, stderr: %s", code, stderr)
	}
	evidencePath = filepath.Join(dir, "evidence.jsonl")
	metricsPath = filepath.Join(dir, "probavi.prom")
	cfg := `target:
  name: cov-unit
  adapter: covfake
  source:
    kind: pgdump
    path: ` + filepath.Join(dir, "backup.dump") + `
sandbox:
  provider: docker
  timeout: 1m
checks:
  - builtin: service_healthy
evidence:
  path: ` + evidencePath + `
  sign_key: ` + keyPath + `
metrics:
  prometheus_textfile: ` + metricsPath + `
`
	cfgPath = filepath.Join(dir, "drill.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, evidencePath, keyPath + ".pub", metricsPath
}

// TestRunDrillWithoutContainerRuntime drives `probavi run` end to end with
// a docker CLI that always fails: the drill must still leave a signed,
// verifiable evidence record saying so, write its metrics, print the
// machine summary, and exit 2 — an infrastructure error is a verdict, not
// a crash.
func TestRunDrillWithoutContainerRuntime(t *testing.T) {
	installFakeToolchain(t)
	cfgPath, evidencePath, pubPath, metricsPath := failingDrill(t)

	code, stdout, stderr := runCLI(t, "run", "--config", cfgPath)
	if code != exitError {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitError, stderr)
	}
	var summary gameday.DrillSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("summary is not JSON: %v (%q)", err, stdout)
	}
	if summary.Outcome != "error" || summary.ErrorCode != "sandbox_error" || summary.Seq != 1 {
		t.Errorf("summary = %+v, want error/sandbox_error at seq 1", summary)
	}
	if summary.EvidencePath != evidencePath {
		t.Errorf("summary.EvidencePath = %q, want %q", summary.EvidencePath, evidencePath)
	}

	if code, out, stderr := runCLI(t, "evidence", "verify", "--log", evidencePath, "--key", pubPath); code != 0 {
		t.Errorf("verify exit %d, stderr: %s", code, stderr)
	} else if !strings.Contains(out, `"status":"VALID"`) || !strings.Contains(out, `"records":1`) {
		t.Errorf("verify output = %q, want VALID with 1 record", out)
	}
	if _, err := os.Stat(metricsPath); err != nil {
		t.Errorf("metrics textfile was not written: %v", err)
	}
}

// TestRunDrillSurvivesAnUnwritableStdout: a broken pipe on stdout loses
// the summary print, never the verdict — the exit code and the evidence
// record still stand.
func TestRunDrillSurvivesAnUnwritableStdout(t *testing.T) {
	installFakeToolchain(t)
	cfgPath, evidencePath, pubPath, _ := failingDrill(t)

	var stderr bytes.Buffer
	if code := runDrill([]string{"--config", cfgPath}, failingWriter{}, &stderr, i18n.English()); code != exitError {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitError, stderr.String())
	}
	if code, _, stderr := runCLI(t, "evidence", "verify", "--log", evidencePath, "--key", pubPath); code != 0 {
		t.Errorf("verify exit %d, stderr: %s", code, stderr)
	}
}

// TestGameDayRunsItsMembersThroughTheDrillPipeline: a member whose sandbox
// cannot be created reports the drill's own summary — code, checks, seq —
// through the game-day runner, and the exercise exits 2.
func TestGameDayRunsItsMembersThroughTheDrillPipeline(t *testing.T) {
	installFakeToolchain(t)
	cfgPath, _, _, _ := failingDrill(t)
	gdPath := filepath.Join(filepath.Dir(cfgPath), "gameday.yaml")
	gd := `name: gd-cov
timeout: 5m
members:
  - name: alpha
    config: ` + filepath.Base(cfgPath) + `
`
	if err := os.WriteFile(gdPath, []byte(gd), 0o600); err != nil {
		t.Fatalf("write game-day config: %v", err)
	}

	code, stdout, stderr := runCLI(t, "gameday", "--config", gdPath)
	if code != exitError {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitError, stderr)
	}
	var summary gameday.Summary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("summary is not JSON: %v (%q)", err, stdout)
	}
	if len(summary.Members) != 1 || summary.Members[0].DrillSummary == nil ||
		summary.Members[0].ErrorCode != "sandbox_error" {
		t.Fatalf("summary = %+v, want alpha carrying its drill summary with sandbox_error", summary)
	}

	// The same exercise against an unwritable stdout keeps its exit code.
	var errBuf bytes.Buffer
	if code := runGameDay([]string{"--config", gdPath}, failingWriter{}, &errBuf, i18n.English()); code != exitError {
		t.Errorf("exit %d with unwritable stdout, want %d", code, exitError)
	}
}

func TestSummarize(t *testing.T) {
	ms := int64(1234)
	rec := testRecord("2026-01-02T15:04:05.000Z")
	rec.Seq = 7
	rec.Checks = append(rec.Checks, evidence.Check{Name: "row_count", OK: false})
	rec.Timings = evidence.Timings{Restore: &ms, Total: &ms}

	s := summarize(rec, "/var/lib/probavi/evidence.jsonl")
	if s.Outcome != "pass" || s.Seq != 7 || s.ChecksPassed != 1 || s.ChecksTotal != 2 {
		t.Errorf("summary = %+v, want pass 1/2 at seq 7", s)
	}
	if s.RestoreMS == nil || *s.RestoreMS != ms || s.EvidencePath != "/var/lib/probavi/evidence.jsonl" {
		t.Errorf("summary carries wrong timings or path: %+v", s)
	}
	if s.ErrorCode != "" {
		t.Errorf("ErrorCode = %q for a record without error", s.ErrorCode)
	}

	rec.Outcome = evidence.OutcomeError
	rec.Error = &evidence.DrillError{Code: "sandbox_error", Message: "no runtime"}
	if s := summarize(rec, "p"); s.ErrorCode != "sandbox_error" {
		t.Errorf("ErrorCode = %q, want sandbox_error", s.ErrorCode)
	}
}

func TestGamedayExit(t *testing.T) {
	member := func(errCode string) gameday.MemberResult {
		return gameday.MemberResult{Outcome: "error", DrillSummary: &gameday.DrillSummary{ErrorCode: errCode}}
	}
	tests := []struct {
		name    string
		summary gameday.Summary
		want    int
	}{
		{"pass", gameday.Summary{Outcome: "pass"}, exitPass},
		{"fail", gameday.Summary{Outcome: "fail"}, exitFail},
		{"error", gameday.Summary{Outcome: "error"}, exitError},
		{"lost evidence dominates", gameday.Summary{
			Outcome: "pass",
			Members: []gameday.MemberResult{member(gameday.ErrCodeEvidenceLost)},
		}, exitEvidenceLost},
		{"skipped member carries no drill summary", gameday.Summary{
			Outcome: "fail",
			Members: []gameday.MemberResult{{Outcome: "skipped"}},
		}, exitFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gamedayExit(&tt.summary); got != tt.want {
				t.Errorf("gamedayExit = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveAdapterExecutable(t *testing.T) {
	installFakeToolchain(t)
	dir := t.TempDir()
	explicit := filepath.Join(dir, "my-adapter")
	if err := os.WriteFile(explicit, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}

	tests := []struct {
		name, arg, wantErr string
	}{
		{"name on PATH", "covfake", ""},
		{"unknown name", "covfake-missing", "resolve adapter"},
		{"explicit path", explicit, ""},
		{"missing explicit path", filepath.Join(dir, "absent"), "adapter executable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := resolveAdapterExecutable(tt.arg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || path == "" {
				t.Fatalf("resolved (%q, %v), want a path", path, err)
			}
		})
	}
}

func TestAdapterProbeCommand(t *testing.T) {
	installFakeToolchain(t)

	if code, _, _ := runCLI(t, "adapter", "probe"); code != exitUsage {
		t.Errorf("probe without a name: exit %d, want %d", code, exitUsage)
	}
	if code, _, _ := runCLI(t, "adapter", "probe", "covfake-missing"); code != exitUsage {
		t.Errorf("probe of an unresolvable name: exit %d, want %d", code, exitUsage)
	}

	code, stdout, stderr := runCLI(t, "adapter", "probe", "covfake")
	if code != exitPass {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitPass, stderr)
	}
	if !strings.Contains(stdout, `"name":"covfake"`) || !strings.Contains(stdout, `"kind":"pgdump"`) {
		t.Errorf("probe output = %q, want the adapter's declaration", stdout)
	}

	var errBuf bytes.Buffer
	if code := runAdapterProbe([]string{"covfake"}, failingWriter{}, &errBuf, i18n.English()); code != exitError {
		t.Errorf("probe with unwritable stdout: exit %d, want %d", code, exitError)
	}
}

// TestConformanceAgainstABrokenAdapter: an executable that exits without
// speaking the protocol must produce a failing report — verdicts on
// stderr, the JSON report on stdout, exit 1 — not a driver error.
func TestConformanceAgainstABrokenAdapter(t *testing.T) {
	installFakeToolchain(t)

	code, stdout, stderr := runCLI(t, "adapter", "conformance", "broken")
	if code != exitFail {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitFail, stderr)
	}
	if !strings.Contains(stderr, "FAIL") {
		t.Errorf("stderr carries no FAIL verdicts: %s", stderr)
	}
	report := struct {
		Failed int `json:"failed"`
	}{}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.Failed == 0 {
		t.Errorf("report = %q (err %v), want JSON with failed > 0", stdout, err)
	}
}

func TestConformanceRejectsAMalformedSourceParam(t *testing.T) {
	if code, _, stderr := runCLI(t, "adapter", "conformance", "--source-param", "no-equals-sign", "x"); code != exitUsage {
		t.Errorf("exit %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
}

// TestVerifyReportsAnUnreadableLog: a log path that can be opened but not
// read (a directory) is an I/O error, distinct from any integrity verdict.
func TestVerifyReportsAnUnreadableLog(t *testing.T) {
	_, _, pubPath := setupLog(t)
	code, _, stderr := runCLI(t, "evidence", "verify", "--log", t.TempDir(), "--key", pubPath)
	if code != exitUsage || !strings.Contains(stderr, "evidence verify") {
		t.Errorf("exit %d (stderr: %s), want %d with the verify prefix", code, stderr, exitUsage)
	}
}

func TestVerifySurvivesAnUnwritableStdout(t *testing.T) {
	logPath, _, pubPath := setupLog(t)
	var stderr bytes.Buffer
	if code := runEvidenceVerify([]string{"--log", logPath, "--key", pubPath}, failingWriter{}, &stderr, i18n.English()); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
}

// TestKeygenSurvivesAnUnwritableStdout: the key pair must exist even when
// reporting it fails — generation is the irreversible part.
func TestKeygenSurvivesAnUnwritableStdout(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "ed25519.key")
	var stderr bytes.Buffer
	if code := runEvidenceKeygen([]string{"--out", keyPath}, failingWriter{}, &stderr, i18n.English()); code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
	for _, p := range []string{keyPath, keyPath + ".pub"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was not written: %v", p, err)
		}
	}
}

// failingWriter simulates a stdout nobody can write to — a closed pipe, a
// full disk — which for a cron-driven tool is a real condition, not an
// edge case.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout is gone") }

// coreWitnessScript is one of the committed §9.2.5 scripts. The core's
// copy lives beside its own verifier, because sharing a fixture with
// spec/evidence would be sharing something with the module that exists in
// order to share nothing.
func coreWitnessScript(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "internal", "evidence", "testdata", name))
	if err != nil {
		t.Fatalf("resolve witness script: %v", err)
	}
	return path
}

// witnessFixture sets up a log and returns it with the head it carries,
// read the way an operator reads one — and asserts on the way through that
// a run with no witness reports a null one, which §9.2.3 requires on every
// run.
func witnessFixture(t *testing.T) (logPath, pubPath, head string) {
	t.Helper()
	logPath, _, pubPath = setupLog(t)
	code, stdout, stderr := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath)
	if code != exitValid {
		t.Fatalf("plain verify: exit %d (stderr: %s)", code, stderr)
	}
	var out verifyOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("verify output is not JSON: %v (%q)", err, stdout)
	}
	if out.Witness != nil {
		t.Errorf("witness = %+v, want null when none was asked", out.Witness)
	}
	return logPath, pubPath, fmt.Sprintf("%d:%s", out.Head.Seq, out.Head.Hash)
}

// TestVerifyWitnessAttestsThroughTheCLI drives §9.2 through the command an
// operator runs: one invocation reporting both whether the log is intact
// and whether an outside party attested its content.
func TestVerifyWitnessAttestsThroughTheCLI(t *testing.T) {
	const at = "2026-09-27T09:00:00Z"
	logPath, pubPath, head := witnessFixture(t)
	t.Setenv("PROBAVI_TEST_HEAD", head)
	t.Setenv("PROBAVI_TEST_AT", at)

	code, stdout, stderr := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath,
		"--witness", coreWitnessScript(t, "witness-expect.sh"))
	if code != exitValid {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitValid, stderr)
	}
	var out verifyOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("verify output is not JSON: %v (%q)", err, stdout)
	}
	if out.Witness == nil || !out.Witness.Attested {
		t.Fatalf("witness = %+v, want an attestation", out.Witness)
	}
	if out.Witness.AttestedAt == nil || *out.Witness.AttestedAt != at {
		t.Errorf("attested_at = %v, want %q", out.Witness.AttestedAt, at)
	}
}

// TestVerifyWitnessRefusalIsInvalid: a head no witness will attest is the
// same class of finding as a head that disagrees with an anchor.
func TestVerifyWitnessRefusalIsInvalid(t *testing.T) {
	logPath, pubPath, _ := witnessFixture(t)
	t.Setenv("PROBAVI_TEST_HEAD", "2:sha256:"+strings.Repeat("0", 64))

	code, stdout, _ := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath,
		"--witness", coreWitnessScript(t, "witness-expect.sh"))
	if code != exitInvalid {
		t.Fatalf("exit %d, want %d (stdout: %s)", code, exitInvalid, stdout)
	}
	if !strings.Contains(stdout, "holds no attestation") {
		t.Errorf("stdout = %q, want a reason naming the refusal", stdout)
	}
}

// TestVerifyWitnessFailureIsAUsageError: not exit 2. A witness that could
// not answer says nothing about the log, and turning that into INVALID
// would teach an operator that INVALID sometimes means nothing.
func TestVerifyWitnessFailureIsAUsageError(t *testing.T) {
	logPath, pubPath, _ := witnessFixture(t)

	code, stdout, _ := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath,
		"--witness", coreWitnessScript(t, "witness-fail.sh"))
	if code != exitUsage {
		t.Fatalf("exit %d, want %d (stdout: %s)", code, exitUsage, stdout)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want no status printed at all", stdout)
	}
}
