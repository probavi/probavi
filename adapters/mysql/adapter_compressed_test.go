package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
func stubPath(t *testing.T, stubs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range stubs {
		stubTool(t, dir, name, body)
	}
	real, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("no cat on this host: %v", err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "cat")); err != nil {
		t.Fatalf("link cat: %v", err)
	}
	return dir
}

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
	emitsWholeDump = `printf 'INSERT INTO orders VALUES (1);\n'; exit 0`
)

// TestCompressedRestoreScriptJudgesBothEnds is the protocol's partial
// restore rule (§5) at the level it can actually be broken: a pipeline
// reports its last command's status, so a decompressor that dies after
// emitting a prefix is invisible to the client that happily loaded it.
func TestCompressedRestoreScriptJudgesBothEnds(t *testing.T) {
	tests := []struct {
		name     string
		stubs    map[string]string
		wantExit int
	}{
		{"both ends succeed",
			map[string]string{"gzip": emitsWholeDump, "mysql": drainsThenSucceeds}, 0},
		{"a truncated archive the client was content with",
			map[string]string{"gzip": emitsThenFails, "mysql": drainsThenSucceeds}, decompressFailedExit},
		{"the client rejects the SQL",
			map[string]string{"gzip": emitsWholeDump, "mysql": abortsWithoutReading}, 1},
		{"the image has no gzip at all",
			map[string]string{"mysql": drainsThenSucceeds}, decompressFailedExit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := stubPath(t, tt.stubs)
			scratch := t.TempDir()
			got := runLoadScript(t, compressedRestoreScript, binDir,
				filepath.Join(scratch, "dump.sql.gz"), filepath.Join(scratch, "dump.status"),
				"root", "orders")
			if got != tt.wantExit {
				t.Errorf("exit = %d, want %d", got, tt.wantExit)
			}
		})
	}
}

// TestCompressedUsersScriptJudgesBothEnds covers the same rule for the
// accounts replay, where --force keeps the client from ever aborting — so
// the decompressor's status is the only thing that can report a truncated
// script.
func TestCompressedUsersScriptJudgesBothEnds(t *testing.T) {
	tests := []struct {
		name     string
		stubs    map[string]string
		wantExit int
	}{
		{"both ends succeed",
			map[string]string{"gzip": emitsWholeDump, "mysql": drainsThenSucceeds}, 0},
		{"a truncated script the forced client swallowed",
			map[string]string{"gzip": emitsThenFails, "mysql": drainsThenSucceeds}, decompressFailedExit},
		{"the client refuses the replay",
			map[string]string{"gzip": emitsWholeDump, "mysql": `cat >/dev/null; exit 1`}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := stubPath(t, tt.stubs)
			scratch := t.TempDir()
			got := runLoadScript(t, compressedUsersLoadScript, binDir, "root",
				filepath.Join(scratch, "users.sql.gz"), filepath.Join(scratch, "users.status"))
			if got != tt.wantExit {
				t.Errorf("exit = %d, want %d", got, tt.wantExit)
			}
		})
	}
}

// compressedFixture writes a dump stored the way a dump pipeline stores
// one, carrying its own trailer.
func compressedFixture(t *testing.T) string {
	t.Helper()
	return writeGzipDump(t, t.TempDir(), "orders.sql.gz",
		"INSERT INTO `orders` VALUES (1);\n-- Dump completed on 2026-08-09 21:08:17\n")
}

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
		fmt.Sprintf(`{"source":{"kind":"mysqldump","path":%q,"params":{"backup_timezone":"UTC"}},`+
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
		compressedInSandbox, compressedInSandbox + ".status", defaultUser, defaultDatabase}
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, _, exit := driveOp(t, "provision",
				fmt.Sprintf(`{"source":{"kind":"mysqldump","path":%q},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, fixture),
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

// TestProvisionWithCompressedMembers drives the two-member kind with both
// members stored compressed: each is transferred as stored and replayed
// through its own decompressor, and the account layer still goes in before
// the dump.
func TestProvisionWithCompressedMembers(t *testing.T) {
	dir := t.TempDir()
	writeGzipDump(t, dir, "users.sql", "CREATE USER 'app'@'%';\n")
	writeGzipDump(t, dir, "orders.sql", "INSERT INTO `orders` VALUES (1);\n")

	const (
		usersInSandbox = "/scratch/probavi-users.sql.gz"
		dumpInSandbox  = "/scratch/probavi-restore.sql.gz"
	)
	var order []string
	line, _, exit := driveOp(t, "provision", withUsersPayload(dir),
		compressedMembersHandler(t, usersInSandbox, dumpInSandbox, &order))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	if strings.Join(order, ",") != "users,dump" {
		t.Errorf("load order = %v, want the account layer before the data", order)
	}
}

// compressedMembersHandler answers the two-member provision, checking each
// load's argv and recording the order the members went in.
func compressedMembersHandler(t *testing.T, usersInSandbox, dumpInSandbox string, order *[]string) func(verbCall) (any, *protoError) {
	loads := map[string][]string{
		"users": {"sh", "-c", compressedUsersLoadScript, "sh", defaultUser,
			usersInSandbox, usersInSandbox + ".status"},
		"dump": {"sh", "-c", compressedRestoreScript, "sh", dumpInSandbox,
			dumpInSandbox + ".status", defaultUser, "shop"},
	}
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.DestPath != usersInSandbox && args.DestPath != dumpInSandbox {
				t.Errorf("put_file destination %s — a compressed member keeps its own name", args.DestPath)
			}
			return putFileValue{BytesCopied: 40, DurationSeconds: 0.25}, nil
		}
		args, stmt := lastArg(t, call)
		if args.Argv[0] == "sh" {
			member := "dump"
			if strings.Contains(args.Argv[2], "-f") {
				member = "users"
			}
			*order = append(*order, member)
			if strings.Join(args.Argv, "\x00") != strings.Join(loads[member], "\x00") {
				t.Errorf("%s load argv = %q, want %q", member, args.Argv, loads[member])
			}
			return execValue{ExitCode: 0, DurationSeconds: 1.5}, nil
		}
		if stmt == "SELECT 1" || strings.HasPrefix(stmt, "SELECT COUNT(*) FROM (") {
			return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}, nil
		}
		// The remaining gate queries report nothing wrong.
		return okExec(0), nil
	}
}
