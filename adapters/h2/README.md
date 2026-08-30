# probavi-adapter-h2

Restores H2 backups into a disposable sandbox so a drill can prove they
still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

H2 is the third embedded engine in this repository, after SQLite and
DuckDB: there is no server to start and nothing to dial. `provision`
places the artifact under the sandbox's scratch directory, and every check
opens the restored database directly — its in-sandbox base path travels
through `connection.database` into the declared runner's `{{database}}`
placeholder. Nothing listens on a port; a drill runs with no server, no
password prompt and no network.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `h2_backup` | one `BACKUP TO` archive — H2's own online backup |
| `h2_backup_dir` | a directory of them; the newest by file time is restored |
| `h2_db` | one `<database>.mv.db` file, copied while the database was closed |
| `h2_db_dir` | a directory of them, ranked the same way |

PITR does not exist for an H2 file; the probe declares `pitr: false` so
the core refuses a `target.pitr` drill before anything runs.

Neither form records when the backup was taken — the MVStore header's
`created` field dates the database, not the backup, and does not move when
one is taken (measured) — so `backup.created_at` is always null,
directories rank by modification time, and `source.params.backup_timezone`
is refused rather than silently ignored.

**Prefer `BACKUP TO`.** It is the one artifact form that is consistent by
construction *and* whose completeness this adapter can check before a byte
moves — its zip container has no valid form when truncated. The section on
what a `.mv.db` cannot tell you says what the alternative costs.

## The sandbox image: there is no H2 image, so you build one

H2 ships as a jar on Maven Central, not as a container image. A drill
sandbox therefore runs a two-line wrapper you build yourself:

```dockerfile
FROM eclipse-temurin:21-jre
ADD https://repo1.maven.org/maven2/com/h2database/h2/2.4.240/h2-2.4.240.jar /opt/h2/h2.jar
```

`/opt/h2/h2.jar` is the contract, not a suggestion: it is where this
adapter looks, and a sandbox without it fails up front with a message
naming this section. The base must carry a POSIX shell with `grep` and
`sed`, which the check runner uses to turn H2's output into the runner
contract (below); the Temurin images do.

The only versioned community H2 image was measured and rejected: two
release lines behind, on Java 11, and carrying neither `grep` nor `sed`.
The manifest therefore names the base in `image` and the jar in
`engine_artifact`, and CI builds this same wrapper from that pair — so
"verified against H2 2.4.240" means that jar, exercised this way
([`docs/engine-versions.md`](../../docs/engine-versions.md) §1).

```yaml
sandbox:
  provider: docker
  params:
    image: your-registry/probavi-h2:2.4.240   # the wrapper above
    command: sleep infinity                    # the adapter owns the whole flow
    memory: 512m
source:
  kind: h2_backup
  path: /backups/orders/nightly.zip
```

## The check runner is a script, and it has to be

H2's `Shell` tool meets neither half of the runner contract
(adapter protocol §6.1) on its own, both measured:

- **It does not exit non-zero on a SQL error.** It prints
  `Error: org.h2.jdbc...` on *stdout* and returns 0 — and mid-stream, after
  whatever the statements before it printed. A runner built on the bare
  tool would report every failing check as passing.
- **It decorates.** A result arrives as a column header, the rows, and a
  `(N rows, M ms)` trailer.

So the declared runner is a small shell script around the tool: any
`Error:` line fails the check with the tool's own words on stderr, the two
decoration lines are stripped, and what remains is the undecorated rows
the contract asks for. A zero-row result correctly leaves nothing.

Every URL the adapter builds carries `IFEXISTS=TRUE`. That is not a
preference either: pointed at a path holding no database, H2 **creates
one** and answers queries against it (measured), so without the flag a
drill whose restore silently produced nothing would check a fresh, empty
database and pass.

## The storage-format fence

The MVStore header states the format the file was written in: `format:1`
for H2 1.4, `format:3` for every 2.x release (measured across 1.4.200,
2.2.224, 2.3.232 and 2.4.240). A 1.x database is refused here, by name,
before a byte moves — the engine's own refusal
(`Unsupported database file version or invalid file header`) names neither
the file's format nor its own.

