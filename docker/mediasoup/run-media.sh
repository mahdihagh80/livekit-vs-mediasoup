#!/usr/bin/env bash
# Media entrypoint. CPU pin and worker list come from media.yaml.
# Default path is the file mounted at /config/media.yaml.
set -euo pipefail
CFG=${1:-/config/media.yaml}

expand_cpus() {
  local out=() part a b i
  [[ -z "${1:-}" || "$1" == "null" ]] && return 0
  IFS=',' read -ra parts <<<"$1"
  for part in "${parts[@]}"; do
    if [[ $part == *-* ]]; then a=${part%-*}; b=${part#*-}; for ((i=a;i<=b;i++)); do out+=("$i"); done
    else out+=("$part"); fi
  done
  echo "${out[@]}"
}
count_cpus() { local a; a=($(expand_cpus "$1")); echo "${#a[@]}"; }

control=$(yq e '.control_cpus' "$CFG")
workers=$(yq e '.worker_cpus // ""' "$CFG")
budget=$(yq e '.cpu_budget // ""' "$CFG")
if [[ -n "$budget" && "$budget" != "null" ]]; then
  total=$(( $(count_cpus "$control") + $(count_cpus "$workers") ))
  [[ $total -eq $(count_cpus "$budget") ]] || echo "WARN: control_cpus+worker_cpus = $total cores, cpu_budget = $(count_cpus "$budget"). Unequal budgets!" >&2
fi
exec taskset -c "$control" node /app/media/dist/index.js --config "$CFG"
