# Backup manifest — design spec

Status: **v2 — NORMATIVE and implemented**, specified and frozen
2026-09-26, implemented 2026-09-27 (`internal/manifest`, the `baseline`
check kind, `§11.1`). v1 was specified and shipped 2026-09-25 —
`target.source.manifest`, `internal/manifest` — and stays exactly as it
was; v2 adds one optional object and changes nothing else (§11). Every
decision this document held open is recorded in §9 with the alternatives
it beat, and in both versions the code followed this document rather than
the other way round (AGENTS.md §5.1). §8's two record fields shipped with
`probavi-evidence/3` on 2026-09-26.

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

The example says `probavi-manifest/1` because that is what a backup job
writes today: `/2` is specified and not yet read by a release, and §2.1
says what changes when it is.

`schema` is required and pins the shape. Everything else is optional,
and a backup manifest carrying no expectation at all is refused as a
configuration mistake rather than accepted as a no-op: a file that
asserts nothing, named by a config that believes in it, is the failure
this whole document exists to remove.

| Field | Required | Meaning |
|---|---|---|
| `schema` | yes | `probavi-manifest/1` or `/2`. A v2 manifest is a v1 manifest plus `baseline` (§2.1); nothing else differs, and a v1 manifest stays valid forever (§12). |
| `expected_checksum` | one of the two | `sha256:<64 lowercase hex>` under the rule in §3 — **not** the adapter's checksum; see §4. |
| `expected_size_bytes` | one of the two | Total bytes under the same rule: a file's size, or the sum of the regular files in a tree. |
| `created_at` | no | RFC 3339. Informational: it is the backup tool's claim, and never populates `backup.created_at`, which is the adapter's measurement of the artifact. |
| `engine_version` | no | Informational. Version compatibility is the adapter's to enforce, from the artifact itself, and several already do. |
| `baseline` | no (v2) | What the backup job counted, per table, for the `baseline` check to reconcile against (§2.1). |

### 2.1 `baseline`: what the backup job counted (v2)

A drill proves a backup restores. It does not, on its own, prove that
**everything** came back — and a restore tool exiting 0 having restored
part of the data is the failure class three engines in this catalogue
have shown.

`row_count` is the instrument that exists for it today, and it is blunt
by construction: the bound is written by hand in the drill config, so a
restore that lands ninety per cent of the rows passes a loose one. The
sharper half of the same question is an expectation **recorded at backup
time by the party that knows it**, which is what this section is.

```json
"baseline": {
  "orders":         {"rows": 100000},
  "public.invoices": {"rows": 42},
  "customers":      {"rows_min": 4980, "rows_max": 5020}
}
```

**Check which versions the build you drill with reads before writing
`/2`.** A version is governed the moment this document specifies it and
read the moment a release implements it, and those were not the same day
here (§11.1). A core that does not know a `schema` value refuses the drill
— that is §2's rule working as intended, not a fault — so a backup job
that adopts v2 ahead of the fleet stops drills rather than degrading
quietly. What a build reads is
`contracts.backup_manifest.readable_versions` in its
`docs/capabilities.json`, which is why that list is published beside the
version rather than only the version.

`baseline` also never stands alone: a v2 manifest still states a checksum
or a size, because refusing the drill **before** the restore is what this
file is for, and an expectation that can only be reconciled afterwards
does not replace it.

Each key names a table the way the drill's engine names one — the same
vocabulary `table` takes elsewhere (`drill-config.md` §3.5), so it is a
collection on MongoDB, a label on Neo4j, a class on Weaviate, a key prefix
on Redis. Each value states either an exact count or a range, and nothing
else:

| Field | Meaning |
|---|---|
| `rows` | The exact number of rows the backup holds. Mutually exclusive with the pair below. |
| `rows_min`, `rows_max` | The narrowest range the backup job can honestly state. Both required together, `rows_min` ≤ `rows_max`, and both ≥ 0. |

