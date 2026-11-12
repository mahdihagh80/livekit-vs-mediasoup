#!/usr/bin/env bash
# STEP 2 on ALL THREE hosts (root): kernel network settings and file limits.
# The same file goes on SFU and generators. Revert: rm the two files below and reboot.
source "$(dirname "$0")/lib.sh"
need_root

cat >/etc/sysctl.d/90-sfu-bench.conf <<'EOF'
# --- sfu-bench (LiveKit vs mediasoup) ---
# UDP socket buffers: LiveKit asks for ~5 MB, mediasoup workers get 16 MB (WebRtcServer recv/sendBufferSize)
net.core.rmem_max = 67108864
net.core.wmem_max = 67108864
net.core.rmem_default = 4194304
net.core.wmem_default = 4194304
net.ipv4.udp_rmem_min = 16384
net.ipv4.udp_wmem_min = 16384
net.ipv4.udp_mem = 3145728 4194304 16777216
# More packets per softirq round + bigger backlog (millions of small RTP packets/s)
net.core.netdev_max_backlog = 250000
net.core.netdev_budget = 1200
net.core.netdev_budget_usecs = 8000
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
# Generators open ~2 UDP sockets + 1 TCP (signaling) per simulated participant
net.ipv4.ip_local_port_range = 10000 65000
fs.file-max = 4194304
fs.nr_open = 4194304
# Many PeerConnections -> many timers/epoll
fs.epoll.max_user_watches = 4194304
vm.max_map_count = 1048576
EOF
sysctl --system >/dev/null
c_ok "sysctl applied (/etc/sysctl.d/90-sfu-bench.conf)"

cat >/etc/security/limits.d/90-sfu-bench.conf <<'EOF'
*    soft nofile 1048576
*    hard nofile 1048576
root soft nofile 1048576
root hard nofile 1048576
EOF
mkdir -p /etc/systemd/system.conf.d
cat >/etc/systemd/system.conf.d/90-sfu-bench.conf <<'EOF'
[Manager]
DefaultLimitNOFILE=1048576
EOF
c_ok "nofile limit 1048576 (log out/in for shells; the run scripts also call ulimit -n)"

# conntrack: millions of UDP flows through the firewall cost CPU and can fill the table.
if lsmod | grep -q nf_conntrack; then
  c_warn "nf_conntrack is loaded. Either raise it or exempt media ports (preferred):"
  echo "    sysctl -w net.netfilter.nf_conntrack_max=4194304"
  echo "    iptables -t raw -A PREROUTING -p udp -j NOTRACK ; iptables -t raw -A OUTPUT -p udp -j NOTRACK"
  echo "  Do the SAME on all hosts and for both SFU runs."
else
  c_ok "nf_conntrack not loaded"
fi
