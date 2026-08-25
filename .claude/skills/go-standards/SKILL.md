---
name: go-standards
description: Probavi's Go coding standards, repository conventions, and review checklist. Use this skill for ANY code change in this repository — writing new Go code, refactoring, adding dependencies, writing tests, creating commits or PRs — even small ones. Also use it when deciding where a new package or file should live in the directory layout.
---

# Probavi Go standards

Full standards live in `AGENTS.md` §3 — read that section if you have not in this session. This skill is the working checklist.

## Layout

- `cmd/probavi/` — main package only: flag parsing, wiring, exit codes. No business logic.
- `internal/` — one package per concern. Read the directory rather than a list here; a list drifts, and this one did. `internal/evidence` and `internal/adapter` are the trust core and are held to a higher bar than the rest.
- `adapters/<engine>/` — external adapter processes (separate module allowed).
- New package? Justify why existing ones don't fit; prefer fewer, cohesive packages.

## Code checklist (apply to every change)

- [ ] `gofmt`/`goimports` clean; `golangci-lint run` zero warnings.
- [ ] Errors wrapped with context (`%w`); no swallowed errors; sentinel/typed errors at package boundaries.
- [ ] Every blocking call takes `context.Context` and honors cancellation — drills must be killable.
- [ ] No global mutable state; dependencies injected via constructors; interfaces defined at the consumer.
- [ ] Resource cleanup on ALL paths (`defer` + timeout contexts); for sandbox/Docker resources also label + orphan-sweep.
- [ ] Logs: structured, no secrets, stderr; stdout is reserved (protocol streams, machine-readable output).
- [ ] Table-driven tests for new logic; integration tests behind `//go:build integration`. Tests ship in the same PR as the change — "tests later" does not exist.
- [ ] `go test -race ./...` passes; coverage does not decrease (near-100% for `internal/evidence` and `internal/adapter`).
- [ ] New dependency? One-line justification required; prefer stdlib; pin versions; `govulncheck` clean.
- [ ] Never lower, disable, or bypass a quality gate (linter suppression, skipped test, coverage exclusion) to make something pass — that is design feedback; fix the design.
- [ ] Shipped a new adapter, sandbox provider, built-in check, CLI command or exit code, notification transport, locale catalog, or contract version? Declare it in the registry that drives the code (`config.CheckKinds()`, a sandbox `Descriptor`, `internal/cli`, `adapters/<id>/adapter.json`), then re-run `go generate ./...` and commit `docs/capabilities.json` in the same PR — never hand-edit it (AGENTS.md §5.8).

## Commits and PRs

- Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`), imperative mood, body explains why.
- One vertical slice per PR where possible (config → run → evidence), not horizontal layers.
- Update `ROADMAP.md` checkboxes and keep `README.md` examples honest (mark aspirational ones).
- Specs (`docs/`) change BEFORE code when the adapter protocol or evidence schema is affected — see the adapter-development and evidence-integrity skills, which override this one where they are stricter.
