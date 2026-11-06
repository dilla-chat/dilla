import { test } from 'node:test';
import assert from 'node:assert/strict';
import { addInstance, canonicalInstance, CONNECT_ERROR, currentInstance, listInstances, reachInstance, selectInstance, type PreferenceStore } from '../src/instances.ts';

/** A probe that always reaches the instance. */
const reachable = async (): Promise<void> => {};

/** An in-memory @capacitor/preferences. */
function memory(initial: Record<string, string> = {}): PreferenceStore & { values: Map<string, string> } {
  const values = new Map(Object.entries(initial));
  return {
    values,
    get: async ({ key }) => ({ value: values.get(key) ?? null }),
    set: async ({ key, value }) => { values.set(key, value); },
  };
}

test('the picker copy is the desktop copy, word for word', () => {
  assert.equal(CONNECT_ERROR, 'Could not connect. Enter a complete HTTPS instance address, then try again.');
});

test('https://chat.example is accepted, stored once, and becomes current', async () => {
  const store = memory();
  assert.equal(await addInstance(store, 'https://chat.example', reachable), 'https://chat.example/');
  assert.equal(await addInstance(store, 'https://chat.example/', reachable), 'https://chat.example/');
  assert.deepEqual(await listInstances(store), ['https://chat.example/']);
  assert.equal(await currentInstance(store), 'https://chat.example/');
  assert.deepEqual(JSON.parse(store.values.get('instances')!), ['https://chat.example/']);
  assert.equal(store.values.get('current'), 'https://chat.example/');
});

test('incomplete or non-HTTPS addresses are rejected with the desktop copy and nothing is stored', async () => {
  // IP literals too, as the core worker's gate (client-core worker/instance-url.ts) refuses them.
  for (const input of ['http://x', 'https://x/path', 'https://x?q', 'x', 'https://u:p@x', 'https://x#f', 'https://x.', '', 'https://10.0.2.2:18443', 'https://127.0.0.1', 'https://[::1]', 'https://0x7f.1',
    'https://localhost', 'https://localhost:8443', 'https://chat.localhost', 'https://LocalHost']) {
    const store = memory();
    await assert.rejects(addInstance(store, input, reachable), { message: CONNECT_ERROR }, input);
    assert.deepEqual(await listInstances(store), [], input);
    assert.equal(await currentInstance(store), null, input);
  }
});

test('selecting the second instance sets current to it; an unknown one is refused', async () => {
  const store = memory();
  await addInstance(store, 'https://one.example', reachable);
  await addInstance(store, 'https://two.example:8443', reachable);
  assert.deepEqual(await listInstances(store), ['https://one.example/', 'https://two.example:8443/']);
  await selectInstance(store, 'https://one.example/');
  assert.equal(await currentInstance(store), 'https://one.example/');
  await selectInstance(store, (await listInstances(store))[1]);
  assert.equal(await currentInstance(store), 'https://two.example:8443/');
  await assert.rejects(selectInstance(store, 'https://three.example/'), { message: CONNECT_ERROR });
  assert.equal(await currentInstance(store), 'https://two.example:8443/');
});

test('corrupt stored preferences read as empty, and a current not in the list is ignored', async () => {
  assert.deepEqual(await listInstances(memory({ instances: '{"a":1}' })), []);
  assert.deepEqual(await listInstances(memory({ instances: 'nope' })), []);
  assert.deepEqual(await listInstances(memory({ instances: '["http://evil.test/","https://ok.test/"]' })), ['https://ok.test/']);
  assert.equal(await currentInstance(memory({ instances: '["https://ok.test/"]', current: 'https://other.test/' })), null);
});

test('canonicalInstance is the desktop policy copy (no loopback on a phone)', () => {
  assert.equal(canonicalInstance('https://chat.example'), 'https://chat.example/');
  assert.equal(canonicalInstance('https://chat.example/team'), 'https://chat.example/team/');
  assert.throws(() => canonicalInstance('http://127.0.0.1:8463/'), /HTTPS/);
  assert.throws(() => canonicalInstance('https://chat.example/assets/'), /reserves/);
  assert.throws(() => canonicalInstance('https://chat.example/%2e%2e/'), /Invalid instance URL/);
});

// Final review I2c: the picker probes GET <instance>/v1/instance through the WebView's fetch (the server's mobile CORS)
// before it stores anything, and shows the desktop copy when the instance cannot be reached.
test('an instance is probed at /v1/instance before it is stored', async () => {
  const store = memory();
  const probed: string[] = [];
  await addInstance(store, 'https://chat.example', async instance => { probed.push(instance); });
  assert.deepEqual(probed, ['https://chat.example/']);
  assert.deepEqual(await listInstances(store), ['https://chat.example/']);
});

test('an unreachable instance is refused with the desktop copy and nothing is stored', async () => {
  const store = memory();
  await addInstance(store, 'https://kept.example', reachable);
  await assert.rejects(addInstance(store, 'https://down.example', async () => { throw new TypeError('Load failed'); }), { message: CONNECT_ERROR });
  assert.deepEqual(await listInstances(store), ['https://kept.example/']);
  assert.equal(await currentInstance(store), 'https://kept.example/');
});

test('an invalid address is refused before any probe', async () => {
  let probes = 0;
  await assert.rejects(addInstance(memory(), 'http://x', async () => { probes++; }), { message: CONNECT_ERROR });
  assert.equal(probes, 0);
});

test('reachInstance GETs /v1/instance without credentials or redirects and needs a 2xx', async () => {
  const calls: { url: string; init: RequestInit | undefined }[] = [];
  const answer = (status: number) => async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), init });
    return new Response(null, { status });
  };
  await reachInstance(answer(200))('https://chat.example/');
  assert.equal(calls[0]!.url, 'https://chat.example/v1/instance');
  assert.equal(calls[0]!.init?.method, 'GET');
  assert.equal(calls[0]!.init?.credentials, 'omit');
  assert.equal(calls[0]!.init?.redirect, 'error');
  assert.ok(calls[0]!.init?.signal instanceof AbortSignal);
  await assert.rejects(reachInstance(answer(404))('https://chat.example/'));
  await assert.rejects(reachInstance(answer(502))('https://chat.example/'));
  await assert.rejects(reachInstance(async () => { throw new TypeError('Failed to fetch'); })('https://chat.example/'));
});
