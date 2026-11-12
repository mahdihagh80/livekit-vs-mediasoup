import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { parse as parseYaml } from 'yaml';
import type { types } from 'mediasoup';

/**
 * All settings come from media.yaml. No environment variables.
 *
 *   node dist/index.js --config mediasoup/media.yaml [--pid-file run/media.pid]
 *
 * Default --config: mediasoup/media.yaml (two levels above dist/ or src/).
 */

/** Parse "8-63" or "2,4,6-9" into [8,9,...]. */
export function parseCpuList(s: string): number[] {
  const out: number[] = [];
  for (const part of s.split(',').map((x) => x.trim()).filter(Boolean)) {
    const [a, b] = part.split('-').map(Number);
    if (Number.isNaN(a)) throw new Error(`bad CPU list "${s}"`);
    for (let c = a; c <= (b ?? a); c++) out.push(c);
  }
  return out;
}

function arg(name: string): string | undefined {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 ? process.argv[i + 1] : undefined;
}

function splitHostPort(s: string, what: string): { host: string; port: number } {
  const i = s.lastIndexOf(':');
  const port = Number(s.slice(i + 1));
  if (i < 0 || !port) throw new Error(`${what}: expected host:port, got "${s}"`);
  return { host: s.slice(0, i) || '0.0.0.0', port };
}

/** Opus + VP8, the same set as the LiveKit run. Used when media_codecs is omitted. */
const defaultMediaCodecs: types.RouterRtpCodecCapability[] = [
  { kind: 'audio', mimeType: 'audio/opus', clockRate: 48000, channels: 2 },
  { kind: 'video', mimeType: 'video/VP8', clockRate: 90000, parameters: { 'x-google-start-bitrate': 1000 } },
];

function mediaCodecs(rawCodecs: unknown): types.RouterRtpCodecCapability[] {
  if (rawCodecs == null) return defaultMediaCodecs;
  if (!Array.isArray(rawCodecs) || rawCodecs.length === 0) {
    throw new Error('media_codecs must be a non-empty list');
  }
  return rawCodecs.map((c, i) => {
    const at = `media_codecs[${i}]`;
    if (!c || typeof c !== 'object') throw new Error(`${at}: expected a codec mapping`);
    const codec = c as Record<string, unknown>;
    if (codec.kind !== 'audio' && codec.kind !== 'video') throw new Error(`${at}: kind must be audio or video`);
    if (typeof codec.mimeType !== 'string' || !codec.mimeType) throw new Error(`${at}: mimeType is required`);
    const clockRate = Number(codec.clockRate);
    if (!clockRate) throw new Error(`${at}: clockRate is required`);
    const out: types.RouterRtpCodecCapability = { kind: codec.kind, mimeType: codec.mimeType, clockRate };
    if (codec.channels != null) out.channels = Number(codec.channels);
    if (codec.parameters != null && typeof codec.parameters === 'object') out.parameters = codec.parameters as typeof out.parameters;
    if (codec.preferredPayloadType != null) out.preferredPayloadType = Number(codec.preferredPayloadType);
    if (codec.rtcpFeedback != null) out.rtcpFeedback = codec.rtcpFeedback as types.RtcpFeedback[];
    return out;
  });
}

const SCHEDULERS = ['webinar', 'one_to_one', 'capacity'] as const;
export type Scheduler = (typeof SCHEDULERS)[number];

function parseScheduler(s: string): Scheduler {
  if (!(SCHEDULERS as readonly string[]).includes(s)) throw new Error(`scheduler must be one of ${SCHEDULERS.join(', ')}, got ${s}`);
  return s as Scheduler;
}

export const configPath = path.resolve(arg('config') ?? path.join(__dirname, '..', '..', 'media.yaml'));
export const pidFile = arg('pid-file');

