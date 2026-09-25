package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers the policy that decides which backup directory
// solr_backup_dir restores.
//
// A candidate here is a subdirectory, as it is in the cassandra and
// prometheus adapters, but unlike those it is ranked by directory time
// rather than by anything it states about itself (selection.go says why).
// So these fixtures set file times deliberately, and they backdate the
// whole tree: the settle check measures the newest mtime anywhere under a
// candidate, and it applies under every policy.

// backdate sets one instant on every path in a candidate tree, so the
// candidate is both ranked where the test wants it and already settled.
func backdate(t *testing.T, root string, when time.Time) string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		paths = append(paths, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Deepest first: setting a directory's time before its children would
	// be undone by writing to them.
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], when, when); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// writeCandidate moves one backup fixture into the directory under test and
// dates it.
func writeCandidate(t *testing.T, base, name string, when time.Time) string {
	t.Helper()
	path := filepath.Join(base, name)
	if err := os.Rename(writeBackup(t, nil), path); err != nil {
		t.Fatal(err)
	}
	return backdate(t, path, when)
}

// selectionWindow builds a directory of three candidates whose names
// deliberately run against their times, so a ranking that had quietly
// become a name sort would fail every test built on it.
func selectionWindow(t *testing.T) (base, oldest, middle, newest string) {
	t.Helper()
	base = t.TempDir()
	now := time.Now()
	newest = writeCandidate(t, base, "a-saturday", now.Add(-24*time.Hour))
	middle = writeCandidate(t, base, "m-friday", now.Add(-48*time.Hour))
	oldest = writeCandidate(t, base, "z-thursday", now.Add(-72*time.Hour))
	return base, oldest, middle, newest
}

