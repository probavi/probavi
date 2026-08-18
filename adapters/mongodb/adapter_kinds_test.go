package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func writeInto(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func kindPayload(kind, path, options string) string {
	return fmt.Sprintf(`{"source":{"kind":%q,"path":%q,"params":{},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":%s}`,
		kind, path, options)
}

// kindBehavior scripts the fake sandbox's answers for the calls specific
// to the account/consistency kinds; everything else succeeds.
type kindBehavior struct {
	restoreStderr string
	restoreExit   int
	userCount     string // stdout of the account-count eval; "" means "2"
	orphans       string // stdout of the orphaned-role eval
	gateExit      int
	gateStderr    string
	restoreArgv   *[]string
	gateEvals     *[]string
}

func kindHandler(t *testing.T, b kindBehavior) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{BytesCopied: 20, DurationSeconds: 0.2}, nil
		}
		args := parseExec(t, call)
		switch {
		case args.Argv[0] == "mongorestore":
			if b.restoreArgv != nil {
				*b.restoreArgv = args.Argv
			}
			return execValue{ExitCode: b.restoreExit, DurationSeconds: 1.5,
				StderrB64: b64(b.restoreStderr)}, nil
		case slices.Contains(args.Argv, pingURI):
			return stdoutExec("1\n"), nil
		case args.Argv[0] == "mongosh":
			return kindEvalResponse(t, b, args.Argv[len(args.Argv)-1]), nil
		}
		t.Fatalf("unexpected exec: %v", args.Argv)
		return nil, nil
	}
}

// kindEvalResponse answers one mongosh expression. The TTL pin runs before
// every restore and is not a gate, so it is answered ahead of the gate
// scripting — which keeps the gate cases measuring gates.
func kindEvalResponse(t *testing.T, b kindBehavior, eval string) any {
	if eval == ttlPinEval {
		return stdoutExec("1\n")
	}
	if b.gateEvals != nil {
		*b.gateEvals = append(*b.gateEvals, eval)
	}
	if b.gateExit != 0 {
		return execValue{ExitCode: b.gateExit, StderrB64: b64(b.gateStderr)}
	}
	switch eval {
	case userCountEval:
		count := b.userCount
		if count == "" {
			count = "2"
		}
		return stdoutExec(count + "\n")
	case orphanedRoleRefsEval:
		return stdoutExec(b.orphans)
	}
	t.Fatalf("unexpected mongosh eval: %s", eval)
	return nil
}

// oplogSuccessStderr is what mongorestore logs when it replays a captured
// window, timestamps and all — the shape measured on a real server.
const oplogSuccessStderr = "2026-08-09T06:36:42.395+0000\treplaying oplog\n" +
	"2026-08-09T06:36:42.395+0000\tapplied 2 oplog entries\n" +
	"2026-08-09T06:36:42.395+0000\t3 document(s) restored successfully. 0 document(s) failed to restore."

func TestProvisionWithUsers(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MONGODUMP-BYTES")
	var argv []string
	var evals []string
	line, calls, exit := driveOp(t, "provision",
		kindPayload("mongodump_with_users", fixture, `{"database":"shop"}`),
		kindHandler(t, kindBehavior{restoreArgv: &argv, gateEvals: &evals}))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	// readiness, ttl pin, put_file, mongorestore, account count,
	// orphaned roles
	if len(calls) != 6 {
		t.Errorf("calls = %d, want 6", len(calls))
	}
	if i := slices.Index(argv, "--restoreDbUsersAndRoles"); i < 0 {
		t.Errorf("restore argv = %v, want --restoreDbUsersAndRoles", argv)
	}
	if i := slices.Index(argv, "--db"); i < 0 || i+1 >= len(argv) || argv[i+1] != "shop" {
		t.Errorf("restore argv = %v, want --db shop (mongorestore refuses the flag without one)", argv)
	}
	if slices.Contains(argv, "--oplogReplay") {
		t.Errorf("restore argv = %v — the users kind must not claim oplog consistency", argv)
	}
	if len(evals) != 2 || evals[0] != userCountEval || evals[1] != orphanedRoleRefsEval {
		t.Errorf("gate evals = %d, want the account count then the orphaned-role check", len(evals))
	}
	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// The gates are a verdict, not recovery work: they must not inflate
	// the RTO figure.
	if res.Timings.Restore != 1.5 {
		t.Errorf("restore_seconds = %v, want the restore alone", res.Timings.Restore)
	}
}

