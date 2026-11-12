#!/usr/bin/env bash
# Container entrypoint: start LiveKit in a layout, in the foreground.
#   run-livekit            -> livekit.active_layout from bench.yaml
#   run-livekit 3x16       -> explicit layout
# The repo is mounted at /bench; bench.yaml is the only config. Instance i is
# pinned (taskset) to slice i of cpu.sfu; Go then sets GOMAXPROCS = slice size.
# Per-instance configs: /bench/livekit/run/livekit-<i>.yaml (base config + bench.yaml overrides).
source /bench/host/lib.sh
LAYOUT=${1:-$(cfg .livekit.active_layout)}
[[ $LAYOUT =~ ^[0-9]+x[0-9]+$ ]] || { echo "bad/missing layout '$LAYOUT' (arg or livekit.active_layout)" >&2; exit 1; }
N=${LAYOUT%x*}; C=${LAYOUT#*x}
CPU_ARR=($(expand_cpus "$SFU_CPUS"))
(( N * C <= ${#CPU_ARR[@]} )) || { echo "layout $LAYOUT needs $((N*C)) cores, cpu.sfu has ${#CPU_ARR[@]}" >&2; exit 1; }
[[ -f "$LK_BASE_CONFIG" ]] || { echo "livekit.base_config=$LK_BASE_CONFIG missing" >&2; exit 1; }
[[ "$LIVEKIT_API_KEY$LIVEKIT_API_SECRET" != *[\"\\]* ]] || { echo "api_key/api_secret must not contain quotes or backslashes" >&2; exit 1; }

RUN=/bench/livekit/run; mkdir -p "$RUN"
PIDS=()
stop() { echo "stopping LiveKit"; kill -TERM "${PIDS[@]}" 2>/dev/null; wait || true; exit 0; }
trap stop TERM INT

for ((i=0; i<N; i++)); do
  cpus=$(echo "${CPU_ARR[@]:$((i*C)):$C}" | tr ' ' ',')
  http=$((LK_HTTP_BASE + i*LK_PORT_STEP)); tcp=$((LK_TCP_BASE + i*LK_PORT_STEP)); udp=$((LK_UDP_BASE + i*LK_PORT_STEP)); prom=$((LK_METRICS_BASE + i))
  "$YQ" e ".port = $http
    | .rtc.tcp_port = $tcp | .rtc.udp_port = $udp
    | .rtc.node_ip = \"$SFU_IP\" | .rtc.use_external_ip = false
    | .keys = {\"$LIVEKIT_API_KEY\": \"$LIVEKIT_API_SECRET\"}
    | .prometheus.port = $prom | del(.prometheus_port)" "$LK_BASE_CONFIG" >"$RUN/livekit-$i.yaml"
  # stdout/stderr of all instances go to the container log, prefixed
  # (process substitution, so $! is livekit-server itself: taskset execs it)
  taskset -c "$cpus" livekit-server --config "$RUN/livekit-$i.yaml" > >(sed -u "s/^/[lk$i] /") 2>&1 &
  PIDS+=($!)
  echo "instance $i: cpus $cpus http :$http udp :$udp metrics :$prom"
done
echo "$LAYOUT" >"$RUN/layout.txt"
echo "LiveKit $LAYOUT running (controller: scripts/run-scenario.sh livekit:$LAYOUT <plan> <run-name>)"

# If any instance exits, stop all: a partial layout would skew the benchmark.
set +e
wait -n "${PIDS[@]}"
echo "a LiveKit instance exited; stopping the others" >&2
kill -TERM "${PIDS[@]}" 2>/dev/null; wait
exit 1
