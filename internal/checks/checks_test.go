package checks

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/sandbox"
)

// testRunner mirrors the postgres adapter's probe-declared template.
var testRunner = Runner{
	Argv: []string{"psql", "-U", "{{user}}", "-d", "{{database}}", "-tA", "-c", "{{sql}}"},
	Env:  map[string]string{"PGPASSWORD": "{{password}}"},
}

// fakeExec scripts sql_runner executions and records every request.
type fakeExec struct {
	t        *testing.T
	requests []sandbox.ExecRequest
	respond  func(sql string) *sandbox.ExecResult
	err      error
}

func (f *fakeExec) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	f.t.Helper()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.respond(req.Argv[len(req.Argv)-1]), nil
}

func (f *fakeExec) lastSQL() string {
	f.t.Helper()
	if len(f.requests) == 0 {
		f.t.Fatal("no sql_runner execution happened")
	}
	argv := f.requests[len(f.requests)-1].Argv
	return argv[len(argv)-1]
}

func value(v string) func(string) *sandbox.ExecResult {
	return func(string) *sandbox.ExecResult {
		return &sandbox.ExecResult{ExitCode: 0, Stdout: []byte(v + "\n")}
	}
}

func queryFailure(stderr string) func(string) *sandbox.ExecResult {
	return func(string) *sandbox.ExecResult {
		return &sandbox.ExecResult{ExitCode: 1, Stderr: []byte(stderr)}
	}
}

func testDeps(exec *fakeExec) Deps {
	return Deps{
		Exec: exec,
		Healthcheck: func(context.Context) (bool, string, error) {
			return true, "accepting queries", nil
		},
		Runner: testRunner,
		Target: Target{User: "u", Database: "d", Password: "s3cret"},
		Now:    func() time.Time { return time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC) },
	}
}

func runSingle(t *testing.T, c config.Check, exec *fakeExec) Result {
	t.Helper()
	results, err := Run(context.Background(), []config.Check{c}, testDeps(exec))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	return results[0]
}

func i64(n int64) *int64 { return &n }

func TestRenderRunner(t *testing.T) {
	argv, env, err := renderRunner(testRunner, Target{User: "u", Database: "d", Password: "pw"}, "SELECT 1")
	if err != nil {
		t.Fatalf("renderRunner: %v", err)
	}
	if got := strings.Join(argv, " "); got != "psql -U u -d d -tA -c SELECT 1" {
		t.Errorf("argv = %q", got)
	}
	if env["PGPASSWORD"] != "pw" {
		t.Errorf("env = %v — {{password}} must resolve in env values", env)
	}

	if _, _, err := renderRunner(Runner{Argv: []string{"tool", "{{password}}"}}, Target{}, "x"); err == nil {
		t.Error("renderRunner must reject {{password}} in argv — it would leak into process listings")
	}
	if _, _, err := renderRunner(Runner{}, Target{}, "x"); err == nil {
		t.Error("renderRunner must reject an empty template")
	}
}

func TestServiceHealthy(t *testing.T) {
	c := config.Check{Builtin: "service_healthy"}

	res := runSingle(t, c, &fakeExec{t: t})
	if !res.OK || res.Name != "service_healthy" || res.Detail != "accepting queries" {
		t.Errorf("result = %+v", res)
	}

	deps := testDeps(&fakeExec{t: t})
	deps.Healthcheck = func(context.Context) (bool, string, error) { return false, "psql exited 2", nil }
	results, err := Run(context.Background(), []config.Check{c}, deps)
	if err != nil || results[0].OK {
		t.Errorf("unhealthy: results=%+v err=%v", results, err)
	}

	deps.Healthcheck = func(context.Context) (bool, string, error) { return false, "", errors.New("adapter crashed") }
	if _, err := Run(context.Background(), []config.Check{c}, deps); err == nil {
		t.Error("healthcheck infrastructure failure must abort the run")
	}
}

