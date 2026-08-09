# probavi-adapter-postgres

The PostgreSQL engine adapter for Probavi, implementing `probavi-adapter/0`
(see `docs/adapter-protocol.md`). Standard library only — deliberately no
imports from the Probavi core, as proof that the protocol document alone is
enough to build an adapter.

## Supported source kinds

| Kind                  | Meaning                                              |
|-----------------------|------------------------------------------------------|
| `pgdump`              | One `pg_dump` custom-format (`-Fc`) file.            |
| `pgdump_dir`          | A directory of dump files; the newest regular file is restored (mtime, ties broken by name). |
| `pgdump_with_globals` | A directory holding a `pg_dumpall --globals-only` script and one dump; the globals are loaded before the dump. |
| `pgbackrest`          | A pgBackRest repository directory (filesystem repo) — a physical restore. Declares the `pitr` capability. |

## The pgdump_with_globals kind (cluster globals first)

A logical recovery runs in two steps: the cluster-level objects — roles,
their memberships, and the grants that reference them — then the database
dumps. No `pg_dump` carries the first step; `pg_dumpall --globals-only`
does. A drill restoring only the dump proves the second half of a recovery
path that has two, and it does not fail quietly: `pg_restore --no-owner`
drops `OWNER TO` but never `GRANT`, so the restore dies on the first grant
naming a role nothing created.

This kind restores both halves:

```yaml
source:
  kind: pgdump_with_globals
  path: /backups/prod            # a directory holding both members
  params:
    globals: globals.sql         # a filename inside path, never a path
    dump: orders.dump            # optional; default: the newest file that is not the globals
```

- **One directory, members named in `params`.** The core only hands an
  adapter files belonging to the drill's configured backup source
  (protocol §4.2), so both members live under `path`. They are named
  explicitly rather than recognised by filename pattern: renaming a backup
  file must not silently change what a drill proves. Both values are plain
  filenames — a value containing a path separator is refused.
- **`params.dump` is optional.** Left out, the newest regular file other
  than the globals script is restored, so a drill against a rotating
  backup directory keeps working unattended. Naming it explicitly is what
  lets one directory hold the globals beside several databases' dumps,
  each drilled separately with its own checks.
- **Sandbox auth must be trust** (`env.POSTGRES_HOST_AUTH_METHOD=trust`
  with the docker provider). A globals script carries every role's
  password verifier, including the bootstrap superuser's, and replaying it
  resets that role's password mid-restore — which would lock the adapter
  out of its own sandbox. The sandbox has no network exposure whatsoever,
  so trust never extends beyond the disposable container.
- **PITR is refused**, as for every logical kind.

## The pgbackrest kind (physical restore)

A pgBackRest restore replaces the data directory, so the engine must not be
running when the drill starts. Requirements:

- **Sandbox image** containing `postgres`, `pgbackrest`, and `gosu`
  (e.g. built `FROM postgres:16` + `apt-get install pgbackrest`).
- **Idle start**: the sandbox must not boot the engine — with the docker
  provider set `command: sleep infinity` in `sandbox.params`. The adapter
  refuses to run against an already-running engine.
- **`source.params.stanza`**: the stanza name inside the repository
  (letters, digits, `-`, `_`).

The adapter transfers the repo into the sandbox, writes
`/etc/pgbackrest/pgbackrest.conf`, restores as the `postgres` user, then
**overwrites `pg_hba.conf` with sandbox-local trust auth** before starting
the server: the restored cluster's own auth config expects credentials the
drill does not have, and the sandbox has no network exposure whatsoever
(`--network none`, no published ports), so trust never extends beyond the
disposable container. Recovery replays WAL from the repo's archive; the
adapter waits until `pg_is_in_recovery()` reports false — checks never run
against a still-recovering instance — and the measured `engine_ready` phase
covers server start plus the full recovery.

## Point-in-time recovery (pitr)

The `pgbackrest` kind accepts the protocol's `pitr.target_time` (sent by the
core when the drill config has a `target.pitr` block):

```yaml
target:
  source:
    kind: pgbackrest
    path: /backups/orders/repo
    params: {stanza: orders}
  pitr:
    target_age: "24h"     # or an absolute instant: target_time: "2026-07-30T14:32:00Z"
```

The adapter maps the target onto `pgbackrest restore --type=time
--target=<instant> --target-action=promote`: recovery replays the archive up
to the requested instant, then promotes to read-write. Semantics worth
knowing:

- The target must lie **after the backup's end** and **within the archived
  WAL**. A target the archive cannot reach makes the server refuse to start
  (`FATAL: recovery ended before configured recovery target was reached`),
  which the adapter reports as `restore_failed` — a genuine recoverability
  verdict about that backup + archive combination, and exactly what a PITR
  drill exists to catch.
- PostgreSQL stops at the first commit **after** the target, so the restored
  state is "everything committed at or before `target_time`".
- The logical kinds (`pgdump`, `pgdump_dir`) reject `pitr` — a dump is a
  single frozen snapshot.

## Which backup a drill restores, and when it refuses

When the drill config names a **directory**, the adapter picks the
artifact itself: the newest regular file (mtime, ties broken by name).
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

