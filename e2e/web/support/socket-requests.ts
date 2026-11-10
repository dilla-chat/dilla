// The app's authenticated requests are gateway request frames (op 59), not HTTP requests, so Playwright's `request` event never
// sees them. This reads them from the page's gateway WebSocket frames instead: `[59, 0, null, [id, method, route, body, idem]]`.
// It listens from the call on, across reloads (a page's `websocket` event fires for each new socket, the worker's included).
import type { Page } from '@playwright/test';
import { arr, decode, encode } from '../../../packages/client-core/src/cbor/index';
import { Op, encodeFrame, encodeRequestFrame } from '../../../packages/client-core/src/gateway/frames';

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

/**
 * One request as a short-lived second gateway connection of the session `bearer` names (a device may hold several): mint a
 * ticket (an HTTP route), identify, send the request frame, return its answer and close. For a spec that must call a socket
 * route as a person's session without going through the app: the routes have no HTTP form.
 */
export async function socketRequestOnce(origin: string, bearer: string, method: string, route: string, body: Uint8Array = new Uint8Array()): Promise<{ status: number; body: Uint8Array }> {
  const response = await fetch(`${origin}/v1/gateway/ticket`, {
    method: 'POST', headers: { authorization: bearer, 'content-type': 'application/cbor', accept: 'application/cbor' }, body: encode([]) as unknown as BodyInit,
  });
  if (!response.ok) throw new Error(`gateway ticket = HTTP ${String(response.status)}`);
  const ticket = arr(decode(new Uint8Array(await response.arrayBuffer())))[0];
  const ws = new WebSocket(`${origin.replace(/^http/, 'ws')}/gateway`, ['dilla.v1', `dilla.ticket.${String(ticket)}`]);
  ws.binaryType = 'arraybuffer';
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { ws.close(); reject(new Error(`no answer to ${method} ${route}`)); }, 30_000);
    const done = (fn: () => void): void => { clearTimeout(timer); fn(); ws.close(); };
    ws.onerror = () => { done(() => { reject(new Error('gateway socket error')); }); };
    ws.onmessage = (event: MessageEvent<ArrayBuffer>) => {
      const frame = arr(decode(new Uint8Array(event.data)));
      const op = Number(frame[0]);
      if (op === Op.hello) ws.send(encodeFrame(Op.identify, 1, null, ['', 6, 1, 1, 0]));
      else if (op === Op.ready) {
        const idem = method === 'GET' ? null : crypto.getRandomValues(new Uint8Array(16));
        ws.send(encodeRequestFrame({ id: 1, method, route, body, idem }));
      } else if (op === Op.response) {
        const [, status, answer] = arr(frame[3]);
        done(() => { resolve({ status: Number(status), body: answer instanceof Uint8Array ? answer : new Uint8Array() }); });
      } else if (op === Op.error || op === Op.invalidSession) done(() => { reject(new Error(`gateway refused: op ${String(op)}`)); });
    };
  });
}
