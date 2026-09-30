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

## 7. Encrypted backups

A backup that cannot be decrypted is not a backup, and a drill that never
tries is not proving much. **Probavi holds no key of its own and decrypts
nothing**: it has no cipher, no key store and no notion of what your
archive is wrapped in. What it needs is either a readable artifact on the
drill host, or an adapter whose engine tooling can read the encrypted one.

Today that means the first. This section is how to arrange it, what an
encrypted artifact actually meets if you hand one over unchanged, and two
boundaries that are deliberate rather than unfinished.

### 7.1 Decrypt while staging, which is the answer today

The cron entry that copies also decrypts. The drill then sees an ordinary
artifact and nothing about it is special:

```sh
#!/bin/sh
# Stage tonight's backup, decrypted, for the drill that follows.
set -eu
stage=/srv/backups/prod-a/orders
mkdir -p "$stage"

# age, with the identity readable only by this job's user.
age --decrypt -i /etc/probavi/staging.age-key \
    -o "$stage/latest.dump.new" /mnt/archive/orders/latest.dump.age
mv "$stage/latest.dump.new" "$stage/latest.dump"   # atomic, so no drill
                                                   # reads a partial file
```

Three things make this the recommended route rather than merely the
available one.

**The key never enters Probavi.** It lives where the staging job's other
credentials live — a file that job reads, or an agent it talks to — and no
Probavi configuration, protocol message or record has a field for it.

**The decryption is measured by nothing, which is correct.** A record
states what was restored and how long the restore took. How the bytes came
to be readable is outside the proof, exactly as §1 says of staging
generally.

**It fails where failure is cheap.** A rotated key, a missing identity or a
corrupt wrapper stops the cron entry with the tool's own message, before a
drill starts. That is a better place to find out than in a signed record.

The cost, stated plainly: **a decrypted copy exists on the drill host for
as long as you keep it.** That widens exposure by exactly one host, and
§8's rule about the staging tree applies to it with more force than usual.
Two mitigations worth the trouble:

- Stage onto a `tmpfs` where the host has the memory, so the plaintext
  never reaches a disk that outlives a reboot. Size it for one artifact,
  not the retention window.
- Delete after the drill in the same cron entry, rather than by a separate
  job that can stop without anyone noticing — the failure mode §6 is about,
  pointed the other way.

### 7.2 What an encrypted artifact meets today

Handing an encrypted file to a drill unchanged is a supported thing to do
in the sense that it fails honestly. It is worth knowing *how*, because the
three shipped answers differ and none of them is "it silently passed":

| Where | What happens | Outcome |
|---|---|---|
| ArangoDB | An encrypted dump is **recognised and refused by name**: `arangodump` writes the `ENCRYPTION` marker either way, so the adapter reads it and says which encryption the dump states rather than failing obscurely later. | `source_corrupt` |
| DuckDB | An encrypted database **fails its opening read with the engine's own words**, carried into the message verbatim. Key handling is a design that adapter's first release did not assume. | `source_corrupt` |
| pgBackRest (postgres) | An encrypted **manifest cannot be read** — `repo1-cipher-type` makes `backup.info` unreadable — so `backup.created_at` is **null rather than guessed**. The restore is a separate question the repository's own tooling owns. | not a failure |

**No shipped adapter decrypts an artifact.** That is the state of the
catalogue rather than an oversight: decryption is engine-specific and
key-handling is a design, so it arrives per adapter when someone needs it,
the way everything else in that catalogue arrived. Each adapter's README is
the answer for that adapter, and this document does not speak for them.

The shape such an adapter would use needs nothing new from the core.
`source.credential_env` is already defined as the names of the environment
variables an adapter needs **in order to read the backup**, which is a
decryption passphrase exactly; adapters already use it for engine
passwords. So an encrypted source is an adapter-side source kind on a
mechanism that exists — no core configuration key, no protocol version, and
no adapter obliged to care. Names, never values, as always.

### 7.3 Decrypting inside the sandbox, and the trap in it

