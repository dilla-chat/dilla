import { describe, expect, it, vi } from 'vitest';
import type { BootOutcome } from '../account/boot';
import type { CorePort } from '../core-port';
import { INSTANCE_DOC, LOCK, cbor, world } from './world';
import { instanceStorageKey } from './storage-key';

// Review C1 (Task 6): in the mobile app instances A and B share one origin. B is hostile and reports A's instanceId.
// The worker for B must neither take A's leader lock nor open A's store, so A's session token never reaches B.
const A = 'https://a.example', B = 'https://b.example';

describe('two instances on one origin with the same instanceId', () => {
  it('get different lock names and stores, and no request to B carries A\'s bearer', async () => {
    // The origin's storage holds one store: A's, with A's session in it. A boot opens it only under A's key.
    const keyA = await instanceStorageKey(new Uint8Array(16).fill(0x42), A);
    const authorizations: (string | null)[] = [];
    const fetchB = ((input: RequestInfo | URL, init?: RequestInit) => {
      authorizations.push(new Request(input, init).headers.get('authorization'));
      return Promise.resolve(cbor(INSTANCE_DOC));          // B answers with A's instanceId (0x42…)
    }) as typeof fetch;
    const storageKey = (id: Uint8Array) => instanceStorageKey(id, B);
    let core: CorePort | null = null;
    const bootKeys: string[] = [];
    const w = world({
      httpInstance: true, fetch: fetchB, origin: B, storageKey,
      boot: async (instance): Promise<BootOutcome> => {
        const key = await storageKey(instance.instanceId);
        bootKeys.push(key);
        return key === keyA && core !== null ? { kind: 'opened', core, instance } : { kind: 'unsupported', reason: 'a fresh store' };
      },
    });
    core = w.core as unknown as CorePort;
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('unsupported'));
    const lockB = w.locks.requests.map((r) => r.name);
    expect(lockB.length).toBeGreaterThan(0);
    expect(lockB.every((name) => name === `dilla-core:${bootKeys[0]}`)).toBe(true);
    expect(lockB).not.toContain(LOCK);                       // A's lock in the desktop keying
    expect(lockB).not.toContain(`dilla-core:${keyA}`);       // A's lock in the mobile keying
    expect(bootKeys).toHaveLength(1);
    expect(bootKeys[0]).not.toBe(keyA);
    expect(authorizations.length).toBeGreaterThan(0);
    expect(authorizations.every((value) => value === null)).toBe(true);
  });

  it('without a storage key the worker keeps the desktop lock name (dilla-core:<instance hex>)', async () => {
    const w = world();
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
    expect(w.locks.requests.map((r) => r.name)).toContain(LOCK);
  });
});
