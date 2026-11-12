#!/usr/bin/env bash
# OPTIONAL, generators only (root): add WAN-like impairment on the generator's
# egress to test NACK/RTX/BWE behaviour. Use the SAME settings for both SFUs.
#   sudo host/netem.sh on  [delay_ms=40] [jitter_ms=5] [loss_pct=1]
#   sudo host/netem.sh off
# Note: egress-only. Packets generator -> SFU are impaired; SFU -> generator
# are not (run it on both generators so every media path crosses one impaired hop).
source "$(dirname "$0")/lib.sh"
need_root
DEV=$GEN_NIC
case ${1:-} in
  on)
    tc qdisc replace dev "$DEV" root netem delay "${2:-40}ms" "${3:-5}ms" distribution normal loss "${4:-1}%" limit 1000000
    c_ok "netem on $DEV: delay ${2:-40}ms±${3:-5}ms loss ${4:-1}%" ;;
  off)
    tc qdisc del dev "$DEV" root 2>/dev/null || true
    c_ok "netem off on $DEV" ;;
  *) echo "usage: $0 on [delay_ms] [jitter_ms] [loss_pct] | off"; exit 2 ;;
esac
tc qdisc show dev "$DEV"
