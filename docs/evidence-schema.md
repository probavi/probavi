# Probavi Evidence Schema — v2

Status: **v2 — approved by the maintainer 2026-08-05. NORMATIVE.** v1 was
frozen 2026-08-01; v2 adds two nullable fields and changes nothing else
(§10). The evidence format is the product's core trust artifact; treat
every field and byte here as a public API. Any change requires a schema
version bump in this document before any code changes. The key words MUST,
MUST NOT, SHOULD, and MAY are to be interpreted as described in RFC 2119.
A machine-readable JSON Schema covering every published version lives at
`docs/schemas/evidence/record.json` (derived from this document; on any
disagreement this document wins).

Schema identifier: `probavi-evidence/2`. Writers emit v2; verifiers MUST
accept every published version — `probavi-evidence/0`, `/1` and `/2`
(§10).

---

## 1. Goals and threat model

- A third party holding only (a) the log file and (b) the signer's public
  key(s) MUST be able to verify **offline** that: every record is authentic,
  the sequence is complete and ordered, and nothing was altered, reordered,
  or removed from *within* it. The qualifier is exact and it is not
  decoration: a record removed from within the sequence leaves a hole the
  chain reports, while records removed from the *end* leave a shorter chain
  that is still perfect. No value a file contains can attest to what was
  deleted past its end; §9 states what does.
- Records are self-describing enough to reconstruct *what was proven*: which
  backup, restored how, checked how, with what results and measured
  durations.
- Assumed attacker: someone with write access to the log file who wants to
  forge "everything was fine" (or hide a failure) after the fact —
  including the operator. Defenses: per-record ed25519 signatures over
  canonical bytes, a SHA-256 hash chain, and an append-only writer.
- Out of scope for v0: confidentiality of the log (it is designed to be
  shareable — see redaction, §8), proving *absence* of additional logs, and
  — from the file alone — proving that the log has not been truncated at
  its end (§9).

## 2. The log

- A log file is UTF-8 JSONL: one record per line, terminated by `\n`.
- Each stored line IS the canonical serialization (§4) of its record —
  byte-for-byte. Pretty-printing, re-ordering, or re-encoding a stored line
  destroys verifiability by construction.
- One file = one hash chain. Multiple drills MAY share a file; their records
  interleave in append order. `seq` is per-file.
- **Append-only, forever.** No code path may mutate, reorder, or delete
  existing bytes. Corrections are new records; retention/compaction, if ever
  needed, is a future spec-level design task.
- Single writer at a time: the writer takes an advisory lock on
  `<path>.lock`, opens the log with `O_APPEND`, writes the full line, and
  fsyncs before releasing the lock.
- Torn tail (crash mid-write): on open, if the file's final bytes are not a
  complete `\n`-terminated line, the writer MUST NOT rewrite or truncate
  them; it appends a single `\n` to close the fragment (a pure append),
  logs a warning, and chains the next record from the last *valid* record.
  Verification (§9) reports such fragments as damage, distinct from
  tampering.

## 3. Record shape

Every record has exactly the fields below (fixed shape per schema version;
unknown/unavailable values are `null`, never omitted). All numbers are
integers (§4). Example — one record, shown pretty-printed for readability
only:

```json
{
  "schema": "probavi-evidence/2",
  "seq": 1042,
  "prev_hash": "sha256:b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c",
  "ts": "2026-07-31T02:00:11.482Z",
  "drill": {
    "name": "prod-orders-db",
    "config_hash": "sha256:7d865e959b2466918c9863afca942d0fb89d7c9ac0c99bafc3749504ded97730",
    "pitr_target": null
  },
  "backup": {
    "kind": "pgdump",
    "checksum": "sha256:9f2a11a6a9e1a76f7e4c62b9b2b0a3f2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6",
    "size_bytes": 565248,
    "created_at": "2026-07-30T01:58:02.000Z"
  },
  "adapter": {
    "name": "postgres",
    "version": "0.1.0",
    "protocol": "probavi-adapter/0",
    "digest": "sha256:05f3b8f6ec13d17858d1b7ec47108f519f2c86d9a013bacb90b44f14577d6795"
  },
  "sandbox": {"provider": "docker", "params": {"image": "postgres:16", "memory": "2GiB"}},
  "timings_ms": {
    "provision": 1170,
    "engine_ready": 1166,
    "transfer": 110,
    "restore": 190,
    "validate": 61,
    "total": 2840
  },
  "checks": [
    {"name": "service_healthy", "ok": true, "detail": "accepting connections"},
    {"name": "row_count:orders", "ok": true, "detail": "100000 rows (min 100000)"}
  ],
  "outcome": "pass",
  "error": null,
  "env": {
    "probavi_version": "0.1.0",
    "os": "linux",
    "arch": "amd64",
    "host_id": "3f7a9c2e5b1d8e04",
    "probavi_digest": "sha256:1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddeeff001"
  },
  "sig": {
    "alg": "ed25519",
    "key_id": "a1b2c3d4e5f60718",
    "sig_b64": "hVb0(…88 base64 chars encoding the 64-byte signature…)Cg=="
  }
}
```

