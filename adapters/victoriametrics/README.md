# probavi-adapter-victoriametrics

Restores VictoriaMetrics backups into a disposable sandbox so a drill can
prove they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

VictoriaMetrics is where long-term metrics history goes, which makes its
backups the ones nobody notices are unrestorable until the day they are
needed. The engine is also unusually easy to back up *wrongly*: the
supported route is a snapshot plus `vmbackup`, while a plain copy of the
storage directory starts and serves happily in a quiet moment — and is
inconsistent under write load. This adapter refuses that copy by name.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `victoriametrics_backup` | one `vmbackup` output directory |
| `victoriametrics_backup_tar` | one tar archive (plain or gzip) of a `vmbackup` output — its files at the root, or under one wrapping directory (both measured) |
| `victoriametrics_backup_dir` | a directory of `vmbackup` outputs; the one whose own metadata claims the newest instant is restored |

The backup an operator takes is the two-step one the project documents:

```sh
curl http://vm:8428/snapshot/create          # freeze a consistent view
vmbackup -storageDataPath=/var/lib/vm \
         -snapshotName=<name> -dst=fs:///backups/vm-2026-08-18
```

PITR does not apply; the probe declares `pitr: false` so the core refuses
a `target.pitr` drill before anything runs.

## The sandbox image: three binaries, three upstream images

A drill needs the server, `vmrestore`, and a query client in one image.
VictoriaMetrics ships the first two separately and the third not at all,
so the drill sandbox runs a small wrapper you build yourself:

```dockerfile
FROM victoriametrics/victoria-metrics:v1.150.0
COPY --from=victoriametrics/vmrestore:v1.150.0 /vmrestore-prod /usr/local/bin/vmrestore
COPY --from=prom/prometheus:v3.5.5 /bin/promtool /usr/local/bin/promtool
RUN ln -s /victoria-metrics-prod /usr/local/bin/victoria-metrics
ENTRYPOINT []
```

Keep the two VictoriaMetrics tags identical: that is the whole point of
building the image rather than mounting a tool into it. `promtool` is
there as a **query client**, not as a second engine — see the checks
section for why an HTTP one-liner is not a substitute. A drill pointed at
an image missing any of the three fails up front, naming the one it could
not find.

```yaml
sandbox:
  provider: docker
  params:
    image: your-registry/probavi-victoriametrics:1.150   # the wrapper above
    command: sleep infinity                              # the adapter owns the engine lifecycle
    memory: 1g
source:
  kind: victoriametrics_backup
  path: /backups/vm-2026-08-18
```

The restored server listens on `127.0.0.1:8428` with no TLS and no auth,
acceptable for exactly one reason: a Probavi sandbox is zero-ingress
(`--network none`, no ports expressible). It scrapes nothing — the
restored server serves the backup, it must not collect.

## Retention is disabled, because a drill proves the artifact

The sandbox server starts with `-retentionPeriod=100y`.

VictoriaMetrics keeps **one month** by default (measured: the flag reads
`retentionPeriod=1M, is_set=false`), and it applies that to data it has
just been handed: a restored 90-day history serves **48 of its 89
samples**, with `vmrestore` reporting success and nothing anywhere
reporting the loss. Retention states what a *running* server should keep;
a drill proves what the backup holds, and the operator's real policy is
already expressed in which samples the backup contains.

`100y` is the largest value the server accepts — `1000y` is refused, and
`0` is refused outright rather than silently meaning "unlimited"
(`-retentionPeriod cannot be smaller than a day`, all measured).

## The four fences

Measured behaviours this adapter refuses to paper over:

1. **A copy of a live `-storageDataPath` is refused by name.** It carries
   `flock.lock`, `snapshots/` and `tmp/`, which a `vmbackup` output never
   does, and it lacks the completion marker one always has. This is the
   dangerous artifact precisely because it *works* in a quiet moment: it
   starts and serves every sample (measured). Under write load it is a
   torn copy, and no drill should have blessed it.
2. **A backup that never finished is refused.** `vmbackup` writes an
   empty `backup_complete.ignore` last. `vmrestore` refuses a directory
   without it — and names `-skipBackupCompleteCheck`, a flag that would
   restore it anyway. This adapter never passes that flag: restoring past
   the tool's own refusal proves nothing about the backup.
3. **A truncated copy is refused by the artifact's own account of
   itself.** Every partition's `parts.json` names the parts that
   partition holds, so a part named there but absent is a copy that lost
   something in transit — refused host-side, before a byte is
   transferred. The engine makes the same check when it opens the
   storage, and its message is exact (`part "…" is listed in
   "…/parts.json", but is missing on disk`), which is the backstop for an
   archive the host could only walk for markers.
