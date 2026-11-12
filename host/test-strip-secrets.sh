#!/usr/bin/env bash
# Tests strip_secrets (lib.sh) on the fake configs in host/testdata.
#   host/test-strip-secrets.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# lib.sh reads bench.yaml next to it, so source a copy that sees the fake one.
mkdir -p "$TMP/root/host" "$TMP/root/tools/bin" "$TMP/out"
cp "$HERE/lib.sh" "$TMP/root/host/lib.sh"
cp "$HERE/testdata/bench.yaml" "$TMP/root/bench.yaml"
if [[ -x "$ROOT/tools/bin/yq" ]]; then ln -s "$ROOT/tools/bin/yq" "$TMP/root/tools/bin/yq"; fi
# shellcheck source=lib.sh
source "$TMP/root/host/lib.sh"

fails=0
bad() { echo "FAIL: $*"; fails=$((fails+1)); }

for f in livekit bench; do
  if ! strip_secrets "$HERE/testdata/$f.yaml" "$TMP/out/$f.yaml" >/dev/null; then bad "$f: strip_secrets failed"; continue; fi
  if grep -q fake "$TMP/out/$f.yaml"; then bad "$f: a fake secret survived"; fi
done
[[ $("$YQ" e '.loadgen.report.slo.video_delay_p95' "$TMP/out/bench.yaml") == 100ms ]] || bad "bench: SLO lost"
[[ $("$YQ" e '.loadgen.report.slo.join_success' "$TMP/out/bench.yaml") == 0.995 ]] || bad "bench: SLO lost"
[[ $("$YQ" e '.livekit.url' "$TMP/out/bench.yaml") == ws://10.0.0.1:7880 ]] || bad "bench: livekit.url lost"
[[ $("$YQ" e '.rtc.tcp_port' "$TMP/out/livekit.yaml") == 7881 ]] || bad "livekit: rtc lost"
[[ $("$YQ" e 'has("keys")' "$TMP/out/livekit.yaml") == false ]] || bad "livekit: keys kept"

rc=0
strip_secrets "$HERE/testdata/planted.yaml" "$TMP/out/planted.yaml" >/dev/null || rc=$?
[[ $rc -ne 0 ]] || bad "planted: did not fail closed"
[[ ! -e "$TMP/out/planted.yaml" ]] || bad "planted: output left behind"

rc=0
strip_secrets "$HERE/testdata/missing.yaml" "$TMP/out/missing.yaml" >/dev/null 2>&1 || rc=$?
[[ $rc -ne 0 && ! -e "$TMP/out/missing.yaml" ]] || bad "missing input: no failure"

leftover=$(find "${TMPDIR:-/tmp}" -maxdepth 1 -newer "$TMP" -name 'tmp.*' -type f -perm 600 2>/dev/null | wc -l)
[[ $leftover -eq 0 ]] || bad "temp value files left behind"

if [[ $fails -ne 0 ]]; then echo "test-strip-secrets: $fails failure(s)"; exit 1; fi
echo "test-strip-secrets: ok"
