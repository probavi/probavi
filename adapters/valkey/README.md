# probavi-adapter-valkey

Restores Valkey RDB snapshots and append-only directories into a
disposable sandbox and serves the restored keys, implementing
`probavi-adapter/0` (docs/adapter-protocol.md). Self-contained: standard
library only, no imports from the Probavi core.

## Supported source kinds

| Kind             | Path points at |
|------------------|----------------|
| `valkey_rdb`     | One RDB file — a copied `dump.rdb`, or the output of `valkey-cli --rdb` |
| `valkey_rdb_dir` | A directory of RDB files; the newest **by the artifact's own save instant** is restored |
| `valkey_aof`     | A copy of the append-only directory (`appendonlydir`): manifest, base, incremental segments — replayed in full |

RDB is what the engine itself recommends for backups. What this adapter
does not restore, on purpose, is in "Deliberately not here" below.

## Sandbox image: start it idle

The official `valkey/valkey` images carry everything the flow needs —
`valkey-server`, `valkey-cli`, `valkey-check-rdb` — but their default
command boots an empty server on the port the restored one needs. Start
the sandbox idle and let the adapter own the lifecycle:

```yaml
sandbox:
  provider: docker
  params:
    image: valkey/valkey:7.2.14
    command: sleep infinity
    memory: 256m
source:
  kind: valkey_rdb
  path: /backups/valkey/dump.rdb
```

The adapter places the RDB in its own data directory, has
`valkey-check-rdb` vet it, then starts the server daemonized with
persistence pinned off (`--appendonly no`, `--save ""`): the server loads
the placed RDB and never rewrites the artifact under the drill. No shell
is required for the engine flow — every step is direct argv — though the
check runner below does use `sh` for word splitting, which every official
image carries.

## The append-only kind: the manifest is the contract

Valkey kept Redis 7's append-only layout: a directory holding a text
manifest plus the files it names — one base (an RDB by default) and
incremental segments. `valkey_aof` restores a copy of that directory,
and the manifest gives it the gate AOF backups need most: **a copy
taken mid-rewrite loses members**, and a manifest naming a file the
backup does not hold is refused by name as an incomplete copy, before a
byte reaches the sandbox. In the sandbox the set is vetted **member by
member**, mirroring exactly what the server loads: the base goes to
`valkey-check-rdb`, an `.aof` base and every incremental segment to
`valkey-check-aof`'s single-file mode, and history members the server
never loads are transferred but not vetted. Member-by-member is not a
style choice: `valkey-check-aof`'s manifest mode misreads the
`VALKEY`-magic base a 9.x rewrite writes as a RESP base and rejects the
healthy set the server itself produced (measured), and a gate stricter
than the engine would fail restorable backups. The server is then
started with `--appendonly yes`, `--save ""`, and `--appendfilename`
derived from the manifest's own name — an unmatched name would make the
server silently start a fresh, empty append-only set, exactly the false
green a drill must not produce. The base RDB's header feeds the same
version pre-check and Redis-dialect fence as the rdb kinds, both header
layouts included (`REDIS` through 8.x, `VALKEY` from 9.0).

Two deliberate differences from the rdb kinds. `backup.created_at` is
**null**: the base's `ctime` dates the last rewrite, not the backup,
and the incremental tail extends past it — an append-only directory
does not date itself, and a wrong instant is worse than none. And there
is no `valkey_aof_dir`: with nothing to rank candidates by except
copy-fragile file times, "newest" would be a guess; point the drill at
one specific append-only directory instead.

There is no auth to reset, unlike every SQL-family engine: `requirepass`
and ACLs live in server configuration, not in the RDB, so the restored
data carries no credentials. The server serves without auth inside a
sandbox that has no network exposure whatsoever (`--network none`, no
ports expressible).

## Expiry: what a drill can and cannot prove

Valkey expires by wall clock, not by elapsed time. Every key with a TTL
is stored with an **absolute instant**, and the artifact carries those
instants unchanged — so a backup taken while its keys were alive holds
keys that are dead by the time a drill reads it, and the restored server
treats them exactly as a production server would.

It happens twice over, by two different mechanisms. Both measured on every
verified version, with 100 permanent keys beside 100 given a
twenty-second expiry:

| | at the backup | in the drill |
| --- | --- | --- |
| RDB kind — keys dropped as the artifact is read | 200 | **100** |
| append-only kind — keys removed by the expiry cycle after load | 200 | **100** |

The engine is candid about the first: `rdb_last_load_keys_loaded:100`,
`rdb_last_load_keys_expired:100`, and a log line saying the same. The
second is quieter — the base file is read as an append-only preamble,
which skips the check entirely (`expired:0`, `loaded:200`), and the
ordinary expiry cycle takes the keys seconds later.

**There is nothing to switch off.** The discard is what a master does; the
one shape that skips it is a replica, and that is worse rather than
better. Measured: a replica started on the same artifact reports
`DBSIZE 200` and `rdb_last_load_keys_expired:0` while `GET session:1`
returns nothing. The keys are in the keyspace and unreadable — a count
that lies is a worse foundation for evidence than a count that is short,
so the sandbox stays a master.

**What the drill does instead** is read the engine's own account of the
load. The artifact carried `loaded + expired` keys; the server now serves
what `INFO keyspace` adds up to. When that sum is zero and the artifact
carried keys, the drill has nothing to prove anything with, and fails
saying so rather than reporting a successful restore of an empty server.

