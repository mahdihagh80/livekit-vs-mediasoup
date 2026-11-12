# 00. Setup flow: from empty servers to the first real run

The order to follow on the real hosts. Each step links to the doc with the
details. Tick the boxes as you go and don't skip a check: a missed step shows
up later as a wrong number, not as an error.

```
  laptop                     gen-1 (controller)           gen-2            SFU (xFusion)
  ──────                     ──────────────────           ─────            ─────────────
  A. bench.yaml        ───►  copy repo + bench.yaml  ───►  same      ───►  same
                             B. host prep (root)          same             same
                                                                           C. install SFUs
                             D. build loadgen + agent     agent
                             E. Prometheus (127.0.0.1)
                             F. smoke both SFUs
                             G. calibrate generators
                             H. LiveKit layout sweep → matrix
  I. export.sh  ◄────────── results/metrics/*.tar.zst
     view-local.sh (Grafana on the laptop)
```

Roles: **SFU** = the xFusion, runs LiveKit *or* mediasoup. **gen-1, gen-2** =
client servers, each runs one loadgen agent. **gen-1** also runs the controller
(`run-scenario.sh`) and the only Prometheus. No host runs Grafana or exposes a UI.

## Before you start (decide, don't configure yet)

- [ ] The SFU is out of production traffic for the whole campaign.
- [ ] You know each host's **data-plane IP** and **data NIC** name (the 40 Gb link, not management).
- [ ] SSH from your laptop to all three hosts; sudo on all three.
- [ ] The three hosts reach each other on the data IPs with no NAT/stateful firewall between them.
- [ ] SLO in `loadgen.report.slo` agreed (docs/01). Fix it before the first real run; never change it mid-campaign.

## A. One bench.yaml for all hosts (laptop)

```bash
cd sfu-bench
cp bench.yaml.example bench.yaml
```

Fill in, at minimum:

| Key | Value |
|---|---|
| `hosts.sfu_ip`, `hosts.gen_ips`, `hosts.ctl_host` | data-plane IPs; `gen_ips[0]` = gen-1 = `ctl_host` |
| `nic.sfu.name`, `nic.gen.name` (+ `slaves` if bonded) | data NIC names |
| `cpu.*`, `mediasoup.control_cpus` / `worker_cpus`, `livekit.layouts` | CPU plan. Leave the example for now; finalize after step B1 (inventory) |
| `livekit.api_secret` | a long random string |
| `monitoring.prometheus_url` | keep `http://127.0.0.1:9090` (Prometheus stays private on gen-1) |

Then copy the **whole repo** (with `bench.yaml`) to `/bench` on all three hosts.
That is the layout on every server: `/bench/mediasoup`, `/bench/livekit`,
`/bench/client`, and the shared files directly in `/bench`
(`bench.yaml`, `host/`, `tools/`, `monitoring/`, `scripts/`, `docker/`, `docs/`).

```bash
for h in sfu gen-1 gen-2; do rsync -a --exclude results --exclude '**/node_modules' ./ $h:/bench/; done
```

Every later edit of `bench.yaml`: make it on the laptop and re-copy it to all three.
Preflight saves each host's copy, so a mismatch is visible afterwards.

## B. Host preparation (all three hosts, root)

On each host, in the repo directory:

```bash
tools/get-yq.sh                  # or copy tools/bin/yq if there's no internet
host/00-inventory.sh             # B1: records hardware, changes nothing
```

