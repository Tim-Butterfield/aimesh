#!/usr/bin/env bash
# acp-sanitize-transcript.sh — OPT-IN, MANUAL sanitizer for a captured real-ACP-host
# transcript. It reads a RAW capture (one JSON frame per line) that you saved under
# tmp/acp-host-verification/<timestamp>/ during docs/acp-host-verification.md, and writes a
# SANITIZED version to stdout with volatile/sensitive fields replaced by placeholders.
#
# It is NEVER invoked by `go test ./...`. It performs NO network/provider calls. Review the
# output by eye before promoting anything to testdata/acp/ (see testdata/acp/README.md) —
# this is a best-effort redactor, not a guarantee. If in doubt, redact more by hand.
#
# Usage:
#   scripts/acp-sanitize-transcript.sh tmp/acp-host-verification/<ts>/raw.jsonl > sanitized.jsonl
#
# Redactions (best-effort):
#   - session ids            "sessionId":"s-0007"            -> "sessionId":"s-XXXX"
#   - RFC3339 timestamps      2026-06-28T22:00:00Z / .123Z   -> <TS>
#   - run directories         .../<id>/... under an artifacts/run dir -> <RUNDIR>
#   - home paths              /Users/<name> | /home/<name>   -> <HOME>
#   - UUIDs                   8-4-4-4-12 hex                  -> <UUID>
#   - secret-ish fields       any key containing token/secret/password/authorization/
#                             bearer/apikey/api_key/credential/private_key -> "<REDACTED>"
#                             (case-insensitive, substring match — over-redacts by design)
set -uo pipefail

src="${1:-}"
[ -n "$src" ] || { echo "usage: $0 <raw-transcript-under-tmp>" >&2; exit 2; }
[ -f "$src" ] || { echo "no such file: $src" >&2; exit 2; }
case "$src" in
  */tmp/*|tmp/*) : ;; # raw captures must live under tmp/ (gitignored)
  *) echo "refusing: raw captures must live under tmp/ (got $src)" >&2; exit 2 ;;
esac

sed -E \
  -e 's/("(sessionId)"[[:space:]]*:[[:space:]]*")s-[0-9A-Za-z._-]+(")/\1s-XXXX\3/g' \
  -e 's/("[^"]*(token|secret|password|passwd|authorization|bearer|apikey|api_key|credential|private_key|privatekey)[^"]*"[[:space:]]*:[[:space:]]*")[^"]*(")/\1<REDACTED>\3/gI' \
  -e 's/[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})/<TS>/g' \
  -e 's#/Users/[^/"[:space:]]+#<HOME>#g' \
  -e 's#/home/[^/"[:space:]]+#<HOME>#g' \
  -e 's/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/<UUID>/g' \
  -e 's#(<HOME>[^"]*/)?[^"/]*/(artifacts|reviewmesh-[^/"]*)/[0-9A-Za-z._-]+#<RUNDIR>#g' \
  "$src"
