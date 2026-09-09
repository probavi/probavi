# Staging backups for a drill

A drill restores a backup that is already on the drill host: `probavi run`
hands the adapter a path, and the adapter moves those bytes into the
sandbox. Probavi fetches nothing over the network, so getting the artifact
to that host is the operator's job — this document is how to do it well.

This is guidance, not a contract. The configuration keys it uses are
specified in [`docs/drill-config.md`](drill-config.md); the sandbox verbs
it rests on are in [`docs/adapter-protocol.md`](adapter-protocol.md) §4.

---

## 1. Why staging exists

The adapter reaches the sandbox through exactly two verbs, `exec` and
`put_file`, and `put_file` copies a **host path** the drill configured.
There is no verb that reads a URL, and a protocol message is capped at
4 MiB, so bytes cannot be streamed in through `exec` either — the protocol
lists streaming transfer as a future verb, not a current one. Local is
therefore what the architecture allows today, not a default someone picked.

Two consequences worth internalising before choosing a pattern:

- **The artifact is copied twice**: once onto the drill host, once into the
  sandbox. Any design that moves the first copy into Probavi would still
  make both, so a staging step is not the thing standing between you and
  one copy.
- **Nothing about staging reaches the evidence record.** A record states
  what was restored — the source kind, the checksum over the bytes, the
  size, the backup's own creation time — never where the file came from.
  Your staging design is free; it just never appears in the proof.

## 2. Choosing a topology

| Pattern | Use when | The cost |
|---|---|---|
| **Push** from each backup host | You can schedule a command where the backups are written | One restricted SSH key per source host, pointing *at* the drill host |
| **Pull** from the drill host | You cannot schedule anything on the backup hosts | The drill host holds read credentials to every source |
| **Shared filesystem** (NFS/CIFS) | The storage is already exported and mounted estate-wide | Mount semantics can outlive the drill's timeout (§4) |
| **Object storage** via `rclone`/`aws` | The backups only ever exist in a bucket | Bucket credentials live on the drill host |

Push is the default recommendation, and the reason is not convenience.
The drill host already holds production data for the duration of every
drill, and it holds the signing key that makes records trustworthy. Giving
it credentials to reach every production host as well concentrates three
things in one place that do not need to be together. With push, the
direction reverses: production hosts hold a narrow key to the drill host,
and the drill host holds nothing.

## 3. Push from the backup host

On the drill host, one account per source host, with a key that can do
exactly one thing — write into that host's directory:

```
# /home/stage-prod-a/.ssh/authorized_keys  (mode 0600)
command="rrsync -wo /srv/backups/prod-a",restrict ssh-ed25519 AAAA… prod-a backup push
```

`rrsync` ships with rsync; `-wo` makes the channel write-only, and
`restrict` removes port forwarding, agent forwarding and PTY allocation.
A key that leaks buys an attacker the ability to write files into one
directory — not a shell, not the evidence log, not the other hosts' trees.

On the backup host, after the backup finishes:

```
# /etc/cron.d/probavi-stage — runs after the nightly backup
40 1 * * *  postgres  rsync -a --delete --partial-dir=.partial \
              /var/backups/orders/ stage-prod-a@drill.internal:orders/
```

`--partial-dir` keeps a half-transferred file out of the directory the
drill reads: an incomplete artifact under the name the drill picks up
would be a `source_corrupt` verdict blamed on the backup rather than on
the copy. `--delete` keeps the tree bounded, which matters because the
drill host now stores every database's artifact plus everything the
restores unpack.

## 4. Pull, mounts, and object storage

**Pull** is the same shape with the direction reversed: `rsync` from the
drill host over a key restricted to `rrsync -ro` on the source. Use it
when you cannot place a cron entry on the backup hosts, and accept that
the drill host now holds credentials to every source.

**NFS and CIFS** need one warning. A read from a hung *hard* NFS mount is
uninterruptible: the process sits in D state, and no context deadline,
signal or `sandbox.timeout` can end it. A drill blocked there does not
fail — it stops, past its own wall-clock limit, leaving no record until
the mount recovers, which is the one outcome this software treats as
severe. Mount with `soft` plus a bounded `timeo`/`retrans`, or use a FUSE
mount (sshfs, `rclone mount`), which stays killable. A read that fails is
a verdict; a read that hangs is not.

