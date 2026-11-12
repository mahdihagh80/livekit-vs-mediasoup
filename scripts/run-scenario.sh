#!/usr/bin/env bash
# Run ONE scenario against the SFU that is currently running, then build its report.
# One process on this machine. Pinning uses cpu.gen.
#
#   scripts/run-scenario.sh livekit   client/plans/02-group-6.yaml <run-name>
#   scripts/run-scenario.sh mediasoup client/plans/02-group-6.yaml <run-name>
#
# SFU URLs, Prometheus URL, SLO and node selectors all come from bench.yaml.
# LiveKit is livekit.url (one HAProxy address).
# Works natively (client/loadgen) and inside the loadgen container (loadgen in PATH).
source "$(dirname "$0")/../host/lib.sh"
TARGET=${1:?usage: $0 <mediasoup|livekit> <plan.yaml> <run-name>}
PLAN=${2:?plan file}
if [[ -z ${3:-} && ${ALLOW_UNNAMED:-} != 1 ]]; then
  echo "usage: $0 <mediasoup|livekit> <plan.yaml> <run-name>" >&2
  echo "  <run-name> must equal the preflight run name (sfu.json 'run')." >&2
  echo "  ALLOW_UNNAMED=1 runs without a name and without an SFU snapshot: not for A/B runs." >&2
  exit 2
fi
[[ $PLAN == /* ]] || PLAN="$BENCH_ROOT/$PLAN"
LG="$BENCH_ROOT/client/loadgen"
[[ -x $LG ]] || LG=$(command -v loadgen || true)
[[ -n $LG ]] || { echo "build first: client/build.sh" >&2; exit 1; }

ARGS=("$TARGET" "$PLAN")
[[ -n ${3:-} ]] && ARGS+=("$3")
SNAP=${SFU_SNAPSHOT:-}
if [[ -z $SNAP && -n ${3:-} ]]; then
  snaps=("$BENCH_ROOT/results/preflight/$3"/*/sfu.json)
  [[ ${#snaps[@]} -eq 1 && -f ${snaps[0]} ]] && SNAP=${snaps[0]}
fi
SNAP_ARGS=()
if [[ -n $SNAP ]]; then
  SNAP_ARGS=(-sfu-snapshot "$SNAP")
  echo "SFU snapshot: $SNAP"
else
  echo "WARNING: no SFU snapshot for run '${3:-<unnamed>}' (want exactly one results/preflight/<run>/*/sfu.json, or SFU_SNAPSHOT=<path>)." >&2
  echo "WARNING: run.json will record sfu_snapshot: missing. Copy it from the SFU host after its preflight:" >&2
  echo "WARNING:   scp $SFU_IP:<bench-root>/results/preflight/${3:-<run>}/<sfu-hostname>/sfu.json $BENCH_ROOT/results/preflight/${3:-<run>}/<sfu-hostname>/" >&2
fi
mkdir -p "$BENCH_ROOT/results"
echo "REMINDER: run 'sudo host/90-preflight.sh <run-name>' on all three hosts first, with the same run name, and copy the SFU host's sfu.json here (docs/06-running.md)."
LOG=$(mktemp)
taskset -c "$GEN_CPUS" "$LG" -config "$BENCH_YAML" "${SNAP_ARGS[@]}" "${ARGS[@]}" 2>&1 | tee "$LOG"
RUN_DIR=$(grep -o 'results: .*' "$LOG" | awk '{print $NF}' | tail -1)
[[ -n $RUN_DIR ]] || { echo "could not find the run directory in the output" >&2; exit 1; }
cp "$LOG" "$RUN_DIR/ctl.log"; rm -f "$LOG"
echo
echo "results: $RUN_DIR"
