package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEngineStartsWithRetentionEnforcementPinned pins the flag on the
// launch line, because that flag is the whole fix: without it the restored
// server enforces the backup's own retention on the backup's own data, and
// the bucket census this adapter runs would not notice (see retention.go).
func TestEngineStartsWithRetentionEnforcementPinned(t *testing.T) {
	dir := writeBackup(t, filepath.Join(t.TempDir(), "bak"), stemA, singleOrg())
	var sequence []string
	_, calls, exit := driveOp(t, "provision",
		provisionPayload(t, "influx_backup", dir, nil, nil),
		provisionHandler(t, &sequence, defaultSimulated(restoredBucketsJSON)))
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}

	for _, call := range calls {
		if call.Verb != "exec" {
			continue
		}
		argv := argvOf(t, call)
		if argv[0] != "sh" || !strings.Contains(argv[2], "influxd --bolt-path") {
			continue
		}
		if !strings.Contains(argv[2], "--storage-retention-check-interval "+retentionCheckInterval) {
			t.Fatalf("launch line = %q, want the retention enforcer pinned past the drill", argv[2])
		}
		return
	}
	t.Fatal("provision never launched an engine")
}

// TestRetentionCheckIntervalOutlastsAnyDrill guards the constant itself.
// Zero is the value someone reaches for to mean "never", and it is the one
// value that must not be used: the flag parser accepts it and the server
// then dies with `panic: non-positive interval for NewTicker` before it
// opens a port (measured).
func TestRetentionCheckIntervalOutlastsAnyDrill(t *testing.T) {
	d, err := time.ParseDuration(retentionCheckInterval)
	if err != nil {
		t.Fatalf("parse %q: %v", retentionCheckInterval, err)
	}
	if d <= 0 {
		t.Fatalf("interval = %s: a non-positive interval kills the engine at startup", d)
	}
	if lifetime := 10 * 365 * 24 * time.Hour; d < lifetime {
		t.Errorf("interval = %s, want more than %s so no drill can outlive one tick", d, lifetime)
	}
}
