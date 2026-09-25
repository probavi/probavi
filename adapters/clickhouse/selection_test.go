package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers the policy that decides which archive
// clickhouse_backup_dir restores. A candidate is dated by the backup time
// its own manifest records, so the fixtures state their instants and their
// file times deliberately run the other way.

const (
	windowOldest = "2026-08-14 02:00:00"
	windowMiddle = "2026-08-15 02:00:00"
	windowNewest = "2026-08-16 02:00:00"
)

// writeCandidate lays down one candidate archive claiming when the backup
// was taken, and copied in at the given age.
func writeCandidate(t *testing.T, base, name, wallClock string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(base, name)
	writeArchive(t, path, wallClock)
	backdate(t, path, age)
	return path
}

// selectionWindow builds a directory of three archives whose names and
// file times both deliberately run against their manifest timestamps, so a
// ranking that had quietly become either a name sort or an mtime sort
// would fail every test built on it.
func selectionWindow(t *testing.T) (base, oldest, middle, newest string) {
	t.Helper()
	base = t.TempDir()
	newest = writeCandidate(t, base, "a-saturday.zip", windowNewest, 72*time.Hour)
	middle = writeCandidate(t, base, "m-friday.zip", windowMiddle, 48*time.Hour)
	oldest = writeCandidate(t, base, "z-thursday.zip", windowOldest, 0)
	return base, oldest, middle, newest
}

