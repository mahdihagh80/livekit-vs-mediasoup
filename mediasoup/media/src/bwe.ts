import fs from 'node:fs';
import client from 'prom-client';
import type { types } from 'mediasoup';
import { config } from './config';
import { registry } from './metrics';

/**
 * Per-peer BWE telemetry, on when media.yaml has bwe_telemetry: true (or bwe_log).
 *
 * For every recv transport (SFU -> peer, where the send-side BWE runs):
 *   - the 'bwe' trace event: desired and available bitrate, pushed by the
 *     worker each time the target changes (at most every 100 ms);
 *   - transport.getStats() every second: bytes sent as media, RTX and probes
 *     (three disjoint counters) and in total on the wire (incl. RTCP);
 *   - the spatial layer each video consumer forwards (Consumer.currentLayers).
 *
 * Exposed as mediasoup_peer_* series labelled by peer id, to put next to the
 * load generator's bench_peer_link_bps{peer}. With bwe_log set, every trace
 * event and every 1 s sample is also appended as one JSON line with a
 * wall-clock ms timestamp: Prometheus scrapes are too coarse to time how fast
 * the estimate reacts to a dip.
 *
 * Only transports with at least one video consumer are exported and logged:
 * a recv transport with nothing to receive (a room's only publisher) would
 * otherwise sit at the initial estimate and skew the per-profile numbers.
 *
 * The 'probation' trace is not used: it is one event per probe packet with
 * the whole packet attached. probationBytesSent gives the probe rate cheaply.
 */

type Bytes = { all: number; rtp: number; rtx: number; probation: number };

type Entry = {
  peer: string;
  transport: types.WebRtcTransport;
  video: Set<types.Consumer>;
  /** Latest 'bwe' trace, bps. */
  desired?: number;
  available?: number;
  /** Latest getStats() counters, and when they were read (ms). */
  bytes?: Bytes;
  at?: number;
};

const KINDS = ['rtp', 'rtx', 'probation', 'all'] as const;
const POLL_MS = 1000;

const entries = new Map<string, Entry>(); // by transport id
let log: fs.WriteStream | undefined;
let logBroken = false;

function writeLine(obj: Record<string, unknown>) {
  if (!log || logBroken) return;
  log.write(JSON.stringify(obj) + '\n');
}

/** Lowest spatial layer the peer's video consumers forward; -1 = none forwarding. */
/** The transport carries video to the peer (it has a video consumer). */
const viewing = (e: Entry) => e.video.size > 0;

function videoLayer(e: Entry): number {
  let low = Infinity;
  for (const c of e.video) {
    const l = c.currentLayers?.spatialLayer;
    if (l !== undefined && l < low) low = l;
  }
  return low === Infinity ? -1 : low;
}

function gauge(name: string, help: string, value: (e: Entry) => number | undefined) {
  return new client.Gauge({
    name,
    help,
    labelNames: ['peer'],
    registers: [registry],
    collect() {
      this.reset();
      for (const e of entries.values()) {
        if (!viewing(e)) continue;
        const v = value(e);
        if (v !== undefined) this.set({ peer: e.peer }, v);
      }
    },
  });
}

const traceEvents = new client.Counter({
  name: 'mediasoup_bwe_trace_events_total',
  help: "'bwe' trace events received from the workers (bwe_telemetry)",
  registers: [registry],
});

let started = false;

function start() {
  if (started) return;
  started = true;

  gauge('mediasoup_peer_bwe_available_bps', 'Send-side BWE estimate for the SFU -> peer transport (latest bwe trace)', (e) => e.available);
  gauge('mediasoup_peer_bwe_desired_bps', 'Bitrate the peer\'s consumers want at their preferred layers (latest bwe trace)', (e) => e.desired);
  gauge('mediasoup_peer_video_layer', 'Lowest spatial layer forwarded to the peer (-1 = no video forwarded)', (e) => videoLayer(e));
  new client.Counter({
    name: 'mediasoup_peer_sent_bytes_total',
    help: 'Bytes sent on the SFU -> peer transport: rtp = media, rtx = retransmissions, probation = probes (disjoint), all = everything on the wire incl. RTCP',
    labelNames: ['peer', 'kind'],
    registers: [registry],
    collect() {
      this.reset();
      for (const e of entries.values()) {
        if (!e.bytes || !viewing(e)) continue;
        for (const k of KINDS) this.inc({ peer: e.peer, kind: k }, e.bytes[k]);
      }
    },
  });

  if (config.bweLog) {
    log = fs.createWriteStream(config.bweLog, { flags: 'a' });
    log.on('error', (err) => {
      if (!logBroken) console.error(`bwe_log ${config.bweLog}: ${err.message}; JSONL logging stopped`);
      logBroken = true;
    });
  }

  let polling = false;
  setInterval(async () => {
    if (polling) return; // a slow poll must not pile up
    polling = true;
    try {
      await Promise.allSettled([...entries.values()].map(poll));
    } finally {
      polling = false;
    }
  }, POLL_MS).unref();
}

async function poll(e: Entry) {
  if (e.transport.closed) return;
  const [s] = await e.transport.getStats();
  const now = Date.now();
  const bytes: Bytes = { all: s.bytesSent, rtp: s.rtpBytesSent, rtx: s.rtxBytesSent, probation: s.probationBytesSent };
  if (e.available === undefined && s.availableOutgoingBitrate !== undefined) e.available = s.availableOutgoingBitrate;
  if (log && viewing(e) && e.bytes && e.at !== undefined && now > e.at) {
    const bps = (k: keyof Bytes) => Math.round(((bytes[k] - e.bytes![k]) * 8 * 1000) / (now - e.at!));
    writeLine({
      t: now,
      ev: 'stats',
      peer: e.peer,
      avail: s.availableOutgoingBitrate,
      send: bps('all'),
      rtp: bps('rtp'),
      rtx: bps('rtx'),
      probe: bps('probation'),
      layer: videoLayer(e),
    });
  }
  e.bytes = bytes;
  e.at = now;
}

/** Start collecting for a peer's recv transport. No-op unless bwe_telemetry is on. */
export function trackTransport(peer: string, transport: types.WebRtcTransport) {
  if (!config.bweTelemetry) return;
  start();
  const e: Entry = { peer, transport, video: new Set() };
  entries.set(transport.id, e);
  transport.observer.on('close', () => entries.delete(transport.id));
  transport.on('trace', (t) => {
    if (t.type !== 'bwe' || t.direction !== 'out') return;
    const info = t.info as { desiredBitrate: number; minBitrate: number; maxBitrate: number; availableBitrate: number };
    e.desired = info.desiredBitrate;
    e.available = info.availableBitrate;
    traceEvents.inc();
    if (viewing(e)) writeLine({
      t: Date.now(),
      ev: 'bwe',
      peer,
      desired: info.desiredBitrate,
      avail: info.availableBitrate,
      min: info.minBitrate,
      max: info.maxBitrate,
    });
  });
  transport.enableTraceEvent(['bwe']).catch((err) => console.warn(`bwe trace on ${transport.id}: ${(err as Error).message}`));
}

/** Follow a video consumer's forwarded layer. No-op unless the transport is tracked. */
export function trackConsumer(transportId: string, consumer: types.Consumer) {
  const e = entries.get(transportId);
  if (!e || consumer.kind !== 'video') return;
  e.video.add(consumer);
  consumer.observer.on('close', () => e.video.delete(consumer));
}
