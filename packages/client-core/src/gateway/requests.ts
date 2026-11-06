// Bot v2: app requests over the gateway socket. One request frame (op 59) per request, answered by a response frame
// (op 60) with its id, in any order.
//
// Ordering is the same as REST today: requests are WRITTEN in submission order, and the instance runs them concurrently,
// so only calls the caller awaited one after the other are applied in that order. While the socket is reconnecting,
// requests wait and are written in submission order once it is ready again. A request whose answer was lost with a
// socket, or did not come within the attempt deadline, is written again with the same idempotency key, so the
// instance's replay record (and the route's own id) answers it instead of applying it twice.
//
// A stopped gateway (status idle: stop(), a revoked session, a refused version) carries nothing: every waiting request
// is rejected and new ones are rejected at once until the gateway starts again. A request never written rejects
// E_NETWORK; a mutation that was written and has no answer rejects E_OUTCOME_UNKNOWN (it may have been applied).
import { HTTP_ATTEMPT_TIMEOUT_MS, type HttpRequest, type HttpResponse } from '../http/client';
import { DillaHttpError, decodeErrorBody } from '../http/errors';
import type { RouteRtt } from '../http/route-rtt';
import { MAX_REQUEST_BODY, encodeRequestFrame } from './frames';
import { CLIENT_CLOSE, type Gateway, type GatewayEvent } from './gateway';

/** What HttpClient sends a socket route through, with the route's matched pattern (`METHOD /pattern`, the RTT key).
 *  `up` is true while the socket can carry a request now. */
export interface SocketTransport {
  request(r: HttpRequest, pattern: string): Promise<HttpResponse>;
  readonly up: boolean;
}

/** The part of the Gateway SocketRequests uses. */
export type RequestGateway = Pick<Gateway, 'status' | 'subscribe' | 'sendRequest'>;

export interface SocketRequestsDeps {
  now(): number;
  random(): number;
  rtt: RouteRtt;
  setTimeout?: (fn: () => void, ms: number) => number;
  clearTimeout?: (id: number) => void;
  /** Overrides HTTP_ATTEMPT_TIMEOUT_MS (tests). */
  attemptTimeoutMs?: number;
}

export const SOCKET_REQUESTS = {
  /** The instance's in-flight cap per connection (gateway.MaxInFlightRequests); more would only earn E_BUSY. */
  maxInFlight: 64,
  /** After E_BUSY nothing more is sent until the next answer, or this long (plus jitter) when none comes. */
  busyPauseMs: 100, busyJitterMs: 100,
  /** 503 E_UNAVAILABLE (the instance's dispatch deadline or its shutdown): sent again, same idem, at most this often. */
  unavailableAttempts: 5, unavailableBaseMs: 500, unavailableCapMs: 8000, unavailableJitterMs: 250, unavailableMaxWaitMs: 60_000,
  /** Attempts that ran out their deadline before the request fails (HttpClient's RETRY.maxAttempts). */
  timeoutAttempts: 5,
} as const;

interface Pending {
  readonly seq: number;
  readonly method: HttpRequest['method'];
  readonly route: string;
  readonly body: Uint8Array;
  readonly idem: Uint8Array | null;
  readonly pattern: string;
  readonly timeoutMs: number | null;
  readonly resolve: (r: HttpResponse) => void;
  readonly reject: (e: unknown) => void;
  readonly signal: AbortSignal | undefined;
  onAbort: (() => void) | null;
  unavailable: number;
  timedOut: number;
  /** True once a frame of this request was written: a mutation's outcome is then unknown until it is answered. */
  written: boolean;
  /** The frame id of the attempt in flight, or null. */
  frameId: number | null;
  sentAt: number;
  timer: number | null;
  deadline: number | null;
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
  /** Why the gateway is stopped (revoked, upgrade, version, stop); cleared when it starts again. */
  private stoppedWith: Error | null = null;
  private readonly unsubscribe: () => void;
  private readonly setTimer: (fn: () => void, ms: number) => number;
  private readonly clearTimer: (id: number) => void;

  constructor(private readonly gateway: RequestGateway, private readonly deps: SocketRequestsDeps) {
    this.setTimer = deps.setTimeout ?? ((fn, ms) => globalThis.setTimeout(fn, ms) as unknown as number);
    this.clearTimer = deps.clearTimeout ?? ((id) => { globalThis.clearTimeout(id); });
    this.unsubscribe = gateway.subscribe((e) => { this.onGateway(e); });
  }

  get up(): boolean { return this.closedWith === null && this.gateway.status === 'ready'; }

  request(r: HttpRequest, pattern: string): Promise<HttpResponse> {
    if (r.method !== 'GET' && (r.idem === undefined || r.idem.length === 0)) {
      // A mutation without a key could be applied twice after a lost answer: a programming error, never sent.
      return Promise.reject(refusal(0, 'E_IDEMPOTENCY_REQUIRED', 'a socket mutation needs an idempotency key'));
    }
    if (this.closedWith !== null) return Promise.reject(this.closedWith);
    if (this.gateway.status === 'idle') return Promise.reject(this.stoppedWith ?? stopped());
    if (r.signal?.aborted === true) return Promise.reject(abortError());
    const body = r.body ?? new Uint8Array();
    if (body.length > MAX_REQUEST_BODY) return Promise.reject(refusal(0, 'E_FRAME_LIMIT', 'the request body is over the frame limit'));
    const timeoutMs = r.timeoutMs === undefined ? (this.deps.attemptTimeoutMs ?? HTTP_ATTEMPT_TIMEOUT_MS) : r.timeoutMs;
    return new Promise<HttpResponse>((resolve, reject) => {
      const p: Pending = { seq: this.nextSeq++, method: r.method, route: r.path, body, idem: r.idem ?? null, pattern, timeoutMs,
        resolve, reject, signal: r.signal, onAbort: null, unavailable: 0, timedOut: 0, written: false, frameId: null, sentAt: 0,
        timer: null, deadline: null, done: false };
      if (r.signal !== undefined) {
        p.onAbort = () => { this.abort(p); };
        r.signal.addEventListener('abort', p.onAbort, { once: true });
      }
      this.queue.push(p);
      this.pump();
    });
  }

