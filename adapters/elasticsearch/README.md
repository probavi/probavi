# probavi-adapter-elasticsearch

Restores Elasticsearch fs snapshot repositories into a disposable
sandbox so a drill can prove they still work. Implements
`probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

Search clusters hold data that is expensive to rebuild and often
impossible to re-ingest, and Elasticsearch is an engine whose
forgiveness makes a dishonest drill easy: it registers a directory that
is no repository at all without a word and simply lists zero snapshots,
a restore from damaged repository data returns HTTP 200 while every
shard fails, and a restored index arrives with its lifecycle policy
armed — all measured. This adapter's job is to make none of them callable
green.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `elasticsearch_repo_zip` | one zip archive of an fs snapshot repository — the repository at the root, or under one wrapping directory (the layout `zip -r repo.zip repo` produces) |
| `elasticsearch_repo` | one fs snapshot repository directory (the `location` of a registered `fs` repository) |

A repository holds every snapshot ever taken into it, so "which backup"
is decided inside the artifact: the drill restores the snapshot whose
own metadata claims the newest instant (`end_time_in_millis`), refuses
it by name if its state is not `SUCCESS`, and records that claim as
`created_at`. PITR does not apply; the probe declares `pitr: false` so
the core refuses a `target.pitr` drill before anything runs.

The archive form is **zip, not tar**, because of what the verified
images carry: the official 9.x image is UBI-based and ships no `tar`,
`gzip`, `python3` or `jq` at all — 224 binaries, `unzip` among them
(measured; the 8.x image has both). An archive kind that unpacks on one
verified line and not the other would be a claim this repository cannot
keep. `zip -r` is every bit as scriptable as `tar`, and the census below
reads the archive's central directory without unpacking it.

## The raw-copy fence: a live data directory is refused by name

The wrong artifact here is a copy (or zip) of the node's *data
directory*. It carries `nodes` or `_state` — entries an fs repository
never contains — and it is not restorable through the snapshot API at
all. Both kinds refuse those markers by name, with the fix in the
message: register an `fs` repository, snapshot into it, and back up
*that* directory.

The right artifact states its own structure: the repository root
carries `index.latest` (the current generation) and `index-<N>` (every
snapshot, with the index version of the engine that wrote it — measured
on 8.19 and 9.5). A directory without them is refused as
`source_corrupt` before a byte is transferred, because the engine
itself would have accepted it silently and listed nothing.

## The verdict reads: where the engine actually tells the truth

Measured behaviors this adapter refuses to paper over:

- a damaged or foreign directory **registers silently** and lists zero
  snapshots — so after registration the engine's snapshot list is
  compared against the repository's own files, and an engine that lists
  none where the artifact claims some is `source_corrupt`;
- a restore from corrupted repository data **returns HTTP 200** with
  `shards.failed` equal to `shards.total` and the cluster red — so the
  verdict is read from the shard counts, never from the HTTP status;
- after the restore the cluster must be **green**: replicas are forced
  to zero on the single node, so anything below green means restored
  data is not fully served. The gate waits for green through the
  engine's own `wait_for_status` rather than reading one instant — the
  restore call returns before the shards' started events land, and a
  primary still initializing from a snapshot reads *yellow* (measured on
  a slower host); a wait that expires answers HTTP 408 with the current
  status in the body, which is the verdict.

The API calls run `curl` without `-f` deliberately: an HTTP error's
body is where the engine states its reason in its own words, and `-f`
would discard it.

## Lifecycle policies are suspended, not rewritten

This is the engine's instance of the rule every adapter follows: a
drill sandbox proves the artifact, so it must not apply the engine's
own data-lifecycle policy to it. Elasticsearch has two such machineries,
and a restored snapshot re-arms both:

- **Index Lifecycle Management.** A restored index keeps its
  `index.lifecycle.name` setting — and it should; a check reading the
  settings is entitled to see what the operator declared. A fresh node
  is not empty of policies to match it: it ships **47 built-in ones**
  (measured on both verified images), `7-days-default` through
  `365-days-default` and the stack's `logs@lifecycle` among them. An
  index naming one of those is managed the moment it lands.
- **Data stream lifecycle.** A data stream's retention travels inside
  the data stream's own metadata, which the restore brings along with
  the backing indices. No policy lookup is involved at all.

Both measure age from what the artifact carries, so a backup older
than its own retention is past due the moment it is restored. Measured,
with polling accelerated so the answer arrived in seconds rather than
at the default five- and ten-minute intervals — a data stream under
`7-days-default` and one under a one-day retention, each with a
rolled-over generation past its age, restored with 4 of 4 shards
reported successful:

| | in the snapshot | without the pins | in the drill |
| --- | --- | --- | --- |
| `dlm-drill-app` documents | 5 | **2**, eight seconds after the restore | 5 |
| `ilm-drill-app` documents | 5 | **2**, twenty seconds later | 5 |
| backing indices | 4 | 2 | 4 |
| restore response | — | `"failed":0` | `"failed":0` |

"Data stream lifecycle successfully deleted index […] due to the lapsed
[1d] retention period", in the node's own log, while the restore stood
as a success. A check counting documents after that reads less than the
backup holds, and the drill reports green.

The pins, both applied after readiness and before the repository is
registered — nothing the artifact carries has landed yet, so nothing can
have run:

- ILM is stopped through its own switch, `POST _ilm/stop`, and verified
  by `GET _ilm/status` reading `STOPPED` (the stop is asynchronous and
  reads `STOPPING` first, measured; the adapter polls).
- Data stream lifecycle has no stop switch (measured: no such setting
  exists in either verified line), so it is held off by its poll
  interval — `data_streams.lifecycle.poll_interval`, pinned to a
  hundred years as a **launch setting**, before the node exists to poll
  anything — and verified by reading the effective value back through
  the cluster settings API. A node that reports anything else is
  refused as `invalid_request`.

Neither touches the artifact: the index setting and the data stream's
retention stay exactly as the backup recorded them (`1d` still reads
back on the drill's data stream), and only execution is suspended, for
the life of the sandbox. The cluster state stays in the artifact too —
`include_global_state` is left false, because restoring it would bring
the backup's own persistent settings, which could override the poll
interval pinned here.

An integration test builds that fixture and proves it from both sides:
a control node without the pins loses a generation within seconds, and
the drill — with ILM's poll interval then forced down to one second,
harsher than any default — keeps every generation and every document.
Remove either pin and the test goes red.

## Version pairing: snapshots do not restore on older engines

A snapshot restores on an engine at least as new as the one that wrote
it; the reverse is refused by Elasticsearch ("the snapshot was created
with version [8.19.21-[9111000]] which is higher than the version of
this node [8.19.9-8.19.20]", measured — the newer version is rendered
from a number the older node cannot name). Elasticsearch names the
writing engine by its **index version**, an integer (8537000 for
8.19.20, 9111000 for 9.5.2, measured), not by a release string: the
`version` field beside it in the repository metadata is the snapshot
format's own and reads "8.11.0" on both lines. The repository's own
generation file carries that integer per snapshot and the sandbox node
states its own through `_nodes`, so the drill refuses the pairing before
the transfer, naming both numbers and the engine's release; if the
metadata carried no readable version, the engine's own refusal at
restore is surfaced as the same `invalid_request`. The other direction
works: a repository written by 8.19 restores on 9.5 (measured).

OpenSearch snapshot repositories are a different engine's artifact —
their metadata names a release string where this engine expects an
integer, and the pairing refusal is where that surfaces.

## The sandbox: no sysctl, no privilege, engine before transfer

The restored node runs as a loopback single node in the mode
Elasticsearch itself defines for it: `discovery.type=single-node`
(bootstrap checks not enforced), security disabled, and
`node.store.allow_mmap=false` — which removes the `vm.max_map_count`
requirement entirely (that sysctl is not namespaced, so a zero-ingress
sandbox could never grant it). The GeoIP downloader is switched off; it
has no network to reach. No sysctl or privileged flag ever enters the
sandbox parameters.

`path.repo` is a static setting, so the adapter starts the engine
*first* — pointed at an empty repository directory — and transfers the
artifact into it afterwards; `transfer_seconds` and `restore_seconds`
stay honestly separated. The official images idle under
`command: sleep infinity` with no wrapper (their entrypoint passes
non-elasticsearch commands through, measured), and ship everything the
adapter needs: the server, `curl`, `unzip`, `bash`.

One requirement is the 8.x line's own. Under a zero-ingress sandbox the
container's hostname resolves to nothing, and an 8.19 node's logging
resolves it at startup and dies — "Could not determine local host
name", exit within four seconds, measured — where 9.x tolerates the
same. The image runs as uid 1000 and cannot edit the root-owned
`/etc/hosts`, so the adapter writes a hosts file of its own in the
scratch directory and points the JDK at it (`-Djdk.net.hosts.file`,
appended to whatever `ES_JAVA_OPTS` the operator set). Measured green
on both lines.

```yaml
sandbox:
  provider: docker
  params:
    image: elasticsearch:8.19.20
    command: sleep infinity          # the adapter owns the engine lifecycle
    memory: 2g                       # the image sizes its heap from the container limit