A key obeys **the identifier rule a check's `table` obeys**
(`drill-config.md` §3.5): at most two dot-separated segments, each
matching `^[A-Za-z_][A-Za-z0-9_]*$`. The reason is not tidiness — the
reconciliation runs the adapter's declared statement against exactly this
name, so a name the check could never run is refused when the manifest is
read, with the message naming the key, rather than surfacing later as a
reconciliation that mysteriously cannot be performed.

**A range, and deliberately not a tolerance.** The two express the same
uncertainty and differ in who owns it, which is the whole point: a dial in
the drill configuration is turned by whoever wants a green drill, and a
range in the backup manifest is a claim made at the source by the job that
counted. It is also **pinned** — `backup.manifest_hash` reaches every
record (evidence schema §3), so a range that widened between two drills
is visible to anyone reading the log and a dial that widened is not. The
full argument, and what the rejected half was right about, is §9.3.

**Where the numbers come from is the backup job's business, and its
honesty is the whole contract.** A job that can count the backup itself
states `rows`. A job that can only count the live database around the
backup states the range its own uncertainty justifies — the rows written
while the copy ran, say. A job that can state neither honestly states
**nothing**: `baseline` is optional, an absent table is simply not
reconciled, and a baseline nobody can meet is worse than no baseline,
because a check that always fails gets switched off and takes the honest
ones with it.

**What it is not.** It is not a checksum of the data, and it is not a
second copy of it: §8 of the evidence schema forbids a record from
carrying row values, and these are aggregates for the same reason. It
does not catch rows restored with wrong values — only their number. What
catches that is the engine's own corruption detector, which is a
different item on the ROADMAP, and this section must never be described
as doing its work.

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

### 5.1 The `baseline` check: the same file's other half, after the restore

Everything above happens before a sandbox exists. `baseline` (§2.1) is
the part of the backup manifest that cannot: reconciling counts needs a
restored database to count. It is therefore **not** part of the gate — it
is a built-in check, configured like the other four, running where checks
run:

```yaml
checks:
  - builtin: baseline
```

**It takes no parameters, and that is the design.** The manifest is the
list of what to reconcile; naming the tables again in the drill config
would let a table added to the backup job and not to the drill go
silently unreconciled, which is the gap §2.1 exists to close, reopened
one level up. One entry expands to **one result per table the manifest
declares**, named `baseline:<table>` under the derived-name rule
(`drill-config.md` §3.5), so the record shows each table's verdict
separately and a reader can see which one moved.

| Situation | Where it is caught | Result |
|---|---|---|
| `builtin: baseline` with no `source.manifest` | config load | Refused; the config names no file to reconcile against. |
| the backup manifest is `probavi-manifest/1`, or `/2` without `baseline` | §5's read, before the sandbox | `invalid_request`, outcome `error`. A check that would validate nothing is a configuration mistake, not a pass. |
| the count is within the stated bound | the check | Pass, the detail naming the count and the expectation. |
| the count is outside it | the check | False verdict for that table; the other tables and the other checks still run and still report. |
| a table the manifest declares is not in the restored database | the check | False verdict for that table. The reconciliation asked a question the restore could not answer, and that is the finding — not an abandoned drill. |
| the engine has no way to count rows | the adapter | The same limitation `row_count` has, stated in the same place: the adapter's README. `baseline` asks that question and inherits its answer. |
| the manifest declares a baseline and no check reconciles it | nowhere — this is not an error | The drill decides what it proves. Refusing it would break a working drill the day a backup job elsewhere moved to v2, and the backup job is not the same party as the drill. |

The count comes from the adapter's declared `row_count` statement where
there is one, and from the core's composition where there is not (adapter
protocol §6.1.1). **No protocol version moves for this**: the statement
that counts rows already exists — seven adapters declare one, and the
rest are counted by the composition — so `baseline` is a second question
asked of an answer the protocol already has. **No evidence
version moves either**: each expansion is an ordinary check result in
`checks[]`, which the schema has carried since v0.

