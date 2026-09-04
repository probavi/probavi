package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/i18n"
)

// validYAML is a minimal drill config that must pass validation; invalid
// test cases are derived from it by targeted replacement.
const validYAML = `target:
  name: test-db
  adapter: postgres
  source:
    kind: pgdump
    path: /backups/test.dump
sandbox:
  provider: docker
  params:
    image: postgres:16
  timeout: 30m
checks:
  - builtin: service_healthy
evidence:
  path: /var/lib/probavi/evidence.jsonl
  sign_key: /etc/probavi/ed25519.key
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drill.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadExampleConfig(t *testing.T) {
	// The committed example must always parse and validate: this test is
	// what keeps README/examples honest (AGENTS.md §5.5).
	cfg, err := Load(filepath.Join("..", "..", "examples", "drill.example.yaml"), nil)
	if err != nil {
		t.Fatalf("Load(examples/drill.example.yaml, nil): %v", err)
	}
	if cfg.Target.Name != "prod-orders-db" || cfg.Target.Adapter != "postgres" {
		t.Errorf("target = %+v, want prod-orders-db/postgres", cfg.Target)
	}
	if cfg.Sandbox.Provider != "docker" || cfg.Sandbox.Params["image"] != "postgres:16" {
		t.Errorf("sandbox = %+v, want docker with postgres:16 image", cfg.Sandbox)
	}
	if cfg.Sandbox.Timeout.Std() != 30*time.Minute {
		t.Errorf("timeout = %v, want 30m", cfg.Sandbox.Timeout.Std())
	}
	if len(cfg.Checks) != 5 {
		t.Fatalf("checks = %d, want 5", len(cfg.Checks))
	}
	sql := cfg.Checks[4]
	if sql.Name != "no-negative-totals" || !sql.Expect.IsSet() || sql.Expect.String() != "0" {
		t.Errorf("sql check = %+v, want named with expect 0", sql)
	}
	fresh := cfg.Checks[3]
	if fresh.Builtin != "freshness" || fresh.MaxAge.Std() != 24*time.Hour {
		t.Errorf("freshness check = %+v, want max_age 24h", fresh)
	}
	if cfg.Metrics == nil || cfg.Metrics.PrometheusTextfile == "" {
		t.Errorf("metrics = %+v, want prometheus_textfile set", cfg.Metrics)
	}
	assertExampleNotify(t, cfg)
}

// assertExampleNotify checks the example's notify section: one env-based
// webhook with HMAC and an on filter, one literal-URL webhook on defaults.
func assertExampleNotify(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.Notify == nil || len(cfg.Notify.Webhooks) != 2 {
		t.Fatalf("notify = %+v, want 2 webhooks", cfg.Notify)
	}
	first := cfg.Notify.Webhooks[0]
	if first.URLEnv != "PROBAVI_WEBHOOK_URL" || first.SecretEnv != "PROBAVI_WEBHOOK_SECRET" {
		t.Errorf("webhook[0] = %+v, want url_env and secret_env set", first)
	}
	if len(first.On) != 2 || first.On[0] != "fail" || first.On[1] != "error" {
		t.Errorf("webhook[0].On = %v, want [fail error]", first.On)
	}
	second := cfg.Notify.Webhooks[1]
	if second.URL == "" || second.URLEnv != "" || len(second.On) != 0 {
		t.Errorf("webhook[1] = %+v, want literal url with default on", second)
	}
}

func TestLoadComputesConfigHash(t *testing.T) {
	path := writeConfig(t, validYAML)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sum := sha256.Sum256([]byte(validYAML))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if cfg.Hash != want {
		t.Errorf("Hash = %q, want %q", cfg.Hash, want)
	}
	if cfg.Path != path {
		t.Errorf("Path = %q, want %q", cfg.Path, path)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string // substrings the error must contain
	}{
		{"unknown top-level field", validYAML + "banana: yes\n", []string{"unknown field", "banana"}},
		{"unknown field with typo", strings.Replace(validYAML, "adapter:", "adaptor:", 1), []string{"unknown field", "adaptor"}},
		{"duplicate key", validYAML + "sandbox:\n  provider: docker\n", []string{"duplicate"}},
		{"missing target name", strings.Replace(validYAML, "name: test-db", "name: \"\"", 1), []string{"target.name is required"}},
		{"bad adapter name", strings.Replace(validYAML, "adapter: postgres", "adapter: Postgres_16", 1), []string{"target.adapter", "probavi-adapter-"}},
		{"missing source kind", strings.Replace(validYAML, "kind: pgdump", "kind: \"\"", 1), []string{"target.source.kind is required"}},
		{"bad credential env name", strings.Replace(validYAML, "    path: /backups/test.dump", "    path: /backups/test.dump\n    credential_env: [\"1BAD-NAME\"]", 1), []string{"credential_env", "1BAD-NAME"}},
		{"missing provider", strings.Replace(validYAML, "provider: docker", "provider: \"\"", 1), []string{"sandbox.provider is required"}},
		{"missing timeout", strings.Replace(validYAML, "  timeout: 30m\n", "", 1), []string{"sandbox.timeout is required"}},
		{"bad duration", strings.Replace(validYAML, "timeout: 30m", "timeout: half an hour", 1), []string{"invalid duration"}},
		{"negative duration", strings.Replace(validYAML, "timeout: 30m", "timeout: -5m", 1), []string{"must be positive"}},
		{"no checks", strings.Replace(validYAML, "checks:\n  - builtin: service_healthy\n", "checks: []\n", 1), []string{"at least one check"}},
		{"unknown builtin", strings.Replace(validYAML, "builtin: service_healthy", "builtin: row_cnt", 1), []string{"unknown builtin", "row_cnt", "supported:"}},
		{"builtin and sql together", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: service_healthy\n    sql: SELECT 1", 1), []string{"not both"}},
		{"neither builtin nor sql", strings.Replace(validYAML, "- builtin: service_healthy", "- table: orders", 1), []string{"exactly one of builtin or sql"}},
		{"service_healthy with table", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: service_healthy\n    table: orders", 1), []string{"table is not valid for service_healthy"}},
		{"table_exists without table", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: table_exists", 1), []string{"table_exists requires table"}},
		{"row_count without bounds", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: row_count\n    table: orders", 1), []string{"min, max, or both"}},
		{"row_count negative min", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: row_count\n    table: orders\n    min: -1", 1), []string{"must not be negative"}},
		{"row_count min above max", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: row_count\n    table: orders\n    min: 10\n    max: 5", 1), []string{"min (10) exceeds max (5)"}},
		{"freshness without column", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: freshness\n    table: orders\n    max_age: 24h", 1), []string{"freshness requires column"}},
		{"freshness without max_age", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: freshness\n    table: orders\n    column: created_at", 1), []string{"freshness requires max_age"}},
		{"sql without expect", strings.Replace(validYAML, "- builtin: service_healthy", "- sql: SELECT 1", 1), []string{"require expect"}},
		{"sql with table", strings.Replace(validYAML, "- builtin: service_healthy", "- sql: SELECT 1\n    expect: 1\n    table: orders", 1), []string{"table is not valid for sql checks"}},
		{"builtin with expect", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: service_healthy\n    expect: true", 1), []string{"expect is only valid for sql checks"}},
		{"service_healthy with all forbidden fields", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: service_healthy\n    column: c\n    min: 1\n    max_age: 1h", 1), []string{"column is not valid", "min/max are not valid", "max_age is not valid"}},
		{"sql with all forbidden fields", strings.Replace(validYAML, "- builtin: service_healthy", "- sql: SELECT 1\n    expect: 1\n    column: c\n    max: 2\n    max_age: 1h", 1), []string{"column is not valid for sql checks", "min/max are not valid for sql checks", "max_age is not valid for sql checks"}},
		{"non-string timeout", strings.Replace(validYAML, "timeout: 30m", "timeout: [30]", 1), []string{"duration must be a string"}},
		{"builtin with name", strings.Replace(validYAML, "- builtin: service_healthy", "- builtin: service_healthy\n    name: nope", 1), []string{"name is only valid for sql checks"}},
		{"expect float", strings.Replace(validYAML, "- builtin: service_healthy", "- sql: SELECT 1.5\n    expect: 1.5", 1), []string{"string, boolean, or integer"}},
		{"missing evidence path", strings.Replace(validYAML, "path: /var/lib/probavi/evidence.jsonl", "path: \"\"", 1), []string{"evidence.path is required"}},
		{"missing sign key", strings.Replace(validYAML, "sign_key: /etc/probavi/ed25519.key", "sign_key: \"\"", 1), []string{"evidence.sign_key is required", "keygen"}},
		{"empty metrics section", validYAML + "metrics:\n  prometheus_textfile: \"\"\n", []string{"metrics.prometheus_textfile is required"}},
		{"pitr with both targets", strings.Replace(validYAML, "adapter: postgres", "adapter: postgres\n  pitr:\n    target_time: \"2026-07-30T14:32:00Z\"\n    target_age: 24h", 1), []string{"exactly one of target_time"}},
		{"pitr with neither target", strings.Replace(validYAML, "adapter: postgres", "adapter: postgres\n  pitr: {}", 1), []string{"exactly one of target_time"}},
		{"pitr bad target_time", strings.Replace(validYAML, "adapter: postgres", "adapter: postgres\n  pitr:\n    target_time: \"yesterday 14:32\"", 1), []string{"not an RFC 3339 timestamp"}},
		{"pitr negative target_age", strings.Replace(validYAML, "adapter: postgres", "adapter: postgres\n  pitr:\n    target_age: -24h", 1), []string{"must be positive"}},
		{"empty notify section", validYAML + "notify:\n  webhooks: []\n", []string{"notify.webhooks must list at least one webhook"}},
		{"webhook with url and url_env", validYAML + "notify:\n  webhooks:\n    - url: https://example.internal/hook\n      url_env: HOOK_URL\n", []string{"notify.webhooks[0]", "not both"}},
		{"webhook with neither url nor url_env", validYAML + "notify:\n  webhooks:\n    - on: [fail]\n", []string{"notify.webhooks[0]", "exactly one of url or url_env"}},
		{"webhook relative url", validYAML + "notify:\n  webhooks:\n    - url: not-a-url\n", []string{"notify.webhooks[0]", "absolute http(s) URL"}},
		{"webhook non-http url", validYAML + "notify:\n  webhooks:\n    - url: ftp://example.internal/hook\n", []string{"notify.webhooks[0]", "absolute http(s) URL"}},
		{"webhook bad url_env name", validYAML + "notify:\n  webhooks:\n    - url_env: 1BAD-NAME\n", []string{"notify.webhooks[0]", "url_env", "1BAD-NAME"}},
		{"webhook bad secret_env name", validYAML + "notify:\n  webhooks:\n    - url: https://example.internal/hook\n      secret_env: bad name\n", []string{"notify.webhooks[0]", "secret_env", "bad name"}},
		{"webhook unknown on outcome", validYAML + "notify:\n  webhooks:\n    - url: https://example.internal/hook\n      on: [pass, success]\n", []string{"notify.webhooks[0]", `unknown outcome "success"`, "supported: pass, fail, error, cancelled"}},
		{"webhook duplicate on outcome", validYAML + "notify:\n  webhooks:\n    - url: https://example.internal/hook\n      on: [fail, fail]\n", []string{"notify.webhooks[0]", `duplicate outcome "fail"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml), nil)
			if err == nil {
				t.Fatal("Load accepted an invalid config")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestPITRResolve(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	abs := strings.Replace(validYAML, "adapter: postgres",
		"adapter: postgres\n  pitr:\n    target_time: \"2026-07-30T14:32:00+02:00\"", 1)
	cfg, err := Load(writeConfig(t, abs), nil)
	if err != nil {
		t.Fatalf("Load absolute pitr: %v", err)
	}
	got := cfg.Target.PITR.Resolve(now).UTC()
	if want := time.Date(2026, 7, 30, 12, 32, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Resolve(target_time) = %v, want %v (offset normalized to UTC)", got, want)
	}

	rel := strings.Replace(validYAML, "adapter: postgres",
		"adapter: postgres\n  pitr:\n    target_age: 24h", 1)
	cfg, err = Load(writeConfig(t, rel), nil)
	if err != nil {
		t.Fatalf("Load relative pitr: %v", err)
	}
	if got := cfg.Target.PITR.Resolve(now); !got.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("Resolve(target_age) = %v, want now−24h", got)
	}
}

