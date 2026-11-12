# shellcheck shell=bash
# Shared helpers for all scripts — source, don't run.
# Reads bench.yaml (repo root) with yq (mikefarah v4) into plain shell
# variables. Nothing is exported; no environment variables are used as config.
set -euo pipefail

BENCH_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_YAML="$BENCH_ROOT/bench.yaml"

# --- yq ------------------------------------------------------------------------
if [[ -x "$BENCH_ROOT/tools/bin/yq" ]]; then YQ="$BENCH_ROOT/tools/bin/yq"
elif command -v yq >/dev/null 2>&1; then YQ=$(command -v yq)
else
  echo "yq (mikefarah v4) not found: run tools/get-yq.sh, or copy the yq binary to $BENCH_ROOT/tools/bin/yq" >&2; exit 1
fi
"$YQ" --version 2>&1 | grep -q mikefarah || { echo "$YQ is not mikefarah/yq v4 (the python 'yq' has a different syntax). Run tools/get-yq.sh" >&2; exit 1; }

[[ -f "$BENCH_YAML" ]] || { echo "missing $BENCH_YAML (cp bench.yaml.example bench.yaml and edit it)" >&2; exit 1; }

# cfg <yq-path> [default]  -> scalar value ("" / default when null)
cfg() { local v; v=$("$YQ" e "$1 // \"\"" "$BENCH_YAML"); [[ -n "$v" ]] && echo "$v" || echo "${2:-}"; }
# cfg_list <yq-path>       -> space-separated list
cfg_list() { "$YQ" e "($1 // []) | join(\" \")" "$BENCH_YAML"; }

# --- hosts / nic / cpu ------------------------------------------------------------
SFU_IP=$(cfg .hosts.sfu_ip)
GEN_IPS=$(cfg_list .hosts.gen_ips)
CTL_HOST=$(cfg .hosts.ctl_host)
NODE_EXPORTER_PORT=$(cfg .hosts.node_exporter_monitoring_port 9100)
SFU_NIC=$(cfg .nic.sfu.name)
SFU_NIC_SLAVES=$(cfg_list .nic.sfu.slaves)
GEN_NIC=$(cfg .nic.gen.name)
GEN_NIC_SLAVES=$(cfg_list .nic.gen.slaves)
IRQ_CPUS=$(cfg .cpu.sfu_irq)
SFU_CPUS=$(cfg .cpu.sfu)
GEN_IRQ_CPUS=$(cfg .cpu.gen_irq)
GEN_CPUS=$(cfg .cpu.gen)
CHRONY_SERVER=$(cfg .chrony.server "$SFU_IP")

