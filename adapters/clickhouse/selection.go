package main

import (
	"crypto/rand"
	"math/big"
)

// selection.go decides *which* backup a directory source restores.
//
// clickhouse_backup_dir restored the newest member, and nothing else was
// on offer. A drill that runs every night then proves the newest backup
// every night and says nothing whatever about the oldest one in the
// retention window — which is the one an incident reaches for, once it is
// clear the damage predates yesterday. A rotated encryption key, bit rot
// on colder media, a format the current tooling no longer reads: every one
// of them is invisible under a newest-only policy, and every one of them
// is what a restore drill exists to find.
//
// So the policy is the operator's: newest (the default, and what every
// drill written before this parameter existed did), oldest, or random.
//
// It arrives in source.params rather than as a core config key, and that
// is a decision rather than an expedient. What "newest" means is engine
// knowledge — a dump header in one adapter, a plain file time where the
// artifact records nothing about itself, and here the backup time the
// archive's own manifest records — so selection is adapter behaviour, and
// params is what the core hands an adapter uninterpreted (protocol §6.2).
// It therefore costs no core config key, no protocol version, and no
// adapter that does not want it.
//
// A candidate is dated by what it states about itself rather than by a
// file time a copy would reset, so oldest is exactly as strong here as
// newest already was. This adapter also needs no separate rule about
// candidates that state nothing, which the postgres and cassandra adapters
// do: an archive whose manifest cannot be read is never ranked at all —
// it is either not an archive, and skipped, or it is one and it refuses
// the drill (source.go). So precedes is the exact mirror of beats. It is
// written out anyway, beside beats, because in those other adapters it is
// not one.
//
// random is not reproducible, and does not need to be. source.params never
// reaches an evidence record (docs/drill-config.md §7) but what was
// restored does — backup.checksum, backup.size_bytes and
// backup.created_at name the artifact — so a record still says which
// backup it proved. A scheduled drill choosing randomly covers the
// retention window over time, which is the honest statistical shape of
// proving a window rather than a day.
//
// What does not change with the policy: the settle window. The adapter
// chose the artifact under every one of them, so one a backup job is still
// writing is still refused rather than quietly passed over (settle.go).
// What does change is the newer-unreadable-archive refusal, and that is
// argued where it applies, at chooseBackupIn in source.go.

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
// single-artifact kind restores what source.path names, so a selection
// policy there would mean nothing, and accepting the word for it would be
// the wrong kind of convenience.
func selectsAMember(kind string) bool {
	return kind == "clickhouse_backup_dir"
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
			"source.params.%s applies only to clickhouse_backup_dir, which chooses a backup for "+
				"the drill: kind %s restores what source.path names", selectParam, kind)
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
// a directory offering no readable archive is reported by the caller, in
// the words its own kind calls for. No narrowing is needed before a random
// draw here — everything in the slice states its own backup time, which is
// what made it a candidate at all.
func pick(candidates []candidate, policy selectPolicy) candidate {
	if policy == selectRandom {
		return candidates[randomIndex(len(candidates))]
	}
	wins := candidate.beats
	if policy == selectOldest {
		wins = candidate.precedes
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
