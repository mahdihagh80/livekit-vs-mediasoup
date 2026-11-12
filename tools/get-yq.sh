#!/usr/bin/env bash
# Installs yq (mikefarah v4, a single static binary) into tools/bin/yq.
# All scripts use it to read bench.yaml. If a host has no internet access,
# download https://github.com/mikefarah/yq/releases/download/v4.47.1/yq_linux_amd64
# elsewhere and copy it to tools/bin/yq (chmod +x).
set -euo pipefail
VER=v4.47.1
DIR="$(cd "$(dirname "$0")" && pwd)/bin"
mkdir -p "$DIR"
curl -fsSL -o "$DIR/yq" "https://github.com/mikefarah/yq/releases/download/$VER/yq_linux_amd64"
chmod +x "$DIR/yq"
"$DIR/yq" --version
