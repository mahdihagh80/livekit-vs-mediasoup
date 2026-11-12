#!/usr/bin/env bash
# Native runs: downloads the open-source livekit-server release given by
# livekit.version in bench.yaml and installs it at livekit.bin.
# (Docker runs use the official image of the same version: docker/livekit/Dockerfile.)
#   livekit/get-binary.sh
source "$(dirname "$0")/../host/lib.sh"
VER=$(cfg .livekit.version)
[[ -n $VER ]] || { echo "set livekit.version in bench.yaml (e.g. 1.13.11)" >&2; exit 1; }
VER=${VER#v}
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
curl -fsSL "https://github.com/livekit/livekit/releases/download/v$VER/livekit_${VER}_linux_amd64.tar.gz" | tar -xz -C "$TMP"
mkdir -p "$(dirname "$LIVEKIT_BIN")"
install -m 0755 "$TMP/livekit-server" "$LIVEKIT_BIN"
echo "installed $("$LIVEKIT_BIN" --version) -> $LIVEKIT_BIN"
