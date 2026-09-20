package main

import (
	"context"
	"strconv"
	"strings"
)

// retention.go carries this adapter's answer to the data-lifecycle
// question every engine in issue #166 raises: what does the engine do to
// the artifact, unbidden, that can only subtract from what the backup
// holds?
//
// For this engine the answer is time-to-live, and the shape is the fence
// rather than the suspend. A cell with a TTL carries its own expiry and
// reads filter it out the instant that passes; compaction reclaims the
// space later, but the data is invisible before then. So a snapshot's
// rows are provable only while they are inside their TTL.
//
// Measured 2026-09-20 on 2026.3.1 — 200 rows written USING TTL 60,
// flushed, snapshotted while every row was live, then restored:
//
//	t+51s   source 200   drill 200
//	t+67s   source   0   drill   0
//
// The snapshot is not damaged: its sstables still carry every row, and
// the engine's own statistics name the TTL they were written with. The
// engine will simply not serve them.
//
// And there is nothing to switch off. Of the 352 options `scylla --help`
// lists, the ones whose names touch expiry are about something else —
// `auto-snapshot-ttl` is snapshot retention, `tombstone-warn-threshold`
// and `query-tombstone-page-limit` are diagnostics,
// `enable-tombstone-gc-for-streaming-and-repair` governs garbage
// collection during streaming, `alternator-ttl-period-in-seconds` is the
// DynamoDB API's own scanner, and `restrict-twcs-without-default-ttl` is
// a schema guardrail. None makes a single read return an expired cell.
// Moving the sandbox's clock to make old data look fresh is the one
// thing an evidence product must never do. So where the MongoDB,
// TimescaleDB, ClickHouse and InfluxDB adapters suspend the policy for
// the drill, this one cannot.
//
// What it does instead is refuse to call a table proven when it reads
// nothing: a restored table that returns no rows fails the drill if the
// artifact's own sstables declare a time-to-live. Both halves are
// required, which is what keeps the fence from firing on the cases where
// an empty read is legitimate:
//
//   - A table nobody ever wrote to contributes only `manifest.json` and
//     `schema.cql` to a snapshot — no sstable at all, so nothing declares
//     a TTL and an empty read stays legitimate.
//   - A table whose every row was deleted contributes tombstones written
//     with no TTL. Its rows are meant to be gone.
//   - A table that lost only some rows to expiry still reads rows, so the
//     drill proceeds and proves what remains. That residual is real and
//     documented in the README: nothing here can make a drill prove data
//     the engine will not serve.

// ttlProbeScript reports the largest TTL any of the restored table's
// sstables declares, or 0 when none does.
//
// It reads the live table directory rather than the staged copy, and for
// a measured reason: `scylla sstable` needs a schema, and the artifact's
// own schema.cql cannot supply one — loading it fails with
// `tombstone_gc option with mode = repair not supported for table with
// local replication strategy`, because the tool assumes a local strategy
// for a bare file. Against the restored table the `--keyspace/--table`
// form resolves the schema from the running engine, which is exactly the
// state this runs in. An engine without the tool reports 0 and the fence
// stands down: the tool's absence is not evidence of anything, and the
// version matrix is what proves the fence still fires on the images this
// adapter claims.
const ttlProbeScript = `set -u
ks=$1; tbl=$2
command -v scylla >/dev/null 2>&1 || { echo 0; exit 0; }
` + tableDirScript + `
if [ -z "$d" ]; then echo 0; exit 0; fi
max=0
for f in "$d"/*-big-Data.db; do
  [ -e "$f" ] || continue
  ttl=$(scylla sstable dump-statistics --keyspace "$ks" --table "$tbl" "$f" 2>/dev/null \
        | grep -o '"max_ttl":[0-9]*' | head -1 | cut -d: -f2)
  case "$ttl" in ''|*[!0-9]*) continue ;; esac
  if [ "$ttl" -gt "$max" ]; then max=$ttl; fi
done
echo "$max"`

// emptyResultFooter is what cqlsh prints when a SELECT returned nothing.
const emptyResultFooter = "(0 rows)"

// declaredTTL asks the restored table's own sstables whether they carried
// expiring rows. A tool that cannot answer is not an accusation: the
// answer is zero, and the caller lets the table pass.
func declaredTTL(ctx context.Context, c *core, ref tableRef) (int, float64, *protoError) {
	val, stdout, _, perr := c.exec(ctx, execArgs{
		Argv: []string{"bash", "-c", ttlProbeScript, "bash", ref.keyspace, ref.table}})
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
			"read. The backup is intact — the rows are in its sstables, marked with that TTL — "+
			"but the engine offers no setting that serves expired data, so this drill would "+
			"prove nothing about this table. Drill a snapshot younger than the table's "+
			"time-to-live, or collect the snapshot without it",
		ref, ttl)
}
