#!/usr/bin/env bash
# STEP 4 on ALL THREE hosts (root): NIC queues, rings, RSS hashing and IRQ pinning.
#
# Why this matters: in an earlier LiveKit test, NIC RX softirq was pinned to
# only CPUs 0-7 and drops happened at the NIC ring. Fix that first, otherwise both
# SFUs hit the same NIC wall and the benchmark measures the NIC, not the SFU.
#
# 1. One combined queue per IRQ core (IRQ_CPUS on SFU, GEN_IRQ_CPUS on generators)
# 2. Max RX/TX ring size
# 3. UDP RSS hash on IP *and ports* (sdfn). With only two client IPs, hashing on
#    IPs alone would put almost all traffic on 1-2 queues.
# 4. Each queue IRQ pinned to exactly one IRQ core (round-robin)
# Not persistent across reboots: re-run after every reboot (preflight checks it).
source "$(dirname "$0")/lib.sh"
need_root

ROLE=$(host_role)
case $ROLE in
  sfu) NIC=$SFU_NIC; SLAVES=$SFU_NIC_SLAVES; CPUS=$IRQ_CPUS ;;
  gen) NIC=$GEN_NIC; SLAVES=$GEN_NIC_SLAVES; CPUS=$GEN_IRQ_CPUS ;;
  *) echo "this host's IPs match neither hosts.sfu_ip nor hosts.gen_ips in bench.yaml" >&2; exit 1 ;;
esac
CPU_ARR=($(expand_cpus "$CPUS"))
NQ=${#CPU_ARR[@]}
c_info "role=$ROLE nic=$NIC irq cpus=$CPUS ($NQ)"

for n in $(phys_nics "$NIC" "$SLAVES"); do
  c_info "--- $n ($(ethtool -i "$n" | awk '/driver/{print $2}'), numa node $(cat /sys/class/net/$n/device/numa_node 2>/dev/null))"

  max_q=$(ethtool -l "$n" | awk '/Pre-set/{p=1} p&&/Combined/{print $2; exit}')
  q=$NQ; [[ -n "$max_q" && "$max_q" != "n/a" && $max_q -lt $q ]] && q=$max_q
  ethtool -L "$n" combined "$q" 2>/dev/null && c_ok "$n: $q combined queues" || c_warn "$n: could not set queues (driver may use rx/tx instead of combined)"

  rx_max=$(ethtool -g "$n" | awk '/Pre-set/{p=1} p&&/^RX:/{print $2; exit}')
  tx_max=$(ethtool -g "$n" | awk '/Pre-set/{p=1} p&&/^TX:/{print $2; exit}')
  ethtool -G "$n" rx "$rx_max" tx "$tx_max" 2>/dev/null && c_ok "$n: rings rx=$rx_max tx=$tx_max" || c_warn "$n: ring resize failed/unsupported"

  ethtool -N "$n" rx-flow-hash udp4 sdfn 2>/dev/null && c_ok "$n: udp4 RSS hash = src/dst IP + ports" || c_fail "$n: cannot set udp4 hash to sdfn"
  ethtool -n "$n" rx-flow-hash udp4 | sed 's/^/      /'

  # Adaptive interrupt coalescing is usually right for RTP; record, don't change.
  ethtool -c "$n" 2>/dev/null | grep -E "Adaptive|rx-usecs:" | sed 's/^/      coalesce: /' || true

  # Pin IRQs. Queue IRQ names differ per driver (mlx5_comp0@pci..., ens1f0-TxRx-0, i40e-ens1f0-TxRx-0 ...).
  pci=$(basename "$(readlink -f /sys/class/net/$n/device)")
  mapfile -t irqs < <(ls /sys/class/net/$n/device/msi_irqs 2>/dev/null | sort -n)
  i=0
  for irq in "${irqs[@]}"; do
    name=$(awk -v irq="$irq:" '$1==irq {print $NF}' /proc/interrupts)
    # skip the async/control vector (mlx5_async, ...-misc, fw)
    [[ "$name" =~ async|misc|fw|ctrl ]] && continue
    cpu=${CPU_ARR[$((i % NQ))]}
    echo "$cpu" >/proc/irq/$irq/smp_affinity_list 2>/dev/null && i=$((i+1))
  done
  c_ok "$n: pinned $i queue IRQs over CPUs $CPUS (pci $pci)"

  # RPS/XPS off: hardware RSS already spreads the load
  for f in /sys/class/net/$n/queues/rx-*/rps_cpus; do echo 0 >"$f" 2>/dev/null || true; done
done

c_info "verify under load: mpstat -P ALL 1  (softirq should be on IRQ_CPUS only, spread evenly)"
c_info "                   ethtool -S <nic> | grep -iE 'drop|discard|miss|buf'  (should stay flat)"