func TestTableExists(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		exec := &fakeExec{t: t, respond: value("0")}
		res := runSingle(t, config.Check{Builtin: "table_exists", Table: "orders"}, exec)
		if !res.OK || res.Name != "table_exists:orders" || res.Detail != "table exists" {
			t.Errorf("result = %+v", res)
		}
		if exec.lastSQL() != `SELECT count(*) FROM "orders" WHERE 1=0` {
			t.Errorf("sql = %q", exec.lastSQL())
		}
	})
	t.Run("schema qualified", func(t *testing.T) {
		exec := &fakeExec{t: t, respond: value("0")}
		runSingle(t, config.Check{Builtin: "table_exists", Table: "sales.orders"}, exec)
		if exec.lastSQL() != `SELECT count(*) FROM "sales"."orders" WHERE 1=0` {
			t.Errorf("sql = %q", exec.lastSQL())
		}
	})
	t.Run("missing table is a verdict", func(t *testing.T) {
		exec := &fakeExec{t: t, respond: queryFailure(`ERROR: relation "orders" does not exist` + "\nLINE 1: ...")}
		res := runSingle(t, config.Check{Builtin: "table_exists", Table: "orders"}, exec)
		if res.OK || !strings.Contains(res.Detail, "sql_runner exited 1") {
			t.Errorf("result = %+v — a failed runner is recorded by its exit code", res)
		}
	})
	t.Run("injection attempt aborts the run", func(t *testing.T) {
		poisoned := []config.Check{
			{Builtin: "table_exists", Table: `orders"; DROP TABLE x; --`},
			{Builtin: "row_count", Table: "orders; --", Min: i64(1)},
			{Builtin: "freshness", Table: "bad name", Column: "created_at", MaxAge: config.Duration(time.Hour)},
			{Builtin: "freshness", Table: "orders", Column: `c"ol`, MaxAge: config.Duration(time.Hour)},
		}
		for _, c := range poisoned {
			exec := &fakeExec{t: t, respond: value("0")}
			_, err := Run(context.Background(), []config.Check{c}, testDeps(exec))
			if err == nil || len(exec.requests) != 0 {
				t.Errorf("check %+v: err=%v requests=%d — poisoned identifiers must never reach the engine", c, err, len(exec.requests))
			}
		}
	})
}

func TestRunDefaults(t *testing.T) {
	// Nil Now falls back to time.Now; an impossible check shape (guarded
	// by config validation) is an infrastructure error, not a panic.
	deps := testDeps(&fakeExec{t: t, respond: value("0")})
	deps.Now = nil
	if _, err := Run(context.Background(), []config.Check{{Builtin: "table_exists", Table: "t"}}, deps); err != nil {
		t.Errorf("Run with nil Now: %v", err)
	}
	if _, err := Run(context.Background(), []config.Check{{}}, deps); err == nil {
		t.Error("empty check shape must be an error")
	}
}

func TestBrokenRunnerTemplateAbortsRun(t *testing.T) {
	exec := &fakeExec{t: t, respond: value("1")}
	deps := testDeps(exec)
	deps.Runner = Runner{Argv: []string{"tool", "{{password}}"}}
	_, err := Run(context.Background(), []config.Check{{SQL: "SELECT 1", Expect: config.ScalarFromString("1")}}, deps)
	if err == nil || len(exec.requests) != 0 {
		t.Errorf("err=%v requests=%d — a template leaking secrets into argv must never execute", err, len(exec.requests))
	}
}

func TestRowCount(t *testing.T) {
	base := config.Check{Builtin: "row_count", Table: "orders"}
	tests := []struct {
		name       string
		min, max   *int64
		output     func(string) *sandbox.ExecResult
		wantOK     bool
		wantDetail string
	}{
		{"within min", i64(100), nil, value("1000"), true, "1000 rows (min 100)"},
		{"within max", nil, i64(2000), value("1000"), true, "1000 rows (max 2000)"},
		{"within both", i64(100), i64(2000), value("1000"), true, "1000 rows (min 100, max 2000)"},
		{"below min", i64(1001), nil, value("1000"), false, "1000 rows (min 1001)"},
		{"above max", nil, i64(999), value("1000"), false, "1000 rows (max 999)"},
		{"garbage output", i64(1), nil, value("banana"), false, "unexpected output"},
		{"query failure", i64(1), nil, queryFailure("ERROR: permission denied"), false, "count query failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			c.Min, c.Max = tt.min, tt.max
			exec := &fakeExec{t: t, respond: tt.output}
			res := runSingle(t, c, exec)
			if res.OK != tt.wantOK || !strings.Contains(res.Detail, tt.wantDetail) {
				t.Errorf("result = %+v, want ok=%v detail~%q", res, tt.wantOK, tt.wantDetail)
			}
			if exec.lastSQL() != `SELECT count(*) FROM "orders"` {
				t.Errorf("sql = %q", exec.lastSQL())
			}
		})
	}
}

