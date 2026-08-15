package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlausibleSQLServerMajor(t *testing.T) {
	tests := []struct {
		value string
		want  int
	}{
		{"16", 16},
		{" 16 \n", 16},
		{"9", 9},
		{"4608", 0}, // SoftwareVendorId, where a shifted row would put it
		{"160", 0},  // a compatibility level, same story
		{"NULL", 0},
		{"", 0},
		{"8", 0},    // below anything a restorable backup states
		{"1", 0},    // the conformance sandbox's fixed stdout
		{"16.0", 0}, // majors are integers; anything else is not this column
	}
	for _, tt := range tests {
		if got := plausibleSQLServerMajor(tt.value); got != tt.want {
			t.Errorf("plausibleSQLServerMajor(%q) = %d, want %d", tt.value, got, tt.want)
		}
	}
}

// headerRowWithVersion extends the base row out to SoftwareVersionMajor.
// headerRow carries eleven columns (indexes 0–10); the padding reaches
// index 24 so the appended value lands at headerVersionIdx.
func headerRowWithVersion(major string) string {
	const headerRowColumns = 11
	row := headerRow(backupTypeFull, 1)
	for range headerVersionIdx - headerRowColumns {
		row += "|x"
	}
	return row + "|" + major
}

func TestParseBackupSetsReadsSoftwareVersionMajor(t *testing.T) {
	tests := []struct {
		name string
		row  string
		want int
	}{
		{"version column present", headerRowWithVersion("16"), 16},
		{"row stops short of the column", headerRow(backupTypeFull, 1), 0},
		{"implausible value is not a version", headerRowWithVersion("4608"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sets, perr := parseBackupSets([]byte(tt.row + "\n"))
			if perr != nil {
				t.Fatalf("parseBackupSets: %+v", perr)
			}
			if len(sets) != 1 || sets[0].softwareMajor != tt.want {
				t.Errorf("sets = %+v, want one set with softwareMajor %d", sets, tt.want)
			}
		})
	}
}

// versionProvisionHandler wraps idleProvisionHandler: the HEADERONLY probe
// answers with a row naming the backup's origin major, and the
// SERVERPROPERTY query answers with the engine's — or fails, when
// engineMajor is empty.
func versionProvisionHandler(t *testing.T, fixture string, probes *int,
	backupMajor, engineMajor string, restored *bool) func(verbCall) (any, *protoError) {
	inner := idleProvisionHandler(t, fixture, probes)
	return func(call verbCall) (any, *protoError) {
		if call.Verb == "exec" {
			args := execArgs{}
			if err := json.Unmarshal(call.Args, &args); err != nil {
				t.Fatalf("exec args: %v", err)
			}
			last := args.Argv[len(args.Argv)-1]
			if strings.Contains(last, "ProductMajorVersion") {
				if engineMajor == "" {
					return errExec(1, "Sqlcmd: Error: query failed"), nil
				}
				return outExec(engineMajor + "\n"), nil
			}
			if strings.Contains(last, "RESTORE HEADERONLY") {
				return outExec(headerRowWithVersion(backupMajor) + "\n"), nil
			}
			if len(args.Argv) >= 3 && args.Argv[2] == restoreScript {
				*restored = true
			}
		}
		return inner(call)
	}
}

func TestProvisionVersionPrecheck(t *testing.T) {
	t.Run("newer backup on older engine refused", func(t *testing.T) {
		fixture := writeMedia(t, t.TempDir(), "orders.bak", "media")
		var probes int
		var restored bool
		line, _, _ := driveOp(t, "provision", provisionPayload(fixture, `{"database":"orders"}`),
			versionProvisionHandler(t, fixture, &probes, "17", "16", &restored))
		f := parseFinal(t, line)
		if f.OK || f.Error.Code != "invalid_request" {
			t.Fatalf("final = %+v, want invalid_request", f)
		}
		for _, want := range []string{"SQL Server 2025", "SQL Server 2022", "older engine"} {
			if !strings.Contains(f.Error.Message, want) {
				t.Errorf("message %q missing %q", f.Error.Message, want)
			}
		}
		if restored {
			t.Error("a refused pairing must not attempt the restore")
		}
	})

	t.Run("older backup on newer engine is the upgrade path", func(t *testing.T) {
		fixture := writeMedia(t, t.TempDir(), "orders.bak", "media")
		var probes int
		var restored bool
		line, _, _ := driveOp(t, "provision", provisionPayload(fixture, `{"database":"orders"}`),
			versionProvisionHandler(t, fixture, &probes, "15", "16", &restored))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v — restoring an older backup upward is supported", f)
		}
		if !restored {
			t.Error("the restore must have run")
		}
	})

	t.Run("unanswerable engine version skips the check", func(t *testing.T) {
		fixture := writeMedia(t, t.TempDir(), "orders.bak", "media")
		var probes int
		var restored bool
		line, _, _ := driveOp(t, "provision", provisionPayload(fixture, `{"database":"orders"}`),
			versionProvisionHandler(t, fixture, &probes, "17", "", &restored))
		if f := parseFinal(t, line); !f.OK {
			t.Fatalf("final = %+v — a refusal needs positive evidence, not a failed query", f)
		}
	})
}
