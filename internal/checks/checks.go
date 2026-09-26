// Package checks executes drill validation checks against a restored
// database — through the adapter-declared sql_runner template and the
// sandbox exec verb, so the core never learns an engine concept. Verdicts
// are per-check; only infrastructure failures abort a run.
//
// Details obey the evidence redaction rules (evidence-schema.md §8):
// aggregates (counts, ages, latencies) yes, query result values no, and
// no engine diagnostic text — a failed check records that the runner
// failed and with what exit code, while the engine's own message goes to
// the drill host's log (runner.go).
package checks

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/probavi/probavi/internal/adapter"
	"github.com/probavi/probavi/internal/config"
	"github.com/probavi/probavi/internal/evidence"
	"github.com/probavi/probavi/internal/sandbox"
)

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Execer runs one command inside the sandbox; *docker.Sandbox implements it.
type Execer interface {
	Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error)
}

// Runner is the sql_runner template from the adapter's probe response
// (adapter protocol §6.1).
type Runner struct {
	Argv []string
	Env  map[string]string
}

// Target identifies the restored database from the provision result.
// Password carries the resolved secret value for {{password}} substitution
// in runner env values — it must never appear in argv, results, or logs.
type Target struct {
	User     string
	Database string
	Password string
}

// Deps are the injected collaborators for one check run.
type Deps struct {
	Exec Execer
	// Healthcheck runs the adapter's healthcheck operation; the
	// service_healthy builtin delegates to it.
	Healthcheck func(ctx context.Context) (healthy bool, detail string, err error)
	Runner      Runner
	Target      Target
	// Now is injectable for freshness tests; nil means time.Now.
	Now func() time.Time
	// Logger receives what a check must not record: the engine's own
	// diagnostics on a failed runner. Nil discards them.
	Logger *slog.Logger
	// Dialect is what the adapter declared about how to ask (adapter
	// protocol §6.1.1). The zero value is what every v0 adapter means:
	// the core composes its own statements and quotes SQL-standard.
	Dialect Dialect
}

// Dialect carries an adapter's §6.1.1 declarations into the check runner.
//
// It exists so the core stops writing SQL for engines that have none or
// have their own — four built-ins composed here produced four
// engine-specific failures across the catalogue. What the adapter chooses
// is how to ask; what the answer means stays the core's, so a record says
// the same thing for every engine.
type Dialect struct {
	// Statements maps a built-in check kind to the statement the adapter
	// declared for it. A kind absent here is composed by the core.
	Statements map[string]string
	// Open, Close and Separator spell a qualified identifier. Separator
	// empty means nothing was declared, and the SQL-standard default
	// applies — a declared Identifier always carries one, while Open and
	// Close are legitimately empty for an engine that takes bare names.
	Open, Close, Separator string
}

// DialectFrom carries an adapter's §6.1.1 declarations into this package.
// It lives here rather than in the caller because the declarations are
// consumed here, and because more than one caller drives checks against a
// probe — two conversions would be two chances to forget one.
//
// Nothing declared yields the zero Dialect: the core composes its own
// statements and quotes SQL-standard, which is what every v0 adapter
// means.
func DialectFrom(probe *adapter.ProbeResult) Dialect {
	d := Dialect{}
	if probe == nil {
		return d
	}
	if probe.Identifier != nil {
		d.Open, d.Close, d.Separator = probe.Identifier.Open, probe.Identifier.Close, probe.Identifier.Separator
	}
	for kind, declared := range probe.Checks {
		if d.Statements == nil {
			d.Statements = make(map[string]string, len(probe.Checks))
		}
		d.Statements[kind] = declared.Statement
	}
	return d
}

// statement is what to run for a built-in kind: the adapter's declaration
// with the identifiers substituted, or the core's own composition when the
// adapter declared none. The core substitutes and runs; it does not parse,
// and a declared statement is not required to be SQL.
func (d Dialect) statement(kind, composed, table, column string) string {
	tmpl, ok := d.Statements[kind]
	if !ok {
		return composed
	}
	return strings.NewReplacer("{{table}}", table, "{{column}}", column).Replace(tmpl)
}

