#!/usr/bin/env bash
# Run one plan in this container, pinned to cpu.gen.
#   loadgen args follow: mediasoup client/plans/00-smoke.yaml [run-name]
source /bench/host/lib.sh
[[ -f /bench/client/media/vp8-720.ivf || -n "$(ls /bench/client/media/*.ivf 2>/dev/null)" ]] \
  || { echo "no media files: docker compose -f docker/compose.gen.yaml run --rm make-media" >&2; exit 1; }
[[ $# -ge 2 ]] || { echo "usage: run-agent.sh <mediasoup|livekit> <plan.yaml> [run-name]" >&2; exit 2; }
exec taskset -c "$GEN_CPUS" loadgen -config "$BENCH_YAML" "$@"
