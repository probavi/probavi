# probavi-adapter-influxdb

Restores InfluxDB 2.x `influx backup` outputs into a disposable sandbox
and serves the restored buckets, implementing `probavi-adapter/0`
(docs/adapter-protocol.md). Self-contained: standard library only, no
imports from the Probavi core.

## Supported source kinds

| Kind                | Path points at |
|---------------------|----------------|
| `influx_backup_tar` | One tar archive (plain or gzip) of an `influx backup` directory — members at the root, or under one wrapping directory |
| `influx_backup`     | One `influx backup` output directory: the timestamped manifest beside the KV store, the SQL store, and the shard files |
| `influx_backup_dir` | A directory of them; the newest **by the backups' own timestamp stems** is restored |

A reused backup target directory — `influx backup` run repeatedly into
the same path — accumulates timestamped sets side by side; the
`influx_backup` kind restores the newest of them by the stems the
backups named themselves, and only that set's files enter the backup
identity. What this adapter does not restore, on purpose, is in
"Deliberately not here" below.

## The manifest is the contract

A 2.x backup's manifest (JSON, `manifestVersion: 2` — measured) names
every member the artifact must contain: the KV store, the SQL store,
and one file per shard. That gives this adapter the gate the format
needs most: **a partial copy is refused by the member it lost**, before
a byte reaches the sandbox — for the tar kind, against the archive's
own table of contents. In the sandbox, `influx restore` stays the
authority on what actually loads, and after it the **bucket census**
compares the restored organization's own bucket listing against the
manifest's — a bucket missing from an organization the restore itself
created can only mean part of the backup did not come back, and the
drill refuses to prove a partial restore.

An archive the host cannot walk still drills: the sandbox extraction is
the authority, and the manifest is recovered from the unpacked tree so
the census and the fences run on facts read from somewhere real.

The walk itself is bounded, and that bound is a refusal rather than a
silent skip. A tar entry is a 512-byte header that compresses to almost
nothing, so a small archive can carry any number of manifest members,
each read into memory at up to 8 MiB — an archive would otherwise
decide how much memory the drill host spends. Past the bound the
adapter answers `source_corrupt` and stops reading: an archive built
that way is not an `influx backup` directory, and a drill killed for
memory would leave no evidence record at all.

## No credential from the backup is ever needed

The adapter initializes the fresh instance with fixed sandbox-local
constants (`probavi`/`probavi-sandbox`, token `probavi-sandbox-token`,
organization `probavi-drill`) and drives a **plain** `influx restore`
with that token. Measured: the plain restore creates the backup's own
organizations and buckets beside the sandbox one and needs nothing
else. The constants are documented public values, never secrets — the
influxdb analog of the postgres adapter's trust auth and the mssql
adapter's published sa password — and the only reason that is
acceptable is the sandbox's zero-ingress default (`--network none`, no
ports expressible).

`influx restore --full` is deliberately not used: a full restore
replaces the KV store and with it every credential, locking the drill
out behind the backup's own tokens. Restoring the data while proving
recoverability with sandbox-local auth is the honest trade, and the
README says it so nobody widens the claim.

## Organizations

Checks run against one organization — the connection's `database`
carries its name for the declared runner's `-o`. A backup holding a
single organization needs no configuration; one holding several is
restored whole either way, but the drill refuses to guess which one the
checks should target: set `options.database` in the drill config.

## Checks: one Flux query

InfluxDB 2.x has no SQL, so the generating built-in checks
(`row_count`, `table_exists`, `freshness`) do not apply — the same
consequence the MongoDB, Redis, and etcd adapters document. A check's
text is one Flux query, delivered as a single argument (no shell
anywhere):

```yaml
checks:
  - kind: query
    sql: from(bucket:"metrics") |> range(start:0) |> group() |> count()
    expect: "500"
```

## Retention is not enforced in the drill