// TestLoadHungarianDiagnostics proves validation diagnostics speak the
// injected translator's language end to end (docs/i18n.md).
func TestLoadHungarianDiagnostics(t *testing.T) {
	hu, err := i18n.New("hu")
	if err != nil {
		t.Fatalf("New(hu): %v", err)
	}
	bad := strings.Replace(validYAML, "name: test-db", `name: ""`, 1)
	_, err = Load(writeConfig(t, bad), hu)
	if err == nil {
		t.Fatal("Load accepted an invalid config")
	}
	for _, want := range []string{"konfiguráció érvénytelen", "a target.name megadása kötelező"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "target.name is required") {
		t.Errorf("error %q still contains English", err)
	}

	if _, err := LoadGameDay(writeGameDay(t, "name: gd\ntimeout: 1h\nmembers: []\n"), hu); err == nil ||
		!strings.Contains(err.Error(), "legalább egy tag szükséges") {
		t.Errorf("game-day error = %v, want Hungarian member diagnostic", err)
	}
}

func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	bad := strings.Replace(validYAML, "name: test-db", "name: \"\"", 1)
	bad = strings.Replace(bad, "provider: docker", "provider: \"\"", 1)
	bad = strings.Replace(bad, "sign_key: /etc/probavi/ed25519.key", "sign_key: \"\"", 1)
	_, err := Load(writeConfig(t, bad), nil)
	if err == nil {
		t.Fatal("Load accepted an invalid config")
	}
	for _, want := range []string{"target.name", "sandbox.provider", "evidence.sign_key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should report %q too; got:\n%v", want, err)
		}
	}
}

