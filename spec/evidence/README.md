# Probavi evidence format — independent verifier

An independent implementation of the Probavi evidence log format, covering
schema versions `probavi-evidence/0`, `/1` and `/2`.

The normative specification is [`docs/evidence-schema.md`](../../docs/evidence-schema.md).
This directory is the *second* implementation of it.

## Why a second implementation exists

Probavi's claim is that a backup was provably restorable on a given date.
That claim is only worth what its verification is worth — and verification
performed by the same code that wrote the record proves very little. It shows
the writer is self-consistent, not that the record means what the
specification says it means.

So this module is deliberately kept at arm's length from the product:

- **It shares no code with the Probavi core.** It is a separate Go module, so
  the language itself refuses any import of `github.com/probavi/probavi/internal/...`.
  The independence is enforced by the toolchain, not by discipline.
- **It was written from the specification document alone**, not by reading
  the core's implementation.
- **It has no dependencies.** Standard library only. A verifier whose supply
  chain an auditor cannot read in an afternoon is not much of a verifier.

If this module and the core ever disagree about a log, either the
specification is ambiguous or one of the two is wrong. Catching that is the
entire point.

## Verifying a log

```sh
go run ./cmd/probavi-evidence-verify \
    --log /var/lib/probavi/evidence.jsonl \
    --key /etc/probavi/signer.pub
```

Or install it as a standalone tool — no Probavi installation required:

```sh
go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@latest
```

This module is versioned and tagged independently of the `probavi` binary,
with its own `spec/evidence/vX.Y.Z` tags. Pin one when the verification
itself has to be reproducible — an audit that records *which* verifier
accepted a log has to be able to name it, and `@latest` moves:

```sh
go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@v0.4.0
```

`--key` is repeatable; pass every public key a log may have been signed
under, so that a log spanning a key rotation verifies end to end.

Exit codes follow the specification's §9:

| Code | Meaning |
|------|---------|
| 0 | `VALID` — every record authentic, complete and in order |
| 1 | `VALID_WITH_DAMAGE` — as above, plus unparseable fragments (a crash artifact, not a forgery) |
| 2 | `INVALID` — the log cannot be trusted; the reason and line number are reported |
| 3 | Usage or I/O error — no verdict was reached |

The result is also written to stdout as one JSON object:

```json
{"status":"VALID","records":3,"damaged_lines":[]}
```

## The worked example

[`docs/schemas/evidence/examples/`](../../docs/schemas/evidence/examples)
holds byte-frozen 3-record logs for both published schema versions and the
public key that signed them. They are the conformance vectors of the format
and the contract surface between the two implementations: the core pins that
it writes exactly those bytes, and this module pins that an implementation
which has never seen the core's code accepts exactly those bytes. Neither
side imports the other, so a drift turns one of the two test suites red.

The signing key is the deterministic test key (seed bytes `0x00`…`0x1f`) and
is published on purpose — anyone can re-derive it and reproduce the logs.
It is a test vector, never an operational key.

```sh
go run ./cmd/probavi-evidence-verify \
    --log ../../docs/schemas/evidence/examples/log_v1.jsonl \
    --key ../../docs/schemas/evidence/examples/signer.pub
```

## Writing a third implementation

Everything needed is in `docs/evidence-schema.md`; §9 states the algorithm in
full. The parts that reward care, all of them places where this module found
the specification worth re-reading:

- **Canonicalization is RFC 8785 (JCS)**, restricted by §4 to integer-only
  numbers. Do not reach for a language's default JSON encoder: Go's escapes
  `<`, `>` and `&`, which no conforming JCS implementation does. Object keys
  sort by UTF-16 code unit, which differs from code-point order above the
  BMP — schema-defined keys are all ASCII, but `sandbox.params` keys come
  from user config.
- **The chain covers the signed line**, including `sig`, so substituting a
  signature also breaks the chain.
- **The signed message omits `sig` entirely** — absent, not null.
- **Damage is not tampering.** An unparseable line is reported and skipped
  without advancing the chain; it can only ever be a crash artifact.

## Known limitation

A valid prefix of a log is itself a valid log: nothing inside the format
proves that records were not removed from the *end*. This is
`TestTailTruncationIsNotDetectable`, and it follows from §1 putting "proving
absence of additional records" out of scope. Pair the chain with an external
note of the expected record count or last sequence number when that matters.

## Licence

Apache-2.0, as the rest of this repository. The evidence format specification
and this verifier are freely available permanently and are never part of a
commercial offering — paywalling verification would destroy the very thing
the evidence is for.
