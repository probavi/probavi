# probavi-adapter-opensearch

Restores OpenSearch fs snapshot repositories into a disposable sandbox
so a drill can prove they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

Search clusters hold data that is expensive to rebuild and often
impossible to re-ingest, and OpenSearch is an engine whose forgiveness
makes a dishonest drill easy: it registers a directory that is no
repository at all without a word and simply lists zero snapshots, and a
restore from damaged repository data returns HTTP 200 while shards fail
(both measured). This adapter's job is to make neither callable green.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `opensearch_repo_tar` | one tar archive (plain or gzip) of an fs snapshot repository — the repository at the root, or under one wrapping directory |
| `opensearch_repo` | one fs snapshot repository directory (the `location` of a registered `fs` repository) |

A repository holds every snapshot ever taken into it, so "which backup"
is decided inside the artifact: the drill restores the snapshot whose
own metadata claims the newest instant (`end_time_in_millis`), refuses
it by name if its state is not `SUCCESS`, and records that claim as
`created_at`. PITR does not apply; the probe declares `pitr: false` so
the core refuses a `target.pitr` drill before anything runs.

## The raw-copy fence: a live data directory is refused by name

The wrong artifact here is a copy (or tar) of the node's *data
directory*. It carries `nodes` or `_state` — entries an fs repository
never contains — and it is not restorable through the snapshot API at
all. Both kinds refuse those markers by name, with the fix in the
message: register an `fs` repository, snapshot into it, and back up
*that* directory.

The right artifact states its own structure: the repository root
carries `index.latest` (the current generation) and `index-<N>` (every
snapshot with the OpenSearch version that wrote it — all measured on
2.19 and 3.8). A directory without them is refused as
`source_corrupt` before a byte is transferred, because the engine
itself would have accepted it silently and listed nothing.

## The verdict reads: where the engine actually tells the truth

Measured behaviors this adapter refuses to paper over:

- a damaged or foreign directory **registers silently** and lists zero
  snapshots — so after registration the engine's snapshot list is
  compared against the repository's own files, and an engine that lists
  none where the artifact claims some is `source_corrupt`;
- a restore from corrupted repository data **returns HTTP 200** with
  `shards.failed > 0` and the cluster red — so the verdict is read from
  the shard counts, never from the HTTP status;
- after the restore the cluster must be **green**: replicas are forced
  to zero on the single node, so anything below green means restored
  data is not fully served. The gate waits for green through the
  engine's own `wait_for_status` rather than reading one instant — the
  restore call returns before the shards' started events land, and a
  primary still initializing from a snapshot reads *yellow*; a wait that
  expires answers HTTP 408 with the current status in the body
  (measured), which is the verdict.

The API calls run `curl` without `-f` deliberately: an HTTP error's
body is where the engine states its reason in its own words, and `-f`
would discard it.

## Version pairing: snapshots do not restore on older engines

