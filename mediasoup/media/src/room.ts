import type { types } from 'mediasoup';
import { trackConsumer, trackTransport } from './bwe';
import { config } from './config';
import { emitEvent } from './events';
import { metrics } from './metrics';
import { pickWorker, type WorkerEntry } from './workers';

const W = config.loadWeights;

/** Refused because it would put a worker over worker_capacity. */
export class CapacityError extends Error {
  constructor(what: 'join' | 'produce' | 'consume' | 'pipe', message: string) {
    super(message);
    metrics.capacityRejections.inc({ what });
  }
}

/** Declared by the client at join; the webinar scheduler places by it. */
export type Role = 'publisher' | 'subscriber';

/** Our bookkeeping for one client (mediasoup has no "participant" concept). */
export class Peer {
  transports = new Map<string, types.WebRtcTransport>();
  producers = new Map<string, types.Producer>();
  consumers = new Map<string, types.Consumer>();
  /** Units still held on slot.worker for streams this peer hasn't created yet. */
  reserved = 0;

  constructor(
    readonly id: string,
    readonly role: Role,
    readonly slot: RouterSlot,
  ) {}

  close() {
    for (const t of this.transports.values()) t.close(); // closes producers/consumers too
  }
}

/** One Router of a room, living on one worker. */
type RouterSlot = { router: types.Router; worker: WorkerEntry; peers: number; subs: number };

type ProducerEntry = {
  owner: Peer;
  producer: types.Producer;
  /** Routers that have this producer (its own + piped ones): possible pipe sources. */
  holders: Set<RouterSlot>;
};

/**
 * A Room is 1..N Routers on different workers; producers are piped
 * (pipeToRouter) to wherever their consumers live. Where peers go depends on
 * config.scheduler:
 *
 *   webinar     The room's first router is its publisher router: every
 *               publisher, plus the first max_subs_per_worker subscribers.
 *               More subscribers fill subscriber routers on further workers,
 *               max_subs_per_worker each, fed by pipes from the publisher
 *               router. Each of these workers is dedicated to this room until
 *               its router closes (an emptied subscriber router closes at once).
 *   one_to_one  One router on a random worker for everyone.
 *   capacity    Every stream is charged to its worker in load units
 *               (config.loadWeights) and nothing is created that would put a
 *               worker over worker_capacity. Full routers grow the room.
 *
 * Joins, produces and consumes that can't be placed fail with a CapacityError.
 */
export class Room {
  readonly peers = new Map<string, Peer>();
  private slots: RouterSlot[] = [];
  private producers = new Map<string, ProducerEntry>();
  private pipes = new Map<string, Promise<void>>();
  // Serialize growth so concurrent joins don't each create a router.
  private growing?: Promise<void>;
  private joining = 0;
  private closed = false;

  constructor(
    readonly id: string,
    private readonly workers: WorkerEntry[],
  ) {}

  get rtpCapabilities() {
    return this.slots[0].router.rtpCapabilities;
  }

  /** No peers and no join in flight: safe to close and forget. */
  get idle() {
    return this.peers.size === 0 && this.joining === 0;
  }

  /** True while some worker has no router of this room yet. */
  private get canGrow() {
    return this.slots.length < this.workers.length;
  }

  /** Units to hold for a joining peer: at least one subscription to every producer in the room. */
  private peerEstimate() {
    return Math.max(config.peerReserve, W.consumer * this.producers.size);
  }

  /**
   * Units a router keeps free (from its own room's peers) so its producers can
   * still be piped on to one more router (pipes fan out from any router that
   * has the producer, see ensurePiped). Without it the first router of a
   * webinar would fill up with viewers and could never feed a second one.
   * It is a placement limit, not a reservation: other rooms on the worker may
   * still use it, in which case growth fails cleanly (see newSlot).
   */
  private pipeHeadroom() {
    return this.canGrow ? W.pipeConsumer * this.producers.size : 0;
  }

  /** Start piping a producer to `target` now; failures are retried by the first consume there. */
  private prePipe(producerId: string, entry: ProducerEntry, target: RouterSlot) {
    try {
      this.ensurePiped(producerId, entry, target).catch(() => {});
    } catch (err) {
      console.warn(`room ${this.id}: pipe ${producerId} -> worker #${target.worker.index}: ${(err as Error).message}`);
    }
  }

  /** An existing router of the room that can take this peer, if any. */
  private openSlot(role: Role, need: number): RouterSlot | undefined {
    switch (config.scheduler) {
      case 'one_to_one':
        return this.slots[0];
      case 'webinar':
        if (role === 'publisher') return this.slots[0];
        // Publisher router first, then subscriber routers in creation order.
        return this.slots.find((s) => s.subs < config.maxSubsPerWorker);
      case 'capacity':
        return this.openSlotByCapacity(need);
    }
  }

