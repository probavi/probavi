# probavi-adapter-mssql

The Microsoft SQL Server engine adapter for Probavi, implementing
`probavi-adapter/0` (see `docs/adapter-protocol.md`). Standard library
only — deliberately no imports from the Probavi core; like the other
in-repo adapters, it is written from the protocol document alone.

## Supported source kinds

| Kind      | Meaning                                                       |
|-----------|---------------------------------------------------------------|
| `bak`     | One native `BACKUP DATABASE ... TO DISK` file.                |
| `bak_dir` | A directory of backup files; the **full** backup whose header records the newest completion time is restored (see below). |
| `bak_with_logins` | A directory holding a server-logins T-SQL script (`params.logins`) and one `.bak`; the logins are replayed first, and the drill fails if any restored SQL user is left without a matching server login. |
| `bak_chain` | A directory of backups replayed as a **chain**: the newest full, its newest differential, and the log backups that follow — the state the backup set can actually recover to. |

## Sandbox image: start it idle

```yaml
sandbox:
  provider: docker
  params:
    image: mcr.microsoft.com/mssql/server:2022-latest
    command: sleep infinity
```

The idle start is **required**, and the reason is evidence integrity, not
convenience: SQL Server refuses to run without a superuser password, the
image only accepts one through environment variables, and sandbox params
are recorded verbatim in signed evidence records — where credentials must
never appear. So the adapter starts `sqlservr` itself and owns the engine
lifecycle. A sandbox whose engine is already running with its own
credentials is refused with a clear error.

By configuring this image you accept Microsoft's EULA for it; the adapter
passes `ACCEPT_EULA=Y` on your behalf when starting the server.

## The sandbox password is a documented constant

The drill engine's `sa` password is `Probavi!DrillSandbox0` — a **public
constant compiled into the adapter**, not a secret. It cannot be the
core's ephemeral per-drill secret: the protocol forbids secret values in
any protocol message (§2.5), and setting an engine password requires
sending the value through one. This is the SQL Server equivalent of the
postgres adapter's `pg_hba` trust overwrite and the mysql adapter's empty
root password: publicly known access, acceptable **only** because Probavi
sandboxes have zero ingress — `--network none`, and publishing ports is
not expressible at all. The credential never protects anything reachable.

## Which backup a drill restores

A real SQL Server backup directory holds full, differential, and
transaction log backups side by side, and the newest file is typically a
log backup — which **cannot create a database**. Picking by modification
time alone therefore fails a perfectly restorable backup set.

Nothing outside the engine can tell the types apart: the `.bak`/`.trn`
extensions are pure convention (SQL Server ignores them), and all three
types share one media format. So the adapter asks the engine. Every
candidate is transferred into one scratch path and identified with
`RESTORE HEADERONLY`, and the **full backup whose header records the
newest completion time** is the one restored — then, and only then, is it
transferred to the path the restore reads. Files that do not start like
backup media — checksum sidecars, log files — are skipped without being
transferred, and named in the failure message if nothing is restorable.

Ranking by the header rather than by the file's modification time matters
because copying a backup in (`cp` without `-p`, an object-store download,
an `rsync` without `-t`) resets the file time: a stale artifact would
otherwise look like the newest thing in the directory. What the header
records does not move when the file is copied. Ranking needs no declared
zone — the backups being compared came off the same server, so whatever
zone it was in cancels out; `params.backup_timezone` is only needed to
*report* `backup.created_at`. Media whose header carries no completion
date ranks below media that does, and between two such files the older
rule still decides: newest file first, ties broken by the larger name.

The cost is one probe per candidate, where an earlier version could stop
at the first full backup it found. Probing is how the drill *finds* the
backup; only the transfer that feeds the restore is counted as recovery
time.

Two consequences worth knowing:

- **A drill restores the newest full backup, not the latest state.** The
  differential and log backups taken after it are not applied — restoring
  a chain is a different feature, and this kind does not claim it.
  `backup.created_at` in the evidence record is the chosen full backup's
  timestamp, so the record never implies more than that.
- **Backup media may hold several backup sets** (SQL Server appends unless
  told to overwrite). The newest full set is restored explicitly with
  `WITH FILE = n`; without that, the engine would take the *first* set on
  the file, which is the oldest backup on it.
- A backup file the engine cannot read fails the drill as `source_corrupt`
  rather than falling back to an older one: a corrupt newest backup is
  exactly what a drill exists to surface.

