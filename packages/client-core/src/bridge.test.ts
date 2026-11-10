import { describe, expect, it, vi } from 'vitest';
import { CoreCallError, connectCore } from './bridge';
import { CALL_IDLE, type AccountState, type CallState } from './state/types';

class FakeWorker {
  readonly posted: unknown[] = [];
  terminated = false;
  private readonly listeners: ((e: MessageEvent) => void)[] = [];
  postMessage(message: unknown): void { this.posted.push(message); }
  terminate(): void { this.terminated = true; }
  addEventListener(type: string, listener: (e: MessageEvent) => void): void {
    if (type === 'message') this.listeners.push(listener);
  }
  removeEventListener(): void {}
  emit(data: unknown): void { for (const l of this.listeners) l({ data } as MessageEvent); }
}

function account(phase: AccountState['phase']): AccountState {
  return { phase, instance: null, user: null, deviceId: null, recoveryKey: null, error: null, signIn: null, rootMismatch: false, ownTrust: null, factorSetup: null };
}

function connected() {
  const worker = new FakeWorker();
  return { worker, client: connectCore(worker as unknown as Worker) };
}

describe('connectCore', () => {
  it("posts each call with a fresh id and resolves it with the worker's value", async () => {
    const { worker, client } = connected();
    const first = client.call({ m: 'joinCommunity', invite: 'code' });
    const second = client.call({ m: 'selectCommunity', communityId: 'a'.repeat(32) });
    expect(worker.posted).toEqual([
      { t: 'call', id: 1, command: { m: 'joinCommunity', invite: 'code' } },
      { t: 'call', id: 2, command: { m: 'selectCommunity', communityId: 'a'.repeat(32) } },
    ]);
    worker.emit({ t: 'ret', id: 2, ok: true, value: null });
    worker.emit({ t: 'ret', id: 1, ok: true, value: { communityId: 'b'.repeat(32) } });
    await expect(first).resolves.toEqual({ communityId: 'b'.repeat(32) });
    await expect(second).resolves.toBeNull();
  });

  it("rejects with the worker's error: code, detail, status and wait", async () => {
    const { worker, client } = connected();
    const pending = client.call({ m: 'send', channelId: 'a'.repeat(32), text: 'x' });
    const later = client.call({ m: 'joinCommunity', invite: 'code' });
    worker.emit({ t: 'ret', id: 1, ok: false, error: { code: 'E_NOT_READY', detail: 'the phase is needs-signup', status: 0, retryAfterMs: null } });
    worker.emit({ t: 'ret', id: 2, ok: false, error: { code: 'E_RATE_LIMITED', detail: '', status: 429, retryAfterMs: 4200 } });
    const error: unknown = await pending.catch((e: unknown) => e);
    expect(error).toBeInstanceOf(CoreCallError);
    expect(error).toMatchObject({
      name: 'CoreCallError', code: 'E_NOT_READY', detail: 'the phase is needs-signup', status: 0, retryAfterMs: null,
      message: 'E_NOT_READY: the phase is needs-signup',
    });
    const limited: unknown = await later.catch((e: unknown) => e);
    expect(limited).toMatchObject({ code: 'E_RATE_LIMITED', detail: '', status: 429, retryAfterMs: 4200, message: 'E_RATE_LIMITED' });
    expect(new CoreCallError({ code: 'E_X', detail: 'd', status: 409, retryAfterMs: null })).toMatchObject({ code: 'E_X', detail: 'd', status: 409, message: 'E_X: d' });
  });

  it("carries a worker refusal's reason to the page, and only a string one", async () => {
    const { worker, client } = connected();
    const first = client.call({ m: 'joinCommunity', invite: 'code' });
    const second = client.call({ m: 'joinCommunity', invite: 'code' });
    worker.emit({ t: 'ret', id: 1, ok: false, error: { code: 'E_BAD_INPUT', detail: '', status: 0, retryAfterMs: null, reason: 'announce.noHeldRole' } });
    worker.emit({ t: 'ret', id: 2, ok: false, error: { code: 'E_BAD_INPUT', detail: '', status: 0, retryAfterMs: null, reason: 7 } });
    expect(await first.catch((e: unknown) => e)).toMatchObject({ code: 'E_BAD_INPUT', reason: 'announce.noHeldRole' });
    expect(await second.catch((e: unknown) => e)).toHaveProperty('reason', undefined);
  });

  it('ignores an answer to an id it never sent and a message of no known kind', () => {
    const { worker, client } = connected();
    expect(() => worker.emit({ t: 'ret', id: 99, ok: true, value: 1 })).not.toThrow();
    expect(() => worker.emit({ t: 'nonsense' })).not.toThrow();
    expect(client.get('account')).toBeUndefined();
  });

  it('keeps the newest revision of each slice and notifies its listeners once per accepted revision', () => {
    const { worker, client } = connected();
    let calls = 0;
    const off = client.subscribe('account', () => { calls += 1; });
    worker.emit({ t: 'slice', name: 'account', rev: 1, value: account('loading') });
    const held = client.get('account');
    expect(held?.phase).toBe('loading');
    expect(client.get('account')).toBe(held);
    worker.emit({ t: 'slice', name: 'account', rev: 3, value: account('ready') });
    worker.emit({ t: 'slice', name: 'account', rev: 2, value: account('other-tab') });
    worker.emit({ t: 'slice', name: 'account', rev: 3, value: account('error') });
    expect(client.get('account')?.phase).toBe('ready');
    expect(calls).toBe(2);
    off();
    worker.emit({ t: 'slice', name: 'account', rev: 4, value: account('revoked') });
    expect(calls).toBe(2);
    expect(client.get('account')?.phase).toBe('revoked');
    expect(client.get('communities')).toBeUndefined();
  });

  it('dispose terminates the worker and rejects pending and later calls with E_DISPOSED', async () => {
    const { worker, client } = connected();
    const pending = client.call({ m: 'start' });
    client.dispose();
    expect(worker.terminated).toBe(true);
    await expect(pending).rejects.toMatchObject({ code: 'E_DISPOSED', detail: '', status: 0, retryAfterMs: null });
    await expect(client.call({ m: 'start' })).rejects.toMatchObject({ code: 'E_DISPOSED', detail: '', status: 0, retryAfterMs: null });
    expect(worker.posted).toHaveLength(1);
  });
});