source:
  kind: elasticsearch_repo
  path: /backups/elasticsearch/repo
```

The restored node listens on `127.0.0.1:9200` with security disabled —
no TLS, no auth — acceptable for exactly one reason: a Probavi sandbox
is zero-ingress (`--network none`, no ports expressible). The restore
asks for `*`: every regular index and every data stream with its
hidden `.ds-` backing indices (measured — a restored data stream came
back whole and nothing collided), while system indices stay with the
feature states the restore does not request.

A host prerequisite worth stating: the engine writes
`/proc/self/coredump_filter` unconditionally at startup, so a kernel
built without `CONFIG_ELF_CORE` cannot run Elasticsearch at all. Stock
distribution kernels have it; a trimmed custom kernel may not.

## Checks: Elasticsearch SQL, and the built-ins apply

Checks are one Elasticsearch SQL statement each, sent to the node's SQL
API (`_sql?format=tsv`, in the free Basic tier); the answer's header
line is dropped and the tab-separated rows are printed as the protocol
requires (a tab inside a value arrives escaped as the two characters
`\t`, so a row is always one line — measured). The check text is
JSON-encoded into the request body by the shell itself, since the 9.x
image ships neither `python3` nor `jq`; it escapes the backslash, the
double quote and the three whitespace controls SQL text carries, and
refuses any other control character loudly rather than sending it
malformed. Nothing is interpolated: `$(…)`, `;`, a quote or a `\u`
escape in the text reach the engine as the characters they are
(measured).

Unlike the other non-relational engines in the catalog, the core's
generating built-in checks — `table_exists`, `row_count`, `freshness` —
**apply to this adapter**: the dialect accepts SQL-standard quoted
identifiers (`SELECT count(*) FROM "orders"`) and answers `max()` of a
date field as an RFC 3339 instant, which the core parses (measured, and
exercised through the core's own `checks` package in the integration
suite). Name the index — or the data stream, which the dialect reads the
same way — as the `table`; there is no database concept to select.

```yaml
checks:
  - builtin: table_exists
    table: orders
  - builtin: row_count
    table: orders
    min: 1
  - builtin: freshness
    table: orders
    column: created_at
    max_age: 48h
  - name: newest_sku
    sql: SELECT sku FROM orders WHERE qty = 4
    expect: item-4
