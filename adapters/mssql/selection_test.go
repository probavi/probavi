package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// selection_test.go covers source.params.select, which this adapter has to
// answer in two places: which artifact a directory of backups restores
// (backupset.go, ranked in the sandbox over what RESTORE HEADERONLY
// reported) and which full backup a chain is anchored on (chain.go, ranked
// by the engine's own checkpoint log sequence numbers).

func selectPayload(kind, dir, policy string) string {
	return fmt.Sprintf(
		`{"source":{"kind":%q,"path":%q,"params":{"select":%q},"credential_env":[]},`+
			`"sandbox":{"scratch_dir":"/scratch"},"options":{"database":"shop"}}`,
		kind, dir, policy)
}

func TestBackupSelectionAccepts(t *testing.T) {
	for _, kind := range []string{"bak_dir", "bak_chain", "bak_with_logins"} {
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
	if _, perr := backupSelection("bak", map[string]string{"backup_timezone": "UTC"}); perr != nil {
		t.Errorf("bak with no select declared: %+v", perr)
	}
}

func TestBackupSelectionRefuses(t *testing.T) {
	t.Run("a value that is none of the three", func(t *testing.T) {
		_, perr := backupSelection("bak_dir", map[string]string{selectParam: "latest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "latest is none of them") {
			t.Fatalf("perr = %+v, want the declared value named back", perr)
		}
	})

	t.Run("a kind that selects nothing", func(t *testing.T) {
		_, perr := backupSelection("bak", map[string]string{selectParam: "oldest"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "restores what source.path names") {
			t.Fatalf("perr = %+v, want the parameter refused rather than ignored", perr)
		}
	})

	t.Run("a member already named outright", func(t *testing.T) {
		_, perr := backupSelection("bak_with_logins",
			map[string]string{selectParam: "oldest", "bak": "nightly.bak"})
		if perr == nil || perr.Code != "invalid_request" ||
			!strings.Contains(perr.Message, "nightly.bak") {
			t.Fatalf("perr = %+v, want the conflict named", perr)
		}
	})
}

// TestSelectionRefusalReachesTheDrill: the refusal is not a library
// nicety — a drill configured this way fails before a byte moves.
func TestSelectionRefusalReachesTheDrill(t *testing.T) {
	dir := t.TempDir()
	writeMedia(t, dir, "a-full.bak", "FULL")
	h := &scanHandler{t: t, headers: map[string]string{}}
	line, _, _ := driveOp(t, "provision", selectPayload("bak_dir", dir, "latest"), h.handle)
	f := parseFinal(t, line)
	if f.OK || f.Error.Code != "invalid_request" ||
		!strings.Contains(f.Error.Message, "latest is none of them") {
		t.Fatalf("final = %+v, want the drill refused by name", f)
	}
	if len(h.probed) != 0 {
		t.Errorf("probed = %v, want nothing transferred before the config was refused", h.probed)
	}
}

// TestScanPicksTheEndsOfTheWindow drives a real provision over three dated
// candidates whose file times deliberately run against their headers, so
// nothing but the recorded completion time can decide.
func TestScanPicksTheEndsOfTheWindow(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   string
	}{
		{"newest", "m-newest.bak"},
		{"oldest", "z-oldest.bak"},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			dir := t.TempDir()
			oldest := writeMedia(t, dir, "z-oldest.bak", "OLDEST")
			middle := writeMedia(t, dir, "a-middle.bak", "MIDDLE")
			newest := writeMedia(t, dir, "m-newest.bak", "NEWEST")
			touch(t, oldest, time.Minute) // copied in last, so it looks newest
			touch(t, middle, time.Hour)   //
			touch(t, newest, 3*time.Hour) // taken last, but copied in first
			h := &scanHandler{t: t, headers: map[string]string{
				"z-oldest.bak": headerRowWithDate("2026-08-01 03:00:00.000") + "\n",
				"a-middle.bak": headerRowWithDate("2026-08-05 03:00:00.000") + "\n",
				"m-newest.bak": headerRowWithDate("2026-08-09 03:00:00.000") + "\n",
			}}
			line, _, exit := driveOp(t, "provision", selectPayload("bak_dir", dir, tc.policy), h.handle)
			f := parseFinal(t, line)
			if exit != 0 || !f.OK {
				t.Fatalf("exit=%d final=%+v", exit, f)
			}
			if len(h.probed) != 3 {
				t.Errorf("probed = %v, want every candidate examined whichever end is asked for", h.probed)
			}
			if strings.Join(h.transferred, ",") != tc.want {
				t.Errorf("transferred = %v, want %s", h.transferred, tc.want)
			}
			res := provisionWire{}
			if err := json.Unmarshal(f.Payload, &res); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if res.SourceIdentity.SizeBytes != int64(len("TAPE"+strings.ToUpper(strings.TrimSuffix(tc.want[2:], ".bak")))) {
				t.Errorf("size_bytes = %d, want the chosen artifact's size", res.SourceIdentity.SizeBytes)
			}
		})
	}
}

