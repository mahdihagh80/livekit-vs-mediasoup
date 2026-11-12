#!/usr/bin/env bash
# NATIVE alternative to the docker "monitoring" profile (docker/compose.gen.yaml).
# Run on hosts.ctl_host: renders the config from bench.yaml (monitoring/render.sh)
# and starts a dedicated Prometheus binary, so the production Prometheus isn't
# loaded and the data stays with the results. No Grafana here: export the data
# (monitoring/export.sh) and view it on your laptop (monitoring/view-local.sh).
#   monitoring/start-prometheus.sh
source "$(dirname "$0")/../host/lib.sh"
DIR="$BENCH_ROOT/monitoring"
bash "$DIR/render.sh"
command -v prometheus >/dev/null || { echo "prometheus binary not found; use: docker compose -f docker/compose.gen.yaml --profile monitoring up -d" >&2; exit 1; }
mkdir -p "$DIR/prometheus/data"
mapfile -t ARGS <"$DIR/prometheus/args"
ARGS=("${ARGS[@]/\/etc\/prometheus\/prometheus.yml/$DIR/prometheus/prometheus.yml}")
ARGS=("${ARGS[@]/--storage.tsdb.path=\/prometheus/--storage.tsdb.path=$DIR/prometheus/data}")
nohup prometheus "${ARGS[@]}" >"$DIR/prometheus/prometheus.log" 2>&1 &
echo $! >"$DIR/prometheus/prometheus.pid"
echo "prometheus started on $PROM_LISTEN"
echo "  targets from your laptop: ssh -L 9090:$PROM_LISTEN $(hostname -s)  then http://localhost:9090/targets"
echo "  after a run:              monitoring/export.sh results/<run>   (then view-local.sh on your laptop)"