```

## When the backup was taken

`created_at` is the restored snapshot's own `end_time_in_millis` —
epoch milliseconds, which carry no timezone question at all. A file's
mtime dates a copy, not a backup, so it is never used. The
`source.params.backup_timezone` key the other adapters use has nothing
to act on here and is refused rather than silently ignored.

## Backup identity

For the archive kind, `checksum` is SHA-256 over the zip's bytes,
exactly as stored. For the directory kind it is a canonical hash of the
repository tree: entries sorted by relative path; each regular file
contributes its path, size, and content bytes; symlinks contribute path
and target. The same tree always hashes the same, and any content
change changes the hash.

## Deliberately not here

- **Raw data-directory copies** (and zips of them) — refused by name;
  see the fence above.
- **Tar archives** — the 9.x official image carries no tar (measured).
  Point the drill at the repository directory, or zip it.
- **Remote repositories** (S3, Azure, GCS, HDFS) — a zero-ingress
  sandbox cannot reach them by design. Sync the repository to a
  directory (or zip it) and point the drill at that.
- **System indices and cluster state** — the restore asks for regular
  indices and data streams only and leaves the global state in the
  artifact; the drill proves the data restores, not the cluster's
  identity — and restoring the cluster state could undo the lifecycle
  pins above.
- **Searchable snapshots, CCR, and the rest of the paid tiers** — the
  drill proves a snapshot restores, which the Basic tier does.
- **OpenSearch snapshots** — a different engine's artifact, with its own
  adapter.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| a live data directory (or a zip of one) | `unsupported_source`, teaching the fs repository |
| a directory without `index.latest`, or a repository listing no snapshots | `source_corrupt` |
| the repository names a generation the copy does not carry | `source_corrupt`, naming it |
| unzip cannot unpack the archive | `source_corrupt` |
| the sandbox image lacks the toolchain | `invalid_request`, naming it |
| the sandbox engine would not suspend its lifecycle policies | `invalid_request`, naming what it still reports |
| a snapshot written by a newer Elasticsearch than the sandbox runs | `invalid_request`, naming both index versions |
| the newest snapshot's state is not `SUCCESS` | `source_corrupt`, refusing it by name |
| the engine lists no snapshots where the repository's files claim some | `source_corrupt` — the census |
| failed shards or a cluster below green after the restore | `source_corrupt` — the verdict reads |
| the restored node never became ready | `engine_not_ready`, or `restore_failed` with the node's own log line |
