import { describe, expect, it } from 'vitest';
import { SliceStore } from './store';
import type { AccountState, CommunitySummary, SliceName, TrayItem } from './types';

function account(phase: AccountState['phase']): AccountState {
  return { phase, instance: null, user: null, deviceId: null, recoveryKey: null, error: null, signIn: null, rootMismatch: false, ownTrust: null, factorSetup: null };
}

function recording() {
  const posted: { name: SliceName; rev: number; value: unknown }[] = [];
  const store = new SliceStore((name, rev, value) => posted.push({ name, rev, value }));
  return { store, posted };
}

describe('SliceStore', () => {
  it('publishes only a boolean for pending attachments across every retained tray', () => {
    const { store, posted } = recording();
    const file: TrayItem = { id: 'file', name: 'note.txt', size: 4, mime: 'text/plain', image: false, phase: 'preparing', reason: '' };
    store.set('tray:a', [file]);
    store.set('tray:b', [file]);
    store.set('tray:a', []);
    store.set('tray:b', [{ ...file, phase: 'ready' }]);
    // Sending/discarding one conversation must not clear another conversation's guard.
    expect(store.get('pendingAttachments')).toBe(true);
    expect(posted.filter(p => p.name === 'pendingAttachments').map(p => p.value)).toEqual([true]);
    store.set('tray:b', []);
    expect(store.get('pendingAttachments')).toBe(false);
    // Restored files participate too, and clearing every tray (wipe/reset) releases the guard.
    store.set('tray:a', [file]);
    store.set('tray:b', [file]);
    store.set('tray:a', []);
    store.set('tray:b', []);
    expect(posted.filter(p => p.name === 'pendingAttachments').map(p => p.value)).toEqual([true, false, true, false]);
  });

  it('publishes every change with a revision per slice name that starts at 1', () => {
    const { store, posted } = recording();
    store.set('account', account('loading'));
    store.set('account', account('needs-signup'));
    store.set('communities', []);
    expect(posted.map((p) => [p.name, p.rev])).toEqual([['account', 1], ['account', 2], ['communities', 1]]);
    expect(posted[1]?.value).toEqual(account('needs-signup'));
    expect(store.rev('account')).toBe(2);
    expect(store.rev(`timeline:${'a'.repeat(32)}`)).toBe(0);
    expect(store.get('account')?.phase).toBe('needs-signup');
  });

  it('does not publish a value equal to the one it holds', () => {
    const { store, posted } = recording();
    store.set('connection', { status: 'offline', generation: null });
    store.set('connection', { status: 'offline', generation: null });
    expect(posted).toHaveLength(1);
    expect(store.rev('connection')).toBe(1);
  });

  it('stores a deep-frozen copy, so neither the caller nor a reader can change what was published', () => {
    const { store } = recording();
    const list: CommunitySummary[] = [{ id: 'a'.repeat(32), name: 'one', owner: 'ee'.repeat(16) }];
    store.set('communities', list);
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-type-assertion -- the brief's test, kept as written
    list[0]!.name = 'changed';
    list.push({ id: 'b'.repeat(32), name: 'two', owner: 'ee'.repeat(16) });
    const held = store.get('communities')!;
    expect(held).toEqual([{ id: 'a'.repeat(32), name: 'one', owner: 'ee'.repeat(16) }]);
    expect(Object.isFrozen(held)).toBe(true);
    expect(Object.isFrozen(held[0])).toBe(true);
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-type-assertion -- the brief's test, kept as written
    expect(() => (held as CommunitySummary[]).push({ id: 'c'.repeat(32), name: 'three', owner: 'ee'.repeat(16) })).toThrow(TypeError);
  });

  it('returns the same object until the slice changes', () => {
    const { store } = recording();
    store.set('communities', []);
    const first = store.get('communities');
    store.set('communities', []);
    expect(store.get('communities')).toBe(first);
    store.set('communities', [{ id: 'a'.repeat(32), name: 'x', owner: 'ee'.repeat(16) }]);
    expect(store.get('communities')).not.toBe(first);
  });
});
