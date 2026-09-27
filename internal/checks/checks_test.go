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

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/manifest"
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
		// The basic-format offset, which cqlsh prints and which this list
		// did not carry until issue #277: a value the runner delivered
		// correctly was read as "unparseable output".
		{"fresh with basic-format tz", withAge(2 * time.Hour), value("2026-07-31 01:00:00.294000+0000"), true, "59m59s old (max_age 2h0m0s)"},
		{"stale with basic-format tz", withAge(30 * time.Minute), value("2026-07-31 01:00:00.294000+0000"), false, "59m59s old (max_age 30m0s)"},
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
	// The zero Dialect is what every v0 adapter means, and it must keep
	// spelling identifiers exactly as the core always has.
	var standard Dialect
	valid := map[string]string{
		"orders":       `"orders"`,
		"sales.orders": `"sales"."orders"`,
		"_x1":          `"_x1"`,
	}
	for in, want := range valid {
		if got, err := standard.quote(in); err != nil || got != want {
			t.Errorf("quote(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{`a"b`, "a;b", "a b", "a.b.c", "1abc", "", "a.", `x); DROP`} {
		if _, err := standard.quote(in); err == nil {
			t.Errorf("quote(%q) succeeded, want rejection", in)
		}
	}
}

// TestDeclaredQuotingIsHonoured: the engines that motivated the bump. One
// refuses SQL-standard quoting outright; another spells a qualified name
// with brackets. The core applies what was declared and validates exactly
// as before, which is why no declaration can widen what it accepts.
func TestDeclaredQuotingIsHonoured(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		in      string
		want    string
	}{
		{"bare names", Dialect{Separator: "."}, "sales.orders", "sales.orders"},
		{"backticks", Dialect{Open: "`", Close: "`", Separator: "."}, "orders", "`orders`"},
		{"brackets", Dialect{Open: "[", Close: "]", Separator: "."}, "sales.orders", "[sales].[orders]"},
		{"other separator", Dialect{Open: "", Close: "", Separator: ":"}, "sales.orders", "sales:orders"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.dialect.quote(tc.in)
			if err != nil || got != tc.want {
				t.Errorf("quote(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}

	// Validation does not move with the quoting: whatever an adapter
	// declares, a part that could carry a statement is still refused.
	bare := Dialect{Separator: "."}
	for _, in := range []string{`a"b`, "a;b", "a b", `x); DROP`, "a.b.c"} {
		if _, err := bare.quote(in); err == nil {
			t.Errorf("quote(%q) succeeded under a bare-name dialect, want rejection", in)
		}
	}
}