# --- monitoring ---------------------------------------------------------------------
PROM_URL=$(cfg .monitoring.prometheus_url "http://$CTL_HOST:9090")
PROM_SCRAPE=$(cfg .monitoring.scrape_interval 5s)
PROM_RETENTION=$(cfg .monitoring.retention 90d)
# Prometheus listens on prometheus_url's host:port. Keep it 127.0.0.1 on the
# controller: view it via `ssh -L` or download it with monitoring/export.sh.
PROM_LISTEN=${PROM_URL#*://}; PROM_LISTEN=${PROM_LISTEN%%/*}
PROM_LOCAL="http://${PROM_LISTEN/#0.0.0.0/127.0.0.1}"
GEN_GW_PORT=$(cfg .monitoring.gen_gateway_port 30699)

# --- livekit --------------------------------------------------------------------------
LIVEKIT_BIN=$(cfg .livekit.bin /opt/sfu-bench/livekit-server)
LK_BASE_CONFIG=$(cfg .livekit.base_config livekit/livekit.base.yaml)
[[ "$LK_BASE_CONFIG" == /* ]] || LK_BASE_CONFIG="$BENCH_ROOT/$LK_BASE_CONFIG"
LIVEKIT_API_KEY=$(cfg .livekit.api_key)
LIVEKIT_API_SECRET=$(cfg .livekit.api_secret)
LK_LAYOUTS=$(cfg_list .livekit.layouts)
LK_HTTP_BASE=$(cfg .livekit.http_port_base 7880)
LK_TCP_BASE=$(cfg .livekit.tcp_port_base 7881)
LK_UDP_BASE=$(cfg .livekit.udp_port_base 7882)
LK_PORT_STEP=$(cfg .livekit.port_step 10)
LK_METRICS_BASE=$(cfg .livekit.monitoring_port_base 6789)

# --- mediasoup ----------------------------------------------------------------------
MS_METRICS_PORT=$(cfg .mediasoup.media.monitoring_listen 0.0.0.0:9310); MS_METRICS_PORT=${MS_METRICS_PORT##*:}
MS_SIG_METRICS_PORT=$(cfg .mediasoup.signaling.monitoring_listen 0.0.0.0:9311); MS_SIG_METRICS_PORT=${MS_SIG_METRICS_PORT##*:}

# --- loadgen ----------------------------------------------------------------------------
AGENT_PORT=$(cfg .loadgen.monitoring_port 9200)

# --- output helpers -----------------------------------------------------------------------
c_ok()   { printf '\e[32m[ OK ]\e[0m %s\n' "$*"; }
c_warn() { printf '\e[33m[WARN]\e[0m %s\n' "$*"; }
c_fail() { printf '\e[31m[FAIL]\e[0m %s\n' "$*"; FAILED=1; }
c_info() { printf '\e[36m[INFO]\e[0m %s\n' "$*"; }
FAILED=0

# strip_secrets <in.yaml> <out.yaml>: copy a YAML file without secrets or
# comments. Deletes every map entry whose key looks secret (LiveKit's whole
# `keys:` map included), then fails closed: if any deleted string or map key
# still appears in <out>, <out> is removed and it returns 1. Never prints a value.
STRIP_SECRETS_RE='(?i)^keys$|secret|password|passwd|token|api_?key|private_?key|credential'
strip_secrets() (
  local in=$1 out=$2 vals rc=0
  local sel='.. | select((key | tag) == "!!str" and (key | test(strenv(STRIP_SECRETS_RE))))'
  vals=$(mktemp) || { c_fail "strip_secrets: mktemp failed"; return 1; }
  trap 'rm -f "$vals"' EXIT
  chmod 600 "$vals"
  if ! STRIP_SECRETS_RE=$STRIP_SECRETS_RE "$YQ" e "[$sel] | .. | ((select(tag == \"!!map\") | keys | .[]), select(tag == \"!!str\"))" "$in" >"$vals" 2>/dev/null; then
    c_fail "strip_secrets: cannot read $in"; return 1
  fi
  sed -i '/^[[:space:]]*$/d' "$vals"
  if ! STRIP_SECRETS_RE=$STRIP_SECRETS_RE "$YQ" e "del($sel) | ... comments = \"\"" "$in" >"$out" 2>/dev/null; then
    rm -f "$out"; c_fail "strip_secrets: cannot strip $in"; return 1
  fi
  grep -qF -f "$vals" -- "$out" || rc=$?
  if [[ $rc -ne 1 ]]; then
    rm -f "$out"; c_fail "strip_secrets: a secret from $in survived stripping; $out not written"; return 1
  fi
)

need_root() { [[ $EUID -eq 0 ]] || { echo "run as root (sudo $0 $*)" >&2; exit 1; }; }

# expand "0-3,8,10-11" -> "0 1 2 3 8 10 11"
expand_cpus() {
  local out=() part a b
  IFS=',' read -ra parts <<<"$1"
  for part in "${parts[@]}"; do
    if [[ $part == *-* ]]; then a=${part%-*}; b=${part#*-}; for ((i=a;i<=b;i++)); do out+=("$i"); done
    else out+=("$part"); fi
  done
  echo "${out[@]}"
}
count_cpus() { local a; a=($(expand_cpus "$1")); echo "${#a[@]}"; }

# physical NICs behind a (possibly bonded) interface
phys_nics() { # <nic> <slaves>
  if [[ -n "${2:-}" ]]; then echo "$2"; elif [[ -d /sys/class/net/$1/bonding ]]; then cat /sys/class/net/$1/bonding/slaves; else echo "$1"; fi
}

# role of this host: sfu | gen
host_role() {
  local ips; ips=" $(hostname -I) "
  if [[ "$ips" == *" $SFU_IP "* ]]; then echo sfu; return; fi
  for g in $GEN_IPS; do [[ "$ips" == *" $g "* ]] && { echo gen; return; }; done
  echo unknown
}

# this host's bench IP (hosts.sfu_ip or its hosts.gen_ips entry); exporters bind to it, not 0.0.0.0
my_ip() {
  local ips; ips=" $(hostname -I) "
  if [[ "$ips" == *" $SFU_IP "* ]]; then echo "$SFU_IP"; return; fi
  for g in $GEN_IPS; do [[ "$ips" == *" $g "* ]] && { echo "$g"; return; }; done
  echo 127.0.0.1
}
