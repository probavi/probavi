# probavi-adapter-prometheus

Restores Prometheus TSDB snapshots into a disposable sandbox so a drill
can prove they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

Monitoring history is backup-worthy data with a compliance clock of its
own — an unrestorable snapshot is an observability history gone — and
Prometheus is an engine whose forgiveness makes a dishonest drill easy:
the server skips a block it cannot load and stays up (measured). This
adapter's job is to make that impossible to call green.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `prometheus_snapshot_tar` | one tar archive (plain or gzip) of a snapshot — blocks at the root, or under one wrapping directory (both measured) |
| `prometheus_snapshot` | one snapshot directory from `POST /api/v1/admin/tsdb/snapshot` |
| `prometheus_snapshot_dir` | a directory of snapshot directories; the one whose own blocks claim the newest instant is restored |

PITR does not apply; the probe declares `pitr: false` so the core
refuses a `target.pitr` drill before anything runs.

## The raw-copy fence: a live data directory is refused by name

The one wrong way to take this backup is also the most tempting one:
copying (or tarring) the data directory from under the running server.
The copy carries `wal`, `chunks_head` and `lock` — entries a snapshot
from the API never contains (measured) — and its blocks alone miss
whatever was still in the write-ahead log. Both the directory kinds and
the archive kind refuse those markers by name, with the fix in the
message: `POST /api/v1/admin/tsdb/snapshot` produces a consistent
snapshot under any load, and that is the artifact this adapter restores.

## The census: a partial restore is never green

Measured behaviors this adapter refuses to paper over:

- a block with a **corrupted index** makes the server refuse to start
  ("corrupted block …: invalid checksum") — surfaced from the server's
  own log as `source_corrupt`;
- a block whose **meta.json is missing or unreadable** is *silently
  skipped*: the server starts, reports ready, and serves the rest;
- **corrupted chunk data** is caught only when the chunks are actually
  read — a query then fails with "cannot populate chunk …: checksum
  mismatch".