Field reference:

| Field | Type | Nullable | Meaning |
|-------|------|----------|---------|
| `schema` | string | no | Schema identifier, `probavi-evidence/<major>`. |
| `seq` | integer | no | 1-based, strictly consecutive per file. |
| `prev_hash` | string | no | `sha256:<64 lowercase hex>` of the previous stored line (§5); genesis: 64 zeros. |
| `ts` | string | no | Record creation time, RFC 3339 UTC, exactly millisecond precision, `Z` suffix. |
| `drill.name` | string | no | Drill identity from config. |
| `drill.config_hash` | string | no | `sha256:` over the exact drill-config file bytes as read. Proves which config ran without embedding its contents. |
| `drill.pitr_target` | string | yes | The resolved point-in-time recovery target the drill demanded of the adapter (RFC 3339 UTC, ms, `Z`). Null when the drill did not request PITR. The config may express the target relatively (e.g. "24h ago" — pinned by `config_hash`); this field records the absolute instant actually requested, which is what the record proves recoverability *to*. |
| `backup.kind` | string | no | Source kind (adapter-defined, from config). |
| `backup.checksum` | string | yes | `sha256:` over source bytes, from the adapter's `source_identity`. Null if provisioning never got that far. |
| `backup.size_bytes` | integer | yes | Source size. |
| `backup.created_at` | string | yes | Backup's own creation time if derivable (RFC 3339 UTC, ms, `Z`). Normalized by the core from the adapter's `source_identity.created_at`, which may carry any RFC 3339 precision or offset: converted to UTC and truncated — never rounded — to milliseconds (adapter protocol §6.2). |
| `adapter.name` / `.version` / `.protocol` | string | version: yes | Adapter identity; protocol version actually spoken. |
| `adapter.digest` | string | yes | `sha256:` of the adapter executable the core resolved and launched (v2). Build identity, which `adapter.version` is not: the version is a semantic number the adapter reports about itself, so two different builds can share one. Null when the file could not be read — a digest is never worth failing a drill for. **What it attests:** the bytes of the file the core selected at the path `probavi-adapter-<name>` resolved to, hashed before launch. It does not prove those bytes are the instructions that ran: a file replaced between hashing and `exec` would go unnoticed. Closing that window would mean reading `/proc/<pid>/exe`, which does not exist on every platform Probavi supports, so the narrower claim is the one this field makes. |
| `env.probavi_digest` | string | yes | `sha256:` of the `probavi` executable that wrote the record (v2), obtained from the running program's own path. Same rationale and the same attestation limit as `adapter.digest`: the core chooses the sandbox, runs the checks and signs the record, so "which bytes produced this proof" is unanswered without it. Null when the path could not be read. |
| `sandbox.provider` | string | no | Provider name (`docker`, …). |
| `sandbox.params` | object (string→string) | no | Provider parameters from config, values as written. Never tokens/handles. |
| `timings_ms.*` | integer | yes (per phase) | Per-phase durations in milliseconds (§3.1). Phases that never ran are null. |
| `checks[]` | array | no (may be empty) | Executed checks in execution order. |
| `checks[].name` | string | no | Builtin: `<builtin>[:<target>]`; custom SQL: `sql:<user-given name or index>`. Never the SQL text. |
| `checks[].ok` | boolean | no | Verdict. |
| `checks[].detail` | string | yes | Single line, ≤ 256 chars, aggregates only (§8). |
| `outcome` | string | no | `pass` \| `fail` \| `error` \| `cancelled` (§7). |
| `error` | object | yes | Null on `pass`; else `{"code": …, "message": …}` — code from the adapter-protocol registry or a check failure; message redacted, single line, ≤ 512 chars. |
| `env.probavi_version` | string | no | Core version. |
| `env.os` / `env.arch` | string | no | Runtime platform. |
| `env.host_id` | string | no | First 16 hex chars of SHA-256 of the hostname. The raw hostname MUST NOT appear (v0). |
| `sig.alg` | string | no | `ed25519`. |
| `sig.key_id` | string | no | First 16 hex chars of SHA-256 of the 32-byte public key. |
| `sig.sig_b64` | string | no | RFC 4648 base64 (with padding) of the 64-byte signature (§6). |

