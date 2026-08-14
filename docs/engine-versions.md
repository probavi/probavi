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
   otherwise. End-of-life dates are the vendor's, checked when the entry is
   added and rechecked whenever the list is touched.
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

## 2. Listed means exercised

`adapters/<id>/adapter.json` is the single source. Two mechanisms keep the
claim and the run in step, in both directions:

- The generator refuses a manifest whose `engine_version` does not appear
  in the tag of the image beside it, whose entries repeat a version or an
  image, or which marks anything other than exactly one entry as the
  `baseline`.
- `internal/tools/versionmatrix` turns every entry into one CI job, and
  `TestMatrixCoversEveryClaimedVersion` fails when the jobs and
  `docs/capabilities.json` disagree either way — a claimed version with no
  job, or a job for a version nothing claims.

The integration suites resolve their sandbox image through
`AdapterManifest.SandboxImage`, which accepts `PROBAVI_IT_IMAGE` **only**
when the manifest already lists it. A workflow cannot point the suite at
an arbitrary image and publish the resulting green run.

`baseline` is a fact about this repository's pipeline, not a capability, so
it stays out of `docs/capabilities.json`. A consumer is told which versions
CI restores from; which of them ran on the last push is our business.

## 3. When each version runs

| Trigger | What runs | Why |
| --- | --- | --- |
| Every push and pull request | the baseline version of every adapter | The everyday gate has to stay minutes, not hours, or it stops being a gate people wait for. |
| A pull request touching a manifest, the matrix tool, or the workflow | every listed version | The change that edits a claim is the one that has to prove it, and asking a reviewer to remember that is not a mechanism. |
| Weekly, on schedule | every listed version | Engine images move under us — a new patch release, a changed entrypoint, a dropped tool — and this is what notices. |
| Every `v*` tag, before artifacts are built | every listed version | A release publishes the manifest. The claims in it are re-earned by that commit rather than inherited from whenever the schedule last ran. |
| On demand (`workflow_dispatch`) | every listed version | For the change that adds or removes a version, so it is proven before merge rather than discovered by the schedule. |

The scheduled and release runs use `fail-fast: false`. One version failing
is information about that version; hiding the rest behind it is not.

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
possible today and produces whatever error the engine happens to emit.
Where an adapter can read the version out of the backup itself — pgBackRest
`backup.info`, `xtrabackup_info`, a `.bak` header — it should compare that
to the engine it was handed and refuse with its own message instead. That
work is tracked in `ROADMAP.md`; this document is where the requirement is
recorded.

## 6. Changing the list

A change to `verified` is a normal pull request with three obligations:

1. Nothing to remember: editing a manifest triggers the matrix on the pull
   request itself. A version added without a green job is a claim, not a
   fact, and now it is a red check as well.
2. State the vendor's support window for anything added, and the date it
   was checked.
3. Regenerate `docs/capabilities.json` in the same commit — CI fails on
   drift, and the file is what downstream surfaces read.

Moving the `baseline` is the same change plus one sentence of reasoning:
it decides what every future pull request proves, so it should be the
version most operators run, not the newest one available.