if (!fs.existsSync(configPath)) {
  throw new Error(`config not found: ${configPath} (pass --config mediasoup/media.yaml)`);
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
const raw: any = parseYaml(fs.readFileSync(configPath, 'utf8')) ?? {};

// mediasoup 3.32 replaced libwebrtc's GoogCC with its own BWE and removed
// WorkerSettings.libwebrtcFieldTrials. createWorker() would drop the option
// without a word, so a stale tuned config would run untuned. Refuse it.
if (Object.hasOwn(raw, 'libwebrtc_field_trials')) {
  throw new Error(
    `${configPath}: libwebrtc_field_trials is set, but libwebrtc field trials were removed in mediasoup 3.32 ` +
      `(new built-in BWE) and this image runs mediasoup 3.32+. Remove the key ` +
      `(media.tuned-3.31.yaml only works on the mediasoup 3.31 image).`,
  );
}

/** Optional bitrate key in bps: undefined when omitted, else a non-negative integer. */
function optBitrate(key: string): number | undefined {
  const v = raw[key];
  if (v == null) return undefined;
  const n = Number(v);
  if (!Number.isSafeInteger(n) || n < 0) throw new Error(`${key}: expected an integer bitrate in bps, got ${JSON.stringify(v)}`);
  return n;
}

/**
 * Optional per-transport BWE settings. Each one is passed to mediasoup only
 * when its key is in media.yaml, so an omitted key keeps mediasoup's default.
 */
type BweOptions = Pick<
  types.WebRtcTransportOptions,
  'enableBwe' | 'minOutgoingBitrate' | 'maxOutgoingBitrate' | 'maxIncomingBitrate'
>;
function bweOptions(): BweOptions {
  const out: BweOptions = {};
  if (raw.enable_bwe != null) {
    if (typeof raw.enable_bwe !== 'boolean') throw new Error(`enable_bwe: expected true or false, got ${JSON.stringify(raw.enable_bwe)}`);
    out.enableBwe = raw.enable_bwe;
  }
  const minOut = optBitrate('min_outgoing_bitrate');
  const maxOut = optBitrate('max_outgoing_bitrate');
  const maxIn = optBitrate('max_incoming_bitrate');
  // At transport creation the worker raises any limit below 30 kbps to 30 kbps,
  // so 0 would mean a 30 kbps cap here, not "no cap".
  for (const [key, v] of [['max_outgoing_bitrate', maxOut], ['max_incoming_bitrate', maxIn]] as const) {
    if (v !== undefined && v < 30_000) throw new Error(`${key}: ${v} is below the worker minimum of 30000 bps (omit the key for no cap)`);
  }
  if (minOut !== undefined && maxOut !== undefined && minOut > maxOut) {
    throw new Error(`min_outgoing_bitrate (${minOut}) is above max_outgoing_bitrate (${maxOut})`);
  }
  if (minOut !== undefined) out.minOutgoingBitrate = minOut;
  if (maxOut !== undefined) out.maxOutgoingBitrate = maxOut;
  if (maxIn !== undefined) out.maxIncomingBitrate = maxIn;
  return out;
}

if (raw.bwe_telemetry != null && typeof raw.bwe_telemetry !== 'boolean') {
  throw new Error(`bwe_telemetry: expected true or false, got ${JSON.stringify(raw.bwe_telemetry)}`);
}
const bweLog: string | undefined = raw.bwe_log ? String(raw.bwe_log) : undefined;

const workerCpus: number[] = raw.worker_cpus ? parseCpuList(String(raw.worker_cpus)) : [];
const rpc = splitHostPort(raw.rpc_listen ?? '127.0.0.1:3100', 'rpc_listen');
const metricsListen = splitHostPort(raw.metrics_listen ?? '0.0.0.0:9310', 'metrics_listen');
const listenIp: string = raw.listen_ip ?? '0.0.0.0';
const announced: string | undefined = raw.announced_address || (listenIp !== '0.0.0.0' ? listenIp : undefined);
const socketBuf = Number(raw.socket_buffer_bytes ?? 16 * 1024 * 1024);

export const config = {
  configPath,

  /** Internal RPC listener. Only the Go signaling server talks to this. */
  httpHost: rpc.host,
  httpPort: rpc.port,

  /** Prometheus /metrics. */
  metricsHost: metricsListen.host,
  metricsPort: metricsListen.port,

  /** Async events (consumerClosed ...) are POSTed here. */
  eventsUrl: String(raw.events_url ?? 'http://127.0.0.1:8081/internal/events'),

  /**
   * One worker per CPU in worker_cpus, each pinned to its CPU with taskset.
   * If worker_cpus is empty: num_workers workers (0 = all cores), not pinned.
   */
  workerCpus,
  numWorkers: workerCpus.length || Number(raw.num_workers) || os.cpus().length,

  /**
   * How rooms are placed on workers (see room.ts):
   *   webinar     A room's publishers share one worker. That worker also takes
   *               up to max_subs_per_worker subscribers; further subscribers go
   *               to new workers fed by pipes. Every such worker belongs to one
   *               room only, so a room is refused when no worker is free.
   *   one_to_one  The whole room on one randomly picked worker. No limits.
   *   capacity    Load-unit admission control (worker_capacity, load_weights).
   */
  scheduler: parseScheduler(raw.scheduler ?? 'capacity'),
  maxSubsPerWorker: Number(raw.max_subs_per_worker ?? 300),

  /**
   * capacity scheduler: a room starts on the worker with the most free
   * capacity. Once its routers are full (router_max_peers peers, or no
   * capacity left on their workers), the next peers go to a new router on
   * another worker and producers are piped over.
   */
  routerMaxPeers: Number(raw.router_max_peers ?? 200),

  /**
   * capacity scheduler: admission control in load units. A worker never takes more than
   * worker_capacity units: joins, produces and consumes that would go over are
   * rejected. load_weights is the cost of one stream on the worker that
   * forwards it (pipe_producer defaults to the producer weight).
   */
  workerCapacity: Number(raw.worker_capacity ?? 100),
  loadWeights: {
    producer: Number(raw.load_weights?.producer ?? 1),
    consumer: Number(raw.load_weights?.consumer ?? 1.5),
    pipeConsumer: Number(raw.load_weights?.pipe_consumer ?? 2),
    pipeProducer: Number(raw.load_weights?.pipe_producer ?? raw.load_weights?.producer ?? 1),
  },
  /**
   * Units held for a peer from join until its streams exist, so a burst of
   * joins can't overfill a worker before anyone consumes. The hold is
   * max(peer_reserve, consumer weight x producers in the room) and is used up
   * by the peer's own produces/consumes. Default 3 = one audio+video subscription.
   */
  peerReserve: Number(raw.peer_reserve ?? 3),

  webRtcServer: {
    listenIp,
    announcedAddress: announced,
    basePort: Number(raw.rtc_base_port ?? 44444), // worker i -> basePort + i (UDP)
    socketBufferBytes: socketBuf,
  },

  workerSettings: {
    logLevel: (raw.worker_log_level ?? 'warn') as types.WorkerLogLevel,
    logTags: [] as types.WorkerLogTag[],
  },

  /**
   * Per-viewer BWE telemetry (bwe.ts): mediasoup_peer_* metrics from the 'bwe'
   * trace and getStats() of every recv transport. bwe_log also appends it as
   * JSON lines to that path and turns bwe_telemetry on. Both off by default.
   */
  bweTelemetry: raw.bwe_telemetry === true || bweLog !== undefined,
  bweLog,

  /** Router codecs from media_codecs. Omit the key for Opus + VP8. */
  mediaCodecs: mediaCodecs(raw.media_codecs),

  webRtcTransport: {
    enableUdp: true,
    enableTcp: Boolean(raw.enable_tcp ?? false),
    preferUdp: true,
    enableSctp: false, // no DataChannels in the benchmark
    // Start BWE high enough to forward the top simulcast layer (1.5 Mbps)
    // right away, as LiveKit does. After that, transport-cc BWE takes over.
    // The worker clamps it into [min_outgoing_bitrate, max_outgoing_bitrate].
    initialAvailableOutgoingBitrate: optBitrate('initial_outgoing_bitrate') ?? 3_000_000,
    // enable_bwe, min/max_outgoing_bitrate, max_incoming_bitrate (see media.yaml).
    ...bweOptions(),
  },
};

if (config.webRtcServer.listenIp === '0.0.0.0' && !config.webRtcServer.announcedAddress) {
  throw new Error('announced_address must be set when listen_ip is 0.0.0.0');
}
