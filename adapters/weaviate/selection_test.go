package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers the policy that decides which backup
// weaviate_backup_dir restores. A candidate here is a backup directory
// judged entirely by its own backup_config.json — status and completion
// instant — so no test leans on a file time to mean "newer".

const (
	windowOldest = "2026-09-14T09:00:00Z"
	windowMiddle = "2026-09-15T09:00:00Z"
	windowNewest = "2026-09-16T09:00:00Z"
)

// writeCandidate lays down one completed candidate backup claiming when it
// finished.
func writeCandidate(t *testing.T, base, name, completedAt string) string {
	t.Helper()
	return writeBackupFixture(t, base, name, backupSpec{
		startedAt: completedAt, completedAt: completedAt,
	})
}

// selectionWindow builds a directory of three completed candidates whose
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
	if policy, perr := backupSelection("weaviate_backup_dir", nil); perr != nil || policy != selectNewest {
		t.Fatalf("policy = %q, %+v; want newest where nothing was declared", policy, perr)
	}
	for _, raw := range []string{"newest", "oldest", "random"} {
		policy, perr := backupSelection("weaviate_backup_dir", map[string]string{selectParam: raw})
		if perr != nil || string(policy) != raw {
			t.Errorf("select=%s: policy = %q, %+v", raw, policy, perr)
		}
	}
	// An absent parameter is fine on every kind, selecting or not: the
	// default is exactly what those kinds already did.
	for _, kind := range []string{"weaviate_backup", "weaviate_backup_tar"} {
		if _, perr := backupSelection(kind, map[string]string{"backup_timezone": "UTC"}); perr != nil {
			t.Errorf("%s with no select declared: %+v", kind, perr)
		}
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("weaviate_backup_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		for _, kind := range []string{"weaviate_backup", "weaviate_backup_tar"} {
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

// TestPrecedesMirrorsBeatsHere: unlike the postgres and cassandra
// adapters, this one has no datedness rule to preserve — completedOnly has
// already dropped everything that states nothing — so the oldest ordering
// really is the newest ordering turned around, in both fields.
func TestPrecedesMirrorsBeatsHere(t *testing.T) {
	early := backupCandidate{name: "a", status: "SUCCESS", completed: time.Unix(1_000, 0)}
	late := backupCandidate{name: "z", status: "SUCCESS", completed: time.Unix(2_000, 0)}
	if !late.beats(early) || !early.precedes(late) {
		t.Error("the completion instant does not order the two policies in opposite directions")
	}
	tieA := backupCandidate{name: "a", status: "SUCCESS", completed: time.Unix(1_000, 0)}
	tieZ := backupCandidate{name: "z", status: "SUCCESS", completed: time.Unix(1_000, 0)}
	if !tieZ.beats(tieA) || !tieA.precedes(tieZ) {
		t.Error("a tie does not break toward opposite names under the two policies")
	}
}

func TestSelectionTieBreaksDeterministically(t *testing.T) {
	base := t.TempDir()
	writeCandidate(t, base, "a-tie", windowMiddle)
	writeCandidate(t, base, "z-tie", windowMiddle)
	// One completion instant between them, so only the name is left to
	// decide — and it must decide, or the answer follows whatever order the
	// filesystem happened to hand back.
	if got, _ := chooseBackupIn(base, selectNewest); filepath.Base(got) != "z-tie" {
		t.Errorf("newest broke the tie at %s, want z-tie", filepath.Base(got))
	}
	if got, _ := chooseBackupIn(base, selectOldest); filepath.Base(got) != "a-tie" {
		t.Errorf("oldest broke the tie at %s, want a-tie", filepath.Base(got))
	}
}

func TestRandomCoversTheWindowAndNothingElse(t *testing.T) {
	base, oldest, middle, newest := selectionWindow(t)
	// Two things a draw must never reach: an attempt that never completed,
	// and an entry that is not a backup at all.
	writeBackupFixture(t, base, "half-written", backupSpec{
		status: "TRANSFERRING", startedAt: windowOldest,
		completedAt: "0001-01-01T00:00:00Z",
	})
	if err := os.MkdirAll(filepath.Join(base, "not-a-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for range 200 {
		got, perr := chooseBackupIn(base, selectRandom)
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
		t.Errorf("draws landed on %d candidates, want only the three completed backups", len(seen))
	}
}

// TestNewestStillRefusesANewerAttempt keeps the refusal that guards the
// default policy: proving an older backup while a newer attempt sits
// unfinished beside it would let the record imply something the operator
// does not have.
func TestNewestStillRefusesANewerAttempt(t *testing.T) {
	base := t.TempDir()
	writeCandidate(t, base, "good", windowOldest)
	writeBackupFixture(t, base, "broken", backupSpec{
		status: "FAILED", startedAt: windowNewest, completedAt: "0001-01-01T00:00:00Z",
	})
	_, perr := chooseBackupIn(base, selectNewest)
	if perr == nil || perr.Code != "source_corrupt" || !strings.Contains(perr.Message, "broken") {
		t.Fatalf("perr = %+v, want the newer failed attempt refused by name", perr)
	}
}

// TestOldestAndRandomIgnoreANewerAttempt records the one place in this
// rollout where a policy changed more than an ordering. The operator has
// named which end of the retention window the drill is about and the record
// names the artifact it proved, so a newer failed attempt is nothing the
// result could be read as claiming — and keeping the refusal would let one
// failed backup job make the far end of the window undrillable, which is
// the only thing oldest exists to reach.
func TestOldestAndRandomIgnoreANewerAttempt(t *testing.T) {
	base := t.TempDir()
	wanted := writeCandidate(t, base, "good", windowOldest)
	writeBackupFixture(t, base, "broken", backupSpec{
		status: "FAILED", startedAt: windowNewest, completedAt: "0001-01-01T00:00:00Z",
	})
	for _, policy := range []selectPolicy{selectOldest, selectRandom} {
		got, perr := chooseBackupIn(base, policy)
		if perr != nil {
			t.Fatalf("%s: %+v", policy, perr)
		}
		if got != wanted {
			t.Errorf("%s picked %s, want the one completed backup", policy, filepath.Base(got))
		}
	}
}

// TestNoCompletedBackupRefusesUnderEveryPolicy: dropping the newer-attempt
// refusal for oldest and random must not turn into restoring nothing. With
// no completed backup at all there is no far end of the window to reach,
// and the census refusal still applies.
func TestNoCompletedBackupRefusesUnderEveryPolicy(t *testing.T) {
	base := t.TempDir()
	writeBackupFixture(t, base, "broken", backupSpec{
		status: "FAILED", startedAt: windowNewest, completedAt: "0001-01-01T00:00:00Z",
	})
	for _, policy := range []selectPolicy{selectNewest, selectOldest, selectRandom} {
		if _, perr := chooseBackupIn(base, policy); perr == nil {
			t.Errorf("%s accepted a directory holding no completed backup", policy)
		}
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
		src, perr := resolveSource("weaviate_backup_dir", base, tc.params)
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
	_, perr := resolveSource("weaviate_backup", newest, map[string]string{selectParam: "oldest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want the selection refused on a kind that selects nothing", perr)
	}
}
