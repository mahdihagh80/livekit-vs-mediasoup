# 08. Docker deployment

Everything can run in containers instead of natively. **Use the same method
for both SFUs** (both native or both in Docker) within one comparison.

| Host | Compose file | Services |
|---|---|---|
| SFU (xFusion) | `docker/compose.sfu.yaml` for LiveKit; `mediasoup/compose.yaml` for mediasoup | `livekit` · `mediasoup-media`, `mediasoup-signaling` |
| gen-1, gen-2 | `docker/compose.gen.yaml` | `agent` · tools: `scenario`, `cli`, `make-media`, `render-monitoring` |
| controller (gen-1) | `docker/compose.gen.yaml` | profile `monitoring`: `prometheus` (loopback; no Grafana) |

## Configuration

Mediasoup containers mount only `mediasoup/media.yaml` and `mediasoup/signaling.yaml` at `/config`. The entrypoints pin from `control_cpus` in each file and pass that file to `--config` / `-config`. They do not mount the repo and do not read `bench.yaml`.

LiveKit and the generator containers still mount the repo at `/bench` and read `bench.yaml`. Compose files contain no environment variables and no `${...}` interpolation. Change the file that process reads, then recreate the container. Monitoring containers get generated files (`render-monitoring`), so they follow `bench.yaml`.

## Why the containers look the way they do

| Choice | Reason |
|---|---|
| `network_mode: host` | No docker bridge, NAT or veth in the media path. UDP ports (LiveKit mux, mediasoup WebRtcServer) bind directly on the host, exactly as native. |
| No `cpus:`/`cpuset:` limits; `taskset` inside | Mediasoup pins from `control_cpus` / `worker_cpus` in its two yaml files. LiveKit and the generators pin from `cpu.sfu` / `cpu.gen` in bench.yaml. cgroup CPU quotas would add throttling that native runs don't have. |
| `restart: "no"` for SFUs and agents | A crash must show up as a failed run, not get hidden by an automatic restart. |
| `ulimits.nofile` 1048576 | Tens of thousands of sockets. |
| Host prep stays on the host | sysctl, NIC queues/RSS/IRQ pinning, chrony and node_exporter are host-level; run `host/*.sh` exactly as in docs/02 and docs/03. With host networking the sysctls apply to the containers too. |
| LiveKit image = official binary on Debian slim | `livekit-server` copied from `livekit/livekit-server:v1.13.11` (statically linked), plus bash/taskset/yq for the entrypoint. No `apt` needed. |
| mediasoup worker fetched by the Dockerfile | The prebuilt `mediasoup-worker` is downloaded and extracted with `tar --no-same-owner` (works in rootless builders too); variant set by build arg `MS_WORKER_KERNEL` (`auto` = from the build host's kernel, or `kernel6`/`kernel5`). Ship `kernel6` to the SFU host (kernel 6.1); it can be built anywhere. mediasoup has no kernel5 prebuilt since 3.31, so `kernel5` compiles from source; `kernel7` needs glibc 2.38 and does not run on bookworm. The binary must load (exit 41 without `MEDIASOUP_VERSION`), else the worker is compiled from source (minutes). |
| static ffmpeg in the loadgen image | Debian's ffmpeg pulls ~400 MB of X/cairo libraries; a static build with libvpx (from `mwader/static-ffmpeg`) is enough for `make-media`. |

## Versions

| Image | Source | Change it in |
|---|---|---|
| `sfu-bench/livekit:1.13.11` | `livekit/livekit-server:v1.13.11` | `docker/livekit/Dockerfile` (`LIVEKIT_IMAGE`) and the tag in `docker/compose.sfu.yaml`; keep `livekit.version` in bench.yaml equal for native runs |
| `sfu-bench/mediasoup:local` (3.31.0), `sfu-bench/mediasoup:3.32.2-k6` | mediasoup from `mediasoup/media/package.json` (3.32.2), Node 22, Go 1.26 | `package.json` |
| `sfu-bench/loadgen:local` | `client/` (Go 1.26), static ffmpeg 7.1 | `client/go.mod` |

## SFU host

```bash
cd /bench
docker compose -f docker/compose.sfu.yaml --profile livekit --profile mediasoup build
```

Builds need the internet (Docker Hub, npm registry, Go module proxy, GitHub
for the mediasoup worker, Debian mirror for curl). Configure registry/proxy
mirrors in Docker if the host has no internet, or build elsewhere and
`docker save | docker load` (build the mediasoup image with
`--build-arg MS_WORKER_KERNEL=kernel6` for the kernel 6.1 SFU host).

LiveKit:

```bash
docker compose -f docker/compose.sfu.yaml --profile livekit up -d       # layout = livekit.active_layout
docker compose -f docker/compose.sfu.yaml logs -f livekit               # [lk0] [lk1] ... per instance
docker compose -f docker/compose.sfu.yaml --profile livekit down
```

Layout sweep: change `livekit.active_layout` in bench.yaml (on the SFU **and**
the controller host, since the controller derives the URLs from it), then
`up -d --force-recreate livekit`.

mediasoup:

```bash
docker compose -f mediasoup/compose.yaml up -d     # signaling waits until media is healthy
docker compose -f mediasoup/compose.yaml ps
docker compose -f mediasoup/compose.yaml down
```

Verify worker pinning from the host: `for p in $(pgrep -f mediasoup-worker); do taskset -pc $p; done`.

Never run both profiles at once: they'd share cores and the NIC.

## Generator hosts

```bash
cd /bench
docker compose -f docker/compose.gen.yaml build
docker compose -f docker/compose.gen.yaml run --rm make-media   # on ONE host; copy client/media/ to the other
docker compose -f docker/compose.gen.yaml up -d agent
curl -s localhost:9200/status
```

## Controller (gen-1)

```bash
docker compose -f docker/compose.gen.yaml run --rm render-monitoring
docker compose -f docker/compose.gen.yaml --profile monitoring up -d --force-recreate
# Prometheus only, on monitoring.prometheus_url (127.0.0.1). No Grafana on bench hosts:
# monitoring/export.sh here, monitoring/view-local.sh on your laptop (docs/03, section 5).

# A/B runs use `scenario` (mounted loadgen + run-scenario.sh), not `agent`.
# <run-name> = the preflight run name; an unnamed run needs `-e ALLOW_UNNAMED=1` (no SFU snapshot).
docker compose -f docker/compose.gen.yaml run --rm scenario mediasoup    client/plans/00-smoke.yaml <run-name>
docker compose -f docker/compose.gen.yaml run --rm scenario livekit      client/plans/00-smoke.yaml <run-name>   # active_layout
docker compose -f docker/compose.gen.yaml run --rm scenario livekit:3x16 client/plans/02-group-6.yaml <run-name>
docker compose -f docker/compose.gen.yaml run --rm cli report  -dir results/<run>
docker compose -f docker/compose.gen.yaml run --rm cli compare results/<run-a> results/<run-b>
```

Results land in `results/` on the host, owned by root (containers run as root):
`sudo chown -R $USER results` if needed.

## Per-run checklist (docker)

1. Only one SFU profile up: `docker ps` on the SFU host
2. `sudo host/90-preflight.sh <run-name>` on all three hosts (host-level checks still apply)
3. Agents up on both generators
4. `docker compose ... run --rm scenario <sfu> <plan>` on the controller
5. Stop the SFU profile, wait ~60 s idle, start the other one
