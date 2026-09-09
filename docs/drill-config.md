# Probavi drill configuration

This document is normative for the drill configuration file — the
`drill.yaml` that `probavi run --config` reads — for the validation it is
held to, and for which of its values reach the adapter, the sandbox, and
the signed evidence record. The file that orders several drills into one
exercise is a different format, specified in
[`docs/gameday.md`](gameday.md).

Status: the format ships since 0.1.0 (2026-08-01); this document is its
specification, written 2026-09-09 from the loader (`internal/config`) and
the pipeline that consumes it. The annotated
[`examples/drill.example.yaml`](../examples/drill.example.yaml) is loaded
by the loader's own test suite, so the example cannot drift from the
schema described here.

---

## 1. Principles

1. **One file is one drill**: one database, one backup source, one
   sandbox, one set of checks, one signed evidence record. Two databases
   are two files — and, when the order between them matters, a game-day.
2. **The core interprets only what it must.** Three maps travel through
   it untouched: `target.source.params` and `target.options` to the
   adapter, `sandbox.params` to the sandbox provider. Engine-specific
   knowledge belongs at the ends of that pipe, never in the middle
   (AGENTS.md §2.1).
3. **Credentials are named here, never written here.** What a drill needs
   to read a backup or authenticate to a restored engine is an
   environment variable *name* in the config; the value is read from the
   environment for the duration of the drill. Config values are the wrong
   place for it in any case: `sandbox.params` are copied verbatim into a
   signed record (§7), and a record is a document written to be shown to
   somebody.
4. **Validation happens before anything exists.** The document is decoded
   strictly and validated in one pass that reports every problem it
   finds, before a sandbox, a key, or an adapter process is touched.
5. **The file is part of the proof.** Its exact bytes are hashed into
   every record it produces (`drill.config_hash`), so a record states
   which configuration ran without embedding its contents.

## 2. The file

Every key this specification defines, in one document:

```yaml
target:
  name: prod-orders-db              # identifies the drill in evidence records
  adapter: postgres                 # resolves to the executable probavi-adapter-postgres
  source:
    kind: pgdump                    # adapter-defined; see `probavi adapter probe postgres`
    path: /backups/orders/latest.dump
    params: {}                      # engine-specific, passed to the adapter uninterpreted
    credential_env: [ORDERS_BACKUP_PASSPHRASE]
  options: {}                       # engine-specific, passed to the adapter uninterpreted
  pitr:                             # optional; exactly one of the two keys
    target_age: "24h"
    # target_time: "2026-07-30T14:32:00Z"

sandbox:
  provider: docker                  # docker | k8s | remotehost
  params:                           # provider-specific, passed through uninterpreted
    image: postgres:16
    memory: 2GiB
  timeout: 30m                      # hard wall clock for the whole drill

checks:                             # at least one
  - builtin: service_healthy
  - builtin: table_exists
    table: orders
  - builtin: row_count
    table: orders
    min: 100000
    max: 50000000
  - builtin: freshness
    table: orders
    column: created_at
    max_age: 24h
  - name: no-negative-totals
    sql: "SELECT count(*) FROM orders WHERE total < 0"
    expect: 0

evidence:
  path: /var/lib/probavi/evidence.jsonl
  sign_key: /etc/probavi/ed25519.key

metrics:                            # optional section
  prometheus_textfile: /var/lib/node_exporter/probavi.prom

notify:                             # optional section (docs/notifications.md)
  webhooks:
    - url_env: PROBAVI_WEBHOOK_URL
      secret_env: PROBAVI_WEBHOOK_SECRET
      on: [fail, error]
    - url: https://alerts.internal.example/probavi
```

Required: `target.name`, `target.adapter`, `target.source.kind`,
`sandbox.provider`, `sandbox.timeout`, at least one entry under `checks`,
`evidence.path`, `evidence.sign_key`. Everything else is optional — but an
optional *section* that is present must be complete (§5.2).

