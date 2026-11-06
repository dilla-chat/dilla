// Bot v2: app requests over the gateway socket. One request frame (op 59) per request, answered by a response frame
// (op 60) with its id. While the socket is down requests wait in submission order and drain in that order once it is
// ready again; a request whose answer was lost with a socket is sent again with the same idempotency key, so the
// instance's replay record (and the route's own id) answers it instead of applying it twice.
import type { HttpRequest, HttpResponse } from '../http/client';
import { DillaHttpError, decodeErrorBody } from '../http/errors';
import type { RouteRtt } from '../http/route-rtt';
import { socketRouteFor, type SocketRoute } from '../http/socket-routes';
import { MAX_REQUEST_BODY, encodeRequestFrame } from './frames';
import { CLIENT_CLOSE, type Gateway, type GatewayEvent } from './gateway';

/** What HttpClient sends a socket route through. `up` is true while the socket can carry a request now. */
export interface SocketTransport {
  request(r: HttpRequest): Promise<HttpResponse>;
  readonly up: boolean;
}

/** The part of the Gateway SocketRequests uses. */
export type RequestGateway = Pick<Gateway, 'status' | 'subscribe' | 'sendRequest'>;

export interface SocketRequestsDeps {
  now(): number;
  random(): number;
  rtt: RouteRtt;
  /** The socket route table; SOCKET_ROUTES unless a test passes its own. Only used to name a request's pattern. */
  routes?: readonly SocketRoute[];
  setTimeout?: (fn: () => void, ms: number) => number;
  clearTimeout?: (id: number) => void;
}

export const SOCKET_REQUESTS = {
  /** The instance's in-flight cap per connection (gateway.MaxInFlightRequests); more would only earn E_BUSY. */
  maxInFlight: 64,
  /** After E_BUSY nothing more is sent until the next answer, or this long (plus jitter) when none comes. */
  busyPauseMs: 100, busyJitterMs: 100,
  /** 503 E_UNAVAILABLE (the instance's dispatch deadline or its shutdown): sent again, same idem, at most this often. */
  unavailableAttempts: 5, unavailableBaseMs: 500, unavailableCapMs: 8000, unavailableJitterMs: 250, unavailableMaxWaitMs: 60_000,
} as const;

interface Pending {
  readonly seq: number;
  readonly method: HttpRequest['method'];
  readonly route: string;
  readonly body: Uint8Array;
  readonly idem: Uint8Array | null;
  readonly pattern: string;
  readonly resolve: (r: HttpResponse) => void;
  readonly reject: (e: unknown) => void;
  readonly signal: AbortSignal | undefined;
  onAbort: (() => void) | null;
  unavailable: number;
  sentAt: number;
  timer: number | null;
  done: boolean;
}

const abortError = (): DOMException => new DOMException('The operation was aborted.', 'AbortError');

export class SocketRequests implements SocketTransport {
  private readonly queue: Pending[] = [];
  private readonly inFlight = new Map<number, Pending>();
  private readonly waiting = new Set<Pending>();
  private nextSeq = 0;
  private nextId = 1;
  private paused = false;
  private pauseTimer: number | null = null;
  private closedWith: Error | null = null;
  private readonly unsubscribe: () => void;
  private readonly setTimer: (fn: () => void, ms: number) => number;
  private readonly clearTimer: (id: number) => void;

  constructor(private readonly gateway: RequestGateway, private readonly deps: SocketRequestsDeps) {
    this.setTimer = deps.setTimeout ?? ((fn, ms) => globalThis.setTimeout(fn, ms) as unknown as number);
    this.clearTimer = deps.clearTimeout ?? ((id) => { globalThis.clearTimeout(id); });
    this.unsubscribe = gateway.subscribe((e) => { this.onGateway(e); });
  }

  get up(): boolean { return this.closedWith === null && this.gateway.status === 'ready'; }

  request(r: HttpRequest): Promise<HttpResponse> {
    if (r.method !== 'GET' && (r.idem === undefined || r.idem.length === 0)) {
      // A mutation without a key could be applied twice after a lost answer: a programming error, never sent.
      return Promise.reject(new Error('E_IDEMPOTENCY_REQUIRED'));
    }
    if (this.closedWith !== null) return Promise.reject(this.closedWith);
    if (r.signal?.aborted === true) return Promise.reject(abortError());
    const body = r.body ?? new Uint8Array();
    if (body.length > MAX_REQUEST_BODY) return Promise.reject(new Error('E_FRAME_LIMIT'));
    const bare = r.path.split('?', 1)[0];
    const pattern = socketRouteFor(r.method, r.path, this.deps.routes) ?? `${r.method} ${bare}`;
    return new Promise<HttpResponse>((resolve, reject) => {
      const p: Pending = { seq: this.nextSeq++, method: r.method, route: r.path, body, idem: r.idem ?? null, pattern,
        resolve, reject, signal: r.signal, onAbort: null, unavailable: 0, sentAt: 0, timer: null, done: false };
      if (r.signal !== undefined) {
        p.onAbort = () => { this.abort(p); };
        r.signal.addEventListener('abort', p.onAbort, { once: true });
      }
      this.queue.push(p);
      this.pump();
    });
  }

  /** Ends this transport (its gateway is being replaced): every waiting request fails with `error`. */
  close(error: Error = new DillaHttpError({ status: 0, code: 'E_NETWORK', detail: 'the gateway socket was closed',
    retryAfterMs: null, extra: [] })): void {
    if (this.closedWith !== null) return;
    this.closedWith = error;
    this.unsubscribe();
    this.failAll(error);
  }

