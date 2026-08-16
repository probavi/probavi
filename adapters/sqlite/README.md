# probavi-adapter-sqlite

Restores SQLite backups into a disposable sandbox so a drill can prove
they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

SQLite is the first embedded engine in this repository: there is no
server to start and nothing to dial. The engine is the `sqlite3` process
each step runs, `provision` places the artifact (or replays the dump)
under the sandbox's scratch directory, and every check opens the restored
file directly — its in-sandbox path travels through
`connection.database`, so the probe-declared runner reaches it without
the template ever hardcoding a path.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `sqlite_db` | one database file from `sqlite3 .backup` or `VACUUM INTO` (or a copy of a cleanly closed database) |
| `sqlite_db_dir` | a directory of them; the newest by file time is restored |
| `sqlite_dump` | SQL text from `sqlite3 .dump` |
| `sqlite_dump_dir` | a directory of dumps; the newest by file time is restored |

PITR does not exist for a bare SQLite file (Litestream is a different
product and a different artifact); the probe declares `pitr: false` so
the core refuses a `target.pitr` drill before anything runs.

## The live-copy fence: an open database's copy is refused by name

The one way to take a *wrong* SQLite backup is also the most tempting
one: copying the database file while an application has it open. The
copy usually still passes every integrity check — which is exactly what
makes it the false green this project exists to catch. Measured on the
verified images: with a live connection holding write-ahead-log frames,
the copied main file passes `PRAGMA integrity_check` with `ok` while
silently missing every transaction still in the `-wal`.

The adapter refuses the copies it can prove wrong, on the artifact's own
evidence:

- a **non-empty `-wal` sibling** beside the artifact — committed
  transactions sit in the write-ahead log, and the database file alone
  misses all of them;
- a **non-empty `-journal` sibling** — a rollback-journal write was in
  flight, and the copy may hold a state no transaction ever committed;
- a **zero-byte file** — sqlite3 would accept it as a valid empty
  database (measured: `integrity_check` says `ok` and queries answer),
  but no backup procedure ever produces one: even a database with no
  schema backs up as a full 4096-byte header page (measured).

Absence of siblings proves nothing — a live copy taken without them is
indistinguishable from a cold copy — so the honest recommendation is to
never copy a live database at all: `sqlite3 prod.db ".backup nightly.db"`
and `VACUUM INTO` produce self-contained, consistent artifacts under any
load, and a clean close removes the `-wal` (measured), so a well-taken
backup never trips the fence.

## The truncation sqlite3 cannot see

A `.dump` wraps everything in one transaction and ends with a `COMMIT;`
line (measured on every verified version, the dump of an empty database
included). Replaying a dump whose tail was lost *between* statements
therefore exits 0 and leaves an **empty database** — the transaction
never commits, and the implicit rollback erases every row without a word
(measured). No exit code will ever report it, so the adapter refuses a
file that opens with `.dump`'s exact signature but does not end with the
trailer, before a byte is transferred. Generic SQL text without the
signature skips the gate: the trailer contract belongs to `.dump`, and
`sqlite3 -bail` judges everything else during the replay (a mid-statement
truncation does exit non-zero, and surfaces as `source_corrupt` with the
parser's own words).

## The sandbox image: any image with a shell and sqlite3

There is no official SQLite image — SQLite ships as a library and a CLI,
not a server — so the drill sandbox is any image carrying a POSIX shell
and the `sqlite3` binary, started idle. CI verifies against the
community image [`keinos/sqlite3`](https://hub.docker.com/r/keinos/sqlite3)
(Alpine-based, tags matching SQLite versions, cosign-signed), which is
what the manifest's `verified` list names; a distro image with the
`sqlite` package installed works the same. An image lacking either is
refused up front with a message naming this section.

```yaml
sandbox:
  provider: docker
  params:
    image: keinos/sqlite3:3.46.1
    command: sleep infinity        # the adapter owns the whole flow
    memory: 256m
source:
  kind: sqlite_db
  path: /backups/app/nightly.db
```

Versions: SQLite's upstream supports only its latest release, so the
older lines in the `verified` list are the ones distributions maintain —
the baseline is 3.46, the line Debian stable ships. The database file
format is compatible in both directions since 3.0.0, which is also why
this adapter has **no version pre-check**: unlike the physical-restore
engines, an artifact written by a newer library normally opens fine on
an older one, and the exceptions (a schema using newer features such as
`STRICT` tables) surface as sqlite3's own precise error during the
integrity check rather than anything a header field could predict.

## Checks: plain SQL

SQLite speaks SQL, so the generating built-in checks (`row_count`,
`table_exists`, `freshness`, user-defined assertions) apply unchanged.
The declared runner is one `sqlite3` invocation per check — no shell
anywhere in the template — with `-bail` making the exit code report the
first SQL error and a literal tab separator producing the undecorated
rows the protocol requires (measured):

```yaml
checks:
  - name: orders_survived
    type: row_count
    table: orders
    min: 100000
```

The runner's own healthcheck counts `sqlite_schema` instead of running
`SELECT 1`: a bare constant query reads no page at all, and older CLIs
answer it even against a file that is not a database (measured on 3.46).

## When the backup was taken

`created_at` in the evidence record is **always null** for this adapter,
and that is deliberate: a database file's header carries format and
version fields but no wall clock, and a dump is undated SQL text (both
measured). A file's mtime dates a copy, not a backup, so it is not
reported either. The `source.params.backup_timezone` key the other
adapters use has nothing to act on here, and a config that sets it is
refused rather than silently ignored.

The same fact drives the directory kinds: with nothing better to rank
by, they pick the newest file by mtime, with the in-flight guard the
other directory kinds share (see `settle.go`). `sqlite_db_dir` considers
only files carrying the SQLite magic (checksum sidecars and stray `-wal`
files are not candidates — but a chosen artifact with a live sibling is
still refused by name, never silently passed over); `sqlite_dump_dir`
ranks every regular file, SQL text having no magic to filter by.

## Backup identity

`checksum` is SHA-256 over the artifact's bytes, exactly as stored.

## Deliberately not here

- **Copies of live databases** — refused where provable, discouraged
  always; see the fence above.
- **Compressed artifacts** (`.gz`) — refused by name; decompress first,
  so the drill restores the bytes the evidence record identifies.
- **Litestream / WAL-shipping PITR** — a different artifact shape and a
  real one-way door (replica layouts, generations); it would be its own
  source kind designed measured, not assumed.
- **`db` + `-wal` pair restores** — recovering a crash-consistent pair
  is what the engine does on open, but proving *recovery of a crash
  image* is a different claim than proving a backup restores; the drill
  refuses the pair rather than blurring the two.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| the artifact is gzip-compressed | `unsupported_source`, saying to decompress |
| a database file handed to the dump kind, or vice versa | `invalid_request`, naming the right kind |
| a non-empty `-wal`/`-journal` sibling (live copy) | `unsupported_source`, teaching `.backup` / `VACUUM INTO` |
| a zero-byte artifact | `source_corrupt` |
| a `.dump` without its `COMMIT;` trailer | `source_corrupt`, before transfer |
| the sandbox image lacks a shell or sqlite3 | `invalid_request`, naming this README |
| `PRAGMA integrity_check` finds anything but `ok` | `source_corrupt`, carrying sqlite3's words |
| the dump stops parsing mid-statement | `source_corrupt` |
| the replay fails for engine reasons | `restore_failed` |
