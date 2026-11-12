#!/usr/bin/env python3
"""Generates sfu-bench.json (Grafana dashboard). Edit here, re-run, re-import."""
import json, os

DS = {"type": "prometheus", "uid": "${ds}"}
panels, pid, y = [], 1, 0

def row(title):
    global pid, y
    panels.append({"type": "row", "title": title, "id": pid, "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}})
    pid += 1; y += 1

_x = 0
def ts(title, targets, unit="short", w=8, h=8, desc="", stack=False, maxv=None):
    global pid, y, _x
    if _x + w > 24:
        _x = 0; y += h
    p = {"type": "timeseries", "title": title, "id": pid, "datasource": DS, "description": desc,
         "gridPos": {"h": h, "w": w, "x": _x, "y": y},
         "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 1, "fillOpacity": 10 if stack else 0,
                          "stacking": {"mode": "normal" if stack else "none"}, "showPoints": "never"}}, "overrides": []},
         "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi", "sort": "desc"}},
         "targets": [{"refId": chr(65 + i), "expr": e, "legendFormat": l, "datasource": DS} for i, (e, l) in enumerate(targets)]}
    if maxv is not None:
        p["fieldConfig"]["defaults"]["max"] = maxv
    panels.append(p)
    pid += 1; _x += w

def endrow():
    global y, _x
    y += 8; _x = 0

R = "$__rate_interval"
P = 'profile=~"$profile"'   # dashboard variable; publishers are always "clean"
q = lambda qq, kind, metric: f'histogram_quantile({qq}, sum by (le) (rate({metric}_bucket{{kind="{kind}",{P}}}[{R}])))'
# per-track window histograms: one sample per track per 5 s, so quantiles describe viewers
hq = lambda qq, metric, sel="", by="profile": f'histogram_quantile({qq}, sum by (le, {by}) (rate({metric}_bucket{{{P}{sel}}}[{R}])))'
# share of viewer samples at or below a bucket bound (Prometheus 3 stores le="25" as "25.0")
below = lambda metric, le, sel="", by="profile": f'100 * sum by ({by}) (rate({metric}_bucket{{le=~"{le}(\\\\.0)?",{P}{sel}}}[{R}])) / sum by ({by}) (rate({metric}_count{{{P}{sel}}}[{R}]))'
# sparse events (recoveries, layer switches): a longer window so single events show
EV = "2m"
eq = lambda qq, metric, by: f'histogram_quantile({qq}, sum by (le, {by}) (increase({metric}_bucket[{EV}])))'

row("Load (what the generators are doing)")
ts("Participants", [('sum by (state) (bench_participants)', "{{state}}"), ('sum(bench_target_participants)', "target"),
                    ('max(bench_step)', "step #")], desc="target = participants requested for the current step")
ts("Forwarded tracks (received by subscribers)", [('sum by (kind) (bench_sub_tracks)', "{{kind}}")])
ts("Join latency / time to first frame", [(f'histogram_quantile(0.95, sum by (le) (rate(bench_join_duration_seconds_bucket{{result="ok"}}[{R}])))', "join p95"),
    (f'histogram_quantile(0.95, sum by (le) (rate(bench_ttff_seconds_bucket[{R}])))', "ttff p95"),
    (f'sum(rate(bench_join_failures_total[{R}]))', "failures/s")], unit="s")
endrow()

row("Quality (what users would feel) — compare these between SFUs")
ts("One-way delay video (pub → SFU → sub)", [(q(0.5, "video", "bench_sub_e2e_delay_seconds"), "p50"),
    (q(0.95, "video", "bench_sub_e2e_delay_seconds"), "p95"), (q(0.99, "video", "bench_sub_e2e_delay_seconds"), "p99")], unit="s")
ts("One-way delay audio", [(q(0.5, "audio", "bench_sub_e2e_delay_seconds"), "p50"),
    (q(0.95, "audio", "bench_sub_e2e_delay_seconds"), "p95"), (q(0.99, "audio", "bench_sub_e2e_delay_seconds"), "p99")], unit="s")
ts("Jitter p95 (RFC3550)", [(q(0.95, "video", "bench_sub_jitter_seconds"), "video"), (q(0.95, "audio", "bench_sub_jitter_seconds"), "audio")], unit="s")
endrow()
ts("Packet loss at subscribers", [(f'100 * sum by (kind) (rate(bench_sub_lost_packets_total{{{P}}}[{R}])) / (sum by (kind) (rate(bench_sub_packets_total{{{P}}}[{R}])) + sum by (kind) (rate(bench_sub_lost_packets_total{{{P}}}[{R}])))', "{{kind}}")], unit="percent")
ts("Video frozen time", [(f'100 * sum(rate(bench_sub_freeze_seconds_total{{{P}}}[{R}])) / sum(bench_sub_tracks{{kind="video",{P}}})', "% of time frozen"),
    (f'sum(rate(bench_sub_freezes_total{{{P}}}[{R}]))', "freezes/s")], unit="short")