// I2 (controller ruling, 2026-10-13): CallState names the changed contact in `changedUser` (32-hex user id or null), so
// the page can fill {name} in voice.paused and voice.blockedChanged even when a refused join left `roster` empty.
describe('the call slice', () => {
  it('starts with no changed user and no rejoin wait', () => {
    expect(CALL_IDLE).toEqual({ phase: 'idle', channelId: null, callId: null, epoch: null, code: null, error: null, roster: [],
      outgoing: null, changedUser: null, rejoining: false });
  });

  it('reaches the page with changedUser intact', () => {
    const { bridge, deliver } = createBridgeForTest();
    const paused: CallState = { ...CALL_IDLE, phase: 'paused', channelId: 'cc'.repeat(16), callId: 'cc'.repeat(16), epoch: '4',
      error: { code: 'E_UMK_CHANGED', detail: '', status: 0, retryAfterMs: null }, changedUser: 'dd'.repeat(16) };
    deliver({ t: 'slice', name: 'call', rev: 1, value: paused });
    expect(bridge.get('call')?.changedUser).toBe('dd'.repeat(16));
    expect(bridge.get('call')?.error?.code).toBe('E_UMK_CHANGED');
  });
});

/** The real connectCore over a fake Worker whose deliver dispatches a message event (L-TS-52). */
function createBridgeForTest() {
  const worker = new FakeWorker();
  const bridge = connectCore(worker as unknown as Worker);
  return { bridge, deliver: (data: unknown): void => { worker.emit(data); } };
}

