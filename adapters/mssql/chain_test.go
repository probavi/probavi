package main

import (
	"strings"
	"testing"
)

// The log sequence numbers below are the real ones, read off a directory
// built on SQL Server 2022: one full, two differentials and four log
// backups of `shop`, plus a full backup of `other` in the same place.
// Every relationship the chain builder relies on is visible in them —
// each differential and log carries the full's checkpoint as its
// databaseLSN, and the logs cover contiguous ranges.
const (
	lsnFullFirst = "42000000036800001" // the full: 368 .. 392, checkpoint 368
	lsnFullLast  = "42000000039200001"
	lsnAnchor    = "42000000036800001"

	lsnLog2First = "42000000036800001" // 368 .. 448
	lsnLog2Last  = "42000000044800001"
	lsnDiff3Last = "42000000056800001" // 544 .. 568
	lsnLog4First = "42000000044800001" // 448 .. 576
	lsnLog4Last  = "42000000057600001"
	lsnDiff5Last = "42000000068000001" // 656 .. 680
	lsnLog6First = "42000000057600001" // 576 .. 688
	lsnLog6Last  = "42000000068800001"
	lsnLog7First = "42000000068800001" // 688 .. 720
	lsnLog7Last  = "42000000072000001"
)

func node(name string, kind int, first, last, checkpoint, dbLSN string) chainNode {
	return chainNode{
		hostPath: "/backups/" + name,
		set: backupSet{
			position: 1, backupType: kind, database: "shop",
			firstLSN: first, lastLSN: last, checkpoint: checkpoint, databaseLSN: dbLSN,
		},
	}
}

// realDirectory is the measured directory, in the order a scan produces.
func realDirectory() []chainNode {
	return []chainNode{
		node("01-full.bak", backupTypeFull, lsnFullFirst, lsnFullLast, lsnAnchor, "0"),
		node("02-log.trn", backupTypeLog, lsnLog2First, lsnLog2Last, lsnFullFirst, lsnAnchor),
		node("03-diff.bak", backupTypeDifferential, "42000000054400001", lsnDiff3Last, "42000000054400001", lsnAnchor),
		node("04-log.trn", backupTypeLog, lsnLog4First, lsnLog4Last, "42000000054400001", lsnAnchor),
		node("05-diff.bak", backupTypeDifferential, "42000000065600001", lsnDiff5Last, "42000000065600001", lsnAnchor),
		node("06-log.trn", backupTypeLog, lsnLog6First, lsnLog6Last, "42000000065600001", lsnAnchor),
		node("07-log.trn", backupTypeLog, lsnLog7First, lsnLog7Last, "42000000069600002", lsnAnchor),
	}
}

func names(chain []chainNode) string {
	parts := make([]string, 0, len(chain))
	for _, n := range chain {
		parts = append(parts, n.name())
	}
	return strings.Join(parts, ",")
}

// TestBuildChainOnTheMeasuredDirectory pins the run-book order: the full,
// its newest differential, then the logs that carry the redo point
// forward. The intermediate logs 02 and 04 are covered by differential
// 05 and are deliberately not replayed.
func TestBuildChainOnTheMeasuredDirectory(t *testing.T) {
	chain, perr := buildChain(realDirectory())
	if perr != nil {
		t.Fatalf("buildChain: %+v", perr)
	}
	want := "01-full.bak,05-diff.bak,06-log.trn,07-log.trn"
	if got := names(chain); got != want {
		t.Errorf("chain = %s, want %s", got, want)
	}
}

func TestBuildChainVariants(t *testing.T) {
	all := realDirectory()
	tests := []struct {
		name  string
		nodes []chainNode
		want  string
	}{
		{"a full alone recovers itself", all[:1], "01-full.bak"},
		// With no differential, every log from the full is needed.
		{"full and logs only",
			[]chainNode{all[0], all[1], all[3], all[5], all[6]},
			"01-full.bak,02-log.trn,04-log.trn,06-log.trn,07-log.trn"},
		// The older differential is skipped: the newer one supersedes it.
		{"two differentials", all, "01-full.bak,05-diff.bak,06-log.trn,07-log.trn"},
		{"a differential with no logs after it",
			[]chainNode{all[0], all[4]}, "01-full.bak,05-diff.bak"},
		// Scan order must not matter: the chain follows log sequence
		// numbers, not the directory listing.
		{"reversed input", reversed(all), "01-full.bak,05-diff.bak,06-log.trn,07-log.trn"},
		// A backup directory that was copied or rsynced can hold the same
		// log twice under two names. The second copy adds nothing, and
		// must not send the walk round again.
		{"a log present twice", append(realDirectory(), duplicateOf(all[5], "06-log-copy.trn")),
			"01-full.bak,05-diff.bak,06-log.trn,07-log.trn"},
		// A retention window that keeps two weekly fulls: the drill must
		// stand for the newest recovery point, so the older full and
		// everything anchored on it stay out.
		{"an older full is superseded",
			append([]chainNode{olderFull, olderFullsLog}, realDirectory()...),
			"01-full.bak,05-diff.bak,06-log.trn,07-log.trn"},
		// COPY_ONLY log backups do not truncate the log, so the next
		// regular backup covers their range again. Both carry the chain
		// forward; the walk takes them in order rather than jumping over
		// one, so the drill replays the whole set.
		{"two logs span the same point",
			append(realDirectory(), copyOnlyLog),
			"01-full.bak,05-diff.bak,06-log.trn,06-log-copy_only.trn,07-log.trn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chain, perr := buildChain(tt.nodes)
			if perr != nil {
				t.Fatalf("buildChain: %+v", perr)
			}
			if got := names(chain); got != tt.want {
				t.Errorf("chain = %s, want %s", got, tt.want)
			}
		})
	}
}

