# probavi-adapter-mongodb

The MongoDB engine adapter for Probavi, implementing `probavi-adapter/0`
(see `docs/adapter-protocol.md`). Standard library only — deliberately no
imports from the Probavi core; like the postgres and mysql adapters, it is
written from the protocol document alone.

## Supported source kinds

| Kind             | Meaning                                                    |
|------------------|------------------------------------------------------------|
| `mongodump`      | One `mongodump --archive` file, plain or `--gzip` — the compression is sniffed from the bytes, never from the file name. |
| `mongodump_dir`  | A directory of archive files; the newest regular file is restored (mtime, ties broken by name — an archive carries no timestamp of its own). |
| `mongodump_with_users` | An archive taken with `--dumpDbUsersAndRoles`; the account layer is restored with the data, and the drill fails unless it arrived and resolves. |
| `mongodump_with_oplog` | A full archive taken with `--oplog`; the captured oplog is replayed, and the drill fails unless the replay happened. |

The directory-tree format (`mongodump` without `--archive`) is not
supported: take archives (`mongodump --archive=backup.archive --gzip`) —
one artifact, one checksum, one identity in the evidence record.

## Sandbox image and authentication

The sandbox image must contain `mongod`, `mongosh`, and `mongorestore` —
the official `mongo:6`/`mongo:7`/`mongo:8` images do. Run the image
**bare**, with no `MONGO_INITDB_*` variables:

```yaml
sandbox:
  provider: docker
  params:
    image: mongo:7
```

mongod then starts without access control. That is acceptable **only**
because Probavi sandboxes have zero ingress by default: `--network none`,
and publishing ports is not expressible at all. It also skips the image's
first-boot initialization phase (a temporary localhost-only server), so
readiness probes cannot be fooled by a server that is about to restart.
Because no authentication exists in the sandbox, `connection.user` is
reported empty and the declared `sql_runner` references no `{{user}}`.

## MongoDB 8.0 and Linux kernels 6.19 and newer

A drill against MongoDB 8.0 fails on a host running Linux 6.19 or newer,
and the failure looks like nothing in particular: the sandbox comes up,
the engine never answers, and the drill ends as `engine_not_ready`.

The reason is upstream, not here. `mongod` 8.0 refuses to start on those
kernels and says so before doing anything else:

```
MongoDB cannot start: Linux kernel versions 6.19 and newer has a known
incompatibility with this version of MongoDB.
See https://jira.mongodb.org/browse/SERVER-121912
```

The message goes to the container's own output, which an adapter cannot
read — the protocol gives it `exec` and `put_file`, and there is nothing
left in the sandbox to exec against once the engine has exited. So the
adapter reports the readiness timeout truthfully and cannot explain it,
which is why the explanation is here instead. Checked 2026-08-27 against
`mongo:8.0` on kernel 7.1.9; MongoDB 7.0 starts on the same host.

If your drill hosts run a recent kernel, this affects the 8.0 line only.

## The TTL monitor is disabled before the restore

A drill proves what the backup holds, not what a running server would
keep — so the first thing the adapter does once the engine answers is
`setParameter ttlMonitorEnabled=false`, before a byte of the archive
moves.

Without it, MongoDB deletes every document past a TTL index's expiry from
a background thread whose first pass lands about a minute after mongod
starts (measured: 500 of 500 expired documents gone in one pass, a
collection without a TTL index untouched, `mongorestore` reporting every
document restored successfully). A backup restored later than its own TTL
window — an hour-long session expiry in yesterday's archive, a ninety-day
audit collection in an archive older than that — therefore arrives intact
and empties itself while the drill is still running.

The deeper problem is not the loss but what it depends on. The pass fires
on the server's clock, not the drill's, so a small backup that finishes
inside the first minute sees its data and a production-sized one does
not: the same backup, the same drill, two different answers. A record
that depends on how long a restore took is not evidence.

