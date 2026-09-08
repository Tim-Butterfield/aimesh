#!/usr/bin/env bash
# golden-run-exploremesh.sh — exploremesh "behavior unchanged" golden equivalence gate (aimesh C20).
# The mirror of scripts/golden-run.sh for the second binary: it runs deterministic FAKE-ONLY
# explorations with --dump-run, normalizes the legitimately volatile fields, and diffs the captured
# artifacts against a stored baseline (testdata/golden-run-exploremesh/). `GOLDEN_UPDATE=1` (re)captures.
#
# TWO modes are run, deliberately:
#   map       — the formulation-free default: prompt payload, envelopes, plain collation.
#   shortlist — the adjudicative, count-bearing path: dual canonicalizer + append-only merge ledger +
#               the binding confirmation round + the frozen/hashed decision inputs + the HOST tally.
#               Nearly every governance hash in the system is computed only on this path, so a gate that
#               ran `map` alone would leave the parts most worth freezing unpinned.
#
# HERMETIC: the binary is built once and run in a THROWAWAY working directory with AIMESH_HOME /
# AIMESH_HOME / EXPLOREMESH_ARTIFACT_DIR all pointed inside the temp workspace. The child therefore
# resolves ONLY the fake-only profile written below — never the developer's real config, never this
# repo's project-scope config (resolution is root-anchored, so a child run inside the checkout would
# bind to it). Every adapter is the in-process `fake`: no CLI is spawned and no network is touched.
#
# Normalized (legitimately volatile): the run ID (the run directory is timestamped and the manifest
# records the same id), any RFC3339 timestamp, the temp workspace path, and the repo-root path.
# NOT normalized (must match): mode, adapter/model/effort triples, envelope IDs + the envelope-ID algo,
# payload/artifact/policy/decision/governance hashes, the merge ledger, identity status + evidence tier,
# every collated finding/ranking/label, and the rendered human output.
#
# DELIBERATELY EXCLUDED (with reasons — nothing here is faked to look stable):
#   * the run DIRECTORY NAME — `audit.NewRun` names it `<UTC timestamp>-<nanos>`, so it is volatile by
#     construction. The script locates the directory it just wrote instead of baselining its name.
#   * `logs/events.jsonl` — the CLI passes a nil progress hook, so `explore` writes no event log at all.
#     Baselining an always-absent file would assert nothing. (Its content is timestamp-per-line anyway;
#     the ACP surface's streaming is covered by the ACP tests.)
set -uo pipefail

# The golden run is an INTERNAL harness use of the hidden fake adapter: unlock it for this run.
export AIMESH_INTERNAL_FAKE=1

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"
GOLDEN_DIR="$REPO_ROOT/testdata/golden-run-exploremesh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# ONE home: the profiles and the shared adapters file both live under its `.aimesh/` state root, so a
# single AIMESH_HOME makes the run hermetic (it used to take two variables that had to agree).
HOME_DIR="$WORK/home"; ART_DIR="$WORK/artifacts"; WD="$WORK/wd"; CUR="$WORK/current"
mkdir -p "$HOME_DIR/.aimesh/explore" "$ART_DIR" "$WD" "$CUR"

# The FAKE-ONLY profile the whole gate runs on. Written explicitly rather than relying on the built-in
# demo default: the baseline must be pinned to a roster this script owns, so a future change to the
# shipped default profile shows up as a deliberate edit here instead of a mysterious golden mismatch.
# Three explorers (the panel size an adjudicative tally needs) with distinct models so the roster's
# unique-triple rule holds; `fake` is the ONLY non-CLI adapter, and it runs in-process.
cat > "$HOME_DIR/.aimesh/explore/profiles.yaml" <<'YAML'
schemaVersion: 1
defaultProfile: golden
profiles:
  golden:
    explorers:
      - adapter: fake
        model: fake-a
        effort: high
      - adapter: fake
        model: fake-b
        effort: medium
      - adapter: fake
        model: fake-c
        effort: low
    collator:
      adapter: fake
      model: fake-collator
      effort: high
