import { test } from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';

const id = '8c6b13cb-667a-4657-9b1d-bdb16fe2ac32';
async function fixture(mainFrame = true, href = 'dilla-internal://capture/') {
  const handlers = new Map<string, (event: any, value: any) => void>();
  const sent: any[] = [], posted: any[] = [];
  let bridge: any;
  const compiled = await build({ entryPoints: [fileURLToPath(new URL('../src/screen-capture-preload.ts', import.meta.url))],
    write: false, bundle: true, platform: 'node', format: 'cjs', external: ['electron'] });
  vm.runInNewContext(compiled.outputFiles[0].text, {
    require: (name: string) => {
      assert.equal(name, 'electron');
      return { contextBridge: { exposeInMainWorld: (name: string, value: any) => { assert.equal(name, 'dillaCaptureBroker'); bridge = value; } },
        ipcRenderer: { on: (name: string, handler: any) => handlers.set(name, handler), send: (...args: any[]) => sent.push(args) } };
    },
    process: { isMainFrame: mainFrame }, location: { href, origin: 'dilla-internal://capture' },
    window: { postMessage: (...args: any[]) => posted.push(args) },
  });
  return { handlers, sent, posted, get bridge() { return bridge; } };
}
const start = { id, sourceId: 'screen:123:0', options: { width: 1920, height: 1080, frameRate: 30 } };

test('capture preload exposes no privilege outside its immutable top-level document', async () => {
  for (const [mainFrame, href] of [[false, 'dilla-internal://capture/'], [true, 'https://chat.test/']] as const) {
    const f = await fixture(mainFrame, href);
    assert.equal(f.bridge, undefined);
    assert.equal(f.handlers.size, 0);
  }
});

test('capture preload accepts only one main-supplied selected source and a single port', async () => {
  const f = await fixture();
  let closed = 0;
  const port = { close: () => { closed++; } };
  f.handlers.get('dilla:screen-broker-start')!({ ports: [port] }, { ...start, options: { ...start.options, sourceId: 'window:9:0' } });
  assert.equal(closed, 1);
  assert.equal(f.posted.length, 0);
  f.handlers.get('dilla:screen-broker-start')!({ ports: [port] }, start);
  assert.equal(f.posted.length, 1);
  assert.equal(f.posted[0][0].sourceId, 'screen:123:0');
  assert.equal(f.posted[0][1], 'dilla-internal://capture');
  assert.equal(f.posted[0][2][0], port);
  f.handlers.get('dilla:screen-broker-start')!({ ports: [port] }, { ...start, sourceId: 'window:9:0' });
  assert.equal(closed, 2);
  assert.equal(f.posted.length, 1);
  assert.deepEqual(Object.keys(f.bridge), ['status']);
  f.bridge.status('started', 'other');
  f.bridge.status('arbitrary-channel', id);
  assert.equal(f.sent.length, 0);
  f.bridge.status('started', id);
  assert.equal(f.sent[0][0], 'dilla:screen-broker-status');
  assert.deepEqual(JSON.parse(JSON.stringify(f.sent[0][1])), { type: 'started', id });
});