// olderFull is last week's full backup of the same database, still in the
// directory, together with a log anchored on it.
var (
	olderFull = node("00-old-full.bak", backupTypeFull,
		"42000000012800001", "42000000015200001", "42000000012800001", "0")
	olderFullsLog = node("00-old-log.trn", backupTypeLog,
		"42000000015200001", "42000000018400001", "42000000012800001", "42000000012800001")

	// copyOnlyLog overlaps 06-log.trn: taken WITH COPY_ONLY, it left the
	// log untruncated, so the regular backup that followed covered the
	// same range and a little more.
	copyOnlyLog = node("06-log-copy_only.trn", backupTypeLog,
		lsnLog6First, "42000000070000001", "42000000065600001", lsnAnchor)
)

// duplicateOf is the same backup set under another file name, as a copied
// directory produces.
func duplicateOf(n chainNode, name string) chainNode {
	n.hostPath = "/backups/" + name
	return n
}

// TestNextLogAlwaysAdvances pins the invariant the walk's termination
// rests on: a step must end strictly beyond the redo point it started
// from. It is asserted directly rather than through buildChain because a
// step that does not advance makes the walk loop forever — a regression
// would hang the suite and grow until the kernel intervened, instead of
// failing.
func TestNextLogAlwaysAdvances(t *testing.T) {
	all := realDirectory()
	all = append(all, duplicateOf(all[5], "06-log-copy.trn"))
	anchor, _ := lsn(lsnAnchor)

	// Every point the real chain passes through, plus two the differential
	// jumps over — those are exactly where a covered log invites a step
	// backwards.
	for _, start := range []string{
		lsnFullLast, lsnLog2Last, lsnDiff3Last, lsnLog4Last, lsnDiff5Last, lsnLog6Last, lsnLog7Last,
	} {
		redo, ok := lsn(start)
		if !ok {
			t.Fatalf("test fixture: %s is not a log sequence number", start)
		}
		next, perr, found := nextLog(all, anchor, redo)
		if perr != nil || !found {
			continue // a gap or the end of the chain, both fine here
		}
		last, ok := lsn(next.set.lastLSN)
		if !ok {
			t.Fatalf("nextLog from %s chose %s, which has no readable end", start, next.name())
		}
		if last.Cmp(redo) <= 0 {
			t.Errorf("nextLog from %s chose %s ending at %s — the walk would not advance",
				start, next.name(), next.set.lastLSN)
		}
	}
}

func reversed(nodes []chainNode) []chainNode {
	out := make([]chainNode, 0, len(nodes))
	for i := len(nodes) - 1; i >= 0; i-- {
		out = append(out, nodes[i])
	}
	return out
}

// TestBuildChainRefusesAGap is the rule the maintainer chose: a hole in
// the log sequence fails the drill. Stopping quietly at the last
// reachable log would leave the record claiming a chain restore that
// ended early, which is the failure this kind exists to avoid.
func TestBuildChainRefusesAGap(t *testing.T) {
	all := realDirectory()
	// Full, then log 04 and later: nothing carries the redo point from
	// the full's end (392) to log 04's start (448).
	nodes := []chainNode{all[0], all[3], all[5], all[6]}
	_, perr := buildChain(nodes)
	if perr == nil {
		t.Fatal("buildChain accepted a chain with a missing log")
	}
	if perr.Code != "source_not_found" {
		t.Errorf("code = %s, want source_not_found", perr.Code)
	}
	for _, want := range []string{"gap", lsnFullLast, "04-log.trn", lsnLog4First} {
		if !strings.Contains(perr.Message, want) {
			t.Errorf("message = %q, want it to carry %q", perr.Message, want)
		}
	}
}

