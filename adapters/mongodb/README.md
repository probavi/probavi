# probavi-adapter-mongodb

The MongoDB engine adapter for Probavi, implementing `probavi-adapter/0`
(see `docs/adapter-protocol.md`). Standard library only — deliberately no
imports from the Probavi core; like the postgres and mysql adapters, it is
written from the protocol document alone.

## Supported source kinds

| Kind             | Meaning                                                    |
|------------------|------------------------------------------------------------|
| `mongodump`      | One `mongodump --archive` file, plain or `--gzip` — the compression is sniffed from the bytes, never from the file name. |
| `mongodump_dir`  | A directory of archive files; the newest regular file is restored (mtime, ties broken by name). |
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

## Backup identity

- `checksum`: SHA-256 over the selected artifact's bytes (for
  `mongodump_dir`, the chosen file). For gzip archives the hash covers the
  compressed bytes exactly as stored.
- `created_at`: the artifact's modification time — the closest derivable
  stand-in for the backup's creation time; treat it accordingly if you
  copy backup files around without preserving timestamps.

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
