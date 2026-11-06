import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { decode, encode, type CborInput } from '../cbor';
import { HttpClient, type HttpDeps, type HttpRequest } from '../http/client';
import { DillaHttpError } from '../http/errors';
import { RouteRtt } from '../http/route-rtt';
import type { SocketRoute } from '../http/socket-routes';
import { decodeFrame, Op } from './frames';
import type { GatewayEvent, GatewayStatus } from './gateway';
import { SocketRequests } from './requests';

interface Sent { id: number; method: string; route: string; body: Uint8Array; idem: Uint8Array | null }

/** The gateway as SocketRequests sees it: a status, its events, and request frames written while ready. */
class FakeGateway {
  status: GatewayStatus = 'ready';
  readonly sent: Sent[] = [];
  private readonly listeners = new Set<(e: GatewayEvent) => void>();
  subscribe(listener: (e: GatewayEvent) => void): () => void {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  }
  sendRequest(bytes: Uint8Array): boolean {
    if (this.status !== 'ready') return false;
    const frame = decodeFrame(bytes);
    expect(frame.op).toBe(Op.request);
    const [id, method, route, body, idem] = frame.payload as [bigint, string, string, Uint8Array, Uint8Array | null];
    this.sent.push({ id: Number(id), method, route, body, idem });
    return true;
  }
  setStatus(status: GatewayStatus, closeCode: number | null = null): void {
    this.status = status;
    this.emit({ type: 'status', status, closeCode });
  }
  respond(id: number, status: number, body: Uint8Array = new Uint8Array()): void {
    this.emit({ type: 'response', response: { id, status, body } });
  }
  emit(e: GatewayEvent): void {
    for (const listener of [...this.listeners]) listener(e);
  }
}

const ROUTES: readonly SocketRoute[] = [
  { method: 'POST', pattern: '/v1/test/{id}' },
  { method: 'GET', pattern: '/v1/test/{id}' },
];
const idem = (n: number): Uint8Array => new Uint8Array(16).fill(n);
const post = (path: string, key: Uint8Array | undefined, body: CborInput = []): HttpRequest =>
  ({ method: 'POST', path, body: encode(body), bucket: 'none', idempotent: false, ...(key === undefined ? {} : { idem: key }) });
const get = (path: string): HttpRequest => ({ method: 'GET', path, bucket: 'none', idempotent: true });
const refusal = (code: string, retryMs: number | null = null): Uint8Array => encode([code, '', retryMs]);
const flush = async (): Promise<void> => { for (let i = 0; i < 5; i++) await Promise.resolve(); };

function setup(opts: { status?: GatewayStatus } = {}) {
  const gw = new FakeGateway();
  if (opts.status !== undefined) gw.status = opts.status;
  let now = 0;
  const rtt = new RouteRtt();
  const sr = new SocketRequests(gw, { now: () => now, random: () => 0, rtt, routes: ROUTES });
  return { gw, sr, rtt, advance: (ms: number) => { now += ms; } };
}

function httpClient(sr: SocketRequests | null, fetchFn: typeof fetch): HttpClient {
  const deps: HttpDeps = {
    baseUrl: 'https://dilla.test', fetch: fetchFn, now: () => 0, sleep: () => Promise.resolve(), random: () => 0,
    token: () => 'tok', reauthenticate: () => Promise.resolve(false), onGeneration: () => undefined,
    socket: () => sr, socketRoutes: ROUTES,
  };
  return new HttpClient(deps);
}

beforeEach(() => { vi.useFakeTimers(); });
afterEach(() => { vi.useRealTimers(); });