**A backup still being written is refused, not skipped.** The newest file
in a backup directory is quite often the one a backup job is writing right
now, and a truncated backup still reads as a valid one — measured:
`RESTORE HEADERONLY` accepts it without complaint. So the chosen artifact
is looked at twice, a moment apart, and one that changed in between is
refused (`source_unreadable`, with a message that says so). The adapter
deliberately does **not** fall back to the previous backup: that would
prove an older backup while the record implied the newest, and nothing in
the evidence would say which one it was. A backup job that writes to a
temporary name and renames on completion never trips this at all — the
directory only ever shows finished files, and that is the arrangement
worth having. An artifact the config names outright is never
second-guessed this way: the operator chose that file.

Selection is not part of the measured recovery time. `transfer_seconds`
counts the chosen artifact's transfer only — an operator recovering for
real reads their backup catalogue instead of probing.

## The bak_chain kind (the whole backup set, not just the full)

A SQL Server backup set is a chain: a full backup, then differentials,
then transaction log backups. `bak_dir` restores the newest **full** and
stops there, which is honest but proves less than the recovery it stands
for — measured on a directory holding one full, two differentials and
four logs, the full alone recovers **1 row** and the chain recovers all
**7**.

```yaml
source:
  kind: bak_chain
  path: /backups/shop
  params:
    database_name: shop     # only when the directory holds more than one database
```

### How the chain is built

Not from file names — those are convention SQL Server ignores — but from
the log sequence numbers in each backup's header, and the relationships
were measured rather than assumed:

- every differential and log carries the **checkpoint of the full it
  builds on**, which is what ties one chain together and keeps two fulls'
  chains apart, even in the same directory;
- log backups cover **contiguous ranges**, each one starting where the
  previous ended, so the chain is followed by carrying a redo point
  forward rather than by sorting on file times.

The order is: the newest full, its newest differential (if any), then the
logs from there. Every member restores `WITH NORECOVERY` and the last one
`WITH RECOVERY`, so the database becomes usable exactly once, at the end.
Intermediate logs the differential already covers are not replayed.

### What fails the drill

- **A gap in the log sequence.** The failure names the point the restore
  reaches and the log that starts too late, so the missing backup is
  identifiable. Stopping quietly at the last reachable log would leave
  the record claiming a chain restore that ended early.
- **Several databases with no `database_name`.** One directory can hold
  backups of many; picking for you would mean a record naming a database
  nobody chose.
- **Backup media the engine cannot read**, exactly as for the other
  kinds — falling back would build a chain out of whatever happened to
  parse.

### Costs and limits

Every candidate is transferred once to be identified, through a single
scratch path the next probe overwrites, so the sandbox never holds the
whole directory. The chain's own members are then transferred to stay —
and **all of them count as the measured recovery**, unlike the probing,
because replaying them is the recovery this drill performs.

`backup.checksum` covers **every member in restore order**; a checksum
blind to the logs would let the data the drill actually recovered change
without the record noticing. `created_at` is the last member's completion
time — a chain is only as current as the log it ends on.

Point-in-time recovery is **not** part of this kind, and the reason is
measured: `RESTORE LOG ... WITH STOPAT` beyond the available logs exits
successfully while leaving the database in the restoring state, which
would look like a passing drill on a database nobody can open. `STOPAT`
belongs to a design that handles that outcome explicitly; the probe still
declares `pitr: false`.

## Restore behavior

- The backup's file list is read with `RESTORE FILELISTONLY` and every
  logical file is `MOVE`d to a fresh path under `/var/opt/mssql/data/` —
  the paths inside the `.bak` belong to the production server, not this
  sandbox. Logical names are quoted defensively on the way into T-SQL.
- The database is restored **under the drill's target name** (default
  `probavi`, override with `target.options.database`), regardless of the
  name it carried in the backup — `connection.database` and your checks
  point at it directly.
- Backup media the engine rejects (Msg 3241 "incorrectly formed",
  Msg 3254 "volume ... is empty", Msg 3242 "not a valid ... backup set")
  is classified `source_corrupt`; restores that run and fail are
  `restore_failed`.
