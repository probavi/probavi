# probavi-adapter-oracle

Restores Oracle Data Pump dumps into a disposable sandbox so a drill can
prove they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

Oracle is the top of the DB-Engines ranking and the engine most often
named in a recovery audit, and its free edition makes a dishonest drill
easy in three particular ways — all measured on the verified image: the
instance refuses to start on a loopback-only host and dies with an
internal error, an import whose dump carries a scheduler job reports
every row imported while the job deletes them before the first read, and
a dump damaged in its middle passes the header check and then hangs the
import client forever. This adapter's job is to make none of them
callable green.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `oracle_datapump` | one Data Pump dump file — the `DUMPFILE` an `expdp` schema, table or tablespace export wrote |

The dump is imported as it is, into the pluggable database the image's
prebuilt instance has open — `FREEPDB1` on the verified image, read
from the instance rather than assumed — under the schema names it
carries. PITR does not apply; the probe declares `pitr: false` so the
core refuses a `target.pitr` drill before anything runs.

Full-database exports (`expdp FULL=Y`) are not the first slice: they
carry tablespaces, roles and system grants the sandbox's own database
already has, so the import completes with errors — and an import that
completed with errors is reported as `restore_failed` with the first
of them, never as a green. Export the schemas the drill is meant to
prove, or wait for the remapping kind listed under "deliberately not
here".

## The sandbox: the image's own instance, started the adapter's way

The official image — `container-registry.oracle.com/database/free`,
pulled anonymously, no licence key, no registration (measured) —
carries a prebuilt database, and `command: sleep infinity` idles it
because the image has no entrypoint. The adapter starts that instance
itself, from a parameter file of its own that references the image's
spfile and never rewrites it:

```text
spfile=$ORACLE_HOME/dbs/spfile$ORACLE_SID.ora
dispatchers=""
shared_servers=0
job_queue_processes=0
aq_tm_processes=0
```

SQL*Plus startup is synchronous — the call returns when the database is
open, or prints why it is not — so there is no readiness poll to get
wrong; the instance is open in 12–13 seconds on the verified image. Every
session the drill opens, the check runner's included, connects as SYSDBA
over the bequeath adapter (`/ as sysdba`): local IPC, no password, no
listener. The listener is never started.

```yaml
sandbox:
  provider: docker
  params:
    image: container-registry.oracle.com/database/free:23.26.3.0
    command: sleep infinity          # the adapter owns the engine lifecycle
    memory: 3g                       # the measured floor — see below
    network: probavi                 # an internal network — see below
source:
  kind: oracle_datapump
  path: /backups/oracle/orders.dmp
  params:
    backup_timezone: Europe/Budapest # the zone the export ran in — see below
```

**The network.** This is the first adapter in the catalog that cannot
run under the docker provider's default `--network none`: the instance's
IPC layer enumerates the host's interfaces at startup and, finding only
loopback, terminates itself with `ORA-00600 [ksipc: no private ips avail
for use]` — measured at every memory size, and unchanged by
`cluster_interconnects`, `_disable_interface_checking`,
`_ksipc_loopback_ips` or `_ksipc_mode`. The instance needs an interface
to exist; it does not need anything on it. So the sandbox joins an
**internal** Docker network — `docker network create --internal probavi`
— which has no route out (measured: no egress), and the adapter restores
zero ingress on the other side: with the listener never started and the
shared-server dispatchers off, the instance holds **no listening TCP
socket at all** (measured: `ss -ltn` shows only Docker's embedded DNS on
the container's own loopback; the Docker host's scan of the container
finds every port closed). The integration suite reads this back after
every restore. A Kubernetes pod and a bare host always have an
interface; only the docker provider's default is affected. Under
`--network none` the adapter refuses with this instruction in the
engine's own words.

**The memory.** The image's spfile sizes the SGA at 1.5 GiB and the PGA
at 512 MiB, and the instance keeps roughly 2 GiB of anonymous and shared
memory resident; at a 2 GiB container limit it mounts and is killed
while opening (`ORA-03113`, measured), and at 3 GiB it runs with room for
the import. The adapter does not resize the instance to fit a smaller
sandbox: the drill restores into the engine as its vendor ships it.

The `-lite` variant of the image is refused by the toolchain check: it
ships no `impdp`, and its database has the XML component removed, on
which Data Pump's data layer runs (measured: even the server-side
`DBMS_DATAPUMP` fails with `ORA-65047` there). Use the full image; it
is 3.7 GB compressed and 10 GB on disk, which is the cost of this
engine.

## Lifecycle jobs are suspended, not rewritten

