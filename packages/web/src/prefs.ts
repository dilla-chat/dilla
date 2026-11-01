// The page's own preferences (L-TS-26): one IndexedDB database, one store, one record — the theme. It is read
// before the first paint and never holds a key, a token or an id; the worker's store is not touched.

export type ThemePref = 'system' | 'signal' | 'light' | 'high-contrast';

export const PREFS_DB = 'dilla-ui';
export const PREFS_DB_VERSION = 1;
export const PREFS_STORE = 'prefs';
/** A stuck IndexedDB never holds the first paint longer than this. */
export const PREFS_TIMEOUT_MS = 500;

const THEME_KEY = 'theme';
const THEME_PREFS: readonly string[] = ['system', 'signal', 'light', 'high-contrast'];

export function isThemePref(value: unknown): value is ThemePref {
  return typeof value === 'string' && THEME_PREFS.includes(value);
}

function defaultIdb(): IDBFactory | undefined {
  return (globalThis as { indexedDB?: IDBFactory }).indexedDB;
}

/** Opens the database, creating the store on the first open; rejects on an open error. */
function openDb(idb: IDBFactory): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = idb.open(PREFS_DB, PREFS_DB_VERSION);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(PREFS_STORE)) db.createObjectStore(PREFS_STORE);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('E_PREFS'));
  });
}

async function readRecord(idb: IDBFactory): Promise<unknown> {
  const db = await openDb(idb);
  try {
    return await new Promise<unknown>((resolve, reject) => {
      const request = db.transaction(PREFS_STORE, 'readonly').objectStore(PREFS_STORE).get(THEME_KEY);
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error ?? new Error('E_PREFS'));
    });
  } finally {
    db.close();
  }
}

function themeOf(record: unknown): ThemePref {
  if (typeof record !== 'object' || record === null) return 'system';
  const { v, theme } = record as Record<string, unknown>;
  if (v !== 1) return 'system';
  // Mesh was removed; a preference stored before that reads as Signal (no DB version bump: the next write stores it).
  const current = theme === 'mesh' ? 'signal' : theme;
  return isThemePref(current) ? current : 'system';
}

/** The stored theme preference, or 'system' on any failure, a malformed record or no answer within the timeout. */
export function readTheme(idb: IDBFactory | undefined = defaultIdb()): Promise<ThemePref> {
  if (idb === undefined) return Promise.resolve('system');
  return new Promise<ThemePref>(resolve => {
    const timer = setTimeout(() => resolve('system'), PREFS_TIMEOUT_MS);
    const settle = (theme: ThemePref) => { clearTimeout(timer); resolve(theme); };
    // readRecord is async: a throw anywhere in it (open, transaction, get) arrives here as a rejection.
    readRecord(idb).then(record => settle(themeOf(record)), () => settle('system'));
  });
}

/** Stores the theme preference as the one record `{ v: 1, theme }`; any failure rejects with Error('E_PREFS'). */
export async function writeTheme(theme: ThemePref, idb: IDBFactory | undefined = defaultIdb()): Promise<void> {
  if (idb === undefined) throw new Error('E_PREFS');
  let db: IDBDatabase;
  try {
    db = await openDb(idb);
  } catch {
    throw new Error('E_PREFS');
  }
  try {
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(PREFS_STORE, 'readwrite');
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error ?? new Error('E_PREFS'));
      tx.onabort = () => reject(tx.error ?? new Error('E_PREFS'));
      const request = tx.objectStore(PREFS_STORE).put({ v: 1, theme }, THEME_KEY);
      request.onerror = () => reject(request.error ?? new Error('E_PREFS'));
    });
  } catch {
    throw new Error('E_PREFS');
  } finally {
    db.close();
  }
}
