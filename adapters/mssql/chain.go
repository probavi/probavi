package main

import (
	"fmt"
	"math/big"
	"path/filepath"
	"sort"
	"strings"
)

// chain.go builds the restore chain a SQL Server backup set really is.
//
// The bak_dir kind restores the newest full backup and stops there, which
// is honest but proves less than the recovery it stands for: everything
// written after that full is outside the drill. Measured on a real
// server, a directory holding one full, two differentials and four log
// backups recovers 1 row through the full alone and all 7 through the
// chain.
//
// What links a chain is not the file names — those are convention SQL
// Server ignores — but the log sequence numbers in each backup's header,
// and those relationships were measured rather than assumed:
//
//   - every differential and log carries databaseLSN equal to the
//     checkpoint of the full it builds on, which is what ties a chain
//     together and separates two fulls' chains from each other;
//   - logs cover contiguous ranges, each one's first equal to the
//     previous one's last, so a chain is followed by carrying a redo
//     point forward rather than by sorting on time;
//   - a backup of another database restored into this one fails loudly
//     ("the backup set holds a backup of a database other than…"), which
//     is why the database name is a filter here rather than a surprise
//     later.

// chainNode is one backup set, in one file, that the chain may restore.
type chainNode struct {
	hostPath string
	set      backupSet
}

// name is what an operator recognises in a diagnostic.
func (n chainNode) name() string {
	base := filepath.Base(n.hostPath)
	if n.set.position > 1 {
		return fmt.Sprintf("%s (set %d)", base, n.set.position)
	}
	return base
}

// lsn parses a log sequence number. They are decimal numbers wider than
// any integer type this adapter could hold, so they are compared as
// arbitrary-precision values rather than truncated into one.
func lsn(value string) (*big.Int, bool) {
	if value == "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(value, 10)
	if !ok || n.Sign() < 0 {
		return nil, false
	}
	return n, true
}

// buildChain assembles the restore order for one database: the newest
// full, the newest differential that builds on it, and then the log
// backups that carry the redo point forward from there. A gap in the log
// sequence is an error, not a place to stop quietly — stopping would
// leave the record claiming a chain restore that silently ended early.
func buildChain(nodes []chainNode) ([]chainNode, *protoError) {
	full, perr := newestFull(nodes)
	if perr != nil {
		return nil, perr
	}
	anchor, _ := lsn(full.set.checkpoint) // newestFull refused an unreadable one
	chain := []chainNode{full}
	redo, ok := lsn(full.set.lastLSN)
	if !ok {
		return nil, protoErr("source_corrupt", false,
			"the full backup %s has no readable end position", full.name())
	}

	if diff, found := newestDifferential(nodes, anchor, redo); found {
		chain = append(chain, diff)
		redo, _ = lsn(diff.set.lastLSN)
	}
	logs, perr := logsFrom(nodes, anchor, redo)
	if perr != nil {
		return nil, perr
	}
	return append(chain, logs...), nil
}

// newestFull picks the full backup the chain starts from: the one with
// the greatest checkpoint, which is the engine's own ordering and does
// not depend on file times or clocks.
//
// A full whose checkpoint cannot be read is refused rather than passed
// over: skipping it would report "no full backup" while naming one, and a
// header this adapter cannot read is exactly what a drill should surface.
func newestFull(nodes []chainNode) (chainNode, *protoError) {
	var best chainNode
	var bestLSN *big.Int
	for _, n := range nodes {
		if n.set.backupType != backupTypeFull {
			continue
		}
		value, ok := lsn(n.set.checkpoint)
		if !ok {
			return chainNode{}, protoErr("source_corrupt", false,
				"the full backup %s has no readable checkpoint, so nothing can be chained onto it", n.name())
		}
		if bestLSN == nil || value.Cmp(bestLSN) > 0 {
			best, bestLSN = n, value
		}
	}
	if bestLSN == nil {
		return chainNode{}, protoErr("source_not_found", false,
			"no full backup to start a chain from: %s", describeNodes(nodes))
	}
	return best, nil
}

// newestDifferential picks the latest differential that builds on this
// full and moves the restore forward. A differential older than the point
// already reached would undo progress, so it is not a candidate.
func newestDifferential(nodes []chainNode, anchor, redo *big.Int) (chainNode, bool) {
	var best chainNode
	var bestLSN *big.Int
	for _, n := range nodes {
		if n.set.backupType != backupTypeDifferential {
			continue
		}
		if !buildsOn(n, anchor) {
			continue
		}
		last, ok := lsn(n.set.lastLSN)
		if !ok || last.Cmp(redo) <= 0 {
			continue
		}
		if bestLSN == nil || last.Cmp(bestLSN) > 0 {
			best, bestLSN = n, last
		}
	}
	return best, bestLSN != nil
}

