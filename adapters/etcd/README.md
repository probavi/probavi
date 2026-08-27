# probavi-adapter-etcd

Restores etcd snapshots into a disposable sandbox so a drill can prove
they still work. Implements `probavi-adapter/0`
([`docs/adapter-protocol.md`](../../docs/adapter-protocol.md)); like the
other adapters it is standard-library-only Go with no imports from the
Probavi core.

etcd is the smallest adapter in this repository and arguably the highest
stakes per byte: it is the Kubernetes state store, and an etcd snapshot
that does not restore is a cluster that does not come back.

## What it restores

| `source.kind` | What `source.path` points at |
| --- | --- |
| `etcd_snapshot` | one snapshot file from `etcdctl snapshot save` |
| `etcd_snapshot_dir` | a directory of them; the newest file is restored |

Only `etcdctl snapshot save` output is supported. That format carries an
appended integrity hash; a `db` file copied out of a live data directory
(`member/snap/db`) lacks it, and the drill refuses it with a message that
says to change how the backup is taken rather than "corrupt". The hash is
this format's only self-verification, and a drill should not paper over
its absence.

PITR does not exist for etcd; the probe declares `pitr: false` so the
core refuses a `target.pitr` drill before anything runs.

## The sandbox image needs a shell — the official images have none

Measured: `quay.io/coreos/etcd` and `gcr.io/etcd-development/etcd` images
contain exactly three files of interest — `etcd`, `etcdctl`, `etcdutl` —
and nothing else. No shell, no coreutils.

A restore *requires* starting the server after `etcdutl snapshot restore`
writes the data directory, and that sequencing needs a shell to detach
the process (etcd has no daemonize mode, and the protocol's `exec` verb
runs one foreground command). So the drill sandbox runs a two-line
wrapper you build yourself:

```dockerfile
FROM quay.io/coreos/etcd:v3.5.21 AS etcd
FROM alpine:3.22
COPY --from=etcd /usr/local/bin/etcd /usr/local/bin/etcdctl /usr/local/bin/etcdutl /usr/local/bin/
```

The engine binaries are exactly the official image's; alpine adds the
shell. A drill pointed at the raw official image fails up front with a
message that names this section. The integration suite builds the same
wrapper from the manifest's listed image, so "verified against etcd
3.5" means those binaries, exercised this way.

```yaml
sandbox:
  provider: docker
  params:
    image: your-registry/probavi-etcd:3.5   # the wrapper above
    command: sleep infinity                  # the adapter owns the engine lifecycle
    memory: 512m
source:
  kind: etcd_snapshot
  path: /backups/etcd/snap-2026-08-14.db
```

The restored server listens on `127.0.0.1:2379` with no TLS and no auth.
That is acceptable for exactly one reason: a Probavi sandbox is
zero-ingress (`--network none`, no ports expressible), so nothing outside
the disposable container can reach the restored data. A snapshot restore
also generates a fresh cluster identity — the restored member never tries
to reach its original peers, and could not if it tried.

## Leases are held open for the drill

