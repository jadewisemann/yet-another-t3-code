#!/usr/bin/env bash
# Regenerates spec/generated from an upstream T3 Code tag and shows what
# changed against the spec in the repository.
#
#   scripts/spec-update.sh v0.1.12            # a release tag
#   scripts/spec-update.sh nightly            # the newest nightly tag
#
# Then record new golden transcripts against that version:
#   scripts/conformance.sh record <version>
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
ref="${1:?usage: spec-update.sh <tag|nightly>}"
cache="${YATT_UPSTREAM_CACHE:-$root/.cache/upstream}"

if [ "$ref" = nightly ]; then
  ref="$(git ls-remote --tags --refs https://github.com/pingdotgg/t3code.git 'v*-nightly.*' \
    | awk '{print $2}' | sed 's#refs/tags/##' | sort -V | tail -1)"
  echo "newest nightly: $ref"
fi

rm -rf "$cache"
git clone --quiet --depth 1 --branch "$ref" https://github.com/pingdotgg/t3code.git "$cache"

old="$(mktemp -d)"
cp "$root/spec/generated/rpc.json" "$old/rpc.json"

(cd "$root/tools/spec-extract" && bun install --frozen-lockfile >/dev/null && bun extract.ts "$cache" "$root/spec/generated")
(cd "$root/server" && go run ./cmd/specdiff "$old/rpc.json" "$root/spec/generated/rpc.json") || true
(cd "$root/server" && go run ./cmd/speccov)
echo "${ref#v}" > "$root/spec/upstream-version"
