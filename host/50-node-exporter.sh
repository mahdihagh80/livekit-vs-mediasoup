#!/usr/bin/env bash
# STEP 6 on ALL THREE hosts (root): node_exporter with the collectors the report uses
# (cpu per core incl. softirq, netdev, softnet, ethtool per-queue NIC drops).
# Port: hosts.node_exporter_monitoring_port in bench.yaml. If a node_exporter already runs
# there, it's reused as long as it has the cpu/netdev/softnet
# collectors. The ethtool collector is optional but very useful.
source "$(dirname "$0")/lib.sh"
need_root

VER=1.9.1
P=$NODE_EXPORTER_PORT
if curl -fs "localhost:$P/metrics" >/dev/null 2>&1; then
  c_ok "a node_exporter is already listening on :$P"
  curl -fs "localhost:$P/metrics" | grep -q '^node_softnet_dropped_total' && c_ok "softnet collector present" || c_warn "softnet collector missing"
  curl -fs "localhost:$P/metrics" | grep -q '^node_ethtool_' && c_ok "ethtool collector present" || c_warn "ethtool collector missing (add --collector.ethtool)"
  exit 0
fi

ROLE=$(host_role)
[[ $ROLE == sfu ]] && NIC=$SFU_NIC SLAVES=$SFU_NIC_SLAVES || NIC=$GEN_NIC SLAVES=$GEN_NIC_SLAVES
DEVS=$(echo "$NIC $(phys_nics "$NIC" "$SLAVES")" | tr ' ' '\n' | sort -u | paste -sd'|')

if ! command -v node_exporter >/dev/null; then
  TGZ=/tmp/node_exporter-$VER.linux-amd64.tar.gz
  [[ -f $TGZ ]] || curl -fsSL -o "$TGZ" "https://github.com/prometheus/node_exporter/releases/download/v$VER/node_exporter-$VER.linux-amd64.tar.gz" \
    || { c_fail "download failed; copy node_exporter-$VER.linux-amd64.tar.gz to /tmp and re-run"; exit 1; }
  tar -xzf "$TGZ" -C /tmp && install -m 0755 /tmp/node_exporter-$VER.linux-amd64/node_exporter /usr/local/bin/node_exporter
fi

cat >/etc/systemd/system/node-exporter-bench.service <<EOF
[Unit]
Description=node_exporter for sfu-bench
After=network-online.target
[Service]
ExecStart=/usr/local/bin/node_exporter --web.listen-address=:$P \\
  --collector.ethtool --collector.ethtool.device-include='^($DEVS)\$' \\
  --collector.softirqs --collector.interrupts \\
  --collector.netdev.device-include='^($DEVS)\$'
Restart=always
Nice=10
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now node-exporter-bench >/dev/null 2>&1
sleep 2
curl -fs "localhost:$P/metrics" | grep -q '^node_cpu_seconds_total' && c_ok "node_exporter running on :$P (devices $DEVS)" || c_fail "node_exporter not answering"
c_info "node_exporter runs on the SFU host too: its CPU (<1%) is the same for both runs."
