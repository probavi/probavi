# probavi-adapter-cassandra

Restores Apache Cassandra snapshots into a disposable single-node
sandbox so a drill can prove they still work. Implements
`probavi-adapter/0` ([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md));
like the other adapters it is standard-library-only Go with no imports
from the Probavi core.

Cassandra's restore tooling is too forgiving to be trusted alone —
measured: `sstableloader` finding no complete sstable streams nothing
and **exits 0**, and it streams a corrupted Data file **without a word**,
the damage surfacing only when the restored table is read. This
adapter's job is to make neither of those reportable as green.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `cassandra_snapshot_tar` | one tar archive (plain or gzip) of a collected snapshot — keyspaces at the root, or under one wrapping directory |
| `cassandra_snapshot` | one collected snapshot tree: `<keyspace>/<table>/` holding each table's `snapshots/<tag>/` contents |
| `cassandra_snapshot_dir` | a directory of such trees; the one whose own manifests claim the newest instant is restored |

**Collecting a snapshot.** `nodetool snapshot -t <tag> <keyspace>` writes
each table's snapshot under
`<data>/<keyspace>/<table>-<id>/snapshots/<tag>/` — SSTable components
plus three self-descriptions this adapter builds on: `schema.cql` (the
table's own DDL), `manifest.json` (the Data files and the snapshot
instant, UTC — measured on 4.1 and 5.0), and per-sstable `TOC.txt` and
`Digest.crc32`. Collect them as a flat tree, one directory per table:

```console
$ tag=nightly; dest=/backups/cassandra/$(date +%F)
$ for snap in /var/lib/cassandra/data/*/*/snapshots/$tag; do
    tbl=${snap%/snapshots/*}; ks=$(basename "$(dirname "$tbl")")
    name=$(basename "$tbl"); name=${name%-*}
    mkdir -p "$dest/$ks/$name" && cp -a "$snap/." "$dest/$ks/$name/"
  done
```

PITR does not apply; the probe declares `pitr: false`.

## The completeness and integrity fences

Everything below is judged **host-side, before a byte is transferred**,
from the artifact's own claims:

- **Component census**: every component a sstable's own `TOC.txt` lists,
  and every file the `manifest.json` lists, must exist. Measured: with a
  component missing, `sstableloader` finds no complete sstable, streams
  zero bytes, and exits 0 — a restore that "succeeds" with none of the
  data, which is precisely the false green a backup drill exists to
  catch.
- **Digest verification**: each Data file is checked against the CRC-32
  its own `Digest.crc32` sidecar claims. Measured: the loader streams a
  corrupted Data file without complaint, and the damage surfaces only
  when the restored table is read (`ReadFailure`) — the drill refuses
  the bit-rotted backup by its own sidecar instead.
- **Raw-copy fence**: a table directory containing a `snapshots/` or
  `backups/` subdirectory is a copy taken from under a running node, not
  a snapshot — refused with the collection recipe in the message.
- **System keyspaces are refused**: a whole-data-directory rsync drags
  `system*` in, and a drill restoring Cassandra's own tables proves
  nothing about your data.
- Names must be unquoted CQL identifiers (`[a-z][a-z0-9_]*`): directory
  names flow into composed CQL, so anything else is refused rather than
  quoted around.

For the archive kind all of the above is judged from the tar **stream**
in one pass, without unpacking; an archive Go's reader cannot walk falls
back to in-sandbox extraction as the authority. After the restore, the
drill additionally reads one row of every restored table, so a failure
mode nothing above predicted still surfaces as the engine's own refusal
rather than a green record.

## The sandbox: one node, honestly

The official `cassandra` images idle under `command: sleep infinity`
with no wrapper (their entrypoint passes non-cassandra commands
through). The adapter prepares the node for a zero-ingress sandbox
itself — both fixes measured as required under `--network none`: the
daemon refuses to start when the container hostname does not resolve
(mapped to loopback), and every address in `cassandra.yaml` is pinned to
`127.0.0.1`. It then starts the node, recreates each table **from the
backup's own `schema.cql`**, and streams the sstables in with
`sstableloader`.

```yaml
sandbox:
  provider: docker
  params:
    image: cassandra:4.1.12
    command: sleep infinity        # the adapter owns the engine lifecycle
    memory: 1536m
    env.MAX_HEAP_SIZE: 512M        # the JVM heap; exec inherits sandbox env
    env.HEAP_NEWSIZE: 100M
source:
  kind: cassandra_snapshot
  path: /backups/cassandra/2026-08-16
```

Two honesty notes, both deliberate:

- **A per-node snapshot is not a cluster-consistent backup.** The drill
  proves that *this node's* snapshot restores and serves; with
  replication factor > 1 a single node's snapshot holds that node's
  replicas, not necessarily every row of the cluster. The record proves
  exactly what was restored — no more.
- **The keyspace is created drill-locally** with `SimpleStrategy` and
  replication factor 1 — `schema.cql` carries the table DDL only
  (measured), and a single-node drill has no other honest setting. Table
  schema comes verbatim from the backup; keyspace-level replication
  settings are not part of what this drill proves.

## Time-to-live: what a drill can and cannot prove

