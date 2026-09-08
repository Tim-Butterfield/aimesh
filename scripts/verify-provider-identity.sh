#!/usr/bin/env bash
# verify-provider-identity.sh — OPT-IN, MANUAL real-provider identity verification.
#
# Runs ONE bounded identity capture against a real provider CLI and writes raw artifacts
# under tmp/provider-verification/<timestamp>/<adapter>/<case>/. It MAY spend subscription
# usage / tokens / ACU — run it deliberately. It is NEVER invoked by `go test ./...`.
#
# Usage:
#   scripts/verify-provider-identity.sh <adapter> <model> [prompt]
#   adapter ∈ devin-cli | claude-code | codex-cli | agy-cli | ollama
#
# It runs the provider from a fresh empty temp dir (no repository code in scope) and
# records argv/stdout/stderr/exit-code. SANITIZE before promoting anything to testdata/.
set -uo pipefail

adapter="${1:-}"
model="${2:-}"
prompt="${3:-Output only your model identifier/name}"
[ -n "$adapter" ] && [ -n "$model" ] || { echo "usage: $0 <adapter> <model> [prompt]"; exit 2; }

REPO="$(cd "$(dirname "$0")/.." && pwd)"
TS="$(date -u +%Y%m%dT%H%M%S)"
case="${model//[^A-Za-z0-9._-]/_}"
OUT="${REPO}/tmp/provider-verification/${TS}/${adapter}/${case}"
mkdir -p "$OUT"

# Build argv per adapter (best-known shapes; confirm against your installed CLI).
case "$adapter" in
  devin-cli)   bin=devin;  argv=(-p "$prompt" --model "$model" --respect-workspace-trust false) ;;
  claude-code) bin=claude; argv=(-p "$prompt" --output-format json) ;;
  codex-cli)   bin=codex;  argv=(exec -s read-only -m "$model" "$prompt") ;;
  agy-cli)     bin=agy;    argv=(--model "$model" -p "$prompt") ;;  # agy 1.0.13: no --approval-mode
  ollama)      bin=ollama; argv=(run "$model" "$prompt") ;;
  *) echo "unknown adapter: $adapter"; exit 2 ;;
esac

printf '%s ' "$bin" > "${OUT}/argv.txt"; printf '%q ' "${argv[@]}" >> "${OUT}/argv.txt"; echo >> "${OUT}/argv.txt"

# Run from an empty dir so no repo content is in scope.
WORK="$(mktemp -d)"
( cd "$WORK" && "$bin" "${argv[@]}" ) > "${OUT}/stdout.txt" 2> "${OUT}/stderr.txt"
echo "$?" > "${OUT}/exit-code.txt"
rm -rf "$WORK"

cat > "${OUT}/notes.md" <<EOF
# Provider identity capture (RAW — sanitize before promoting)

- adapter: ${adapter}
- requested model: ${model}
- prompt contained NO repository source code.
- fixtureKind: real (once sanitized + promoted to testdata/)

## Sanitize before copying to testdata/adapters/${adapter}/<case>/
Remove: usernames, home paths, account/org IDs, tokens, session/request IDs, and any
environment-identifying CLI diagnostics. Keep only the model-identity text.
EOF

echo "captured → ${OUT}"
echo "exit=$(cat "${OUT}/exit-code.txt")  stdout-bytes=$(wc -c < "${OUT}/stdout.txt")"
