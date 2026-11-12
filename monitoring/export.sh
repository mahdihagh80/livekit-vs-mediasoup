#!/usr/bin/env bash
# Run on hosts.ctl_host. Packs the bench Prometheus TSDB into one tarball you can
# copy to your laptop and open with monitoring/view-local.sh. Takes a TSDB
# snapshot through the admin API (no downtime, consistent blocks + head).
#   monitoring/export.sh                 # -> results/metrics/prom-<host>-<time>.tar.zst
#   monitoring/export.sh <run-dir>...    # also bundles those results/ dirs (report, steps.csv)
source "$(dirname "$0")/../host/lib.sh"
DIR="$BENCH_ROOT/monitoring"
OUT_DIR="$BENCH_ROOT/results/metrics"; mkdir -p "$OUT_DIR"
NAME="prom-$(hostname -s)-$(date +%Y%m%d-%H%M)"

name=$(curl -fsS -X POST "$PROM_LOCAL/api/v1/admin/tsdb/snapshot" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p') \
  || { echo "snapshot failed: is Prometheus up on $PROM_LOCAL (started by start-prometheus.sh / compose, which enable the admin API)?" >&2; exit 1; }
[[ -n $name ]] || { echo "snapshot API returned no name" >&2; exit 1; }

STAGE=$(mktemp -d); trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/$NAME/results"
# Native: data dir is monitoring/prometheus/data. Docker: copy out of the container
# (its volume is root-owned), then drop the snapshot inside it.
SNAP="$DIR/prometheus/data/snapshots/$name"
CTR=sfu-bench-gen-prometheus-1
if [[ -d $SNAP ]]; then
  mv "$SNAP" "$STAGE/$NAME/tsdb"
elif docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$CTR"; then
  docker cp "$CTR:/prometheus/snapshots/$name" "$STAGE/$NAME/tsdb" >/dev/null
  docker exec "$CTR" rm -rf "/prometheus/snapshots/$name"
else
  echo "snapshot $name not found (native data dir or container $CTR)" >&2; exit 1
fi
cp "$DIR/prometheus/prometheus.yml" "$STAGE/$NAME/" 2>/dev/null || true
for r in "$@"; do [[ -d $r ]] && cp -r "$r" "$STAGE/$NAME/results/"; done

if command -v zstd >/dev/null; then EXT=tar.zst; COMP=(--zstd); else EXT=tar.gz; COMP=(-z); fi
tar -C "$STAGE" -c "${COMP[@]}" -f "$OUT_DIR/$NAME.$EXT" "$NAME"
echo "exported: $OUT_DIR/$NAME.$EXT ($(du -h "$OUT_DIR/$NAME.$EXT" | cut -f1))"
echo "on your laptop:"
echo "  scp $(hostname -s):$OUT_DIR/$NAME.$EXT . && monitoring/view-local.sh $NAME.$EXT"