If the engine refuses the parameter, the drill **fails** with
`invalid_request` naming it, rather than proving whatever survived the
clock. The pin is a runtime parameter and not a `mongod` flag because the
drill config supplies the sandbox command — the adapter never sees the
server's argv — and it lasts exactly as long as the sandbox does.

## Restore behavior

- The archive is replayed with `mongorestore --stopOnError`: partial
  restores fail loudly as `restore_failed` (§5) instead of being papered
  over by the tool's default keep-going behavior. Databases and
  collections are restored under their original names from the archive.
- Input mongorestore rejects as an archive (wrong magic number, "does not
  appear to be a mongodump archive", a lying gzip header) is classified
  `source_corrupt`.
- `options.database` (default `admin`) selects the database that
  `connection.database`, the healthcheck ping, and the sql_runner target —
  set it to the database your checks query (typically the one the archive
  restores). It never influences what gets restored.

## Which backup a drill restores, and when it refuses

When the drill config names a **directory**, the adapter picks the
artifact itself: the newest regular file (mtime, ties broken by name).

This is the one directory kind still ranked that way, and the reason is
the archive format: a `mongodump` archive records **no timestamp of its
own** (measured), so there is nothing else to rank by. The other adapters
rank by what the backup says about itself, which survives copying; here a
backup copied in later (`cp` without `-p`, an object-store download, an
`rsync` without `-t`) still looks like the newest thing in the
directory. Preserve modification times, or name the artifact outright
with a file `path`.
Two things follow from that, and both are stated here rather than left
for an operator to discover.

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

## The mongodump_with_users kind (the account layer)

MongoDB keeps users and roles in the `admin` database
(`admin.system.users`, `admin.system.roles`). A per-database archive
carries them **only** when it was taken with `--dumpDbUsersAndRoles`, and
`mongorestore` puts them back **only** when it is asked with
`--restoreDbUsersAndRoles`. The sharp edge, measured on a real server: an
archive that *does* carry the account layer restores without it silently —
exit 0, every collection in place, zero users. The operator did everything
right on the backup side and the drill still proved half the recovery.

```yaml
source:
  kind: mongodump_with_users
  path: /backups/shop-2026-08-09.archive
target:
  options:
    database: shop        # required: the database the accounts belong to
```

`options.database` is **required** for this kind and must name the
database the archive was dumped from: `mongorestore` refuses the request
without it ("cannot use --restoreDbUsersAndRoles without a specified
database"), and defaulting it would restore the accounts into `admin` —
proving the wrong thing quietly.

Two gates run after the restore, because asking for the accounts is not
the same as getting them:

1. **The account layer arrived** — the restored database has at least one
   user. An archive taken without `--dumpDbUsersAndRoles` fails here (and
   `mongorestore` usually fails first, loudly); the error says which flag
   the backup needs.
2. **Role references resolve** — every role a restored user holds, and
   every role a restored role inherits, is asked back from the server with
   `rolesInfo`. Built-in roles resolve and never look orphaned, so the
   check needs no hardcoded list and keeps working across server versions.
   This is the failure a per-database archive leaves behind: a user may
   hold a role defined in *another* database, which the archive does not
   carry — the user restores, the role does not, and the privileges are
   silently gone (measured: `usersInfo` still reports the user with `ok:1`
   and zero inherited privileges, so nothing else notices).

Note that a **full** archive (no `--db`) already carries `admin`, so
`mongorestore` restores users and roles from it without any flag.

## The mongodump_with_oplog kind (point-consistent restore)

`mongodump` copies collections one after another while the cluster keeps
writing, so a dump of a live replica set is **not** point-consistent
across collections. `mongodump --oplog` exists for exactly that: it
captures the oplog entries produced during the dump window, and replaying
them rolls the restored data forward to a single point — the end of the
dump. It requires a replica-set source and a full dump (`--oplog` is
rejected together with `--db`).

```yaml
source:
  kind: mongodump_with_oplog
  path: /backups/cluster-2026-08-09.archive
```

The measured defect this closes: the adapter never asked `mongorestore` to
replay the captured window. An archive taken with `--oplog` restored
cleanly, exit 0, with its oplog section ignored — a write issued during
the dump window into an already-copied collection was simply absent from
the restored data, and nothing in the record said so. The operator
captured the tail and the drill threw it away.

The drill now replays it and fails unless the replay actually happened, so
this kind's record means *restored to a consistent point*, not merely
*restored*. Declaring it on an archive with no oplog fails loudly
("no oplog file to replay; make sure you run mongodump with --oplog")
rather than passing while proving less.

## What the mongodump and mongodump_dir kinds do not prove

A passing `mongodump` or `mongodump_dir` drill proves the archive restores
and its data validates — nothing about users and roles, and nothing about
cross-collection consistency if the archive came from a live cluster. Use
`mongodump_with_users` and `mongodump_with_oplog` when your recovery
depends on those, and note that the sandbox runs without access control:
the account layer is proven to *restore and resolve*, not to authenticate.

## Checks: the mongosh --eval dialect

MongoDB has no SQL. The declared `sql_runner` template runs the check text
through `mongosh --quiet --eval`, so **checks for this adapter are mongosh
expressions**, written in the drill config's raw `sql` field:

```yaml
checks:
  - name: orders_present
    sql: "db.orders.countDocuments({})"
    min: 1
```

The expression's result is printed as the row output (a number prints as a
bare number). For multi-column rows, `print()` tab-separated values.
Builtin checks that generate SQL (`row_count`, etc.) do not apply to this
adapter — use raw expressions. This is the protocol's design working as
intended: the engine dialect is absorbed by the adapter's declared
template, and the core never learns it (§6.1).

### When the backup was taken

`created_at` in the evidence record is **always null** for this adapter,
and that is deliberate. A mongodump archive records no backup timestamp —
its header carries the archive format version, the server version and the
tool version, and nothing else (measured against MongoDB 7). The file's
modification time is not a substitute: copying a backup without
preserving timestamps resets it, and a month-old artifact then looks like
last night's, so reporting it as a creation time would put a claim in a
signed record that the backup does not support.

The `source.params.backup_timezone` key the other adapters use to place a
backup's wall clock in a zone has nothing to act on here, and a config
that sets it is **refused** rather than silently ignored — an operator who
wrote it is expecting an accuracy this kind cannot deliver.

## Backup identity

- `checksum`: SHA-256 over the selected artifact's bytes (for
  `mongodump_dir`, the chosen file). For gzip archives the hash covers the
  compressed bytes exactly as stored.