describe('SocketRequests', () => {
  it('a socket route never touches fetch while the socket is up', async () => {
    const { gw, sr } = setup();
    const fetchFn = vi.fn<typeof fetch>();
    const answer = httpClient(sr, fetchFn).request(post('/v1/test/ab', idem(1), [7]));
    await flush();
    expect(gw.sent.map((s) => [s.method, s.route, s.idem])).toEqual([['POST', '/v1/test/ab', idem(1)]]);
    expect(decode(gw.sent[0].body)).toEqual([7n]);
    gw.respond(gw.sent[0].id, 200, encode([1]));
    await expect(answer).resolves.toEqual({ status: 200, body: encode([1]) });
    expect(fetchFn).not.toHaveBeenCalled();
    expect(sr.up).toBe(true);
  });

  it('an HTTP-only route uses fetch', async () => {
    const { gw, sr } = setup();
    const fetchFn = vi.fn<typeof fetch>(() => Promise.resolve(new Response(encode([2]).slice(), { status: 200 })));
    const answer = await httpClient(sr, fetchFn).request(get('/v1/instance'));
    expect(answer).toEqual({ status: 200, body: encode([2]) });
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(fetchFn.mock.calls[0][0]).toBe('https://dilla.test/v1/instance');
    expect(gw.sent).toEqual([]);
  });

  it('requests queue while the socket is down and drain in submission order', async () => {
    const { gw, sr } = setup({ status: 'waiting' });
    expect(sr.up).toBe(false);
    const a = sr.request(post('/v1/test/a', idem(1)));
    const b = sr.request(get('/v1/test/b'));
    const c = sr.request(post('/v1/test/c', idem(3)));
    await flush();
    expect(gw.sent).toEqual([]);
    gw.setStatus('connecting');
    expect(gw.sent).toEqual([]);
    gw.setStatus('ready');
    expect(gw.sent.map((s) => s.route)).toEqual(['/v1/test/a', '/v1/test/b', '/v1/test/c']);
    expect(gw.sent[1].idem).toBeNull();
    for (const s of [...gw.sent].reverse()) gw.respond(s.id, 204);
    await expect(Promise.all([a, b, c])).resolves.toEqual([{ status: 204, body: new Uint8Array() },
      { status: 204, body: new Uint8Array() }, { status: 204, body: new Uint8Array() }]);
  });

  it('a mutation in flight across a reconnect is applied once', async () => {
    const { gw, sr } = setup();
    // The fake instance applies a request once per idem and answers a repeat from its record (the replay record).
    let effects = 0;
    const record = new Map<string, Uint8Array>();
    const serve = (s: Sent): Uint8Array => {
      const key = Array.from(s.idem ?? []).join(',');
      const hit = record.get(key);
      if (hit !== undefined) return hit;
      effects += 1;
      const answer = encode([effects]);
      record.set(key, answer);
      return answer;
    };
    let resolved = 0;
    const answer = sr.request(post('/v1/test/m', idem(9))).then((r) => { resolved += 1; return r; });
    expect(gw.sent).toHaveLength(1);
    serve(gw.sent[0]);                          // applied, but the answer is lost with the socket
    gw.setStatus('waiting', 1006);
    gw.setStatus('connecting');
    gw.setStatus('ready');
    expect(gw.sent).toHaveLength(2);
    expect(gw.sent[1].idem).toEqual(idem(9));
    expect(gw.sent[1].id).not.toBe(gw.sent[0].id);
    gw.respond(gw.sent[0].id, 200, encode([99]));  // a stale id from the old socket is ignored
    gw.respond(gw.sent[1].id, 200, serve(gw.sent[1]));
    await expect(answer).resolves.toEqual({ status: 200, body: encode([1]) });
    await flush();
    expect(effects).toBe(1);
    expect(resolved).toBe(1);
  });

  it('E_BUSY re-queues at the head and does not reject', async () => {
    const { gw, sr } = setup();
    let rejected = false;
    const a = sr.request(post('/v1/test/a', idem(1))).catch((e: unknown) => { rejected = true; throw e; });
    const b = sr.request(post('/v1/test/b', idem(2)));
    expect(gw.sent.map((s) => s.route)).toEqual(['/v1/test/a', '/v1/test/b']);
    gw.respond(gw.sent[0].id, 429, refusal('E_BUSY'));
    const c = sr.request(post('/v1/test/c', idem(3)));
    // Nothing more is sent until the instance has room again: here, the next answer.
    expect(gw.sent).toHaveLength(2);
    gw.respond(gw.sent[1].id, 204);
    expect(gw.sent.map((s) => s.route)).toEqual(['/v1/test/a', '/v1/test/b', '/v1/test/a', '/v1/test/c']);
    expect(gw.sent[2].idem).toEqual(idem(1));
    gw.respond(gw.sent[2].id, 204);
    gw.respond(gw.sent[3].id, 204);
    await expect(Promise.all([a, b, c])).resolves.toHaveLength(3);
    expect(rejected).toBe(false);
  });

  it('E_BUSY with nothing else in flight retries after a short pause', async () => {
    const { gw, sr } = setup();
    const a = sr.request(post('/v1/test/a', idem(1)));
    gw.respond(gw.sent[0].id, 429, refusal('E_BUSY'));
    expect(gw.sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(250);
    expect(gw.sent).toHaveLength(2);
    expect(gw.sent[1].idem).toEqual(idem(1));
    gw.respond(gw.sent[1].id, 204);
    await expect(a).resolves.toEqual({ status: 204, body: new Uint8Array() });
  });

  it('a mutation without idem throws E_IDEMPOTENCY_REQUIRED', async () => {
    const { gw, sr } = setup();
    await expect(sr.request(post('/v1/test/a', undefined))).rejects.toThrow('E_IDEMPOTENCY_REQUIRED');
    await expect(sr.request({ method: 'DELETE', path: '/v1/test/a', bucket: 'none', idempotent: true }))
      .rejects.toThrow('E_IDEMPOTENCY_REQUIRED');
    expect(gw.sent).toEqual([]);
  });

  it('503 E_UNAVAILABLE retries with the same idem after a backoff', async () => {
    const { gw, sr } = setup();
    const a = sr.request(post('/v1/test/a', idem(4)));
    gw.respond(gw.sent[0].id, 503, refusal('E_UNAVAILABLE', 1000));
    expect(gw.sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(999);
    expect(gw.sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(gw.sent).toHaveLength(2);
    expect(gw.sent[1].idem).toEqual(idem(4));
    gw.respond(gw.sent[1].id, 200, encode([1]));
    await expect(a).resolves.toEqual({ status: 200, body: encode([1]) });
  });

  it('surfaces 409, 401, 426 and a route 503 to the caller without retrying', async () => {
    const { gw, sr } = setup();
    const answers = [409, 401, 426, 503].map((status, i) => {
      const p = sr.request(post('/v1/test/x', idem(10 + i)));
      gw.respond(gw.sent[i].id, status, refusal(status === 503 ? 'E_BOT_OFFLINE' : 'E_CONFLICT'));
      return p;
    });
    expect((await Promise.all(answers)).map((r) => r.status)).toEqual([409, 401, 426, 503]);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(gw.sent).toHaveLength(4);
  });

  it('through HttpClient a 409 rejects as a refusal and is not resent', async () => {
    const { gw, sr } = setup();
    const answer = httpClient(sr, vi.fn<typeof fetch>()).request(post('/v1/test/x', idem(5)));
    await flush();
    gw.respond(gw.sent[0].id, 409, refusal('E_INVALID_REQUEST'));
    await expect(answer).rejects.toMatchObject({ status: 409, code: 'E_INVALID_REQUEST' });
    expect(gw.sent).toHaveLength(1);
  });

  it('a GET without idem lost in a disconnect is sent again', async () => {
    const { gw, sr } = setup();
    const a = sr.request(get('/v1/test/g'));
    gw.setStatus('waiting', 1006);
    gw.setStatus('ready');
    expect(gw.sent.map((s) => [s.route, s.idem])).toEqual([['/v1/test/g', null], ['/v1/test/g', null]]);
    gw.respond(gw.sent[1].id, 200, encode([3]));
    await expect(a).resolves.toEqual({ status: 200, body: encode([3]) });
  });

  it('rejects what is queued when the instance asks for an update or revokes the session', async () => {
    const { gw, sr } = setup({ status: 'waiting' });
    const a = sr.request(post('/v1/test/a', idem(1)));
    gw.emit({ type: 'upgradeRequired' });
    await expect(a).rejects.toMatchObject({ status: 426, code: 'E_UPGRADE_REQUIRED' });
    const b = sr.request(post('/v1/test/b', idem(2)));
    gw.emit({ type: 'revoked' });
    await expect(b).rejects.toBeInstanceOf(DillaHttpError);
    gw.setStatus('ready');
    expect(gw.sent).toEqual([]);
  });

  it('a caller abort removes a queued request', async () => {
    const { gw, sr } = setup({ status: 'waiting' });
    const controller = new AbortController();
    const a = sr.request({ ...post('/v1/test/a', idem(1)), signal: controller.signal });
    controller.abort();
    await expect(a).rejects.toThrow(/abort/i);
    gw.setStatus('ready');
    expect(gw.sent).toEqual([]);
  });

  it('records each answered request under its route pattern', async () => {
    const { gw, sr, rtt, advance } = setup();
    const a = sr.request(post('/v1/test/aa', idem(1)));
    advance(40);
    gw.respond(gw.sent[0].id, 204);
    await a;
    expect(rtt.slowest(5)).toEqual([{ pattern: 'POST /v1/test/{id}', p50: 40, p95: 40, count: 1 }]);
  });
});

describe('RouteRtt', () => {
  it('RouteRtt reports p95 by pattern, slowest first', () => {
    const rtt = new RouteRtt();
    for (let ms = 1; ms <= 100; ms++) rtt.record('GET /v1/a', ms);
    for (let i = 0; i < 20; i++) rtt.record('POST /v1/b', i < 18 ? 10 : 500);
    rtt.record('GET /v1/c', 300);
    expect(rtt.slowest(2)).toEqual([
      { pattern: 'POST /v1/b', p50: 10, p95: 500, count: 20 },
      { pattern: 'GET /v1/c', p50: 300, p95: 300, count: 1 },
    ]);
    expect(rtt.slowest(5).map((r) => r.pattern)).toEqual(['POST /v1/b', 'GET /v1/c', 'GET /v1/a']);
    expect(rtt.slowest(5)[2]).toEqual({ pattern: 'GET /v1/a', p50: 50, p95: 95, count: 100 });
  });
});
