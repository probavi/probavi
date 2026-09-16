# Apache IoTDB adapter

Restores an [Apache IoTDB](https://iotdb.apache.org/) offline copy — the data
directory of a stopped standalone node — into a disposable sandbox, and proves
the engine can read every value in it.

Implements `probavi-adapter/0` ([docs/adapter-protocol.md](../../docs/adapter-protocol.md)).
Standard library only, no imports from the Probavi core.

## The headline: a count proves nothing here

IoTDB answers `count(*)`, `sum` and their neighbours from chunk statistics, not
by reading the pages the statistics describe. Twelve single-byte changes to a
TsFile, at evenly spaced offsets, all loaded as success with `count` and `sum`
identical to the source (measured 2026-09-16 on 2.0.11). Reading every row
told them apart:

| | whole file | 8 of the 12 changes | 4 of the 12 changes |
|---|---|---|---|
| loads | yes | **yes** | **yes** |
| `count`, `sum` | as written | **as written** | **as written** |
| every value read | as written | refused: `724 Failed to decode page data` | **other values, no error** |

So the drill's verdict is a full read. After the restore this adapter asks the
engine two questions of every tree database (one row per device) and every
table (every column): the counts from statistics, and
`count(cast(… as TEXT))`, which the engine can only answer by decoding every
value. A read the engine refuses fails the drill as `source_corrupt`, naming
the database and the engine's words; so does a read that counts differently
from the statistics. Six single-byte changes to a data file inside an offline
copy all failed that read, and a truncated data file surfaces the same way —
the engine starts on it normally and refuses the first read that reaches its
metadata (measured). IoTDB 1.3 words the same refusals differently — a generic
`301` carrying `java.nio.BufferUnderflowException` or "Error happened while
scanning the file" — and the adapter reads both lines' words as damage; a
refusal in any other words fails the drill without blaming the backup.

The four changes that still decoded are the residual, stated rather than
implied: a TsFile page carries no checksum, and **no read the engine can do
tells a changed value that still decodes from a written one.** In a 1.3.7
copy, one of the changes the suite tried read back without an error at all.
A drill proves
that the copy restores and reads whole; what a value was is the operator's
backup job's to protect.

The full read takes time in proportion to the data. It is not counted in
`restore_seconds`: an operator reads that number as the time a recovery
takes, and a recovery does not read every value first.

## Source kinds

| Kind | What it is |
|---|---|
| `iotdb_data` | The data directory of a stopped standalone node, or the target of `tools/ops/backup.sh`. |
| `iotdb_data_tar` | A tar archive of one, plain or gzip. |

The artifact is the directory holding both nodes' records of themselves,
`confignode/system/confignode-system.properties` and
`datanode/system/system.properties`. It may sit at the path the drill names or
up to three levels below it: the vendor's backup tool copies a whole
installation (its target holds `lib/`, `conf/` and `sbin/` beside `data/`),
and only `data/` moves into the sandbox. A path holding two nodes' data
directories is refused rather than guessed between. For an archive the
compression is read from the first bytes, never from the name.

Taking the copy, on the node:

```sh
stop-standalone.sh
tools/ops/backup.sh -node all -targetdir /backups/iotdb/2026-09-16   # or copy the data directory
start-standalone.sh
```

The node has to be stopped: a data directory copied from a running node is
the copy of a database in the middle of a write. A directory that is still
changing when the drill starts is refused as still being written; a copy job
that writes to a temporary name and renames on completion never trips that.

**Backup identity.** A directory is hashed as one artifact: every regular file
under the data directory, by its path relative to it and its bytes, in sorted
path order. An archive is hashed as the bytes on disk. `backup.created_at` is
always null: nothing in the copy dates it — a data file's name records when
that file was created, and a file's modification time dates the copy.

Not a source kind, each for a measured reason:

- **`export-data.sh` SQL and CSV.** Both lose data and exit 0. The tree
  dialect's SQL export of `root.rig.**` left out an aligned device entirely,
  the table dialect's writes timestamps unquoted so that its import refuses
  every line, and a CSV import without the schema turns INT64 into FLOAT and
  DATE and BLOB into STRING.
- **TsFile exports.** The tree dialect's restored identical, but carries no
  TTL and no schema settings; the table dialect's drops ATTRIBUTE columns
  without a word, and a TsFile exported at nanosecond precision does not load
  into a sandbox at millisecond precision. The import tool exits 0 for every
  outcome, reporting a zero-byte file as a success.

## Credentials

An offline copy keeps the users and passwords of the node it was taken from:
`root`/`root` is refused by a copy whose root password was changed (measured).
The drill names the password the copy holds, as a variable name the core
resolves:

```yaml
target:
  adapter: iotdb
  source:
    kind: iotdb_data
    path: /backups/iotdb/2026-09-16
    credential_env: [IOTDB_ROOT_PASSWORD]
  options:
    password_env: IOTDB_ROOT_PASSWORD
```

Without `password_env` the adapter uses IoTDB's default, and a refused login
fails the drill as `invalid_request` saying so. The password reaches the
sandbox in a process environment and curl on its standard input; it is on no
argument list.

The user has to be able to read everything. A user without read privilege is
answered an empty result, not an error (measured), so a drill read as such a
user would find nothing to prove; `root`, the default, can.

## Checks

IoTDB speaks two dialects on one server, and `options.database` chooses
between them:

- **Unset: the tree dialect.** Statements address series by path
  (`select count(v) from root.rig.d2`). The generating built-ins
  (`table_exists`, `row_count`, `freshness`) do not apply — the tree dialect
  has no tables and refuses their SQL.
- **Set: the table dialect**, run in that database (IoTDB 2.0 and later). The
  generating built-ins work as the core composes them, SQL-standard quotes
  included; the database must be one the copy holds, or the drill is refused.

```yaml
target:
  options:
    database: rig_t
checks:
  - builtin: service_healthy
  - builtin: row_count
    table: rig_t.sensors
    min: 1
  - builtin: freshness
    table: rig_t.sensors
    column: time
    max_age: 24h
  - name: every plant still reports
    sql: "SELECT count(DISTINCT plant) FROM sensors"
    expect: "5"
```

A check runs over the engine's REST service, which the adapter turns on in the
sandbox, and its answer is written as §6.1 rows: tab-separated, a NULL as an
empty field, a string exactly as stored (the CLI's bordered table is not used:
it splits a value containing `|`, trims the spaces around a value, and prints
NULL and the string `null` alike). A timestamp — the tree dialect's time
column, and any TIMESTAMP value — is RFC 3339 in UTC at the cluster's
precision. A number is the engine's own text. IoTDB 1.3 answers a DATE as
`20260901` and a BLOB as its raw bytes where 2.0 answers `2026-09-01` and
`0x…`; the runner passes both through as the engine wrote them.

## Data lifecycle: TTL

A TTL travels in the copy and is enforced when a query runs. Rows backed up
inside a five-minute TTL read back 14 of 120 two and a half minutes later, and
none 24 seconds after that (measured). Nothing suspends it —
`ttl_check_interval` schedules only the physical deletion — and unsetting a TTL
would rewrite a policy a check is entitled to read.

So a drill is refused where a TTL hides everything it covers: a path that
holds series, or a table, and reads no row. The message names the scope and
the TTL. A scope that still reads rows is drilled as it reads: rows that
crossed the TTL since the copy was taken are hidden, which a `row_count` bound
has to allow for.

## Node addresses

A copy records the addresses and ports its node was configured with, and the
engine does not start on other ones (measured). The adapter writes them into
the sandbox's configuration:

- **Loopback** (`127.0.0.1`, `::1`) starts as recorded.
- **A host name** starts when the sandbox names the same host and the name
  resolves to loopback; the adapter maps it in the sandbox's `/etc/hosts`.
  Either half alone gave no answer in 150 s.
- **Any other IP address** cannot start: the sandbox has only loopback, and
  the ConfigNode exits with `Network is unreachable`. The drill is refused
  before the engine is started, naming the address.

## Versions

A copy written by a newer minor line than the sandbox's engine is refused
before the engine starts: a 2.0.11 copy never serves under 1.3.7 — its
ConfigNode fails at startup — while a 1.3.7 copy restores identical under
2.0.11 (both measured). Patch releases within one line were not measured
against each other and are not refused.

## The sandbox

The official standalone image, idle:

```yaml
sandbox:
  provider: docker
  params:
    image: apache/iotdb:2.0.11-standalone
    command: sleep infinity
    memory: 1g
```

`command: sleep infinity` replaces the image's `entrypoint.sh all`, and the
adapter starts both nodes itself on a copy of the image's configuration under
the scratch directory; the image's own `/iotdb/data` is never written. Memory
is split from the sandbox's limit the way 2.0.11 splits it for itself — half
to the DataNode, three tenths to the ConfigNode — and written into the copied
configuration, because 1.3.7 sizes both heaps from the host instead (`-Xmx12724M`
inside a 2 GiB container, measured) and ignores the environment variable that
would override it.

**The memory floor is set by the region count, not the data size.** Every data
region reserves a WAL buffer of direct memory. The adapter sets that buffer to
4 MiB rather than the default 32 MiB — a drill writes nothing — which took a
copy of four regions from not starting at 1 GiB to starting in 4.8 s. A copy
with many databases needs a correspondingly larger sandbox; a copy the engine
cannot fit fails its readiness, and the drill carries the last error the
engine logged.

## What this adapter does not claim

- **Clusters.** One standalone node's data directory is one drill. A copy of
  one member of a multi-node cluster is not a whole database, and is not
  treated as one.
- **Values that still decode.** See the headline.
- **Continuous queries, pipes and triggers.** Whether they travel in a copy and
  run unbidden in the sandbox was not measured. The sandbox has no network, so
  a pipe has nowhere to send to.
- **The bare-host provider.** Not verified: it has neither the `/etc/hosts`
  mapping a host-named copy needs nor a cgroup the engine's own sizing reads.

## Drill config options

| Option | Default | Meaning |
|---|---|---|
| `user` | `root` | The user the verdict and the checks read as. |
| `password_env` | — | Name of the environment variable holding that user's password. It must also appear in the drill's `source.credential_env`. |
| `database` | — | A table-model database: checks run in the table dialect, in it. Unset, checks run in the tree dialect. |

The adapter declares no `source.params`; a drill that sets one is refused
rather than silently ignored.

## Environment variables

None of its own. The variable `options.password_env` names is read through the
core's allowlist like any declared credential.
