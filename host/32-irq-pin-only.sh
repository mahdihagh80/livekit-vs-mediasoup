#!/usr/bin/env bash
# Pin-only variant of 30-nic.sh step 4: spread every NIC queue IRQ over the IRQ cores
# (one core each, round-robin across all slave NICs). No queue/ring/hash change,
# so the NIC is not reset. Not persistent across reboots.
source "$(dirname "$0")/lib.sh"
need_root
ROLE=$(host_role)
case $ROLE in
  sfu) NIC=$SFU_NIC; SLAVES=$SFU_NIC_SLAVES; CPUS=$IRQ_CPUS ;;
  gen) NIC=$GEN_NIC; SLAVES=$GEN_NIC_SLAVES; CPUS=$GEN_IRQ_CPUS ;;
  *) echo "unknown role" >&2; exit 1 ;;
esac
CPU_ARR=($(expand_cpus "$CPUS")); NQ=${#CPU_ARR[@]}
i=0
for n in $(phys_nics "$NIC" "$SLAVES"); do
  c=0
  for f in /sys/class/net/"$n"/device/msi_irqs/*; do
    [[ -e "$f" ]] || continue
    irq=$(basename "$f")
    name=$(awk -v q="$irq:" '$1==q {print $NF}' /proc/interrupts)
    [[ "$name" =~ async|misc|fw|ctrl ]] && continue
    echo "${CPU_ARR[$((i % NQ))]}" >/proc/irq/"$irq"/smp_affinity_list 2>/dev/null && { i=$((i+1)); c=$((c+1)); }
  done
  c_ok "$n: pinned $c queue IRQs"
done
c_ok "total $i IRQs over CPUs $CPUS"
