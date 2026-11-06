import { afterEach, describe, expect, it, vi } from 'vitest';
import type { MobileBridge, MobileInfo } from '../../mobile/src/contract.ts';
import { MOBILE_INFO_TIMEOUT_MS, mobileBridge, mobileInfoWithin } from './mobile.ts';

afterEach(() => vi.unstubAllGlobals());

describe('mobileBridge', () => {
  it('is undefined without the global', () => {
    expect(mobileBridge()).toBeUndefined();
  });
  it('returns the injected object', () => {
    const bridge = {};
    vi.stubGlobal('dillaMobile', bridge);
    expect(mobileBridge()).toBe(bridge);
  });
});

// A lost native answer (the Capacitor Android reply-proxy race) must end in the error splash, not a blank page.
describe('mobileInfoWithin', () => {
  const info: MobileInfo = { platform: 'android', version: '1', instance: 'https://a.example/' };
  const bridge = (answer: Promise<MobileInfo>) => ({ info: () => answer }) as unknown as MobileBridge;
  afterEach(() => vi.useRealTimers());

  it('answers the bridge info when the shell answers in time', async () => {
    await expect(mobileInfoWithin(bridge(Promise.resolve(info)))).resolves.toEqual(info);
  });
  it('answers timeout after MOBILE_INFO_TIMEOUT_MS when the shell never answers', async () => {
    vi.useFakeTimers();
    const result = mobileInfoWithin(bridge(new Promise(() => {})));
    let settled: unknown;
    void result.then(v => { settled = v; });
    await vi.advanceTimersByTimeAsync(MOBILE_INFO_TIMEOUT_MS - 1);
    expect(settled).toBeUndefined();
    await vi.advanceTimersByTimeAsync(1);
    expect(settled).toBe('timeout');
  });
  it('turns a refusal into failed, so boot shows the splash', async () => {
    await expect(mobileInfoWithin(bridge(Promise.reject(new Error('no'))))).resolves.toBe('failed');
  });
});
