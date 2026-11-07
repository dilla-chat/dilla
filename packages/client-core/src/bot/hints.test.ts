import { describe, expect, it } from 'vitest';
import { BotHints, watchBotHint, type HintClient, type HintScope } from './hints';

const CH = 'c'.repeat(32);
const CALL = 'd'.repeat(32);
const COMMUNITY = 'a'.repeat(32);

function rig(visible: (scope: HintScope) => boolean = () => true) {
  let clock = 0;
  const fetched: HintScope[] = [];
  const timers: { at: number; fn: () => void; live: boolean }[] = [];
  const hints = new BotHints({
    fetch: (scope) => { fetched.push(scope); return Promise.resolve(); },
    now: () => clock,
    setTimer: (fn, ms) => { const t = { at: clock + ms, fn, live: true }; timers.push(t); return () => { t.live = false; }; },
    visible,
  });
  const advance = (to: number) => {
    clock = to;
    for (const t of timers) if (t.live && t.at <= to) { t.live = false; t.fn(); }
  };
  return { hints, fetched, advance };
}

describe('BotHints', () => {
  it('a burst of hints for one scope fetches once per 250 ms', () => {
    const r = rig();
    for (let i = 1; i <= 10; i++) { r.advance(i * 10); r.hints.onHint({ kind: 1, id: CH, revision: BigInt(i) }); }
    expect(r.fetched).toHaveLength(1);
    r.advance(249);
    expect(r.fetched).toHaveLength(1);
    r.advance(260);
    expect(r.fetched).toHaveLength(2);
    r.advance(1000);
    expect(r.fetched).toHaveLength(2);
    r.hints.onHint({ kind: 1, id: CH, revision: 11n });
    expect(r.fetched).toHaveLength(3);
  });

  it('keeps scopes apart', () => {
    const r = rig();
    r.hints.onHint({ kind: 1, id: CH, revision: 1n });
    r.hints.onHint({ kind: 2, id: CALL, revision: 1n });
    expect(r.fetched).toEqual([{ kind: 1, id: CH }, { kind: 2, id: CALL }]);
  });

  it('ignores a hint that is not newer than the last seen one', () => {
    const r = rig();
    r.hints.onHint({ kind: 1, id: CH, revision: 5n });
    r.advance(500);
    r.hints.onHint({ kind: 1, id: CH, revision: 5n });
    r.hints.onHint({ kind: 1, id: CH, revision: 4n });
    expect(r.fetched).toHaveLength(1);
  });

  it('a hint for a channel not shown fetches nothing', () => {
    const r = rig((s) => s.kind === 0);
    r.hints.onHint({ kind: 1, id: CH, revision: 1n });
    r.hints.onHint({ kind: 2, id: CALL, revision: 1n });
    r.advance(1000);
    expect(r.fetched).toEqual([]);
  });

  it('a user hint always fetches reminders.due', () => {
    const r = rig(() => false);
    r.hints.onHint({ kind: 0, id: COMMUNITY, revision: 1n });
    expect(r.fetched).toEqual([{ kind: 0, id: COMMUNITY }]);
  });

  it('a device that was offline fetches once on connect when behind', () => {
    const r = rig();
    r.hints.onHint({ kind: 1, id: CH, revision: 3n });
    r.advance(1000);
    r.fetched.length = 0;
    r.hints.onConnect([
      { kind: 1, scopeId: CH, revision: 3n },        // not behind
      { kind: 2, scopeId: CALL, revision: 7n },      // never seen: behind
      { kind: 3, scopeId: COMMUNITY, revision: 9n }, // the catalog is the catalog store's
    ]);
    expect(r.fetched).toEqual([{ kind: 2, id: CALL }]);
    r.hints.onConnect([{ kind: 1, scopeId: CH, revision: 4n }]);
    expect(r.fetched).toEqual([{ kind: 2, id: CALL }, { kind: 1, id: CH }]);
    r.advance(5000);
    r.hints.onConnect([{ kind: 1, scopeId: CH, revision: 4n }, { kind: 2, scopeId: CALL, revision: 7n }]);
    expect(r.fetched).toHaveLength(2);
  });

  it('a reminder that fell due while every device was offline shows on the next connect', () => {
    const r = rig(() => false);
    // This device never saw a user hint; the instance holds revision 2 for the community.
    r.hints.onConnect([{ kind: 0, scopeId: COMMUNITY, revision: 2n }]);
    expect(r.fetched).toEqual([{ kind: 0, id: COMMUNITY }]);
  });

  it('clear drops pending fetches and the stored revisions', () => {
    const r = rig();
    r.hints.onHint({ kind: 1, id: CH, revision: 1n });
    r.advance(10);
    r.hints.onHint({ kind: 1, id: CH, revision: 2n });
    r.hints.clear();
    r.advance(1000);
    expect(r.fetched).toHaveLength(1);
    r.hints.onHint({ kind: 1, id: CH, revision: 1n });
    expect(r.fetched).toHaveLength(2);
  });
});

describe('watchBotHint', () => {
  function fakeClient() {
    let slice: Record<string, number> = {};
    const listeners = new Set<() => void>();
    const calls: unknown[] = [];
    const client: HintClient = {
      call: (c) => { calls.push(c); return Promise.resolve(null); },
      get: () => slice,
      subscribe: (_n, l) => { listeners.add(l); return () => { listeners.delete(l); }; },
    };
    const bump = (key: string) => { slice = { ...slice, [key]: (slice[key] ?? 0) + 1 }; for (const l of [...listeners]) l(); };
    return { client, bump, calls, listeners };
  }

  it('calls back when its channel scope rises and registers the scope as shown', () => {
    const f = fakeClient();
    let n = 0;
    const stop = watchBotHint(f.client, { kind: 1, id: CH }, () => { n++; });
    expect(f.calls).toEqual([{ m: 'watchBotScope', kind: 1, channelId: CH, on: true }]);
    f.bump(`1:${'e'.repeat(32)}`);
    expect(n).toBe(0);
    f.bump(`1:${CH}`);
    expect(n).toBe(1);
    stop();
    f.bump(`1:${CH}`);
    expect(n).toBe(1);
    expect(f.calls[1]).toEqual({ m: 'watchBotScope', kind: 1, channelId: CH, on: false });
    expect(f.listeners.size).toBe(0);
  });

  it('the user scope follows any community and registers nothing', () => {
    const f = fakeClient();
    let n = 0;
    watchBotHint(f.client, { kind: 0 }, () => { n++; });
    f.bump(`0:${COMMUNITY}`);
    f.bump(`1:${CH}`);
    expect(n).toBe(1);
    expect(f.calls).toEqual([]);
  });

  it('a signal already in the slice at watch time is not a hint', () => {
    const f = fakeClient();
    f.bump(`2:${CALL}`);
    let n = 0;
    watchBotHint(f.client, { kind: 2, id: CALL }, () => { n++; });
    f.bump(`1:${CH}`);
    expect(n).toBe(0);
    f.bump(`2:${CALL}`);
    expect(n).toBe(1);
  });
});