## 3. Key reference

### 3.1 `target`

| Key | Required | Meaning |
|---|---|---|
| `name` | yes | Identity of the drill. Recorded as `drill.name`, and it is the `drill=` label of the metrics (§3.7) and the key the restore trend is computed per. |
| `adapter` | yes | Adapter name, lowercase letters, digits and hyphens. Resolved to the executable `probavi-adapter-<name>` on `PATH` at wiring time. |
| `source` | yes | The backup to restore (§3.2). |
| `options` | no | Engine-specific settings handed to the adapter as the protocol's `options` map, uninterpreted by the core. |
| `pitr` | no | Point-in-time recovery target (§3.3). |

### 3.2 `target.source`

| Key | Required | Meaning |
|---|---|---|
| `kind` | yes | Source kind, defined by the adapter, not by the core. What an adapter accepts is what its `probe` declares — `probavi adapter probe <name>` prints it, and `docs/capabilities.json` states the same for every shipped adapter. |
| `path` | no at load | Location of the backup on the **drill host's** filesystem. The loader does not require it: whether a kind needs one, and what it means, is the adapter's business. |
| `params` | no | Engine-specific settings for this source, handed to the adapter uninterpreted. Where an engine-specific recovery coordinate (LSN, GTID, binlog position) is ever needed, this is where it belongs — not in the core schema. |
| `credential_env` | no | Names of environment variables the adapter needs in order to *read* the backup. Names only; values never enter this file or any protocol message. Each must match `^[A-Za-z_][A-Za-z0-9_]*$`. |

`path` is a host path, and it stays one. The core does not fetch backups:
an artifact that lives in object storage has to be present on the drill
host's filesystem by the time the drill runs (a mount, or a sync step
scheduled ahead of it). The adapter moves it into the sandbox through the
core-mediated `put_file` verb, and the core permits only paths that
*belong* to the configured source — belonging decided by resolving the
path with every symlink component followed, so a symlink inside the source
that leads out of it is refused. The configured path itself is permitted
unresolved, because `/backups/latest` pointing at today's directory is an
ordinary layout (adapter protocol §4.2).

### 3.3 `target.pitr`

Exactly one of:

| Key | Meaning |
|---|---|
| `target_time` | Absolute RFC 3339 instant. It must already have passed; a target more than a minute ahead of this host's clock is a configuration error, not a request (a drill can only prove recovery to an instant that happened, and the grace absorbs ordinary skew between the host that wrote the config and the one running the drill). |
| `target_age` | Duration; the core resolves it to `now − age` at drill start, so a scheduled drill never goes stale. |

Time is the only engine-neutral recovery target the core knows. Whichever
key is written, the adapter is handed one absolute `pitr.target_time`
(adapter protocol §6.2), and the resolved instant — the thing the record
actually proves recoverability to — is stored as `drill.pitr_target`.

The section is sent only to a source kind whose `probe` declares the
`pitr` capability; configuring it against a kind that does not ends the
drill with `unsupported_source` (§5.3).

### 3.4 `sandbox`

| Key | Required | Meaning |
|---|---|---|
| `provider` | yes | Sandbox provider id. The shipped providers and every parameter each one takes — with defaults and isolation properties — are stated in `docs/capabilities.json`; the bare-host provider has its own spec in [`docs/sandbox-bare-host.md`](sandbox-bare-host.md), and Docker deployment notes are in [`docs/docker.md`](docker.md). |
| `params` | provider-dependent | Provider-specific parameters, passed through uninterpreted (an unknown key is the provider's error, not the loader's). **Recorded verbatim in the signed record**, so nothing secret and no connection endpoint may be written here (§7). |
| `timeout` | yes | Hard wall-clock limit for the **whole** drill, not just the restore: the run executes under a context with this deadline, and exceeding it produces a signed record with outcome `error` and code `timeout`. Teardown is deliberately not bound by it — cleanup runs on its own budget after the drill's context is already dead. |

