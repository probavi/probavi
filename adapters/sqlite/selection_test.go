package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers which backup a directory source restores.
// Neither a SQLite database header nor a plain-SQL dump records a
// wall clock, so file modification time is the whole of the ordering
// (see selection.go).

// writeCandidate writes a database file with a modification time in the past,
// far enough back that nothing here trips the settle window (settle.go).
func writeCandidate(t *testing.T, dir, name string, back time.Duration) string {
	t.Helper()
	path := writeArtifact(t, dir, name, dbFixture())
	age(t, path, back)
	return path
}

// chooseIn runs the directory scan and fails the test on a refusal.
func chooseIn(t *testing.T, dir string, policy selectPolicy) string {
	t.Helper()
	got, perr := chosenDatabaseIn(context.Background(), dir, policy)
	if perr != nil {
		t.Fatalf("chosenDatabaseIn: %+v", perr)
	}
	return got
}

// stamp gives several files one modification time, so a test can make the
// names the only difference left between them.
func stamp(t *testing.T, when time.Time, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

// TestBackupSelectionAccepts pins what the parameter takes, and that a
// drill written before it existed is unaffected.
func TestBackupSelectionAccepts(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		params map[string]string
		want   selectPolicy
	}{
		{
			name: "no parameter is what every drill written before this got",
			kind: "sqlite_db_dir", params: nil, want: selectNewest,
		},
		{name: "newest", kind: "sqlite_db_dir", params: map[string]string{"select": "newest"}, want: selectNewest},
		{name: "oldest", kind: "sqlite_db_dir", params: map[string]string{"select": "oldest"}, want: selectOldest},
		{name: "random", kind: "sqlite_db_dir", params: map[string]string{"select": "random"}, want: selectRandom},
		{
			name: "the dump directory kind takes it too",
			kind: "sqlite_dump_dir", params: map[string]string{"select": "oldest"}, want: selectOldest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, perr := backupSelection(tt.kind, tt.params)
			if perr != nil {
				t.Fatalf("refused a valid selection: %s: %s", perr.Code, perr.Message)
			}
			if got != tt.want {
				t.Errorf("policy = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBackupSelectionRefuses is the half that matters more: a parameter
// nothing reads is a config the operator believes in and a drill doing
// something else, so a kind that chooses nothing refuses the word rather
// than accepting it — the same discipline rejectBackupTimezone already
// applies to the other source parameter.
func TestBackupSelectionRefuses(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		params map[string]string
		want   string // substring of the refusal
	}{
		{
			name: "an unknown policy is refused, not rounded to the default",
			kind: "sqlite_db_dir", params: map[string]string{"select": "latest"},
			want: "must be newest, oldest or random",
		},
		{
			name: "capitalisation is not a policy either",
			kind: "sqlite_db_dir", params: map[string]string{"select": "Oldest"},
			want: "Oldest is none of them",
		},
		{
			name: "the single-artifact kinds choose nothing",
			kind: "sqlite_db", params: map[string]string{"select": "oldest"},
			want: "kind sqlite_db restores what source.path names",
		},
		{
			name: "nor does the single dump kind",
			kind: "sqlite_dump", params: map[string]string{"select": "random"},
			want: "applies only to sqlite_db_dir and sqlite_dump_dir",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, perr := backupSelection(tt.kind, tt.params)
			if perr == nil {
				t.Fatalf("accepted %v, want a refusal mentioning %q", tt.params, tt.want)
			}
			if perr.Code != "invalid_request" {
				t.Errorf("code = %q, want invalid_request — the config asked for something impossible",
					perr.Code)
			}
			if !strings.Contains(perr.Message, tt.want) {
				t.Errorf("message = %q, want it to mention %q", perr.Message, tt.want)
			}
		})
	}
}

// TestSelectionPicksTheEndsOfTheWindow is the item's point: a directory
// holding a retention window can be drilled at either end, and the oldest
// backup is the one an incident reaches for once the damage turns out to
// predate yesterday.
func TestSelectionPicksTheEndsOfTheWindow(t *testing.T) {
	dir := t.TempDir()
	// Written in an order matching neither the file times nor the names,
	// so a policy reading directory order would be visible.
	middle := writeCandidate(t, dir, "b.db", 48*time.Hour)
	oldest := writeCandidate(t, dir, "c.db", 72*time.Hour)
	newest := writeCandidate(t, dir, "a.db", 24*time.Hour)

	for _, tt := range []struct {
		policy selectPolicy
		want   string
	}{
		{selectNewest, newest},
		{selectOldest, oldest},
	} {
		t.Run(string(tt.policy), func(t *testing.T) {
			got := chooseIn(t, dir, tt.policy)
			if got != tt.want {
				t.Errorf("picked %s, want %s", filepath.Base(got), filepath.Base(tt.want))
			}
			if got == middle {
				t.Error("picked the middle of the window under an end-of-window policy")
			}
		})
	}
}

// TestSelectionTieBreaksDeterministically keeps a choice from depending
// on the order readdir happens to return entries in — the property the
// newest policy has always had, now in the other direction too.
func TestSelectionTieBreaksDeterministically(t *testing.T) {
	dir := t.TempDir()
	// One instant for both, because two calls a moment apart do not tie:
	// modification times carry nanoseconds, and the rule under test is
	// the one that decides when they are genuinely equal.
	second := writeCandidate(t, dir, "b.db", 24*time.Hour)
	first := writeCandidate(t, dir, "a.db", 24*time.Hour)
	stamp(t, time.Now().Add(-24*time.Hour), first, second)

	if got := chooseIn(t, dir, selectOldest); got != first {
		t.Errorf("picked %s, want %s — ties break by name, and oldest takes the smaller one",
			filepath.Base(got), filepath.Base(first))
	}
}

// TestPrecedesMirrorsBeatsHere states the asymmetry this adapter does not
// have. In the postgres and mysql adapters a backup carrying its own
// recorded time outranks one that does not, and that rule cannot invert —
// here file time is the only fact, so the two orders really are mirrors.
// Written down because the difference is the kind a reader assumes away.
func TestPrecedesMirrorsBeatsHere(t *testing.T) {
	base := time.Now()
	early := dirCandidate{name: "a", mtime: base.Add(-time.Hour)}
	late := dirCandidate{name: "b", mtime: base}

	if !late.beats(early) || early.beats(late) {
		t.Error("beats must take the newer file")
	}
	if !early.precedes(late) || late.precedes(early) {
		t.Error("precedes must take the older file")
	}
	same := dirCandidate{name: "b", mtime: base}
	if late.beats(same) || late.precedes(same) {
		t.Error("a candidate must not outrank an identical one in either direction")
	}
}

// TestRandomCoversTheWindow. Random is not reproducible and is not asked
// to be; what it must do is reach every backup over time. Three candidates
// over 200 draws leave the probability that any single one is missed
// below 3·(2/3)²⁰⁰.
func TestRandomCoversTheWindow(t *testing.T) {
	dir := t.TempDir()
	wanted := map[string]bool{
		writeCandidate(t, dir, "a.db", 72*time.Hour): false,
		writeCandidate(t, dir, "b.db", 48*time.Hour): false,
		writeCandidate(t, dir, "c.db", 24*time.Hour): false,
	}
	// A sidecar is newer than every database and is still not a candidate:
	// the magic predicate runs before the policy, which is what keeps a
	// random draw off a file that is not a backup.
	stray := writeArtifact(t, dir, "checksums.txt", []byte("sha256 sums\n"))
	age(t, stray, time.Hour)

	for range 200 {
		got := chooseIn(t, dir, selectRandom)
		if got == stray {
			t.Fatalf("a random draw reached %s, which holds no SQLite magic", filepath.Base(stray))
		}
		if _, ok := wanted[got]; !ok {
			t.Fatalf("a random draw reached %s, which is not in the directory", got)
		}
		wanted[got] = true
	}
	for path, seen := range wanted {
		if !seen {
			t.Errorf("200 draws never reached %s: a scheduled drill would not cover the window",
				filepath.Base(path))
		}
	}
}

// TestResolveSourceHonoursTheSelection walks the parameter the whole way
// in, through every kind that chooses a backup.
func TestResolveSourceHonoursTheSelection(t *testing.T) {
	t.Run("sqlite_db_dir", func(t *testing.T) {
		dir := t.TempDir()
		oldest := writeCandidate(t, dir, "z-oldest.db", 48*time.Hour)
		writeCandidate(t, dir, "a-newest.db", 24*time.Hour)

		src, perr := resolveSource(context.Background(), "sqlite_db_dir", dir,
			map[string]string{selectParam: "oldest"})
		if perr != nil {
			t.Fatalf("resolveSource: %s: %s", perr.Code, perr.Message)
		}
		if src.path != oldest {
			t.Errorf("restored %s, want %s", filepath.Base(src.path), filepath.Base(oldest))
		}
	})

	t.Run("sqlite_dump_dir", func(t *testing.T) {
		dir := t.TempDir()
		oldest := writeArtifact(t, dir, "z-oldest.sql", dumpFixture())
		age(t, oldest, 48*time.Hour)
		newest := writeArtifact(t, dir, "a-newest.sql", dumpFixture())
		age(t, newest, 24*time.Hour)

		src, perr := resolveSource(context.Background(), "sqlite_dump_dir", dir,
			map[string]string{selectParam: "oldest"})
		if perr != nil {
			t.Fatalf("resolveSource: %s: %s", perr.Code, perr.Message)
		}
		if src.path != oldest {
			t.Errorf("restored %s, want %s", filepath.Base(src.path), filepath.Base(oldest))
		}
	})
}

// TestResolveSourceRefusesAnImpossibleSelection proves the refusal is not
// only reachable from backupSelection's own test: a drill config asking a
// kind that chooses nothing to choose fails before any byte is read.
func TestResolveSourceRefusesAnImpossibleSelection(t *testing.T) {
	dir := t.TempDir()
	path := writeCandidate(t, dir, "one.db", 24*time.Hour)

	_, perr := resolveSource(context.Background(), "sqlite_db", path,
		map[string]string{selectParam: "oldest"})
	if perr == nil {
		t.Fatal("accepted a selection policy on a kind that chooses nothing")
	}
	if perr.Code != "invalid_request" {
		t.Errorf("code = %q, want invalid_request", perr.Code)
	}
}