ts("Received video bitrate per track", [(f'sum(rate(bench_sub_bytes_total{{kind="video"}}[{R}])) * 8 / sum(bench_sub_tracks{{kind="video"}})', "bps/track")], unit="bps",
   desc="All viewers (no profile filter). By requested layer: the panel in the next row.")
endrow()
ts("RTCP arriving at publishers", [(f'sum by (type) (rate(bench_pub_rtcp_total[{R}]))', "{{type}}")], unit="pps")
ts("Forced keyframes (PLI/FIR answered)", [(f'sum(rate(bench_pub_forced_keyframes_total[{R}]))', "keyframes/s")])
ts("Unexpected drops", [(f'sum(increase(bench_disconnects_total[1m]))', "dropped participants / min")])
endrow()

row("Per-viewer quality (one sample per track per 5 s: the worst viewers, not the average packet)")
ts("Viewers with video fps ≤ 25", [(below("bench_sub_track_fps", "25"), "{{profile}}")], unit="percent",
   desc="Share of video tracks that got 25 fps or less in a 5 s window. Sent at 30 fps.")
ts("Video fps per viewer", [(hq(0.05, "bench_sub_track_fps"), "p5 {{profile}}"), (hq(0.5, "bench_sub_track_fps"), "p50 {{profile}}")], unit="short", desc="Sent at 30 fps")
ts("Packet loss per viewer", [(f'100 * {hq(0.95, "bench_sub_track_loss_ratio", by="kind, profile")}', "p95 loss {{kind}} {{profile}}"),
    (f'100 - {below("bench_sub_track_loss_ratio", "0.02", by="kind, profile")}', "tracks with > 2% loss {{kind}} {{profile}}")], unit="percent",
   desc="p95 of per-track loss %, and the share of tracks above 2% loss")
endrow()
ts("Received / requested kbps per viewer", [(hq(0.05, "bench_sub_track_kbps_ratio", by="quality"), "p5 {{quality}}"),
    (hq(0.5, "bench_sub_track_kbps_ratio", by="quality"), "p50 {{quality}}")], unit="percentunit",
   desc="1.0 = the viewer gets the layer it asked for. The SLO line is min_video_ratio (0.8).")
ts("Freeze length", [(hq(0.5, "bench_sub_freeze_duration_seconds"), "p50 {{profile}}"), (hq(0.95, "bench_sub_freeze_duration_seconds"), "p95 {{profile}}")], unit="s",
   desc="How long each freeze lasted (gap above loadgen.freeze_threshold). Many short ones and a few long ones feel different.")
ts("Frames vs requested layer", [(f'100 * sum by (delivered) (rate(bench_sub_layer_frames_total{{delivered!="requested",{P}}}[{R}])) / scalar(sum(rate(bench_sub_layer_frames_total{{{P}}}[{R}])))', "{{delivered}} than requested")], unit="percent",
   desc="lower = the SFU downgraded the viewer (BWE / congestion); 0 on a clean network")
endrow()
ts("Received bitrate per track by requested layer", [(f'8 * sum by (quality) (rate(bench_sub_video_bytes_by_quality_total{{{P}}}[{R}])) / sum by (quality) (bench_sub_video_tracks_by_quality{{{P}}})', "{{quality}}")], unit="bps",
   desc="Compare with loadgen.media.layers kbps (default 150 / 400 / 1500)")
ts("Video tracks by requested layer", [(f'sum by (quality) (bench_sub_video_tracks_by_quality{{{P}}})', "{{quality}}")], stack=True)
ts("Time to first frame", [(hq(0.5, "bench_ttff_seconds", by="profile"), "p50 {{profile}}"), (hq(0.95, "bench_ttff_seconds", by="profile"), "p95 {{profile}}")], unit="s")
endrow()
ts("Retransmission and feedback (subscribers)", [(f'sum by (kind) (rate(bench_sub_rtx_packets_total[{R}]))', "RTX received {{kind}}"),
    (f'sum by (profile) (rate(bench_sub_rtx_duplicate_packets_total[{R}]))', "RTX duplicate {{profile}}"),
    (f'sum(rate(bench_sub_nack_requested_packets_total[{R}]))', "seq requested in NACKs"),
    (f'sum by (kind) (rate(bench_sub_out_of_order_packets_total[{R}]))', "out of order {{kind}}"),
    (f'sum by (type) (rate(bench_sub_rtcp_sent_total[{R}]))', "sent {{type}}")], unit="pps")