- SQL Server's version rule is asymmetric: a backup restores onto an
  engine of the same or a newer major — that is the supported upgrade
  path — but never onto an older one. The backup header states its origin
  (`SoftwareVersionMajor`, read from the same `RESTORE HEADERONLY` the
  selection uses), so a newer backup handed to an older engine is refused
  before the restore is attempted, as `invalid_request` with a message
  naming both product versions (docs/engine-versions.md §5). Only the
  downgrade is refused, and only on positive evidence — a header that
  does not state a version skips the check.

## The bak_with_logins kind (server logins first)

SQL Server splits principals across two scopes: **logins** live in
`master`, **database users** live inside each database, linked by SID. A
`.bak` carries the users but never the logins — restore it alone and every
SQL user whose login is missing comes back **orphaned**: `RESTORE` succeeds,
checks pass, and the application still cannot log in. A `bak` drill's
record therefore proves data recoverability only. `bak_with_logins` makes
the drill cover the whole principal chain:

```yaml
source:
  kind: bak_with_logins
  path: /backups/shop              # a directory holding both members
  params:
    logins: logins.sql             # bare filename inside the directory
    bak: shop-2026-08-08.bak       # optional: without it, the newest full backup in the directory
```

The members are named explicitly (no filename-pattern guessing), and one
directory can hold a shared logins script beside several databases'
backups — one drill per database, each with its own checks and its own
evidence record. `params.logins` is required; `params.bak` is optional so
a drill against a rotating backup directory keeps working unattended.

Export the logins the way recovery run-books do — `CREATE LOGIN` with the
password hash and the **original SID**, so restored users re-link without
repair. A minimal export, run on the production server:

```sql
SET NOCOUNT ON;
SELECT 'CREATE LOGIN [' + name + '] WITH PASSWORD = '
     + CONVERT(varchar(max), LOGINPROPERTY(name, 'PasswordHash'), 1)
     + ' HASHED, SID = ' + CONVERT(varchar(max), sid, 1)
     + ', CHECK_POLICY = OFF;'
FROM sys.sql_logins
WHERE name NOT LIKE '##%' AND name <> 'sa';
```

Tool-generated scripts (`sp_help_revlogin` descendants, dbatools
`Export-DbaLogin`) work too. Two rules about script content:

- The script is replayed as `sa`; it must not `ALTER` or disable the `sa`
  login the adapter operates with — doing so fails the drill loudly.
- Windows logins (`FROM WINDOWS`) cannot be created in a sandbox with no
  domain; leave them out. The orphan check ignores Windows users for the
  same reason (see below).

### How the replay is judged

