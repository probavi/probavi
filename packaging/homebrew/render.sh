#!/bin/sh
# Render the Homebrew formulae for one release.
#
# Checksums are read from the release's own SHA256SUMS rather than
# recomputed, so a formula can only ever pin what the release actually
# published. A missing entry is a hard error: a formula with a wrong or
# stale sha256 fails at `brew install` time, on the user's machine.
#
#   VERSION=0.5.0 packaging/homebrew/render.sh dist/SHA256SUMS dist/Formula
#
# Adapters come from the adapters/* glob, the same registry the archives,
# the packages and the container image use.

set -eu

SUMS="${1:?path to SHA256SUMS}"
OUT_DIR="${2:?output directory}"
: "${VERSION:?VERSION is required}"

mkdir -p "${OUT_DIR}"

# sha_of <archive name> — the checksum SHA256SUMS records for it.
sha_of() {
  sum="$(awk -v want="$1" '$2 == want || $2 == "*" want {print $1}' "${SUMS}")"
  if [ -z "${sum}" ]; then
    echo "homebrew: ${SUMS} has no entry for $1 — the formula would pin nothing" >&2
    exit 1
  fi
  printf '%s' "${sum}"
}

# Ruby class name: probavi-adapter-postgres -> ProbaviAdapterPostgres.
class_of() {
  echo "$1" | awk -F- '{for (i = 1; i <= NF; i++) printf "%s%s", toupper(substr($i,1,1)), substr($i,2)}'
}

export VERSION
SHA_ARM64="$(sha_of "probavi_${VERSION}_darwin_arm64.tar.gz")"
SHA_AMD64="$(sha_of "probavi_${VERSION}_darwin_amd64.tar.gz")"
export SHA_ARM64 SHA_AMD64
envsubst '${VERSION} ${SHA_ARM64} ${SHA_AMD64}' \
  < packaging/homebrew/probavi.rb.tmpl > "${OUT_DIR}/probavi.rb"

for dir in adapters/*/; do
  id="${dir#adapters/}"
  ADAPTER="${id%/}"
  ENGINE="$(jq -r --arg id "${ADAPTER}" '.adapters[] | select(.id == $id) | .name' docs/capabilities.json)"
  if [ -z "${ENGINE}" ] || [ "${ENGINE}" = "null" ]; then
    echo "homebrew: adapters/${ADAPTER} is not declared in docs/capabilities.json" >&2
    exit 1
  fi
  CLASS="$(class_of "probavi-adapter-${ADAPTER}")"
  SHA_ARM64="$(sha_of "probavi-adapter-${ADAPTER}_${VERSION}_darwin_arm64.tar.gz")"
  SHA_AMD64="$(sha_of "probavi-adapter-${ADAPTER}_${VERSION}_darwin_amd64.tar.gz")"
  export ADAPTER ENGINE CLASS SHA_ARM64 SHA_AMD64
  envsubst '${VERSION} ${SHA_ARM64} ${SHA_AMD64} ${ADAPTER} ${ENGINE} ${CLASS}' \
    < packaging/homebrew/adapter.rb.tmpl > "${OUT_DIR}/probavi-adapter-${ADAPTER}.rb"
done
