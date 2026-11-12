#!/usr/bin/env bash
# Signaling entrypoint. The pin and the listen addresses come from signaling.yaml.
set -euo pipefail
CFG=${1:-/config/signaling.yaml}
exec taskset -c "$(yq e '.control_cpus' "$CFG")" /app/ms-signaling -config "$CFG"
