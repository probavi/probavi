package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/i18n"
	"github.com/probavi/probavi/internal/sandbox/remotehost"
)

// TestVersionCommand pins the version output: the binary version plus both
// contract versions, nothing on stderr, exit 0.
func TestVersionCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("version exit %d, want 0 (stderr: %s)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("version wrote to stderr: %q", stderr)
	}
	for _, want := range []string{"probavi " + version, adapter.ProtocolVersion, evidence.SchemaID} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version output %q does not contain %q", stdout, want)
		}
	}
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	// Tests run in the canonical English regardless of the machine's
	// locale; Hungarian output is exercised by the dedicated i18n tests.
	var out, errBuf bytes.Buffer
	code = run(args, &out, &errBuf, i18n.English())
	return code, out.String(), errBuf.String()
}

func testRecord(ts string) *evidence.Record {
	detail := "accepting connections"
	return &evidence.Record{
		Schema:  evidence.SchemaID,
		TS:      ts,
		Drill:   evidence.Drill{Name: "cli-test", ConfigHash: "sha256:" + strings.Repeat("ab", 32)},
		Backup:  evidence.Backup{Kind: "pgdump"},
		Adapter: evidence.Adapter{Name: "postgres", Protocol: "probavi-adapter/0"},
		Sandbox: evidence.Sandbox{Provider: "docker", Params: map[string]string{}},
		Checks:  []evidence.Check{{Name: "service_healthy", OK: true, Detail: &detail}},
		Outcome: evidence.OutcomePass,
		Env:     evidence.Env{ProbaviVersion: "test", OS: "linux", Arch: "amd64", HostID: "0123456789abcdef"},
	}
}

// setupLog generates a key pair through the CLI and writes a two-record log
// with it, returning the log and public key paths.
func setupLog(t *testing.T) (logPath, keyPath, pubPath string) {
	t.Helper()
	dir := t.TempDir()
	keyPath = filepath.Join(dir, "ed25519.key")
	pubPath = keyPath + ".pub"
	logPath = filepath.Join(dir, "evidence.jsonl")

	code, stdout, stderr := runCLI(t, "evidence", "keygen", "--out", keyPath)
	if code != 0 {
		t.Fatalf("keygen exit %d, stderr: %s", code, stderr)
	}
	var kg keygenOutput
	if err := json.Unmarshal([]byte(stdout), &kg); err != nil {
		t.Fatalf("keygen output is not JSON: %v (%q)", err, stdout)
	}

	signer, err := evidence.LoadSigner(keyPath)
	if err != nil {
		t.Fatalf("LoadSigner on generated key: %v", err)
	}
	if signer.KeyID() != kg.KeyID {
		t.Fatalf("keygen reported key_id %q, key derives %q", kg.KeyID, signer.KeyID())
	}
	st, err := evidence.Open(logPath, signer, nil)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	for _, ts := range []string{"2026-07-31T10:00:00.000Z", "2026-07-31T10:05:00.000Z"} {
		if err := st.Append(testRecord(ts)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return logPath, keyPath, pubPath
}

func TestKeygenThenVerifyRoundTrip(t *testing.T) {
	logPath, keyPath, pubPath := setupLog(t)

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode = %04o, want 0600", perm)
	}

	code, stdout, stderr := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath)
	if code != exitValid {
		t.Fatalf("verify exit %d, want 0; stderr: %s", code, stderr)
	}
	var res verifyOutput
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("verify output is not JSON: %v (%q)", err, stdout)
	}
	if res.Status != "VALID" || res.Records != 2 || len(res.DamagedLines) != 0 {
		t.Errorf("verify output = %+v, want VALID with 2 records", res)
	}
}

