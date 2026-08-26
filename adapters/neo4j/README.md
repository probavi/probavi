# probavi-adapter-neo4j

The Neo4j engine adapter for Probavi, implementing `probavi-adapter/0`
(see `docs/adapter-protocol.md`). Standard library only — deliberately no
imports from the Probavi core; like the postgres and mysql adapters, it is
written from the protocol document alone.

## Supported source kinds

| Kind             | Meaning                                                    |
|------------------|------------------------------------------------------------|
| `neo4j_dump`     | One `neo4j-admin database dump` archive, under any file name. |
| `neo4j_dump_dir` | A directory of dump archives; the newest regular file is restored (mtime, ties broken by name — an archive carries no timestamp of its own). |

The file name is the backup job's business, not Probavi's.
`neo4j-admin database load` derives the file it reads from the *database*
name, so the adapter places whatever it was handed inside the sandbox as
`<database>.dump`. A job writing `orders-2026-08-26.dump.gz` needs no
renaming step to be drillable.

Enterprise's `neo4j-admin database backup` artifact is not a source kind
here. `load` accepts one, but nothing in this repository verifies that
path, and "verified against" is never widened into "supports".

## The sandbox starts idle, and the adapter starts the engine

The sandbox image must contain `neo4j`, `neo4j-admin` and `cypher-shell` —
the official `neo4j:5.26-community` image does — and it must **idle**:

```yaml
sandbox:
  provider: docker
  params:
    image: neo4j:5.26-community
    command: sleep infinity
    memory: 1g
```

`command: sleep infinity` is not a convenience. Two facts make it
necessary, both measured on the verified image:

- **A dump can only be loaded into a database no server has mounted.**
  With the server running, `neo4j-admin database load` refuses: *"The
  database is in use. Stop database 'neo4j' and try again."*
- **The initial password only takes effect before the first start.** The
  tool says so itself when it sets one.

So the adapter prepares the store while nothing is running — hostname,
toolchain, password, dump, load — and then starts the server with
`neo4j start`. The image's own entrypoint still runs (it fixes
directory ownership and applies any `NEO4J_*` settings) and then execs
the idle command instead of the engine.

Memory: the container needs about **1 GiB**. Measured — 512 MiB never
becomes ready, 768 MiB serves with roughly 420 MiB in use.

### Authentication in the sandbox

The engine's `neo4j` user is given a **documented constant** password,
`Probavi-DrillSandbox-0`, which the declared `sql_runner` passes to
`cypher-shell` through `NEO4J_PASSWORD`. It is not a secret: Neo4j has no
way to authenticate a client without a password, and the core's ephemeral
per-drill secret cannot be used, because its value would have to cross
the protocol to reach the engine and §2.5 forbids that. This is the same
choice the mssql adapter makes for `sa` and the postgres adapter makes
with a `pg_hba` trust overwrite: publicly known access, confined to a
sandbox with zero ingress (`--network none`, publishing ports not
expressible). The credential never protects anything reachable.

`neo4j-admin` accepts the password only as a positional argument — stdin
is refused with *"Missing required parameter: '<password>'"* — so it is
visible in the sandbox's own process list. That list belongs to a
disposable container which already holds the restored production data.

### A zero-ingress sandbox needs one line in `/etc/hosts`

Neo4j refuses to boot in a host that cannot resolve its own name, and a
`--network none` container is exactly that: Docker gives it a hostname
but no address, so no entry is written for it. What the operator sees is
*"Configuration is invalid. See log for more info."* — with nothing in
`neo4j.log`, nothing in `debug.log`, and a configuration
`neo4j-admin server validate-config` calls valid. Setting
`server.default_advertised_address=localhost` does not help; measured.

The adapter therefore adds `127.0.0.1 <hostname>` to the sandbox's
`/etc/hosts` **only when the name does not already resolve**. On a bare
host — the `remotehost` provider — it always resolves, so the file is
never touched.

