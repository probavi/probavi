# probavi-adapter-mysql

The MySQL engine adapter for Probavi, implementing `probavi-adapter/0`
(see `docs/adapter-protocol.md`). Standard library only — deliberately no
imports from the Probavi core; like the postgres adapter, it is written
from the protocol document alone.

## Supported source kinds

| Kind            | Meaning                                                     |
|-----------------|-------------------------------------------------------------|
| `mysqldump`     | One `mysqldump` SQL file, plain or gzip-compressed.         |
| `mysqldump_dir` | A directory of dump files, plain or gzip-compressed; the dump whose own trailer records the newest time is restored. |
| `mysqldump_with_users` | A directory holding an accounts-and-grants script (`params.users`) and one dump — either may be gzip-compressed; the accounts are replayed first, and the drill fails while the restored principal chain is broken. |
| `xtrabackup`    | A Percona XtraBackup full-backup directory (unprepared, as `xtrabackup --backup` leaves it) — a physical restore. |

## Sandbox image and authentication

The sandbox image must contain `mysqld`, the `mysql` client, and
`mysqldump`-compatible tooling — the official `mysql:8.x` images do. The
adapter connects as the configured superuser without a password, so the
image must allow it:

```yaml
sandbox:
  provider: docker
  params:
    image: mysql:8.4
    env.MYSQL_ALLOW_EMPTY_PASSWORD: "yes"
```

An empty root password is acceptable **only** because Probavi sandboxes
have zero ingress by default: `--network none`, and publishing ports is not
expressible at all. The credential never protects anything reachable.

## Restore behavior

- The target database (default `probavi`, override with
  `target.options.database`) is created with `CREATE DATABASE IF NOT
  EXISTS` before the load: plain `mysqldump` output carries no `CREATE
  DATABASE` statement. For dumps taken with `--databases` (which embed
  `CREATE DATABASE`/`USE`), set `options.database` to the dumped schema
  name so the connection info and checks point at the restored data.
- The dump is loaded with the mysql client's `source` command, which stops
  at the first error: partial restores fail loudly as `restore_failed`;
  input the parser rejects (`ERROR 1064`, `ASCII '\0'`) is classified
  `source_corrupt`.

## Compressed dumps

A dump stored gzip-compressed — what `mysqldump … | gzip -c > db.sql.gz`
leaves behind — is restored as it is. Nothing needs declaring: the
adapter recognises the compression from the artifact's **magic bytes**,
never from its name, so renaming a backup cannot change what a drill does
and a `.gz` name over plain SQL cannot break one.

The point is not convenience. Decompressing the artifact outside Probavi
and pointing the drill at the result would make `backup.checksum` cover a
temporary file that is in no backup archive: the record would identify
something nobody keeps, and the link back to the stored backup would rest
on whatever script produced it. Here the checksum and `size_bytes` cover
the bytes actually retained.

What this costs, stated plainly:

- **The sandbox image needs `gzip`.** Every official `mysql:8.x` image has
  it. An image without it fails the drill as `restore_failed` with a
  message naming the image rather than the backup.
- **The decompression happens inside the sandbox**, streamed into the
  client, so the sandbox never needs room for the uncompressed dump.
- **Both ends of that pipeline are judged.** A decompressor that dies
  partway leaves a truncated stream the client may well accept, so its
  status is captured separately; a truncated archive fails the drill as
  `source_corrupt` and never passes as a complete restore.
- **`backup.created_at` still comes from the dump's own trailer** — the
  adapter reads it through the decompressor. See the ranking cost below.

Only gzip is recognised. Other compressors (`zstd`, `xz`, `bzip2`) are
refused as unusable SQL, the same as any other unreadable input.

## The mysqldump_with_users kind (accounts first)

MySQL accounts and grants live in the `mysql` system schema — a
single-database dump never contains them. Restore such a dump alone and
everything succeeds while the application account cannot log in and every
`SQL SECURITY DEFINER` view, routine, trigger, and event fails at
invocation (`ERROR 1449`). A `mysqldump` drill's record therefore proves
data recoverability only. `mysqldump_with_users` makes the drill cover the
whole principal chain:

```yaml
source:
  kind: mysqldump_with_users
  path: /backups/shop              # a directory holding both members
  params:
    users: users.sql               # bare filename inside the directory
    dump: shop-2026-08-08.sql      # optional: without it, the newest-by-trailer non-users file
target:
  options:
    database: shop                 # the SOURCE database name — see below
```

The members are named explicitly (no filename-pattern guessing), and one
directory can hold a shared users script beside several databases' dumps —
one drill per database, each with its own checks and its own evidence
record. `params.users` is required; `params.dump` is optional so a drill
against a rotating backup directory keeps working unattended.

**Restore under the source database name.** MySQL grants are
database-scoped: a faithful script exported from production says `GRANT
... ON `` `shop` ``.*`, and restored under any other name nothing can
reach the target — the account logs in and is denied, definer views fail
with `ERROR 1356`. Set `options.database` to the source database's name.
Getting this wrong is caught, not silently passed: see the gates below.

Export the accounts the way recovery run-books do — `CREATE USER` with the
password hash and `SHOW GRANTS` output. A minimal export, run on the
production server (the `print_identified_with_as_hex` session variable
makes the hash printable, which is what export tooling does too):

```sql
SET SESSION print_identified_with_as_hex = ON;
SHOW CREATE USER 'app'@'%';
SHOW GRANTS FOR 'app'@'%';
-- append ';' to each returned line
```

Scripts from `pt-show-grants` and similar tooling work too, including ones
written with `CREATE USER IF NOT EXISTS` (collisions then produce
warnings, which need no tolerance at all).

### How the replay is judged

The script is fed to the mysql client **through stdin with `--force`**,
deliberately: without it the client aborts at the first failed statement
and silently skips every account after it, so the completeness of the
replay would depend on account ordering — and the `source` client command
aborts even *with* `--force`. Under `--force` the exit code stays 0, and
the verdict comes from classifying stderr. Exactly one failure class is
tolerated — `ERROR 1396` (how a `CREATE USER` collision reports) for
accounts the sandbox engine itself created: `root`, and the reserved
`mysql.`-prefixed system accounts (`mysql.sys`, `mysql.session`,
`mysql.infoschema`), which appear in faithful exports. Any other
diagnostic fails the drill as `restore_failed`.

### The principal-chain gates

After the dump is loaded, the adapter fails the provision — otherwise an
incomplete or mismatched script would reintroduce the very defect this
kind closes:

1. **Orphaned definers**: any view, routine, trigger, or event in the
   restored database whose `DEFINER` account does not exist.
2. **Reachability**: at least one restored account (or role) must hold a
   privilege that reaches the restored database — a grant scoped to it, or
   a global non-`USAGE` privilege. This is what catches the
   wrong-database-name trap, and its message says how to fix it. System
   accounts (`root`, `mysql.*`) do not count: the drill proves the
   *restored* principal layer, not the sandbox's. If no application
   account is supposed to reach this database, the plain `mysqldump` kind
   is the honest choice.
3. **View resolution**: every restored view is `EXPLAIN`ed (no data is
   read); a definer that exists but lacks rights fails here.

### Credentials never reach the record

A users script carries password hashes and possibly plaintext passwords,
and the server quotes the offending source token back in syntax errors.
Every diagnostic bound for a protocol message is therefore scrubbed
(`IDENTIFIED ...` literals, `$A$...` hash literals, and long hex literals
are redacted) before it can reach a signed evidence record.

## What the mysqldump and mysqldump_dir kinds do not prove

A passing `mysqldump` or `mysqldump_dir` drill proves the dump loads and
its data validates — nothing about accounts, grants, or whether definer
objects are invocable. Two more silent gaps live in the dump itself:
`mysqldump` omits stored routines and events unless `--routines`/`--events`
were passed (triggers are included by default), and a dump without
`--databases` carries no `CREATE DATABASE`, so the restore target's
default charset and collation are the sandbox server's, not the source's —
pin them with the `charset`/`collation` options if your checks depend on
collation semantics. If your recovery depends on the application logging
in afterwards, use `mysqldump_with_users`.

## Which backup a drill restores, and when it refuses

When the drill config names a **directory**, the adapter picks the
artifact itself: the dump whose **own `-- Dump completed on` trailer
records the newest time**. The file's modification time is not what ranks
candidates — copying a backup in (`cp` without `-p`, an object-store
download, an `rsync` without `-t`) resets it, and a stale artifact would
then look like the newest thing in the directory. What a dump says about
itself does not move when the file is copied.