What the record gains over a hand-written `row_count` is the pairing.
Both write a count and a verdict into the log; only `baseline` also
carries `backup.manifest_hash`, which pins the file the expectation came
from. A bound that was widened to make a drill green is visible in the
log for `baseline` and invisible for `row_count`, and that difference is
the whole reason to prefer it where a backup job can state one.

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

- **Row *values*.** §2.1 carries counts and nothing else. Evidence schema
  §8 forbids a record from carrying row data, and an expectation is
  compared inside the drill and summarised into a record the same way a
  check's detail is — in aggregate only.
- **A tolerance.** §2.1 states a range instead, and says why.
- **Anything the core would have to interpret per engine.** The core
  knows nothing about pg_dump, WAL or binlogs (AGENTS.md §2.1), and a
  backup manifest field that needed engine knowledge to check would move
  that boundary.
- **A signature.** See §6: a signature by the party that wrote the
  backup, verified with a key kept beside it, adds ceremony and not
  evidence. If the backup manifest is ever signed it will be by a key
  the drill host does not hold, and that is a different item.

## 8. The record

Two nullable fields, which arrived with the single `probavi-evidence/3`
bump the ROADMAP gathered four items into rather than with a bump of
their own — **shipped 2026-09-26**, so a passing record now describes
the backup manifest it believed. A refusal was already complete without
them, because the code and the message carry the finding (§5):

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

## 9. The decisions

§9.1 and §9.2 were taken on 2026-09-25, before any code; §9.3 on
2026-09-26, with v2. The alternatives are kept because a decision without
its rejected half is a preference.

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

### 9.3 The backup manifest states a range, the drill config does not state a tolerance

An exact count is not always honest. A backup job that copies a live
database knows roughly how many rows it took and not exactly, and the two
ways to admit that are a **tolerance in the drill configuration** —
`baseline` grows a `tolerance: 1%`, reusing the `row_count` bounds
machinery an operator already understands — or a **range in the backup
manifest**, which is §2.1.

The range, and the argument is about who owns the uncertainty rather than
about shape. Both express the same thing; a tolerance puts it in the file
edited by whoever wants the drill green, and a range puts it in the file
written by the job that counted. Those are not the same party, and the
gap between them is the failure this feature addresses: a hand-written
bound loosened until it passes is how `row_count` stops meaning anything,
and a dial on top of a manifest would import that property into the
manifest's own check.

There is a second difference, and it is the one that decides it for a
trust product. `backup.manifest_hash` reaches every record, so **a range
that widened between two drills is visible in the log**; a tolerance that
widened is not, because the drill configuration's hash covers it but the
before-and-after are two different drills nobody is comparing. The
evidence is the product (§1 of AGENTS.md), and between two designs that
verify the same thing, the one whose loosening leaves a mark in the
append-only log is the one this project ships.

**What the rejected half was right about**, and §2.1 keeps: an
expectation nobody can meet gets switched off and takes the honest checks
with it. The answer is not a dial but an omission — a job that cannot
state a bound honestly states none, and the table is simply not
reconciled.

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
  §5.8). The same rule decided when `/2` appeared there: specified
  2026-09-26, declared 2026-09-27, with the implementation — and it is
  declared as a **list**, because the other party to this contract is a
  backup job outside this repository and the newest version alone would
  read as a requirement to move.
- **The evidence record is not settled there.** What a record carries
  about a backup manifest — §8 and §9.2 above — is the evidence
  schema's question, and it moves `probavi-evidence/N` by that schema's
  own rule.

**`probavi-manifest/2` needs no second decision.** ADR 0047 governs the
identifier, not one version of it — the same way ADR 0035 governs
`probavi-evidence/N` while the evidence schema decides what each N
contains. What a major bump does owe is §11: the shape difference, the
migration, and the freeze list a build has to complete before the
capabilities manifest names it.

