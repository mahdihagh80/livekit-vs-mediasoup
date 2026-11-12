import { execFile } from "node:child_process";
import { promisify } from "node:util";
import * as mediasoup from "mediasoup";
import type { types } from "mediasoup";
import { config } from "./config";

const execFileP = promisify(execFile);

/** Slack for float sums of load weights (1.5 + 0.1 + ...). */
const EPS = 1e-9;

/**
 * One mediasoup worker = one single-threaded C++ process. We count what lives
 * on each one so placement can pick the worker with the most free capacity and
 * the metrics can show how evenly load is spread.
 */
export class WorkerEntry {
  routers = 0;
  transports = 0;
  producers = 0;
  consumers = 0; // includes pipe consumers
  peers = 0;

  /** Load units of live producers, consumers and pipes (config.loadWeights). */
  used = 0;
  /** Load units held for peers (and routers being created) whose streams don't exist yet. */
  reserved = 0;
  readonly capacity = config.workerCapacity;
  /** webinar scheduler: the room this worker is dedicated to, if any. */
  owner?: string;

  constructor(
    readonly index: number,
    readonly worker: types.Worker,
    readonly webRtcServer: types.WebRtcServer,
    readonly cpu: number | null,
  ) {}

  get load() {
    return this.used + this.reserved;
  }

  get free() {
    return this.capacity - this.load;
  }

  /** Load units are only enforced by the capacity scheduler; the others still count them for metrics. */
  fits(units: number) {
    return config.scheduler !== 'capacity' || units <= this.free + EPS;
  }
}

export async function startWorkers(): Promise<WorkerEntry[]> {
  const entries: WorkerEntry[] = [];

  for (let i = 0; i < config.numWorkers; i++) {
    const worker = await mediasoup.createWorker(config.workerSettings);

    worker.on("died", (err) => {
      // Mid-benchmark this invalidates the run, so fail loudly.
      console.error(`FATAL worker #${i} pid=${worker.pid} died: ${err}`);
      process.exit(1);
    });

    const cpu = config.workerCpus.length ? config.workerCpus[i] : null;
    if (cpu !== null) {
      // Pin this worker to one core. The NIC IRQ cores and the control plane
      // (node + signaling) use different cores; see host/sfu/cpu-plan.md.
      await execFileP("taskset", ["-pc", String(cpu), String(worker.pid)]);
    }

    const port = config.webRtcServer.basePort + i;
    const webRtcServer = await worker.createWebRtcServer({
      listenInfos: [
        {
          protocol: "udp",
          ip: config.webRtcServer.listenIp,
          announcedAddress: config.webRtcServer.announcedAddress,
          port,
          // Large socket buffers; also raise net.core.rmem_max (host/10-sysctl-limits.sh).
          recvBufferSize: config.webRtcServer.socketBufferBytes,
          sendBufferSize: config.webRtcServer.socketBufferBytes,
        },
        ...(config.webRtcTransport.enableTcp
          ? [
              {
                protocol: "tcp" as const,
                ip: config.webRtcServer.listenIp,
                announcedAddress: config.webRtcServer.announcedAddress,
                port,
              },
            ]
          : []),
      ],
    });

    entries.push(new WorkerEntry(i, worker, webRtcServer, cpu));
  }

  console.log(
    `started ${entries.length} workers` +
      (config.workerCpus.length
        ? ` pinned to CPUs ${config.workerCpus.join(",")}`
        : " (not pinned)") +
      `, UDP ports ${config.webRtcServer.basePort}-${config.webRtcServer.basePort + entries.length - 1}`,
  );
  return entries;
}

/**
 * The worker with the most free capacity that still fits `need` units, or
 * undefined if none does. Ties go to the worker with fewer routers, so a burst
 * of new rooms (all workers equally empty) spreads instead of piling on #0.
 */
export function pickWorker(
  workers: WorkerEntry[],
  need: number,
  exclude: Set<number> = new Set(),
): WorkerEntry | undefined {
  let best: WorkerEntry | undefined;
  for (const w of workers) {
    if (exclude.has(w.index) || !w.fits(need)) continue;
    if (!best || w.free > best.free + EPS || (Math.abs(w.free - best.free) <= EPS && w.routers < best.routers)) best = w;
  }
  return best;
}