  private onGateway(e: GatewayEvent): void {
    if (this.closedWith !== null) return;
    switch (e.type) {
      case 'status':
        if (e.status === 'ready') { this.pump(); return; }
        this.requeueInFlight();
        // A plain version refusal leaves the gateway idle until the page reloads. An upgrade refusal is announced right
        // after this status, and answers first.
        if (e.status === 'idle' && e.closeCode === CLIENT_CLOSE.version) {
          queueMicrotask(() => { this.failAll(refusal(0, 'E_VERSION', 'the instance refused this client\'s wire version')); });
        }
        return;
      case 'upgradeRequired':
        this.failAll(refusal(426, 'E_UPGRADE_REQUIRED', 'the instance asks for a newer app'));
        return;
      case 'revoked':
        this.failAll(refusal(401, 'E_UNAUTHENTICATED', 'the session was revoked'));
        return;
      case 'response':
        this.onResponse(e.response.id, e.response.status, e.response.body);
        return;
      default:
        return;
    }
  }

  private pump(): void {
    if (!this.up || this.paused) return;
    while (this.queue.length > 0 && this.inFlight.size < SOCKET_REQUESTS.maxInFlight) {
      const p = this.queue[0];
      const id = this.nextId++;
      let frame: Uint8Array;
      try { frame = encodeRequestFrame({ id, method: p.method, route: p.route, body: p.body, idem: p.idem }); }
      catch (e) {
        this.queue.shift();
        this.settle(p);
        p.reject(e);
        continue;
      }
      if (!this.gateway.sendRequest(frame)) return;
      this.queue.shift();
      p.sentAt = this.deps.now();
      this.inFlight.set(id, p);
    }
  }

  private onResponse(id: number, status: number, body: Uint8Array): void {
    const p = this.inFlight.get(id);
    if (p === undefined) return;                      // an answer for a socket since replaced, or a duplicate
    this.inFlight.delete(id);
    this.resume();                                    // any answer frees room at the instance
    if (!p.done) {
      const code = status === 429 || status === 503 ? decodeErrorBody(status, body, null) : null;
      if (status === 429 && code?.code === 'E_BUSY') {
        // Over the instance's in-flight or byte budget: back to its place at the head, sent again once there is room.
        this.insert(p);
        this.pause();
      } else if (status === 503 && code?.code === 'E_UNAVAILABLE' && p.unavailable + 1 < SOCKET_REQUESTS.unavailableAttempts) {
        this.retryLater(p, code.retryAfterMs);
      } else {
        this.deps.rtt.record(p.pattern, this.deps.now() - p.sentAt);
        this.settle(p);
        p.resolve({ status, body });
      }
    }
    this.pump();
  }

  /** 503 E_UNAVAILABLE: the same request (same idem) goes back in its place after a jittered backoff. */
  private retryLater(p: Pending, retryAfterMs: number | null): void {
    p.unavailable += 1;
    const backoff = retryAfterMs ?? Math.min(SOCKET_REQUESTS.unavailableCapMs, SOCKET_REQUESTS.unavailableBaseMs * 2 ** (p.unavailable - 1));
    const wait = Math.min(backoff, SOCKET_REQUESTS.unavailableMaxWaitMs) + Math.floor(this.deps.random() * SOCKET_REQUESTS.unavailableJitterMs);
    this.waiting.add(p);
    p.timer = this.setTimer(() => {
      p.timer = null;
      this.waiting.delete(p);
      if (p.done || this.closedWith !== null) return;
      this.insert(p);
      this.pump();
    }, wait);
  }

  private pause(): void {
    this.paused = true;
    if (this.pauseTimer !== null) this.clearTimer(this.pauseTimer);
    const ms = SOCKET_REQUESTS.busyPauseMs + Math.floor(this.deps.random() * SOCKET_REQUESTS.busyJitterMs);
    this.pauseTimer = this.setTimer(() => {
      this.pauseTimer = null;
      this.paused = false;
      this.pump();
    }, ms);
  }

  private resume(): void {
    this.paused = false;
    if (this.pauseTimer !== null) { this.clearTimer(this.pauseTimer); this.pauseTimer = null; }
  }

  /** The socket is gone: every request it carried is queued again, in submission order, ahead of the newer ones. */
  private requeueInFlight(): void {
    const lost = [...this.inFlight.values()];
    this.inFlight.clear();
    for (const p of lost) if (!p.done) this.insert(p);
    this.resume();
  }

  /** Puts a request back by its submission order. */
  private insert(p: Pending): void {
    let i = 0;
    while (i < this.queue.length && this.queue[i].seq < p.seq) i++;
    this.queue.splice(i, 0, p);
  }

  private abort(p: Pending): void {
    if (p.done) return;
    const queued = this.queue.indexOf(p);
    if (queued >= 0) this.queue.splice(queued, 1);
    this.waiting.delete(p);
    // An in-flight request stays in the table until its answer, which is then dropped; it is not sent again.
    this.settle(p);
    p.reject(abortError());
  }

  private settle(p: Pending): void {
    p.done = true;
    if (p.timer !== null) { this.clearTimer(p.timer); p.timer = null; }
    if (p.onAbort !== null) { p.signal?.removeEventListener('abort', p.onAbort); p.onAbort = null; }
  }

  private failAll(error: unknown): void {
    const all = [...this.queue, ...this.waiting, ...this.inFlight.values()];
    this.queue.length = 0;
    this.waiting.clear();
    this.inFlight.clear();
    this.resume();
    for (const p of all) {
      if (p.done) continue;
      this.settle(p);
      p.reject(error);
    }
  }
}

function refusal(status: number, code: string, detail: string): DillaHttpError {
  return new DillaHttpError({ status, code, detail, retryAfterMs: null, extra: [] });
}
