package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scriptRunBudget bounds a stub run: the scripts below are a handful of
// shell commands, so anything slower is a hang, not slow hardware. The
// compressed one starts a background reader, and a hang there would be the
// interesting kind of bug.
const scriptRunBudget = 30 * time.Second

// scriptTools are the commands the replay scripts call for themselves, as
// opposed to the clients they drive. They are linked in from the host so
// the scripts run for real; anything not listed is genuinely absent, which
// is how the missing-tool paths are exercised.
var scriptTools = []string{"cat", "tail", "grep", "rm", "mkfifo", "tee"}

// stubPath builds a PATH holding the named stubs plus the tools above, so a
// client the script must not depend on is absent rather than merely mocked.
func stubPath(t *testing.T, stubs map[string]string, omit ...string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	for _, name := range scriptTools {
		if contains(omit, name) {
			continue
		}
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s on this host: %v", name, err)
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
	}
	return dir
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// runReplay runs a replay script over member with the stubbed tools and
// returns its exit status.
func runReplay(t *testing.T, script, binDir, member string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scriptRunBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "sh", member,
		"postgres", "orders", dumpCompleteMarker, strconv.Itoa(markerTailBytes), errorStopOn)
	cmd.Env = append(os.Environ(), "PATH="+binDir)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	exitErr := &exec.ExitError{}
	if !errors.As(err, &exitErr) {
		t.Fatalf("run script: %v", err)
	}
	return exitErr.ExitCode()
}

const (
	// drainsThenSucceeds is a client that reads its whole input and is
	// content with it — the shape that makes a truncated dump dangerous,
	// and the shape psql really has (measured: fed a dump cut in half it
	// restores the half and exits 0).
	drainsThenSucceeds = `cat >/dev/null; exit 0`
	// abortsOnBadSQL is psql under ON_ERROR_STOP=1 meeting a statement the
	// server rejects, which leaves the decompressor writing into a closed
	// pipe.
	abortsOnBadSQL = `echo "psql:dump.sql:12: ERROR:  relation \"public.orders\" does not exist" >&2; exit 3`
	// psqlExitCode is what psql leaves behind on that failure.
	psqlExitCode = 3
)

// emits builds a decompressor stub that writes the file at path and then
// exits with code, which is how a truncated member (non-zero) is told from
// a whole one. The output comes from a file rather than an inlined string
// so the newlines the marker is anchored to survive into the stream.
func emits(path string, code int) string {
	return fmt.Sprintf("cat %q\nexit %d", path, code)
}

// TestReplayScriptProvesTheDumpWasWhole covers the uncompressed plain-SQL
// path, where psql's own exit code cannot tell a complete dump from one
// that stops halfway.
func TestReplayScriptProvesTheDumpWasWhole(t *testing.T) {
	half := plainDumpBody[:len(plainDumpBody)/2]
	tests := []struct {
		name     string
		body     string
		psql     string
		wantExit int
	}{
		{"a whole dump psql was content with", plainDumpBody, drainsThenSucceeds, 0},
		{"a dump that stops halfway", half, drainsThenSucceeds, incompleteDumpExit},
		{"psql's own verdict is passed through", plainDumpBody, abortsOnBadSQL, psqlExitCode},
		// A failing psql is never asked about completeness: its diagnosis is
		// the better one, and the dump may be whole.
		{"a truncated dump psql also rejected", half, abortsOnBadSQL, psqlExitCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := stubPath(t, map[string]string{"psql": tt.psql})
			member := writePlain(t, t.TempDir(), "probavi-restore.sql", tt.body)
			if got := runReplay(t, scriptReplayScript, binDir, member); got != tt.wantExit {
				t.Errorf("exit = %d, want %d", got, tt.wantExit)
			}
		})
	}
}