  /** Ends this transport (its gateway is being replaced): every waiting request fails (E_NETWORK, or E_OUTCOME_UNKNOWN
   *  for a written mutation), and so does every later one. */
  close(): void {
    if (this.closedWith !== null) return;
    this.closedWith = refusal(0, 'E_NETWORK', 'the gateway socket was closed');
    this.unsubscribe();
    this.failAll(null);
  }

  private onGateway(e: GatewayEvent): void {
    if (this.closedWith !== null) return;
    switch (e.type) {
      case 'status':
        if (e.status === 'idle') {
          // Stopped: nothing will carry these. A revocation or an upgrade refusal is announced right after this status
          // and names its own code first; whatever is left then fails as stopped (or as a refused version).
          const reason = e.closeCode === CLIENT_CLOSE.version
            ? refusal(0, 'E_VERSION', 'the instance refused this client\'s wire version') : null;
          this.stoppedWith = reason;
          this.requeueInFlight();
          queueMicrotask(() => { if (this.gateway.status === 'idle') this.failAll(this.stoppedWith); });
          return;
        }
        this.stoppedWith = null;
        if (e.status === 'ready') { this.pump(); return; }
        // Disconnected but reconnecting: what was in flight waits for the next socket.
        this.requeueInFlight();
        return;
      case 'upgradeRequired':
        this.stoppedWith = refusal(426, 'E_UPGRADE_REQUIRED', 'the instance asks for a newer app');
        this.failAll(this.stoppedWith);
        return;
      case 'revoked':
        this.stoppedWith = refusal(401, 'E_UNAUTHENTICATED', 'the session was revoked');
        this.failAll(this.stoppedWith);
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
      p.written = true;
      p.frameId = id;
      p.sentAt = this.deps.now();
      this.inFlight.set(id, p);
      if (p.timeoutMs !== null) p.deadline = this.setTimer(() => { this.onDeadline(p, id); }, p.timeoutMs);
    }
  }

  /** The attempt ran out its deadline: its late answer is dropped (the id leaves the table) and the request is written
   *  again with the same idem, or fails once the attempts are spent. */
  private onDeadline(p: Pending, id: number): void {
    p.deadline = null;
    if (this.inFlight.get(id) !== p) return;
    this.inFlight.delete(id);
    p.frameId = null;
    if (!p.done) {
      p.timedOut += 1;
      if (p.timedOut < SOCKET_REQUESTS.timeoutAttempts) this.insert(p);
      else {
        this.settle(p);
        p.reject(this.lostError(p, 'the request timed out'));
      }
    }
    this.pump();
  }

  private onResponse(id: number, status: number, body: Uint8Array): void {
    const p = this.inFlight.get(id);
    if (p === undefined) return;                      // an answer for a socket since replaced, a timed-out attempt, or a duplicate
    this.inFlight.delete(id);
    this.clearDeadline(p);
    p.frameId = null;
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
    for (const p of lost) {
      this.clearDeadline(p);
      p.frameId = null;
      if (!p.done) this.insert(p);
    }
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
    // An in-flight request stays in the table until its answer (or deadline), which is then dropped; it is not sent again.
    this.settle(p);
    p.reject(abortError());
  }

  private clearDeadline(p: Pending): void {
    if (p.deadline !== null) { this.clearTimer(p.deadline); p.deadline = null; }
  }

  private settle(p: Pending): void {
    p.done = true;
    if (p.timer !== null) { this.clearTimer(p.timer); p.timer = null; }
    if (p.onAbort !== null) { p.signal?.removeEventListener('abort', p.onAbort); p.onAbort = null; }
  }

  /** A request that will get no answer: E_OUTCOME_UNKNOWN for a written mutation, E_NETWORK otherwise. */
  private lostError(p: Pending, detail: string): DillaHttpError {
    return p.written && p.method !== 'GET' ? refusal(0, 'E_OUTCOME_UNKNOWN', detail) : refusal(0, 'E_NETWORK', detail);
  }

  /** Rejects everything waiting: with `error` when one is named (revoked, upgrade, version), else by the lost split. */
  private failAll(error: Error | null): void {
    const all = [...this.queue, ...this.waiting, ...this.inFlight.values()];
    this.queue.length = 0;
    this.waiting.clear();
    this.inFlight.clear();
    this.resume();
    for (const p of all) {
      if (p.done) continue;
      this.clearDeadline(p);
      this.settle(p);
      p.reject(error ?? this.lostError(p, 'the gateway socket was stopped'));
    }
  }
}

function stopped(): DillaHttpError {
  return refusal(0, 'E_NETWORK', 'the gateway socket is stopped');
}

function refusal(status: number, code: string, detail: string): DillaHttpError {
  return new DillaHttpError({ status, code, detail, retryAfterMs: null, extra: [] });
}