### 3.5 `checks`

At least one check is required — a drill that validates nothing proves
nothing. Checks run in file order; a false verdict does not stop the run,
because the record carries every check individually. Only an
infrastructure failure (the sandbox dying, an unusable identifier)
abandons the remaining checks and ends the drill with outcome `error`.

Each entry sets exactly one of `builtin` or `sql`:

| `builtin` | Parameters | Verdict |
|---|---|---|
| `service_healthy` | none | The adapter's `healthcheck` operation answers healthy. |
| `table_exists` | `table` (required) | The table exists and is queryable. |
| `row_count` | `table` (required), `min`, `max` (at least one, non-negative, `min` ≤ `max`) | The row count lies within the inclusive bounds. |
| `freshness` | `table`, `column`, `max_age` (all required) | The newest value in the timestamp column is younger than `max_age`. |

A check that sets `sql` instead of `builtin` requires `sql` and `expect`,
and takes an optional `name`: it passes when the statement returns a
single scalar equal to `expect`. This is where an invariant the four
shapes above cannot express goes — a business rule, a cross-table
consistency claim, a count that must be zero.

Everything except `service_healthy` runs through the adapter-declared
`sql_runner` — the engine's own client, whatever language it answers to —
so `table` and `column` name things in the engine's terms. They are
validated when the check runs, not at load: each segment must match
`^[A-Za-z_][A-Za-z0-9_]*$`, at most `schema.name`, and both segments are
quoted before use.

`expect` is compared against the runner's textual output, so prefer
numeric results: they print identically across engines, while engine-typed
values do not (PostgreSQL prints booleans `t`/`f`). It accepts a string, a
boolean, or an integer; floats are refused, because their textual
round-trip is ambiguous and an assertion is the last place for that.

Check names in the record are derived, not free text: `service_healthy`,
`table_exists:<table>`, `row_count:<table>`, `freshness:<table>.<column>`,
and `sql:<name>` — or `sql:<index>` when a SQL check has no `name`. That
is why `name` is valid only for SQL checks: the built-ins already have
one.

### 3.6 `evidence`

| Key | Required | Meaning |
|---|---|---|
| `path` | yes | The append-only JSONL log this drill's record is appended to. Created with mode 0600 if absent. The store takes a single-writer lock at `<path>.lock`, so overlapping drills against one log fail fast instead of interleaving. |
| `sign_key` | yes | The ed25519 private key file. It must be a regular file with mode 0600 or stricter — a wider mode is refused, not warned about. Generate with `probavi evidence keygen --out <path>`, which also writes `<path>.pub` for verification. |

Two drills may share a log — the records simply chain in append order,
which reads naturally in an audit — as long as they never run at the same
time; a game-day enforces exactly that rule for its members
(`docs/gameday.md` §2).

### 3.7 `metrics`

| Key | Required | Meaning |
|---|---|---|
| `prometheus_textfile` | yes, when the section is present | Path of a node_exporter textfile collector file, replaced atomically after the record is signed (mode 0644). |

The file carries the last run's headline numbers plus rolling
restore-duration quantiles recomputed from the evidence log itself, every
series labelled `drill="<target.name>"`; the series and two ready-made
alert rules are in the README. Give each drill its own textfile: the file
is replaced whole, so two drills pointed at one path publish only whichever
ran last.

Metrics are observability, not evidence. A failure to compute the trend or
write the file is logged loudly and changes neither the verdict nor the
exit code.

### 3.8 `notify`

| Key | Required | Meaning |
|---|---|---|
| `webhooks` | at least one, when the section is present | Destinations for one JSON POST each, sent after the record is signed. |
| `webhooks[].url` | exactly one of `url` / `url_env` | Literal absolute `http(s)` URL, for non-secret endpoints. |
| `webhooks[].url_env` | exactly one of `url` / `url_env` | Name of an environment variable holding the URL. Token-bearing URLs (Slack, healthchecks.io, …) are credentials and belong here. |
| `webhooks[].secret_env` | no | Name of an environment variable holding the HMAC signing secret. |
| `webhooks[].on` | no | Outcomes to deliver on, from `pass`, `fail`, `error`, `cancelled`; no duplicates. Absent means every outcome — which is what a dead-man's-switch receiver needs. |

