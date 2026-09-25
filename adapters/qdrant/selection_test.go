package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers the policy that decides which backup
// qdrant_snapshot_dir restores. File time is the only fact this adapter has to
// order a directory by (selection.go), so the fixtures set file times
// deliberately — and stamp two candidates with one instant where a tie is
// the point, because two writes a moment apart differ by nanoseconds and
// would not be a tie at all.

// writeCandidate lays down one candidate snapshot with the checksum
// sidecar the engine writes beside it, backdated. The sidecar does not end
// in .snapshot, so it is never a candidate itself.
func writeCandidate(t *testing.T, base, name string, age time.Duration) string {
	t.Helper()
	body := []byte("a snapshot as Qdrant wrote it: " + name)
	path := writeSnapshot(t, base, name+snapshotSuffix, body)
	writeSidecar(t, path, digestOf(body))
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// chooseCandidate names chooseIn the way the shared tests below call it.
func chooseCandidate(dir string, policy selectPolicy) (string, *protoError) {
	return chooseIn(context.Background(), dir, policy)
}

// selectionWindow builds a directory of three candidates whose names
// deliberately run against their file times, so a ranking that had quietly
// become a name sort would fail every test built on it.
func selectionWindow(t *testing.T) (base, oldest, middle, newest string) {
	t.Helper()
	base = t.TempDir()
	newest = writeCandidate(t, base, "a-saturday", 24*time.Hour)
	middle = writeCandidate(t, base, "m-friday", 48*time.Hour)
	oldest = writeCandidate(t, base, "z-thursday", 72*time.Hour)
	return base, oldest, middle, newest
}

// stamp forces one instant on several paths, so a tie is a tie.
func stamp(t *testing.T, when time.Time, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupSelectionAccepts(t *testing.T) {
	for _, kind := range []string{"qdrant_snapshot_dir", "qdrant_full_snapshot_dir"} {
		if policy, perr := backupSelection(kind, nil); perr != nil || policy != selectNewest {
			t.Fatalf("%s: policy = %q, %+v; want newest where nothing was declared", kind, policy, perr)
		}
		for _, raw := range []string{"newest", "oldest", "random"} {
			policy, perr := backupSelection(kind, map[string]string{selectParam: raw})
			if perr != nil || string(policy) != raw {
				t.Errorf("%s select=%s: policy = %q, %+v", kind, raw, policy, perr)
			}
		}
	}
	// An absent parameter is fine on every kind, selecting or not: the
	// default is exactly what those kinds already did.
	for _, kind := range []string{"qdrant_snapshot", "qdrant_full_snapshot"} {
		if _, perr := backupSelection(kind, map[string]string{"backup_timezone": "UTC"}); perr != nil {
			t.Errorf("%s with no select declared: %+v", kind, perr)
		}
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("qdrant_snapshot_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		for _, kind := range []string{"qdrant_snapshot", "qdrant_full_snapshot"} {
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
		got, perr := chooseCandidate(base, tc.policy)
		if perr != nil {
			t.Fatalf("%s: %+v", tc.policy, perr)
		}
		if got != tc.want {
			t.Errorf("%s picked %s, want %s", tc.policy, filepath.Base(got), filepath.Base(tc.want))
		}
	}
}

// TestPrecedesMirrorsBeatsHere: with file time as the only fact there is
// no datedness rule to preserve, so the oldest ordering really is the
// newest ordering turned around — unlike the postgres and cassandra
// adapters, where one rule cannot invert.
func TestPrecedesMirrorsBeatsHere(t *testing.T) {
	early := dirCandidate{name: "a", mtime: time.Unix(1_000, 0)}
	late := dirCandidate{name: "z", mtime: time.Unix(2_000, 0)}
	if !late.beats(early) || !early.precedes(late) {
		t.Error("file time does not order the two policies in opposite directions")
	}
	tieA := dirCandidate{name: "a", mtime: time.Unix(1_000, 0)}
	tieZ := dirCandidate{name: "z", mtime: time.Unix(1_000, 0)}
	if !tieZ.beats(tieA) || !tieA.precedes(tieZ) {
		t.Error("a tie does not break toward opposite names under the two policies")
	}
}

func TestSelectionTieBreaksDeterministically(t *testing.T) {
	base := t.TempDir()
	a := writeCandidate(t, base, "a-tie", 48*time.Hour)
	z := writeCandidate(t, base, "z-tie", 48*time.Hour)
	// One file time between them, so only the name is left to decide — and
	// it must decide, or the answer follows whatever order the filesystem
	// happened to hand back.
	stamp(t, time.Now().Add(-48*time.Hour), a, z)
	if got, _ := chooseCandidate(base, selectNewest); got != z {
		t.Errorf("newest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(z))
	}
	if got, _ := chooseCandidate(base, selectOldest); got != a {
		t.Errorf("oldest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(a))
	}
}

// TestRandomCoversTheWindow: every artifact of this kind is a candidate
// here. There is no fact to narrow a draw with — the adapters whose
// candidates date themselves keep a draw off an undatable sibling, and
// this one cannot — so what the test pins is that the draw reaches the
// whole window, and that the checksum sidecar Qdrant leaves beside every snapshot is still not a candidate.
func TestRandomCoversTheWindow(t *testing.T) {
	base, oldest, middle, newest := selectionWindow(t)
	seen := map[string]int{}
	for range 200 {
		got, perr := chooseCandidate(base, selectRandom)
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

// TestTheSettleCheckAppliesUnderEveryPolicy: the adapter still chose the
// artifact, so a backup job writing into the chosen one must still refuse
// the drill — and must not fall back to a neighbour, which would prove a
// backup the record does not name.
func TestTheSettleCheckAppliesUnderEveryPolicy(t *testing.T) {
	base := t.TempDir()
	inFlight := writeCandidate(t, base, "a-inflight", 0)
	other := writeCandidate(t, base, "z-other", 0)
	// A backup job keeps touching the file it is writing, so the only way
	// to keep that file the *oldest* candidate is to put the neighbour
	// ahead of the clock. What is being pinned here is the policy's effect,
	// not the timestamps.
	stamp(t, time.Now().Add(time.Hour), other)
	stop := keepAppending(t, inFlight)
	_, perr := chooseCandidate(base, selectOldest)
	stop()

	if perr == nil || perr.Code != "source_unreadable" {
		t.Fatalf("perr = %+v, want the in-flight backup refused", perr)
	}
	if strings.Contains(perr.Message, "z-other") {
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
		src, perr := resolveSource(context.Background(), "qdrant_snapshot_dir", base, tc.params)
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
	_, perr := resolveSource(context.Background(), "qdrant_snapshot", newest,
		map[string]string{selectParam: "oldest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want the selection refused on a kind that selects nothing", perr)
	}
}
