# Probavi in a container — the core plus every in-repo adapter, one image.
#
# Deliberately not the per-binary split the release archives and distro
# packages use (AGENTS.md §2.1): adapters run as child processes of the
# core and must share its filesystem, so separate images would only
# compose through COPY --from in a Dockerfile the user writes. Friction
# for almost everyone, to save about 12 MB.
#
# Read docs/docker.md before deploying this. The short version: the docker
# sandbox provider creates *sibling* containers through the docker CLI, so
# this image needs a daemon it can reach — and bind-mounting the host
# socket to give it one grants root-equivalent access to that host.

# --- build -------------------------------------------------------------
# Pinned by digest (AGENTS.md §3.3). The Go version tracks go.mod; a
# mismatch is a build failure, not a silent downgrade.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

WORKDIR /src

# Dependencies first so a source-only change does not refetch them. The
# module graph is tiny and fully pinned by go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is stamped into the core only. Adapters carry their own
# adapterVersion, which reaches every signed evidence record as
# adapter.version; a second, disagreeing number on the same binary would
# leave an auditor deciding which one is authoritative.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

# The same reproducible flags the release archives are built with, so a
# binary from this image and one from the matching tarball are identical.
RUN set -eux; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/probavi ./cmd/probavi; \
    for dir in adapters/*/; do \
      id="${dir#adapters/}"; id="${id%/}"; \
      CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
        go build -trimpath -ldflags "-s -w -buildid=" \
        -o "/out/probavi-adapter-${id}" "./${dir%/}"; \
    done

# --- runtime -----------------------------------------------------------
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Every package here is load-bearing; none is a convenience:
#
#   docker-cli       the docker sandbox provider shells out to it. Without
#                    it the image supports no sandbox at all.
#   openssh-client   needed twice over — by the remotehost provider, and
#                    by DOCKER_HOST=ssh://…, which docs/docker.md
#                    recommends over mounting the host socket.
#   ca-certificates  webhook notifications go over HTTPS. The public roots
#                    themselves come from ca-certificates-bundle, which
#                    alpine already ships — measured, not assumed. This
#                    package is here for the two things the bundle alone
#                    does not give: the dependency is stated rather than
#                    inherited from a base image that may change, and
#                    update-ca-certificates lets an operator trust a
#                    private CA (docs/docker.md §9).
#   tzdata           so a cron/systemd schedule on the host and the
#                    container agree about what "02:00" meant.
#
# kubectl is deliberately absent: the k8s provider needs a kubeconfig and
# RBAC to create Jobs, which is an in-cluster deployment, not a container
# with a mounted socket. Two different operational shapes; see
# docs/docker.md.
RUN apk add --no-cache \
      ca-certificates \
      docker-cli \
      openssh-client \
      tzdata

# Restored sandboxes contain production data (AGENTS.md §3.3). Running as
# root inside the container would add nothing but blast radius.
RUN addgroup -S probavi && adduser -S -G probavi -h /var/lib/probavi probavi

COPY --from=build /out/ /usr/local/bin/

USER probavi
WORKDIR /var/lib/probavi

# No ENTRYPOINT wrapper: probavi's exit codes are the cron/CI contract
# (0 proven, 1 recoverability failure, 2 infrastructure, 3 usage, 5 no
# record written), and a shell in front of them is a way to lose one.
ENTRYPOINT ["probavi"]
CMD ["version"]