The payload contract, delivery semantics, signing and the Slack/email
recipes are specified in [`docs/notifications.md`](notifications.md).
Delivery failures are logged and never change the drill's verdict or exit
code.

## 4. Value types

| Type | Written as | Notes |
|---|---|---|
| duration | quoted or bare Go duration string — `"90s"`, `30m`, `24h` | Must be positive. Used by `sandbox.timeout`, `freshness.max_age`, `pitr.target_age`. |
| instant | RFC 3339 — `"2026-07-30T14:32:00Z"` | Used by `pitr.target_time`. |
| identifier | `orders`, `sales.orders` | Engine identifier for `table` / `column`; see §3.5. |
| environment variable name | `ORDERS_BACKUP_PASSPHRASE` | `^[A-Za-z_][A-Za-z0-9_]*$`. The config carries the name; the environment carries the value. |
| adapter name | `postgres` | `^[a-z0-9][a-z0-9-]*$`; resolves to `probavi-adapter-<name>`. |
| scalar | string, boolean, or integer | `expect` only; floats are refused. |
| string map | `{key: value}` | `source.params`, `target.options`, `sandbox.params` — values are strings. |

## 5. Validation

### 5.1 Decoding

The document is decoded strictly: an unknown field and a duplicate key are
both errors. A typo in a key name therefore fails the drill instead of
silently disabling what the operator thought they had configured — the
failure mode that matters most in a file whose optional sections are
safety equipment.

### 5.2 What the loader checks

Validation collects **every** problem and reports them together, so a
config author fixes a file in one pass rather than one field per run.
Beyond the required keys of §3 it holds: the adapter name pattern;
environment variable name patterns; that `pitr` sets exactly one of its
two keys and that `target_time` parses and has passed; that each check
sets exactly one of `builtin`/`sql`, names a known built-in, carries the
parameters that built-in requires — and carries none that it does not
(`max_age` on a `row_count` check is an error, not a decoration); that a
present `metrics` section names a file; and that a present `notify`
section lists webhooks, each with exactly one of `url`/`url_env`, a
well-formed literal URL, valid environment variable names, and no unknown
or repeated outcome in `on`.

Diagnostics are translated (`docs/i18n.md`); the language comes from
`PROBAVI_LANG`, then `LC_ALL`, `LC_MESSAGES`, `LANG`. Key locators such as
`checks[2]`, and messages produced inside YAML decoding, stay English.

### 5.3 What the loader cannot check, and where it fails instead

Three classes of mistake are invisible to the loader, and they surface at
different points with deliberately different consequences:

| Wrong | Detected | Result |
|---|---|---|
| Unknown adapter, unknown sandbox provider, missing or too-permissive key file, evidence log already locked, `url_env`/`secret_env` unset | Wiring, before the drill starts | Exit code 3 and **no evidence record** — nothing ran, so there is nothing to prove. |
| `source.kind` the adapter does not declare; `pitr` against a kind without the capability | The adapter's `probe`, before a sandbox is created | A signed record with outcome `error` and code `unsupported_source`. |
| Backup absent, unreadable, or rejected by the engine's tooling; a check that fails | The adapter, or the check | A signed record with outcome `fail` (`source_not_found`, `source_unreadable`, `source_corrupt`, `restore_failed`, `check_failed`). |

The middle row is the important one: once the drill is under way, a
configuration mistake is *recorded* rather than merely reported. A drill
that ran and left no record is the highest-severity failure this software
has (evidence schema §7), and a mistyped source kind is not an excuse for
one.

## 6. What the adapter receives

