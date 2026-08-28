# probavi-adapter-firebird

Restores Firebird `gbak` backups into a disposable sandbox so a drill can
prove they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

## Supported source kinds

| Kind | What it takes |
| --- | --- |
| `firebird_gbak` | One gbak transportable backup file |
| `firebird_gbak_dir` | A directory of gbak backups; the newest is restored |

A gbak backup is what `gbak -b <database> <file>` writes: a single
transportable file, restored with `gbak -c`.

`nbackup` incremental artifacts are a different format and are not
supported.

## No server, no port, no password

Unlike most adapters in this catalog, this one starts nothing. `isql`
against a plain file path uses Firebird's **embedded** engine, so the whole
drill — place the backup, `gbak -c` it into a database, run the checks —
happens without a listener, a TCP port or a credential. Measured on the
official image under `--network none`.

The drill config needs an image and a memory limit and nothing else. There
is no idle command, and nothing is read from the environment.

## A backup can delete its own rows on first contact

This is the one thing to know before pointing a drill at a Firebird backup.

An `ON CONNECT` database trigger travels **inside** the backup, and a
restore installs it. Measured end to end on Firebird 5.0.4, with a trigger
that deletes rows below a threshold:

| | |
| --- | --- |
| rows in the source when the backup was taken | 3 |
| `gbak -c` restoring it | exit 0 — **and all 3 rows are present** |
| the first ordinary connection | **1 row** |

The restore is not what loses the data. The first connection is, and the
loss is irreversible — a second look, however careful, sees what the first
one left.

So every connection this adapter makes carries `-nodbtriggers`, and so does
the check runner it declares. The trigger is **suspended, never rewritten**:
it survives the drill present and `RDB$TRIGGER_INACTIVE = 0`, exactly as
the operator declared it, so a check that reads `RDB$TRIGGERS` still sees
the truth.

Nothing here needs configuring. It is documented because a drill that
dropped the flag would fail a row count and blame a backup that is
perfectly intact.

## gbak's exit code is the verdict — never the restored database

Measured, on three damage forms:

| Artifact | `gbak -c` | What it left behind |
| --- | --- | --- |
| truncated part-way | exit 1, `ERROR: Backup incomplete` | a database that opens and answers, holding every row |
| bytes overwritten mid-file | exit 1, `ERROR:string truncated` | a database that opens and answers |
| not a backup at all | exit 1, `expected backup description record` | nothing |

A failed restore leaving a queryable database is the false green this
adapter exists to refuse. The database file does not survive the failure
that produced it: it is removed, and the drill fails with gbak's own words.

Two more measured details shape the restore. gbak writes everything to
**stdout** and nothing to stderr, so its output is redirected to keep this
adapter's own stdout clean for the protocol. And it **prompts** on a
damaged volume — "press return to reopen that file, or type a new name" —
so its stdin is closed: a restore that waits forever for an answer nobody
is there to give is worse than one that fails.

## Restoring across versions

Verified against Firebird 5.0.4 and 4.0.7, and measured in both
directions. A 4.0 backup restores into 5.0 cleanly. A 5.0 backup restores
into 4.0 cleanly **only while the schema uses nothing the older engine
lacks** — a Firebird 5 partial index makes gbak print
`do not recognize index attribute 12 -- continuing` and then fail.

That marker is not treated as a version diagnosis, deliberately. The same
line appears when a backup is simply damaged (measured:
`do not recognize privilege attribute 65` from random bytes written over a
valid backup), so the error message names both possibilities and leaves
gbak's own words to separate them.

The backup file carries no engine version — the 4.0 and 5.0 headers are
identical in structure — so the version pre-check
[`docs/engine-versions.md`](../../docs/engine-versions.md) §5 asks of
physical restores has nothing to read here.

## Checks: identifiers are case-sensitive

The core quotes identifiers the SQL standard way, which Firebird honours
**case-sensitively**: name tables as the dictionary stores them — upper
case unless they were created quoted.

```yaml
checks:
  - builtin: table_exists
    table: ORDERS            # not `orders`
  - builtin: row_count
    table: ORDERS
    min: 1
  - name: no-negative-totals
    sql: "SELECT count(*) FROM ORDERS WHERE total < 0"
    expect: 0
```

The generating built-ins (`table_exists`, `row_count`, `freshness`) apply
unchanged.

**One limit worth stating.** `isql` has no delimiter setting: `SET HEADING
OFF` removes the column titles, but columns are separated by padding
rather than tabs. A single-value result — which is what every generating
built-in produces — arrives exact. A multi-column custom check comes back
in isql's own column layout instead of the tab-separated form the runner
contract describes. Parsing fixed-width columns back into fields would
mean guessing where a padded value ends and the padding begins, which is a
worse trade than saying so here.

A trailing semicolon in a check is accepted and stripped.

## When the backup was taken

gbak stamps the artifact, so the adapter reports it: the header's own clock
reaches the evidence record as `backup.created_at`. Nothing is invented
from a file's mtime — that would date a copy.

The clock carries **no time zone** (`Fri Aug 28 17:59:41 2026`, C's asctime
form). The offset is a fact only you have, so declare it:

```yaml
source:
  kind: firebird_gbak
  path: /backups/orders/latest.fbk
  params:
    backup_timezone: Europe/Budapest
```

Without the declaration the record's `created_at` is `null` rather than a
guess. An unknown zone name fails the drill rather than silently dropping
the timestamp it was meant to anchor.

## Backup identity

A gbak backup is a single file, so the checksum in the evidence record is a
sha256 of exactly the bytes that were restored.

## Errors it reports

| Code | When |
| --- | --- |
| `source_not_found` | the path does not exist, or a directory holds no backups |
| `source_unreadable` | the artifact cannot be read, or is still being written |
| `source_corrupt` | gbak read the artifact and rejected it |
| `unsupported_source` | an unknown source kind |
| `invalid_request` | a PITR request, a directory given as a single backup, an unknown time zone, or a sandbox without `gbak` and `isql` |
| `restore_failed` | gbak failed for another reason, or reported success for a database that then answered nothing |

That last one is a gate, not a formality: an exit code of 0 is not proof
that a database serves, and every check after provision runs against
whatever the serving probe reached.

## Drill config options

None beyond `source.params.backup_timezone` above.

## Environment

None. The sandbox has no published ports and the embedded engine needs no
credentials, so nothing is read from the environment and nothing is
redacted from the record.

## Point-in-time recovery

Not supported. A gbak backup is a snapshot of one instant, and the engine
offers nothing to recover between two of them.
