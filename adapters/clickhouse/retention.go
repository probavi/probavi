package main

import "fmt"

// retention.go stops the sandbox from applying its own retention policy to
// the artifact it was handed.
//
// A ClickHouse `TTL` clause is materialised by background merges. The rows
// a backup holds were inside their TTL when the `BACKUP` statement ran —
// an artifact whose rows were already past it could not exist, because the
// engine applies row TTL when a part is written (measured: 1000 expired
// rows inserted with every merge stopped are invisible immediately). Time
// then passes, and by the drill the same rows are past their expiry. The
// restored server treats them as a running server would: it deletes them.
//
// Measured on both verified images, restoring a backup whose 120-second
// TTL had elapsed while it sat on disk — the counts are what a check would
// read seconds after `RESTORE ALL` reported `RESTORED`:
//
//	table shape                      backup   drill
//	row TTL, all rows expired          60       0     (TTLDropMerge)
//	row TTL, some rows expired        200     146     (TTLDeleteMerge)
//	TTL … GROUP BY (rollup)            60      10     (TTLDropMerge)
//	column TTL on a payload column     60      60     — payload blanked
//	no TTL (control)                   60      60
//
// The column-TTL row is the quiet one: the row count a census would read
// does not move at all, and the values are gone. The adapter runs no
// census, so nothing anywhere reports any of this — the drill goes green
// having proved less than the backup holds. That is the false-green half
// of the class in issue #166.
//
// # Why the restore is in two passes
//
// The lever is `SYSTEM STOP TTL MERGES`, and it applies to the tables that
// exist when it is issued: measured, a table locked while it existed kept
// 100 rows fifty seconds past their thirty-second TTL and lost every one
// of them within five seconds of the lock being released, while a table
// created *after* the same statement was not covered and expired normally.
//
// `RESTORE ALL` creates the tables, so a statement before it locks nothing
// and a statement after it is too late — the part_log timestamps the TTL
// merge in the same second as the restore. Restoring the structure first
// gives the lock something to hold, which is the whole reason for the
// extra pass. It costs one metadata read: measured 143 ms + 264 ms against
// a single 146 ms pass on the suite's fixture, both dwarfed by the
// transfer of any real archive.
//
// The lock is a runtime action, not a schema change: `SHOW CREATE TABLE`
// after a pinned restore is byte-for-byte what the backup carried, so a
// check reading the table definition sees the operator's policy exactly as
// it was. And it lasts as long as the server, which is the drill.

const (
	// pinRefusedExit is what the restore script exits with when the engine
	// would not stop its TTL merges. No clickhouse-client produces it —
	// the client's own codes are engine error codes, and none is 92.
	pinRefusedExit = 92
	// emptyRestoreExit is what the restore script exits with when the
	// engine said RESTORED and produced no table. No clickhouse-client
	// produces it either, for the reason given at notRestoredExit.
	emptyRestoreExit = 93

	// pinStatement is the lock. It takes no table, so it covers every
	// table the structure pass has just created, in every restored
	// database — including the ones a check will never name.
	pinStatement = "SYSTEM STOP TTL MERGES"
)

// restoreStatements returns the three statements the restore is made of,
// in the order the script runs them.
func restoreStatements() (structure, pin, data, census string) {
	restore := fmt.Sprintf("RESTORE ALL FROM File('%s')", restoreArchiveName)
	return restore + " SETTINGS structure_only = true", pinStatement, restore, censusStatement
}

// censusStatement counts what the restore produced.
//
// The sandbox starts empty, so every non-system table the server holds
// afterwards came out of the archive — the databases named here are the
// three the engine keeps for itself, and `default` is left in because a
// backup may legitimately restore into it (measured: the image ships it
// holding nothing).
//
// It answers a bare number, which is what the protocol's conformance
// contract expects of a census: §10 drives an adapter against a simulated
// sandbox where every exec answers `1`.
const censusStatement = "SELECT count() FROM system.tables WHERE database NOT IN " +
	"('system', 'INFORMATION_SCHEMA', 'information_schema')"

// restoreScript replays the archive around the pin, proves at each step
// that the engine said what it did, and finishes by counting what came
// back.
//
// The count is there because the status word is not enough on its own:
// an archive holding no table restores with `RESTORED` printed for both
// passes and leaves a server with nothing in it (measured on ClickHouse
// 26.3 — `BACKUP` of a database with no tables, then `RESTORE ALL`,
// answering RESTORED twice and zero tables afterwards). A drill exists to
// catch a backup job that has been writing nothing.
//
// The statements arrive as arguments rather than being written here so
// that the adapter's Go code — where they are asserted in tests — remains
// the single place they are stated.
//
// The status checks live in the script for the same reason the mysql
// adapter's completeness gate does: an exit code is a verdict the sandbox
// reaches, and a verdict reached inside the sandbox is one this adapter
// cannot accidentally soften while reading output.
const restoreScript = `
user=$1; db=$2
run() { clickhouse-client --host 127.0.0.1 --user "$user" --database "$db" --query "$1"; }
restored() { printf '%s\n' "$1" | grep -q RESTORED; }

out=$(run "$3") || exit $?
restored "$out" || exit 91
run "$4" >/dev/null || exit 92
out=$(run "$5") || exit $?
restored "$out" || exit 91
tables=$(run "$6") || exit $?
[ "${tables:-0}" -gt 0 ] 2>/dev/null || { printf 'restored tables: %s\n' "$tables" >&2; exit 93; }
`

// refusedPin is the verdict when the engine will not stop expiring data.
//
// A refusal fails the drill. Carrying on and noting it would hand back a
// record whose content depends on how long the restore took and how much
// of the backup had aged past its TTL by then — two drills of the same
// backup, two answers. That is the one thing this product must never
// produce. Because the message names the statement, a server that changed
// or removed it says so out loud instead of costing an operator a day.
func refusedPin(stderr []byte) *protoError {
	return protoErr("invalid_request", false,
		"the sandbox engine would not stop its TTL merges (%s): %s — a restored table whose "+
			"rows aged past their TTL while the backup sat on disk loses them to a background "+
			"merge seconds after the restore (measured), so the drill would prove whatever "+
			"survived the clock rather than what the backup holds",
		pinStatement, verdictLine(stderr))
}
