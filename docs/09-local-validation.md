# 09. Local validation (2026-10-25)

Everything was run on one machine: a 2 vCPU / 3 GB Ubuntu 22.04 VM, kernel 6.8,
config `examples/local.yaml` with the VM's LAN IP (LiveKit doesn't offer
loopback candidates). Plans: 4 then 8 participants, 4-person rooms, 2
publishers each, audio + VP8 simulcast. This validates the tooling, not capacity.

## What ran and passed

| Path | Commands | Result |
|---|---|---|
| Native build | `mediasoup/build.sh` steps, `go build` loadgen + signaling, `go vet` | OK |
| Native mediasoup | `mediasoup/start.sh` → `loadgen run` | 8/8 joined, 0 loss, e2e p95 ≈ 1 ms, ~1.5 Mbps/track |
| mediasoup placement | 1 pinned worker; 2 workers with a room spanning both (`router_max_peers: 3`) | 6/6 video tracks in both; pipes only between different workers |
| Native LiveKit 1.13.11 | `livekit/get-binary.sh`, `livekit/start.sh` (active_layout) → `loadgen run` | 8/8, 0 loss, e2e p95 ≈ 1.5 ms |
| Native full workflow | node_exporter, `monitoring/start-prometheus.sh`, `client/start-agent.sh`, SFU start, `scripts/run-scenario.sh mediasoup …` and `… livekit …` | Both reports PASS, all columns filled |
| Docker images | `docker compose … build` for livekit (official v1.13.11), mediasoup, loadgen | built |
| Docker tools | `run --rm make-media`, `run --rm render-monitoring`, `run --rm cli compare` | OK |
| Docker mediasoup | `--profile monitoring up`, `--profile mediasoup up` (signaling waits for healthy media), `up agent`, `run --rm scenario mediasoup …` | PASS, Prometheus targets up, Grafana datasource + dashboard provisioned |
| Docker LiveKit 1x2 | same with `--profile livekit`, `scenario livekit …` | PASS |
| Docker LiveKit 2x1 | two instances: each pinned to its own CPU, own ports; controller derived both URLs | PASS |
| Scripts | shellcheck (error level) on all scripts; `host/00-inventory.sh` | OK |

## Bugs found and fixed during this validation

| Bug | Effect | Fix |
|---|---|---|
| loadgen logged nothing | LiveKit SDK logger setup redirects Go's standard `log` into zap at warn level | `main()` restores `log` output |
| mediasoup same-worker pipe | a room growing past `router_max_peers` with no free worker created a 2nd router on the same worker; `pipeToRouter` failed ("Channel request handler … already exists"), some subscribers got no video | a room only grows onto workers it doesn't use; otherwise peers join its least-full router |
| Report said PASS without data | with Prometheus down, missing values counted as "no violation" | verdict `NO DATA`; join success also checked from the controller's own counts |
| Short windows lost columns | track/kbps/freeze subqueries used a fixed 15 s resolution | resolution scales with the window |
| Port 3000 clash | mediasoup's internal RPC default = Grafana's default port; on a shared host signaling talked to Grafana | RPC default moved to 127.0.0.1:3100 |
| `examples/local.yaml` | signaling and metrics listened on 127.0.0.1, but clients/Prometheus use `sfu_ip` | listen on 0.0.0.0 |
| mediasoup worker missing | an interrupted `npm install` left no `mediasoup-worker` | the image build downloads the prebuilt worker, or compiles it |
| mediasoup Docker build | worker tarball extraction fails in rootless/userns builders (tar keeps owners); source-build fallback lacked pip | Dockerfile downloads the worker with `tar --no-same-owner`; fallback installs python3-pip |
| loadgen image | Debian ffmpeg = ~400 MB of X libs and its postinst failed in rootless builds | static ffmpeg with libvpx |

## How Docker was tested here, and what that doesn't cover

The VM has no root and no Docker daemon, so the compose files were run with the
official `docker-compose` v5.6.1 binary against **rootless Podman 6.1** (Docker
API socket). Sandbox-only adjustments, kept **outside** the repo:

* an override lowering `ulimits.nofile` to the VM user's hard limit (524288)
  and running Prometheus/Grafana as root (single-UID user namespace);
* a loop running `podman healthcheck run` (Podman needs systemd for that;
  Docker runs healthchecks itself).

Not covered by this validation: real Docker Engine (expected to behave the same
or better), root-only host scripts (`10`–`50`, `90-preflight`, `netem`), the
multi-host setup (two generators, chrony), and anything about performance.
