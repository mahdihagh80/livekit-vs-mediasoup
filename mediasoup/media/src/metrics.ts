import fs from 'node:fs';
import os from 'node:os';
import client from 'prom-client';
import type { WorkerEntry } from './workers';
import type { Room } from './room';

/**
 * Prometheus exporter for the mediasoup side. Names are prefixed `mediasoup_`.
 * The LiveKit side uses LiveKit's built-in `livekit_*` metrics. Both sides also
 * get node-exporter and the load generator's `bench_*` metrics, which are the
 * ones used to compare.
 */
export const registry = new client.Registry();
client.collectDefaultMetrics({ register: registry, prefix: 'mediasoup_node_' });

export const metrics = {
  rpcSeconds: new client.Histogram({
    name: 'mediasoup_rpc_duration_seconds',
    help: 'Media service RPC latency by method',
    labelNames: ['method', 'ok'],
    buckets: [0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5],
    registers: [registry],
  }),
  iceState: new client.Counter({ name: 'mediasoup_ice_state_changes_total', help: 'ICE state changes', labelNames: ['state'], registers: [registry] }),
  dtlsState: new client.Counter({ name: 'mediasoup_dtls_state_changes_total', help: 'DTLS state changes', labelNames: ['state'], registers: [registry] }),
  pipes: new client.Counter({ name: 'mediasoup_pipes_created_total', help: 'pipeToRouter calls', registers: [registry] }),
  capacityRejections: new client.Counter({
    name: 'mediasoup_capacity_rejections_total',
    help: 'Requests rejected because a worker had no load capacity left',
    labelNames: ['what'],
    registers: [registry],
  }),
};

const CLK_TCK = 100; // getconf CLK_TCK is 100 on every Linux we care about
const PAGE = 4096;

function procStat(pid: number): { cpuSeconds: number; rssBytes: number } | null {
  try {
    const s = fs.readFileSync(`/proc/${pid}/stat`, 'utf8');
    // Fields after the ")" of comm: state is field 3. utime=14, stime=15, rss=24.
    const f = s.slice(s.lastIndexOf(')') + 2).split(' ');
    const utime = Number(f[11]);
    const stime = Number(f[12]);
    const rss = Number(f[21]);
    return { cpuSeconds: (utime + stime) / CLK_TCK, rssBytes: rss * PAGE };
  } catch {
    return null;
  }
}

/** Latest consumer-score sample, refreshed in the background (getStats is not free). */
let scoreSample = { count: 0, sum: 0, low: 0 };

export function registerWorkerMetrics(workers: WorkerEntry[], rooms: Map<string, Promise<Room>>) {
  const lbl = ['worker', 'cpu'];
  const g = (name: string, help: string, fn: (w: WorkerEntry) => number) =>
    new client.Gauge({
      name,
      help,
      labelNames: lbl,
      registers: [registry],
      collect() {
        this.reset();
        for (const w of workers) this.set({ worker: String(w.index), cpu: String(w.cpu ?? '') }, fn(w));
      },
    });

  g('mediasoup_worker_routers', 'Routers per worker', (w) => w.routers);
  g('mediasoup_worker_transports', 'WebRtcTransports per worker', (w) => w.transports);
  g('mediasoup_worker_producers', 'Producers per worker (incl. pipe producers)', (w) => w.producers);
  g('mediasoup_worker_consumers', 'Consumers per worker (incl. pipe consumers)', (w) => w.consumers);
  g('mediasoup_worker_peers', 'Peers placed on worker', (w) => w.peers);
  g('mediasoup_worker_load_units', 'Load units of live streams on worker (load_weights)', (w) => w.used);
  g('mediasoup_worker_reserved_units', 'Load units held for joined peers without streams yet', (w) => w.reserved);
  g('mediasoup_worker_capacity_units', 'worker_capacity', (w) => w.capacity);

  // Counter semantics (monotonic), exposed via collect(). Use rate() on it.
  new client.Counter({
    name: 'mediasoup_worker_cpu_seconds_total',
    help: 'CPU seconds (user+sys) consumed by each mediasoup-worker process (from /proc)',
    labelNames: lbl,
    registers: [registry],
    collect() {
      this.reset();
      for (const w of workers) {
        const st = procStat(w.worker.pid);
        if (st) this.inc({ worker: String(w.index), cpu: String(w.cpu ?? '') }, st.cpuSeconds);
      }
    },
  });
  g('mediasoup_worker_rss_bytes', 'RSS of each mediasoup-worker process', (w) => procStat(w.worker.pid)?.rssBytes ?? 0);

  new client.Gauge({
    name: 'mediasoup_rooms',
    help: 'Open rooms',
    registers: [registry],
    collect() {
      this.set(rooms.size);
    },
  });
  new client.Gauge({
    name: 'mediasoup_consumer_score_avg',
    help: 'Average consumer score (0..10) over a random sample of consumers (refreshed every 15s)',
    registers: [registry],
    collect() {
      this.set(scoreSample.count ? scoreSample.sum / scoreSample.count : 0);
    },
  });
  new client.Gauge({
    name: 'mediasoup_consumer_score_low_ratio',
    help: 'Share of sampled consumers with score < 7',
    registers: [registry],
    collect() {
      this.set(scoreSample.count ? scoreSample.low / scoreSample.count : 0);
    },
  });
  new client.Gauge({
    name: 'mediasoup_host_cpus',
    help: 'Number of CPUs on the host',
    registers: [registry],
    collect() {
      this.set(os.cpus().length);
    },
  });

  // Consumer.score is pushed by the worker on change, so reading it is free;
  // we just bound how many we look at.
  setInterval(async () => {
    let count = 0, sum = 0, low = 0;
    for (const rp of rooms.values()) {
      const room = await rp.catch(() => null);
      if (!room) continue;
      for (const c of room.allConsumers()) {
        if (c.paused) continue;
        const s = c.score.score;
        count++;
        sum += s;
        if (s < 7) low++;
        if (count >= 5000) break;
      }
      if (count >= 5000) break;
    }
    scoreSample = { count, sum, low };
  }, 15_000).unref();
}
