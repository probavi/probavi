# probavi-adapter-clickhouse

Restores ClickHouse backups into a disposable sandbox so a drill can prove
they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `clickhouse_backup` | one native backup archive |
| `clickhouse_backup_dir` | a directory of them; the archive whose own manifest records the newest backup time is restored |

Both kinds expect the **archive** form, which is what you get when the
backup destination ends in `.zip`:

```sql
BACKUP DATABASE shop TO File('shop-2026-08-14.zip');
```

`BACKUP … TO Disk('backups', 'shop')` without the suffix writes an
unpacked directory tree instead, and this adapter does not read it: one
artifact means one checksum and one backup identity in the evidence
record, and a tree hash of a directory somebody may still be writing into
is a weaker claim than the bytes of a finished file.

The archive is restored with `RESTORE ALL`, which covers every artifact
shape — measured against ClickHouse 26.3, it restores a `BACKUP DATABASE`
archive as readily as a `BACKUP ALL` one — so the adapter never has to
know what you backed up.

`clickhouse-backup` (the community tool) writes its own layout and is not
supported. Neither is PITR: ClickHouse has no point-in-time recovery to
drive, and the probe declares `pitr: false` for both kinds so the core
refuses a `target.pitr` drill before anything runs.

## Sandbox image

The image must contain `clickhouse-server` and `clickhouse-client` — the
official `clickhouse` images do. Run it with its own entrypoint:

```yaml
sandbox:
  provider: docker
  params:
    image: clickhouse:26.3
    memory: 2g
```

Nothing else is required. The server needs no configuration this adapter
adds, and it starts on its own; the adapter waits for it to answer a query
rather than for the container to be running, which are different moments.

**The server runs unauthenticated.** ClickHouse's default account has no
password in these images, and that is acceptable here for exactly one
reason: a Probavi sandbox is zero-ingress — `--network none`, no ports
expressible in the drill config — so nothing outside it can reach the
restored production data. On any other network setting that reasoning
does not hold, and neither does this adapter's safety story.

## What the drill measures, and what it cannot

`backup.created_at` in the evidence record is **real** for this engine: a
ClickHouse archive carries a `.backup` manifest whose header records the
moment the `BACKUP` statement ran (measured against 26.3). What it does
not carry is an offset — the timestamp is the *server's* wall clock — so
the drill config has to say where that server stood:

```yaml
source:
  kind: clickhouse_backup
  path: /backups/shop-2026-08-14.zip
  params:
    backup_timezone: Europe/Budapest
```

The zone is named rather than given as a number because the offset depends
on the date: a January backup in Budapest is `+01:00` and a July one
`+02:00`. Without the declaration the adapter reports **no** creation time
and the record's `created_at` is null — which is the honest answer. A wall
clock recorded as if it were UTC would be a specific, signed, wrong
instant.

The same timestamp is what ranks a directory: `clickhouse_backup_dir`
chooses by what each archive says about itself, never by file
modification time, because copying a backup without preserving timestamps
makes a month-old artifact look like last night's.

## Checks

ClickHouse speaks SQL, so the core's built-in checks reach it unchanged —
`table_exists`, `row_count` and `freshness` all work, and so do raw `sql:`
checks:

```yaml
checks:
  - kind: row_count
    table: shop.orders
    min: 1
  - name: revenue_present
    sql: SELECT count(*) FROM "shop"."orders" WHERE total > 0
    min: 1
```

Timestamps come back as `2026-08-14 14:37:45`, which the core reads as
UTC — the documented behaviour for naive timestamps in `freshness` checks.

## Retention is pinned off, because a drill proves the artifact

A `TTL` clause states what a *running* server should keep. A drill proves
what a backup holds, and the two disagree the moment a backup spends a
night in storage: rows that were inside their TTL when the `BACKUP`
statement ran are past it by the time the drill reads them, and the
restored server deletes them exactly as a production server would.

Measured on both verified images, restoring an artifact whose TTL had
elapsed while it sat on disk — these are the counts a check reads seconds
after `RESTORE ALL` reports `RESTORED`:

