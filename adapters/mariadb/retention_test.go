package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// pinnedExec is what a server answers the pin query with when it will not
// run events. Shared by the other fake sandboxes in this package, which
// simulate a sandbox that is already safe.
func pinnedExec() any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}
}

// notPinnedExec is a server whose scheduler is running — the default on
// the MySQL side of the family (measured).
func notPinnedExec() any {
	return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("0\n"))}
}

// schedulerSandbox answers the whole provision flow, recording the
// statements the pin issues so their order can be asserted: the pin has
// to land before the dump can create the events it suspends.
func schedulerSandbox(t *testing.T, seen *[]string, answers []any, setResult any) func(verbCall) (any, *protoError) {
	t.Helper()
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{BytesCopied: 8, DurationSeconds: 0.1}, nil
		}
		args, stmt := lastArg(t, call)
		switch {
		case stmt == pinnedQuery:
			*seen = append(*seen, "query")
			answer := answers[0]
			if len(answers) > 1 {
				answers = answers[1:]
			}
			return answer, nil
		case stmt == pinStatement:
			*seen = append(*seen, "set")
			return setResult, nil
		case stmt == "SELECT 1":
			*seen = append(*seen, "ready")
			return okExec(0), nil
		case strings.HasPrefix(stmt, "CREATE DATABASE"):
			return okExec(0), nil
		}
		if _, ok := parseReplay(args.Argv); ok {
			*seen = append(*seen, "restore")
			return execValue{ExitCode: 0, DurationSeconds: 0.5}, nil
		}
		t.Fatalf("unexpected exec: %v", args.Argv)
		return nil, nil
	}
}

// TestPinsTheSchedulerBeforeTheDumpCanCreateEvents is the ordering the
// fix rests on: the statement has to land before the restore, because
// after it the backup's own events exist and a scheduler that is still
// running will have started them.
func TestPinsTheSchedulerBeforeTheDumpCanCreateEvents(t *testing.T) {
	fixture := writeFixture(t, "-- Dump completed on 2026-08-19 10:00:00\n")
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(fixture, `{}`),
		schedulerSandbox(t, &seen, []any{notPinnedExec(), pinnedExec()}, okExec(0)))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	got := strings.Join(seen, "|")
	if !strings.Contains(got, "ready|query|set|query") {
		t.Errorf("sequence = %s, want the pin read, set and read back before anything else", got)
	}
	if !strings.Contains(got, "query|restore") {
		t.Errorf("sequence = %s, want the restore only after the scheduler was pinned", got)
	}
}

// TestAsksBeforeItActs covers the server that already will not run
// events. DISABLED is the sharp case: it cannot run events and cannot be
// told to stop either — `SET GLOBAL event_scheduler = OFF` fails against
// it with ERROR 1290 (measured), so a pin that acted unconditionally
// would refuse the safest state there is.
func TestAsksBeforeItActs(t *testing.T) {
	fixture := writeFixture(t, "-- Dump completed on 2026-08-19 10:00:00\n")
	var seen []string
	line, _, exit := driveOp(t, "provision", provisionPayload(fixture, `{}`),
		schedulerSandbox(t, &seen, []any{pinnedExec()}, nil))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	for _, label := range seen {
		if label == "set" {
			t.Fatalf("sequence = %v, want no statement against a server that already runs no events", seen)
		}
	}
}

// TestRefusesASandboxThatKeepsItsSchedulerRunning is the loud half. An
// event deletes rows on its own schedule, so a drill that let one run
// would produce a record whose contents depend on how long the restore
// took.
func TestRefusesASandboxThatKeepsItsSchedulerRunning(t *testing.T) {
	tests := []struct {
		name      string
		answers   []any
		setResult any
		wantIn    string
	}{
		{
			name:      "the engine refuses the statement",
			answers:   []any{notPinnedExec()},
			setResult: execValue{ExitCode: 1, StderrB64: base64.StdEncoding.EncodeToString([]byte("ERROR 1227 (42000): Access denied"))},
			wantIn:    "ERROR 1227",
		},
		{
			name:      "the engine accepts it and keeps running events",
			answers:   []any{notPinnedExec(), notPinnedExec()},
			setResult: okExec(0),
			wantIn:    "still reports the scheduler running",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := writeFixture(t, "-- Dump completed on 2026-08-19 10:00:00\n")
			var seen []string
			line, _, _ := driveOp(t, "provision", provisionPayload(fixture, `{}`),
				schedulerSandbox(t, &seen, tc.answers, tc.setResult))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != "invalid_request" {
				t.Fatalf("final = %+v, want invalid_request", f)
			}
			for _, want := range []string{pinStatement, tc.wantIn, "--events"} {
				if !strings.Contains(f.Error.Message, want) {
					t.Errorf("message = %q, want it to carry %q", f.Error.Message, want)
				}
			}
			for _, label := range seen {
				if label == "restore" {
					t.Fatal("the dump was loaded into a sandbox that still runs events")
				}
			}
		})
	}
}

// TestPhysicalRestoreStartsTheServerWithTheSchedulerOff covers the other
// path, where a statement would come too late: the restored data
// directory already holds the event definitions, so the server has to
// start with the scheduler off rather than be told afterwards. The
// verification query still runs, because a flag the drill passed is not
// the same thing as a server that agrees.
func TestPhysicalRestoreStartsTheServerWithTheSchedulerOff(t *testing.T) {
	backup := writeBackupFixture(t)
	var sequence []string
	_, calls, exit := driveOp(t, "provision", physicalPayload(backup), physicalHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	launched := false
	for _, call := range calls {
		if call.Verb != "exec" {
			continue
		}
		args, _ := lastArg(t, call)
		script := strings.Join(args.Argv, " ")
		if !strings.Contains(script, "mariadbd") || !strings.Contains(script, "--init-file") {
			continue
		}
		launched = true
		if !strings.Contains(script, eventSchedulerFlag) {
			t.Errorf("launch script = %q, want it to carry %s", script, eventSchedulerFlag)
		}
	}
	if !launched {
		t.Fatal("the physical flow never launched a server")
	}
}
