# probavi-adapter-postgres

The PostgreSQL engine adapter for Probavi, implementing `probavi-adapter/0`
(see `docs/adapter-protocol.md`). Standard library only — deliberately no
imports from the Probavi core, as proof that the protocol document alone is
enough to build an adapter.

## Supported source kinds

| Kind                  | Meaning                                              |
|-----------------------|------------------------------------------------------|
| `pgdump`              | One `pg_dump` file — custom-format (`-Fc`) or plain SQL (`-Fp`), stored plain or gzip-compressed. |
| `pgdump_dir`          | A directory of dump files; the dump whose own head records the newest time is restored. |
| `pgdump_with_globals` | A directory holding a `pg_dumpall --globals-only` script and one dump; the globals are loaded before the dump. Either member may be gzip-compressed. |
| `timescaledb_dump`    | One `pg_dump` file of a TimescaleDB database; the restore is framed with the extension's own `timescaledb_pre_restore()`/`timescaledb_post_restore()` procedure. |
| `timescaledb_dump_dir` | A directory of them, chosen like `pgdump_dir` and framed the same way. |
| `pgbackrest`          | A pgBackRest repository directory (filesystem repo) — a physical restore. Declares the `pitr` capability. |

## How a dump is stored (format and compression)

Every logical kind takes the artifact **as it is stored**, and works out
what it is from the bytes rather than the file name:

| Stored as | Restored with |
|-----------|---------------|
| custom-format archive (`-Fc`) | `pg_restore --no-owner --exit-on-error` |
| custom-format archive, gzipped | the same, fed by `gzip -dc` |
| plain SQL (`-Fp`, `pg_dumpall`) | `psql -v ON_ERROR_STOP=1` |
| plain SQL, gzipped | the same, fed by `gzip -dc` |

Renaming a backup therefore never changes what a drill does, and a
compressed dump is never decompressed outside Probavi to make a drill
possible. That matters for the evidence: `backup.checksum` covers the bytes
the operator actually retained, so the record and the stored artifact are
the same thing. Decompression happens inside the sandbox, streamed, so the
sandbox needs room for the stored artifact only.

The engine image must provide `gzip`, and — for a compressed plain-SQL dump
— `mkfifo` and `tee`. The official `postgres` images do. An image without
them fails the drill naming the image, not the backup.

Only gzip is recognised. The `-Ft` tar format is not a supported source and
is refused by name rather than handed to a client that would misreport it.

### A dump has to be whole, and psql cannot say whether it is

`psql` reports that no statement it executed failed. It does **not** report
that it reached the end of a complete dump: fed a plain-SQL dump cut on a
line boundary it restores what it got, treats the stream's end as the end of
the data, and exits 0 (measured: 477 of 1000 rows, exit 0). The failure that
produces such a file is ordinary — a backup job running `pg_dump | gzip`
whose `pg_dump` dies of a full disk still leaves a perfectly valid gzip file
behind, and every byte in it restores.

So a plain-SQL restore is only a pass when the dump's own closing line —
`-- PostgreSQL database dump complete`, or the `cluster` variant from
`pg_dumpall` — arrives with it. For a compressed dump the stream is tapped
while `psql` consumes it and only its tail kept, rather than inflating the
artifact twice (measured on a 218 MiB dump: the tap costs 2% of the restore,
a second inflate pass would cost 70%, and the restore duration is an RTO
figure somebody reads). A dump without that line fails the drill as
`source_corrupt`, saying the backup stops early — which is a claim about the
backup job that wrote it, not about the restore.

Custom-format archives need none of this: the format carries a table of
contents, so `pg_restore` refuses a truncated archive on its own.

**A plain-SQL dump carries its ownership inline.** `pg_restore --no-owner`
can drop `OWNER TO`; `psql` has no such flag, because the statements are in
the script. A plain dump taken from a cluster with its own roles therefore
needs those roles present — which is what the `pgdump_with_globals` kind is
for, and what the adapter's diagnostic points at when a restore dies on a
missing role.

## The timescaledb kinds (the restore must be framed)

TimescaleDB mandates its own logical-restore procedure: create the
extension, call `timescaledb_pre_restore()`, restore, call
`timescaledb_post_restore()`. This is not ceremony. Measured on 2.29.1:
a production-shaped dump — compressed chunks, a continuous aggregate, a
retention policy — restored without the frame aborts partway with
"could not find hypertable with id 1" after a fraction of the rows,
while a trivial hypertable happens to restore whole; whether the plain
flow breaks depends on the backup's shape, which is exactly what an
operator must not have to know. The `timescaledb_dump` kinds run the
mandated frame around the ordinary restore, and every framing second is
part of the measured restore, because the real recovery path cannot
skip it. The dump's own `CREATE EXTENSION IF NOT EXISTS` skips with a
NOTICE inside the frame (measured), so `--exit-on-error` holds.

Both sides are fenced. On an image without the extension the framed
kind refuses up front (`invalid_request`), naming the
`timescale/timescaledb` image as the fix. And the plain logical kinds
refuse a TimescaleDB dump by name (`unsupported_source`, pointing at
`timescaledb_dump`) on positive evidence read in the sandbox: the
archive's own table of contents (`pg_restore -l` naming the extension),
or the `CREATE EXTENSION` statement in a plain script's bounded head —
pg_dump writes extensions before any data. One form goes unfenced,
deliberately: a gzip-compressed custom-format archive offers no exact
probe without inflating it, and its unframed restore still fails
loudly, never silently. Version discipline is the extension's own rule
— the restoring image's timescaledb version should match the backup's —
and the dump does not state its origin extension version, so mismatches
surface as the engine's own errors rather than a pre-check.

