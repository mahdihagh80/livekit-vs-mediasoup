# 07. Analysis and the final report

## Per run

`results/<run>/report.md` has one row per step:

* **verdict**: `PASS`, `FAIL: <which checks>`, or `INVALID (generator)`
* quality columns (delay p50/p95/p99, jitter, loss, freeze %, kbps/track overall and per requested layer low/medium/high, join, ttff)
* SFU host columns (CPU %, cores used, softirq %, busiest core %, memory, rx/tx Mpps, rx/tx Gbps, drops)

Subscribers don't all pull the top layer: each plan's `subscribe: {high, medium, low}`
(percent of participants, default 20/50/30) picks the layer each participant
requests, the same participants on both SFUs (LiveKit: `UpdateTrackSettings`
quality; mediasoup: consumer `preferredLayers`). The `video bitrate <layer>` check
fails a step when a layer arrives below `min_video_ratio` (0.8) of its bitrate.
mediasoup reports carry a note: its signaling sends no presence notifications and
runs no active-speaker detection, so it does less control-plane work than LiveKit.
* generator columns (CPU max %, drops)

**Capacity at SLO** = the target of the last PASS step before the first
non-PASS step. Re-generate any time; Prometheus keeps the data for
`monitoring.retention`. The SLO, warmup, Prometheus URL and node selectors all
come from `bench.yaml` (`loadgen.report`, `monitoring`, `hosts`, `nic.sfu`).
To try a stricter SLO, edit a copy and point at it:

```bash
client/loadgen report -dir results/<run>                          # uses bench.yaml
client/loadgen report -dir results/<run> -config /tmp/strict.yaml # e.g. video_delay_p95: 80ms
```

Always report which SLO was used (the run's `results/preflight/<run>/*/bench.yaml`
has the file as it was at run time, secrets stripped).

## Comparing

```bash
client/loadgen compare results/livekit-1x48-02-group-6-20261101-1000 \
                        results/mediasoup-02-group-6-20261101-1110 \
                        results/livekit-1x48-02-group-6-20261101-1220 > results/compare-group-6.md
```

The output gives capacity per run, then per-target rows (pass, cores,
busiest core, delay p95/p99, jitter, loss, freeze, kbps, join p95), then an
efficiency table (SFU cores per 1000 forwarded video tracks).

## Results made before the NACK fix

Until the shared NACK generator, the two clients did not ask for repairs the same way:

- **LiveKit**: the SDK's NACK generator never saw RTX repairs, so it kept NACKing packets
  that had already been repaired.
- **mediasoup** before 2026-11-04 18:11 UTC (generator clock): Pion's stock generator did the
  same; mediasoup resent each one every time.

Runs from that time are **not comparable** on NACK/s, RTX pkt/s, dup RTX %, loss after
repair, freeze, and, on shaped profiles, every BWE-derived column. On a clean profile with ~0
subscriber loss the effect is negligible, so check sub loss before discarding a run. The
report applies the same rule (`nackParity`) and prints a banner on these runs; a run whose
`run.json` has `loadgen.nack_generator` used the fixed generator on both SFUs.


## How to read it

1. **Validity first.** Any INVALID step, disconnects, or preflight failures? If
   the two LiveKit runs differ by more than ~10% in capacity, find out why before
   you trust the mediasoup run in between.
2. **Capacity.** Compare capacity at SLO. If neither SFU fails before `max`
   (NIC- or generator-bound), capacity is "≥ max" for both; go to step 3.
3. **Efficiency at equal load.** At the highest step both pass, compare SFU
   cores used and cores per 1000 tracks. This is the number that becomes
   "servers needed" in the fleet plan.
4. **What fails first.** Look at the FAIL reasons and the dashboard at the knee:
   * LiveKit: GC pause/goroutines rising, delay p99 growing before loss; all cores busy evenly.
   * mediasoup: a single worker core at 100% (busiest core %), that worker's rooms degrade.
   * Either: SFU rx drops / softirq at 100% on IRQ cores means a NIC/IRQ limit; fix the plan and re-run both.
5. **Tails, not averages.** Compare delay p99 and freeze % at the same load.
   Users notice those.

## Final report outline (docs/templates/final-report.md)

1. Question and decision rule (from docs/01, written before the runs)
2. Setup: hardware inventory, CPU plan, versions (LiveKit build, mediasoup, kernel, NIC driver/firmware), the LiveKit config diff vs production
3. Validity: generator calibration, chrony offsets, preflight snapshots, run-to-run spread
4. Results per scenario: capacity, efficiency, tails; one comparison table and 2-3 Grafana screenshots each (participants + delay p95 + per-core CPU)
5. Known differences and their estimated effect (missing features on the mediasoup side, synthetic clients)
6. Decision, plus what it means for the fleet: servers for today's peak with each SFU
7. Next steps if migrating: what the production mediasoup service needs that the benchmark build lacks
