# Writing a Probavi adapter

This guide walks you through building an engine adapter for Probavi in any
language. The protocol specification (`adapter-protocol.md`) is normative;
this document is the practical companion, and `schemas/adapter/` holds
machine-readable JSON Schemas (draft 2020-12) for every message and payload
shape — wire them into your test suite from day one. The in-repo adapters
(`adapters/postgres`, `adapters/mysql`, `adapters/mongodb`,
`adapters/mssql`) were written from the spec alone — standard library
only, zero imports from the Probavi core — as proof that the documents
suffice.

## What an adapter is

An external executable named `probavi-adapter-<name>`, found on `PATH` (or
referenced by explicit path). The core starts one fresh process per
operation, writes a single JSON request on stdin, and expects exactly one
final JSON response on stdout. In between, the adapter may act on the
disposable sandbox — and only through the core: it sends `exec` and
`put_file` requests over the same stdout/stdin channel, and the core
executes them via the sandbox provider. The adapter never talks to Docker
or Kubernetes and never needs network access to the restored database;
engine tools run inside the sandbox image, so tool and server versions
always match.

Four operations, four process invocations:

| Op | Job | Typical body |
|----|-----|--------------|
| `probe` | Declare identity and capabilities. No sandbox, no credentials. | Return a static document. |
| `provision` | Backup source → serving database inside the sandbox. | Wait for engine readiness, transfer the source, restore, measure. |
| `healthcheck` | Is the provisioned instance still serving? | One cheap query via `exec`. |
| `teardown` | Release anything created **outside** the sandbox. | Usually nothing — return immediately. Must be idempotent, must cope with empty state. |

## Ground rules (the ones people trip over)

- **stdout is protocol, stderr is logs.** One stray `print` to stdout and
  the core fails the operation as `adapter_crash`. Redirect engine tools'
  output or capture it.
- **Echo `request_id`** on every message you send.
- **One outstanding sandbox call at a time**; wait for the matching
  `sandbox_result` before the next call or the final response.
- **Exit 0 whenever you wrote a final response** — including error
  responses. Non-zero exit means "I crashed".
- **Map failures onto the §5 error registry honestly.** A missing source is
  `source_not_found`, a source the engine tooling rejects is
  `source_corrupt`, a restore that ran and failed is `restore_failed` —
  and a partial restore is a failure, never a success past ignored errors.
  The registry codes drive the evidence record's recoverability verdict;
  miscoding them poisons the audit trail your users show their auditors.
- **Secrets travel in environment variables only** (`source.credential_env`
  names them; `PROBAVI_SANDBOX_PASSWORD` is the per-drill superuser
  secret). Never put a secret in a protocol message, an error text, or a
  log line.
- **Measure, never estimate.** `engine_ready`, `transfer`, and `restore`
  timings come from monotonic clocks around the real work; they feed the
  evidence record and the RTO trend.
- **Handle SIGTERM**: stop issuing sandbox calls, write a final `cancelled`
  response, exit. The core SIGKILLs after the grace period.
- **Readiness is engine readiness**, not sandbox readiness. Beware init
  sequences that answer probes before the real server is up (PostgreSQL's
  initdb-phase temporary server listens on the unix socket; MySQL's
  first-boot temp server runs with networking off — poll over TCP).

## A workable build order

1. **`probe` first.** Make `probavi adapter probe <name>` print your
   capabilities. Declare a `sql_runner` template so the core can run
   validation checks without learning your engine's dialect — absorb
   dialect quirks declaratively here (the mysql adapter injects an
   `ANSI_QUOTES` init-command so SQL-standard quoted identifiers work).
2. **`provision` for your simplest source kind** — usually a single dump
   file. Get one manual end-to-end drill green.
3. **`healthcheck` and `teardown`**, then the failure paths: missing
   source, corrupt source, sandbox death mid-restore.
4. **Physical restores** (if your engine has them) need an idle sandbox:
   document `command: sleep infinity` and drive the whole engine lifecycle
   yourself. The restored cluster's auth expects credentials the drill
   does not have — overwrite auth locally (the sandbox has no network
   exposure; see the postgres adapter's `pg_hba` rewrite and the mysql
   adapter's `--init-file` reset for the pattern and its rationale).
5. **Run conformance until it passes** (below). Then run a real drill via
   `probavi run` against a real backup.

## Testing patterns that earned their keep

- **Golden-file the probe response.** It is your public capability
  contract; unreviewed drift should fail CI.
- **In-process harness for operations**: drive your op functions through a
  scripted core that answers sandbox calls from a table, and assert the
  full call sequence (`adapters/postgres/adapter_test.go` `driveOp` is the
  reference). This covers restore flows, error mapping, and framing
  without any container.
- **Integration tests against the real engine image** behind a build tag:
  seed a real backup fixture in a throwaway container, then prove the full
  stack restores it and the data answers queries.
- **Checksum discipline**: `source_identity.checksum` is sha256 over the
  source bytes; for multi-file sources define a canonical tree hash and
  document it in your README (both in-repo adapters hash
  `relpath NUL size NUL content` over sorted paths).

## Conformance

```console
$ probavi adapter conformance <name-or-path>
ok   probe.shape
ok   probe.sql_runner
...
{"adapter":"...","passed":17,"failed":0,...}
```

The command drives your adapter through the check list of
`adapter-protocol.md` §10, frozen per protocol version — fifteen for v0 and
two more for v1's optional declarations, which an adapter that declares
none of them passes trivially — framing discipline, handshake behavior, error
registry mapping, timing plausibility, teardown idempotence, SIGTERM
handling — against a simulated sandbox where every command succeeds. No
container runtime is needed, so it belongs in your adapter's CI on every
commit. Checks 8–10 use your first declared source kind by default;
`--source-kind` and repeated `--source-param k=v` select another logical
kind. Exit code 0 means conformant; the JSON report on stdout carries
per-check verdicts for tooling.

Conformance proves protocol discipline, not engine correctness — a real
drill against a real backup remains the definition of working.

## Shipping it

- Name the binary `probavi-adapter-<name>`; `<name>` is lowercase letters,
  digits, and hyphens.
- Write a README documenting: supported source kinds and their `params`,
  required sandbox image contents, environment variables, the checksum
  rule, and any auth-reset behavior inside the sandbox.
- If the image needs an acceptance token to start, or its tooling needs a
  licence key, read [`engine-licensing.md`](engine-licensing.md) first: the
  adapter sends an acceptance as a constant and says so in its README, a
  key travels as a credential, and neither becomes a drill-config key.
- Version your adapter independently (`adapter_version` in probe); declare
  every protocol version you speak in `protocol_versions`.
