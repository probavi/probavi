package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers which backup a directory source restores. The
// clocks here are the sentence mysqldump writes at the end of a dump, so
// "older" is a time the tool actually recorded (see timestamp.go).

// selectionBase is old enough that nothing written by these tests trips
// the settle window (settle.go).
var selectionBase = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

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
			kind: "mysqldump_dir", params: nil, want: selectNewest,
		},
		{name: "newest", kind: "mysqldump_dir", params: map[string]string{"select": "newest"}, want: selectNewest},
		{name: "oldest", kind: "mysqldump_dir", params: map[string]string{"select": "oldest"}, want: selectOldest},
		{name: "random", kind: "mysqldump_dir", params: map[string]string{"select": "random"}, want: selectRandom},
		{
			name: "the with_users kind takes it too, because it also chooses a member",
			kind: "mysqldump_with_users", params: map[string]string{"select": "oldest", "users": "u.sql"},
			want: selectOldest,
		},
		{
			name: "naming the dump outright on its own is unaffected",
			kind: "mysqldump_with_users", params: map[string]string{"users": "u.sql", "dump": "monday.sql"},
			want: selectNewest,
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
// something else, so every configuration this adapter cannot carry out
// fails rather than being quietly ignored.
func TestBackupSelectionRefuses(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		params map[string]string
		want   string // substring of the refusal
	}{
		{
			name: "an unknown policy is refused, not rounded to the default",
			kind: "mysqldump_dir", params: map[string]string{"select": "latest"},
			want: "must be newest, oldest or random",
		},
		{
			name: "capitalisation is not a policy either",
			kind: "mysqldump_dir", params: map[string]string{"select": "Oldest"},
			want: "Oldest is none of them",
		},
		{
			name: "a single-artifact kind selects nothing",
			kind: "mysqldump", params: map[string]string{"select": "oldest"},
			want: "kind mysqldump restores what source.path names",
		},
		{
			name: "a physical backup directory selects nothing either",
			kind: "xtrabackup", params: map[string]string{"select": "random"},
			want: "applies only to the kinds that choose a backup",
		},
		{
			name:   "naming the dump outright and asking for a selection are two requests",
			kind:   "mysqldump_with_users",
			params: map[string]string{"select": "oldest", "users": "u.sql", "dump": "monday.sql"},
			want:   "dump names monday.sql outright",
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
	// Written in an order matching neither clock, so a policy that read
	// the directory instead of the dumps would be visible.
	middle := writeDumpAs(t, dir, "b.sql", "2026-08-05 03:00:00", selectionBase.Add(-48*time.Hour))
	oldest := writeDumpAs(t, dir, "c.sql", "2026-08-01 03:00:00", selectionBase.Add(-24*time.Hour))
	newest := writeDumpAs(t, dir, "a.sql", "2026-08-09 03:00:00", selectionBase.Add(-72*time.Hour))

	for _, tt := range []struct {
		policy selectPolicy
		want   string
	}{
		{selectNewest, newest},
		{selectOldest, oldest},
	} {
		t.Run(string(tt.policy), func(t *testing.T) {
			got, perr := chooseBackupIn(context.Background(), dir, "", tt.policy)
			if perr != nil {
				t.Fatalf("chooseBackupIn: %+v", perr)
			}
			if got != tt.want {
				t.Errorf("picked %s, want %s — by the clock each dump records, not by file name or file time",
					filepath.Base(got), filepath.Base(tt.want))
			}
			if got == middle {
				t.Error("picked the middle of the window under an end-of-window policy")
			}
		})
	}
}

// TestOldestKeepsTheDatednessRule is where oldest is not the negation of
// newest. A dump taken with --skip-dump-date carries no time at all, and
// it is not "the oldest backup" — it is the one nothing is known about.
func TestOldestKeepsTheDatednessRule(t *testing.T) {
	dir := t.TempDir()
	// The undatable dump is the oldest thing in the directory by every
	// measure a filesystem has.
	writeDumpAs(t, dir, "a-undatable.sql", "", selectionBase.Add(-96*time.Hour))
	oldestDated := writeDumpAs(t, dir, "z-early.sql", "2026-08-01 03:00:00", selectionBase.Add(-24*time.Hour))
	writeDumpAs(t, dir, "z-late.sql", "2026-08-09 03:00:00", selectionBase.Add(-time.Hour))

	got, perr := chooseBackupIn(context.Background(), dir, "", selectOldest)
	if perr != nil {
		t.Fatalf("chooseBackupIn: %+v", perr)
	}
	if got != oldestDated {
		t.Errorf("picked %s, want %s — a dump that cannot be dated is not the oldest one",
			filepath.Base(got), filepath.Base(oldestDated))
	}
}

// TestOldestFallsBackTheSameWayNewestDoes: with nothing datable in the
// directory, the file-time rule still decides, in the direction asked for.
func TestOldestFallsBackTheSameWayNewestDoes(t *testing.T) {
	dir := t.TempDir()
	older := writeDumpAs(t, dir, "z-first.sql", "", selectionBase.Add(-48*time.Hour))
	newer := writeDumpAs(t, dir, "a-second.sql", "", selectionBase.Add(-time.Hour))

	for policy, want := range map[selectPolicy]string{selectOldest: older, selectNewest: newer} {
		got, perr := chooseBackupIn(context.Background(), dir, "", policy)
		if perr != nil {
			t.Fatalf("chooseBackupIn(%s): %+v", policy, perr)
		}
		if got != want {
			t.Errorf("%s picked %s, want %s", policy, filepath.Base(got), filepath.Base(want))
		}
	}
}

// TestPrecedesIsNotTheNegationOfBeats states the asymmetry directly, so a
// later simplification into !beats has something to fail against.
func TestPrecedesIsNotTheNegationOfBeats(t *testing.T) {
	dated := dirCandidate{name: "dated", clock: selectionBase, dated: true, mtime: selectionBase}
	undated := dirCandidate{name: "undated", mtime: selectionBase.Add(-72 * time.Hour)}

	if !dated.beats(undated) {
		t.Error("a dated dump must win under newest")
	}
	if !dated.precedes(undated) {
		t.Error("a dated dump must win under oldest too — datedness is not a clock")
	}
	if undated.precedes(dated) {
		t.Error("an undatable file older by file time took the oldest slot from a dated dump")
	}
}

// TestSelectionTieBreaksDeterministically keeps a choice from depending
// on the order readdir happens to return entries in — the same property
// the newest policy has always had, in the other direction.
func TestSelectionTieBreaksDeterministically(t *testing.T) {
	dir := t.TempDir()
	// Same recorded clock, same file time: only the name is left.
	writeDumpAs(t, dir, "b.sql", "2026-08-09 03:00:00", selectionBase.Add(-24*time.Hour))
	first := writeDumpAs(t, dir, "a.sql", "2026-08-09 03:00:00", selectionBase.Add(-24*time.Hour))

	got, perr := chooseBackupIn(context.Background(), dir, "", selectOldest)
	if perr != nil {
		t.Fatalf("chooseBackupIn: %+v", perr)
	}
	if got != first {
		t.Errorf("picked %s, want %s — ties break by name, and oldest takes the smaller one",
			filepath.Base(got), filepath.Base(first))
	}
}

// TestRandomCoversTheWindowAndNothingElse. Random is not reproducible and
// is not asked to be; what it must do is reach every backup over time and
// never reach for something that is not one. Three candidates over 200
// draws leave the probability that any single one is missed below 3·(2/3)²⁰⁰.
func TestRandomCoversTheWindowAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	stray := writeDumpAs(t, dir, "SHA256SUMS", "", selectionBase.Add(-time.Hour))
	wanted := map[string]bool{
		writeDumpAs(t, dir, "a.sql", "2026-08-01 03:00:00", selectionBase.Add(-72*time.Hour)): false,
		writeDumpAs(t, dir, "b.sql", "2026-08-05 03:00:00", selectionBase.Add(-48*time.Hour)): false,
		writeDumpAs(t, dir, "c.sql", "2026-08-09 03:00:00", selectionBase.Add(-24*time.Hour)): false,
	}

	for range 200 {
		got, perr := chooseBackupIn(context.Background(), dir, "", selectRandom)
		if perr != nil {
			t.Fatalf("chooseBackupIn: %+v", perr)
		}
		if got == stray {
			t.Fatalf("a random draw reached %s, which is not a backup — a directory collects "+
				"checksum listings and lock files, and an ordering would never have chosen one",
				filepath.Base(stray))
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

// TestRandomFallsBackToWhatIsThere: a directory of dumps none of which can
// be dated — every one taken with --skip-dump-date — still gets a draw
// rather than a refusal.
func TestRandomFallsBackToWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	writeDumpAs(t, dir, "monday.sql", "", selectionBase.Add(-48*time.Hour))
	writeDumpAs(t, dir, "tuesday.sql", "", selectionBase.Add(-24*time.Hour))

	for range 20 {
		got, perr := chooseBackupIn(context.Background(), dir, "", selectRandom)
		if perr != nil || got == "" {
			t.Fatalf("chooseBackupIn = %q (%+v), want one of the two undatable dumps", got, perr)
		}
	}
}

// TestResolveSourceHonoursTheSelection walks the parameter the whole way
// in, through the kind that takes a directory of dumps.
func TestResolveSourceHonoursTheSelection(t *testing.T) {
	dir := t.TempDir()
	oldest := writeDumpAs(t, dir, "z-oldest.sql", "2026-08-01 03:00:00", selectionBase.Add(-48*time.Hour))
	writeDumpAs(t, dir, "a-newest.sql", "2026-08-09 03:00:00", selectionBase.Add(-24*time.Hour))

	src, perr := resolveSource(context.Background(), "mysqldump_dir", dir,
		map[string]string{selectParam: "oldest"})
	if perr != nil {
		t.Fatalf("resolveSource: %s: %s", perr.Code, perr.Message)
	}
	if src.path != oldest {
		t.Errorf("restored %s, want %s", filepath.Base(src.path), filepath.Base(oldest))
	}
}

// TestResolveWithUsersHonoursTheSelection: the users script is never a
// candidate, whichever end of the window is asked for.
func TestResolveWithUsersHonoursTheSelection(t *testing.T) {
	dir := t.TempDir()
	writeDumpAs(t, dir, "users.sql", "", selectionBase.Add(-96*time.Hour))
	oldest := writeDumpAs(t, dir, "z-oldest.sql", "2026-08-01 03:00:00", selectionBase.Add(-48*time.Hour))
	writeDumpAs(t, dir, "a-newest.sql", "2026-08-09 03:00:00", selectionBase.Add(-24*time.Hour))

	src, perr := resolveSource(context.Background(), "mysqldump_with_users", dir,
		map[string]string{"users": "users.sql", selectParam: "oldest"})
	if perr != nil {
		t.Fatalf("resolveSource: %s: %s", perr.Code, perr.Message)
	}
	if src.path != oldest {
		t.Errorf("restored %s, want %s", filepath.Base(src.path), filepath.Base(oldest))
	}
	if filepath.Base(src.usersPath) != "users.sql" {
		t.Errorf("users script resolved to %s, want users.sql", src.usersPath)
	}
}

// TestResolveSourceRefusesAnImpossibleSelection proves the refusal is not
// only reachable from backupSelection's own test: a drill config asking a
// single-artifact kind to select fails before any byte is read.
func TestResolveSourceRefusesAnImpossibleSelection(t *testing.T) {
	dir := t.TempDir()
	path := writeDumpAs(t, dir, "orders.sql", "2026-08-01 03:00:00", selectionBase.Add(-24*time.Hour))

	_, perr := resolveSource(context.Background(), "mysqldump", path,
		map[string]string{selectParam: "oldest"})
	if perr == nil {
		t.Fatal("accepted a selection policy on a kind that selects nothing")
	}
	if perr.Code != "invalid_request" {
		t.Errorf("code = %q, want invalid_request", perr.Code)
	}
}
