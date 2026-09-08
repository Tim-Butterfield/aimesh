#!/usr/bin/env bash
# smoke-dist.sh — extract the host-platform release artifact OUTSIDE the repo and run
# the default (no-external-CLI) flows from the extracted binary, not `go run`.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="${ROOT}/dist"
VERSION="${REVIEWMESH_VERSION:-dev-local}"
os="$(go env GOOS)"
arch="$(go env GOARCH)"

art="${DIST}/aimesh_${VERSION}_${os}_${arch}.tar.gz"
[ -f "${art}" ] || { echo "host artifact not found: ${art} (run scripts/build-dist.sh first)"; exit 1; }

SMOKE="${TMPDIR:-/tmp}/aimesh-release-smoke"
rm -rf "${SMOKE}"
mkdir -p "${SMOKE}"
tar -xzf "${art}" -C "${SMOKE}"

BIN="${SMOKE}/aimesh_${VERSION}_${os}_${arch}/aimesh"
[ -x "${BIN}" ] || { echo "extracted binary missing/not executable: ${BIN}"; exit 1; }

# a small copied fixture workspace OUTSIDE the repo
WS="${SMOKE}/ws"
mkdir -p "${WS}"
printf 'package main\n\nfunc main() {}\n' > "${WS}/main.go"

export REVIEWMESH_ARTIFACT_DIR="${SMOKE}/artifacts"

echo "== ${BIN} --version --json"
"${BIN}" --version --json
echo "== ${BIN} --help (suppressed)"
"${BIN}" --help >/dev/null
echo "== ${BIN} doctor"
"${BIN}" doctor
echo "== ${BIN} review --report ${WS}"
"${BIN}" review --report "${WS}"

echo "smoke OK from extracted binary: ${BIN}"