## Which database a drill restores into

Neo4j Community Edition serves exactly one user database, the one its
configuration names (`neo4j` in the official image), and it cannot create
another. The adapter loads into that database and the restored data is
proven there; the dump's own database name is irrelevant to the outcome
and is recorded in the drill log for reference.

`target.options.database` overrides the target name for an image
configured differently. It is not a way to restore several databases into
one sandbox: naming a database the server does not serve fails the drill —
loudly, and for a reason worth stating.

**A dump loaded under a name the server cannot mount lands on disk and is
never served.** Measured: `/data/databases/orders` exists, the load
reports success, the server starts, `SHOW DATABASES` never lists
`orders`, and a query against it answers *"this database does not
exist"*. Every check would then have run against the default database —
empty — and reported green. So the adapter does not call a restore
successful until the engine itself says the restored database is
`online`, and a refusal names what the server does serve:

```
the engine does not serve the restored database orders (it serves
neo4j (online), system (online)): a dump is only proven once the server
mounts it, and Community Edition mounts only the database its
configuration names
```

A database that is mounted but stopped (`offline`) fails the same way.
Only states a database leaves on its own — `starting`, `initial`, `store
copying` — are waited out, within the three-minute readiness budget.

## What a drill with this adapter proves, and what it does not

A dump of a user database carries that database: its nodes,
relationships, properties, indexes and constraints. It does **not**
carry the `system` database, so users, roles and database definitions are
not part of what a `neo4j_dump` drill proves. On Community Edition there
is one built-in user and no roles, so this costs nothing there; on a
server with its own accounts, recovery of those accounts is a separate
question this kind does not answer.

## Nothing in the sandbox expires the artifact

A drill sandbox must not apply the engine's own data lifecycle to the
backup it is proving. Neo4j Community is the rare engine with nothing to
suspend: measured across its settings, there is no TTL, no expiry and no
scheduled deletion. The two settings whose names suggest otherwise are
`dbms.routing_ttl` (how long a client may cache a routing table) and
`db.tx_log.rotation.retention_policy` (which prunes transaction logs, not
data).

Because that answer is a property of the image rather than of the
adapter, the integration suite guards it: a test fails if the sandbox
grows a setting matching `ttl`, `expir`, `retention` or `evict` that this
adapter has not considered, if `db.transaction.timeout` is no longer `0s`
(a long check must not be cut short mid-validation), or if the image
ships plugin jars — APOC's TTL deletes nodes on a schedule, and a
zero-ingress sandbox cannot download it, but an image could carry it.

## Checks: the Cypher dialect

Neo4j has no SQL. The declared `sql_runner` runs the check text through
`cypher-shell`, so **checks for this adapter are Cypher queries**,
written in the drill config's raw `sql` field:

```yaml
checks:
  - name: orders_present
    sql: "MATCH (o:Order) RETURN count(o)"
    expect: "500"
  - name: relationships_survived
    sql: "MATCH ()-[r:PLACED]->() RETURN count(r)"
    expect: "100"
```

Builtin checks that generate SQL (`row_count`, `table_exists`,
`freshness`) do not apply — use raw Cypher. `service_healthy` works
unchanged.

Output is the undecorated rows the runner contract requires, and the
adapter does the undecorating: `cypher-shell --format plain` still prints
a header row, quotes string values, and joins columns with `", "`, so the
runner drops the header, splits rows outside the quoting, and prints
fields tab-separated and unquoted. A string that itself contains `", "`
therefore stays one column. A failed query exits non-zero with the
engine's message on stderr.