**The restored policies are held back for the life of the sandbox.**
Inside the frame, after the restore and before `timescaledb_post_restore()`,
every job in the restored catalog gets `next_start => 'infinity'`. This
is not tidiness: `timescaledb_post_restore()` releases the background
workers, and a restored retention policy runs *in the same second it
returns* — measured on a hypertable holding 200 days under a 90-day
policy, 15 of 29 chunks and 52% of the rows were gone before the frame
closed, with the restore reported successful. Nothing is racing there:
`bgw_job_stat` is absent from the dump, so a restored job has no
`next_start` and the scheduler treats it as due immediately.

A policy states what a *running* database should keep; a drill proves
what the backup holds, and the operator's real policy is already
expressed in which chunks the dump contains. The lever is deliberately
`next_start` and not the job's `scheduled` flag: the dump carries
`scheduled`, but not the statistics row `next_start` lives in, so the pin
fills a field the restore left empty and overwrites nothing the backup
contained. A check that reads `timescaledb_information.jobs` still sees
the policies exactly as the backup had them, `scheduled` included — they
simply never run. If they cannot be held back, the restore fails
(`restore_failed`) rather than proving a database that deleted part of
itself.

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
    dump: orders.dump            # optional; default: the newest-by-header file that is not the globals
```

- **One directory, members named in `params`.** The core only hands an
  adapter files belonging to the drill's configured backup source
  (protocol §4.2), so both members live under `path`. They are named
  explicitly rather than recognised by filename pattern: renaming a backup
  file must not silently change what a drill proves. Both values are plain
  filenames — a value containing a path separator is refused.
- **`params.dump` is optional.** Left out, the newest-by-header file other
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

Before anything is transferred, the adapter compares the repository's own
manifest (`db-version` in `backup.info`'s `[db]` section) against the
sandbox engine's `postgres --version`: a physical backup restores only
into its own major (docs/engine-versions.md §5), and an impossible
pairing is refused up front as `invalid_request` with a message naming
both versions — instead of surfacing whatever pgbackrest prints minutes
later. The check refuses only on positive evidence: an encrypted or
otherwise unreadable manifest simply skips it, and the restore speaks for
itself.

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
artifact itself: the dump whose **own head records the newest time**.
The file's modification time is not what ranks candidates — copying a
backup in (`cp` without `-p`, an object-store download, an `rsync`
without `-t`) resets it, and a stale artifact would then look like the
newest thing in the directory. What a dump says about itself does not
move when the file is copied.

Both formats record that time in their head — a custom-format archive in
its header, a plain-SQL dump in the `-- Started on` line — so ranking reads
a bounded few kilobytes per candidate whatever the artifact's size, and a
candidate stored compressed is inflated only that far. Neither the format
nor the compression affects the ordering: a directory may hold all four
shapes, and only what each artifact records about itself decides.

Ranking needs no declared zone: two dumps being compared came off the
same backup host, so whatever zone it was in cancels out. Declaring
`params.backup_timezone` is only needed to *report* `backup.created_at`.

A dump the adapter cannot date ranks below every dump it can. Between two
such files the previous rule still decides: newest mtime, ties broken by
the larger name. **Most plain-SQL dumps land here**: `pg_dump` writes the
`-- Started on` line only under `--verbose`, so a dump taken without it
carries no date at all, and none is invented for it.

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
one). A compressed archive is read through the decompressor, so how the
artifact is stored does not change what it says about itself.

A plain-SQL dump usually carries no date at all: `pg_dump` writes
`-- Started on <clock> <zone abbreviation>` only under `--verbose`. Where
that line is present it is read, and the abbreviation deliberately is not —
`CST` names three different zones, and one that resolves today may not next
year, so believing it would put a guess into a signed record. The zone
comes from the declaration below, the same as for an archive.

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
| `dump`            | `pgdump_with_globals` | Optional. Bare filename of the dump; without it the newest-by-header non-globals file is used. |
| `backup_timezone` | `pgdump*`, `timescaledb_dump*` | Optional. IANA zone name of the host that took the backup (e.g. `Europe/Budapest`). Without it `backup.created_at` is null — see above. Not needed for `pgbackrest`, whose repository records absolute timestamps. |

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

## pgvector

The `verified` list carries a pgvector entry (`pgvector/pgvector`, the
extension's own image — PostgreSQL 17 with the vector extension
bundled), and its matrix job exercises what makes the variant a variant:
the suite seeds a `vector(3)` column under an HNSW index, dumps it,
restores it through the drill, proves the index was rebuilt, and answers
a nearest-neighbour query through the declared runner. Two consequences
an operator should know:

- **The extension comes from the image, not the backup.** `pg_dump`
  records `CREATE EXTENSION vector` without a version, so the restored
  database gets whatever the sandbox image ships; restoring a dump that
  used newer extension features on an image with an older extension
  fails with the engine's own error. Keep the sandbox image's pgvector
  at least as new as production's.
- **Index rebuilds are part of the measured restore.** A logical restore
  rebuilds HNSW/IVFFlat indexes from scratch, and on real datasets that
  rebuild can dominate `restore_seconds` — expect the RTO trend of a
  vector-heavy drill to track index build time, not just data volume.
