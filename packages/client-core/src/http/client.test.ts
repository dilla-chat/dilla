import { describe, expect, it } from 'vitest';
import { encode } from '../cbor';
import { HTTP_ATTEMPT_TIMEOUT_MS, HttpClient, RETRY, type HttpRequest } from './client';
import { DillaHttpError } from './errors';
import { cborReply, fakeTransport, noContent, refusal, textReply } from './fake-fetch';

const GET_READ: HttpRequest = { method: 'GET', path: '/v1/thing', bucket: 'read', idempotent: true };
const POST_WRITE: HttpRequest = { method: 'POST', path: '/v1/thing', body: encode(['x']), bucket: 'write', idempotent: false };
const REF = new Uint8Array(32).fill(0x11);
const WIN = new Uint8Array(48).fill(0x12);

it('caps a non-2xx body independently of the successful page cap', async () => {
  const t = fakeTransport();
  t.replies.push(new Response(new Uint8Array(65_537), { status: 403 }));
  const err = await failure(new HttpClient(t.deps).request({
    ...GET_READ, maxBodyBytes: 1_048_576,
  }));
  expect(err).toMatchObject({ code: 'E_BODY_TOO_LARGE', status: 403 });
  expect(t.seen).toHaveLength(1);
});

async function failure(p: Promise<unknown>): Promise<DillaHttpError> {
  try {
    await p;
  } catch (e) {
    if (e instanceof DillaHttpError) return e;
    throw e;
  }
  throw new Error('expected a DillaHttpError');
}

describe('request shape', () => {
  it('sends method, url, bearer token and body, and returns the body as received', async () => {
    const t = fakeTransport();
    t.replies.push(cborReply(201, ['ok', 1]));
    const res = await new HttpClient(t.deps).request(POST_WRITE);
    expect(res.status).toBe(201);
    expect(res.body).toEqual(encode(['ok', 1]));
    expect(t.seen).toHaveLength(1);
    const s = t.seen[0];
    expect(s.url).toBe('https://dilla.test/v1/thing');
    expect(s.method).toBe('POST');
    expect(s.headers.get('authorization')).toBe('Bearer tok-1');
    expect(s.headers.get('content-type')).toBe('application/cbor');
    expect(s.headers.get('accept')).toBeNull();
    expect(s.body).toEqual(encode(['x']));
    expect(s.init.credentials).toBe('omit');
    expect(s.init.cache).toBe('no-store');
    expect(s.init.redirect).toBe('error');
    expect(t.generations).toEqual([7n]);
  });

  it('drops one trailing slash of the base url', async () => {
    const t = fakeTransport();
    t.replies.push(cborReply(200, []));
    await new HttpClient({ ...t.deps, baseUrl: 'https://dilla.test/' }).request(GET_READ);
    expect(t.seen[0].url).toBe('https://dilla.test/v1/thing');
  });

  it('omits Authorization without auth, Content-Type without a body, and sets Accept when asked', async () => {
    const t = fakeTransport();
    t.replies.push(cborReply(200, []));
    await new HttpClient(t.deps).request({
      method: 'GET', path: '/i/CODE', bucket: 'none', idempotent: true, auth: false, accept: 'application/cbor',
    });
    const s = t.seen[0];
    expect(s.headers.get('authorization')).toBeNull();
    expect(s.headers.get('content-type')).toBeNull();
    expect(s.headers.get('accept')).toBe('application/cbor');
    expect(s.body).toBeNull();
  });

  it('sends no Authorization header while there is no token', async () => {
    const t = fakeTransport({ token: null });
    t.replies.push(cborReply(200, []));
    await new HttpClient(t.deps).request(GET_READ);
    expect(t.seen[0].headers.get('authorization')).toBeNull();
  });

  it('resolves a 204 with an empty body and reports its generation', async () => {
    const t = fakeTransport();
    t.replies.push(noContent({ 'X-Dilla-Generation': '18446744073709551615' }));
    const res = await new HttpClient(t.deps).request(GET_READ);
    expect(res).toEqual({ status: 204, body: new Uint8Array(0) });
    expect(t.generations).toEqual([18446744073709551615n]);
  });

  it('ignores a malformed or oversized X-Dilla-Generation', async () => {
    const t = fakeTransport();
    t.replies.push(cborReply(200, [], { 'X-Dilla-Generation': 'abc' }), cborReply(200, [], { 'X-Dilla-Generation': '18446744073709551616' }));
    const client = new HttpClient(t.deps);
    await client.request(GET_READ);
    await client.request(GET_READ);
    expect(t.generations).toEqual([]);
  });

  it('resolves any status listed in ok', async () => {
    const t = fakeTransport();
    t.replies.push(textReply(404, '404 page not found\n'));
    const res = await new HttpClient(t.deps).request({ ...GET_READ, ok: [200, 404] });
    expect(res.status).toBe(404);
    expect(new TextDecoder().decode(res.body)).toBe('404 page not found\n');
    expect(t.generations).toEqual([7n]);
  });
});

