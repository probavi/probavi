# probavi-adapter-scylladb

Restores a ScyllaDB snapshot into a disposable sandbox and proves the
restored tables serve rows.

Written against `docs/adapter-protocol.md` alone: standard library only,
no imports from the Probavi core, and no knowledge of Probavi beyond the
protocol document.

Everything below was measured on 2026-09-20 against
`scylladb/scylla:2026.3.1` unless it says otherwise. Where a measurement
corrected an assumption, the correction is stated rather than quietly
applied — including two that a port of the Cassandra adapter would have
got wrong.

## What it restores

Three source kinds, all of them a collected `nodetool snapshot`:

| kind | `source.path` points at |
| --- | --- |
| `scylladb_snapshot` | one collected snapshot tree: `<keyspace>/<table>/` holding each table's `snapshots/<tag>/` contents |
| `scylladb_snapshot_tar` | one tar archive (plain or gzip) of such a tree, keyspaces at the root or under one wrapping directory |
| `scylladb_snapshot_dir` | a directory of such trees; the one whose own manifests claim the newest instant is restored |

A snapshot's table directory holds the sstables plus two files the engine
writes beside them — `schema.cql` and `manifest.json` — and this adapter
needs both.

Point-in-time recovery is not offered: every source kind probes
`pitr: false`.

## The sandbox runs the engine — and `sleep infinity` breaks it

This is the one structural difference from the Cassandra adapter, and it
is forced by the image.

The official `cassandra` images pass a non-Cassandra command through their
entrypoint, so `command: sleep infinity` idles them and the adapter starts
the node itself. **The ScyllaDB image does the opposite**: its entrypoint
*appends* the container command to the server's own argv. With
`command: sleep infinity` the server launches as

```
/usr/bin/scylla … --blocked-reactor-notify-ms 25 sleep infinity
```

and exits with `error: too many positional options have been specified on
the command line` before it serves anything.

So the sandbox runs the engine, and the drill's `sandbox.params.command`
carries **the engine's own flags**:

```yaml
sandbox:
  provider: docker
  params:
    image: scylladb/scylla:2026.3.1
    command: "--smp 1"
    memory: 4GiB
```

Two numbers are worth knowing. `--smp 1` is what makes a small sandbox
viable. And **below about 2 GiB the server aborts on startup** — with the
image's own defaults a 2 GiB container dies with `SIGABRT (core dumped)`,
while the same container with `--smp 1` serves. The adapter's readiness
message says both, because a drill that times out here is almost always
one of the two.

Readiness itself is honest, which is not true of every engine: the first
CQL query answers and a `CREATE KEYSPACE` succeeds at the same instant —
t+2.9s at a 2 GiB cap, t+5s with the image defaults. There is no gap
between "answers" and "usable" to guard against.

### The node serves on 127.0.0.2

Not 127.0.0.1. The image's entrypoint pins `listen_address`, `rpc_address`
and the seed to `127.0.0.2`, and a connection to `127.0.0.1` is **refused**
(measured). The provision response reports `127.0.0.2`, so the address that
reaches the evidence record is where the drill really talked. The image's
own `cqlsh` defaults to the same address, which is why the declared check
runner passes no host.

### The version-check service is stopped

The image runs `scylla-housekeeping`, which wakes daily and posts an
installation UUID to the vendor. A Probavi sandbox is zero-ingress, so it
cannot reach anything — and the adapter stops the service anyway rather
than relying on that. A restore of production data should not carry a
reporting process at all. Best effort by design: an image without
`supervisorctl` is not an error and does not fail a drill.

## How the restore works

1. Wait for the node to answer CQL.
2. Stop the version-check service.
3. Move the artifact into the sandbox (`put_file` per file, or one archive
   plus `tar -xf`).
4. Create each keyspace, then apply each table's own `schema.cql`.
5. Copy each table's sstables into that table's `upload/` directory and
   ask the engine to take them with `nodetool refresh`.
6. Read one row of every restored table.

Step 6 is not a formality. **`nodetool refresh` pointed at an empty
`upload/` directory loads nothing and exits 0** (measured) — the same
silent no-op the Cassandra loader has — so the exit code is not a verdict.
The adapter refuses an empty stage before refresh is asked, and reads the
table back afterwards.

A damaged sstable, by contrast, is loud: the engine validates compressed
chunks as it reads them and names the file and the offset
(`malformed_sstable_exception … failed checksum`), so refresh exits
non-zero and the drill reports `source_corrupt`.

Note that refresh **consumes** `upload/`: the files are moved out, and the
directory is empty afterwards.

## Tablets, and why the drill's shape need not match production's

This engine places data in tablets by default, and two consequences follow.

**`SimpleStrategy` is refused.** A tablet-enabled keyspace answers
`ConfigurationException: SimpleStrategy doesn't support tablet
replication`, so the adapter creates keyspaces with
`NetworkTopologyStrategy` and replication factor 1.

