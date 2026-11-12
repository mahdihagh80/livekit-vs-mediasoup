#!/usr/bin/env bash
# Generates the pre-encoded layers the load generator sends, as defined in
# bench.yaml (loadgen.media). Needs ffmpeg with libvpx.
# codec vp8 (default): <dir>/vp8-<height>.ivf, sent as simulcast.
# codec vp9: <dir>/vp9-<height>.ivf, sent as one SVC stream (L<n>T3_KEY: the temporal layers
#   are marked in the payload descriptor by the sender, not encoded; receivers never decode).
# Generate once and copy the SAME files to every generator, so the bytes on the
# wire are identical for both SFUs.
source "$(dirname "$0")/../../host/lib.sh"
cd "$BENCH_ROOT/client"
DIR=$(cfg .loadgen.media.dir media)
CODEC=$(cfg .loadgen.media.codec vp8)
DUR=$(cfg .loadgen.media.duration 20s); DUR=${DUR%s}
FPS=$(cfg .loadgen.media.fps 30)
GOP=$(cfg .loadgen.media.keyframe_interval 60)
case "$CODEC" in
  vp8) venc=( -c:v libvpx ) ;;
  vp9) venc=( -c:v libvpx-vp9 ) ;;
  *) echo "loadgen.media.codec must be vp8 or vp9" >&2; exit 1 ;;
esac
N=$("$YQ" e '.loadgen.media.layers | length' "$BENCH_YAML")
[[ $N -ge 1 && $N -le 3 ]] || { echo "loadgen.media.layers must have 1..3 entries" >&2; exit 1; }
mkdir -p "$DIR"
for ((i=0; i<N; i++)); do
  w=$("$YQ" e ".loadgen.media.layers[$i].width" "$BENCH_YAML")
  h=$("$YQ" e ".loadgen.media.layers[$i].height" "$BENCH_YAML")
  k=$("$YQ" e ".loadgen.media.layers[$i].kbps" "$BENCH_YAML")
  out="$DIR/$CODEC-$h.ivf"
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc2=size=${w}x${h}:rate=$FPS" -t "$DUR" \
    "${venc[@]}" -deadline realtime -cpu-used 8 -lag-in-frames 0 -auto-alt-ref 0 -error-resilient 1 \
    -b:v "${k}k" -minrate "${k}k" -maxrate "${k}k" -bufsize "$(( k / 2 ))k" \
    -g "$GOP" -keyint_min "$GOP" -pix_fmt yuv420p -f ivf "$out"
  echo "$out: ${w}x${h} ${k} kbps, $(stat -c%s "$out") bytes"
done