describe('refusals', () => {
  it('throws the decoded error array of a 409 at once, extras included', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(409, 'E_COMMIT_CONFLICT', 'another commit won this epoch', null, [WIN, [REF]]));
    const err = await failure(new HttpClient(t.deps).request({
      // An HTTP route: a socket route's answer carries no X-Dilla-Generation header.
      method: 'POST', path: '/v1/keypackages', body: encode([1]), bucket: 'write', idempotent: false,
    }));
    expect(err.status).toBe(409);
    expect(err.code).toBe('E_COMMIT_CONFLICT');
    expect(err.detail).toBe('another commit won this epoch');
    expect(err.retryAfterMs).toBeNull();
    expect(err.extra).toEqual([WIN, [REF]]);
    expect(t.seen).toHaveLength(1);
    expect(t.sleeps).toEqual([]);
    expect(t.generations).toEqual([7n]);
  });

  it('does not retry a 425 here', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(425, 'E_COMMIT_REQUIRED', 'outstanding proposals must be committed first', 1500, [[REF]]));
    const err = await failure(new HttpClient(t.deps).request({ ...POST_WRITE, bucket: 'message' }));
    expect(err.code).toBe('E_COMMIT_REQUIRED');
    expect(err.retryAfterMs).toBe(1500);
    expect(t.seen).toHaveLength(1);
    expect(t.sleeps).toEqual([]);
  });

  it('reads a text/plain body as E_HTTP', async () => {
    const t = fakeTransport();
    t.replies.push(textReply(404, '404 page not found\n'));
    const err = await failure(new HttpClient(t.deps).request(GET_READ));
    expect(err).toMatchObject({ status: 404, code: 'E_HTTP', detail: '', retryAfterMs: null, extra: [] });
  });

  it('throws a 502 and a 501 at once, even for an idempotent GET', async () => {
    const t = fakeTransport();
    t.replies.push(cborReply(502, [1, 2, 3]), refusal(501, 'E_INTERNAL'));
    const client = new HttpClient(t.deps);
    expect(await failure(client.request(GET_READ))).toMatchObject({ status: 502, code: 'E_HTTP' });
    expect(await failure(client.request(GET_READ))).toMatchObject({ status: 501, code: 'E_INTERNAL' });
    expect(t.seen).toHaveLength(2);
    expect(t.sleeps).toEqual([]);
  });
});