### 3.1 Timings

All durations are integer milliseconds, converted from measured values by
rounding half away from zero. Sources (see adapter protocol §7):

| Field | Measured by | Covers |
|-------|-------------|--------|
| `provision` | core | Sandbox creation until the runtime is up. |
| `engine_ready` | adapter | Waiting for the engine inside the sandbox to accept connections. |
| `transfer` | adapter | Moving the backup source into the sandbox. |
| `restore` | adapter | The engine restore itself. This is the headline RTO-trend number. |
| `validate` | core | Running all checks. |
| `total` | core | Drill start to verdict (excludes evidence writing). |

## 4. Canonicalization

Canonical serialization is **RFC 8785 (JSON Canonicalization Scheme, JCS)**
with one schema-level restriction:

> **Integer-only numbers.** Every number in a record MUST be an integer `n`
> with `|n| ≤ 2^53 − 1`. Fractional values, exponent notation, `-0`, `NaN`,
> and `Infinity` never occur (durations are integer milliseconds, sizes are
> integer bytes).

Consequences an implementer may rely on:

- JCS number serialization degenerates to plain decimal integers with an
  optional leading `-`; the ES6 shortest-round-trip float algorithm is never
  exercised.
- A serializer that (a) sorts object keys, (b) emits no insignificant
  whitespace, (c) escapes strings per RFC 8785 §3.2.2.2 (minimal escaping),
  and (d) outputs UTF-8 produces byte-identical results to any conforming
  JCS library for valid records.
- Key ordering follows RFC 8785: property names sorted by UTF-16 code
  units. All schema-defined keys are ASCII; `sandbox.params` keys come from
  user config and MUST be compared per the RFC rule.

Writers MUST reject (refuse to sign) any record that violates the integer
restriction, contains invalid UTF-8 in any string (serializers commonly
substitute U+FFFD silently, which would alter content before signing), or
exceeds 64 KiB canonical size. Verifiers MUST check the integer restriction
and the size limit; invalid UTF-8 in stored bytes surfaces as a
canonical-form mismatch.

## 5. Hash chain

- `record_hash(n)` = SHA-256 over the exact stored line bytes of record *n*
  — the canonical serialization **including** `sig` — excluding the
  trailing `\n`.
- `prev_hash` of record *n+1* = `"sha256:" + lowercase_hex(record_hash(n))`,
  where *n* is the previous **valid** record (§2 torn-tail rule).
- Genesis: the first record has `seq = 1` and
  `prev_hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"`.
- `seq` increments by exactly 1 per valid record.

Chaining over the *signed* line (not the pre-signature bytes) makes
signature substitution a chain break as well.

## 6. Signing and keys

- Algorithm: Ed25519 (RFC 8032, "pure" — no pre-hash).
- Signed message: the canonical serialization of the record **with the
  `sig` field removed entirely** (not null — absent).
- `sig.sig_b64`: the 64-byte signature, RFC 4648 standard base64 with
  padding.
- Private key file: the 32-byte seed as 64 lowercase hex chars, single
  line, `\n`-terminated. The writer MUST refuse keys whose file mode allows
  group/other access, MUST never log key material, and MUST NOT accept keys
  from config values or environment variables — file path only.
- Public key file: 64 lowercase hex chars of the 32-byte public key.
  `key_id` = first 16 hex chars of SHA-256 over the 32 raw public-key
  bytes.
- `probavi evidence keygen` (Phase 1 CLI) generates the pair and prints the
  `key_id`.
