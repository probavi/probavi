package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers which backup a directory source restores.
//
// The headers are the real ones timestamp_test.go collected from four
// server versions, so "older" here is a clock pg_dump actually wrote:
// archiveHeaders[0] records 21:26:26, [1] 21:26:42, [2] 21:26:45 and [3]
// 21:27:02 on 2026-08-09.

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
			kind: "pgdump_dir", params: nil, want: selectNewest,
		},
		{name: "newest", kind: "pgdump_dir", params: map[string]string{"select": "newest"}, want: selectNewest},
		{name: "oldest", kind: "pgdump_dir", params: map[string]string{"select": "oldest"}, want: selectOldest},
		{name: "random", kind: "pgdump_dir", params: map[string]string{"select": "random"}, want: selectRandom},
		{
			name: "the timescale directory kind takes it too",
			kind: "timescaledb_dump_dir", params: map[string]string{"select": "oldest"}, want: selectOldest,
		},
		{
			name: "so does a with_globals directory, which also chooses a member",
			kind: "pgdump_with_globals", params: map[string]string{"select": "oldest", "globals": "g.sql"},
			want: selectOldest,
		},
		{
			name: "naming the dump outright on its own is unaffected",
			kind: "pgdump_with_globals", params: map[string]string{"globals": "g.sql", "dump": "monday.dump"},
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
			kind: "pgdump_dir", params: map[string]string{"select": "latest"},
			want: "must be newest, oldest or random",
		},
		{
			name: "capitalisation is not a policy either",
			kind: "pgdump_dir", params: map[string]string{"select": "Oldest"},
			want: "Oldest is none of them",
		},
		{
			name: "a single-artifact kind selects nothing",
			kind: "pgdump", params: map[string]string{"select": "oldest"},
			want: "kind pgdump restores what source.path names",
		},
		{
			name: "a physical repository selects nothing either",
			kind: "pgbackrest", params: map[string]string{"select": "random", "stanza": "demo"},
			want: "applies only to the kinds that choose a backup",
		},
		{
			name: "barman likewise",
			kind: "barman", params: map[string]string{"select": "newest"},
			want: "applies only to the kinds that choose a backup",
		},
		{
			name:   "naming the dump outright and asking for a selection are two requests",
			kind:   "pgdump_with_globals",
			params: map[string]string{"select": "oldest", "globals": "g.sql", "dump": "monday.dump"},
			want:   "dump names monday.dump outright",
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

// writeGeneration writes one dated archive into dir under a name and a
// modification time of its own.
func writeGeneration(t *testing.T, dir, name string, header int, age time.Duration) string {
	t.Helper()
	return writeArchiveAs(t, dir, name, archiveHeaders[header].head, selectionBase.Add(-age))
}

// TestSelectionPicksTheEndsOfTheWindow is the item's point: a directory
// holding a retention window can be drilled at either end, and the oldest
// backup is the one an incident reaches for once the damage turns out to
// predate yesterday.
func TestSelectionPicksTheEndsOfTheWindow(t *testing.T) {
	dir := t.TempDir()
	// Deliberately written in an order matching neither clock, so a policy
	// that read the directory instead of the backups would be visible.
	middle := writeGeneration(t, dir, "b.dump", 1, 48*time.Hour)
	oldest := writeGeneration(t, dir, "c.dump", 0, 24*time.Hour)
	newest := writeGeneration(t, dir, "a.dump", 3, 72*time.Hour)

	for _, tt := range []struct {
		policy selectPolicy
		want   string
	}{
		{selectNewest, newest},
		{selectOldest, oldest},
	} {
		t.Run(string(tt.policy), func(t *testing.T) {
			got, perr := chooseBackupIn(dir, "", tt.policy)
			if perr != nil {
				t.Fatalf("chooseBackupIn: %+v", perr)
			}
			if got != tt.want {
				t.Errorf("picked %s, want %s — by the clock each backup records, not by file name or file time",
					filepath.Base(got), filepath.Base(tt.want))
			}
			if got == middle {
				t.Error("picked the middle of the window under an end-of-window policy")
			}
		})
	}
}

// TestOldestKeepsTheDatednessRule is where oldest is not the negation of
// newest. A file the adapter cannot date is not "the oldest backup" — it
// is the one nothing is known about, and restoring it because it looks
// undated would turn a policy into an accident.
func TestOldestKeepsTheDatednessRule(t *testing.T) {
	dir := t.TempDir()
	// The undatable file is the oldest thing in the directory by every
	// measure a filesystem has.
	touch(t, dir, "a-undatable.sql", selectionBase.Add(-96*time.Hour))
	oldestDated := writeGeneration(t, dir, "z-early.dump", 0, 24*time.Hour)
	writeGeneration(t, dir, "z-late.dump", 3, time.Hour)

	got, perr := chooseBackupIn(dir, "", selectOldest)
	if perr != nil {
		t.Fatalf("chooseBackupIn: %+v", perr)
	}
	if got != oldestDated {
		t.Errorf("picked %s, want %s — a backup that cannot be dated is not the oldest one",
			filepath.Base(got), filepath.Base(oldestDated))
	}
}

// TestOldestFallsBackTheSameWayNewestDoes: with nothing datable in the
// directory, the file-time rule still decides, in the direction asked for.
func TestOldestFallsBackTheSameWayNewestDoes(t *testing.T) {
	dir := t.TempDir()
	older := touch(t, dir, "z-first.sql", selectionBase.Add(-48*time.Hour))
	newer := touch(t, dir, "a-second.sql", selectionBase.Add(-time.Hour))

	for policy, want := range map[selectPolicy]string{selectOldest: older, selectNewest: newer} {
		got, perr := chooseBackupIn(dir, "", policy)
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
		t.Error("a dated backup must win under newest")
	}
	if !dated.precedes(undated) {
		t.Error("a dated backup must win under oldest too — datedness is not a clock")
	}
	if undated.precedes(dated) {
		t.Error("an undatable file older by file time took the oldest slot from a dated backup")
	}
}

// TestSelectionTieBreaksDeterministically keeps a choice from depending
// on the order readdir happens to return entries in — the same property
// the newest policy has always had, in the other direction.
func TestSelectionTieBreaksDeterministically(t *testing.T) {
	dir := t.TempDir()
	// Same recorded clock, same file time: only the name is left.
	writeGeneration(t, dir, "b.dump", 2, 24*time.Hour)
	first := writeGeneration(t, dir, "a.dump", 2, 24*time.Hour)

	got, perr := chooseBackupIn(dir, "", selectOldest)
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
	stray := touch(t, dir, "SHA256SUMS", selectionBase.Add(-time.Hour))
	wanted := map[string]bool{
		writeGeneration(t, dir, "a.dump", 0, 72*time.Hour): false,
		writeGeneration(t, dir, "b.dump", 1, 48*time.Hour): false,
		writeGeneration(t, dir, "c.dump", 3, 24*time.Hour): false,
	}

	for range 200 {
		got, perr := chooseBackupIn(dir, "", selectRandom)
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

// TestRandomFallsBackToWhatIsThere: a directory of dumps none of which
// can be dated — plain-SQL dumps taken without --verbose, which is the
// ordinary case — still gets a draw rather than a refusal.
func TestRandomFallsBackToWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "monday.sql", selectionBase.Add(-48*time.Hour))
	touch(t, dir, "tuesday.sql", selectionBase.Add(-24*time.Hour))

	for range 20 {
		got, perr := chooseBackupIn(dir, "", selectRandom)
		if perr != nil || got == "" {
			t.Fatalf("chooseBackupIn = %q (%+v), want one of the two undatable dumps", got, perr)
		}
	}
}

// TestResolveSourceHonoursTheSelection walks the parameter the whole way
// in, through the two kinds that take a directory of dumps.
func TestResolveSourceHonoursTheSelection(t *testing.T) {
	for _, kind := range []string{"pgdump_dir", "timescaledb_dump_dir"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			oldest := writeGeneration(t, dir, "z-oldest.dump", 0, 48*time.Hour)
			writeGeneration(t, dir, "a-newest.dump", 3, 24*time.Hour)

			src, perr := resolveSource(context.Background(), kind, dir,
				map[string]string{selectParam: "oldest"})
			if perr != nil {
				t.Fatalf("resolveSource: %s: %s", perr.Code, perr.Message)
			}
			if src.path != oldest {
				t.Errorf("restored %s, want %s", filepath.Base(src.path), filepath.Base(oldest))
			}
			if kind == "timescaledb_dump_dir" && !src.timescale {
				t.Error("the timescale framing was lost on the way through the selection")
			}
		})
	}
}