// quote validates a possibly qualified identifier and spells it the way
// the adapter declared. Validation is the core's and never moves: each
// part must match identPattern, so a drill configuration cannot inject a
// statement and no declared quoting rule can make it able to — a
// validated part cannot contain any quoting character.
func (d Dialect) quote(name string) (string, error) {
	open, closing, sep := `"`, `"`, "."
	if d.Separator != "" {
		open, closing, sep = d.Open, d.Close, d.Separator
	}
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return "", fmt.Errorf("invalid identifier %s: at most schema.name", name)
	}
	for i, part := range parts {
		if !identPattern.MatchString(part) {
			return "", fmt.Errorf("invalid identifier: %s", name)
		}
		parts[i] = open + part + closing
	}
	return strings.Join(parts, sep), nil
}

// Result is one executed check, ready to be mapped into an evidence record.
// There is no duration field: the evidence schema records per-phase
// timings, not per-check ones, so measuring here would produce a number
// nothing may publish.
type Result struct {
	Name string
	OK   bool
	// NewestData is the instant a freshness check read out of the
	// restored data, kept because the record carries it as
	// backup.newest_data_at (evidence-schema.md §3). Nil for every other
	// kind, and for a freshness check that read nothing.
	//
	// It is deliberately the check's by-product rather than a measurement
	// of its own: reading the newest value means a statement in the
	// engine's own dialect, and freshness is where a drill already says
	// which table and column that statement is about.
	NewestData *time.Time
	Detail     string
}

// Run executes every check in order. A false verdict does not stop the run
// — evidence records each check individually; the returned error is
// reserved for infrastructure failures (sandbox death, invalid template),
// after which the partial results are still returned.
func Run(ctx context.Context, list []config.Check, deps Deps) ([]Result, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	results := make([]Result, 0, len(list))
	for i := range list {
		res, err := runOne(ctx, &list[i], i, &deps)
		if err != nil {
			return results, fmt.Errorf("check %s: %w", checkName(&list[i], i), err)
		}
		results = append(results, *res)
	}
	return results, nil
}

func runOne(ctx context.Context, c *config.Check, i int, deps *Deps) (*Result, error) {
	var (
		ok     bool
		detail string
		newest *time.Time
		err    error
	)
	switch {
	case c.SQL != "":
		ok, detail, err = runSQL(ctx, deps, c.SQL, c.Expect.String())
	case c.Builtin == config.CheckServiceHealthy:
		ok, detail, err = deps.Healthcheck(ctx)
	case c.Builtin == config.CheckTableExists:
		ok, detail, err = runTableExists(ctx, deps, c.Table)
	case c.Builtin == config.CheckRowCount:
		ok, detail, err = runRowCount(ctx, deps, c)
	case c.Builtin == config.CheckFreshness:
		ok, detail, newest, err = runFreshness(ctx, deps, c)
	default:
		// config.Load validates check shapes; reaching this is a bug.
		err = fmt.Errorf("unrunnable check configuration")
	}
	if err != nil {
		return nil, err
	}
	return &Result{
		Name:       checkName(c, i),
		OK:         ok,
		Detail:     truncateDetail(detail),
		NewestData: newest,
	}, nil
}

// checkName derives the evidence record name (evidence-schema.md §3).
func checkName(c *config.Check, i int) string {
	if c.SQL != "" {
		if c.Name != "" {
			return "sql:" + c.Name
		}
		return "sql:" + strconv.Itoa(i)
	}
	switch c.Builtin {
	case config.CheckTableExists, config.CheckRowCount:
		return c.Builtin + ":" + c.Table
	case config.CheckFreshness:
		return c.Builtin + ":" + c.Table + "." + c.Column
	default:
		return c.Builtin
	}
}

func runTableExists(ctx context.Context, deps *Deps, table string) (bool, string, error) {
	ident, err := deps.Dialect.quote(table)
	if err != nil {
		return false, "", err
	}
	stmt := deps.Dialect.statement(config.CheckTableExists, "SELECT count(*) FROM "+ident+" WHERE 1=0", ident, "")
	out, qerr := query(ctx, deps, stmt)
	if qerr != nil {
		return false, "", qerr
	}
	if !out.succeeded {
		return false, "table is not queryable: " + runnerFailure(out), nil
	}
	return true, "table exists", nil
}

