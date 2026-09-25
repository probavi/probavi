package main

import (
	"crypto/rand"
	"math/big"
)

// selection.go decides *which* backup a directory source restores.
//
// influx_backup_dir restored the newest member, and nothing else was on
// offer. A drill that runs every night then proves the newest backup every
// night and says nothing whatever about the oldest one in the retention
// window — which is the one an incident reaches for, once it is clear the
// damage predates yesterday. A rotated encryption key, bit rot on colder
// media, a format the current tooling no longer reads: every one of them
// is invisible under a newest-only policy, and every one of them is what a
// restore drill exists to find.
//
// So the policy is the operator's: newest (the default, and what every
// drill written before this parameter existed did), oldest, or random.
//
// It arrives in source.params rather than as a core config key, and that
// is a decision rather than an expedient. What "newest" means is engine
// knowledge — a dump header in one adapter, a plain file time where the
// artifact records nothing about itself, and here the timestamp stem
// `influx backup` writes into each manifest's own file name — so selection
// is adapter behaviour, and params is what the core hands an adapter
// uninterpreted (protocol §6.2). It therefore costs no core config key, no
// protocol version, and no adapter that does not want it.
//
// A candidate is dated by what it states about itself rather than by a
// file time a copy would reset, so oldest is exactly as strong here as
// newest already was. This adapter also needs no separate rule about
// candidates that state nothing, which the postgres and arangodb adapters
// do: a subdirectory holding no timestamped manifest is not an
// `influx backup` output and was never a candidate (newestStemIn), so
// nothing undatable reaches the ordering — and precedes is therefore the
// exact mirror of beats. It is written out anyway, beside beats, because
// in those other adapters it is not one.
//
// random is not reproducible, and does not need to be. source.params never
// reaches an evidence record (docs/drill-config.md §7) but what was
// restored does — backup.checksum, backup.size_bytes and
// backup.created_at name the artifact — so a record still says which
// backup it proved. A scheduled drill choosing randomly covers the
// retention window over time, which is the honest statistical shape of
// proving a window rather than a day.
//
// The price of ordering a directory is unchanged by any of this: every
// candidate is read whichever policy is asked for, because a candidate
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

// selectsAMember reports whether a kind chooses a backup at all. The other
// kinds restore what source.path names, so a selection policy there would
// mean nothing, and accepting the word for them would be the wrong kind of
// convenience.
//
// influx_backup is the near miss worth naming: a reused target directory
// holds several backups and the adapter restores its newest, but that is
// the engine's own layout inside one artifact rather than a directory of
// artifacts, and the two would not mean the same thing.
func selectsAMember(kind string) bool {
	return kind == "influx_backup_dir"
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
			"source.params.%s applies only to influx_backup_dir, which chooses a backup for the "+
				"drill: kind %s restores what source.path names", selectParam, kind)
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
// a directory offering none is reported by the caller, which can say what
// it passed over. No narrowing is needed before a random draw here —
// everything in the slice states its own instant, which is what made it a
// candidate at all.
func pick(candidates []backupCandidate, policy selectPolicy) backupCandidate {
	if policy == selectRandom {
		return candidates[randomIndex(len(candidates))]
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
