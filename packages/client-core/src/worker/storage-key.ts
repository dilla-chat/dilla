import { toHex } from '../hex';
import { isMobileAppOrigin, MOBILE_BUILD } from './mobile-build';

/**
 * The one key for an instance's local state: the OPFS directory dilla/<key>, the KEK record <key> and the leader lock
 * dilla-core:<key> all use it, so they always agree (review C1, Task 6).
 *
 * Desktop and the browser keep their pages on the instance's own origin, so storage is already separated per instance
 * and the key stays the instanceId in hex, byte-identical to before (existing installs keep their stores).
 * The mobile app runs every instance on one origin (capacitor://localhost, https://localhost), and instanceId is only
 * what the server asserts: a hostile instance could name another's and be handed its store and bearer token. There the
 * key is hex(SHA-256(instance URL || 0x00 || instanceId)), with the instance URL from workerInstanceURL.
 */
export async function instanceStorageKey(instanceId: Uint8Array, binding: string | null): Promise<string> {
  if (binding === null) return toHex(instanceId);
  const url = new TextEncoder().encode(binding);
  const input = new Uint8Array(url.length + 1 + instanceId.length);
  input.set(url);
  input[url.length] = 0;
  input.set(instanceId, url.length + 1);
  return toHex(new Uint8Array(await crypto.subtle.digest('SHA-256', input)));
}

/** The instance URL a mobile worker binds its storage to; null (the instanceId alone) everywhere else. */
export function storageBinding(workerOrigin: string, instanceURL: string): string | null {
  return MOBILE_BUILD && isMobileAppOrigin(workerOrigin) ? instanceURL : null;
}