func runRowCount(ctx context.Context, deps *Deps, c *config.Check) (bool, string, error) {
	ident, err := deps.Dialect.quote(c.Table)
	if err != nil {
		return false, "", err
	}
	stmt := deps.Dialect.statement(config.CheckRowCount, "SELECT count(*) FROM "+ident, ident, "")
	out, qerr := query(ctx, deps, stmt)
	if qerr != nil {
		return false, "", qerr
	}
	if !out.succeeded {
		return false, "count query failed: " + runnerFailure(out), nil
	}
	n, perr := strconv.ParseInt(out.value, 10, 64)
	if perr != nil {
		return false, "count query returned unexpected output", nil
	}
	ok := (c.Min == nil || n >= *c.Min) && (c.Max == nil || n <= *c.Max)
	return ok, fmt.Sprintf("%d rows (%s)", n, boundsText(c.Min, c.Max)), nil
}

func runFreshness(ctx context.Context, deps *Deps, c *config.Check) (bool, string, *time.Time, error) {
	table, err := deps.Dialect.quote(c.Table)
	if err != nil {
		return false, "", nil, err
	}
	column, err := deps.Dialect.quote(c.Column)
	if err != nil {
		return false, "", nil, err
	}
	stmt := deps.Dialect.statement(config.CheckFreshness, "SELECT max("+column+") FROM "+table, table, column)
	out, qerr := query(ctx, deps, stmt)
	if qerr != nil {
		return false, "", nil, qerr
	}
	if !out.succeeded {
		return false, "freshness query failed: " + runnerFailure(out), nil, nil
	}
	if out.value == "" {
		return false, "table has no rows or only NULL timestamps", nil, nil
	}
	newest, perr := parseTimestamp(out.value)
	if perr != nil {
		return false, "timestamp column returned unparseable output", nil, nil
	}
	age := deps.Now().Sub(newest)
	if age < 0 {
		age = 0
	}
	maxAge := c.MaxAge.Std()
	// The instant goes back whatever the verdict is: a check that failed
	// because the data is old still read when the data is from, and that
	// is exactly the number a record carrying backup.newest_data_at wants.
	return age <= maxAge, fmt.Sprintf("newest row is %s old (max_age %s)",
		age.Truncate(time.Second), maxAge), &newest, nil
}

func runSQL(ctx context.Context, deps *Deps, sql, expect string) (bool, string, error) {
	out, qerr := query(ctx, deps, sql)
	if qerr != nil {
		return false, "", qerr
	}
	if !out.succeeded {
		return false, "query failed: " + runnerFailure(out), nil
	}
	// Redaction: never embed the returned value — a custom query may
	// select anything; the check name identifies what mismatched.
	if out.value != expect {
		return false, "query returned a different value than expected", nil
	}
	return true, "matched expectation", nil
}

// runnerFailure is what a failed check may say about the runner: that it
// failed, and with which of its own exit codes. The engine's message is
// not here by design — see runner.go and evidence-schema.md §8.
func runnerFailure(out *queryResult) string {
	return fmt.Sprintf("sql_runner exited %d", out.exitCode)
}

func boundsText(minBound, maxBound *int64) string {
	switch {
	case minBound != nil && maxBound != nil:
		return fmt.Sprintf("min %d, max %d", *minBound, *maxBound)
	case minBound != nil:
		return fmt.Sprintf("min %d", *minBound)
	default:
		return fmt.Sprintf("max %d", *maxBound)
	}
}

// truncateDetail keeps details inside the evidence limit. It delegates to
// the evidence package rather than slicing: the service_healthy detail
// comes from the adapter and may be any UTF-8, and a cut inside a
// multi-byte rune produces invalid UTF-8 that the record layer rejects —
// turning a completed drill into one with no evidence at all.
func truncateDetail(s string) string {
	return evidence.TruncateLine(s, evidence.MaxDetailBytes)
}
