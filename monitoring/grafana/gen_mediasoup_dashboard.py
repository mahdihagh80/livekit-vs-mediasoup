#!/usr/bin/env python3
"""Generates mediasoup.json (Grafana dashboard, mediasoup only). Edit here, re-run, re-import.

Sources: media service /metrics (mediasoup_*, :446) and Go signaling /metrics (mssig_*, :447).
"""
import json, os

DS = {"type": "prometheus", "uid": "${ds}"}
panels, pid, y = [], 1, 0

def row(title):
    global pid, y, _x
    if _x:
        endrow()
    panels.append({"type": "row", "title": title, "id": pid, "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}})
    pid += 1; y += 1

_x, _h = 0, 8
def _place(w, h):
    global y, _x, _h
    if _x + w > 24:
        _x = 0; y += _h
    g = {"h": h, "w": w, "x": _x, "y": y}
    _x += w; _h = h
    return g

def _targets(targets):
    return [{"refId": chr(65 + i), "expr": e, "legendFormat": l, "datasource": DS} for i, (e, l) in enumerate(targets)]

def ts(title, targets, unit="short", w=8, h=8, desc="", stack=False, maxv=None, minv=None):
    global pid
    p = {"type": "timeseries", "title": title, "id": pid, "datasource": DS, "description": desc,
         "gridPos": _place(w, h),
         "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 1, "fillOpacity": 10 if stack else 0,
                          "stacking": {"mode": "normal" if stack else "none"}, "showPoints": "never"}}, "overrides": []},
         "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi", "sort": "desc"}},
         "targets": _targets(targets)}
    if maxv is not None:
        p["fieldConfig"]["defaults"]["max"] = maxv
    if minv is not None:
        p["fieldConfig"]["defaults"]["min"] = minv
    panels.append(p)
    pid += 1

def stat(title, expr, unit="short", w=3, h=4, desc="", steps=None, decimals=None):
    global pid
    d = {"unit": unit, "thresholds": {"mode": "absolute", "steps": steps or [{"color": "blue", "value": None}]}}
    if decimals is not None:
        d["decimals"] = decimals
    panels.append({"type": "stat", "title": title, "id": pid, "datasource": DS, "description": desc,
                   "gridPos": _place(w, h), "fieldConfig": {"defaults": d, "overrides": []},
                   "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "area"},
                   "targets": _targets([(expr, "")])})
    pid += 1

def endrow():
    global y, _x
    y += _h; _x = 0

R = "$__rate_interval"
W = 'worker=~"$worker"'
G, Y, RED = "green", "yellow", "red"
hq = lambda qq, metric, by="le", sel="": f'histogram_quantile({qq}, sum by ({by}) (rate({metric}_bucket{sel}[{R}])))'

row("Overview")
stat("Rooms", "max(mediasoup_rooms)")
stat("WebSocket connections", "sum(mssig_ws_connections)")
stat("Peers", "sum(mediasoup_worker_peers)")
stat("Transports", "sum(mediasoup_worker_transports)")
stat("Producers", "sum(mediasoup_worker_producers)", desc="Includes pipe producers between workers")
stat("Consumers", "sum(mediasoup_worker_consumers)", desc="Includes pipe consumers between workers")
stat("Busiest worker CPU", f"max(100 * rate(mediasoup_worker_cpu_seconds_total[{R}]))", unit="percent", decimals=0,
     steps=[{"color": G, "value": None}, {"color": Y, "value": 70}, {"color": RED, "value": 90}],
     desc="A worker is one thread on one core: at ~100% it starts dropping/delaying packets")
stat("Consumer score avg", "avg(mediasoup_consumer_score_avg)", decimals=1,
     steps=[{"color": RED, "value": None}, {"color": Y, "value": 7}, {"color": G, "value": 9}],
     desc="mediasoup's 0..10 score over a random consumer sample (10 = no loss)")

row("Workers (one mediasoup-worker process per pinned core)")
ts("Worker CPU % (per worker)", [(f'100 * rate(mediasoup_worker_cpu_seconds_total{{{W}}}[{R}])', "w{{worker}} cpu{{cpu}}")],
   unit="percent", w=12, maxv=100, minv=0, desc="user+sys from /proc. Softirq for this core is not included — see the SFU dashboard")
