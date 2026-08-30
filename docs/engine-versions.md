# Engine versions: what Probavi is verified against

`docs/capabilities.json` lists, per adapter, the engine versions this
repository restores from. This document is normative for how that list is
chosen, what it does and does not promise, and when CI has to earn it
again.

It exists because the list is the easiest place in the project to publish
something untrue. A version is one line of JSON; the run behind it is a
container pull, a fixture, a restore and a set of checks. Nothing about
reading the file tells a consumer which of the two happened, so the rule
here is mechanical rather than editorial: the same manifest drives the
claim and the job, and neither can move without the other.

---

## 1. What goes on the list

An engine version is listed when all three hold:

1. **Its vendor still supports it.** A version past end-of-life is not
   something an operator should be running, and verifying it would suggest
   otherwise.

   The date is recorded rather than remembered. Each entry carries
   `supported_until`, the day the engine's vendor stops supporting that
   version, and `TestNoVerifiedVersionIsPastItsSupport` fails the build the
   day one of them closes — on a date, not on a commit, which is how a
   claim like this goes stale. Until 2026-08-27 this rule had nobody to
   enforce it, and two entries had quietly outlived their vendors' windows.

   Only a date the **vendor itself publishes** belongs there. Several
   publish none — their policy is relative ("the last three minor
   releases"), or, in SQLite's case, the opposite of an end date — and
   those entries carry `null`. A null is a statement, not a gap: the
   manifest's `versions_checked` records the day a human last read that
   vendor's page, which is what separates "the vendor publishes no end
   date" from "nobody looked". Aggregators are not vendors: one reported
   Redis 7.2 as unmaintained while Redis's own table gave it until
   2029-12-01, and the vendor was right.
2. **It is a long-term or major supported line**, not a rapid or
   innovation release. Those are superseded within months, and a manifest
   that chases them turns into version bookkeeping that tells a reader
   nothing about durability. Where a vendor's own naming makes a rapid
   release the only current option, the entry says so in the adapter's
   README.
3. **This repository can actually run it in CI.** An engine whose image
   needs a licence key, a registration, a privileged container, or more
   memory than a hosted runner has is not verified here, and must not be
   listed here. If such an engine is otherwise supported, its adapter's
   README states what was tested and where — never `verified`.

Removing a version is a normal change, not an admission: when a vendor
ends support, the entry goes, and the adapter's README records that the
code may still work while nothing in CI proves it any more.

A **variant image** — the same engine shipped differently, such as a
distribution (Percona Server for the mysql adapter) or the engine with a
bundled extension (pgvector for the postgres adapter) — may be listed
under the adapter it is a variant of, under one extra obligation: the
suite must exercise what makes the variant a variant, or the entry says
nothing a plain-engine entry does not already say. The `engine_version`
string carries the variant's own version exactly as the image tag states
it, and the `image` column is what tells a consumer which flavour a row
is; the three rules above apply to the variant's vendor unchanged.

An **engine that ships as a library** rather than as an image has no image
whose tag could carry its version, because the engine is not inside any
image until somebody puts it there: H2 is a jar on Maven Central, and
Derby and HyperSQL are the same shape. Such an entry states the two facts
separately. `image` names the base a drill's sandbox is built from — what
CI pulls — and `engine_artifact` names the engine itself, as the URL the
build fetches, whose path carries the version.

None of the obligations soften, and one of them gets stronger:

- The version must appear in `engine_artifact` exactly as `engine_version`
  spells it, which is the same agreement the image tag provides for every
  other entry.
- Uniqueness moves to whichever field carries the engine. Two entries may
  share a base image — the same JRE hosts every H2 version — but never an
  artifact, because that is the thing being claimed.
- The suite builds its sandbox from that pairing and no other, so a green
  run proves the artifact the manifest names rather than whatever the base
  happened to contain.
- A consumer can fetch exactly what CI fetched, which is more than an
  image tag offers: a tag can be moved, a versioned artifact URL cannot.

Where an engine does ship as an image, listing one remains the rule. This
is for engines that do not, and an entry may not carry both.

## 2. Listed means exercised

`adapters/<id>/adapter.json` is the single source. Two mechanisms keep the
claim and the run in step, in both directions:

- The generator refuses a manifest whose `engine_version` does not appear
  in the tag of the image beside it — or, where the engine ships as a
  library, in the `engine_artifact` beside it — whose entries repeat a
  version or the field that carries the engine, or which marks anything
  other than exactly one entry as the `baseline`.
- `internal/tools/versionmatrix` turns every entry into one CI job, and
  `TestMatrixCoversEveryClaimedVersion` fails when the jobs and
  `docs/capabilities.json` disagree either way — a claimed version with no
  job, or a job for a version nothing claims.

The integration suites resolve their sandbox image through
`AdapterManifest.SandboxImage`, which accepts `PROBAVI_IT_IMAGE` **only**
when the manifest already lists it. A workflow cannot point the suite at
an arbitrary image and publish the resulting green run. A library-shipped
engine resolves the same way and one step further: the matrix job also
names the version it is running, and `AdapterManifest.SandboxEngine`
returns the base and the artifact the manifest pairs with it — so the jar
under test is the manifest's, never the job's.

`baseline` is a fact about this repository's pipeline, not a capability, so
it stays out of `docs/capabilities.json`. A consumer is told which versions
CI restores from; which of them ran on the last push is our business.

## 3. When each version runs

| Trigger | What runs | Why |
| --- | --- | --- |
| A pull request whose every changed file sits under adapter directories, bookkeeping aside | **every version each touched adapter claims** | An adapter is an external process nobody imports, so its change cannot alter another adapter's restore — but it can affect any version of its own. The gate proves exactly what changed, and its clock stays bounded by one adapter's column instead of the whole catalog. |
| Every other pull request | the baseline version of every adapter, one job each | Shared code can affect any restore, and the everyday gate has to stay minutes, not hours, or it stops being a gate people wait for. |
| A pull request touching a manifest, the matrix tool, or the workflow | every listed version | The change that edits a claim is the one that has to prove it, and asking a reviewer to remember that is not a mechanism. This row wins over the two above: a manifest change is adapter-local and still runs everything. |
| Weekly, on schedule | every listed version | Engine images move under us — a new patch release, a changed entrypoint, a dropped tool — and this is what notices. |
| Every `v*` tag, before artifacts are built | every listed version | A release publishes the manifest. The claims in it are re-earned by that commit rather than inherited from whenever the schedule last ran. |
| On demand (`workflow_dispatch`) | every listed version | For the change that adds or removes a version, so it is proven before merge rather than discovered by the schedule. |

*Bookkeeping* means `docs/capabilities.json` and `CHANGELOG.md`, and the
exception is what makes the first row reachable at all. An adapter's
source may not change without its `adapterVersion` moving, moving it
regenerates the capability statement, and a change worth releasing writes
a changelog entry too — so without this, an adapter-local pull request
would carry two non-adapter files and widen every time, leaving the
narrowing to fire for test-only changes alone. Neither file can alter
what a restore job does: the matrix is derived from the manifests, and a
manifest change is already the row below.

The narrowing has **one dimension, and only one**: which adapters run,
never which of their versions. A change to an adapter can break any
engine version that adapter claims, and a variant image — a
`timescale/timescaledb`, a `pgvector/pgvector`, a `postgis/postgis`, a
`percona/percona-server` — cannot be exercised by the baseline at all, which §1 already requires
of every variant entry. Two measured reminders of what version narrowing
would hide: the valkey append-only work failed on 9.0 and 9.1 alone
(`valkey-check-aof` misreads the `VALKEY`-magic base a 9.x rewrite
writes), and the TimescaleDB policy pin lives in a code path the
`postgres` baseline never reaches. A version nobody ran on the pull
request that wrote the code is a version that pull request did not prove.

The decision is made by `internal/tools/versionmatrix -scope`, which
reads the changed paths and answers with the scope; the workflow supplies
the paths and reads the answer back. It lives there rather than in the
workflow's shell because every row of this table is then a test case, and
shell inside a workflow is the one place in this repository no test
reaches.

Every run uses `fail-fast: false`. One version failing is information
about that version; hiding the rest behind it is not.

This workflow is therefore the **adapter half of the everyday gate**, not
an extra on top of it: `ci.yml`'s integration job covers the packages
below `internal/` and `cmd/`, and stops where `adapters/` begins. The two
never restore the same thing twice.

## 4. What the list does not promise

`verified` is a record of what CI ran. It is not a supported-version range,
and rendering it as one is forbidden by
[`capabilities.md`](capabilities.md) §1.3. Two consequences follow, and both
belong in user-facing prose whenever the subject comes up:

- **A version absent from the list is not a version that fails.** Most
  adapters drive vendor tools that are stable across releases; an unlisted
  version very likely works. Nobody here has proven it, which is a
  different statement, and the only one this project is entitled to make.
- **A version on the list is not a promise about your data.** It says a
  fixture restored in a container here. Your backup, your extensions, your
  collation, your storage layout are what your own drills are for — which
  is the whole point of the product.

## 5. Physical restores: the version lock is correctness

A logical dump generally restores into a newer engine. A physical backup
does not: pgBackRest, XtraBackup, `mariadb-backup`, and file-level
snapshots restore into their own major version and nothing else.

For those source kinds the matrix is not test hygiene, it is the only way
the claim means anything — verifying a pgBackRest restore on PostgreSQL 16
says nothing whatsoever about the same adapter on 14, because the format
underneath changed.

The operator picks the sandbox image in the drill config, so a mismatch is
possible. Where an adapter can read the version out of the backup itself,
it compares that to the engine it was handed and refuses with its own
message before the restore is attempted — implemented 2026-08-15 by all
four physical-restore adapters: postgres reads `db-version` from
pgBackRest's `backup.info`, mysql and mariadb read `server_version` from
`xtrabackup_info` / `mariadb_backup_info`, and mssql reads
`SoftwareVersionMajor` from the `RESTORE HEADERONLY` row its selection
already parses. The refusal is `invalid_request`, named for what it is — a
drill config pairing a backup with a sandbox that cannot restore it — and
where the adapter reads the backup host-side it happens before a byte is
transferred.

The RDB engines carry the same check from the artifact's own header: an
RDB names the server that saved it (`redis-ver`; `valkey-ver` for Valkey,
which has written its own field and never `redis-ver` since the fork —
measured), and the redis and valkey adapters compare that against the
sandbox's `--version` before anything is transferred. The fork adds a
stricter cousin with the same shape: above format version 11 the Redis
and Valkey RDB dialects diverge and neither engine loads the other's
files, so each adapter refuses the other dialect's artifact on positive
header evidence (`unsupported_source`, pointing at the right adapter) —
timely precisely because the in-sandbox integrity tool passes a post-fork
file of the other dialect that the server then refuses at startup
(measured).

Two rules bound the check. It refuses only on positive evidence: an
unreadable or encrypted manifest, a missing `server_version`, a header row
that stops short, or an engine that cannot answer skips the check, and the
restore then speaks for itself. And it encodes each engine's real rule
rather than one slogan: same major for PostgreSQL, same release series for
MySQL and MariaDB, and for SQL Server and the RDB engines only the
downgrade — restoring an older backup onto a newer engine is the supported
upgrade path and passes.

## 6. Changing the list

A change to `verified` is a normal pull request with three obligations:

1. Nothing to remember: editing a manifest triggers the matrix on the pull
   request itself. A version added without a green job is a claim, not a
   fact, and now it is a red check as well.
2. Record the vendor's support window in `supported_until`, from the
   vendor's own page, and move `versions_checked` to the day you read it.
   Both are manifest-local: a third party's support calendar is a fact
   about their product, not a capability of this one, so neither reaches
   `docs/capabilities.json`.
3. Regenerate `docs/capabilities.json` in the same commit — CI fails on
   drift, and the file is what downstream surfaces read.

Moving the `baseline` is the same change plus one sentence of reasoning:
it decides what every future pull request proves, so it should be the
version most operators run, not the newest one available.
