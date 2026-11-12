# 10. Metric stack for the LiveKit vs mediasoup decision

This file covers what is measured, where it is measured, and how the numbers
decide between the two SFUs. It refines docs/01 ("What is measured") and
docs/07. Where they disagree, this file wins.

## The two questions

| # | Question |
|---|---|
| Q1 | **Same load → which SFU gives users a better experience?** (latency, jitter, loss as the client sees them) |
| Q2 | **Same resources → how much load can each SFU carry?** |

**In one line:** clients decide quality, the SFU machine's CPU decides cost,
and each SFU's own metrics only explain the result.

---

# Part 1: What is measured, and where

There are four sources.

## A. Clients (the loadgen, the same Go binary for both SFUs)

Code: `client/internal/stats/`. Served by every loadgen agent and scraped by Prometheus.

### Session

| Metric | Meaning |
|---|---|
| `bench_info`, `bench_step`, `bench_target_participants` | Run labels, the current ramp step, and how many participants the step should have |
| `bench_participants{state}` | Participants by state (connecting / connected / failed) |
| `bench_join_duration_seconds` | Join start → connected and publishing (signaling + ICE + DTLS + publish) |
| `bench_ttff_seconds` | Time to first frame: join start → first complete video frame received |
| `bench_join_failures_total{stage}` | Failed joins, by the stage where they failed |
| `bench_disconnects_total` | Connected participants that dropped unexpectedly |

### Publisher side

| Metric | Meaning |
|---|---|
| `bench_pub_bytes_total{kind}`, `bench_pub_frames_total{kind}` | What the publishers sent |
| `bench_pub_rtcp_total{type}` | RTCP the publisher **received** from the SFU (NACK, PLI, FIR) |
| `bench_pub_forced_keyframes_total` | Keyframes sent because the SFU asked with PLI/FIR |
| `bench_pub_rtt_seconds` | Round trip publisher ↔ SFU, from the SFU's receiver reports (RTCP LSR/DLSR). It is the same path for both SFUs. DLSR subtracts the time the SFU holds the report, so this shows network and socket queuing, not forwarding delay |

### Subscriber side

| Metric | Meaning |
|---|---|
| `bench_sub_tracks{kind}` | Tracks currently receiving media |
| `bench_sub_packets_total{kind}`, `bench_sub_bytes_total{kind}` | What the subscribers received (RTX included) |
| `bench_sub_lost_packets_total{kind}` | **Loss after repair**: packets that never arrived, even after retransmission |
| `bench_sub_nack_requested_packets_total` | Packets the subscriber asked to be resent. This is roughly **loss before repair** |
| `bench_sub_rtx_packets_total{kind}` | Retransmitted packets received |
| `bench_sub_out_of_order_packets_total{kind}` | Packets that arrived after a later one (RTX not counted) |
| `bench_sub_rtcp_sent_total{type}` | NACK/PLI/FIR the subscriber sent |
| `bench_sub_e2e_delay_seconds{kind}` | **One-way delay**: publisher sends → subscriber receives. A send timestamp (`BNCH` trailer) is embedded in each frame or packet, so it includes the time spent inside the SFU. Needs chrony |
| `bench_sub_jitter_seconds{kind}` | How uneven packet arrival is (RFC 3550 interarrival jitter), one sample per track per second |
| `bench_sub_frames_total` | Video frames received |
| `bench_sub_freezes_total`, `bench_sub_freeze_seconds_total` | Video stalls: a gap between frames > 200 ms |
| `bench_sub_video_tracks_by_quality{quality}`, `bench_sub_video_bytes_by_quality_total{quality}` | Tracks and bytes per requested layer (low/medium/high). Checks that the SFU sends the layer that was asked for |

## B. LiveKit server (`livekit_*`, built into LiveKit)

* Rooms, participants, tracks published and subscribed.
* Packets and bytes in and out, NACK/PLI/FIR counts.
* `livekit_jitter_us`, `livekit_rtt_ms`, `livekit_packet_loss_percent`, by `direction`:
  * **incoming** (publisher → LiveKit) is LiveKit's own measurement;
  * **outgoing** (LiveKit → subscriber) is copied from the subscriber's RTCP receiver reports.

  Each is one sample per track per 5 s. Jitter and RTT are the *max* in that window.
* Go runtime: goroutines, GC, memory.

## C. mediasoup server (our code)

