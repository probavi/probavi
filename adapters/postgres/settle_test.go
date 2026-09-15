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
	if err := os.WriteFile(path, []byte("DUMP"), 0o600); err != nil {
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
	writeAgedIn(t, dir, "a-yesterday.dump", 24*time.Hour)
	newest := writeAgedIn(t, dir, "z-in-flight.dump", 0)

	stop := keepAppending(t, newest)
	_, perr := resolveSource(context.Background(), "pgdump_dir", dir, nil)
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

// TestImplicitMemberChoiceRefusesAnArtifactInFlight covers the other place
// the adapter chooses for the operator: the two-member kind picking the
// dump beside the globals script when the config does not name one.
func TestImplicitMemberChoiceRefusesAnArtifactInFlight(t *testing.T) {
	dir := t.TempDir()
	writeAgedIn(t, dir, "globals.sql", 24*time.Hour)
	writeAgedIn(t, dir, "a-yesterday.dump", 24*time.Hour)
	newest := writeAgedIn(t, dir, "z-in-flight.dump", 0)

	stop := keepAppending(t, newest)
	_, perr := resolveSource(context.Background(), "pgdump_with_globals", dir,
		map[string]string{"globals": "globals.sql"})
	stop()

	if perr == nil || perr.Code != "source_unreadable" {
		t.Fatalf("perr = %+v, want source_unreadable — the implicit choice must be checked too", perr)
	}
	if strings.Contains(perr.Message, "a-yesterday") {
		t.Error("the drill fell back to the older dump — that would prove a backup the record does not name")
	}
}

// TestMissingRoleDiagnosticNamesTheKindThatCarriesTheRoles covers what a
// drill tells an operator when the restored cluster is short a role the
// backup names — the failure of issue #278, and the one class where the
// artifact is intact and the remedy is a different source kind.
//
// Nothing asserted these messages before, which is how the custom-format
// path came to have none: pg_restore's own line went into the record, and
// an operator reading "role \"rig\" does not exist" had nothing to act on.
func TestMissingRoleDiagnosticNamesTheKindThatCarriesTheRoles(t *testing.T) {
	// The line pg_restore prints for the failure the issue reported: a
	// TimescaleDB policy's owner is a regrole, written into the catalog
	// COPY as the role's name, which --no-owner cannot touch.
	const bgwJob = `pg_restore: error: COPY failed for table "bgw_job": ERROR:  role "rig" does not exist`
	const inlineGrant = `psql:dump.sql:42: ERROR:  role "rig" does not exist`

	tests := []struct {
		name    string
		perr    *protoError
		wantAll []string
	}{
		{
			"custom-format archive, no globals in the source",
			mapRestoreFailure(1, []byte(bgwJob), dumpStorage{}, globalsKind(false, false)),
			// The engine's line survives with its quotes turned to
			// apostrophes, which is what firstLine does to everything
			// bound for a protocol message and an evidence record.
			[]string{"pgdump_with_globals", "regrole column is data", `role 'rig' does not exist`},
		},
		{
			"custom-format archive from a framed kind",
			mapRestoreFailure(1, []byte(bgwJob), dumpStorage{}, globalsKind(true, false)),
			[]string{"timescaledb_dump_with_globals"},
		},
		{
			// The globals ran and the role is still missing: the script is
			// short, and telling the operator to use the kind they are
			// already using would be advice to do what they did.
			"the source already carried globals",
			mapRestoreFailure(1, []byte(bgwJob), dumpStorage{}, globalsKind(true, true)),
			[]string{"did not create it"},
		},
		{
			"plain-SQL dump",
			mapRestoreFailure(1, []byte(inlineGrant), dumpStorage{plain: true}, globalsKind(false, false)),
			[]string{"pgdump_with_globals", "carries ownership and grants inline"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.perr == nil {
				t.Fatal("a failed restore must produce an error")
			}
			if tt.perr.Code != "restore_failed" {
				t.Errorf("code = %s, want restore_failed — the backup is intact", tt.perr.Code)
			}
			for _, want := range tt.wantAll {
				if !strings.Contains(tt.perr.Message, want) {
					t.Errorf("message = %q, want it to contain %q", tt.perr.Message, want)
				}
			}
		})
	}

	t.Run("the source that already carried globals recommends nothing", func(t *testing.T) {
		perr := mapRestoreFailure(1, []byte(bgwJob), dumpStorage{}, globalsKind(false, true))
		if strings.Contains(perr.Message, "with_globals") {
			t.Errorf("message = %q, want no kind recommended when the drill already uses one", perr.Message)
		}
	})
}