The core forwards these fields and interprets none of them
(adapter protocol §6.2):

| Config | Protocol field |
|---|---|
| `target.source.kind` | `source.kind` |
| `target.source.path` | `source.path` |
| `target.source.params` | `source.params` |
| `target.source.credential_env` | `source.credential_env` (names; the values are in the adapter's environment) |
| `target.options` | `options` |
| `target.pitr` | `pitr.target_time`, always absolute |
| — | `sandbox.scratch_dir`, from the provider |

`target.adapter` selects the process; `sandbox.*` never reaches the
adapter at all — a sandbox is something the adapter acts on through
core-mediated verbs, not something it configures.

## 7. What the evidence record carries

| Config | Record field |
|---|---|
| the file's bytes | `drill.config_hash` |
| `target.name` | `drill.name` |
| `target.pitr` (resolved) | `drill.pitr_target` |
| `target.source.kind` | `backup.kind` |
| `target.adapter` | `adapter.name` |
| `sandbox.provider` | `sandbox.provider` |
| `sandbox.params` | `sandbox.params`, **verbatim** |
| `checks[]` | `checks[].name`, `.ok`, `.detail` |

What is *not* in the record is as much a part of this contract:
`source.path`, `source.params`, `target.options`, and every environment
variable name the file lists. A record states what was restored — the
backup's kind, its checksum, its size, its own creation time — never where
it was fetched from or with what.

Hence the one rule this section exists for: **`sandbox.params` are
published**. An image name and a memory limit belong there; a Docker
endpoint, an SSH target, a password or a token does not. Connection
details for the providers that need them are read from the environment
(`DOCKER_HOST`, `PROBAVI_SSH_TARGET`) precisely because params are
recorded.

## 8. Environment

| Variable | Set by | Used for |
|---|---|---|
| every name in `target.source.credential_env` | the operator | Passed through to the adapter unchanged; the core's allowlist for the adapter's environment is this list plus the baseline (`PATH`, `HOME`, `LANG`, `TZ`) and the variable below. |
| `PROBAVI_SANDBOX_PASSWORD` | the core, per drill | An ephemeral secret the adapter may set as the restored engine's superuser password and reference back as `connection.password_env`. |
| `DOCKER_HOST`, `PROBAVI_SSH_TARGET` | the operator | Provider endpoints, kept out of config because params are recorded (§7). |
| `PROBAVI_LANG` (then `LC_ALL`, `LC_MESSAGES`, `LANG`) | the operator | Language of diagnostics; never of records, logs, or machine output. |

An adapter's `connection.password_env` is honoured only when it names
`PROBAVI_SANDBOX_PASSWORD` or a variable the drill declared in
`credential_env`; anything else is ignored with a warning. The name comes
from the adapter, and an adapter free to name any variable the core
process holds could point a field meant for a database password at a cloud
credential, and have the core hand it to a process the adapter controls.

## 9. What a drill writes

| Path | From | Notes |
|---|---|---|
| `evidence.path` | `evidence` | Appended to, never rewritten; created 0600. |
| `evidence.path` + `.lock` | `evidence` | Single-writer lock, held for the run. |
| `metrics.prometheus_textfile` | `metrics` | Replaced atomically, mode 0644. |

The sandbox itself is disposable by construction and is destroyed on every
path out of the drill, including failure and cancellation.

## 10. Running it

`probavi run --config drill.yaml` executes one drill and prints a one-line
JSON summary on stdout; exit codes are the cron/CI contract (`0` proven
restorable, `1` recoverability failure, `2` infrastructure error or
cancelled, `3` usage or setup error, `5` the record could not be written).
There is no built-in scheduler: cron or a systemd timer owns the cadence,
with a lock file against overlap — the README's "Running on a schedule"
applies verbatim. Several drills that must be exercised in dependency
order are a game-day ([`docs/gameday.md`](gameday.md)), whose members are
ordinary drill files and stay independently runnable.
