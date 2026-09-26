# Sandbox providers: what a provider must guarantee

`docs/sandbox-bare-host.md` is one provider's design spec. This document
is the general one: the rules every sandbox provider satisfies, gathered
from the five places they were spread across — the adapter protocol's §4,
`internal/sandbox/descriptor.go`, the `Provider` and `Sandbox` interfaces
in `internal/core`, `AGENTS.md` §2.2, and the three providers that ship.

It exists because a fourth provider has nothing general to be written
against. The bare-host provider was required to be spec-first and got a
spec of its own; the next one would either repeat that document's
provider-specific reasoning or invent the general rules again, and the
second is how a guarantee quietly changes.

The asymmetry with the other pluggable axis is the thing to hold on to.
**An adapter is an external process anyone can write, own and version; a
sandbox provider is core Go, for the life of the project.** The three that
ship measure 446, 587 and 608 lines against 895, 972 and 958 lines of unit
tests, an integration suite each, and a dedicated setup block in the
pull-request workflow. That is the cost a fourth inherits before it
restores anything, and it is why the bar below is where it is.

---

## 1. What a sandbox is, and what the core asks of it

One disposable runtime, for one drill, destroyed afterwards — guaranteed.
The core asks for exactly that and nothing more. Two interfaces say so,
and they are deliberately small:

```go
type Provider interface {
    Create(ctx context.Context, params map[string]string) (Sandbox, error)
    SweepOrphans(ctx context.Context) ([]string, error)
}

type Sandbox interface {
    adapter.SandboxVerbs           // Exec and PutFile
    ID() string
    ScratchDir() string
    Destroy(ctx context.Context) error
}
```

What is **not** there is the point. The core never asks a provider what
engine it is running, what a backup is, what a restore means, or whether
the drill passed. A provider that needs to know any of those is the wrong
shape, and the same rule that governs adapters applies here: engine
knowledge in `internal/` is a mistake (`AGENTS.md` §2.1).

"Pluggable" means the core never learns which runtime it got. It does not
mean a provider can live outside this repository: `cmd/probavi/run.go`
resolves a configured provider name through a switch over concrete
constructors, and `internal/sandbox/registry` holds the descriptions the
CLI and the capabilities generator both read. A provider is chosen by
name from a fixed list.

## 2. The two verbs

A sandbox implements `exec` and `put_file`, defined normatively in
[`adapter-protocol.md`](adapter-protocol.md) §4. A provider owns the
mechanics; the core owns the rules.

**`exec`** runs `argv` directly, with no shell. The provider captures
stdout and stderr, **each capped at 256 KiB before encoding**, and reports
`truncated` when either capture hit the cap. A non-zero exit code is not a
failed call — it is a result the adapter inspects — so a provider must
report the code rather than turning it into an error. The core enforces
`timeout_seconds` and measures `duration_seconds`; a provider that
measures its own would be reporting a different number into a signed
record.

**`put_file`** copies one host file into the sandbox. **The containment
rule is the core's, not the provider's**: only source paths belonging to
the drill's configured backup source are permitted, and belonging is
decided by resolving the path with every symlink component followed —
never by comparing strings. A provider therefore receives a path the core
has already vetted, and must not widen it: no path expansion of its own,
no globbing, no following a component the core did not resolve.

One residual is stated rather than implied, and it belongs to both sides:
the core resolves the path and the provider then opens it again, so a
process able to write inside the backup source could swap a component
between the two calls. It is bounded by what such a process can do anyway
— the backup being restored is in that same tree — and closing it means
handing the open file to the provider instead of its path.

New verbs require a protocol version bump. A provider may not invent a
side channel to make one unnecessary.

## 3. Cleanup is the load-bearing promise

A sandbox holds a restored copy of **production data**. Everything else in
this document is secondary to it going away.

Three mechanisms, and a provider states each one in its descriptor:

1. **Forced teardown on every path.** `ForcedTeardown` is true for all
   three shipped providers, and a new one that cannot say the same is not
   ready. Destruction runs on failure paths too — `defer` plus a context
   timeout — because the paths where a drill fails are exactly the paths
   where data is most likely to be left behind.
