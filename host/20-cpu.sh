#!/usr/bin/env bash
# STEP 3 on ALL THREE hosts (root): CPU frequency/idle settings so clock speed
# doesn't drift between the LiveKit and mediasoup runs.
source "$(dirname "$0")/lib.sh"
need_root

if command -v cpupower >/dev/null; then
  cpupower frequency-set -g performance >/dev/null && c_ok "governor=performance (cpupower)"
else
  for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do echo performance >"$g" 2>/dev/null || true; done
  c_ok "governor=performance (sysfs)"
fi
# Deep C-states add wake-up latency to interrupt handling. Disabling states with
# exit latency > 10us is a common middle ground (BIOS "performance" profile is better).
if command -v cpupower >/dev/null; then
  cpupower idle-set -D 10 >/dev/null 2>&1 && c_ok "idle states with latency >10us disabled" || c_warn "cpupower idle-set not supported"
fi

# irqbalance would undo the IRQ pinning done by 30-nic.sh.
if systemctl is-active --quiet irqbalance; then
  systemctl stop irqbalance && systemctl disable irqbalance >/dev/null 2>&1 || true
  c_ok "irqbalance stopped+disabled"
else
  c_ok "irqbalance not running"
fi

# Transparent huge pages: 'madvise' is a safe default for Go and Node.
echo madvise >/sys/kernel/mm/transparent_hugepage/enabled 2>/dev/null && c_ok "THP=madvise" || true

c_info "BIOS (once, by hand, same on all hosts): Power profile = Max Performance, SMT as in production, turbo ON."
c_info "Record the BIOS profile in docs/templates/hardware.md."