The remaining route is to put the ciphertext in the sandbox and decrypt it
there, so no plaintext ever lands on the drill host. It works, and it has
one trap that this project has already paid for once.

**Do not assume the tool is in the image.** An adapter that reached for a
binary its engine's image did not ship once blamed the *backup* for the
binary's absence — a failure that read as "your artifact is bad" when it
meant "this image has no such program". `age` and `gpg` are absent from
most of the engine images in this catalogue, and the sandbox has **no
network by default**, so nothing can install one at drill time.

If you take this route, the image is yours: build it, pin it, and put the
tool in it. The record then carries `sandbox.params.image` — the tag you
asked for — and `sandbox.image_digest`, the bytes that actually ran, which
is the field that tells an auditor the decrypting image was the one you
think it was.

### 7.4 Two boundaries, both deliberate

**There is no `source.decrypt`.** A configuration key naming a command to
run would be arbitrary code execution driven from the file whose bytes
`drill.config_hash` signs — so a record would attest that a command ran
without attesting what it was. It would also be a new configuration key for
a job the same cron entry already does, which is §9's argument applied one
level down.

**No key material can reach a record, and four rules keep it out.**
Credentials are names in `source.credential_env` and the core passes only
the named variables. `sandbox.params` are recorded verbatim, so nothing
secret belongs there — which is why `DOCKER_HOST` and `PROBAVI_SSH_TARGET`
are environment variables (§8). And a record carries checksums, sizes and
durations, never bytes of the artifact.

The fourth covers what the first three cannot. Those three keep the value
out of the places the core *chooses* what to write; the remaining route in
is text the core did not compose — an adapter's error message, an engine's
healthcheck reply — and an engine quoting its own configuration back at a
failure is ordinary behaviour, not misbehaviour. So the core masks the
values of the variables named in `source.credential_env`, together with
the ephemeral sandbox password, out of every such string before it reaches
a record, a log line or a notification (`docs/evidence-schema.md` §8). It
is defence in depth and not permission: an adapter still must not put a
secret in a protocol message, because masking only catches a value that
arrives recognisable — one an engine hex-encoded or wrapped across lines
passes straight through.

This has a consequence for what you name in `source.credential_env`.
Declare what an adapter needs in order to read the backup, and nothing
else: a variable declared there is masked wherever its value appears, so
declaring a non-secret — a user name, a database name, a path — replaces
that word with `[redacted]` in the diagnostics you will want to read.

One consequence worth knowing before it surprises you: **a backup
manifest's checksum is over the bytes the drill is handed.** If the backup
job writes one over the ciphertext and the drill is handed plaintext, the
two can never agree, and every drill fails `source_corrupt` on an artifact
that is fine (`docs/backup-manifest.md` §3). Write the manifest over
whatever the drill will actually see, which for the pattern in §7.1 means
after decryption rather than before.

Finally, a note on what this is usually *for*. The failure most often
imagined here is a key rotated out from under the archive, and that one is
caught more cheaply somewhere else: `select: oldest`
(`docs/drill-config.md` §3.2) drills the oldest member still in the
retention window, which is both the artifact an incident reaches for and
the one whose key is most likely gone. Encryption support and key-rotation
coverage are different problems, and only one of them needs anything from
this section.

## 8. Rules that do not bend

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
  same treatment they get on the database host — and a tree holding
  *decrypted* copies of an encrypted archive (§7.1) deserves more, because
  it is the one place the plaintext exists outside the database.

## 9. What Probavi will not do for you

There is no fetcher, no scheduler and no pre-drill hook — cron owns the
cadence and the staging step, and a drill starts with the artifact already
in place. That is a deliberate boundary, not a gap waiting to be filled:
the same two copies happen either way, and a fetch inside the core would
have to be measured, which makes it an evidence-schema question rather than
a convenience.

The one case this document cannot cover is an engine whose backups only
ever exist in a bucket, with no filesystem form to copy. [The engine
catalogue](engine-catalog.md) tracks that as an open one-way door; if that is your situation, it is worth
saying so on the issue tracker rather than working around it.
