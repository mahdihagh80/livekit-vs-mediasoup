#!/usr/bin/env bash
# SFU host, as root. Experiment NIC layout:
#   apply    every bond1 slave gets 2 combined queues (rings untouched, 512), and the 8 queue
#            IRQs are pinned one each to CPUs 0-7 (ens2f0 -> 0,1  ens2f1 -> 2,3  ...).
#            CPUs 64-71 (the hyperthread siblings) then carry no NIC interrupt.
#   restore  back to the layout before: 119 combined queues per slave, queue IRQs round-robin
#            over CPUs 0-7,64-71 (as host/32-irq-pin-only.sh), XPS/RPS masks written back
#            from the state saved by apply.
#   status   print the current layout, change nothing.
# usage: sudo bash 33-nic-2q.sh apply|restore|status
# One slave at a time: changing the queue count resets that port for a moment, so the script
# waits until bond1 shows the slave up again before it touches the next one.
# Not persistent across reboots. State of the last apply: /var/tmp/x-nic-state/.
set -u
PATH=$PATH:/usr/sbin:/sbin
BOND=bond1 NICS="ens2f0 ens2f1 ens2f2 ens2f3"
Q_APPLY=2 CPUS_APPLY="0 1 2 3 4 5 6 7"
Q_RESTORE=119 CPUS_RESTORE="0 1 2 3 4 5 6 7 64 65 66 67 68 69 70 71"
ST=/var/tmp/x-nic-state
MODE=${1:-status}

qirqs() { awk -v n="$1" '$NF ~ ("^i40e-" n "-TxRx-[0-9]+$") {sub(":", "", $1); split($NF, a, "-"); print a[length(a)], $1}' /proc/interrupts | sort -n | awk '{print $2}'; }
status() {
  for n in $NICS; do
    q=$(ethtool -l "$n" | awk '/^Combined:/{v=$2} END{print v}')
    r=$(ethtool -g "$n" | awk '/^RX:/{rx=$2} /^TX:/{tx=$2} END{print rx "/" tx}')
    a=$(for i in $(qirqs "$n"); do cat /proc/irq/"$i"/smp_affinity_list; done | sort -n | uniq -c | awk '{printf "%s%s", (NR>1?" ":""), ($1>1 ? $2 "x" $1 : $2)}')
    s=$(awk -v n="$n" '$0 ~ "^Slave Interface: " n "$" {f=1} f && /^MII Status:/ {print $3; exit}' /proc/net/bonding/$BOND)
    echo "$n: combined $q, rings rx/tx $r, bond slave $s, $(qirqs "$n" | wc -l) queue IRQs on CPUs: $a"
  done
}
wait_slave() { # wait until the bond shows the slave up and LACP has a partner again
  for _ in $(seq 1 30); do
    s=$(awk -v n="$1" '$0 ~ "^Slave Interface: " n "$" {f=1} f && /^MII Status:/ {print $3; exit}' /proc/net/bonding/$BOND)
    [ "$s" = up ] && [ "$(cat /sys/class/net/"$1"/operstate)" = up ] && { sleep 3; return 0; }
    sleep 1
  done
  echo "FAIL $1 did not come back up in the bond: stopping here, check 'cat /proc/net/bonding/$BOND'"; exit 1
}
set_queues() { # set_queues <count>
  for n in $NICS; do
    cur=$(ethtool -l "$n" | awk '/^Combined:/{v=$2} END{print v}')
    if [ "$cur" = "$1" ]; then echo "$n: already $1 combined queues"; continue; fi
    ethtool -L "$n" combined "$1" || { echo "FAIL $n: ethtool -L combined $1"; exit 1; }
    wait_slave "$n"; echo "$n: combined $cur -> $1, slave up"
  done
}
pin() { # pin <cpu list>: all queue IRQs of all slaves, round-robin over the list
  local cpus=($1) i=0
  for n in $NICS; do
    for irq in $(qirqs "$n"); do
      echo "${cpus[$((i % ${#cpus[@]}))]}" >/proc/irq/"$irq"/smp_affinity_list || { echo "FAIL irq $irq"; exit 1; }
      i=$((i + 1))
    done
  done
  echo "pinned $i queue IRQs over CPUs: $1"
}

[ "$MODE" = status ] && { status; exit 0; }
[ "$(id -u)" = 0 ] || { echo "run with sudo"; exit 1; }
pgrep -x irqbalance >/dev/null && { echo "irqbalance is running: it would undo the pinning. Stopping."; exit 1; }
echo "--- before"; status
case $MODE in
  apply)
    if [ "$(ethtool -l ens2f0 | awk '/^Combined:/{v=$2} END{print v}')" != "$Q_APPLY" ]; then
      rm -rf "$ST"; mkdir -p "$ST"
      for n in $NICS; do
        ethtool -l "$n" >"$ST/$n.channels"; ethtool -g "$n" >"$ST/$n.rings"; ethtool -n "$n" rx-flow-hash udp4 >"$ST/$n.udp4hash" 2>&1
        for i in $(qirqs "$n"); do echo "$i $(cat /proc/irq/"$i"/smp_affinity_list)"; done >"$ST/$n.irqs"
        for f in /sys/class/net/"$n"/queues/tx-*/xps_cpus /sys/class/net/"$n"/queues/rx-*/rps_cpus; do echo "$f $(cat "$f" 2>/dev/null)"; done >"$ST/$n.masks"
      done
      date -u +%FT%TZ >"$ST/saved-at"; echo "state saved in $ST"
    else echo "already at $Q_APPLY queues: keeping the state saved at $(cat "$ST/saved-at" 2>/dev/null || echo '?')"; fi
    set_queues "$Q_APPLY"
    n8=$(for n in $NICS; do qirqs "$n"; done | wc -l)
    [ "$n8" = 8 ] || { echo "FAIL expected 8 queue IRQs, found $n8: not pinning"; exit 1; }
    pin "$CPUS_APPLY" ;;
  restore)
    set_queues "$Q_RESTORE"
    pin "$CPUS_RESTORE"
    if [ -d "$ST" ]; then
      w=0
      for n in $NICS; do
        while read -r f v; do [ -n "$v" ] && [ -w "$f" ] && echo "$v" >"$f" 2>/dev/null && w=$((w + 1)); done <"$ST/$n.masks"
        want=$(grep -E '^(IP|L4)' "$ST/$n.udp4hash" | paste -sd, -); have=$(ethtool -n "$n" rx-flow-hash udp4 | grep -E '^(IP|L4)' | paste -sd, -)
        [ "$want" = "$have" ] || echo "WARN $n: udp4 hash fields differ from the saved state (saved: $want; now: $have)"
      done
      echo "wrote back $w XPS/RPS masks from $ST (saved $(cat "$ST/saved-at"))"
    else echo "WARN no saved state in $ST: XPS/RPS masks left as the driver set them"; fi ;;
  *) echo "usage: $0 apply|restore|status"; exit 1 ;;
esac
for n in $NICS; do
  h=$(ethtool -n "$n" rx-flow-hash udp4 | grep -E '^(IP|L4)' | sed 's/ *$//' | paste -sd, -)
  echo "$n: udp4 hash: $h"
done
echo "--- after"; status
grep -E "MII Status|Slave Interface|Aggregator ID|Number of ports" /proc/net/bonding/$BOND | paste -sd' ' - | cut -c1-600