A Data Pump dump carries the schema's `DBMS_SCHEDULER` jobs, and they
arrive **enabled**. Measured on the verified image: a job purging rows
older than two minutes, every five seconds, travelled in a dump of five
rows; `impdp` reported `5 rows` imported and `successfully completed`,
and the first read after it found **zero rows** — the job had run once.
Exactly the shape the data-lifecycle rule exists for: a restore that
stands as a success while the engine's own scheduled work subtracts
from what the backup held.

The pin is `job_queue_processes=0` in the launch parameter file: the
scheduler's coordinator never runs, so no job does. The job itself is
untouched — it reads `ENABLED` in `DBA_SCHEDULER_JOBS` exactly as the
operator declared it, with `RUN_COUNT` 0 (measured: all five rows kept,
thirty seconds past every purge window). `aq_tm_processes=0` suspends
the Advanced Queuing time manager the same way, so messages in queue
tables are not expired. Both pins are read back through the engine after
the launch, and an instance that reports either still running is refused
rather than assumed — the rule is never to rest on a default.

Two related mechanisms need no pin and are named for completeness. The
automatic maintenance tasks (statistics, segment and SQL tuning
advisors) run as scheduler jobs and are suspended with the rest. ADO /
ILM policies that travel with a table only act on heat-map data, and the
image ships with `heat_map=OFF` (measured), which the drill never turns
on. Flashback Data Archive history is not part of a schema export.

The integration suite proves the rule from both sides: a control
instance, started exactly as the image's own run script starts it,
imports the fixture and loses rows within seconds; the drill imports the
same fixture through the adapter and keeps every one.

## The verdict reads: exit codes, a header, and a watchdog

**The header, through the engine.** A dump file's format is Oracle's own,
so the host vets nothing about the bytes — it measures the file's size
and SHA-256 and leaves every verdict to the documented reader,
`DBMS_DATAPUMP.GET_DUMPFILE_INFO`, run in the sandbox after the
transfer. From it the adapter reads the file type (a Data Pump dump, an
original `exp` file, or neither), the writing version, the encryption
flags, and the export's wall clock. A file the engine cannot read as a
dump — truncated, or random bytes — is refused there with `ORA-39211`
(measured, within seconds), before the import is attempted. An `exp`
file is `unsupported_source` by name; an encrypted dump is refused
because importing it needs the encryption password, which would have
to cross the protocol inside a payload.

**The import's exit code.** `impdp` exits 0 on success, 5 when the job
completed with errors (`ORA-31684 … already exists` and its kin), and 1
when it could not open the file at all (`ORA-27046: file size is not a
multiple of logical block size` for a truncated file, `ORA-39411: header
checksum error` for random bytes — measured). Zero is the only green;
five is `restore_failed` with the count of error lines and the first of
them — a restore with errors proves nothing about the backup; one is
`source_corrupt` when the engine's words say the file, `restore_failed`
otherwise.

