package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// timescale_test.go drives the timescaledb_dump kinds and the timescale
// fence at the protocol level. The measured facts they encode: a
// production-shaped TimescaleDB dump restores partially under the plain
// flow ("could not find hypertable"), the framed procedure restores it
// whole, and pg_dump's own CREATE EXTENSION IF NOT EXISTS skips with a
// NOTICE inside the frame, so --exit-on-error survives.

func timescalePayload(kind, path string) string {
	return fmt.Sprintf(`{"source":{"kind":%q,"path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, kind, path)
}

// framedExec answers the framed flow's five exec shapes, recording their
// order; the three framing statements answer with distinct durations so
// the timing assertion can prove they entered the restore figure.
func framedExec(t *testing.T, args execArgs, sequence *[]string) any {
	t.Helper()
	switch execRole(args.Argv) {
	case "pg_isready":
		*sequence = append(*sequence, "isready")
		return okExec(0)
	case "psql":
		sql := args.Argv[len(args.Argv)-1]
		switch {
		case strings.Contains(sql, "CREATE EXTENSION IF NOT EXISTS timescaledb"):
			*sequence = append(*sequence, "create-extension")
			return execValue{ExitCode: 0, DurationSeconds: 0.1}
		case strings.Contains(sql, "timescaledb_pre_restore"):
			*sequence = append(*sequence, "pre-restore")
			return execValue{ExitCode: 0, DurationSeconds: 0.2}
		case strings.Contains(sql, "alter_job"):
			*sequence = append(*sequence, "pin-jobs")
			return execValue{ExitCode: 0, DurationSeconds: 0.4}
		case strings.Contains(sql, "timescaledb_post_restore"):
			*sequence = append(*sequence, "post-restore")
			return execValue{ExitCode: 0, DurationSeconds: 0.3}
		}
		t.Fatalf("unexpected psql statement: %v", args.Argv)
	case "pg_restore":
		restore, _ := parseArchiveRestore(args.Argv)
		if restore.fence != "" {
			t.Errorf("fence = %q, want it disarmed on the framed kind", restore.fence)
		}
		*sequence = append(*sequence, "restore")
		return execValue{ExitCode: 0, DurationSeconds: 1.5}
	}
	t.Fatalf("unexpected exec: %v", args.Argv)
	return nil
}

func timescaleHandler(t *testing.T, sequence *[]string, exec func(*testing.T, execArgs, *[]string) any) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "put_file":
			*sequence = append(*sequence, "put_file")
			return putFileValue{DurationSeconds: 0.2}, nil
		case "exec":
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			return exec(t, args, sequence), nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

// TestProvisionTimescaleFramesTheRestore pins the framed flow: extension,
// pre_restore, the restore with the fence disarmed, the policy-job pin,
// post_restore — in that order — and every framing second inside the
// measured restore, because the real recovery path cannot skip them. The
// pin's place in that line is the whole of it: post_restore releases the
// background workers, and the retention policy runs in the same second.
func TestProvisionTimescaleFramesTheRestore(t *testing.T) {
	fixture := writeFixture(t)
	var sequence []string
	line, _, exit := driveOp(t, "provision",
		timescalePayload("timescaledb_dump", fixture),
		timescaleHandler(t, &sequence, framedExec))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	want := "isready|put_file|create-extension|pre-restore|restore|pin-jobs|post-restore"
	if got := strings.Join(sequence, "|"); got != want {
		t.Errorf("sequence = %s, want %s", got, want)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if diff := res.Timings.Restore - 2.5; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("restore_seconds = %v, want the framing statements' 1.0 inside the restore's 1.5", res.Timings.Restore)
	}
}

// TestProvisionTimescaleFailures pins the two framing refusals: an image
// without the extension is the operator's image problem, named with the
// fix; a failed post_restore is a failed restore, because the database
// is left in the restoring state.
func TestProvisionTimescaleFailures(t *testing.T) {
	tests := []struct {
		name     string
		fail     string // which framing statement fails
		stderr   string
		wantCode string
		wantMsg  string
	}{
		{"an image without the extension names the fix", "CREATE EXTENSION",
			`ERROR:  extension "timescaledb" is not available`,
			"invalid_request", "timescale/timescaledb image"},
		{"a failing pre_restore is a failed restore", "timescaledb_pre_restore",
			"ERROR:  something broke", "restore_failed", "timescaledb_pre_restore"},
		{"a failing post_restore is a failed restore", "timescaledb_post_restore",
			"ERROR:  cannot complete", "restore_failed", "restoring state"},
		{"policy jobs that cannot be held back fail the restore", "alter_job",
			"ERROR:  function alter_job(integer, next_start => timestamptz) does not exist",
			"restore_failed", "policy jobs could not be held back"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := writeFixture(t)
			var sequence []string
			exec := func(t *testing.T, args execArgs, seq *[]string) any {
				if execRole(args.Argv) == "psql" &&
					strings.Contains(args.Argv[len(args.Argv)-1], tt.fail) {
					return errExec(1, tt.stderr)
				}
				return framedExec(t, args, seq)
			}
			line, _, _ := driveOp(t, "provision",
				timescalePayload("timescaledb_dump", fixture),
				timescaleHandler(t, &sequence, exec))
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantMsg) {
				t.Errorf("final = %+v, want %s containing %q", f, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// TestUnpinnablePolicyJobsStopTheFrame holds the deliberate choice behind
// the pin: the frame closes only when the restored automation is out of
// reach. Releasing the background workers regardless would hand back a
// record of a database that deleted part of itself between the restore
// and the first check, with the restore reported successful.
func TestUnpinnablePolicyJobsStopTheFrame(t *testing.T) {
	fixture := writeFixture(t)
	var sequence []string
	exec := func(t *testing.T, args execArgs, seq *[]string) any {
		if execRole(args.Argv) == "psql" &&
			strings.Contains(args.Argv[len(args.Argv)-1], "alter_job") {
			*seq = append(*seq, "pin-jobs")
			return errExec(1, "ERROR:  permission denied for function alter_job")
		}
		return framedExec(t, args, seq)
	}
	line, _, _ := driveOp(t, "provision",
		timescalePayload("timescaledb_dump", fixture),
		timescaleHandler(t, &sequence, exec))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "restore_failed" {
		t.Fatalf("final = %+v, want restore_failed", f)
	}
	for _, step := range sequence {
		if step == "post-restore" {
			t.Errorf("the frame closed anyway: %s", strings.Join(sequence, "|"))
		}
	}
}

// TestPlainKindFencesTimescaleDump pins the fence's verdict: when the
// restore script reports the timescale exit, the drill refuses with the
// kind that would have restored it correctly — before the restore ran.
func TestPlainKindFencesTimescaleDump(t *testing.T) {
	fixture := writeFixture(t)
	var sequence []string
	exec := func(t *testing.T, args execArgs, seq *[]string) any {
		switch execRole(args.Argv) {
		case "pg_isready":
			return okExec(0)
		case "pg_restore":
			restore, _ := parseArchiveRestore(args.Argv)
			if restore.fence != timescaleTOCMark {
				t.Errorf("fence = %q, want it armed on the plain kind", restore.fence)
			}
			return errExec(timescaleFencedExit, "")
		}
		t.Fatalf("unexpected exec: %v", args.Argv)
		return nil
	}
	line, _, _ := driveOp(t, "provision",
		timescalePayload("pgdump", fixture),
		timescaleHandler(t, &sequence, exec))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "unsupported_source" ||
		!strings.Contains(f.Error.Message, "timescaledb_dump") ||
		!strings.Contains(f.Error.Message, "timescaledb_pre_restore") {
		t.Errorf("final = %+v, want unsupported_source teaching the framed kind", f)
	}
}

// TestFenceArming pins which flows arm the fence: the plain logical
// kinds do, the framed kinds and the globals member never.
func TestFenceArming(t *testing.T) {
	if got := archiveFence(false); got != timescaleTOCMark {
		t.Errorf("archiveFence(unframed) = %q", got)
	}
	if got := archiveFence(true); got != "" {
		t.Errorf("archiveFence(framed) = %q, want disarmed", got)
	}
	if got := scriptFence(false); got != timescaleScriptPattern {
		t.Errorf("scriptFence(unframed) = %q", got)
	}
	if got := scriptFence(true); got != "" {
		t.Errorf("scriptFence(framed) = %q, want disarmed", got)
	}

	member := sandboxFile{path: "/scratch/probavi-restore.sql"}
	replay, ok := parseReplay(psqlReplayArgv(member, "u", "d", errorStopOn, scriptFence(false)))
	if !ok || replay.fence != timescaleScriptPattern {
		t.Errorf("replay fence = %+v, want the armed pattern", replay)
	}
	replay, ok = parseReplay(psqlReplayArgv(member, "u", "d", errorStopOn, scriptFence(true)))
	if !ok || replay.fence != "" {
		t.Errorf("framed replay fence = %+v, want disarmed", replay)
	}
}

// TestTimescaleKindResolution pins the kind wiring: both framed kinds
// mark the source, and the directory kind picks the newest exactly like
// pgdump_dir.
func TestTimescaleKindResolution(t *testing.T) {
	fixture := writeFixture(t)
	src, perr := resolveSource(t.Context(), "timescaledb_dump", fixture, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !src.timescale {
		t.Error("timescaledb_dump did not mark the source as framed")
	}
	plain, perr := resolveSource(t.Context(), "pgdump", fixture, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if plain.timescale {
		t.Error("pgdump must not mark the source as framed")
	}
}