func TestBuildChainRefusals(t *testing.T) {
	all := realDirectory()
	t.Run("no full backup at all", func(t *testing.T) {
		_, perr := buildChain(all[1:2])
		if perr == nil || perr.Code != "source_not_found" {
			t.Fatalf("perr = %+v, want source_not_found", perr)
		}
		if !strings.Contains(perr.Message, "transaction log backup") {
			t.Errorf("message = %q, want it to say what the directory holds instead", perr.Message)
		}
	})
	t.Run("a full with no readable checkpoint", func(t *testing.T) {
		broken := node("bad.bak", backupTypeFull, lsnFullFirst, lsnFullLast, "", "0")
		if _, perr := buildChain([]chainNode{broken}); perr == nil || perr.Code != "source_corrupt" {
			t.Errorf("perr = %+v, want source_corrupt", perr)
		}
	})
}

// TestChainIgnoresAnotherFullsBackups proves the anchor works: a second
// full backup's differentials and logs carry a different databaseLSN and
// must not be mixed into this chain, even though they sit in the same
// directory and look newer.
func TestChainIgnoresAnotherFullsBackups(t *testing.T) {
	const otherAnchor = "42000000099900001"
	nodes := append(realDirectory(),
		node("08-other-chain-log.trn", backupTypeLog, lsnLog7Last, "42000000099999999",
			otherAnchor, otherAnchor))
	chain, perr := buildChain(nodes)
	if perr != nil {
		t.Fatalf("buildChain: %+v", perr)
	}
	if strings.Contains(names(chain), "08-other-chain") {
		t.Errorf("chain = %s, want no member anchored on another full backup", names(chain))
	}
}

func TestChooseDatabase(t *testing.T) {
	shop := realDirectory()
	other := node("99-other-full.bak", backupTypeFull, lsnFullFirst, lsnFullLast, lsnAnchor, "0")
	other.set.database = "other"
	mixed := append(realDirectory(), other)

	t.Run("one database needs no saying", func(t *testing.T) {
		got, perr := chooseDatabase(shop, "")
		if perr != nil || got != "shop" {
			t.Errorf("chooseDatabase = %q, %+v", got, perr)
		}
	})
	t.Run("several databases must be settled by the config", func(t *testing.T) {
		_, perr := chooseDatabase(mixed, "")
		if perr == nil || perr.Code != "invalid_request" {
			t.Fatalf("perr = %+v, want invalid_request", perr)
		}
		for _, want := range []string{"several databases", "database_name", "shop", "other"} {
			if !strings.Contains(perr.Message, want) {
				t.Errorf("message = %q, want it to carry %q", perr.Message, want)
			}
		}
	})
	t.Run("the named database is used", func(t *testing.T) {
		got, perr := chooseDatabase(mixed, "other")
		if perr != nil || got != "other" {
			t.Errorf("chooseDatabase = %q, %+v", got, perr)
		}
	})
	t.Run("a database that is not there", func(t *testing.T) {
		_, perr := chooseDatabase(mixed, "ghost")
		if perr == nil || perr.Code != "source_not_found" {
			t.Errorf("perr = %+v, want source_not_found", perr)
		}
	})
}

func TestNodesFor(t *testing.T) {
	other := node("99-other-full.bak", backupTypeFull, lsnFullFirst, lsnFullLast, lsnAnchor, "0")
	other.set.database = "other"
	mixed := append(realDirectory(), other)
	if got := len(nodesFor(mixed, "shop")); got != 7 {
		t.Errorf("nodesFor(shop) = %d, want 7", got)
	}
	if got := len(nodesFor(mixed, "other")); got != 1 {
		t.Errorf("nodesFor(other) = %d, want 1", got)
	}
}

func TestChainNodeName(t *testing.T) {
	n := node("multi.bak", backupTypeFull, lsnFullFirst, lsnFullLast, lsnAnchor, "0")
	if got := n.name(); got != "multi.bak" {
		t.Errorf("name = %q", got)
	}
	n.set.position = 3
	if got := n.name(); got != "multi.bak (set 3)" {
		t.Errorf("name = %q, want the set named when a file holds several", got)
	}
}

// TestLSNComparisonIsExact pins the reason these are compared as
// arbitrary-precision numbers: a log sequence number is a 25-digit
// decimal, and truncating one into a machine integer would make two
// different positions compare equal.
func TestLSNComparisonIsExact(t *testing.T) {
	big1, ok1 := lsn("99999999999999999999999990")
	big2, ok2 := lsn("99999999999999999999999991")
	if !ok1 || !ok2 {
		t.Fatal("lsn refused a 26-digit value")
	}
	if big1.Cmp(big2) >= 0 {
		t.Error("two log sequence numbers beyond 64 bits compared wrongly")
	}
	for _, bad := range []string{"", "not-a-number", "-1", "12x"} {
		if _, ok := lsn(bad); ok {
			t.Errorf("lsn(%q) accepted", bad)
		}
	}
}