  /** Worker for the room's next router; throws if there is none. */
  private growTarget(peerId: string, need: number): WorkerEntry {
    switch (config.scheduler) {
      case 'one_to_one':
        return this.workers[Math.floor(Math.random() * this.workers.length)];
      case 'webinar': {
        const w = this.workers.find((w) => w.owner === undefined);
        if (!w) throw new CapacityError('join', `room ${this.id}: no free worker for peer ${peerId}`);
        return w;
      }
      case 'capacity': {
        // A new router also receives a pipe of every producer in the room.
        const hold = need + W.pipeProducer * this.producers.size;
        const used = new Set(this.slots.map((s) => s.worker.index));
        const w = pickWorker(this.workers, hold + this.pipeHeadroom(), used);
        if (!w) throw new CapacityError('join', `room ${this.id}: no worker has ${hold} free units for peer ${peerId}`);
        return w;
      }
    }
  }

  /** The room's router with the most free capacity that can take a peer needing `need` units. */
  private openSlotByCapacity(need: number): RouterSlot | undefined {
    const headroom = this.pipeHeadroom();
    let best: RouterSlot | undefined;
    for (const s of this.slots) {
      if (s.peers >= config.routerMaxPeers || !s.worker.fits(need + headroom)) continue;
      if (!best || s.worker.free > best.worker.free) best = s;
    }
    return best;
  }

  /**
   * New router on `worker`, which must not host a router of this room yet:
   * pipeToRouter keeps the producer id, and one worker can't hold two
   * producers with the same id ("Channel request handler ... already exists").
   * It would also waste CPU on a same-core pipe.
   *
   * `need` (the first peer) plus the pipe producers for every producer in the
   * room stay reserved on the worker while the router is created, and the
   * router is counted before the await, so concurrent joins of other rooms
   * place around it. Every producer is then piped in before the router is
   * offered to peers: if some producer can't be fed (no holder has a pipe
   * consumer's worth free) the router is dropped and the join refused, rather
   * than placing peers that would fail halfway through their consumes.
   */
  private async newSlot(worker: WorkerEntry, need: number) {
    const pipeIn = W.pipeProducer * this.producers.size;
    const dedicated = config.scheduler === 'webinar';
    if (dedicated) worker.owner = this.id; // before the await: other rooms must skip it
    worker.routers++;
    worker.reserved += need + pipeIn;
    let router: types.Router;
    try {
      router = await worker.worker.createRouter({ mediaCodecs: config.mediaCodecs });
    } catch (err) {
      if (dedicated) worker.owner = undefined;
      worker.routers--;
      worker.reserved -= need + pipeIn;
      throw err;
    }
    router.observer.on('close', () => {
      worker.routers--;
      if (dedicated) worker.owner = undefined;
    });
    const slot = { router, worker, peers: 0, subs: 0 };
    worker.reserved -= pipeIn; // the pipes charge it themselves
    try {
      if (this.closed) throw new Error(`room ${this.id} is closed`);
      await Promise.all([...this.producers].map(([id, entry]) => this.ensurePiped(id, entry, slot)));
      if (this.closed) throw new Error(`room ${this.id} is closed`);
    } catch (err) {
      router.close(); // also closes the pipes that did get made, releasing their charges
      throw err;
    } finally {
      worker.reserved -= need;
    }
    this.slots.push(slot);
    if (this.slots.length > 1) console.log(`room ${this.id}: grew to ${this.slots.length} routers (worker #${worker.index})`);
  }

  async addPeer(peerId: string, role: Role): Promise<Peer> {
    this.joining++;
    try {
      for (;;) {
        const existing = this.peers.get(peerId);
        if (existing) return existing;
        if (this.closed) throw new Error(`room ${this.id} is closed`);

        const need = this.peerEstimate();
        const slot = this.openSlot(role, need);
        if (slot) return this.place(peerId, role, slot, need);

        if (this.growing) {
          await this.growing;
          continue; // re-check: the new router may already be full again
        }
        const worker = this.growTarget(peerId, need);
        this.growing = this.newSlot(worker, need).finally(() => (this.growing = undefined));
        await this.growing;
      }
    } finally {
      this.joining--;
    }
  }

  private place(peerId: string, role: Role, slot: RouterSlot, need: number): Peer {
    slot.peers++;
    if (role === 'subscriber') slot.subs++;
    slot.worker.peers++;
    slot.worker.reserved += need;
    const peer = new Peer(peerId, role, slot);
    peer.reserved = need;
    this.peers.set(peerId, peer);
    return peer;
  }

