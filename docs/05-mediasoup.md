# 05. mediasoup on the xFusion

## What is being tested

`mediasoup/` is a small but production-shaped mediasoup SFU, stripped down to what the benchmark needs:

```
loadgen ──WS──> signaling/ (Go, :8080)  ──HTTP /rpc──>  media/ (Node, :3100)  ──pipe──> mediasoup-worker × N
loadgen <═════════════ ICE/DTLS/SRTP, UDP 44444+i (one WebRtcServer port per worker) ═════════════> workers
```

| Piece | Behaviour |
|---|---|
| Workers | one per CPU in `media.yaml` `worker_cpus`, each pinned with `taskset` to its CPU, 16 MB socket buffers |
| Transports | WebRtcServer (single UDP port per worker), UDP only, no SCTP, `initialAvailableOutgoingBitrate` from `initial_outgoing_bitrate` (default 3 Mbps; the A/B config `media.tuned.20c.yaml` sets 100 Mbps), `maxOutgoingBitrate` from `max_outgoing_bitrate` (unset: the worker maximum, 1 Gbps; the A/B config sets 20 Mbps) |
| Placement | a room starts on the least-loaded worker (load = consumers + producers + 2×peers). Small rooms stay on one router: no pipes, cheapest path |
| Big rooms | after `router_max_peers` (default 200) peers on the current router, the room adds a router on the least-loaded worker it does **not** use yet; producers are `pipeToRouter`'d where needed. Never two routers of one room on the same worker (pipeToRouter keeps producer ids, so a same-worker pipe fails). Once a room spans every worker, new peers join its least-full router |
| Subscription | server-driven, like LiveKit auto-subscribe: join → consume all existing producers; produce → consume for all joined peers. Consumers start paused and are resumed by the client (plus a keyframe request) |
| Metrics | `:446` per-worker CPU (from /proc), consumers/producers/transports/peers, RPC latency, consumer score sample; `:447` signaling metrics |

Left out on purpose, as in any benchmark build: auth, TURN, reconnection/ICE
restart, recording, active-speaker detection, data channels. See
"Known differences" in docs/01.

## Configuration

Each process reads its own file (no environment variables):

| Process | File | Started as |
|---|---|---|
| Node media service | `mediasoup/media.yaml` | `node media/dist/index.js --config mediasoup/media.yaml [--pid-file f]` |
| Go signaling | `mediasoup/signaling.yaml` | `signaling/ms-signaling -config mediasoup/signaling.yaml` |

`media.yaml` holds `control_cpus`, `worker_cpus`, `cpu_budget`, the listen addresses, `announced_address` (empty means `listen_ip`), `rtc_base_port`, `router_max_peers`, the BWE keys (`initial_outgoing_bitrate`; optional `enable_bwe`, `min_outgoing_bitrate`, `max_outgoing_bitrate`, `max_incoming_bitrate`: leave one out for mediasoup's default, which for `max_outgoing_bitrate` is the worker maximum, 1 Gbps), `events_url`, `media_codecs` (omit it for Opus + VP8; the shipped file also lists VP9 profile-id 0), socket buffer size and worker log level. `signaling.yaml` holds `control_cpus`, the WebSocket and metrics listen addresses, the loopback `events_listen` (events from the media service and the health probe), `media_url` and `consume_concurrency`. `events_url` in `media.yaml` must be `events_listen` plus `/internal/events`. `cpu_budget` must equal `control_cpus` plus `worker_cpus`.

The load generator reads its signaling URL from `bench.yaml`. Set `mediasoup.signaling.url` to the WebSocket clients actually open (`wss://sfu.example.com/ws` in our setup: HAProxy terminates TLS on 443 and forwards to `signaling.yaml` `listen`). When `url` is empty, loadgen builds `ws://<hosts.sfu_ip>:<port of mediasoup.signaling.listen>/ws`, and that port has to be the one clients can reach.

On a host with a public IP, bind `listen_ip`, both `metrics_listen` addresses and signaling `listen` to the internal IP instead of `0.0.0.0`. The internal RPC and events listener stay on loopback.

## Build and run

The image is `docker/mediasoup/Dockerfile`. On the SFU host:

```bash
docker compose -f mediasoup/compose.yaml up -d
docker compose -f mediasoup/compose.yaml down
```

The media log lists the worker CPUs. Every worker must be on a different CPU from `media.yaml` `worker_cpus`. Then on gen-1: `scripts/run-scenario.sh mediasoup client/plans/<plan>.yaml <run-name>`. The URL comes from `bench.yaml` (`mediasoup.signaling.url`, or `ws://<hosts.sfu_ip>:<signaling port>/ws`). Record the `mediasoup <version>` line from the media log in the report.

## Tuning `router_max_peers` (webinar)

In a webinar room every viewer is a consumer of the presenter's audio + video.
One worker forwarding to `router_max_peers` viewers handles 2 × that many
consumers, plus one pipe consumer per extra router on the presenter's worker.

* Too high: one worker saturates (a single core at 100% in "SFU per-core busy")
  while others idle, and the room's viewers degrade together.
* Too low: more pipes and routers than needed (small overhead).

Run `03-webinar` with 200 (default), then try 100 and 400 for the best
mediasoup configuration, the same way LiveKit gets a layout sweep. Set it in
`mediasoup/media.yaml` (`router_max_peers`) and recreate the media container.

## What to watch during mediasoup runs

* **Per-core busy %**: mediasoup's failure mode is one worker (one core) hitting
  100% while the host total looks fine. The report's "SFU busiest core %"
  column shows it.
* "mediasoup worker CPU %" and "consumers per worker": placement should spread evenly.
* `mediasoup/run/media.log`: any `FATAL worker ... died` invalidates the run.
* Signaling: `mssig_dropped_messages_total` must stay 0.
