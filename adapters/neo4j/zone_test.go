package main

import (
	"strings"
	"testing"
)

func TestRejectBackupTimezone(t *testing.T) {
	t.Run("unset is fine", func(t *testing.T) {
		if perr := rejectBackupTimezone(nil); perr != nil {
			t.Errorf("rejectBackupTimezone = %+v", perr)
		}
		if perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: ""}); perr != nil {
			t.Errorf("rejectBackupTimezone = %+v", perr)
		}
	})
	// Silently ignoring the key would leave an operator believing the
	// record carries a backup time it cannot carry.
	t.Run("set is refused with the reason", func(t *testing.T) {
		perr := rejectBackupTimezone(map[string]string{backupTimezoneParam: "Europe/Budapest"})
		if perr == nil || perr.Code != "invalid_request" {
			t.Fatalf("perr = %+v, want invalid_request", perr)
		}
		for _, want := range []string{backupTimezoneParam, "records no backup timestamp", "created_at"} {
			if !strings.Contains(perr.Message, want) {
				t.Errorf("message = %q, want it to carry %q", perr.Message, want)
			}
		}
	})
}
