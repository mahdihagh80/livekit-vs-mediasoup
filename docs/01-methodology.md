# 01. Methodology: rules to agree on BEFORE the first run

Write these decisions down (copy this file into the final report) before any
numbers exist. A benchmark built to prove "mediasoup beats LiveKit" will find a
way to; one built to *test* it can come out either way.

## The question, made measurable

For each scenario, the result is:

1. **Capacity at SLO**: the largest step (participants) where every quality
   check passes and the generators are not saturated.
2. **Efficiency**: SFU cores used per 1000 forwarded video tracks, at the same
   step for both SFUs.
3. **Tail quality** at a common load (for example 70% of LiveKit's capacity):
   delay p99, jitter p95, freeze %.
4. **Join behaviour**: join p95 and success rate under a join storm.

**Decision rule (proposal, edit before running):** mediasoup "beats" LiveKit
if, on the target scenarios (1:1 and group-6, plus webinar for the class
product), its capacity at SLO is at least **20%** higher, or it uses at least
20% fewer cores at equal load, with no scenario where it is more than 10% worse.
The gaps must be larger than the run-to-run spread (see "Repeatability").

## Fairness rules

| Rule | How it's enforced |
|---|---|
| Same machine, same kernel, same NIC settings | `host/*.sh`, verified by `host/90-preflight.sh` before every run, snapshot saved |
| Same CPU budget | `cpu.sfu` in `bench.yaml`: LiveKit is pinned to it; mediasoup control plane + workers are carved from the same cores. NIC IRQs on their own `cpu.sfu_irq` |
| Same client stack and media | one `loadgen` binary, same Opus bytes and the same video (`loadgen.media.codec`: VP8 simulcast, or VP9 spatial SVC), same interceptors (NACK, RTCP reports, TWCC) |
| Same subscription model | both auto-subscribe to everything in the room (LiveKit default; our mediasoup signaling consumes server-side) |
| Same BWE situation | both run transport-cc BWE. Neither holds a subscriber's bitrate back, and both start high so the top layer flows at once: LiveKit's estimate starts at 100 Mbps; mediasoup's A/B config (`media.tuned.20c.yaml`) sets `initial_outgoing_bitrate` 100 Mbps and a 20 Mbps `max_outgoing_bitrate`, above every plan's per-subscriber demand, so its effective start is 20 Mbps |
| Each SFU in its best configuration | LiveKit: sweep of process layouts; mediasoup: one worker per core, placement tuned (`mediasoup.media.router_max_peers`) |
| Current open-source LiveKit | release `livekit.version` (1.13.11): GitHub binary natively, official image in Docker; config in `livekit/livekit.base.yaml` |
| No order/drift bias | LiveKit → mediasoup → LiveKit again, same time of day; repeat |
| Load generator is never the bottleneck | generator CPU/drops are in every report; steps over 70% CPU are marked INVALID |

## What is measured, and where it comes from

| Metric | Source | Notes |
|---|---|---|
| One-way delay video/audio | 12-byte trailer `BNCH`+send-ns at the end of every frame/packet, read on the receiver | needs chrony; on a LAN expect ~1 ms, so growth is SFU queuing |
| Jitter | RFC3550 interarrival jitter per track | |
| Loss | RFC3550 cumulative lost (sequence gaps not repaired by NACK/RTX) | |
| Freezes | inter-frame gap > 200 ms on the receiver | close to Chrome's definition at 30 fps |
| Received bitrate per video track | bytes / tracks | ~1.5 Mbps = top layer; lower means the SFU downgraded layers |
| Join time, TTFF | join start → connected / first full video frame | |
| SFU CPU (total, per core, softirq), NIC pps/bps/drops, memory | node_exporter on the SFU | host-level, so it covers *everything* the SFU needs (incl. mediasoup's Node + signaling) |
| Generator health | node_exporter on generators | validity check |
| Internals (diagnosis only) | `mediasoup_*`, `livekit_*`, Go runtime | not used for the verdict |

## Default SLO (pass line per step)

| Check | Limit | Why |
|---|---|---|
| video one-way delay p95 | ≤ 100 ms | LAN baseline is ~1-2 ms; 100 ms of SFU queuing is clearly broken |
| audio one-way delay p95 | ≤ 60 ms | |
| video jitter p95 | ≤ 30 ms | |
| packet loss after NACK | ≤ 1% | |
| frozen video time | ≤ 1% | |
| join success | ≥ 99.5% | |
| received video bitrate | ≥ 1200 kbps per track | catches an SFU "surviving" by sending lower layers |
| unexpected disconnects | 0 | |

They live in `bench.yaml` under `loadgen.report.slo`. Change them there (same
values for both SFUs) and re-run `loadgen report` on old runs any time.

## Scenarios (`client/plans/`)

| Plan | Shape | Stresses | Relevance |
|---|---|---|---|
| `01-one-to-one` | 2/room, both publish A+V | per-transport overhead, pps | most calls |
| `02-group-6` | 6/room, all publish | fan-out inside the SFU | group calls |
| `03-webinar` | 2000/room, 1 publisher | single-room fan-out across cores | class/webinar product |
| `04-audio-rooms` | 10/room audio only | small-packet pps | voice chats |
| `05-join-storm` | 6000 joins at N/s | signaling + ICE/DTLS setup | peak hours |
| `90-generator-calibration` | big 1:1 ramp | the rig itself | validity |

Each step holds steady for 5 min; the first 60 s after everyone joined are
excluded (`warmup`).

## Known differences (state them in the report)

* **Features.** LiveKit does more per participant (data channels, speaker
  detection, connection quality, dynacast plumbing). The mediasoup server here
  is a benchmark build with no auth, no recording, no reconnection, no speaker
  detection. A production mediasoup service will cost somewhat more. Estimate
  that cost, or add active-speaker observers to the mediasoup side as a
  sensitivity run.
* **Room size limits.** A LiveKit room lives on one process. A mediasoup room
  can spread over workers with `pipeToRouter`. That is a real architectural
  difference and part of what's being measured in the webinar plan.
* **Synthetic clients.** No decoding, no real camera, no browser. Real-browser
  probes (a few Chrome tabs) are recommended as a sanity check at one load
  level. See docs/06.
* **Network.** A clean LAN. Use `host/netem.sh` for an impaired-network pass
  if loss recovery matters to the decision.

## Repeatability

Run every SFU × scenario at least **twice** (three times for the scenarios
that decide). Report the capacity spread. A difference smaller than the
spread between two runs of the same SFU is not a difference.