YAML

# Build the single aimesh binary once, then run it from the throwaway cwd.
BIN="$WORK/aimesh"
if ! (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/aimesh); then
  echo "golden-run-exploremesh: build failed" >&2
  exit 2
fi

# run_mode <mode> <extra args...> — runs one exploration and copies its stable artifacts into $CUR
# under a `<mode>-` prefix. The run directory is the newest one under $ART_DIR after the run.
run_mode() {
  local mode="$1"; shift
  local before after out exit_code run_dir
  before="$(ls -1 "$ART_DIR" 2>/dev/null | wc -l | tr -d ' ')"
  out="$(cd "$WD" && AIMESH_HOME="$HOME_DIR" EXPLOREMESH_ARTIFACT_DIR="$ART_DIR" \
    "$BIN" explore run --mode "$mode" "$@" --dump-run 2>"$WORK/$mode.stderr")"
  exit_code=$?
  after="$(ls -1 "$ART_DIR" 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$after" -le "$before" ]; then
    echo "golden-run-exploremesh: --dump-run wrote no run directory for mode $mode" >&2
    cat "$WORK/$mode.stderr" >&2
    return 1
  fi
  run_dir="$(ls -1dt "$ART_DIR"/*/ | head -1)"

  {
    echo "exit=$exit_code"
    # The PANEL line is the pre-spend statement of what actually ran (source profile + selected/full).
    grep -E '^panel: ' "$WORK/$mode.stderr" || true
  } > "$CUR/$mode-summary.txt"
  # The rendered human result: the mode's own terminal renderer, which is the user-facing behavior.
  printf '%s\n' "$out" > "$CUR/$mode-stdout.txt"

  # The captured run artifacts. Absent ones are skipped: each is written only when its stage ran, and
  # WHICH ones exist is itself pinned by the manifest's artifact index.
  local rel
  for rel in manifest.json task.json formulation.json synthesis.json \
             decision.json governance-claims.json confirmation.json degraded.json \
             merge-ledger.jsonl merge-ledger-provisional.jsonl rounds.json mediations.json \
             calls/envelope-0/envelope.json calls/envelope-1/envelope.json calls/envelope-2/envelope.json; do
    if [ -f "$run_dir/$rel" ]; then
      cp "$run_dir/$rel" "$CUR/$mode-$(echo "$rel" | tr '/' '-')"
    fi
  done
}

run_mode map       --purpose "choose a datastore for the ingest service" --criteria "cost,latency,operability" || exit 1
run_mode shortlist --purpose "choose a datastore for the ingest service" --criteria "cost,latency,operability" || exit 1

# Normalize the legitimately volatile fields in every captured file (see the header).
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
  echo "golden-run-exploremesh: baseline captured to testdata/golden-run-exploremesh/"
  exit 0
fi

if [ ! -d "$GOLDEN_DIR" ]; then
  echo "golden-run-exploremesh: no baseline — run 'GOLDEN_UPDATE=1 make golden-run-exploremesh' first" >&2
  exit 2
fi

FAIL=0
# Both directions: a captured file that drifted, and a baselined file the run stopped producing (an
# artifact silently disappearing is a behavior change too).
for f in "$CUR"/*; do
  name="$(basename "$f")"
  if [ ! -f "$GOLDEN_DIR/$name" ]; then
    echo ">>> golden-run-exploremesh: NEW artifact $name (not in the baseline)"; FAIL=1; continue
  fi
  if ! diff -u "$GOLDEN_DIR/$name" "$f"; then
    echo ">>> golden-run-exploremesh MISMATCH in $name"; FAIL=1
  fi
done
for g in "$GOLDEN_DIR"/*; do
  name="$(basename "$g")"
  if [ ! -f "$CUR/$name" ]; then
    echo ">>> golden-run-exploremesh: MISSING artifact $name (the run no longer produces it)"; FAIL=1
  fi
done
[ "$FAIL" = 0 ] && echo "golden-run-exploremesh: OK (exploremesh behavior unchanged)"
exit $FAIL