ts("Publisher RTT to the SFU", [(f'histogram_quantile(0.5, sum by (le) (rate(bench_pub_rtt_seconds_bucket[{R}])))', "p50"),
    (f'histogram_quantile(0.95, sum by (le) (rate(bench_pub_rtt_seconds_bucket[{R}])))', "p95")], unit="s",
   desc="From the SFU's receiver reports. Rising with load = SFU queueing")
ts("Delay p95 by profile", [(hq(0.95, "bench_sub_e2e_delay_seconds", ',kind="video"'), "video {{profile}}"),
    (hq(0.95, "bench_sub_e2e_delay_seconds", ',kind="audio"'), "audio {{profile}}")], unit="s")
endrow()

row("Disturbances and recovery (plans with events, churn, rejoin or network dips)")
ts("Events, rejoins, reconnecting", [('sum by (type) (increase(bench_events_total[1m]))', "event {{type}} / min"),
    ('sum by (reason) (increase(bench_rejoins_total[1m]))', "rejoin {{reason}} / min"),
    ('sum(bench_participants{state="reconnecting"})', "reconnecting now")])
ts(f"Recovery time (window {EV})", [(eq(0.5, "bench_recovery_seconds", "event"), "p50 {{event}}"), (eq(0.95, "bench_recovery_seconds", "event"), "p95 {{event}}")], unit="s",
   desc="Disturbance (or end of an outage) -> media again: first video keyframe, else audio")
ts("Recovery missed (no media within 60 s)", [(f'sum by (event) (increase(bench_recovery_missed_total[{EV}]))', "{{event}}")])
endrow()
ts(f"Layer switch time (window {EV})", [(eq(0.5, "bench_layer_switch_seconds", "direction"), "p50 {{direction}}"), (eq(0.95, "bench_layer_switch_seconds", "direction"), "p95 {{direction}}")], unit="s",
   desc="down: dip start -> lower layer delivered; up: dip end -> the layer from before the dip back. LiveKit needs use_send_side_bwe: true")
ts("Layer switch missed", [(f'sum by (direction) (increase(bench_layer_switch_missed_total[{EV}]))', "{{direction}}")])
ts("Unmute heard", [(f'histogram_quantile(0.5, sum by (le) (increase(bench_unmute_heard_seconds_bucket[{EV}])))', "p50"),
    (f'histogram_quantile(0.95, sum by (le) (increase(bench_unmute_heard_seconds_bucket[{EV}])))', "p95")], unit="s",
   desc="Speaker unmute -> first audio of that talk spurt at a listener (plans with speakers)")
endrow()

row("SFU host (xFusion) — node_exporter job=node")
ts("SFU CPU by mode (all cores)", [(f'100 * sum by (mode) (rate(node_cpu_seconds_total{{job="node",mode!="idle"}}[{R}])) / scalar(count(node_cpu_seconds_total{{job="node",mode="idle"}}))', "{{mode}}")], unit="percent", stack=True)
ts("SFU per-core busy %", [(f'100 * (1 - rate(node_cpu_seconds_total{{job="node",mode="idle"}}[{R}]))', "cpu{{cpu}}")], unit="percent", maxv=100,
   desc="IRQ cores should show softirq only; mediasoup: one line per pinned worker; a core at 100% = a saturated worker")
ts("SFU softirq per core", [(f'100 * rate(node_cpu_seconds_total{{job="node",mode="softirq"}}[{R}])', "cpu{{cpu}}")], unit="percent")
endrow()
ts("SFU NIC packets/s", [(f'sum(rate(node_network_receive_packets_total{{job="node"}}[{R}]))', "rx"), (f'sum(rate(node_network_transmit_packets_total{{job="node"}}[{R}]))', "tx")], unit="pps")
ts("SFU NIC bits/s", [(f'sum(rate(node_network_receive_bytes_total{{job="node"}}[{R}])) * 8', "rx"), (f'sum(rate(node_network_transmit_bytes_total{{job="node"}}[{R}])) * 8', "tx")], unit="bps")
ts("SFU drops (must stay 0)", [(f'sum(rate(node_network_receive_drop_total{{job="node"}}[{R}]))', "netdev rx drop"),
    (f'sum(rate(node_softnet_dropped_total{{job="node"}}[{R}]))', "softnet dropped"), (f'sum(rate(node_softnet_times_squeezed_total{{job="node"}}[{R}]))', "softnet squeezed"),
    (f'sum(rate(node_netstat_Udp_RcvbufErrors{{job="node"}}[{R}]))', "UDP rcvbuf errors"),
    (f'sum by (device) (rate({{job="node",__name__=~"node_ethtool_.*(rx_missed|rx_out_of_buffer|rx_discards|rx_dropped|missed).*"}}[{R}]))', "{{__name__}} {{device}}")], unit="pps")
