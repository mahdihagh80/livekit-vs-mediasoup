#!/usr/bin/env bash
# Record what the SFU host is running (image, version, cpuset, config) in
# results/preflight/<run>/<host>/sfu.json, for the loadgen's -sfu-snapshot.
# Read-only: changes nothing on the host. Called by 90-preflight.sh on the SFU.
#   sudo host/91-sfu-snapshot.sh <run-name>
source "$(dirname "$0")/lib.sh"
need_root

RUN=${1:?usage: 91-sfu-snapshot.sh <run-name>}
OUT="$BENCH_ROOT/results/preflight/$RUN/$(hostname)"
mkdir -p "$OUT"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
chmod 700 "$TMP"
J="$TMP/sfu.yaml"
echo '{}' >"$J"

put() { if [[ -n ${2-} ]]; then V=$2 "$YQ" -i "$1 = strenv(V)" "$J"; else "$YQ" -i "$1 = null" "$J"; fi; }
putnum() { if [[ ${2-} =~ ^[0-9]+$ ]]; then "$YQ" -i "$1 = $2" "$J"; else "$YQ" -i "$1 = null" "$J"; fi; }
want() { [[ -n ${2-} ]] || c_warn "sfu snapshot: $1 unknown, recorded as null"; }

inspect() { docker inspect -f "$2" "$1" 2>/dev/null || true; }
mount_src() { inspect "$1" "{{range .Mounts}}{{if eq .Destination \"$2\"}}{{.Source}}{{end}}{{end}}"; }

