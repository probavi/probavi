# Machine-readable schemas

JSON Schema (draft 2020-12) files for Probavi's frozen contracts:

- `adapter/` — every message and payload shape of the adapter protocol
  (`probavi-adapter/0`). Start at `request.json`, `sandbox-call.json`,
  `sandbox-result.json`, and `response.json` for the four wire shapes;
  the `*-request.json` / `*-response.json` files describe the per-operation
  payloads.
- `evidence/record.json` — one evidence record as stored on a log line,
  covering every published schema version (`probavi-evidence/0` and `/1`).
- `notification/payload.json` — the webhook notification body
  (`probavi-notification/1`) receivers parse.
- `manifest/manifest.json` — a backup manifest (`probavi-manifest/1`), the
  file a backup job writes beside its output and a drill checks the
  artifact against before the sandbox is created. Its normative document
  is `../backup-manifest.md`.
- `capabilities/capabilities.json` — the generated capabilities manifest
  (`probavi-capabilities/1`), which states what Probavi can do in this
  repository. Its normative document is `../capabilities.md`; the
  committed `../capabilities.json` is validated against this schema on
  every CI run.

The markdown specifications (`../adapter-protocol.md`,
`../evidence-schema.md`, `../notifications.md`) are **normative**; these
schemas are derived from them, and on any disagreement the markdown wins. Properties the schemas
cannot express — canonical byte form, hash chaining, signatures, framing,
call ordering — live only in the markdown.

CI keeps the schemas honest: `internal/spec` compiles every file and
validates the repository's golden files (evidence logs, adapter probe
responses) plus positive and negative message samples against them on every
run. A schema change that disagrees with the implementation fails the build.

Validate your own adapter's messages with any draft 2020-12 validator, e.g.:

```sh
npx ajv-cli validate --spec=draft2020 -r 'docs/schemas/adapter/*.json' \
  -s docs/schemas/adapter/response.json -d my-response.json
```