describe('401', () => {
  it('re-authenticates once and retries with the new token', async () => {
    const t = fakeTransport({ reauth: () => ({ ok: true, token: 'tok-2' }) });
    t.replies.push(refusal(401, 'E_UNAUTHENTICATED'), cborReply(200, ['fine']));
    const res = await new HttpClient(t.deps).request(GET_READ);
    expect(res.status).toBe(200);
    expect(t.reauthCalls).toBe(1);
    expect(t.seen.map((s) => s.headers.get('authorization'))).toEqual(['Bearer tok-1', 'Bearer tok-2']);
  });

  it('throws the 401 when re-authentication fails', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(401, 'E_UNAUTHENTICATED'));
    const err = await failure(new HttpClient(t.deps).request(GET_READ));
    expect(err).toMatchObject({ status: 401, code: 'E_UNAUTHENTICATED' });
    expect(t.reauthCalls).toBe(1);
    expect(t.seen).toHaveLength(1);
  });

  it('re-authenticates at most once per request', async () => {
    const t = fakeTransport({ reauth: () => ({ ok: true, token: 'tok-2' }) });
    t.replies.push(refusal(401, 'E_UNAUTHENTICATED'), refusal(401, 'E_UNAUTHENTICATED'));
    const err = await failure(new HttpClient(t.deps).request(GET_READ));
    expect(err.code).toBe('E_UNAUTHENTICATED');
    expect(t.reauthCalls).toBe(1);
    expect(t.seen).toHaveLength(2);
  });

  it('never re-authenticates a request sent without auth', async () => {
    const t = fakeTransport({ reauth: () => ({ ok: true, token: 'tok-2' }) });
    t.replies.push(refusal(401, 'E_UNAUTHENTICATED'), cborReply(201, ['token']));
    const outcome = await new HttpClient(t.deps)
      .request({ method: 'POST', path: '/v1/devices/x/sessions', body: encode([1]), bucket: 'none', idempotent: false, auth: false })
      .then(() => 'resolved', (e: unknown) => e);
    expect(t.reauthCalls).toBe(0);
    expect(t.seen).toHaveLength(1);
    expect(outcome).toBeInstanceOf(DillaHttpError);
    expect(outcome).toMatchObject({ status: 401, code: 'E_UNAUTHENTICATED' });
  });
});

describe('429 and 503', () => {
  it('waits retry_after_ms from the body of a 503, then retries', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(503, 'E_UNAVAILABLE', 'busy', 2500), cborReply(200, []));
    const res = await new HttpClient(t.deps).request(GET_READ);
    expect(res.status).toBe(200);
    expect(t.sleeps).toEqual([2500]);
    expect(t.seen).toHaveLength(2);
  });

  it('retries a rate-limited POST, which the server did not store, with the body delay', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(429, 'E_RATE_LIMITED', 'rate limited', 1200, [], { 'Retry-After': '2' }), cborReply(200, []));
    await new HttpClient(t.deps).request(POST_WRITE);
    expect(t.sleeps).toEqual([1200]);
    expect(t.seen).toHaveLength(2);
  });

  it('falls back to Retry-After seconds, then to one second', async () => {
    const t = fakeTransport();
    t.replies.push(textReply(429, 'slow down', { 'Retry-After': '3' }), textReply(503, 'busy'), cborReply(200, []));
    await new HttpClient(t.deps).request(GET_READ);
    expect(t.sleeps).toEqual([3000, 1000]);
  });

  it('throws instead of waiting longer than maxWaitMs', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(429, 'E_RATE_LIMITED', 'rate limited', RETRY.maxWaitMs + 1));
    const err = await failure(new HttpClient(t.deps).request(GET_READ));
    expect(err.retryAfterMs).toBe(60001);
    expect(t.sleeps).toEqual([]);
    expect(t.seen).toHaveLength(1);
  });

  it('gives up after maxAttempts', async () => {
    const t = fakeTransport();
    for (let i = 0; i < 6; i++) t.replies.push(refusal(503, 'E_UNAVAILABLE', 'busy', 100));
    const err = await failure(new HttpClient(t.deps).request(GET_READ));
    expect(err.code).toBe('E_UNAVAILABLE');
    expect(t.seen).toHaveLength(5);
    expect(t.sleeps).toEqual([100, 100, 100, 100]);
  });
});

