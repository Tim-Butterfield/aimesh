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

# Hermetic: a throwaway home so the smoke never reads a real config, and run artifacts kept
# beside it. The fake adapter is the internal test harness (see CONTRIBUTING.md) — it is the only
# way to drive a real end-to-end run on a machine with no model CLI installed, which is exactly
# what a release runner is.
export AIMESH_HOME="${SMOKE}/home"
export REVIEWMESH_ARTIFACT_DIR="${SMOKE}/artifacts"
export EXPLOREMESH_ARTIFACT_DIR="${SMOKE}/artifacts"
export AIMESH_INTERNAL_FAKE=1
mkdir -p "${AIMESH_HOME}"

echo "== ${BIN} --version --json"
"${BIN}" --version --json
echo "== ${BIN} --help (suppressed)"
"${BIN}" --help >/dev/null
echo "== ${BIN} doctor"
( cd "${WS}" && "${BIN}" doctor )
echo "== ${BIN} review list --json / explore list --json (suppressed)"
"${BIN}" review list --json >/dev/null
"${BIN}" explore list --json >/dev/null
echo "== ${BIN} agents-md (suppressed)"
"${BIN}" agents-md >/dev/null
echo "== ${BIN} review run --report --profile fake-smoke ${WS}"
( cd "${WS}" && "${BIN}" review run --report --profile fake-smoke . )
echo "== ${BIN} explore run (ad-hoc fake panel)"
( cd "${WS}" && "${BIN}" explore run "smoke: pick a queue" --criteria cost,latency \
    --explorer adapter=fake,model=fake-a --explorer adapter=fake,model=fake-b \
    --collator adapter=fake,model=fake-c >/dev/null )

echo "smoke OK from extracted binary: ${BIN}"
