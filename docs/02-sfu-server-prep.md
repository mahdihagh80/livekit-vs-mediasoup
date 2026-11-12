# 02. SFU server (xFusion) preparation: exact steps

Do these once, in order, as root unless noted. Steps marked **(reboot → redo)**
don't survive a reboot; `host/90-preflight.sh` catches it if you forget.

## 0. Requirements

* The xFusion is **out of production traffic** for the whole campaign (no
  LiveKit from the fleet, no other containers). Check `docker ps` and `top`.
* OS packages: `ethtool chrony numactl linux-tools-common/cpupower curl git`,
  Node.js ≥ 20 + npm, Go ≥ 1.24, `python3 make g++` (mediasoup builds its worker from source if no prebuilt binary matches).
* **yq** (mikefarah v4, one static binary): every script reads `bench.yaml` with it.
  `tools/get-yq.sh` installs it into `tools/bin/yq`; without internet, copy the
  binary there by hand (same on all three hosts).
* The lab network: SFU and both generators on the same switch, 40 Gb links,
  **nothing in between that does NAT or stateful firewalling**.

## 1. Get the repo and config onto the host

```bash
sudo mkdir -p /bench && sudo chown "$USER" /bench
# copy the repo contents into /bench (from the laptop: docs/00 step A)
cd /bench
tools/get-yq.sh
cp bench.yaml.example bench.yaml
vi bench.yaml         # IPs, NIC names, CPU plan, LiveKit key/secret: see below
```

`bench.yaml` is the **only** configuration file. The Node media service, the Go
signaling, the load generator and every script read it; nothing uses
environment variables. Copy the **same** file to both generators later
(`scp bench.yaml gen-1:/bench/`).

## 2. Record the hardware (nothing changes)

```bash
host/00-inventory.sh           # as root gives dmidecode output too
```

Read the summary it prints and decide the **CPU plan**:

* Which NUMA node is the data NIC on? (`numa-node-<nic>.txt`)
* Which CPUs belong to that node? (`lscpu.txt`)
* How many queues does the NIC support? (`ethtool-<nic>-channels.txt`)

### Choosing the CPU plan (bench.yaml)

The NIC interrupts (softirq) get dedicated cores; the SFU under test gets the
rest. Both SFUs get exactly the same `cpu.sfu`.

Example for 64 logical CPUs, 2 sockets × 32, NIC on node 0 (CPUs 0-31):

```yaml
cpu:
  sfu_irq: "0-15"          # 16 queues' interrupts, on the NIC's own NUMA node
  sfu: "16-63"             # 48 CPUs for the SFU
livekit:
  layouts: ["1x48", "3x16", "6x8"]
mediasoup:
  control_cpus: "16-17"    # Node + Go signaling
  worker_cpus: "18-63"     # 46 workers, one per CPU
```

How many IRQ CPUs? On the test host, LiveKit at ~26k participants needed about 560k
packets/s **per IRQ CPU on 8 CPUs** and dropped packets. With 16 IRQ CPUs that
load halves. If the inventory shows hyper-threading, prefer whole physical cores
for IRQs (both siblings), and keep the plan identical for both SFUs. Write the
plan into `docs/templates/hardware.md`.

## 3. Kernel network settings and file limits

```bash
sudo host/10-sysctl-limits.sh
```

This sets UDP buffers (64 MB max), `netdev_max_backlog`, `netdev_budget`, the
local port range, nofile 1M (`/etc/sysctl.d/90-sfu-bench.conf`,
`/etc/security/limits.d/90-sfu-bench.conf`). **Log out and back in** so your
shell gets the new nofile limit. If it warns about `nf_conntrack`, apply the
NOTRACK rules it prints (or raise `nf_conntrack_max`) on all 3 hosts.

## 4. CPU frequency, idle states, irqbalance  (reboot → redo the governor part)

```bash
sudo host/20-cpu.sh
```

Also, once, in the BIOS of all three machines: performance power profile,
turbo on, same SMT setting as production. Record it in `hardware.md`.

## 5. NIC: queues, rings, RSS on ports, IRQ pinning  (reboot → redo)

```bash
sudo host/30-nic.sh
```

Then check:

```bash
ethtool -n <nic> rx-flow-hash udp4      # must list "L4 bytes 0 & 1" and "2 & 3"
cat /proc/interrupts | grep <nic|mlx5_comp>   # each queue counts on one IRQ CPU
```

**Why the port hash matters:** the generators are 2 IP addresses. With an
IP-only hash all traffic lands in 1-2 RX queues, so 1-2 CPUs do all the
softirq work and the NIC drops packets long before the SFU is loaded. Real
users come from millions of IPs.

## 6. Time server

```bash
sudo host/40-chrony.sh          # on the SFU: becomes the chrony server for the lab
```

## 7. node_exporter

```bash
sudo host/50-node-exporter.sh   # :9100 with cpu/netdev/softnet/ethtool collectors
```

If a node_exporter already runs on :9100 it's reused. Make sure
the bench Prometheus (on gen-1) can reach `<hosts.sfu_ip>:<hosts.node_exporter_monitoring_port>`.

## 8. Install both SFUs

* LiveKit: [04-livekit.md](04-livekit.md) (`livekit/get-binary.sh`, review `livekit/livekit.base.yaml`)
* Or both in Docker: [08-docker.md](08-docker.md)
* mediasoup: [05-mediasoup.md](05-mediasoup.md) (Docker image, `mediasoup/compose.yaml`)

## 9. Open ports (if a host firewall is active)

Defaults from `bench.yaml.example`; if you change ports there, change the firewall too.

| Port | Proto | What |
|---|---|---|
| 7880,7890,7900,... | TCP | LiveKit signaling (one per instance) |
| 7882,7892,7902,... | UDP | LiveKit media (single-port mux per instance) |
| 6789-6796 | TCP | LiveKit /metrics |
| 8080 | TCP | mediasoup signaling (WebSocket) |
| 44444-44507 | UDP | mediasoup WebRtcServer, one port per worker |
| 446, 447 | TCP | mediasoup media / signaling /metrics (public, scraped by a remote Prometheus) |
| 444 | TCP | HAProxy → generators' metrics-gw :30699 (public, scraped by a remote Prometheus) |
| 3100, 8081 | TCP | loopback only (mediasoup internal RPC, signaling internal events): never open |
| 9100 | TCP | node_exporter |
| 123 | UDP | chrony (server) |

## 10. Preflight (and again before every run)

```bash
sudo host/90-preflight.sh first-check
```

Every line should be `[ OK ]`. The snapshot goes to `results/preflight/<run>/<host>/`.

## Per-run checklist on the SFU host

1. Only one SFU running: `pgrep -fa 'livekit-server|mediasoup-worker'`
2. `sudo host/90-preflight.sh <run-name>` passes
3. Start the SFU (`livekit/start.sh <layout>`, or `docker compose -f mediasoup/compose.yaml up -d`). Nothing to copy: the controller derives the URLs from `bench.yaml`
4. After the run: stop it (`livekit/stop.sh`, or `--profile mediasoup down`), wait ~1 min idle before the next run
