# 03. Client (load generator) servers: exact steps

Do this on **both** generator hosts (gen-1 and gen-2) unless it says "gen-1 only".
gen-1 also runs the controller and Prometheus (loopback only; you view metrics on your laptop).

## 0. Requirements

* OS packages: `ethtool chrony numactl cpupower curl git ffmpeg` (ffmpeg only on the host that generates media)
* Go ≥ 1.26 (server-sdk-go v2.18 needs it). If you build on one host, copy
  `client/loadgen` and `client/media/` to the other one.
* Docker (optional) for Prometheus on gen-1.

## 1. Repo and config

```bash
sudo mkdir -p /bench && sudo chown "$USER" /bench
cd /bench          # same path as on the SFU; copy the repo contents here (docs/00 step A)
tools/get-yq.sh                                   # or copy tools/bin/yq from the SFU host
scp <sfu-host>:/bench/bench.yaml .      # SAME bench.yaml on all hosts
host/00-inventory.sh
```

CPU plan on generators (`bench.yaml`): `cpu.gen_irq` for NIC interrupts,
`cpu.gen` for the loadgen process. 16 + 48 is a sensible start on a 64-CPU box.
The order of `hosts.gen_ips` matters: agent 0 is the first IP, agent 1 the second.

## 2. Same host preparation as the SFU

```bash
sudo host/10-sysctl-limits.sh   # log out/in afterwards
sudo host/20-cpu.sh
sudo host/30-nic.sh             # uses nic.gen / cpu.gen_irq on generators  (reboot → redo)
sudo host/40-chrony.sh          # becomes a chrony client of chrony.server (the SFU)
sudo host/50-node-exporter.sh
```

Wait ~5 minutes, then check the clocks. Both generators must be within 100 µs
of the server, and therefore of each other:

```bash
chronyc tracking | grep -E "System time|RMS offset"
```

The one-way delay numbers are only as good as this sync. If you can't get below
100 µs, use only the delay **difference between SFUs** (the offset error is
the same for both) and say so in the report.

## 3. Build the load generator

```bash
cd /bench/client
./build.sh                      # go build + scripts/make-media.sh (ffmpeg, ~10 s)
ls media/                       # vp8-180.ivf vp8-360.ivf vp8-720.ivf
```

The IVF files are the layers every simulated publisher sends, as defined in
`bench.yaml` → `loadgen.media` (default VP8 simulcast, 150/400/1500 kbps,
30 fps, keyframe every 2 s plus on PLI). `codec: vp9` generates
`vp9-<height>.ivf` and publishes them as one spatial SVC stream. Use the same
files on both generators (copy them; don't regenerate per host). If you change
`loadgen.media.layers` or `codec`, regenerate with `client/scripts/make-media.sh`
and copy again.

## 4. Run a plan

One process runs the plan, pinned to `cpu.gen`. It serves Prometheus on `loadgen.monitoring_port` (9200) for the duration of the run.

```bash
client/start-agent.sh mediasoup client/plans/00-smoke.yaml
```

Participant settings (freeze threshold, max concurrent joins, ICE addresses, SDP debug logging) are under `loadgen` in `bench.yaml`. The process advertises only the addresses in `loadgen.ice_local_ips`.

What the agent does per simulated participant: one WebSocket to the SFU's
signaling, one send PeerConnection (publishers only) and one receive
PeerConnection, so up to 2 UDP sockets + 1 TCP connection. How many
participants one generator can carry depends on the scenario (a group-6
subscriber receives 5 videos, a webinar viewer 1). Don't guess: the
calibration run (docs/06) measures it.

## 5. Monitoring (gen-1 only)

No Grafana and no public port on any bench host. Prometheus runs on gen-1 only,
bound to `monitoring.prometheus_url` (keep it `http://127.0.0.1:9090`); the
report (`loadgen report`) runs on gen-1 too, so it reaches it there. Exporters
(node_exporter :9100, agent :9200, SFU /metrics) only need to be reachable
from gen-1: firewall them to the bench subnet.

```bash
monitoring/start-prometheus.sh  # or: docker compose -f docker/compose.gen.yaml --profile monitoring up -d
```

Check targets from your laptop through SSH: `ssh -L 9090:127.0.0.1:9090 gen-1`,
then `http://localhost:9090/targets`. `node`, `node-gen` and `loadgen` must be
UP; `livekit` / `mediasoup` are UP only while that SFU runs.

### Viewing metrics on your laptop

After a run (or a batch of runs), on gen-1:

```bash
monitoring/export.sh results/<run> [results/<run2> ...]
# -> results/metrics/prom-gen-1-<time>.tar.zst  (TSDB snapshot + those reports)
```

On your laptop (needs Docker and a copy of this repo):

```bash
scp gen-1:/bench/results/metrics/prom-gen-1-<time>.tar.zst .
monitoring/view-local.sh prom-gen-1-<time>.tar.zst   # Prometheus :9091 + Grafana :3001, both on 127.0.0.1
monitoring/view-local.sh --down
```

It prints a Grafana link already set to the data's time range. The
"SFU bench" dashboard and its datasource are provisioned. The data on gen-1 is
kept for `monitoring.retention`, so you can export again at any time.

Live view during a run, without exposing anything: `ssh -L 9090:127.0.0.1:9090 gen-1`
and point a local Grafana datasource at `http://host.docker.internal:9090`
(or `http://localhost:9090` for a native Grafana).

### Live view through the SFU host (metrics gateway)

When the laptop can reach only the SFU host's public ports,
every generator runs `metrics-gw`, an HAProxy on `monitoring.gen_gateway_port`
(30699) bound to the generator's bench IP. It answers only `GET /loadgen/metrics`
(loadgen :9200, 503 between runs) and `GET /node/metrics` (node_exporter :9100):

```bash
docker compose -f docker/compose.gen.yaml run --rm render-monitoring   # also writes monitoring/gen-gw/haproxy.cfg
docker compose -f docker/compose.gen.yaml --profile gateway up -d metrics-gw
curl -s -o /dev/null -w '%{http_code}\n' http://<gen-ip>:30699/node/metrics   # 200
```

render-monitoring rewrites `monitoring/prometheus/prometheus.yml` too: keep a
copy first if you edited it by hand.

To scrape the generators from outside, have a reverse proxy on the SFU host
forward a public port to each generator's metrics-gw (:30699), for example
`/gen-1/…` → `<gen-1-ip>:30699`, and point your Prometheus at it with each
generator's own `instance` label, so the dashboards read the same series as
from the bench Prometheus. A generator with ufw on must allow the SFU host's
address to reach port 30699.

## 6. Preflight (before every run, both generators)

```bash
sudo host/90-preflight.sh <run-name>
```

## Per-run checklist on the generators

1. Agent running on both (`curl gen-1:9200/status`, `curl gen-2:9200/status`)
2. `sudo host/90-preflight.sh <run-name>` passes (chrony offset < 100 µs!)
3. If testing impairment: `sudo host/netem.sh on 40 5 1` on **both**, and the same for both SFUs
4. gen-1 only: `scripts/run-scenario.sh <sfu> <plan>`
5. While it runs: watch the "Generators" row in Grafana (through the SSH tunnel). Above ~70% CPU, or any drops, means the step is invalid. The report checks the same thing afterwards.
6. `monitoring/export.sh results/<run>` and download it (see above)
