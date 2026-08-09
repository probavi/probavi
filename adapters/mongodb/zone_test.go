package main

import (
	"strings"
	"testing"
)

// TestRejectBackupTimezone pins the refusal. Silently ignoring the key
// would leave an operator believing backup.created_at is exact when this
// adapter cannot fill it at all.
func TestRejectBackupTimezone(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		if perr := rejectBackupTimezone(nil); perr != nil {
			t.Errorf("perr = %+v, want none", perr)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: ""}); perr != nil {
			t.Errorf("perr = %+v, want none", perr)
		}
	})
	t.Run("declared", func(t *testing.T) {
		perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"})
		if perr == nil || perr.Code != "invalid_request" {
			t.Fatalf("perr = %+v, want invalid_request", perr)
		}
		// The message has to say why, or the operator will assume a typo.
		for _, want := range []string{"records no backup timestamp", "created_at"} {
			if !strings.Contains(perr.Message, want) {
				t.Errorf("message = %q, want it to carry %q", perr.Message, want)
			}
		}
	})
}

// TestProvisionRefusesADeclaredZone proves the refusal happens before any
// sandbox call, not after a transfer.
func TestProvisionRefusesADeclaredZone(t *testing.T) {
	fixture := writeFixture(t, "FAKE-MONGODUMP-BYTES")
	payload := `{"source":{"kind":"mongodump","path":"` + fixture +
		`","params":{"backup_timezone":"Europe/Budapest"},"credential_env":[]},"sandbox":{"scratch_dir":"/scratch"},"options":{}}`
	line, calls, _ := driveOp(t, "provision", payload, func(verbCall) (any, *protoError) {
		return nil, protoErr("internal", false, "must not be called")
	})
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" || len(calls) != 0 {
		t.Fatalf("final = %+v, calls = %d, want invalid_request before any verb", f, len(calls))
	}
}