// TestOldestKeepsTheDatednessRule pins the one rule that does not invert:
// media the engine could date outranks media it could not, under oldest
// exactly as under newest. An undatable backup is not an old one — it has
// no age at all, and restoring it in preference to a dated backup would
// prove less.
func TestOldestKeepsTheDatednessRule(t *testing.T) {
	dir := t.TempDir()
	dated := writeMedia(t, dir, "a-dated.bak", "DATED")
	undated := writeMedia(t, dir, "z-undated.bak", "PLAIN")
	touch(t, dated, time.Minute)   // newest file
	touch(t, undated, 3*time.Hour) // oldest file, and undatable
	h := &scanHandler{t: t, headers: map[string]string{
		"a-dated.bak":   headerRowWithDate("2026-08-09 03:00:00.000") + "\n",
		"z-undated.bak": headerRow(backupTypeFull, 1) + "\n",
	}}
	line, _, _ := driveOp(t, "provision", selectPayload("bak_dir", dir, "oldest"), h.handle)
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if strings.Join(h.transferred, ",") != "a-dated.bak" {
		t.Errorf("transferred = %v, want the backup the drill can also date", h.transferred)
	}
}

// TestOldestFallsBackUpTheScanOrder: with nothing the engine could date,
// the scan order decides — and because that order is newest file first,
// oldest has to read it from the other end.
func TestOldestFallsBackUpTheScanOrder(t *testing.T) {
	dir := t.TempDir()
	older := writeMedia(t, dir, "a-older.bak", "OLD")
	newest := writeMedia(t, dir, "z-newest.bak", "NEW")
	touch(t, older, 3*time.Hour)
	touch(t, newest, time.Minute)
	h := &scanHandler{t: t, headers: map[string]string{
		"a-older.bak":  headerRow(backupTypeFull, 1) + "\n",
		"z-newest.bak": headerRow(backupTypeFull, 1) + "\n",
	}}
	line, _, _ := driveOp(t, "provision", selectPayload("bak_dir", dir, "oldest"), h.handle)
	if f := parseFinal(t, line); !f.OK {
		t.Fatalf("final = %+v", f)
	}
	if strings.Join(h.transferred, ",") != "a-older.bak" {
		t.Errorf("transferred = %v, want the oldest file when no header can rank them", h.transferred)
	}
}

// TestPrecedesIsNotTheNegationOfBeats fails if the two orderings are ever
// folded into one. On the datedness rule both answer the same way, which
// is exactly what !beats could not do.
func TestPrecedesIsNotTheNegationOfBeats(t *testing.T) {
	early := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	dated := mediaRank{clock: early, dated: true, index: 3}
	undated := mediaRank{index: 0}
	if !dated.beats(undated) {
		t.Error("beats: dated media must outrank undatable media")
	}
	if !dated.precedes(undated) {
		t.Error("precedes: dated media must outrank undatable media here too")
	}
	if dated.precedes(undated) == !dated.beats(undated) {
		t.Error("precedes has become the negation of beats; datedness is not a clock")
	}
	// The rules that do invert.
	late := mediaRank{clock: early.Add(time.Hour), dated: true, index: 0}
	if !late.beats(dated) || !dated.precedes(late) {
		t.Error("the completion time does not order the two policies in opposite directions")
	}
	first := mediaRank{index: 0}
	last := mediaRank{index: 1}
	if !first.beats(last) || !last.precedes(first) {
		t.Error("the scan order does not order the two policies in opposite directions")
	}
	same := mediaRank{clock: early, dated: true, index: 2}
	if same.precedes(same) {
		t.Error("a rank precedes itself — the ordering is not strict")
	}
}