- Rotation: begin signing with a new key at any time; old records remain
  valid under the old key. Verifiers accept a **keyring** (one or more
  public keys) and select by `key_id`. Re-signing existing records is
  forbidden — there is no honest way to do it.

## 7. Outcomes and failure records

**Every started drill MUST end in exactly one appended, signed record** —
including crashes, timeouts, and cancellations. An early-return path that
skips evidence writing is a bug of the highest severity. The core builds the
record incrementally during the drill; on any abort it fills what it knows,
nulls the rest, and signs.

| `outcome` | Meaning | Typical `error.code` |
|-----------|---------|----------------------|
| `pass` | Restore succeeded and every check passed. The backup is proven restorable. | — (`error` is null) |
| `fail` | The drill reached a verdict and the verdict is negative: the backup or restore is the problem. | `source_not_found`, `source_unreadable`, `source_corrupt`, `restore_failed`, or `check_failed` (one or more checks false) |
| `error` | Infrastructure prevented a verdict; says nothing about the backup. | `sandbox_error`, `adapter_crash`, `timeout`, `engine_not_ready`, `invalid_request`, `internal` |
| `cancelled` | Operator or signal aborted the drill. | `cancelled` |

`check_failed` is defined by this schema (it is not an adapter code). For
reporting: `fail` is a recoverability red flag; `error` is an operational
red flag; both trends matter, and conflating them would poison the audit
story.

### 7.1 Degraded records

A record this schema cannot represent must not become silence. If the core
composes a record the store refuses on shape grounds — an unrepresentable
value, an oversized record — it appends a **degraded record** in its place
rather than ending the drill with nothing written.

A degraded record carries only what the core itself produced: the drill
identity (`drill.name`, `drill.config_hash`, `drill.pitr_target`), the
environment fingerprint, `timings_ms.total`, `outcome: error` and
`error.code: internal` with a message naming both the rejection and the
outcome the drill had reached. Everything supplied by an adapter or copied
from the drill configuration — backup identity, sandbox parameters,
checks, per-phase timings — is dropped: one of those values is why the
record was refused.

It uses no new fields and no new serialization; a verifier needs no
knowledge of it. A reader recognizes one by its message together with
`outcome: error`, `checks: []`, empty `sandbox.params` and null per-phase
timings.

A degraded record is a bug report, not a verdict. It states that a drill
ran and that its result could not be stored — strictly more than the log
would otherwise contain, and strictly less than a proof. It is always
preferable to a lost record and never preferable to a correct one; the
core logs its appearance at error level.

## 8. Redaction rules

A record must be shareable with an external auditor as-is. The following
MUST NOT appear anywhere in a record:

- credential values, key material, or the *names* of credential env vars;
- connection details (hosts, ports, users, connection strings, sandbox
  tokens or handles);
- SQL text of custom checks (the check *name* identifies it; `config_hash`
  pins its definition);
- result rows or any per-row data — `checks[].detail` carries aggregates
  only (counts, ages, latencies);
- the text of an engine's own diagnostics. A failed check records that its
  runner failed and with which exit code, never the message: engines
  routinely quote row data in error text — PostgreSQL answers a violated
  unique constraint with `DETAIL: Key (email)=(…) already exists.` — and a
  record that carries it is no longer shareable as it stands. The
  diagnostic goes to the drill host's log, which is where an operator
  debugs from;
- raw hostnames (§3 `env.host_id`), file paths outside `drill.config_hash`
  scope, or adapter stderr content.

The core passes every adapter-originated string destined for a record
(error messages, check details) through a redactor that masks the values of
all secrets it holds; truncation limits (§3) apply after redaction.

## 9. Verification

`probavi evidence verify --log <file> --key <pub> [--key <pub>…]`
implements exactly this algorithm; independent implementations need nothing
else. That claim is not left as an assertion: `spec/evidence` is a second
implementation written from this document alone (§12).