ts("Worker CPU: max / avg / min", [(f'max(100 * rate(mediasoup_worker_cpu_seconds_total[{R}]))', "max"),
    (f'avg(100 * rate(mediasoup_worker_cpu_seconds_total[{R}]))', "avg"),
    (f'min(100 * rate(mediasoup_worker_cpu_seconds_total[{R}]))', "min")], unit="percent", w=12, maxv=100, minv=0,
   desc="A wide max-avg gap means placement is unbalanced: one worker saturates while others idle")
ts("Consumers per worker", [(f'mediasoup_worker_consumers{{{W}}}', "w{{worker}}")])
ts("Peers per worker", [(f'mediasoup_worker_peers{{{W}}}', "w{{worker}}")])
ts("Transports per worker", [(f'mediasoup_worker_transports{{{W}}}', "w{{worker}}")])
ts("Producers per worker", [(f'mediasoup_worker_producers{{{W}}}', "w{{worker}}")])
ts("Routers per worker", [(f'mediasoup_worker_routers{{{W}}}', "w{{worker}}")])
ts("Worker RSS", [(f'mediasoup_worker_rss_bytes{{{W}}}', "w{{worker}}")], unit="bytes")
ts("Consumer imbalance (max / avg)", [('max(mediasoup_worker_consumers) / avg(mediasoup_worker_consumers)', "max/avg consumers"),
    (f'max(rate(mediasoup_worker_cpu_seconds_total[{R}])) / avg(rate(mediasoup_worker_cpu_seconds_total[{R}]))', "max/avg CPU")],
   desc="1.0 = perfectly balanced")
ts("CPU per consumer (µs of worker CPU per consumer-second)", [(f'1e6 * sum(rate(mediasoup_worker_cpu_seconds_total[{R}])) / sum(mediasoup_worker_consumers)', "all workers")],
   unit="µs", desc="Efficiency: should stay flat as load grows. Rising = per-packet overhead growing (cache, pipes, NACK storms)")
ts("Pipes created (pipeToRouter)", [(f'sum(rate(mediasoup_pipes_created_total[{R}]))', "pipes/s"), ('sum(mediasoup_pipes_created_total)', "total")],
   desc="A room that outgrows one router gets piped to another worker")

row("Media quality (server side)")
ts("Consumer score", [('avg(mediasoup_consumer_score_avg)', "avg score (0..10)")], minv=0, maxv=10)
ts("Consumers with score < 7", [('100 * avg(mediasoup_consumer_score_low_ratio)', "% of sample")], unit="percent", minv=0)
ts("ICE / DTLS state changes", [(f'sum by (state) (rate(mediasoup_ice_state_changes_total[{R}]))', "ice {{state}}"),
    (f'sum by (state) (rate(mediasoup_dtls_state_changes_total[{R}]))', "dtls {{state}}")], unit="ops",
   desc="disconnected/failed/closed while load is steady = transports dying")

row("BWE vs link (media.yaml bwe_telemetry: true; profile/peer from the loadgen)")
# mediasoup_peer_* is per viewer (its recv transport); bench_peer_info / bench_peer_link_bps come from the load
# generators and give each peer its network profile and its downlink limit right now (only while a limit applies).
P = 'peer=~"$peer"'
PROF = ' * on(peer) group_left(profile) bench_peer_info{profile=~"$profile"}'
LIM = ' / on(peer) group_left(profile) bench_peer_link_bps{profile=~"$profile"}'
EST = 'mediasoup_peer_bwe_available_bps'
SENT = f'(rate(mediasoup_peer_sent_bytes_total{{kind="all"}}[{R}]) * 8)'
ts("Estimate, sent and link limit — $peer", [(f'{EST}{{{P}}}', "est {{peer}}"),
    (f'rate(mediasoup_peer_sent_bytes_total{{{P},kind="all"}}[{R}]) * 8', "sent {{peer}}"),
    (f'bench_peer_link_bps{{{P}}}', "limit {{peer}}")], unit="bps", w=16, minv=0,
   desc="est = mediasoup's send-side estimate for this viewer (bwe trace, ~100 ms updates, scraped every 5 s); "
        "sent = all bytes to the viewer incl. RTX, probes, RTCP; limit = the generator's downlink shaper (dippy: only during dips)")