- `created_at`: always null — a mongodump archive records no backup
  timestamp, and an mtime dates a copy rather than a backup (see above).

## Source params

Set under `source.params` in the drill config.

| Param             | Kinds | Meaning                                                            |
|-------------------|-------|--------------------------------------------------------------------|
| `backup_timezone` | none  | **Refused.** A mongodump archive records no backup timestamp, so a declared zone has nothing to act on and a config that sets it fails the drill rather than implying an accuracy this adapter cannot deliver — see above. |

## Drill config options

| Option     | Default | Meaning                                          |
|------------|---------|--------------------------------------------------|
| `database` | `admin` | Database for connection info, healthcheck, and checks (letters, digits, underscores, hyphens only). **Required** for `mongodump_with_users`, where it also names the database whose accounts the archive carries. |

## Environment

Credentials for reading backup sources arrive via the environment variables
declared in the drill config's `source.credential_env` (none are needed for
local files). Secrets never appear in protocol messages or logs.

## Behavior notes

- Engine readiness is probed with `db.runCommand({ping:1})` over a
  connection string that bounds server selection to 2 s, so an unready
  engine answers the poll quickly instead of hanging into the per-command
  timeout.
- Point-in-time recovery (oplog replay) is not supported yet; the probe
  declares `pitr: false` for both kinds.
- `teardown` has nothing to release — everything this adapter creates
  lives inside the sandbox, which the provider destroys.
