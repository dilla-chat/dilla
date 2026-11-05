import type { BrowserAuthRequest, BrowserAuthResult } from '../../desktop/src/browser-auth.ts';
import { canonicalInstance } from './instances.ts';

// A port of packages/desktop/src/browser-auth.ts: the same flow, limits, timeouts and error strings, on WebCrypto.
// The desktop-auth requests go through native HTTP (CapacitorHttp, wired in bridge.ts), never the WebView fetch:
// a WebView request carries Origin: capacitor://localhost or https://localhost, and /desktop-auth/ refuses any
// foreign Origin (internal/desktopauth/http.go). Native HTTP sends no Origin, as desktop's Node main process does.

/** The subset of CapacitorHttp this flow uses. */
export type NativeHttpOptions = {
  url: string; method: 'POST'; headers: Record<string, string>; data: unknown;
  disableRedirects: true; responseType: 'text'; connectTimeout: number; readTimeout: number;
};
export type NativeHttp = { request(options: NativeHttpOptions): Promise<{ status: number; data: unknown }> };
/** Injectable for tests; the defaults are AbortSignal.timeout and a cancellable setTimeout. */
export type Clock = { timeout(ms: number): AbortSignal; sleep(ms: number, signal: AbortSignal): Promise<void> };

export const CEREMONY_TIMEOUT_MS = 120000;
const MAX_REPLY_BYTES = 70000;

const systemClock: Clock = {
  timeout: ms => AbortSignal.timeout(ms),
  sleep: (ms, signal) => new Promise((resolve, reject) => {
    if (signal.aborted) { reject(signal.reason); return; }
    const timer = setTimeout(() => { signal.removeEventListener('abort', abort); resolve(); }, ms);
    function abort() { clearTimeout(timer); reject(signal.reason); }
    signal.addEventListener('abort', abort, { once: true });
  }),
};

function base64url(bytes: Uint8Array): string {
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** CapacitorHttp cannot be aborted; the race makes the ceremony's deadline hold anyway. */
function abortable<T>(work: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(signal.reason); return; }
    const abort = () => reject(signal.reason);
    signal.addEventListener('abort', abort, { once: true });
    work.then(resolve, reject).finally(() => signal.removeEventListener('abort', abort));
  });
}

/** Only a fixed ceremony navigation reaches the system browser. The verifier is
 * held in memory, expires after two minutes, and never occurs in a URL. */
export async function browserAuth(instance: string, value: unknown, deps: {
  open(url: string): Promise<unknown>; http: NativeHttp; signal?: AbortSignal; clock?: Clock;
}): Promise<BrowserAuthResult> {
  const request = value as Partial<BrowserAuthRequest> | null;
  if (!request || typeof request !== 'object' || Object.keys(request).sort().join(',') !== 'kind,options'
    || !['get', 'create', 'oidc'].includes(request.kind ?? '') || typeof request.options !== 'string' || request.options.length > 65536) throw new Error('Invalid browser ceremony');
  const base = new URL('desktop-auth/', canonicalInstance(instance));
  const clock = deps.clock ?? systemClock;
  const signal = AbortSignal.any([clock.timeout(CEREMONY_TIMEOUT_MS), ...(deps.signal ? [deps.signal] : [])]);
  const verifier = base64url(crypto.getRandomValues(new Uint8Array(32)));
  const challenge = base64url(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier))));
  async function post(url: URL, body: unknown, first = false): Promise<{ status: number; value: Record<string, unknown> }> {
    signal.throwIfAborted();
    const response = await abortable(deps.http.request({
      url: url.href, method: 'POST', headers: { 'Content-Type': 'application/json' }, data: body,
      disableRedirects: true, responseType: 'text', connectTimeout: 30000, readTimeout: 30000,
    }), signal);
    if (response.status === 202) return { status: 202, value: {} };
    // An instance older than the desktop sign-in has no such route: say so, so the page does not blame the passkey.
    // These two codes are stable strings the web client reads out of the wrapped error message.
    if (first && (response.status === 404 || response.status === 405)) throw new Error('desktop-auth-unsupported');
    if (response.status === 429) throw new Error('desktop-auth-rate-limited');
    if (response.status < 200 || response.status > 299) throw new Error('Browser sign-in unavailable. Start again in Dilla.');
    // CapacitorHttp hands a JSON reply over already parsed (it ignores responseType for JSON content types), so the
    // size limit is applied to the text, or to the re-serialised value, before anything is used.
    const text = typeof response.data === 'string' ? response.data : JSON.stringify(response.data ?? null);
    if (new TextEncoder().encode(text).byteLength > MAX_REPLY_BYTES) throw new Error('Invalid browser reply');
    let value: unknown;
    try { value = typeof response.data === 'string' ? JSON.parse(text) : response.data; } catch { throw new Error('Invalid browser reply'); }
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid browser reply');
    return { status: response.status, value: value as Record<string, unknown> };
  }
  const created = await post(new URL('request', base), { ...request, challenge }, true);
  if (typeof created.value.id !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(created.value.id)) throw new Error('Invalid browser request ID');
  const navigation = new URL(created.value.id, base);
  await deps.open(navigation.href);
  for (;;) {
    signal.throwIfAborted();
    const result = await post(new URL(`${created.value.id}/consume`, base), { verifier });
    if (result.status !== 202) {
      const { response, assertion, needs, error } = result.value;
      if (error !== undefined) throw new Error(typeof error === 'string' && error.length <= 128 ? error : 'Browser sign-in failed');
      if (!Number.isInteger(needs) || (needs as number) < 0 || (needs as number) > 2
        || (request.kind === 'oidc' ? typeof assertion !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(assertion) : typeof response !== 'string' || response.length > 65536)) throw new Error('Invalid browser completion');
      return { ...(request.kind === 'oidc' ? { assertion: assertion as string } : { response: response as string }), needs: needs as number };
    }
    await clock.sleep(1000, signal);
  }
}