ts("Forwarded spatial layer — $peer", [(f'mediasoup_peer_video_layer{{{P}}}', "{{peer}}")], w=8, minv=-1, maxv=2,
   desc="Lowest spatial layer over the viewer's video consumers; -1 = no video forwarded")
ts("Estimate ÷ limit by profile", [(f'quantile by (profile) (0.5, {EST}{LIM})', "p50 {{profile}}"),
    (f'quantile by (profile) (0.95, {EST}{LIM})', "p95 {{profile}}"), ('vector(1)', "= limit")], minv=0,
   desc="Across the profile's viewers while a limit applies. > 1: the estimate is above what the link carries")
ts("Sent ÷ limit by profile", [(f'quantile by (profile) (0.5, {SENT}{LIM})', "p50 {{profile}}"),
    (f'quantile by (profile) (0.95, {SENT}{LIM})', "p95 {{profile}}"), ('vector(1)', "= limit")], minv=0,
   desc=f"Averaged over {R}, so short bursts above the limit are hidden: see scripts/bwe_analyze.py for 1 s resolution")
ts("Viewers with estimate > limit", [(f'100 * avg by (profile) (({EST}{LIM}) > bool 1)', "{{profile}}")], unit="percent", minv=0, maxv=100)
ts("Estimate by profile (all viewers)", [(f'quantile by (profile) (0.05, {EST}{PROF})', "p5 {{profile}}"),
    (f'quantile by (profile) (0.5, {EST}{PROF})', "p50 {{profile}}")], unit="bps", minv=0,
   desc="Includes clean and loss/delay-only profiles: their estimate should stay high")
ts("RTX and probe share of bytes sent", [
    (f'100 * sum by (profile) (rate(mediasoup_peer_sent_bytes_total{{kind="rtx"}}[{R}]){PROF}) / sum by (profile) (rate(mediasoup_peer_sent_bytes_total{{kind="all"}}[{R}]){PROF})', "rtx {{profile}}"),
    (f'100 * sum by (profile) (rate(mediasoup_peer_sent_bytes_total{{kind="probation"}}[{R}]){PROF}) / sum by (profile) (rate(mediasoup_peer_sent_bytes_total{{kind="all"}}[{R}]){PROF})', "probe {{profile}}")],
   unit="percent", minv=0)
ts("Viewers on the top layer", [(f'100 * avg by (profile) (((mediasoup_peer_video_layer{PROF}) >= 0) == bool scalar(max(mediasoup_peer_video_layer)))', "{{profile}}")],
   unit="percent", minv=0, maxv=100, desc="Of viewers with video forwarded; top = the highest layer any viewer gets now")
ts("BWE trace events", [(f'sum(rate(mediasoup_bwe_trace_events_total[{R}]))', "events/s")], unit="ops",
   desc="Sanity: ~10/s per viewer with video. 0 = bwe_telemetry off")

row("Control plane (signaling + media RPC)")
ts("Media RPC latency p95 by method", [(hq(0.95, "mediasoup_rpc_duration_seconds", "le, method"), "{{method}}")], unit="s")
ts("Media RPC rate by method", [(f'sum by (method) (rate(mediasoup_rpc_duration_seconds_count[{R}]))', "{{method}}")], unit="reqps")
ts("Media RPC latency (all methods)", [(hq(0.5, "mediasoup_rpc_duration_seconds"), "p50"), (hq(0.95, "mediasoup_rpc_duration_seconds"), "p95"),
    (hq(0.99, "mediasoup_rpc_duration_seconds"), "p99")], unit="s")
ts("Signaling request latency p95 by method", [(hq(0.95, "mssig_request_duration_seconds", "le, method"), "{{method}}")], unit="s",
   desc="Client request → reply, including the media RPC")
ts("Signaling → media RPC p95 by method", [(hq(0.95, "mssig_media_rpc_duration_seconds", "le, method"), "{{method}}")], unit="s",
   desc="Compare with media-side RPC latency: the gap is HTTP/queueing between the two processes")
