#!/usr/bin/env bash
# Start LiveKit on the SFU host in a given layout, pinned to cpu.sfu (bench.yaml).
#   livekit/start.sh 1x48     one instance on 48 cores of cpu.sfu
#   livekit/start.sh 3x16     three instances, 16 cores each
# Per-instance configs are written to livekit/run/livekit-<i>.yaml: the base
# config (livekit.base_config) plus ports/keys/node_ip from bench.yaml.
# Instances are standalone (no Redis); the load generator spreads rooms over
# them round-robin. A LiveKit room always lives on ONE instance, so the webinar
# plan must use a 1xN layout.
# GOMAXPROCS: Go sets it to the number of CPUs in the taskset mask, i.e. the
# instance's core count. No environment variable needed.
source "$(dirname "$0")/../host/lib.sh"
LAYOUT=${1:-$(cfg .livekit.active_layout)}
[[ -n $LAYOUT ]] || { echo "usage: $0 <instances>x<cores> (or set livekit.active_layout). layouts: $LK_LAYOUTS" >&2; exit 1; }
N=${LAYOUT%x*}; C=${LAYOUT#*x}
CPU_ARR=($(expand_cpus "$SFU_CPUS"))
(( N * C <= ${#CPU_ARR[@]} )) || { echo "layout $LAYOUT needs $((N*C)) cores, cpu.sfu has ${#CPU_ARR[@]}" >&2; exit 1; }
[[ -x "$LIVEKIT_BIN" ]] || { echo "livekit.bin=$LIVEKIT_BIN missing (run livekit/get-binary.sh)" >&2; exit 1; }
[[ -f "$LK_BASE_CONFIG" ]] || { echo "livekit.base_config=$LK_BASE_CONFIG missing" >&2; exit 1; }
[[ "$LIVEKIT_API_KEY$LIVEKIT_API_SECRET" != *[\"\\]* ]] || { echo "livekit api_key/api_secret must not contain quotes or backslashes" >&2; exit 1; }
pgrep -f mediasoup-worker >/dev/null && { echo "mediasoup is running; stop it first (docker compose -f docker/compose.sfu.yaml --profile mediasoup down)" >&2; exit 1; }

DIR="$BENCH_ROOT/livekit"; RUN="$DIR/run"; mkdir -p "$RUN"
ulimit -n 1048576 2>/dev/null || echo "WARN: could not raise nofile (run host/10-sysctl-limits.sh, re-login)"
for ((i=0; i<N; i++)); do
  cpus=$(echo "${CPU_ARR[@]:$((i*C)):$C}" | tr ' ' ',')
  http=$((LK_HTTP_BASE + i*LK_PORT_STEP)); tcp=$((LK_TCP_BASE + i*LK_PORT_STEP)); udp=$((LK_UDP_BASE + i*LK_PORT_STEP)); prom=$((LK_METRICS_BASE + i))
  "$YQ" e ".port = $http
    | .rtc.tcp_port = $tcp | .rtc.udp_port = $udp
    | .rtc.node_ip = \"$SFU_IP\" | .rtc.use_external_ip = false
    | .keys = {\"$LIVEKIT_API_KEY\": \"$LIVEKIT_API_SECRET\"}
    | .prometheus.port = $prom | del(.prometheus_port)" "$LK_BASE_CONFIG" >"$RUN/livekit-$i.yaml"
  nohup taskset -c "$cpus" "$LIVEKIT_BIN" --config "$RUN/livekit-$i.yaml" >"$RUN/livekit-$i.log" 2>&1 &
  echo $! >"$RUN/livekit-$i.pid"
  echo "instance $i: pid $! cpus $cpus http :$http udp :$udp metrics :$prom"
done
sleep 3
for ((i=0; i<N; i++)); do
  curl -fs "localhost:$((LK_HTTP_BASE + i*LK_PORT_STEP))" >/dev/null && echo "instance $i up" || { echo "instance $i NOT up, see $RUN/livekit-$i.log"; exit 1; }
done
echo "$LAYOUT" >"$RUN/layout.txt"
echo
echo "LiveKit $LAYOUT running. On the controller host:"
echo "  scripts/run-scenario.sh livekit:$LAYOUT client/plans/<plan>.yaml"
