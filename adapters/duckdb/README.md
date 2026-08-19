# probavi-adapter-duckdb

Restores DuckDB backups into a disposable sandbox so a drill can prove
they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

DuckDB is the second embedded engine in this repository, after SQLite:
there is no server to start and nothing to dial. `provision` places the
database file (or imports the export) under the sandbox's scratch
directory, and every check opens the restored file directly — its
in-sandbox path travels through `connection.database` into the declared
runner's `{{database}}` placeholder.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `duckdb_db` | one database file — a copy of a cleanly closed database |
| `duckdb_db_dir` | a directory of them; the newest by file time is restored |
| `duckdb_export` | one `EXPORT DATABASE` directory: `schema.sql`, `load.sql` and one data file per table (CSV or Parquet — both restore offline, measured) |

PITR does not exist for a DuckDB file; the probe declares `pitr: false`
so the core refuses a `target.pitr` drill before anything runs.

`IMPORT DATABASE` resolves the data-file paths against the directory it
is handed rather than the absolute paths `load.sql` recorded at export
time (measured against a moved export), so an export drills correctly
wherever it was taken.

## The sandbox image: the official image cannot host a drill alone

The official `duckdb/duckdb` images carry exactly one file of interest —
the `/duckdb` binary — and nothing else: no shell, no coreutils, no way
to idle (measured: `sh` does not exist, and the binary exits on EOF). A
drill sandbox must sit idle while the adapter works, so it runs a
two-line wrapper you build yourself:

```dockerfile
FROM duckdb/duckdb:1.4.5 AS duckdb
FROM debian:12-slim
COPY --from=duckdb /duckdb /usr/local/bin/duckdb
```

The engine binary is exactly the official image's; debian adds the shell
and coreutils. The base must be glibc: the binary does not start on
alpine (measured — musl cannot load it). A drill pointed at an image
without the CLI fails up front with a message naming this section. The
integration suite builds the same wrapper from the manifest's listed
image, so "verified against DuckDB 1.4" means that binary, exercised
this way.

```yaml
sandbox:
  provider: docker
  params:
    image: your-registry/probavi-duckdb:1.4   # the wrapper above
    command: sleep infinity                    # the adapter owns the whole flow
    memory: 512m
source:
  kind: duckdb_db
  path: /backups/analytics/nightly.duckdb
```

Versions: the `verified` baseline is the 1.4 line — DuckDB's designated
LTS — with the current stable line beside it.

## The storage-format fence: a newer file is refused naming both sides

A DuckDB file's header states its storage format version, and an engine
refuses formats newer than it reads (measured: a file written with
`STORAGE_VERSION 'v1.5.0'` carries format 68, and 1.4.5 answers *"we can
only read versions between 64 and 67"* at open). By default DuckDB
writes the oldest compatible format — 64 — so files travel both
directions between the verified lines (measured), and a version refusal
here is never guesswork: it fires only when the engine itself refuses,
and the adapter then names both sides — the storage format version and
writing library version read from the file's own header (offsets 12 and
52, measured), and the sandbox engine's reported version — as
`invalid_request`: a drill config pairing a backup with a sandbox image
that cannot read it (`docs/engine-versions.md` §5). Corruption is a
different verdict: the format checksums its blocks, an invalid,
truncated, or damaged file fails its first read loudly (measured — a
zero-byte file included, which is why no host-side gate second-guesses
the engine), and the drill reports `source_corrupt` with the engine's
words.

## The live-copy fence: an open database's copy is refused by name

Copying the database file while a connection is writing invites the
false green this project exists to catch: the copy opens cleanly and
silently misses every transaction still in the write-ahead log
(measured: 500 rows where the live database holds 505). The evidence is
the non-empty `.wal` sibling DuckDB maintains between checkpoints — a
clean close checkpoints and removes it (measured) — so an artifact with
one beside it is refused by name, with the fix in the message: copy the
database only after it is closed, or drill an `EXPORT DATABASE`
directory, which is consistent under any load. Absence of the sibling
proves nothing and stays silent; the recommendation stands regardless.

## Checks: plain SQL

DuckDB speaks SQL, so the generating built-in checks (`row_count`,
`table_exists`, `freshness`, user-defined assertions) apply unchanged.
The declared runner is one `duckdb` invocation per check — no shell
anywhere in the template — and its mode flags are load-bearing, not
cosmetic: the CLI keeps its decorated box output even when piped
(measured), so `-list -noheader` with a literal tab separator produce
the undecorated rows the protocol requires, and `-bail` makes the exit
code report the first SQL error.

The adapter's own healthcheck counts `duckdb_tables()` rather than
running `SELECT 1`: the catalog read forces checksummed pages through
the engine, so the verdict says the file still serves, not merely that
the CLI starts.

## When the backup was taken

`created_at` in the evidence record is **always null** for this adapter,
and that is deliberate: the database header carries checksums and
version fields but no wall clock, and an export is undated SQL plus data
files (both measured). A file's mtime dates a copy, not a backup, so it
is not reported either. The `source.params.backup_timezone` key the
other adapters use has nothing to act on here, and a config that sets it
is refused rather than silently ignored.

The same fact drives the directory kind: with nothing better to rank by,
`duckdb_db_dir` picks the newest file by mtime, with the in-flight guard
the other directory kinds share (see `settle.go`). Only files carrying
the `DUCK` magic are candidates (checksum sidecars and stray `.wal`
files are not) — but a chosen artifact with a live sibling is still
refused by name, never silently passed over.

## Backup identity

For the file kinds, `checksum` is SHA-256 over the artifact's bytes,
exactly as stored. For `duckdb_export` it is a canonical hash of the
directory tree: entries sorted by relative path; each regular file
contributes its path, size, and content bytes; symlinks contribute path
and target. The same tree always hashes the same, and any content change
changes the hash.

## Deliberately not here

- **Copies of live databases** — refused where provable, discouraged
  always; see the fence above.
- **`db` + `.wal` pair restores** — recovering a crash-consistent pair
  is what the engine does on open, but proving *recovery of a crash
  image* is a different claim than proving a backup restores.
- **Compressed artifacts** (`.gz`) — refused by name; decompress first,
  so the drill restores the bytes the evidence record identifies.
- **Encrypted databases** (`ATTACH … ENCRYPTION_KEY`, since 1.4) — a
  key-handling design the first release does not assume; an encrypted
  file fails its opening read with the engine's own words.

## Nothing expires between checks

DuckDB is a library, not a server. The drill sandbox runs **no engine
process between checks** — each check is one `duckdb` invocation — so there is
nothing that could expire, compact or purge anything on its own while a
drill is running.

Measured on every verified version, to close the question rather than
reason it away: a restored database left alone for twenty seconds is
**byte-identical** afterwards, with no engine process in the sandbox. An
integration test asserts the same thing on every drill, so the day this
engine grows a background task, a red test says so.

That is this adapter's line of the data-lifecycle survey (issue #166),
which found nine other engines that do subtract from a restored artifact.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| the artifact is gzip-compressed | `unsupported_source`, saying to decompress |
| a non-empty `.wal` sibling (live copy) | `unsupported_source`, teaching the fix |
| a directory lacking `schema.sql` or `load.sql` | `source_corrupt` — not an export directory |
| the sandbox image cannot run the duckdb CLI | `invalid_request`, naming the wrapper recipe |
| the engine refuses the file as invalid, truncated, or corrupt | `source_corrupt`, carrying its words |
| the file's storage format is newer than the engine reads | `invalid_request`, naming both sides' versions |
| a data file the import needs is missing | `source_corrupt` |
| the import fails for engine reasons | `restore_failed` |
