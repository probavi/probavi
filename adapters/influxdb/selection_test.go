package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers the policy that decides which backup
// influx_backup_dir restores. A candidate here is dated from what it states
// about itself, so the fixtures state their instants and no test leans on
// a file time to mean "newer".

const (
	windowOldest = "20260814T090000Z"
	windowMiddle = "20260815T090000Z"
	windowNewest = "20260816T090000Z"
)

// writeCandidate lays down one candidate `influx backup` output whose
// manifest stem states when it was taken.
func writeCandidate(t *testing.T, base, name, stem string) string {
	t.Helper()
	return writeBackup(t, filepath.Join(base, name), stem, singleOrg())
}

// selectionWindow builds a directory of three candidates whose names
// deliberately run against their instants, so a ranking that had quietly
// become a name sort would fail every test built on it.
func selectionWindow(t *testing.T) (base, oldest, middle, newest string) {
	t.Helper()
	base = t.TempDir()
	newest = writeCandidate(t, base, "a-saturday", windowNewest)
	middle = writeCandidate(t, base, "m-friday", windowMiddle)
	oldest = writeCandidate(t, base, "z-thursday", windowOldest)
	return base, oldest, middle, newest
}

func TestBackupSelectionAccepts(t *testing.T) {
	if policy, perr := backupSelection("influx_backup_dir", nil); perr != nil || policy != selectNewest {
		t.Fatalf("policy = %q, %+v; want newest where nothing was declared", policy, perr)
	}
	for _, raw := range []string{"newest", "oldest", "random"} {
		policy, perr := backupSelection("influx_backup_dir", map[string]string{selectParam: raw})
		if perr != nil || string(policy) != raw {
			t.Errorf("select=%s: policy = %q, %+v", raw, policy, perr)
		}
	}
	// An absent parameter is fine on every kind, selecting or not: the
	// default is exactly what those kinds already did.
	for _, kind := range []string{"influx_backup", "influx_backup_tar"} {
		if _, perr := backupSelection(kind, map[string]string{"backup_timezone": "UTC"}); perr != nil {
			t.Errorf("%s with no select declared: %+v", kind, perr)
		}
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("influx_backup_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		for _, kind := range []string{"influx_backup", "influx_backup_tar"} {
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
		got, perr := chooseBackupIn(base, tc.policy)
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
	if got, _ := chooseBackupIn(base, selectNewest); got != z {
		t.Errorf("newest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(z))
	}
	if got, _ := chooseBackupIn(base, selectOldest); got != a {
		t.Errorf("oldest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(a))
	}
}

func TestRandomCoversTheWindow(t *testing.T) {
	base, oldest, middle, newest := selectionWindow(t)
	seen := map[string]int{}
	for range 200 {
		got, perr := chooseBackupIn(base, selectRandom)
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
		src, perr := resolveSource("influx_backup_dir", base, tc.params)
		if perr != nil {
			t.Fatalf("%v: %+v", tc.params, perr)
		}
		if src.dir != tc.want {
			t.Errorf("%v resolved %s, want %s", tc.params,
				filepath.Base(src.dir), filepath.Base(tc.want))
		}
	}
}

func TestResolveSourceRefusesAnImpossibleSelection(t *testing.T) {
	_, _, _, newest := selectionWindow(t)
	_, perr := resolveSource("influx_backup", newest, map[string]string{selectParam: "oldest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want the selection refused on a kind that selects nothing", perr)
	}
}

// TestPrecedesMirrorsBeatsHere: unlike the postgres and arangodb adapters,
// this one has no datedness rule to preserve — a subdirectory holding no
// timestamped manifest never became a candidate — so the oldest ordering
// really is the newest ordering turned around, in both fields.
func TestPrecedesMirrorsBeatsHere(t *testing.T) {
	early := backupCandidate{path: "/b/a", ts: time.Unix(1_000, 0)}
	late := backupCandidate{path: "/b/z", ts: time.Unix(2_000, 0)}
	if !late.beats(early) || !early.precedes(late) {
		t.Error("the stated instant does not order the two policies in opposite directions")
	}
	tieA := backupCandidate{path: "/b/a", ts: time.Unix(1_000, 0)}
	tieZ := backupCandidate{path: "/b/z", ts: time.Unix(1_000, 0)}
	if !tieZ.beats(tieA) || !tieA.precedes(tieZ) {
		t.Error("a tie does not break toward opposite names under the two policies")
	}
}

// TestASubdirectoryWithoutAManifestIsNoCandidate is what makes the mirror
// above safe: the thing that would be "undatable" is not in the running at
// all, under any policy, and the refusal counts it rather than dropping it
// silently.
func TestASubdirectoryWithoutAManifestIsNoCandidate(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "not-a-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, perr := chooseBackupIn(base, selectOldest)
	if perr == nil || perr.Code != "source_not_found" ||
		!strings.Contains(perr.Message, "1 subdirectories") {
		t.Fatalf("perr = %+v, want the passed-over subdirectory counted", perr)
	}
}
