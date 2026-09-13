# Chroma adapter

Restores a [Chroma](https://www.trychroma.com/) persistence directory into a
disposable sandbox and proves the restored database can still answer the one
question a vector store exists for.

Implements `probavi-adapter/0` ([docs/adapter-protocol.md](../../docs/adapter-protocol.md)).
Standard library only, no imports from the Probavi core.

## The headline: a record count proves nothing here

Chroma keeps two things in a persistence directory — `chroma.sqlite3`, which
holds the collections, the records and the segment declarations, and one
directory per vector segment holding the HNSW index. The second is derived
from the first **only while the write queue still covers it**. Chroma purges
that queue once the segments have caught up (`embeddings_queue_config` carries
`{"automatically_purge": true}`, and a 2000-record write collapsed a 200-entry
queue to 1 — measured 2026-09-13 on 1.5.9).

After the purge, a backup that lost a segment directory behaves like this:

| | whole backup | segment directory missing |
|---|---|---|
| server starts | yes | **yes** |
| `count` | 2200 | **2200** |
| documents read back | all | **all** |
| engine log | quiet | **quiet** |
| nearest-neighbour query, 5 asked for | 5 returned | **0 returned** |

So the drill's verdict is **not** a count. After the restore this adapter
takes one embedding out of each collection, queries the collection with that
same vector, and requires as many neighbours back as it asked for. A backup
whose index did not survive fails as `source_corrupt` with the collection
named, rather than passing as a green restore of a database that can no
longer search.

## Source kinds

| Kind | What it is |
|---|---|
| `chroma_data` | The persistence directory `chroma run --path` was given, copied with the server stopped. |
| `chroma_data_tar` | A tar archive of one, plain or gzip. |

Chroma has no backup command, so a copy taken with the server stopped is the
whole artifact an operator can hold. There is deliberately **no
newest-in-a-directory kind**: nothing inside a persistence directory dates the
backup, and ranking candidates by file time would date the copy rather than
the data. For the same reason `backup.created_at` is always null in the
evidence record.

For an archive the compression is read from the artifact's first bytes, never
from its name. The archive may hold the directory's contents at its root
(`tar -czf backup.tar.gz -C /data .`) or the directory itself
(`tar -czf backup.tar.gz /data`); both are accepted, and an archive holding
two candidate directories is refused rather than guessed between.

**Backup identity.** A directory is hashed as one artifact: every regular
file's path and bytes, in sorted path order, so the same tree always hashes
the same way and a moved file changes the sum. An archive is hashed as the
bytes on disk, as stored.

## The sandbox image

Chroma's official image cannot idle as a drill sandbox. Its entrypoint is
`dumb-init -- chroma`, and `chroma` is a CLI that reads its arguments as
subcommands, so the sandbox's `command: sleep infinity` arrives as
`chroma sleep infinity` and the container exits with *"unrecognized subcommand
'sleep'"* (measured). A drill therefore runs a two-line wrapper:

```dockerfile
FROM chromadb/chroma:1.5.9
ENTRYPOINT []
```

```sh
docker build -t probavi-chroma-sandbox:1.5.9 .
```

The wrapper **installs nothing**. The official image already carries `bash`,
and this adapter's entire HTTP client is bash's own `/dev/tcp` — the image has
no `curl`, no `python3` and no `jq`, and needs none. Engine versions in
[docs/capabilities.json](../../docs/capabilities.json) name the official
image, because that is the server the wrapper runs.

```yaml
sandbox:
  provider: docker
  params:
    image: probavi-chroma-sandbox:1.5.9
    command: sleep infinity
    memory: 2GiB
```

## Checks

Chroma has no SQL, so the generating built-ins (`table_exists`, `row_count`,
`freshness`) do not apply — the MongoDB precedent. `service_healthy` works,
and everything else is written as a request:

```yaml
checks:
  - builtin: service_healthy
  - name: the collection still holds every record
    sql: "drills/count"
    expect: "2200"
  - name: search still returns neighbours
    sql: 'drills/query {"query_embeddings":[[1.0,2.0,3.0]],"n_results":5}'
```

The check text is a path, optionally followed by a space and a JSON body; a
body makes it a POST. A path that does not begin with `/` is taken as relative
to the restored database's collections, and its first segment may be the
**collection's name** — the runner resolves it to the id Chroma's read
endpoints require, so a drill config never carries a UUID that changes every
time the collection is recreated. A path beginning with `/` is used as it
stands, for anything the v2 API offers.

The engine's status line is the verdict: a non-2xx answer fails the check and
its body is reported.

## What this adapter does not claim

- **Client/server deployments.** The artifact here is a local persistence
  directory. A distributed Chroma keeps its state elsewhere.
- **A version fence.** Nothing states the engine version: the image tag says
  1.5.9, `chroma --version` answers `1.4.4` (that is the CLI), and
  `/api/v2/version` answers `"1.0.0"` (that is the API). A 1.5.9 artifact
  restored cleanly under 1.4.1 in both directions when measured, so no
  refusal is asserted where no evidence could support one.
- **A live copy.** A copy taken while the server was writing is refused when
  it carries `chroma.sqlite3-journal`, which is the sidecar Chroma's journal
  mode leaves (`PRAGMA journal_mode` reports `delete`; the header's bytes 18
  and 19 are both 1 — it is not WAL). That fence catches a copy caught
  mid-transaction; it cannot catch one taken between transactions, which is
  why the remedy in every message is to stop the server first.

## Environment variables

None. The adapter reads the artifact from the drill host and talks to the
engine only inside the sandbox, over loopback.