Checks run with `--access-mode read`. A check may read what the drill
restored and may not change it; one that writes fails loudly (*"Writing
in read access mode not allowed"*) rather than quietly altering the
artifact its evidence record is about.

### When the backup was taken

`created_at` in the evidence record is **always null** for this adapter,
and that is deliberate. The engine's own reader for the artifact —
`neo4j-admin database load --info` — reports the database name, the
archive format, and the file and byte counts, and no timestamp at all
(measured against Neo4j 5.26). The file's modification time is not a
substitute: copying a backup without preserving timestamps resets it, and
a month-old artifact then looks like last night's, so reporting it as a
creation time would put a claim in a signed record that the backup does
not support.

The `source.params.backup_timezone` key the other adapters use to place a
backup's wall clock in a zone has nothing to act on here, and a config
that sets it is **refused** rather than silently ignored — an operator who
wrote it is expecting an accuracy this kind cannot deliver.

## Backup identity

- `checksum`: SHA-256 over the selected artifact's bytes (for
  `neo4j_dump_dir`, the chosen file).
- `created_at`: always null — see above.

## Which backup a drill restores, and when it refuses

For `neo4j_dump_dir` the adapter picks the newest regular file, breaking
ties toward the lexicographically larger name so the choice is the same
on every run. Because the adapter chose the file rather than the
operator, it also refuses one a backup job is still writing: an artifact
whose size or mtime moves while it is being looked at fails the drill
with a message that says what to do. Skipping it and restoring
yesterday's would be worse — the drill would prove an older backup while
the record implied the newest, and nothing in the evidence would say so.
A job that writes to a temporary name and renames on completion never
trips this at all; that is the real fix.

## Errors it reports

| Code | When |
|---|---|
| `unsupported_source` | A kind this adapter does not declare. |
| `source_not_found` | The file or directory is not there, or the directory holds no files. |
| `source_unreadable` | The artifact cannot be read, or is still being written. |
| `source_corrupt` | The engine does not recognize the artifact as a Neo4j archive — measured shapes: *"Not a valid Neo4j archive"* (a file that never was one) and *"ZstdIOException: Truncated source"* (a transfer cut short). |
| `restore_failed` | The load ran and failed for engine reasons, or the server did not end up serving the restored database. |
| `engine_not_ready` | The sandbox could not be made to resolve its own name, the password could not be set, or the server never started or never served the database. |
| `invalid_request` | PITR requested, `backup_timezone` set, an unusable database name, or an image without the engine's tools. |

## Drill config options

| Key | Default | Meaning |
|---|---|---|
| `target.options.database` | `neo4j` | The database to restore into and run checks against. 3 to 63 characters, beginning with a lowercase letter, then lowercase letters, digits, dots and dashes — Neo4j's own rule. Uppercase is refused rather than folded: the engine normalizes names to lowercase, and a record naming a database the server never had would be worse than a stopped drill. |

## Environment

None. The adapter needs no credentials to read a dump; a dump is a file.

## Verified engine versions

`adapter.json` lists what CI restores from, and
`docs/engine-versions.md` is normative for how that list is chosen. Only
the **5.26 LTS** line is listed; the vendor's published end of support
for it is **2028-06-06** (checked 2026-08-26).

The calendar-versioned line (2025.x, 2026.x) is deliberately absent, and
not because it does not work: Neo4j supports exactly one calendar release
at a time — the newest — so an entry naming one would be end-of-life
within weeks of being written, and the manifest would turn into monthly
version bookkeeping that tells a reader nothing about durability
(`docs/engine-versions.md` §1, rule 2). The LTS line says something
durable instead. Nothing here claims the calendar line does or does not
work: it is simply not verified in this repository.

## Version pairing

A dump loads into the version that took it or a newer one; the engine
refuses to open a store from a newer release, and a major-version jump
needs `neo4j-admin database migrate` after the load. A drill sandbox
should therefore run the same line as the server the backup came from,
which is what a drill is for anyway: proving the backup restores into the
engine you actually run.

## PITR

Not supported. Neo4j Community has no point-in-time recovery from a dump;
a dump is a full copy of the database as of the moment it was taken. A
drill requesting PITR is refused rather than silently ignored.
