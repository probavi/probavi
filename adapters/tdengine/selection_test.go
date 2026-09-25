package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers the policy that decides which taosdump output
// taosdump_dir restores — and, with it, the ordering this slice fixed: the
// instant a dump records about itself and the modification time of the
// directory it was copied into are two different clocks, and are never
// compared against each other (selection.go).

// writeCandidate lays down one candidate dump recording when taosdump
// started, and copied in at the given age.
func writeCandidate(t *testing.T, base, name string, startedAgo, age time.Duration) string {
	t.Helper()
	return placeDump(t, base, name,
		dumpOptions{database: name, nested: true, startedAgo: startedAgo},
		time.Now().Add(-age))
}

// writeUndatedCandidate lays down a dump with no dump_result.txt, so
// nothing can be said about when it was taken.
func writeUndatedCandidate(t *testing.T, base, name string, age time.Duration) string {
	t.Helper()
	return placeDump(t, base, name,
		dumpOptions{database: name, nested: true},
		time.Now().Add(-age))
}

// selectionWindow builds a directory of three dated candidates whose names
// and directory times both deliberately run against their recorded
// instants, so a ranking that had quietly become either a name sort or an
// mtime sort would fail every test built on it.
func selectionWindow(t *testing.T) (base, oldest, middle, newest string) {
	t.Helper()
	base = t.TempDir()
	newest = writeCandidate(t, base, "asat", 24*time.Hour, 72*time.Hour)
	middle = writeCandidate(t, base, "mfri", 48*time.Hour, 48*time.Hour)
	oldest = writeCandidate(t, base, "zthu", 72*time.Hour, 24*time.Hour)
	return base, oldest, middle, newest
}

func TestBackupSelectionAccepts(t *testing.T) {
	if policy, perr := backupSelection("taosdump_dir", nil); perr != nil || policy != selectNewest {
		t.Fatalf("policy = %q, %+v; want newest where nothing was declared", policy, perr)
	}
	for _, raw := range []string{"newest", "oldest", "random"} {
		policy, perr := backupSelection("taosdump_dir", map[string]string{selectParam: raw})
		if perr != nil || string(policy) != raw {
			t.Errorf("select=%s: policy = %q, %+v", raw, policy, perr)
		}
	}
	// An absent parameter is fine on every kind, selecting or not: the
	// default is exactly what those kinds already did.
	for _, kind := range []string{"taosdump", "taosdump_tar"} {
		if _, perr := backupSelection(kind, map[string]string{"backup_timezone": "UTC"}); perr != nil {
			t.Errorf("%s with no select declared: %+v", kind, perr)
		}
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("taosdump_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		for _, kind := range []string{"taosdump", "taosdump_tar"} {
			_, perr := backupSelection(kind, map[string]string{selectParam: "oldest"})
			if perr == nil || perr.Code != "invalid_request" ||
				!strings.Contains(perr.Message, "restores what source.path names") {
				t.Fatalf("%s: perr = %+v, want the parameter refused rather than ignored", kind, perr)
			}
		}
	})
}

func TestSelectionPicksTheEndsOfTheWindow(t *testing.T) {
	base, oldest, _, newest := selectionWindow(t)
	for _, tc := range []struct {
		policy selectPolicy
		want   string
	}{
		{selectNewest, newest},
		{selectOldest, oldest},
	} {
		got, perr := chooseDumpIn(context.Background(), base, tc.policy)
		if perr != nil {
			t.Fatalf("%s: %+v", tc.policy, perr)
		}
		if got != tc.want {
			t.Errorf("%s picked %s, want %s", tc.policy, filepath.Base(got), filepath.Base(tc.want))
		}
	}
}

// TestOldestKeepsTheDatednessRule pins the one rule that does not invert:
// a dump recording its own instant outranks one recording none, under
// oldest exactly as under newest. An undated dump is not older — it has no
// age at all, and restoring it in preference to a dated one would prove
// less. This is also the rule the old ordering did not have.
func TestOldestKeepsTheDatednessRule(t *testing.T) {
	base := t.TempDir()
	dated := writeCandidate(t, base, "adated", time.Hour, 0)
	// The undated dump is older on disk too, so an ordering that fell
	// through to directory time would take it.
	writeUndatedCandidate(t, base, "zundated", 72*time.Hour)
	got, perr := chooseDumpIn(context.Background(), base, selectOldest)
	if perr != nil {
		t.Fatalf("chooseDumpIn: %+v", perr)
	}
	if got != dated {
		t.Errorf("oldest picked %s, want the dated dump %s",
			filepath.Base(got), filepath.Base(dated))
	}
}

// TestTheTwoClocksAreNeverCompared is the defect this slice fixed, stated
// as a test: a dump copied in this morning and recording nothing must not
// outrank the dump that records the newest instant. The old ranking put a
// directory mtime and a recorded instant into one comparison and got this
// backwards.
func TestTheTwoClocksAreNeverCompared(t *testing.T) {
	base := t.TempDir()
	// Recorded a day ago, but copied in two days ago.
	dated := writeCandidate(t, base, "adated", 24*time.Hour, 48*time.Hour)
	// Records nothing, and its directory is as fresh as it gets.
	writeUndatedCandidate(t, base, "zfreshcopy", 0)
	got, perr := chooseDumpIn(context.Background(), base, selectNewest)
	if perr != nil {
		t.Fatalf("chooseDumpIn: %+v", perr)
	}
	if got != dated {
		t.Errorf("newest picked %s, want the dump that records its own instant — a fresh copy "+
			"is not a fresh backup", filepath.Base(got))
	}
}

