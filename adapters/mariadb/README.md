# probavi-adapter-mariadb

Restores MariaDB backups into a disposable sandbox so a drill can prove
they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

## Why this is not the mysql adapter

The forks have diverged past the point where one adapter can honestly
serve both, and the deciding fact is measured, not aesthetic: **the
official `mariadb:12` image no longer ships `mysql`-named binaries at
all** — `mysql`, `mysqldump` and `mysqld` are gone, only the
`mariadb`-named tools remain. The sibling adapter, which drives the
`mysql` client, cannot even start against the newest MariaDB line. This
adapter drives `mariadb`, `mariadb-dump`, `mariadb-backup` and `mariadbd`
throughout, and works identically on 10.11 where both names exist.

Just as important: an evidence record names the engine its drill proved.
A MariaDB restore recorded as `engine: mysql` would be a claim an auditor
could fault, and the record is the product.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `mariadb_dump` | one SQL dump, plain or gzip-compressed |
| `mariadb_dump_dir` | a directory of them; the dump whose own trailer records the newest time is restored |
| `mariadb_backup` | an unprepared `mariadb-backup` full-backup directory (physical restore) |

Dumps taken with either `mariadb-dump` or its `mysqldump` ancestor are
accepted: both banners are recognised, and both write the same
`-- Dump completed` sign-off (measured on 10.11 and 12.3). Newer 10.11
dumps open with a `/*M!999999\- enable the sandbox mode */` line that the
**MySQL** client chokes on — one more reason these drills run the
`mariadb` client.

PITR via binlogs is not implemented; the probe declares `pitr: false` so
the core refuses a `target.pitr` drill before anything runs.

## Logical restores (`mariadb_dump`, `mariadb_dump_dir`)

```yaml
sandbox:
  provider: docker
  params:
    image: mariadb:10.11
    env.MARIADB_ALLOW_EMPTY_ROOT_PASSWORD: "yes"
    memory: 1g
source:
  kind: mariadb_dump
  path: /backups/shop.sql.gz
  params:
    backup_timezone: Europe/Budapest
```

The empty root password is acceptable for one reason only: a Probavi
sandbox is zero-ingress (`--network none`, no ports expressible), so
nothing outside the disposable container can reach the restored data.

The dump is fed to the client on **stdin** — deliberately not the
client-side `source` command, whose handling has already shifted between
client generations. A compressed dump is streamed through `gzip -dc`
without ever materialising the plain SQL. In both forms the replay is
judged at both ends: the client must succeed **and** the dump must end
with the `-- Dump completed` sign-off it announced itself with, so a
backup job that died mid-dump fails the drill as `source_corrupt` instead
of passing on the fragment that survived. A `--compact`/`--skip-comments`
dump carries no sign-off and is exempt rather than failed.

Options: `database` (default `probavi`), `user` (default `root`), and
`charset`/`collation` to pin the restore target's defaults when the dump
carries no `CREATE DATABASE` of its own.

## Physical restores (`mariadb_backup`)

A physical restore replaces the data directory, so the engine must not be
running: start the sandbox idle and let the adapter own the lifecycle.

```yaml
sandbox:
  provider: docker
  params:
    image: mariadb:10.11
    command: sleep infinity
    memory: 1g
source:
  kind: mariadb_backup
  path: /backups/mariadb-full/
  params:
    backup_timezone: Europe/Budapest
```

Unlike the sibling adapter's XtraBackup flow, **no separate tool image is
needed**: the official `mariadb` images carry `mariadb-backup`, `mariadbd`
and `gosu`. The adapter transfers the unprepared backup, runs
`mariadb-backup --prepare` and `--copy-back`, resets sandbox-local root
auth through an `--init-file` (the restored grant tables carry production
credentials this drill does not have — same rationale as the postgres
adapter's pg_hba overwrite), and starts the server.

One MariaDB-specific mechanic: `mariadbd` has no `--daemonize` (measured —
the option its `mysqld` ancestor has does not exist), so the server is
backgrounded by the shell and readiness is polled. A launch failure
therefore cannot surface in the launch step's exit code; the readiness
timeout path reads the server's own error log and reports the engine's
reason instead of "never became ready".

The backup identity is a canonical tree hash over the whole directory;
`created_at` comes from the `end_time` the backup's own metadata records,
placed in the `backup_timezone` zone — without the declaration it stays
null rather than guessing. Both metadata generations are read: 10.x
writes `xtrabackup_info`/`xtrabackup_checkpoints` (the XtraBackup
ancestry), 11.0 renamed them to `mariadb_backup_info`/
`mariadb_backup_checkpoints` (both measured), and a drill accepts either.

The same metadata names the origin server (`server_version`), and before
anything is transferred the adapter compares it against the sandbox
engine's `mariadbd --version`: a physical backup restores only into its
own release series — 10.11 into 10.11, 11.4 into 11.4
(docs/engine-versions.md §5) — and an impossible pairing is refused up
front as `invalid_request` with a message naming both sides and the image
to use instead. The check refuses only on positive evidence: a backup
without a readable `server_version` simply skips it, and the restore
speaks for itself (with the error-log surfacing above as the diagnostic
of last resort).

## Checks

MariaDB speaks SQL, so the core's built-in checks work unchanged. The
declared `sql_runner` appends `ANSI_QUOTES` to the session `sql_mode`, so
the SQL-standard double-quoted identifiers the core emits are accepted:

```yaml
checks:
  - kind: row_count
    table: shop.orders
    min: 1
  - kind: freshness
    table: shop.orders
    column: created_at
    max_age: 26h
```

For physical restores the connection database is the system schema
(`mysql`) — the only database guaranteed to exist in an arbitrary restored
server — so checks there should use schema-qualified names.

## What is deliberately not here yet

The sibling adapter's `*_with_users` kind (accounts-and-grants script
replayed before the dump, with a principal-chain verification) has no
MariaDB counterpart yet. MariaDB's account and role machinery differs
enough from MySQL 8's that porting the verification without measuring it
would risk proving the wrong thing; it is tracked as follow-up work.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| a directory that is not a mariadb-backup backup | `source_corrupt` |
| the client rejects the file as SQL (`ERROR 1064`, binary garbage) | `source_corrupt` |
| the dump ends without its announced sign-off | `source_corrupt` |
| the restore ran and failed for engine reasons | `restore_failed` |
| a physical restore against a running engine | `invalid_request` |
| the server never accepted connections | `engine_not_ready` |
