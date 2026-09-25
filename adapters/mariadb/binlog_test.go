package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// binlog_test.go covers the replay that turns a physical full into a
// point-in-time recovery: where the chain starts, which logs are in it,
// where it stops, and the two gaps that must refuse rather than quietly
// prove less than the record will claim.

// writeBinlogs lays down a directory of logs named the way a server names
// them, plus the stray files a real one collects.
func writeBinlogs(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("log "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, stray := range []string{"binlog.index", "SHA256SUMS", "README"} {
		if err := os.WriteFile(filepath.Join(dir, stray), []byte("not a log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeBinlogInfo puts the coordinate a full backup leaves behind into a
// backup directory.
func writeBinlogInfo(t *testing.T, backup, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(backup, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadBinlogStart(t *testing.T) {
	t.Run("file and position", func(t *testing.T) {
		backup := writeBackupFixture(t)
		writeBinlogInfo(t, backup, binlogInfoNames[0], "binlog.000004\t1234\n")
		start, perr := readBinlogStart(backup)
		if perr != nil {
			t.Fatalf("readBinlogStart: %+v", perr)
		}
		if start.file != "binlog.000004" || start.position != 1234 {
			t.Errorf("start = %+v, want binlog.000004 at 1234", start)
		}
	})

	// A GTID-enabled server writes a third field. The replay is positional
	// and ignores it; what matters here is that its presence does not stop
	// the first two being read.
	t.Run("a gtid set beside them is ignored", func(t *testing.T) {
		backup := writeBackupFixture(t)
		writeBinlogInfo(t, backup, binlogInfoNames[0], "binlog.000009\t196\t3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5\n")
		start, perr := readBinlogStart(backup)
		if perr != nil {
			t.Fatalf("readBinlogStart: %+v", perr)
		}
		if start.file != "binlog.000009" || start.position != 196 {
			t.Errorf("start = %+v", start)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for _, tc := range []struct {
			name, content, want string
		}{
			{"no position", "binlog.000004\n", "does not name a log file and position"},
			{"unreadable position", "binlog.000004\tsoon\n", "readable position"},
			{"a path rather than a name", "/var/log/binlog.000004\t12\n", "a path rather than"},
			{"empty", "\n", "does not name a log file and position"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				backup := writeBackupFixture(t)
				writeBinlogInfo(t, backup, binlogInfoNames[0], tc.content)
				_, perr := readBinlogStart(backup)
				if perr == nil || !strings.Contains(perr.Message, tc.want) {
					t.Fatalf("perr = %+v, want a refusal carrying %q", perr, tc.want)
				}
			})
		}
	})

	// The backup came off a server with the binary log switched off. That
	// is not a corrupt backup, but it is one no point-in-time drill can
	// rest on, and the message says which of the two it is.
	t.Run("a backup with no coordinate at all", func(t *testing.T) {
		_, perr := readBinlogStart(writeBackupFixture(t))
		if perr == nil || perr.Code != "source_corrupt" ||
			!strings.Contains(perr.Message, "binary log disabled") {
			t.Fatalf("perr = %+v, want the absence explained", perr)
		}
	})
}

// TestReadBinlogStartAcceptsEitherName is the MariaDB-specific half of the
// coordinate reader: MariaDB renamed the xtrabackup_* metadata files at
// 11.0, as source.go already knows for the checkpoints file, so a drill
// reads whichever name the release that took the backup wrote.
func TestReadBinlogStartAcceptsEitherName(t *testing.T) {
	for _, name := range binlogInfoNames {
		t.Run(name, func(t *testing.T) {
			backup := writeBackupFixture(t)
			writeBinlogInfo(t, backup, name, "mariadb-bin.000007\t480\n")
			start, perr := readBinlogStart(backup)
			if perr != nil {
				t.Fatalf("readBinlogStart: %+v", perr)
			}
			if start.file != "mariadb-bin.000007" || start.position != 480 {
				t.Errorf("start = %+v", start)
			}
		})
	}
}

func TestBinlogFilesFrom(t *testing.T) {
	t.Run("from the named log forward, strays skipped", func(t *testing.T) {
		dir := writeBinlogs(t, "binlog.000002", "binlog.000003", "binlog.000004", "binlog.000005")
		files, perr := binlogFilesFrom(dir, binlogStart{file: "binlog.000003", position: 4})
		if perr != nil {
			t.Fatalf("binlogFilesFrom: %+v", perr)
		}
		want := "binlog.000003,binlog.000004,binlog.000005"
		if strings.Join(files, ",") != want {
			t.Errorf("files = %v, want %s — and nothing before the start", files, want)
		}
	})

	// The ordinal is a number, not text: compared as strings, binlog.999999
	// sorts after binlog.1000000 and the chain would replay an hour of
	// writes twice and then refuse.
	t.Run("ordering is numeric, not lexical", func(t *testing.T) {
		dir := writeBinlogs(t, "binlog.1000000", "binlog.999999", "binlog.1000001")
		files, perr := binlogFilesFrom(dir, binlogStart{file: "binlog.999999"})
		if perr != nil {
			t.Fatalf("binlogFilesFrom: %+v", perr)
		}
		want := "binlog.999999,binlog.1000000,binlog.1000001"
		if strings.Join(files, ",") != want {
			t.Errorf("files = %v, want %s", files, want)
		}
	})

	t.Run("a directory that is not there", func(t *testing.T) {
		_, perr := binlogFilesFrom(filepath.Join(t.TempDir(), "gone"), binlogStart{file: "binlog.000001"})
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
	})
}

// TestBinlogChainRefusals covers the two gaps that must fail the drill
// rather than let a partial replay be recorded as a complete one.
func TestBinlogChainRefusals(t *testing.T) {
	t.Run("the log the backup named is missing", func(t *testing.T) {
		dir := writeBinlogs(t, "binlog.000005", "binlog.000006")
		_, perr := binlogFilesFrom(dir, binlogStart{file: "binlog.000004"})
		if perr == nil || perr.Code != "source_not_found" ||
			!strings.Contains(perr.Message, "binlog.000004") {
			t.Fatalf("perr = %+v, want the missing first log named", perr)
		}
	})

	// The dangerous one: without this the replay succeeds, stops early,
	// and the record claims a recovery that skipped what the missing log
	// held.
	t.Run("a gap in the middle", func(t *testing.T) {
		dir := writeBinlogs(t, "binlog.000003", "binlog.000004", "binlog.000006")
		_, perr := binlogFilesFrom(dir, binlogStart{file: "binlog.000003"})
		if perr == nil || perr.Code != "source_not_found" ||
			!strings.Contains(perr.Message, "gap") ||
			!strings.Contains(perr.Message, "binlog.000004") ||
			!strings.Contains(perr.Message, "binlog.000006") {
			t.Fatalf("perr = %+v, want the gap named from both sides", perr)
		}
	})

	t.Run("two servers' logs in one directory", func(t *testing.T) {
		dir := writeBinlogs(t, "binlog.000003", "binlog.000004", "mysql-bin.000001")
		_, perr := binlogFilesFrom(dir, binlogStart{file: "binlog.000003"})
		if perr == nil || perr.Code != "source_corrupt" ||
			!strings.Contains(perr.Message, "two log series") {
			t.Fatalf("perr = %+v, want the two series refused", perr)
		}
	})

}

// TestBinlogStopDatetime pins the conversion and the zone it rests on.
func TestBinlogStopDatetime(t *testing.T) {
	t.Run("no target asked for", func(t *testing.T) {
		stop, perr := binlogStopDatetime(&provisionRequest{})
		if perr != nil || stop != "" {
			t.Fatalf("stop = %q, %+v; want empty — replay to the end of the archive", stop, perr)
		}
	})

	// The protocol hands over an absolute instant; an offset one must come
	// out in UTC, because that is the zone the replay runs under.
	t.Run("an offset instant becomes UTC", func(t *testing.T) {
		req := &provisionRequest{}
		req.PITR = &struct {
			TargetTime string `json:"target_time"`
		}{TargetTime: "2026-09-25T16:30:00+02:00"}
		stop, perr := binlogStopDatetime(req)
		if perr != nil {
			t.Fatalf("binlogStopDatetime: %+v", perr)
		}
		if stop != "2026-09-25 14:30:00" {
			t.Errorf("stop = %q, want the same instant in UTC", stop)
		}
	})

	t.Run("not a timestamp", func(t *testing.T) {
		req := &provisionRequest{}
		req.PITR = &struct {
			TargetTime string `json:"target_time"`
		}{TargetTime: "yesterday"}
		_, perr := binlogStopDatetime(req)
		if perr == nil || perr.Code != "invalid_request" {
			t.Fatalf("perr = %+v, want invalid_request", perr)
		}
	})
}

// TestBinlogReplayArgv pins the call's shape: the fixed parameters the
// script reads, then one argument per log. The script runs under bash for
// pipefail — without it a mariadb-binlog that dies half way leaves the exit
// status to mysql, which reports success for the fragment it received.
func TestBinlogReplayArgv(t *testing.T) {
	files := []string{"binlog.000003", "binlog.000004"}
	argv := binlogReplayArgv("/scratch/logs", binlogStart{file: files[0], position: 812},
		"2026-09-25 14:30:00", files)
	if argv[0] != "bash" || argv[1] != "-c" {
		t.Errorf("argv = %v, want a bash -c call", argv[:2])
	}
	if !strings.Contains(argv[2], "set -o pipefail") {
		t.Error("the replay script does not set pipefail — a half-written log would report success")
	}
	if !strings.Contains(argv[2], "TZ=UTC") {
		t.Error("the replay does not pin TZ=UTC — --stop-datetime is read in the client's zone")
	}
	tail := strings.Join(argv[4:], " ")
	if tail != "/scratch/logs 812 2026-09-25 14:30:00 "+defaultUser+" binlog.000003 binlog.000004" {
		t.Errorf("parameters = %q", tail)
	}

	// No target: the stop parameter is empty and the script takes the
	// other branch.
	argv = binlogReplayArgv("/scratch/logs", binlogStart{position: 4}, "", files)
	if argv[6] != "" {
		t.Errorf("stop = %q, want empty when no target was asked for", argv[6])
	}
}

func TestBinlogSummary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		stop  string
		want  string
	}{
		{"one log to the end", []string{"binlog.000004"}, "", "binlog.000004, to the end of the archive"},
		{"a chain to a target", []string{"binlog.000004", "binlog.000005", "binlog.000006"},
			"2026-09-25 14:30:00", "binlog.000004..binlog.000006 (3 logs), to 2026-09-25 14:30:00 UTC"},
		{"nothing", nil, "", "no binary logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := binlogSummary(tc.files, tc.stop); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProbeDeclaresEveryPITRKind holds the probe and the provision gate
// together: a kind that declares pitr the gate would refuse, or a kind the
// gate admits without the declaration, is a protocol violation (§6.2).
func TestProbeDeclaresEveryPITRKind(t *testing.T) {
	line, _, exit := driveOp(t, "probe", "{}", func(verbCall) (any, *protoError) {
		t.Fatal("probe must not touch the sandbox")
		return nil, nil
	})
	if exit != 0 {
		t.Fatalf("probe exit %d", exit)
	}
	var resp struct {
		Payload struct {
			Sources []struct {
				Kind         string `json:"kind"`
				Capabilities struct {
					PITR bool `json:"pitr"`
				} `json:"capabilities"`
			} `json:"sources"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("probe payload: %v", err)
	}
	declared := map[string]bool{}
	for _, s := range resp.Payload.Sources {
		if s.Capabilities.PITR {
			declared[s.Kind] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("the probe declares pitr for nothing — the payload was not read")
	}
	if fmt.Sprint(declared) != fmt.Sprint(pitrKinds) {
		t.Errorf("probe declares pitr for %v, the gate admits %v", declared, pitrKinds)
	}
}
