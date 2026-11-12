# 04. LiveKit on the xFusion

## 1. The open-source release

The benchmark uses the open-source LiveKit server, version `livekit.version` in
`bench.yaml` (1.13.11, the latest release on 2026-10-25).

* **Native:** `livekit/get-binary.sh` downloads that release from GitHub and
  installs it at `livekit.bin`.
* **Docker:** `docker/livekit/Dockerfile` takes the binary from the official
  image `livekit/livekit-server:v1.13.11`. Keep its `LIVEKIT_IMAGE` tag on the
  same version as `livekit.version` (see [08-docker.md](08-docker.md)).

```bash
sudo mkdir -p /opt/sfu-bench && sudo chown $USER /opt/sfu-bench
livekit/get-binary.sh          # -> installed livekit-server version 1.13.11 -> /opt/sfu-bench/livekit-server
```

Both run the process natively on the host, pinned with `taskset` (Docker uses
host networking and no CPU quotas). Never mix native and Docker within one
comparison.

## 2. The config

Two places hold LiveKit settings:

| File | What goes there |
|---|---|
| `livekit/livekit.base.yaml` (path: `livekit.base_config`) | A normal LiveKit config, close to defaults. If you want results that match how you'd run LiveKit in production, copy those settings in here and keep the changes marked `[bench]` (logging level, STUN) |
| `bench.yaml` → `livekit:` | What the benchmark controls: binary path, API key/secret, layouts, port bases |

`livekit/start.sh` writes `livekit/run/livekit-<i>.yaml` per instance: the base
file with these keys overwritten from `bench.yaml` (with yq): `port`,
`rtc.tcp_port`, `rtc.udp_port`, `rtc.node_ip`, `rtc.use_external_ip`, `keys`,
`prometheus.port`. Everything else comes from the base file unchanged.

Settings that change the result most, worth deciding deliberately:

* `rtc.udp_port` (single-port mux, used here) vs `rtc.port_range_start/end`
* `rtc.congestion_control`, `rtc.packet_buffer_size*`, `rtc.batch_io`
* `room` / `audio` / `video` sections (dynacast, playout delay, ...)

Save the rendered config with the results (`cp livekit/run/livekit-0.yaml results/<run>/`).

## 3. Start / stop

```bash
livekit/start.sh 1x48     # layout = <instances>x<cores>, carved out of cpu.sfu
livekit/stop.sh
```

Then on gen-1: `scripts/run-scenario.sh livekit:1x48 client/plans/<plan>.yaml <run-name>`.
Nothing needs copying: the controller builds the URLs from `bench.yaml`.
Instance `i` uses HTTP `http_port_base + i*port_step`, UDP
`udp_port_base + i*port_step`, metrics `monitoring_port_base + i`, and CPU slice `i`
of `cpu.sfu`. Go sets GOMAXPROCS to the number of CPUs in the `taskset` mask,
so each instance gets GOMAXPROCS = its core count automatically.

Instances are standalone (no Redis). The load generator puts room `r` on
instance `r % n`. That gives the same total capacity as a Redis cluster on one
host, without Redis traffic in the measurement. A room can never span
instances, so the webinar plan must use `1xN`.

## Layout sweep

On the test host the layout mattered (3 instances with GOMAXPROCS 40/32/32 behaved
differently from 13-19 × GOMAXPROCS 8). Before the main matrix:

1. Run `02-group-6` (or `01-one-to-one`) for each layout in `livekit.layouts`
   (e.g. `1x48`, `3x16`, `6x8`).
2. Keep the layout with the highest capacity at SLO. Use the next-best as a
   tie-breaker on CPU efficiency.
3. Use that layout for all LiveKit runs in the matrix (webinar: `1x48`).

That's the "LiveKit in its best configuration" the comparison is against.

## What to watch during LiveKit runs

* Go scheduler/GC: the "LiveKit GC pause" and goroutines panels. On the test host,
  degradation started around 18k participants with GC pause p99 at 10 ms and
  ~1.6M goroutines.
* Per-core busy %: LiveKit spreads over all its cores. If the IRQ cores
  saturate first, that's a NIC/IRQ plan problem, not LiveKit.
* `livekit/run/livekit-*.log` for warnings during the run.
