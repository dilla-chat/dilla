import { describe, it, expect, vi, afterEach } from 'vitest';
import { FakeIdb } from './test/fake-idb.ts';
import { PREFS_DB, PREFS_DB_VERSION, PREFS_STORE, PREFS_TIMEOUT_MS, isThemePref, readTheme, writeTheme } from './prefs.ts';

afterEach(() => { vi.useRealTimers(); });

describe('the theme preference', () => {
  it('reads system when nothing is stored, and round-trips every preference', async () => {
    const idb = new FakeIdb();
    expect(await readTheme(idb.factory)).toBe('system');
    for (const theme of ['signal', 'system'] as const) {
      await writeTheme(theme, idb.factory);
      expect(await readTheme(idb.factory)).toBe(theme);
    }
    expect(idb.opened[0]).toEqual({ name: PREFS_DB, version: PREFS_DB_VERSION });
    expect(PREFS_DB).toBe('dilla-ui');
    expect(PREFS_DB_VERSION).toBe(1);
  });
  it('stores exactly one record, the theme, and nothing else', async () => {
    const idb = new FakeIdb();
    await writeTheme('signal', idb.factory);
    expect([...idb.stores.keys()]).toEqual([PREFS_STORE]);
    expect([...(idb.stores.get(PREFS_STORE) ?? new Map()).entries()]).toEqual([['theme', { v: 1, theme: 'signal' }]]);
  });
  it.each([
    ['a newer record', { v: 2, theme: 'light' }],
    ['an unknown theme', { v: 1, theme: 'neon' }],
    ['a bare string', 'light'],
    ['null', null],
  ])('reads system for %s', async (_, record) => {
    const idb = new FakeIdb();
    await writeTheme('signal', idb.factory);
    idb.stores.get(PREFS_STORE)?.set('theme', record);
    expect(await readTheme(idb.factory)).toBe('system');
  });
  it('reads a stored legacy mesh preference as signal, and mesh is no longer a preference', async () => {
    const idb = new FakeIdb();
    await writeTheme('signal', idb.factory);
    idb.stores.get(PREFS_STORE)?.set('theme', { v: 1, theme: 'mesh' });
    expect(await readTheme(idb.factory)).toBe('signal');
    expect(isThemePref('mesh')).toBe(false);
  });
  it('reads system without IndexedDB and when the open fails', async () => {
    expect(await readTheme(undefined)).toBe('system');
    const idb = new FakeIdb();
    idb.failOpen = true;
    expect(await readTheme(idb.factory)).toBe('system');
    await expect(writeTheme('signal', idb.factory)).rejects.toThrow('E_PREFS');
  });
  it('reads system when the open never answers, after the timeout', async () => {
    vi.useFakeTimers();
    const idb = new FakeIdb();
    idb.hang = true;
    const read = readTheme(idb.factory);
    await vi.advanceTimersByTimeAsync(PREFS_TIMEOUT_MS - 1);
    let settled = false;
    void read.then(() => { settled = true; });
    await Promise.resolve();
    expect(settled).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    await expect(read).resolves.toBe('system');
  });
  it('knows the catalog theme and legacy system preference', () => {
    expect(['system', 'signal'].every(isThemePref)).toBe(true);
    expect(['light', 'high-contrast'].some(isThemePref)).toBe(false);
    expect(isThemePref('dark')).toBe(false);
    expect(isThemePref(undefined)).toBe(false);
  });
});


it.each(['light', 'high-contrast'])('maps the retired %s preference to Signal without changing the stored record', async theme => {
 const idb = new FakeIdb(); await writeTheme('signal', idb.factory);
 idb.stores.get(PREFS_STORE)?.set('theme', { v: 1, theme });
 expect(await readTheme(idb.factory)).toBe('signal');
 expect(idb.stores.get(PREFS_STORE)?.get('theme')).toEqual({ v: 1, theme });
});
