#!/usr/bin/env bash
# STEP 5 on ALL THREE hosts (root): time sync.
#
# One-way delay = receive time on generator B - send time on generator A, so the
# two generators must agree on time to well below 1 ms. CHRONY_SERVER (default:
# the SFU host) serves time to the generators over the lab LAN, polling every 1-2 s.
# The SFU's absolute time doesn't matter, only gen<->gen agreement.
source "$(dirname "$0")/lib.sh"
need_root

command -v chronyd >/dev/null || { apt-get install -y chrony >/dev/null 2>&1 || yum install -y chrony >/dev/null 2>&1; }
CONF=/etc/chrony/chrony.conf; [[ -f $CONF ]] || CONF=/etc/chrony.conf
cp -n "$CONF" "$CONF.pre-sfu-bench" 2>/dev/null || true

MYIPS=" $(hostname -I) "
if [[ "$MYIPS" == *" $CHRONY_SERVER "* ]]; then
  subnet="$(echo "$CHRONY_SERVER" | cut -d. -f1-3).0/24"
  cat >"$CONF" <<EOF
# sfu-bench: time server for the generators. Keeps upstream NTP if reachable.
pool pool.ntp.org iburst maxsources 2
local stratum 8
allow $subnet
driftfile /var/lib/chrony/chrony.drift
makestep 1.0 3
rtcsync
EOF
  c_ok "chrony server mode (allow $subnet)"
else
  cat >"$CONF" <<EOF
# sfu-bench: follow the lab time server closely
server $CHRONY_SERVER iburst minpoll 0 maxpoll 1 xleave
driftfile /var/lib/chrony/chrony.drift
makestep 0.1 3
rtcsync
EOF
  c_ok "chrony client of $CHRONY_SERVER (poll 1-2s)"
fi
systemctl restart chrony 2>/dev/null || systemctl restart chronyd
sleep 5
chronyc tracking | grep -E "Reference|System time|RMS offset"
c_info "Wait a few minutes, then preflight requires |System time| < 100us on generators."
