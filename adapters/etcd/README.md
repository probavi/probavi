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