The residual is worth stating plainly: a backup that lost only *some* of
its keys still serves keys, so the drill proceeds and proves what remains.
No fence can do better — a threshold would be arbitrary, and no setting
makes a server return an expired key. **Drill backups younger than their
keys' time to live**; for a keyspace that is all sessions or caches, that
is the only arrangement that proves anything.

## Checks: lines of valkey-cli arguments

Valkey has no SQL, so the generating built-in checks (`row_count`,
`table_exists`, `freshness`) do not apply — the same consequence the
Redis, MongoDB, and etcd adapters document. A check's text is one line of
`valkey-cli` arguments, run through the probe-declared template:

```yaml
checks:
  - kind: query
    sql: get probavi:config
    expect: restored-ok
  - kind: query
    sql: dbsize
    expect: "501"
```

The template word-splits the text without shell parsing (`set -f`, `$0`
expansion), so `;`, `|`, `$()` and glob characters in a check stay literal
arguments. `valkey-cli -e` makes a command-level error exit non-zero,
which is how a failing check fails.

## When the backup was taken

An RDB records its save instant in the header (`ctime`, epoch seconds —
measured). Epoch seconds carry no zone question, so `backup.created_at` is
exact with **no** `backup_timezone` declaration — like the postgres
adapter's pgBackRest kind, and unlike every wall-clock format. Declaring
`backup_timezone` anyway is refused rather than ignored.

The same rule ranks the directory kind: the newest artifact **by its own
ctime** wins, a dated artifact outranks every undated one, and only
artifacts with no readable ctime fall back to file time. Files without an
RDB magic (checksum sidecars, READMEs) are not candidates; if the chosen
artifact turns out broken — or turns out to be a Redis artifact, see the
dialect fence below — the drill fails rather than quietly restoring an
older neighbour.

## The dialect fence: Redis artifacts are refused by name

Since the fork the two RDB dialects diverge above format version 11:
Redis 7.4+ writes format version 12 under the `REDIS` magic, Valkey kept
11 until 9.0 switched to its own `VALKEY` magic and numbering — and
neither engine loads the other's post-fork files. Restoring a Redis
backup into a Valkey sandbox — even a pre-fork one that would load —
would prove recovery into an engine the backup does not belong to, the
false green ROADMAP.md names.

So the adapter refuses, before a byte is transferred, on positive
evidence of a Redis save (all measured against the official images):

- a `redis-ver` aux field — Valkey has written `valkey-ver` and never
  `redis-ver` since the fork, so the field can only come from Redis;
- a `REDIS`-magic format version of 12 or above — only post-fork Redis
  writes those, and `valkey-check-rdb` **passes** them even though the
  server then refuses the load ("Can't handle RDB format version 12"),
  which is why the fence cannot be left to the in-sandbox integrity
  check.

Both refusals are `unsupported_source` and point at the redis adapter.
Absence stays silent: an artifact with neither field is refused by
nothing, and the load speaks for itself. The mirror-image fence lives in
the redis adapter.

The sandbox side is fenced the same way: a sandbox whose
`valkey-server --version` reports "Redis server" is refused as
`invalid_request` (the 7.2-line Valkey names no engine in that line — the
refusal fires only on positive evidence of the wrong one).

## Engine-version pre-check

The RDB header also names the server that saved it (`valkey-ver`), and
the version rule is asymmetric: an older RDB loads on a newer server —
the supported path — but a newer server's RDB does not load on an older
one ("Can't handle RDB format version …", measured with a 9.x file on
8.1). Before anything is transferred, the adapter compares the header
against the sandbox's `valkey-server --version` and refuses that one
direction up front as `invalid_request`, naming both sides
(docs/engine-versions.md §5). The check refuses only on positive
evidence: an RDB without a readable `valkey-ver` — or one from an
unstable build, which writes 255.255.255 — skips it, and the load speaks
for itself.

## Backup identity

`source_identity.checksum` is sha256 over the artifact's bytes, exactly as
stored. For the directory kind, the checksum covers the one file chosen
for restore.

## Source params

| Param             | Meaning |
|-------------------|---------|
| `backup_timezone` | Refused — an RDB dates itself in epoch seconds, and an append-only directory is deliberately not dated; the declaration has nothing to add either way. |

## Environment

No credentials are needed to read an RDB file; `source.credential_env` is
unused. The adapter never prints secrets, and there are none to print.

## Deliberately not here

- **A single-file AOF.** The verified engines all write the directory
  form Valkey kept from Redis 7; a bare `appendonly.aof` file is refused
  with a message saying so rather than restored unverified.
- **A `valkey_aof_dir` kind.** See above: an append-only set does not
  date itself, so "newest" among several would rest on file times a copy
  resets — a guess, not a measurement.
- **Compressed artifacts.** A gzip-compressed RDB is refused by name
  (`unsupported_source`) rather than handed to the server to fail
  cryptically — decompress first, or back up uncompressed.
- **Redis.** The other side of the dialect fence above: a Redis artifact
  is refused by name and belongs to the redis adapter, a distinct engine
  with its own matrix (ROADMAP.md). Nothing here is verified against
  Redis.
- **PITR.** Valkey has no point-in-time recovery to drive; every source
  kind probes `pitr: false`.
