// A KeyPackage publish failure is never silent: it is shown in diagnostics, retried with backoff, re-checked after a
// gateway reconnect, and a recovery-key enrolment publishes its first batch before it is ready.
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { DiagnosticsState } from '../state/types';
import { PASSWORD, READY_INFO, RECOVERY_KEY, SUBMIT, refusal, toReady, world, type World } from './world';

afterEach(() => { vi.useRealTimers(); });

const diag = (w: World): DiagnosticsState['keyPackages'] | undefined => w.last<DiagnosticsState>('diagnostics')?.keyPackages;
const ready = (w: World, keypackagesRemaining: number): void => { w.gateway.emit({ type: 'ready', info: { ...READY_INFO, keypackagesRemaining } }); };
const status = (w: World, s: 'waiting' | 'connecting' | 'ready'): void => { w.gateway.emit({ type: 'status', status: s, closeCode: null }); };
const settle = async (ms = 0): Promise<void> => { await vi.advanceTimersByTimeAsync(ms); };

/** A ready world with fake timers armed afterwards, so the harness's own waits stay real. */
async function readyWorld(): Promise<World> {
  const w = world();
  await toReady(w);
  w.routes.postKeyPackages.mockClear();
  vi.useFakeTimers();
  return w;
}

describe('a failed publish is recorded and retried with backoff', () => {
  it.each([[403, 'E_FORBIDDEN'], [500, 'E_INTERNAL']])('a %i is shown, retried at 5 s, 10 s, 20 s ... capped at 5 min, until it succeeds', async (code, name) => {
    const w = await readyWorld();
    w.routes.postKeyPackages.mockImplementation(() => Promise.reject(refusal(code, name)));
    ready(w, 1);
    await settle();
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
    expect(diag(w)).toMatchObject({ lastError: name, lastAttempt: 1_700_000_000_000 });
    let calls = 1;
    for (const wait of [5, 10, 20, 40, 80, 160, 300, 300]) {
      await settle(wait * 1000 - 1);
      expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(calls);
      await settle(1);
      expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(++calls);
      expect(diag(w)?.lastError).toBe(name);
    }
    w.routes.postKeyPackages.mockImplementation(() => Promise.resolve());
    await settle(300_000);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(calls + 1);
    expect(diag(w)).toEqual({ count: 32, lastAttempt: 1_700_000_000_000, lastError: null });
    await settle(600_000);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(calls + 1);
  });

  it('a success resets the backoff to 5 s', async () => {
    const w = await readyWorld();
    w.routes.postKeyPackages.mockImplementationOnce(() => Promise.reject(refusal(500, 'E_INTERNAL')))
      .mockImplementationOnce(() => Promise.reject(refusal(500, 'E_INTERNAL')));
    ready(w, 1);
    await settle(5_000 + 10_000);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(3);
    expect(diag(w)?.lastError).toBeNull();
    w.routes.postKeyPackages.mockImplementationOnce(() => Promise.reject(refusal(500, 'E_INTERNAL')));
    ready(w, 1);
    await settle(4_999);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(4);
    await settle(1);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(5);
  });

  it('the retry stops when the session ends', async () => {
    const w = await readyWorld();
    w.routes.postKeyPackages.mockImplementation(() => Promise.reject(refusal(403, 'E_FORBIDDEN')));
    ready(w, 1);
    await settle();
    w.session.establish.mockResolvedValueOnce(false);
    w.gateway.emit({ type: 'revoked' });
    await settle();
    expect(w.account()?.phase).toBe('revoked');
    const before = w.routes.postKeyPackages.mock.calls.length;
    await settle(900_000);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(before);
  });

  it('the retry stops when the worker lets go of the account (sign-out, resign)', async () => {
    const w = await readyWorld();
    w.routes.postKeyPackages.mockImplementation(() => Promise.reject(refusal(403, 'E_FORBIDDEN')));
    ready(w, 1);
    await settle();
    w.call(92, { m: 'resign', nonce: 'aa'.repeat(16) });
    await settle();
    expect(w.ret(92)).toMatchObject({ ok: true });
    const before = w.routes.postKeyPackages.mock.calls.length;
    await settle(900_000);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(before);
    expect(diag(w)).toEqual({ count: null, lastAttempt: null, lastError: null });
  });
});

