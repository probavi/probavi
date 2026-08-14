# Contributing to Probavi

Thank you for considering it. This file is the on-ramp; the engineering
rules themselves live in [AGENTS.md](AGENTS.md), which is the single
source of truth for how this repository is built. Read it before writing
code — when this file and that one disagree, that one wins.

## The short version

- Discuss before building: open an issue for anything design-shaped.
- Specs before code: the adapter protocol and the evidence schema are
  normative documents in `docs/`; changing either means a spec PR first.
- Sign your commits off (`git commit -s`) — contributions are accepted
  under the [DCO](https://developercertificate.org/), not a CLA.
- Every change ships with its tests, and every quality gate stays green.

## What helps most right now

- **Feedback on the frozen specs.** The adapter protocol (v0) and the
  evidence schema (v2) are normative and frozen; a design flaw found now
  is worth more than any feature. Open an issue.
- **New engine adapters.** Adapters are external processes speaking a
  small line-delimited JSON protocol — any language, buildable from
  [docs/adapter-protocol.md](docs/adapter-protocol.md) alone. That is the
  point of the protocol.
- **Bug reports with reproductions.** The version or commit, the drill
  config (redacted), and the smallest backup that shows the problem.
- **Translations** of the CLI catalog, under the gates in
  [docs/i18n.md](docs/i18n.md). Machine outputs — evidence records, JSON
  summaries, the protocol, logs — are contracts and stay English.

If you believe you have found a security problem, do not open a public
issue — [SECURITY.md](SECURITY.md) describes private reporting.

## Licensing and sign-off

Probavi is [Apache-2.0](LICENSE) and open-core: everything in this
repository is and stays free software, and the evidence format plus the
verifier are freely available forever — verification is never paywalled.
Nothing contributed here may depend on the commercial layer.

Contributions are accepted under the
[Developer Certificate of Origin 1.1](https://developercertificate.org/).
The sign-off is a one-line statement that you have the right to submit
the work under the project's license; add it with `git commit -s`. There
is no CLA and there will not be one: you keep your copyright.

## Before you write code

Some doors only open one way. Ask first — in an issue — before adding:

- a new dependency (every module in `go.mod` needs a one-line
  justification in the PR),
- a new CLI flag or config key,
- anything touching the evidence format or the adapter protocol — spec
  PR in `docs/` first,
- a new capability of any kind: adapters, sandbox providers, built-in
  checks, notification transports. Capabilities are declared in the
  registries that drive the code, and `docs/capabilities.json` is
  regenerated with `go generate ./...` in the same PR — never hand-edited.

## Development

Standard Go tooling, no Makefile:

```sh
go build ./...
go test -race ./...
go test ./internal/evidence -run TestName   # a single test
go test -tags integration ./...             # integration tests (real Docker)
golangci-lint run                           # zero warnings required
go generate ./...                           # regenerates docs/capabilities.json
```

Every PR runs the full gate set in CI, and a red check blocks merge with
no exceptions: `golangci-lint` with the strict committed config,
`go test -race`, a coverage ratchet (coverage may not decrease, and
`internal/evidence` and `internal/adapter` are held near 100%),
`govulncheck`, integration tests against real Docker, and a check that an
adapter's source cannot change without its `adapterVersion` moving — that
constant reaches every signed evidence record, so two different builds
may never share one.

Tests ship in the same PR as the change they test; "tests later" does not
exist here. Gates are never loosened to make something pass — if a test
is hard to write, that is design feedback.

## Commits and pull requests

- [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`), imperative
  mood, body explains *why*.
- Small vertical slices over broad horizontal layers.
- All code, comments, docs and commits in English — the project's
  canonical source language.
- Update `ROADMAP.md` and `CHANGELOG.md` in the same PR when the change
  warrants it.

## Conduct and governance

The [code of conduct](CODE_OF_CONDUCT.md) applies in every project
space. How decisions are made — and which commitments stand regardless
of who maintains the project — is written down in
[GOVERNANCE.md](GOVERNANCE.md).