A key attached to a lease exists only while somebody keeps renewing it. A
snapshot captures both the key and the lease, and on restore the lessor
**re-arms every lease with its full time to live** — etcd says so itself,
in the help text of the flag that exists to prevent it
(`--experimental-enable-lease-checkpoint`, which "prevents indefinite
auto-renewal of long lived leases"). So the countdown starts again when
the sandbox starts, and then runs out *during the drill*.

Measured on 3.5 and 3.6, a snapshot of 100 plain keys beside 100 attached
to a twenty-second lease — and `TestLeasedKeysSurviveTheDrill` runs
against every listed version, 3.7 included:

| seconds after the restore | plain keys | leased keys |
| --- | --- | --- |
| 1 | 100 | 100 |
| 20 | 100 | 100 |
| 27 | 100 | **0** |

The restore reports success either way. What a check reads depends on when
it runs — the same backup, drilled twice, answering differently for
reasons that have nothing to do with the backup.

**Auto-compaction, by contrast, is not the problem.** It is off by default
in all three listed versions (`"auto-compaction-retention":"0s"` in each
server's own startup log) and removes superseded revisions rather than
live keys, so a drill reading current values would not notice it either
way.

**There is no server-side pin**, and the flags moved under us without
changing that. 3.5 offers the `--experimental-enable-lease-checkpoint`
pair; 3.6 keeps it, marks it deprecated and adds the same thing as
feature gates; 3.7 has dropped the experimental pair exactly as 3.6 said
it would, leaving `LeaseCheckpoint` and `LeaseCheckpointPersist` (both
ALPHA, both off) plus a new `FastLeaseKeepAlive` (BETA, on by default).
None of them pins a lease: the checkpoint pair makes expiry *stricter*,
and `FastLeaseKeepAlive` changes how keep-alives are served rather than
whether a lease can run out.

So the drill uses the mechanism etcd's own clients use: it refreshes the
snapshot's leases for as long as the sandbox lives. The leases stay
exactly as the backup declared them — `lease timetolive` in a drill
reports the operator's own `granted with TTL(20s)` — and only their expiry
is suspended. A snapshot with no leases gets no keeper at all.

### One keeper, not one per lease

The obvious shape is one streaming `etcdctl lease keep-alive` per lease,
and it is a trap worth naming: measured, 200 leases spawned 133 client
processes and the sandbox's own server was killed for memory before the
drill could read anything. A single loop refreshing each lease in turn
costs one process at a time and held the same 200 leases indefinitely,
with the server healthy throughout.

The cost is about **10 ms per lease per sweep** — 200 leases sweep in two
seconds — which bounds the residual honestly: a snapshot with enough
leases, or leases short enough, that a sweep cannot outrun the shortest
time to live will still lose them. That is no worse than what happens
without a keeper, and it is stated here rather than implied away.

## Checks: the etcdctl dialect

etcd has no SQL. The declared runner passes the check text to `etcdctl`
as arguments, so **checks for this adapter are etcdctl argument lines**,
written in the drill config's raw `sql` field:

```yaml
checks:
  - name: config_present
    sql: get /probavi/config --print-value-only
  - name: keys_survived
    sql: get --prefix /probavi/ --count-only --write-out=fields
```

The expansion is word splitting only, never shell parsing: a POSIX shell
does not re-read expansions as syntax, so `;`, `|`, `$()` and friends in
a check stay literal arguments (unit-tested), and globbing is disabled.
Built-in checks that generate SQL (`row_count`, `table_exists`,
`freshness`) do not apply to this adapter — the same trade the mongodb
adapter documents, and the protocol's design working as intended (§6.1).

## When the backup was taken

`created_at` in the evidence record is **always null** for this adapter,
and that is deliberate: an etcd snapshot records revisions and raft
terms, not the wall clock it was taken at (measured — `etcdutl snapshot
status` reports hash, revision, key count and size, nothing more). A
file's mtime dates a copy, not a backup, so it is not reported either.
The `source.params.backup_timezone` key the other adapters use has
nothing to act on here, and a config that sets it is refused rather than
silently ignored.

The same fact drives the directory kind: with nothing better to rank by,
`etcd_snapshot_dir` picks the newest file by mtime, with the in-flight
guard the other directory kinds share (see `settle.go`).

## Backup identity

`checksum` is SHA-256 over the snapshot file's bytes, exactly as stored.

## Errors it reports

| Situation | Code |
| --- | --- |
| `source.path` does not exist | `source_not_found` |
| the sandbox image has no shell | `invalid_request`, naming the wrapper recipe |
| `etcdutl` rejects the file as a snapshot | `source_corrupt` |
| the snapshot lacks its integrity hash (data-dir copy) | `source_corrupt`, saying how to take snapshots |
| the restore ran and failed | `restore_failed` |
| the restored server never became healthy | `engine_not_ready`, or `restore_failed` with the server's own log line when it has one |