Cassandra does not delete expired data in the background the way other
engines do. A cell with a TTL carries its own expiry and **reads filter it
out** the instant that passes; compaction reclaims the space later, but
the data is invisible before then. So a snapshot's rows are provable only
while they are inside their TTL.

Measured on the baseline image — 100 rows per table, snapshot taken with
every row live, restored after a 60-second TTL had passed:

| table | in the snapshot | in the drill |
| --- | --- | --- |
| `orders` (no TTL) | 100 | 100 |
| `sessions` (`default_time_to_live = 60`) | 100 | **0** |
| `mixed` (half written `USING TTL 60`) | 100 | **50** |

**The snapshot is not damaged.** `sstabledump` of its own sstable lists
all 100 partitions, each row carrying `"ttl" : 60, "expired" : true`.
Everything the backup promised is in it; the engine will not serve it.

**And there is nothing to switch off.** Of the 358 `cassandra.*` system
properties the 4.1 jar names, the only ones touching expiry are the 2038
overflow policy, the hint TTL, and `never_purge_tombstones` — which stops
compaction from reclaiming expired data but does not make a single read
return it. `cassandra.clock` accepts a replacement clock implementation,
and moving a sandbox's clock to make old data look fresh is the one thing
an evidence product must never do. So where the MongoDB, TimescaleDB,
ClickHouse and InfluxDB adapters suspend the policy for the drill, this
one cannot.

What it does instead is refuse to call a table proven when it reads
nothing: **a restored table that returns no rows fails the drill if the
artifact's own sstables declare a time-to-live.** Both halves are
required, and the measurements say why — a table nobody ever wrote
contributes no sstable at all to a snapshot (`manifest.json` and
`schema.cql` only), and a table whose every row was deleted contributes
tombstones with `TTL max: 0`. Neither is refused; both are legitimate.

The residual is real and worth stating plainly: a table that lost only
*some* rows to expiry still reads rows, so the drill proceeds and proves
what remains. Nothing here can make a drill prove data the engine will not
serve. **Drill snapshots younger than their tables' time-to-live** — for a
short-TTL table that is the only arrangement that proves anything.

## Checks: CQL, with the built-ins working

The declared runner absorbs cqlsh's decorated output — header, dash
separator, padded columns, `(N rows)` footer (measured) — into the
undecorated tab-separated rows the protocol requires, via an awk filter
in the template; pipefail carries cqlsh's own exit code through. That
means **the core's generating built-in checks apply unchanged**
(`row_count`, `table_exists`, `freshness`, user-defined CQL), evaluated
against the restored keyspace `{{database}}` delivers:

```yaml
checks:
  - name: orders_survived
    type: row_count
    table: orders
    min: 100000
```

With several keyspaces in one artifact, `connection.database` is the
alphabetically first; checks against the others use qualified names.
Text values containing a literal ` | ` sequence would be re-split by the
filter — a documented edge of the dialect, not of your data's safety.

## When the backup was taken

`created_at` is the newest instant the snapshot's own manifests claim —
`manifest.json` states it in UTC (measured), so it is exact with no
timezone question, and `source.params.backup_timezone` is refused rather
than silently ignored. The `cassandra_snapshot_dir` kind ranks
candidates by this same claim, so a stale tree copied yesterday never
outranks the genuinely newest one.

## Backup identity

For the archive kind, `checksum` is SHA-256 over the tar's bytes,
exactly as stored. For the tree kinds it is a canonical hash of the
tree: entries sorted by relative path; each regular file contributes its
path, size, and content bytes; symlinks contribute path and target.

## Version pairing

An older snapshot restores on a newer engine (measured: 4.1 sstables
stream into a 5.0 node cleanly). The reverse fails on the backup's own
schema: a 5.0 snapshot's `schema.cql` states table options 4.1 does not
know (measured: `Unknown property 'allow_auto_snapshot'`), and the drill
maps that to a refusal naming both sides — a config pairing a backup
with a sandbox image that cannot restore it — rather than a bare parse
error.

## Deliberately not here

- **Raw data-directory copies** (and tars of them) — refused by name.
- **Incremental backups** (`backups/` directories) — a different
  artifact and bookkeeping; the fence treats their presence as evidence
  of a raw copy.
- **Multi-node topology restores** — token-aware, cluster-consistent
  restores are an operational exercise (`probavi gameday` orchestrates
  multi-drill scenarios); this drill proves one node's snapshot.
- **Commitlog archives / PITR** — not part of a snapshot.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| a raw data-directory copy (or a tar of one) | `unsupported_source`, teaching `nodetool snapshot` |
| a component the TOC or manifest lists is missing | `source_corrupt` — the loader would stream nothing and exit 0 (measured) |
| a Data file contradicting its own Digest.crc32 | `source_corrupt`, naming both values |
| a table without schema.cql | `source_corrupt`, teaching the collection loop |
| a system keyspace, or a name no unquoted identifier allows | `invalid_request` |
| the image lacks the toolchain | `invalid_request` |
| the backup's schema does not parse on this engine | `invalid_request`, naming both sides |
| sstableloader fails | `restore_failed` |
| reading a restored table fails | `source_corrupt`, carrying the engine's refusal |
| a restored table reads no rows while its snapshot declares a TTL | `restore_failed`, naming the table and the TTL |
| the node never became ready | `engine_not_ready`, or `restore_failed` with the node's own log line |