  removePeer(peerId: string) {
    const peer = this.peers.get(peerId);
    if (!peer) return;
    const slot = peer.slot;
    peer.close();
    slot.worker.reserved -= peer.reserved;
    peer.reserved = 0;
    slot.peers--;
    if (peer.role === 'subscriber') slot.subs--;
    slot.worker.peers--;
    this.peers.delete(peerId);
    // webinar: give an emptied subscriber worker back to other rooms.
    if (config.scheduler === 'webinar' && slot.peers === 0 && slot !== this.slots[0] && !this.closed) {
      this.slots.splice(this.slots.indexOf(slot), 1);
      slot.router.close();
    }
  }

  getPeer(peerId: string): Peer {
    const p = this.peers.get(peerId);
    if (!p) throw new Error(`peer ${peerId} not found`);
    return p;
  }

  close() {
    if (this.closed) return;
    this.closed = true;
    for (const peerId of [...this.peers.keys()]) this.removePeer(peerId);
    for (const s of this.slots) s.router.close();
  }

  /**
   * Charge `units` to `w`, taking them first from `peer`'s reservation (which
   * already counts toward the worker). Throws if the worker would go over
   * capacity. Returns the release to call when the stream goes away.
   */
  private charge(what: 'produce' | 'consume' | 'pipe', w: WorkerEntry, units: number, peer?: Peer): () => void {
    const fromHold = peer ? Math.min(peer.reserved, units) : 0;
    if (!w.fits(units - fromHold)) {
      throw new CapacityError(what, `worker #${w.index}: ${what} needs ${units - fromHold} units, ${w.free.toFixed(1)}/${w.capacity} free`);
    }
    if (peer) {
      peer.reserved -= fromHold;
      w.reserved -= fromHold;
    }
    w.used += units;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      w.used -= units;
    };
  }

  // --- transports -----------------------------------------------------------

  async createWebRtcTransport(peer: Peer, direction: 'send' | 'recv') {
    const w = peer.slot.worker;
    const transport = await peer.slot.router.createWebRtcTransport({
      ...config.webRtcTransport,
      webRtcServer: w.webRtcServer,
      appData: { direction },
    });
    w.transports++;
    peer.transports.set(transport.id, transport);
    transport.observer.on('close', () => {
      w.transports--;
      peer.transports.delete(transport.id);
    });
    transport.on('icestatechange', (s) => metrics.iceState.inc({ state: s }));
    transport.on('dtlsstatechange', (s) => metrics.dtlsState.inc({ state: s }));
    if (direction === 'recv') trackTransport(peer.id, transport);

    return {
      id: transport.id,
      iceParameters: transport.iceParameters,
      iceCandidates: transport.iceCandidates,
      dtlsParameters: transport.dtlsParameters,
    };
  }

  async connectWebRtcTransport(peer: Peer, transportId: string, dtlsParameters: types.DtlsParameters) {
    await this.transport(peer, transportId).connect({ dtlsParameters });
  }

  private transport(peer: Peer, transportId: string) {
    const t = peer.transports.get(transportId);
    if (!t) throw new Error(`transport ${transportId} not found`);
    return t;
  }

  // --- media ----------------------------------------------------------------

  async produce(peer: Peer, transportId: string, kind: types.MediaKind, rtpParameters: types.RtpParameters) {
    const w = peer.slot.worker;
    const transport = this.transport(peer, transportId);
    const release = this.charge('produce', w, W.producer, peer);
    let producer: types.Producer;
    try {
      producer = await transport.produce({ kind, rtpParameters, appData: { peerId: peer.id } });
    } catch (err) {
      release();
      throw err;
    }
    w.producers++;
    peer.producers.set(producer.id, producer);
    const entry = { owner: peer, producer, holders: new Set([peer.slot]) };
    this.producers.set(producer.id, entry);
    producer.observer.on('close', () => {
      release();
      w.producers--;
      peer.producers.delete(producer.id);
      this.producers.delete(producer.id);
    });
    // Every router of the room serves subscribers, so feed them all now.
    for (const s of this.slots) if (s !== peer.slot) this.prePipe(producer.id, entry, s);
    return { id: producer.id };
  }

  private findProducer(producerId: string): ProducerEntry {
    const e = this.producers.get(producerId);
    if (!e) throw new Error(`producer ${producerId} not found`);
    return e;
  }

  /**
   * Where to pipe a producer from: its own router if that worker has room for
   * one more pipe consumer, else the router holding it with the most free
   * capacity. So a webinar fans out as a tree once the publisher's worker is full.
   */
  private pipeSource(entry: ProducerEntry): RouterSlot | undefined {
    const own = entry.owner.slot;
    if (entry.holders.has(own) && own.worker.fits(W.pipeConsumer)) return own;
    let best: RouterSlot | undefined;
    for (const s of entry.holders) {
      if (s.worker.fits(W.pipeConsumer) && (!best || s.worker.free > best.worker.free)) best = s;
    }
    return best;
  }

  /** Make the producer available on `target` (pipeToRouter once, then cached). */
  private ensurePiped(producerId: string, entry: ProducerEntry, target: RouterSlot): Promise<void> {
    if (entry.holders.has(target)) return Promise.resolve();
    const key = `${producerId}:${target.router.id}`;
    let p = this.pipes.get(key);
    if (p) return p;

    const source = this.pipeSource(entry);
    if (!source) {
      throw new CapacityError('pipe', `room ${this.id}: no router holding ${producerId} has ${W.pipeConsumer} units free for a pipe`);
    }
    // A pipe is one pipe consumer on the source worker and one pipe producer on the target.
    const releaseOut = this.charge('pipe', source.worker, W.pipeConsumer);
    let releaseIn: () => void;
    try {
      releaseIn = this.charge('pipe', target.worker, W.pipeProducer);
    } catch (err) {
      releaseOut();
      throw err;
    }
    p = (async () => {
      let res: types.PipeToRouterResult;
      try {
        res = await source.router.pipeToRouter({ producerId, router: target.router });
      } catch (err) {
        releaseOut();
        releaseIn();
        throw err;
      }
      source.worker.consumers++;
      target.worker.producers++;
      metrics.pipes.inc();
      entry.holders.add(target);
      res.pipeConsumer?.observer.on('close', () => {
        releaseOut();
        source.worker.consumers--;
      });
      res.pipeProducer?.observer.on('close', () => {
        releaseIn();
        target.worker.producers--;
        entry.holders.delete(target);
        this.pipes.delete(key);
      });
    })();
    p.catch(() => this.pipes.delete(key));
    this.pipes.set(key, p);
    return p;
  }

  /**
   * preferredSpatialLayer: the simulcast/SVC layer this subscriber wants
   * (the loadgen's per-participant quality, same layer LiveKit is asked for).
   * Undefined = highest, mediasoup's default.
   */
  async consume(
    peer: Peer,
    transportId: string,
    producerId: string,
    rtpCapabilities: types.RtpCapabilities,
    preferredSpatialLayer?: number,
  ) {
    const entry = this.findProducer(producerId);
    const { owner, producer } = entry;
    const router = peer.slot.router;
    const w = peer.slot.worker;
    const transport = this.transport(peer, transportId);

    // Charge the consumer first so a consume that can't fit never creates a pipe.
    const release = this.charge('consume', w, W.consumer, peer);
    let consumer: types.Consumer;
    try {
      await this.ensurePiped(producerId, entry, peer.slot);
      if (!router.canConsume({ producerId, rtpCapabilities })) {
        throw new Error(`peer ${peer.id} cannot consume ${producerId}`);
      }
      consumer = await transport.consume({
        producerId,
        rtpCapabilities,
        paused: true, // client resumes once its receiver is ready (no lost keyframe)
        ...(producer.kind === 'video' && typeof preferredSpatialLayer === 'number'
          ? { preferredLayers: { spatialLayer: preferredSpatialLayer } }
          : {}),
        appData: { peerId: peer.id, producerPeerId: owner.id },
      });
    } catch (err) {
      release();
      throw err;
    }
    w.consumers++;
    peer.consumers.set(consumer.id, consumer);
    trackConsumer(transport.id, consumer);
    consumer.observer.on('close', () => {
      release();
      w.consumers--;
      peer.consumers.delete(consumer.id);
    });
    consumer.on('producerclose', () => {
      emitEvent({ roomId: this.id, peerId: peer.id, method: 'consumerClosed', data: { consumerId: consumer.id } });
    });

    return {
      id: consumer.id,
      producerId,
      producerPeerId: owner.id,
      kind: consumer.kind,
      type: consumer.type,
      rtpParameters: consumer.rtpParameters,
    };
  }

  async resumeConsumer(peer: Peer, consumerId: string) {
    const c = peer.consumers.get(consumerId);
    if (!c) throw new Error(`consumer ${consumerId} not found`);
    await c.resume();
    if (c.kind === 'video') await c.requestKeyFrame();
  }

  /** All consumers in the room (used by the metrics sampler). */
  *allConsumers(): Iterable<types.Consumer> {
    for (const p of this.peers.values()) yield* p.consumers.values();
  }
}