func TestProvisionWithUsersRequiresDatabase(t *testing.T) {
	fixture := writeFixture(t, "X")
	line, calls, _ := driveOp(t, "provision",
		kindPayload("mongodump_with_users", fixture, `{}`),
		func(verbCall) (any, *protoError) {
			return nil, protoErr("internal", false, "must not be called")
		})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || len(calls) != 0 {
		t.Fatalf("final=%+v calls=%d, want invalid_request before any verb", f, len(calls))
	}
	if !strings.Contains(f.Error.Message, "options.database") {
		t.Errorf("message = %q, want it to name the missing option", f.Error.Message)
	}
}

func TestProvisionWithOplog(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MONGODUMP-BYTES")
	var argv []string
	var evals []string
	line, calls, exit := driveOp(t, "provision",
		kindPayload("mongodump_with_oplog", fixture, `{"database":"shop"}`),
		kindHandler(t, kindBehavior{restoreStderr: oplogSuccessStderr, restoreArgv: &argv, gateEvals: &evals}))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}
	// readiness, ttl pin, put_file, mongorestore — the oplog verdict is
	// read from the restore's own output, so it costs no extra sandbox
	// call.
	if len(calls) != 4 {
		t.Errorf("calls = %d, want 4", len(calls))
	}
	if !slices.Contains(argv, "--oplogReplay") {
		t.Errorf("restore argv = %v, want --oplogReplay", argv)
	}
	if slices.Contains(argv, "--restoreDbUsersAndRoles") {
		t.Errorf("restore argv = %v — the oplog kind must not claim the account layer", argv)
	}
	if len(evals) != 0 {
		t.Errorf("gate evals = %v, want none", evals)
	}
}

func TestPlainKindsKeepTheirFlags(t *testing.T) {
	fixture := writeFixture(t, "X")
	for _, kind := range []string{"mongodump", "mongodump_dir"} {
		path := fixture
		if kind == "mongodump_dir" {
			path = t.TempDir()
			writeInto(t, path, "a.archive", "X")
		}
		var argv []string
		line, _, _ := driveOp(t, "provision", kindPayload(kind, path, `{"database":"shop"}`),
			kindHandler(t, kindBehavior{restoreArgv: &argv}))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("%s: final = %+v", kind, f)
		}
		for _, flag := range []string{"--restoreDbUsersAndRoles", "--oplogReplay"} {
			if slices.Contains(argv, flag) {
				t.Errorf("%s argv = %v — must not carry %s", kind, argv, flag)
			}
		}
	}
}

func TestProvisionWithUsersFailures(t *testing.T) {
	tests := []struct {
		name     string
		behavior kindBehavior
		wantHas  string
	}{
		{"no accounts restored",
			kindBehavior{userCount: "0"},
			"has no user accounts"},
		{"orphaned role references",
			kindBehavior{orphans: "user shop.crossdb -> admin.global_reader\nuser shop.b -> admin.x\n"},
			"user shop.crossdb -> admin.global_reader; user shop.b -> admin.x"},
		{"gate query breaks",
			kindBehavior{gateExit: 1, gateStderr: "MongoServerError: not authorized on admin"},
			"failed"},
		{"account count is not a number",
			kindBehavior{userCount: "MongoServerError: boom"},
			"not a count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := writeFixture(t, "X")
			line, _, exit := driveOp(t, "provision",
				kindPayload("mongodump_with_users", fixture, `{"database":"shop"}`),
				kindHandler(t, tt.behavior))
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
			if strings.Contains(f.Error.Message, `"`) {
				t.Errorf("message %q must stay quote-free for protocol embedding", f.Error.Message)
			}
		})
	}
}