A snapshot restores on an engine at least as new as the one that wrote
it; the reverse is refused by OpenSearch ("the snapshot was created
with OpenSearch version [X] which is higher than the version of this
node [Y]", measured). The repository's own metadata names the writing
version per snapshot, so the drill refuses that pairing before the
transfer, naming both sides; if the metadata carried no readable
version, the engine's own refusal at restore is surfaced as the same
`invalid_request`. Elasticsearch snapshot repositories are a different
engine's artifact — the pairing refusal is where that surfaces.

## The sandbox: no sysctl, no privilege, engine before transfer

The restored node runs as a loopback single node in the mode OpenSearch
itself defines for it: `discovery.type=single-node` (bootstrap checks
not enforced), the security plugin disabled, and
`node.store.allow_mmap=false` — which removes the `vm.max_map_count`
requirement entirely (measured; that sysctl is not namespaced, so a
zero-ingress sandbox could never grant it). No sysctl or privileged
flag ever enters the sandbox parameters.

`path.repo` is a static setting, so the adapter starts the engine
*first* — pointed at an empty repository directory — and transfers the
artifact into it afterwards; `transfer_seconds` and `restore_seconds`
stay honestly separated. The official images idle under
`command: sleep infinity` with no wrapper (their entrypoint passes
non-opensearch commands through, measured), and ship everything the
adapter needs: the server, `curl`, `python3`, `bash`.

```yaml
sandbox:
  provider: docker
  params:
    image: opensearchproject/opensearch:2.19.6
    command: sleep infinity          # the adapter owns the engine lifecycle
    memory: 2g                       # the image's JVM defaults to a 1g heap
source:
  kind: opensearch_repo_tar
  path: /backups/opensearch/repo-2026-08-16.tar.gz
```

The restored node listens on `127.0.0.1:9200` with the security plugin
disabled — no TLS, no auth — acceptable for exactly one reason: a
Probavi sandbox is zero-ingress (`--network none`, no ports
expressible). System indices (`.` -prefixed, the security plugin's
among them) are excluded from the restore; they belong to the running
node and collide with it by name (measured).

## Lifecycle automation stays out of the drill

Index State Management is OpenSearch's retention machinery: policies that
roll over, shrink or **delete** indices as they age. A drill must not run
one — the artifact is what it is proving, and a policy inherited from the
backup can only subtract from it.

It does not, and the reason is worth stating because it is not obvious:
ISM keeps both the policies and the jobs that run them in
`.opendistro-ism-config`, and the restore excludes every dot-index. So the
automation stays in the artifact.

Measured on both verified images, with a snapshot deliberately built to
carry it — a policy whose only action is `delete`, attached to one index
through the ISM API and to another through the per-index setting, snapshot
taken with `include_global_state: true`:

| | in the snapshot | in the drill |
| --- | --- | --- |
| `.opendistro-ism-config` | present | not restored |
| ISM policies | 1 | **0** |
| managed indices | 1 | **0** |
| documents in each index | 3 | 3 |

One detail travels and is inert: an index whose *settings* name a policy
keeps `index.plugins.index_state_management.policy_id` after the restore.
It names a policy the sandbox does not have, so nothing runs — the
restored index is not managed, on either version.

The dot-index exclusion is in this adapter for a different reason
(collision with the running node's own system indices), so this property
was nobody's stated intent. It is now: an integration test builds that
snapshot and fails if any policy or managed index appears in the drill.
Widen the restore pattern and it goes red — which is the only warning
anyone would get before a drill began running someone's retention policy
against the backup it is meant to prove.

## Checks: OpenSearch SQL through the bundled SQL plugin

Checks are one OpenSearch SQL statement each, sent to the node's
bundled SQL plugin (`_plugins/_sql`); the plugin's raw answer is
filtered to the undecorated tab-separated rows the protocol requires.
The check text is JSON-encoded into the request body, never
interpolated into shell:

```yaml
checks:
  - name: orders_survived
    sql: SELECT COUNT(*) FROM orders
  - name: newest_order_is_recent
    sql: SELECT MAX(created_at) FROM orders
```

Built-in checks that generate SQL (`row_count`, `table_exists`,
`freshness`) do not apply to this adapter: they quote identifiers the
SQL-standard way (`"name"`), which the plugin does not accept
(measured) — the same trade the mongodb, etcd and prometheus adapters
document, and the protocol's design working as intended (§6.1). Name
indices in the SQL itself; there is no database concept to select.

## When the backup was taken

`created_at` is the restored snapshot's own `end_time_in_millis` —
epoch milliseconds, which carry no timezone question at all. A file's
mtime dates a copy, not a backup, so it is never used. The
`source.params.backup_timezone` key the other adapters use has nothing
to act on here and is refused rather than silently ignored.

## Backup identity

For the archive kind, `checksum` is SHA-256 over the tar's bytes,
exactly as stored. For the directory kind it is a canonical hash of the
repository tree: entries sorted by relative path; each regular file
contributes its path, size, and content bytes; symlinks contribute path
and target. The same tree always hashes the same, and any content
change changes the hash.

## Deliberately not here

- **Raw data-directory copies** (and tars of them) — refused by name;
  see the fence above.
- **Remote repositories** (S3, Azure, GCS, HDFS) — a zero-ingress
  sandbox cannot reach them by design. Sync the repository to a
  directory (or tar it) and point the drill at that.
- **System indices and cluster state** — the restore excludes
  `.`-prefixed indices; the drill proves the data restores, not the
  cluster's identity.
- **Elasticsearch snapshots** — a different engine's artifact; the
  version-pairing refusal names the mismatch instead of guessing.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| a live data directory (or a tar of one) | `unsupported_source`, teaching the fs repository |
| a directory without `index.latest`, or a repository listing no snapshots | `source_corrupt` |
| the repository names a generation the copy does not carry | `source_corrupt`, naming it |
| tar cannot unpack the archive | `source_corrupt` |
| the sandbox image lacks the toolchain | `invalid_request`, naming it |
| a snapshot written by a newer OpenSearch than the sandbox runs | `invalid_request`, naming both sides |
| the newest snapshot's state is not `SUCCESS` | `source_corrupt`, refusing it by name |
| the engine lists no snapshots where the repository's files claim some | `source_corrupt` — the census |
| failed shards or a cluster below green after the restore | `source_corrupt` — the verdict reads |
| the restored node never became ready | `engine_not_ready`, or `restore_failed` with the node's own log line |
