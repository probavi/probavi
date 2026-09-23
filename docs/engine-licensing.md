# Engine licences: who accepts what, and on whose behalf

One group in [the engine catalogue](engine-catalog.md) is headed "Needs a
decision before code", and what it turns on is licensing, registration, or
a sandbox capability we do not have. Every entry under it has been read from
scratch, because this project's position on engine licences survived only
as half a sentence above the list. This document is that position, stated
normatively: whose question a licence is, where an acceptance may live,
what never reaches an evidence record, and when this repository may publish
a claim about an engine it had to agree to something in order to run.

A licence is the one input to a drill that cannot be measured in a
container. Everything else this project claims is earned by a run — a
restore that worked, a check that passed, a version a job exercised. A
licence is read, by a person, on a date, and it changes without telling
anyone. So the rules below govern where a reading is written down and what
may be built on it. None of them enforces a licence, and none could.

This is an engineering position, not legal advice. Where terms bind
anyone, they bind the operator who runs the engine, and reading them is
their decision to make.

---

## 1. Whose question it is

**The operator's, by default.** Probavi ships no engine and bundles none: a
drill starts the image named in the operator's drill configuration, on the
operator's machine, against the operator's backup. Whoever holds a backup
that an Enterprise tool produced already holds the licence that tool ran
under, and an image the operator pulled and named is one they chose to run.

So the software does not ask, gate, warn, or record. It has no standing
to: it cannot read the operator's agreement and cannot tell a development
host from a production one, and it would be wrong about both often enough
to be worse than silent.

The question becomes this project's in exactly two places:

1. **The image refuses to start without an acceptance token.** Something
   has to send it, and the only thing in a position to is the adapter (§2).
2. **This repository's CI has to run the engine** in order to publish a
   verified claim about it (§6).

Everything else belongs to the operator, and most of what follows is about
not getting in their way.

## 2. Naming the image is the acceptance

Where an image demands an acceptance token to start, the adapter sends it
as a constant, and the adapter's README states plainly that configuring
the image is what accepted it.

This is settled rather than proposed. `adapters/mssql` has shipped it since
its first version, for the largest EULA in the catalogue: the adapter puts
`ACCEPT_EULA=Y` in the environment of the exec that starts `sqlservr`, and
[its README](../adapters/mssql/README.md) gives the fact a section of its
own — "By configuring this image you accept Microsoft's EULA for it; the
adapter passes `ACCEPT_EULA=Y` on your behalf when starting the server."

Two requirements come with it.

- **The value is a constant compiled into the adapter, never operator
  input.** An acceptance the operator types is a checkbox, and a checkbox
  implies that something is checking. Nothing is. A constant is honest
  about that: the adapter sends a fixed token because the image needs one
  to boot, and the acceptance happened when the operator wrote the image
  name.
- **The README says it in prose, not in a code comment.** An operator who
  has to read the source to learn what was accepted on their behalf has not
  been told.

## 3. A licence key is a credential, not configuration

Some engines gate their backup or restore path behind a key or a licence
file that the operator obtained by registering. That value is
secret-shaped — per-operator, not publishable, and nothing an evidence
record may carry — which makes it a credential, and credentials already
have a path: `target.source.credential_env` in
[`drill-config.md`](drill-config.md) §3.2. Names live in the drill
configuration, values live in the environment, and
[`evidence-schema.md`](evidence-schema.md) §8 keeps even the *names* out of
the record.

One caveat belongs to whoever ships the first such engine. The field is
documented today as naming the variables an adapter needs "in order to
*read* the backup", and an engine licence key is not that. Widening that
sentence is part of that adapter's pull request — it is a sentence, not a
new key.

Where an engine needs an engine-specific value that is neither a secret nor
an acceptance, the channel exists and the core already ignores it:
`target.options`, "handed to the adapter as the protocol's `options` map,
uninterpreted by the core". It is pinned by `config_hash` and is not
transcribed into the record.

## 4. Four things that never happen