func TestProvisionWithOplogFailures(t *testing.T) {
	tests := []struct {
		name     string
		behavior kindBehavior
		wantCode string
		wantHas  string
	}{
		// The measured loud path: mongorestore refuses an archive with no
		// oplog section.
		{"archive carries no oplog",
			kindBehavior{restoreExit: 1, restoreStderr: "2026-08-09T06:18:11.216+0000\tFailed: no oplog file to replay; make sure you run mongodump with --oplog"},
			"restore_failed", "no oplog file to replay"},
		// The gate itself: a restore that succeeded without replaying.
		{"restore succeeded without replaying",
			kindBehavior{restoreStderr: "2026-08-09T06:18:06.211+0000\t2 document(s) restored successfully. 0 document(s) failed to restore."},
			"restore_failed", "not point-consistent"},
		{"replay announced but nothing applied",
			kindBehavior{restoreStderr: "2026-08-09T06:18:06.211+0000\treplaying oplog"},
			"restore_failed", "not point-consistent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := writeFixture(t, "X")
			line, _, exit := driveOp(t, "provision",
				kindPayload("mongodump_with_oplog", fixture, `{"database":"shop"}`),
				kindHandler(t, tt.behavior))
			f := parseFinal(t, line)
			if exit != 0 || f.OK {
				t.Fatalf("exit=%d final=%+v, want a failure", exit, f)
			}
			if f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantHas) {
				t.Errorf("error = %s (%s), want %s carrying %q",
					f.Error.Code, f.Error.Message, tt.wantCode, tt.wantHas)
			}
		})
	}
}

func TestOplogReplayGate(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		wantOK bool
	}{
		{"both markers", oplogSuccessStderr, true},
		{"zero entries still counts as replayed",
			"replaying oplog\napplied 0 oplog entries", true},
		{"announcement only", "replaying oplog", false},
		{"count only", "applied 2 oplog entries", false},
		{"silence", "", false},
		{"a plain restore", "2 document(s) restored successfully.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perr := verifyOplogReplayed([]byte(tt.stderr))
			if tt.wantOK != (perr == nil) {
				t.Errorf("verifyOplogReplayed = %+v, wantOK = %v", perr, tt.wantOK)
			}
		})
	}
}

func TestScrubSecrets(t *testing.T) {
	// Measured: admin.system.users documents embed each account's SCRAM
	// salt and derived keys, and a failed write echoes document content.
	tests := []struct{ name, in, bad string }{
		{"json shape", `{"salt":"Zxwz5b2CAXvkpUF+I/ccwg==","storedKey":"O1o0hlJvo8yIdQwTaBa4Fa3+53k="}`, "Zxwz5b2"},
		{"stored key", `credentials: { storedKey: "O1o0hlJvo8yIdQwTaBa4Fa3+53k=" }`, "O1o0hlJ"},
		{"server key", `serverKey: "AbCdEfGhIjKlMnOpQrStUvWxYz01"`, "AbCdEfG"},
		{"mixed case field", `"StoredKey": "SeCrEtMaTeRiAl"`, "SeCrEtMaTeRiAl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrubSecrets(tt.in); strings.Contains(got, tt.bad) {
				t.Errorf("scrubSecrets(%q) = %q — still carries %q", tt.in, got, tt.bad)
			}
		})
	}
	t.Run("ordinary text survives", func(t *testing.T) {
		in := `Failed: shop.orders: E11000 duplicate key error dup key: { _id: 1 }`
		if got := scrubSecrets(in); got != in {
			t.Errorf("scrubSecrets(%q) = %q — non-credential text must pass through", in, got)
		}
	})
	t.Run("verdictLine scrubs on the shared path", func(t *testing.T) {
		got := verdictLine([]byte("ts\tFailed: bulk write: salt: \"Zxwz5b2CAXvkpUF\""))
		if strings.Contains(got, "Zxwz5b2") {
			t.Errorf("verdictLine = %q — the shared message path must scrub", got)
		}
	})
	t.Run("firstLine scrubs on the shared path", func(t *testing.T) {
		got := firstLine([]byte("storedKey: \"O1o0hlJvo8yIdQ\"\nsecond line"))
		if strings.Contains(got, "O1o0hlJ") {
			t.Errorf("firstLine = %q — the shared message path must scrub", got)
		}
	})
}

func TestNameList(t *testing.T) {
	if got := nameList([]string{"a", "b"}, 5); got != "a; b" {
		t.Errorf("nameList = %q", got)
	}
	if got := nameList([]string{"a", "b", "c"}, 2); got != "a; b and 1 more" {
		t.Errorf("nameList = %q, want the capped form", got)
	}
}
