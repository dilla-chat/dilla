import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { decode, encode, type CborInput } from '../cbor';
import { HttpClient, type HttpDeps, type HttpRequest, type HttpResponse } from '../http/client';
import { DillaHttpError } from '../http/errors';
import { RouteRtt } from '../http/route-rtt';
import { socketRouteFor, type SocketRoute } from '../http/socket-routes';
import { decodeFrame, Op } from './frames';
import { CLIENT_CLOSE, type GatewayEvent, type GatewayStatus } from './gateway';
import { SOCKET_ATTEMPT_TIMEOUT_MS, SocketRequests } from './requests';

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
/** What HttpClient does: the request with the pattern it matched in the socket table. */
const send = (sr: SocketRequests, r: HttpRequest): Promise<HttpResponse> =>
  sr.request(r, socketRouteFor(r.method, r.path, ROUTES) ?? 'unmatched');
const flush = async (): Promise<void> => { for (let i = 0; i < 5; i++) await Promise.resolve(); };

function setup(opts: { status?: GatewayStatus } = {}) {
  const gw = new FakeGateway();
  if (opts.status !== undefined) gw.status = opts.status;
  let now = 0;
  const rtt = new RouteRtt();
  const sr = new SocketRequests(gw, { now: () => now, random: () => 0, rtt });
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

  it('requests queue while the socket reconnects and are written in submission order', async () => {
    const { gw, sr } = setup({ status: 'waiting' });
    expect(sr.up).toBe(false);
    const a = send(sr, post('/v1/test/a', idem(1)));
    const b = send(sr, get('/v1/test/b'));
    const c = send(sr, post('/v1/test/c', idem(3)));
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
    const answer = send(sr, post('/v1/test/m', idem(9))).then((r) => { resolved += 1; return r; });
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
    const a = send(sr, post('/v1/test/a', idem(1))).catch((e: unknown) => { rejected = true; throw e; });
    const b = send(sr, post('/v1/test/b', idem(2)));
    expect(gw.sent.map((s) => s.route)).toEqual(['/v1/test/a', '/v1/test/b']);
    gw.respond(gw.sent[0].id, 429, refusal('E_BUSY'));
    const c = send(sr, post('/v1/test/c', idem(3)));
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
    const a = send(sr, post('/v1/test/a', idem(1)));
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
    await expect(send(sr, post('/v1/test/a', undefined))).rejects.toMatchObject({ status: 0, code: 'E_IDEMPOTENCY_REQUIRED' });
    await expect(send(sr, post('/v1/test/a', undefined))).rejects.toBeInstanceOf(DillaHttpError);
    await expect(send(sr, { method: 'PUT', path: '/v1/test/a', body: new Uint8Array(16 * 1024 * 1024 + 1), bucket: 'none',
      idempotent: true, idem: idem(1) })).rejects.toMatchObject({ status: 0, code: 'E_FRAME_LIMIT' });
    await expect(send(sr, { method: 'DELETE', path: '/v1/test/a', bucket: 'none', idempotent: true }))
      .rejects.toThrow('E_IDEMPOTENCY_REQUIRED');
    expect(gw.sent).toEqual([]);
  });

  it('503 E_UNAVAILABLE retries with the same idem after a backoff', async () => {
    const { gw, sr } = setup();
    const a = send(sr, post('/v1/test/a', idem(4)));
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
      const p = send(sr, post('/v1/test/x', idem(10 + i)));
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
    const a = send(sr, get('/v1/test/g'));
    gw.setStatus('waiting', 1006);
    gw.setStatus('ready');
    expect(gw.sent.map((s) => [s.route, s.idem])).toEqual([['/v1/test/g', null], ['/v1/test/g', null]]);
    gw.respond(gw.sent[1].id, 200, encode([3]));
    await expect(a).resolves.toEqual({ status: 200, body: encode([3]) });
  });

  it('rejects what is queued when the instance asks for an update or revokes the session', async () => {
    const { gw, sr } = setup({ status: 'waiting' });
    const a = send(sr, post('/v1/test/a', idem(1)));
    gw.setStatus('idle', CLIENT_CLOSE.version);
    gw.emit({ type: 'upgradeRequired' });
    await expect(a).rejects.toMatchObject({ status: 426, code: 'E_UPGRADE_REQUIRED' });
    gw.setStatus('connecting');
    const b = send(sr, post('/v1/test/b', idem(2)));
    gw.setStatus('idle', 4004);
    gw.emit({ type: 'revoked' });
    await expect(b).rejects.toMatchObject({ status: 401, code: 'E_UNAUTHENTICATED' });
    expect(gw.sent).toEqual([]);
  });

  it('revoke rejects a written mutation as outcome-unknown with the 401 cause, a never-written one with 401', async () => {
    const { gw, sr } = setup();
    const written = send(sr, post('/v1/test/w', idem(1)));
    const read = send(sr, get('/v1/test/g'));
    gw.setStatus('waiting', 1006);
    const queued = send(sr, post('/v1/test/q', idem(2)));
    gw.setStatus('idle', 4004);
    gw.emit({ type: 'revoked' });
    await expect(written).rejects.toMatchObject({ status: 0, code: 'E_OUTCOME_UNKNOWN', cause: { status: 401, code: 'E_UNAUTHENTICATED' } });
    await expect(read).rejects.toMatchObject({ status: 401, code: 'E_UNAUTHENTICATED' });
    await expect(queued).rejects.toMatchObject({ status: 401, code: 'E_UNAUTHENTICATED' });
  });

  it('an upgrade refusal rejects a written mutation as outcome-unknown with the 426 cause, a never-written one with 426', async () => {
    const { gw, sr } = setup();
    const written = send(sr, post('/v1/test/w', idem(1)));
    gw.setStatus('waiting', 1006);
    const queued = send(sr, post('/v1/test/q', idem(2)));
    gw.setStatus('idle', CLIENT_CLOSE.version);
    gw.emit({ type: 'upgradeRequired' });
    await expect(written).rejects.toMatchObject({ status: 0, code: 'E_OUTCOME_UNKNOWN', cause: { status: 426, code: 'E_UPGRADE_REQUIRED' } });
    await expect(queued).rejects.toMatchObject({ status: 426, code: 'E_UPGRADE_REQUIRED' });
  });

  it('a new request after revoke is rejected at once', async () => {
    const { gw, sr } = setup();
    gw.setStatus('idle', 4004);
    gw.emit({ type: 'revoked' });
    await expect(send(sr, post('/v1/test/a', idem(1)))).rejects.toMatchObject({ status: 401, code: 'E_UNAUTHENTICATED' });
    await expect(send(sr, get('/v1/test/g'))).rejects.toMatchObject({ code: 'E_UNAUTHENTICATED' });
    expect(gw.sent).toEqual([]);
  });

  it('stop with a request queued rejects it E_NETWORK, never leaving it waiting', async () => {
    const { gw, sr } = setup({ status: 'waiting' });
    const a = send(sr, post('/v1/test/a', idem(1)));
    gw.setStatus('idle');
    await expect(a).rejects.toMatchObject({ status: 0, code: 'E_NETWORK' });
    await expect(send(sr, get('/v1/test/g'))).rejects.toMatchObject({ status: 0, code: 'E_NETWORK' });
  });

  it('stop with a request in flight rejects a mutation E_OUTCOME_UNKNOWN and a read E_NETWORK', async () => {
    const { gw, sr } = setup();
    const m = send(sr, post('/v1/test/m', idem(1)));
    const g = send(sr, get('/v1/test/g'));
    // A mutation written before, lost in a reconnect and queued again, is just as unknown.
    gw.setStatus('waiting', 1006);
    gw.setStatus('ready');
    const n = send(sr, post('/v1/test/n', idem(2)));
    expect(gw.sent).toHaveLength(5);
    gw.setStatus('idle');
    await expect(m).rejects.toMatchObject({ status: 0, code: 'E_OUTCOME_UNKNOWN' });
    await expect(n).rejects.toMatchObject({ status: 0, code: 'E_OUTCOME_UNKNOWN' });
    await expect(g).rejects.toMatchObject({ status: 0, code: 'E_NETWORK' });
    gw.respond(gw.sent[4].id, 204);                       // a late answer for a rejected request is dropped
  });

  it('a new request after stop then start succeeds', async () => {
    const { gw, sr } = setup();
    gw.setStatus('idle');
    await expect(send(sr, get('/v1/test/g'))).rejects.toMatchObject({ code: 'E_NETWORK' });
    gw.setStatus('connecting');
    const a = send(sr, post('/v1/test/a', idem(1)));
    expect(gw.sent).toEqual([]);
    gw.setStatus('ready');
    gw.respond(gw.sent[0].id, 204);
    await expect(a).resolves.toEqual({ status: 204, body: new Uint8Array() });
  });

  it('close rejects never-written requests E_NETWORK and a written mutation E_OUTCOME_UNKNOWN', async () => {
    const { gw, sr } = setup();
    const m = send(sr, post('/v1/test/m', idem(1)));
    gw.setStatus('waiting', 1006);
    const q = send(sr, post('/v1/test/q', idem(2)));
    sr.close();
    await expect(m).rejects.toMatchObject({ code: 'E_OUTCOME_UNKNOWN' });
    await expect(q).rejects.toMatchObject({ code: 'E_NETWORK' });
    await expect(send(sr, get('/v1/test/g'))).rejects.toMatchObject({ code: 'E_NETWORK' });
  });

  it('an attempt past its deadline is written again with the same idem and its late answer is dropped', async () => {
    const { gw, sr } = setup();
    const a = send(sr, post('/v1/test/a', idem(7)));
    // The default deadline sits just above the instance's 60 s dispatch deadline: a slow mutation still running there
    // is not written again.
    expect(SOCKET_ATTEMPT_TIMEOUT_MS).toBe(65_000);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(gw.sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(gw.sent).toHaveLength(2);
    expect(gw.sent[1].idem).toEqual(idem(7));
    gw.respond(gw.sent[0].id, 200, encode(['late']));
    gw.respond(gw.sent[1].id, 200, encode(['fresh']));
    await expect(a).resolves.toEqual({ status: 200, body: encode(['fresh']) });
  });

  it('after the last timed-out attempt a mutation is E_OUTCOME_UNKNOWN and a read E_NETWORK', async () => {
    const { gw, sr } = setup();
    const m = send(sr, post('/v1/test/m', idem(7)));
    const g = send(sr, get('/v1/test/g'));
    const caught = Promise.allSettled([m, g]);
    await vi.advanceTimersByTimeAsync(5 * 65_000);
    expect(gw.sent).toHaveLength(10);
    const [mr, gr] = await caught;
    expect(mr).toMatchObject({ status: 'rejected', reason: { code: 'E_OUTCOME_UNKNOWN' } });
    expect(gr).toMatchObject({ status: 'rejected', reason: { code: 'E_NETWORK' } });
  });

  it('a caller abort removes a queued request', async () => {
    const { gw, sr } = setup({ status: 'waiting' });
    const controller = new AbortController();
    const a = send(sr, { ...post('/v1/test/a', idem(1)), signal: controller.signal });
    controller.abort();
    await expect(a).rejects.toThrow(/abort/i);
    gw.setStatus('ready');
    expect(gw.sent).toEqual([]);
  });

  it('records each answered request under its route pattern', async () => {
    const { gw, sr, rtt, advance } = setup();
    const a = send(sr, post('/v1/test/aa', idem(1)));
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
