import test from 'node:test';
import assert from 'node:assert/strict';
import { fetchInstanceData } from '../src/request-forwarding.ts';

test('native forwarding preserves the exact request/session/response while restoring cookie, redirect and cache policy', async () => {
 const request = new Request('https://owned.invalid/v1/data', { method: 'POST', headers: { authorization: 'Bearer owned-fixture' }, body: 'owned-body' });
 const response = new Response('owned-response', { status: 201 });
 let calls = 0;
 const ses = { fetch(input: string | Request, options?: RequestInit & { bypassCustomProtocolHandlers?: boolean }) {
  assert.equal(this, ses); assert.equal(input, request); calls++;
  assert.deepEqual(options, { bypassCustomProtocolHandlers: true, credentials: 'omit', redirect: 'error', cache: 'no-store' });
  return Promise.resolve(response);
 } };
 assert.equal(await fetchInstanceData(ses, request), response); assert.equal(calls, 1);
 assert.equal(request.bodyUsed, false); assert.equal(response.bodyUsed, false);
 assert.equal(request.headers.get('authorization'), 'Bearer owned-fixture'); assert.equal(await request.text(), 'owned-body');
});

test('native forwarding preserves the original rejection', async () => {
 const failure = new Error('owned transport failure');
 const ses = { fetch() { return Promise.reject(failure); } };
 await assert.rejects(fetchInstanceData(ses, new Request('https://owned.invalid/v1/data')), error => error === failure);
});
