#!/usr/bin/env bash
# Runs the conformance scenarios.
#
#   scripts/conformance.sh record [version]   # record golden transcripts from upstream t3
#   scripts/conformance.sh upstream [version] # check upstream against the goldens (should pass)
#   scripts/conformance.sh go                 # check yatt-server against the goldens
#
# version defaults to spec/upstream-version. Needs Go and npm.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
mode="${1:?usage: conformance.sh record|upstream|go [version]}"
version="${2:-$(cat "$root/spec/upstream-version")}"
bin="$root/.cache/bin"
mkdir -p "$bin"
(cd "$root/server" && go build -o "$bin/t3conform" ./cmd/t3conform && go build -o "$bin/yatt-server" ./cmd/yatt-server)

upstream_t3() {
  local dir="$root/.cache/t3-$version"
  if [ ! -x "$dir/node_modules/.bin/t3" ]; then
    mkdir -p "$dir"
    (cd "$dir" && npm init -y >/dev/null && npm install --silent "t3@$version")
  fi
  echo "$dir/node_modules/.bin/t3"
}

cd "$root"
case "$mode" in
  record) "$bin/t3conform" -server "$(upstream_t3)" -record ;;
  upstream) "$bin/t3conform" -server "$(upstream_t3)" ;;
  go) "$bin/t3conform" -server "$bin/yatt-server" -expect-fail conformance/expected-failures.txt -report .cache/conformance-report.json ;;
  *) echo "unknown mode $mode" >&2; exit 2 ;;
esac
