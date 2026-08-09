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

**`created_at` is the file's modification time**, which is the closest
thing available here, not the time the backup was taken. Copying backups
around without preserving timestamps (`cp` without `-p`, `rsync` without
`-t`, most object-store downloads) resets it, and a stale artifact then
looks like the newest one. Preserve timestamps, or point the drill at a
fixed path when it matters.

An artifact the config names outright is never second-guessed this way:
the operator chose that file, so the drill restores that file.

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
- `created_at`: the artifact's modification time (for repositories: the
  newest file's mtime) — the closest derivable stand-in for the backup's
  creation time; treat it accordingly if you copy backup files around
  without preserving timestamps. For `pgdump_with_globals` it is the
  **older** of the two members: a set is only as current as its stalest
  part, and a globals script older than the dump is precisely the gap this
  kind exists to close.

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
