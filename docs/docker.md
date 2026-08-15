# Running Probavi in a container

Status: **Normative for the published image**, 2026-08-05. The image is
`ghcr.io/probavi/probavi`, built on every release for `linux/amd64` and
`linux/arm64` from the `Dockerfile` at the repository root.

Read §1 before deploying this. The image is convenient; the deployment it
implies is not free of consequence, and this document states the cost
plainly rather than in a footnote.

---

## 1. The consequence you are accepting

Probavi's docker sandbox provider drives the `docker` CLI to create
**sibling** containers — the disposable sandbox a backup is restored into
is a container next to Probavi's, not inside it. So a containerised
Probavi needs a daemon it can reach, and there are exactly two ways to
give it one:

| | What it costs |
|---|---|
| Bind-mount the host's `/var/run/docker.sock` | **Root-equivalent access to that host.** Anything that can talk to the daemon can start a privileged container mounting `/`. |
| `DOCKER_HOST=ssh://user@host` (or `tcp://` with TLS) | An SSH credential to a remote daemon, and no local socket at all. |

Neither is Docker-in-Docker; Probavi never nests a daemon.

**The container is not the recommended deployment.** Running `probavi` as
a binary on the host — from the release archives or a distribution
package — needs no socket mount and no extra credential. The image exists
for people who already run everything in containers, and for whom adding
one more is cheaper than adding a binary. If that is not you, the plain
binary is the smaller trust decision.

If you do mount the socket, the second residual risk of AGENTS.md §3.3
still applies unchanged: restored sandboxes contain production data.

## 2. What this image supports

| Sandbox provider | In this image | Why |
|---|---|---|
| `docker` | yes | `docker-cli` is installed; needs a reachable daemon (§1). |
| `remotehost` | yes | `openssh-client` is installed. |
| `k8s` | **no** | `kubectl` is absent on purpose. |

The Kubernetes provider needs a kubeconfig and RBAC to create, exec into
and delete Jobs in a namespace — an in-cluster deployment with a service
account, not a container holding a socket. Two different operational
shapes; bundling `kubectl` here would suggest they are one. If you drill
from inside a cluster, run Probavi as a Job with a service account and
build an image that adds `kubectl`.

Also installed:

- **`ca-certificates`** — webhook notifications go over HTTPS. The public
  roots come from `ca-certificates-bundle`, which the Alpine base already
  carries; this package states that dependency instead of inheriting it
  from a base image that may change, and it brings
  `update-ca-certificates`, which is how you trust a private CA (§9).
- **`tzdata`** — so a schedule on the host and the container agree about
  what `02:00` meant.

## 3. Install

```console
$ docker pull ghcr.io/probavi/probavi:0.7.0
$ docker run --rm ghcr.io/probavi/probavi:0.7.0 version
probavi 0.7.0 linux/amd64
adapter protocol: probavi-adapter/0
evidence schema:  probavi-evidence/2 (verifies all published versions)
```

Every release publishes an immutable digest alongside the tag; pin it in
anything automated:

```console
$ docker pull ghcr.io/probavi/probavi@sha256:<digest from the release notes>
```

The image carries `probavi` and every in-repo adapter
(`probavi-adapter-postgres`, `-mysql`, `-mongodb`, `-mssql`) on `PATH`,
because the core launches an adapter as a child process and finds it by
name. That is why this image is a bundle where the release archives and
distribution packages are one artifact per binary.

## 4. A drill, start to finish

### 4.1 Three rules that save an afternoon

1. **`--user "$(id -u):$(id -g)"`.** The image runs as an unprivileged
   built-in user, whose uid will not match yours. Without this the
   evidence log and the signing key it writes are unreadable to you, and
   files you own are unreadable to it.
2. **`--group-add "$(stat -c %g /var/run/docker.sock)"`** when mounting
   the socket, or the daemon refuses the connection.
3. **The backup must be visible to the Probavi container**, not to the
   host at the same path. The adapter runs inside this container and
   pushes the file into the sandbox through the core's `put_file` verb,
   which is a `docker cp` from *this* filesystem. A path that exists only
   on the daemon's host will not be found.

### 4.2 Generate a signing key

Keys are read from a file with restrictive permissions. They are never
baked into an image layer, never passed through `ENV`, and never appear
in a config value (AGENTS.md §3.3).

```console
$ mkdir -p keys evidence
$ docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/keys:/keys" \
    ghcr.io/probavi/probavi:0.7.0 evidence keygen --out /keys/probavi.key
{"key_id":"…","key_file":"/keys/probavi.key","public_key_file":"/keys/probavi.key.pub"}
```

### 4.3 `drill.yaml`

Paths are as the **container** sees them.

```yaml
target:
  name: containerised-drill
  adapter: postgres
  source:
    kind: pgdump
    path: /backups/orders.dump
sandbox:
  provider: docker
  timeout: 10m
  params:
    image: postgres:16
    # trust auth is sandbox-only: the container runs with --network none
    # and no published ports, and is destroyed after the drill.
    env.POSTGRES_HOST_AUTH_METHOD: trust
checks:
  - builtin: service_healthy
  - builtin: row_count
    table: orders
    min: 1000
evidence:
  path: /evidence/evidence.jsonl
  sign_key: /keys/probavi.key
```

### 4.4 Run it

