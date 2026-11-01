import { createHash, randomBytes } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import { canonicalInstance } from './policy.ts';

export type BrowserAuthRequest = { kind: 'get' | 'create' | 'oidc'; options: string };
export type BrowserAuthResult = { response?: string; assertion?: string; needs: number; error?: string };

/** Only a fixed ceremony navigation reaches the system browser. The verifier is
 * held in main memory, expires after two minutes, and never occurs in a URL. */
export async function browserAuth(instance: string, value: unknown, deps: {
  open(url: string): Promise<unknown>; fetch?: typeof fetch; signal?: AbortSignal;
}): Promise<BrowserAuthResult> {
  const request = value as Partial<BrowserAuthRequest> | null;
  if (!request || typeof request !== 'object' || Object.keys(request).sort().join(',') !== 'kind,options'
    || !['get', 'create', 'oidc'].includes(request.kind ?? '') || typeof request.options !== 'string' || request.options.length > 65536) throw new Error('Invalid browser ceremony');
  const base = new URL('desktop-auth/', canonicalInstance(instance));
  const signal = AbortSignal.any([AbortSignal.timeout(120000), ...(deps.signal ? [deps.signal] : [])]);
  const verifier = randomBytes(32).toString('base64url');
  const transport = deps.fetch ?? fetch;
  async function post(url: URL, body: unknown): Promise<{ status: number; value: Record<string, unknown> }> {
    const response = await transport(url.href, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), redirect: 'error', signal, credentials: 'omit', cache: 'no-store' });
    if (response.status === 202) { await response.body?.cancel(); return { status: 202, value: {} }; }
    if (!response.ok) { await response.body?.cancel(); throw new Error('Browser sign-in unavailable. Start again in Dilla.'); }
    const reader = response.body?.getReader(); if (!reader) throw new Error('Invalid browser reply');
    let size = 0; const parts: Uint8Array[] = [];
    try { for (;;) { const { value: part, done } = await reader.read(); if (done) break; size += part.byteLength; if (size > 70000) throw new Error('Invalid browser reply'); parts.push(part); } }
    finally { await reader.cancel(); }
    const value: unknown = JSON.parse(Buffer.concat(parts).toString());
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid browser reply');
    return { status: response.status, value: value as Record<string, unknown> };
  }
  const created = await post(new URL('request', base), { ...request, challenge: createHash('sha256').update(verifier).digest('base64url') });
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
    await delay(1000, undefined, { signal });
  }
}
