# probavi-adapter-weaviate

Restores Weaviate filesystem-backend backups into a disposable sandbox so
a drill can prove they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

Weaviate is a server, but a drill never dials it from outside: the
adapter starts it inside the sandbox and drives it over loopback, through
the core-mediated `exec` verb. Nothing is published, and a drill runs
under `--network none` (measured — with `CLUSTER_ADVERTISE_ADDR` pinned
to loopback, without which the engine's memberlist layer looks for a
private IP, finds none, and refuses to start).

The restore is the engine's own: the backup tree is placed under the
filesystem backend's root, the engine starts with the `backup-filesystem`
module enabled, and one `POST /v1/backups/filesystem/<id>/restore` call —
made with busybox `wget`, because that is the HTTP client the Alpine
image actually carries (`sh`, `wget`, `nc`; no `bash`, no `curl`, no
`python3`; measured on 1.39.2) — drives it, with the status endpoint
polled until the engine's own verdict arrives.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `weaviate_backup_tar` | One tar archive (plain or gzip) of a filesystem-backend backup directory, its tree at the root or under one wrapping directory |
| `weaviate_backup` | One backup directory, as `POST /v1/backups/filesystem` wrote it (`backup_config.json` at the root) |
| `weaviate_backup_dir` | A directory of them; the one whose **own metadata** claims the newest completion is restored — never file times, which do not survive a copy |

PITR is not supported and a drill that requests it is refused up front:
a Weaviate backup is one instant, and nothing in the artifact can move it.

`backup.created_at` is the completion instant the backup states about
itself (`completedAt` in `backup_config.json`) — RFC 3339 UTC with the
zone attached (measured), so no `backup_timezone` declaration is ever
needed, and passing one is refused rather than ignored.

For the directory kinds the backup's identity checksum is a canonical
tree hash: entries sorted by relative path, each regular file
contributing its path, size and content bytes, symlinks their path and
target. The same tree always hashes the same; any content change changes
the hash.

A backup taken on more than one node is refused by name: the filesystem
backend is single-node by Weaviate's own documentation, and a drill
sandbox is a single node by construction.

## The sandbox must start idle

The official image cannot do that on its own: its entrypoint is pinned to
`/bin/weaviate`, **and the binary ignores unknown positional arguments**
(measured), so `command: sleep infinity` under the stock image starts a
serving engine on defaults — no backup module, telemetry on. The drill
sandbox therefore runs a two-line wrapper you build yourself:

```dockerfile
FROM semitechnologies/weaviate:1.39.2
ENTRYPOINT []
```

Everything in the image — the engine, the busybox userland — is exactly
the official image's; only the entrypoint pin is lifted. A drill pointed
at a sandbox where something already serves on 8080 fails up front with a
message naming this section. The integration suite builds the same
wrapper from the manifest's listed image, so "verified against Weaviate
1.39" means that server, exercised this way.

```yaml
sandbox:
  provider: docker
  params:
    image: your-registry/probavi-weaviate:1.39.2   # the wrapper above
    command: sleep infinity                         # the adapter owns the engine lifetime
    memory: 512m
source:
  kind: weaviate_backup
  path: /backups/weaviate/nightly
```

The adapter starts the engine with `DISABLE_TELEMETRY=true`, always:
without it the engine POSTs usage data to `telemetry.weaviate.io` at
startup (measured). The drill's `--network none` would stop the packet
anyway, but an environment that states it is worth more than a network
that happens to prevent it.

`CLUSTER_HOSTNAME` is pinned to the node the backup was taken on, read
from the backup's own metadata: the engine refuses to restore another
node's backup (measured, HTTP 500). No credential exists anywhere in a
drill — the engine runs with anonymous access enabled, bound to loopback
inside an unpublished sandbox.

The engine also opens a gRPC port (50051), gossip ports (7946/7947) and a
pprof endpoint (6060) on all interfaces; under the default `--network
none` there is no interface for them to be reached on, and the HTTP API
the drill uses is bound to loopback explicitly.

## What a check looks like

Weaviate speaks HTTP and GraphQL rather than SQL, so a check is written
in what the engine answers to — the engine's own client arguments, as the
project glossary allows where there is no SQL. The runner accepts three
forms:

| Check text | What the runner does |
| --- | --- |
| `{Aggregate{Books{meta{count}}}}` | Text beginning with `{` is a **GraphQL query**, POSTed to `/v1/graphql` |
| `/v1/graphql {"query":"…"}` | An absolute path, optionally followed by a space and a JSON body; a body makes it a POST |
| `schema` | A path without a leading `/` hangs off `/v1/` |