**The tablet count does not have to match.** A snapshot taken from a
16-tablet table restored whole — 500 rows of 500 — into a table the drill
created with 2 tablets, with a plain `nodetool refresh` and no
`--load-and-stream`. This is what makes the adapter possible at all: a
production source has many nodes and many tablets, and a drill sandbox has
one node.

## The drill's replication is the sandbox's, not production's

`schema.cql` carries the table DDL alone — **never the keyspace**
(measured). So the keyspace is created drill-locally at replication factor
1, which is the honest setting for a single node. A drill proves that the
backup's rows come back and can be read; it does not reproduce the
production topology, and no evidence record should be read as saying it
does.

## Time-to-live: what a drill can and cannot prove

A cell with a TTL carries its own expiry, and reads filter it out the
instant that passes. Compaction reclaims the space later, but the data is
invisible before then — so **a snapshot's rows are provable only while
they are inside their TTL.**

Measured: 200 rows written `USING TTL 60`, flushed, snapshotted while
every row was live, then restored.

| | source | drill |
| --- | --- | --- |
| t+51s | 200 | 200 |
| t+67s | **0** | **0** |

The snapshot is not damaged. Its sstables still carry every row, and the
engine's own statistics name the TTL they were written with. The engine
will simply not serve them.

**And there is nothing to switch off.** Of the 352 options
`scylla --help` lists, the ones whose names touch expiry are about
something else: `auto-snapshot-ttl` is snapshot retention,
`tombstone-warn-threshold` and `query-tombstone-page-limit` are
diagnostics, `enable-tombstone-gc-for-streaming-and-repair` governs
garbage collection during streaming, `alternator-ttl-period-in-seconds` is
the DynamoDB API's own scanner, and `restrict-twcs-without-default-ttl` is
a schema guardrail. None makes a single read return an expired cell. And
moving a sandbox's clock to make old data look fresh is the one thing an
evidence product must never do.

So this adapter fences rather than suspends: **a restored table that
returns no rows fails the drill if the artifact's own sstables declare a
time-to-live.** Both halves are required, which keeps the fence off the
cases where an empty read is legitimate — a table nobody ever wrote to
contributes no sstable at all, and a table whose rows were deleted
contributes tombstones written with no TTL.

The residual is real and worth stating: a table that lost *some* rows to
expiry still reads rows, so the drill proceeds and proves what remains.
Nothing here can make a drill prove data the engine will not serve.

## What the backup states about itself

`manifest.json` is where this engine is more generous than its ancestor,
and the adapter leans on two of its fields.

**When it was taken.** `snapshot.created_at` is epoch seconds, so
`backup.created_at` is exact with no timezone to declare — the directory
kind ranks candidates by it, never by file times a copy would reset. A
`source.params.backup_timezone` is **refused** rather than ignored: an
operator who wrote it expects an accuracy the artifact already delivers,
and silence would leave them believing it did something.

**Whether the copy is complete.** `sstables[]` names every component set
the snapshot should hold — one per tablet, each with its `toc_name` and
`data_size`. The adapter holds the copy against that list before a byte is
restored, so a copy that lost a tablet's files is refused by name. This is
the failure a directory listing cannot see, because what remains still
looks like a snapshot.

## Backup identity

A tree hashes canonically — entries sorted by relative path, each
contributing path, size and content — and an archive hashes as its bytes.
Either way `source_identity.checksum` is a real measurement of what was
restored, not of what was named.

## Checks: CQL, with the built-ins working

Checks are CQL, run through the declared runner, and the core's generating
built-ins apply unchanged. `{{database}}` resolves to the keyspace the
provision response returned — with several keyspaces, the alphabetically
first; checks against the others use qualified names.

One statement the core generates is rewritten, for the same reason the
Cassandra adapter rewrites it: `table_exists` probes with
`SELECT count(*) FROM <table> WHERE 1=0`, and CQL has no such predicate.
`DESCRIBE TABLE` is the engine's own way to ask, exits 0 for a table that
exists and non-zero for one that does not, and answers from the schema —
so nothing is scanned and no row of restored production data reaches
stdout. The rewrite is guarded by the whole statement, so a check of the
operator's own can never be caught by it.

## Licensing

ScyllaDB Enterprise 2025.1 onward is source-available with a free tier of
all features within 10 TB of disk and 50 vCPUs per organization, and the
vendor states plainly that there is **no distinction between production
and non-production** as it relates to licensing. So an operator drilling
their own backup is inside the free tier, and a commercial customer is
covered by the agreement their entire organization must already be under —
the free tier may not be mixed with paid use. Probavi ships no engine and
accepts nothing on anyone's behalf here; the project's position is in
`docs/engine-licensing.md`.

## Deliberately not here

- **Scylla Manager backups.** The manager's own repository layout is a
  different artifact with its own metadata, not a collected snapshot.
- **Incremental or differential restore.** A snapshot is whole or it is
  refused.
- **Multi-node topology.** One node, replication factor 1, by design.
- **Per-cell TTL survival.** See the fence above; this is a property of
  the engine, not a gap in the adapter.
