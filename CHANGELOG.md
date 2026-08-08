# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Pre-1.0, minor versions may contain breaking changes; every one of them is
recorded here. The adapter protocol and the evidence schema carry their own
versions, independent of the binary (see `docs/`); changes to either are
always called out explicitly.

## [Unreleased]

### Added

- **`mysqldump_with_users`, a MySQL source kind that replays the account
  layer before the dump and gates on the restored principal chain** (mysql
  adapter 0.3.0; tracked in #89). MySQL accounts and grants live in the
  `mysql` system schema, never in a single-database dump, so a plain
  `mysqldump` drill passes while the application account cannot log in and
  every `SQL SECURITY DEFINER` object fails at invocation (`ERROR 1449`).
  The source is one directory with both members named in `source.params`
  (`users`, and optionally `dump`); identity is the size-framed two-member
  composite with the *older* member's mtime, as in the sibling kinds.

  Decisions measured against a real MySQL 8.4 instance: the script is fed
  through stdin with `--force` (without it the client aborts mid-script,
  and the `source` command aborts even with it); exactly `ERROR 1396` for
  `root` and the reserved `mysql.*` accounts is tolerated. Three gates run
  after the load — orphaned definers, database reachability, and view
  resolution via `EXPLAIN`. The reachability gate exists because grants
  are database-scoped: restored under a name the script does not grant on,
  the account layer silently covers nothing (measured), so the drill must
  restore under the source database name and the gate's message says so.
  Diagnostics bound for protocol messages are scrubbed of `IDENTIFIED`
  literals, `$A$...` hash literals, and long hex literals.

- **`charset` and `collation` drill options for MySQL logical restores**
  (part of #89). The restore target used to be created with the sandbox
  server's defaults, silently discarding the source database's collation —
  which governs comparisons, ordering, and uniqueness. The options pin the
  source values when the adapter creates the target; the README documents
  the default's limits.

- **`bak_with_logins`, a SQL Server source kind that replays server
  logins before the restore and refuses to pass while any restored user
  is orphaned** (mssql adapter 0.2.0; tracked in #87). SQL Server splits
  principals across two scopes — logins in `master`, database users
  inside the `.bak`, linked by SID — so a plain `bak` drill restores the
  users orphaned: `RESTORE` succeeds, checks pass, the record says
  `pass`, and the application still cannot log in. This is the quiet
  sibling of the PostgreSQL globals gap fixed in 0.4.0, without the loud
  failure that made that one visible.

  The source is one directory with both members named in `source.params`
  (`logins`, and optionally `bak`; plain filenames, never paths). The
  backup identity covers both members — a composite, size-framed SHA-256
  in load order — and `created_at` is the *older* member's mtime: the set
  is only as current as its stalest member. The logins script is replayed
  with `sqlcmd -i` deliberately without `-b` (batch-abort would silently
  skip every login after a mid-script collision); exactly one failure
  class is tolerated, `Msg 15025` for principals the sandbox engine
  itself created (`sa` and the `##...##` internal principals). After the
  restore an orphan gate queries the restored database and fails the
  provision if any SQL user is left without a matching server login — an
  incomplete logins script cannot pass. Engine diagnostics bound for
  protocol messages are scrubbed of `PASSWORD` literals and long hex
  literals: a measured syntax error echoed a full password hash back, and
  such text must never reach a signed evidence record.

## [0.4.0] - 2026-08-07

### Added

- **`pgdump_with_globals`, a PostgreSQL source kind that restores the
  cluster globals before the dump** (postgres adapter 0.4.0; reported in
  #84). A logical recovery has two steps — `pg_dumpall --globals-only`,
  then the database dumps — and no `pg_dump` carries the first. A drill
  restoring only the dump proved the second half of a two-step path, and
  its `backup.checksum` covered only the dump, so the record claimed less
  than the recovery it stood for. `pg_restore --no-owner` drops `OWNER TO`
  but never `GRANT`, so such a restore dies on the first grant naming a
  role nothing created — the drill was right, and the gap was what came
  after it.

  The source is one directory with both members named in `source.params`
  (`globals`, and optionally `dump`; plain filenames, never paths).
  Explicit names rather than a filename pattern: renaming a backup file
  must not silently change what a drill proves. One directory rather than
  two paths because the core only hands an adapter files belonging to the
  drill's configured backup source (protocol §4.2), a guard that exists so
  a third-party adapter binary cannot pull arbitrary host files into a
  sandbox it controls. Leaving `dump` out restores the newest file that is
  not the globals, so a rotating backup directory keeps working
  unattended; naming it lets one directory serve several databases, each
  drilled separately with its own checks and its own record.

  `backup.checksum` covers **both** members — a checksum blind to the
  globals would let the roles a restore depends on change without the
  evidence noticing — using the tree hash's framing over exactly the two
  chosen members, so a sibling database's backup never moves this drill's
  identity. `backup.created_at` is the **older** member's mtime: a set is
  only as current as its stalest part, and stale globals are the gap being
  closed. The globals load counts into the measured restore duration,
  because the recovery it stands for includes it.

  **No evidence schema change** (v2 stays frozen; `backup.kind` carries
  the new value) and **no core change** — the second extension axis proven
  again.

  Two behaviours were measured against a real cluster rather than assumed,
  and both shaped the implementation: `pg_dumpall` emits `CREATE ROLE` for
  the bootstrap superuser as well, so **every** globals script collides
  with the role `initdb` already created. That one error — and only that
  one, for the connected superuser — is tolerated; `ON_ERROR_STOP` stays
  off, because the collision sits mid-script and stopping there would
  silently skip every role sorting after it, making a drill's completeness
  depend on role naming. And a globals script carries role password
  verifiers, which PostgreSQL quotes back in syntax errors: `--echo-errors`
  is refused, and SQL password literals are scrubbed from every engine
  diagnostic before it can reach a protocol message and, through it, a
  signed record (evidence schema §8).

## [0.3.1] - 2026-08-05

### Fixed

- **The container image stamped its binary with the git tag**, so
  `probavi version` reported `probavi v0.3.0` inside the image while the
  archives and packages of the same release reported `probavi 0.3.0`.
  That stamp is what a drill signs into every record as
  `env.probavi_version`, so one release produced two spellings of itself
  in an audit trail — and `docs/docker.md` §3 documented the output
  without the `v`, which made the document wrong rather than the image.

  The same mistake as the image *tag* fixed a moment earlier, in the
  second place it appears: the tag was corrected, the build argument next
  to it was not. The gate now covers both.

  **v0.3.0 was not re-cut**, deliberately. The Go module proxy had
  already recorded `github.com/probavi/probavi@v0.3.0` and
  `…/spec/evidence@v0.3.0` against the tagged commit, and the checksum
  database is immutable by design: re-pointing either tag would make
  `go install …@v0.3.0` fail permanently for everyone, exactly the way
  `v0.1.0` fails under the new module path today. A published image tag
  and a digest already printed in published release notes carry the same
  rule. The stamp is inconsistent, not invalid — the schema asks only for
  a non-empty string — so 0.3.1 supersedes it rather than rewriting it.

  The version gate learned something from this too: it required the
  documented `probavi-evidence-verify@vX.Y.Z` to equal the release, but
  `spec/evidence` is a separate module tagged independently, and this
  release does not touch it. Its pin is now held to the newest
  `spec/evidence` tag the changelog names, which is what is actually
  true, and a release no longer drags a meaningless module tag along.

## [0.3.0] - 2026-08-05

### Added

- **Ready-made Homebrew formulae attached to every release**, one per
  binary, with both architectures selected automatically. **There is no
  hosted Probavi tap** — decided 2026-08-05, for the same reason there is
  no apt repository: one more thing to host and keep in step, for little
  gain over a checksummed download. The formulae name no tap, so
  `brew tap-new` plus a `curl` per formula gives a working `brew install`
  in a tap of your own (`docs/packaging.md` §5.2).

  **No signed `.pkg`, and none is needed.** Homebrew downloads without
  setting the quarantine attribute, so Gatekeeper does not block the
  binaries — while a `.pkg` distributed any other way would need a
  Developer ID certificate, notarisation on every release, and Apple
  credentials in CI: a paid account and a new long-lived secret, for a
  CLI tool whose users already have `brew`. `docs/packaging.md` §5 states
  the other side of that trade too — downloading a release tarball
  directly *does* get quarantined, and how to clear it.

  Note the split is the opposite of the container image's, deliberately:
  installing a second formula is one command, while composing a derived
  Docker image is a Dockerfile plus a build plus a registry. Adapter
  formulae depend on the core, unversioned, and on nothing else; the core
  formula depends on nothing at all, so verifying an evidence log stays a
  one-formula install.

  Formulae are rendered from the release's own `SHA256SUMS`, never from
  recomputed checksums, so one can only pin what the release actually
  published — a missing entry stops the render rather than yielding a
  formula that fails on a user's Mac. CI renders them on every pull
  request, syntax-checks the Ruby, and proves the missing-checksum guard
  fires. It earned that on the first run: the initial templates produced
  Ruby that did not parse.

  `docs/packaging.md` §5.1 states the cost of that decision rather than
  hiding it: a release tarball downloaded **directly** is quarantined by
  macOS, and Gatekeeper refuses it until `xattr -d com.apple.quarantine`
  clears the flag. Homebrew does not set the attribute, so §5.2 has no
  such step. A signed, notarised `.pkg` would remove it everywhere, in
  exchange for a paid Apple account and a signing certificate to guard —
  a second long-lived secret, which is what the whole packaging set
  avoids.

  A gate holds this honest: nothing in the README, the packaging document
  or the formulae may name `probavi/tap/…` outside a comment. It is a
  plausible line to write and an install command nobody can run, which is
  the worst kind of documentation error — it looks tested.

- **Distribution packages for every release**: `.deb`, `.rpm` and `.apk`
  for amd64 and arm64, plus a `PKGBUILD` and a Gentoo ebuild that build
  from source. `docs/packaging.md` is normative for what they contain.

  One package per binary, matching the release archives: `probavi` plus
  `probavi-adapter-<engine>`. **`probavi` declares no hard dependency at
  all** — `probavi evidence verify` reads a log and a public key and needs
  no runtime, so an auditor who installs it to check a log must not have a
  container engine pulled onto their machine. A sandbox runtime belongs to
  whichever *provider* the drill config names; `apt` installs `Recommends`
  by default, so a container engine sits in `Suggests` or it is a hard
  dependency wearing a different hat.

  Adapter packages depend on `probavi`, unversioned, and on nothing else —
  in particular **no engine client**, because the engine's own tools run
  inside the sandbox image, not on the drill host.

  **No Probavi apt or yum repository, and no packaging GPG key.** Hosting
  one means a second long-lived signing key to guard, in a project whose
  trust proposition is how it handles the first — the key that signs
  evidence. Instead every artifact carries a **sigstore build-provenance
  attestation**, which proves the file came from this repository's release
  workflow and needs no key from anyone (`gh attestation verify`). Signing
  the `.deb` files would be close to theatre anyway: `dpkg` does not check
  package signatures by default.

  **A new CI job builds the packages and installs them** on Debian,
  Fedora and Alpine, then asks each install to resolve an adapter for
  real. It exists because building a package proves nothing about whether
  it works — and it caught precisely that: `nfpm` expands environment
  variables in scalar fields but **not** in a content path, so the first
  version produced packages that installed cleanly with the adapter at a
  literal `/usr/bin/probavi-adapter-${ADAPTER}`, where the core’s `PATH`
  lookup would never find it. The recipes are now rendered with
  `envsubst` before nfpm sees them, which removes the distinction.

  Four gates in `internal/docs` hold the recipes to the decisions behind
  them: binaries on `PATH` and never in `/usr/libexec` (the FHS instinct
  that would break every drill), no engine client and no version pin in
  the adapter dependency, no hard dependency on the core package, and the
  hand-written Arch and Gentoo recipes covering every adapter the
  generated manifest declares. All were checked to fail when violated.

- **A container image**, `ghcr.io/probavi/probavi`, built for
  `linux/amd64` and `linux/arm64` on every release, plus
  `docs/docker.md` — normative for what the image is and what running it
  costs.

  One image carrying the core and every in-repo adapter, deliberately not
  the per-binary split the archives and packages use: adapters run as
  child processes of the core and must share its filesystem, so separate
  images would only compose through a `COPY --from` Dockerfile the user
  writes.

  **The document leads with the consequence rather than burying it.** The
  docker sandbox provider creates *sibling* containers, so a containerised
  Probavi needs a daemon it can reach — and bind-mounting the host socket
  to give it one grants root-equivalent access to that host. The
  alternative, `DOCKER_HOST=ssh://…`, is stated beside it, and the
  container is explicitly **not** presented as the recommended
  deployment: the plain binary needs neither a socket nor a credential.

  `kubectl` is absent on purpose. The Kubernetes provider needs a
  kubeconfig and RBAC to create Jobs — an in-cluster deployment with a
  service account, not a container holding a socket. Bundling it would
  suggest the two are one shape.

  Verified by running a real drill from inside the image against the host
  daemon: sandbox created, `pg_dump` backup restored, both checks passed,
  a `probavi-evidence/2` record appended with both build digests, sandbox
  destroyed, zero orphans — and the log then verified both by the image
  and by the independent verifier. That run is also what confirmed the
  file-visibility rule now in §4.1: the backup must be readable by the
  *Probavi container*, not by the daemon's host, because `put_file` is a
  `docker cp` from the client's own filesystem.

  Three gates in `internal/docs` keep the image honest: the runtime
  packages each shipped feature needs, the unprivileged user, and both
  base images pinned by digest. All three were checked to fail when
  violated.

  One claim was corrected before it shipped. The first draft said HTTPS
  notifications fail without the `ca-certificates` package; measuring it
  showed the Alpine base already carries `ca-certificates-bundle`, so
  public HTTPS works without it. The package stays for the two reasons
  that survive measurement — the dependency is stated rather than
  inherited from a base image that may change, and `update-ca-certificates`
  is how an operator trusts a private CA (§9) — and the Dockerfile, the
  document and the gate all say that instead.

- **The core writes evidence schema v2**: every record now carries
  `adapter.digest` — the adapter executable that performed the restore —
  and `env.probavi_digest` — the `probavi` binary that signed the result.
  Schema v2 is frozen (`docs/evidence-schema.md` §11.2).

  A record already named the adapter and its version; it could not say
  which *build*. Two materially different builds could produce records
  claiming the same provenance, which is exactly the question an auditor
  asks when a restore is disputed. `adapter.version` cannot answer it:
  it is a number the adapter reports about itself.

  **An unreadable executable records null and never fails the drill.** A
  record with a null digest still proves the restore; a drill that died
  because a hash could not be taken proves nothing. Both paths are tested.

  The hashing lives on the core side of the adapter boundary. The protocol
  client exposes the executable it resolved and knows nothing about the
  record format — `sha256:` is an evidence-schema convention, and
  `internal/adapter` deliberately does not import `internal/evidence`.

  `log_v2.jsonl` joins the published conformance vectors, carrying both
  forms deliberately: a record with the digests populated, and an
  infrastructure failure whose `adapter.digest` is null because the
  adapter never resolved. It verifies offline with only the committed
  public key — by this repository's writer *and* by the independent
  verifier that shares no code with it. `log_v1.jsonl` is byte-frozen
  alongside `log_v0.jsonl`: a record written under a published version is
  never regenerated (§10), and a test now holds both.

- **The independent verifier accepts `probavi-evidence/2`**, and a new
  test pins its supported set to the versions the published JSON Schema
  declares — in both directions.

  §10 has always obliged a verifier to support every published version,
  for the lifetime of the format. Nothing enforced it. The specification
  could publish a version `spec/evidence` refuses, which is how an auditor
  ends up holding a log the independent verifier calls INVALID for no
  reason but a stale allow-list; the reverse — a version silently dropped
  — would be worse, because records already written would stop verifying.
  Both are now build failures.

  Covered by construction rather than by assertion about a map: a v2
  record signed the way §6 prescribes verifies against the committed
  public key, with the digests populated *and* null, since §3 makes them
  nullable so an unreadable executable never costs a drill its record. A
  log whose writer moved from v1 to v2 mid-file chains straight through,
  which is what a real upgrade produces. `probavi-evidence/3` is still
  refused, so the allow-list grew by one entry rather than into a
  wildcard. `spec/evidence` holds its 100% statement coverage.

- **Evidence schema v2 (`probavi-evidence/2`): `adapter.digest` and
  `env.probavi_digest`** — spec and JSON Schema; no writer emits v2 yet,
  and `docs/evidence-schema.md` §11.1 lists what still has to land before
  one may.

  A record named the adapter and its semantic version but carried no
  **build identity**, so two materially different builds could produce
  records claiming the same provenance — indistinguishable to the auditor
  those records exist for. `adapter.version` cannot close this: it is a
  number the adapter reports about itself, and the CI gate below reduces
  the drift without removing it, and says nothing at all about a
  third-party adapter.

  Both fields were taken in one version rather than the adapter alone. The
  same argument applies verbatim to the orchestrator — the core chooses
  the sandbox, runs the checks and signs the record — and §10 makes every
  field addition a major bump, so deferring the symmetric half would have
  cost a second migration for every verifier. Both are nullable: an
  unreadable executable must never cost a drill its signed record.

  The attestation limit is written into §3 rather than implied. The digest
  covers the bytes of the file the core selected, hashed before launch. It
  does not prove those bytes are the instructions that ran — a file
  swapped between hashing and `exec` would go unnoticed, and closing that
  window means reading `/proc/<pid>/exe`, which does not exist on every
  platform Probavi supports.

  **A pre-existing defect surfaced and is fixed here:** the §3 worked
  example declared `probavi-evidence/0` while carrying
  `drill.pitr_target`, a combination the published JSON Schema rejects —
  the example was never moved to v1 when that field was added, so a reader
  copying it as a starting point would have produced records their own
  verifier refuses. The example is now a v2 record, and a new gate in
  `internal/spec` validates it against the schema on every run, so the
  normative document and the schema derived from it cannot drift again.
  That gate caught a malformed digest in this very change before it
  reached review.

- **A CI gate that will not let an adapter's source change without its
  `adapterVersion` moving** (`internal/tools/adapterversion`, wired into
  `ci.yml` for pull requests).

  The constant is not bookkeeping: each adapter reports it through the
  protocol and the core signs it into every evidence record as
  `adapter.version`. Nothing forced it up, so two materially different
  adapter builds could publish records claiming the same provenance —
  indistinguishable to the auditor those records exist for.

  What counts as a change is a **deny-list, not an allow-list**, so a file
  type nobody has thought of yet fails closed. Only four things under an
  adapter directory are known not to reach its binary and are excluded:
  `*_test.go`, anything under `testdata/`, `README.md`, and `adapter.json`
  (read from disk by the capabilities generator, never compiled in). An
  adapter that grows a `//go:embed` is already covered.

  Two limits are stated in the tool rather than implied. It cannot see a
  Go toolchain change, which is pinned by `go.mod` and would alter every
  binary at once — a repository-wide event that should *not* push every
  adapter's semantic version up, or the number stops meaning anything. And
  it says nothing about which bytes produced a given record: that is build
  identity, and the evidence schema carries no digest today.

  The escape hatch is a pull-request label, `adapter-version-exempt`, not
  a commit marker — a change that genuinely cannot alter behaviour should
  be waved through where a reviewer can see the waiver. The failure
  message names the label, so nobody has to go looking for it.

- Translated README introductions in Hungarian, German, French, and
  Spanish (`README.hu.md`, `README.de.md`, `README.fr.md`,
  `README.es.md`), matching four of the shipped CLI locales — an operator
  who reads Probavi's diagnostics in their language can now read what
  Probavi is in the same language. Deliberately narrow (`docs/i18n.md`
  §7): only the stable spans of `README.md` are translated — what Probavi
  is, why it exists, the non-goals. Status, install, examples, and every
  capability inventory stay English-only, because a translated copy of
  them is a claim nobody can keep in sync; `docs/capabilities.json`
  remains the single machine-readable statement of what ships. The
  specs in `docs/` are normative and stay English-only.

  Translations are pinned to the English bytes they were made from: each
  file records the SHA-256 of every span it covers, and the new
  test-only `internal/docs` package fails the build when an English edit
  leaves a translation behind, when a pin outlives its span, when a
  translation smuggles in a version claim, or when the language row and
  the committed files disagree. Same principle as the catalog gates of
  `docs/i18n.md` §4: a translation may exist here only while a machine
  can prove it is current. Terminology follows each language's shipped
  CLI catalog, so the README and the terminal agree. All four texts have
  since been through the linguistic review recorded under *Changed*
  below; native-speaker feedback remains welcome and is folded in as it
  arrives (docs/i18n.md §5).

- The independent evidence verifier has a tagged, pinnable release:
  `spec/evidence/v0.2.0`. It lives in its own Go module, so it carries its
  own `spec/evidence/vX.Y.Z` tags and versions independently of the
  `probavi` binary — until now it had no tags at all, and
  `@latest` resolved to a pseudo-version of `main`, which meant the one
  artifact whose whole purpose is independent verification could not be
  named in an audit. Pin it when the verification itself has to be
  reproducible:

  ```sh
  go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@v0.2.0
  ```

  `@latest` keeps working and stays the recommended default for casual use.

- **`spec/evidence/v0.3.0`** — the independent verifier is retagged with
  this release because it changed materially: it accepts
  `probavi-evidence/2`, which the core now writes. Pinning
  `spec/evidence/v0.2.0` would pin a verifier that rejects every record
  produced from this release onward. The two tags move together from now
  on whenever the verifier changes; the schema version, not either tag,
  remains the compatibility contract (evidence-schema.md §10).

### Changed

- **A full linguistic review of every translated surface** — all 23 locale
  catalogs and all four README translations — landing 170 catalog
  corrections and the terminology and typography rules behind them
  (`docs/i18n.md` §8, new and normative).

  The headline finding is a product-level one, not a wording one:
  **nineteen of the twenty-three catalogs used the same word for a *drill*
  and for a *game-day*.** Both render as "exercise" in most European
  languages, so a localized operator could not tell one restore proof from
  an exercise made of many. Where the collision exists the game-day is now
  named `game-day` — a term already untranslated in every catalog — while
  the drill keeps the language's own word. Two further distinctions the
  product depends on are now stated and applied: *restore* against
  *recovery/recoverability*, and the frozen protocol *conformance checks*
  against a drill's *validation checks*.

  The rest are ordinary translation defects, each verified against the
  English source: `hard wall-clock limit` had been flattened to "time
  limit" in twenty catalogs, dropping the fact that the limit is on real
  elapsed time; Swedish said "dependencies to a failed member" where the
  English says *dependents of*; five catalogs left a bare adjective where
  the noun it modified had been dropped; Danish had a wrong-gender pronoun
  and a wrong-gender article; Greek left an English word (`mode`) inside a
  Greek sentence, and Greek and Irish both described the frozen check list
  with the word for *frozen solid*. In the READMEs: Hungarian rendered
  "row counts" as *ordinal numbers*, Spanish rendered "data freshness"
  with the word used for food and inverted the range it appears in,
  German used *Korruption* — bribery, not data corruption — and both
  German and Hungarian closed their opening typographic quote with an
  ASCII `"`.

  Three of the four open questions from the translations' own review notes
  are answered in `docs/i18n.md` §8 rather than left to the next
  translator: the drill/game-day and restore/recovery splits above, and
  French spacing — the README takes the non-breaking space French
  typography requires, the catalogs keep a plain space, because terminal
  output is grepped and copied.

- **Specs:** `docs/adapter-protocol.md` §6.2 and `docs/evidence-schema.md`
  §3 now state the `created_at` contract that neither of them carried: the
  adapter may report any RFC 3339 instant, and the core converts it to the
  evidence record's UTC millisecond form by truncation — never rounding, so
  a backup is never recorded as newer than it is. An unparseable value is
  an `adapter_crash` verdict, never a lost record.

  The gap was real and reachable: the evidence schema requires exactly
  millisecond precision, the protocol required nothing, the protocol's own
  example showed a second-precision instant, and `probavi adapter
  conformance` accepts any RFC 3339 — so a third-party adapter could pass
  "15/15 conformant" and then end every drill with "evidence record could
  not be written" after a successful restore. The four in-repo adapters
  already emit the millisecond form, which is why CI never saw it.

  No wire change and no version bump for either contract: nothing required
  of adapters is tightened, and no adapter that conforms today stops
  conforming. The core-side normalization that makes this true is a
  separate change.

### Fixed

- **The documented checksum command verified nothing, and said it was
  fine.** `nfpm` spells a pre-release `0.3.0~rc.1` — correct version
  ordering, since `~` sorts before the final release — but GitHub will
  not keep a `~` in a release asset's filename. The file uploaded as
  `probavi_0.3.0.rc.1_amd64.deb` was therefore checksummed under a name
  nobody could download, so `sha256sum -c SHA256SUMS --ignore-missing`,
  the command the release notes print, skipped **every package** and
  exited 0. A green tick for something it never looked at, in a product
  whose whole proposition is verifiable artifacts. Package filenames are
  now normalised where they are produced, and any name a release asset
  could not keep is a hard error. The version inside the package is
  untouched, so the ordering still holds.

- **The container image was published under a tag no document names.**
  `github.ref_name` is the git tag, so the workflow pushed
  `probavi:v0.3.0` while `docs/docker.md` told readers to
  `docker pull ghcr.io/probavi/probavi:0.3.0` — a 404 for every one of
  them, and invisible to CI, because the push succeeds and the
  documentation is prose. The image now carries the version without the
  leading `v`, matching the archives and the documentation.

  Both were found by cutting `v0.3.0-rc.1` and inspecting what it
  produced, which is what a release candidate is for.

- **The rendered `PKGBUILD` and ebuild were unusable**, and would have
  shipped attached to the first release that produced them. `envsubst`
  with no argument substitutes *every* `${...}` it finds and replaces the
  unset ones with nothing, so a PKGBUILD came out with `${pkgdir}`,
  `${srcdir}` and `${pkgbase}` erased — `cd "/probavi-"`, a download URL
  with no version in it, `-X main.version=`. It built cleanly into a
  release asset; the first person to notice would have been an Arch user
  running `makepkg`. Every render now names the variables it fills.

- **A pre-release tag produced invalid recipes.** `makepkg` rejects a
  hyphen in `pkgver` and portage's grammar wants `_rc1`, so `0.3.0-rc.1`
  is renamed to `0.3.0rc1` and `0.3.0_rc1` for those two — while the
  download URL keeps the tag that was actually pushed, which the naive
  rename would have broken. `nfpm` needed nothing: it already emits
  `~rc.1`, the form that sorts *before* the final release. Found by
  rendering the recipes for a release-candidate tag before cutting one.

  Three gates: a recipe must survive rendering with its own shell
  variables intact, every `envsubst` call must carry a SHELL-FORMAT
  argument, and a source recipe must build its download URL from the tag
  rather than from the version.

- **A downloaded release could not run a single drill.** The release
  workflow built `./cmd/probavi` and nothing else, but the core resolves
  `probavi-adapter-<name>` on `PATH` (AGENTS.md §2.1) — no config
  override, no bundled fallback, no search directory. So every published
  archive contained an orchestrator with nothing to orchestrate, and the
  only working install path was the Quickstart's `git clone` + `go build`.
  README had no install section at all.

  A release now publishes **one archive per binary**: `probavi` plus a
  separate `probavi-adapter-<engine>` for each adapter, on the same four
  platforms, through the same reproducible chain (`CGO_ENABLED=0`,
  `-trimpath`, empty build id, deterministic tar/gzip), all covered by one
  `SHA256SUMS`. Twenty archives per release instead of four, deliberately:
  an adapter is a separately installable thing, distro packaging splits
  the same way, and if adapters ever move to their own repositories
  (AGENTS.md §6) these artifact names do not change, so no install breaks.

  The adapters deliberately get no `-X main.version`. Each already carries
  its own version — the `adapterVersion` const it reports through the
  protocol, which lands in every signed evidence record as
  `adapter.version` and moves independently of the release tag. A second,
  disagreeing number on the same binary would leave an auditor deciding
  which one is authoritative. The generated release notes instead list
  each adapter's version, read straight from `docs/capabilities.json`, and
  state that the compatibility contract is neither number but the adapter
  protocol version negotiated at handshake.

  `README.md` gains an **Install** section: verify against `SHA256SUMS`,
  take the core plus an adapter per engine, put both on the same `PATH`.
  It also records what an auditor needs, which is less — `probavi evidence
  verify` reads a log and a public key, so the core alone suffices.

  The workflow enumerates adapters with a glob, so a new adapter ships
  without anyone editing CI. Two new gates in `internal/docs` keep that
  honest: the set of `adapters/*` directories must equal the set of
  adapters `docs/capabilities.json` declares — a directory the manifest
  does not know about would otherwise be built and published as a Probavi
  adapter — and the workflow must still derive its list from that glob, so
  the first gate keeps proving something about the release. Both were
  verified to fail when violated, not merely to pass today.

- The German usage block had one line running past the width the rest of
  the block wraps at, which shows in a terminal as a ragged paragraph. It
  is rewrapped; the block now measures exactly what the English does.

  Found by a systematic review pass over the translated surfaces —
  terminology against each catalog's own vocabulary, register, line widths,
  format verbs. It is not the native-speaker review `docs/i18n.md` §5 asks
  for, and that one is still owed; what it found is recorded as follow-up
  work rather than acted on where the call belongs to a native speaker.
- A `target.pitr.target_time` in the future is refused at config load. A
  drill can only prove recovery to an instant that has happened; an engine
  handed a future target simply recovers as far as it can, so the drill
  quietly proved something other than what the config asked for. The usual
  cause is a typed year or month, and catching it before a sandbox exists
  costs nothing.

  Targets within a minute of now are accepted, so ordinary clock skew
  between the host that wrote the config and the one running the drill
  cannot fail a drill.

  The diagnostic is translated into all 23 locale catalogs, as the gates of
  `docs/i18n.md` §4 require. **The 22 non-Hungarian translations ship
  without a native-speaker review pass** (§5); that review is owed, and
  corrections will follow.

- A recycled pid no longer makes a dead sandbox owner look alive. The
  orphan sweep decided ownership from the pid alone, so when the process
  that created a sandbox died abnormally and an unrelated process later
  inherited its pid, the sweep read "alive" and left the sandbox — holding
  restored production data — until that pid happened to be free during some
  later sweep.

  The pid label and the remotehost marker now carry an owner id: the pid,
  plus a token that a process inheriting the pid cannot match. The token is
  the process start time, which two processes sharing a pid cannot share,
  because the second started after the first exited.

  Linux only, and safe elsewhere by construction: where the operating
  system does not offer a start time cheaply, the id is the bare pid and
  the sweep falls back to the previous rule — the same answer as before,
  never the destructive one. An id written before this change is a bare pid
  too, so existing sandboxes keep being judged exactly as they were.

  The separator is a dash, not a colon: these ids become Kubernetes label
  values, where a colon is invalid — a test pins the id against that shape
  so the constraint cannot be broken from a unit test's blind side.
- The evidence log's directory entry is now flushed when the store opens.
  Every append fsyncs the file, which promises its bytes reached the disk —
  but the name pointing at those bytes lives in the parent directory, and
  for a newly created log that entry could still be in cache. A crash there
  lost the whole file, fsynced record and all: for an append-only log, the
  proof that a drill ran at all.

  One directory sync per drill, not per record. A filesystem that does not
  support syncing a directory (EINVAL) is not a reason to refuse to run a
  drill, so that one error is passed over; any other is reported rather
  than a durability guarantee being claimed that the store cannot make.

- Four small correctness items found during the audit:

  - `Timings.validate` ranged over a map, so a record with several negative
    phases named a different field on each run. Diagnostics a trust product
    prints have to be reproducible for whoever is comparing two logs.
  - The adapter environment could carry a variable twice when
    `source.credential_env` named a baseline one (`PATH`, `HOME`, `LANG`,
    `TZ`), leaving which value the adapter saw to exec's last-wins rule
    rather than to the §2.5 allow-list.
  - `checks.Result` carried a `Duration` nothing ever read, measured with a
    clock that ignored the injected one. The evidence schema records
    per-phase timings, not per-check ones, so the field is gone rather than
    published.
  - `evidence keygen` did not fsync the key files it wrote. A signing key
    that never reached a platter leaves a log nobody can verify and a
    signer nobody can rotate away from, because the records referencing it
    already exist.
- Three README claims had gone stale, all in the understating direction:
  the sandbox list omitted the `remotehost` provider that shipped
  2026-08-01, the design principles still called bare hosts future work,
  and the localization paragraph described Hungarian as "the first
  supported national language (further EU languages arrive per the
  ROADMAP)" while all 24 official EU languages were already shipping.

  `docs/capabilities.json` is generated from the code and CI-checked for
  drift, and AGENTS.md §5.8 makes it the only permitted source of
  capability claims for downstream surfaces — but nothing tied the
  README itself to it. `internal/docs` now does: every shipped adapter and
  sandbox provider must appear in the README, and a line that both names
  one and describes it as future work ("later", "planned", "arrives", …)
  fails the build. The rule reads only lines naming a shipped capability,
  so ordinary roadmap prose is untouched.

- A cancelled drill is now recorded as `cancelled`. `classify` mapped only
  `context.DeadlineExceeded` to a verdict, so Ctrl-C or SIGTERM produced
  whatever the dying adapter last managed to say — usually
  `adapter_crash`. The signed record therefore blamed a third party's
  adapter for the operator's own interrupt, in a document written to be
  read by an auditor, and `ROADMAP.md`'s "SIGTERM → cancelled record" was
  not true of any adapter that did not catch the signal in time.

  The drill's context now outranks the adapter's parting words in both
  directions: a deadline is a `timeout`, a cancellation is `cancelled`, and
  the adapter's message is kept inside the record either way. Teardown is
  told the matching reason, as the protocol §6.4 vocabulary expects.
- Both evidence verifiers read a log line into memory before applying the
  §4 size ceiling, so a file containing no newline at all was bounded only
  by the machine's memory. That is a poor property for the one part of
  Probavi designed to be pointed at a file someone else produced: `probavi
  evidence verify` and the standalone `probavi-evidence-verify` exist so an
  auditor can check a log they were handed.

  The size rule now bounds the read itself, in both implementations,
  written separately as the two-implementation rule requires: a line past
  the ceiling is an INVALID verdict at that line number and its remaining
  bytes are never gathered. Verdicts and line numbering are unchanged for
  every log that was valid before.

  The record-level size checks that used to sit after parsing are gone with
  it — the boundary that enforces the rule is now the read, and keeping a
  second, unreachable copy of it would be a branch no test could enter.

- `probavi adapter conformance` could abort with a harness error instead of
  reporting verdicts. The driver treated **any** stdin write failure as a
  suite-side failure, so an adapter that exited before the request landed —
  a cancelled run, an operator's Ctrl-C, or an adapter that answers and
  exits quickly — produced `write request: broken pipe` and exit 2 ("the
  suite could not be run to completion"). An adapter author saw the tool
  break for behaviour their adapter got right.

  A closed stdin is now no verdict by itself, which is what the core's own
  protocol client has always done: §2.1 lets an adapter stop the moment it
  sees EOF, and §2.3 makes "exited without a final response" a crash, so
  the read loop and the exit status decide what happened. `internal/conformance`
  is deliberately an independent implementation of the protocol, so it gets
  its own rule rather than a shared helper — the divergence CI surfaced is
  exactly what that independence is for.
- **Security:** the k8s and remotehost providers no longer put per-command
  environment values on the command line. Both passed `env NAME=value` to
  the command they ran, so a database password a check needs was readable
  from the process list — on the drill host and inside the pod for k8s, on
  the drill host and on the target for remotehost. The docker provider's
  half of this was fixed in the previous release entry; this completes it.

  Neither provider has an out-of-band environment channel (`kubectl exec`
  has no environment flag, ssh's `SendEnv` depends on the target's
  `AcceptEnv`), so the values now travel through stdin: the command runs
  under a shell prelude that reads exactly N `NAME=value` lines, exports
  each, and execs the command, whose own stdin continues untouched after
  those lines. Both providers already require `sh`, as `put_file` does.

  A value containing a newline cannot be expressed in a line protocol and
  is rejected rather than silently truncated — exporting the tail of a
  credential as a second variable would be the worse failure. The rejection
  never echoes the value.

  The `Descriptor.Constraints` of both providers — and therefore
  `docs/capabilities.json` — now describe the stdin channel instead of the
  exposure.

- **Security:** resolved database passwords no longer reach the docker
  provider's command line. `internal/checks` refuses `{{password}}` in an
  `sql_runner` argv template because "a password in argv would leak into
  process listings" — and the provider then passed the value as
  `docker exec -e NAME=value`, putting it in the drill host's process list
  for every local user to read. The value now travels in the docker CLI's
  own environment and only the variable's name appears in argv.

  The k8s and remotehost providers still have this exposure: `kubectl exec`
  has no environment flag and ssh's `SendEnv` depends on the server's
  `AcceptEnv`. It is now declared in both providers' `Descriptor.Constraints`
  and therefore in `docs/capabilities.json`, so the difference between the
  providers is published rather than discovered. A fix is designed and
  tracked separately.

- **Security:** an adapter could name any environment variable of the core
  process in `connection.password_env` and have its value handed to a
  process it controls inside the sandbox — exfiltration through a field
  meant for a database password. The name is now resolved against the same
  allow-list the adapter's own environment is built from (protocol §2.5):
  the core-generated ephemeral secret, or a variable the drill declared in
  `source.credential_env`. Anything else resolves to empty and is logged.
- Orphan detection was Linux-only, and releases ship macOS binaries. All
  three sandbox providers decided whether a sandbox's creating process was
  still running by stat-ing `/proc/<pid>`; on macOS that path does not
  exist, so the check failed for every pid and **every labeled sandbox
  looked orphaned**. Since the sweep runs at the start of every drill, a
  starting drill destroyed the running sandbox of any concurrent one on the
  same host — including parallel game-day members — mid-restore. The
  docker provider's own comment promised the opposite.

  Liveness is now decided by signalling the process with signal 0, which
  delivers nothing and only asks the kernel whether the pid exists and
  whether we may signal it. `EPERM` counts as alive: a process owned by
  another user is running, and treating "may not signal" as "gone" is the
  destructive answer. The check lives in `internal/sandbox` behind an
  injectable seam, so each provider's sweep decision is now unit-tested for
  both answers instead of depending on the host's filesystem layout.

  Unchanged: a recycled pid still looks alive, so an orphan can survive
  until its pid is free. That errs toward leaving a sandbox behind rather
  than destroying a live one.

- A drill whose composed record the store refused now leaves a **degraded
  record** instead of nothing (`docs/evidence-schema.md` §7.1, new). §7 has
  always required that every started drill end in exactly one appended,
  signed record; until now an unrepresentable value anywhere in the record
  broke that rule outright, and the drill vanished from the log — which
  reads exactly like a drill that was never scheduled.

  The replacement carries only what the core itself produced — drill
  identity, environment, total duration, `outcome: error` with
  `error.code: internal` and a message naming both the rejection and the
  verdict that was reached — and drops everything an adapter or the
  configuration supplied, since one of those values is why the record was
  refused. It uses no new fields, so no verifier needs to learn about it.

  Deliberately narrow: only shape rejections are retried. A store that
  cannot write is not a record the core can fix by rewriting it, and a
  second attempt would bury the real error. If even the degraded record
  fails, the original "left no evidence" error surfaces as before.

  A degraded record is a bug report, not a verdict — it is logged at error
  level and never claims `pass`.
- A drill that restored a backup successfully could end with **no evidence
  record at all** — exit 5, the failure mode `internal/core`'s package
  comment calls the highest-severity bug — because values an adapter
  reports were copied into the record without being checked against what
  the record accepts. `evidence.Record.Validate` rejects; it does not
  repair. Three reachable inputs did it:

  - `created_at` at second precision (valid RFC 3339, and the form the
    protocol's own example showed) — rejected as "not RFC 3339 UTC with
    millisecond precision",
  - a NaN phase timing — the `< 0` guard is false for NaN, and
    `int64(math.Round(NaN*1000))` is the most negative int64 there is, so
    the record was rejected for a negative duration,
  - a negative `size_bytes`.

  `validateProvisionResult` now holds the provision response to everything
  the record will demand of it: `created_at` is parsed and normalized to
  the schema's UTC millisecond form (truncated, never rounded), timings
  must be finite, non-negative and representable, `size_bytes`
  non-negative. A violation is an `adapter_crash` verdict **with** a
  signed record, which is what the boundary is for.

  Also accepted, here and in `probavi adapter conformance`: RFC 3339's
  lowercase `t`/`z` designators, which
  `docs/schemas/adapter/provision-response.json` has always declared valid
  while Go's `time.RFC3339` layout rejects them — an adapter whose output
  validates against the published schema must not fail certification.

- Probavi could sign records that fail Probavi's own published schema.
  `docs/schemas/evidence/record.json` constrains `error.code` to fourteen
  values; the core copied whatever code the adapter returned, and
  `Record.Validate` only checked that it was non-empty. An adapter
  answering `{"code":"banana_peel"}` produced a chain-valid, signed record
  that `probavi evidence verify` reports VALID while a schema check reports
  INVALID — the one contradiction a trust product cannot ship.

  The vocabulary now has a single home in `internal/evidence` (thirteen
  codes from the adapter protocol's §5 registry plus schema-defined
  `check_failed`), the core normalizes an unregistered code to `internal`
  and keeps the original in the message, and a schema test pins the Go list
  to the published enum in both directions so they cannot drift.

  It is normalization rather than rejection on purpose: refusing the record
  would trade a wrong code for no evidence at all, which is the worse
  failure.
- Non-ASCII failure text could make a completed drill leave **no evidence
  record**. Two producers of record fields measured in characters what the
  record layer measures in bytes, or cut a string without regard for
  encoding:

  - `core.sanitizeMessage` capped `error.message` at 500 *runes* against a
    512-*byte* limit, so an adapter reporting an engine error in an
    accented language — 400 characters, 800 bytes — was rejected;
  - `checks.truncateDetail` sliced `checks[].detail` at a byte offset,
    splitting a rune whenever engine stderr crossed the cap with non-ASCII
    text, and the invalid UTF-8 was rejected.

  Either one meant exit 5 after a successful restore. Both now use
  `evidence.TruncateLine`, which truncates on byte length at a rune
  boundary, alongside the exported `MaxDetailBytes` / `MaxErrorMessageBytes`
  caps — the mismatch between a producer's private constant and the
  schema's real limit was the bug, so the limits now have one home. The
  helper is swept across every budget and every UTF-8 stride, and a
  regression test drives a full drill with non-ASCII text through the real
  store.

- The open-core boundary in `ROADMAP.md` said "proving a **single drill**
  stays free forever". It was written on 2026-07-31, before game-days
  existed, and read as if `probavi gameday` — multi-database,
  dependency-ordered orchestration — were part of the commercial
  organisational layer. It never was: game-days shipped on 2026-08-02 as
  Apache-2.0 core and are a full free feature with no member limit. The
  boundary is now stated by what stays free rather than by scale, the
  README's license enumeration names game-days (plus notifications and
  metrics) explicitly, `docs/gameday.md` says it where a game-day user
  reads it, and `AGENTS.md` §6 declares the commercial list exhaustive so
  the ambiguity cannot come back. No licensing change — a wording
  correction to match what has always shipped.
- The README's status paragraph said "Released as **v0.1.0**" after
  `0.2.0` had shipped, while the install note two sections below already
  explained why `v0.2.0` is the version to use. The landing page of a
  trust product understating its own released version is exactly the
  drift the capabilities manifest exists to prevent, one surface up.

## [0.2.0] - 2026-08-03

### Changed

- **Breaking — the module path is now `github.com/probavi/probavi`.** The
  repository moved from `aafeher/probavi` to the `probavi` organisation on
  2026-08-03. GitHub redirects the old path, so clones and links keep
  working, but a Go module is identified by what its `go.mod` declares, not
  by where it is served from: without this change the canonical URL would
  have been the one address the project could not be installed from. Both
  modules moved — the core and `spec/evidence`, the independent verifier
  that lives in its own module so that the toolchain, not discipline,
  forbids it importing `internal/`. Update imports and install commands:

  ```sh
  go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@latest
  ```

  **`v0.1.0` is not installable under the new path and never will be.** Its
  `go.mod` declares the old module path, and Go checks that a module says
  what it was asked for, so building `github.com/probavi/probavi@v0.1.0`
  fails with a path mismatch. Retagging cannot fix it: `proxy.golang.org`
  and `sum.golang.org` have already recorded that version, and both are
  immutable by design — re-pointing the tag would replace a clear error
  with a checksum-mismatch security error. Use
  `github.com/aafeher/probavi@v0.1.0`, which still resolves and builds, or
  move to `v0.2.0` under the new path. The `v0.1.0` GitHub release and its
  binaries are unaffected; only module resolution is.

### Fixed

- The README's status paragraph said "Released as **v0.1.0**" after
  `0.2.0` had shipped, while the install note two sections below already
  explained why `v0.2.0` is the version to use. The landing page of a
  trust product understating its own released version is exactly the
  drift the capabilities manifest exists to prevent, one surface up.

- `probavi adapter conformance` usage text now documents exit code `2`.
  The command has always returned it when the suite cannot be driven to
  completion — an adapter that fails to exec, a stdout that cannot be
  written — but the help text listed only `0`, `1`, and `3`, so a CI
  pipeline branching on the documented set would have treated a suite
  that never ran as an unrecognized status. Surfaced while building the
  CLI contract table for `docs/capabilities.json`, which reads the exit
  codes from the same table the binary dispatches from. All 23 locale
  catalogs carry the new clause. **It shipped without a native-speaker
  review pass** (docs/i18n.md §5); that review is still owed, and a
  correction will follow in a later release if one is needed.

### Added

- Generated capabilities manifest: `docs/capabilities.json`
  (`probavi-capabilities/1`), the machine-readable statement of what
  Probavi can do in this repository — adapters with the engine versions
  CI actually restores from, sandbox providers with their parameters and
  isolation properties, built-in checks, the CLI commands with their
  exit-code contract, notification transports, available locales, the
  three contract versions, and the non-goals a consumer must never
  contradict. Its consumer contract is `docs/capabilities.md`
  (normative), its schema `docs/schemas/capabilities/capabilities.json`,
  and it is versioned independently of the binary like the adapter
  protocol and the evidence schema.

  It exists because downstream surfaces — the website reads this
  repository as a submodule — had no machine-readable source for
  capability claims and were writing them from README prose, which is
  exactly how a trust product ends up overstating what it does. Every
  fact is read from the code that implements it: the adapters' own probe
  goldens, the sandbox provider descriptors, the check registry, the CLI
  command table, the notification constants, the embedded locale
  catalogs, and the frozen contract version constants. The generator
  refuses to publish a claim the repository does not back — an adapter
  whose probe declares a source kind its manifest does not name, an
  engine version that does not appear in the image the integration suite
  pulls, a document path that no longer exists, an unknown maturity
  value — and a fifth CI gate ("Capabilities manifest") regenerates the
  file and fails on any diff. Output is deterministic and carries no
  timestamp or build metadata, so a diff always means a capability
  changed. `probavi` gains no new subcommand: the manifest describes the
  repository, not the binary.

- Independent evidence verifier (`spec/evidence`, evidence schema §12).
  Verifying a Probavi evidence log no longer depends on Probavi: a
  second implementation of the format, written from
  `docs/evidence-schema.md` alone, ships as the dependency-free
  `probavi-evidence-verify` tool, installable on its own with
  `go install github.com/probavi/probavi/spec/evidence/cmd/probavi-evidence-verify@latest`.
  It lives in a separate Go module, so the toolchain — not convention —
  forbids it importing Probavi's own evidence code. Until now the only
  thing checking the hash chain and the signatures was the code that
  wrote them; both implementations now agree on every published schema
  version and on tampered input, down to the failing line number. The
  worked example moved from `internal/evidence/testdata/` to
  `docs/schemas/evidence/examples/` and is published as a conformance
  vector anyone can verify against. **No evidence-format change:** no
  field, serialization rule, or record byte is affected, and the frozen
  example logs are byte-identical.

- Internationalized CLI output (spec docs/i18n.md): the usage text and
  CLI diagnostics are now localizable, with Hungarian (`hu`) as the
  first national language. The locale comes from
  `PROBAVI_LANG → LC_ALL → LC_MESSAGES → LANG` (POSIX order, no new
  flags or config keys); English remains the default and the fallback
  for anything unknown. Catalogs are zero-dependency embedded JSON
  keyed by the English text itself, and CI gates enforce completeness,
  staleness, and format-verb parity per language — a partially
  translated language cannot ship. Machine contracts are never
  translated: evidence records, JSON summaries, the adapter protocol,
  notification payloads, and structured logs stay English.
  Configuration-validation diagnostics (drill and game-day files) are
  part of the translated surface: `probavi run --config broken.yaml`
  explains the problem in the operator's language, with config keys and
  locators kept verbatim.
- DR game-day orchestration (`probavi gameday`, spec docs/gameday.md):
  multi-database restore exercises in dependency order. A game-day
  config references normal drill files as members with `depends_on`
  edges; each member runs the full drill pipeline — sandbox, restore,
  checks, its own signed evidence record, metrics, notifications — so
  the evidence schema is untouched and members stay independently
  runnable. Dependents of a failed member are skipped with a recorded
  reason (cascading), independent branches always run to completion,
  and cancellation leaves signed `cancelled` records for running
  members. Execution is sequential by default; `max_parallel` opts in
  to bounded concurrency, with a load-time guard rejecting members
  that share an evidence log while allowed to overlap. The one-line
  JSON summary lists every member with its record location
  (evidence path + seq) and reports the end-to-end wall clock — the
  service-level recovery time. Exit codes: 0 all passed, 1 a member
  drill failed, 2 errors/cancellation left members unproven, 5 a
  member's record could not be written.
- Webhook notifications (`notify` config section, docs/notifications.md):
  one JSON POST per configured webhook after the evidence record is
  signed, carrying the `probavi-notification/1` payload — a signpost to
  the record (outcome, check counts, restore timing, sequence number),
  never a substitute for it. URLs come from config or, for token-bearing
  endpoints, from the environment (`url_env`) and are redacted from all
  logs and errors; optional HMAC signing (`secret_env`,
  `X-Probavi-Signature-256`) lets receivers authenticate pushes. An `on`
  filter narrows delivery per outcome; the default — every outcome —
  keeps dead-man's-switch receivers working. Delivery is bounded (60 s
  budget, 3 attempts, no redirects), runs outside the drill timeout so
  cancelled drills still notify, and never changes the drill's verdict
  or exit code. The payload has a machine-readable schema
  (`docs/schemas/notification/payload.json`) validated in CI.
- SQL Server adapter (`adapters/mssql`, `probavi-adapter-mssql`):
  restores native `BACKUP DATABASE` artifacts (`bak`, `bak_dir` kinds)
  under the drill's target name, with the file list read from the backup
  and every logical file `MOVE`d to sandbox paths. The sandbox starts
  idle and the adapter owns the engine: SQL Server cannot run without a
  superuser password, and a password in sandbox params would enter the
  signed evidence record — so the drill engine uses a documented public
  constant confined to the zero-ingress sandbox, the mssql analog of the
  postgres trust overwrite and the mysql empty root password. sqlcmd's
  dialect quirks are absorbed declaratively (`-I` for double-quoted
  identifiers, a `SQLCMDINI` startup script for undecorated rows), so
  builtin checks work unchanged. Conformance 15/15.
- MongoDB adapter (`adapters/mongodb`, `probavi-adapter-mongodb`):
  restores `mongodump --archive` backups — plain or `--gzip`, the
  compression sniffed from the artifact bytes — with
  `mongorestore --stopOnError`, so partial restores fail loudly. Source
  kinds: `mongodump` (one archive file) and `mongodump_dir` (newest file
  in a directory). Checks are mongosh `--eval` expressions carried by the
  declared sql_runner template; the core stays engine-free, and the
  adapter passes all 15 conformance checks. Third engine, zero core
  changes.
- `remotehost` sandbox provider: restore drills on dedicated hosts that
  cannot run any container runtime, over plain SSH + systemd
  (`docs/sandbox-bare-host.md`). One sandbox is one transient systemd
  slice plus one per-drill workspace; every command — including the
  engine the adapter starts — runs as a transient unit inside the slice,
  so resource caps bound the whole sandbox and stopping the slice kills
  the entire process tree. Cleanup is three-layered (destroy on every
  outcome, host-scoped orphan sweep, target-side deadline timer that
  survives a vanished drill host). The target is selected with
  `PROBAVI_SSH_TARGET` in the environment only; connection details never
  enter drill config or evidence records.
- Remote Docker over SSH: the docker sandbox provider is documented and
  CI-proven against remote daemons selected with `DOCKER_HOST=ssh://…` —
  drills run on the remote machine while backups stream through the SSH
  connection, never a published port. The endpoint lives in the
  environment only; connection details never enter evidence records.

### Fixed

- The docker provider's `put_file` lands files owned by the identity
  exec commands run as (root-run chown after the copy), matching the k8s
  and remotehost providers, where the exec user creates the file by
  construction. Previously `docker cp` preserved the host file's numeric
  uid, so on images with a non-root default user (SQL Server runs as
  `mssql`) the copied backup was unreadable by the engine and the mode
  step failed outright. `put_file` on the docker provider now requires
  `sh` in the image, like the other providers always did.
- The docker orphan sweep is host-scoped now (matching the k8s provider):
  sandboxes carry a `com.probavi.host` label, and when several drill
  hosts share one daemon, a host can no longer mistake another host's
  live drill for a dead orphan and sweep it mid-run.

## [0.1.0] - 2026-08-01

First tagged release. Everything below is new.

### Added

- `probavi run`: config-driven restore drills — disposable sandbox up,
  backup restored by an engine adapter, validation checks, guaranteed
  teardown, and exactly one signed evidence record per started drill, on
  every path including crashes and cancellation. Cron/CI-friendly exit
  codes; no built-in scheduler by design.
- **Adapter protocol v0** (`docs/adapter-protocol.md`, frozen): engine
  adapters are external processes speaking line-delimited JSON on
  stdin/stdout, acting on the sandbox only through core-mediated verbs
  (`exec`, `put_file`). Machine-readable JSON Schemas for every message
  shape in `docs/schemas/adapter/`.
- **PostgreSQL adapter**: `pgdump`, `pgdump_dir` (logical), `pgbackrest`
  (physical) sources; point-in-time recovery on pgBackRest sources.
- **MySQL/MariaDB adapter**: `mysqldump`, `mysqldump_dir` (logical),
  `xtrabackup` (Percona XtraBackup, physical) sources.
- Point-in-time recovery drills: `target.pitr` drill config with exactly
  one of `target_time` (absolute) or `target_age` (relative, resolved at
  drill start); the resolved instant is recorded in evidence as
  `drill.pitr_target`.
- Sandbox providers: **Docker** (zero-ingress defaults: no published
  ports, `--network none`) and **Kubernetes Job** (no service-account
  token, cluster-side cleanup backstop); both drive the respective CLI,
  label every resource, and sweep orphans on startup.
- **Evidence store** (`docs/evidence-schema.md` v1, frozen): append-only
  JSONL, RFC 8785 canonical bytes, SHA-256 hash chain, ed25519 signatures.
  `probavi evidence verify` proves a log offline with only the public key;
  `probavi evidence keygen` generates key pairs. Machine-readable schema
  covering all published record versions in `docs/schemas/evidence/`.
- Validation checks: `service_healthy`, `table_exists`, `row_count`,
  `freshness`, and user-defined SQL assertions — engine-agnostic via the
  adapter-declared `sql_runner` template, redaction-safe by construction.
- `probavi adapter conformance`: the frozen 15-check protocol conformance
  suite against a simulated sandbox — no container runtime needed; the
  mechanical definition of done for third-party adapters, with the
  developer guide in `docs/adapter-development.md`.
- `probavi adapter probe`: resolve an adapter and print its capabilities.
- Prometheus textfile metrics per drill, including rolling restore-duration
  quantiles (p50/p95/max over the last 100 restores) for RTO trend
  alerting.
- `probavi version`: prints the binary version and the contract versions
  the build speaks.

[Unreleased]: https://github.com/probavi/probavi/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/probavi/probavi/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/probavi/probavi/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/probavi/probavi/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/probavi/probavi/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/probavi/probavi/releases/tag/v0.1.0
