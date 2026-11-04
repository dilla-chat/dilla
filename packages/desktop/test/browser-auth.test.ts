import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { browserAuth } from '../src/browser-auth.ts';

test('browser ceremony uses fixed instance, private PKCE and single-use POST completion', async () => {
  const requests: { url: string; body: Record<string, string> }[] = [];
  let opened = '';
  const id = 'A'.repeat(43);
  const result = await browserAuth('https://chat.test/team/', { kind: 'get', options: '{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}' }, {
    open: async url => { opened = url; },
    fetch: async (input, init) => {
      const url = String(input), body = JSON.parse(String(init?.body)) as Record<string, string>;
      requests.push({ url, body });
      assert.equal(init?.redirect, 'error');
      return Response.json(url.endsWith('/request') ? { id } : { response: '{"id":"credential"}', needs: 0 });
    },
  });
  assert.deepEqual(result, { response: '{"id":"credential"}', needs: 0 });
  assert.equal(opened, `https://chat.test/team/desktop-auth/${id}`);
  assert.equal(requests[1].url, `${opened}/consume`);
  const verifier = requests[1].body.verifier;
  assert.equal(createHash('sha256').update(verifier).digest('base64url'), requests[0].body.challenge);
  assert.ok(!opened.includes(verifier));
});

test('browser handoff rejects malformed replies and arbitrary capability arguments', async () => {
  let opened = false;
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'exec', options: '' }, { open: async () => { opened = true; } }), /Invalid/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'oidc', options: '' }, { open: async () => { opened = true; }, fetch: async () => Response.json({ id: 'https://evil.test' }) }), /Invalid/);
  assert.equal(opened, false);
});

test('an instance without desktop sign-in (404 or 405 on the first request) is reported as unsupported', async () => {
  for (const status of [404, 405]) {
    let opened = false;
    await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
      open: async () => { opened = true; }, fetch: async () => new Response('nope', { status }),
    }), /^Error: desktop-auth-unsupported$/);
    assert.equal(opened, false);
  }
});

test('too many attempts (429) is reported as rate-limited, and a later 404 is not unsupported', async () => {
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => {}, fetch: async () => new Response('slow down', { status: 429 }),
  }), /^Error: desktop-auth-rate-limited$/);
  await assert.rejects(browserAuth('https://chat.test/', { kind: 'get', options: '{}' }, {
    open: async () => {},
    fetch: async input => String(input).endsWith('/request') ? Response.json({ id: 'A'.repeat(43) }) : new Response('gone', { status: 404 }),
  }), /Browser sign-in unavailable/);
});

test('loopback ceremonies require the development-only authorization used by the native instance boundary', async () => {
 const request = { kind: 'get', options: '{}' };
 const deps = { open: async (_url: string) => {}, fetch: async (url: string | URL | Request) => Response.json(String(url).endsWith('/request') ? { id: 'A'.repeat(43) } : { response: '{}', needs: 0 }) };
 await assert.rejects(browserAuth('http://127.0.0.1:8463/', request, deps), /HTTPS/);
 assert.deepEqual(await browserAuth('http://127.0.0.1:8463/', request, { ...deps, allowLoopback: true }), { response: '{}', needs: 0 });
 await assert.rejects(browserAuth('http://remote.test/', request, { ...deps, allowLoopback: true }), /HTTPS/);
});
