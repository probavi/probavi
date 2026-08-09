package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func withLoginsPayload(dir string) string {
	return fmt.Sprintf(`{"source":{"kind":"bak_with_logins","path":%q,"params":{"logins":"logins.sql","bak":"orders.bak"},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":{"database":"orders"}}`, dir)
}

// withLoginsBehavior scripts the fake sandbox's answers for the two calls
// specific to this kind; everything else succeeds.
type withLoginsBehavior struct {
	loginsStderr string
	loginsExit   int
	orphanStdout string
	orphanStderr string
	orphanExit   int
}

func withLoginsHandler(t *testing.T, dir string, b withLoginsBehavior) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "put_file":
			return withLoginsPutFile(t, call, dir)
		case "exec":
			return withLoginsExec(t, call, b), nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

func withLoginsPutFile(t *testing.T, call verbCall, dir string) (any, *protoError) {
	t.Helper()
	args := putFileArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("put_file args: %v", err)
	}
	switch args.DestPath {
	case "/scratch/probavi-logins.sql":
		if args.SourcePath != filepath.Join(dir, "logins.sql") || args.Mode != "0600" {
			t.Errorf("logins put_file = %+v", args)
		}
		return putFileValue{BytesCopied: 12, DurationSeconds: 0.25}, nil
	case "/scratch/probavi-restore.bak":
		if args.SourcePath != filepath.Join(dir, "orders.bak") || args.Mode != "0600" {
			t.Errorf("bak put_file = %+v", args)
		}
		return putFileValue{BytesCopied: 9, DurationSeconds: 0.5}, nil
	}
	t.Errorf("unexpected put_file destination %s", args.DestPath)
	return nil, protoErr("internal", false, "unexpected put_file")
}

func withLoginsExec(t *testing.T, call verbCall, b withLoginsBehavior) any {
	t.Helper()
	args, kind := classify(t, call)
	switch kind {
	case "initfile":
		return okExec(0)
	case "probe":
		return servingExec()
	case "headeronly":
		return fullHeaderExec()
	case "logins":
		assertLoginsArgv(t, args)
		return execValue{ExitCode: b.loginsExit, DurationSeconds: 0.5,
			StderrB64: base64.StdEncoding.EncodeToString([]byte(b.loginsStderr))}
	case "restore":
		want := []string{"sh", "-c", restoreScript, "sh", "/scratch/probavi-restore.bak", "orders", "1"}
		if strings.Join(args.Argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("restore argv = %v", args.Argv)
		}
		return execValue{ExitCode: 0, DurationSeconds: 1.5}
	case "orphans":
		assertOrphanArgv(t, args)
		return execValue{ExitCode: b.orphanExit, DurationSeconds: 0.125,
			StdoutB64: base64.StdEncoding.EncodeToString([]byte(b.orphanStdout)),
			StderrB64: base64.StdEncoding.EncodeToString([]byte(b.orphanStderr))}
	default:
		return nil
	}
}

// assertLoginsArgv pins the replay's safety-critical flags: the script
// runs via -i with errors classified from a clean stderr (-r 0), and never
// with -b — a batch-abort flag would stop mid-script and silently skip
// every login after the first collision.
func assertLoginsArgv(t *testing.T, args execArgs) {
	t.Helper()
	if i := slices.Index(args.Argv, "-i"); i < 0 || i+1 >= len(args.Argv) || args.Argv[i+1] != "/scratch/probavi-logins.sql" {
		t.Errorf("logins argv = %v, want -i with the transferred script", args.Argv)
	}
	if i := slices.Index(args.Argv, "-r"); i < 0 || i+1 >= len(args.Argv) || args.Argv[i+1] != "0" {
		t.Errorf("logins argv = %v, want -r 0 so only diagnostics reach stderr", args.Argv)
	}
	if slices.Contains(args.Argv, "-b") {
		t.Errorf("logins argv = %v — -b would abort mid-script and silently skip logins", args.Argv)
	}
	if args.Env["SQLCMDPASSWORD"] != sandboxPassword {
		t.Errorf("logins env = %v, want the sandbox constant", args.Env)
	}
}

func assertOrphanArgv(t *testing.T, args execArgs) {
	t.Helper()
	joined := strings.Join(args.Argv, "\x00")
	if !strings.Contains(joined, "-d\x00orders") {
		t.Errorf("orphan-check argv = %v, want the restored database", args.Argv)
	}
	if !slices.Contains(args.Argv, "-b") {
		t.Errorf("orphan-check argv = %v, want -b so a broken check cannot pass silently", args.Argv)
	}
}