```text
expected_prev ← "sha256:" + 64×"0"
expected_seq  ← 1
damage        ← []

for each line L (bytes between \n terminators, in file order):
    if L is not parseable as a JSON object:
        damage.append(line_number)            # torn tail fragment (§2)
        continue                              # chain does not advance
    R ← parse(L)
    assert R.schema is a supported version                 else INVALID
    assert canonical(R) == L (byte-for-byte)               else INVALID
    assert every number in R is an integer, |n| ≤ 2^53−1   else INVALID
    assert R.seq == expected_seq                           else INVALID
    assert R.prev_hash == expected_prev                    else INVALID
    K ← keyring[R.sig.key_id]                              else INVALID (unknown key)
    M ← canonical(R without sig)
    assert ed25519_verify(K, M, base64_decode(R.sig.sig_b64)) else INVALID
    expected_prev ← "sha256:" + hex(sha256(L))
    expected_seq  ← expected_seq + 1

result: INVALID on first assertion failure (report line + reason)
        VALID_WITH_DAMAGE if damage nonempty (report damaged lines)
        VALID otherwise
```

Exit codes: `0` VALID, `1` VALID_WITH_DAMAGE, `2` INVALID.

Security note: an unparseable line can only ever be a crash artifact —
signed content cannot be altered or removed this way. Modifying any stored
line fails the canonical-bytes or signature check; deleting a line from
within the sequence breaks `seq`/`prev_hash` continuity; reordering breaks
`prev_hash`. Appending garbage is detected and reported but forges nothing.

**Truncation is the exception, and the list above is incomplete without
it.** Deleting the newest N lines leaves records 1…M whose sequence still
starts at 1 and whose chain is unbroken, so the algorithm returns VALID
with M records and exit code 0. This is not a defect in the algorithm and
no algorithm reading only the file can do better: the chain proves what
the file holds, never what was removed beyond its end. Nor does the
removal leave a trace afterwards — a drill that runs later appends onto
the shortened chain, and the log stays internally valid forever. What an
attacker with write access gains is bounded but real: they can delete the
newest records, and that is exactly enough to hide the drill that just
failed. Hiding an *older* failure means deleting everything after it too,
which is a much larger and more visible gap.

Closing it requires an anchor kept outside the file: the highest `seq`
with the SHA-256 of its stored line, held where the attacker cannot
rewrite it, and compared against what the log now shows. This schema
defines no such anchor, and adding one is a change to this document
first. Until then two properties already in the product bound the
exposure, and both are the operator's to use:

- **A retained copy detects a shrink.** `probavi push` sends the whole
  file on every run (`evidence-push.md` §1.4). A receiver that *keeps*
  what it was sent can compare successive pushes: an append-only log
  never gets shorter, and a shorter log whose records match the prefix of
  an earlier copy has been truncated. A receiver that overwrites its copy
  is no anchor at all.
- **The record count never decreases.** `probavi evidence verify` reports
  `records`. For an append-only log that number is monotonic, so an
  operator who writes it down after each drill — or who simply knows how
  many drills have run — holds the cheapest anchor there is.

Neither is a substitute for a signed head, and neither survives an
attacker who also controls the anchor. They are written down so that the
gap is bounded in the specification rather than discovered during an
audit.

## 10. Versioning and migration

- The schema identifier is `probavi-evidence/<major>`. **Any** field
  addition, removal, rename, type change, or semantic change increments the
  major and adds a migration note here.
- A log file MAY contain records of different schema versions (an upgrade
  happened mid-file); each record is validated against its own declared
  version. Verifiers MUST support every published version.
- Existing records are never rewritten to a new version.

Published versions and migration notes:

| Version | Shape difference | Migration |
|---------|------------------|-----------|
| `probavi-evidence/0` | v1 without `drill.pitr_target`. | None — v0 records lack the field entirely (fixed shape per version) and remain valid forever under v0. Writers emit v1 from 2026-08-01. |
| `probavi-evidence/1` | v2 without `adapter.digest` and `env.probavi_digest`. | None — v1 records lack both fields entirely and remain valid forever under v1. |
| `probavi-evidence/2` | Current (§3). | None — both new fields are nullable, so a writer that cannot read an executable still emits a conforming record, and v1 records lack them entirely. Writers emit v2 from 2026-08-05. |

## 11. v1 freeze

**v1 is frozen as of 2026-08-01** — every item below is complete. Any
further change to this schema is a version bump (§10).

- [x] Machine-readable JSON Schema (`docs/schemas/evidence/record.json`),
      covering both published versions, verified in CI against the
      golden-file tests plus mutation samples (`internal/spec`).
      Done 2026-08-01.
