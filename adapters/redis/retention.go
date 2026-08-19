package main

import (
	"context"
	"strconv"
	"strings"
)

// retention.go is this adapter's answer to issue #166.
//
// Redis expires by wall clock, not by elapsed time: every key with a TTL
// is stored with an absolute instant, and the artifact carries those
// instants unchanged. A backup taken while its keys were alive therefore
// holds keys that are dead by the time a drill loads it, and the engine
// treats them exactly as a production server would.
//
// It happens twice over, by two different mechanisms — both measured on
// every verified version of both engines, 100 permanent keys beside 100
// with a twenty-second expiry:
//
//   - Loading an RDB, the server discards a key whose instant has passed
//     before it ever enters the keyspace, and says so:
//     `rdb_last_load_keys_loaded:100`, `rdb_last_load_keys_expired:100`,
//     with a matching log line. DBSIZE is 100.
//   - Loading an append-only set, nothing is discarded at load — the base
//     RDB is read as an AOF preamble, which skips the check, so the
//     counters report 200 loaded and 0 expired. The ordinary expiry cycle
//     then removes them seconds later: DBSIZE 100, `GET session:1` empty,
//     `EXISTS session:1` zero.
//
// Either way the drill serves less than the backup holds, and this
// adapter ran no key census, so nothing said so.
//
// # There is no pin
//
// The discard is not configurable — the server does it whenever it is a
// master and the load is not an AOF preamble. The one shape that skips it
// is a replica, and that is worse rather than better: measured, a replica
// started on the same artifact reports `DBSIZE 200` and
// `rdb_last_load_keys_expired:0` while `GET session:1` returns nothing.
// The keys are in the keyspace and unreadable — a count that lies is a
// worse foundation for evidence than a count that is short. Nothing is
// gained and honesty is lost, so the sandbox stays a master.
//
// # The fence
//
// What the engine does give is an exact account of the load, and the two
// mechanisms above are both visible in it: the artifact carried
// `loaded + expired` keys, and the server now serves what INFO keyspace
// adds up to. When that sum is zero and the artifact carried keys, the
// drill has nothing to prove anything with, and says so instead of
// reporting a successful restore of an empty server.
//
// The residual is real and documented in the README: a backup that lost
// *some* of its keys still serves keys, so the drill proceeds and proves
// what remains. No fence can do better — a threshold would be arbitrary,
// and there is no setting that makes a server return an expired key.

const (
	loadedField  = "rdb_last_load_keys_loaded:"
	expiredField = "rdb_last_load_keys_expired:"

	// keyspacePrefix opens every INFO keyspace line: `db0:keys=50,…`. The
	// fields after `keys=` differ by version (7.4 added `subexpiry`), so
	// the parse anchors on the name and never on a position.
	keyspacePrefix = "db"
	keysField      = "keys="
)

// keyCensus is the engine's own account of the load it just performed.
type keyCensus struct {
	// loaded and expired are global across databases (measured: a key in
	// db3 counts in the same total as db0's).
	loaded  int
	expired int
	// serving is what the keyspace holds now, summed over every database.
	serving int
}

// carried is how many keys the artifact is known to have held.
//
// For an append-only set this is a floor rather than a total: the
// counters describe the base RDB, and keys written by the incremental
// file after it are not in them. The fence only asks whether the artifact
// held anything at all, so a floor is enough — and erring low means the
// fence can miss, never misfire.
func (k keyCensus) carried() int { return k.loaded + k.expired }

// parseKeyCensus reads INFO. Anything it cannot read stays zero, which
// leaves the fence standing down: a verdict of "the artifact held keys"
// must rest on the engine having said so.
func parseKeyCensus(info []byte) keyCensus {
	census := keyCensus{}
	for _, raw := range strings.Split(string(info), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, loadedField):
			census.loaded += atMostOne(strings.TrimPrefix(line, loadedField))
		case strings.HasPrefix(line, expiredField):
			census.expired += atMostOne(strings.TrimPrefix(line, expiredField))
		case strings.HasPrefix(line, keyspacePrefix):
			census.serving += keysIn(line)
		}
	}
	return census
}

// keysIn pulls the key count out of one INFO keyspace line.
func keysIn(line string) int {
	_, fields, found := strings.Cut(line, ":")
	if !found {
		return 0
	}
	for _, field := range strings.Split(fields, ",") {
		if value, ok := strings.CutPrefix(field, keysField); ok {
			return atMostOne(value)
		}
	}
	return 0
}

// atMostOne reads a count, treating anything unreadable as none.
func atMostOne(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// censusArgv asks the server for its own account of the load.
func censusArgv() []string {
	return []string{"redis-cli", "-e", "-h", "127.0.0.1", "-p", strconv.Itoa(defaultPort), "info"}
}

// readKeyCensus asks the restored server what it loaded and what it holds.
func readKeyCensus(ctx context.Context, c *core) (keyCensus, float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{Argv: censusArgv(), TimeoutSeconds: 15})
	if perr != nil {
		return keyCensus{}, 0, perr
	}
	if val.ExitCode != 0 {
		// Not an accusation: a server that will not describe itself has
		// said nothing about the artifact either way.
		return keyCensus{}, val.DurationSeconds, nil
	}
	return parseKeyCensus(stdout), val.DurationSeconds, nil
}

// refusedEmptyKeyspace is the verdict for a restored server holding
// nothing while the artifact it was pointed at held keys.
//
// The message says whose fault it is not, because the natural reading of
// a failed drill is "the backup is bad" and here the backup is intact —
// what expired is time, not data.
func refusedEmptyKeyspace(k keyCensus) *protoError {
	return protoErr("restore_failed", false,
		"the restored server holds no keys while the backup carried at least %d: %d were already "+
			"past their expiry when the engine read the artifact and were dropped on the spot, and "+
			"any others expired once the server was up. The backup is not damaged — Redis stores an "+
			"absolute instant with every expiring key, so a backup drilled after those instants "+
			"serves none of them, and no setting makes a server return an expired key. A drill of an "+
			"empty server proves nothing: drill a backup younger than its keys' time to live",
		k.carried(), k.expired)
}
