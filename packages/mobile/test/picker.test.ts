import { test } from 'node:test';
import assert from 'node:assert/strict';
import { addInstance, canonicalInstance, CONNECT_ERROR, currentInstance, listInstances, selectInstance, type PreferenceStore } from '../src/instances.ts';

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
  assert.equal(await addInstance(store, 'https://chat.example'), 'https://chat.example/');
  assert.equal(await addInstance(store, 'https://chat.example/'), 'https://chat.example/');
  assert.deepEqual(await listInstances(store), ['https://chat.example/']);
  assert.equal(await currentInstance(store), 'https://chat.example/');
  assert.deepEqual(JSON.parse(store.values.get('instances')!), ['https://chat.example/']);
  assert.equal(store.values.get('current'), 'https://chat.example/');
});

test('incomplete or non-HTTPS addresses are rejected with the desktop copy and nothing is stored', async () => {
  for (const input of ['http://x', 'https://x/path', 'https://x?q', 'x', 'https://u:p@x', 'https://x#f', 'https://x.', '']) {
    const store = memory();
    await assert.rejects(addInstance(store, input), { message: CONNECT_ERROR }, input);
    assert.deepEqual(await listInstances(store), [], input);
    assert.equal(await currentInstance(store), null, input);
  }
});

test('selecting the second instance sets current to it; an unknown one is refused', async () => {
  const store = memory();
  await addInstance(store, 'https://one.example');
  await addInstance(store, 'https://two.example:8443');
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