| table | in the backup | in the drill |
| --- | --- | --- |
| row TTL, all rows expired | 60 | **0** |
| row TTL, some rows expired | 200 | **146** |
| `TTL … GROUP BY` (rollup) | 60 | **10** |
| column TTL on a payload column | 60 | 60, payload **blanked** |
| no TTL (control) | 60 | 60 |

Nothing reports any of it. `RESTORE ALL` succeeds, the healthcheck passes,
and the drill goes green having proved less than the backup holds — the
column-TTL row is the quiet extreme, where even a row census would see
nothing wrong. So the sandbox is pinned:

```sql
SYSTEM STOP TTL MERGES
```

**The restore therefore runs in two passes.** The lock covers the tables
that exist when it is issued — measured: a table locked while it existed
kept rows fifty seconds past a thirty-second TTL and lost every one of
them within five seconds of the lock being released, while a table created
*after* the same statement was not covered at all. Since `RESTORE ALL`
creates the tables, a lock before it holds nothing and a lock after it is
too late: the engine logs the TTL merge in the same second as the restore.
So the structure is restored first (`SETTINGS structure_only = true`), the
lock is taken, and the data follows. The extra pass reads metadata and
nothing else — 143 ms + 264 ms against a single 146 ms pass on the test
fixture, either way dwarfed by transferring a real archive.

The lock is a runtime action, not a schema change: `SHOW CREATE TABLE`
after a drill is byte-for-byte what the backup carried, so a check reading
the table definition sees the operator's retention policy exactly as it
was. If the engine refuses the statement, the **drill fails** rather than
proceeding — a record whose contents depend on how much of the backup had
aged past its TTL by restore time is not evidence.

## Two things that surprised the implementation

Both are measured, both shape the code, and both are the kind of detail
that would otherwise be rediscovered as a failing drill at 3 a.m.

**The client logs a DNS error on every invocation.** A zero-ingress
sandbox has no name resolution, and `clickhouse-client` looks up its own
hostname at startup. The lookup fails, a stack trace lands on **stderr**,
and the query then runs perfectly. Nothing is wrong; the adapter keys on
exit codes and stdout, and picks the engine's own diagnostic out of stderr
by its `Code:` prefix rather than treating the noise as a failure. Passing
`--host 127.0.0.1` is what makes the connection work at all — without it
the client has nowhere to go.

**The transferred archive needs to be world-readable.** Sandbox commands
run as root while the server runs as its own account, so the protocol's
default `0600` produces a file the engine cannot open — and the failure
surfaces as `CANNOT_OPEN_FILE` from the server, which reads like a broken
backup rather than a permission the adapter chose. The archive is
transferred `0644` into the server's backup directory, which is created if
this server has never taken a backup.

## Where the archive is put

The adapter asks the server for its data path
(`system.server_settings`) and appends `backups`, the default
`backups.allowed_path`. It does not hold the location as a constant: the
official PostgreSQL image moved its data directory in version 18 and broke
exactly that assumption in the sibling adapter, and there is no reason to
expect ClickHouse to be different forever.

If a server cannot answer, the documented default is used, and if that
server also overrides `backups.allowed_path`, the restore fails with the
engine's own message about it — which is a better diagnosis than a guess.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| a directory holds no readable archive | `source_not_found` |
| an unreadable archive is newer than the one chosen | `source_unreadable` |
| the engine cannot unpack the archive | `source_corrupt` |
| the engine restored nothing, or refused for engine reasons | `restore_failed` |
| the engine will not stop its TTL merges | `invalid_request` |
| the server never answered a query | `engine_not_ready` |

The third one is worth a sentence. A backup job still writing its zip
leaves an archive with no central directory — unreadable, and newer than
everything else in the directory. Skipping it and restoring last night's
backup instead would leave an evidence record the operator reads as
covering tonight's, so the drill refuses instead. An unreadable file that
is *older* than the chosen archive is ignored: it cannot be the backup
anyone meant, and one broken artifact should not block every future drill.
