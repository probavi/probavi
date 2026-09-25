package main

import (
	"crypto/rand"
	"math/big"
)

// selection.go decides *which* backup a directory source restores.
//
// victoriametrics_backup_dir restored the newest member, and nothing else was on offer. A
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
// knowledge — a dump header in one adapter, a plain file time where the
// artifact records nothing about itself, and here the instant a backup's own metadata file records — so
// selection is adapter behaviour, and params is what the core hands an
// adapter uninterpreted (protocol §6.2). It therefore costs no core
// config key, no protocol version, and no adapter that does not want it.
//
// A candidate on this kind is a **backup directory**, not a file, and it is
// dated by what it says about itself rather than by a file time a copy
// would reset. So oldest is exactly as strong here as newest already was,
// which is not true of every adapter: where the artifact states nothing,
// the ordering is only as good as the directory's modification times.
// A candidate that states nothing still ranks below every one that does,
// under both policies — see precedes in source.go for why that one rule
// does not invert.
//
// random is not reproducible, and does not need to be. source.params
// never reaches an evidence record (docs/drill-config.md §7) but what was
// restored does — backup.checksum, backup.size_bytes and
// backup.created_at name the artifact — so a record still says which
// backup it proved. A scheduled drill choosing randomly covers the
// retention window over time, which is the honest statistical shape of
// proving a window rather than a day.
//
// The price of ordering a directory is unchanged by any of this: every
// candidate is opened whichever policy is asked for, because a candidate
// that is not read cannot be dated. oldest and random cost exactly what
// newest already cost.

// selectParam names the policy in source.params.
const selectParam = "select"

// selectPolicy is how a directory source picks among its members.
type selectPolicy string

const (
	selectNewest selectPolicy = "newest"
	selectOldest selectPolicy = "oldest"
	selectRandom selectPolicy = "random"
)

// selectsAMember reports whether a kind chooses a backup at all. The
// other kinds restore what source.path names, so a selection policy there
// would mean nothing, and accepting the word for them would be the wrong
// kind of convenience.
func selectsAMember(kind string) bool {
	return kind == "victoriametrics_backup_dir"
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
			"source.params.%s applies only to victoriametrics_backup_dir, which chooses a backup for the drill: "+
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

// pick chooses one candidate under the policy. The slice is never empty:
// an empty directory is reported by the caller, in the words its own kind
// calls for.
func pick(candidates []backupCandidate, policy selectPolicy) backupCandidate {
	if policy == selectRandom {
		pool := datable(candidates)
		return pool[randomIndex(len(pool))]
	}
	wins := backupCandidate.beats
	if policy == selectOldest {
		wins = backupCandidate.precedes
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if wins(c, best) {
			best = c
		}
	}
	return best
}

// datable narrows a random draw to the candidates that state an instant,
// where there are any. That is the preference the ordering already
// applies — a drill would rather restore the backup it can also say
// something true about — and here it does a second job: it keeps a random
// draw away from the stray sibling a backup directory collects (a
// half-finished copy, an unpacked scratch tree, a directory of checksums),
// which under an ordering could only ever be reached when nothing else
// was there at all.
func datable(candidates []backupCandidate) []backupCandidate {
	dated := make([]backupCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.createdAtMs != 0 {
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
