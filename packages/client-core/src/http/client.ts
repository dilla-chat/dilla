// The client core's HTTP transport; every typed route passes through request.
import { DillaHttpError, decodeErrorBody } from './errors';
import { TokenBuckets, type Bucket } from './pacing';
import { socketRouteFor, type SocketRoute } from './socket-routes';
import type { SocketTransport } from '../gateway/requests';
export { PACING, type Bucket } from './pacing';

export interface HttpRequest {
  method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  path: string;
  body?: Uint8Array;
  contentType?: 'application/cbor' | 'application/octet-stream';
  maxBodyBytes?: number;
  bucket: Bucket;
  idempotent: boolean;
  auth?: boolean;
  accept?: string;
  ok?: readonly number[];
  retry429?: boolean;
  signal?: AbortSignal;
  /** The per-attempt timeout; null opts out (a blob transfer, which may outlast it on a slow link). */
  timeoutMs?: number | null;
  /** Bot v2: the idempotency key a socket route's request frame carries, kept by every retry of this call. Required for
   *  every method but GET on a socket route (E_IDEMPOTENCY_REQUIRED); Routes sets it on every mutation. */
  idem?: Uint8Array;
}
export interface HttpResponse { status: number; body: Uint8Array; }
export interface HttpDeps {
  baseUrl: string;
  fetch: typeof fetch;
  now(): number;
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
  random(): number;
  token(): string | null;
  reauthenticate(): Promise<boolean>;
  onGeneration(generation: bigint): void;
  /** web3.6: told the code of every refusal the instance answered (status 100..599), once per request, just before it
   *  is thrown (after any retry); never for a network failure. The worker turns E_FACTOR_REQUIRED into its factor-setup
   *  phase. A hook that throws does not replace the refusal. */
  onRefusal?(code: string): void;
  /** Overrides HTTP_ATTEMPT_TIMEOUT_MS (tests). */
  attemptTimeoutMs?: number;
  /** Bot v2: the gateway's request transport, or null while there is no gateway. A route in the socket table goes only
   *  through it (no HTTP fallback: the instance does not serve it there). */
  socket?(): SocketTransport | null;
  /** The socket route table; SOCKET_ROUTES unless a test passes its own. */
  socketRoutes?: readonly SocketRoute[];
}
export const RETRY = { maxAttempts: 5, baseMs: 500, capMs: 8000, jitterMs: 250, maxWaitMs: 60000 } as const;
const ERROR_BODY_CAP = 64 * 1024;
/** WB I1: one attempt that has not answered in this long ends as E_NETWORK status 0, so a stalled request never holds
 *  its caller; an idempotent one is retried as any network failure is. */
export const HTTP_ATTEMPT_TIMEOUT_MS = 30_000;
const abort = (): DOMException => new DOMException('The operation was aborted.', 'AbortError');

export class HttpClient {
  private readonly baseUrl: string;
  private readonly fetchFn: typeof fetch;
  private readonly buckets: TokenBuckets;

  constructor(private readonly deps: HttpDeps) {
    this.baseUrl = deps.baseUrl.endsWith('/') ? deps.baseUrl.slice(0, -1) : deps.baseUrl;
    this.fetchFn = deps.fetch;
    this.buckets = new TokenBuckets({ now: () => deps.now(), sleep: (ms, signal) => deps.sleep(ms, signal) });
  }

