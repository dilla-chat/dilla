/** The instance list the picker keeps in @capacitor/preferences: `instances` (a JSON array of canonical URLs) and
 *  `current` (one of them). No Capacitor import here, so the logic is testable under plain Node. */

export interface PreferenceStore {
  get(options: { key: string }): Promise<{ value: string | null }>;
  set(options: { key: string; value: string }): Promise<void>;
}

/** The desktop picker's copy (packages/desktop/src/manager.ts), word for word. */
export const CONNECT_ERROR = 'Could not connect. Enter a complete HTTPS instance address, then try again.';

// Copied from packages/desktop/src/policy.ts (canonicalInstance and unsafeUrl). Not imported: policy.ts pulls in
// node:crypto and the desktop contract, which a WebView bundle cannot carry. Keep the two in step.
const unsafeUrl = /[\s\\\u0000-\u001f\u007f]/;

export function canonicalInstance(input: string, allowLoopback = false): string {
  if (typeof input !== 'string' || input.length > 2048 || unsafeUrl.test(input) || /%(?:2f|5c|00|2e)/i.test(input)) throw new Error('Invalid instance URL');
  const url = new URL(input);
  if (url.username || url.password || url.search || url.hash) throw new Error('Instance URLs cannot contain credentials, queries or fragments');
  const local = allowLoopback && ['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname);
  if (url.protocol !== 'https:' && !(local && url.protocol === 'http:')) throw new Error('An HTTPS instance is required');
  if (!url.hostname || url.hostname.endsWith('.')) throw new Error('Invalid instance host');
  if (!url.pathname.endsWith('/')) url.pathname += '/';
  if (url.pathname.startsWith('/assets/')) throw new Error('The installed client reserves /assets/; choose another instance base path');
  return url.href;
}

/** A phone instance is an HTTPS origin with a DNS name: the server's mobile CORS covers /v1/ and /i/ at the root only
 *  (internal/dillad/server.go mobileCORS), so an instance under a base path could not be reached from the app. */
export function mobileInstance(input: string): string {
  let instance: string;
  try { instance = canonicalInstance(input.trim()); } catch { throw new Error(CONNECT_ERROR); }
  const url = new URL(instance);
  // An origin with a DNS name, as the core worker's gate requires (client-core worker/instance-url.ts): no IP literal
  // (IPv4 in any form parses to a dotted quad, IPv6 to a [bracketed] host).
  // Nor localhost or *.localhost: the device itself, and https://localhost is the Android app's own origin.
  if (url.pathname !== '/' || url.hostname.startsWith('[') || /^\d{1,3}(\.\d{1,3}){3}$/.test(url.hostname)
    || url.hostname === 'localhost' || url.hostname.endsWith('.localhost')) throw new Error(CONNECT_ERROR);
  return instance;
}

function valid(value: unknown): value is string {
  if (typeof value !== 'string') return false;
  try { return mobileInstance(value) === value; } catch { return false; }
}

export async function listInstances(store: PreferenceStore): Promise<string[]> {
  const { value } = await store.get({ key: 'instances' });
  if (!value) return [];
  try {
    const parsed: unknown = JSON.parse(value);
    return Array.isArray(parsed) ? [...new Set(parsed.filter(valid))] : [];
  } catch { return []; }
}

export async function currentInstance(store: PreferenceStore): Promise<string | null> {
  const { value } = await store.get({ key: 'current' });
  return value && (await listInstances(store)).includes(value) ? value : null;
}

/** Checks that an instance answers, before the picker stores it. Rejects when it cannot be reached. */
export type InstanceProbe = (instance: string) => Promise<void>;

type Fetch = (url: string, init?: RequestInit) => Promise<Response>;

/** The picker's probe: GET <instance>/v1/instance through the WebView's own fetch, so it also proves the server
 *  answers the app's origin (http.mobile_origins, CORS). No credentials, no redirects, a 10 s cap, and a 2xx only. */
export function reachInstance(fetchImpl: Fetch): InstanceProbe {
  return async instance => {
    const response = await fetchImpl(new URL('v1/instance', instance).href, {
      method: 'GET', credentials: 'omit', redirect: 'error', cache: 'no-store', signal: AbortSignal.timeout(10_000),
    });
    if (!response.ok) throw new Error(`instance answered ${response.status}`);
  };
}

/** Validates, probes, stores once (a duplicate add is a no-op), and makes the instance current. An instance that cannot
 *  be reached is refused with the desktop copy and nothing is stored. */
export async function addInstance(store: PreferenceStore, input: string, probe: InstanceProbe): Promise<string> {
  const instance = mobileInstance(input);
  try { await probe(instance); } catch { throw new Error(CONNECT_ERROR); }
  const instances = await listInstances(store);
  if (!instances.includes(instance)) await store.set({ key: 'instances', value: JSON.stringify([...instances, instance]) });
  await store.set({ key: 'current', value: instance });
  return instance;
}

export async function selectInstance(store: PreferenceStore, instance: string): Promise<void> {
  if (!(await listInstances(store)).includes(instance)) throw new Error(CONNECT_ERROR);
  await store.set({ key: 'current', value: instance });
}
