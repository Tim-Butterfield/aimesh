#!/usr/bin/env bash
# build-dist.sh — LOCAL release-artifact simulation for the reviewmesh binary.
#
# This builds the same archive layout a future GitHub Actions release will produce,
# under ./dist/, using version ldflags. It is NOT a published release: with no git
# commit it stamps dev-local/nohead/dirty. Pure-Go (CGO disabled) so every target
# cross-compiles locally. Generated artifacts are gitignored (never source).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="${ROOT}/dist"

VERSION="${REVIEWMESH_VERSION:-dev-local}"
COMMIT="${REVIEWMESH_COMMIT:-nohead}"
DIRTY="${REVIEWMESH_DIRTY:-true}"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PKG="github.com/Tim-Butterfield/aimesh/internal/review/version"
LDFLAGS="-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT} -X ${PKG}.Date=${DATE} -X ${PKG}.Dirty=${DIRTY}"

rm -rf "${DIST}"
mkdir -p "${DIST}"

# Targets Go cross-compiles without CGO. Extend as needed.
targets="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64"

for t in ${targets}; do
  os="${t%/*}"
  arch="${t#*/}"
  name="aimesh_${VERSION}_${os}_${arch}"
  staging="${DIST}/${name}"
  mkdir -p "${staging}"

  bin="aimesh"
  [ "${os}" = "windows" ] && bin="aimesh.exe"

  echo "building ${name} ..."
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${staging}/${bin}" "${ROOT}/cmd/aimesh"

  [ -f "${ROOT}/LICENSE" ] && cp "${ROOT}/LICENSE" "${staging}/"
  [ -f "${ROOT}/README.md" ] && cp "${ROOT}/README.md" "${staging}/"

  if [ "${os}" = "windows" ]; then
    ( cd "${DIST}" && zip -qr "${name}.zip" "${name}" )
  else
    ( cd "${DIST}" && tar -czf "${name}.tar.gz" "${name}" )
  fi
  rm -rf "${staging}"
done

# checksums over the archives (shasum on macOS, sha256sum on Linux)
( cd "${DIST}" && { shasum -a 256 aimesh_* 2>/dev/null || sha256sum aimesh_*; } > checksums.txt )

echo "artifacts in ${DIST}:"
ls -1 "${DIST}"