  async request(r: HttpRequest): Promise<HttpResponse> {
    const pattern = socketRouteFor(r.method, r.path, this.deps.socketRoutes);
    if (pattern !== null) return this.requestSocket(r, pattern);
    let reauthenticated = false;
    for (let attempt = 1; attempt <= RETRY.maxAttempts; attempt++) {
      if (r.signal?.aborted) throw abort();
      await this.buckets.take(r.bucket, r.signal);
      if (r.signal?.aborted) throw abort();
      const headers = new Headers();
      const token = r.auth !== false ? this.deps.token() : null;
      if (token !== null) headers.set('Authorization', `Bearer ${token}`);
      if (r.body !== undefined) headers.set('Content-Type', r.contentType ?? 'application/cbor');
      if (r.accept !== undefined) headers.set('Accept', r.accept);
      // Routes supplies ArrayBuffer-backed octets; a 25 MiB upload must not be copied here.
      const body = r.contentType === 'application/octet-stream' ? (r.body as Uint8Array<ArrayBuffer>) : r.body?.slice();
      const timeoutMs = r.timeoutMs === undefined ? (this.deps.attemptTimeoutMs ?? HTTP_ATTEMPT_TIMEOUT_MS) : r.timeoutMs;
      const signal = timeoutMs === null ? r.signal
        : r.signal === undefined ? AbortSignal.timeout(timeoutMs) : AbortSignal.any([r.signal, AbortSignal.timeout(timeoutMs)]);
      const init: RequestInit = { method: r.method, headers, body,
        signal, credentials: 'omit', cache: 'no-store', redirect: 'error' };
      let error: DillaHttpError;
      try {
        const response = await this.fetchFn(this.baseUrl + r.path, init);
        const generation = response.headers.get('X-Dilla-Generation');
        if (generation !== null && /^[0-9]{1,20}$/.test(generation)) {
          const number = BigInt(generation);
          if (number <= (1n << 64n) - 1n) this.deps.onGeneration(number);
        }
        const responseBody = await readBody(response, r.maxBodyBytes);
        if ((r.ok ?? [200, 201, 204]).includes(response.status)) return { status: response.status, body: responseBody };
        error = decodeErrorBody(response.status, responseBody, response.headers.get('Retry-After'));
      } catch (e) {
        if (r.signal?.aborted) throw abort();
        if (e instanceof DillaHttpError) throw e;
        // A per-attempt timeout (the caller's own abort answered above) is a network failure like any other.
        const timedOut = signal?.aborted === true;
        error = new DillaHttpError({ status: 0, code: 'E_NETWORK',
          detail: timedOut ? 'the request timed out' : e instanceof Error ? e.message : 'network failure', retryAfterMs: null, extra: [] });
      }
      if (error.status === 401 && r.auth !== false && !reauthenticated) {
        reauthenticated = true;
        const renewed = await this.deps.reauthenticate();
        if (renewed && attempt < RETRY.maxAttempts) continue;
      }
      if ((error.status === 429 && r.retry429 !== false) || error.status === 503) {
        const wait = error.retryAfterMs ?? 1000;
        if (attempt < RETRY.maxAttempts && wait <= RETRY.maxWaitMs) {
          try { await this.deps.sleep(wait, r.signal); }
          catch (e) { if (r.signal?.aborted) throw abort(); throw e; }
          continue;
        }
      }
      if (r.idempotent && (error.status === 0 || error.status === 500) && attempt < RETRY.maxAttempts) {
        const wait = Math.min(RETRY.capMs, RETRY.baseMs * 2 ** (attempt - 1))
          + Math.floor(this.deps.random() * RETRY.jitterMs);
        try { await this.deps.sleep(wait, r.signal); }
        catch (e) { if (r.signal?.aborted) throw abort(); throw e; }
        continue;
      }
      if (error.status !== 0) {
        try { this.deps.onRefusal?.(error.code); } catch { /* the refusal is what the caller gets */ }
      }
      throw error;
    }
    throw new Error('unreachable');
  }

  /** A socket route: the request frame's answer, decoded as an HTTP answer. SocketRequests already waits out E_BUSY,
   *  503 E_UNAVAILABLE and a lost socket (same idem); a route meter's 429 is waited out here as on HTTP. Every other
   *  refusal (409, 401, 426, a route's own 503) reaches the caller. There are no headers: no generation, no Retry-After. */
  private async requestSocket(r: HttpRequest, pattern: string): Promise<HttpResponse> {
    const socket = this.deps.socket?.() ?? null;
    if (socket === null) {
      throw new DillaHttpError({ status: 0, code: 'E_NETWORK', detail: 'the gateway socket is not running', retryAfterMs: null, extra: [] });
    }
    for (let attempt = 1; attempt <= RETRY.maxAttempts; attempt++) {
      if (r.signal?.aborted) throw abort();
      await this.buckets.take(r.bucket, r.signal);
      if (r.signal?.aborted) throw abort();
      const response = await socket.request(r, pattern);
      const ok = (r.ok ?? [200, 201, 204]).includes(response.status);
      const bound = ok ? r.maxBodyBytes : ERROR_BODY_CAP;
      if (bound !== undefined && response.body.length > bound) {
        throw new DillaHttpError({ status: response.status, code: 'E_BODY_TOO_LARGE',
          detail: 'the response body is larger than ' + bound + ' bytes', retryAfterMs: null, extra: [] });
      }
      if (ok) return response;
      const error = decodeErrorBody(response.status, response.body, null);
      if (error.status === 429 && r.retry429 !== false && attempt < RETRY.maxAttempts) {
        const wait = error.retryAfterMs ?? 1000;
        if (wait <= RETRY.maxWaitMs) {
          try { await this.deps.sleep(wait, r.signal); }
          catch (e) { if (r.signal?.aborted) throw abort(); throw e; }
          continue;
        }
      }
      try { this.deps.onRefusal?.(error.code); } catch { /* the refusal is what the caller gets */ }
      throw error;
    }
    throw new Error('unreachable');
  }
}

async function readBody(response: Response, maxBodyBytes?: number): Promise<Uint8Array> {
  const bound = response.status >= 200 && response.status <= 299
    ? maxBodyBytes : ERROR_BODY_CAP;
  if (bound === undefined) {
    return new Uint8Array(await response.arrayBuffer());
  }
  const tooLarge = (): DillaHttpError => new DillaHttpError({ status: response.status, code: 'E_BODY_TOO_LARGE',
    detail: 'the response body is larger than ' + bound + ' bytes', retryAfterMs: null, extra: [] });
  const length = response.headers.get('Content-Length');
  if (length !== null && /^\d+$/.test(length) && Number(length) > bound) {
    await response.body?.cancel();
    throw tooLarge();
  }
  if (response.body === null) return new Uint8Array();
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > bound) { await reader.cancel(); throw tooLarge(); }
    chunks.push(value);
  }
  const result = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength; }
  return result;
}
