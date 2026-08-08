package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	sum1, size, newest, perr := dirChecksum(backup)
	if perr != nil {
		t.Fatalf("dirChecksum: %+v", perr)
	}
	if size != int64(len("backup_type = full-backuped\n")+len("innodb-bytes")) || newest.IsZero() {
		t.Errorf("size=%d newest=%v", size, newest)
	}
	sum2, _, _, _ := dirChecksum(backup)
	if sum1 != sum2 {
		t.Error("dirChecksum is not deterministic")
	}
	if err := os.WriteFile(filepath.Join(backup, "ibdata1"), []byte("tampered!"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if sum3, _, _, _ := dirChecksum(backup); sum3 == sum1 {
		t.Error("content change must change the tree hash")
	}

	if _, _, _, perr := dirChecksum(t.TempDir()); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("empty dir: %+v, want source_not_found", perr)
	}
}

func TestResolveXtraBackupSource(t *testing.T) {
	backup := writeBackupFixture(t)
	src, perr := resolveSource("xtrabackup", backup, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if !strings.HasPrefix(src.checksum, "sha256:") || src.createdAt == nil {
		t.Errorf("src = %+v", src)
	}

	file := filepath.Join(t.TempDir(), "f.sql")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, perr := resolveSource("xtrabackup", file, nil); perr == nil || perr.Code != "invalid_request" {
		t.Errorf("file as backup dir: %+v, want invalid_request", perr)
	}
	if _, perr := resolveSource("xtrabackup", filepath.Join(t.TempDir(), "gone"), nil); perr == nil || perr.Code != "source_not_found" {
		t.Errorf("missing backup dir: %+v, want source_not_found", perr)
	}

	// A directory of files that is not an xtrabackup backup must be
	// refused before any transfer.
	notBackup := t.TempDir()
	if err := os.WriteFile(filepath.Join(notBackup, "random.dat"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, perr := resolveSource("xtrabackup", notBackup, nil); perr == nil || perr.Code != "source_corrupt" ||
		!strings.Contains(perr.Message, "xtrabackup_checkpoints") {
		t.Errorf("non-backup dir: %+v, want source_corrupt naming the missing file", perr)
	}
}

func TestBackupNewestMtimeIsCreatedAt(t *testing.T) {
	backup := writeBackupFixture(t)
	newest := time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(backup, "ibdata1"), newest, newest); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	old := newest.Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(backup, "xtrabackup_checkpoints"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	src, perr := resolveSource("xtrabackup", backup, nil)
	if perr != nil {
		t.Fatalf("resolveSource: %+v", perr)
	}
	if *src.createdAt != "2026-07-30T06:00:00.000Z" {
		t.Errorf("createdAt = %s, want the newest file's mtime", *src.createdAt)
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
		case strings.Contains(stmt, "--copy-back"):
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
		args, stmt := lastArg(t, call)
		switch {
		case args.Argv[0] == "mysql":
			return okExec(1), nil
		case strings.Contains(stmt, "--copy-back"):
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
