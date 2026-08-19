package main

import (
	"context"
	"strconv"
	"strings"
)

// retention.go is this adapter's answer to issue #166 — and it is a
// different answer from the other engines', because Cassandra offers no
// switch to give.
//
// Cassandra does not delete expired data in the background the way the
// other engines do. A cell with a TTL carries its own expiry, and reads
// filter it out the instant that passes; compaction reclaims the space
// later, but the data is already invisible before then. So the rows a
// snapshot holds are readable in a drill only while they are inside their
// TTL, and a snapshot drilled after its own TTL serves less than it holds.
//
// Measured on the baseline image — 100 rows per table, snapshot taken with
// every row live, restored after a 60-second TTL had passed:
//
//	table                              in the snapshot   in the drill
//	orders  (no TTL)                        100             100
//	sessions (default_time_to_live 60)      100               0
//	mixed   (half written USING TTL 60)     100              50
//
// The artifact is not at fault and is not damaged: `sstabledump` of the
// snapshot's own sstable lists all 100 partitions, each row carrying
// `"ttl" : 60, "expired" : true`. Everything the backup promised is
// present in it. The engine simply will not serve it.
//
// # There is no pin
//
// Searched exhaustively rather than assumed: of the 358 `cassandra.*`
// system properties the 4.1 jar names, the only ones touching expiry are
// the 2038 overflow policy, the hint TTL, and `never_purge_tombstones` —
// which stops compaction from reclaiming expired data but does not make a
// single read return it. `cassandra.clock` accepts a replacement clock
// implementation, and moving a sandbox's clock to make old data look
// fresh is the one thing an evidence product must never do. Read-time
// expiry is unconditional, so unlike MongoDB, TimescaleDB, ClickHouse and
// InfluxDB, this adapter cannot suspend the policy for the drill.
//
// # What it can do instead
//
// It can refuse to call a table proven when it reads nothing. The drill
// already reads one row of every restored table; an empty answer used to
// pass. It now fails the drill when the artifact itself declares that the
// table held expiring rows — `TTL max` above zero in an sstable's own
// metadata — which is exactly the case where the snapshot holds data the
// drill cannot see.
//
// The two conditions are both necessary, and the measurements say why:
//
//   - A table nobody ever wrote to contributes only `manifest.json` and
//     `schema.cql` to a snapshot — no sstable at all, so nothing declares
//     a TTL and an empty read stays legitimate.
//   - A table whose every row was deleted contributes a full sstable of
//     tombstones with `TTL max: 0`. Its rows are meant to be gone, and the
//     drill says nothing about it.
//   - A table that lost only some rows to expiry still reads rows, so the
//     drill proceeds and proves what remains. That residual is real and
//     documented in the README: nothing here can make a drill prove data
//     the engine will not serve.

// ttlProbeScript reports the largest TTL any of a table's sstables
// declares, or 0 when none does.
//
// `sstablemetadata` is not on PATH in the official images while
// `sstableloader` is (both measured), so the tools directory is the
// fallback. An image without the tool at all reports 0 and the fence
// stands down: the tool's absence is not evidence of anything, and the
// version matrix is what proves the fence still fires on the images this
// adapter claims.
const ttlProbeScript = `d="$1"
tool=$(command -v sstablemetadata || echo /opt/cassandra/tools/bin/sstablemetadata)
[ -x "$tool" ] || { echo 0; exit 0; }
max=0
for f in "$d"/*Data.db; do
  [ -e "$f" ] || continue
  ttl=$("$tool" "$f" 2>/dev/null | sed -n 's/^TTL max: \([0-9][0-9]*\).*/\1/p' | head -1)
  case "$ttl" in ''|*[!0-9]*) continue ;; esac
  [ "$ttl" -gt "$max" ] && max=$ttl
done
echo "$max"`

// emptyResultFooter is what cqlsh prints when a SELECT returned nothing.
// Identical on both verified versions (measured): `(0 rows)`.
const emptyResultFooter = "(0 rows)"

// declaredTTL asks the artifact whether the table it holds carried
// expiring rows. A tool that cannot answer is not an accusation: the
// answer is zero, and the caller lets the table pass.
func declaredTTL(ctx context.Context, c *core, tableDir string) (int, float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", ttlProbeScript, "bash", tableDir}})
	if perr != nil {
		return 0, 0, perr
	}
	if val.ExitCode != 0 {
		return 0, val.DurationSeconds, nil
	}
	ttl, err := strconv.Atoi(strings.TrimSpace(firstLine(stdout)))
	if err != nil || ttl < 0 {
		return 0, val.DurationSeconds, nil
	}
	return ttl, val.DurationSeconds, nil
}

// refusedExpiredTable is the verdict for a restored table that reads
// nothing while its own artifact says it held rows that expire.
//
// The message says whose fault it is not, because the natural reading of
// a failed drill is "the backup is bad" and here the backup is provably
// intact. What the operator can change is the pairing: a snapshot younger
// than the table's TTL proves the table, and one older cannot.
func refusedExpiredTable(ref tableRef, ttl int) *protoError {
	return protoErr("restore_failed", false,
		"restored table %s holds no readable rows: its snapshot declares a time-to-live of %d "+
			"seconds and every row it carries has passed it, so the engine filters them out on "+
			"read. The backup is intact — the rows are in its sstables, marked expired — but "+
			"Cassandra offers no setting that serves expired data, so this drill would prove "+
			"nothing about this table. Drill a snapshot younger than the table's time-to-live, "+
			"or collect the snapshot without it",
		ref, ttl)
}