func TestProvisionWithLogins(t *testing.T) {
	dir := withLoginsDir(t)
	line, calls, exit := driveOp(t, "provision", withLoginsPayload(dir),
		withLoginsHandler(t, dir, withLoginsBehavior{}))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	// initfile, probe(ok), put_file(bak), headeronly, put_file(logins),
	// replay, restore, orphan check — the backup is transferred first
	// because choosing it is what asks the engine to identify it.
	if len(calls) != 8 {
		t.Errorf("calls = %d, want 8", len(calls))
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}

	// The identity covers both members with size framing, in load order.
	h := sha256.New()
	fmt.Fprintf(h, "logins\x00%d\x00%s", len("CREATE LOGIN"), "CREATE LOGIN")
	fmt.Fprintf(h, "bak\x00%d\x00%s", len("TAPEBAK-BYTES"), "TAPEBAK-BYTES")
	if want := "sha256:" + hex.EncodeToString(h.Sum(nil)); res.SourceIdentity.Checksum != want {
		t.Errorf("checksum = %s, want the two-member composite %s", res.SourceIdentity.Checksum, want)
	}
	if res.SourceIdentity.SizeBytes != int64(len("CREATE LOGIN")+len("TAPEBAK-BYTES")) {
		t.Errorf("size_bytes = %d, want the sum of both members", res.SourceIdentity.SizeBytes)
	}

	// Both transfers and both load phases are accounted; the orphan check
	// is a verdict, not recovery work, and must not inflate the RTO figure.
	if res.Timings.Transfer != 0.25+0.5 {
		t.Errorf("transfer_seconds = %v, want both transfers", res.Timings.Transfer)
	}
	if res.Timings.Restore != 0.5+1.5 {
		t.Errorf("restore_seconds = %v, want replay plus restore and nothing else", res.Timings.Restore)
	}

	if res.State["logins_path"] != "/scratch/probavi-logins.sql" || res.State["bak_path"] != "/scratch/probavi-restore.bak" {
		t.Errorf("state = %+v", res.State)
	}
}

func TestProvisionWithLoginsToleratesSandboxCollisions(t *testing.T) {
	dir := withLoginsDir(t)
	stderr := "Msg 15025, Level 16, State 1, Server x, Line 1\n" +
		"The server principal 'sa' already exists.\n" +
		"Msg 15025, Level 16, State 1, Server x, Line 1\n" +
		"The server principal '##MS_PolicyEventProcessingLogin##' already exists."
	line, _, exit := driveOp(t, "provision", withLoginsPayload(dir),
		withLoginsHandler(t, dir, withLoginsBehavior{loginsStderr: stderr}))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v — collisions on sandbox-created principals must be tolerated", exit, f)
	}
}

func TestProvisionWithLoginsFailures(t *testing.T) {
	tests := []struct {
		name     string
		behavior withLoginsBehavior
		wantHas  string
		maxCalls int
	}{
		{"application login collision",
			withLoginsBehavior{loginsStderr: "Msg 15025, Level 16, State 1, Server x, Line 1\nThe server principal 'app_login' already exists."},
			"app_login", 6},
		{"replay client failure",
			withLoginsBehavior{loginsExit: 1, loginsStderr: "Sqlcmd: '/scratch/probavi-logins.sql': Invalid filename."},
			"Invalid filename", 6},
		{"replay refused with silent stderr",
			withLoginsBehavior{loginsExit: 1},
			"sqlcmd exited 1", 6},
		{"orphaned users flagged",
			withLoginsBehavior{orphanStdout: "app_user\nreport_user\n"},
			"app_user; report_user", 8},
		{"orphan check breaks",
			withLoginsBehavior{orphanExit: 1, orphanStderr: "Msg 916, Level 14, State 2, Server x, Line 1\nThe server principal is not able to access the database under the current security context."},
			"orphaned-user check failed", 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := withLoginsDir(t)
			line, calls, exit := driveOp(t, "provision", withLoginsPayload(dir),
				withLoginsHandler(t, dir, tt.behavior))
			f := parseFinal(t, line)
			if exit != 0 || f.OK {
				t.Fatalf("exit=%d final=%+v, want a failure", exit, f)
			}
			if f.Error.Code != "restore_failed" {
				t.Errorf("code = %s (%s), want restore_failed", f.Error.Code, f.Error.Message)
			}
			if !strings.Contains(f.Error.Message, tt.wantHas) {
				t.Errorf("message = %q, want it to carry %q", f.Error.Message, tt.wantHas)
			}
			if len(calls) > tt.maxCalls {
				t.Errorf("calls = %d, want at most %d", len(calls), tt.maxCalls)
			}
			if strings.Contains(f.Error.Message, `"`) {
				t.Errorf("message %q must stay quote-free for protocol embedding", f.Error.Message)
			}
		})
	}
}

func TestProvisionWithLoginsSandboxFailure(t *testing.T) {
	dir := withLoginsDir(t)
	line, calls, exit := driveOp(t, "provision", withLoginsPayload(dir),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return nil, protoErr("sandbox_error", true, "container gone")
			}
			_, kind := classify(t, call)
			if kind == "initfile" || kind == "probe" {
				return servingExec(), nil
			}
			t.Errorf("unexpected exec after sandbox death: %v", kind)
			return nil, protoErr("internal", false, "unexpected")
		})
	f := parseFinal(t, line)
	if exit != 0 || f.OK || f.Error.Code != "sandbox_error" {
		t.Fatalf("exit=%d final=%+v, want the sandbox error to pass through untranslated", exit, f)
	}
	// initfile, probe, put_file(bak) — nothing after the dead sandbox
	if len(calls) != 3 {
		t.Errorf("calls = %d, want 3", len(calls))
	}
}
