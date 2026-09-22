#!/bin/sh
# Run a real drill with the binaries a release actually publishes.
#
# Everything else proves the source tree: the unit suite, the integration
# suite and the version matrix all build from the working directory, and
# the release workflow builds archives, checksums them and attests them
# without ever running one. The packages are the exception — CI installs
# the .deb, .rpm and .apk and asks the core to resolve an adapter — but
# that is a probe, not a drill, and it never touches the tar.gz archives
# the README tells a reader to download first.
#
# Two things are only provable here. The release binaries are built with
# flags nothing else uses (-trimpath, -s -w, and -X main.version), and the
# version that flag stamps is what every signed evidence record carries as
# env.probavi_version — so a stamp that silently failed to apply would put
# the wrong build identity into an auditor's record, and -X against a
# symbol that moved is silent by design. And the archives are meant to run
# on a machine that has no Go, no build tree and no glibc, which is what
# CGO_ENABLED=0 is for; a musl container with nothing in it but the docker
# CLI is where that claim either holds or does not.
#
# Used by the release workflow, before it drafts anything, and by CI on
# every pull request, so what CI proves is what a release publishes. Run
# from the repository root:
#
#   VERSION=0.32.0 ARCH=amd64 packaging/drill-from-archives.sh dist
#
# Argument: <directory holding the archives and their SHA256SUMS>.
#
# Needs a Docker daemon: the drill restores into a real sandbox, and the
# clean container reaches the same daemon through the mounted socket, so
# the sandbox is its sibling rather than its child. That is the deployment
# docs/docker.md describes, and it exercises put_file the long way round —
# the adapter streams the backup from inside the clean container into a
# container it does not share a filesystem with.

set -eu

ARCHIVE_DIR="${1:?directory holding the release archives}"
: "${VERSION:?VERSION is required}"
: "${ARCH:?ARCH is required (amd64 or arm64)}"

# Pinned the way the other containers in these workflows are pinned. This
# one is alpine-based and carries the docker CLI and nothing else, which
# is both requirements at once: the sandbox provider drives that CLI
# (never an SDK, AGENTS.md §2.2), and musl is what makes "static" a claim
# rather than an assumption.
CLEAN_IMAGE="docker:28-cli"

# The engine the quickstart uses, and the only adapter this drill needs.
# One engine is the right scope: every adapter's restore path is proven
# against every version it claims by the version matrix, from source. What
# is unproven here is the shape of the artifact, which is the same for all
# thirty-three binaries because one loop builds them.
ENGINE_IMAGE="postgres:16"

ARCHIVE_DIR=$(cd "${ARCHIVE_DIR}" && pwd)
CORE="probavi_${VERSION}_linux_${ARCH}.tar.gz"
ADAPTER="probavi-adapter-postgres_${VERSION}_linux_${ARCH}.tar.gz"
for f in "${CORE}" "${ADAPTER}" SHA256SUMS; do
	test -f "${ARCHIVE_DIR}/${f}" || {
		echo "missing ${f} in ${ARCHIVE_DIR}" >&2
		exit 1
	}
done

WORK=$(mktemp -d)
FIXTURE_CTR="probavi-artifact-smoke-$$"
cleanup() {
	docker rm -f "${FIXTURE_CTR}" >/dev/null 2>&1 || true
	# The clean container runs as root, so anything it wrote is root-owned
	# and the host cannot remove it. Delete from inside a container that
	# can, then take the directory itself.
	docker run --rm -v "${WORK}:/w" "${CLEAN_IMAGE}" sh -c 'rm -rf /w/..?* /w/.[!.]* /w/*' >/dev/null 2>&1 || true
	rm -rf "${WORK}"
}
trap cleanup EXIT

# A backup to restore. Made here rather than committed because a drill
# should prove a real pg_dump, and because the fixture's engine version
# and the sandbox's then move together.
echo "--- building a pg_dump fixture"
docker run -d --name "${FIXTURE_CTR}" --memory 1g \
	-e POSTGRES_HOST_AUTH_METHOD=trust "${ENGINE_IMAGE}" >/dev/null
ready=""
i=0
while [ "${i}" -lt 90 ]; do
	if docker exec "${FIXTURE_CTR}" pg_isready -h 127.0.0.1 -q 2>/dev/null; then
		ready=yes
		break
	fi
	i=$((i + 1))
	sleep 1
