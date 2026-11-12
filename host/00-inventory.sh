#!/usr/bin/env bash
# STEP 1 on ALL THREE hosts: record the hardware/OS/NIC facts that the results
# depend on. Nothing is changed. Output: results/inventory/<hostname>/
source "$(dirname "$0")/lib.sh"

OUT="$BENCH_ROOT/results/inventory/$(hostname)"
mkdir -p "$OUT"
run() { local f=$1; shift; { echo "\$ $*"; "$@" 2>&1 || true; } >"$OUT/$f"; }

ROLE=$(host_role)
c_info "host $(hostname) role=$ROLE -> $OUT"
[[ $ROLE == sfu ]] && NIC=$SFU_NIC SLAVES=$SFU_NIC_SLAVES || NIC=$GEN_NIC SLAVES=$GEN_NIC_SLAVES

run uname.txt uname -a
run os-release.txt cat /etc/os-release
run lscpu.txt lscpu
run numa.txt bash -c 'numactl -H 2>/dev/null || lscpu | grep -i numa'
run mem.txt free -g
run dmidecode.txt bash -c 'dmidecode -t system -t processor -t memory 2>/dev/null | head -200'
run governor.txt bash -c 'cat /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor 2>/dev/null | sort | uniq -c'
run cpuidle.txt bash -c 'cpupower idle-info 2>/dev/null || ls /sys/devices/system/cpu/cpu0/cpuidle 2>/dev/null'
run lspci-net.txt bash -c 'lspci -nn | grep -iE "ethernet|network"'
run ip-addr.txt ip -d addr
run sysctl-net.txt bash -c 'sysctl -a 2>/dev/null | grep -E "^net\.(core|ipv4\.udp|ipv4\.ip_local_port_range|netfilter)|^fs\.file"'
run ulimit.txt bash -c 'ulimit -a'
run irqbalance.txt systemctl status irqbalance --no-pager
run chrony.txt bash -c 'chronyc tracking; chronyc sources -v'
run docker.txt bash -c 'docker ps 2>/dev/null; docker info 2>/dev/null | head -30'
run top.txt bash -c 'top -b -n1 | head -40'
for n in $(phys_nics "$NIC" "$SLAVES"); do
  run "ethtool-$n-driver.txt" ethtool -i "$n"
  run "ethtool-$n-channels.txt" ethtool -l "$n"
  run "ethtool-$n-rings.txt" ethtool -g "$n"
  run "ethtool-$n-features.txt" ethtool -k "$n"
  run "ethtool-$n-rss.txt" ethtool -x "$n"
  run "ethtool-$n-udp4hash.txt" ethtool -n "$n" rx-flow-hash udp4
  run "ethtool-$n-coalesce.txt" ethtool -c "$n"
  run "ethtool-$n-stats.txt" ethtool -S "$n"
  run "numa-node-$n.txt" cat "/sys/class/net/$n/device/numa_node"
  run "irqs-$n.txt" bash -c "grep -E '$n' /proc/interrupts | awk '{print \$1, \$NF}'"
done
c_ok "inventory written. Copy results/inventory/ into the final report."
echo
echo "Fill in docs/templates/hardware.md from these files. Key questions:"
echo "  - NUMA node of the data NIC:  $(cat /sys/class/net/$(phys_nics "$NIC" "$SLAVES" | awk '{print $1}')/device/numa_node 2>/dev/null || echo '?')"
echo "  - CPUs per NUMA node:         $(lscpu | grep -E 'NUMA node[0-9]+ CPU' | tr -s ' ' | paste -sd ';')"
echo "  - max combined NIC queues:    $(ethtool -l "$(phys_nics "$NIC" "$SLAVES" | awk '{print $1}')" 2>/dev/null | awk '/Combined/{print $2; exit}')"
