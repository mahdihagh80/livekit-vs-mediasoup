#!/usr/bin/env bash
# Prints the hash build.sh links into the loadgen as main.sourceHash (run.json
# loadgen.source_sha256): sha256 over the client's Go sources, go.mod, go.sum
# and third_party/, text files only, in sorted path order.
set -euo pipefail
cd "$(dirname "$0")/.."
{
  find . -path ./third_party -prune -o -type f -name '*.go' -print0
  printf '%s\0' ./go.mod ./go.sum
  find ./third_party -type f -print0
} | LC_ALL=C sort -z | xargs -0 grep -lIZ '' -- | xargs -0 sha256sum | sha256sum | cut -d' ' -f1