Ranking needs no declared zone: two dumps being compared came off the
same backup host, so whatever zone it was in cancels out. Declaring
`params.backup_timezone` is only needed to *report* `backup.created_at`.

A dump the adapter cannot date — taken with `--skip-dump-date` or
`--compact`, so no trailer carries a date — ranks below every dump it
can. Between two such files the previous rule still decides: newest
mtime, ties broken by the larger name.

**Compressed candidates cost a pass each.** A gzip member carries no index
and its header records no usable date (measured: `gzip` zeroes that field
whenever it compresses a pipe, which is exactly the `mysqldump | gzip -c`
shape), so the only way to its trailer is through the whole member.
Ranking a directory therefore decompresses every compressed candidate —
about a second per 60 MiB of compressed data on ordinary hardware, so
seven daily 1 GiB dumps add roughly two minutes before the restore
starts. The alternative was ranking compressed backups by file
modification time, which is the claim this whole section exists to stop
making. Naming the file outright (kind `mysqldump`, or `params.dump`)
skips the ranking entirely and pays the pass only for the artifact the
drill restores. The scan honors the drill's deadline.

Two more things follow, and both are stated here rather than left for an
operator to discover.

**A backup still being written is refused, not skipped.** The newest file
in a backup directory is quite often the one a backup job is writing
right now, and restoring a partial artifact fails in whatever way the
engine happens to fail — a drill reporting trouble against a backup set
that is perfectly healthy. So the adapter looks twice, a moment apart,
and refuses an artifact that changed in between (`source_unreadable`,
with a message that says so). It deliberately does **not** fall back to
the previous backup: that would prove an older backup while the record
implied the newest, and nothing in the evidence would say which one it
was. A backup job that writes to a temporary name and renames on
completion never trips this at all — the directory only ever shows
finished files, and that is the arrangement worth having.

An artifact the config names outright is never second-guessed this way:
the operator chose that file, so the drill restores that file.

## The xtrabackup kind (physical restore)

An XtraBackup restore replaces the data directory, so the engine must not
be running when the drill starts. Requirements:

- **Sandbox image** containing `mysqld`, `xtrabackup`, and `gosu`, with the
  XtraBackup major version matching the server (e.g. built
  `FROM mysql:8.0-debian` + `percona-xtrabackup-80` from the Percona apt
  repository — the integration test builds exactly this).
- **Idle start**: the sandbox must not boot the engine — with the docker
  provider set `command: sleep infinity` in `sandbox.params`. The adapter
  refuses to run against an already-running engine.
- **Unprepared full backup**: the source directory is what
  `xtrabackup --backup --target-dir=...` produced; the adapter runs
  `--prepare` and `--copy-back` itself (both timed as restore work). A
  directory without `xtrabackup_checkpoints` is refused as
  `source_corrupt` before any transfer.

The restored grant tables carry **production credentials the drill does
not have**, so the adapter starts the server with an `--init-file` that
resets `'root'@'%'` to an empty password with full privileges — the MySQL
equivalent of the postgres adapter's `pg_hba.conf` trust overwrite. The
sandbox has no network exposure whatsoever (`--network none`, no published
ports), so this access never extends beyond the disposable container.

Physical mode ignores `options.user`/`options.database`: the connection is
always `root` on the `mysql` system schema (the only database guaranteed
to exist in an arbitrary restored server). Point checks at restored data
with schema-qualified table names (e.g. `table: shop.orders`).

## The ANSI_QUOTES bridge

The core validates and quotes check identifiers in SQL-standard form
(`SELECT count(*) FROM "orders"`). MySQL's default `sql_mode` does not
accept double-quoted identifiers, so the declared `sql_runner` template
appends `ANSI_QUOTES` to the session `sql_mode` via `--init-command`. The
engine dialect is absorbed by the adapter's declaration — the core stays
engine-free, which is exactly what the protocol's sql_runner template
exists for (§6.1).

### When the backup was taken

`created_at` in the evidence record is an absolute instant, and the honest
answer is often "not derivable". A file's modification time is **not** the
backup's creation time: copying a backup without preserving timestamps
(`cp` without `-p`, `rsync` without `-t`, most object-store downloads)
resets it, and a month-old artifact then looks like last night's. This
adapter therefore never reports an mtime as a creation time.

