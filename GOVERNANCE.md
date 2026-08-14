# Governance

Probavi is a small project, and this document does not pretend otherwise.
It writes down how decisions are made today, how that changes as the
project grows, and which commitments stand regardless of who maintains
it.

## Roles

**Maintainer** — currently one: Ákos Fehér
([@aafeher](https://github.com/aafeher)). The maintainer sets direction,
reviews and merges pull requests, and cuts releases.

**Contributors** — anyone with a commit merged under DCO sign-off. The
git history is the ledger; there is no separate one.

## How decisions are made

Decisions are made in public, in this repository. Anything design-shaped
starts as an issue; the maintainer decides after discussion, and the
decision is recorded where it is binding: architecture in
[AGENTS.md](AGENTS.md), normative behaviour in the specs under `docs/`,
capability claims in the generated `docs/capabilities.json`. A decision
recorded only in a chat or an issue comment has not been recorded
(AGENTS.md §5.9) — if it matters, it lands in a file, in the same pull
request that settles it.

Disagreement is welcome and belongs in an issue. The maintainer has the
final word, and is held to the written rules like everyone else: the CI
gates block the maintainer's merges too.

## Standing commitments

These are load-bearing promises users rely on. A future maintainer
inherits them; changing one is not a normal decision but a break with the
project's stated identity, and this section exists partly so that such a
break would be visible:

- **Verification is never paywalled.** The evidence format spec and the
  offline verifier are freely available, forever.
- **The open-core boundary is exhaustive.** The commercial feature list
  in AGENTS.md §6 is closed; everything else in this repository is free
  software, permanently — orchestration scale included.
- **Evidence is append-only.** No code path mutates or deletes existing
  records.
- **No telemetry.** The software never phones home.
- **Contributions stay under Apache-2.0 with DCO.** No CLA, no
  retroactive relicensing of contributed work.

## Adding maintainers

There is no fixed bar, but the pattern is: sustained contributions that
required no lowering of any gate, review comments that showed judgment
about the trust core, and time. The existing maintainer(s) invite; the
invitation and its acceptance happen in a public issue. Once there are
two maintainers, review by another maintainer becomes the norm for
non-trivial changes.

## Continuity

A single maintainer is a real limitation, and it is not spun otherwise.
The mitigations are structural rather than promissory: the license and
the DCO make a community fork legally clean; everything needed to
continue development — architecture, normative specs, the capability
statement, CI — is in this public repository; and evidence logs already
written remain verifiable offline by anyone with the public key and the
spec, with no dependence on this project's continued existence. That
last property is the product's own trust proposition applied to itself.

## Changing this document

By pull request, like everything else. A change to the standing
commitments deserves an issue first and a loud changelog entry.