// TestDeclaredStatementReplacesTheComposedOne: the point of §6.1.1.
func TestDeclaredStatementReplacesTheComposedOne(t *testing.T) {
	d := Dialect{
		Separator:  ".",
		Statements: map[string]string{"row_count": "SELECT COUNT(*) FROM {{table}} WHERE 1=1"},
	}
	got := d.statement("row_count", "SELECT count(*) FROM `t`", "`t`", "")
	if got != "SELECT COUNT(*) FROM `t` WHERE 1=1" {
		t.Errorf("statement = %q, want the declaration with the identifier substituted", got)
	}
	// An undeclared kind still gets the core's composition, which is what
	// lets an adapter declare one built-in and leave the others alone.
	if got := d.statement("freshness", "SELECT max(c) FROM t", "t", "c"); got != "SELECT max(c) FROM t" {
		t.Errorf("statement = %q, want the core's own composition", got)
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
	// The log must also name *which program* failed, which is the first
	// thing an operator needs: a runner that exited 1 is either the engine
	// refusing the statement or the client not being in the image, and the
	// program name is what tells the two apart. Mutation testing found this
	// unasserted — logging argv[1] instead of argv[0] would have named a
	// flag, and nothing would have noticed.
	if !strings.Contains(logged, testRunner.Argv[0]) {
		t.Errorf("the log does not name the program that failed (%q): %s", testRunner.Argv[0], logged)
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

// baselineDeps is testDeps plus what the backup manifest declared.
func baselineDeps(exec *fakeExec, baseline map[string]manifest.Expectation) Deps {
	deps := testDeps(exec)
	deps.Baseline = baseline
	return deps
}

// exact and ranged spell an expectation the way §2.1 does.
func exact(n int64) manifest.Expectation { return manifest.Expectation{Rows: i64(n)} }

func ranged(lo, hi int64) manifest.Expectation {
	return manifest.Expectation{RowsMin: i64(lo), RowsMax: i64(hi)}
}

// TestBaselineExpandsToOneResultPerTable is the design §5.1 states: one
// configured check, one result per table the manifest declared. The names
// are what a record carries, so they are the assertion.
func TestBaselineExpandsToOneResultPerTable(t *testing.T) {
	exec := &fakeExec{t: t, respond: value("100")}
	results, err := Run(context.Background(), []config.Check{{Builtin: config.CheckBaseline}},
		baselineDeps(exec, map[string]manifest.Expectation{
			"orders":          exact(100),
			"public.invoices": ranged(90, 110),
			"customers":       exact(100),
		}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Sorted, not map order: a record's checks are read side by side
	// across drills, and an order that reshuffled per run would make two
	// records of one drill look like different drills.
	want := []string{"baseline:customers", "baseline:orders", "baseline:public.invoices"}
	if len(results) != len(want) {
		t.Fatalf("results = %d, want %d", len(results), len(want))
	}
	for i, name := range want {
		if results[i].Name != name {
			t.Errorf("results[%d].Name = %q, want %q", i, results[i].Name, name)
		}
		if !results[i].OK {
			t.Errorf("%s = %+v, want a pass", name, results[i])
		}
	}
}

// TestBaselineOrderIsStable runs the same reconciliation repeatedly: map
// iteration is random, so an unsorted expansion would pass a single run and
// fail an audit that compared two records.
func TestBaselineOrderIsStable(t *testing.T) {
	baseline := map[string]manifest.Expectation{}
	for _, name := range []string{"zeta", "alpha", "mu", "beta", "omega", "kappa", "delta"} {
		baseline[name] = exact(1)
	}
	var first []string
	for i := range 20 {
		exec := &fakeExec{t: t, respond: value("1")}
		results, err := Run(context.Background(), []config.Check{{Builtin: config.CheckBaseline}},
			baselineDeps(exec, baseline))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		names := make([]string, len(results))
		for j, r := range results {
			names[j] = r.Name
		}
		if i == 0 {
			first = names
			continue
		}
		if strings.Join(names, ",") != strings.Join(first, ",") {
			t.Fatalf("two runs produced different orders:\n%v\n%v", first, names)
		}
	}
}

// TestBaselineVerdicts covers what one table's reconciliation can produce.
// The detail is part of the record, so it is asserted rather than ignored:
// a reader has to see the count and the expectation side by side to know
// what moved.
func TestBaselineVerdicts(t *testing.T) {
	tests := []struct {
		name       string
		want       manifest.Expectation
		output     func(string) *sandbox.ExecResult
		wantOK     bool
		wantDetail string
	}{
		{"exact count met", exact(100000), value("100000"), true, "100000 rows (backup manifest states 100000)"},
		{"exact count short", exact(100000), value("90000"), false, "90000 rows (backup manifest states 100000)"},
		{"exact count over", exact(100000), value("100001"), false, "100001 rows (backup manifest states 100000)"},
		{"range met", ranged(4980, 5020), value("5000"), true, "5000 rows (backup manifest states 4980–5020)"},
		{"range low bound", ranged(4980, 5020), value("4980"), true, "4980 rows"},
		{"range high bound", ranged(4980, 5020), value("5020"), true, "5020 rows"},
		{"range missed", ranged(4980, 5020), value("4979"), false, "4979 rows (backup manifest states 4980–5020)"},
		{"zero met", exact(0), value("0"), true, "0 rows (backup manifest states 0)"},
		// A table the restored database does not have is a false verdict
		// for that table, not an abandoned drill (§5.1). The
		// reconciliation asked a question the restore could not answer,
		// and that is the finding.
		{"table absent", exact(1), queryFailure(`ERROR:  relation "orders" does not exist`), false, "count query failed"},
		{"unreadable output", exact(1), value("one hundred"), false, "unexpected output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExec{t: t, respond: tt.output}
			results, err := Run(context.Background(), []config.Check{{Builtin: config.CheckBaseline}},
				baselineDeps(exec, map[string]manifest.Expectation{"orders": tt.want}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("results = %d, want 1", len(results))
			}
			if results[0].OK != tt.wantOK || !strings.Contains(results[0].Detail, tt.wantDetail) {
				t.Errorf("result = %+v, want ok=%v detail~%q", results[0], tt.wantOK, tt.wantDetail)
			}
			if exec.lastSQL() != `SELECT count(*) FROM "orders"` {
				t.Errorf("sql = %q", exec.lastSQL())
			}
		})
	}
}

// TestBaselineAsksTheRowCountQuestion is why no protocol version moves for
// this: the statement that counts rows already exists, and baseline asks a
// second question of the same answer (§5.1). An adapter that declared one
// gets it used, without declaring anything new.
func TestBaselineAsksTheRowCountQuestion(t *testing.T) {
	exec := &fakeExec{t: t, respond: value("42")}
	deps := baselineDeps(exec, map[string]manifest.Expectation{"orders": exact(42)})
	deps.Dialect = Dialect{
		Statements: map[string]string{config.CheckRowCount: "db.{{table}}.countDocuments()"},
		Open:       "", Close: "", Separator: ".",
	}
	results, err := Run(context.Background(), []config.Check{{Builtin: config.CheckBaseline}}, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].OK {
		t.Errorf("result = %+v, want a pass", results[0])
	}
	if got, want := exec.lastSQL(), "db.orders.countDocuments()"; got != want {
		t.Errorf("statement = %q, want the adapter's declared %q", got, want)
	}
}

// TestBaselineWithATableTheEngineCannotName is the one infrastructure
// failure this check can hit: a name the identifier rule refuses cannot be
// quoted, so nothing can be asked. The manifest reader refuses such a name
// first, so reaching this is a defect — but it must abandon the run rather
// than sign a verdict about a question nobody asked.
func TestBaselineWithATableTheEngineCannotName(t *testing.T) {
	exec := &fakeExec{t: t, respond: value("1")}
	_, err := Run(context.Background(), []config.Check{{Builtin: config.CheckBaseline}},
		baselineDeps(exec, map[string]manifest.Expectation{"order items": exact(1)}))
	if err == nil {
		t.Fatal("an unquotable table produced a verdict instead of an error")
	}
	if !strings.Contains(err.Error(), config.CheckBaseline) {
		t.Errorf("error %q does not name the check", err)
	}
}

// TestBaselineWithNothingDeclaredRunsNothing: the core refuses this
// configuration where the manifest is read, so this package's behaviour is
// only that it invents no result. A pass here would be a check that
// validated nothing reporting success.
func TestBaselineWithNothingDeclaredRunsNothing(t *testing.T) {
	exec := &fakeExec{t: t, respond: value("1")}
	results, err := Run(context.Background(), []config.Check{{Builtin: config.CheckBaseline}}, testDeps(exec))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want none", results)
	}
	if len(exec.requests) != 0 {
		t.Errorf("%d statements ran with nothing to reconcile", len(exec.requests))
	}
}

// TestBaselineKeepsWhatRanWhenSomethingBreaks: Run returns partial results
// on an infrastructure failure, and the tables reconciled before it must
// survive — a drill that reconciled four tables and then lost the sandbox
// still found out something about those four.
func TestBaselineKeepsWhatRanWhenSomethingBreaks(t *testing.T) {
	var calls int
	exec := &fakeExec{t: t}
	// The sandbox survives two counts and then dies, so the run has both
	// halves: results that were reached, and a failure that stopped the rest.
	exec.respond = func(string) *sandbox.ExecResult {
		calls++
		if calls > 2 {
			exec.err = errors.New("sandbox died")
		}
		return &sandbox.ExecResult{ExitCode: 0, Stdout: []byte("1\n")}
	}
	results, err := Run(context.Background(), []config.Check{{Builtin: config.CheckBaseline}},
		baselineDeps(exec, map[string]manifest.Expectation{
			"a": exact(1), "b": exact(1), "c": exact(1), "d": exact(1),
		}))
	if err == nil {
		t.Fatal("a dead sandbox produced no error")
	}
	if len(results) == 0 {
		t.Error("the tables reconciled before the failure were discarded")
	}
	if len(results) == 4 {
		t.Error("every table reported despite the sandbox dying")
	}
}

// DialectFrom is the conversion the core actually uses, and the rest of
// this suite skips it by building a Dialect by hand.
//
// Mutation testing found it: every branch survived, because nothing here
// had ever driven it. A defect in this function does not produce a wrong
// answer — it produces an adapter's declarations silently never taking
// effect, with the core going on composing its own statements and every
// check still passing. That is the failure conformance check 16 exists to
// prevent one level up, and it deserves an assertion here too.

// TestDialectFromNothingDeclared: the zero Dialect is what every v0
// adapter means, and the core composes its own statements from it.
func TestDialectFromNothingDeclared(t *testing.T) {
	for name, probe := range map[string]*adapter.ProbeResult{
		"no probe at all": nil,
		"an empty probe":  {},
	} {
		t.Run(name, func(t *testing.T) {
			if got := DialectFrom(probe); got.Statements != nil || got.Separator != "" {
				t.Errorf("DialectFrom = %+v, want the zero Dialect", got)
			}
		})
	}
}

// TestDialectFromStatementsOnly: an adapter may declare some kinds and no
// identifier at all — the declarations are independent of each other.
func TestDialectFromStatementsOnly(t *testing.T) {
	got := DialectFrom(&adapter.ProbeResult{Checks: map[string]adapter.CheckStatement{
		config.CheckRowCount:    {Statement: "db.{{table}}.countDocuments()"},
		config.CheckTableExists: {Statement: "assert({{table}})"},
	}})
	if len(got.Statements) != 2 {
		t.Fatalf("Statements = %v, want both declarations", got.Statements)
	}
	if got.Statements[config.CheckRowCount] != "db.{{table}}.countDocuments()" {
		t.Errorf("row_count = %q", got.Statements[config.CheckRowCount])
	}
	if got.Open != "" || got.Close != "" || got.Separator != "" {
		t.Errorf("identifier = %+v, want nothing declared", got)
	}
}

// TestDialectFromIdentifierOnly is the other half of that independence.
func TestDialectFromIdentifierOnly(t *testing.T) {
	got := DialectFrom(&adapter.ProbeResult{
		Identifier: &adapter.Identifier{Open: "`", Close: "`", Separator: "."},
	})
	if got.Open != "`" || got.Close != "`" || got.Separator != "." {
		t.Errorf("identifier = %+v, want the declared backticks", got)
	}
	if got.Statements != nil {
		t.Errorf("Statements = %v, want nil when none was declared", got.Statements)
	}
}

// TestDialectFromReachesTheStatement is the end of the path: what a probe
// declared reaches the statement a check runs, quoting included.
func TestDialectFromReachesTheStatement(t *testing.T) {
	exec := &fakeExec{t: t, respond: value("7")}
	deps := testDeps(exec)
	deps.Dialect = DialectFrom(&adapter.ProbeResult{
		Identifier: &adapter.Identifier{Open: "", Close: "", Separator: "."},
		Checks: map[string]adapter.CheckStatement{
			config.CheckRowCount: {Statement: "db.{{table}}.countDocuments()"},
		},
	})
	// Run directly rather than through runSingle, which builds its own
	// deps and would discard the Dialect under test.
	results, err := Run(context.Background(), []config.Check{
		{Builtin: config.CheckRowCount, Table: "orders", Min: i64(1)},
	}, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].OK {
		t.Errorf("result = %+v, want a pass", results[0])
	}
	if got, want := exec.lastSQL(), "db.orders.countDocuments()"; got != want {
		t.Errorf("statement = %q, want the declared %q", got, want)
	}
}

// TestRowCountBoundsAreInclusive pins the boundary the documentation calls
// inclusive. Mutation testing found both edges unasserted: a count exactly
// equal to min or max could have been refused without a test noticing, and
// `row_count … max: 1000` on a table holding exactly 1000 rows is the
// commonest configuration there is.
func TestRowCountBoundsAreInclusive(t *testing.T) {
	tests := []struct {
		name     string
		min, max *int64
		count    string
		wantOK   bool
	}{
		{"exactly at min", i64(1000), nil, "1000", true},
		{"one below min", i64(1000), nil, "999", false},
		{"exactly at max", nil, i64(1000), "1000", true},
		{"one above max", nil, i64(1000), "1001", false},
		{"exactly at both", i64(1000), i64(1000), "1000", true},
		{"zero rows against a zero minimum", i64(0), nil, "0", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Check{Builtin: config.CheckRowCount, Table: "orders", Min: tc.min, Max: tc.max}
			res := runSingle(t, c, &fakeExec{t: t, respond: value(tc.count)})
			if res.OK != tc.wantOK {
				t.Errorf("%s rows against min=%v max=%v: ok=%v, want %v (%s)",
					tc.count, tc.min, tc.max, res.OK, tc.wantOK, res.Detail)
			}
		})
	}
}

// TestFreshnessBoundaryAndFutureTimestamps pins two things mutation testing
// found unasserted: max_age is inclusive, and a timestamp in the future is
// clamped to zero age rather than producing a negative one.
//
// The clamp is not defensive decoration. A restored copy can legitimately
// carry a timestamp ahead of the drill host's clock — the two hosts are
// different machines, and env.clock_synchronised exists because the drill
// host's own clock is a belief. A negative age would print as
// "newest row is -3m0s old", which is a record nobody can read.
func TestFreshnessBoundaryAndFutureTimestamps(t *testing.T) {
	now := time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC)
	withAge := func(d time.Duration) config.Check {
		return config.Check{Builtin: config.CheckFreshness, Table: "orders", Column: "created_at",
			MaxAge: config.Duration(d)}
	}
	tests := []struct {
		name       string
		stamp      time.Time
		maxAge     time.Duration
		wantOK     bool
		wantDetail string
	}{
		{"exactly at max_age", now.Add(-24 * time.Hour), 24 * time.Hour, true, "24h0m0s old"},
		{"one second past max_age", now.Add(-24*time.Hour - time.Second), 24 * time.Hour, false, "24h0m1s old"},
		{"one second inside", now.Add(-24*time.Hour + time.Second), 24 * time.Hour, true, "23h59m59s old"},
		{"the same instant as now", now, 24 * time.Hour, true, "0s old"},
		{"three minutes in the future", now.Add(3 * time.Minute), 24 * time.Hour, true, "0s old"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExec{t: t, respond: value(tc.stamp.Format(time.RFC3339))}
			deps := testDeps(exec)
			deps.Now = func() time.Time { return now }
			results, err := Run(context.Background(), []config.Check{withAge(tc.maxAge)}, deps)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			res := results[0]
			if res.OK != tc.wantOK || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("result = %+v, want ok=%v detail~%q", res, tc.wantOK, tc.wantDetail)
			}
			if strings.Contains(res.Detail, "-") {
				t.Errorf("detail carries a negative age: %q", res.Detail)
			}
		})
	}
}