- [x] Worked example: byte-exact 3-record logs for both published versions
      (`docs/schemas/evidence/examples/log_v0.jsonl`, `log_v1.jsonl`) with
      the signer's public key committed alongside (`examples/signer.pub`;
      the key pair is the deterministic test key with seed bytes 0x00…0x1f).
      CI verifies both logs offline with only the committed public key.
      Done 2026-08-01; moved out of `internal/` and published as conformance
      vectors 2026-08-02 (§12).

### 11.2 v2 freeze

**v2 is frozen as of 2026-08-05** — every item below is complete. Any
further change to this schema is a version bump (§10).

- [x] Machine-readable JSON Schema: `recordV2` in
      `docs/schemas/evidence/record.json`, with `internal/spec` proving it
      constrains — required digests, `sha256:` form or null and nothing
      else, and rejection of a v0/v1 record carrying either field.
      Done 2026-08-05.
- [x] The §3 example is a v2 record and CI validates it against the
      schema, so the document and its derived schema cannot drift.
      Done 2026-08-05.
- [x] `spec/evidence` accepts `probavi-evidence/2`, with a v2 record shown
      to verify against the committed public key, a v1→v2 chain shown to
      run straight through, and `probavi-evidence/3` still refused. A test
      pins the verifier's supported set to the versions this schema
      publishes, in both directions — §10 as a gate rather than a promise.
      Done 2026-08-05.
- [x] Worked example: a byte-exact signed `log_v2.jsonl` beside the v0 and
      v1 vectors (§12), verified offline in CI with the committed public
      key — by this repository's writer *and* by the independent verifier,
      which shares no code with it. The vector deliberately carries both
      forms: a record with the digests populated, and an infrastructure
      failure whose `adapter.digest` is null because the adapter never
      resolved. `log_v1.jsonl` joins `log_v0.jsonl` as byte-frozen: it has
      no updater and MUST never be regenerated. Done 2026-08-05.
- [x] The core populates both digests: `adapter.digest` from the
      executable the protocol client resolved, hashed before launch;
      `env.probavi_digest` from the running program's own path. A read
      failure records null and never fails the drill. Done 2026-08-05.

## 12. Independent verification

This section is normative about *availability*, not about the format: it
records a standing commitment, and nothing here changes a byte of §1–§11.

Verification of the evidence format is permanently free and permanently
independent of the Probavi product:

- **The specification is this document**, versioned as
  `probavi-evidence/<major>` on its own cadence, independent of the Probavi
  binary's version. The machine-readable JSON Schema for every published
  version lives at `docs/schemas/evidence/record.json`.
- **The conformance vectors are published**, not internal test fixtures:
  `docs/schemas/evidence/examples/` holds byte-frozen logs for every
  published schema version plus the public key that signed them. The signing
  key is the deterministic test key and is published deliberately, so anyone
  can reproduce the logs. It is a test vector and MUST NOT be used
  operationally.
- **A reference verifier ships as a standalone tool**, `spec/evidence`, a
  separate Go module with no dependencies and no code shared with the
  Probavi core. Being a separate module, it *cannot* import
  `internal/evidence` — the independence is enforced by the Go toolchain
  rather than by convention. It was written from this document alone.

The two implementations are held together by the published examples rather
than by shared code: the core's tests pin that it emits exactly those bytes,
and the verifier's tests pin that an implementation which has never seen the
core accepts exactly those bytes. A divergence turns one of the two suites
red. This is the same technique the adapter protocol uses in `internal/conformance`,
applied to the format that carries the product's actual claim.

A third-party implementation is expected to need nothing beyond this
document; where the reference verifier found the text worth re-reading, the
notes are collected in `spec/evidence/README.md`.

## Changelog

- Editorial (2026-08-29, no format change): §8 names engine diagnostics
  among the things a record must not carry. The rule was already there in
  substance — "result rows or any per-row data" and "adapter stderr
  content" — but the everyday case sat between the two: a check's runner
  fails, and the engine's message quotes the row that caused it. The core
  recorded that message as the check's `detail`; it now records the
  runner's exit code and logs the message on the drill host instead. No
  field, no serialization rule and no shape changes: `checks[].detail` is
  the same string field with the same limits, carrying less.