describe('500 and network failures', () => {
  it('has the fixed retry table', () => {
    expect(RETRY).toEqual({ maxAttempts: 5, baseMs: 500, capMs: 8000, jitterMs: 250, maxWaitMs: 60000 });
  });

  it('retries an idempotent request after a 500 with exponential back-off', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(500, 'E_INTERNAL'), refusal(500, 'E_INTERNAL'), cborReply(200, []));
    await new HttpClient(t.deps).request(GET_READ);
    expect(t.sleeps).toEqual([625, 1125]);
  });

  it('does not retry a non-idempotent request after a 500', async () => {
    const t = fakeTransport();
    t.replies.push(refusal(500, 'E_INTERNAL'), cborReply(200, []));
    const err = await failure(new HttpClient(t.deps).request(POST_WRITE));
    expect(err.code).toBe('E_INTERNAL');
    expect(t.seen).toHaveLength(1);
    expect(t.sleeps).toEqual([]);
  });

  it('retries an idempotent request after network failures up to maxAttempts', async () => {
    const t = fakeTransport();
    for (let i = 0; i < 5; i++) t.replies.push(new TypeError('fetch failed'));
    const err = await failure(new HttpClient(t.deps).request(GET_READ));
    expect(err).toMatchObject({ status: 0, code: 'E_NETWORK', detail: 'fetch failed', retryAfterMs: null, extra: [] });
    expect(t.seen).toHaveLength(5);
    expect(t.sleeps).toEqual([625, 1125, 2125, 4125]);
    expect(t.generations).toEqual([]);
  });

  it('does not retry a non-idempotent request after a network failure', async () => {
    const t = fakeTransport();
    t.replies.push(new TypeError('fetch failed'), cborReply(200, []));
    const err = await failure(new HttpClient(t.deps).request(POST_WRITE));
    expect(err.code).toBe('E_NETWORK');
    expect(t.seen).toHaveLength(1);
  });

  it('never puts the token or the body into an error', async () => {
    const t = fakeTransport({ token: 'secret-token-value' });
    t.replies.push(refusal(400, 'E_INVALID_REQUEST', 'bad body'));
    const err = await failure(new HttpClient(t.deps).request(POST_WRITE));
    expect(err.message).not.toContain('secret-token-value');
    expect(err.detail).toBe('bad body');
  });
});

describe('abort', () => {
  it('rejects with AbortError before sending when the signal is already aborted', async () => {
    const t = fakeTransport();
    const aborted = new AbortController();
    aborted.abort();
    await expect(new HttpClient(t.deps).request({ ...GET_READ, signal: aborted.signal }))
      .rejects.toMatchObject({ name: 'AbortError' });
    expect(t.seen).toHaveLength(0);
  });

  it('reports a fetch aborted in flight as AbortError and does not retry', async () => {
    const t = fakeTransport();
    const controller = new AbortController();
    t.replies.push(() => {
      controller.abort();
      return new DOMException('The operation was aborted.', 'AbortError');
    });
    await expect(new HttpClient(t.deps).request({ ...GET_READ, signal: controller.signal }))
      .rejects.toMatchObject({ name: 'AbortError' });
    expect(t.seen).toHaveLength(1);
    expect(t.sleeps).toEqual([]);
  });
});

// WB I1: a request whose answer never comes no longer holds its caller (a create dialog left busy until a reload).
describe('per-attempt timeout', () => {
  /** A fetch that never answers, and rejects only when its signal aborts, as a browser's stalled fetch does. */
  const hang = (t: ReturnType<typeof fakeTransport>): typeof fetch => (input, init) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    t.seen.push({ url, method: init?.method ?? 'GET', headers: new Headers(init?.headers), body: null, init: init ?? {} });
    return new Promise((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => { reject(init.signal?.reason as Error); });
    });
  };

  it('a stalled attempt ends as E_NETWORK status 0, and a write is not sent again', async () => {
    const t = fakeTransport();
    const client = new HttpClient({ ...t.deps, fetch: hang(t), attemptTimeoutMs: 20 });
    const err = await failure(client.request(POST_WRITE));
    expect(err).toMatchObject({ code: 'E_NETWORK', status: 0 });
    expect(t.seen).toHaveLength(1);
  });

  it('defaults to 30 s, and the caller\'s own abort still answers AbortError', async () => {
    expect(HTTP_ATTEMPT_TIMEOUT_MS).toBe(30_000);
    const t = fakeTransport();
    const controller = new AbortController();
    const pending = new HttpClient({ ...t.deps, fetch: hang(t) }).request({ ...GET_READ, signal: controller.signal });
    await new Promise((resolve) => { setTimeout(resolve, 5); });
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('a request that opts out (timeoutMs: null) is sent with the caller\'s signal alone', async () => {
    const t = fakeTransport();
    t.replies.push(cborReply(200, []), cborReply(200, []));
    const client = new HttpClient(t.deps);
    await client.request(GET_READ);
    await client.request({ ...GET_READ, timeoutMs: null });
    expect(t.seen[0].init.signal).toBeInstanceOf(AbortSignal);
    expect(t.seen[1].init.signal).toBeUndefined();
  });
});