**The core never learns an engine's licence.** [`AGENTS.md`](../AGENTS.md)
§2.1 is both the rule and the reason: an `if engine == …` in `internal/` is
a mistake, and adapters are external processes precisely so that the core
cannot accumulate engine knowledge. A licence rule that differs per engine
is engine knowledge. It belongs in the adapter or in prose, and nowhere
else.

**No drill-configuration key is added for acceptance.** A new config key is
a one-way door (`AGENTS.md` §5.3), and the three channels above already
cover every shape the problem takes. A fourth would be a second way to say
the same thing — per engine, inside the core, and in every operator's
`drill.yaml` from then on.

**No licence assertion reaches an evidence record.** `sandbox.params` is
copied into the signed record verbatim, "values as written", so an
acceptance placed there would put a legal claim into a document written to
be handed to an auditor — a claim the software cannot verify and the
signature would vouch for anyway. A record states what was restored and
what was proven. What the operator agreed to is not its subject, in any
field.

**No warning-and-link gate.** A licence notice printed before a drill
asserts that this project decides whether the operator may run their
engine. It does not and could not. The link would rot besides, and a stale
licence URL in a released binary is worse than none at all. Where terms
matter, they are read, dated and written down.

## 5. A reading is a dated fact, and is recorded like one

A licence reading is a fact about a third party's product on a particular
day — exactly what `supported_until` and `versions_checked` already are,
and recorded the same way: manifest-local, never reaching
`docs/capabilities.json`, because a vendor's terms are not a capability of
this software.

The adapter's `adapter.json` comment carries what the reading changed about
what may be claimed; the adapter's README carries what an operator has to
know. `adapters/oracle` already does both — its manifest records that the
image is "pulled anonymously from its registry (measured: no login, no
licence key)", measured rather than assumed, which is the standard.

A reading answers three questions, and one that answers fewer is not
finished:

1. **May an operator run this image for a restore drill?** Where the terms
   distinguish development from production use, say which a drill is. That
   distinction is the entire question for several entries in the catalogue,
   and it has no obvious answer.
2. **Does the image or the tool demand an acceptance token?** If so, §2
   applies and the README gets its section.
3. **Can this repository's CI run it?** §6.

An undated reading is worthless. Terms change, and a reading with no date
cannot be re-checked — only repeated from scratch, which is the situation
this document exists to end.

## 6. A claim CI cannot repeat is not a claim

[`engine-versions.md`](engine-versions.md) §2 is unconditional: listed
means exercised. Every adapter here carries at least one `verified` entry
that CI restores from, and that is what entitles it to claim anything at
all.

It follows that **an engine whose verification requires a credential this
repository holds cannot be listed.** The version matrix runs on
`pull_request`, and a workflow triggered by a fork's pull request is not
given the repository's secrets. A EULA carried in a secret, a registration
key, a licence file: each would make "listed means exercised" true for
maintainers and quietly false for every contributor, which is the failure
the manifest-drives-the-job design exists to prevent. The only secret any
workflow here holds today is a coverage-upload token, and that is the line.

Two consequences:

- **Verification elsewhere is not a substitute.** There is no claim class
  for a restore that worked on somebody's laptop, and inventing one would
  widen "verified against" into something weaker, which
  [`capabilities.md`](capabilities.md) §1.3 forbids by name.
- **An obligation a drill cannot meet is a reason to refuse.** A sandbox
  runs with no network by default, so an engine whose free terms oblige the
  operator to report usage cannot meet that obligation inside a drill. The
  reading says whether that matters; it is not something to discover
  afterwards.

## 7. Out of reach is written down, by name

Where the answer is no, the engine is named in [the engine
catalogue](engine-catalog.md) with its reason, at its rank, rather than left as a silent gap. IBM Db2 is
recorded there as out of reach on this provider because `--privileged`
stays shut; Greenplum because its open development stopped. Both are
refusals a user can read and argue with.

The alternative — an engine that simply never appears — turns every request
for it into a backlog entry and every answer into a fresh investigation.
That is the cost this document pays down.