**Object storage** has no special support and needs none: `rclone copy` or
`aws s3 cp` into the staging tree, in the cron entry before the drill.
Note the boundary — `source.credential_env` names variables for the
*adapter* reading the backup, not for the copy step. Staging happens
before Probavi starts and manages its own credentials.

## 5. Many databases, one drill host

The shape that scales without surprises:

```
/srv/backups/prod-a/orders/          # one directory per database
/srv/backups/prod-a/billing/
/srv/backups/prod-b/analytics/
/etc/probavi/drills/prod-a-orders.yaml
/var/lib/probavi/prod-a.jsonl        # one evidence log per source host
```

- **One drill file per database.** That is the unit; a file never covers
  two.
- **Name drills `<host>-<db>`.** `target.name` is the identity in every
  record and the `drill=` label on every metric, so it has to be unique
  across the fleet.
- **Point `source.path` at the directory and use a `*_dir` source kind.**
  The adapter then picks the current artifact on every run and the file
  name stays out of the configuration.
- **One evidence log per source host** (or per database). The store holds a
  single-writer lock, so two drills can never share a log concurrently;
  splitting by host keeps that from becoming a scheduling constraint, and
  `probavi push` sends one log per invocation, so the split also decides
  what arrives separately at the receiver. Do not move a drill between
  logs: its RTO trend is computed from the log it writes to.
- **Stagger the cron entries, one `flock` per drill.** Restores are
  resource-hungry; one at a time on one host is the sane default, and it is
  a requirement rather than a preference when drills share a log.
- **Set `memory` and `cpus` in `sandbox.params`** for every drill, so one
  large restore cannot take the machine down with it.
- **One metrics textfile per drill.** The file is replaced whole, so two
  drills pointed at one path publish only whichever ran last.

Size the host for the largest staged artifact plus what it expands to when
restored, and rotate the staged copies (`--delete` above, or a retention
job) — the drill will happily keep restoring while the filesystem fills.

## 6. The failure mode staging adds

Staging inserts a step that can stop without anything noticing. When the
copy stalls, the tree still holds yesterday's — or last month's — artifact.
A `*_dir` source kind selects the newest one *present*, the restore
succeeds, every check passes, and the drill reports `pass` for weeks while
proving something progressively less useful.

Neither the exit code nor the usual alert catches this. `probavi run`
answers "did this artifact restore", which is true;
`probavi_last_success_timestamp_seconds` says when a drill last passed, not
how old the backup was.

What catches it is a **freshness check** — the one built-in that looks at
the data's age rather than the restore's success:

```yaml
- builtin: freshness
  table: orders
  column: created_at
  max_age: 36h        # backup cadence plus a margin
```

In a staged topology this is not optional decoration. Give every drill one,
on a table that is written continuously, with a `max_age` that would be
violated the day the copy stops. The record also carries
`backup.created_at` where the artifact states its own creation time, so an
auditor reading the log can see the age even where no check asserts it.

## 7. Rules that do not bend

- **`put_file` resolves paths.** A request beneath the configured source
  must resolve inside it with every symlink followed, so a symlink in the
  staging tree that leads out of it is refused. The configured path itself
  may be a symlink — `/srv/backups/prod-a/orders/latest` pointing at
  today's directory is an ordinary layout.
- **`sandbox.params` are recorded verbatim** in the signed record. An image
  name and a memory limit belong there; an endpoint, a host name or a
  secret does not. `DOCKER_HOST` and `PROBAVI_SSH_TARGET` are environment
  variables for exactly this reason.
- **Credentials for reading a backup are names, not values.** List them in
  `source.credential_env`; the core passes the named variables to the
  adapter and nothing else.
- **The staging tree is production data.** It holds the same bytes the
  database does. Permissions, encryption at rest and retention deserve the
  same treatment they get on the database host.

## 8. What Probavi will not do for you

There is no fetcher, no scheduler and no pre-drill hook — cron owns the
cadence and the staging step, and a drill starts with the artifact already
in place. That is a deliberate boundary, not a gap waiting to be filled:
the same two copies happen either way, and a fetch inside the core would
have to be measured, which makes it an evidence-schema question rather than
a convenience.

The one case this document cannot cover is an engine whose backups only
ever exist in a bucket, with no filesystem form to copy. `ROADMAP.md`
tracks that as an open question; if that is your situation, it is worth
saying so on the issue tracker rather than working around it.