2. **An orphan sweep.** A drill host that crashes leaves a sandbox nobody
   will destroy. `SweepOrphans` reclaims them on startup, and a provider
   must be able to recognise its own leftovers without mistaking a
   concurrent drill's sandbox for one: label what you create, and scope
   the sweep by owner.
3. **An external backstop, where one exists.** `ExternalBackstop`
   describes cleanup that survives the drill host dying outright. Not
   every runtime offers one; a provider that has none says so rather than
   leaving the field to be read as an oversight.

`Destroy` is idempotent. It is called on a sandbox that may be partially
created, already gone, or in a state the provider did not expect, and
"already destroyed" is a success.

## 4. Isolation, and the defaults other decisions lean on

`Isolation` in the descriptor states what an operator must know before
pointing a provider at restored production data. Two of its fields are not
negotiable.

**No published ports. Ever.** `PublishedPorts` is false for every
provider, by design (`AGENTS.md` §3.3), and it is not a default an
operator may override — the parameter to do it does not exist. Several
adapters lean on this: SQL Server's documented public `sa` constant,
PostgreSQL's `pg_hba` trust overwrite, MySQL's empty root password are all
acceptable **only** because nothing outside the sandbox can reach the
engine. A provider that could publish a port would invalidate those
adapters rather than merely relax itself.

**A stated network default.** `NetworkDefault` is the network the sandbox
joins unless configured otherwise — `none` for the docker provider. A
provider that does not control networking says so; the k8s provider has no
pod-level equivalent of `--network none`, and that gap is documented
rather than papered over, which is part of why it is still `experimental`.

`Storage` describes where restored data lives and how it goes away, in the
operator's terms — "container filesystem and anonymous volumes, removed
with the container", "pod filesystem, deleted with the Job", "per-drill
workspace under the workspace root, mode 0700, deleted (not shredded) at
teardown". Note the last one says *deleted, not shredded*: a residual
stated plainly is worth more than a guarantee nobody can keep.

## 5. The descriptor is the only place a parameter may be declared

Every provider resolves each configured parameter through
`Descriptor.Lookup` and **rejects what the descriptor does not list**, so
a parameter cannot be honoured without being declared. That is what lets
the generated capabilities manifest state the parameter surface and the
isolation properties without a second, hand-maintained list — the same
manifest-drives-the-claim design the adapters use.

Two consequences bind a new provider.

**Undeclared is unsupported.** A provider that quietly honours a key its
descriptor does not carry has published a capability outside the manifest,
which is exactly the drift `docs/capabilities.md` exists to prevent.

**An endpoint is not a parameter.** `sandbox.params` is copied into the
signed evidence record **verbatim and unfiltered** — the redactor covers
adapter-originated strings, and nothing inspects a parameter value. The
rule holds today because no descriptor declares an endpoint: the docker
daemon comes from `DOCKER_HOST`, the cluster from `KUBECONFIG`, the bare
host from `PROBAVI_SSH_TARGET`, all environment-only. Most candidate
providers address a remote system by name, so each of them meets this
first: either the endpoint stays in the environment as it does for all
three, or the descriptor gate learns to refuse a parameter that carries
one. A provider that declared `host` or `endpoint` would compile,
validate, and sign it.

Credentials are not parameters either, for the same reason and more
sharply: no provider accepts a secret through config.

## 6. What a record may say about a sandbox

`sandbox.params` records the values **as written in the drill
configuration**. A record therefore states what was *requested*, never
what ran.

### 6.1 What a provider must answer about what it ran

`probavi-evidence/3` closes half of that gap, and it does so by asking
providers a question they were not asked before. A provider MUST be able
to report, after a sandbox exists:

| What | Reported as | When it is null |
|---|---|---|
| The engine image actually run | `sha256:<64 lowercase hex>` | There is no image (a bare host), or the runtime cannot be asked for one |
| The memory limit actually applied | bytes | The provider cannot read back what it set, or nothing was applied |
| The CPU limit actually applied | thousandths of a CPU | The same |

Three rules hold this honest, and each exists because the alternative
would be worse than the missing value.

