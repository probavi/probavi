package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers the policy that decides which backup
// arangodb_dump_dir restores. A candidate here is dated from what it states
// about itself, so the fixtures state their instants and no test leans on
// a file time to mean "newer".

const (
	windowOldest = "2026-08-14T09:00:00Z"
	windowMiddle = "2026-08-15T09:00:00Z"
	windowNewest = "2026-08-16T09:00:00Z"
)

// writeCandidate lays down one candidate dump claiming when it was taken.
func writeCandidate(t *testing.T, base, name, createdAt string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	writeDump(t, dir, dumpFixture{
		database: "shop", createdAt: createdAt, collections: []string{"orders"},
	})
	return dir
}

// writeUndatedCandidate lays down a dump whose manifest is missing, so
// nothing can be said about when it was taken.
func writeUndatedCandidate(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	writeDump(t, dir, dumpFixture{
		database: "shop", collections: []string{"orders"}, omitManifest: true,
	})
	return dir
}

// selectionWindow builds a directory of three datable candidates whose
// names deliberately run against their instants, so a ranking that had
// quietly become a name sort would fail every test built on it.
func selectionWindow(t *testing.T) (base, oldest, middle, newest string) {
	t.Helper()
	base = t.TempDir()
	newest = writeCandidate(t, base, "a-saturday", windowNewest)
	middle = writeCandidate(t, base, "m-friday", windowMiddle)
	oldest = writeCandidate(t, base, "z-thursday", windowOldest)
	return base, oldest, middle, newest
}

func TestBackupSelectionAccepts(t *testing.T) {
	if policy, perr := backupSelection("arangodb_dump_dir", nil); perr != nil || policy != selectNewest {
		t.Fatalf("policy = %q, %+v; want newest where nothing was declared", policy, perr)
	}
	for _, raw := range []string{"newest", "oldest", "random"} {
		policy, perr := backupSelection("arangodb_dump_dir", map[string]string{selectParam: raw})
		if perr != nil || string(policy) != raw {
			t.Errorf("select=%s: policy = %q, %+v", raw, policy, perr)
		}
	}
	// An absent parameter is fine on every kind, selecting or not: the
	// default is exactly what those kinds already did.
	for _, kind := range []string{"arangodb_dump", "arangodb_dump_tar"} {
		if _, perr := backupSelection(kind, map[string]string{"backup_timezone": "UTC"}); perr != nil {
			t.Errorf("%s with no select declared: %+v", kind, perr)
		}
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("arangodb_dump_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		for _, kind := range []string{"arangodb_dump", "arangodb_dump_tar"} {
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
		got, perr := chooseDumpIn(base, tc.policy)
		if perr != nil {
			t.Fatalf("%s: %+v", tc.policy, perr)
		}
		if got != tc.want {
			t.Errorf("%s picked %s, want %s", tc.policy, filepath.Base(got), filepath.Base(tc.want))
		}
	}
}

func TestSelectionTieBreaksDeterministically(t *testing.T) {
	base := t.TempDir()
	a := writeCandidate(t, base, "a-tie", windowMiddle)
	z := writeCandidate(t, base, "z-tie", windowMiddle)
	// One claimed instant and one file time between them, so only the name
	// is left to decide — and it must decide, or the answer follows
	// whatever order the filesystem happened to hand back.
	same := time.Unix(1_786_000_000, 0)
	for _, p := range []string{a, z} {
		if err := os.Chtimes(p, same, same); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := chooseDumpIn(base, selectNewest); got != z {
		t.Errorf("newest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(z))
	}
	if got, _ := chooseDumpIn(base, selectOldest); got != a {
		t.Errorf("oldest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(a))
	}
}

func TestRandomCoversTheWindow(t *testing.T) {
	base, oldest, middle, newest := selectionWindow(t)
	// A stray sibling that states nothing about itself. An ordering could
	// reach it only if the directory held nothing else; a draw must not
	// reach it at all.
	writeUndatedCandidate(t, base, "unpacked-scratch")
	seen := map[string]int{}
	for range 200 {
		got, perr := chooseDumpIn(base, selectRandom)
		if perr != nil {
			t.Fatalf("choosing at random: %+v", perr)
		}
		seen[got]++
	}
	for _, want := range []string{oldest, middle, newest} {
		if seen[want] == 0 {
			t.Errorf("200 draws never reached %s", filepath.Base(want))
		}
	}
	if len(seen) != 3 {
		t.Errorf("draws landed on %d candidates, want only the three in the window", len(seen))
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
		src, perr := resolveSource("arangodb_dump_dir", base, tc.params)
		if perr != nil {
			t.Fatalf("%v: %+v", tc.params, perr)
		}
		if src.path != tc.want {
			t.Errorf("%v resolved %s, want %s", tc.params,
				filepath.Base(src.path), filepath.Base(tc.want))
		}
	}
}

func TestResolveSourceRefusesAnImpossibleSelection(t *testing.T) {
	_, _, _, newest := selectionWindow(t)
	_, perr := resolveSource("arangodb_dump", newest, map[string]string{selectParam: "oldest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want the selection refused on a kind that selects nothing", perr)
	}
}

// TestOldestKeepsTheDatednessRule pins the one rule that does not invert:
// a candidate stating its own instant outranks one stating nothing, under
// oldest exactly as under newest. An undated candidate is not older — it
// has no age at all, and restoring it in preference to a dated one would
// prove less.
func TestOldestKeepsTheDatednessRule(t *testing.T) {
	base := t.TempDir()
	dated := writeCandidate(t, base, "a-dated", windowNewest)
	undated := writeUndatedCandidate(t, base, "z-undated")
	// The undated candidate is older on disk too, so a rule that fell
	// through to file time would take it.
	past := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(undated, past, past); err != nil {
		t.Fatal(err)
	}
	got, perr := chooseDumpIn(base, selectOldest)
	if perr != nil {
		t.Fatalf("choosing the oldest: %+v", perr)
	}
	if got != dated {
		t.Errorf("oldest picked %s, want the dated candidate %s",
			filepath.Base(got), filepath.Base(dated))
	}
}

// TestPrecedesIsNotTheNegationOfBeats fails if the two orderings are ever
// folded into one. On the datedness rule both answer the same way, which
// is exactly what !beats could not do.
func TestPrecedesIsNotTheNegationOfBeats(t *testing.T) {
	dated, undated := dumpCandidate{name: "dated", createdMs: 1, mtime: time.Unix(1_786_000_000, 0)},
		dumpCandidate{name: "undated", mtime: time.Unix(1_786_000_000, 0)}
	if !dated.beats(undated) {
		t.Error("beats: a dated candidate must outrank an undated one")
	}
	if !dated.precedes(undated) {
		t.Error("precedes: a dated candidate must outrank an undated one here too")
	}
	if dated.precedes(undated) == !dated.beats(undated) {
		t.Error("precedes has become the negation of beats; datedness is not a clock")
	}
}
