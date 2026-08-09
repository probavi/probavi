package main

import "strings"

// oplog.go covers the consistency half of a MongoDB logical backup.
//
// mongodump copies collections one after another while the cluster keeps
// writing, so a dump of a live replica set is not point-consistent across
// collections. `mongodump --oplog` exists for exactly that: it captures
// the oplog entries produced during the dump window, and replaying them
// afterwards rolls the restored data forward to a single point — the end
// of the dump.
//
// The measured defect: the adapter never asked mongorestore to replay it.
// An archive taken with --oplog restored cleanly, exit 0, with the oplog
// section simply ignored — a write issued during the dump window, into a
// collection the dump had already copied, was absent from the restored
// data and nothing in the record said so. The operator captured the tail
// and the drill threw it away.
//
// The mongodump_with_oplog kind replays it and refuses to pass unless the
// replay actually happened.

// oplogRestoreFlags are the flags that make mongorestore roll the captured
// oplog forward. mongorestore refuses an archive that carries no oplog
// ("no oplog file to replay; make sure you run mongodump with --oplog"),
// so declaring this kind on a plain archive fails loudly rather than
// quietly proving less.
func oplogRestoreFlags() []string {
	return []string{"--oplogReplay"}
}

// oplogReplayMarker is what mongorestore logs when it applies the captured
// window. Both halves are checked: the announcement and the count line
// ("applied N oplog entries"), the second being the one that only appears
// after the entries are actually applied.
const (
	oplogReplayMarker  = "replaying oplog"
	oplogAppliedMarker = "oplog entries"
)

// verifyOplogReplayed is the gate that distinguishes this kind: a restore
// that ran without replaying the captured window proves less than the kind
// claims, so it must not pass. mongorestore fails loudly today when the
// oplog is missing; this gate is what keeps the claim true if it ever
// stops doing so — and it is the only thing standing between "restored"
// and "restored to a consistent point" in the evidence record.
func verifyOplogReplayed(stderr []byte) *protoError {
	out := string(stderr)
	if strings.Contains(out, oplogReplayMarker) && strings.Contains(out, oplogAppliedMarker) {
		return nil
	}
	return protoErr("restore_failed", false,
		"the archive was restored without replaying its oplog, so it is not point-consistent: %s",
		verdictLine(stderr))
}