The script is replayed with `sqlcmd -i` **without** `-b`, deliberately:
with `-b`, sqlcmd stops at the first failed batch and silently skips every
login after it, so the completeness of the replay would depend on login
ordering. Without it a failure stops nothing, the exit code stays 0, and
the verdict comes from classifying stderr instead. Exactly one failure
class is tolerated — `Msg 15025` ("The server principal ... already
exists") for principals the sandbox engine itself created: the `sa` login
and the `##...##`-wrapped internal principals SQL Server setup installs
(both `##MS_Policy...##` logins appear in `sys.sql_logins`, so faithful
exports carry them). Any other diagnostic fails the drill as
`restore_failed`.

### The orphan gate

After the restore, the adapter queries the restored database for SQL users
(`type = 'S'`, `authentication_type = 1`) whose SID matches no server
login, and **fails the provision** if any exist — otherwise an incomplete
logins script would reintroduce the very defect this kind closes. Windows
principals, contained users, and `WITHOUT LOGIN` users are outside the
gate: the first cannot exist in the sandbox, the rest need no login.

### Credentials never reach the record

A logins script carries password verifiers, and the engine quotes the
offending token back in syntax errors — including a full password hash.
Every engine diagnostic bound for a protocol message is therefore scrubbed
(`PASSWORD = '...'` literals and long hex literals are redacted) before it
can reach a signed evidence record.

## What the bak and bak_dir kinds do not prove

A passing `bak` or `bak_dir` drill proves the database restores and its
data validates — nothing about server-level objects: logins, server roles,
linked servers, jobs. If your recovery depends on the application logging
in afterwards, use `bak_with_logins`, or add a user-defined check with the
orphan query from this README against your restored database.

## The dialect bridges

The core validates and quotes check identifiers in SQL-standard form
(`SELECT count(*) FROM "orders"`) and expects undecorated result rows.
The declared `sql_runner` absorbs both SQL Server quirks declaratively
(§6.1), so builtin checks work unchanged:

- `-I` turns on `QUOTED_IDENTIFIER`, accepting double-quoted identifiers
  (sqlcmd's default is off);
- `SQLCMDINI` points at a startup script the adapter writes during
  provision (`SET NOCOUNT ON`), which removes the `(N rows affected)`
  trailer from sqlcmd's stdout — the sqlcmd equivalent of the mysql
  adapter's `--init-command` bridge.

Checks are plain T-SQL against the restored database.

### When the backup was taken

`created_at` in the evidence record is an absolute instant, and the honest
answer is often "not derivable". A file's modification time is **not** the
backup's creation time: copying a backup without preserving timestamps
(`cp` without `-p`, `rsync` without `-t`, most object-store downloads)
resets it, and a month-old artifact then looks like last night's. This
adapter therefore never reports an mtime as a creation time.

The backup header the adapter already reads to pick the right backup set
(see above) also carries `BackupFinishDate`, and that is what dates the
backup — the set that was actually restored, not the file that held it.

What no backup format records is a UTC offset — the value is the wall
clock of the host that took the backup, and reading it as UTC would be
wrong by that host's offset (measured: a backup taken at 12:08 UTC on a
host in `Asia/Tokyo` is written as `21:08`). The offset is a fact only you
have, so the drill declares it:

```yaml
source:
  kind: bak_dir
  path: /backups/shop
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

- `checksum`: SHA-256 over the selected artifact's bytes — the file the
  engine identified as holding a full backup, not merely the newest file.
- `created_at`: the restored backup set's own `BackupFinishDate`, placed
  in the zone the drill declares (see above); null when no zone was
  declared. For `bak_with_logins` it dates the backup member: a logins
  script carries no timestamp of its own, so the pair's freshness rests on
  the member that can be dated.

## The engine disables temporal retention on restore

SQL Server's per-row expiry is a system-versioned temporal table with a
`HISTORY_RETENTION_PERIOD`, cleaned by a background task. A drill must not
run one — the artifact is what it is proving.

It does not, and the reason is the engine's own: measured on both verified
versions, `is_temporal_history_retention_enabled` is **1 on the source and
0 in the drill**. SQL Server turns the cleanup task off when a database is
restored, while the table goes on declaring the operator's period, so a
check reading `sys.tables` sees the truth:

| | on the source | in the drill |
| --- | --- | --- |
| `is_temporal_history_retention_enabled` | 1 | **0** |
| the table's declared `HISTORY_RETENTION_PERIOD` | 1 DAY | 1 DAY |
| history rows aged past it | 50 | 50 |

**SQL Agent answers itself.** Its jobs live in `msdb`, and a drill restores
one user database — measured, `msdb.dbo.sysjobs` is empty in the sandbox
and the Agent service is not running in the official Linux images.

This adapter therefore needs none of the pins its siblings grew in the
data-lifecycle survey (issue #166). It does have an integration test for
it, because the first line above is an upstream behaviour the drill relies
on without stating it: the day a release stops disabling that task, a red
test says so.

## Source params

Set under `source.params` in the drill config.

| Param             | Kinds             | Meaning                                                 |
|-------------------|-------------------|---------------------------------------------------------|
| `database_name`   | `bak_chain`       | Required only when the directory holds backups of more than one database; names the one to restore. |
| `logins`          | `bak_with_logins` | **Required.** Bare filename of the server-logins script inside the source directory. |
| `bak`             | `bak_with_logins` | Optional. Bare filename of the backup; without it the directory is scanned for the newest full backup. |
| `backup_timezone` | all               | Optional. IANA zone name of the host that took the backup (e.g. `Europe/Budapest`). Without it `backup.created_at` is null — see above. |

## Drill config options

| Option     | Default   | Meaning                                        |
|------------|-----------|------------------------------------------------|
| `database` | `probavi` | Name the backup is restored under (letters, digits, underscores, hyphens only). |

## Environment

Credentials for reading backup sources arrive via the environment variables
declared in the drill config's `source.credential_env` (none are needed for
local files). Secrets never appear in protocol messages or logs.

## Behavior notes

- The image runs as the non-root `mssql` user; the docker provider's
  `put_file` lands the backup owned by that user (this adapter is why).
- Point-in-time recovery (`STOPAT` over log backups) is not supported
  yet; the probe declares `pitr: false` for both kinds.
- `teardown` has nothing to release — everything this adapter creates
  lives inside the sandbox, which the provider destroys.
