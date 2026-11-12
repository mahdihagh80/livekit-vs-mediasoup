#!/usr/bin/env bash
# healthcheck.sh media|signaling [config-path]
set -euo pipefail
kind=${1:-media}
cfg=${2:-/config/${kind}.yaml}
case "$kind" in
  media) key=rpc_listen ;;
  signaling) key=events_listen ;;
  *) exit 2 ;;
esac
addr=$(yq e ".$key" "$cfg")
exec node -e "fetch('http://${addr}/healthz').then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"