// TestCompressedReplayScriptJudgesEveryEnd is the protocol's partial-restore
// rule (§5) at the level it can actually be broken. Three things can go
// wrong behind a pipeline that reports only its last command's status, and
// each sends an operator somewhere different.
func TestCompressedReplayScriptJudgesEveryEnd(t *testing.T) {
	dir := t.TempDir()
	whole := writePlain(t, dir, "whole.sql", plainDumpBody)
	half := writePlain(t, dir, "half.sql", plainDumpBody[:len(plainDumpBody)/2])
	tests := []struct {
		name     string
		stubs    map[string]string
		omit     []string
		wantExit int
	}{
		{"a whole dump through a healthy decompressor",
			map[string]string{"gzip": emits(whole, 0), "psql": drainsThenSucceeds}, nil, 0},
		// The case the witness exists for: a backup job whose pg_dump died
		// leaves a valid gzip file holding an incomplete dump. Everything in
		// it restores, and nothing but the missing closing line says so.
		{"a valid member holding a dump that was never finished",
			map[string]string{"gzip": emits(half, 0), "psql": drainsThenSucceeds},
			nil, incompleteDumpExit},
		{"a member truncated after it was compressed",
			map[string]string{"gzip": emits(half, 1), "psql": drainsThenSucceeds},
			nil, decompressFailedExit},
		// Every byte arrived and only the trailing checksum disagreed. The
		// data may well be whole; the drill still refuses, because "may
		// well be" is not what a signed record should rest on.
		{"a member whose checksum failed after the last byte",
			map[string]string{"gzip": emits(whole, 1), "psql": drainsThenSucceeds},
			nil, decompressFailedExit},
		{"psql's own verdict comes first",
			map[string]string{"gzip": emits(whole, 0), "psql": abortsOnBadSQL}, nil, psqlExitCode},
		{"an image with no gzip in it",
			map[string]string{"psql": drainsThenSucceeds}, nil, decompressFailedExit},
		{"an image that cannot make a fifo",
			map[string]string{"gzip": emits(whole, 0), "psql": drainsThenSucceeds},
			[]string{"mkfifo"}, witnessSetupExit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := stubPath(t, tt.stubs, tt.omit...)
			member := filepath.Join(t.TempDir(), "probavi-restore.sql.gz")
			if got := runReplay(t, compressedScriptReplayScript, binDir, member); got != tt.wantExit {
				t.Errorf("exit = %d, want %d", got, tt.wantExit)
			}
		})
	}
}

// storedFixture writes a dump the way an operator stores one and returns
// the path together with the name it must land under in the sandbox.
func storedFixture(t *testing.T, name, body string, compress bool) (path, inSandbox string) {
	t.Helper()
	dir := t.TempDir()
	if compress {
		return writeGzip(t, dir, name, body), "/scratch/" + sandboxBase(body) + ".gz"
	}
	return writePlain(t, dir, name, body), "/scratch/" + sandboxBase(body)
}

func sandboxBase(body string) string {
	if strings.HasPrefix(body, pgdumpMagic) {
		return "probavi-restore.dump"
	}
	return "probavi-restore.sql"
}

// restoreHandler answers a whole provision and records the argv the restore
// ran under, plus where the artifact was staged.
func restoreHandler(t *testing.T, fixture string, staged *string, argv *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "put_file":
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.SourcePath != fixture {
				t.Errorf("put_file source = %s, want the stored artifact %s", args.SourcePath, fixture)
			}
			*staged = args.DestPath
			return putFileValue{DurationSeconds: 0.2}, nil
		case "exec":
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			if args.Argv[0] == "pg_isready" {
				return okExec(0), nil
			}
			*argv = args.Argv
			return execValue{ExitCode: 0, DurationSeconds: 1.5}, nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

// TestProvisionRestoresWhatItWasGiven drives a whole provision for each
// shape a pg_dump artifact comes in, and pins the two things that follow
// from the sniff: the name the stored bytes land under, and the client that
// reads them.
func TestProvisionRestoresWhatItWasGiven(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		compress   bool
		wantClient string
		wantScript string
	}{
		{"a custom-format archive", archiveBytes(), false, "pg_restore", ""},
		{"a custom-format archive stored compressed", archiveBytes(), true, "sh", compressedArchiveRestoreScript},
		{"a plain-SQL dump", plainDumpBody, false, "sh", scriptReplayScript},
		{"a plain-SQL dump stored compressed", plainDumpBody, true, "sh", compressedScriptReplayScript},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture, wantStaged := storedFixture(t, "backup", tt.body, tt.compress)
			var staged string
			var argv []string
			line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"),
				restoreHandler(t, fixture, &staged, &argv))
			f := parseFinal(t, line)
			if exit != 0 || !f.OK {
				t.Fatalf("exit=%d final=%+v", exit, f)
			}
			if staged != wantStaged {
				t.Errorf("staged as %s, want %s — the name follows what the bytes are", staged, wantStaged)
			}
			assertRestoreClient(t, argv, tt.wantClient, tt.wantScript)
			assertIdentifiesStoredBytes(t, f.Payload, fixture)
		})
	}
}

