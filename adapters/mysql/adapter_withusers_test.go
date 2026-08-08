package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func withUsersPayload(dir string) string {
	return fmt.Sprintf(`{"source":{"kind":"mysqldump_with_users","path":%q,"params":{"users":"users.sql","dump":"orders.sql"},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":{"database":"shop"}}`, dir)
}

// withUsersBehavior scripts the fake sandbox's answers for the calls
// specific to this kind; everything else succeeds.
type withUsersBehavior struct {
	usersStderr  string
	usersExit    int
	definers     string // G1 stdout: orphaned definers, one per line
	views        string // G2 stdout: view names, one per line
	explainExit  int
	explainErr   string
	reachCount   string // G3 stdout; empty defaults to "1"
	checkExit    int    // exit for the gate queries themselves
	checkStderr  string
	explainCalls *int
}

func withUsersHandler(t *testing.T, dir string, b withUsersBehavior) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		switch call.Verb {
		case "put_file":
			return withUsersPutFile(t, call, dir)
		case "exec":
			return withUsersExec(t, call, b), nil
		}
		return nil, protoErr("internal", false, "unexpected verb")
	}
}

func withUsersPutFile(t *testing.T, call verbCall, dir string) (any, *protoError) {
	t.Helper()
	args := putFileArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("put_file args: %v", err)
	}
	switch args.DestPath {
	case "/scratch/probavi-users.sql":
		if args.SourcePath != filepath.Join(dir, "users.sql") || args.Mode != "0600" {
			t.Errorf("users put_file = %+v", args)
		}
		return putFileValue{BytesCopied: 11, DurationSeconds: 0.25}, nil
	case "/scratch/probavi-restore.sql":
		if args.SourcePath != filepath.Join(dir, "orders.sql") || args.Mode != "0600" {
			t.Errorf("dump put_file = %+v", args)
		}
		return putFileValue{BytesCopied: 10, DurationSeconds: 0.5}, nil
	}
	t.Errorf("unexpected put_file destination %s", args.DestPath)
	return nil, protoErr("internal", false, "unexpected put_file")
}

func withUsersExec(t *testing.T, call verbCall, b withUsersBehavior) any {
	t.Helper()
	args := execArgs{}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		t.Fatalf("exec args: %v", err)
	}
	if args.Argv[0] == "sh" {
		assertUsersArgv(t, args)
		return execValue{ExitCode: b.usersExit, DurationSeconds: 0.5,
			StderrB64: base64.StdEncoding.EncodeToString([]byte(b.usersStderr))}
	}
	stmt := args.Argv[len(args.Argv)-1]
	switch {
	case stmt == "SELECT 1":
		return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}
	case strings.HasPrefix(stmt, "CREATE DATABASE IF NOT EXISTS `shop`"):
		return okExec(0)
	case stmt == "source /scratch/probavi-restore.sql":
		return execValue{ExitCode: 0, DurationSeconds: 1.5}
	case strings.HasPrefix(stmt, "SELECT DISTINCT o.definer"):
		return gateExec(b, b.definers)
	case strings.HasPrefix(stmt, "SELECT table_name FROM information_schema.views"):
		return gateExec(b, b.views)
	case strings.HasPrefix(stmt, "EXPLAIN SELECT"):
		if b.explainCalls != nil {
			*b.explainCalls++
		}
		return execValue{ExitCode: b.explainExit, DurationSeconds: 0.125,
			StderrB64: base64.StdEncoding.EncodeToString([]byte(b.explainErr))}
	case strings.HasPrefix(stmt, "SELECT COUNT(*) FROM ("):
		count := b.reachCount
		if count == "" {
			count = "1"
		}
		return gateExec(b, count+"\n")
	default:
		t.Fatalf("unexpected exec: %v", args.Argv)
		return nil
	}
}

func gateExec(b withUsersBehavior, stdout string) any {
	return execValue{ExitCode: b.checkExit, DurationSeconds: 0.125,
		StdoutB64: base64.StdEncoding.EncodeToString([]byte(stdout)),
		StderrB64: base64.StdEncoding.EncodeToString([]byte(b.checkStderr))}
}

// assertUsersArgv pins the replay's safety-critical shape: stdin with
// --force, through positional parameters. The `source` client command
// aborts mid-script even under --force (measured), so any drift from this
// shape reintroduces the ordering-dependent partial load.
func assertUsersArgv(t *testing.T, args execArgs) {
	t.Helper()
	want := []string{"sh", "-c", usersLoadScript, "sh", "root", "/scratch/probavi-users.sql"}
	if strings.Join(args.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("users replay argv = %v, want %v", args.Argv, want)
	}
	if !strings.Contains(usersLoadScript, "-f") || !strings.Contains(usersLoadScript, "<") {
		t.Errorf("usersLoadScript = %q — must force-continue and feed via stdin", usersLoadScript)
	}
}