endrow()

row("Generators — if these are saturated the step is INVALID")
ts("Generator CPU %", [(f'100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{{job="node-gen",mode="idle"}}[{R}])))', "{{instance}}")], unit="percent", maxv=100)
ts("Generator drops", [(f'sum by (instance) (rate(node_network_receive_drop_total{{job="node-gen"}}[{R}]))', "{{instance}} netdev"),
    (f'sum by (instance) (rate(node_netstat_Udp_RcvbufErrors{{job="node-gen"}}[{R}]))', "{{instance}} UDP rcvbuf")], unit="pps")
ts("Loadgen process", [(f'rate(process_cpu_seconds_total{{job="loadgen"}}[{R}])', "{{instance}} cores"), ('go_goroutines{job="loadgen"}', "{{instance}} goroutines")])
ts("Generator send lag p99 (INVALID above report.max_gen_send_lag)", [(f'histogram_quantile(0.99, sum by (le, instance, kind) (rate(bench_gen_send_lag_seconds_bucket[{R}])))', "{{instance}} {{kind}}")], unit="s",
   desc="Publisher frame tick -> write returned, sampled. Rising = the generator is late and its numbers are not trustworthy")
ts("Generator NIC bits/s", [(f'sum by (instance) (rate(node_network_receive_bytes_total{{job="node-gen"}}[{R}])) * 8', "{{instance}} rx"),
    (f'sum by (instance) (rate(node_network_transmit_bytes_total{{job="node-gen"}}[{R}])) * 8', "{{instance}} tx")], unit="bps")
ts("Loadgen memory", [('process_resident_memory_bytes{job="loadgen"}', "{{instance}} RSS"), ('go_memstats_heap_inuse_bytes{job="loadgen"}', "{{instance}} heap")], unit="bytes")
endrow()

row("SFU internals (one of these is empty depending on which SFU runs)")
ts("mediasoup worker CPU % (per worker)", [(f'100 * rate(mediasoup_worker_cpu_seconds_total[{R}])', "w{{worker}} cpu{{cpu}}")], unit="percent", maxv=100)
ts("mediasoup consumers per worker", [('mediasoup_worker_consumers', "w{{worker}}")])
ts("mediasoup control plane", [(f'histogram_quantile(0.95, sum by (le, method) (rate(mediasoup_rpc_duration_seconds_bucket[{R}])))', "rpc p95 {{method}}"),
    (f'rate(mediasoup_node_process_cpu_seconds_total[{R}])', "node cores"), ('mediasoup_consumer_score_avg', "consumer score avg")], unit="short")
endrow()
ts("LiveKit process CPU (cores) per instance", [(f'rate(process_cpu_seconds_total{{job="livekit"}}[{R}])', "{{instance}}")])
ts("LiveKit Go runtime", [('go_goroutines{job="livekit"}', "{{instance}} goroutines")])
ts("LiveKit GC pause (avg per GC)", [(f'rate(go_gc_duration_seconds_sum{{job="livekit"}}[{R}]) / rate(go_gc_duration_seconds_count{{job="livekit"}}[{R}])', "{{instance}}")], unit="s")
endrow()

dash = {
    "title": "SFU bench: LiveKit vs mediasoup", "uid": "sfu-bench", "editable": True, "schemaVersion": 39,
    "time": {"from": "now-1h", "to": "now"}, "refresh": "10s", "tags": ["sfu-bench"],
    "templating": {"list": [
        {"name": "ds", "type": "datasource", "query": "prometheus", "label": "Prometheus (bench)"},
        {"name": "profile", "label": "Viewer network profile", "type": "query", "datasource": DS,
         "query": {"query": "label_values(bench_sub_tracks, profile)", "refId": "profile"}, "definition": "label_values(bench_sub_tracks, profile)",
         "refresh": 2, "multi": True, "includeAll": True, "allValue": ".*", "current": {"text": "All", "value": "$__all"}, "sort": 1},
    ]},
    "panels": panels,
}
out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "sfu-bench.json")
json.dump(dash, open(out, "w"), indent=1)
print(f"wrote {out} ({len(panels)} panels)")
