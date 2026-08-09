package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeRepoFixture(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	for path, content := range map[string]string{
		"backup/demo/backup.info": "backup-info",
		"archive/demo/wal-001":    "wal-bytes",
	} {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return repo
}

func TestDirChecksum(t *testing.T) {
	repo := writeRepoFixture(t)
	sum1, size, perr := dirChecksum(repo)
	if perr != nil {
		t.Fatalf("dirChecksum: %+v", perr)
	}
	if size != int64(len("backup-info")+len("wal-bytes")) {
		t.Errorf("size=%d", size)
	}
	sum2, _, _ := dirChecksum(repo)
	if sum1 != sum2 {
		t.Error("dirChecksum is not deterministic")
	}
	if err := os.WriteFile(filepath.Join(repo, "archive/demo/wal-001"), []byte("tampered!"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if sum3, _, _ := dirChecksum(repo); sum3 == sum1 {
		t.Error("content change must change the tree hash")
	}

	if _, _, perr := dirChecksum(t.TempDir()); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("empty dir: %+v, want source_not_found", perr)
	}
}

func TestResolveRepo(t *testing.T) {
	repo := writeRepoFixture(t)
	src, perr := resolveSource(context.Background(), "pgbackrest", repo, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || src.createdAt != nil {
		t.Errorf("src = %+v — a repository's mtimes date copies, not backups", src)
	}

	file := filepath.Join(t.TempDir(), "f.dump")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, perr := resolveSource(context.Background(), "pgbackrest", file, nil); perr == nil || perr.Code != "invalid_request" {
		t.Errorf("file as repo: %+v, want invalid_request", perr)
	}
	if _, perr := resolveSource(context.Background(), "pgbackrest", filepath.Join(t.TempDir(), "gone"), nil); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("missing repo: %+v, want source_not_found", perr)
	}
}

func pgbackrestPayload(repo, stanza string) string {
	params := "{}"
	if stanza != "" {
		params = fmt.Sprintf(`{"stanza":%q}`, stanza)
	}
	return fmt.Sprintf(`{"source":{"kind":"pgbackrest","path":%q,"params":%s},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, repo, params)
}

// physicalHandler simulates the idle sandbox through the whole physical
// flow, recording the argv sequence.
func physicalHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 42, DurationSeconds: 0.5}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("exec args: %v", err)
		}
		head := strings.Join(args.Argv[:min(3, len(args.Argv))], " ")
		if args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "pg_ctl") {
			head = "sh: pg_ctl start"
		}
		*sequence = append(*sequence, head)
		switch {
		case args.Argv[0] == "pg_isready" && len(*sequence) <= 2:
			return okExec(2), nil // idle: engine not running
		case args.Argv[0] == "pg_isready":
			return okExec(0), nil // after start: ready
		case args.Argv[0] == "psql":
			return outExec("f\n"), nil // recovery finished, promoted
		case head == "gosu postgres pgbackrest":
			return execValue{ExitCode: 0, DurationSeconds: 3.5}, nil
		case head == "sh: pg_ctl start":
			return execValue{ExitCode: 0, DurationSeconds: 2.0}, nil
		default:
			return okExec(0), nil
		}
	}
}

func TestProvisionPhysicalHappyPath(t *testing.T) {
	repo := writeRepoFixture(t)
	var sequence []string
	line, _, exit := driveOp(t, "provision", pgbackrestPayload(repo, "demo"), physicalHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	joined := strings.Join(sequence, " | ")
	for _, want := range []string{
		"pg_isready",               // idle check first
		"pgbackrest version",       // capability check
		"put_file",                 // repo transfer
		"sh -c",                    // prepare (config + empty PGDATA + chown)
		"gosu postgres pgbackrest", // the restore itself
		"sh: pg_ctl start",         // start + recovery
		"psql -h 127.0.0.1",        // promotion check: recovery must be over
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("sequence %q missing %q", joined, want)
		}
	}
	if strings.Index(joined, "put_file") < strings.Index(joined, "pgbackrest version") {
		t.Error("capability checks must run before the repo transfer")
	}

	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.Timings.Restore != 3.5 || res.Timings.Transfer != 0.5 || res.Timings.EngineReady <= 0 {
		t.Errorf("timings = %+v", res.Timings)
	}
	if res.State["mode"] != "physical" || res.State["stanza"] != "demo" {
		t.Errorf("state = %+v", res.State)
	}
}

func mustNotBeCalled(t *testing.T) func(verbCall) (any, *protoError) {
	return func(verbCall) (any, *protoError) {
		t.Error("no sandbox call may happen for this case")
		return nil, protoErr("internal", false, "must not be called")
	}
}

// alreadyRunning simulates pg_isready reporting a live engine.
func alreadyRunning(verbCall) (any, *protoError) { return okExec(0), nil }

// noPgbackrest simulates an idle sandbox whose image lacks pgbackrest.
func noPgbackrest() func(verbCall) (any, *protoError) {
	call := 0
	return func(verbCall) (any, *protoError) {
		call++
		if call == 1 {
			return okExec(2), nil // idle
		}
		return errExec(127, "pgbackrest: not found"), nil
	}
}

// restoreFails simulates the flow up to a failing pgbackrest restore.
func restoreFails(t *testing.T) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("args: %v", err)
		}
		switch {
		case args.Argv[0] == "pg_isready":
			return okExec(2), nil
		case len(args.Argv) > 2 && args.Argv[2] == "pgbackrest":
			return errExec(1, "ERROR: [055]: unable to load info file"), nil
		default:
			return okExec(0), nil
		}
	}
}

func TestProvisionPhysicalFailures(t *testing.T) {
	repo := writeRepoFixture(t)
	tests := []struct {
		name       string
		stanza     string
		handler    func(verbCall) (any, *protoError)
		wantCode   string
		wantSubstr string
	}{
		{"missing stanza", "", mustNotBeCalled(t), "invalid_request", "stanza"},
		{"stanza injection rejected", "demo; rm -rf /", mustNotBeCalled(t), "invalid_request", "stanza"},
		{"running engine refused", "demo", alreadyRunning, "invalid_request", "idle sandbox"},
		{"image without pgbackrest refused", "demo", noPgbackrest(), "invalid_request", "lacks pgbackrest"},
		{"restore failure classified", "demo", restoreFails(t), "restore_failed", "unable to load info file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, _, _ := driveOp(t, "provision", pgbackrestPayload(repo, tt.stanza), tt.handler)
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantSubstr) {
				t.Errorf("final = %+v, want %s containing %q", f, tt.wantCode, tt.wantSubstr)
			}
		})
	}
}

func pitrPayload(repo, target string) string {
	return fmt.Sprintf(
		`{"source":{"kind":"pgbackrest","path":%q,"params":{"stanza":"demo"}},"sandbox":{"scratch_dir":"/scratch"},"options":{},"pitr":{"target_time":%q}}`,
		repo, target)
}

func TestProvisionPhysicalPITR(t *testing.T) {
	repo := writeRepoFixture(t)
	var sequence []string
	line, calls, exit := driveOp(t, "provision", pitrPayload(repo, "2026-07-30T14:32:00Z"), physicalHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}

	var restoreArgv []string
	for _, call := range calls {
		if call.Verb != "exec" {
			continue
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("args: %v", err)
		}
		if slices.Contains(args.Argv, "restore") {
			restoreArgv = args.Argv
		}
	}
	for _, want := range []string{
		"--type=time",
		"--target=2026-07-30 14:32:00.000000+00", // RFC 3339 Z converted to pgbackrest's form
		"--target-action=promote",
	} {
		if !slices.Contains(restoreArgv, want) {
			t.Errorf("restore argv %v missing %q", restoreArgv, want)
		}
	}
}

// startFailsBeforeTarget simulates a restore whose WAL archive ends before
// the requested pitr target: the server refuses to start and the FATAL
// reaches stderr via the log tail.
func startFailsBeforeTarget(t *testing.T) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		args := execArgs{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("args: %v", err)
		}
		switch {
		case args.Argv[0] == "pg_isready":
			return okExec(2), nil // idle
		case args.Argv[0] == "sh" && strings.Contains(args.Argv[2], "pg_ctl"):
			return errExec(1, "pg_ctl: could not start server\nFATAL:  recovery ended before configured recovery target was reached"), nil
		default:
			return okExec(0), nil
		}
	}
}

func TestPITRRequestHandling(t *testing.T) {
	repo := writeRepoFixture(t)

	t.Run("malformed target_time refused", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", pitrPayload(repo, "yesterday 14:32"), mustNotBeCalled(t))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "RFC 3339") {
			t.Errorf("final = %+v, want invalid_request about RFC 3339", f)
		}
	})

	t.Run("logical source kind refused", func(t *testing.T) {
		payload := `{"source":{"kind":"pgdump","path":"/nonexistent.dump"},"pitr":{"target_time":"2026-07-30T14:32:00Z"}}`
		line, _, _ := driveOp(t, "provision", payload, mustNotBeCalled(t))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" || !strings.Contains(f.Error.Message, "pgbackrest") {
			t.Errorf("final = %+v, want invalid_request naming pgbackrest", f)
		}
	})

	t.Run("unreachable target is a restore verdict", func(t *testing.T) {
		line, _, _ := driveOp(t, "provision", pitrPayload(repo, "2026-07-30T14:32:00Z"), startFailsBeforeTarget(t))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "restore_failed" || !strings.Contains(f.Error.Message, "not reachable") {
			t.Errorf("final = %+v, want restore_failed about an unreachable target", f)
		}
	})
}

// TestRepoReportsNoCreationTime pins the deliberate absence: a
// repository's newest file dates a copy, not a backup, so this kind
// claims no creation time at all rather than an mtime dressed up as one.
// pgBackRest records real timestamps in backup.info; reading them is
// separate work.
func TestRepoReportsNoCreationTime(t *testing.T) {
	repo := writeRepoFixture(t)
	newest := time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(repo, "archive/demo/wal-001"), newest, newest); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	src, perr := resolveSource(context.Background(), "pgbackrest", repo, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.createdAt != nil {
		t.Errorf("createdAt = %s, want none — an mtime is not the backup's creation time", *src.createdAt)
	}
}
