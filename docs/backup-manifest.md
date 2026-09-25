# Backup manifest — design spec

Status: **Normative, and implemented** — `target.source.manifest`,
`internal/manifest`, shipped 2026-09-25. The two decisions §9 held open
were taken the same day and are recorded there with the alternatives they
beat; the code followed this document rather than the other way round
(AGENTS.md §5.1). One part of §8 is deliberately not built yet and says
so where it is specified: the two record fields ride on the single
`probavi-evidence/3` bump the ROADMAP gathers four items into.

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

A backup manifest written at backup time, beside the backup, closes
that: the backup tool states what it produced, and the drill refuses to
start if what it finds disagrees.

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

The file is the **backup manifest**, and this document always says both
words. In this repository the unqualified *manifest* is
`docs/capabilities.json`, the capabilities manifest — a different file,
written by a generator rather than by a backup job, and the collision is
worth spending a word on every time.

`schema` is required and pins the shape. Everything else is optional,
and a backup manifest carrying no expectation at all is refused as a
configuration mistake rather than accepted as a no-op: a file that
asserts nothing, named by a config that believes in it, is the failure
this whole document exists to remove.

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
- **A directory**: a canonical tree hash — one SHA-256, fed in this order
  and no other. Take every regular file and every symlink below the
  directory; name each by its slash-separated path relative to that
  directory; sort those paths by byte value. Then, in that order, feed
  `<path>` `NUL` `<size in decimal>` `NUL` `<the file's bytes>` for a
  regular file, and `<path>` `NUL` `L<target>` `NUL` for a symlink.
  Directories themselves, device nodes, sockets and FIFOs contribute
  nothing. The same tree always hashes the same, and any content change
  changes the hash.

The framing is stated to the byte because reproducing it is the entire
point of this rule (§9.1). `NUL` is the zero byte; a size is plain decimal
with no padding; the `L` in front of a symlink's target is what stops a
link pointing at `x` from framing identically to a one-byte file
containing `x`. Byte-value sort order is not the order a directory walk
produces — `a.txt` precedes `a/b`, because `.` is 0x2E and `/` is 0x2F —
and that is the detail a second implementation gets wrong first.

`expected_size_bytes` follows the same shapes: the file's size, or the sum
of every regular file's size in the tree. Symlinks contribute to the
checksum and not to the size: a symlink's own size is the length of its
target and not a byte of the backup.

Anything else at `source.path` — a device node, a socket, a path that does
not exist, a directory holding no regular file at all — is refused by
name (§5). The rule is deliberately small, and §9.1 is what keeps it
small.

### 3.1 Reproducible is a claim, so it is a test

A backup manifest is written by whoever takes the backup, on a host that
has never heard of Probavi. So the rule above ships with a recipe:

```sh
# The directory rule. bash, GNU findutils, GNU coreutils.
# Prints "<hex>  -"; $dir is the directory named by source.path.
cd "$dir" || exit 1
find . \( -type f -o -type l \) -printf '%P\0' |
  LC_ALL=C sort -z |
  while IFS= read -r -d '' p; do
    if [ -L "$p" ]; then
      printf '%s\0L%s\0' "$p" "$(readlink -- "$p")"
    else
      printf '%s\0%s\0' "$p" "$(stat -c '%s' -- "$p")"
      cat -- "$p"
    fi
  done | sha256sum
```

A single-file source needs no recipe: it is `sha256sum <path>`. Either
way the backup manifest's value is `sha256:` followed by that hex — the
prefix is part of the field, not decoration, and §2's pattern requires
it.

**The implementation carries a test that runs the recipe above** against a
fixture tree — nested directories, a symlink, an empty file, and a file
whose name sorts before a sibling directory's — and fails if its digest
differs from the core's. It is
`TestThePublishedRecipeAgreesWithTheRule`, and it reads the recipe out of
this document rather than carrying a copy, so editing the block above
without editing the rule fails the build. The recipe is therefore a gate and not an illustration: the rule
and the published way to satisfy it cannot drift apart without CI saying
so. A specification that asserts reproducibility without ever executing
the reproduction is asserting the one thing it is least able to check.

