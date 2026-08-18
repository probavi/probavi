# Installing Probavi from a package

Status: **Normative for the published packages**, 2026-08-05. Every
release attaches `.deb`, `.rpm` and `.apk` files for `amd64` and `arm64`,
plus a `PKGBUILD` and an ebuild for the two distributions that build from
source.

---

## 1. There is no Probavi apt or yum repository

On Linux, packages are attached to the [GitHub
release](https://github.com/probavi/probavi/releases); you install the
file. `apt install probavi` will not work, and neither will automatic
updates; macOS is the same story (§5).

That is a deliberate trade. Hosting a signed apt or yum repository means
a **second long-lived signing key** to guard, in a project whose entire
trust proposition is how it handles the *first* one — the key that signs
evidence records. A leaked repository key would let an attacker ship a
`probavi` that writes whatever records they like.

What you get instead:

- **`SHA256SUMS`** covering every artifact of a release.
- **A sigstore build-provenance attestation** on every archive and
  package, which proves the file was built by this repository's release
  workflow from a specific commit, and needs no key from anyone:

  ```console
  $ gh attestation verify probavi_0.15.0_amd64.deb --repo probavi/probavi
  ```

Signing the `.deb` files themselves would be close to theatre: `dpkg` does
not verify package signatures by default (`debsig-verify` ships disabled
on Debian and Ubuntu), so most users would gain nothing for that second
key.

## 2. One package per binary

`probavi` is the orchestrator. It resolves an engine adapter as
`probavi-adapter-<engine>` **on `PATH`** and launches it as a child
process, so the core alone cannot run a drill:

| Package | Install it when |
|---|---|
| `probavi` | always |
| `probavi-adapter-postgres` | you drill PostgreSQL |
| `probavi-adapter-mysql` | you drill MySQL or MariaDB |
| `probavi-adapter-mongodb` | you drill MongoDB |
| `probavi-adapter-clickhouse` | you drill ClickHouse |
| `probavi-adapter-mariadb` | you drill MariaDB |
| `probavi-adapter-etcd` | you drill etcd |
| `probavi-adapter-redis` | you drill Redis |
| `probavi-adapter-valkey` | you drill Valkey |
| `probavi-adapter-sqlite` | you drill SQLite |
| `probavi-adapter-duckdb` | you drill DuckDB |
| `probavi-adapter-prometheus` | you drill Prometheus |
| `probavi-adapter-cassandra` | you drill Apache Cassandra |
| `probavi-adapter-opensearch` | you drill OpenSearch |
| `probavi-adapter-influxdb` | you drill InfluxDB |
| `probavi-adapter-victoriametrics` | you drill VictoriaMetrics |
| `probavi-adapter-mssql` | you drill SQL Server |

**Verifying an evidence log needs only `probavi`.** `probavi evidence
verify` reads a log and a public key; an auditor installs one package and
nothing else — no adapter, no container runtime.

## 3. Dependencies, and why there are almost none

`probavi` declares **no hard dependency**. Nothing is pulled in that a
verification-only install would not want.

| | `.deb` | `.rpm` | `.apk` |
|---|---|---|---|
| Required | — | — | `ca-certificates` |
| Installed by default | `Recommends: ca-certificates` | `Recommends: ca-certificates` | — |
| Offered, not installed | `Suggests: docker.io \| podman-docker, openssh-client, kubernetes-client` | `Suggests: docker, openssh-clients, kubernetes-client` | — |

A sandbox runtime belongs to whichever **provider** your drill config
names, not to the binary. `apt` installs `Recommends` by default, which
is why a container engine sits in `Suggests` — otherwise it would be a
hard dependency wearing a different hat. Docker CE is not in the Debian
archive at all, so the alternatives listed are the ones the archive can
actually satisfy.

`apk` has no weak dependencies, so `ca-certificates` is required there.
Alpine's base is the most likely to lack it, and that is exactly where an
HTTPS webhook notification would fail silently **while the drill itself
succeeded**.

`probavi-adapter-<engine>` depends on `probavi`, **unversioned**: the
compatibility contract between core and adapter is the adapter protocol
version negotiated at handshake, not either package version. It depends
on nothing else — in particular **no engine client**, because the engine's
own tools run inside the sandbox image, not on the drill host.

## 4. Install

### Debian, Ubuntu, Mint, Raspbian, Devuan

```console
$ ver=0.15.0 arch=amd64
$ base="https://github.com/probavi/probavi/releases/download/v${ver}"
$ curl -fsSLO "${base}/probavi_${ver}_${arch}.deb"
$ curl -fsSLO "${base}/probavi-adapter-postgres_${ver}_${arch}.deb"
$ curl -fsSLO "${base}/SHA256SUMS"
$ sha256sum -c SHA256SUMS --ignore-missing
$ sudo apt install ./probavi_${ver}_${arch}.deb ./probavi-adapter-postgres_${ver}_${arch}.deb
```

Use `apt install ./file.deb`, not `dpkg -i`: apt resolves the
`Recommends` and the adapter's dependency on the core.

Upgrade by installing the newer file. Remove with
`sudo apt remove probavi-adapter-postgres probavi`.

### Fedora, RHEL, CentOS, Rocky, Alma, openSUSE

```console
$ sudo dnf install ./probavi-0.15.0-1.x86_64.rpm ./probavi-adapter-postgres-0.15.0-1.x86_64.rpm
```

`zypper install` on openSUSE. Remove with `sudo dnf remove probavi-adapter-postgres probavi`.

### Alpine, postmarketOS

```console
$ sudo apk add --allow-untrusted ./probavi_0.15.0_x86_64.apk ./probavi-adapter-postgres_0.15.0_x86_64.apk
```

`--allow-untrusted` is required because the package is not signed by an
Alpine repository key — verify `SHA256SUMS` and the attestation instead
(§1). Remove with `sudo apk del probavi-adapter-postgres probavi`.

### Arch, Manjaro, EndeavourOS

Each release attaches a `PKGBUILD` that builds from the source tarball
and produces the split packages:

```console
$ curl -fsSLO "https://github.com/probavi/probavi/releases/download/v0.15.0/PKGBUILD"
$ makepkg -si
```

### Gentoo

Each release attaches `probavi-<version>.ebuild`. Adapters are USE flags
(`postgres`, `mysql`, `mariadb`, `mongodb`, `mssql`, `clickhouse`, `etcd`, `redis`, `valkey`, `sqlite`, `duckdb`, `prometheus`, `cassandra`, `opensearch`, `influxdb`, `victoriametrics`) rather than separate packages,
since the tree builds from source anyway. Drop it into a local overlay:

```console
$ mkdir -p /var/db/repos/local/app-backup/probavi
$ cp probavi-0.15.0.ebuild /var/db/repos/local/app-backup/probavi/
$ ebuild /var/db/repos/local/app-backup/probavi/probavi-0.15.0.ebuild manifest
$ USE="postgres" emerge app-backup/probavi
```

Publishing these two to the AUR and to a Gentoo overlay is a manual step
for now: the AUR wants an SSH key that cannot be scoped to a single
repository, and there is no overlay repository yet. CI renders and
checksums both files; nothing is hand-edited.

## 5. macOS

**There is no hosted Probavi Homebrew tap**, for the same reason there is
no apt repository: it would be one more thing to host and keep in step,
and the value it adds over a checksummed download is small for a tool run
from cron.

Two routes, both supported.

### 5.1 The release tarball

```console
$ ver=0.15.0 arch=arm64        # or amd64 on an Intel Mac
$ base="https://github.com/probavi/probavi/releases/download/v${ver}"
$ curl -fsSLO "${base}/probavi_${ver}_darwin_${arch}.tar.gz"
$ curl -fsSLO "${base}/probavi-adapter-postgres_${ver}_darwin_${arch}.tar.gz"
$ curl -fsSLO "${base}/SHA256SUMS"
$ shasum -a 256 -c SHA256SUMS --ignore-missing
$ tar -xzf "probavi_${ver}_darwin_${arch}.tar.gz" ./probavi
$ tar -xzf "probavi-adapter-postgres_${ver}_darwin_${arch}.tar.gz" ./probavi-adapter-postgres
$ sudo install -m0755 probavi probavi-adapter-postgres /usr/local/bin/
```

A file downloaded this way **is quarantined** by macOS, and Gatekeeper
will refuse to run it until you clear the attribute:

```console
$ xattr -d com.apple.quarantine /usr/local/bin/probavi
$ xattr -d com.apple.quarantine /usr/local/bin/probavi-adapter-postgres
```

That is the honest cost of shipping without an Apple Developer ID. A
signed, notarised `.pkg` would remove the step, in exchange for a paid
Apple account, a signing certificate to guard, and Apple credentials in
CI — a second long-lived secret, which is exactly what §1 avoids.

### 5.2 Homebrew, in a tap of your own

Every release attaches ready-made formulae, one per binary, with their
`sha256` taken from that release's own `SHA256SUMS`. They name no tap, so
they work in any:

```console
$ brew tap-new "$USER/probavi"
$ curl -fsSL -o "$(brew --repository "$USER/probavi")/Formula/probavi.rb" \
    "https://github.com/probavi/probavi/releases/download/v0.15.0/probavi.rb"
$ curl -fsSL -o "$(brew --repository "$USER/probavi")/Formula/probavi-adapter-postgres.rb" \
    "https://github.com/probavi/probavi/releases/download/v0.15.0/probavi-adapter-postgres.rb"
$ brew install "$USER/probavi/probavi" "$USER/probavi/probavi-adapter-postgres"
```

Homebrew downloads **without setting the quarantine attribute**, so
nothing needs clearing on this route. Upgrade by replacing the formulae
with the next release's and running `brew upgrade`; remove with `brew
uninstall probavi-adapter-postgres probavi`.

The adapter formula depends on `probavi` by plain name, so it resolves to
whichever tap you put them in.

### 5.3 Before the first drill

**macOS has no native container runtime.** The `docker` sandbox provider
needs Docker Desktop, colima, OrbStack, or a remote `DOCKER_HOST`; the
`remotehost` provider needs only an SSH target. Install one first rather
than discovering it at teardown time. Everything in §7 then works
unchanged, with the evidence log and key wherever your drill config puts
them.

## 6. Where things live

| | |
|---|---|
| Binaries | `/usr/bin/probavi`, `/usr/bin/probavi-adapter-<engine>` |
| Documentation | `/usr/share/doc/probavi/` |
| Licence | `/usr/share/doc/probavi/LICENSE` (Arch: `/usr/share/licenses/`) |

**Binaries go in `/usr/bin`, never `/usr/libexec`.** An adapter looks like
a helper program, and FHS instinct says to hide one — but the core finds
it with `exec.LookPath`, so anything off `PATH` fails every drill with
`resolve adapter: executable file not found`.

Probavi creates no directories, no system user, and no unit files. It has
no daemon and no built-in scheduler; the evidence log and the signing key
live wherever your drill config says.

## 7. A first drill from a packaged install

```console
$ sudo mkdir -p /etc/probavi /var/lib/probavi
$ sudo probavi evidence keygen --out /etc/probavi/ed25519.key
```

`/etc/probavi/drill.yaml`:

```yaml
target:
  name: prod-orders-db
  adapter: postgres
  source:
    kind: pgdump
    path: /backups/orders/latest.dump
sandbox:
  provider: docker
  timeout: 30m
  params:
    image: postgres:16
    env.POSTGRES_HOST_AUTH_METHOD: trust
checks:
  - builtin: service_healthy
  - builtin: row_count
    table: orders
    min: 1
evidence:
  path: /var/lib/probavi/evidence.jsonl
  sign_key: /etc/probavi/ed25519.key
```

```console
$ probavi run --config /etc/probavi/drill.yaml
$ probavi evidence verify --log /var/lib/probavi/evidence.jsonl \
    --key /etc/probavi/ed25519.key.pub
```

Schedule it from cron or a systemd timer — Probavi ships no scheduler on
purpose, and its exit codes are the contract: `0` proven restorable, `1`
recoverability failure, `2` infrastructure error or cancelled, `3` usage
or setup error, `5` no evidence record could be written. Take a lock:
two drills writing one evidence log collide on its single-writer lock.

```cron
17 2 * * *  flock -n /var/lock/probavi-orders.lock \
              probavi run --config /etc/probavi/drill.yaml
```