describe('a gateway reconnect re-checks the count', () => {
  async function reconnect(w: World, remaining: number): Promise<void> {
    status(w, 'waiting');
    status(w, 'connecting');
    status(w, 'ready');
    ready(w, remaining);
    await settle();
  }

  it('refills from the server count when it is below the threshold, whatever ready said', async () => {
    const w = await readyWorld();
    w.routes.getKeyPackageCount.mockResolvedValue({ ordinary: 3, fallback: true });
    await reconnect(w, 32);
    expect(w.routes.getKeyPackageCount).toHaveBeenCalledTimes(1);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
    expect(w.core.keyPackages).toHaveBeenLastCalledWith(29, false);
    expect(diag(w)).toMatchObject({ count: 32, lastError: null });
  });

  it('does nothing at or above the threshold', async () => {
    const w = await readyWorld();
    w.routes.getKeyPackageCount.mockResolvedValue({ ordinary: 8, fallback: true });
    await reconnect(w, 1);
    expect(w.routes.postKeyPackages).not.toHaveBeenCalled();
    expect(diag(w)?.count).toBe(8);
  });

  it('a first ready, with no reconnect before it, trusts ready and asks nothing', async () => {
    const w = await readyWorld();
    ready(w, 1);
    await settle();
    expect(w.routes.getKeyPackageCount).not.toHaveBeenCalled();
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
  });

  it('an older server without the route leaves the count unknown: ready\'s own count stays the fallback', async () => {
    const w = await readyWorld();
    w.routes.getKeyPackageCount.mockResolvedValue(null);
    await reconnect(w, 32);
    expect(w.routes.postKeyPackages).not.toHaveBeenCalled();
    expect(diag(w)).toMatchObject({ count: 32, lastError: null });
    await reconnect(w, 2);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
  });

  it('a failed count read is recorded and retried', async () => {
    const w = await readyWorld();
    w.routes.getKeyPackageCount.mockRejectedValueOnce(refusal(500, 'E_INTERNAL'));
    await reconnect(w, 32);
    expect(diag(w)?.lastError).toBe('E_INTERNAL');
    w.routes.getKeyPackageCount.mockResolvedValue({ ordinary: 0, fallback: false });
    await settle(5_000);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
    expect(w.core.keyPackages).toHaveBeenLastCalledWith(32, true);
  });
});

describe('the first batch of a new device', () => {
  it('a recovery-key enrolment posts 32 and a last-resort package before sign-in resolves', async () => {
    const w = world({ phase: 0 });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
    w.call(2, { m: 'signInBegin' });
    await vi.waitFor(() => expect(w.ret(2)).toBeDefined());
    w.call(3, { m: 'signInLogin', username: ' web ', password: PASSWORD });
    await vi.waitFor(() => expect(w.ret(3)).toBeDefined());
    w.call(4, { m: 'signInKey', recoveryKey: RECOVERY_KEY });
    await vi.waitFor(() => expect(w.ret(4)).toEqual({ t: 'ret', id: 4, ok: true, value: null }));
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
    expect(w.core.keyPackages).toHaveBeenCalledWith(32, true);
  });

  it('a signup that cannot publish still reaches ready, shows the refusal and retries', async () => {
    const w = world({ phase: 0 });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
    w.call(2, { m: 'signupBegin' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('signup-keys'));
    w.routes.postKeyPackages.mockImplementationOnce(() => Promise.reject(refusal(403, 'E_FORBIDDEN')));
    vi.useFakeTimers();
    w.call(3, SUBMIT);
    await settle(100);
    expect(w.ret(3)).toMatchObject({ ok: true });
    expect(w.account()?.phase).toBe('ready');
    expect(diag(w)).toMatchObject({ lastError: 'E_FORBIDDEN' });
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
    await settle(5_000);
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(2);
    expect(diag(w)).toMatchObject({ lastError: null, count: 32 });
  });
});
