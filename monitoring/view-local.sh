#!/usr/bin/env bash
# LAPTOP ONLY (needs docker, not bench.yaml). Opens an export from
# monitoring/export.sh in a local Prometheus + Grafana bound to 127.0.0.1.
#   monitoring/view-local.sh prom-gen-1-20261026-1200.tar.zst
#   monitoring/view-local.sh --down
# Then open http://localhost:3001 (dashboard "SFU bench"); set the time range to
# the run (results/<run>/steps.csv has the step start/end times, in UTC).
set -euo pipefail
DIR="$(cd "$(dirname "$0")/local" && pwd)"
COMPOSE=(docker compose -f "$DIR/compose.yaml")
if [[ ${1:-} == --down ]]; then "${COMPOSE[@]}" down; exit 0; fi
ARCHIVE=${1:?usage: $0 <prom-export.tar.zst|tar.gz> | --down}
[[ -f $ARCHIVE ]] || { echo "no such file: $ARCHIVE" >&2; exit 1; }

# data/ gets root-owned files from the container: wipe it through the container too.
[[ -d $DIR/data ]] && "${COMPOSE[@]}" run --rm --no-deps --entrypoint sh prometheus -c 'rm -rf /prometheus/* /prometheus/.[!.]*' >/dev/null 2>&1 || true
"${COMPOSE[@]}" down >/dev/null 2>&1 || true
rm -rf "$DIR/data" "$DIR/extract"; mkdir -p "$DIR/data" "$DIR/extract"
case $ARCHIVE in
  *.zst) tar --zstd -xf "$ARCHIVE" -C "$DIR/extract" ;;
  *)     tar -xzf "$ARCHIVE" -C "$DIR/extract" ;;
esac
SRC=$(find "$DIR/extract" -mindepth 2 -maxdepth 2 -type d -name tsdb | head -1)
[[ -n $SRC ]] || { echo "archive has no */tsdb directory" >&2; exit 1; }
cp -a "$SRC/." "$DIR/data/"
[[ -d $SRC/../results ]] && echo "run reports: $(dirname "$SRC")/results/"

"${COMPOSE[@]}" up -d
for _ in $(seq 30); do curl -fs localhost:9091/-/ready >/dev/null 2>&1 && break; sleep 1; done
from=$(cat "$DIR"/data/*/meta.json 2>/dev/null | grep -o '"minTime": *[0-9]*' | tr -dc '0-9\n' | sort -n | head -1 || true)
to=$(cat "$DIR"/data/*/meta.json 2>/dev/null | grep -o '"maxTime": *[0-9]*' | tr -dc '0-9\n' | sort -n | tail -1 || true)
if [[ -n $from && -n $to ]]; then
  echo "data:       $(date -d @$((from/1000))) .. $(date -d @$((to/1000)))"
  echo "Grafana:    http://localhost:3001/d/sfu-bench?from=$from&to=$to"
else
  echo "Grafana:    http://localhost:3001/d/sfu-bench   (set the time range to your run)"
fi
echo "Prometheus: http://localhost:9091"
echo "stop:       $0 --down"
