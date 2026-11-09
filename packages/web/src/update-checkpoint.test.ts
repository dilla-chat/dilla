import { IDBFactory } from 'fake-indexeddb';
import { webcrypto } from 'node:crypto';
import { expect, it } from 'vitest';
import { checkpointScope, createUpdateCheckpointStore, updateTabId } from './update-checkpoint.ts';

const scope = { instance: 'ab'.repeat(16), user: 'cd'.repeat(16), tab: 'ef'.repeat(16) };

it('keeps a content-free tab identifier across reloads', () => {
  const values = new Map<string, string>();
  const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); } };
  const first = updateTabId(storage, webcrypto as Crypto);
  expect(first).toMatch(/^[0-9a-f]{32}$/);
  expect(updateTabId(storage, webcrypto as Crypto)).toBe(first);
});

it('restores a working view and selected file after a fresh store opens the same database', async () => {
  const idb = new IDBFactory();
  const first = createUpdateCheckpointStore(idb, webcrypto as Crypto, () => 1000);
  const file = new File(['private attachment'], 'note.txt', { type: 'text/plain', lastModified: 42 });
  // jsdom's File omits arrayBuffer; browsers provide it.
  Object.defineProperty(file, 'arrayBuffer', { value: () => Promise.resolve(new TextEncoder().encode('private attachment').buffer) });
  await first.save(scope, { draft: 'private draft', side: 'thread' }, [file]);
  const second = createUpdateCheckpointStore(idb, webcrypto as Crypto, () => 1001);
  const restored = await second.load(scope);
  expect(restored?.state).toEqual({ draft: 'private draft', side: 'thread' });
  expect(restored?.files).toHaveLength(1);
  expect(restored?.files[0]?.name).toBe('note.txt');
  expect(restored?.files[0]?.type).toBe('text/plain');
  const restoredFile = restored?.files[0];
  if (restoredFile === undefined) throw new Error('missing restored file');
  expect(await new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => typeof reader.result === 'string' ? resolve(reader.result) : reject(new Error('file was not text'));
    reader.onerror = () => reject(reader.error ?? new Error('file read failed'));
    reader.readAsText(restoredFile);
  })).toBe('private attachment');
  expect((await second.load(scope))?.state).toEqual(restored?.state);
  expect(await second.load({ ...scope, user: '01'.repeat(16) })).toBeNull();
});

it('expires a checkpoint and removes it after restoration', async () => {
  const idb = new IDBFactory();
  const first = createUpdateCheckpointStore(idb, webcrypto as Crypto, () => 1000);
  await first.save(scope, { draft: 'private' }, []);
  expect(await createUpdateCheckpointStore(idb, webcrypto as Crypto, () => 1000 + 16 * 60_000).load(scope)).toBeNull();
  expect(await first.load(scope)).toBeNull();
  await first.save(scope, { draft: 'second' }, []);
  await first.remove(scope);
  expect(await first.load(scope)).toBeNull();
});

// Review round 2, item 1 (Task 6): in the mobile app instances share one origin and one IndexedDB. The scope uses the
// worker's storage key (instance URL bound) there, so an instance asserting another's instanceId and userId finds nothing.
it('keys the scope on the storage key when the worker publishes one, and on the instance id otherwise', async () => {
  const id = 'ab'.repeat(16), user = 'cd'.repeat(16), tab = 'ef'.repeat(16);
  expect(checkpointScope({ id }, user, tab)).toEqual({ instance: id, user, tab });
  const a = checkpointScope({ id, storageKey: '1f'.repeat(32) }, user, tab);
  const b = checkpointScope({ id, storageKey: 'd9'.repeat(32) }, user, tab);
  expect(a.instance).toBe('1f'.repeat(32));
  const idb = new IDBFactory();
  const store = createUpdateCheckpointStore(idb, webcrypto as Crypto, () => 1000);
  await store.save(a, { draft: 'A only' }, []);
  expect(await createUpdateCheckpointStore(idb, webcrypto as Crypto, () => 1001).load(b)).toBeNull();
  expect((await createUpdateCheckpointStore(idb, webcrypto as Crypto, () => 1002).load(a))?.state).toEqual({ draft: 'A only' });
});
