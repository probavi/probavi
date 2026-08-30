# probavi-adapter-qdrant

Restores Qdrant snapshots into a disposable sandbox so a drill can prove
they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

Qdrant is a server, but a drill never dials it from outside: the adapter
starts it inside the sandbox and drives it over loopback, through the
core-mediated `exec` verb. Nothing is published, and a drill runs under
`--network none` (measured).

The engine restores its own snapshots. `qdrant --snapshot` and
`qdrant --storage-snapshot` read the artifact at startup, so the whole
restore is one process launch and no HTTP call at all — which matters
here, because the official image carries **no HTTP client**: no `curl`,
no `wget`, no `nc`, no `python3` (measured on 1.19.0). It does carry
`bash`, and that is what the healthcheck and the checks speak through, over
`/dev/tcp`.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `qdrant_snapshot` | one collection snapshot, from `POST /collections/<c>/snapshots` |
| `qdrant_snapshot_dir` | a directory of them; the newest by file time is restored |
| `qdrant_full_snapshot` | one whole-storage snapshot, from `POST /snapshots` |
| `qdrant_full_snapshot_dir` | a directory of them |

PITR does not exist for a Qdrant snapshot; the probe declares `pitr: false`
so the core refuses a `target.pitr` drill before anything runs.

Nothing inside a snapshot records when it was taken — the creation time
lives in the API response that made it and in the file name, neither of
which survives a copy — so `backup.created_at` is always null and
directories rank by modification time.

**Copy the `.checksum` sidecar too.** Qdrant writes a `<name>.checksum`
next to every snapshot, holding the SHA-256 of the file, and it matches to
the byte. When it is there, the adapter verifies the artifact against it
before a byte crosses into the sandbox and names the mismatch. When it is
not, the drill still runs — the engine's own refusal is the fence — but it
loses the one check that can tell "the backup is bad" from "the copy is
bad".

## The sandbox must start idle

```yaml
sandbox:
  provider: docker
  params:
    image: qdrant/qdrant:v1.19.0
    command: sleep infinity   # the adapter owns the engine's whole lifetime
    memory: 512m
```

`--snapshot` and `--storage-snapshot` are *startup* arguments. An image
that has already started Qdrant has read an empty storage tree, and there
is no way to hand it a snapshot afterwards without an HTTP client the image
does not have. So the sandbox starts idle, the adapter puts the artifact in
place and starts the engine itself. It refuses a sandbox where something is
already serving on 6333 rather than restoring into a running engine and
reporting a success it cannot support.

The engine is started as `/qdrant/qdrant`, not through the image's
`entrypoint.sh`. That wrapper exists to restart the engine in *recovery
mode* after an OOM — an engine deciding on its own to serve something other
than what the backup held, which is the one thing a drill must not allow.

`--disable-telemetry` is passed on every start and is not configurable.
Qdrant ships with `telemetry_disabled: false` and sends usage data to its
developers; Probavi does not phone home from any process it starts. The
drill runs under `--network none` so nothing could leave anyway — the flag
is what states it.

Qdrant needs no credential: the default configuration has no `api_key`, so
nothing about this engine puts a secret on the wire.

`512m` is comfortable rather than necessary — this suite's fixture restores
at `128m` (measured).

## What a check looks like

Qdrant speaks HTTP, not SQL, so a check is written in what the engine
answers to: a **path, optionally followed by a space and a JSON body**. A
body makes it a `POST`, which is how Qdrant asks its most useful question.
A path that does not start with `/` is relative to the restored collection.

| check text | what it asks |
| --- | --- |
| `/collections/orders` | how many points the collection holds |
| `points/count {"exact":true}` | the same number, exactly counted |
| `points/count {"exact":true,"filter":{"must":[{"key":"region","match":{"value":"eu"}}]}}` | how many match a filter |
| `points/12` | one point, as itself |

The runner takes the engine's HTTP status as the verdict, and reduces the
body to the one number Qdrant states where it states one (`count` or
`points_count`), passing it through otherwise.

## What a drill can prove here, and what it cannot

More than most, and the difference is the engine's.

**A damaged snapshot does not restore at all.** Qdrant validates what it
reads and exits 101 without ever listening — measured at truncations of
25%, 50%, 75%, 90% and 99%, and on a 4 KB bit flip in the middle of a
structurally valid archive. There is no equivalent of the h2 adapter's
silently recovered empty database or the couchdb adapter's shorter one: a
drill that passes here restored the whole snapshot.

**A snapshot that never held anything still restores.** An empty collection
has a perfectly valid snapshot, and it comes back green with
`points_count: 0`. So the restore's verdict is the restored point count and
a well-formed zero is refused.

**A snapshot cannot say whether the collection was complete when it was
taken.** Nothing about a backup of an empty database looks different from a
backup taken correctly of a database that was already empty. Write a
count check with the number you expect; that is what the drill's own checks
are for.

## Data lifecycle: nothing to suspend, and a test that says so

[Issue #166](https://github.com/probavi/probavi/issues/166) asks what the
engine runs unbidden that can only subtract from what the backup holds.
For Qdrant the answer is nothing, and the adapter proves it rather than
assuming it.

There is no TTL and no expiry anywhere in the configuration. The one thing
that runs on its own is the optimizer, and the part of it that removes
anything — vacuum — reclaims points that were *already soft-deleted*, which
a point count never counted. Measured: a snapshot holding 300 deleted
points of 1000 restores as 700 and is still 700 after the optimizer's own
window, with optimization enabled and with it turned off.

So this is the **guard** shape rather than the suspend one, and
`TestTheRestoredPointCountDoesNotShrink` is the guard: suspending a
mechanism that cannot subtract would be theatre, but leaving the property
unchecked would be a guess.

## Drill config options

| Option | Default | Meaning |
| --- | --- | --- |
| `collection` | `restored` | For `qdrant_snapshot*`, the collection the snapshot is restored into. For `qdrant_full_snapshot*`, the collection that must be present afterwards — name the one your checks query. |

A collection snapshot's file name does begin with the collection it came
from, and the adapter deliberately does not read it: a renamed file would
then restore under a name nobody chose, and the drill config is where that
belongs.

## Behavior notes

- **Cross-version restore is wider than expected.** All nine combinations
  of {1.17.0, 1.18.1, 1.19.0} snapshot against engine restore this suite's
  fixture completely, downgrades included. The manifest still records only
  what CI restores from: "verified against" is never widened into
  "supports", and a collection using features one of those versions lacks
  is a different question from the one measured.
- **A directory scan refuses an artifact still being written** rather than
  falling back to the older one beside it, which would prove a backup the
  evidence record does not name. A job that writes to a temporary name and
  renames on completion never trips it.
- **The `.checksum` sidecar is never itself a restore candidate**, and a
  sidecar that exists but states no digest is refused rather than treated
  as absent: a file nobody wrote and a file that is broken are different
  situations.

## Deliberately not here

- **A copy of Qdrant's storage directory.** It restores, and it is a
  terrible artifact. For a thousand points the tree measures **593 MB
  apparent against 924 KB real** — a forest of 32 MB sparse files (the
  write-ahead log, one memory-mapped vector chunk and one payload page per
  segment) — so a copy made with anything that does not understand holes
  writes hundreds of megabytes of zeroes per drill. The snapshot API is the
  vendor's answer to exactly that, and it is the one this adapter takes.
- **gRPC.** Qdrant serves the same operations on 6334, and nothing here
  needs them: the restore is a startup flag and the checks are HTTP.