describe('pacing', () => {
  it('lets a burst of 48 reads through, then waits 125 ms for the 49th', async () => {
    const t = fakeTransport();
    for (let i = 0; i < 49; i++) t.replies.push(cborReply(200, []));
    const client = new HttpClient(t.deps);
    for (let i = 0; i < 48; i++) await client.request(GET_READ);
    expect(t.sleeps).toEqual([]);
    await client.request(GET_READ);
    expect(t.sleeps).toEqual([125]);
  });

  it('paces each bucket separately and never paces none', async () => {
    const t = fakeTransport();
    for (let i = 0; i < 48 + 16 + 100; i++) t.replies.push(cborReply(200, []));
    const client = new HttpClient(t.deps);
    for (let i = 0; i < 48; i++) await client.request(GET_READ);
    for (let i = 0; i < 16; i++) await client.request({ ...POST_WRITE, idempotent: true });
    for (let i = 0; i < 100; i++) await client.request({ ...GET_READ, bucket: 'none' });
    expect(t.sleeps).toEqual([]);
  });
});

describe('octet-stream bodies (L-HTTP-91)', () => {
  it('labels an octet-stream body as such and hands fetch the very same bytes', async () => {
    const t = fakeTransport();
    t.replies.push(cborReply(201, [new Uint8Array(32).fill(0x17), 3]));
    const stored = new Uint8Array([1, 2, 3]);
    await new HttpClient(t.deps).request({ method: 'PUT', path: '/v1/blob', body: stored, bucket: 'upload', idempotent: true,
      contentType: 'application/octet-stream' });
    expect(t.seen[0].headers.get('content-type')).toBe('application/octet-stream');
    expect(t.seen[0].init.body).toBe(stored);
  });

  it('still sends a copy of a CBOR body, labelled application/cbor, when no content type is named', async () => {
    const t = fakeTransport();
    t.replies.push(noContent());
    const body = encode(['x']);
    await new HttpClient(t.deps).request({ method: 'POST', path: '/v1/thing', body, bucket: 'write', idempotent: false });
    expect(t.seen[0].headers.get('content-type')).toBe('application/cbor');
    expect(t.seen[0].init.body).not.toBe(body);
    expect(t.seen[0].body).toEqual(body);
  });

  it('sends the same bytes again when an idempotent upload is retried after a network failure', async () => {
    const t = fakeTransport();
    t.replies.push(new TypeError('fetch failed'), cborReply(200, [new Uint8Array(32), 3]));
    const stored = new Uint8Array([4, 5, 6]);
    const res = await new HttpClient(t.deps).request({ method: 'PUT', path: '/v1/blob', body: stored, bucket: 'upload',
      idempotent: true, contentType: 'application/octet-stream' });
    expect(res.status).toBe(200);
    expect(t.seen).toHaveLength(2);
    expect(t.seen[1].init.body).toBe(stored);
    expect(t.seen[1].body).toEqual(new Uint8Array([4, 5, 6]));
  });
});