func TestKeygenRefusesOverwrite(t *testing.T) {
	_, keyPath, _ := setupLog(t)
	code, _, stderr := runCLI(t, "evidence", "keygen", "--out", keyPath)
	if code != exitUsage {
		t.Fatalf("keygen over existing key: exit %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, keyPath) {
		t.Errorf("stderr should name the offending file, got: %s", stderr)
	}
}

func TestVerifyDamagedLog(t *testing.T) {
	logPath, _, pubPath := setupLog(t)
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := f.WriteString(`{"torn":`); err != nil {
		t.Fatalf("append fragment: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	code, stdout, _ := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath)
	if code != exitValidWithDamage {
		t.Fatalf("verify exit %d, want %d (stdout: %s)", code, exitValidWithDamage, stdout)
	}
}

// TestVerifyEmptyLogWarnsWithoutChangingTheVerdict pins the one verdict a
// reader can mistake for good news. An intact, empty log is VALID and exits
// 0 — the specification says so (§9), and the exit code is not ours to move
// — so the difference between "nothing was ever proven" and "every drill
// verified" is stated on stderr instead. The machine-readable result on
// stdout is untouched: it already carries the record count.
func TestVerifyEmptyLogWarnsWithoutChangingTheVerdict(t *testing.T) {
	_, _, pubPath := setupLog(t)
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty log: %v", err)
	}

	code, stdout, stderr := runCLI(t, "evidence", "verify", "--log", empty, "--key", pubPath)
	if code != exitValid {
		t.Fatalf("exit = %d, want %d — the §9 exit code must not move", code, exitValid)
	}
	var res struct {
		Status  string `json:"status"`
		Records int    `json:"records"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not JSON (%q): %v", stdout, err)
	}
	if res.Status != "VALID" || res.Records != 0 {
		t.Errorf("result = %s with %d records, want VALID with 0", res.Status, res.Records)
	}
	if !strings.Contains(stderr, "holds no records") {
		t.Errorf("stderr = %q, want it to say the log proves nothing", stderr)
	}
}

// TestVerifyNonEmptyLogStaysQuiet is the other half: the warning must not
// fire for a log that did prove something, or it becomes noise nobody reads.
func TestVerifyNonEmptyLogStaysQuiet(t *testing.T) {
	logPath, _, pubPath := setupLog(t)
	code, _, stderr := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath)
	if code != exitValid {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitValid, stderr)
	}
	if strings.Contains(stderr, "holds no records") {
		t.Errorf("stderr = %q, want no empty-log warning for a log with records", stderr)
	}
}

func TestVerifyTamperedLog(t *testing.T) {
	logPath, _, pubPath := setupLog(t)
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	tampered := strings.Replace(string(raw), "cli-test", "cli-tesx", 1)
	if err := os.WriteFile(logPath, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}

	code, stdout, _ := runCLI(t, "evidence", "verify", "--log", logPath, "--key", pubPath)
	if code != exitInvalid {
		t.Fatalf("verify exit %d, want %d (stdout: %s)", code, exitInvalid, stdout)
	}
	var res verifyOutput
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("verify output is not JSON: %v", err)
	}
	if res.Status != "INVALID" || res.FailedLine != 1 || res.Reason == "" {
		t.Errorf("verify output = %+v, want INVALID at line 1 with a reason", res)
	}
}

func TestSandboxProviderResolution(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for _, name := range []string{"docker", "k8s"} {
		if p, err := sandboxProvider(name, nil, logger); err != nil || p == nil {
			t.Errorf("sandboxProvider(%q) = %v, %v", name, p, err)
		}
	}
	if _, err := sandboxProvider("nomad", nil, logger); err == nil || !strings.Contains(err.Error(), "supported: docker, k8s, remotehost") {
		t.Errorf("unknown provider: %v, want the supported list", err)
	}

	// remotehost needs its ssh target from the environment — never from
	// config, which is recorded verbatim in evidence records.
	t.Setenv(remotehost.EnvTarget, "")
	if _, err := sandboxProvider("remotehost", nil, logger); err == nil || !strings.Contains(err.Error(), remotehost.EnvTarget) {
		t.Errorf("remotehost without %s: %v, want a clear error", remotehost.EnvTarget, err)
	}
	t.Setenv(remotehost.EnvTarget, "drill@target.example")
	if p, err := sandboxProvider("remotehost", map[string]string{"memory": "1G"}, logger); err != nil || p == nil {
		t.Errorf("sandboxProvider(remotehost) = %v, %v", p, err)
	}
	if _, err := sandboxProvider("remotehost", map[string]string{"image": "x"}, logger); err == nil {
		t.Error("remotehost with invalid params must fail at wiring time")
	}
}

func TestUsageErrors(t *testing.T) {
	logPath, _, pubPath := setupLog(t)
	missing := filepath.Join(t.TempDir(), "nope")

	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"unknown command", []string{"restore"}},
		{"evidence without subcommand", []string{"evidence"}},
		{"evidence unknown subcommand", []string{"evidence", "sign"}},
		{"verify without flags", []string{"evidence", "verify"}},
		{"verify without key", []string{"evidence", "verify", "--log", logPath}},
		{"verify missing log file", []string{"evidence", "verify", "--log", missing, "--key", pubPath}},
		{"verify unreadable key", []string{"evidence", "verify", "--log", logPath, "--key", missing}},
		{"verify bad flag", []string{"evidence", "verify", "--no-such-flag"}},
		{"keygen without out", []string{"evidence", "keygen"}},
		{"keygen bad flag", []string{"evidence", "keygen", "--no-such-flag"}},
		{"keygen uncreatable path", []string{"evidence", "keygen", "--out", filepath.Join(missing, "sub", "k")}},
		{"gameday without config", []string{"gameday"}},
		{"gameday bad flag", []string{"gameday", "--no-such-flag"}},
		{"gameday missing file", []string{"gameday", "--config", missing}},
		{"adapter without subcommand", []string{"adapter"}},
		{"adapter unknown subcommand", []string{"adapter", "fuzz"}},
		{"conformance without adapter", []string{"adapter", "conformance"}},
		{"conformance bad source-param", []string{"adapter", "conformance", "--source-param", "novalue", "x"}},
		{"conformance unresolvable adapter", []string{"adapter", "conformance", "no-such-adapter-installed"}},
		{"conformance bad flag", []string{"adapter", "conformance", "--no-such-flag", "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, tt.args...)
			if code != exitUsage {
				t.Errorf("exit %d, want %d (stderr: %s)", code, exitUsage, stderr)
			}
		})
	}
}

// TestGameDayMemberSetupErrorAndSkip drives `probavi gameday` end to end
// without a container runtime: the first member cannot be wired (its
// adapter binary does not exist), so it reports a setup error, its
// dependent is skipped, and the exercise exits 2 with the full summary
// on stdout.
func TestGameDayMemberSetupErrorAndSkip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ed25519.key")
	if code, _, stderr := runCLI(t, "evidence", "keygen", "--out", keyPath); code != 0 {
		t.Fatalf("keygen exit %d, stderr: %s", code, stderr)
	}
	drill := `target:
  name: gd-unit-test
  adapter: no-such-engine
  source:
    kind: pgdump
    path: /backups/test.dump
sandbox:
  provider: docker
  timeout: 5m
checks:
  - builtin: service_healthy
evidence:
  path: ` + filepath.Join(dir, "evidence.jsonl") + `
  sign_key: ` + keyPath + `
`
	if err := os.WriteFile(filepath.Join(dir, "member.yaml"), []byte(drill), 0o600); err != nil {
		t.Fatalf("write member config: %v", err)
	}
	gameday := `name: gd-unit
timeout: 10m
members:
  - name: alpha
    config: member.yaml
  - name: beta
    config: member.yaml
    depends_on: [alpha]
`
	gdPath := filepath.Join(dir, "gameday.yaml")
	if err := os.WriteFile(gdPath, []byte(gameday), 0o600); err != nil {
		t.Fatalf("write game-day config: %v", err)
	}

	code, stdout, stderr := runCLI(t, "gameday", "--config", gdPath)
	if code != exitInvalid { // 2: errors left members unproven, nothing failed outright
		t.Fatalf("exit %d, want 2 (stderr: %s)", code, stderr)
	}
	summary := struct {
		GameDay string `json:"gameday"`
		Outcome string `json:"outcome"`
		Members []struct {
			Name       string `json:"name"`
			Outcome    string `json:"outcome"`
			ErrorCode  string `json:"error_code"`
			SkipReason string `json:"skip_reason"`
		} `json:"members"`
	}{}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("summary is not JSON: %v (%q)", err, stdout)
	}
	if summary.GameDay != "gd-unit" || summary.Outcome != "error" || len(summary.Members) != 2 {
		t.Fatalf("summary = %+v, want gd-unit error with 2 members", summary)
	}
	if m := summary.Members[0]; m.Outcome != "error" || m.ErrorCode != "setup_error" {
		t.Errorf("alpha = %+v, want error/setup_error", m)
	}
	if m := summary.Members[1]; m.Outcome != "skipped" || !strings.Contains(m.SkipReason, "alpha did not pass (error)") {
		t.Errorf("beta = %+v, want skipped behind alpha", m)
	}
	if !strings.Contains(stderr, "member alpha") {
		t.Errorf("stderr should attribute the wiring failure to member alpha, got: %s", stderr)
	}
}

// TestRunRejectsUnresolvedWebhookEnv proves webhook environment variables
// are resolved at wiring time: a drill with an unset url_env must abort
// before any sandbox or adapter work, naming the missing variable.
func TestRunRejectsUnresolvedWebhookEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "drill.yaml")
	cfg := `target:
  name: notify-env-test
  adapter: postgres
  source:
    kind: pgdump
    path: /backups/test.dump
sandbox:
  provider: docker
  timeout: 5m
checks:
  - builtin: service_healthy
evidence:
  path: ` + filepath.Join(dir, "evidence.jsonl") + `
  sign_key: ` + filepath.Join(dir, "unused.key") + `
notify:
  webhooks:
    - url_env: PROBAVI_TEST_UNSET_WEBHOOK_URL
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("PROBAVI_TEST_UNSET_WEBHOOK_URL", "")

	code, _, stderr := runCLI(t, "run", "--config", cfgPath)
	if code != exitUsage {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "PROBAVI_TEST_UNSET_WEBHOOK_URL") {
		t.Errorf("stderr should name the missing variable, got: %s", stderr)
	}
}