So after the restored server is ready, the drill runs two verdict
reads. The **block census** compares `prometheus_tsdb_blocks_loaded`
from the server's own `/metrics` against the number of blocks the
artifact *requires loading* — counted host-side from the snapshot
itself (or from the archive's table of contents) — and refuses a
partial load. Required is not the same as present: a snapshot taken
while compaction sources still sat on disk legitimately holds both a
compacted block and the parents it replaced, and the server skips a
block that another present block's `compaction.parents` names — that is
deduplication, not a failed load (measured). The census applies the
server's own rule, so a compaction-window snapshot passes while a block
that truly failed to load still refuses the drill. The
**series probe** counts every series at the newest instant the backup's
own blocks claim to cover, and refuses a well-formed zero: a server
that is up but serves none of the promised data is exactly the false
green a monitoring backup invites. Chunk payloads are checksum-verified
by the engine on every read (measured), so each check an operator
writes extends the verification to the data it touches.

## The sandbox image: the official image cannot idle

`prom/prometheus` pins the server binary as its entrypoint, so
`command: sleep infinity` cannot hold it idle for the adapter to work in
(measured). The drill sandbox runs a two-line wrapper you build
yourself:

```dockerfile
FROM prom/prometheus:v3.5.5
ENTRYPOINT []
```

Everything in the image — server, promtool, the busybox userland — is
exactly the official image's; only the entrypoint pin is lifted. A
drill pointed at an image that cannot run the server through a shell
fails up front with a message naming this section. The integration
suite builds the same wrapper from the manifest's listed image, so
"verified against Prometheus 3.5" means that server, exercised this
way.

```yaml
sandbox:
  provider: docker
  params:
    image: your-registry/probavi-prometheus:3.5   # the wrapper above
    command: sleep infinity                        # the adapter owns the engine lifecycle
    memory: 512m
source:
  kind: prometheus_snapshot_tar
  path: /backups/prometheus/snap-2026-08-16.tar.gz
```

The restored server listens on `127.0.0.1:9090` with no TLS and no
auth, acceptable for exactly one reason: a Probavi sandbox is
zero-ingress (`--network none`, no ports expressible). Its config
scrapes nothing — the restored server serves the backup, it must not
collect.

It also runs with **retention disabled**
(`--storage.tsdb.retention.time=100y --storage.tsdb.retention.size=0`).
A server started without those flags applies its default 15-day window
when the TSDB opens and *deletes* every block lying wholly outside it —
measured: a snapshot covering 30 days lost two of its four blocks from
the restored copy before a single check ran. Retention states what a
running server should keep; a drill proves what the backup holds, and
the operator's own policy is already expressed in which blocks the
snapshot contains. The trigger is the snapshot's own span rather than
its age: one taken a minute ago loses blocks if it covers more than the
window, while a year-old one covering two days does not. (Should you
ever pin these yourself, note that `retention.time=0` does not disable
retention — the server reads it as unset and restores the same 15-day
default.)

## Checks: the PromQL dialect, evaluated at the backup's own instant

Prometheus has no SQL. The declared runner passes the check text to
`promtool query instant` as **one PromQL expression** — no shell
anywhere — and evaluates it at the newest instant the backup's own
blocks claim to cover, delivered through the protocol's `{{database}}`
placeholder. Checks therefore read the restored data deterministically,
instead of an empty "now" hours after the backup was taken:

```yaml
checks:
  - name: targets_were_up
    sql: count(up == 1)
  - name: samples_survived
    sql: sum(max_over_time(prometheus_tsdb_head_samples_appended_total[1h]))
```

Built-in checks that generate SQL (`row_count`, `table_exists`,
`freshness`) do not apply to this adapter — the same trade the mongodb
and etcd adapters document, and the protocol's design working as
intended (§6.1). A range query in a check (`…[1h]`) reads — and thereby
checksum-verifies — every chunk it covers.

## When the backup was taken

`created_at` is the newest instant the backup's own blocks claim to
cover: each block's `meta.json` states its time range in epoch
milliseconds (measured), which carry no timezone question at all. A
file's mtime dates a copy, not a backup, so it is never used — and the
`prometheus_snapshot_dir` kind ranks candidates by this same claim, so
a stale snapshot copied yesterday never outranks the genuinely newest
one. The `source.params.backup_timezone` key the other adapters use has
nothing to act on here and is refused rather than silently ignored.

## Backup identity

For the archive kind, `checksum` is SHA-256 over the tar's bytes,
exactly as stored. For the directory kinds it is a canonical hash of
the snapshot tree: entries sorted by relative path; each regular file
contributes its path, size, and content bytes; symlinks contribute path
and target. The same tree always hashes the same, and any content
change changes the hash.

## Deliberately not here

- **Raw data-directory copies** (and tars of them) — refused by name;
  see the fence above.
- **Remote-write / Thanos / Mimir / Cortex object-store blocks** — a
  different system's artifact and retention story, not a Prometheus
  snapshot.
- **A full-payload read of every chunk at provision time** — PromQL has
  no collision-free "read everything" query (measured) and an unbounded
  read would fail on large healthy backups; the census plus the
  at-instant probe run always, and each check extends chunk
  verification to the data it reads. The README says exactly what is
  verified because the difference matters.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| a live data directory (or a tar of one) | `unsupported_source`, teaching the snapshot API |
| a walkable archive or directory with no blocks | `source_corrupt` |
| a block without readable `meta.json` | `source_corrupt`, naming the block |
| tar cannot unpack the archive | `source_corrupt` |
| the sandbox image cannot run the server | `invalid_request`, naming the wrapper recipe |
| the server refuses to start on a corrupted block | `source_corrupt`, carrying its log line |
| the server loaded fewer blocks than the snapshot requires (compaction sources already excluded) | `source_corrupt` — the census |
| every block named as another present block's compaction source | `source_corrupt` — cyclic metadata |
| no series at the instant the backup claims | `source_corrupt` — the probe |
| the restored server never became ready | `engine_not_ready`, or `restore_failed` with the server's own log line |