// TestRandomDrawsOnlyFromDatableMedia: a draw reaches every backup the
// record could name a completion time for, and nothing else.
func TestRandomDrawsOnlyFromDatableMedia(t *testing.T) {
	at := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	candidates := []mediaCandidate{
		{sel: selection{hostPath: "/b/newest.bak"}, rank: mediaRank{clock: at.Add(time.Hour), dated: true, index: 0}},
		{sel: selection{hostPath: "/b/middle.bak"}, rank: mediaRank{clock: at, dated: true, index: 1}},
		{sel: selection{hostPath: "/b/oldest.bak"}, rank: mediaRank{clock: at.Add(-time.Hour), dated: true, index: 2}},
		{sel: selection{hostPath: "/b/undatable.bak"}, rank: mediaRank{index: 3}},
	}
	seen := map[string]int{}
	for range 200 {
		seen[pickMedia(candidates, selectRandom).hostPath]++
	}
	for _, want := range []string{"/b/newest.bak", "/b/middle.bak", "/b/oldest.bak"} {
		if seen[want] == 0 {
			t.Errorf("200 draws never reached %s", want)
		}
	}
	if len(seen) != 3 {
		t.Errorf("draws landed on %d candidates, want only the three datable ones", len(seen))
	}

	// Narrowing to the datable media must not narrow to none of them.
	undatable := []mediaCandidate{
		{sel: selection{hostPath: "/b/one.bak"}, rank: mediaRank{index: 0}},
		{sel: selection{hostPath: "/b/two.bak"}, rank: mediaRank{index: 1}},
	}
	seen = map[string]int{}
	for range 100 {
		seen[pickMedia(undatable, selectRandom).hostPath]++
	}
	if len(seen) != 2 {
		t.Errorf("draws landed on %d candidates, want both undatable ones", len(seen))
	}
}

// twoFullDirectory is the measured directory plus a second, newer full and
// the log that builds on it — the shape a retention window really has.
func twoFullDirectory() []chainNode {
	const (
		full2First = "42000000073600001" // the second full: 736 .. 760, checkpoint 736
		full2Last  = "42000000076000001"
		anchor2    = "42000000073600001"
		log9Last   = "42000000080000001" // 760 .. 800
	)
	return append(realDirectory(),
		node("08-full.bak", backupTypeFull, full2First, full2Last, anchor2, "0"),
		node("09-log.trn", backupTypeLog, full2Last, log9Last, anchor2, anchor2))
}

