#!/usr/bin/env bash
# Ring-only variant of 30-nic.sh step 2, for the GENERATORS: set every bond slave's RX/TX
# ring to the driver maximum. No queue, RSS-hash or IRQ-pin change.
#
# Why: in the ab13 LiveKit leg (2026-11-06) g1/g2 dropped ~1.4k packets/s at the NIC
# (rx_missed +199k / +285k) with rings at 512 of 8160.
#
# On i40e a ring change resets that port, so the port drops out of the LACP bond for a
# few seconds. The script does one port at a time and waits until the port is back in
# the bond's active aggregator before touching the next one; the bond keeps 3 of 4 ports
# up the whole time. It saves each queue IRQ's CPU before the change and puts it back
# after, and stops if the RSS hash changed. Do not run during a test.
# Not persistent across reboots (like 30-nic.sh).
source "$(dirname "$0")/lib.sh"
need_root
[[ $(host_role) == gen ]] || { echo "generators only (the SFU host is the tested host: rings stay as they are)" >&2; exit 1; }
NIC=$GEN_NIC; SLAVES=$GEN_NIC_SLAVES
BOND=/proc/net/bonding/$NIC
[[ -r $BOND ]] || { echo "no $BOND" >&2; exit 1; }
if docker ps --format '{{.Names}}' 2>/dev/null | grep -q scenario; then
  echo "a scenario container is running: wait for the test to end" >&2; exit 1
fi

active_agg() { awk '/^Active Aggregator Info:/{a=1} a&&/Aggregator ID:/{print $3; exit}' "$BOND"; }
# "<mii> <aggregator id>" of one slave
slave_state() { awk -v s="$1" '$1=="Slave"&&$2=="Interface:"{x=($3==s)} x&&/^MII Status:/{m=$3} x&&/^Aggregator ID:/{print m, $3; exit}' "$BOND"; }
in_bond() { local st; st=$(slave_state "$1"); [[ ${st%% *} == up && ${st##* } == "$(active_agg)" ]]; }
queue_irqs() {
  local f irq name
  for f in /sys/class/net/"$1"/device/msi_irqs/*; do
    [[ -e $f ]] || continue
    irq=$(basename "$f")
    [[ -r /proc/irq/$irq/smp_affinity_list ]] || continue   # MSI vector the driver does not use
    name=$(awk -v q="$irq:" '$1==q {print $NF}' /proc/interrupts)
    [[ $name =~ async|misc|fw|ctrl ]] && continue
    echo "$irq"
  done
}

for n in $(phys_nics "$NIC" "$SLAVES"); do
  rx_max=$(ethtool -g "$n" | awk '/Pre-set/{p=1} p&&/^RX:/{print $2; exit}')
  tx_max=$(ethtool -g "$n" | awk '/Pre-set/{p=1} p&&/^TX:/{print $2; exit}')
  rx_cur=$(ethtool -g "$n" | awk '/Current/{p=1} p&&/^RX:/{print $2; exit}')
  tx_cur=$(ethtool -g "$n" | awk '/Current/{p=1} p&&/^TX:/{print $2; exit}')
  if [[ $rx_cur == "$rx_max" && $tx_cur == "$tx_max" ]]; then c_ok "$n: rings already rx=$rx_cur tx=$tx_cur (max)"; continue; fi
  for m in $(phys_nics "$NIC" "$SLAVES"); do
    in_bond "$m" || { c_fail "$m is not up in the active aggregator: stop before touching $n"; exit 1; }
  done
  declare -A AFF=()
  for irq in $(queue_irqs "$n"); do AFF[$irq]=$(cat /proc/irq/"$irq"/smp_affinity_list); done
  hash_before=$(ethtool -n "$n" rx-flow-hash udp4 2>/dev/null)
  q_before=$(ethtool -l "$n" | awk '/Current/{p=1} p&&/Combined/{print $2; exit}')

  c_info "$n: rings rx $rx_cur -> $rx_max, tx $tx_cur -> $tx_max (port resets)"
  ethtool -G "$n" rx "$rx_max" tx "$tx_max" || { c_fail "$n: ethtool -G failed"; exit 1; }
  for i in $(seq 1 60); do in_bond "$n" && break; sleep 1; done
  in_bond "$n" || { c_fail "$n: not back in the bond's active aggregator after 60 s: stop"; exit 1; }
  c_ok "$n: back in the bond after ${i} s"

  moved=0
  for irq in "${!AFF[@]}"; do
    [[ -e /proc/irq/$irq/smp_affinity_list ]] || continue
    if [[ $(cat /proc/irq/"$irq"/smp_affinity_list) != "${AFF[$irq]}" ]]; then
      echo "${AFF[$irq]}" >/proc/irq/"$irq"/smp_affinity_list && moved=$((moved+1))
    fi
  done
  [[ $(queue_irqs "$n" | sort) == $(printf '%s\n' "${!AFF[@]}" | sort) ]] \
    && c_ok "$n: ${#AFF[@]} queue IRQs kept their CPUs ($moved put back)" \
    || c_warn "$n: queue IRQ numbers changed: re-run host/32-irq-pin-only.sh"
  unset AFF
  [[ $(ethtool -n "$n" rx-flow-hash udp4 2>/dev/null) == "$hash_before" ]] || { c_fail "$n: udp4 RSS hash changed: run host/30-nic.sh"; exit 1; }
  [[ $(ethtool -l "$n" | awk '/Current/{p=1} p&&/Combined/{print $2; exit}') == "$q_before" ]] || c_warn "$n: queue count changed"
  c_ok "$n: $(ethtool -g "$n" | awk '/Current/{p=1} p&&/^(RX|TX):/{printf "%s%s ", $1, $2}')"
done
c_ok "done: $(grep -c '^Slave Interface' "$BOND") slaves; bond $NIC $(awk '/^MII Status:/{print $3; exit}' "$BOND")"
