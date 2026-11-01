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