// TestChainAnchorsOnThePolicysFull is what makes this feature worth having
// on a chain kind: oldest proves that the far end of the retention window
// still replays forward through its own differentials and logs, which is
// the recovery an incident performs. The second full's chain is not part
// of it — a differential or log carries the checkpoint of the full it
// builds on, so each full brings only its own.
func TestChainAnchorsOnThePolicysFull(t *testing.T) {
	for _, tc := range []struct {
		policy selectPolicy
		want   string
	}{
		{selectNewest, "08-full.bak,09-log.trn"},
		{selectOldest, "01-full.bak,05-diff.bak,06-log.trn,07-log.trn"},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			chain, perr := buildChain(twoFullDirectory(), tc.policy)
			if perr != nil {
				t.Fatalf("buildChain: %+v", perr)
			}
			if got := names(chain); got != tc.want {
				t.Errorf("chain = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestChooseFullMirrorsItselfHere: unlike the directory ranking, the chain
// anchor really is newest turned around. A full whose checkpoint cannot be
// read is refused rather than ranked, so nothing undatable ever reaches
// the comparison and there is no asymmetric first rule to preserve.
func TestChooseFullMirrorsItselfHere(t *testing.T) {
	nodes := twoFullDirectory()
	newest, perr := chooseFull(nodes, selectNewest)
	if perr != nil {
		t.Fatalf("chooseFull(newest): %+v", perr)
	}
	oldest, perr := chooseFull(nodes, selectOldest)
	if perr != nil {
		t.Fatalf("chooseFull(oldest): %+v", perr)
	}
	if newest.name() != "08-full.bak" || oldest.name() != "01-full.bak" {
		t.Errorf("newest = %s, oldest = %s; want the two ends of the window",
			newest.name(), oldest.name())
	}
}

func TestChainRandomStaysWithinTheFulls(t *testing.T) {
	nodes := twoFullDirectory()
	seen := map[string]int{}
	for range 200 {
		full, perr := chooseFull(nodes, selectRandom)
		if perr != nil {
			t.Fatalf("chooseFull: %+v", perr)
		}
		seen[full.name()]++
	}
	if len(seen) != 2 || seen["01-full.bak"] == 0 || seen["08-full.bak"] == 0 {
		t.Errorf("draws = %v, want both fulls and nothing that is not one", seen)
	}
}

// TestChainRefusesAnUnreadableCheckpointUnderEveryPolicy: the refusal
// belongs to the reading of the directory, not to the ranking, so no
// policy skips past it — a random draw included.
func TestChainRefusesAnUnreadableCheckpointUnderEveryPolicy(t *testing.T) {
	nodes := append(twoFullDirectory(),
		node("10-full.bak", backupTypeFull, lsnFullFirst, lsnFullLast, "", "0"))
	for _, policy := range []selectPolicy{selectNewest, selectOldest, selectRandom} {
		_, perr := chooseFull(nodes, policy)
		if perr == nil || perr.Code != "source_corrupt" ||
			!strings.Contains(perr.Message, "10-full.bak") {
			t.Errorf("%s: perr = %+v, want the unreadable full refused by name", policy, perr)
		}
	}
}

// TestChainDrillHonoursTheSelection drives a real provision so the
// parameter is proved to travel from the drill config to the anchor.
func TestChainDrillHonoursTheSelection(t *testing.T) {
	dir, headers := chainDir(t)
	// A second, newer full beside the measured directory, with its own log.
	const (
		full2First = "42000000073600001"
		full2Last  = "42000000076000001"
		log9Last   = "42000000080000001"
	)
	headers["08-full.bak"] = chainHeaderRow(backupTypeFull, "shop", full2First, full2Last,
		full2First, "0", "2026-08-09 21:00:08.000")
	headers["09-log.trn"] = chainHeaderRow(backupTypeLog, "shop", full2Last, log9Last,
		full2First, full2First, "2026-08-09 21:00:09.000")
	writeMedia(t, dir, "08-full.bak", "payload-08-full.bak")
	writeMedia(t, dir, "09-log.trn", "payload-09-log.trn")

	r := &chainRun{t: t, headers: headers, transfers: map[string]string{}}
	line, _, exit := driveOp(t, "provision", selectPayload("bak_chain", dir, "oldest"), r.handle)
	f := parseFinal(t, line)
	if exit != 0 || !f.OK {
		t.Fatalf("exit=%d final=%+v", exit, f)
	}
	restored := strings.Join(r.restores, " | ")
	for _, member := range []string{"01-full.bak", "05-diff.bak", "06-log.trn", "07-log.trn"} {
		if !strings.Contains(restored, member) {
			t.Errorf("restores = %s, want the older full's own chain member %s", restored, member)
		}
	}
	for _, member := range []string{"08-full.bak", "09-log.trn"} {
		if strings.Contains(restored, member) {
			t.Errorf("restores = %s, want the newer full's chain left out under oldest", restored)
		}
	}
}
