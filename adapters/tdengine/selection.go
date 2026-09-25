package main

import (
	"crypto/rand"
	"math/big"
	"time"
)

// selection.go decides *which* backup a directory source restores.
//
// taosdump_dir restored the newest member, and nothing else was on offer.
// A drill that runs every night then proves the newest backup every night
// and says nothing whatever about the oldest one in the retention window —
// which is the one an incident reaches for, once it is clear the damage
// predates yesterday. A rotated encryption key, bit rot on colder media, a
// format the current tooling no longer reads: every one of them is
// invisible under a newest-only policy, and every one of them is what a
// restore drill exists to find.
//
// So the policy is the operator's: newest (the default, and what every
// drill written before this parameter existed did), oldest, or random.
//
// It arrives in source.params rather than as a core config key, and that
// is a decision rather than an expedient. What "newest" means is engine
// knowledge — a dump header in one adapter, a plain file time where the
// artifact records nothing about itself, and here the instant taosdump
// prints into its own dump_result.txt — so selection is adapter behaviour,
// and params is what the core hands an adapter uninterpreted (protocol
// §6.2). It therefore costs no core config key, no protocol version, and
// no adapter that does not want it.
//
// # The ordering this file replaces
//
// The previous ranking read the dump's own instant, and where the artifact
// recorded none it substituted the directory's modification time into the
// same variable and compared the two against each other. Those are
// different clocks — when the backup was taken *there* against when the
// file was written *here* — and the clickhouse adapter refuses that
// comparison in as many words. Two things followed from it, both wrong in
// the same direction: a dump freshly copied in and recording nothing could
// outrank the genuinely newest dump that does record its instant, and a
// dated dump could lose to an undated copy made this morning.
//
// So the two facts are now kept apart, in the shape the postgres and
// cassandra adapters already use. A dump that states its own instant
// outranks one that does not, whichever end of the window is asked for;
// among dated dumps the recorded instant decides; among undated ones
// directory time decides, and only there; and a tie breaks on the name, in
// the direction the policy runs. **This changes what `newest` picks in a
// directory that mixes dated and undated dumps** — deliberately, and
// toward the backup a record can say something true about.
//
// random is not reproducible, and does not need to be. source.params never
// reaches an evidence record (docs/drill-config.md §7) but what was
// restored does — backup.checksum, backup.size_bytes and
// backup.created_at name the artifact — so a record still says which
// backup it proved. A scheduled drill choosing randomly covers the
// retention window over time, which is the honest statistical shape of
// proving a window rather than a day.

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
	return kind == "taosdump_dir"
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
			"source.params.%s applies only to taosdump_dir, which chooses a backup for the drill: "+
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

// dumpCandidate is one taosdump output a directory source could restore.
// The two clocks are kept in separate fields on purpose: clock is what the
// artifact says about itself, mtime is what the filesystem says about the
// copy, and nothing compares one against the other.
type dumpCandidate struct {
	path  string
	name  string
	dated bool // whether the dump records its own instant at all
	clock time.Time
	mtime time.Time
}

// beats orders two candidates for the newest policy: a dated dump outranks
// every undated one, a later recorded instant outranks an earlier, undated
// candidates fall back to directory time, and remaining ties break toward
// the lexicographically larger name so the choice never depends on
// directory iteration order.
func (c dumpCandidate) beats(other dumpCandidate) bool {
	switch {
	case c.dated != other.dated:
		return c.dated
	case c.dated && !c.clock.Equal(other.clock):
		return c.clock.After(other.clock)
	case !c.mtime.Equal(other.mtime):
		return c.mtime.After(other.mtime)
	default:
		return c.name > other.name
	}
}

// precedes orders two candidates for the oldest policy — and is not the
// negation of beats, which is the whole reason it is written out. The
// first rule does not invert: a dump that states its own instant still
// outranks one that does not, because datedness is not a clock. Reversing
// it would make "oldest" mean "prefer the candidate nothing can be said
// about", and restoring an undatable dump in preference to a dated one
// proves less, not more. Past that rule everything is turned around.
func (c dumpCandidate) precedes(other dumpCandidate) bool {
	switch {
	case c.dated != other.dated:
		return c.dated
	case c.dated && !c.clock.Equal(other.clock):
		return c.clock.Before(other.clock)
	case !c.mtime.Equal(other.mtime):
		return c.mtime.Before(other.mtime)
	default:
		return c.name < other.name
	}
}

// pick chooses one candidate under the policy. The slice is never empty:
// a directory holding no taosdump output is reported by the caller, in the
// words its own kind calls for.
func pick(candidates []dumpCandidate, policy selectPolicy) dumpCandidate {
	if policy == selectRandom {
		pool := datable(candidates)
		return pool[randomIndex(len(pool))]
	}
	wins := dumpCandidate.beats
	if policy == selectOldest {
		wins = dumpCandidate.precedes
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if wins(c, best) {
			best = c
		}
	}
	return best
}

// datable narrows a random draw to the dumps that state their own instant,
// where there are any. That is the preference the ordering already applies
// — a drill would rather restore the backup it can also say something true
// about — and a draw that ignored it would make a dump the record cannot
// date as likely to be proved as one it can.
func datable(candidates []dumpCandidate) []dumpCandidate {
	dated := make([]dumpCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.dated {
			dated = append(dated, c)
		}
	}
	if len(dated) == 0 {
		return candidates
	}
	return dated
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