func TestFreshness(t *testing.T) {
	base := config.Check{Builtin: "freshness", Table: "orders", Column: "created_at"}
	maxAge := config.Check{}
	_ = maxAge
	withAge := func(d time.Duration) config.Check {
		c := base
		c.MaxAge = config.Duration(d)
		return c
	}
	tests := []struct {
		name       string
		check      config.Check
		output     func(string) *sandbox.ExecResult
		wantOK     bool
		wantDetail string
	}{
		{"fresh with offset tz", withAge(2 * time.Hour), value("2026-07-31 01:00:00+00"), true, "newest row is 1h0m0s old (max_age 2h0m0s)"},
		{"fresh with colon tz and fraction", withAge(2 * time.Hour), value("2026-07-31 03:00:00.123+02:00"), true, "59m59s old"},
		{"stale", withAge(30 * time.Minute), value("2026-07-31 01:00:00+00"), false, "1h0m0s old (max_age 30m0s)"},
		{"naive timestamp treated as UTC", withAge(2 * time.Hour), value("2026-07-31 01:30:00"), true, "30m0s old"},
		{"future timestamp counts as fresh", withAge(time.Hour), value("2026-07-31 02:30:00+00"), true, "0s old"},
		{"empty table", withAge(time.Hour), value(""), false, "no rows or only NULL"},
		{"unparseable", withAge(time.Hour), value("yesterday-ish"), false, "unparseable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExec{t: t, respond: tt.output}
			res := runSingle(t, tt.check, exec)
			if res.OK != tt.wantOK || !strings.Contains(res.Detail, tt.wantDetail) {
				t.Errorf("result = %+v, want ok=%v detail~%q", res, tt.wantOK, tt.wantDetail)
			}
			if res.Name != "freshness:orders.created_at" {
				t.Errorf("name = %q", res.Name)
			}
			if exec.lastSQL() != `SELECT max("created_at") FROM "orders"` {
				t.Errorf("sql = %q", exec.lastSQL())
			}
		})
	}
}