func TestLoadFileProblems(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), nil); err == nil {
		t.Error("Load accepted a missing file")
	}
	if _, err := Load(writeConfig(t, ""), nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty config: got %v, want empty-config error", err)
	}
	if _, err := Load(writeConfig(t, "target: [broken\n"), nil); err == nil {
		t.Error("Load accepted broken YAML syntax")
	}
}

func TestScalarNormalization(t *testing.T) {
	tests := []struct {
		yaml string
		want string
	}{
		{"expect: true", "true"},
		{"expect: false", "false"},
		{"expect: 42", "42"},
		{"expect: -3", "-3"},
		{"expect: 18446744073709551615", "18446744073709551615"},
		{"expect: hello", "hello"},
		{"expect: \"1.5\"", "1.5"}, // quoted: a string, allowed
	}
	for _, tt := range tests {
		t.Run(tt.yaml, func(t *testing.T) {
			y := strings.Replace(validYAML, "- builtin: service_healthy", "- sql: SELECT 1\n    "+tt.yaml, 1)
			cfg, err := Load(writeConfig(t, y), nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Checks[0].Expect.String(); got != tt.want {
				t.Errorf("Expect = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPITRRejectsAFutureTarget covers the one point-in-time target that
// cannot be proven. An engine handed a future instant simply recovers as
// far as it can, so the drill would quietly prove something other than
// what the config asked for — and the usual cause is a typed year or
// month, which is worth catching before a sandbox exists.
func TestPITRRejectsAFutureTarget(t *testing.T) {
	tests := []struct {
		name       string
		targetTime string
		wantErr    bool
	}{
		{"a past instant is fine", "2020-07-30T14:32:00Z", false},
		{"a mistyped year is refused", "2999-07-30T14:32:00Z", true},
		{"an offset that resolves to the past is fine", "2020-07-30T16:32:00+02:00", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &problems{tr: i18n.English()}
			(&PITR{TargetTime: tt.targetTime}).validate(p)
			err := errors.Join(p.errs...)
			switch {
			case tt.wantErr && err == nil:
				t.Error("a future target must be refused at load, before any sandbox exists")
			case !tt.wantErr && err != nil:
				t.Errorf("unexpected rejection: %v", err)
			case tt.wantErr && !strings.Contains(err.Error(), "in the future"):
				t.Errorf("error = %v, want it to say the target is in the future", err)
			}
		})
	}

	t.Run("ordinary clock skew is absorbed", func(t *testing.T) {
		// A config written on a host whose clock runs slightly ahead must
		// not fail a drill; only a target far enough ahead to be a mistake
		// is refused.
		skewed := time.Now().Add(pitrClockSkewGrace / 2).UTC().Format(time.RFC3339)
		p := &problems{tr: i18n.English()}
		(&PITR{TargetTime: skewed}).validate(p)
		if err := errors.Join(p.errs...); err != nil {
			t.Errorf("target %q within the skew grace was refused: %v", skewed, err)
		}
	})

	t.Run("a refused target leaves nothing resolvable", func(t *testing.T) {
		pt := &PITR{TargetTime: "2999-07-30T14:32:00Z"}
		p := &problems{tr: i18n.English()}
		pt.validate(p)
		if !pt.parsedTime.IsZero() {
			t.Error("a rejected target_time must not be cached for Resolve")
		}
	})
}

// TestLoadersReportAMalformedDocumentRatherThanCrashing pins the boundary
// around the YAML decoder.
//
// Found by FuzzLoadGameDay and reproduced on the drill loader: on the
// pinned decoder version a tag on an empty node where a sequence belongs
// dereferences nil inside its own AST walk, and the process dies. That is
// the worst shape a config error can take — no diagnostic, no record, and
// an exit code that says the binary died rather than that the file is
// wrong.
func TestLoadersReportAMalformedDocumentRatherThanCrashing(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		load func(string) error
	}{
		"drill config, a tagged empty sequence": {
			"drill:\n  name: x\nchecks: !000000 \n",
			func(p string) error { _, err := Load(p, nil); return err },
		},
		"game-day, a tagged empty depends_on": {
			"name: g\ntimeout: 1h\nmembers:\n  - name: A\n    depends_on: !000000 \n",
			func(p string) error { _, err := LoadGameDay(p, nil); return err },
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			err := tc.load(path)
			if err == nil {
				t.Fatal("a document the decoder cannot read must be an error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error = %q, want it to name the file", err)
			}
		})
	}
}
