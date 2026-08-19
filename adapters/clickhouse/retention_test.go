package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// scriptRunBudget bounds a stub run: the restore script is a handful of
// shell commands, so anything slower is a hang, not slow hardware.
const scriptRunBudget = 30 * time.Second

// clientStub stands in for clickhouse-client. It records the statement it
// was handed — the order of those records is what these tests are about —
// and then behaves as the scenario's environment tells it to, so one stub
// covers every failure the script has to tell apart.
const clientStub = `
q=
while [ $# -gt 0 ]; do
  case "$1" in --query) q=$2; shift ;; esac
  shift
done
printf '%s\n' "$q" >> "$STUB_LOG"
case "$q" in
  *"STOP TTL MERGES"*) exit ${STUB_PIN_EXIT:-0} ;;
  *structure_only*)    [ "${STUB_STRUCTURE_EXIT:-0}" = 0 ] || exit "$STUB_STRUCTURE_EXIT"
                       printf '%s\n' "$STUB_STRUCTURE_OUT" ;;
  *)                   [ "${STUB_DATA_EXIT:-0}" = 0 ] || exit "$STUB_DATA_EXIT"
                       printf '%s\n' "$STUB_DATA_OUT" ;;
esac
`

// restored is what a successful RESTORE prints: an operation id and the
// engine's own status word.
const restored = "3b7daaa5-e7bf-4f51-afa0-cb1168306227\tRESTORED"

// runRestoreScript runs the real script against the stub and reports the
// exit status together with the statements the engine actually received.
func runRestoreScript(t *testing.T, env map[string]string) (exit int, seen []string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "clickhouse-client"),
		[]byte("#!/bin/sh\n"+clientStub), 0o700); err != nil {
		t.Fatalf("write client stub: %v", err)
	}
	// grep is the script's own tool rather than a client it drives, so it
	// is linked in from the host and genuinely runs.
	real, err := exec.LookPath("grep")
	if err != nil {
		t.Skipf("no grep on this host: %v", err)
	}
	if err := os.Symlink(real, filepath.Join(binDir, "grep")); err != nil {
		t.Fatalf("link grep: %v", err)
	}

	log := filepath.Join(t.TempDir(), "statements")
	ctx, cancel := context.WithTimeout(context.Background(), scriptRunBudget)
	defer cancel()
	structure, pin, data := restoreStatements()
	cmd := exec.CommandContext(ctx, "sh", "-c", restoreScript, "sh",
		defaultUser, defaultDatabase, structure, pin, data)
	cmd.Env = []string{"PATH=" + binDir, "STUB_LOG=" + log,
		"STUB_STRUCTURE_OUT=" + restored, "STUB_DATA_OUT=" + restored}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	runErr := cmd.Run()
	if runErr != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("run restore script: %v", runErr)
		}
		exit = exitErr.ExitCode()
	}
	if b, err := os.ReadFile(log); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				seen = append(seen, l)
			}
		}
	}
	return exit, seen
}

// TestRestoreScriptPinsRetentionBetweenTheTwoPasses is the ordering the
// whole fix rests on. The lock covers the tables that exist when it is
// issued (measured), so it has to land after the structure pass created
// them and before the data pass hands them anything to expire — any other
// order silently proves nothing.
func TestRestoreScriptPinsRetentionBetweenTheTwoPasses(t *testing.T) {
	exit, seen := runRestoreScript(t, nil)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	structure, pin, data := restoreStatements()
	if !slices.Equal(seen, []string{structure, pin, data}) {
		t.Fatalf("statements = %q, want the structure pass, the pin, then the data pass", seen)
	}
}

// TestRestoreScriptStopsWhereItMustStop covers every way a step can fail.
// The refusals matter more than the codes: a restore that carried on past
// a failed pin would hand back a record whose content depends on the
// clock, and one that carried on past a structure pass the engine never
// confirmed would pin nothing.
func TestRestoreScriptStopsWhereItMustStop(t *testing.T) {
	structure, pin, data := restoreStatements()
	tests := []struct {
		name     string
		env      map[string]string
		wantExit int
		wantSeen []string
	}{
		{
			name:     "the engine will not stop expiring data",
			env:      map[string]string{"STUB_PIN_EXIT": "1"},
			wantExit: pinRefusedExit,
			wantSeen: []string{structure, pin},
		},
		{
			name:     "the structure pass fails outright",
			env:      map[string]string{"STUB_STRUCTURE_EXIT": "60"},
			wantExit: 60,
			wantSeen: []string{structure},
		},
		{
			name:     "the structure pass says nothing about restoring",
			env:      map[string]string{"STUB_STRUCTURE_OUT": "Ok."},
			wantExit: notRestoredExit,
			wantSeen: []string{structure},
		},
		{
			name:     "the data pass fails after the pin held",
			env:      map[string]string{"STUB_DATA_EXIT": "60"},
			wantExit: 60,
			wantSeen: []string{structure, pin, data},
		},
		{
			name:     "the data pass says nothing about restoring",
			env:      map[string]string{"STUB_DATA_OUT": "Ok."},
			wantExit: notRestoredExit,
			wantSeen: []string{structure, pin, data},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exit, seen := runRestoreScript(t, tc.env)
			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d", exit, tc.wantExit)
			}
			if !slices.Equal(seen, tc.wantSeen) {
				t.Errorf("statements = %q, want %q", seen, tc.wantSeen)
			}
		})
	}
}

// TestProvisionSendsTheRestoreStatementsVerbatim pins what crosses the
// protocol, because the statements are the fix.
//
// The data pass deliberately carries no settings of its own: measured,
// repeating it with allow_non_empty_tables restores every row a second
// time, and a drill that doubled the artifact would report a row count no
// backup ever held.
func TestProvisionSendsTheRestoreStatementsVerbatim(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "shop.zip")
	writeArchive(t, archive, "2026-08-14 14:37:45")

	payload := provisionPayload(t, "clickhouse_backup", archive, nil)
	_, calls, _ := driveOp(t, "provision", payload, restoreSandbox(t, restored+"\n", 0, ""))

	structure, pin, data := restoreStatements()
	want := []string{"sh", "-c", restoreScript, "sh", defaultUser, defaultDatabase, structure, pin, data}
	for _, c := range calls {
		if c.Verb != "exec" {
			continue
		}
		args := execArgs{}
		if err := json.Unmarshal(c.Args, &args); err != nil {
			t.Fatalf("exec args: %v", err)
		}
		if len(args.Argv) > 2 && args.Argv[2] == restoreScript {
			if !slices.Equal(args.Argv, want) {
				t.Fatalf("restore argv = %q, want %q", args.Argv, want)
			}
			return
		}
	}
	t.Fatal("provision never ran the restore script")
}

// TestProvisionRefusesASandboxThatWillNotStopExpiringData is the loud half
// of the fix: the drill fails rather than producing a record whose content
// depends on how much of the backup had aged past its TTL by restore time.
func TestProvisionRefusesASandboxThatWillNotStopExpiringData(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "shop.zip")
	writeArchive(t, archive, "2026-08-14 14:37:45")

	payload := provisionPayload(t, "clickhouse_backup", archive, nil)
	line, _, _ := driveOp(t, "provision", payload, restoreSandbox(t, "", pinRefusedExit,
		"Code: 497. DB::Exception: default: Not enough privileges. (ACCESS_DENIED)"))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request when the engine will not stop its TTL merges", f)
	}
	for _, want := range []string{pinStatement, "ACCESS_DENIED"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
		}
	}
}