A bucket carries a retention period, and `influx restore` restores it
with the data — a bucket backed up at `1h0m0s` comes back at `1h0m0s`
(measured). The restored server then enforces it exactly as a production
server would, on data it has just been handed.

The points in a backup were inside their bucket's window when
`influx backup` ran; an artifact holding points already outside it cannot
even be written, because the write path rejects them outright (`422 …
dropped 1 points outside retention policy of duration 1h0m0s`). Time then
passes — or the operator shortens the bucket — and by the drill the same
points are outside.

Measured on the baseline image, restoring a backup of a one-hour bucket
holding seven points spread over three hours, beside a control bucket with
infinite retention:

| | at the restore | one check later |
| --- | --- | --- |
| `metrics` (retention 1h) | 7 points | **3 points** |
| `audit` (retention ∞) | 1 point | 1 point |
| buckets the census sees | 5 | 5 |

The census is why this matters here. The adapter's own completeness
verdict compares the buckets the instance holds against the ones the
manifest names, and a bucket that lost every point it had is still a
bucket — so nothing reports the loss and the drill goes green having
proved less than the backup holds.

Worse, whether it happens at all is a matter of timing: the enforcer runs
on a ticker whose default is thirty minutes, so a short drill escapes and
a long one does not. The same backup, drilled twice, could give two
answers.

So the sandbox instance starts with the enforcer's first tick moved past
any drill:

```
influxd … --storage-retention-check-interval 876000h
```

Retention itself is untouched: `influx bucket list` inside a drill reports
the operator's own periods, so a check reading a bucket's configuration
sees the truth. Only the enforcement is suspended, and only in the
sandbox.

Zero is not the way to say "never", and is worth naming because it is the
obvious guess: the flag parser accepts `0` and the server then dies with
`panic: non-positive interval for NewTicker` without ever opening a port.

## When the backup was taken

`influx backup` names every file of a set with the UTC instant it wrote
it (`20260817T194144Z.…` — measured), so `backup.created_at` comes from
the artifact's own stem with no timezone question, and the same stems
rank the directory kinds. Declaring `source.params.backup_timezone` is
refused rather than ignored. A renamed set restores but reports no
created_at — nothing is invented.

## The 1.x fence: a migration is not a restore

An InfluxDB 1.x portable backup (`influxd backup -portable`) carries a
manifest of its own shape (a top-level `meta` entry, no `kv` —
measured on 1.12.4). Restoring one into 2.x is a migration (`influxd
upgrade`), not a recovery, and a drill that ran one would prove a path
nobody's recovery takes — so the artifact is refused by name, host-side
when the manifest is readable and from the recovered manifest when it
was tarred opaque. The sandbox side is fenced too: an image whose
`influxd version` names a non-2.x line is refused up front.

## Backup identity

`source_identity.checksum` is sha256 over the chosen set — the manifest
plus every member it names, sorted, each contributing name, size, and
content — so a neighbouring backup in a reused directory never enters
the identity. For the tar kind it is sha256 over the archive's bytes,
exactly as stored.

## Source params and options

| Name                       | Meaning |
|----------------------------|---------|
| `source.params.backup_timezone` | Refused — the backup's stem is UTC by construction; the declaration has nothing to add. |
| `options.database`         | The organization checks run against; required only when the backup holds several. |

## Environment

No credentials are needed to read a backup; `source.credential_env` is
unused. The adapter never prints secrets, and there are none to print.

## Deliberately not here

- **InfluxDB 1.x restores.** The 1.x portable format is refused by name
  (see the fence above); restoring it *into a 1.x engine* would be a
  separate, measured piece of work on a legacy line, taken up if real
  demand appears.
- **InfluxDB 3.x.** The 3.x line has no backup command at all — its
  backup story is an object-store copy whose consistency contract would
  have to be established first (ROADMAP.md names it separately).
- **`influx restore --full`.** See above: it would trade the drill's
  independence for the backup's credentials.
- **PITR.** `influx backup` captures a point in time and the format
  offers no replay target to drive; every source kind probes
  `pitr: false`.