done
test -n "${ready}" || {
	echo "the fixture engine never became ready" >&2
	exit 1
}
docker exec "${FIXTURE_CTR}" psql -h 127.0.0.1 -U postgres -q \
	-c "CREATE TABLE orders AS SELECT generate_series(1,50000) AS id;"
docker exec "${FIXTURE_CTR}" pg_dump -h 127.0.0.1 -U postgres -Fc -f /tmp/orders.dump postgres
docker cp "${FIXTURE_CTR}:/tmp/orders.dump" "${WORK}/orders.dump"
docker rm -f "${FIXTURE_CTR}" >/dev/null

# The quickstart's drill configuration, with one real check beside the
# health one: row_count is what makes a restore that produced an empty
# database fail here rather than pass.
cat >"${WORK}/drill.yaml" <<YAML
target:
  name: released-artifact-drill
  adapter: postgres
  source:
    kind: pgdump
    path: /smoke/work/orders.dump
sandbox:
  provider: docker
  params:
    image: ${ENGINE_IMAGE}
    memory: 1GiB
    env.POSTGRES_HOST_AUTH_METHOD: trust
  timeout: 15m
checks:
  - builtin: service_healthy
  - builtin: row_count
    table: orders
    min: 50000
evidence:
  path: /smoke/work/evidence.jsonl
  sign_key: /smoke/work/probavi.key
YAML

echo "--- running the quickstart from the archives, in ${CLEAN_IMAGE}"
# -i is load-bearing, not decoration: without it the container's stdin is
# empty, `sh -s` reads EOF, runs nothing and exits 0 — a step that passes
# by doing nothing at all. Three steps in ci.yml did exactly that from
# 2026-08-05 until this change.
docker run --rm -i \
	-e VERSION -e ARCH \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v "${ARCHIVE_DIR}:/smoke/dist:ro" \
	-v "${WORK}:/smoke/work" \
	"${CLEAN_IMAGE}" sh -eus <<'SH'
	cd /smoke/dist

	# The README tells a reader to check the download against SHA256SUMS.
	# --ignore-missing is GNU-only and this shell is busybox, and a
	# release's SHA256SUMS lists all 367 assets while two are mounted, so
	# each line is selected and fed in on its own.
	for f in "probavi_${VERSION}_linux_${ARCH}.tar.gz" "probavi-adapter-postgres_${VERSION}_linux_${ARCH}.tar.gz"; do
		grep -F "  ${f}" SHA256SUMS | sha256sum -c -
	done

	mkdir -p /opt/probavi
	tar -xzf "probavi_${VERSION}_linux_${ARCH}.tar.gz" -C /opt/probavi ./probavi
	tar -xzf "probavi-adapter-postgres_${VERSION}_linux_${ARCH}.tar.gz" -C /opt/probavi ./probavi-adapter-postgres
	PATH="/opt/probavi:${PATH}"
	export PATH

	# The premise of this whole script. If a toolchain ever appears in
	# this image the test still passes while proving something weaker, so
	# it is asserted rather than assumed.
	if command -v go >/dev/null 2>&1; then
		echo "a Go toolchain is present in the clean container" >&2
		exit 1
	fi

	# -X main.version, at runtime. Nothing else in the repository reads
	# this value back: CI builds with the flag but never looks, and the
	# unit gate holds the source constant to the changelog without being
	# able to say whether the flag reached the binary.
	want="probavi ${VERSION} linux/${ARCH}"
	got=$(probavi version | head -1)
	test "${got}" = "${want}" || {
		echo "version stamp: got '${got}', want '${want}' — -X main.version did not reach this build" >&2
		exit 1
	}

	cd /smoke/work
	probavi evidence keygen --out probavi.key
	probavi run --config drill.yaml
	probavi evidence verify --log evidence.jsonl --key probavi.key.pub | grep -q '"status":"VALID"' || {
		echo "the released binary could not verify the log it had just written" >&2
		exit 1
	}

	# The stamp again, this time where it actually matters: inside the
	# signed record, which is what an auditor reads.
	grep -q "\"probavi_version\":\"${VERSION}\"" evidence.jsonl || {
		echo "the signed record does not name ${VERSION} as the build that wrote it" >&2
		exit 1
	}
	echo "--- drilled, verified, and stamped ${VERSION}"
SH
