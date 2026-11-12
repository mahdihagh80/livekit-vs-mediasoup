#!/usr/bin/env bash
# Run one plan in this process, pinned to cpu.gen.
#   client/start-agent.sh mediasoup client/plans/00-smoke.yaml [run-name]
#   client/start-agent.sh livekit client/plans/00-smoke.yaml
source "$(dirname "$0")/../host/lib.sh"
DIR="$BENCH_ROOT/client"
[[ -x "$DIR/loadgen" ]] || { echo "build first: client/build.sh" >&2; exit 1; }
[[ -f "$DIR/media/vp8-720.ivf" ]] || { echo "media missing: client/scripts/make-media.sh" >&2; exit 1; }
[[ $# -ge 2 ]] || { echo "usage: $0 <mediasoup|livekit> <plan.yaml> [run-name]" >&2; exit 2; }
ulimit -n 1048576 2>/dev/null || echo "WARN: could not raise nofile (run host/10-sysctl-limits.sh, re-login)"
cd "$DIR" || exit 1
exec taskset -c "$GEN_CPUS" ./loadgen -config "$BENCH_YAML" "$@"