Within the 2.x line there is nothing to fence: a `.mv.db` written by any
of 2.2, 2.3 and 2.4 opens in any of the others, in both directions
(measured). Converting a 1.x database needs the 1.x engine's own
`SCRIPT TO` output, which is a migration step rather than a drill.

## What a `.mv.db` cannot tell you

MVStore does not refuse a damaged file. It reconstructs whatever
consistent state the bytes present can support and opens **that** — an
older database, with no complaint of any kind. Two artifacts share this
one property, and it is the reason `BACKUP TO` is the recommended form.

**A file truncated at the tail** opens as the database it was before the
lost bytes were written. Measured on the suite's own fixture: at every
truncation tried, the recovered database had **no tables at all**, while
the engine reported success and a `SELECT 1` passed against it. So the
restore's verdict here is the restored database's table count, and a
well-formed zero is refused (`source_corrupt`). What that catches is a
restore that produced nothing; what it cannot catch is a truncation that
leaves the tables and loses rows.

**A file copied while the database was open** has exactly the same shape,
for the same reason: it holds the last checkpoint the copy caught.
Measured: a copy taken during a bulk insert answered `SELECT COUNT(*)`
with 0 while the real database held 3,000,000 rows.

There is no signal to tell either of them from a sound backup, and one was
looked for four ways:

- H2 leaves no lock file beside an open database, so the sibling-file
  evidence the SQLite adapter uses has no analogue here;
- the MVStore header says `clean:1` in both the damaged file and the
  cleanly closed one;
- both end in a valid chunk footer;
- the engine opens both with exit 0.

So this adapter says so rather than implying a fence it does not have.
`BACKUP TO` is the answer: H2's own online backup is consistent by
construction, and its archive is refused host-side the moment it is
incomplete. If you must copy the file, copy it from a database that is
closed.

## Deliberately not here

- **`SCRIPT TO` SQL text.** A script truncated *at a statement boundary*
  replays with exit 0, no error of any kind, and an arbitrary amount of
  data missing — measured at 162 of 1000 rows — and H2 writes no
  end-of-script marker to fence it with. (Truncation *within* a statement
  is caught, but that is the easy half.) The row-count comments H2 emits
  are exact, but only for tables the truncation did not erase outright, so
  they close part of the gap and not the dangerous part. A backup format
  that can lose most of a database and still drill green is not one this
  adapter will accept.
- **Encrypted databases** (`CIPHER=AES`). The key would have to reach the
  sandbox, and the shape of that is a design question rather than an
  omission.
- **Compressed artifacts.** A gzip-compressed backup is refused by name;
  decompress it first.
- **The H2 server mode.** Every drill runs embedded, which is what makes
  the sandbox portless.

## Drill config options

| Option | Default | Meaning |
|---|---|---|
| `user` | `sa` | The database user checks connect as. |
| `password_env` | — | Name of the environment variable holding that user's password. It must also appear in the drill's `source.credential_env`, or the adapter refuses rather than letting the password be silently empty. |

The password travels as a variable *name* through the protocol and is
resolved by the core (adapter protocol §2.5, §6.1); it never appears in an
argument list, where the sandbox's process table would show it.

## Behavior notes

- **What `restore_seconds` measures**: the archive's extraction, where
  there is one, plus the engine's first open of the restored database and
  the count that decides the verdict. It is deliberately not a full read:
  H2 offers no whole-file verification pass, and dumping the database to
  force one would put work into the number an operator reads as their RTO
  that no real recovery does.
- **An archive is unpacked by H2 itself** (`org.h2.tools.Restore`), so the
  sandbox needs no unzip utility beyond the jar it already carries.
- **Nothing in the backup can run**: an H2 trigger needs a Java class on
  the classpath, and the sandbox's classpath is the H2 jar alone. A
  database carrying a trigger whose class is absent fails every check
  against that table (measured) rather than quietly deleting rows — the
  drill reports that it cannot prove the database, which is the right
  answer.
