# AGENTS.md — Design brief and development instructions

This file is the single source of truth for AI coding agents (and humans) working on Probavi. Read it fully before writing code. When an instruction here conflicts with your general habits, this file wins. When a change would contradict this file, stop and ask the maintainer.

---

## 1. What Probavi is

Probavi is a self-hosted, engine-agnostic **continuous restore verification** platform. It proves — on a schedule, automatically — that database backups are actually restorable, and records each proof as a signed, tamper-evident evidence record with measured restore times.

**The product is the evidence, not the test.** A restore test is a copyable feature; a continuously maintained, cryptographically verifiable, auditor-ready recoverability history is the differentiator. Every design decision should be weighed against this sentence.

Probavi builds **on top of** existing backup tools (pgBackRest, wal-g, Barman, mysqldump, …): they are the foundation, never competitors — see also the non-goals in §2.4.

## 2. Architecture (binding decisions)

```
drill config (YAML)
      │
      ▼
core orchestrator (Go, single binary)      ← scheduling hooks, measurement, lifecycle
      │                    │
      ▼                    ▼
engine adapters      sandbox providers      ← both are PLUGGABLE axes
(external procs,     (docker, k8s job,
 JSON over stdio)     remote host…)
      │                    │
      └────────┬───────────┘
               ▼
evidence store (append-only, hash-chained, ed25519-signed)
               │
               ▼
metrics / notifications / reports / audit export
```

### 2.1 The adapter contract (the most important decision)

- Adapters are **external processes** speaking a small line-delimited JSON protocol on stdin/stdout. Not Go plugins, not compiled-in interfaces. Rationale: any language can implement one (community reach), and the core physically cannot accumulate engine-specific logic.
- Exactly four operations: `probe` (capabilities/versions), `provision` (backup → running DB instance, returns connection info), `healthcheck`, `teardown`.
- Adapters act on the sandbox only through **core-mediated sandbox verbs** (`exec`, `put_file`): during an operation the adapter sends verb requests, the core fulfills them via the sandbox provider. Engine tools run *inside* the sandbox image (tool and server versions always match); adapters never talk to Docker/K8s and never need network access to the restored database.
- The full protocol lives in `docs/adapter-protocol.md`. That document is normative; this section is a summary. Any protocol change requires a version bump there first, code second.
- The core knows nothing about pg_dump, WAL, binlogs, or any engine concept. If you find yourself writing `if engine == "postgres"` in `internal/`, you are making a mistake — move it into the adapter.

### 2.2 The sandbox abstraction

- A sandbox provider answers one request: "give me a disposable runtime with X resources; destroy it afterwards, guaranteed."
- Providers: Docker (`internal/sandbox/docker`) and Kubernetes Job (`internal/sandbox/k8s`), both driving the respective CLI — never an SDK. The docker provider also serves the remote-host deployment via the CLI's native SSH transport (`DOCKER_HOST=ssh://…`; endpoint in the environment only — sandbox params enter evidence records, connection details must not). For targets without any container runtime there is the bare-host provider (`remotehost`, `internal/sandbox/remotehost`): one transient systemd slice + per-drill workspace over the OpenSSH CLI, spec in `docs/sandbox-bare-host.md` (`PROBAVI_SSH_TARGET` in the environment only, same evidence rule; dedicated-host premise required). Same rule as adapters: no provider-specific logic in the core.
- Cleanup is sacred: label every created resource, sweep orphans on startup, always tear down on failure paths (defer + context timeout).

### 2.3 The evidence store

- Append-only JSONL. Each record embeds the SHA-256 of the previous record (hash chain) and is signed with ed25519.
- Canonical serialization is defined in `docs/evidence-schema.md` (normative). Signing happens over the canonical bytes, never over pretty-printed JSON.
- A record captures: timestamp, backup identity (checksum), adapter + version, sandbox + parameters, checks executed with individual results, durations (restore vs. validation separately), outcome, environment fingerprint.
- Verification must be possible offline by a third party with only the public key and the log file. `probavi evidence verify` implements this.
- Never add a code path that mutates or deletes existing records. Retention/compaction, if ever needed, is a spec-level design task, not a quick fix.

### 2.4 Explicit non-goals (enforce actively)

No backup engine. No built-in scheduler (cron/systemd-timer + lock file + timeout is the way). No daemon on database hosts. No UI before Phase 3 of ROADMAP.md. No secrets management beyond reading credentials from env/file for a drill.

## 3. Development standards (mandatory)

**Guiding rule:** always, under all circumstances, apply industry best practices and the strictest coding standards, and strive for the highest achievable conformance to them. Code must be clean, understandable, and well-structured — readability and maintainability are requirements, not aspirations. Quality gates are never lowered, disabled, or bypassed to make something pass; if a gate is inconvenient, that is design feedback. This discipline is maintained continuously throughout development, not added later.

