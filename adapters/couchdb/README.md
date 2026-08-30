# probavi-adapter-couchdb

Restores CouchDB backups into a disposable sandbox so a drill can prove
they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

CouchDB is a server, but a drill never dials it from outside: the adapter
starts it inside the sandbox and drives it over loopback with `curl`,
through the core-mediated `exec` verb. Nothing is published, and a drill
runs under `--network none` (measured).

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `couchdb_data_tar` | a tar of CouchDB's data directory |
| `couchdb_data` | one copy of that directory — the tree holding `_dbs.couch`, `_nodes.couch` and `shards/` |
| `couchbackup` | one [`couchbackup`](https://github.com/IBM/couchbackup) file |
| `couchbackup_dir` | a directory of them; the newest by file time is restored |

PITR does not exist for a CouchDB backup; the probe declares `pitr: false`
so the core refuses a `target.pitr` drill before anything runs.

No CouchDB artifact records when it was taken — a `couchbackup` header
names the tool and mode but carries no clock, and the shard filenames carry
the database's own creation instant rather than the backup's (both
measured) — so `backup.created_at` is always null, directories rank by
modification time, and `source.params.backup_timezone` is refused rather
than silently ignored.

## The sandbox must start idle

```yaml
sandbox:
  provider: docker
  params:
    image: couchdb:3.5.2
    command: sleep infinity   # the adapter owns the engine's whole lifetime
    memory: 512m
source:
  kind: couchbackup
  path: /backups/orders/nightly.jsonl
options:
  database: orders            # what the checks will query
```

`command: sleep infinity` is not optional. CouchDB reads its database
registry (`_dbs.couch`) once at startup and caches it, so a data directory
placed underneath a *running* server is invisible to it — measured: the
shards are on disk and the database answers **HTTP 404**. The adapter
therefore places the artifact first and starts the engine afterwards.

The official image needs no wrapper: it already carries a POSIX shell and
`curl`, which is everything the adapter drives the engine with.

**The engine's admin password is a documented constant**, not a secret.
CouchDB 3.x refuses to run without an administrator, and the core's
ephemeral per-drill secret cannot be used for one — its value must never
cross the protocol (adapter protocol §2.5), yet setting an engine password
requires exactly that. So the adapter starts the engine itself with a fixed
account, the CouchDB equivalent of the postgres adapter's `pg_hba` trust
overwrite: publicly known access, confined to a sandbox with zero ingress.
The credential never protects anything reachable.

## What a check looks like

CouchDB speaks HTTP, not SQL, so a check is written in what the engine
answers to: a **path with a query string, relative to the restored
database**.

| check text | what it asks |
| --- | --- |
| `_all_docs?limit=0` | how many documents the database holds |
| `_design/reports/_view/by_day?group=true` | a view's rows |
| `_all_docs?startkey="order-"&endkey="order-￰"&limit=0` | a keyed subset's size |

The runner takes the engine's HTTP status as the verdict — `curl` exits 0
for a 404 as readily as for a 200, so the status is what decides — and
reduces the body to the one number CouchDB states where it states one
(`total_rows` or `doc_count`), passing it through otherwise.

## What a drill can prove here, and what it cannot

**No CouchDB artifact declares how much it should hold.** That is a
property of the engine's formats, not a gap in this adapter, and it shapes
what the restore can promise.

- **A `.couch` shard file truncated at its tail** is opened without
  complaint. Its header sits at the *end*, so the engine falls back to the
  last valid one and serves an older, smaller database — measured: HTTP
  200 and **280 documents of 500**, with no warning anywhere.
- **A `couchbackup` file cut between two lines** is a shorter backup as far
  as any reader can tell: the format writes nothing at its end.

So the restore's verdict is the restored database's **document count**, and
a well-formed zero is refused: a restore that produced nothing has proven
nothing. What that catches is an empty or absent database. What it cannot
catch is an artifact that is merely *short*.

Two things do close part of the gap, and both are real:

- A `couchbackup` file cut **inside** a line has no final newline. The
  adapter counts the batches before the transfer and holds the replay to
  that number, so a mid-batch cut is refused before a byte moves.
- A data directory without `_dbs.couch` is refused: CouchDB reads that file
  to learn which databases exist, and a tree without it serves none of them
  (measured).

**For the rest, write a row-count check.** The core has generating built-in
checks for exactly this, and against an engine whose backups do not state
their own size, the drill's own assertion is what proves completeness.
`options.database` names the database those checks query, and for the
data-directory kinds the adapter refuses if that database is not there
after the restore.

## Compaction is suspended for the drill's duration

CouchDB's compactor (`smoosh`) runs unbidden, and compaction is precisely
the operation that drops old revisions and the bodies of deleted documents:
it can only subtract from what the backup holds. Every start therefore
empties smoosh's `db_channels` and `view_channels`, which stops it
enqueueing anything (measured).

It is a suspension, not a rewrite. An explicit `POST /<db>/_compact` still
works: what a drill must not do is let the engine decide, not stop an
operator from asking. A check reading the compaction settings still sees
what the operator declared.

## Drill config options

| Option | Default | Meaning |
|---|---|---|
| `user` | `admin` | The administrator account the adapter creates in the sandbox. |
| `database` | `restored` | For the `couchbackup` kinds, where the batches are replayed. For the data-directory kinds, the database that must be present after the restore — name the one your checks query. |

## Behavior notes

- **Revisions are preserved.** The replay posts each batch with
  `new_edits=false`, so documents come back under the revisions the backup
  recorded rather than freshly minted ones. A restore that renumbered every
  revision would not be the database that was backed up.
- **What `restore_seconds` measures**: for a `couchbackup`, the engine's
  start plus every batch POST; for a data directory, the placement plus the
  start. The document count that decides the verdict runs after it.
- **Attachments** are whatever the artifact carries. `couchbackup`'s
  default excludes them (`"attachments":false` in its header line); a
  drill restores what it is given and proves that, no more.

## Deliberately not here

- **`_all_docs` exports.** The output of `_all_docs?include_docs=true` is
  not `_bulk_docs` shaped and needs restructuring, and the official image
  carries no `jq`, `python3` or `node` to do it with (measured). Rebuilding
  it inside the adapter would mean holding a whole database in memory on
  the drill host — the shape of a hazard this project has already fixed
  once.
- **Replication as a source.** It needs a live server to replicate from,
  which is a running database rather than a backup.
- **Compressed artifacts.** A gzip-compressed backup is refused by name;
  decompress it first.