**The watchdog.** One failure never returns. A dump damaged in its
middle has an intact header, and the import's worker dies loading the
dump's master table — `impdp` prints `ORA-39776: fatal Direct Path API
error` and then waits forever on a job whose state reads `UNDEFINED`
(measured: over ten minutes before it was killed by hand). So the
adapter polls the job's own state in `DBA_DATAPUMP_JOBS` while the
client runs: a job that is neither `DEFINING` nor `EXECUTING` for sixty
seconds straight while its client still waits is dead, the client is
killed, and the drill reports `source_corrupt` with the engine's line. A
healthy job is `EXECUTING` until it completes and its client leaves
within a second, so the grace period costs a sound restore nothing.

## Version pairing: dumps do not import into older releases

The header's writing version is the source instance's `compatible`
setting (`23.06.00.00.00` for a dump the verified image exported —
zero-padded where the engine prints `23.26.3.0.0`, so the two are
compared segment by segment as numbers). A dump written at a version
newer than the sandbox engine is refused before the import, naming both;
the engine's own refusal, `ORA-39142: incompatible version number`, is
mapped to the same code if it arrives anyway. Older dumps import — Data
Pump is upward compatible — and the pre-check stays silent on anything
it cannot read.

## Checks: SQL*Plus in the pluggable database, and the built-ins apply

The declared `sql_runner` runs the check text as one SQL statement
through SQL*Plus, connected over the bequeath adapter into the restored
pluggable database (`ORACLE_PDB_SID` carries the core's `{{database}}`
placeholder), with the session shaped for the protocol's output
contract: no heading, no feedback, CSV markup with a tab delimiter and
no quoting, so rows arrive as undecorated tab-separated values — no
padding, `NULL` empty, numbers unformatted to forty digits (measured). A
failed statement exits non-zero with the engine's `ORA-` line on
stderr. `NLS_LANG=.AL32UTF8` is set for the runner; without it the
client renders every non-ASCII character as `?` (measured).

The core's generating built-ins apply to this engine:

- `table_exists` and `row_count` — the core quotes identifiers the SQL
  standard way, which Oracle honours **case-sensitively**: name tables
  as the dictionary stores them, upper case unless they were created
  quoted (`table: PROBAVI_APP.ORDERS`).
- `freshness` — the session sets `NLS_DATE_FORMAT`,
  `NLS_TIMESTAMP_FORMAT` and `NLS_TIMESTAMP_TZ_FORMAT` so `max()` of a
  `DATE`, `TIMESTAMP` or `TIMESTAMP WITH TIME ZONE` column prints in a
  form the core parses; a column without a zone is read as UTC, as the
  core documents.

A custom check is one query; a trailing semicolon is accepted and
stripped. `&` is literal (`define off`), blank lines inside the text are
fine. The check text reaches the engine as the string it was — measured
against shell and SQL metacharacters.

## When the backup was taken

The dump's header records the export's wall clock as the source
instance printed it — `Fri Aug 21 12:04:02 2026`, no offset. The offset
is a fact only the operator has, so `source.params.backup_timezone`
names the IANA zone the export ran in, and the drill records
`created_at` from the header in that zone. Without the declaration
`created_at` is null; nothing is guessed.

## Backup identity

`source_identity.checksum` is the SHA-256 of the dump file's bytes;
`size_bytes` its size. The checksum is a measurement of the bytes the
sandbox imported, computed on the host before the transfer.

## Licence and support, in the vendor's words

The image ships the *Oracle Free Use Terms and Conditions*, which grant
the right to "internally use the unmodified Programs for the purposes of
developing, testing, prototyping and demonstrating your applications,
and running the Programs for your own internal business operations" — a
restore drill of your own backups, in a sandbox you destroy. The
vendor's stated limits are 2 CPUs for foreground processes, 2 GB of RAM
for the SGA and PGA combined, and 12 GB of user data; a dump larger than
that will not import. The vendor also states that Oracle Database Free
"is not supported and does not receive any patches, including security
patches" — it is a single rolling line, of which one version is current
at a time, and that is the version the manifest lists. Nothing here is
legal advice; read the licence text in the image.

## Deliberately not here

- **RMAN backups** — restorable on the same image (measured: a
  compressed backup set with control file autobackup restored, recovered
  and opened `RESETLOGS` in 71 seconds), but the drill must derive the
  recovery target from the backup's own catalogue and replace the
  sandbox's database by the backup's identity; that is a source kind of
  its own.
- **Full-database dumps and remapping** (`REMAP_SCHEMA`,
  `REMAP_TABLESPACE`) — see "what it restores".
- **Multi-file dump sets** (`DUMPFILE=exp%U.dmp`) — one file for now.
- **Encrypted dumps** — the password would have to cross the protocol.
- **The `-lite` image** — cannot run Data Pump (measured).
- **Enterprise and Standard Edition images** — behind a login the
  verified list cannot carry (`docs/engine-versions.md` §1). The adapter
  does not look for them; it looks for `sqlplus`, `impdp`, `ORACLE_HOME`
  and `ORACLE_SID`.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist, is a directory, or is empty | `source_not_found`, `invalid_request`, `source_corrupt` |
| `backup_timezone` is not an IANA zone name | `invalid_request` |
| the sandbox image lacks the toolchain (the `-lite` variant among them) | `invalid_request`, naming it |
| an instance is already running in the sandbox | `invalid_request`, teaching `sleep infinity` |
| the sandbox has no network interface | `invalid_request`, teaching the internal network |
| the instance dies while starting (`ORA-03113`) | `invalid_request`, naming the 3 GiB floor |
| a pin did not take (`job_queue_processes`, `aq_tm_processes`) | `invalid_request`, naming what the instance reports |
| no pluggable database open read write, or several | `restore_failed`, `invalid_request` |
| the engine cannot read the file as a dump (`ORA-39211`) | `source_corrupt` |
| an original `exp` file, or an encrypted dump | `unsupported_source` |
| a dump written at a newer version than the sandbox engine | `invalid_request`, naming both |
| `impdp` completed with errors (exit 5) | `restore_failed`, with the first error |
| `impdp` rejected the file (`ORA-39411`, `ORA-27046`, `ORA-39000`) | `source_corrupt` |
| the import job died and its client hung | `source_corrupt` — the watchdog |
| anything else the import printed | `restore_failed`, with the engine's line |
