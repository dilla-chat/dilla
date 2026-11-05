import { afterEach, expect, it, vi } from 'vitest';
import { workerInstanceURL } from './instance-url';

it('binds desktop HTTP and gateway configuration to the complete same-origin instance base', () => {
  expect(workerInstanceURL('https://chat.test/nested/', 'https://chat.test')).toBe('https://chat.test/nested');
  expect(workerInstanceURL(undefined, 'https://chat.test')).toBe('https://chat.test');
  expect(workerInstanceURL('https://chat.test/', 'https://chat.test')).toBe('https://chat.test');
  for (const input of ['https://evil.test/nested/', 'https://user:password@chat.test/', 'https://chat.test/?secret=x', 'https://chat.test/#x', 'https://chat.test/nested/%2fother', 'https://chat.test/nested\\other']) {
    expect(() => workerInstanceURL(input, 'https://chat.test')).toThrow();
  }
});

// The mobile branch exists only in a build that defines __DILLA_MOBILE__ true (packages/mobile). Vitest has no define,
// so these cases set the global and load a fresh copy of the module.
async function mobileBuild(): Promise<typeof import('./instance-url')> {
  vi.stubGlobal('__DILLA_MOBILE__', true);
  vi.resetModules();
  return import('./instance-url');
}
afterEach(() => { vi.unstubAllGlobals(); vi.resetModules(); });

it('in the mobile build, a mobile app origin binds to an HTTPS instance origin and nothing looser', async () => {
  const { workerInstanceURL: mobile } = await mobileBuild();
  for (const app of ['capacitor://localhost', 'https://localhost']) {
    expect(mobile('https://chat.example/', app)).toBe('https://chat.example');
    expect(mobile('https://chat.example:8443', app)).toBe('https://chat.example:8443');
    for (const input of ['http://chat.example/', 'wss://chat.example/', 'capacitor://chat.example/', 'https://u:p@chat.example/',
      'https://chat.example/?q', 'https://chat.example/#x', 'https://chat.example/team/', 'https://chat.example/assets/',
      'https://10.0.2.2:18443/', 'https://127.0.0.1/', 'https://[::1]/', 'https://[2001:db8::1]:8443/', 'https://chat.example./',
      // round 2 item 4: the app's own origin and every *.localhost name
      'https://localhost/', 'https://localhost:8443/', 'https://chat.localhost/', 'https://LOCALHOST/', 'https://a.b.localhost:443/']) {
      expect(() => mobile(input, app), input).toThrow();
    }
  }
  // Any other worker origin stays same-origin only, in the mobile build too.
  for (const origin of ['https://chat.test', 'http://localhost', 'https://localhost:8443', 'capacitor://evil.test']) {
    expect(() => mobile('https://chat.example/', origin)).toThrow();
  }
});

it('outside the mobile build (web, desktop) a mobile app origin gets no cross-origin instance', () => {
  for (const app of ['capacitor://localhost', 'https://localhost']) expect(() => workerInstanceURL('https://chat.example/', app)).toThrow();
});
