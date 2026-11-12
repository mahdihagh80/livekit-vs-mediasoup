#!/usr/bin/env python3
"""Generates livekit.json (Grafana dashboard, LiveKit only). Edit here, re-run, re-import.

Sources: LiveKit /metrics (livekit_*, go_*, process_*; prometheus_port 450, scrape job "livekit")
and the SFU node_exporter (job "node") for host CPU and NIC.
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
L = 'job="livekit"'
N = 'job="node"'
G, Y, RED = "green", "yellow", "red"
hq = lambda qq, metric, by="le", sel=L: f'histogram_quantile({qq}, sum by ({by}) (rate({metric}_bucket{{{sel}}}[{R}])))'
pcts = lambda metric, sel=L, scale="": [(f"{hq(q, metric, sel=sel)}{scale}", f"p{int(q * 100)}") for q in (0.5, 0.95, 0.99)]
by_dir_kind = lambda metric, q=0.95: [(hq(q, metric, "le, direction, type"), "{{direction}} {{type}}")]
LK_CPUS = 'cpu=~"1[4-9]|[2-5][0-9]|6[0-3]"'
IRQ_CPUS = 'cpu=~"[0-7]"'

row("Overview")
stat("Rooms", f"sum(livekit_room_total{{{L}}})")
stat("Participants", f"sum(livekit_participant_total{{{L}}})")
stat("Tracks published", f"sum(livekit_track_published_total{{{L}}})")
stat("Tracks subscribed", f"sum(livekit_track_subscribed_total{{{L}}})")
stat("Egress", f"8 * sum(rate(livekit_packet_bytes{{{L},direction=\"outgoing\"}}[{R}]))", unit="bps")
stat("LiveKit CPU (cores)", f"rate(process_cpu_seconds_total{{{L}}}[{R}])", decimals=1,
     steps=[{"color": G, "value": None}, {"color": Y, "value": 35}, {"color": RED, "value": 45}],
     desc="LiveKit runs on 50 cores (cpuset 14-63, GOMAXPROCS 50)")
stat("Join p95", hq(0.95, "livekit_session_join_latency_ms"), unit="ms", decimals=0,
     steps=[{"color": G, "value": None}, {"color": Y, "value": 1000}, {"color": RED, "value": 3000}])
stat("Quality score avg", f"sum(rate(livekit_quality_score_sum{{{L}}}[{R}])) / sum(rate(livekit_quality_score_count{{{L}}}[{R}]))",
     decimals=2, steps=[{"color": RED, "value": None}, {"color": Y, "value": 3.5}, {"color": G, "value": 4.2}],
     desc="LiveKit's MOS-like score (1..5) per participant track")

row("Sessions and joins")
ts("Rooms / participants", [(f"sum(livekit_room_total{{{L}}})", "rooms"), (f"sum(livekit_participant_total{{{L}}})", "participants")])
ts("Tracks", [(f"sum by (kind) (livekit_track_published_total{{{L}}})", "published {{kind}}"),
    (f"sum by (kind) (livekit_track_subscribed_total{{{L}}})", "subscribed {{kind}}")])
ts("Join funnel (per second, by state)", [(f"sum by (state) (rate(livekit_participant_join_total{{{L}}}[{R}]))", "{{state}}")], unit="ops",
   desc="signal_connected → rtc_init → rtc_connected → rtc_success → rtc_active. A gap between states = joins stuck at that stage")
ts("Session join latency", pcts("livekit_session_join_latency_ms"), unit="ms", desc="Signal connect → join response")
ts("Session start time", pcts("livekit_session_start_time_ms"), unit="ms", desc="Join → media connected (ICE + DTLS)")
ts("PeerConnection states", [(f"sum by (transport, state) (livekit_peer_connection_state{{{L}}})", "{{transport}} {{state}}")])
ts("Track publish / subscribe attempts and failures", [
    (f"sum by (state) (rate(livekit_track_publish_counter{{{L}}}[{R}]))", "publish {{state}}"),
    (f"sum by (state, error) (rate(livekit_track_subscribe_counter{{{L}}}[{R}]))", "subscribe {{state}} {{error}}")], unit="ops")
ts("Publish / subscribe time (pubsub)", [(hq(0.95, "livekit_pubsubtime_ms", "le, direction, type"), "p95 {{direction}} {{type}}")], unit="ms",
   desc="Track published/subscribed → first media")
ts("Signal / service operations", [(f"sum by (type, status) (rate(livekit_node_messages{{{L}}}[{R}]))", "msg {{type}} {{status}}"),
    (f'sum by (type, status, error_type) (rate(livekit_node_service_operation{{{L}}}[{R}]))', "op {{type}} {{status}} {{error_type}}")], unit="ops")

row("Media quality")
ts("Packet loss % p95 (by direction, kind)", by_dir_kind("livekit_packet_loss_percent"), unit="percent", minv=0,
   desc="incoming = publisher → SFU, outgoing = SFU → subscriber (from receiver reports)")
ts("Packets lost per second", [(f"sum by (direction, type) (rate(livekit_packet_loss_total{{{L}}}[{R}]))", "{{direction}} {{type}}")], unit="pps")
ts("Jitter p95 (by direction, kind)", [(f'{hq(0.95, "livekit_jitter_us", "le, direction, type")} / 1000', "{{direction}} {{type}}")], unit="ms")
ts("RTT", [(hq(q, "livekit_rtt_ms"), f"p{int(q * 100)}") for q in (0.5, 0.95, 0.99)], unit="ms")
ts("Out-of-order % p95", by_dir_kind("livekit_packet_out_of_order_percent"), unit="percent", minv=0)
ts("NACK / PLI / keyframes per second", [(f"sum by (direction) (rate(livekit_nack_total{{{L}}}[{R}]))", "nack {{direction}}"),
    (f"sum by (direction) (rate(livekit_pli_total{{{L}}}[{R}]))", "pli {{direction}}"),
    (f"sum by (direction) (rate(livekit_keyframe_total{{{L}}}[{R}]))", "keyframe {{direction}}")], unit="ops",
   desc="PLI storms with many subscribers force the publisher to send keyframes often")
ts("Quality score", [(hq(q, "livekit_quality_score"), f"p{int(q * 100)}") for q in (0.05, 0.5)] +
   [(f"sum(rate(livekit_quality_score_sum{{{L}}}[{R}])) / sum(rate(livekit_quality_score_count{{{L}}}[{R}]))", "avg")], minv=1, maxv=5)
ts("Quality rating distribution", [(f"sum by (le) (rate(livekit_quality_rating_bucket{{{L}}}[{R}]))", "≤ {{le}}")], unit="ops",
   desc="Cumulative: rating ≤ le per second (POOR < GOOD < EXCELLENT)")
ts("Forward latency", [(f"max(livekit_forward_latency{{{L}}}) / 1e6", "gauge"),
    (f'{hq(0.95, "livekit_forward_latency_ns")} / 1e6', "p95")], unit="ms", desc="SFU receive → forward. Stays 0 when sampling is off")

row("Traffic")
ts("Bitrate by direction", [(f"8 * sum by (direction, transmission) (rate(livekit_packet_bytes{{{L}}}[{R}]))", "{{direction}} {{transmission}}")],
   unit="bps", w=12)
ts("Packet rate by direction", [(f"sum by (direction, transmission) (rate(livekit_packet_total{{{L}}}[{R}]))", "{{direction}} {{transmission}}")],
   unit="pps", w=12)
ts("Retransmit share of egress", [(f'100 * sum(rate(livekit_packet_bytes{{{L},direction="outgoing",transmission="retransmit"}}[{R}])) / '
    f'sum(rate(livekit_packet_bytes{{{L},direction="outgoing"}}[{R}]))', "% bytes retransmitted")], unit="percent", minv=0)
ts("Node packets (out / dropped)", [(f"sum by (type) (rate(livekit_node_packet_total{{{L}}}[{R}]))", "{{type}}")], unit="pps",
   desc="dropped > 0 = LiveKit's forwarder dropped packets internally (queue full)")
ts("Egress per subscribed track", [(f'8 * sum(rate(livekit_packet_bytes{{{L},direction="outgoing"}}[{R}])) / sum(livekit_track_subscribed_total{{{L}}})',
    "bps per subscribed track")], unit="bps")
ts("psrpc signal relay", [(f"sum by (method, direction) (rate(livekit_psrpc_bytes_total{{{L}}}[{R}])) * 8", "{{method}} {{direction}}"),
    (f'{hq(0.95, "livekit_psrpc_stream_send_time_ms")}', "p95 send time (ms)")],
   desc="Signal path between the RTC and signal services inside the process")

row("Host (SFU node_exporter) and Go runtime")
ts("CPU: LiveKit cores 14-63", [(f'100 * avg(1 - rate(node_cpu_seconds_total{{{N},mode="idle",{LK_CPUS}}}[{R}]))', "avg"),
    (f'100 * max(1 - rate(node_cpu_seconds_total{{{N},mode="idle",{LK_CPUS}}}[{R}]))', "max"),
    (f'100 * min(1 - rate(node_cpu_seconds_total{{{N},mode="idle",{LK_CPUS}}}[{R}]))', "min")], unit="percent", minv=0, maxv=100)
ts("CPU: IRQ cores 0-7", [(f'100 * avg(1 - rate(node_cpu_seconds_total{{{N},mode="idle",{IRQ_CPUS}}}[{R}]))', "avg busy"),
    (f'100 * max(1 - rate(node_cpu_seconds_total{{{N},mode="idle",{IRQ_CPUS}}}[{R}]))', "max busy"),
    (f'100 * max(rate(node_cpu_seconds_total{{{N},mode="softirq",{IRQ_CPUS}}}[{R}]))', "max softirq")], unit="percent", minv=0, maxv=100)
ts("CPU by mode, LiveKit cores", [(f'100 * sum by (mode) (rate(node_cpu_seconds_total{{{N},mode!="idle",{LK_CPUS}}}[{R}])) / 50', "{{mode}}")],
   unit="percent", stack=True, desc="Share of the 50 LiveKit cores. High softirq here = packets steered onto LiveKit cores")
ts("bond1 bitrate", [(f'8 * rate(node_network_receive_bytes_total{{{N},device="bond1"}}[{R}])', "rx"),
    (f'8 * rate(node_network_transmit_bytes_total{{{N},device="bond1"}}[{R}])', "tx")], unit="bps")
ts("NIC drops / errors", [(f'sum by (device) (rate(node_network_receive_drop_total{{{N},device=~"bond1|ens2f.*"}}[{R}]))', "rx drop {{device}}"),
    (f'sum by (device) (rate(node_network_transmit_drop_total{{{N},device=~"bond1|ens2f.*"}}[{R}]))', "tx drop {{device}}"),
    (f'sum by (device) (rate(node_network_receive_errs_total{{{N},device=~"bond1|ens2f.*"}}[{R}]))', "rx err {{device}}")], unit="pps")
ts("UDP socket errors", [(f'rate(node_netstat_Udp_RcvbufErrors{{{N}}}[{R}])', "rcvbuf errors"),
    (f'rate(node_netstat_Udp_SndbufErrors{{{N}}}[{R}])', "sndbuf errors"),
    (f'rate(node_netstat_Udp_InErrors{{{N}}}[{R}])', "in errors")], unit="pps",
   desc="With the 50-port UDP mux every PeerConnection shares 50 sockets: rcvbuf errors = socket buffers overflowing")
ts("LiveKit process CPU (cores)", [(f"rate(process_cpu_seconds_total{{{L}}}[{R}])", "cores")], maxv=50, minv=0)
ts("LiveKit memory", [(f"process_resident_memory_bytes{{{L}}}", "rss"), (f"go_memstats_heap_inuse_bytes{{{L}}}", "heap in use"),
    (f"go_memstats_next_gc_bytes{{{L}}}", "next GC target"), (f"go_gc_gomemlimit_bytes{{{L}}}", "GOMEMLIMIT")], unit="bytes")
ts("Goroutines / threads", [(f"go_goroutines{{{L}}}", "goroutines"), (f"go_threads{{{L}}}", "threads")])
ts("GC pause", [(f"rate(go_gc_duration_seconds_sum{{{L}}}[{R}])", "GC time per second"),
    (f'go_gc_duration_seconds{{{L},quantile="1"}}', "max pause")], unit="s")
ts("Allocation rate", [(f"rate(go_memstats_alloc_bytes_total{{{L}}}[{R}])", "alloc/s")], unit="Bps")
ts("Open file descriptors", [(f"process_open_fds{{{L}}}", "open"), (f"process_max_fds{{{L}}}", "max")])
endrow()

dash = {
    "title": "LiveKit", "uid": "livekit", "editable": True, "schemaVersion": 39,
    "time": {"from": "now-1h", "to": "now"}, "refresh": "10s", "tags": ["sfu-bench", "livekit"],
    "links": [{"title": "Go runtime (full)", "type": "link", "url": "/d/go-overview?var-job=livekit", "keepTime": True}],
    "templating": {"list": [
        {"name": "ds", "type": "datasource", "query": "prometheus", "label": "Prometheus (bench)"},
    ]},
    "panels": panels,
}
out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "livekit.json")
json.dump(dash, open(out, "w"), indent=1)
print(f"wrote {out} ({len(panels)} panels)")
