import http from 'node:http';
import { config } from './config';

export type MediaEvent = { roomId: string; peerId: string; method: string; data: object };

// Keep-alive agent: at teardown thousands of consumerClosed events are fired.
const agent = new http.Agent({ keepAlive: true, maxSockets: 32 });
const url = new URL(config.eventsUrl);

/** Fire-and-forget push to the Go signaling server. */
export function emitEvent(ev: MediaEvent) {
  const body = JSON.stringify(ev);
  const req = http.request(
    {
      agent,
      host: url.hostname,
      port: url.port,
      path: url.pathname,
      method: 'POST',
      headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) },
    },
    (res) => res.resume(),
  );
  req.on('error', () => {});
  req.end(body);
}
