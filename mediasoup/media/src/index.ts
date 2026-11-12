/**
 * mediasoup media service for the benchmark.
 *
 *   loadgen <--ws--> Go signaling <--HTTP /rpc--> this service --(unix pipe)--> mediasoup-worker x N
 *   loadgen <================ ICE/DTLS/SRTP (UDP, one port per worker) ==============> workers
 *
 * The /rpc surface is deliberately tiny. Placement (which worker a peer lands
 * on) and pipeToRouter live in room.ts.
 */
import fs from 'node:fs';
import express from 'express';
import * as mediasoup from 'mediasoup';
import { config, pidFile } from './config';
import { metrics, registerWorkerMetrics, registry } from './metrics';
import { Room } from './room';
import { startWorkers, type WorkerEntry } from './workers';

let workers: WorkerEntry[] = [];
const rooms = new Map<string, Promise<Room>>();

function getOrCreateRoom(roomId: string): Promise<Room> {
  let r = rooms.get(roomId);
  if (!r) {
    // Routers are created by the first addPeer, where capacity is checked.
    r = Promise.resolve(new Room(roomId, workers));
    rooms.set(roomId, r);
  }
  return r;
}

function dropIfIdle(roomId: string, room: Room) {
  if (!room.idle) return;
  room.close();
  rooms.delete(roomId);
}

async function existingRoom(roomId: string): Promise<Room> {
  const r = rooms.get(roomId);
  if (!r) throw new Error(`room ${roomId} not found`);
  return r;
}

type RpcRequest = { method: string; roomId: string; peerId: string; data?: any };

async function handleRpc({ method, roomId, peerId, data = {} }: RpcRequest): Promise<unknown> {
  if (method === 'join') {
    const room = await getOrCreateRoom(roomId);
    try {
      const peer = await room.addPeer(peerId, data.role === 'publisher' ? 'publisher' : 'subscriber');
      return { rtpCapabilities: room.rtpCapabilities, worker: peer.slot.worker.index };
    } catch (err) {
      dropIfIdle(roomId, room); // e.g. first join rejected for capacity
      throw err;
    }
  }

  const room = await existingRoom(roomId);

  if (method === 'closePeer') {
    room.removePeer(peerId);
    dropIfIdle(roomId, room);
    return {};
  }

  const peer = room.getPeer(peerId);
  switch (method) {
    case 'createWebRtcTransport':
      return room.createWebRtcTransport(peer, data.direction);
    case 'connectWebRtcTransport':
      await room.connectWebRtcTransport(peer, data.transportId, data.dtlsParameters);
      return {};
    case 'produce':
      return room.produce(peer, data.transportId, data.kind, data.rtpParameters);
    case 'consume':
      return room.consume(peer, data.transportId, data.producerId, data.rtpCapabilities, data.preferredSpatialLayer);
    case 'consumeMany': {
      // One RPC for every existing producer a joining peer subscribes to (a
      // webinar join would otherwise be one HTTP request per producer). Per-item
      // results: one failed consume doesn't fail the others.
      const ids: string[] = data.producerIds ?? [];
      const settled = await Promise.allSettled(
        ids.map((id) => room.consume(peer, data.transportId, id, data.rtpCapabilities, data.preferredSpatialLayer)),
      );
      return settled.map((s, i) =>
        s.status === 'fulfilled'
          ? { producerId: ids[i], ok: true, data: s.value }
          : { producerId: ids[i], ok: false, error: (s.reason as Error).message },
      );
    }
    case 'resumeConsumer':
      await room.resumeConsumer(peer, data.consumerId);
      return {};
    default:
      throw new Error(`unknown method ${method}`);
  }
}

async function main() {
  console.log(`config ${config.configPath}`);
  if (config.bweTelemetry) console.log(`bwe telemetry on (mediasoup_peer_* metrics${config.bweLog ? `, JSONL to ${config.bweLog}` : ''})`);
  console.log(`mediasoup ${mediasoup.version}, ${config.numWorkers} workers, scheduler=${config.scheduler} max_subs_per_worker=${config.maxSubsPerWorker}, router_max_peers=${config.routerMaxPeers}, worker_capacity=${config.workerCapacity} ${JSON.stringify(config.loadWeights)}`);
  if (pidFile) fs.writeFileSync(pidFile, String(process.pid));
  workers = await startWorkers();
  registerWorkerMetrics(workers, rooms);

  const app = express();
  app.use(express.json({ limit: '2mb' }));
  app.post('/rpc', async (req, res) => {
    const body = req.body as RpcRequest;
    const end = metrics.rpcSeconds.startTimer({ method: body.method });
    try {
      const data = await handleRpc(body);
      end({ ok: 'true' });
      res.json({ ok: true, data });
    } catch (err) {
      end({ ok: 'false' });
      res.status(400).json({ ok: false, error: (err as Error).message });
    }
  });
  app.get('/healthz', (_req, res) => res.json({ ok: true, workers: workers.length, rooms: rooms.size }));
  const rpcServer = app.listen(config.httpPort, config.httpHost, () => console.log(`rpc on http://${config.httpHost}:${config.httpPort}`));
  // Keep idle sockets longer than the Go client's IdleConnTimeout (90 s): with
  // Node's 5 s default, Node closes a socket Go is about to reuse -> ECONNRESET.
  rpcServer.keepAliveTimeout = 120_000;
  rpcServer.headersTimeout = 121_000;

  const m = express();
  m.get('/metrics', async (_req, res) => {
    res.set('Content-Type', registry.contentType);
    res.end(await registry.metrics());
  });
  m.listen(config.metricsPort, config.metricsHost, () => console.log(`metrics on ${config.metricsHost}:${config.metricsPort}/metrics`));

  const shutdown = (sig: string) => {
    console.log(`${sig}: closing ${workers.length} workers`);
    for (const w of workers) {
      w.worker.removeAllListeners('died');
      w.worker.close();
    }
    process.exit(0);
  };
  process.on('SIGTERM', () => shutdown('SIGTERM'));
  process.on('SIGINT', () => shutdown('SIGINT'));
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