func TestBackupSelectionAccepts(t *testing.T) {
	if policy, perr := backupSelection("clickhouse_backup_dir", nil); perr != nil || policy != selectNewest {
		t.Fatalf("policy = %q, %+v; want newest where nothing was declared", policy, perr)
	}
	for _, raw := range []string{"newest", "oldest", "random"} {
		policy, perr := backupSelection("clickhouse_backup_dir", map[string]string{selectParam: raw})
		if perr != nil || string(policy) != raw {
			t.Errorf("select=%s: policy = %q, %+v", raw, policy, perr)
		}
	}
	// An absent parameter is fine on every kind, selecting or not: the
	// default is exactly what that kind already did.
	if _, perr := backupSelection("clickhouse_backup", map[string]string{"backup_timezone": "UTC"}); perr != nil {
		t.Errorf("clickhouse_backup with no select declared: %+v", perr)
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("clickhouse_backup_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		_, perr := backupSelection("clickhouse_backup", map[string]string{selectParam: "oldest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "restores what source.path names") {
			t.Fatalf("perr = %+v, want the parameter refused rather than ignored", perr)
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

// TestPrecedesMirrorsBeatsHere: unlike the postgres and cassandra
// adapters, this one has no datedness rule to preserve — an archive whose
// manifest cannot be read is never ranked at all — so the oldest ordering
// really is the newest ordering turned around, in both fields.
func TestPrecedesMirrorsBeatsHere(t *testing.T) {
	early := candidate{name: "a", wallClock: time.Unix(1_000, 0)}
	late := candidate{name: "z", wallClock: time.Unix(2_000, 0)}
	if !late.beats(early) || !early.precedes(late) {
		t.Error("the manifest timestamp does not order the two policies in opposite directions")
	}
	tieA := candidate{name: "a", wallClock: time.Unix(1_000, 0)}
	tieZ := candidate{name: "z", wallClock: time.Unix(1_000, 0)}
	if !tieZ.beats(tieA) || !tieA.precedes(tieZ) {
		t.Error("a tie does not break toward opposite names under the two policies")
	}
}

func TestSelectionTieBreaksDeterministically(t *testing.T) {
	base := t.TempDir()
	a := writeCandidate(t, base, "a-tie.zip", windowMiddle, 0)
	z := writeCandidate(t, base, "z-tie.zip", windowMiddle, 0)
	// One manifest timestamp between them, so only the name is left to
	// decide — and it must decide, or the answer follows whatever order the
	// filesystem happened to hand back.
	for range 3 {
		if got, _ := chooseBackupIn(context.Background(), base, selectNewest); got != z {
			t.Fatalf("newest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(z))
		}
		if got, _ := chooseBackupIn(context.Background(), base, selectOldest); got != a {
			t.Fatalf("oldest broke the tie at %s, want %s", filepath.Base(got), filepath.Base(a))
		}
	}
}

func TestRandomCoversTheWindowAndNothingElse(t *testing.T) {
	base, oldest, middle, newest := selectionWindow(t)
	// A checksum sidecar beside the archives: not an archive at all, so
	// not a candidate under any policy.
	if err := os.WriteFile(filepath.Join(base, "SHA256SUMS"), []byte("abc  a-saturday.zip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for range 200 {
		got, perr := chooseBackupIn(context.Background(), base, selectRandom)
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
		t.Errorf("draws landed on %d candidates, want only the three readable archives", len(seen))
	}
}

// TestNewestStillRefusesANewerUnreadableArchive keeps the refusal that
// guards the default policy: a backup job still writing its zip leaves an
// archive with no central directory, and quietly restoring last night's
// while the record named tonight's drill is the failure this refusal
// exists to prevent.
func TestNewestStillRefusesANewerUnreadableArchive(t *testing.T) {
	base := t.TempDir()
	writeCandidate(t, base, "a-good.zip", windowOldest, 48*time.Hour)
	partial := filepath.Join(base, "z-partial.zip")
	if err := os.WriteFile(partial, []byte("PK\x03\x04 half a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, perr := chooseBackupIn(context.Background(), base, selectNewest)
	if perr == nil || perr.Code != "source_unreadable" ||
		!strings.Contains(perr.Message, "z-partial.zip") {
		t.Fatalf("perr = %+v, want the newer unreadable archive refused by name", perr)
	}
}

// TestOldestAndRandomIgnoreANewerUnreadableArchive records the one place
// this slice changed more than an ordering. The operator has named which
// end of the retention window the drill is about and the record names the
// artifact it proved, so a half-written archive elsewhere is nothing the
// result could be read as claiming — and keeping the refusal would fire on
// almost every candidate, since under oldest nearly everything in the
// directory is newer than the chosen one. Same scoping as the weaviate
// adapter's newer-attempt refusal, for the same reason.
func TestOldestAndRandomIgnoreANewerUnreadableArchive(t *testing.T) {
	base := t.TempDir()
	wanted := writeCandidate(t, base, "a-good.zip", windowOldest, 48*time.Hour)
	partial := filepath.Join(base, "z-partial.zip")
	if err := os.WriteFile(partial, []byte("PK\x03\x04 half a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []selectPolicy{selectOldest, selectRandom} {
		got, perr := chooseBackupIn(context.Background(), base, policy)
		if perr != nil {
			t.Fatalf("%s: %+v", policy, perr)
		}
		if got != wanted {
			t.Errorf("%s picked %s, want the one readable archive", policy, filepath.Base(got))
		}
	}
}

// TestNoReadableArchiveRefusesUnderEveryPolicy: dropping the
// newer-unreadable refusal for oldest and random must not turn into
// restoring nothing.
func TestNoReadableArchiveRefusesUnderEveryPolicy(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "z-partial.zip"),
		[]byte("PK\x03\x04 half a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []selectPolicy{selectNewest, selectOldest, selectRandom} {
		if _, perr := chooseBackupIn(context.Background(), base, policy); perr == nil {
			t.Errorf("%s accepted a directory holding no readable archive", policy)
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
		src, perr := resolveSource(context.Background(), "clickhouse_backup_dir", base, tc.params)
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
	_, perr := resolveSource(context.Background(), "clickhouse_backup", newest,
		map[string]string{selectParam: "oldest"})
	if perr == nil || perr.Code != "invalid_request" {
		t.Fatalf("perr = %+v, want the selection refused on a kind that selects nothing", perr)
	}
}