The recipe's dependencies are named rather than hidden: GNU `find -printf`,
`sort -z` and `stat -c`, and bash for `read -d ''`, which POSIX `read` does
not have. A backup host with none of them is the first of the triggers in
§9.1 — and it is a trigger precisely because the answer there is not "write
your own loop".

## 4. Why this is a second checksum, and why the two must not be compared

`backup.checksum` in an evidence record is **adapter-defined**, and the
definitions genuinely differ across the catalogue: SHA-256 over a file's
bytes for the single-artifact kinds, a canonical tree hash for the
directory kinds, and a two-member framing (`role NUL size NUL value`) for
the composite kinds that restore a backup *and* a second member. That is
correct: it answers *what did this drill restore*, measured by the only
component that knows what "the artifact" means for its engine.

The backup manifest answers a different question — *are these the bytes
the backup tool wrote* — and has to answer it **before any adapter has
spoken**, because the exit condition for this feature is that a mismatch
fails the drill *before* the restore. A checksum that can only be
computed by resolving the source inside an adapter cannot be checked
before the adapter runs.

So there are two rules, and they are not interchangeable:

| | Who computes it | When | Answers |
|---|---|---|---|
| `backup.checksum` | the adapter, per engine | during provision | what was restored |
| `expected_checksum` | the core, §3 | before the sandbox exists | are these the intended bytes |

**They will not be equal, and equality is not a property this design
offers.** For `kind: pgdump` they happen to coincide, because both are
SHA-256 over one file's bytes; for `pgdump_with_globals` they cannot,
because the adapter frames two members and the backup manifest hashes a
tree. A reader who compares them across kinds will find disagreement and
conclude corruption where there is none.

That is a trap, and §9.2 removes it rather than documenting around it.

## 5. When the check runs, and what a mismatch produces

After the configuration is loaded and the evidence log is open, **before
the sandbox is created and before any bytes are transferred**.

A mismatch is `source_corrupt`, outcome **`fail`**, and a **signed record
is written**. That placement is deliberate and is the one thing this
feature cannot compromise on: a drill that ran and left no record is the
highest-severity failure this software has (evidence schema §7), and "the
backup was not what it should be" is precisely the finding an operator
needs in the log rather than only on a terminal. The check sits below the
first row of `drill-config.md` §5.3 in both its halves: the drill is under
way, so everything it finds is recorded rather than merely reported, and
exit code 3 with no record is not available to it.

`fail` and not `error`: the evidence schema's outcome table reads `fail`
as *the drill reached a verdict and the verdict is negative — the backup
or restore is the problem*, and lists `source_corrupt` among its codes,
while `error` says *infrastructure prevented a verdict; says nothing about
the backup* and does not list it. A mismatch is a verdict about the
artifact, reached without a sandbox.

The message names **both values**, in the order a reader needs them:

```
backup manifest mismatch: /backups/pg/nightly.dump is
sha256:3b7daaa5… (4181504 bytes), the backup manifest at
/backups/pg/nightly.manifest.json expects sha256:9f86d081… (4182016 bytes)
```

**Failures of the backup manifest are configuration failures; failures
of the comparison are verdicts about the backup.** The line is worth
drawing exactly, because a verdict writes *the backup is the problem*
into a log that is append-only and read by auditors — and a drill must
never say that about an artifact it never looked at. A backup manifest
the config named and the backup job never wrote is the config's problem.

| Wrong | Code | Outcome |
|---|---|---|
| `source.manifest` names a file that is not there, or cannot be read | `invalid_request` | `error` |
| the file is not JSON, or `schema` is absent or unknown | `invalid_request` | `error` |
| the file carries neither expectation | `invalid_request` | `error` |
| `source.path` is not a regular file or a directory, or a file below it cannot be read | `source_unreadable` | `fail` |
| a directory source holds no regular file | `source_not_found` | `fail` |
| the artifact disagrees with the backup manifest | `source_corrupt` | `fail` |