func assertRestoreClient(t *testing.T, argv []string, wantClient, wantScript string) {
	t.Helper()
	if argv[0] != wantClient {
		t.Errorf("restore ran %s, want %s", argv[0], wantClient)
	}
	if wantScript != "" && argv[2] != wantScript {
		t.Errorf("restore script = %q, want %q", argv[2], wantScript)
	}
}

// assertIdentifiesStoredBytes is the property compression must not weaken:
// whatever shape the artifact comes in, the evidence identifies the file
// the operator retained rather than some intermediate form of it.
func assertIdentifiesStoredBytes(t *testing.T, payload json.RawMessage, fixture string) {
	t.Helper()
	stored, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	res := provisionWire{}
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.SourceIdentity.SizeBytes != int64(len(stored)) {
		t.Errorf("size_bytes = %d, want the stored %d", res.SourceIdentity.SizeBytes, len(stored))
	}
}

// TestPlainDumpIsDatedFromItsOwnHead proves the head is read through the
// decompressor: the same dump stored compressed reports the same instant.
func TestPlainDumpIsDatedFromItsOwnHead(t *testing.T) {
	for _, compress := range []bool{false, true} {
		name := "stored plain"
		if compress {
			name = "stored compressed"
		}
		t.Run(name, func(t *testing.T) {
			fixture, _ := storedFixture(t, "backup", plainDumpBody, compress)
			var staged string
			var argv []string
			line, _, _ := driveOp(t, "provision",
				fmt.Sprintf(`{"source":{"kind":"pgdump","path":%q,"params":{"backup_timezone":"Asia/Tokyo"}},`+
					`"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, fixture),
				restoreHandler(t, fixture, &staged, &argv))
			f := parseFinal(t, line)
			if !f.OK {
				t.Fatalf("final = %+v", f)
			}
			res := provisionWire{}
			if err := json.Unmarshal(f.Payload, &res); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if res.SourceIdentity.CreatedAt == nil || *res.SourceIdentity.CreatedAt != "2026-08-09T21:26:45.000+09:00" {
				t.Errorf("created_at = %v, want the dump's own start time in the declared zone",
					res.SourceIdentity.CreatedAt)
			}
		})
	}
}

// TestRestoreFailuresAreNamed: the operator has to be sent to the right
// thing to fix — their backup, the job that wrote it, or the image they
// drill in.
func TestRestoreFailuresAreNamed(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		compress bool
		exitCode int
		stderr   string
		wantCode string
		wantIn   string
	}{
		{"a dump that was never finished", plainDumpBody, true, incompleteDumpExit, "",
			"source_corrupt", "not a complete dump"},
		{"a member that does not decompress", plainDumpBody, true, decompressFailedExit,
			"gzip: /scratch/probavi-restore.sql.gz: unexpected end of file",
			"source_corrupt", "could not be decompressed"},
		{"an image without gzip", plainDumpBody, true, decompressFailedExit,
			"sh: line 1: gzip: command not found",
			"restore_failed", "provides no gzip"},
		{"an image without mkfifo", plainDumpBody, true, witnessSetupExit,
			"sh: line 2: mkfifo: command not found",
			"restore_failed", "provides no mkfifo"},
		{"a dump naming a role the cluster has not got", plainDumpBody, false, psqlExitCode,
			`psql:orders.sql:34: ERROR:  role "app_ro" does not exist`,
			"restore_failed", "pgdump_with_globals"},
		{"an engine failure behind a healthy dump", plainDumpBody, false, psqlExitCode,
			`psql:orders.sql:88: ERROR:  out of memory`,
			"restore_failed", "out of memory"},
		// A file that is not a dump at all reaches psql as a script, and the
		// server's verdict on it is about the artifact, not the restore.
		{"a file that is not a dump", plainDumpBody, false, psqlExitCode,
			`psql:orders.sql:1: ERROR:  syntax error at or near "this"`,
			"source_corrupt", "rejected the dump"},
		// The decompressor shares the pipeline's stderr; once psql aborts it
		// adds a broken-pipe note, which must not become the explanation.
		{"the decompressor's broken pipe never explains a psql failure", plainDumpBody, true, psqlExitCode,
			"gzip: stdout: Broken pipe\n" +
				`psql:orders.sql:12: ERROR:  duplicate key value violates unique constraint`,
			"restore_failed", "duplicate key"},
		{"a compressed archive that is not one", archiveBytes(), true, 1,
			"pg_restore: error: input file does not appear to be a valid archive",
			"source_corrupt", "rejected the archive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture, _ := storedFixture(t, "backup", tt.body, tt.compress)
			line, _, exit := driveOp(t, "provision", provisionPayload(fixture, "{}"),
				restoreFailsWith(t, tt.exitCode, tt.stderr))
			f := parseFinal(t, line)
			if exit != 0 || f.OK {
				t.Fatalf("exit=%d final=%+v", exit, f)
			}
			if f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantIn) {
				t.Errorf("error = %s/%q, want %s mentioning %q",
					f.Error.Code, f.Error.Message, tt.wantCode, tt.wantIn)
			}
		})
	}
}

// restoreFailsWith answers a provision where the engine is up and the
// restore itself is the thing that fails.
func restoreFailsWith(t *testing.T, exitCode int, stderr string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("exec args: %v", err)
		}
		if args.Argv[0] == "pg_isready" {
			return okExec(0), nil
		}
		return errExec(exitCode, stderr), nil
	}
}

// TestCompressedGlobalsAreReplayedAsStored covers the other member of the
// two-member kind: a backup job may compress the globals script and the
// dump independently, and each is replayed the way it is stored.
func TestCompressedGlobalsAreReplayedAsStored(t *testing.T) {
	dir := t.TempDir()
	writeGzip(t, dir, "globals.sql", "CREATE ROLE app_ro;\n"+
		"--\n-- PostgreSQL database cluster dump complete\n--\n")
	writePlain(t, dir, "orders.dump", archiveBytes())

	staged := map[string]bool{}
	var globalsArgv []string
	line, _, exit := driveOp(t, "provision", withGlobalsPayload(dir),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				args := putFileArgs{}
				if err := json.Unmarshal(call.Args, &args); err != nil {
					t.Fatalf("put_file args: %v", err)
				}
				staged[args.DestPath] = true
				return putFileValue{DurationSeconds: 0.25}, nil
			}
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			if replay, ok := parseReplay(args.Argv); ok && strings.Contains(replay.path, "globals") {
				globalsArgv = args.Argv
			}
			return execValue{ExitCode: 0, DurationSeconds: 0.5}, nil
		})
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if !staged["/scratch/probavi-globals.sql.gz"] || !staged["/scratch/probavi-restore.dump"] {
		t.Errorf("staged %v — each member keeps the name its own bytes call for", staged)
	}
	replay, ok := parseReplay(globalsArgv)
	if !ok {
		t.Fatalf("globals argv = %v, want the replay script", globalsArgv)
	}
	if replay.script != compressedScriptReplayScript {
		t.Error("a compressed globals script must be replayed through the decompressor")
	}
	if replay.errorStop != errorStopOff {
		t.Errorf("globals ON_ERROR_STOP = %q, want it off whatever the storage", replay.errorStop)
	}
}

// TestGlobalsCompletenessIsProvedToo closes the same hole on the member
// where psql is even less able to report it: the globals load runs with
// ON_ERROR_STOP off by design, so a script that stops halfway creates the
// roles it got to, says nothing, and would have passed the drill.
func TestGlobalsCompletenessIsProvedToo(t *testing.T) {
	dir := t.TempDir()
	writePlain(t, dir, "globals.sql", "CREATE ROLE app_ro;\n")
	writePlain(t, dir, "orders.dump", archiveBytes())

	line, calls, _ := driveOp(t, "provision", withGlobalsPayload(dir),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			if args.Argv[0] == "pg_isready" {
				return okExec(0), nil
			}
			return errExec(incompleteDumpExit, ""), nil
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_corrupt" {
		t.Fatalf("final = %+v, want source_corrupt", f)
	}
	if !strings.Contains(f.Error.Message, "cluster globals script") {
		t.Errorf("message = %q, want it to name the member that was truncated", f.Error.Message)
	}
	// isready, put globals, replay — and nothing after: the dump must not
	// be restored on top of a half-loaded cluster.
	if len(calls) != 3 {
		t.Errorf("calls = %d, want 3", len(calls))
	}
}
