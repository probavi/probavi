# Backup manifest — design spec

Status: **Draft, awaiting the maintainer.** Design-normative once
approved; code follows approval, not the other way round (AGENTS.md §5.1,
and the ROADMAP item that asked for this document by name). Two decisions
in §9 are the maintainer's and are not taken here.

## 1. The gap this closes

An evidence record already carries `backup.checksum`, `backup.size_bytes`
and `backup.created_at` — the checksum, size and creation time the adapter
measured on the source it was handed. Nothing carries what those values
were *meant to be*.

So a drill proves that **the file it was handed restores**, not that **the
file the backup tool wrote restores**. Everything between the backup run
and the drill — a truncated copy, a half-finished `rsync`, an
object-storage download that returned 0 bytes with exit status 0, a
retention job that replaced the artifact with a different night's — is
outside what the record asserts. A drill can pass, honestly and in full,
on an artifact nobody intended to prove.

A manifest written at backup time, beside the backup, closes that: the
backup tool states what it produced, and the drill refuses to start if
what it finds disagrees.

## 2. What it is

One JSON file, written by the backup job, named by the drill config:

```yaml
target:
  source:
    kind: pgdump
    path: /backups/pg/nightly.dump
    manifest: /backups/pg/nightly.manifest.json
```

```json
{
  "schema": "probavi-manifest/1",
  "expected_checksum": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "expected_size_bytes": 4182016,
  "created_at": "2026-09-25T02:14:07.000Z",
  "engine_version": "16.4"
}
```

`schema` is required and pins the shape. Everything else is optional, and
a manifest carrying no expectation at all is refused as a configuration
mistake rather than accepted as a no-op: a file that asserts nothing, named
by a config that believes in it, is the failure this whole document exists
to remove.

| Field | Required | Meaning |
|---|---|---|
| `schema` | yes | `probavi-manifest/1`. |
| `expected_checksum` | one of the two | `sha256:<64 lowercase hex>` under the rule in §3 — **not** the adapter's checksum; see §4. |
| `expected_size_bytes` | one of the two | Total bytes under the same rule: a file's size, or the sum of the regular files in a tree. |
| `created_at` | no | RFC 3339. Informational: it is the backup tool's claim, and never populates `backup.created_at`, which is the adapter's measurement of the artifact. |
| `engine_version` | no | Informational. Version compatibility is the adapter's to enforce, from the artifact itself, and several already do. |

## 3. The checksum rule

The core computes this itself, on the drill host, before anything moves.
Two shapes, and nothing else:

- **A regular file**: SHA-256 over its bytes, exactly as stored.
- **A directory**: a canonical tree hash. Entries sorted by relative path;
  each regular file contributes its relative path, its size and its
  content bytes; each symlink contributes its relative path and its
  target; nothing else contributes. The same tree always hashes the same,
  and any content change changes the hash.

`expected_size_bytes` follows the same shapes: the file's size, or the sum
of every regular file's size in the tree.

Anything else at `source.path` — a device node, a socket, a path that does
not exist — is refused by name. The rule is deliberately small: it has to
be reproducible by whoever writes the manifest, with ordinary tools, on a
host that has never heard of Probavi (§9.1).

## 4. Why this is a second checksum, and why the two must not be compared

`backup.checksum` in an evidence record is **adapter-defined**, and the
definitions genuinely differ across the catalogue: SHA-256 over a file's
bytes for the single-artifact kinds, a canonical tree hash for the
directory kinds, and a two-member framing (`role NUL size NUL value`) for
the composite kinds that restore a backup *and* a second member. That is
correct: it answers *what did this drill restore*, measured by the only
component that knows what "the artifact" means for its engine.

The manifest answers a different question — *are these the bytes the backup
tool wrote* — and has to answer it **before any adapter has spoken**,
because the exit condition for this feature is that a mismatch fails the
drill *before* the restore. A checksum that can only be computed by
resolving the source inside an adapter cannot be checked before the
adapter runs.

So there are two rules, and they are not interchangeable:

| | Who computes it | When | Answers |
|---|---|---|---|
| `backup.checksum` | the adapter, per engine | during provision | what was restored |
| `expected_checksum` | the core, §3 | before the sandbox exists | are these the intended bytes |

**They will not be equal, and equality is not a property this design
offers.** For `kind: pgdump` they happen to coincide, because both are
SHA-256 over one file's bytes; for `pgdump_with_globals` they cannot,
because the adapter frames two members and the manifest hashes a tree. A
reader who compares them across kinds will find disagreement and conclude
corruption where there is none.

That is a trap, and §9.2 is where it is dealt with rather than documented
around.

## 5. When the check runs, and what a mismatch produces

After the configuration is loaded and the evidence log is open, **before
the sandbox is created and before any bytes are transferred**.

A mismatch is `source_corrupt`, outcome `error`, and a **signed record is
written**. That placement is deliberate and is the one thing this feature
cannot compromise on: a drill that ran and left no record is the
highest-severity failure this software has (evidence schema §7), and "the
backup was not what it should be" is precisely the finding an operator
needs in the log rather than only on a terminal. It joins the middle row
of `drill-config.md` §5.3 — detected before a sandbox exists, recorded
rather than merely reported.

The message names **both values**, in the order a reader needs them:

```
backup manifest mismatch: /backups/pg/nightly.dump is
sha256:3b7daaa5… (4181504 bytes), the manifest at
/backups/pg/nightly.manifest.json expects sha256:9f86d081… (4182016 bytes)
```

Failures of the manifest *itself* are separated from failures of the
comparison, because the remedies differ:

| Wrong | Code |
|---|---|
| `source.manifest` names a file that is not there, or cannot be read | `source_not_found` / `source_unreadable` |
| the file is not JSON, or `schema` is absent or unknown | `invalid_request` |
| the file carries neither expectation | `invalid_request` |
| `source.path` is not a file or a directory | `source_unreadable` |
| the artifact disagrees with the manifest | `source_corrupt` |

## 6. What this does not prove

**It catches corruption, not tampering.** Whoever can rewrite the backup
can rewrite the manifest lying beside it. The manifest is an assertion by
the same party, in the same place, with the same permissions — it detects
accident, transport loss and mistaken retention, which is most of what
goes wrong, and it detects nothing at all about an adversary who reached
the backup directory.

The tamper-evident form is a different design and is not this one: an
expected checksum written into the **drill config** is covered by
`drill.config_hash`, which is signed into every record — and it does not
scale, because it means one config file per backup. An external attestation
is the witness item on the ROADMAP, not this.

**It proves bytes, not restorability.** A manifest that matches says the
artifact is the one the backup tool wrote. Whether that artifact restores
is what the rest of the drill is for, and a matching manifest must never be
read as a shortcut past it.

**`created_at` and `engine_version` are claims, not measurements.** They
are recorded in the manifest because a backup job has them cheaply and a
reader wants them; the record keeps taking those two facts from the
artifact itself.

## 7. What is deliberately not in it

- **Expected row and table counts.** They need a live query against the
  restored database, which is after everything this file governs. They
  belong to the baseline check, and the ROADMAP places them there.
- **Anything the core would have to interpret per engine.** The core knows
  nothing about pg_dump, WAL or binlogs (AGENTS.md §2.1), and a manifest
  field that needed engine knowledge to check would move that boundary.
- **A signature.** See §6: a signature by the party that wrote the backup,
  verified with a key kept beside it, adds ceremony and not evidence. If
  the manifest is ever signed it will be by a key the drill host does not
  hold, and that is a different item.

## 8. The record

Three nullable fields, arriving with the single `probavi-evidence/3` bump
the ROADMAP gathers four items into — not a bump of their own:

| Field | Meaning |
|---|---|
| `backup.expected_checksum` | what the manifest said, verbatim; null when no manifest was named |
| `backup.expected_size_bytes` | likewise |
| `backup.manifest_hash` | SHA-256 of the manifest file's bytes, pinning *which* manifest was believed, the way `drill.config_hash` pins the configuration |

`backup.manifest_hash` is the load-bearing one. The other two are the
manifest's own words and are only as trustworthy as §6 allows; the hash is
a measurement the record makes itself, and it is what lets a later reader
ask whether the expectation a drill was checked against is the one still on
disk.

## 9. Open — the maintainer's, not this document's

### 9.1 Does a `probavi manifest write` ship?

The ROADMAP left this open in as many words. The two answers:

**Document the shape and let `sha256sum` produce it.** A backup job appends
three lines of shell. Nothing new ships, nothing new is installed on the
host where backups are taken, and the non-goals stay exactly where they
are. The cost is that the tree rule in §3 has to be reproducible in shell,
which constrains §3 to stay as small as it is — arguably a feature.

**Ship a one-shot `probavi manifest write <path>`.** It removes the chance
of an operator implementing the tree rule slightly differently and
discovering it at 3am. The cost is real and worth naming: it is the first
thing that asks for Probavi to be *present on the host where backups are
taken*. That is a one-shot command rather than the daemon the non-goals
refuse — but the non-goals are a closed list, and moving this line is a
decision, not an implementation detail.

**Recommendation: document the shape first, and ship the helper only if a
real backup job turns out unable to produce the tree hash correctly.** The
single-file case — which is most kinds — is one `sha256sum`. Deferring
costs nothing and keeps the distribution question shut until something
forces it open.

### 9.2 What goes in the record: the pair, or the verdict?

§4 leaves a trap: `backup.checksum` and `backup.expected_checksum` sit in
one block, are computed under different rules, and will disagree for
several kinds. Two ways out:

**Carry both, and document.** What the ROADMAP's field list assumes. The
trap stays, mitigated by the schema description and this document.

**Carry `backup.manifest_hash` and a verdict, not the expectation.** The
record would say a manifest was present, which one, and that the artifact
agreed with it — without placing a second, non-comparable checksum beside
the adapter's. A reader cannot misread what is not there, and nothing is
lost: the expectation itself is on disk, pinned by the hash.

**Recommendation: the second.** It is the cheaper record, the harder one to
misread, and it survives §4 without asking anyone to remember §4. It does
change a field list the ROADMAP recorded as accepted, which is why it is
here as a question rather than done.

## 10. Hand-off: the contract list

`probavi-manifest/1` is a new contract identifier, and the canonical list
of those lives in the workspace root's ADR 0035, not in this repository. It
has to be added there before this ships, or the manifest's `schema` value
is a version nothing governs.

## 11. Exit

A backup altered between the backup run and the drill fails **before the
restore**, and the record names the value that disagreed.