Both halves are recorded; what differs is what the record says. The
`error` rows behave like §5.3's third row — a configuration mistake
caught once the drill is under way, signed, saying nothing about the
backup. The `fail` rows behave like its fourth.

The first row is the one that moved under that rule, from
`source_not_found` to `invalid_request`. A missing backup manifest
beside a *present* artifact is almost always a configuration naming a
file nothing produces: every drill fails, forever, on a backup that is
fine. Where the backup job instead died before writing either,
`source.path` is missing too and answers first, on its own terms.

## 6. What this does not prove

**It catches corruption, not tampering.** Whoever can rewrite the backup
can rewrite the backup manifest lying beside it. The backup manifest is
an assertion by the same party, in the same place, with the same
permissions — it detects accident, transport loss and mistaken
retention, which is most of what goes wrong, and it detects nothing at
all about an adversary who reached the backup directory.

The tamper-evident form is a different design and is not this one: an
expected checksum written into the **drill config** is covered by
`drill.config_hash`, which is signed into every record — and it does not
scale, because it means one config file per backup. An external attestation
is the witness item on the ROADMAP, not this.

**It proves bytes, not restorability.** A backup manifest that matches
says the artifact is the one the backup tool wrote. Whether that
artifact restores is what the rest of the drill is for, and a matching
backup manifest must never be read as a shortcut past it.

**`created_at` and `engine_version` are claims, not measurements.** They
are recorded in the backup manifest because a backup job has them
cheaply and a reader wants them; the record keeps taking those two facts
from the artifact itself.

## 7. What is deliberately not in it

- **Expected row and table counts.** They need a live query against the
  restored database, which is after everything this file governs. They
  belong to the baseline check, and the ROADMAP places them there.
- **Anything the core would have to interpret per engine.** The core
  knows nothing about pg_dump, WAL or binlogs (AGENTS.md §2.1), and a
  backup manifest field that needed engine knowledge to check would move
  that boundary.
- **A signature.** See §6: a signature by the party that wrote the
  backup, verified with a key kept beside it, adds ceremony and not
  evidence. If the backup manifest is ever signed it will be by a key
  the drill host does not hold, and that is a different item.

## 8. The record

Two nullable fields, arriving with the single `probavi-evidence/3` bump
the ROADMAP gathers four items into — not a bump of their own, and
therefore **not yet shipped**: the check runs and refuses, and the fields
that will describe it in a passing record wait for that bump. What a
refusal records is complete without them, because the code and the
message carry the finding (§5):

| Field | Meaning |
|---|---|
| `backup.manifest_hash` | SHA-256 of the backup manifest's bytes, pinning *which* one was believed, the way `drill.config_hash` pins the configuration. Null when the config named none. |
| `backup.manifest_match` | Whether the artifact agreed with it. Null when the config named none. |

The expectation itself is deliberately absent, and §9.2 is the argument.
Two things follow from its absence, and both are the point.

`manifest_hash` is a measurement the record makes itself, where the
backup manifest's own words are only as trustworthy as §6 allows. It is
also the field that works across records: two drills citing different
`manifest_hash` values for the same backup say the expectation changed,
and say it without either file having to be believed.

`manifest_match` is a field rather than something a reader derives,
because the derivation does not work. `source_corrupt` is also what an
adapter reports when it opens the artifact during provision and finds it
damaged, so a failed record carrying a `manifest_hash` does not say
which of the two happened. A record that makes an auditor reason about
control flow to recover a verdict is not carrying that verdict.

Where the values do belong is a mismatch, and they are already there:
`error.message` names both, with the path of each (§5). That is the one
place a second checksum beside the adapter's informs rather than misleads,
because there the disagreement *is* the finding.

## 9. The two decisions

Both were taken on 2026-09-25, before any code. The alternatives are kept
because a decision without its rejected half is a preference.

### 9.1 No `probavi manifest write`. The shape is documented, and the recipe is a test

The two answers were: **document the shape and let `sha256sum` produce
it** — a backup job appends a few lines of shell, nothing new ships,
nothing new is installed where backups are taken, and the non-goals stay
exactly where they are; or **ship a one-shot `probavi manifest write
<path>`**, which removes the chance of an operator implementing the tree
rule slightly differently and discovering it at 3am.

