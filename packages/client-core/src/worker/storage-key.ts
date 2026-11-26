import { toHex } from '../hex';

/**
 * The one key for an instance's local state: the OPFS directory dilla/<key>, the KEK record <key> and the leader lock
 * dilla-core:<key> all use it, so they always agree (review C1, Task 6).
 *
 * Desktop and the browser keep their pages on the instance's own origin, so storage is already separated per instance
 * and the key stays the instanceId in hex, byte-identical to before (existing installs keep their stores).
 * A client that runs several instances on one origin binds the key to the instance URL, because instanceId is only what
 * the server asserts: a hostile instance could name another's and be handed its store and bearer token. Then the key is
 * hex(SHA-256(instance URL || 0x00 || instanceId)). No shipping client does that today, so the worker passes no binding.
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
