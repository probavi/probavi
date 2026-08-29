package checks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/probavi/probavi/internal/sandbox"
)

// queryResult is one sql_runner execution: succeeded reflects the runner's
// exit code, value the trimmed stdout, exitCode the runner's own code when
// it failed. The runner's stderr is deliberately not here — it reaches the
// drill host's log and nothing else (logRunnerFailure).
type queryResult struct {
	succeeded bool
	value     string
	exitCode  int
}

// query renders the sql_runner template and executes it in the sandbox.
func query(ctx context.Context, deps *Deps, sql string) (*queryResult, error) {
	argv, env, err := renderRunner(deps.Runner, deps.Target, sql)
	if err != nil {
		return nil, err
	}
	res, err := deps.Exec.Exec(ctx, sandbox.ExecRequest{Argv: argv, Env: env})
	if err != nil {
		return nil, fmt.Errorf("sql_runner exec: %w", err)
	}
	if res.ExitCode != 0 {
		logRunnerFailure(deps, argv[0], res)
		return &queryResult{succeeded: false, exitCode: res.ExitCode}, nil
	}
	return &queryResult{succeeded: true, value: strings.TrimSpace(string(res.Stdout))}, nil
}

// renderRunner substitutes the §6.1 placeholders: {{user}}, {{database}},
// {{sql}} in argv and env values; {{password}} in env values only — a
// password in argv would leak into process listings.
func renderRunner(r Runner, t Target, sql string) ([]string, map[string]string, error) {
	if len(r.Argv) == 0 {
		return nil, nil, fmt.Errorf("sql_runner template is empty — the adapter's probe did not declare one")
	}
	replace := func(s string) string {
		s = strings.ReplaceAll(s, "{{user}}", t.User)
		s = strings.ReplaceAll(s, "{{database}}", t.Database)
		return strings.ReplaceAll(s, "{{sql}}", sql)
	}
	argv := make([]string, len(r.Argv))
	for i, a := range r.Argv {
		if strings.Contains(a, "{{password}}") {
			return nil, nil, fmt.Errorf("sql_runner argv contains {{password}} — secrets belong in env values only")
		}
		argv[i] = replace(a)
	}
	var env map[string]string
	if len(r.Env) > 0 {
		env = make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			env[k] = strings.ReplaceAll(replace(v), "{{password}}", t.Password)
		}
	}
	return argv, env, nil
}

// logRunnerFailure puts the engine's own diagnostic where an operator can
// read it — the drill host's log — and nowhere else. It is never a check
// detail: details are signed into the evidence record, and an engine
// routinely quotes row data in its error text (PostgreSQL answers a
// violated unique constraint with `DETAIL: Key (email)=(...) already
// exists.`), which evidence-schema.md §8 forbids a record from carrying.
// runSQL has always refused to record the returned value for this reason;
// the diagnostic was the way around it.
//
// The whole diagnostic is logged rather than its first line: an engine
// puts the actionable part below the summary, and the sandbox has already
// capped what it captured — stderr_truncated says whether it did. The one
// secret this package holds is masked first, because an engine that
// echoes its connection settings back must not put a credential in a log
// either (AGENTS.md §3.3).
func logRunnerFailure(deps *Deps, runner string, res *sandbox.ExecResult) {
	deps.Logger.Warn("sql_runner exited non-zero",
		"runner", runner,
		"exit_code", res.ExitCode,
		"stderr", mask(strings.TrimSpace(string(res.Stderr)), deps.Target.Password),
		"stderr_truncated", res.Truncated)
}

// mask removes the ephemeral sandbox password wherever an engine echoed it
// back into its diagnostic.
func mask(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// timestampFormats covers the textual forms engines commonly print for
// max(timestamp) through their CLI runners. Naive timestamps (no zone) are
// interpreted as UTC — documented behavior for freshness checks.
var timestampFormats = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999",
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
}

func parseTimestamp(s string) (time.Time, error) {
	for _, layout := range timestampFormats {
		if ts, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return ts, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format")
}
