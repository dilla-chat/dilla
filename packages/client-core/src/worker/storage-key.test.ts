import { afterEach, describe, expect, it, vi } from 'vitest';
import { storeDirectory } from '../account/boot';
import { lockName } from './leader';
import { instanceStorageKey, storageBinding } from './storage-key';

// Review C1 (Task 6): in the mobile app every instance shares one origin, so the OPFS directory, the KEK record and the
// leader lock are keyed on the instance URL as well as on the instanceId the server asserts.
const ID = new Uint8Array(16).fill(0x42);

describe('instanceStorageKey', () => {
  it('desktop and browser (no binding): byte-identical to the keys before the change', async () => {
    const key = await instanceStorageKey(ID, null);
    expect(key).toBe('42'.repeat(16));
    expect(storeDirectory(key)).toBe('dilla/42424242424242424242424242424242');
    expect(lockName(key)).toBe('dilla-core:42424242424242424242424242424242');
  });

  it('mobile: hex(SHA-256(instance URL || 0x00 || instanceId)), fixed vectors', async () => {
    // Computed independently with node:crypto.
    expect(await instanceStorageKey(ID, 'https://a.example')).toBe('1fb66e8447b7ea615be193fa9f212c4bede73b7060555a3576d50fdb4d61296d');
    expect(await instanceStorageKey(ID, 'https://b.example')).toBe('d95825b6f3d288f414d8c21154bd9e8def5609bca72649eb59fdadaadf5f6091');
  });

  it('two instance URLs reporting the same instanceId get different directories, KEK records and lock names', async () => {
    const a = await instanceStorageKey(ID, 'https://a.example');
    const b = await instanceStorageKey(ID, 'https://b.example');
    expect(a).not.toBe(b);
    expect(storeDirectory(a)).not.toBe(storeDirectory(b));
    expect(lockName(a)).not.toBe(lockName(b));
    for (const key of [a, b]) expect(key).not.toBe('42'.repeat(16));
  });
});

describe('storageBinding', () => {
  afterEach(() => { vi.unstubAllGlobals(); vi.resetModules(); });

  it('binds only a mobile-build worker at a mobile app origin; web and desktop stay unbound', async () => {
    for (const origin of ['capacitor://localhost', 'https://localhost', 'https://chat.example']) expect(storageBinding(origin, 'https://chat.example')).toBeNull();
    vi.stubGlobal('__DILLA_MOBILE__', true);
    vi.resetModules();
    const { storageBinding: mobile } = await import('./storage-key');
    expect(mobile('capacitor://localhost', 'https://chat.example')).toBe('https://chat.example');
    expect(mobile('https://localhost', 'https://chat.example')).toBe('https://chat.example');
    expect(mobile('https://chat.example', 'https://chat.example')).toBeNull();
  });
});