### When the backup was taken

`created_at` in the evidence record is an absolute instant, and the honest
answer is often "not derivable". A file's modification time is **not** the
backup's creation time: copying a backup without preserving timestamps
(`cp` without `-p`, `rsync` without `-t`, most object-store downloads)
resets it, and a month-old artifact then looks like last night's. This
adapter therefore never reports an mtime as a creation time.

A `pg_dump` custom-format archive carries its own creation time in its
header, and that is what this adapter reads (archive versions 1.14 and
1.15/1.16 store it at different offsets; both are handled, and a header
this parser does not recognise yields no timestamp rather than a wrong
one).

A `pgbackrest` repository is the exception in this project: its
`backup.info` records **epoch seconds**, which are an instant already, so
that kind reports an exact creation time **with no zone declaration at
all** — measured, a repository written on a host in `Asia/Tokyo` stores
`1786289869`, which is `15:37:49` UTC. The newest backup in the
repository dates it, because that is the one a restore without a target
uses. An encrypted manifest (`repo1-cipher-type`) cannot be read, and
then the field is null rather than guessed.

What no backup format records is a UTC offset — the value is the wall
clock of the host that took the backup, and reading it as UTC would be
wrong by that host's offset (measured: a backup taken at 12:08 UTC on a
host in `Asia/Tokyo` is written as `21:08`). The offset is a fact only you
have, so the drill declares it:

```yaml
source:
  kind: pgdump_dir
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

- `checksum`: SHA-256 over the selected artifact's bytes (for `pgdump_dir`,
  the chosen file). For `pgbackrest` repositories: a canonical tree hash —
  entries sorted by relative path; each regular file contributes
  `relpath NUL size NUL content`, each symlink `relpath NUL "L" target NUL`.
  For `pgdump_with_globals`: the same framing over exactly the two chosen
  members in a fixed order — `"globals" NUL size NUL content` followed by
  `"dump" NUL size NUL content`. Both members are restored, so both are in
  the identity; nothing else in the directory is, because a drill's backup
  identity must cover what that drill restored and no more.
- `created_at`: the backup's own creation time, read from the artifact and
  placed in the zone the drill declares (see above); null when the backup
  carries none or no zone was declared. For `pgdump_with_globals` it dates
  the dump member: a globals script carries no timestamp of its own, so
  the pair's freshness rests on the member that can be dated.

## Source params

Set under `source.params` in the drill config.

| Param             | Kinds                 | Meaning                                             |
|-------------------|-----------------------|-----------------------------------------------------|
| `globals`         | `pgdump_with_globals` | **Required.** Bare filename of the cluster-globals script inside the source directory. |
| `dump`            | `pgdump_with_globals` | Optional. Bare filename of the dump; without it the newest non-globals file is used. |
| `backup_timezone` | `pgdump*`             | Optional. IANA zone name of the host that took the backup (e.g. `Europe/Budapest`). Without it `backup.created_at` is null — see above. Not needed for `pgbackrest`, whose repository records absolute timestamps. |

## Drill config options

| Option     | Default    | Meaning                              |
|------------|------------|--------------------------------------|
| `user`     | `postgres` | Superuser inside the sandbox engine. |
| `database` | `postgres` | Database to restore into.            |

## Environment

Credentials for reading backup sources arrive via the environment variables
declared in the drill config's `source.credential_env` (none are needed for
local files). Secrets never appear in protocol messages or logs.

## Behavior notes

- Engine readiness is probed over TCP (`pg_isready -h 127.0.0.1`): during
  initdb the official image runs a temporary server on the unix socket
  only, so socket probes would report ready too early.
- Restores run `pg_restore --no-owner --exit-on-error`: partial restores
  fail loudly (`restore_failed`), unreadable archives are classified as
  `source_corrupt`.
- The globals script is replayed with `psql -f`, never through the
  `sql_runner`: `pg_dumpall` wraps its output in `\restrict`/`\unrestrict`
  meta-commands, which only a psql session reading a file executes. The
  sandbox's psql must therefore be no older than the one that produced the
  script — matching the engine version you back up is the safe rule.
- **Exactly one error is tolerated during that replay:** `role "<user>"
  already exists` for the superuser the adapter connects as. `pg_dumpall`
  emits `CREATE ROLE` for the bootstrap superuser too, and `initdb`
  created it before the drill started; every other diagnostic — including
  a collision on any other role — fails the drill as `restore_failed`.
  `ON_ERROR_STOP` stays off deliberately: the collision sits in the middle
  of the script, so stopping there would silently skip every role after
  it, and the completeness of a drill would depend on role naming. psql's
  exit code is therefore not the verdict; the classified diagnostics are.
- `--echo-errors` is deliberately absent, and engine diagnostics are
  scrubbed of SQL password literals before they enter a protocol message:
  a globals script carries role password verifiers, PostgreSQL quotes
  offending source text back in syntax errors, and protocol error messages
  land in signed evidence records, which must never carry credentials
  (evidence schema §8).
- `teardown` has nothing to release — everything this adapter creates lives
  inside the sandbox, which the provider destroys.
