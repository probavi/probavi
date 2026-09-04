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

func writeBackupFixture(t *testing.T) string {
	t.Helper()
	backup := filepath.Join(t.TempDir(), "backup")
	for path, content := range map[string]string{
		"xtrabackup_checkpoints": "backup_type = full-backuped\n",
		"ibdata1":                "innodb-bytes",
	} {
		full := filepath.Join(backup, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return backup
}

func TestDirChecksum(t *testing.T) {
	backup := writeBackupFixture(t)
	sum1, size, perr := dirChecksum(backup)
	if perr != nil {
		t.Fatalf("dirChecksum: %+v", perr)
	}
	if size != int64(len("backup_type = full-backuped\n")+len("innodb-bytes")) {
		t.Errorf("size=%d", size)
	}
	sum2, _, _ := dirChecksum(backup)
	if sum1 != sum2 {
		t.Error("dirChecksum is not deterministic")
	}
	if err := os.WriteFile(filepath.Join(backup, "ibdata1"), []byte("tampered!"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if sum3, _, _ := dirChecksum(backup); sum3 == sum1 {
		t.Error("content change must change the tree hash")
	}

	if _, _, perr := dirChecksum(t.TempDir()); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("empty dir: %+v, want source_not_found", perr)
	}
}

func TestResolveXtraBackupSource(t *testing.T) {
	backup := writeBackupFixture(t)
	src, perr := resolveSource(context.Background(), "xtrabackup", backup, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || src.createdAt != nil {
		t.Errorf("src = %+v — a backup directory's mtimes date copies, not backups", src)
	}

	file := filepath.Join(t.TempDir(), "f.sql")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, perr := resolveSource(context.Background(), "xtrabackup", file, nil); perr == nil || perr.Code != "invalid_request" {
		t.Errorf("file as backup dir: %+v, want invalid_request", perr)
	}
	if _, perr := resolveSource(context.Background(), "xtrabackup", filepath.Join(t.TempDir(), "gone"), nil); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("missing backup dir: %+v, want source_not_found", perr)
	}

	// A directory of files that is not an xtrabackup backup must be
	// refused before any transfer.
	notBackup := t.TempDir()
	if err := os.WriteFile(filepath.Join(notBackup, "random.dat"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, perr := resolveSource(context.Background(), "xtrabackup", notBackup, nil); perr == nil || perr.Code != "source_corrupt" ||
		!strings.Contains(perr.Message, "xtrabackup_checkpoints") {
		t.Errorf("non-backup dir: %+v, want source_corrupt naming the missing file", perr)
	}
}

// TestBackupWithoutMetadata pins the fail-closed half: this fixture
// carries no xtrabackup_info, so nothing dates the backup and nothing is
// invented from the newest file's mtime, which dates a copy. The readable
// case lives in backuptime_test.go.
func TestBackupWithoutMetadata(t *testing.T) {
	backup := writeBackupFixture(t)
	newest := time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(backup, "xtrabackup_checkpoints"), newest, newest); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	src, perr := resolveSource(context.Background(), "xtrabackup", backup,
		map[string]string{backupTimezoneParam: "Asia/Tokyo"})
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.createdAt != nil {
		t.Errorf("createdAt = %s, want none — an mtime is not the backup's creation time", *src.createdAt)
	}
}

// TestBackupIsDatedByItsMetadata proves the wiring: the completion time in
// xtrabackup_info, placed in the declared zone.
func TestBackupIsDatedByItsMetadata(t *testing.T) {
	backup := writeBackupFixture(t)
	if err := os.WriteFile(filepath.Join(backup, xtrabackupInfoName),
		[]byte("tool_name = xtrabackup\nstart_time = 2026-08-10 00:50:23\nend_time = 2026-08-10 00:50:25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, perr := resolveSource(context.Background(), "xtrabackup", backup,
		map[string]string{backupTimezoneParam: "Asia/Tokyo"})
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if src.createdAt == nil || *src.createdAt != "2026-08-10T00:50:25.000+09:00" {
		t.Errorf("createdAt = %v, want the backup's own completion time in the declared zone", src.createdAt)
	}
}

func xtrabackupPayload(backup string) string {
	return fmt.Sprintf(`{"source":{"kind":"xtrabackup","path":%q,"params":{}},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, backup)
}

// physicalHandler simulates the idle sandbox through the whole physical
// flow, recording a label per call.
func physicalHandler(t *testing.T, sequence *[]string) func(verbCall) (any, *protoError) {
	started := false
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.DestPath != "/scratch/probavi-xtrabackup" || args.Mode != "0755" {
				t.Errorf("put_file args = %+v", args)
			}
			*sequence = append(*sequence, "put_file")
			return putFileValue{BytesCopied: 42, DurationSeconds: 0.5}, nil
		}
		args, stmt := lastArg(t, call)
		switch {
		case stmt == pinnedQuery:
			return pinnedExec(), nil
		case args.Argv[0] == "mysql" && !started:
			*sequence = append(*sequence, "mysql-idle")
			return okExec(1), nil // idle: engine not running
		case args.Argv[0] == "mysql":
			*sequence = append(*sequence, "mysql-ready")
			return okExec(0), nil // after start: ready
		case args.Argv[0] == "xtrabackup":
			*sequence = append(*sequence, "xtrabackup-version")
			return okExec(0), nil
		case strings.Contains(stmt, "probavi-init.sql") && !strings.Contains(stmt, "--daemonize"):
			*sequence = append(*sequence, "prepare")
			return okExec(0), nil
		case strings.Contains(shellScript(args), "--copy-back"):
			*sequence = append(*sequence, "restore")
			return execValue{ExitCode: 0, DurationSeconds: 3.5}, nil
		case strings.Contains(stmt, "--daemonize"):
			*sequence = append(*sequence, "start")
			started = true
			return execValue{ExitCode: 0, DurationSeconds: 2.0}, nil
		default:
			t.Fatalf("unexpected exec: %v", args.Argv)
			return nil, nil
		}
	}
}

func TestProvisionPhysicalHappyPath(t *testing.T) {
	backup := writeBackupFixture(t)
	var sequence []string
	line, _, exit := driveOp(t, "provision", xtrabackupPayload(backup), physicalHandler(t, &sequence))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	want := []string{"mysql-idle", "xtrabackup-version", "put_file", "prepare", "restore", "start", "mysql-ready"}
	if strings.Join(sequence, "|") != strings.Join(want, "|") {
		t.Errorf("sequence = %v, want %v", sequence, want)
	}

	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if res.Timings.Restore != 3.5 || res.Timings.Transfer != 0.5 || res.Timings.EngineReady < 2.0 {
		t.Errorf("timings = %+v", res.Timings)
	}
	if res.Connection.Database != "mysql" || res.Connection.User != "root" {
		t.Errorf("connection = %+v — physical mode serves root on the system schema", res.Connection)
	}
	if res.State["mode"] != "physical" {
		t.Errorf("state = %+v", res.State)
	}
}

func mustNotBeCalled(t *testing.T) func(verbCall) (any, *protoError) {
	return func(verbCall) (any, *protoError) {
		t.Error("no sandbox call may happen for this case")
		return nil, protoErr("internal", false, "must not be called")
	}
}

// alreadyRunning simulates a live engine answering the idle probe.
func alreadyRunning(verbCall) (any, *protoError) { return okExec(0), nil }

// noXtrabackup simulates an idle sandbox whose image lacks xtrabackup.
func noXtrabackup() func(verbCall) (any, *protoError) {
	call := 0
	return func(verbCall) (any, *protoError) {
		call++
		if call == 1 {
			return okExec(1), nil // idle
		}
		return errExec(127, "xtrabackup: not found"), nil
	}
}

// restoreFails simulates the flow up to a failing prepare/copy-back.
func restoreFails(t *testing.T) func(verbCall) (any, *protoError) {
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{}, nil
		}
		args, _ := lastArg(t, call)
		switch {
		case args.Argv[0] == "mysql":
			return okExec(1), nil
		case strings.Contains(shellScript(args), "--copy-back"):
			return errExec(1, "xtrabackup: Error: cannot open ./xtrabackup_logfile"), nil
		default:
			return okExec(0), nil
		}
	}
}

func TestProvisionPhysicalFailures(t *testing.T) {
	backup := writeBackupFixture(t)
	tests := []struct {
		name       string
		payload    string
		handler    func(verbCall) (any, *protoError)
		wantCode   string
		wantSubstr string
	}{
		{"running engine refused", xtrabackupPayload(backup), alreadyRunning, "invalid_request", "idle sandbox"},
		{"image without xtrabackup refused", xtrabackupPayload(backup), noXtrabackup(), "invalid_request", "lacks xtrabackup"},
		{"restore failure classified", xtrabackupPayload(backup), restoreFails(t), "restore_failed", "xtrabackup_logfile"},
		{"non-backup directory refused before any verb",
			xtrabackupPayload(t.TempDir()), mustNotBeCalled(t), "source_not_found", "contains no files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, _, _ := driveOp(t, "provision", tt.payload, tt.handler)
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != tt.wantCode || !strings.Contains(f.Error.Message, tt.wantSubstr) {
				t.Errorf("final = %+v, want %s containing %q", f, tt.wantCode, tt.wantSubstr)
			}
		})
	}
}

// versionedBackupFixture writes a backup whose xtrabackup_info names the
// origin server, so the version pre-check has something to read.
func versionedBackupFixture(t *testing.T, serverVersion string) string {
	t.Helper()
	backup := writeBackupFixture(t)
	if err := os.WriteFile(filepath.Join(backup, xtrabackupInfoName),
		[]byte("tool_name = xtrabackup\nserver_version = "+serverVersion+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return backup
}

// mysqldAnswering wraps physicalHandler with an answer for the engine
// version probe; an empty out simulates an image whose PATH has no mysqld.
func mysqldAnswering(t *testing.T, sequence *[]string, out string) func(verbCall) (any, *protoError) {
	inner := physicalHandler(t, sequence)
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "exec" {
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			if args.Argv[0] == "mysqld" {
				*sequence = append(*sequence, "mysqld-version")
				if out == "" {
					return errExec(127, "mysqld: not found"), nil
				}
				return outExec(out), nil
			}
		}
		return inner(call)
	}
}

// driveVersionedProvision runs an xtrabackup provision against a backup
// whose metadata states serverVersion, with the engine answering out.
func driveVersionedProvision(t *testing.T, serverVersion, out string) (finalResponse, string) {
	t.Helper()
	backup := versionedBackupFixture(t, serverVersion)
	var sequence []string
	line, _, _ := driveOp(t, "provision", xtrabackupPayload(backup),
		mysqldAnswering(t, &sequence, out))
	return parseFinal(t, line), strings.Join(sequence, " | ")
}

func TestPhysicalVersionPrecheckRefusesAMismatch(t *testing.T) {
	f, sequence := driveVersionedProvision(t, "8.0.36-28",
		"/usr/sbin/mysqld  Ver 8.4.5 for Linux on x86_64 (MySQL Community Server - GPL)")
	if f.OK || f.Error.Code != "invalid_request" {
		t.Fatalf("final = %+v, want invalid_request", f)
	}
	for _, want := range []string{"series 8.0", "engine is 8.4"} {
		if !strings.Contains(f.Error.Message, want) {
			t.Errorf("message %q missing %q", f.Error.Message, want)
		}
	}
	if strings.Contains(sequence, "put_file") {
		t.Error("a refused pairing must not transfer the backup")
	}
}

func TestPhysicalVersionPrecheckPasses(t *testing.T) {
	const engine84 = "/usr/sbin/mysqld  Ver 8.4.5 for Linux on x86_64 (MySQL Community Server - GPL)"

	t.Run("match proceeds and runs before the transfer", func(t *testing.T) {
		f, sequence := driveVersionedProvision(t, "8.4.2", engine84)
		if !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if !strings.Contains(sequence, "mysqld-version") {
			t.Fatalf("sequence %q missing the version probe", sequence)
		}
		if strings.Index(sequence, "mysqld-version") > strings.Index(sequence, "put_file") {
			t.Error("the version pre-check must run before the backup transfer")
		}
	})
	t.Run("unanswerable engine version skips the check", func(t *testing.T) {
		f, _ := driveVersionedProvision(t, "8.0.36", "")
		if !f.OK {
			t.Fatalf("final = %+v — a refusal needs positive evidence, not a failed probe", f)
		}
	})
	t.Run("backup without metadata skips the check", func(t *testing.T) {
		backup := writeBackupFixture(t) // no xtrabackup_info: no version to read
		var sequence []string
		line, _, _ := driveOp(t, "provision", xtrabackupPayload(backup), physicalHandler(t, &sequence))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v", f)
		}
		if strings.Contains(strings.Join(sequence, " | "), "mysqld-version") {
			t.Error("no version in the backup — the engine must not be queried")
		}
	})
}

// TestPhysicalRestorePassesPathsPositionally covers a scratch directory
// the operator chose the spelling of.
//
// The bare-host provider builds scratch_dir from workspace_root, which is
// only checked for a leading slash — so a space in it used to break the
// restore, and a metacharacter used to reach the shell as script rather
// than as a path. Folding paths into the script text is what made that
// possible; passing them as positional parameters is what the sandbox
// providers already do.
func TestPhysicalRestorePassesPathsPositionally(t *testing.T) {
	const hostile = `/var/lib/probavi drills/$(touch pwned) 'x'`
	backup := writeBackupFixture(t)
	var got []string
	payload := fmt.Sprintf(
		`{"source":{"kind":"xtrabackup","path":%q,"params":{}},"sandbox":{"scratch_dir":%q},"options":{}}`,
		backup, hostile)

	// A handler of its own rather than the shared one: that asserts the
	// transfer lands under /scratch, which is the very thing this test
	// changes.
	started := false
	line, _, _ := driveOp(t, "provision", payload, func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			return putFileValue{BytesCopied: 42, DurationSeconds: 0.5}, nil
		}
		args, stmt := lastArg(t, call)
		script := shellScript(args)
		switch {
		case stmt == pinnedQuery:
			return pinnedExec(), nil
		case args.Argv[0] == "mysql" && !started:
			return okExec(1), nil
		case args.Argv[0] == "mysql":
			return okExec(0), nil
		case args.Argv[0] == "xtrabackup":
			return okExec(0), nil
		case strings.Contains(script, "--copy-back"):
			got = args.Argv
			return execValue{ExitCode: 0, DurationSeconds: 3.5}, nil
		case strings.Contains(script, "--daemonize"):
			started = true
			return execValue{ExitCode: 0, DurationSeconds: 2.0}, nil
		default:
			return okExec(0), nil
		}
	})
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	want := []string{"sh", "-c", physicalRestoreScript, "sh", hostile + "/probavi-xtrabackup", "/var/lib/mysql"}
	if !slices.Equal(got, want) {
		t.Errorf("restore argv = %q,\nwant %q", got, want)
	}
}
