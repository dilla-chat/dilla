// The app's authenticated requests are gateway request frames (op 59), not HTTP requests, so Playwright's `request` event never
// sees them. This reads them from the page's gateway WebSocket frames instead: `[59, 0, null, [id, method, route, body, idem]]`.
// It listens from the call on, across reloads (a page's `websocket` event fires for each new socket, the worker's included).
import type { Page } from '@playwright/test';
import { arr, decode } from '../../../packages/client-core/src/cbor/index';

const OP_REQUEST = 59;
const OP_RESPONSE = 60;

export interface SocketRequestSeen { method: string; route: string; path: string; id: number; body: Uint8Array; /** the response frame's status, once it arrived */ status: number | null }

/** Every request frame `page` sends on its gateway socket from now on, in order. `route` keeps the query, `path` drops it. */
export function watchSocketRequests(page: Page, onRequest?: (r: SocketRequestSeen) => void): SocketRequestSeen[] {
  const seen: SocketRequestSeen[] = [];
  page.on('websocket', (ws) => {
    if (!ws.url().endsWith('/gateway')) return;
    // A response frame `[60, 0, null, [id, status, body]]` answers the request with that id on this socket.
    const open = new Map<number, SocketRequestSeen>();
    ws.on('framereceived', ({ payload }) => {
      if (typeof payload === 'string') return;
      try {
        const frame = arr(decode(new Uint8Array(payload)));
        if (Number(frame[0]) !== OP_RESPONSE) return;
        const [id, status] = arr(frame[3]);
        const request = open.get(Number(id));
        if (request !== undefined) { request.status = Number(status); open.delete(Number(id)); }
      } catch {
        // not a response frame
      }
    });
    ws.on('framesent', ({ payload }) => {
      if (typeof payload === 'string') return;
      try {
        const frame = arr(decode(new Uint8Array(payload)));
        if (Number(frame[0]) !== OP_REQUEST) return;
        const [id, method, route, body] = arr(frame[3]);
        if (typeof method !== 'string' && !(method instanceof Uint8Array)) return;
        const text = (v: unknown): string => (typeof v === 'string' ? v : new TextDecoder().decode(v as Uint8Array));
        const routeText = text(route);
        const request: SocketRequestSeen = { method: text(method), route: routeText, path: routeText.split('?')[0]!, id: Number(id), body: body instanceof Uint8Array ? body : new Uint8Array(0), status: null };
        open.set(request.id, request);
        seen.push(request);
        onRequest?.(request);
      } catch {
        // not a request frame
      }
    });
  });
  return seen;
}