# host_path <cid> <path in container>: the host path behind the container's
# longest matching mount, or nothing.
host_path() {
  local cid=$1 p=$2 dst src best='' bsrc=''
  while IFS=$'\t' read -r dst src; do
    [[ -n $dst && ( $p == "$dst" || $p == "$dst"/* ) && ${#dst} -gt ${#best} ]] && { best=$dst; bsrc=$src; }
  done < <(inspect "$cid" '{{range .Mounts}}{{.Destination}}{{"\t"}}{{.Source}}{{"\n"}}{{end}}')
  [[ -z $best ]] || echo "$bsrc${p#"$best"}"
}

# config_arg <pid>: the --config argument of a process, made absolute.
config_arg() {
  local pid=$1 a next=0 c=''
  while IFS= read -r -d '' a; do
    if (( next )); then c=$a; break; fi
    case $a in --config) next=1 ;; --config=*) c=${a#--config=}; break ;; esac
  done <"/proc/$pid/cmdline"
  [[ -z $c || $c == /* ]] && echo "$c" || echo "$(readlink "/proc/$pid/cwd")/$c"
}

lk_pids() {
  local p
  for p in $(pgrep -f livekit-server); do
    tr '\0' ' ' <"/proc/$p/cmdline" 2>/dev/null | grep -q -- '--config' && echo "$p"
  done
  return 0
}

# embed_config <yaml path> <host path of the config> <path key> <sha key> [config key]
embed_config() {
  local key=$1 f=$2 pkey=$3 skey=$4 ckey=${5-} s
  put "$key.$pkey" "$f"
  if [[ -z $f || ! -r $f ]]; then
    c_warn "sfu snapshot: $key config not found (${f:-no path}), recorded as null"
    put "$key.$skey" ''; [[ -n $ckey ]] && put "$key.$ckey" ''
    return 0
  fi
  s="$TMP/stripped.yaml"
  if ! strip_secrets "$f" "$s"; then
    FAILED=1; put "$key.$skey" ''; [[ -n $ckey ]] && put "$key.$ckey" ''
    return 0
  fi
  put "$key.$skey" "$(sha256sum "$s" | cut -d' ' -f1)"
  [[ -n $ckey ]] && F=$s "$YQ" -i "$key.$ckey = load(strenv(F))" "$J"
  rm -f "$s"
}

"$YQ" -i '.schema = 1' "$J"
put .taken "$(date -Is)"
put .host "$(hostname)"
put .run "$RUN"
put .kernel "$(uname -r)"
smt=$(cat /sys/devices/system/cpu/smt/active 2>/dev/null || true); want smt_active "$smt"; put .smt_active "$smt"
gov=$(cat /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor 2>/dev/null | sort -u | paste -sd, || true); want governor "$gov"; put .governor "$gov"

have_docker=0
command -v docker >/dev/null 2>&1 && docker ps -q >/dev/null 2>&1 && have_docker=1
[[ $have_docker -eq 1 ]] || c_warn "sfu snapshot: docker not available, looking for native processes only"
first_container() { [[ $have_docker -eq 0 ]] || docker ps -q --filter "label=com.docker.compose.service=$1" | head -1 || true; }

# --- LiveKit
lk=''
lk_cid=$(first_container livekit)
if [[ -n $lk_cid ]]; then
  lk=docker
  put .livekit.mode docker
  img=$(inspect "$lk_cid" '{{.Config.Image}}')
  ver=$(inspect "$lk_cid" '{{index .Config.Labels "org.opencontainers.image.version"}}')
  [[ -z $ver && $img == *:* ]] && ver=${img##*:}
  want "livekit version" "$ver"; put .livekit.version "$ver"
  put .livekit.image "$img"
  put .livekit.image_id "$(inspect "$lk_cid" '{{.Image}}')"
  put .livekit.cpuset "$(inspect "$lk_cid" '{{.HostConfig.CpusetCpus}}')"
  cfgp=$(mount_src "$lk_cid" /etc/livekit.yaml)
  if [[ -z $cfgp ]]; then
    for p in $(lk_pids); do
      grep -q "$lk_cid" "/proc/$p/cgroup" 2>/dev/null || continue
      c=$(config_arg "$p"); [[ -n $c ]] && cfgp=$(host_path "$lk_cid" "$c")
      break
    done
  fi
  embed_config .livekit "$cfgp" config_path config_sha256 config
else
  pids=($(lk_pids))
  if (( ${#pids[@]} > 0 )); then
    lk=native
    p=${pids[0]}
    (( ${#pids[@]} > 1 )) && c_warn "sfu snapshot: ${#pids[@]} livekit-server processes, recording pid $p"
    putnum .livekit.instances "${#pids[@]}"
    put .livekit.mode native
    c_warn "sfu snapshot: native livekit version unknown, recorded as null (binary_sha256 identifies it)"
    put .livekit.version ''
    put .livekit.image ''; put .livekit.image_id ''; put .livekit.cpuset ''
    exe=$(readlink "/proc/$p/exe" || true)
    put .livekit.binary "$exe"
    put .livekit.binary_sha256 "$(sha256sum "/proc/$p/exe" 2>/dev/null | cut -d' ' -f1 || true)"
    aff=$(taskset -pc "$p" 2>/dev/null | sed 's/.*: *//' || true); want "livekit affinity" "$aff"
    put .livekit.affinity "$aff"
    embed_config .livekit "$(config_arg "$p")" config_path config_sha256 config
  fi
fi

# --- mediasoup
ms=''
ms_cid=$(first_container mediasoup-media)
if [[ -n $ms_cid ]]; then
  ms=mediasoup
  put .mediasoup.image "$(inspect "$ms_cid" '{{.Config.Image}}')"
  put .mediasoup.image_id "$(inspect "$ms_cid" '{{.Image}}')"
  v=$(timeout 20 docker exec "$ms_cid" node -p "require('/app/media/node_modules/mediasoup/package.json').version" 2>/dev/null || true)
  [[ $v =~ ^[0-9][0-9A-Za-z.+-]*$ ]] || v=''
  want "mediasoup version" "$v"; put .mediasoup.mediasoup_version "$v"
  put .mediasoup.cpuset "$(inspect "$ms_cid" '{{.HostConfig.CpusetCpus}}')"
  putnum .mediasoup.nano_cpus "$(inspect "$ms_cid" '{{.HostConfig.NanoCpus}}')"
  embed_config .mediasoup "$(mount_src "$ms_cid" /config/media.yaml)" media_config_path media_config_sha256 media_config
  sig_cid=$(first_container mediasoup-signaling)
  sigp=''
  [[ -n $sig_cid ]] && sigp=$(mount_src "$sig_cid" /config/signaling.yaml)
  embed_config .mediasoup "$sigp" signaling_config_path signaling_config_sha256
elif pgrep -f mediasoup-worker >/dev/null; then
  c_warn "sfu snapshot: mediasoup-worker runs outside the compose service, not recorded"
fi

case "$lk$ms" in
  '') sfu=none ;;
  docker|native) sfu=livekit ;;
  mediasoup) sfu=mediasoup ;;
  *) sfu=both ;;
esac
put .sfu "$sfu"
[[ $sfu == none || $sfu == both ]] && c_warn "sfu snapshot: sfu=$sfu"

if [[ $FAILED -ne 0 ]]; then
  c_fail "sfu snapshot not written ($OUT/sfu.json)"
  exit 1
fi
"$YQ" -o=json e '.' "$J" >"$OUT/sfu.json.tmp" && mv "$OUT/sfu.json.tmp" "$OUT/sfu.json"
c_ok "sfu snapshot sfu=$sfu -> $OUT/sfu.json"