// The call key path (decision 3, L-TS-52). Attacker: anything that later reads the page's memory or a
// listener that keeps the buffer; the bridge hands the key over synchronously and then zero-fills the
// transferred buffer, so the only live copy is the one a listener made itself.
describe('onCallKeys', () => {
  const keys = (baseKey: ArrayBuffer) => ({ groupId: 'bb'.repeat(16), epoch: '3', baseKey, selfLeaf: 1, roster: [] });

  it('hands the key to every listener synchronously, then zero-fills the transferred buffer', () => {
    const { bridge, deliver } = createBridgeForTest();
    const seen: number[][] = [];
    bridge.onCallKeys((callId, keys) => { expect(callId).toBe('aa'.repeat(16)); seen.push([...new Uint8Array(keys.baseKey)]); });
    const baseKey = new Uint8Array(16).fill(7).buffer;
    deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: { groupId: 'bb'.repeat(16), epoch: '3', baseKey, selfLeaf: 1, roster: [] } });
    expect(seen).toEqual([Array(16).fill(7)]);
    expect([...new Uint8Array(baseKey)]).toEqual(Array(16).fill(0));
  });

  it('drops a callKeys message whose key is not 16 bytes', () => {
    const { bridge, deliver } = createBridgeForTest();
    const fn = vi.fn(); bridge.onCallKeys(fn);
    deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: { groupId: 'bb'.repeat(16), epoch: '3', baseKey: new ArrayBuffer(15), selfLeaf: 1, roster: [] } });
    expect(fn).not.toHaveBeenCalled();
  });

  it('zero-fills and drops a key nobody listens for, and a throwing listener stops neither the next one nor the fill', () => {
    const { bridge, deliver } = createBridgeForTest();
    const unheard = new Uint8Array(16).fill(9).buffer;
    deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: keys(unheard) });
    expect([...new Uint8Array(unheard)]).toEqual(Array(16).fill(0));

    const rethrown: unknown[] = [];
    const micro = vi.spyOn(globalThis, 'queueMicrotask').mockImplementation((cb: () => void) => {
      try { cb(); } catch (err) { rethrown.push(err); }
    });
    try {
      const boom = new Error('listener failed');
      bridge.onCallKeys(() => { throw boom; });
      const second: number[][] = [];
      bridge.onCallKeys((_, k) => { second.push([...new Uint8Array(k.baseKey)]); });
      const baseKey = new Uint8Array(16).fill(5).buffer;
      deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: keys(baseKey) });
      expect(second).toEqual([Array(16).fill(5)]);
      expect([...new Uint8Array(baseKey)]).toEqual(Array(16).fill(0));
      expect(rethrown).toEqual([boom]);
    } finally {
      micro.mockRestore();
    }
  });

  it('accepts the largest u64 epoch and refuses one above it', () => {
    const { bridge, deliver } = createBridgeForTest();
    const epochs: string[] = [];
    bridge.onCallKeys((_, k) => { epochs.push(k.epoch); });
    deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: { ...keys(new ArrayBuffer(16)), epoch: '18446744073709551615' } });
    const over = new Uint8Array(16).fill(2).buffer;
    deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: { ...keys(over), epoch: '18446744073709551616' } });
    expect(epochs).toEqual(['18446744073709551615']);
    expect([...new Uint8Array(over)]).toEqual(Array(16).fill(0));
  });

  it('refuses malformed ids, epochs and roster rows, and stops calling a removed listener', () => {
    const { bridge, deliver } = createBridgeForTest();
    const fn = vi.fn();
    const off = bridge.onCallKeys(fn);
    const good = { groupId: 'bb'.repeat(16), epoch: '3', selfLeaf: 1, roster: [{ leaf: 0, deviceId: 'cc'.repeat(16), userId: 'dd'.repeat(16) }] };
    for (const [callId, over] of [
      ['AA'.repeat(16), {}], ['aa'.repeat(15), {}], ['aa'.repeat(16), { groupId: 'b' }], ['aa'.repeat(16), { epoch: '03' }],
      ['aa'.repeat(16), { epoch: '-1' }], ['aa'.repeat(16), { epoch: 3 }], ['aa'.repeat(16), { selfLeaf: -1 }],
      ['aa'.repeat(16), { roster: [{ leaf: 0, deviceId: 'x', userId: 'dd'.repeat(16) }] }], ['aa'.repeat(16), { roster: null }],
    ] as const) {
      const baseKey = new Uint8Array(16).fill(3).buffer;
      deliver({ t: 'callKeys', callId, keys: { ...good, ...over, baseKey } });
      expect([...new Uint8Array(baseKey)]).toEqual(Array(16).fill(0));
    }
    expect(fn).not.toHaveBeenCalled();
    deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: { ...good, baseKey: new Uint8Array(16).fill(1).buffer } });
    expect(fn).toHaveBeenCalledOnce();
    off();
    deliver({ t: 'callKeys', callId: 'aa'.repeat(16), keys: { ...good, baseKey: new Uint8Array(16).fill(1).buffer } });
    expect(fn).toHaveBeenCalledOnce();
  });
});
