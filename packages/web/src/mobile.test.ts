import { afterEach, describe, expect, it, vi } from 'vitest';
import { mobileBridge } from './mobile.ts';

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
