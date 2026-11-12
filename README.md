# livekit-vs-mediasoup

A benchmark rig for comparing two WebRTC SFUs, [LiveKit](https://github.com/livekit/livekit)
and [mediasoup](https://github.com/versatica/mediasoup), on the same server under the
same load: how many participants each carries at the same call quality, for how many
CPU cores, and how well viewers on impaired networks are served.

```
   loadgen  (one process, this machine)
   simulated users + Prometheus metrics :9200
                │  RTP/RTCP (UDP) + signaling (WS)
                ▼
   xFusion (SFU) — only ONE of these runs at a time:
     LiveKit 1.13.11 (open source, pinned)
     mediasoup (workers pinned, 1 per core)
```

A second generator is another `loadgen` invocation with its own plan. One load generator binary (`loadgen`,
Go + Pion) has two adapters: LiveKit through the official `server-sdk-go`, and
mediasoup through our own signaling. Both adapters send the **same bytes**
(pre-encoded VP8 simulcast 150/400/1500 kbps plus Opus) and measure the
**same things**: one-way delay (from a timestamp trailer in the payload),
jitter, loss, freezes, received bitrate, join time, and time to first frame.

On the servers the repo root is `/bench`:

| Path | Role |
|---|---|
| `/bench/mediasoup` | mediasoup SFU |
| `/bench/livekit` | LiveKit SFU |
| `/bench/client` | load generator. The binary is still `client/loadgen`; `bench.yaml` key `loadgen:` is unchanged |
| `/bench/bench.yaml`, `host/`, `tools/`, `monitoring/`, `scripts/`, `docker/`, `docs/` | shared by all three |

## Repository map

| Path | What |
|---|---|
| `bench.yaml.example` | Shared config for LiveKit, the load generator, and host scripts. Copy it to `bench.yaml`. Mediasoup does not read it; it reads `mediasoup/media.yaml` and `mediasoup/signaling.yaml` |
| `tools/get-yq.sh` | Installs yq (mikefarah v4) into `tools/bin/`; scripts use it to read `bench.yaml` |
| `host/` | Host preparation scripts, in order: `00-inventory` `10-sysctl-limits` `20-cpu` `30-nic` `40-chrony` `50-node-exporter` `90-preflight`, plus `netem` (optional). `lib.sh` loads `bench.yaml` |
| `livekit/` | `get-binary.sh` (open-source release `livekit.version`), `livekit.base.yaml` (the LiveKit config under test), `start.sh <layout>`, `stop.sh` |
| `mediasoup/` | The mediasoup SFU under test: `media/` (Node + mediasoup 3.x) and `signaling/` (Go). Runtime config is `media.yaml` and `signaling.yaml`. Started from `mediasoup/compose.yaml` |
| `client/` | Load generator: `agent`, `ctl`, `report`, `compare`, `run`, `urls`. `plans/*.yaml` are the scenarios. `media/` holds the IVF files (generated, not in the repo) |
| `monitoring/` | Prometheus config + start script (bench hosts), `export.sh` (TSDB tarball), `view-local.sh` + `local/` (Prometheus + Grafana on your laptop), dashboard `grafana/sfu-bench.json` |
| `docker/` | Docker deployment: images for LiveKit (open-source release), mediasoup and loadgen; `compose.sfu.yaml` (profiles `livekit` / `mediasoup`) and `compose.gen.yaml` (agent, tools, monitoring). See [docs/08-docker.md](docs/08-docker.md) |
| `scripts/run-scenario.sh` | Runs one scenario against the running SFU and writes the report |
| `docs/` | Detailed steps; see below |
| `results/` | Everything a run produces: inventory, preflight snapshots, `steps.csv`, `report.md`/`.csv` |

## Media clips

The pre-encoded video clips are not in the repo. Generate them once (needs ffmpeg
with libvpx), from the `loadgen.media` section of `bench.yaml`, and copy the same
files to every generator so both SFUs get identical bytes:

```bash
client/scripts/make-media.sh                                   # native
docker compose -f docker/compose.gen.yaml run --rm make-media  # or with Docker
```

## From zero to a finished benchmark

**Start here: [docs/00-setup-flow.md](docs/00-setup-flow.md)**, the step-by-step
checklist across laptop, gen-1, gen-2 and the SFU. Each phase below links to the
doc with the exact commands. Don't skip phases.

| # | Phase | Where | Doc |
|---|---|---|---|
| 0 | Decide the rules up front: SLO, scenarios, what counts as "mediasoup is better" | paper | [docs/01-methodology.md](docs/01-methodology.md) |
| 1 | Put this repo on all 3 hosts; `tools/get-yq.sh`; fill in `bench.yaml`; run `host/00-inventory.sh` | all | [docs/02-sfu-server-prep.md](docs/02-sfu-server-prep.md) |
| 2 | Prepare the SFU host (sysctl, CPU, NIC/RSS/IRQ, chrony server, node_exporter) | xFusion | [docs/02-sfu-server-prep.md](docs/02-sfu-server-prep.md) |
| 3 | Prepare both generator hosts (same scripts + build loadgen + start agents) | gen-1, gen-2 | [docs/03-client-server-prep.md](docs/03-client-server-prep.md) |
| 4 | Monitoring: loopback-only Prometheus on gen-1; export + view in Grafana on your laptop | gen-1, laptop | [docs/03-client-server-prep.md#monitoring](docs/03-client-server-prep.md#5-monitoring-gen-1-only) |
| 5 | Install both SFUs on the xFusion (LiveKit open-source binary; mediasoup build), natively or in Docker | xFusion | [docs/04-livekit.md](docs/04-livekit.md), [docs/05-mediasoup.md](docs/05-mediasoup.md) |
| 6 | Smoke test both SFUs (`00-smoke`) and check every dashboard panel has data | all | [docs/06-running.md](docs/06-running.md) |
| 7 | Calibrate the generators (find the rig's own ceiling) | all | [docs/06-running.md](docs/06-running.md#calibrate-the-generators) |
| 8 | LiveKit layout sweep (1x48 / 3x16 / 6x8), keep the best | all | [docs/04-livekit.md](docs/04-livekit.md#layout-sweep) |
| 9 | The matrix: each scenario, LiveKit, then mediasoup, then LiveKit again; repeated | all | [docs/06-running.md](docs/06-running.md#the-run-matrix) |
| 10 | Reports, comparison, decision write-up | gen-1 | [docs/07-analysis.md](docs/07-analysis.md) |

## Quick reference (after preparation)

```bash
# SFU host: start ONE SFU
livekit/start.sh 1x48          # mediasoup: docker compose -f mediasoup/compose.yaml up -d

# all hosts, before every run
sudo host/90-preflight.sh <run-name>

# gen-1: run a scenario + report (URLs, agents, Prometheus, SLO all come from bench.yaml)
scripts/run-scenario.sh livekit:1x48 client/plans/02-group-6.yaml <run-name>
scripts/run-scenario.sh mediasoup    client/plans/02-group-6.yaml <run-name>
client/loadgen compare results/livekit-1x48-02-group-6-* results/mediasoup-02-group-6-*
```

### Same thing with Docker

```bash
# SFU host (one profile at a time)
docker compose -f mediasoup/compose.yaml up -d          # image built from docker/mediasoup/Dockerfile
docker compose -f docker/compose.sfu.yaml --profile livekit up -d --build
# each generator
docker compose -f docker/compose.gen.yaml up -d --build agent
# controller
docker compose -f docker/compose.gen.yaml run --rm scenario mediasoup client/plans/02-group-6.yaml <run-name>
```

Host preparation (`host/*.sh`) is still done on the hosts themselves. Details: [docs/08-docker.md](docs/08-docker.md).

## Single-host try-out (development)

`examples/local.yaml` runs everything on one machine (loopback). Copy it to
`bench.yaml`, then:

```bash
docker compose -f mediasoup/compose.yaml up -d
client/loadgen -config bench.yaml mediasoup client/plans/00-smoke.yaml
```

## Validated so far

**2026-10-25, one machine (2 vCPU VM), everything from `bench.yaml`:** see
[docs/09-local-validation.md](docs/09-local-validation.md). Native and Docker
runs of both SFUs (open-source LiveKit 1.13.11, mediasoup 3.31.0) joined,
published simulcast, forwarded the top layer (~1.5 Mbps/track) with 0 loss, and
produced PASS reports through Prometheus. That only shows the tooling works and
treats both SFUs the same; it says nothing about capacity.