## 11. Versioning and migration

The contract identifier is `probavi-manifest/<major>`, and the rule is
the one its siblings carry: any field addition, removal, rename, type
change or semantic change increments the major. The schema is a closed
object and the reader refuses unknown fields, so there is no such thing
as an additive change here — a v1 reader handed a v2 manifest refuses it
by name, which is the behaviour §2 asks for and not a fault to design
around.

| Version | Shape difference | Migration |
|---|---|---|
| `probavi-manifest/1` | v2 without `baseline`. | None. A v1 manifest is valid forever, and a core that reads v2 reads v1 unchanged. |
| `probavi-manifest/2` | Current (§2, §2.1). Read since 2026-09-27. | Write `"schema": "probavi-manifest/2"` and add `baseline`. A backup job with nothing to count stays on v1 rather than writing an empty object: `baseline` is optional in v2, but a job that gains nothing from v2 gains nothing from moving to it. |

**Both versions are read, and only one is refused.** A core accepts either
and acts on what it finds; what it refuses is a `schema` value it does not
know, because the field exists to pin the shape and honouring an unknown
one would defeat it.

### 11.1 v2 freeze

**v2 is frozen as of 2026-09-27** — every item below is complete. Any
further change to this file's shape is a version bump (§11).

- [x] The JSON Schema accepts both versions, with `baseline` constrained
      to what §2.1 states — exact or range, never both, never negative,
      and no other member. Done 2026-09-26, with this specification.
- [x] `internal/manifest` reads a v2 manifest and refuses a malformed
      `baseline` the way it refuses a malformed expectation: as a
      configuration failure, `invalid_request`, because a baseline the
      core cannot read is the operator's file being wrong and not a
      verdict about the backup (§5). Done 2026-09-27, and the tables are
      validated in sorted order, so a file with two problems always names
      the same one first — a message that moved between runs on identical
      input would be a message nobody trusts.
- [x] The `baseline` check kind, to §5.1: no parameters, one result per
      declared table in sorted order, the table of situations in that
      section, and the `drill-config.md` §3.5 entry that goes with it.
      Done 2026-09-27. Neither the adapter protocol nor the evidence
      schema moved, exactly as §5.1 said they would not.
- [x] `docs/capabilities.json` declares the contract only now that a build
      implements it (§10), and publishes
      `contracts.backup_manifest.readable_versions` beside the version.
      Done 2026-09-27: the other party to this contract is a backup job
      outside this repository, and the newest version alone would read as
      a requirement to move.

Two things the build taught rather than the plan. The identifier rule for
a `baseline` key is the same rule a check's `table` obeys, and §2.1 said
so — but the rule lived in `internal/checks`, which reads this package's
output and therefore cannot be read by it. It moved to `internal/config`,
where both reach it, so the two enforcements cannot drift. And the drill
configuration's vocabulary is translated: a fifth built-in changed the
text that lists them, which is itself a catalogue key, so one new check
kind moved 46 translations in 23 languages. That is the cost of the
vocabulary being localised, and it is worth knowing before the sixth.

What v2 deliberately does **not** carry, and why it is not an oversight:
`max(<primary key>)` and `sum(<column>)`, which the ROADMAP names beside
the counts. Both need an aggregate the adapter protocol has no shape for
— `checks` is keyed by built-in check kind and carries one statement
each, so a kind needing three would be a protocol change. The failure
class this item exists for is a restore that landed part of the data, and
that is a count. The rest waits for a demand signal, in its own bump.

## 12. Exit

A backup altered between the backup run and the drill fails **before the
restore**, and the record names the value that disagreed. Met 2026-09-25:
the drill stops at `execute`, before the sandbox provider is asked for
anything, and the signed record carries `source_corrupt` with a message
naming the artifact's digest and the backup manifest's.