**What settled it is where the helper would have to run.** A backup
manifest has to be written at backup time, beside the backup, *before*
it is copied anywhere — §1's entire list is damage between the backup
run and the drill. A backup manifest written later on the drill host,
from the artifact that already arrived, attests that copy against itself
and closes nothing. So the helper could not be a drill-host utility on
the pattern of `evidence keygen`; it is Probavi installed on a third
class of host, and that is a distribution decision this feature does not
get to take on its own.

**What the deferral had to pay for is the risk it leaves,** and the risk
is not inconvenience. A hand-written tree hash framed differently from
§3 does not disagree occasionally: it disagrees on every drill, forever,
and each disagreement writes a signed `fail` naming a backup that is
fine — into a log that is append-only, so the false finding cannot be
withdrawn. A feature whose failure mode is a permanent false accusation
has to earn its reproducibility rather than claim it. Hence §3 stating
the framing to the byte, and §3.1 making the published recipe a test that
runs in CI rather than a snippet that looked right when it was written.

**Revisit on a named trigger**, not on a feeling:

- a real backup host without bash, GNU findutils or GNU coreutils, where
  the recipe cannot run and the answer must not be "write your own loop";
- a false `source_corrupt` traced to a hand-written backup manifest,
  which is this decision being wrong in exactly the way it predicted;
- the rule needing to grow past what one shell pipeline can express —
  which would be the sign that §3 had stopped being small, and the helper
  would then be the second problem rather than the first.

### 9.2 The record carries `manifest_hash` and `manifest_match`, not the expectation

§4 leaves a trap: `backup.checksum` and an `expected_checksum` beside it
are computed under different rules and will disagree for several kinds,
with nothing wrong. The two answers were **carry both and document the
trap** — what the ROADMAP's field list assumed — or **carry the hash and
the verdict, and leave the expectation on disk**.

The second, and the reason is narrower than "it is cleaner". **The trap
exists only in a record that passed.** There, and only there, do the two
numbers sit side by side, disagree, and mean nothing by it; a reader who
compares them concludes corruption where there is none. In a record that
failed you want both values — and they are already in `error.message`,
where the disagreement is the finding rather than a decoration. So the
expectation is recorded exactly where it informs and omitted exactly where
it misleads, which is a better outcome than documenting a trap and hoping
§4 is read first. A reader cannot misread what is not there.

Nothing is lost that the record was carrying. The expectation itself stays
on disk, pinned by `manifest_hash`; by §6 it is the same party's assertion
as the backup, so recording its words adds no evidential weight, while
recording *which* assertion was believed does.

**The cost, stated plainly:** this changes a field list the ROADMAP
recorded as accepted, from three fields to two, and the single
`probavi-evidence/3` bump carries the shorter list. That entry is updated
in the same change, because a plan and a specification disagreeing is how
a schema ends up with a field nobody decided to add.

## 10. The contract list

`probavi-manifest/1` is a new contract identifier, and the canonical
list of those is not in this repository. **It was added on 2026-09-25**
(ADR 0047, extending ADR 0035), so the `schema` value this document
mints is governed rather than a version in name only.

Two bounds come with it and belong here, because they constrain code
this repository will write:

- **Governed now, declared when it ships.** The identifier is reserved
  from that decision onward; `docs/capabilities.json` declares the
  contract only when a build implements it. A regeneration that adds it
  before then would be the capabilities manifest claiming something it
  does not ship, which is the one thing it may never do (AGENTS.md
  §5.8).
- **The evidence record is not settled there.** What a record carries
  about a backup manifest — §8 and §9.2 above — is the evidence
  schema's question, and it moves `probavi-evidence/N` by that schema's
  own rule.

## 11. Exit

A backup altered between the backup run and the drill fails **before the
restore**, and the record names the value that disagreed. Met 2026-09-25:
the drill stops at `execute`, before the sandbox provider is asked for
anything, and the signed record carries `source_corrupt` with a message
naming the artifact's digest and the backup manifest's.