ts("Signaling errors", [(f'sum by (method) (rate(mssig_request_duration_seconds_count{{ok="false"}}[{R}]))', "request {{method}}"),
    (f'sum by (method) (rate(mssig_media_rpc_duration_seconds_count{{ok="false"}}[{R}]))', "media rpc {{method}}"),
    (f'sum(rate(mssig_dropped_messages_total[{R}]))', "dropped msgs (send queue full)")], unit="ops", desc="Must stay 0")
ts("Server-driven consumes", [(f'sum by (ok) (rate(mssig_server_consumes_total[{R}]))', "ok={{ok}}")], unit="ops")
ts("Signaling rooms / connections", [('sum(mssig_rooms)', "rooms"), ('sum(mssig_ws_connections)', "ws connections")])

row("Media service Node process (control thread)")
ts("Node CPU (cores)", [(f'rate(mediasoup_node_process_cpu_user_seconds_total[{R}])', "user"),
    (f'rate(mediasoup_node_process_cpu_system_seconds_total[{R}])', "system")], stack=True,
   desc="Single-threaded: ~1 core = control plane saturated (joins/RPC queue up)")
ts("Event loop lag", [('mediasoup_node_nodejs_eventloop_lag_p50_seconds', "p50"), ('mediasoup_node_nodejs_eventloop_lag_p99_seconds', "p99"),
    ('mediasoup_node_nodejs_eventloop_lag_max_seconds', "max")], unit="s")
ts("Node memory", [('mediasoup_node_process_resident_memory_bytes', "rss"), ('mediasoup_node_nodejs_heap_size_used_bytes', "heap used"),
    ('mediasoup_node_nodejs_heap_size_total_bytes', "heap total"), ('mediasoup_node_nodejs_external_memory_bytes', "external")], unit="bytes")
ts("GC time per second by kind", [(f'sum by (kind) (rate(mediasoup_node_nodejs_gc_duration_seconds_sum[{R}]))', "{{kind}}")], unit="s")
ts("Open file descriptors", [('mediasoup_node_process_open_fds', "open"), ('mediasoup_node_process_max_fds', "max")])
ts("Active libuv handles", [('mediasoup_node_nodejs_active_handles_total', "handles"), ('mediasoup_node_nodejs_active_requests_total', "requests")])
endrow()

dash = {
    "title": "mediasoup", "uid": "mediasoup", "editable": True, "schemaVersion": 39,
    "time": {"from": "now-1h", "to": "now"}, "refresh": "10s", "tags": ["sfu-bench", "mediasoup"],
    "templating": {"list": [
        {"name": "ds", "type": "datasource", "query": "prometheus", "label": "Prometheus (bench)"},
        {"name": "worker", "type": "query", "label": "Worker", "datasource": DS, "refresh": 2, "multi": True, "includeAll": True,
         "current": {"text": "All", "value": "$__all"}, "sort": 3,
         "query": {"query": "label_values(mediasoup_worker_consumers, worker)", "refId": "worker"},
         "definition": "label_values(mediasoup_worker_consumers, worker)"},
        {"name": "profile", "type": "query", "label": "Network profile", "datasource": DS, "refresh": 2, "multi": True,
         "includeAll": True, "current": {"text": "All", "value": "$__all"}, "sort": 1,
         "query": {"query": "label_values(bench_peer_info, profile)", "refId": "profile"},
         "definition": "label_values(bench_peer_info, profile)"},
        {"name": "peer", "type": "query", "label": "Viewer", "datasource": DS, "refresh": 2, "multi": True, "includeAll": False,
         "sort": 1, "description": "For the per-viewer panels of the BWE row",
         "query": {"query": 'label_values(bench_peer_info{profile=~"$profile"}, peer)', "refId": "peer"},
         "definition": 'label_values(bench_peer_info{profile=~"$profile"}, peer)'},
    ]},
    "panels": panels,
}
out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "mediasoup.json")
json.dump(dash, open(out, "w"), indent=1)
print(f"wrote {out} ({len(panels)} panels)")