```console
$ docker run --rm \
    --user "$(id -u):$(id -g)" \
    --group-add "$(stat -c %g /var/run/docker.sock)" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$PWD/backups:/backups:ro" \
    -v "$PWD/keys:/keys:ro" \
    -v "$PWD/evidence:/evidence" \
    -v "$PWD/drill.yaml:/etc/probavi/drill.yaml:ro" \
    ghcr.io/probavi/probavi:0.7.0 run --config /etc/probavi/drill.yaml
{"outcome":"pass","seq":1,"evidence_path":"/evidence/evidence.jsonl","checks_passed":2,"checks_total":2,"restore_ms":77,"total_ms":1739}
```

The backup and the key are mounted read-only; only the evidence directory
is writable. `--rm` removes the Probavi container, not the sandbox — the
provider destroys that itself, and sweeps orphans on the next start.

### 4.5 Verify

Verification needs nothing but the log and the public key, so it runs
anywhere — in the image, from a release binary, or with the independent
verifier that shares no code with the writer:

```console
$ docker run --rm --user "$(id -u):$(id -g)" \
    -v "$PWD/evidence:/evidence:ro" -v "$PWD/keys:/keys:ro" \
    ghcr.io/probavi/probavi:0.7.0 \
    evidence verify --log /evidence/evidence.jsonl --key /keys/probavi.key.pub
{"status":"VALID","records":1,"damaged_lines":[],"failed_line":0,"reason":""}
```

## 5. Compose

```yaml
services:
  drill:
    image: ghcr.io/probavi/probavi:0.7.0
    user: "${UID}:${GID}"
    group_add:
      - "${DOCKER_GID}"          # stat -c %g /var/run/docker.sock
    command: ["run", "--config", "/etc/probavi/drill.yaml"]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./backups:/backups:ro
      - ./keys:/keys:ro
      - ./evidence:/evidence
      - ./drill.yaml:/etc/probavi/drill.yaml:ro
```

`docker compose run --rm drill`. This is deliberately not a long-running
service: Probavi has no built-in scheduler, and a container that exits
with a meaningful code is what cron and CI can act on.

## 6. Scheduling

Schedule from the **host**, not from a process inside the image. Probavi
deliberately ships no scheduler, and its exit codes are the contract:
`0` proven restorable, `1` recoverability failure, `2` infrastructure
error or cancelled, `3` usage or setup error, `5` no evidence record
could be written.

`docker run` passes the container's exit code through, and the image sets
no entrypoint wrapper, so those codes survive:

```cron
17 2 * * *  flock -n /var/lock/probavi-orders.lock \
              docker run --rm --user 1000:1000 --group-add 988 \
              -v /var/run/docker.sock:/var/run/docker.sock \
              -v /srv/backups:/backups:ro -v /srv/probavi/keys:/keys:ro \
              -v /srv/probavi/evidence:/evidence \
              -v /srv/probavi/drill.yaml:/etc/probavi/drill.yaml:ro \
              ghcr.io/probavi/probavi:0.7.0 run --config /etc/probavi/drill.yaml
```

`flock` matters: two drills writing one evidence log collide on its
single-writer lock. Use one log per drill, or serialize as above.

## 7. Persistence

Only the evidence log must outlive the container, and it must outlive it
**intact**: the log is append-only and hash-chained, so a lost tail is
lost proof. Mount a directory you back up, not an anonymous volume.

The signing key is mounted read-only and never written by a drill. Treat
it the way you treat any signing key: if it leaks, every record it signed
becomes forgeable, and the log's whole purpose with it.

## 8. Reproducing the image

The Dockerfile pins both base images by digest and builds the binaries
with the same flags as the release archives (`CGO_ENABLED=0`,
`-trimpath`, empty build id), so a binary extracted from the image and
one from the matching tarball are identical bytes:

```console
$ docker build --build-arg VERSION=0.7.0 -t probavi:local .
$ docker run --rm --entrypoint sha256sum probavi:local /usr/local/bin/probavi
```

Adapters carry no `-X main.version`: each reports its own
`adapterVersion`, which lands in every signed evidence record as
`adapter.version`, and a second, disagreeing number on the same binary
would leave an auditor deciding which one is authoritative.

## 9. A private certificate authority

If your webhook endpoint presents a certificate signed by an internal CA,
mount the CA certificate and rebuild the trust store before the drill:

```console
$ docker run --rm --user root \
    -v "$PWD/corp-ca.crt:/usr/local/share/ca-certificates/corp-ca.crt:ro" \
    --entrypoint sh ghcr.io/probavi/probavi:0.7.0 \
    -c 'update-ca-certificates && exec su probavi -s /usr/local/bin/probavi -- run --config …'
```

For anything beyond a one-off, build a small image on top instead — it
keeps the drill itself running unprivileged:

```dockerfile
FROM ghcr.io/probavi/probavi:0.7.0
USER root
COPY corp-ca.crt /usr/local/share/ca-certificates/
RUN update-ca-certificates
USER probavi
```

## 10. What the image does not do

- **No telemetry**, here or anywhere else in Probavi.
- **No published ports.** Neither this container nor the sandboxes it
  creates expose anything; the sandbox runs with `--network none`.
- **No secrets in the image or its environment.** Backup credentials are
  env or file references named by the drill config; the signing key is a
  file path. Nothing sensitive is ever baked into a layer.
