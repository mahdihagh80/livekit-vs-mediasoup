#!/usr/bin/env bash
# Build the load generator (Go >= 1.26, needed by server-sdk-go v2.18) and the media files.
# Build once on one host and scp the binary + media/ to the other generator if you like.
set -euo pipefail
cd "$(dirname "$0")"
H=$(./scripts/source-hash.sh)
go build -ldflags "-X main.sourceHash=$H" -o loadgen .
[[ -f media/vp8-720.ivf ]] || ./scripts/make-media.sh
./loadgen 2>&1 | head -1 || true
echo "build OK: $(pwd)/loadgen"
