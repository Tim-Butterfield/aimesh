#!/usr/bin/env bash
# golden-run.sh — reviewmesh "behavior unchanged" golden equivalence gate (aimesh P0.4).
# Runs a deterministic fake-adapter review, normalizes volatile fields, and diffs the key
# artifacts against a stored baseline (testdata/golden-run/). `GOLDEN_UPDATE=1` (re)captures.
#
# Normalized (legitimately volatile): run IDs, timestamps, temp paths, repo-root path.
# NOT normalized (must match): role names, adapter/model/evidence status, phase names,
# halt class/reason code, finding/decision content, exit code.
set -uo pipefail

# The golden run is an INTERNAL harness use of the hidden fake adapter: unlock it for this run.
export AIMESH_INTERNAL_FAKE=1

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"
GOLDEN_DIR="$REPO_ROOT/testdata/golden-run"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
HOME_DIR="$WORK/home"; ART_DIR="$WORK/artifacts"; WS_DIR="$WORK/ws"; CUR="$WORK/current"
mkdir -p "$HOME_DIR" "$ART_DIR" "$WS_DIR" "$CUR"

# Deterministic fixture workspace.
printf 'package sample\n\nfunc Sample() int { return 1 }\n' > "$WS_DIR/sample.go"

# Hermetic home => shipped seed. The `default` profile now ships UNCONFIGURED, so select the
# shipped-but-hidden `fake-smoke` profile (the deterministic fake adapter). `valid` scenario is
# deterministic. Runs the single aimesh binary from the repo root.
OUT="$(cd "$REPO_ROOT" && AIMESH_HOME="$HOME_DIR" REVIEWMESH_ARTIFACT_DIR="$ART_DIR" REVIEWMESH_FAKE_SCENARIO=valid \
  go run ./cmd/aimesh review run --report --profile fake-smoke "$WS_DIR" 2>&1)"
EXIT=$?

RUN_DIR="$(find "$ART_DIR" -maxdepth 1 -type d -name '2*' 2>/dev/null | sort | tail -1)"

{
  echo "exit=$EXIT"
  printf '%s\n' "$OUT" | grep -E '^review complete' || true
} > "$CUR/summary.txt"
cp "$RUN_DIR/review-summary.md"  "$CUR/review-summary.md"  2>/dev/null || true
cp "$RUN_DIR/run-state.json"     "$CUR/run-state.json"     2>/dev/null || true
cp "$RUN_DIR/resolved-plan.json" "$CUR/resolved-plan.json" 2>/dev/null || true
FIRST_CALL="$(find "$RUN_DIR/calls" -name call-status.json 2>/dev/null | sort | head -1)"
cp "$FIRST_CALL" "$CUR/call-status.json" 2>/dev/null || true

# Normalize volatile fields in every captured file.
for f in "$CUR"/*; do
  sed -E \
    -e "s#${WORK}#<WORK>#g" \
    -e "s#${REPO_ROOT}#<REPO>#g" \
    -e 's#/private/var/folders/[^"[:space:]]+#<TMP>#g' \
    -e 's#/var/folders/[^"[:space:]]+#<TMP>#g' \
    -e 's#[0-9]{8}T[0-9]{6}(\.[0-9]+)?(-[0-9]+)?#<RUNID>#g' \
    -e 's#[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z?#<TS>#g' \
    "$f" > "$f.norm" && mv "$f.norm" "$f"
done

if [ "${GOLDEN_UPDATE:-0}" = "1" ]; then
  rm -rf "$GOLDEN_DIR"; mkdir -p "$GOLDEN_DIR"; cp "$CUR"/* "$GOLDEN_DIR"/
  echo "golden-run: baseline captured to testdata/golden-run/"
  exit 0
fi

if [ ! -d "$GOLDEN_DIR" ]; then
  echo "golden-run: no baseline — run 'GOLDEN_UPDATE=1 make golden-run' first" >&2
  exit 2
fi

FAIL=0
for f in "$CUR"/*; do
  name="$(basename "$f")"
  if ! diff -u "$GOLDEN_DIR/$name" "$f"; then
    echo ">>> golden-run MISMATCH in $name"; FAIL=1
  fi
done
[ "$FAIL" = 0 ] && echo "golden-run: OK (reviewmesh behavior unchanged)"
exit $FAIL