### 3.1 Go

- Latest stable Go; modules; `gofmt` + `goimports` clean at all times.
- `golangci-lint` with a committed `.golangci.yml`; zero warnings policy on main.
- Errors: wrap with `fmt.Errorf("…: %w", err)`; define sentinel errors / typed errors at package boundaries; never discard errors silently.
- Every blocking operation takes a `context.Context` and honors cancellation. Restore runs must be killable.
- No global mutable state. Constructor-injected dependencies. Interfaces defined where they are consumed, not where implemented.
- Table-driven tests; the adapter protocol and evidence chain get golden-file tests. Target: `internal/evidence` and `internal/adapter` near-100% coverage — they are the trust core.
- Integration tests behind a build tag (`//go:build integration`) using real Docker; CI runs them.
- Keep dependencies minimal and boring. Every new module in `go.mod` needs a one-line justification in the PR description.

Quality tooling (all run in CI on every PR; a red check blocks merge, no exceptions):

- `golangci-lint` with a deliberately strict, committed `.golangci.yml` (including `govet`, `staticcheck`, `errcheck`, `gosec`, and complexity/style linters) — not the permissive defaults.
- `go test -race ./...` always; the race detector is non-negotiable for an orchestrator that manages process lifecycles.
- Test coverage measured and enforced in CI: coverage must not decrease; `internal/evidence` and `internal/adapter` hold the near-100% target above.
- `govulncheck` for known-vulnerability scanning of the module graph.
- An adapter's source may not change without its `adapterVersion` moving (`internal/tools/adapterversion`, pull requests only). The constant reaches every signed evidence record as `adapter.version`, so two builds sharing a version leave an auditor unable to tell them apart. A change that provably cannot alter behaviour is exempted with the `adapter-version-exempt` label, where a reviewer sees it.
- Comprehensive testing is maintained continuously: every change ships with its tests in the same PR — unit (table-driven), golden-file for protocol/evidence bytes, integration behind the build tag. "Tests later" does not exist.

Common commands (no Makefile yet — standard Go tooling):

```sh
go build ./...
go test ./...
go test ./internal/evidence -run TestName   # single test
go test -tags integration ./...             # integration tests (real Docker)
golangci-lint run                           # zero warnings required
```

### 3.2 Repository conventions

- Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`); imperative mood; body explains *why*.
- Semantic versioning. Pre-1.0: minor bumps may break; breaking changes always in the changelog. The adapter protocol and evidence schema carry **their own versions**, independent of the binary.
- `CHANGELOG.md` in Keep-a-Changelog format from the first tagged release.
- Specs in `docs/` are normative. Code follows spec; never the reverse without a spec PR first.

### 3.3 Security engineering

- Threat model to keep in mind: the evidence log may be presented to auditors/insurers — assume an attacker wants to forge "everything was fine". Hash chain + signatures defend this; protect key handling accordingly (keys read from file with restrictive perms; never logged; never in config values).
- Restored sandboxes contain **production data**. Defaults: no published ports, isolated network, ephemeral storage, forced destruction. Document the residual risk clearly.
- Credentials for reading backups: env vars or file references only; redact from all logs and evidence records.
- No telemetry/phone-home. Ever. This is a trust product.
- Supply chain: pinned dependency versions, `go.sum` committed, releases built reproducibly with checksums and (later) signed artifacts + SBOM.

## 4. Compliance alignment (DORA, NIS2, NIST)

Purpose: Probavi's reports should let users demonstrate recovery-testing practice under these frameworks. Wording discipline: Probavi **supports demonstrating** compliance; it never "makes you compliant". Do not write marketing or docs copy claiming otherwise, and include a not-legal-advice note wherever these frameworks are referenced. Verify exact article/control numbers against current official texts before citing them in user-facing reports — they may have been amended.

- **EU DORA (Regulation 2022/2554)** — relevant themes: ICT response and recovery; backup policies with restoration and recovery procedures and methods; periodic testing of ICT systems as part of the digital operational resilience testing programme. Probavi's periodic drills + evidence history map naturally onto "we test restoration and can prove it, with measured recovery times".
- **EU NIS2 (Directive 2022/2555)** — relevant theme: cybersecurity risk-management measures covering business continuity, backup management and disaster recovery. Probavi evidences the "backup management and disaster recovery" practice with dated, signed drill records.
- **NIST** — relevant anchors: SP 800-34 (contingency planning: plans must be *tested and exercised*); SP 800-53 control families CP-4 (contingency plan testing), CP-9 (system backup — including testing backups for reliability and integrity), CP-10 (system recovery and reconstitution); CSF 2.0 Recover function. Probavi's automated drills are a direct mechanization of "test backups for reliability and integrity" and produce the documentation these controls expect.

Implementation rule: the Phase 3 audit-report exporter gets a `mappings/` data file per framework (framework → which Probavi evidence fields demonstrate it), reviewed by a human, versioned, and clearly dated — never hardcoded strings in Go.

## 5. How to work in this repo (agent operating instructions)

1. **Specs first.** If a task touches the adapter protocol or evidence schema, edit the doc in `docs/`, get it approved, then code.
2. **Small vertical slices.** Prefer a thin end-to-end path (config → restore → one check → evidence record) over broad horizontal layers.
3. **Ask before adding:** new dependencies, new CLI flags, new config keys, anything touching the evidence format. These are one-way doors.
4. **Never weaken cleanup or signing paths** to make tests pass. If a test is hard to write, that is design feedback.
5. Update ROADMAP.md checkboxes as work completes; keep README examples in sync with reality (mark clearly what is aspirational).
6. Project skills live in `.claude/skills/` — consult them when the task matches their descriptions (adapter work, evidence work, general Go standards, commit messages).
7. Language: all code, comments, docs, commits in English. English is the canonical source language of the project; localization of user-facing CLI output is underway (`docs/i18n.md` is normative, language roadmap in ROADMAP.md Phase 4) and must never change the language of code, specs, logs, or evidence records.
8. **`docs/capabilities.json` is generated — never hand-edit it.** It is the machine-readable statement of what Probavi can do today, and downstream surfaces consume it as their only permitted source of capability claims (the website reads this repository as a submodule and may not claim anything absent from it). Regenerate with `go generate ./...` **in the same PR** as any change to adapters, sandbox providers, built-in checks, CLI commands or exit codes, notification transports, locale catalogs, or a contract version; CI fails on any diff. It records only what ships and works here: no planned entries (those belong in ROADMAP.md), nothing from the commercial layer, and "verified against" is never widened into "supports". Declare a new capability in the registry that also drives the code — `config.CheckKinds()`, the sandbox `Descriptor`s, `internal/cli`, an adapter's `adapter.json` — never in the generator. Consumer contract: `docs/capabilities.md`.

9. **This repository is the canon.** Architecture decisions live in `AGENTS.md`, normative specs in `docs/`, and the machine-readable capability statement in `docs/capabilities.json` — those are the originals, not copies of something kept elsewhere. Downstream surfaces (the website, packaging, the commercial layer) read from here; a decision recorded only in a chat, an issue comment, or a private repository has not been recorded. When work settles a question this file or `docs/` answers, update it in the same pull request.

10. **Nothing non-public may enter this repository.** Material from the private repositories that hold the commercial layer and its working context stays there — code, prose, screenshots, customer names, unreleased plans. This is not only a licensing boundary: everything here is public the moment it is pushed, and it is what an auditor may read alongside the evidence format. If a task appears to need non-public knowledge to finish, stop and ask the maintainer rather than reconstructing it from memory.

## 6. Open questions (do not resolve unilaterally)

- ~~License~~ — **decided 2026-07-31: open-core.** This repository is Apache-2.0 (`LICENSE`); organisational features planned for Phase 3 (fleet dashboard, audit report export, RTO/RPO trends, SSO/RBAC) will be offered commercially and are developed outside this repository. That list is exhaustive: everything else shipped here is free forever, DR game-days (`probavi gameday`) explicitly included — orchestration scale is not a paid tier, and no document may describe it as one. Binding rules: the evidence format spec and the independent verifier are freely available forever — verification is never paywalled, it is the trust proposition; contributions under DCO; nothing in this repository may depend on the commercial layer. The name is decided: **Probavi** (Latin "I have proven"); probavi.dev is the canonical domain.
- Adapter distribution story: separate repos per adapter vs. monorepo `adapters/` (start monorepo, revisit at Phase 2 exit).
- Evidence store backend beyond JSONL (SQLite index for the dashboard?) — decide in Phase 3, keep JSONL as the canonical format regardless.
- ~~PITR drill UX~~ — **decided 2026-08-01:** the drill config gets a `target.pitr` block with exactly one of `target_time` (absolute RFC 3339) or `target_age` (duration; the core resolves it to `now − age` at drill start, so scheduled drills never go stale). The core always hands the adapter an absolute `pitr.target_time` (adapter protocol §6.2); time is the only engine-neutral target — engine-specific coordinates (LSN, GTID, binlog position) stay out of the core schema and would go through `source.params` if ever needed. The resolved target is recorded in evidence as `drill.pitr_target` (evidence schema v1, decided the same day).
