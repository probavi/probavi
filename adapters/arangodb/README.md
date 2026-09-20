# probavi-adapter-arangodb

Restores an `arangodump` output into a disposable sandbox and proves the
restored collections serve documents.

Written against `docs/adapter-protocol.md` alone: standard library only,
no imports from the Probavi core, and no knowledge of Probavi beyond the
protocol document.

Everything below was measured on 2026-09-20 against
`arangodb/arangodb:3.12.4` unless it says otherwise. Two of the
measurements corrected an assumption that would otherwise have shipped
silently, and both are stated where they belong rather than quietly
applied.

## What it restores

| kind | `source.path` points at |
| --- | --- |
| `arangodb_dump` | one `arangodump` output directory |
| `arangodb_dump_tar` | one tar archive (plain or gzip) of such a directory, its files at the root or under one wrapping directory |
| `arangodb_dump_dir` | a directory of such dumps; the one whose own `dump.json` claims the newest instant is restored |

An `arangodump` output holds `dump.json`, an `ENCRYPTION` marker, and two
files per collection — `<name>_<hash>.structure.json` and
`<name>_<hash>.data.json.gz`.

**HotBackup is not supported, because it is not in this edition.**
`arangobackup` is absent from the Community image and
`POST /_admin/backup/create` answers `404 unknown path` (measured). The
artifact is the logical dump alone.

Point-in-time recovery is not offered: every source kind probes
`pitr: false`.

## The sandbox

The image idles under `command: sleep infinity` — its entrypoint passes a
non-`arangod` command straight through — so the adapter starts the server
itself and owns its lifecycle:

```yaml
sandbox:
  provider: docker
  params:
    image: arangodb/arangodb:3.12.4
    command: "sleep infinity"
    memory: 1GiB
```

The server answered **3 seconds** after launch at a **768 MiB** cap, which
makes this the lightest engine among the recent adapters. Authentication
is off and the endpoint is loopback: a Probavi sandbox is zero-ingress —
no published ports are expressible — which is the only reason that is
acceptable, and it is the same reasoning the PostgreSQL and MySQL
adapters use for their own sandbox credentials.

**The image ships no `bash`.** It is Alpine-based, so every script this
adapter runs is `sh`. An adapter ported from a sibling without noticing
would fail at the first script.

## Time-to-live: suspended, not fenced

A TTL index names a field and an `expireAfter`, and a background thread
deletes documents whose field plus that interval has passed. Unlike the
Cassandra family's read-time filtering, this is a background deletion —
and the vendor gives a switch for it. `--ttl.frequency` is documented as
*"the frequency (in milliseconds) for the TTL background thread
invocation (0 = turn the TTL background thread off entirely)"*, default
30000.

So this adapter **suspends** rather than fences. Measured: a dump of 200
documents each already an hour past a 60-second TTL, restored into two
servers differing in exactly that flag.

| | default | `--ttl.frequency 0` |
| --- | --- | --- |
| t+0s | 200 | 200 |
| t+20s | 200 | 200 |
| **t+40s** | **0** | **200** |
| t+60s | 0 | 200 |

Without it, a drill restores every document and then watches the engine
delete all of them — reporting on data the engine removed while the drill
ran.

**Suspend, never rewrite.** The flag stops the thread; it does not touch
the index. Verified in the same run: the restored collection still
carries its TTL index with `expireAfter = 60` on `fields: ["stamp"]`,
exactly as the operator declared it, so a check that reads the index sees
what the backup held.

The flag is passed explicitly rather than relied on. A default is not a
guarantee — MySQL flipped `event_scheduler` to ON in 8.0 after years of
the opposite.

## What the dump states about itself

`dump.json` carries two fields this adapter depends on:

- **`createdAt`**, RFC 3339 with a literal `Z`, so `backup.created_at` is
  exact and the directory kind ranks candidates by it rather than by file
  times a copy would reset. A `source.params.backup_timezone` is
  **refused** rather than ignored: an operator who wrote it expects an
  accuracy the artifact already delivers.
- **`database`**, which is where the restore goes. There is no default and
  none is invented: a dump that names no database is refused, because
  restoring into a database the backup never mentioned proves nothing
  about the backup.

