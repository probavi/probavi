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

Just as important: an evidence record names the adapter its drill ran.
A MariaDB restore recorded as `adapter.name: mysql` would be a claim an
auditor could fault, and the record is the product.

## When MariaDB drills started

`docs/capabilities.json` records this adapter's `since` as **0.7.0**, and
that is the release from which a MariaDB restore is evidenced as a MariaDB
restore. MariaDB dumps were restorable before it — the mysql adapter has
accepted `mysqldump` SQL since 0.1.0 — but those drills recorded
`adapter.name: mysql`, which is the claim the split exists to correct. The
field dates the adapter, never the engine (`docs/capabilities.md` §3).

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `mariadb_dump` | one SQL dump, plain or gzip-compressed |
| `mariadb_dump_dir` | a directory of them; `params.select` picks which one — `newest` by the time each dump records in its own trailer (the default), `oldest`, or `random` |
| `mariadb_backup` | an unprepared `mariadb-backup` full-backup directory (physical restore) |
| `mariadb_backup_with_binlogs` | a directory holding such a backup (`params.backup`) and the binary logs written after it (`params.binlogs`) — the full is restored, then the logs are replayed. The only kind here that supports **point-in-time recovery**. |

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

### Which backup in the retention window

With `mariadb_dump_dir` the adapter picks the artifact, and
`params.select` says which one. `newest` — the default, and what every
drill written before this parameter existed does — proves last night. A
drill that only ever does that says nothing whatever about the oldest
backup still in the window, which is the one an incident reaches for once
it is clear the damage predates yesterday: a rotated encryption key, bit
rot on colder media, a format the current tooling no longer reads.

```yaml
source:
  kind: mariadb_dump_dir
  path: /backups/shop
  params:
    select: oldest        # newest (default) | oldest | random
```

Whichever policy is asked for, candidates are ordered by the time each
dump records in its own `-- Dump completed on` trailer, never by file
modification time — copying a backup in resets that, and a stale artifact
would then look like the newest thing in the directory. Two properties
follow, and both are worth knowing before choosing a policy.

**`oldest` is not `newest` turned around.** The rule that a datable dump
outranks an undatable one does *not* invert: a dump taken with
`--skip-dump-date` is not "the oldest backup", it is the one nothing is
known about, and it loses under either policy. Only the comparisons after
that one turn around — earlier trailer, then older file, then the smaller
name.

**`random` is not reproducible, and does not need to be.** It draws
uniformly from the dumps carrying a trailer date, which keeps a draw away
from the stray file a backup directory collects (a `SHA256SUMS`, a lock
file). What was restored is still recorded: `source.params` never enters
an evidence record, but `backup.checksum`, `backup.size_bytes` and
`backup.created_at` do, and those name the artifact. A scheduled drill
choosing randomly covers the whole window over time.

Ordering a directory of **compressed** dumps decompresses every candidate
to reach its trailer, so every policy pays the same price — the directory
has to be ordered before either end of it can be named. Naming the file
outright with `mariadb_dump` skips it entirely. And `select` on a kind
that chooses nothing — `mariadb_dump` or `mariadb_backup` — is refused
rather than ignored, because a parameter nothing reads is a config the
operator believes in and a drill doing something else.

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

## Point-in-time recovery (`mariadb_backup_with_binlogs`)

A full backup proves one moment. Everything written after it — the hours
an incident actually spans — is outside the drill unless the logs that
recorded it are replayed too, and a recovery run-book that ends at the
full is not the run-book anyone follows at 3am.

```yaml
target:
  source:
    kind: mariadb_backup_with_binlogs
    path: /backups/mariadb/2026-09-25  # holds both members
    params:
      backup: full                    # the mariadb-backup --target-dir output
      binlogs: binlogs                # the logs written after it
  pitr:
    target_age: 6h                    # or target_time, an absolute instant
```

**Why one directory and two names.** The core hands an adapter only files
belonging to the drill's configured source (adapter protocol §4.2), which
exists so an adapter — a third-party binary — cannot copy arbitrary host
files into a sandbox it controls. A server's live binary log directory is
therefore not something a drill can point at: an archive copies the logs
beside the full they follow, which is the layout a run-book wants anyway.
Both members are named explicitly rather than recognised by layout, so
renaming a directory cannot silently change what a drill proves.

**Where the replay starts.** From the backup itself.
`mariadb-backup --backup` writes a binlog-info file naming the log file
and position the server had reached, so the replay begins exactly where
the full stops — no overlap to re-apply, no gap to guess at. A backup
without it was taken from a server with the binary log switched off, and
is refused with that said rather than worked around.