- v2 frozen (2026-08-05): the core writes v2, the independent verifier
  accepts it, and `log_v2.jsonl` joins the published conformance vectors
  (§11.2). `log_v1.jsonl` is byte-frozen alongside `log_v0.jsonl` — a
  record written under a published version is never regenerated (§10).
  The vector carries both forms of the new fields: populated, and a null
  `adapter.digest` on an infrastructure failure where the adapter never
  resolved. No byte-level change to anything defined earlier the same day.
- v2 (2026-08-05): added `adapter.digest` and `env.probavi_digest` —
  nullable `sha256:` references to the adapter executable the core
  launched and to the `probavi` executable that wrote the record.
  Rationale: a record named the adapter and its semantic version but
  carried no build identity, so two materially different builds could
  produce records claiming the same provenance — indistinguishable to the
  auditor those records exist for. `adapter.version` cannot close this: it
  is a number the adapter reports about itself, and nothing forces it to
  move when the code does (the CI gate added the same week reduces that
  drift but cannot remove it, and says nothing about a third-party
  adapter). Both fields were taken in one version rather than the adapter
  alone: the same argument applies verbatim to the orchestrator, which
  chooses the sandbox, runs the checks and signs the record, and §10 makes
  every field addition a major bump — so deferring the symmetric half
  would have cost a second migration for every verifier. Both are
  nullable, because an unreadable executable must never cost a drill its
  signed record (§7.1 exists for the same reason). The attestation limit
  is stated in §3 rather than implied: the digest covers the file the core
  selected, hashed before launch, not the instructions that ran. Approved
  by the maintainer 2026-08-05. No other shape or byte-level change; v1
  and v0 records remain valid under their own versions (§10).
  Also corrected here: the §3 example declared `probavi-evidence/0` while
  carrying `drill.pitr_target`, a combination the published JSON Schema
  rejects — the example was not moved to v1 when that field was added. It
  now shows a v2 record, and `internal/spec` validates it against the
  schema so the two documents cannot drift again.
- Editorial (2026-08-04, no format change): §7.1 added, specifying the
  degraded record the core appends when the composed one is refused on
  shape grounds. It uses only fields this schema already defines — no new
  field, no serialization rule, nothing a verifier must learn — and closes
  the one path by which the §7 rule ("every started drill MUST end in
  exactly one appended, signed record") could be violated without a bug in
  the store itself.
- Editorial (2026-08-04, no format change): the `backup.created_at` row of
  §3 records where the value comes from and how it is derived — the
  adapter may report any RFC 3339 precision or offset, and the core
  converts to UTC and truncates to milliseconds. The rule itself is
  unchanged; it was simply stated in neither document, which is how an
  adapter emitting a second-precision instant could produce a record this
  schema rejects. Adapter protocol §6.2 carries the matching clause.
- Editorial (2026-08-02, no format change): §12 added, recording that
  verification is permanently free and independently implemented. The worked
  example moved from `internal/evidence/testdata/` to
  `docs/schemas/evidence/examples/` so that it is reachable as a published
  conformance vector — the bytes are unchanged and CI proves it. No schema
  version bump: no field, no serialization rule and no record byte is
  affected.
- v1 (2026-08-01): added `drill.pitr_target` — nullable, the resolved
  absolute point-in-time recovery target of PITR drills. Rationale: a PITR
  drill's compliance claim is "restorable *to instant T*"; without T in the
  signed record the claim would rest on unsigned logs. Approved by the
  maintainer 2026-08-01. No other shape or byte-level change; v0 records
  remain valid under v0 (§10). Addendum (same day, no byte-level change):
  machine-readable JSON Schema added at `docs/schemas/evidence/record.json`
  and the worked-example public key committed — §11 complete, **v1 frozen**.
- v0 (2026-07-31): initial complete draft. Canonicalization decided:
  RFC 8785 JCS restricted to integer-only numbers (maintainer decision
  2026-07-31). Per-phase integer-millisecond timings aligned with adapter
  protocol §7; outcome taxonomy separates recoverability failures from
  infrastructure errors; torn-tail damage semantics defined. Reviewed and
  approved by the maintainer 2026-07-31; normative from this date.
  Editorial clarification (same day, implementation finding): writers MUST
  also reject invalid UTF-8 — common serializers substitute U+FFFD
  silently, which would alter content before signing.