func TestSQLCheck(t *testing.T) {
	c := config.Check{Name: "no-negatives", SQL: "SELECT count(*) = 0 FROM orders WHERE total < 0",
		Expect: config.ScalarFromString("true")}

	t.Run("match", func(t *testing.T) {
		exec := &fakeExec{t: t, respond: value("true")}
		res := runSingle(t, c, exec)
		if !res.OK || res.Name != "sql:no-negatives" || res.Detail != "matched expectation" {
			t.Errorf("result = %+v", res)
		}
		if exec.lastSQL() != c.SQL {
			t.Errorf("sql = %q — custom SQL must pass through verbatim", exec.lastSQL())
		}
	})
	t.Run("mismatch never leaks the value", func(t *testing.T) {
		exec := &fakeExec{t: t, respond: value("secret-user-data")}
		res := runSingle(t, c, exec)
		if res.OK || strings.Contains(res.Detail, "secret-user-data") {
			t.Errorf("result = %+v — returned values must never enter evidence details", res)
		}
	})
	t.Run("query failure", func(t *testing.T) {
		exec := &fakeExec{t: t, respond: queryFailure("ERROR: syntax error")}
		res := runSingle(t, c, exec)
		if res.OK || !strings.Contains(res.Detail, "query failed") {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("unnamed uses index", func(t *testing.T) {
		unnamed := config.Check{SQL: "SELECT 1", Expect: config.ScalarFromString("1")}
		exec := &fakeExec{t: t, respond: value("1")}
		res := runSingle(t, unnamed, exec)
		if res.Name != "sql:0" {
			t.Errorf("name = %q", res.Name)
		}
	})
}

func TestQueryVerdictsAndInfraAborts(t *testing.T) {
	t.Run("row_count query failure is a verdict", func(t *testing.T) {
		res := runSingle(t, config.Check{Builtin: "row_count", Table: "orders", Min: i64(1)},
			&fakeExec{t: t, respond: queryFailure("ERROR: disk on fire")})
		if res.OK || !strings.Contains(res.Detail, "count query failed") {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("freshness query failure is a verdict", func(t *testing.T) {
		res := runSingle(t, config.Check{Builtin: "freshness", Table: "orders", Column: "created_at",
			MaxAge: config.Duration(time.Hour)}, &fakeExec{t: t, respond: queryFailure("ERROR: nope")})
		if res.OK || !strings.Contains(res.Detail, "freshness query failed") {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("infrastructure failure aborts every table builtin", func(t *testing.T) {
		for _, c := range []config.Check{
			{Builtin: "table_exists", Table: "t"},
			{Builtin: "row_count", Table: "t", Min: i64(1)},
			{Builtin: "freshness", Table: "t", Column: "c", MaxAge: config.Duration(time.Hour)},
		} {
			exec := &fakeExec{t: t, err: errors.New("sandbox died")}
			if _, err := Run(context.Background(), []config.Check{c}, testDeps(exec)); err == nil {
				t.Errorf("check %+v must abort on infrastructure failure", c)
			}
		}
	})
}

func TestRunAbortsOnInfrastructureFailure(t *testing.T) {
	list := []config.Check{
		{Builtin: "row_count", Table: "orders", Min: i64(1)},
		{SQL: "SELECT 1", Expect: config.ScalarFromString("1")},
	}
	exec := &fakeExec{t: t}
	exec.respond = func(string) *sandbox.ExecResult {
		// Fail at transport level from the second call on.
		exec.err = errors.New("sandbox died")
		return &sandbox.ExecResult{ExitCode: 0, Stdout: []byte("1\n")}
	}
	deps := testDeps(exec)
	results, err := Run(context.Background(), list, deps)
	if err == nil || !strings.Contains(err.Error(), "sandbox died") {
		t.Fatalf("err = %v, want transport failure", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Errorf("partial results = %+v, want the first verdict preserved", results)
	}
	if !strings.Contains(err.Error(), "sql:1") {
		t.Errorf("err = %v, want the failing check named", err)
	}
}

func TestQuoteIdent(t *testing.T) {
	valid := map[string]string{
		"orders":       `"orders"`,
		"sales.orders": `"sales"."orders"`,
		"_x1":          `"_x1"`,
	}
	for in, want := range valid {
		if got, err := quoteIdent(in); err != nil || got != want {
			t.Errorf("quoteIdent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{`a"b`, "a;b", "a b", "a.b.c", "1abc", "", "a.", `x); DROP`} {
		if _, err := quoteIdent(in); err == nil {
			t.Errorf("quoteIdent(%q) succeeded, want rejection", in)
		}
	}
}

func TestDetailTruncation(t *testing.T) {
	// The service_healthy detail comes from the adapter and may be any
	// UTF-8 (localized messages, accented identifiers). Truncating such a
	// detail by byte offset splits a rune, and the invalid UTF-8 makes the
	// evidence record unwritable — a completed drill that leaves no proof.
	// Every stride is exercised so that no cut position can regress.
	for _, filler := range []string{"e", "é", "€", "𝄞"} {
		t.Run(filler, func(t *testing.T) {
			long := strings.Repeat(filler, 500)
			exec := &fakeExec{t: t, respond: value("1")}
			deps := testDeps(exec)
			deps.Healthcheck = func(context.Context) (bool, string, error) { return true, long, nil }
			results, err := Run(context.Background(),
				[]config.Check{{Builtin: config.CheckServiceHealthy}}, deps)
			if err != nil || len(results) != 1 {
				t.Fatalf("Run = %+v, %v", results, err)
			}
			res := results[0]
			switch {
			case len(res.Detail) > evidence.MaxDetailBytes:
				t.Errorf("detail is %d bytes, want at most %d", len(res.Detail), evidence.MaxDetailBytes)
			case !strings.HasSuffix(res.Detail, "..."):
				t.Errorf("detail %q is not marked as truncated", res.Detail)
			case !utf8.ValidString(res.Detail):
				t.Errorf("detail is not valid UTF-8 — the evidence record would be rejected")
			}
		})
	}
}

// TestEveryRegisteredKindIsRunnable is the runner half of the check
// registry gate. config.CheckKinds is what docs/capabilities.json
// publishes and what config validation admits; this proves the runner
// dispatches every one of them, so a published check can never be one that
// reaches "unrunnable check configuration" at drill time.
func TestEveryRegisteredKindIsRunnable(t *testing.T) {
	for _, k := range config.CheckKinds() {
		t.Run(k.ID, func(t *testing.T) {
			exec := &fakeExec{t: t, respond: value("1")}
			c := config.Check{}
			if k.Builtin {
				c.Builtin = k.ID
			}
			for _, p := range k.Params {
				switch p.Name {
				case "table":
					c.Table = "orders"
				case "column":
					c.Column = "created_at"
				case "max_age":
					c.MaxAge = config.Duration(24 * time.Hour)
				case "sql":
					c.SQL = "SELECT 1"
				}
			}
			if k.ID == config.CheckRowCount {
				// The registry's Requires rule: at least one bound.
				bound := int64(0)
				c.Min = &bound
			}
			if _, err := Run(context.Background(), []config.Check{c}, testDeps(exec)); err != nil {
				t.Fatalf("registered kind %q is not runnable: %v", k.ID, err)
			}
		})
	}
}

// TestUnrunnableConfigurationIsAnInfrastructureError keeps the guard that
// makes the gate above meaningful: a check shape the runner cannot
// dispatch must abort loudly rather than report a silent pass.
func TestUnrunnableConfigurationIsAnInfrastructureError(t *testing.T) {
	exec := &fakeExec{t: t, respond: value("1")}
	_, err := Run(context.Background(), []config.Check{{Builtin: "not_registered"}}, testDeps(exec))
	if err == nil || !strings.Contains(err.Error(), "unrunnable check configuration") {
		t.Fatalf("err = %v, want an unrunnable-configuration failure", err)
	}
}

// TestEngineDiagnosticsNeverReachTheDetail is the §8 redaction rule as a
// gate. An engine quotes row data in its error text — PostgreSQL answers a
// violated unique constraint with `DETAIL: Key (email)=(...) already
// exists.` — and a check detail is signed into a record meant to be handed
// to an auditor as it stands. runSQL has always refused to record the
// returned value for exactly this reason; the diagnostic was the way
// around it. It goes to the drill host's log instead, where the operator
// can read it, with the ephemeral sandbox password masked: an engine that
// echoes its connection settings must not put a credential in a log
// either (AGENTS.md §3.3).
func TestEngineDiagnosticsNeverReachTheDetail(t *testing.T) {
	const (
		rowData = `DETAIL: Key (email)=(alice@example.com) already exists.`
		secret  = "ephemeral-sandbox-password"
	)
	stderr := "ERROR: duplicate key value violates unique constraint \"orders_email_key\"\n" +
		rowData + "\nconnection: password=" + secret

	log := &bytes.Buffer{}
	deps := testDeps(&fakeExec{t: t, respond: queryFailure(stderr)})
	deps.Target.Password = secret
	deps.Logger = slog.New(slog.NewTextHandler(log, nil))

	results, err := Run(context.Background(), []config.Check{
		{SQL: "SELECT 1", Expect: config.ScalarFromString("1")},
		{Builtin: config.CheckTableExists, Table: "orders"},
		{Builtin: config.CheckRowCount, Table: "orders", Min: i64(1)},
		{Builtin: config.CheckFreshness, Table: "orders", Column: "created_at",
			MaxAge: config.Duration(time.Hour)},
	}, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want one per check", len(results))
	}
	for _, res := range results {
		if res.OK {
			t.Errorf("%s passed on a runner that exited 1", res.Name)
		}
		for _, leaked := range []string{"alice@example.com", "duplicate key", "orders_email_key", secret} {
			if strings.Contains(res.Detail, leaked) {
				t.Errorf("%s detail carries the engine's own text (%q): %q", res.Name, leaked, res.Detail)
			}
		}
		if !strings.Contains(res.Detail, "sql_runner exited 1") {
			t.Errorf("%s detail = %q, want the runner's exit code", res.Name, res.Detail)
		}
	}
	logged := log.String()
	if !strings.Contains(logged, rowData) {
		t.Error("the diagnostic must reach the drill host's log — it is the operator's only copy of it")
	}
	if strings.Contains(logged, secret) {
		t.Errorf("the log carries the sandbox password: %s", logged)
	}
}

// TestMask covers both halves of the one redaction this package performs
// on its way to the log. A drill whose engine needs no password has
// nothing to mask, and the diagnostic must reach the operator unaltered;
// one that does must never see it echoed back into a log line.
func TestMask(t *testing.T) {
	for name, tt := range map[string]struct{ in, secret, want string }{
		"nothing to mask":    {"FATAL: connection refused", "", "FATAL: connection refused"},
		"secret echoed back": {`could not connect: "password=hunter2"`, "hunter2", `could not connect: "password=[redacted]"`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mask(tt.in, tt.secret); got != tt.want {
				t.Errorf("mask(%q, %q) = %q, want %q", tt.in, tt.secret, got, tt.want)
			}
		})
	}
}
