package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// adapter_withbinlogs_test.go drives the xtrabackup_with_binlogs kind
// through the whole provision flow: the two members resolve, the physical
// restore runs as before, and the replay goes in *after* the server is
// serving — because mysqlbinlog produces SQL and a server has to be up to
// accept it.

// withBinlogsFixture builds the source directory this kind expects: a
// backup and the logs written after it, side by side.
func withBinlogsFixture(t *testing.T, logs ...string) string {
	t.Helper()
	dir := t.TempDir()
	backup := filepath.Join(dir, "full")
	for path, content := range map[string]string{
		"xtrabackup_checkpoints": "backup_type = full-backuped\n",
		"ibdata1":                "innodb-bytes",
		binlogInfoFile:           logs[0] + "\t812\n",
	} {
		full := filepath.Join(backup, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binlogs := filepath.Join(dir, "binlogs")
	if err := os.MkdirAll(binlogs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range logs {
		if err := os.WriteFile(filepath.Join(binlogs, name), []byte("log "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func withBinlogsPayload(dir, pitr string) string {
	target := ""
	if pitr != "" {
		target = fmt.Sprintf(`,"pitr":{"target_time":%q}`, pitr)
	}
	return fmt.Sprintf(`{"source":{"kind":"xtrabackup_with_binlogs","path":%q,`+
		`"params":{"backup":"full","binlogs":"binlogs"}},`+
		`"sandbox":{"scratch_dir":"/scratch"},"options":{}%s}`, dir, target)
}

// binlogHandler extends the physical handler with the replay call,
// recording the argv so the test can assert what was actually run.
func binlogHandler(t *testing.T, sequence *[]string, replay *[]string) func(verbCall) (any, *protoError) {
	physical := physicalHandler(t, sequence)
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "put_file" {
			args := putFileArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("put_file args: %v", err)
			}
			if args.DestPath == "/scratch/probavi-xtrabackup-binlogs" {
				*sequence = append(*sequence, "put_file-binlogs")
				return putFileValue{BytesCopied: 9, DurationSeconds: 0.25}, nil
			}
			return physical(call)
		}
		args, _ := lastArg(t, call)
		if len(args.Argv) > 2 && strings.Contains(args.Argv[2], "mysqlbinlog") {
			*sequence = append(*sequence, "replay")
			*replay = args.Argv
			return execValue{ExitCode: 0, DurationSeconds: 1.25}, nil
		}
		return physical(call)
	}
}

func TestProvisionWithBinlogsReplaysAfterTheEngineIsUp(t *testing.T) {
	dir := withBinlogsFixture(t, "binlog.000003", "binlog.000004")
	var sequence, replay []string
	line, _, exit := driveOp(t, "provision", withBinlogsPayload(dir, "2026-09-25T16:30:00+02:00"),
		binlogHandler(t, &sequence, &replay))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	f := parseFinal(t, line)
	if !f.OK {
		t.Fatalf("final = %+v", f)
	}

	// The replay is last, and it is after the server is serving.
	want := []string{"mysql-idle", "xtrabackup-version", "put_file", "prepare", "restore",
		"start", "mysql-ready", "put_file-binlogs", "replay"}
	if strings.Join(sequence, "|") != strings.Join(want, "|") {
		t.Errorf("sequence = %v, want %v", sequence, want)
	}

	// The logs go in from the one the backup named, at its position, in
	// order, stopping at the requested instant expressed in UTC.
	tail := strings.Join(replay[4:], " ")
	wantTail := "/scratch/probavi-xtrabackup-binlogs 812 2026-09-25 14:30:00 " +
		defaultUser + " binlog.000003 binlog.000004"
	if tail != wantTail {
		t.Errorf("replay parameters = %q, want %q", tail, wantTail)
	}

	res := provisionWire{}
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// The replay is recovery: its seconds belong to restore_seconds, which
	// is what the RTO trend reads.
	if res.Timings.Restore != 3.5+1.25 {
		t.Errorf("restore_seconds = %v, want the restore and the replay together", res.Timings.Restore)
	}
	if res.State["mode"] != "physical+binlog" {
		t.Errorf("state.mode = %v, want the replay named — it is a different proof from a full-only restore",
			res.State["mode"])
	}
}

// TestProvisionWithBinlogsWithoutATargetReplaysEverything: no pitr block
// means "as far as the archive reaches", which is the recovery an operator
// performs when the question is "how much did we actually keep".
func TestProvisionWithBinlogsWithoutATargetReplaysEverything(t *testing.T) {
	dir := withBinlogsFixture(t, "binlog.000003", "binlog.000004")
	var sequence, replay []string
	line, _, _ := driveOp(t, "provision", withBinlogsPayload(dir, ""),
		binlogHandler(t, &sequence, &replay))
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if replay[6] != "" {
		t.Errorf("stop = %q, want empty — no target means the end of the archive", replay[6])
	}
}

// TestProvisionWithBinlogsRefusesABrokenChain proves the refusal reaches
// the drill rather than living in a unit test: a missing log fails the
// drill instead of replaying part of it.
func TestProvisionWithBinlogsRefusesABrokenChain(t *testing.T) {
	dir := withBinlogsFixture(t, "binlog.000003", "binlog.000005")
	var sequence, replay []string
	line, _, _ := driveOp(t, "provision", withBinlogsPayload(dir, ""),
		binlogHandler(t, &sequence, &replay))
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "source_not_found" || !strings.Contains(f.Error.Message, "gap") {
		t.Fatalf("final = %+v, want the gap refused", f)
	}
	if len(replay) != 0 {
		t.Error("the replay ran anyway — a broken chain must not be partly applied")
	}
}

func TestWithBinlogsSourceRefusals(t *testing.T) {
	dir := withBinlogsFixture(t, "binlog.000003")
	for _, tc := range []struct {
		name, params, want string
	}{
		{"no backup named", `{"binlogs":"binlogs"}`, "requires source.params.backup"},
		{"no binlogs named", `{"backup":"full"}`, "requires source.params.binlogs"},
		{"a path rather than a name", `{"backup":"../full","binlogs":"binlogs"}`, "not a path"},
		{"one name for both", `{"backup":"full","binlogs":"full"}`, "both name full"},
		{"the backup is not there", `{"backup":"gone","binlogs":"binlogs"}`, "does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"source":{"kind":"xtrabackup_with_binlogs","path":%q,"params":%s},`+
				`"sandbox":{"scratch_dir":"/scratch"},"options":{}}`, dir, tc.params)
			line, _, _ := driveOp(t, "provision", payload, func(verbCall) (any, *protoError) {
				t.Fatal("a source refusal must not touch the sandbox")
				return nil, nil
			})
			f := parseFinal(t, line)
			if f.OK || !strings.Contains(f.Error.Message, tc.want) {
				t.Fatalf("final = %+v, want a refusal carrying %q", f, tc.want)
			}
		})
	}
}

// TestPITRIsRefusedOnEveryOtherKind: the protocol forbids sending pitr to
// a kind that did not declare it (§6.2), and the adapter refuses it by
// name rather than ignoring it and proving one instant while the record
// says another.
func TestPITRIsRefusedOnEveryOtherKind(t *testing.T) {
	for _, kind := range []string{"mysqldump", "mysqldump_dir", "mysqldump_with_users", "xtrabackup"} {
		t.Run(kind, func(t *testing.T) {
			payload := fmt.Sprintf(`{"source":{"kind":%q,"path":"/backups/x","params":{}},`+
				`"sandbox":{"scratch_dir":"/scratch"},"options":{},`+
				`"pitr":{"target_time":"2026-09-25T14:30:00Z"}}`, kind)
			line, _, _ := driveOp(t, "provision", payload, func(verbCall) (any, *protoError) {
				t.Fatal("a pitr refusal must not touch the sandbox")
				return nil, nil
			})
			f := parseFinal(t, line)
			if f.OK || f.Error.Code != "invalid_request" ||
				!strings.Contains(f.Error.Message, "xtrabackup_with_binlogs") {
				t.Fatalf("final = %+v, want pitr refused naming the kind that supports it", f)
			}
		})
	}
}