What `dump.json` does **not** carry is a list of the collections. So
completeness here is a pairing check rather than a comparison against the
artifact's own list: every collection must have both its
`structure.json` and its data file. That is safe to require because
`arangodump` writes both even for a collection with no documents at all
(measured) — so a structure with no data is a collection whose contents
went missing, not an empty one.

`ENCRYPTION` is read too. Encryption is an Enterprise feature and the
file is written either way, so an encrypted dump is **recognised and
refused by name** rather than failing obscurely later.

## The restore, and why the exit code is not the verdict

1. Transfer the artifact (`put_file` per file, or one archive plus
   `tar -xf`).
2. Write the check runner into the sandbox.
3. Start the server with the TTL thread held back.
4. `arangorestore --create-database true` into the database `dump.json`
   names.
5. **Count the restored collections.**

Step 5 is not a formality. **Pointed at an empty dump directory,
`arangorestore` reports `Processed 0 collection(s)` and exits 0**
(measured) — and so it does for a directory holding only `dump.json`. A
drill that trusted the exit code would pass having restored nothing. The
count is the verdict.

A damaged dump is caught, with one consequence worth stating: the tool's
error **quotes the request payload**, which is hundreds of restored
production documents. That text is deliberately kept out of the evidence
record — a record must be shareable as it stands (evidence schema §8) —
and goes to the drill host's log instead. What the record carries is the
exit code and this adapter's own words.

## Checks: AQL, with the built-ins working

Checks are AQL. The declared runner absorbs the dialect so that **the
core's three generating built-ins apply unchanged** — which is more than
the MongoDB adapter can offer, where they do not apply at all:

| what the core composes | what the engine is asked |
| --- | --- |
| `SELECT count(*) FROM "c" WHERE 1=0` | the collection exists, or the runner fails |
| `SELECT count(*) FROM "c"` | its document count |
| `SELECT max("f") FROM "c"` | the largest value of that field |

`table_exists` prints **nothing at all**: a probe for a collection's
existence should not put a document of restored production data on
stdout. Anything that is not one of the three reaches the engine as
written — the operator's own AQL, which is what a check is.

One measured trap shapes how the statement travels. **This engine's
option parser collapses `@@` into `@` in an option value**, because
`@file` is its own syntax for reading a value from a file — and an AQL
collection bind parameter is written `@@coll`. A statement passed as
`--javascript.execute-string` therefore arrives mangled. What was
observed was a syntax error, which is the lucky case; a query mangled
into something that still parses is what this arrangement rules out. So
the runner is a shell wrapper that takes the statement as its own
argument and exports it, and a JavaScript file reads it from the
environment. The statement never becomes an option value.

## Conformance

**12 of the 15 checks pass, and `conformance_verified` is `false`** —
stated rather than quietly omitted. Check 9 provisions 64 KiB of random
bytes as the source and expects a restore. This adapter's artifact is an
`arangodump` output whose `dump.json` names the database to restore into;
a random file names none, and inventing a name would restore into a
database the backup never mentioned. The other two failures are that
one's cascade.

## Licensing

The ArangoDB Community licence grants use *"only for your internal
business purposes in a dataset that is less than 100GB aggregated across
the cluster"*, and forbids use with any dataset 100 GB or more in
aggregate. So internal business use **is** permitted — the secondary
sources describing it as barring commercial use have it backwards — and
the cap is the real term. It is a cap that bites a drill specifically,
because a drill restores a production dataset: above 100 GB an operator
is on Enterprise, whose licence covers them. The agreement also forbids
working around technical limits, grants an audit on fifteen days' notice,
and is terminable at ArangoDB's discretion. Probavi ships no engine and
accepts nothing on anyone's behalf; the project's position is in
`docs/engine-licensing.md`.

## Deliberately not here

- **HotBackup.** Not in this edition — see above.
- **Encrypted dumps.** Refused by name; a drill must never hold the key.
- **VPack dumps.** `arangodump --dump-vpack` writes a different data
  format, which this adapter does not read.
- **Cluster topology.** One server, one database, by design.