**A provider reports what it read back, never what it was asked for.**
Echoing the request would turn `sandbox.params` into evidence by copying
it under a different name, which is the exact confusion this section
opens by warning about. A provider that cannot read back a limit reports
null.

**Null is always an acceptable answer, and never fails a drill.** A
Kubernetes Job without limits takes what the node has; a rootless runtime
may accept a cap and drop it; a bare host has no image at all. A record
that says *I do not know* is worth more than one that says a number
nobody applied — the podman measurement in §7 is exactly this hazard,
where a cap the runtime accepted and silently dropped would have left a
signed record stating a limit that never held.

**The image digest identifies bytes, not a tag.** `sandbox.params.image`
holds `postgres:16`, which points at different bytes over time; the
digest is what makes a record say which engine performed the restore.
That completes the set `adapter.digest` and `env.probavi_digest` began.

What this does **not** close is the other half of the gap above: an
isolation class is still not evidence, and §7's door is still shut.

That is exactly right for an image or a memory cap, and not obviously
right for a parameter naming an isolation class, which an auditor will
read as a fact about the drill. Until that door is answered (§7), no
parameter of that kind may ship, and no surface may describe a sandbox
parameter as evidence of isolation.

The provider id has the same property, and the podman measurement sharpened
it: an operator running podman through its docker-compatible shim gets
records that read `provider: docker`, because the id names the code path
and not the runtime that answered it — the same way a remote daemon over
SSH already does. Recording the observed runtime is one answer and a
separate provider id is another; neither is worth taking before the door
below is.

## 7. Three doors, each answered before the provider that needs it

These are one-way doors. Each is to be answered in a spec change *before*
the first provider that needs it, not during it.

1. **Where a remote provider's endpoint lives** — §5. Either the
   environment, as for all three shipped providers, or the descriptor gate
   grows a refusal.
2. **What a sandbox parameter proves** — §6. Either the core reads such a
   value back from the runtime and records what it observed, which is a
   schema question and therefore a spec change first, or a parameter of
   that kind is documented as configuration and nothing more.
3. **Whether a drill may run on the host that signs its record.** Every
   provider today puts the restored copy of production data on a machine
   other than the one holding the signing key, and the bare-host provider
   makes a dedicated target its stated premise rather than a
   recommendation. A provider on the drill host itself ends both. Decide
   whether that co-residency is acceptable, and under what premise, before
   any local provider — not after one exists.

## 8. Shipping a provider

Two rate limits, and they belong to this document rather than to the
closed non-goals list:

- **At most one new sandbox provider per release cycle.**
- **None while a shipped provider is still `experimental`.** All three are
  today, and the gaps that keep them there are documented — the k8s
  provider has no pod-level equivalent of `--network none`; the bare-host
  provider has no container isolation at all.

A candidate also needs the demand signal Phase 2 asked for when it chose
between the second and third providers. "It would fit the contract" is not
one: the contract is satisfied by many runtimes nobody drills on.

What a provider ships with, all of it in the same pull request:

- a `Descriptor` declaring every parameter, the isolation properties, the
  constraints an operator must meet, and what CI verified it against;
- unit tests through an injected runner, on the scale the shipped
  providers set;
- an integration suite against the real runtime, behind the `integration`
  build tag, plus its setup in the pull-request workflow;
- `go generate ./...` in the same commit, so `docs/capabilities.json`
  states the new surface;
- a design document of its own where the provider's model differs enough
  to need one, as the bare-host provider's does.

## 9. What the core will not grow for a provider

- **Engine knowledge.** A provider that needs to know which engine is
  being restored is the wrong shape (§1).
- **`--privileged`, or a sysctl parameter.** Decided 2026-08-16: it would
  dissolve the zero-ingress isolation every signed record leans on. Db2
  remains documented out of reach on the docker provider for this reason.
- **Secrets in parameters.** §5.
- **A verb of its own.** §2.

A provider whose runtime needs one of these is not blocked by an omission;
it is outside the model, and the honest answer is to record it by name
with the reason — as `ROADMAP.md`'s provider catalogue does — rather than
to widen the contract until it fits.