// TestUndatedDumpsOrderAmongThemselvesByDirectoryTime: directory time is
// still used, in the one place where it is the only fact there is.
func TestUndatedDumpsOrderAmongThemselvesByDirectoryTime(t *testing.T) {
	base := t.TempDir()
	older := writeUndatedCandidate(t, base, "zolder", 72*time.Hour)
	newer := writeUndatedCandidate(t, base, "anewer", 24*time.Hour)
	if got, _ := chooseDumpIn(context.Background(), base, selectNewest); got != newer {
		t.Errorf("newest picked %s, want %s", filepath.Base(got), filepath.Base(newer))
	}
	if got, _ := chooseDumpIn(context.Background(), base, selectOldest); got != older {
		t.Errorf("oldest picked %s, want %s", filepath.Base(got), filepath.Base(older))
	}
}

// TestPrecedesIsNotTheNegationOfBeats fails if the two orderings are ever
// folded into one. On the datedness rule both answer the same way, which
// is exactly what !beats could not do.
func TestPrecedesIsNotTheNegationOfBeats(t *testing.T) {
	at := time.Unix(1_786_000_000, 0)
	dated := dumpCandidate{name: "dated", dated: true, clock: at, mtime: at}
	undated := dumpCandidate{name: "undated", mtime: at}
	if !dated.beats(undated) {
		t.Error("beats: a dated dump must outrank an undated one")
	}
	if !dated.precedes(undated) {
		t.Error("precedes: a dated dump must outrank an undated one here too")
	}
	if dated.precedes(undated) == !dated.beats(undated) {
		t.Error("precedes has become the negation of beats; datedness is not a clock")
	}
}

func TestSelectionTieBreaksDeterministically(t *testing.T) {
	base := t.TempDir()
	a := writeCandidate(t, base, "atie", 48*time.Hour, 48*time.Hour)
	z := writeCandidate(t, base, "ztie", 48*time.Hour, 48*time.Hour)
	// One recorded instant and one directory time between them, so only the
	// name is left to decide — and it must decide, or the answer follows
	// whatever order the filesystem happened to hand back.
	same := time.Unix(1_786_000_000, 0)
	for _, p := range []string{a, z} {
		if err := os.Chtimes(p, same, same); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := chooseDumpIn(context.Background(), base, selectNewest); got != z {
		t.Errorf("newest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(z))
	}
	if got, _ := chooseDumpIn(context.Background(), base, selectOldest); got != a {
		t.Errorf("oldest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(a))
	}
}

func TestRandomCoversTheWindowAndNothingElse(t *testing.T) {
	base, oldest, middle, newest := selectionWindow(t)
	// A dump that records nothing about itself: an ordering could reach it
	// only if the directory held nothing else, and a draw must not reach it
	// at all.
	writeUndatedCandidate(t, base, "zundated", 0)
	seen := map[string]int{}
	for range 200 {
		got, perr := chooseDumpIn(context.Background(), base, selectRandom)
		if perr != nil {
			t.Fatalf("chooseDumpIn: %+v", perr)
		}
		seen[got]++
	}
	for _, want := range []string{oldest, middle, newest} {
		if seen[want] == 0 {
			t.Errorf("200 draws never reached %s", filepath.Base(want))
		}
	}
	if len(seen) != 3 {
		t.Errorf("draws landed on %d candidates, want only the three dated dumps", len(seen))
	}
}

// TestRandomFallsBackToWhatIsThere: narrowing a draw to the dated dumps
// must not narrow it to none of them.
func TestRandomFallsBackToWhatIsThere(t *testing.T) {
	base := t.TempDir()
	writeUndatedCandidate(t, base, "aone", 48*time.Hour)
	writeUndatedCandidate(t, base, "ztwo", 24*time.Hour)
	seen := map[string]int{}
	for range 100 {
		got, perr := chooseDumpIn(context.Background(), base, selectRandom)
		if perr != nil {
			t.Fatalf("chooseDumpIn: %+v", perr)
		}
		seen[got]++
	}
	if len(seen) != 2 {
		t.Errorf("draws landed on %d candidates, want both undated ones", len(seen))
	}
}

func TestResolveSourceHonoursTheSelection(t *testing.T) {
	base, oldest, _, newest := selectionWindow(t)
	for _, tc := range []struct {
		params map[string]string
		want   string
	}{
		{nil, newest},
		{map[string]string{selectParam: "newest"}, newest},
		{map[string]string{selectParam: "oldest"}, oldest},
	} {
		src, perr := resolveSource(context.Background(), "taosdump_dir", base, tc.params)
		if perr != nil {
			t.Fatalf("%v: %+v", tc.params, perr)
		}
		// resolveDump descends to the inner taosdump.<serial> directory,
		// so the artifact is under the candidate the policy chose.
		if !strings.HasPrefix(src.path, tc.want+string(filepath.Separator)) {
			t.Errorf("%v resolved %s, want something under %s", tc.params,
				src.path, filepath.Base(tc.want))
		}
	}
}

func TestResolveSourceRefusesAnImpossibleSelection(t *testing.T) {
	_, _, _, newest := selectionWindow(t)
	_, perr := resolveSource(context.Background(), "taosdump", newest,
		map[string]string{selectParam: "oldest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want the selection refused on a kind that selects nothing", perr)
	}
}
