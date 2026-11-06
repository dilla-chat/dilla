import { test, afterEach, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { browserAuth, CEREMONY_TIMEOUT_MS, type NativeHttp, type NativeHttpOptions } from '../src/browser-auth.ts';

// The desktop cases (packages/desktop/test/browser-auth.test.ts) ported to the native transport.
// Every reply is a CapacitorHttp-shaped { status, data, headers, url }.
const reply = (status: number, data: unknown = '') => ({ status, data, headers: {}, url: '' });
const httpOf = (handler: (options: NativeHttpOptions) => ReturnType<typeof reply> | Promise<ReturnType<typeof reply>>): NativeHttp => ({ request: async options => handler(options) });

// The WebView fetch would send Origin: capacitor://localhost or https://localhost, which /desktop-auth/ refuses.
let fetchCalls = 0;
const realFetch = globalThis.fetch;
beforeEach(() => { fetchCalls = 0; globalThis.fetch = (async () => { fetchCalls++; throw new Error('WebView fetch must not be used'); }) as typeof fetch; });
afterEach(() => { globalThis.fetch = realFetch; assert.equal(fetchCalls, 0, 'a desktop-auth request went through the WebView fetch'); });

test('browser ceremony uses fixed instance, private PKCE and single-use POST completion through native HTTP', async () => {
  const requests: { url: string; body: Record<string, string>; options: NativeHttpOptions }[] = [];
  let opened = '';
  const id = 'A'.repeat(43);
  const result = await browserAuth('https://chat.test/', { kind: 'get', options: '{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}' }, {
    open: async url => { opened = url; },
    http: httpOf(options => {
      requests.push({ url: options.url, body: options.data as Record<string, string>, options });
      assert.equal(options.method, 'POST');
      assert.equal(options.disableRedirects, true);
      assert.equal(options.headers['Content-Type'], 'application/json');
      assert.ok(!Object.keys(options.headers).some(name => name.toLowerCase() === 'origin'));
      return reply(200, options.url.endsWith('/request') ? { id } : { response: '{"id":"credential"}', needs: 0 });
    }),
  });
  assert.deepEqual(result, { response: '{"id":"credential"}', needs: 0 });
  assert.equal(opened, `https://chat.test/desktop-auth/${id}`);
  assert.equal(requests[0].url, 'https://chat.test/desktop-auth/request');
  assert.equal(requests[1].url, `${opened}/consume`);
  const verifier = requests[1].body.verifier;
  assert.match(verifier, /^[A-Za-z0-9_-]{43}$/);
  assert.equal(createHash('sha256').update(verifier).digest('base64url'), requests[0].body.challenge);
  // The verifier never occurs in a URL: not the browser navigation, not a request URL.
  assert.ok(!opened.includes(verifier));
  for (const request of requests) assert.ok(!request.url.includes(verifier));
});

test('a pending completion (202) is polled once a second until the reply arrives', async () => {
  const sleeps: number[] = [];
  let polls = 0;
  const result = await browserAuth('https://chat.test/', { kind: 'oidc', options: '{}' }, {
    open: async () => {},
    clock: { timeout: () => new AbortController().signal, sleep: async ms => { sleeps.push(ms); } },
    http: httpOf(options => {
      if (options.url.endsWith('/request')) return reply(200, { id: 'B'.repeat(43) });
      return ++polls < 3 ? reply(202) : reply(200, { assertion: 'C'.repeat(43), needs: 2 });
    }),
  });
  assert.deepEqual(result, { assertion: 'C'.repeat(43), needs: 2 });
  assert.deepEqual(sleeps, [1000, 1000]);
});

test('browser handoff rejects malformed replies and arbitrary capability arguments', async () => {
  let opened = false;
  const never = httpOf(() => { throw new Error('no request expected'); });
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'exec', options: '' }, { open: async () => { opened = true; }, http: never }), /Invalid browser ceremony/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '', extra: 1 }, { open: async () => { opened = true; }, http: never }), /Invalid browser ceremony/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: 'x'.repeat(65537) }, { open: async () => { opened = true; }, http: never }), /Invalid browser ceremony/);
  await assert.rejects(browserAuth('https://chat.test/', null, { open: async () => { opened = true; }, http: never }), /Invalid browser ceremony/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'oidc', options: '' }, { open: async () => { opened = true; }, http: httpOf(() => reply(200, { id: 'https://evil.test' })) }), /Invalid/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'oidc', options: '' }, { open: async () => { opened = true; }, http: httpOf(() => reply(200, '[1]')) }), /Invalid browser reply/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'oidc', options: '' }, { open: async () => { opened = true; }, http: httpOf(() => reply(200, 'not json')) }), /Invalid browser reply|JSON/);
  assert.equal(opened, false);
});

