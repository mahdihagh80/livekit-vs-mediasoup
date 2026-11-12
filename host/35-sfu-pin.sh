#!/usr/bin/env bash
# SFU only (root): pin NIC IRQs and side processes to their physical cores.
# Plan (the SFU host, HT siblings 64-127 unused):
#   0-7 NIC queue IRQs, 8 Node media, 9-10 Go signaling (both pinned by mediasoup's
#   own config), 11-12 HAProxy, 13 housekeeping + all other IRQs, 14-63 workers.
# Works without ethtool (only /proc). Not persistent: re-run after reboot, or after
# HAProxy / node_exporter / chrony / docker restart.
# Undo: echo 0-31,64-95 into each IRQ's smp_affinity_list, and taskset -apc 0-127 <pid>.
set -u
NIC_SLAVES="ens2f0 ens2f1 ens2f2 ens2f3"
IRQ_CPUS=(0 1 2 3 4 5 6 7)
HAPROXY_CPUS=11-12   # informational: haproxy.cfg cpu-map does the pinning
HOUSE_CPU=13

for s in $NIC_SLAVES; do
  drv=$(basename "$(readlink -f /sys/class/net/$s/device/driver)")
  i=0; ok=0; bad=""
  for irq in $(ls /sys/class/net/$s/device/msi_irqs | sort -n); do
    # i40e reserves more MSI-X vectors than it requests: no /proc/irq entry, skip.
    [ -e /proc/irq/$irq ] || continue
    name=$(awk -v q="$irq:" '$1==q{print $NF}' /proc/interrupts)
    # admin/misc vector: housekeeping core
    if [[ "$name" =~ async|misc|fw|ctrl ]] || ! [[ "$name" =~ TxRx|comp ]]; then
      echo "$HOUSE_CPU" > /proc/irq/$irq/smp_affinity_list 2>/dev/null; continue
    fi
    c=${IRQ_CPUS[$((i % ${#IRQ_CPUS[@]}))]}; i=$((i+1))
    if echo "$c" > /proc/irq/$irq/smp_affinity_list 2>/dev/null; then ok=$((ok+1)); else bad="$bad $irq"; fi
  done
  echo "$s driver=$drv queue_irqs=$i pinned=$ok refused:${bad:- none}"
done

# Every other (and future) IRQ goes to the housekeeping core.
mask=$(printf '%032x' $((1 << HOUSE_CPU)) | sed -E 's/(.{8})/\1,/g; s/,$//')
echo "$mask" > /proc/irq/default_smp_affinity
echo "default_smp_affinity=$(cat /proc/irq/default_smp_affinity)"

# HAProxy is pinned by its own config on the test host (nbthread 4, cpu-map 11-12 + 75-76, set
# 2026-11-06); taskset here would undo the per-thread map, so it is left alone.
# -a: every thread of the process.
for n in node_exporter chronyd dockerd containerd sshd; do
  for p in $(pgrep -x "$n"); do taskset -apc "$HOUSE_CPU" "$p" >/dev/null; done
done

echo "--- check"
echo -n "NIC IRQs per core: "
for s in $NIC_SLAVES; do for irq in $(ls /sys/class/net/$s/device/msi_irqs); do
  cat /proc/irq/$irq/effective_affinity_list 2>/dev/null; done; done | sort -n | uniq -c | tr -s ' \n' ' '; echo
for n in haproxy node_exporter chronyd dockerd containerd sshd; do
  p=$(pgrep -x "$n" | head -1); [ -n "$p" ] && echo "$n: $(taskset -cp "$p" | awk -F': ' '{print $2}')"
done