func TestBackupSelectionAccepts(t *testing.T) {
	if policy, perr := backupSelection("solr_backup_dir", nil); perr != nil || policy != selectNewest {
		t.Fatalf("policy = %q, %+v; want newest where nothing was declared", policy, perr)
	}
	for _, raw := range []string{"newest", "oldest", "random"} {
		policy, perr := backupSelection("solr_backup_dir", map[string]string{selectParam: raw})
		if perr != nil || string(policy) != raw {
			t.Errorf("select=%s: policy = %q, %+v", raw, policy, perr)
		}
	}
	// An absent parameter is fine on every kind, selecting or not: the
	// default is exactly what those kinds already did.
	for _, kind := range []string{"solr_backup", "solr_backup_tar"} {
		if _, perr := backupSelection(kind, map[string]string{"backup_timezone": "UTC"}); perr != nil {
			t.Errorf("%s with no select declared: %+v", kind, perr)
		}
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("solr_backup_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		for _, kind := range []string{"solr_backup", "solr_backup_tar"} {
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
		got, perr := chooseBackupIn(context.Background(), base, tc.policy)
		if perr != nil {
			t.Fatalf("%s: %+v", tc.policy, perr)
		}
		if got != tc.want {
			t.Errorf("%s picked %s, want %s", tc.policy, filepath.Base(got), filepath.Base(tc.want))
		}
	}
}

// TestPrecedesMirrorsBeatsHere: with directory time as the only fact there
// is no datedness rule to preserve, so the oldest ordering really is the
// newest ordering turned around — unlike the cassandra and prometheus
// adapters, where one rule cannot invert.
func TestPrecedesMirrorsBeatsHere(t *testing.T) {
	early := dirCandidate{name: "a", mtime: time.Unix(1_000, 0)}
	late := dirCandidate{name: "z", mtime: time.Unix(2_000, 0)}
	if !late.beats(early) || !early.precedes(late) {
		t.Error("directory time does not order the two policies in opposite directions")
	}
	tieA := dirCandidate{name: "a", mtime: time.Unix(1_000, 0)}
	tieZ := dirCandidate{name: "z", mtime: time.Unix(1_000, 0)}
	if !tieZ.beats(tieA) || !tieA.precedes(tieZ) {
		t.Error("a tie does not break toward opposite names under the two policies")
	}
}

func TestSelectionTieBreaksDeterministically(t *testing.T) {
	base := t.TempDir()
	same := time.Now().Add(-48 * time.Hour)
	a := writeCandidate(t, base, "a-tie", same)
	z := writeCandidate(t, base, "z-tie", same)
	// One directory time between them, so only the name is left to decide —
	// and it must decide, or the answer follows whatever order the
	// filesystem happened to hand back.
	if got, _ := chooseBackupIn(context.Background(), base, selectNewest); got != z {
		t.Errorf("newest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(z))
	}
	if got, _ := chooseBackupIn(context.Background(), base, selectOldest); got != a {
		t.Errorf("oldest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(a))
	}
}

// TestRandomCoversTheWindow: every subdirectory is a candidate here. There
// is no fact to narrow a draw with — the adapters whose candidates date
// themselves keep a draw off an undatable sibling, and this one cannot — so
// what the test pins is that the draw reaches the whole window and that a
// stray file is still not a candidate.
func TestRandomCoversTheWindow(t *testing.T) {
	base, oldest, middle, newest := selectionWindow(t)
	if err := os.WriteFile(filepath.Join(base, "zz-SHA256SUMS"), []byte("sums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for range 200 {
		got, perr := chooseBackupIn(context.Background(), base, selectRandom)
		if perr != nil {
			t.Fatalf("chooseBackupIn: %+v", perr)
		}
		seen[got]++
	}
	for _, want := range []string{oldest, middle, newest} {
		if seen[want] == 0 {
			t.Errorf("200 draws never reached %s", filepath.Base(want))
		}
	}
	if len(seen) != 3 {
		t.Errorf("draws landed on %d candidates, want only the three backup directories", len(seen))
	}
}

// TestTheSettleCheckAppliesUnderEveryPolicy: the adapter still chose the
// backup, so a backup job writing into the chosen one must still refuse the
// drill — and must not fall back to a neighbour, which would prove a backup
// the record does not name. Here it is the *oldest* candidate in motion,
// which the default policy would never have looked at.
func TestTheSettleCheckAppliesUnderEveryPolicy(t *testing.T) {
	base := t.TempDir()
	writeCandidate(t, base, "a-newest", time.Now().Add(-24*time.Hour))
	inFlight := writeCandidate(t, base, "z-oldest", time.Now().Add(-72*time.Hour))
	// Only the file inside is fresh, so the candidate keeps the directory
	// time that makes it the oldest — and the settle check, which measures
	// the whole tree, sees a backup job at work in it.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(inFlight, fixtureCollection, "index", "segments_1"), now, now); err != nil {
		t.Fatal(err)
	}
	stop := keepAppending(t, inFlight)
	_, perr := chooseBackupIn(context.Background(), base, selectOldest)
	stop()

	if perr == nil || perr.Code != "source_unreadable" {
		t.Fatalf("perr = %+v, want the in-flight backup refused", perr)
	}
	if strings.Contains(perr.Message, "a-newest") {
		t.Error("the drill fell back to the other backup — that would prove one the record does not name")
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
		src, perr := resolveSource(context.Background(), "solr_backup_dir", base, tc.params)
		if perr != nil {
			t.Fatalf("%v: %+v", tc.params, perr)
		}
		if src.path != tc.want || src.collection != fixtureCollection {
			t.Errorf("%v resolved %s (%s), want %s", tc.params,
				filepath.Base(src.path), src.collection, filepath.Base(tc.want))
		}
	}
}

func TestResolveSourceRefusesAnImpossibleSelection(t *testing.T) {
	_, _, _, newest := selectionWindow(t)
	_, perr := resolveSource(context.Background(), "solr_backup", newest,
		map[string]string{selectParam: "oldest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want the selection refused on a kind that selects nothing", perr)
	}
}
