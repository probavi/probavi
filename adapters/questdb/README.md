# probavi-adapter-questdb

Restores a [QuestDB](https://questdb.io) backup into a disposable sandbox
and proves the engine serves it, for [Probavi](../../README.md). It speaks
`probavi-adapter/0` ([spec](../../docs/adapter-protocol.md)) and is
stdlib-only Go with no imports from the core.

## What a QuestDB backup is

The whole data root — `conf`, `db`, `public` and the `.checkpoint` marker
— copied while a checkpoint is held:

```sql
CHECKPOINT CREATE;   -- then copy /var/lib/questdb somewhere
CHECKPOINT RELEASE;
```

Two things follow, and both are measured on `questdb:10.0.1`:

- **The copy is large and always will be.** A data root holding 250 rows
  measures **328 MB**, because QuestDB preallocates a 16 MiB file per
  column and its own system tables dominate a small database. The files
  are dense, not sparse, so the size is what a copy actually moves.
- **The artifact states its own provenance.** While `CHECKPOINT CREATE` is
  in force, `.checkpoint/db` holds the metadata copy; `CHECKPOINT RELEASE`
  empties it and leaves the directory behind. The marker is therefore the
  *files*, never the directory.

## Source kinds

| Kind | Artifact |
| --- | --- |
| `questdb_checkpoint` | One data root copied while a checkpoint was held |
| `questdb_checkpoint_dir` | A directory of them; the newest by file time is restored |
| `questdb_data` | One data root copied with no checkpoint held |

`questdb_checkpoint` **refuses** a copy whose `.checkpoint` is empty, and
names `questdb_data` in the refusal. The two kinds restore identically;
what differs is the claim. A copy taken between the two statements is the
vendor's backup procedure and is consistent by construction. A copy of a
live data root is whatever the filesystem held at the moment it was read,
and if a table was being written it can be torn — so it drills under a
kind that promises nothing, and its record says which kind ran.

Nothing dates a QuestDB backup: the checkpoint metadata file is 4 KiB of
zeroes on an instance with no configured id, and no other file carries the
instant (measured). `backup.created_at` is therefore always null, and
`questdb_checkpoint_dir` picks by file time because that is the only date
there is.

There is no archive kind. The verified images carry no `tar` (measured),
and an adapter may only place bytes belonging to the configured source, so
nothing could unpack one inside the sandbox.

## The sandbox must be idle

This adapter replaces the data root, which cannot be done under a server
holding those files open, so the drill starts the sandbox idle and the
adapter starts the engine itself:

```yaml
sandbox:
  provider: docker
  params:
    image: questdb/questdb:10.0.1
    command: sleep infinity     # required: the adapter starts the engine
    memory: 1g
  timeout: 30m
```

A sandbox whose engine is already serving is refused up front, naming that
parameter. Memory: **640 MiB restores, 512 MiB does not** — the JVM fails
to set up its writer jobs and the server never comes up (measured on both
verified versions). The engine answers 2.0–2.1 seconds after start under
`--network none`.

Telemetry is switched off for the drill (`QDB_TELEMETRY_ENABLED=false`).
The image ships `telemetry.enabled=true`, and a restored copy of
production data is the last thing that should report anywhere.

## Checks

QuestDB speaks SQL, so the core's generating built-ins apply unchanged —
`table_exists`, `row_count` and `freshness` all work, quoted identifiers
included, and `max(ts)` prints an RFC 3339 instant the freshness check
reads. A `sql` check is one statement, run through the engine's HTTP
endpoint inside the sandbox; the runner drops the CSV header and the
quoting around text values so a check compares the value, not the markup.

```yaml
checks:
  - builtin: service_healthy
  - builtin: row_count
    table: orders
    min: 1
  - name: no-null-prices
    sql: "SELECT count(*) FROM orders WHERE price IS NULL"
    expect: 0
```

## What this adapter cannot prove

**A row count does not prove the data survived.** Measured on 10.0.1: a
column file truncated from 16 MiB to 64 bytes left `count(*)` answering
250 while the column held 8 real values — `sum(id)` came back 36 — with no
error anywhere, in the log or the response. QuestDB serves what the
transaction metadata claims and reads the column file underneath it
without checking that the two agree.

So write at least one check that reads column data: an aggregate over a
value column (`sum`, `avg`, `min`), or a filtered count that has to look at
one. `service_healthy` and `row_count` alone would pass against that
truncated restore.

The restore's own verdict is narrower still, deliberately: the engine must
come up and serve at least one of the tables the artifact holds. A
well-formed zero — a data root with tables that serves none — is refused,
because reporting that green is the failure this project exists to
prevent.

## Data lifecycle

QuestDB tables can declare a TTL, and enforcement drops whole partitions.
It happens on **write**: measured, `ALTER TABLE … SET TTL 1 HOUR` dropped
four of six rows immediately, and inserting a row dated `now()` into a
restored table dropped everything older than the TTL. The smallest unit
the engine accepts is an hour.

A drill only reads, and that is what the measurement shows: a backup nine
days old, whose table declares `TTL 1 HOUR`, restores with every row
present and still holds all of them five seconds later. There is nothing
to suspend — `cairo.ttl.use.wall.clock=false` changed no outcome that
could be produced — so the property is guarded by an integration test
rather than by a switch, and the operator's declared TTL is left exactly as
they wrote it.

## Error codes

| Code | When |
| --- | --- |
| `source_not_found` | the path does not exist, or a directory holds no data root |
| `source_unreadable` | the artifact cannot be read, or is still being copied |
| `source_corrupt` | the directory is not a QuestDB data root, or the restore served no table the artifact holds |
| `unsupported_source` | an unknown source kind |
| `invalid_request` | a checkpoint kind pointed at an unchecked copy, a PITR request, or a sandbox already serving |
| `engine_not_ready` | QuestDB did not answer within three minutes of starting on the restored data root |

## Timings

`transfer` is moving the artifact into the sandbox, `restore` is making it
the server's data root, and `engine_ready` is the server coming up on it.
The engine cannot start before the data is in place, so recovery is those
two in sequence — both are things a real recovery does, and neither is
padded with work that one does not.

## Verified against

`questdb/questdb:10.0.1` (baseline) and `questdb/questdb:9.4.3`. Both
lines take a checkpoint the same way and restore each other's artifacts in
both directions (measured) — which is what those two versions did on the
day, not a supported-version range. QuestDB publishes no end-of-life
dates, so the manifest records none.
