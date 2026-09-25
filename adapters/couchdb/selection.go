package main

import (
	"crypto/rand"
	"math/big"
	"time"
)

// selection.go decides *which* backup a directory source restores.
//
// couchbackup_dir restored the newest member, and nothing else was on offer. A
// drill that runs every night then proves the newest backup every night
// and says nothing whatever about the oldest one in the retention window
// — which is the one an incident reaches for, once it is clear the damage
// predates yesterday. A rotated encryption key, bit rot on colder media,
// a format the current tooling no longer reads: every one of them is
// invisible under a newest-only policy, and every one of them is what a
// restore drill exists to find.
//
// So the policy is the operator's: newest (the default, and what every
// drill written before this parameter existed did), oldest, or random.
//
// It arrives in source.params rather than as a core config key, and that
// is a decision rather than an expedient. What "newest" means is engine
// knowledge — a dump header in one adapter, a snapshot manifest in
// another, and here nothing at all — so selection is adapter behaviour,
// and params is what the core hands an adapter uninterpreted (protocol
// §6.2). It therefore costs no core config key, no protocol version, and
// no adapter that does not want it.
//
// **What this adapter orders by is file modification time**, because
// no CouchDB artifact records when it was taken: a couchbackup
// file's header line names the database and the tool, and nothing dates it
// (measured). That is weaker than the ordering the postgres and
// cassandra adapters apply, and the difference is worth stating rather
// than leaving to be discovered: copying a backup into the directory (cp
// without -p, an object-store download, an rsync without -t) gives it a
// fresh modification time, so a stale artifact looks like the newest thing
// there — and, under oldest, a freshly copied old backup stops looking
// like the oldest. The ordering is honest about what it has; the README
// says so too.
//
// random is not reproducible, and does not need to be. source.params never
// reaches an evidence record (docs/drill-config.md §7) but what was
// restored does — backup.checksum and backup.size_bytes name the
// artifact — so a record still says which backup it proved. A scheduled
// drill choosing randomly covers the retention window over time, which is
// the honest statistical shape of proving a window rather than a day.
//
// What does not change with the policy: the settle window. The adapter
// chose the artifact under every one of them, so one a backup job is still
// writing is still refused rather than quietly passed over (settle.go).

// selectParam names the policy in source.params.
const selectParam = "select"

// selectPolicy is how a directory source picks among its members.
type selectPolicy string

const (
	selectNewest selectPolicy = "newest"
	selectOldest selectPolicy = "oldest"
	selectRandom selectPolicy = "random"
)

// selectsAMember reports whether a kind chooses a backup at all. The other
// kinds restore what source.path names, so a selection policy there would
// mean nothing, and accepting the word for them would be the wrong kind of
// convenience.
func selectsAMember(kind string) bool {
	return kind == "couchbackup_dir"
}

// backupSelection resolves the declared policy, refusing a configuration
// that asks for something the kind cannot do rather than accepting the
// parameter and ignoring it. A parameter nothing reads is a config the
// operator believes in and a drill doing something else, which is the
// failure this whole file exists to remove.
func backupSelection(kind string, params map[string]string) (selectPolicy, *protoError) {
	raw := params[selectParam]
	if raw == "" {
		return selectNewest, nil
	}
	if !selectsAMember(kind) {
		return "", protoErr("invalid_request", false,
			"source.params.%s applies only to couchbackup_dir, which chooses a backup for the drill: "+
				"kind %s restores what source.path names", selectParam, kind)
	}
	switch policy := selectPolicy(raw); policy {
	case selectNewest, selectOldest, selectRandom:
		return policy, nil
	}
	return "", protoErr("invalid_request", false,
		"source.params.%s must be %s, %s or %s: %s is none of them",
		selectParam, selectNewest, selectOldest, selectRandom, raw)
}

// dirCandidate is one artifact a directory source could restore. File time
// is the whole of it, for the reason given at the top of this file.
type dirCandidate struct {
	path  string
	name  string
	mtime time.Time
}

// beats orders two candidates for the newest policy: newer file, then the
// lexicographically larger name, so the choice never depends on directory
// iteration order.
func (c dirCandidate) beats(other dirCandidate) bool {
	if !c.mtime.Equal(other.mtime) {
		return c.mtime.After(other.mtime)
	}
	return c.name > other.name
}

// precedes orders two candidates for the oldest policy. Here it really is
// beats turned around, and that is worth a sentence because in the
// postgres and cassandra adapters it is not: those rank a backup carrying
// its own recorded time above one that does not, and that rule cannot
// invert. With file time as the only fact, there is nothing asymmetric
// left.
func (c dirCandidate) precedes(other dirCandidate) bool {
	if !c.mtime.Equal(other.mtime) {
		return c.mtime.Before(other.mtime)
	}
	return c.name < other.name
}

// pick chooses one candidate under the policy. The slice is never empty:
// an empty directory is reported by the caller, in the words its own kind
// calls for.
func pick(candidates []dirCandidate, policy selectPolicy) dirCandidate {
	if policy == selectRandom {
		return candidates[randomIndex(len(candidates))]
	}
	wins := dirCandidate.beats
	if policy == selectOldest {
		wins = dirCandidate.precedes
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if wins(c, best) {
			best = c
		}
	}
	return best
}

// randomIndex returns a uniform index below n.
//
// crypto/rand because there is no math/rand anywhere in this repository:
// one draw per drill costs nothing measurable, and the alternative is a
// lint suppression on a gate AGENTS.md §3 says is never suppressed.
func randomIndex(n int) int {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// Unreachable: rand.Int fails only on a non-positive bound, and
		// the caller passes the length of a non-empty slice. Checked
		// rather than discarded because an unchecked error is not
		// something this repository writes.
		return 0
	}
	return int(i.Int64())
}