// TestResolveWithGlobalsHonoursTheSelection: the globals script is never a
// candidate, whichever end of the window is asked for.
func TestResolveWithGlobalsHonoursTheSelection(t *testing.T) {
	dir := t.TempDir()
	// The globals script is the oldest thing here and undatable, which is
	// what a pg_dumpall --globals-only script really is.
	touch(t, dir, "globals.sql", selectionBase.Add(-96*time.Hour))
	oldest := writeGeneration(t, dir, "z-oldest.dump", 0, 48*time.Hour)

	src, perr := resolveSource(context.Background(), "pgdump_with_globals", dir,
		map[string]string{"globals": "globals.sql", selectParam: "oldest"})
	if perr != nil {
		t.Fatalf("resolveSource: %s: %s", perr.Code, perr.Message)
	}
	if src.path != oldest {
		t.Errorf("restored %s, want %s", filepath.Base(src.path), filepath.Base(oldest))
	}
	if filepath.Base(src.globalsPath) != "globals.sql" {
		t.Errorf("globals resolved to %s, want globals.sql", src.globalsPath)
	}
}

// TestResolveSourceRefusesAnImpossibleSelection proves the refusal is not
// only reachable from backupSelection's own test: a drill config asking a
// single-artifact kind to select fails before any byte is read.
func TestResolveSourceRefusesAnImpossibleSelection(t *testing.T) {
	dir := t.TempDir()
	path := writeGeneration(t, dir, "orders.dump", 0, 24*time.Hour)

	_, perr := resolveSource(context.Background(), "pgdump", path,
		map[string]string{selectParam: "oldest"})
	if perr == nil {
		t.Fatal("accepted a selection policy on a kind that selects nothing")
	}
	if perr.Code != "invalid_request" {
		t.Errorf("code = %q, want invalid_request", perr.Code)
	}
}
