package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSettled(t *testing.T) {
	at := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		before, after fileState
		want          bool
	}{
		{"nothing moved", fileState{10, at}, fileState{10, at}, true},
		{"still growing", fileState{10, at}, fileState{20, at}, false},
		{"rewritten in place", fileState{10, at}, fileState{10, at.Add(time.Second)}, false},
		{"truncated", fileState{20, at}, fileState{10, at}, false},
		{"empty and still empty", fileState{0, at}, fileState{0, at}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := settled(tt.before, tt.after); got != tt.want {
				t.Errorf("settled = %v, want %v", got, tt.want)
			}
		})
	}
}

// writeAged writes a file and backdates it, so the common case — a backup
// finished long before the drill — needs no waiting at all.
func writeAged(t *testing.T, name string, age time.Duration) string {
	t.Helper()
	return writeAgedIn(t, t.TempDir(), name, age)
}

func writeAgedIn(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, rdbFixture(), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// keepAppending simulates the backup job still writing the file, and
// returns a stop function the test defers.
func keepAppending(t *testing.T, path string) func() {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return
			}
			_, werr := f.WriteString("more bytes\n")
			cerr := f.Close()
			if werr != nil || cerr != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return func() {
		close(stop)
		wg.Wait()
	}
}

func TestAssertSettledAcceptsAFinishedBackup(t *testing.T) {
	path := writeAged(t, "orders.dump", time.Hour)
	start := time.Now()
	if perr := assertSettled(context.Background(), path, settleWindow); perr != nil {
		t.Fatalf("assertSettled = %+v, want a finished backup to pass", perr)
	}
	// An artifact untouched for longer than the window is finished by
	// definition: a drill must not pay the wait for it.
	if elapsed := time.Since(start); elapsed >= settleWindow {
		t.Errorf("took %v, want no wait for a backup last written %v ago", elapsed, time.Hour)
	}
}

func TestAssertSettledAcceptsAJustFinishedBackup(t *testing.T) {
	path := writeAged(t, "orders.dump", 0)
	if perr := assertSettled(context.Background(), path, 50*time.Millisecond); perr != nil {
		t.Fatalf("assertSettled = %+v, want a file nothing is writing to to pass", perr)
	}
}

// TestAssertSettledRefusesAFileInFlight is the reproduction: a backup job
// is appending to the newest file while the drill looks at it.
func TestAssertSettledRefusesAFileInFlight(t *testing.T) {
	path := writeAged(t, "in-flight.dump", 0)
	stop := keepAppending(t, path)
	perr := assertSettled(context.Background(), path, 100*time.Millisecond)
	stop()

	if perr == nil {
		t.Fatal("assertSettled accepted a file that grew while it was looked at")
	}
	if perr.Code != "source_unreadable" {
		t.Errorf("code = %s, want source_unreadable", perr.Code)
	}
	// The message has to teach the fix, not just report the symptom: an
	// operator seeing this needs to know the drill raced their backup job.
	for _, want := range []string{"still being written", "rename"} {
		if !strings.Contains(perr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", perr.Message, want)
		}
	}
	if strings.Contains(perr.Message, `"`) {
		t.Errorf("message %q must stay quote-free for protocol embedding", perr.Message)
	}
}

func TestAssertSettledEdgeCases(t *testing.T) {
	t.Run("missing artifact", func(t *testing.T) {
		perr := assertSettled(context.Background(), filepath.Join(t.TempDir(), "gone"), settleWindow)
		if perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("cancellation is honored during the wait", func(t *testing.T) {
		path := writeAged(t, "fresh.dump", 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		perr := assertSettled(ctx, path, time.Minute)
		if perr == nil || perr.Code != "cancelled" {
			t.Errorf("perr = %+v, want cancelled — a drill must stay killable", perr)
		}
	})
}

// TestDirectoryScanRefusesAnArtifactInFlight proves the check is wired
// into the choice the adapter makes for the operator, and — the point of
// the design — that it refuses rather than quietly restoring the older
// backup sitting next to it.
func TestDirectoryScanRefusesAnArtifactInFlight(t *testing.T) {
	dir := t.TempDir()
	writeAgedIn(t, dir, "a-yesterday.rdb", 24*time.Hour)
	newest := writeAgedIn(t, dir, "z-in-flight.rdb", 0)

	stop := keepAppending(t, newest)
	_, perr := resolveSource(context.Background(), "valkey_rdb_dir", dir)
	stop()

	if perr == nil {
		t.Fatal("resolveSource accepted a backup that was still being written")
	}
	if perr.Code != "source_unreadable" {
		t.Errorf("code = %s (%s), want source_unreadable", perr.Code, perr.Message)
	}
	if strings.Contains(perr.Message, "a-yesterday") {
		t.Error("the drill fell back to the older backup — that would prove a backup the record does not name")
	}
}
