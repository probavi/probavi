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
	at := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		before, after fileState
		want          bool
	}{
		"nothing moved":         {fileState{10, at}, fileState{10, at}, true},
		"still growing":         {fileState{10, at}, fileState{20, at}, false},
		"rewritten in place":    {fileState{10, at}, fileState{10, at.Add(time.Second)}, false},
		"truncated":             {fileState{20, at}, fileState{10, at}, false},
		"empty and still empty": {fileState{0, at}, fileState{0, at}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := settled(tc.before, tc.after); got != tc.want {
				t.Errorf("settled = %v, want %v", got, tc.want)
			}
		})
	}
}

// agedRoot writes a data root and backdates every path in it, so the
// common case — a copy finished long before the drill — waits for nothing.
func agedRoot(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.Rename(writeDataRoot(t, dataRootOptions{checkpointed: true}), path); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	err := filepath.WalkDir(path, func(p string, _ os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		return os.Chtimes(p, when, when)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// keepAppending simulates a copy job still writing. The bytes go into a
// column file inside the data root rather than into the directory itself,
// which is the case a per-directory measurement would miss entirely.
func keepAppending(t *testing.T, root string) func() {
	t.Helper()
	column := filepath.Join(root, "db", "orders~9", "2026-09-01", "id.d")
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
			f, err := os.OpenFile(column, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return
			}
			_, werr := f.WriteString("more column bytes\n")
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

func TestAssertSettledAcceptsAFinishedCopy(t *testing.T) {
	root := agedRoot(t, t.TempDir(), "nightly", time.Hour)
	start := time.Now()
	if perr := assertSettled(context.Background(), root, settleWindow); perr != nil {
		t.Fatalf("assertSettled = %+v, want a finished copy to pass", perr)
	}
	if elapsed := time.Since(start); elapsed >= settleWindow {
		t.Errorf("took %v, want no wait for a copy last written an hour ago", elapsed)
	}
}

func TestAssertSettledAcceptsAJustFinishedCopy(t *testing.T) {
	root := agedRoot(t, t.TempDir(), "nightly", 0)
	if perr := assertSettled(context.Background(), root, 50*time.Millisecond); perr != nil {
		t.Fatalf("assertSettled = %+v, want a copy nothing is writing to to pass", perr)
	}
}

// TestAssertSettledRefusesACopyInFlight is the case settle.go exists for:
// a data root is hundreds of megabytes of preallocated column files, so a
// copy of one is never instantaneous and a drill can easily meet it
// half-written.
func TestAssertSettledRefusesACopyInFlight(t *testing.T) {
	root := agedRoot(t, t.TempDir(), "in-flight", 0)
	stop := keepAppending(t, root)
	perr := assertSettled(context.Background(), root, 100*time.Millisecond)
	stop()

	if perr == nil {
		t.Fatal("assertSettled accepted a data root that grew while it was looked at")
	}
	if perr.Code != "source_unreadable" {
		t.Errorf("code = %s, want source_unreadable", perr.Code)
	}
	// The message teaches the fix rather than reporting the symptom: an
	// operator reading it needs to know the drill raced their copy job.
	for _, want := range []string{"still being written", "rename"} {
		if !strings.Contains(perr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", perr.Message, want)
		}
	}
}

func TestAssertSettledEdgeCases(t *testing.T) {
	t.Run("an artifact that is not there", func(t *testing.T) {
		perr := assertSettled(context.Background(), filepath.Join(t.TempDir(), "gone"), settleWindow)
		if perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
	t.Run("a path beneath a file", func(t *testing.T) {
		root := agedRoot(t, t.TempDir(), "nightly", time.Hour)
		perr := assertSettled(context.Background(), filepath.Join(root, "conf", "server.conf", "db"), settleWindow)
		if perr == nil || perr.Code != "source_unreadable" {
			t.Errorf("perr = %+v, want source_unreadable", perr)
		}
	})
	t.Run("a single file is measured as itself", func(t *testing.T) {
		root := agedRoot(t, t.TempDir(), "nightly", time.Hour)
		if perr := assertSettled(context.Background(), filepath.Join(root, "conf", "server.conf"), settleWindow); perr != nil {
			t.Errorf("perr = %+v, want a still file to pass", perr)
		}
	})
	t.Run("cancellation is honoured during the wait", func(t *testing.T) {
		root := agedRoot(t, t.TempDir(), "fresh", 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		perr := assertSettled(ctx, root, time.Minute)
		if perr == nil || perr.Code != "cancelled" {
			t.Errorf("perr = %+v, want cancelled — a drill must stay killable", perr)
		}
	})
}

// TestADrillRefusesACopyStillBeingWritten proves the check is wired into
// the choice the adapter makes for the operator: the newest copy in the
// directory is the one drilled, and a copy in flight fails the drill
// before the sandbox is touched rather than falling back to the older
// copy beside it — that would prove a backup the record does not name.
func TestADrillRefusesACopyStillBeingWritten(t *testing.T) {
	dir := t.TempDir()
	agedRoot(t, dir, "a-yesterday", 24*time.Hour)
	newest := agedRoot(t, dir, "z-in-flight", 0)

	stop := keepAppending(t, newest)
	line, calls, exit := driveOp(t, "provision", provisionPayload(dir, "questdb_checkpoint_dir"),
		func(verbCall) (any, *protoError) {
			t.Error("the sandbox was touched for a copy that is still being written")
			return okExec(0), nil
		})
	stop()

	if exit != 0 || len(calls) != 0 {
		t.Fatalf("exit = %d, sandbox calls = %d", exit, len(calls))
	}
	f := parseFinal(t, line)
	if f.OK || f.Error == nil || f.Error.Code != "source_unreadable" {
		t.Fatalf("final = %+v, want source_unreadable", f)
	}
	if !strings.Contains(f.Error.Message, "z-in-flight") {
		t.Errorf("message = %q, want it to name the copy in flight rather than the older one",
			f.Error.Message)
	}
}
