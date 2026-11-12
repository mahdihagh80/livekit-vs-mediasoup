# 06. Running the benchmark

## Anatomy of one run

```
$ scripts/run-scenario.sh <mediasoup|livekit> client/plans/<plan>.yaml <run-name>
  0. the SFU URL, Prometheus, and the SLO come from bench.yaml
  1. one process waits 15 s (baseline Prometheus scrape)
  2. each room gains `add` people (publishers of that batch first), wait `every`,
     repeat until each room has maxParticipants, then wait `hold`
  3. everyone leaves
  4. report -> results/<run>/report.md + report.csv
```

Duration ≈ (adds - 1) × every + hold.

## Phase 6: smoke test (both SFUs)

```bash
# SFU host
livekit/start.sh 1x48
# gen-1
scripts/run-scenario.sh livekit:1x48 client/plans/00-smoke.yaml <run-name>
# SFU host
livekit/stop.sh
docker compose -f mediasoup/compose.yaml up -d
# gen-1
scripts/run-scenario.sh mediasoup client/plans/00-smoke.yaml <run-name>
```

Both reports must show PASS, kbps/track high ~1500 / medium ~400 / low ~150
(each plan's `subscribe` mix sets which layer each participant asks for), loss 0, and a delay p50 around
1 ms. Open the Grafana dashboard and check that every panel of the relevant
rows has data. If a delay panel is empty, the trailer isn't arriving; if the
delay is negative or huge, chrony isn't synced.

Optional browser probe: the mediasoup side has no browser client in this repo
(a typical browser client uses a different subscribe flow);
for LiveKit, LiveKit Meet / `lk room join` against the bench server works.
One probe in a 6-person room at a mid load level is enough to check that
synthetic numbers and real-client experience agree.

## Calibrate the generators

```bash
scripts/run-scenario.sh mediasoup client/plans/90-generator-calibration.yaml calib-ms
```

Use whichever SFU holds up better at high load. You're measuring the
**generators**: find the first step where a generator goes above ~70% CPU
(Grafana "Generator CPU %") or shows drops. That number × 0.8 is the most this
rig can test. If an SFU's capacity is above it, you can't measure that SFU's
limit with 2 generators. Add a third generator (add its IP to `hosts.gen_ips`
in bench.yaml on all hosts and start an agent there), or cut the plan's `max`.

### Load from several generators

One generator tops out around 10 Gb/s / its pinned cores. To go further, run
the **same plan on every generator at the same time**, each with its own
`loadgen.name_prefix` in its bench.yaml (`g1-`, `g2-`, …). Rooms become
`g1-room_1`, participants `g1-par_1_1`, so generators never share a room.
The plan is per generator: `rooms: 1500` on two generators = 3000 rooms.

```bash
# bench.yaml on gen-1: loadgen.name_prefix: "g1-"; on gen-2: "g2-"
# every generator's IP in hosts.gen_ips, so Prometheus scrapes all loadgens
# (re-render: docker compose -f docker/compose.gen.yaml run --rm render-monitoring)
# start within a few seconds of each other:
gen-1$ docker compose -f docker/compose.gen.yaml run --rm -T scenario mediasoup client/plans/<plan>.yaml <run-name>
gen-2$ docker compose -f docker/compose.gen.yaml run --rm -T scenario mediasoup client/plans/<plan>.yaml <run-name>
```

Read the report from the generator that runs the Prometheus (ctl_host): its
metrics are summed over all loadgens, the `generators` column says how many
reported, and `target (all gens)` is the total; join success and capacity use
that total when more than one generator reports. Run names get the prefix
(`…-g1`), so results dirs don't collide if you copy them together.

### Several generators in the same rooms (shared rooms)

The mode above gives every generator its own rooms. For one room bigger than a
generator can carry (a 3000-viewer webinar), or to put publisher and viewers
on different hosts, use `loadgen.shared_rooms` instead. Every generator runs
the **same plan** with the **same prefix** and its own shard:

```yaml
# gen-1 bench.yaml                                # gen-2 bench.yaml
loadgen:                                          loadgen:
  shared_rooms: {prefix: "class-", shard: "1/2"}    shared_rooms: {prefix: "class-", shard: "2/2"}
```

- Rooms are `class-room_1…`, the same on every shard. The plan's numbers
  (`rooms`, `maxParticipants`, `add`) are for the whole fleet, not per generator.
- Shard 1 creates the rooms: it runs every publisher and speaker plus its share
  of the viewers. Viewers are dealt round-robin (viewer i → shard i mod N), so
  each step grows every shard evenly.
- Shards 2..N wait `viewer_delay` (default 20 s) after the baseline before the
  first step, so the publishers are in first. Shard 1 holds that much longer, and
  its events are shifted by the same delay. Both shards then end, and fire events,
  at the same wall-clock time. Start the generators within a second or two of
  each other; events and steps line up only if they do.
- Participant identities are distinct per shard (position in the room), so
  `name_prefix` is optional; set it anyway if you want to tell shards apart in
  SFU logs. Run names get `-shard<k>of<N>`.
- Leaving `shared_rooms` unset keeps the isolated mode above.

### Classroom / disturbance plans

Plans can also turn on these options (all off by default; see
`client/plans/30-*.yaml`):

| plan key | what it does | metrics |
|---|---|---|
| `codec: vp9` | VP9 SVC (L3T3_KEY: three spatial layers in one stream, temporal layers 0,2,1,2 marked by the sender) instead of VP8 simulcast for this plan | – |
| `screenShare: true` | publisher 1 of each room also sends a screen track | – |
| `speakers`, `unmuted`, `rotate` | audio-only students; `unmuted` talk at a time, rotating (the rest send DTX) | `bench_unmute_heard_seconds` |
| `churn: {percent, every}` | that share of the viewers leave and rejoin every tick | `bench_rejoins_total{reason=churn}`, `bench_recovery_seconds{event=churn}` |
| `events:` `netloss` / `massRejoin` / `publisherReconnect` | disturbances `at` after hold starts, on `percent` of `target` (viewers / publishers / all) | `bench_events_total`, `bench_recovery_seconds{event}`, `bench_recovery_missed_total` |
| `network:` profiles | per-person subscribe-side downlink delay / jitter / uniform `loss` / rate limit (`kbps`) / periodic bandwidth `dip` / bursty loss (`burst: {loss: 5, length: 50ms}`), applied inside the loadgen (no tc, no root) | everything labelled `profile=…`, `bench_layer_switch_seconds{direction}` |
| `networkTarget: viewers/publishers/all` | whose subscribe connection gets a `network:` profile (default `viewers`). `all` impairs what publishers receive too, e.g. 1:1 calls where everyone publishes; viewers keep the profile they have with `viewers`. Publish connections are always clean | – |
| `steps: [1503, 1500, 1000, 1000]` | uneven ramp: people added per room at each step; replaces `add`, must add up to `maxParticipants` | `bench_target_participants` |
| `red: true` | LiveKit only: audio as `audio/red` (RFC 2198). Publishers send each Opus frame plus the previous one, every client accepts RED, so the SFU forwards RED to all viewers (165 bytes of audio payload per packet instead of 80) | `bench_sub_bytes_total{kind=audio}` ÷ `bench_sub_packets_total{kind=audio}` |
| `rejoin: true/false` | rejoin with backoff (1, 2, 4, 8, 10 s, 5 tries) after an unexpected drop; default on when the plan has events | `bench_rejoins_total{reason=drop}`, `bench_participants{state=reconnecting}` |

`client/plans/40-features.yaml` (isolated, both generators), `41-features-shared.yaml`
(shared rooms, shards 1/2 and 2/2) and `42-features-vp9.yaml` turn on every option
at a small load: run them after a client change to check it end to end.

Recovery is the time from a disturbance (or from the end of an outage) until
the first video keyframe (audio when the plan has no video) arrives again.
A disturbance with no media 60 s later counts as missed.

Things to know:

- mediasoup has no ICE restart in this client. A 5 s `netloss` usually comes
  back on its own (ICE consent holds for about 30 s). A longer one drops the
  transport, and the person rejoins (`reason=drop`). LiveKit resumes in place
  (`reason=sdk_resume`).
- Mute is media-level (DTX silence on the same track), not a signaling mute.
- A `massRejoin` herd is paced by `max_inflight_joins`, as all joins are.
- Network profiles shape only the subscribe connection (LiveKit subscriber PC,
  mediasoup recv transport), on the downlink (SFU → person). The publish
  connection is clean in both directions, also for a publisher with a profile
  (`networkTarget: all`): its media reaches the SFU intact and the SFU's
  NACK/PLI/congestion feedback on it arrives on time. Only outage events
  (`netloss`) hit both connections.
- `burst` is a time-based Gilbert-Elliott link: bad periods (every packet lost)
  last `length` on average, good periods are sized so the link is bad for
  `loss` % of the time, which is the average packet loss. Both periods are
  exponential and per person. It adds to `loss` (uniform) when both are set.
- E2E delay skips retransmitted (RTX) packets; after an outage their send
  times are seconds old.
- `livekit/livekit.base.yaml` sets `congestion_control.use_send_side_bwe:
  true`, as production does. Keep it: without it LiveKit negotiates REMB on the
  subscriber side, the loadgen (Pion) sends only TWCC, and LiveKit never
  switches down under a `dip` or `kbps` cap (the viewer just loses packets).
  With it, LiveKit switched down in about 1.7 s in a local test. mediasoup
  always uses TWCC. On a clean network this makes no difference.

### Per-viewer metrics (what the decision rests on)

Averages hide the viewers who have a bad time. Every subscribed track also
reports one sample per 5 s window, so these percentiles are over viewers, not
packets:

| metric | report column | dashboard row |
|---|---|---|
| `bench_sub_track_fps{profile}` | viewers fps<25 %, viewer fps p5 | Per-viewer quality |
| `bench_sub_track_loss_ratio{kind,profile}` | viewer loss p95 %, viewers loss>2% % | Per-viewer quality |
| `bench_sub_track_kbps_ratio{profile,quality}` (received / requested layer kbps) | viewer kbps/requested p5 | Per-viewer quality |
| `bench_sub_freeze_duration_seconds{profile}` (length of each freeze) | freeze length p95 s | Per-viewer quality |
| `bench_sub_layer_frames_total{delivered}` | frames below requested layer % | Per-viewer quality |
| `bench_gen_send_lag_seconds{kind}` (frame tick → write, sampled) | gen send lag p99 ms | Generators |

A step where a generator's send lag p99 is above `report.max_gen_send_lag`
(default 20 ms) is INVALID, like a generator over `max_gen_cpu`: its
publishers were late, so the SFU's numbers can't be trusted.

When a plan has `network:` profiles, the SLO columns and the verdict use only
the `clean` viewers (all viewers if the plan has no clean share). The report
adds a **Per network profile** table with the same quality columns for each
profile, and a **Disturbances and recovery** table (recovery by event, layer
switch by direction, rejoins by reason, unmute heard) for the whole run. A
requested layer that no clean viewer asked for shows "–", not 0. The Grafana
dashboard has a `profile` selector for the quality rows.

Also check the **SFU NIC**: 1:1 at 20k participants is ~42 Gb/s into the SFU.
If the SFU link is 40 Gb, the NIC caps the 1:1 and group plans before CPU
does. Then the comparison becomes CPU efficiency (cores per 1000 tracks) at
equal load rather than capacity.

## The run matrix

Run order per scenario: **LiveKit → mediasoup → LiveKit (repeat)**, back to
back, same time of day. Then repeat the whole triple on another day for the
deciding scenarios.

| # | Scenario | LiveKit layout | mediasoup | Runs |
|---|---|---|---|---|
| 1 | 02-group-6 | sweep 1x48 / 3x16 / 6x8 (picks best) | `router_max_peers: 200` | 1 each |
| 2 | 01-one-to-one | best | default | LK, MS, LK, then again another day |
| 3 | 02-group-6 | best | default | LK, MS, LK, then again |
| 4 | 03-webinar | 1x48 | sweep 100/200/400, then best | LK, MS, LK |
| 5 | 04-audio-rooms | best | default | LK, MS, LK |
| 6 | 05-join-storm @100/300/600 per s | best | default | LK, MS each rate |
| 7 | (optional) 02-group-6 with `netem on 40 5 1` | best | default | LK, MS |

Between runs: stop the SFU, wait ~60 s idle (the CPU graphs back to flat), run
preflight, start the other SFU.

### SFU snapshot (run.json provenance)

Give every run a name, and use the same name for preflight and the run. Run
preflight on the SFU host **after** the SFU is up: on the SFU, preflight calls
`host/91-sfu-snapshot.sh`, which writes `results/preflight/<run>/<sfu-hostname>/sfu.json`
(image, version, cpuset, the config with secrets stripped). Copy it to the
generator that runs `run-scenario.sh` before the run:

```bash
# gen-1, after preflight on the SFU host
scp <sfu-host>:<bench-root>/results/preflight/<run>/<sfu-hostname>/sfu.json results/preflight/<run>/<sfu-hostname>/
scripts/run-scenario.sh mediasoup client/plans/02-group-6.yaml <run>
```

`run-scenario.sh` passes the one `results/preflight/<run>/*/sfu.json` it finds to
the loadgen (`-sfu-snapshot`; `SFU_SNAPSHOT=<path>` overrides). Without it the
run still starts, with a WARNING, and `run.json` records `sfu_snapshot_problems: ["missing"]`.
A snapshot taken more than 12 h before the run, after it started, or for the
other SFU is recorded but flagged in the report's "Run provenance" section.

Run naming (automatic): `<sfu>[-<layout>]-<plan>-<yyyymmdd-hhmm>`. Keep a run log
(`docs/templates/run-log.md`) with anything unusual: restarts, warnings, a
generator hiccup.

## When something goes wrong

| Symptom | Likely cause | Fix |
|---|---|---|
| all joins fail (`stage=signal`) | wrong URL / firewall / SFU not started / wrong layout given | `client/loadgen urls -sfu ... [-layout ...]` prints the URLs; `curl` them from gen-1 |
| joins OK, no media (`bench_sub_tracks` 0) | UDP blocked, wrong `listen_ip` / `announced_address` in `mediasoup/media.yaml` | check the SFU's candidates; ports table in docs/02 |
| kbps/track high ~400 instead of ~1500 (`FAIL: video bitrate high`) | SFU BWE downgraded layers, or the SFU ignored the requested layer | under load that's a real result. At low load, check `initial_outgoing_bitrate` / `max_outgoing_bitrate` in the `mediasoup/media*.yaml` that `MEDIA_CONFIG` selects / LiveKit congestion config |
| delay p50 negative or > 10 ms at low load | clocks not synced | `chronyc tracking` on generators |
| one generator far busier | different `bench.yaml` on the two hosts, or IRQs not pinned | preflight on both (it saves each host's bench.yaml, secrets stripped) |
| SFU rx drops > 0 at modest load | RSS hash / IRQ plan | `host/30-nic.sh`, check `/proc/interrupts` spread |
| mediasoup: one core at 100% | a hot worker (big room) | lower `router_max_peers` in `mediasoup/media.yaml` |
| need to see the SDPs | mediasoup adapter negotiation problem | `loadgen.debug_sdp: true`, use a tiny plan |
| `settle timeout` | joinRate too high for the SFU/generators | that step's join metrics are the result; lower joinRate for steady-state plans |
