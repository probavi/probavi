# Security policy

Probavi exists to produce records an auditor may be asked to believe. The
threat model behind that is written down in [AGENTS.md](AGENTS.md) §3.3:
assume someone wants to forge "everything was fine". Reports that bear on
that assumption are the ones we most want to receive.

## Supported versions

Pre-1.0, fixes land on `main` and ship in the next release. Only the newest
release is supported; there are no backports to earlier tags. Releases are
listed on the [releases page](https://github.com/probavi/probavi/releases)
and every change is in [CHANGELOG.md](CHANGELOG.md).

## Reporting a vulnerability

Use GitHub's private vulnerability reporting — the **Report a vulnerability**
button under
[Security](https://github.com/probavi/probavi/security/advisories/new). It is
enabled on this repository, the report stays private until an advisory is
published, and it keeps the fix and the disclosure in one place.

Please do not open a public issue for something you believe is exploitable.

Useful in a report: what an attacker gains, the smallest reproduction you
have, and the version or commit you saw it on. This is a small project — a
first response should be expected in days rather than hours.

## In scope

These are the failures that would undermine what Probavi claims:

- Anything that lets a forged, altered, reordered or removed evidence record
  survive `probavi evidence verify`, or that makes the hash chain or the
  ed25519 signature accept what it should reject. One case is already
  answered and is not a finding on its own: deleting records from the *end*
  of a log leaves a shorter log that verifies, which
  [docs/evidence-schema.md](docs/evidence-schema.md) §1 and §9 state as the
  chain's documented limit, together with the anchors that bound it. A way
  past any of the rest — including hiding a record without shortening the
  log — is very much a report we want.
- Anything that puts a credential where it does not belong: an evidence
  record, a log line, a notification payload, a report, a process argument
  list.
- Anything that mishandles a signing key — reading one from a file the
  policy should reject, writing one anywhere, logging one.
- Anything that leaves a sandbox, or the production data restored into it,
  alive or reachable after a drill: a teardown path that can be skipped, a
  container or Kubernetes Job left behind, a workspace left on a bare host.
- Anything that lets an adapter, a backup file, or a drill config reach past
  the mediated sandbox verbs to the host running `probavi`.
- Supply chain: a published artifact that does not correspond to the tagged
  source, or a dependency or action pinned to something other than what it
  claims.

## Out of scope

- A drill that fails, or an adapter that misreads a backup. Those are bugs —
  please open an issue.
- Vulnerabilities in the engine images a sandbox runs (`postgres:17`,
  `mysql:8.4`, …). Report those upstream. Do keep in mind that a restored
  sandbox holds production data, which is why the defaults publish no ports
  and destroy the sandbox afterwards.
- A deployment that deliberately relaxes those defaults, or that stores
  backups or keys where anyone can read them. The residual risk of running
  restores over real data is documented, not hidden.

## Disclosure

We will coordinate a disclosure date with you and credit you in the advisory
unless you would rather stay anonymous.
