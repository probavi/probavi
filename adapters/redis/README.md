# probavi-adapter-redis

Restores Redis RDB snapshots and Redis 7+ append-only directories into a
disposable sandbox and serves the restored keys, implementing
`probavi-adapter/0` (docs/adapter-protocol.md). Self-contained: standard
library only, no imports from the Probavi core.

## Supported source kinds

| Kind            | Path points at |
|-----------------|----------------|
| `redis_rdb`     | One RDB file — a copied `dump.rdb`, or the output of `redis-cli --rdb` |
| `redis_rdb_dir` | A directory of RDB files; the newest **by the artifact's own save instant** is restored |
| `redis_aof`     | A copy of the Redis 7+ append-only directory (`appendonlydir`): manifest, base, incremental segments — replayed in full |

RDB is what Redis itself recommends for backups. What this adapter does
not restore, on purpose, is in "Deliberately not here" below.

## Sandbox image: start it idle

The official `redis` images carry everything the flow needs — `redis-server`,
`redis-cli`, `redis-check-rdb` — but their default command boots an empty
server on the port the restored one needs. Start the sandbox idle and let
the adapter own the lifecycle:

```yaml
sandbox:
  provider: docker
  params:
    image: redis:7.2.15
    command: sleep infinity
    memory: 256m
source:
  kind: redis_rdb
  path: /backups/redis/dump.rdb
```

The adapter places the RDB in its own data directory, has `redis-check-rdb`
vet it, then starts the server daemonized with persistence pinned off
(`--appendonly no`, `--save ""`): the server loads the placed RDB and never
rewrites the artifact under the drill. No shell is required for the engine
flow — every step is direct argv — though the check runner below does use
`sh` for word splitting, which every official image carries.

## The append-only kind: the manifest is the contract

Since Redis 7.0 the append-only state is a directory holding a text
manifest plus the files it names — one base (an RDB by default) and
incremental segments. `redis_aof` restores a copy of that directory, and
the manifest gives it the gate AOF backups need most: **a copy taken
mid-rewrite loses members**, and a manifest naming a file the backup
does not hold is refused by name as an incomplete copy, before a byte
reaches the sandbox. In the sandbox, `redis-check-aof` (handed the
manifest, so it walks the base and every segment — measured) stays the
authority on loadability, and the server is started with `--appendonly
yes`, `--save ""`, and `--appendfilename` derived from the manifest's
own name — an unmatched name would make the server silently start a
fresh, empty append-only set, exactly the false green a drill must not
produce. The base RDB's header feeds the same version pre-check and
Valkey fence as the rdb kinds.

Two deliberate differences from the rdb kinds. `backup.created_at` is
**null**: the base's `ctime` dates the last rewrite, not the backup, and
the incremental tail extends past it — an append-only directory does not
date itself, and a wrong instant is worse than none. And there is no
`redis_aof_dir`: with nothing to rank candidates by except copy-fragile
file times, "newest" would be a guess; point the drill at one specific
append-only directory instead.

There is no auth to reset, unlike every sibling engine: `requirepass` and
ACLs live in server configuration, not in the RDB, so the restored data
carries no credentials. The server serves without auth inside a sandbox
that has no network exposure whatsoever (`--network none`, no ports
expressible).

## Checks: lines of redis-cli arguments

Redis has no SQL, so the generating built-in checks (`row_count`,
`table_exists`, `freshness`) do not apply — the same consequence the
MongoDB and etcd adapters document. A check's text is one line of
`redis-cli` arguments, run through the probe-declared template:

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
arguments. `redis-cli -e` makes a command-level error exit non-zero, which
is how a failing check fails.

## When the backup was taken

An RDB records its save instant in the header (`ctime`, epoch seconds —
measured). Epoch seconds carry no zone question, so `backup.created_at` is
exact with **no** `backup_timezone` declaration — like the postgres
adapter's pgBackRest kind, and unlike every wall-clock format. Declaring
`backup_timezone` anyway is refused rather than ignored.

The same rule ranks the directory kind: the newest artifact **by its own
ctime** wins, a dated artifact outranks every undated one, and only
artifacts with no readable ctime fall back to file time. Files without the
RDB magic (checksum sidecars, READMEs) are not candidates; if the chosen
artifact turns out broken, the drill fails rather than quietly restoring
an older neighbour.

## Engine-version pre-check

The RDB header also names the server that saved it (`redis-ver`), and the
version rule is asymmetric: an older RDB loads on a newer server — the
supported path — but a newer server's RDB does not load on an older one
("Can't handle RDB format version …"). Before anything is transferred,
the adapter compares the header against the sandbox's
`redis-server --version` and refuses that one direction up front as
`invalid_request`, naming both sides (docs/engine-versions.md §5). The
check refuses only on positive evidence: an RDB without a readable
`redis-ver` — or one from an unstable build, which writes 255.255.255 —
skips it, and the load speaks for itself.

## The dialect fence: Valkey artifacts are refused by name

Since the fork the two RDB dialects diverge above format version 11, and
a drill that restores a Valkey backup into Redis — even a pre-fork one
that would load — proves recovery into an engine the backup does not
belong to, the false green ROADMAP.md names. So the adapter refuses,
before a byte is transferred, on positive evidence of a Valkey save
(measured against the official images): a `valkey-ver` aux field, which
Valkey has always written and Redis never has, or the `VALKEY` magic its
9.x writes. Both refusals are `unsupported_source` and point at the
valkey adapter; absence stays silent — positive evidence only. The
sandbox side is fenced the same way: the Valkey images ship `redis-*`
compatibility symlinks, so a sandbox whose `redis-server --version`
reports Valkey is refused as `invalid_request`. The mirror-image fence
lives in the valkey adapter.

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

- **Pre-7.0 single-file AOF.** The verified engines all write the 7.x
  directory form; a bare `appendonly.aof` file is refused with a message
  saying so rather than restored unverified. If a real need appears, it
  is a separate, measured piece of work.
- **A `redis_aof_dir` kind.** See above: an append-only set does not
  date itself, so "newest" among several would rest on file times a copy
  resets — a guess, not a measurement.
- **Compressed artifacts.** A gzip-compressed RDB is refused by name
  (`unsupported_source`) rather than handed to the server to fail
  cryptically — decompress first, or back up uncompressed.
- **Valkey.** The other side of the dialect fence above: a Valkey
  artifact is refused by name and belongs to the valkey adapter, a
  distinct engine with its own matrix (ROADMAP.md). Nothing here is
  verified against Valkey.
- **PITR.** Redis has no point-in-time recovery to drive; every source
  kind probes `pitr: false`.