The HTTP status is the verdict — `wget` exits non-zero on any non-2xx
answer and the check fails with the engine's status line — and because
Weaviate answers GraphQL errors as **HTTP 200 with an `"errors"` array**
(measured), a body carrying one fails the check too. The body is reduced
to the one number Weaviate states where it states one (`"count"` from
`Aggregate`), passed through otherwise. A body whose data legitimately
contains an `"errors":` key would trip that fence; ask such a question
through a filtered `Aggregate` count instead.

The core's generating built-in checks (`row_count`, `max_value`) compose
SQL and therefore do not apply; write checks as GraphQL, the way the
examples above do.

## What a drill can prove here, and what it cannot

The engine fences its artifact end to end, and the drill leans on that
(all measured): a chunk truncated mid-file fails the restore with
`unexpected EOF`, a flipped byte with `flate: corrupt input`, a missing
chunk with the engine's own words — and a failed restore leaves the class
**absent**, never short. On top of that the backup's own node manifest
names every chunk, so a file lost in a copy is refused on the host before
a byte moves, and a `backup_config.json` reporting `FAILED` or an
in-progress status is refused as what it is: not yet an artifact.

What remains is the well-formed zero: a backup of an empty class restores
green with count 0 (measured), which would be a drill that proved nothing
while reporting success. So the restore's verdict is the restored class's
**object count**, and zero is refused. A deliberately empty class cannot
green-light a drill here; that is the point.

## Data lifecycle: nothing to suspend, and a test that says so

Issue #166's question — what does the engine run unbidden that can
subtract from what the backup holds — lands in the **guard** shape here,
the qdrant and OpenSearch precedent. Weaviate has no TTL and no expiry;
what it runs on its own is the vector index's tombstone cleanup (interval
declared per class in `vectorIndexConfig.cleanupIntervalSeconds`) and LSM
compaction, and both reclaim only objects that were already deleted —
which a count never counted. Measured: a backup carrying soft-deleted
objects restores to its exact live count and holds it across several
cleanup windows, with the cleanup demonstrably running. The property is
proven by `TestTheRestoredObjectCountDoesNotShrink` rather than assumed,
and the class's own declared cleanup interval is left exactly as the
operator wrote it — suspend, never rewrite, and here not even suspend.

## Drill config options

| Option | Meaning |
| --- | --- |
| `class` | The class whose object count is the drill's verdict. Optional when the backup holds exactly one class; required (and checked against the backup's own list) when it holds several. The restore itself always restores every class in the backup. |

## Behavior notes

- **Cross-version restores:** all nine combinations of {1.37.15, 1.38.13,
  1.39.2} backup × engine restored the fixture completely, downgrades
  included (measured 2026-09-03). The manifest still records only what CI
  restores from: a class using features one of those versions lacks is a
  different question from the one measured.
- **A directory of backups refuses a newer failed or in-progress
  attempt by name** rather than silently proving an older backup: the
  record must not imply the newest when the newest is not yet, or never
  became, an artifact.
- **The engine starts fast** — one to seven seconds to ready on the
  measured fixtures — and the restore itself took ~2.5 s for the small
  fixture; the timings in the evidence record are per-phase measurements
  (`engine_ready_seconds`, `transfer_seconds`, `restore_seconds`).
- The suite restores the fixture at `--memory 128m` (measured); 512m is
  the comfortable setting the examples use.
- Weaviate's vendor supports the latest three minor lines as a rolling
  policy and publishes no per-version end-of-life dates, so the
  manifest's `supported_until` is null and `versions_checked` records the
  reading date.

## Deliberately not here

- **Copies of the persistence directory** (`PERSISTENCE_DATA_PATH`). The
  backup API exists exactly so operators never copy a live LSM tree; a
  data-directory copy carries no `backup_config.json` and is refused with
  a message naming `POST /v1/backups/filesystem`.
- **Cloud backup backends** (`backup-s3`, `backup-gcs`, `backup-azure`).
  The sandbox verbs move local files; staging a bucket's backup to disk
  stays the operator's documented job (the object-storage question in
  ROADMAP.md).
- **Multi-node backups** — refused by name, see above.
- **RBAC and user restores** (`rbacBackups`, `userBackups` in the backup)
  ride along untouched through the engine's own restore; the drill's
  verdict does not read them.
