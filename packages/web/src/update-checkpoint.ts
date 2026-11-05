// A one-time, per-tab checkpoint for an intentional application update. Plaintext never enters Web Storage.
import { BROWSER_ATTACHMENT_CAP, MAX_ATTACHMENTS } from '@dilla/client-core';

export interface CheckpointScope { instance: string; user: string; tab: string }
export interface RestoredCheckpoint { state: unknown; files: File[] }

interface SealedFile { name: string; type: string; lastModified: number; iv: Uint8Array; ct: ArrayBuffer }
interface RecordV1 { v: 1; at: number; key: CryptoKey; iv: Uint8Array; ct: ArrayBuffer; files: SealedFile[] }

const DB = 'dilla-update-checkpoints';
const STORE = 'snapshots';
const TTL_MS = 15 * 60_000;
const TAB_KEY = 'dilla.update.tab';

/** A random, content-free ID scoped to this browser tab. */
export function updateTabId(storage: Pick<Storage, 'getItem' | 'setItem'>, crypto: Crypto): string {
  const held = storage.getItem(TAB_KEY);
  if (held !== null && /^[0-9a-f]{32}$/.test(held)) return held;
  const id = [...crypto.getRandomValues(new Uint8Array(16))].map(b => b.toString(16).padStart(2, '0')).join('');
  storage.setItem(TAB_KEY, id);
  return id;
}

/** The checkpoint scope of an account. The worker's storage key stands in for the instance id where it publishes one
 *  (the mobile app: every instance on one origin and one IndexedDB, so the key binds the instance URL; review round 2,
 *  Task 6). Desktop and the web have none, and their record ids stay `<instance id>:<user>:<tab>`. */
export function checkpointScope(instance: { id: string; storageKey?: string }, user: string, tab: string): CheckpointScope {
  return { instance: instance.storageKey ?? instance.id, user, tab };
}

function idOf(scope: CheckpointScope): string { return `${scope.instance}:${scope.user}:${scope.tab}`; }
function aad(scope: CheckpointScope, part: string): Uint8Array<ArrayBuffer> {
  return new TextEncoder().encode(`dilla update checkpoint v1:${idOf(scope)}:${part}`);
}
function open(idb: IDBFactory): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = idb.open(DB, 1);
    req.onupgradeneeded = () => { if (!req.result.objectStoreNames.contains(STORE)) req.result.createObjectStore(STORE); };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error('E_UPDATE_STORE'));
  });
}
async function read(idb: IDBFactory, id: string): Promise<unknown> {
  const db = await open(idb);
  try {
    return await new Promise((resolve, reject) => {
      const req = db.transaction(STORE, 'readonly').objectStore(STORE).get(id);
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error ?? new Error('E_UPDATE_READ'));
    });
  } finally { db.close(); }
}
async function write(idb: IDBFactory, id: string, record?: RecordV1): Promise<void> {
  const db = await open(idb);
  try {
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(STORE, 'readwrite');
      if (record === undefined) tx.objectStore(STORE).delete(id);
      else tx.objectStore(STORE).put(record, id);
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error ?? new Error('E_UPDATE_WRITE'));
      tx.onabort = () => reject(tx.error ?? new Error('E_UPDATE_WRITE'));
    });
  } finally { db.close(); }
}

export function createUpdateCheckpointStore(idb: IDBFactory, crypto: Crypto, now: () => number) {
  return {
    async save(scope: CheckpointScope, state: unknown, files: readonly File[]): Promise<void> {
      if (files.length > MAX_ATTACHMENTS || files.some(file => file.size > BROWSER_ATTACHMENT_CAP)) throw new Error('E_UPDATE_FILES');
      const key = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
      const iv = crypto.getRandomValues(new Uint8Array(12));
      const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv, additionalData: aad(scope, 'state') },
        key, new TextEncoder().encode(JSON.stringify(state)));
      const sealed: SealedFile[] = [];
      for (const [index, file] of files.entries()) {
        const fileIv = crypto.getRandomValues(new Uint8Array(12));
        sealed.push({ name: file.name, type: file.type, lastModified: file.lastModified, iv: fileIv,
          ct: await crypto.subtle.encrypt({ name: 'AES-GCM', iv: fileIv, additionalData: aad(scope, `file:${index}`) },
            key, await file.arrayBuffer()) });
      }
      await write(idb, idOf(scope), { v: 1, at: now(), key, iv, ct, files: sealed });
    },
    async load(scope: CheckpointScope): Promise<RestoredCheckpoint | null> {
      const value = await read(idb, idOf(scope));
      if (typeof value !== 'object' || value === null) return null;
      const record = value as Partial<RecordV1>;
      if (record.v !== 1 || typeof record.at !== 'number' || now() - record.at > TTL_MS || now() < record.at
        || !record.key || !ArrayBuffer.isView(record.iv) || Object.prototype.toString.call(record.ct) !== '[object ArrayBuffer]'
        || !Array.isArray(record.files)) {
        await write(idb, idOf(scope));
        return null;
      }
      try {
        const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: new Uint8Array(record.iv), additionalData: aad(scope, 'state') },
          record.key, record.ct as ArrayBuffer);
        const state: unknown = JSON.parse(new TextDecoder().decode(plain));
        const files: File[] = [];
        for (const [index, file] of record.files.entries()) {
          const bytes = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: new Uint8Array(file.iv), additionalData: aad(scope, `file:${index}`) },
            record.key, file.ct);
          files.push(new File([bytes], file.name, { type: file.type, lastModified: file.lastModified }));
        }
        return { state, files };
      } catch {
        await write(idb, idOf(scope));
        return null;
      }
    },
    remove(scope: CheckpointScope): Promise<void> { return write(idb, idOf(scope)); },
  };
}