4. **A restore that produced no data is refused.** `vmrestore` restores a
   truncated backup *silently* — exit 0, fewer files, not a word
   (measured) — so the tool's verdict is never the drill's. After the
   server is up, the drill reads the series count from the server's own
   `/api/v1/status/tsdb` and refuses a well-formed zero: a server that is
   up but holds none of the promised data is exactly the false green a
   metrics backup invites.

The count comes from the status endpoint rather than from a query
deliberately: no lookback window then stands between the artifact and the
verdict, so a backup of an idle instance is still a backup.

## Checks: the MetricsQL dialect, evaluated at the backup's own instant

VictoriaMetrics has no SQL. The declared runner passes the check text to
`promtool query instant` as **one MetricsQL expression** — no shell
anywhere — and evaluates it at the instant the backup's own metadata
states, delivered through the protocol's `{{database}}` placeholder:

```yaml
checks:
  - name: history_survived
    sql: sum(count_over_time(probavi_history[90d]))
    expect: "12960"
  - name: targets_were_up
    sql: count(count_over_time(up[1h]))
    expect: "3"
```

`expect` is required and compared against the runner's whole output, so a
check has to name one number. The runner prints the sample **value** and
nothing else — one line per series — so aggregate to a single series
(`count`, `sum`) and the comparison is the number you would read off a
graph. Labels are not part of the row, and neither is the evaluation
instant.

That last point is the fix in issue #175: the runner used to print
promtool's annotated sample (`{} => 45886 @[1787113801]`), which no
`expect` could match, and the trailing instant changes with every backup —
so no literal could be written that matched twice. §6.1 of the protocol
requires undecorated rows; the adapter now delivers them.

**Why a client binary and not `wget`.** The obvious one-liner —
`wget --post-data="query=…"` — sends a form-encoded body, in which `+`
decodes as a space. `count({__name__=~".+"})` then asks for `". "` and
answers **zero on a populated server** (measured): a silent wrong answer,
which is worse than a broken one. Passing the query as a single argv
element removes the encoding question entirely.

Prefer **range** expressions (`…[90d]`, `count_over_time`) over bare
instant selectors. An instant check sees only samples within the
lookback window of the evaluation instant, which for a backup of an idle
instance may be none at all — the data is there, the instant is simply
quiet. Ranges read the history the backup exists to preserve, and reading
it is also what verifies it.

## When the backup was taken

`created_at` is what `backup_metadata.ignore` states —
`{"created_at":"2026-08-18T18:23:25Z","completed_at":"…"}` (measured) —
the instant the snapshot froze the data, not the instant the copy
finished. It is already RFC 3339 UTC, so the `source.params.backup_timezone`
key the other adapters use has nothing to act on here and is refused
rather than silently ignored. The `victoriametrics_backup_dir` kind ranks
candidates by this same claim, so a stale backup copied yesterday never
outranks the genuinely newest one.

## Backup identity

For the archive kind, `checksum` is SHA-256 over the tar's bytes, exactly
as stored. For the directory kinds it is a canonical hash of the backup
tree: entries sorted by relative path; each regular file contributes its
path, size, and content bytes; symlinks contribute path and target. The
same tree always hashes the same, and any content change changes the
hash.

## Deliberately not here

- **`vmbackup` itself.** Probavi restores backups; it does not take them.
  The snapshot-then-backup sequence belongs in the operator's runbook,
  and the drill's job is to prove its output.
- **Object-store backups** (`-dst=s3://…`, `gs://…`). The sandbox has no
  network and the source contract is a local path, so staging is the
  operator's documented job — the same trade every object-store engine in
  the catalog gets today.
- **Cluster VictoriaMetrics** (`vmstorage`/`vminsert`/`vmselect`). One
  sandbox hosts one service; a cluster drill is a different shape, and
  claiming it from a single-node restore would be dishonest.
- **`-skipBackupCompleteCheck`.** Named here because it exists and is
  tempting: it is the one flag that would turn fence 2 off.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| a copy of a live storage path (or a tar of one) | `unsupported_source`, teaching the snapshot-plus-vmbackup route |
| a backup with no completion marker | `source_corrupt`, naming the marker |
| a tree that is not a `vmbackup` output | `source_corrupt` |
| metadata stating no readable `created_at` | `source_corrupt` |
| a part a `parts.json` names but the artifact lacks | `source_corrupt`, naming the part |
| tar cannot unpack the archive | `source_corrupt` |
| the sandbox image lacks the server, `vmrestore` or the client | `invalid_request`, naming the missing one |
| `vmrestore` fails for any other reason | `restore_failed`, carrying its message |
| the restored server dies on incomplete storage | `source_corrupt`, carrying the engine's own line |
| the restored server holds no series at all | `source_corrupt` |
| the restored server never became ready | `engine_not_ready`, or `restore_failed` with the server's own log line |
