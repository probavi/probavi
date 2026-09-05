# Aerospike adapter

Restores what `asbackup` writes, into a sandbox this adapter starts and
configures itself, and refuses a restore that would look green while
proving nothing.

Speaks `probavi-adapter/0` (`docs/adapter-protocol.md`). Standard library
only; nothing here imports the Probavi core.

## Source kinds

| Kind | What it is |
|---|---|
| `asbackup` | One `.asb` file, as `asbackup -o` writes it. |
| `asbackup_dir` | A directory `asbackup -d` filled. Every `.asb` file in it belongs to the same backup, and asrestore reads the directory rather than any one file. |

An `.asb` file opens with three lines — the format version, `# namespace
<name>`, and, on exactly one file of a backup, `# first-file`. There is no
timestamp, no engine version and no record count anywhere in it.

Two things follow, and both are refusals rather than guesses:

- **`backup.created_at` is always empty.** Nothing in the artifact dates
  it, so `source.params.backup_timezone` has nothing to interpret and is
  refused rather than ignored.
- **A fragment is refused.** `asbackup` splits a large backup across files
  and writes `# first-file` into the one it wrote first, so a file without
  it is one part of a backup. Restoring it would succeed and restore part
  of the data. The directory kind refuses the same way: no first file, or
  more than one, or two namespaces among the files, and the drill stops
  before a byte moves.

The **backup identity** in the evidence record is the sha256 of the
artifact: for one file, of its bytes; for a directory, of every regular
file in it — relative path then contents, in sorted order — because the
directory is restored whole.

## The sandbox

The sandbox must start **idle**, and the image is the engine's own:

```yaml
sandbox:
  provider: docker
  params:
    image: aerospike/aerospike-server:8.1.2.4
    command: sleep infinity
    memory: 512m
```

One image is the whole sandbox: `aerospike/aerospike-server` already
carries `asbackup`, `asrestore`, `asinfo` and `aql`. No wrapper and no
second image are needed.

The adapter starts the engine on a configuration it writes, and every line
of that configuration was measured against the alternative:

| Setting | Why |
|---|---|
| `node-id` pinned | A node derives one from a network interface's MAC, and under `--network none` there is none: `could not get node id`. |
| loopback `address` in `fabric` and `heartbeat` | Both otherwise look for a routable IPv4 and find none: `no IPv4 addresses configured for fabric`. |
| `proto-fd-max 1024` | Its floor. The image's own configuration asks for 15000, and a container is given 1024 by default, which stops the engine before it starts. Nothing needs raising on the sandbox. |
| the artifact's namespace | asrestore writes each record into the namespace the record names; an engine offering another fails the whole batch. |
| no `admin` / `info` stanza | 8.x renamed it, 7.2 refuses the new name. Leaving it out is what lets one configuration serve both. |

`options.data_size` caps the namespace (default `4G`). Aerospike refuses a
`data-size` below 512 MiB, and the cap is logical rather than an
allocation — a 4 GiB namespace starts in a 384 MiB container — so the
container's own memory limit is the real bound. **384 MiB is the measured
floor for a restore**: at 352 MiB `asrestore` exits 0 while the engine is
OOM-killed and nothing survives.

## Readiness is not what it appears

`asinfo -v status` answers `ok` 0.04 s after launch while a client is still
refused with *not yet fully initialized*, and `unavailable_partitions=0` is
just as early. `asinfo -v 'cluster-stable:'` returns a cluster key at the
instant a client can work — 1.47 s, measured against a polling client — and
that is what the adapter waits for.

## Expiry: the drill is refused rather than reported green

This is the one thing to know before pointing a drill at an Aerospike
backup.

A record's time to live travels inside the artifact as an **absolute
instant**, not as a duration. Once that instant passes, the backup stops
being restorable: `asrestore` drops every such record and **exits 0**.
Measured — three records backed up with a 20-second TTL, restored after it
had passed:

```
Expired 3 : skipped 0 : err_ignored 0 : inserted 0: failed 0
exit 0, and the namespace is empty
```

Nothing on the engine did this, so there is nothing to suspend. The one
switch that would restore them, `asrestore --extra-ttl`, moves the
operator's own recorded expiry forward — and a check reading a record's TTL
must still see what the operator declared. So the adapter **refuses the
drill** on that counter, and the refusal says how many records were
dropped.

For the drill's own duration the engine removes nothing: the namespace runs
with `nsup-period 0`, and `allow-ttl-without-nsup true` beside it, because
the server otherwise refuses every write carrying a TTL once its reaper is
off (`Error while storing record - code 22`, nothing inserted). It is a
suspension, not a rewrite: each record keeps the expiry the operator gave
it.

**It does not make an expired record readable, and nothing does.** Aerospike
applies expiry when a record is *read*. A namespace can report
`objects=1` with `data_used_bytes=64` while a scan returns nothing and a
read by key answers `AEROSPIKE_ERR_RECORD_NOT_FOUND` (measured). That is
why the count the engine reports is never this adapter's verdict.

## What the restore's verdict actually is

Three gates, in this order:

1. **What asrestore reports.** Any expired record, any failure, any error
   it ignored, or nothing inserted at all — and the drill is refused. A
   well-formed zero is refused by name: an empty namespace backs up to a
   structurally valid 42-byte artifact that restores with a zero exit code.
2. **Something a reader can see.** The adapter asks the namespace for its
   sets and scans for one record. A restore nothing can read is refused,
   with the expiry explanation above.
3. **The drill's own checks**, below.

What none of this can catch is a backup directory missing a file from its
*middle*: nothing in the format states how many files or records a backup
holds. A missing first file is caught; a missing middle one is not. The
completeness assertion for that is a check of your own.

## Checks

Aerospike has no SQL, so a check is what the glossary allows where there is
none: the engine's own client arguments. Two clients answer different
questions, and the statement says which by an explicit prefix:

| Check text | Runs | Prints |
|---|---|---|
| `SELECT customer FROM orders.orders WHERE PK = "order-0042"` | `aql` | the row's values, tab-separated |
| `info:sets/orders/orders objects` | `asinfo` | the named field's value |

The prefix exists because **aql cannot count**: `SELECT count(*)` is not a
statement it parses. The core's generating built-in checks — `row_count`,
`table_exists`, `freshness` — emit exactly that, so they do not work
against this engine; a `sql` check with the `info:` form is how a count is
asserted here.

The wrapper around aql is not decoration either. `aql` exits 0 for an
invalid namespace and for a statement it cannot parse at all, so the
runner reads the verdict from what it printed and exits non-zero itself.

## Deliberately not here

- **Conformance is not claimed.** `conformance_verified` is false in the
  manifest. The frozen §10 check 9 provisions 64 KiB of random bytes and
  expects a restore; an `.asb` file states its version and namespace on its
  first two lines, asrestore itself refuses a file without them, and the
  namespace the sandbox must offer is read from that header. Twelve of the
  fifteen checks pass; the other three cannot be made to pass honestly
  without either weakening the fragment fence or widening the harness,
  and the second is a change to a frozen protocol section.
- **No point-in-time recovery.** Nothing in the format expresses one.
- **No secondary indexes or UDFs are asserted.** `asbackup` carries both
  and `asrestore` replays them; this adapter's verdict is about records.
- **Records with no set** cannot be read back through `aql`, which
  addresses a set by name. A backup made entirely of setless records would
  restore and then fail the readable gate.
