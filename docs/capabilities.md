# Probavi capabilities manifest

`docs/capabilities.json` is the machine-readable statement of what Probavi
can do **in this repository, today**. It is generated from the code that
implements each capability and committed to the tree, so consumers that
vendor this repository — a git submodule, a release tarball — read it
without a build step.

This document is normative for the file's contract. The specifications it
points at (`adapter-protocol.md`, `evidence-schema.md`, `notifications.md`,
`evidence-push.md`, `i18n.md`, `sandbox-bare-host.md`) remain normative for
the things they describe; on any disagreement, those documents win.

---

## 1. What the file guarantees

1. **Only what ships.** Every entry is implemented in this repository and
   exercised by its CI. Planned work lives in `ROADMAP.md` and never
   appears here. Nothing from the commercial layer appears here.

2. **Generated, never hand-written.** A CI gate regenerates the file and
   fails on any difference, so a capability cannot ship without appearing
   here, and an entry cannot outlive its implementation. Do not edit it;
   edits are overwritten.

3. **Verified, not "supported".** `adapters[].verified` lists the engine
   versions and images this repository's integration suite actually
   restores from — the suite reads those images from the same manifests,
   so a listed version is one CI exercises. Render it as *"verified
   against PostgreSQL 16"*. Do **not** render it as a supported-version
   range: this project makes no such claim, and inventing one is exactly
   the drift this file exists to prevent. How versions get on the list,
   and when CI re-earns each of them, is normative in
   [`engine-versions.md`](engine-versions.md).

4. **Deterministic.** No timestamps, no build metadata, no version stamp.
   The bytes change when a capability changes and at no other time.

5. **Every field is always present.** An absent optional value is `null`,
   never an omitted key, so a consumer never has to distinguish "missing"
   from "unknown".

6. **Scope.** The file describes this repository. Third-party adapters and
   sandbox providers live elsewhere and are outside it.

## 2. Versioning

The `schema` field carries `probavi-capabilities/N`, versioned
independently of the binary — exactly as `probavi-adapter/N`,
`probavi-evidence/N`, `probavi-notification/N`, and `probavi-push/N` are.

- Within a version, fields may be **added**, and entries may appear or
  disappear. That is not a breaking change: capabilities change, and
  reporting that is the file's purpose.
- Removing or renaming a field, or changing a field's meaning or the
  vocabulary of its values, requires a **new version**.

Parse defensively: ignore unknown fields, and treat an unexpected `schema`
value as unreadable rather than guessing.

## 3. How to read it

- **`project.status`** is the project's overall maturity. At `pre-alpha`,
  nothing in the file may be presented as production-ready.
- **`status`**, on every entry, is one of `experimental`, `beta`,
  `stable`. While the project is pre-alpha everything is `experimental`;
  `beta` and `stable` exist so the vocabulary is defined before it is
  earned.
- **`non_goals`** is normative in the negative. Those statements must
  never be contradicted, softened, or presented as roadmap items.
- **Ordering** is stable. Adapters are discovered on the filesystem and
  sorted by `id`; the declared registries (sandbox providers, checks, CLI
  commands) keep their documentation order, which carries meaning a
  lexicographic sort would destroy.
- **Paths** (`docs`, `spec`, `schema`) are repository-relative; a trailing
  slash marks a directory. The generator fails if any of them is missing,
  so they are never dead links.
- **`contracts`** carries the four independently versioned contracts.
  `evidence_schema.readable_versions` is every version the verifier
  accepts, which is broader than the one version it writes.
- **`checks[].kind`** is `builtin` (selected with the `builtin` key in
  drill config) or `sql` (the user-defined assertion).
- **`cli.commands[].exit_codes`** is the cron/CI contract. It is the
  authoritative list: it comes from the same table the binary dispatches
  from.

## 4. Where the facts come from

Nothing in the file is written twice. Each fact is read from the registry
that also drives the behavior, so the two cannot disagree:

| Section | Source of truth |
| --- | --- |
| `adapters[]` identity, versions, source kinds, `pitr` | each adapter's `testdata/probe_response.golden`, written from the live probe by its own test |
| `adapters[]` display names, maturity, `verified`, `conformance_verified` | `adapters/<id>/adapter.json`; the integration suite restores from the image it lists, and the conformance suite iterates the adapters that declare it |
| `sandbox_providers[]` | the `Descriptor` in each provider package — the same value the provider resolves drill-config parameters through |
| `checks[]` | `config.CheckKinds()`, the vocabulary gate config validation resolves every check against |
| `cli` | `internal/cli`, the table `cmd/probavi` dispatches from |
| `notifications` | `internal/notify` constants and `config.NotifyOutcomes()` |
| `locales` | the embedded catalogs in `internal/i18n/locales/`, plus the canonical source language |
| `contracts` | the frozen version constants of `internal/adapter`, `internal/evidence`, `internal/notify`, `internal/push` |
| `project`, `non_goals` | declared in `internal/capabilities` — a maturity judgement and a set of things no code implements are the only facts here that no registry can hold |

The generator refuses to produce a file when these disagree: an adapter
whose probe declares a source kind its manifest does not name, a manifest
whose engine version does not appear in the image CI pulls, a `docs` path
that no longer exists, an unknown maturity value — each is a build
failure, not a silently wrong claim.

## 5. Non-goals, restated

These are the entries of `non_goals`, by id, and they are binding on
anything that republishes this file:

- `backup_engine` — Probavi **takes no backups**. It verifies backups
  produced by other tools, which are its foundation, never its
  competitors.
- `scheduler` — Probavi ships **no scheduler** and no daemon of its own.
  Drills run from cron or a systemd timer.
- `database_host_daemon` — Probavi runs **no agent on database hosts**.
- `secrets_management` — Probavi **manages no secrets** beyond reading
  credentials for the duration of one drill.
- `telemetry` — Probavi has **no telemetry** and never phones home.
- `web_ui` — Probavi ships **no web interface**.

The statements themselves live in `internal/capabilities`; a test pins
every id above to that list, so an entry cannot be added or dropped on
one side alone.

## 6. Regenerating

```sh
go generate ./...        # rewrites docs/capabilities.json
```

Run it in the same change as any new adapter, sandbox provider, built-in
check, CLI command or exit code, notification transport, locale catalog,
or contract version bump. CI fails on any diff (AGENTS.md §5.8).

The machine-readable schema is `docs/schemas/capabilities/capabilities.json`,
validated against the committed file on every CI run by `internal/spec`.