A `mysqldump` file signs itself off with `-- Dump completed on ...`, and
that is what this adapter reads. Dumps taken with `--skip-dump-date` or
`--compact` carry no date, and then there is nothing to read — which is
not a defect, just an absence. An `xtrabackup` directory is dated by the
`end_time` in its own `xtrabackup_info` — the moment the backup finished
and became consistent.

What no backup format records is a UTC offset — the value is the wall
clock of the host that took the backup, and reading it as UTC would be
wrong by that host's offset (measured: a backup taken at 12:08 UTC on a
host in `Asia/Tokyo` is written as `21:08`). The offset is a fact only you
have, so the drill declares it:

```yaml
source:
  kind: mysqldump_dir
  path: /backups/orders
  params:
    backup_timezone: Europe/Budapest    # IANA name, not an offset
```

A **zone name**, not a `+02:00`: the offset depends on the date of the
backup, so a January backup in Budapest is `+01:00` and a July one
`+02:00` — a fixed number in a config file would be wrong for half of
every year. An unknown name fails the drill immediately rather than
quietly dropping the timestamp it was meant to make exact. Zone data is
compiled into the adapter, so no `/usr/share/zoneinfo` is needed on the
host.

Without the declaration, `backup.created_at` in the record is **null** —
the evidence schema provides for that precisely because a backup's own
creation time is not always derivable. One hour a year is genuinely
ambiguous: when clocks go back, the same wall clock happens twice, and
the earlier of the two is chosen.

## Backup identity

- `checksum`: SHA-256 over the selected artifact's bytes **as stored** —
  for a compressed dump, over the compressed bytes, which are the ones the
  backup archive retains (for `mysqldump_dir`, the chosen file). For
  `xtrabackup` directories: a
  canonical tree hash — entries sorted by relative path; each regular file
  contributes `relpath NUL size NUL content`, each symlink
  `relpath NUL "L" target NUL`.
- `created_at`: the backup's own creation time, read from the artifact and
  placed in the zone the drill declares (see above); null when the backup
  carries none or no zone was declared. For `mysqldump_with_users` it
  dates the dump member: an accounts script carries no timestamp of its
  own, so the pair's freshness rests on the member that can be dated.

## Source params

Set under `source.params` in the drill config.

| Param             | Kinds                  | Meaning                                            |
|-------------------|------------------------|----------------------------------------------------|
| `users`           | `mysqldump_with_users` | **Required.** Bare filename of the accounts-and-grants script inside the source directory. |
| `dump`            | `mysqldump_with_users` | Optional. Bare filename of the dump; without it the newest-by-trailer non-users file is used. |
| `backup_timezone` | all                    | Optional. IANA zone name of the host that took the backup (e.g. `Europe/Budapest`). Without it `backup.created_at` is null — see above. |

## Drill config options

| Option      | Default   | Meaning                               |
|-------------|-----------|---------------------------------------|
| `user`      | `root`    | Superuser inside the sandbox engine (logical restores only). |
| `database`  | `probavi` | Database to restore into (letters, digits, underscores only; logical restores only). For `mysqldump_with_users`, set it to the **source** database name — grants are database-scoped. |
| `charset`   | *(server default)* | Character set for the created target database (logical restores; applies only when the adapter creates it). |
| `collation` | *(server default)* | Collation for the created target database — without it a `utf8mb4_bin` source restores under the sandbox server's default collation, changing comparison, ordering, and uniqueness semantics. |

## Environment

Credentials for reading backup sources arrive via the environment variables
declared in the drill config's `source.credential_env` (none are needed for
local files). Secrets never appear in protocol messages or logs.

## Behavior notes

- Engine readiness is probed with a TCP `SELECT 1`: the official image's
  first-boot initialization runs a temporary server with
  `--skip-networking` (socket only), so a TCP probe cannot report ready
  too early — the same trap as PostgreSQL's initdb-phase server.
- MariaDB is expected to work through the same client tooling but is
  untested; treat it as out of scope until it has its own verified
  integration coverage.
- `teardown` has nothing to release — everything this adapter creates
  lives inside the sandbox, which the provider destroys.
