package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// scriptRunBudget bounds a stub run: the runner is a handful of shell
// commands, so anything slower is a hang, not slow hardware.
const scriptRunBudget = 30 * time.Second

// Measured promtool output, one line per shape it produces (issue #175).
// The scalar has no `=>` at all, and the last one carries the separator
// inside a label value — both are why the filter keys on the evaluation
// instant being the final field rather than on the separator.
const (
	sampleNoLabels    = "{} => 45886 @[1787113801.349]"
	sampleWithLabels  = `up{instance="127.0.0.1:9090", job="self"} => 1 @[1787113801.414]`
	sampleScalar      = "scalar: 3 @[1787170412.246]"
	sampleNaN         = "{} => NaN @[1787170412.45]"
	sampleAdversarial = `{x="a => b"} => 1 @[1787170412.527]`
	sampleTwoSeries   = "{} => 1 @[1787170381.485]\n" + `{job="b"} => 2 @[1787170381.485]`
)

// runnerInstant is what the core substitutes for {{database}}: the instant
// the backup's own data claims, which is why no literal `expect` could
// ever match promtool's trailing timestamp.
const runnerInstant = "2026-08-19T10:00:00Z"

// runRunner runs the declared runner exactly as the core would: the argv
// from the probe, with the placeholders substituted as literal elements.
// promtool is stubbed; awk and the shell are the host's, so the script
// runs for real.
func runRunner(t *testing.T, stub, query string) (stdout string, exit int) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "promtool"),
		[]byte("#!/bin/sh\n"+stub+"\n"), 0o700); err != nil {
		t.Fatalf("write promtool stub: %v", err)
	}
	for _, name := range []string{"awk", "sh"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s on this host: %v", name, err)
		}
		if err := os.Symlink(real, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
	}

	argv := runnerArgv(t)
	for i, a := range argv {
		a = strings.ReplaceAll(a, "{{database}}", runnerInstant)
		argv[i] = strings.ReplaceAll(a, "{{sql}}", query)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scriptRunBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, argv[0]), argv[1:]...)
	cmd.Env = []string{"PATH=" + binDir}
	out, err := cmd.Output()
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatalf("run runner: %v", err)
		}
		exit = exitErr.ExitCode()
	}
	return string(out), exit
}

// runnerArgv reads the argv the probe declares.
func runnerArgv(t *testing.T) []string {
	t.Helper()
	probe, ok := probePayload().(map[string]any)
	if !ok {
		t.Fatal("probe payload is not an object")
	}
	runner, ok := probe["sql_runner"].(map[string]any)
	if !ok {
		t.Fatal("probe declares no sql_runner")
	}
	argv, ok := runner["argv"].([]string)
	if !ok || len(argv) == 0 {
		t.Fatal("runner declares no argv")
	}
	return append([]string(nil), argv...)
}

// TestRunnerTemplateShape pins what the core will run.
func TestRunnerTemplateShape(t *testing.T) {
	want := []string{"sh", "-c", runnerScript, "sh", "{{database}}", "{{sql}}"}
	if got := runnerArgv(t); !reflect.DeepEqual(got, want) {
		t.Errorf("sql_runner argv = %v, want %v", got, want)
	}
	// The check text must reach promtool as one quoted parameter. Anything
	// that interpolated it into the script would hand the shell an
	// operator's expression to interpret.
	if !strings.Contains(runnerScript, `"$2"`) {
		t.Errorf("runner script = %q, want the check text used as a quoted parameter", runnerScript)
	}
	if strings.Contains(runnerScript, "{{sql}}") {
		t.Error("runner script interpolates the check text instead of taking it as a parameter")
	}
}

// TestRunnerPrintsUndecoratedRows is the fix for issue #175: §6.1 requires
// the runner to print result rows with no decoration, and promtool prints
// an annotated sample. Every shape below is measured output.
func TestRunnerPrintsUndecoratedRows(t *testing.T) {
	tests := []struct {
		name, emits, want string
	}{
		{"a vector sample with no labels", sampleNoLabels, "45886\n"},
		{"a vector sample carrying labels", sampleWithLabels, "1\n"},
		{"a scalar, which has no separator at all", sampleScalar, "3\n"},
		{"a value that is not a number", sampleNaN, "NaN\n"},
		{"a label value containing the separator", sampleAdversarial, "1\n"},
		{"one row per series", sampleTwoSeries, "1\n2\n"},
		// A query that matched nothing is not an error, and an empty
		// result must stay empty rather than becoming a blank row.
		{"an empty result", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := "printf '%s' " + shellQuote(tc.emits) + "; [ -z " + shellQuote(tc.emits) + " ] || echo"
			got, exit := runRunner(t, stub, "count(up)")
			if exit != 0 {
				t.Fatalf("exit = %d, want 0", exit)
			}
			if got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunnerFailsLoudly covers the two ways a check must not pass quietly.
func TestRunnerFailsLoudly(t *testing.T) {
	t.Run("promtool refuses the query", func(t *testing.T) {
		stub := "echo 'query error: bad_data: parse error' >&2; exit 1"
		got, exit := runRunner(t, stub, "count(")
		if exit == 0 {
			t.Errorf("exit = 0 with stdout %q, want the query error carried through", got)
		}
	})
	t.Run("output of a shape nothing anticipated", func(t *testing.T) {
		// Passing this through would be the original defect: decoration
		// reaching the core as if it were a value.
		stub := "echo 'something new from a future promtool'"
		got, exit := runRunner(t, stub, "count(up)")
		if exit == 0 {
			t.Errorf("exit = 0 with stdout %q, want an unreadable line to fail the check", got)
		}
		if strings.Contains(got, "something new") {
			t.Errorf("stdout = %q, want the decoration never handed to the core", got)
		}
	})
}

// TestRunnerDoesNotInterpretCheckText is the property the shell-free argv
// used to provide directly: an operator's check text is data, never
// something the sandbox executes.
func TestRunnerDoesNotInterpretCheckText(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	for _, hostile := range []string{
		"count(up); touch " + marker,
		"count(up) && touch " + marker,
		"count(up) `touch " + marker + "`",
		"count(up) $(touch " + marker + ")",
		"count(up) | touch " + marker,
	} {
		// The stub echoes a well-formed sample whatever it is handed: only
		// the shell's treatment of the parameter is under test.
		if _, exit := runRunner(t, "echo '"+sampleNoLabels+"'", hostile); exit != 0 {
			t.Fatalf("runner exited %d for %q", exit, hostile)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("check text %q was executed by the shell", hostile)
		}
	}
}

// shellQuote wraps a string for a POSIX shell single-quoted context.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
