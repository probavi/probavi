package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scriptRunBudget bounds a stub run: the scripts below are a handful of
// shell commands, so anything slower is a hang, not slow hardware.
const scriptRunBudget = 30 * time.Second

// stubTool writes an executable stub that stands in for a sandbox tool.
func stubTool(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write %s stub: %v", name, err)
	}
}

// stubPath builds a PATH holding nothing but the named stubs plus the
// coreutils the scripts themselves call, so a tool the script must not
// depend on is genuinely absent rather than merely mocked.
func stubPath(t *testing.T, stubs map[string]string, omit ...string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range stubs {
		stubTool(t, dir, name, body)
	}
	for _, name := range scriptTools {
		if _, stubbed := stubs[name]; stubbed || slices.Contains(omit, name) {
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

// scriptTools are the commands the load scripts call for themselves, as
// opposed to the clients they drive. They are linked in from the host so
// the scripts run for real; anything not listed is genuinely absent, which
// is how the missing-tool paths are exercised.
var scriptTools = []string{"cat", "tail", "grep", "rm", "mkfifo", "tee"}

// runLoadScript runs one of the load scripts with the stubbed tools and
// returns its exit status.
func runLoadScript(t *testing.T, script, binDir string, args ...string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scriptRunBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...)
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
	// content with it — the shape that makes a truncated archive dangerous.
	drainsThenSucceeds = `cat >/dev/null; exit 0`
	// abortsWithoutReading is a client rejecting the SQL at once, which
	// leaves the decompressor writing into a closed pipe.
	abortsWithoutReading = `echo "ERROR 1064 (42000) at line 1: You have an error in your SQL syntax" >&2; exit 1`
	// emitsThenFails is a decompressor that produces a prefix and then
	// reports the member truncated.
	emitsThenFails = `printf 'INSERT INTO orders VALUES (1);\n'; ` +
		`echo "gzip: /scratch/probavi-restore.sql.gz: unexpected end of file" >&2; exit 1`
	emitsWholeDump = `printf 'INSERT INTO orders VALUES (1);\n-- Dump completed on 2026-08-09 21:08:17\n'; exit 0`
	// emitsUnfinishedDump is the failure no exit code reports: a whole gzip
	// member holding a dump that stops on a statement boundary, which every
	// client loads without complaint. Only the missing sign-off says so.
	emitsUnfinishedDump = `printf 'INSERT INTO orders VALUES (1);\n'; exit 0`
)

// TestCompressedRestoreScriptJudgesBothEnds is the protocol's partial
// restore rule (§5) at the level it can actually be broken: a pipeline
// reports its last command's status, so a decompressor that dies after
// emitting a prefix is invisible to the client that happily loaded it.
func TestCompressedRestoreScriptJudgesBothEnds(t *testing.T) {
	tests := []struct {
		name     string
		stubs    map[string]string
		marker   string
		omit     []string
		wantExit int
	}{
		{"both ends succeed",
			map[string]string{"gzip": emitsWholeDump, "mariadb": drainsThenSucceeds},
			dumpCompleteMarker, nil, 0},
		{"a truncated archive the client was content with",
			map[string]string{"gzip": emitsThenFails, "mariadb": drainsThenSucceeds},
			dumpCompleteMarker, nil, decompressFailedExit},
		{"the client rejects the SQL",
			map[string]string{"gzip": emitsWholeDump, "mariadb": abortsWithoutReading},
			dumpCompleteMarker, nil, 1},
		{"the image has no gzip at all",
			map[string]string{"mariadb": drainsThenSucceeds},
			dumpCompleteMarker, nil, decompressFailedExit},
		// A whole member, a content client, and a dump that simply never
		// ended: the case the sign-off exists to catch.
		{"a whole member holding a dump that was never finished",
			map[string]string{"gzip": emitsUnfinishedDump, "mariadb": drainsThenSucceeds},
			dumpCompleteMarker, nil, incompleteDumpExit},
		// A comment-free dump has no sign-off to carry, so it is exempt
		// rather than failed — and the same bytes now pass.
		{"a dump that announces no ending is not held to one",
			map[string]string{"gzip": emitsUnfinishedDump, "mariadb": drainsThenSucceeds},
			"", nil, 0},
		{"an image that cannot make a fifo",
			map[string]string{"gzip": emitsWholeDump, "mariadb": drainsThenSucceeds},
			dumpCompleteMarker, []string{"mkfifo"}, witnessSetupExit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := stubPath(t, tt.stubs, tt.omit...)
			scratch := t.TempDir()
			got := runLoadScript(t, compressedRestoreScript, binDir,
				filepath.Join(scratch, "dump.sql.gz"), filepath.Join(scratch, "dump.status"),
				"root", "orders", tt.marker, strconv.Itoa(markerTailBytes))
			if got != tt.wantExit {
				t.Errorf("exit = %d, want %d", got, tt.wantExit)
			}
		})
	}
}

// compressedFixture writes a dump stored the way a dump pipeline stores
// one: mysqldump's banner, the data, and the sign-off that says the dump
// finished. The banner matters as much as the trailer — it is what puts the
// artifact under the completeness rule at all (see complete.go).
func compressedFixture(t *testing.T) string {
	t.Helper()
	return writeGzipDump(t, t.TempDir(), "orders.sql.gz", dumpFixtureBody)
}

// dumpFixtureBody is a mysqldump artifact reduced to the three things this
// adapter reads about it.
const dumpFixtureBody = "-- MySQL dump 10.13  Distrib 8.4.11, for Linux (x86_64)\n" +
	"INSERT INTO `orders` VALUES (1);\n" +
	"-- Dump completed on 2026-08-09 21:08:17\n"

const compressedInSandbox = "/scratch/probavi-restore.sql.gz"

// compressedDumpHandler answers a whole provision and records the argv the
// load ran under.
func compressedDumpHandler(t *testing.T, fixture string, loadArgv *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "put_file":
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.SourcePath != fixture || args.DestPath != compressedInSandbox {
				t.Errorf("put_file args = %+v — the stored artifact travels as it is", args)
			}
			return putFileValue{BytesCopied: 60, DurationSeconds: 0.2}, nil
		case "exec":
			args, stmt := lastArg(t, call)
			if stmt == "SELECT 1" || strings.HasPrefix(stmt, "CREATE DATABASE") {
				return okExec(0), nil
			}
			*loadArgv = args.Argv
			return execValue{ExitCode: 0, DurationSeconds: 1.5}, nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

// TestProvisionCompressedDump drives a whole provision against a
// compressed source and pins what reaches the sandbox: the stored bytes,
// under a name that says what they are, loaded through the decompressor.
func TestProvisionCompressedDump(t *testing.T) {
	fixture := compressedFixture(t)
	var loadArgv []string
	line, _, exit := driveOp(t, "provision",
		fmt.Sprintf(`{"source":{"kind":"mariadb_dump","path":%q,"params":{"backup_timezone":"UTC"}},`+
			`"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, fixture),
		compressedDumpHandler(t, fixture, &loadArgv))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	want := []string{"sh", "-c", compressedRestoreScript, "sh",
		compressedInSandbox, compressedInSandbox + ".status", defaultUser, defaultDatabase,
		dumpCompleteMarker, strconv.Itoa(markerTailBytes)}
	if strings.Join(loadArgv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("load argv = %q, want %q", loadArgv, want)
	}

	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	stored, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceIdentity.SizeBytes != int64(len(stored)) {
		t.Errorf("size_bytes = %d, want the stored %d — the record identifies the retained artifact",
			res.SourceIdentity.SizeBytes, len(stored))
	}
	if res.SourceIdentity.CreatedAt == nil || *res.SourceIdentity.CreatedAt != "2026-08-09T21:08:17.000Z" {
		t.Errorf("created_at = %v, want the trailer read through the decompressor",
			res.SourceIdentity.CreatedAt)
	}
	if res.State["dump_path"] != compressedInSandbox {
		t.Errorf("state = %+v", res.State)
	}
}

// TestCompressedRestoreFailuresAreNamed: the operator has to be sent to
// the right thing to fix — their backup, or the image they drill in.
func TestCompressedRestoreFailuresAreNamed(t *testing.T) {
	fixture := compressedFixture(t)
	loadFails := func(exitCode int, stderr string) func(verbCall) (any, *protoError) {
		return func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			switch _, stmt := lastArg(t, call); {
			case stmt == "SELECT 1", strings.HasPrefix(stmt, "CREATE DATABASE"):
				return okExec(0), nil
			}
			return errExec(exitCode, stderr), nil
		}
	}
	tests := []struct {
		name     string
		exitCode int
		stderr   string
		wantCode string
		wantIn   string
	}{
		{"a truncated archive", decompressFailedExit,
			"gzip: /scratch/probavi-restore.sql.gz: unexpected end of file",
			"source_corrupt", "could not be decompressed"},
		{"an image without gzip", decompressFailedExit,
			"sh: line 1: gzip: command not found",
			"restore_failed", "provides no gzip"},
		{"bad SQL behind a healthy archive", 1,
			"ERROR 1064 (42000) at line 1: You have an error in your SQL syntax\n" +
				"gzip: stdout: Broken pipe",
			"source_corrupt", "ERROR 1064"},
		{"the decompressor's broken pipe never explains a client failure", 1,
			"gzip: stdout: Broken pipe\n" +
				"ERROR 1114 (HY000) at line 12: The table 'orders' is full",
			"restore_failed", "ERROR 1114"},
		// A whole member, a content client, and a dump that simply never
		// ended. Nothing in the pipeline noticed; the sign-off did.
		{"a dump that was never finished", incompleteDumpExit, "",
			"source_corrupt", "not a complete dump"},
		{"an image without mkfifo", witnessSetupExit,
			"sh: line 2: mkfifo: command not found",
			"restore_failed", "provides no mkfifo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, _, exit := driveOp(t, "provision",
				fmt.Sprintf(`{"source":{"kind":"mariadb_dump","path":%q},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, fixture),
				loadFails(tt.exitCode, tt.stderr))
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