test('an oversize reply (over 70000 bytes) is refused', async () => {
  let opened = false;
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => { opened = true; }, http: httpOf(() => reply(200, { id: 'A'.repeat(43), pad: 'x'.repeat(70001) })),
  }), /^Error: Invalid browser reply$/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => { opened = true; }, http: httpOf(() => reply(200, JSON.stringify({ id: 'A'.repeat(43), pad: 'é'.repeat(35001) }))),
  }), /^Error: Invalid browser reply$/);
  assert.equal(opened, false);
});

test('an invalid completion is refused, and a server error string is passed through when short', async () => {
  const request = { kind: 'get', options: '{}' };
  const flow = (completion: unknown) => browserAuth('https://chat.test/', request, {
    open: async () => {}, http: httpOf(options => options.url.endsWith('/request') ? reply(200, { id: 'A'.repeat(43) }) : reply(200, completion)),
  });
  await assert.rejects(flow({ response: '{}', needs: 3 }), /^Error: Invalid browser completion$/);
  await assert.rejects(flow({ response: 1, needs: 0 }), /^Error: Invalid browser completion$/);
  await assert.rejects(flow({ error: 'Passkey not recognised' }), /^Error: Passkey not recognised$/);
  await assert.rejects(flow({ error: 'x'.repeat(129) }), /^Error: Browser sign-in failed$/);
});

test('an instance without desktop sign-in (404 or 405 on the first request) is reported as unsupported', async () => {
  for (const status of [404, 405]) {
    let opened = false;
    await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
      open: async () => { opened = true; }, http: httpOf(() => reply(status, 'nope')),
    }), /^Error: desktop-auth-unsupported$/);
    assert.equal(opened, false);
  }
});

test('too many attempts (429) is reported as rate-limited, and a later 404 is not unsupported', async () => {
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => {}, http: httpOf(() => reply(429, 'slow down')),
  }), /^Error: desktop-auth-rate-limited$/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => {},
    http: httpOf(options => options.url.endsWith('/request') ? reply(200, { id: 'A'.repeat(43) }) : reply(404, 'gone')),
  }), /Browser sign-in unavailable/);
  // A redirect is not followed (disableRedirects) and is not a success.
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => {}, http: httpOf(() => reply(302, '')),
  }), /^Error: Browser sign-in unavailable. Start again in Dilla.$/);
});

test('the ceremony expires after two minutes, with no further polling', async () => {
  const expiry = new AbortController();
  let timeoutAsked = 0, polls = 0;
  const pending = browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => {},
    clock: {
      timeout: ms => { timeoutAsked = ms; return expiry.signal; },
      sleep: async (_ms, signal) => { if (polls === 2) expiry.abort(new DOMException('The operation timed out.', 'TimeoutError')); signal.throwIfAborted(); },
    },
    http: httpOf(options => { if (options.url.endsWith('/request')) return reply(200, { id: 'A'.repeat(43) }); polls++; return reply(202); }),
  });
  await assert.rejects(pending, { name: 'TimeoutError' });
  assert.equal(timeoutAsked, 120000);
  assert.equal(CEREMONY_TIMEOUT_MS, 120000);
  assert.equal(polls, 2);
});

test('an HTTP instance or one with a path, query or credentials is refused before any request', async () => {
  const never = httpOf(() => { throw new Error('no request expected'); });
  for (const instance of ['http://chat.test/', 'http://127.0.0.1:8463/', 'https://u:p@chat.test/', 'https://chat.test/?q', 'not a url'])
    await assert.rejects(browserAuth(instance, { kind: 'get', options: '{}' }, { open: async () => {}, http: never }));
});

test('the native transport is CapacitorHttp, never the WebView fetch', () => {
  const source = readFileSync(new URL('../src/browser-auth.ts', import.meta.url), 'utf8');
  assert.doesNotMatch(source, /\bfetch\s*\(/);
  const bridge = readFileSync(new URL('../src/bridge.ts', import.meta.url), 'utf8');
  assert.match(bridge, /http:\s*CapacitorHttp\b/);
  assert.match(bridge, /from '@capacitor\/core'/);
});