describe('bounded bodies (FACTS-SECURITY-13)', () => {
  it('stops reading an oversized 404 error body at the error cap', async () => {
    const t = fakeTransport();
    let pulls = 0;
    const stream = new ReadableStream<Uint8Array>({ pull(controller) {
      pulls++;
      controller.enqueue(new Uint8Array(16 * 1024));
      if (pulls === 8) controller.close();
    } }, { highWaterMark: 0 });
    t.replies.push(new Response(stream, { status: 404 }));
    await expect(new HttpClient(t.deps).request({ ...GET_READ, ok: [200, 404], maxBodyBytes: 32 }))
      .rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE', status: 404 });
    expect(pulls).toBe(5);
    expect(t.seen).toHaveLength(1);
  });

  it('does not retry an oversized 500 error body on an idempotent request', async () => {
    const t = fakeTransport();
    t.replies.push(new Response(new Uint8Array(64 * 1024 + 1), { status: 500 }));
    await expect(new HttpClient(t.deps).request(GET_READ))
      .rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE', status: 500 });
    expect(t.seen).toHaveLength(1);
    expect(t.sleeps).toEqual([]);
  });
  it('refuses a body over maxBodyBytes by its Content-Length, once, without retrying', async () => {
    const t = fakeTransport();
    t.replies.push(new Response(new Uint8Array(33), { status: 200, headers: { 'Content-Length': '33' } }));
    await expect(new HttpClient(t.deps).request({ method: 'GET', path: '/v1/blob', bucket: 'read', idempotent: true, ok: [200, 404], maxBodyBytes: 32 }))
      .rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE', status: 200 });
    expect(t.seen).toHaveLength(1);
  });

  it('refuses a streamed body that passes maxBodyBytes', async () => {
    const t = fakeTransport();
    const stream = new ReadableStream<Uint8Array>({ start(c) { c.enqueue(new Uint8Array(20)); c.enqueue(new Uint8Array(13)); c.close(); } });
    t.replies.push(new Response(stream, { status: 200 }));
    await expect(new HttpClient(t.deps).request({ method: 'GET', path: '/v1/blob', bucket: 'read', idempotent: true, ok: [200, 404], maxBodyBytes: 32 }))
      .rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE' });
    expect(t.seen).toHaveLength(1);
  });

  it('answers a body of exactly maxBodyBytes', async () => {
    const t = fakeTransport();
    t.replies.push(new Response(new Uint8Array(32).fill(7), { status: 200 }));
    const r = await new HttpClient(t.deps).request({ method: 'GET', path: '/v1/blob', bucket: 'read', idempotent: true, ok: [200, 404], maxBodyBytes: 32 });
    expect(r.body).toEqual(new Uint8Array(32).fill(7));
  });
});

// web3.6 C2: the global refusal hook the worker turns into its factor-setup phase on E_FACTOR_REQUIRED.
describe('onRefusal', () => {
  it('is told the code of a refusal once, just before it is thrown, after the retries', async () => {
    const t = fakeTransport();
    const seen: string[] = [];
    const client = new HttpClient({ ...t.deps, onRefusal: (code) => { seen.push(`${code}@${String(t.seen.length)}`); } });
    t.replies.push(refusal(503, 'E_UNAVAILABLE', '', 10), refusal(403, 'E_FACTOR_REQUIRED'));
    expect(await failure(client.request(GET_READ))).toMatchObject({ status: 403, code: 'E_FACTOR_REQUIRED' });
    expect(seen).toEqual(['E_FACTOR_REQUIRED@2']);
  });

  it('is not told of a success, a network failure, or an answer the request accepts', async () => {
    const t = fakeTransport();
    const seen: string[] = [];
    const client = new HttpClient({ ...t.deps, onRefusal: (code) => { seen.push(code); } });
    t.replies.push(cborReply(200, []));
    await client.request(GET_READ);
    t.replies.push(new TypeError('offline'));
    expect(await failure(client.request(POST_WRITE))).toMatchObject({ status: 0, code: 'E_NETWORK' });
    t.replies.push(refusal(404, 'E_NOT_FOUND'));
    await client.request({ ...GET_READ, ok: [200, 404] });
    expect(seen).toEqual([]);
    // Control: the same 404 not accepted is a refusal.
    t.replies.push(refusal(404, 'E_NOT_FOUND'));
    await failure(client.request(GET_READ));
    expect(seen).toEqual(['E_NOT_FOUND']);
  });

  it('a hook that throws does not replace the refusal', async () => {
    const t = fakeTransport();
    const client = new HttpClient({ ...t.deps, onRefusal: () => { throw new Error('hook failed'); } });
    t.replies.push(refusal(403, 'E_FACTOR_REQUIRED'));
    expect(await failure(client.request(GET_READ))).toMatchObject({ status: 403, code: 'E_FACTOR_REQUIRED' });
  });
});