// logsFrom follows the log backups forward from the redo point. Each step
// takes the log whose range contains that point; when none does but later
// logs exist, the sequence has a hole and the drill says so.
//
// The walk terminates because every step ends strictly beyond the point it
// started from (see nextLog): a log that has been applied — or that a
// differential already covered — ends at or before the redo point from
// then on, so it can never be chosen again, and there are finitely many
// logs in a directory.
func logsFrom(nodes []chainNode, anchor, redo *big.Int) ([]chainNode, *protoError) {
	var chain []chainNode
	for {
		next, gap, found := nextLog(nodes, anchor, redo)
		if gap != nil {
			return nil, gap
		}
		if !found {
			return chain, nil
		}
		chain = append(chain, next)
		redo, _ = lsn(next.set.lastLSN)
	}
}

// nextLog returns the log that continues from redo, or reports the gap
// that stops the chain.
//
// A candidate must end beyond the redo point: a log whose whole range is
// already restored — the one just applied, a copy of it, or one a later
// differential superseded — contributes nothing, and replaying it would
// move the walk backwards and never finish. This is the invariant that
// makes logsFrom terminate, and it is not theoretical: the measured
// directory's differential covers two earlier logs, so without it the
// first chain drill would spin instead of restoring.
func nextLog(nodes []chainNode, anchor, redo *big.Int) (chainNode, *protoError, bool) {
	var next chainNode
	var nextLast *big.Int
	var earliestUnreachable *big.Int
	var unreachableName string

	for _, n := range nodes {
		if n.set.backupType != backupTypeLog || !buildsOn(n, anchor) {
			continue
		}
		first, ok1 := lsn(n.set.firstLSN)
		last, ok2 := lsn(n.set.lastLSN)
		if !ok1 || !ok2 {
			continue
		}
		if last.Cmp(redo) <= 0 {
			continue // already covered by what is restored
		}
		if first.Cmp(redo) <= 0 {
			// This log spans the redo point; the shortest such step keeps
			// the chain in the engine's own order.
			if nextLast == nil || last.Cmp(nextLast) < 0 {
				next, nextLast = n, last
			}
			continue
		}
		if earliestUnreachable == nil || first.Cmp(earliestUnreachable) < 0 {
			earliestUnreachable, unreachableName = first, n.name()
		}
	}
	if nextLast != nil {
		return next, nil, true
	}
	if earliestUnreachable != nil {
		return chainNode{}, protoErr("source_not_found", false,
			"the log backup chain has a gap: the restore reaches log sequence number %s, "+
				"and the next log in the directory (%s) begins at %s — the backup covering the "+
				"gap is missing, so the chain cannot be replayed to its end",
			redo.String(), unreachableName, earliestUnreachable.String()), false
	}
	return chainNode{}, nil, false
}

// buildsOn reports whether a differential or log belongs to the chain
// anchored on this full backup.
func buildsOn(n chainNode, anchor *big.Int) bool {
	value, ok := lsn(n.set.databaseLSN)
	return ok && value.Cmp(anchor) == 0
}

// databasesIn lists the databases a directory holds backups of, sorted so
// a diagnostic reads the same twice.
func databasesIn(nodes []chainNode) []string {
	seen := map[string]bool{}
	var names []string
	for _, n := range nodes {
		if name := n.set.database; name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// nodesFor keeps the backups of one database.
func nodesFor(nodes []chainNode, database string) []chainNode {
	kept := make([]chainNode, 0, len(nodes))
	for _, n := range nodes {
		if n.set.database == database {
			kept = append(kept, n)
		}
	}
	return kept
}

// describeNodes names what was examined, for a failure that tells an
// operator what the directory actually holds.
func describeNodes(nodes []chainNode) string {
	if len(nodes) == 0 {
		return "no backup sets were found"
	}
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("%s: %s", n.name(), backupTypeName(n.set.backupType)))
	}
	return nameList(parts, 5)
}

// chainNames lists a chain in restore order for the log line and the
// state an operator may read back.
func chainNames(chain []chainNode) string {
	parts := make([]string, 0, len(chain))
	for _, n := range chain {
		parts = append(parts, n.name())
	}
	return strings.Join(parts, " -> ")
}