**That file has two names.** MariaDB renamed the `xtrabackup_*` metadata
files at 11.0 — the same rename this adapter already handles for
`mariadb_backup_checkpoints` beside the pre-11 `xtrabackup_checkpoints` —
so a drill reads whichever name the release that took the backup wrote:

| Release | The file |
|---|---|
| 10.11 | `xtrabackup_binlog_info` |
| 12.3 | `mariadb_backup_binlog_info` |

Both measured, both carrying the same `file` TAB `position` line, and the
integration suite restores on every verified release so the pair cannot
quietly stop covering one.

**Where it stops.** At `target.pitr` if the drill asks for one, and at the
end of the archive if it does not — "how far can we actually recover" is a
question worth drilling on its own.

**Two refusals rather than a best effort.** A directory missing the log the
backup named cannot be replayed at all. A gap in the middle is worse: the
replay would succeed, stop early, and leave a signed record claiming a
recovery that skipped whatever the missing log held. Both fail the drill
and name the file.

### The precision this can and cannot give you

**Binary log event timestamps are second-granular**, while a drill's target
is an absolute instant in milliseconds. `--stop-datetime` stops at the
first event *at or after* the target, so the point actually reached can be
earlier and coarser than the point requested. The evidence schema is
already right about this — `drill.pitr_target` records the instant
**requested**, never a claim about the instant reached — but a reader who
is not told will assume otherwise.

The replay runs under `TZ=UTC` and the target is converted to UTC, because
`--stop-datetime` is read in the *client's* local zone. Left to the image's
zone, the same drill config would stop at a different point on a
differently configured host, and a recovery point that depends on the
machine it was proved on is not evidence.

The replay is **positional, not GTID-based**. The binlog-info file may
carry a GTID set as a third field and this adapter ignores it: a
GTID-based replay asks the server to skip what it already has, which is a
different guarantee needing a different proof, and mixing the two would
leave a record that does not say which one it rested on.

### What the record says

The replay's seconds count toward `restore_seconds`, not a phase of their
own: every one of them is time an operator would spend before the database
is usable, which is what the RTO trend is for. `backup.created_at` remains
when the **full** was taken — the logs reach further forward and the record
does not pretend otherwise; how far is what `drill.pitr_target` records.
`state.mode` reads `physical+binlog` rather than `physical`, because it is
a different proof from a full-only restore.

The sandbox image needs `mariadb-binlog` beside `mariadbd` and
`mariadb-backup` — the official mariadb images ship all three, which is
why this kind needs no image built for it.

## The event scheduler is suspended for the drill

MariaDB has no per-row expiry, so its instance of the "a drill must not
run the backup's own data-lifecycle policy" problem is the **event
scheduler**. A dump taken with `--events` carries the operator's
`CREATE EVENT` statements, and a purge event —
`DELETE FROM orders WHERE created < NOW() - INTERVAL 90 DAY` is the
canonical shape — deletes rows in the drill exactly as it does in
production.

Measured: an artifact of ten rows and one such event, restored into a
sandbox whose scheduler was running, held **two rows five seconds after
the restore**. The event arrives `ENABLED`, because a dump preserves the
status the backup recorded.

Whether it runs is a default, and defaults move:

| image | `event_scheduler` |
| --- | --- |
| `mysql:8.4` | **ON** |
| `percona/percona-server:8.4.10` | **ON** |
| `mariadb:10.11` … `12.3` | OFF |

MySQL turned this on in 8.0 after shipping it off for years, which is the
whole argument for pinning rather than trusting the answer: a drill's
independence must not rest on an upstream default. On the MariaDB side the
same loss is one sandbox parameter away — a sandbox started with
`--event-scheduler=ON` loses the same eight rows in four seconds
(measured).

So the drill pins it, in the way each restore path allows:

- **Logical kinds** restore into a server this adapter did not start, so
  the pin is `SET GLOBAL event_scheduler = OFF` issued before the load.
  That ordering is what makes it deterministic — the events do not exist
  until the dump creates them.
- **The physical kind** restores a data directory that already holds the
  event definitions and then starts the server itself, where a statement
  would be racing the scheduler. There the pin is a startup flag, and the
  same query verifies afterwards that the server agrees.

A server started with `--event-scheduler=DISABLED` is left alone: it
cannot run events and cannot be told to stop either — the statement fails
against it with ERROR 1290 (measured), and refusing the safest state there
is would be absurd.

**The artifact is untouched.** The events keep the definitions and the
`ENABLED` status the backup recorded, so a check reading
`information_schema.events` sees the operator's own schedule. Only their
execution is suspended, for the life of the sandbox. If the engine will
not suspend it, the drill fails rather than producing a record whose
contents depend on how long the restore took.

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