| Metric | Meaning |
|---|---|
| `mediasoup_*` (Node media service, port 446) | Per worker: CPU (from /proc), memory, routers, transports, producers, consumers, peers. Also rooms, RPC latency, ICE/DTLS state changes, pipes between workers, and consumer score (mediasoup's own 0–10 quality score) |
| `mssig_*` (Go signaling, port 447) | WebSocket connections, rooms, request latency, media RPC latency, server-side consumes, dropped messages |

## D. Machines (node_exporter on the SFU server and both generators)

* CPU, per core and per mode, including softirq (the kernel's network-processing time).
* Memory.
* NIC packets, bytes and drops.
* UDP socket buffer drops (`node_netstat_Udp_RcvbufErrors`, `SndbufErrors`).
* Clock offset (`node_timex_offset_seconds`).

---

# Part 2: How to decide which SFU is better

## Setup, the same for both SFUs

* The same server and the same pinned CPU cores (`cpu.sfu` in `bench.yaml`).
* The same plan: the same rooms, the same number of participants per step, the
  same media, the same layer requests (`subscribe: {high, medium, low}`).
* Run order LiveKit → mediasoup → LiveKit, at the same time of day.
* Each step holds for 5 minutes. The first minute after everyone joins is ignored (`warmup`).

## Q1: same load, which gives users a better experience?

Only **client metrics** are used, compared step by step:

| Compare | From | Better |
|---|---|---|
| Latency | one-way delay p95 / p99 (video and audio) | lower |
| Jitter | subscriber jitter p95 | lower |
| Loss before repair | NACK-requested ÷ packets | lower |
| Loss after repair | lost ÷ (received + lost) | lower |
| Video stalls | freeze time ÷ video track time | lower |
| Network leg RTT (secondary) | publisher RTT p95 | lower |

**Two rules:**

1. **The step must be complete for both SFUs.** All expected tracks arrived,
   at the requested layer (tracks forwarded and kbps per layer). Otherwise an
   SFU that drops tracks or sends low quality would look *better* on jitter
   and loss.
2. **The gap must be bigger than the noise.** If LiveKit run 1 and LiveKit
   run 3 differ by X, a gap smaller than X between the SFUs means no difference.

**Result:** one chart per metric, with participants on the x-axis and both
SFUs as two lines. At low load they are usually equal. The interesting part is
where the lines separate. A table at one fixed load (70% of the lower capacity)
goes in the report next to the charts.

Subscriber RTT is not measured. A subscriber can only measure it if the SFU
answers an RTCP XR request (RRTR → DLRR), and LiveKit and mediasoup differ
there. It wouldn't be the same measurement on both.

## Q2: same resources, how much load?

**1. Capacity.** Every step gets PASS or FAIL against a fixed SLO. The SLO is
the same for both SFUs and lives in `bench.yaml` → `loadgen.report.slo`:

| Check | Limit |
|---|---|
| video one-way delay p95 | ≤ 100 ms |
| audio one-way delay p95 | ≤ 60 ms |
| video jitter p95 | ≤ 30 ms |
| loss after repair | ≤ 1% |
| freeze time | ≤ 1% |
| every layer's bitrate | ≥ 0.8 of what that layer should carry |
| unexpected disconnects | 0 |
| join success | ≥ 99.5% |

**Capacity = the last step that passes.** For example, LiveKit 4,800
participants vs mediasoup 6,000.

**2. Efficiency.** At the highest step **both** pass, compare from the SFU
machine's node_exporter:

* CPU cores used, and cores per 1000 forwarded video tracks;
* memory;
* busiest single core (a mediasoup worker or a NIC IRQ core can hit 100% while the average is low);
* softirq %, NIC Mpps/Gbps and drops (if both SFUs hit these first, the limit is the box, not the SFU).

For example: at 4,800 participants, LiveKit uses 40 cores and mediasoup uses 28.

If neither SFU fails before the plan's max step, capacity is "≥ max" for both,
and efficiency decides.

## What each source is for

| Source | Used for |
|---|---|
| Clients (`bench_*`) | Q1 comparison and the Q2 PASS/FAIL |
| node_exporter on the SFU | Q2 cost (CPU, memory, network) |
| node_exporter on the generators | Validity: proves the generator wasn't the bottleneck. A step with generator CPU > 70% or a generator kernel drop ratio > 0.5% (`max_gen_drop`: softnet backlog + UDP receive-buffer drops ÷ received packets) is **INVALID** for both SFUs. The NIC `rx_dropped` of a bond is shown but not used: it also counts frames the bond discards on purpose (inactive slave, unknown protocol), even on an idle host |
| `livekit_*`, `mediasoup_*`, `mssig_*` | **Not compared.** Each SFU calculates them differently, and they degrade with the SFU (LiveKit's `/metrics` passes the 16 MiB scrape limit at high load). They only explain *why* something failed, for example "one mediasoup worker hit 100% CPU" or "LiveKit's GC paused" |

The decision rule (how big a gap counts as "mediasoup wins") is in docs/01 and
is fixed before the first run.

---

# Part 3: Planned additions

These are not built yet. Each closes a gap in the approach above.

| # | Addition | Why |
|---|---|---|
| 0 | Report/compare: a "loss before repair" column, delay and jitter p50/p99 for audio and video, and quality-vs-load charts in `loadgen compare` | Answers Q1 with today's metrics. No loadgen change needed |
| 1 | **Fix frame completeness**: freezes and video delay should count a frame only when every packet of it arrived (RTX included). Also add late/incomplete frames | Today a frame counts on its last (marker) packet even if earlier packets are missing, so freezes are undercounted under loss |
| 2 | `bench_sub_expected_tracks{kind}` → track delivery ratio | Rule 1 of Q1 needs "all expected tracks arrived" as a number. The remote check's 56/75 video tracks was invisible |
| 3 | Per-track good-window ratio, `bench_sub_track_windows_total{result}`: % of 10 s track windows with good media, plus a worst-rooms log | Catches failures that hit only some rooms (one saturated mediasoup worker) while the overall p95 still looks fine |
| 4 | Validity: publisher send lag, offered tracks, clock offset and UDP drops in the report | A late generator makes the SFU look bad |
| 5 | Cost columns: egress Mpps per core, memory per participant, SFU UDP buffer drops, wire overhead (SFU NIC tx ÷ payload received) | Makes Q2 efficiency comparable across scenarios. Shows padding/probing cost |
| 6 | Layer correctness: received resolution vs requested, layer switches | Proves the layer check with resolution, not only bitrate |
| 7 | Audio gaps (inter-packet gap > 60 ms) | Audio-only scenario needs an audio equivalent of freezes |
