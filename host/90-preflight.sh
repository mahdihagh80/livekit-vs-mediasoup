#!/usr/bin/env bash
# STEP 7, and AGAIN before EVERY run, on ALL THREE hosts (root): verify the
# host state and save a snapshot next to the results. Exit code != 0 means fix
# something first. A run whose preflight failed is not comparable.
#   sudo host/90-preflight.sh [run-name]
source "$(dirname "$0")/lib.sh"
need_root

RUN=${1:-adhoc-$(date +%Y%m%d-%H%M)}
OUT="$BENCH_ROOT/results/preflight/$RUN/$(hostname)"
mkdir -p "$OUT"
ROLE=$(host_role)
case $ROLE in
  sfu) NIC=$SFU_NIC; SLAVES=$SFU_NIC_SLAVES; IRQC=$IRQ_CPUS ;;
  gen) NIC=$GEN_NIC; SLAVES=$GEN_NIC_SLAVES; IRQC=$GEN_IRQ_CPUS ;;
  *) c_fail "host matches neither hosts.sfu_ip nor hosts.gen_ips (bench.yaml)"; exit 1 ;;
esac
c_info "preflight $(hostname) role=$ROLE run=$RUN"

# --- sysctl / limits
chk_sysctl() { local k=$1 want=$2 v; v=$(sysctl -n "$k" 2>/dev/null | awk '{print $1}'); [[ "${v:-0}" -ge "$want" ]] && c_ok "$k=$v" || c_fail "$k=$v (want >= $want; run host/10-sysctl-limits.sh)"; }
chk_sysctl net.core.rmem_max 67108864
chk_sysctl net.core.wmem_max 67108864
chk_sysctl net.core.netdev_max_backlog 250000
[[ $(ulimit -Hn) -ge 1048576 ]] && c_ok "nofile hard=$(ulimit -Hn)" || c_warn "nofile hard=$(ulimit -Hn) in this shell (run scripts raise it)"

# --- CPU
govs=$(cat /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor 2>/dev/null | sort -u | paste -sd,)
[[ -z "$govs" || "$govs" == performance ]] && c_ok "governor=${govs:-n/a}" || c_fail "governor=$govs (run host/20-cpu.sh)"
systemctl is-active --quiet irqbalance && c_fail "irqbalance is running (run host/20-cpu.sh)" || c_ok "irqbalance off"

# --- NIC
IRQ_SET=" $(expand_cpus "$IRQC") "
for n in $(phys_nics "$NIC" "$SLAVES"); do
  ethtool -n "$n" rx-flow-hash udp4 2>/dev/null | grep -q "L4 bytes 0 & 1" && c_ok "$n udp4 hash includes ports" || c_fail "$n udp4 RSS hash does not include ports (run host/30-nic.sh)"
  cur_rx=$(ethtool -g "$n" | awk '/Current/{c=1} c&&/^RX:/{print $2; exit}'); max_rx=$(ethtool -g "$n" | awk '/Pre-set/{p=1} p&&/^RX:/{print $2; exit}')
  [[ "$cur_rx" == "$max_rx" ]] && c_ok "$n rx ring $cur_rx (max)" || c_warn "$n rx ring $cur_rx < max $max_rx"
  bad=0
  for f in /sys/class/net/"$n"/device/msi_irqs/*; do
    [[ -e "$f" ]] || continue
    irq=$(basename "$f"); [[ -e /proc/irq/$irq ]] || continue   # unused MSI vector: nothing to pin
    name=$(awk -v irq="$irq:" '$1==irq {print $NF}' /proc/interrupts); [[ "$name" =~ async|misc|fw|ctrl ]] && continue
    aff=$(cat /proc/irq/$irq/smp_affinity_list 2>/dev/null || echo "?")
    [[ "$IRQ_SET" == *" $aff "* ]] || bad=$((bad+1))
  done
  [[ $bad -eq 0 ]] && c_ok "$n all queue IRQs on IRQ cores ($IRQC)" || c_fail "$n $bad queue IRQs not pinned to a single IRQ core (run host/30-nic.sh)"
  ethtool -S "$n" >"$OUT/ethtool-S-$n.before.txt" 2>&1 || true
done

# --- time
off=$(chronyc tracking 2>/dev/null | awk '/System time/{print $4}')
if [[ $ROLE == gen ]]; then
  # Limit 1 ms: gens follow an external NTP source and wander
  # 150-700 us; isolated rooms measure latency on one host clock anyway.
  awk -v o="${off:-1}" 'BEGIN{exit !(o < 0.001)}' && c_ok "chrony offset ${off}s" || c_fail "chrony offset ${off:-?}s >= 1ms (run host/40-chrony.sh, wait)"
else
  c_ok "chrony offset ${off:-?}s (SFU: informational)"
fi

# --- monitoring
curl -fs "localhost:$NODE_EXPORTER_PORT/metrics" >/dev/null && c_ok "node_exporter :$NODE_EXPORTER_PORT" || c_fail "node_exporter not on :$NODE_EXPORTER_PORT (run host/50-node-exporter.sh)"

# --- noise: nothing else should be eating CPU on the SFU or generators
busy=$(ps -eo pcpu,comm --sort=-pcpu | awk 'NR>1 && $1>5 {printf "%s(%s%%) ", $2, $1}')
case $ROLE in
  sfu) [[ -z "$busy" ]] && c_ok "no process above 5% CPU" || c_warn "busy processes before the run: $busy" ;;
  gen) [[ -z "$busy" ]] && c_ok "no process above 5% CPU" || c_warn "busy processes: $busy" ;;
esac
if [[ $ROLE == sfu ]]; then
  pgrep -f livekit-server >/dev/null && c_warn "livekit-server is running (ok only if this is the LiveKit run)"
  pgrep -f mediasoup-worker >/dev/null && c_warn "mediasoup-worker is running (ok only if this is the mediasoup run)"
fi

# --- snapshot
{ uname -a; echo; sysctl -a 2>/dev/null | grep -E '^net\.core|^net\.ipv4\.udp'; } >"$OUT/sysctl.txt"
cat /proc/interrupts >"$OUT/interrupts.txt"
chronyc tracking >"$OUT/chrony.txt" 2>&1 || true
strip_secrets "$BENCH_YAML" "$OUT/bench.yaml" || FAILED=1
[[ $ROLE != sfu ]] || "$(dirname "$0")/91-sfu-snapshot.sh" "$RUN" || FAILED=1
(cd "$BENCH_ROOT" && git rev-parse HEAD 2>/dev/null) >"$OUT/git-rev.txt" || true

echo
if [[ $FAILED -ne 0 ]]; then c_fail "preflight FAILED, fix the items above"; exit 1; fi
c_ok "preflight passed -> $OUT"