func TestProvisionWithUsers(t *testing.T) {
	dir := withUsersDir(t)
	explains := 0
	line, calls, exit := driveOp(t, "provision", withUsersPayload(dir),
		withUsersHandler(t, dir, withUsersBehavior{views: "v_orders\n", explainCalls: &explains}))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	// readiness, put(users), replay, put(dump), create db, load dump,
	// definer gate, view list, EXPLAIN batch, reachability gate
	if len(calls) != 10 {
		t.Errorf("calls = %d, want 10", len(calls))
	}
	if explains != 1 {
		t.Errorf("EXPLAIN batches = %d, want exactly 1", explains)
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}

	// The identity covers both members with size framing, in load order.
	h := sha256.New()
	fmt.Fprintf(h, "users\x00%d\x00%s", len("CREATE USER"), "CREATE USER")
	fmt.Fprintf(h, "dump\x00%d\x00%s", len("DUMP-BYTES"), "DUMP-BYTES")
	if want := "sha256:" + hex.EncodeToString(h.Sum(nil)); res.SourceIdentity.Checksum != want {
		t.Errorf("checksum = %s, want the two-member composite %s", res.SourceIdentity.Checksum, want)
	}
	if res.SourceIdentity.SizeBytes != int64(len("CREATE USER")+len("DUMP-BYTES")) {
		t.Errorf("size_bytes = %d, want the sum of both members", res.SourceIdentity.SizeBytes)
	}

	// Both transfers and both load phases are accounted; the gates are a
	// verdict, not recovery work, and must not inflate the RTO figure.
	if res.Timings.Transfer != 0.25+0.5 {
		t.Errorf("transfer_seconds = %v, want both transfers", res.Timings.Transfer)
	}
	if res.Timings.Restore != 0.5+1.5 {
		t.Errorf("restore_seconds = %v, want replay plus load and nothing else", res.Timings.Restore)
	}

	if res.State["users_path"] != "/scratch/probavi-users.sql" || res.State["dump_path"] != "/scratch/probavi-restore.sql" {
		t.Errorf("state = %+v", res.State)
	}
}

func TestProvisionWithUsersToleratesSandboxCollisions(t *testing.T) {
	dir := withUsersDir(t)
	stderr := "ERROR 1396 (HY000) at line 1: Operation CREATE USER failed for 'root'@'localhost'\n" +
		"ERROR 1396 (HY000) at line 2: Operation CREATE USER failed for 'mysql.sys'@'localhost'"
	line, _, exit := driveOp(t, "provision", withUsersPayload(dir),
		withUsersHandler(t, dir, withUsersBehavior{usersStderr: stderr}))
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v — collisions on sandbox-created accounts must be tolerated", exit, f)
	}
}

func TestProvisionWithUsersFailures(t *testing.T) {
	tests := []struct {
		name     string
		behavior withUsersBehavior
		wantHas  string
		maxCalls int
	}{
		{"application account collision",
			withUsersBehavior{usersStderr: "ERROR 1396 (HY000) at line 3: Operation CREATE USER failed for 'app'@'%'"},
			"app", 3},
		{"replay refused with silent stderr",
			withUsersBehavior{usersExit: 1},
			"mysql exited 1", 3},
		{"orphaned definers flagged",
			withUsersBehavior{definers: "app@%\nreport@%\n"},
			"app@%, report@%", 7},
		{"broken view flagged",
			withUsersBehavior{views: "v_orders\n", explainExit: 1,
				explainErr: "ERROR 1356 (HY000) at line 1: View 'shop.v_orders' references invalid table(s) or column(s) or function(s) or definer/invoker of view lack rights to use them"},
			"restored view is not usable", 10},
		{"unreachable database flagged before the view check",
			withUsersBehavior{views: "v_orders\n", reachCount: "0"},
			"no restored account can reach database shop", 8},
		{"gate query breaks",
			withUsersBehavior{checkExit: 1, checkStderr: "ERROR 1045 (28000): Access denied for user 'root'@'localhost'"},
			"principal-chain check failed", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := withUsersDir(t)
			line, calls, exit := driveOp(t, "provision", withUsersPayload(dir),
				withUsersHandler(t, dir, tt.behavior))
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

func TestProvisionWithUsersSandboxFailure(t *testing.T) {
	dir := withUsersDir(t)
	line, calls, exit := driveOp(t, "provision", withUsersPayload(dir),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return nil, protoErr("sandbox_error", true, "container gone")
			}
			return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}, nil
		})
	f := parseFinal(t, line)
	if exit != 0 || f.OK || f.Error.Code != "sandbox_error" {
		t.Fatalf("exit=%d final=%+v, want the sandbox error to pass through untranslated", exit, f)
	}
	// readiness, put_file(users) — nothing after the dead sandbox
	if len(calls) != 2 {
		t.Errorf("calls = %d, want 2", len(calls))
	}
}

func TestProvisionCharsetOptions(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MYSQLDUMP-BYTES")
	sawCreate := false
	line, _, exit := driveOp(t, "provision",
		provisionPayload(fixture, `{"database":"orders","charset":"utf8mb4","collation":"utf8mb4_bin"}`),
		func(call verbCall) (any, *protoError) {
			if call.Verb == "put_file" {
				return putFileValue{}, nil
			}
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			stmt := args.Argv[len(args.Argv)-1]
			if strings.HasPrefix(stmt, "CREATE DATABASE") {
				sawCreate = true
				want := "CREATE DATABASE IF NOT EXISTS `orders` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"
				if stmt != want {
					t.Errorf("create statement = %q, want %q", stmt, want)
				}
			}
			if stmt == "SELECT 1" {
				return execValue{ExitCode: 0, StdoutB64: base64.StdEncoding.EncodeToString([]byte("1\n"))}, nil
			}
			return okExec(0), nil
		})
	f := parseFinal(t, line)
	if exit != 0 || !f.OK || !sawCreate {
		t.Fatalf("exit=%d final=%+v sawCreate=%v", exit, f, sawCreate)
	}
}

func TestProvisionRejectsBadCharsetOptions(t *testing.T) {
	fixture := writeFixture(t, "X")
	for _, options := range []string{
		`{"charset":"utf8mb4; DROP DATABASE x"}`,
		`{"collation":"utf8mb4_bin COLLATE evil"}`,
	} {
		line, calls, _ := driveOp(t, "provision", provisionPayload(fixture, options),
			func(verbCall) (any, *protoError) { return nil, protoErr("internal", false, "must not be called") })
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || len(calls) != 0 {
			t.Errorf("options %s: final=%+v calls=%d, want invalid_request before any verb", options, f, len(calls))
		}
	}
}