**B1, SFU only:** read the inventory (NIC's NUMA node, its CPUs, queue count)
and decide the CPU plan ([docs/02 §2](02-sfu-server-prep.md#choosing-the-cpu-plan-benchyaml)).
Put it into `bench.yaml` on the laptop and re-copy to all hosts. Do the same
for `cpu.gen_irq` / `cpu.gen` from a generator's inventory.

Then on each host, SFU first (it is the chrony server):

```bash
sudo host/10-sysctl-limits.sh    # then log out and back in
sudo host/20-cpu.sh              # (reboot → redo)
sudo host/30-nic.sh              # (reboot → redo)
sudo host/40-chrony.sh           # SFU: server; generators: clients of it
sudo host/50-node-exporter.sh
```

Firewall: open the SFU ports from [docs/02 §9](02-sfu-server-prep.md#9-open-ports-if-a-host-firewall-is-active)
to the generators only, and `:9100` (+ `:9200` on generators) to gen-1 only.
Nothing to the internet.

Checks:

- [ ] generators: `chronyc tracking` offset < 100 µs (wait ~5 min after 40-chrony)
- [ ] `ethtool -n <nic> rx-flow-hash udp4` lists L4 bytes on the SFU
- [ ] from gen-1: `curl -s <each host>:9100/metrics | head -1` answers for all three
- [ ] `sudo host/90-preflight.sh setup-check` is all `[ OK ]` on all three

Details: [02-sfu-server-prep.md](02-sfu-server-prep.md), [03-client-server-prep.md](03-client-server-prep.md).

## C. Install both SFUs (SFU host)

Pick **native or Docker** now and use it for both SFUs for the whole campaign.

```bash
# native LiveKit
sudo mkdir -p /opt/sfu-bench && sudo chown $USER /opt/sfu-bench
livekit/get-binary.sh            # LiveKit open-source livekit.version
```

Mediasoup, and LiveKit if you chose Docker: `docker compose -f docker/compose.sfu.yaml --profile livekit --profile mediasoup build` ([08-docker.md](08-docker.md)).

Checks (start each, look, stop it; never both at once):

- [ ] `livekit/start.sh <first layout>` → "instance i up" for every instance; `livekit/stop.sh`
- [ ] `docker compose -f mediasoup/compose.yaml up -d` → one worker per CPU in `worker_cpus`, all different; `docker compose -f mediasoup/compose.yaml down`

Review `livekit/livekit.base.yaml` now if you want production-like LiveKit
settings ([04-livekit.md §2](04-livekit.md#2-the-config)); changing it later
invalidates earlier LiveKit runs.

## D. Load generator (gen-1 and gen-2)

On **one** generator (needs Go ≥ 1.26 and ffmpeg):

```bash
client/build.sh                 # binary + media/vp8-{180,360,720}.ivf
```

Copy `client/loadgen` and `client/media/` to the other generator (same
files on both; don't regenerate). Each machine runs its own plan:

```bash
client/start-agent.sh mediasoup client/plans/00-smoke.yaml
```

Rebuild `loadgen` after a code change, then start the plan again.

## E. Prometheus (gen-1 only)

```bash
monitoring/start-prometheus.sh   # or: docker compose -f docker/compose.gen.yaml --profile monitoring up -d
```

Check from the laptop: `ssh -L 9090:127.0.0.1:9090 gen-1`, open
`http://localhost:9090/targets`.

- [ ] `node` (SFU), `node-gen` (both), `loadgen` (both) are UP
- [ ] `livekit` / `mediasoup` are DOWN for now (UP only while that SFU runs)

Viewing data later: [03 §5](03-client-server-prep.md#5-monitoring-gen-1-only).

## F. First load test: smoke, both SFUs

The first test is deliberately tiny (40 → 80 participants, ~3 min). It proves
the chain works, not capacity. Every run, including this one, has the same shape:

```bash
sudo host/90-preflight.sh <run>                                   # all three hosts
livekit/start.sh <layout>                                         # SFU
scripts/run-scenario.sh livekit:<layout> client/plans/00-smoke.yaml   # gen-1
livekit/stop.sh                                                   # SFU, then wait ~60 s idle
```

Then the same with `docker compose -f mediasoup/compose.yaml up -d`, `scripts/run-scenario.sh mediasoup ...`, and `docker compose -f mediasoup/compose.yaml down`.

Checks, in `results/<run>/report.md` of both:

- [ ] every step PASS, connected = target, failed 0
- [ ] video ~1500 kbps per track, loss 0
- [ ] delay p50 about 1 ms (negative or > 10 ms → chrony, go back to B)
- [ ] every dashboard row has data: `monitoring/export.sh results/<lk-run> results/<ms-run>`,
      download, `monitoring/view-local.sh <file>` on the laptop

If anything fails: the table in [06-running.md](06-running.md#when-something-goes-wrong).

## G. Calibrate the generators

```bash
scripts/run-scenario.sh mediasoup client/plans/90-generator-calibration.yaml calib-ms   # gen-1
```

- [ ] note the first step where a generator passes ~70% CPU or shows drops; × 0.8 = the rig's ceiling
- [ ] cut each plan's `max` to that ceiling, or add a generator ([06](06-running.md#calibrate-the-generators))

## H. The campaign

1. LiveKit layout sweep with `02-group-6`, keep the best layout ([04](04-livekit.md#layout-sweep)).
2. The run matrix: LiveKit → mediasoup → LiveKit per scenario, preflight before each
   run, ~60 s idle between ([06](06-running.md#the-run-matrix)). Log every run in
   `docs/templates/run-log.md`.

## I. Get the data out (after each session)

```bash
monitoring/export.sh results/<run> [...]                            # gen-1
scp gen-1:/bench/results/metrics/prom-*.tar.zst .          # laptop
rsync -a gen-1:/bench/results/ ./results/                  # reports, preflight snapshots
monitoring/view-local.sh prom-<...>.tar.zst                         # laptop: Grafana on 127.0.0.1:3001
```

Write-up: [07-analysis.md](07-analysis.md).
