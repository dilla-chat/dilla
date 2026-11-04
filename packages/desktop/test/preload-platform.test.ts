import { test } from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';

// The page's own device label ("Desktop · macOS") reads the bridge's fixed platform. It is a display hint, never a trust
// signal: the device's custody tier stays in its signed credential.
async function exposed(platform: string, mainFrame = true): Promise<Map<string, any>> {
  const worlds = new Map<string, any>();
  const compiled = await build({ entryPoints: [fileURLToPath(new URL('../src/preload.ts', import.meta.url))],
    write: false, bundle: true, platform: 'node', format: 'cjs', external: ['electron'] });
  vm.runInNewContext(compiled.outputFiles[0].text, {
    require: (name: string) => {
      assert.equal(name, 'electron');
      return { contextBridge: { exposeInMainWorld: (key: string, value: any) => worlds.set(key, value) },
        ipcRenderer: { on: () => {}, send: () => {}, invoke: () => Promise.resolve(), removeListener: () => {} } };
    },
    process: { isMainFrame: mainFrame, platform }, location: { protocol: 'https:', origin: 'https://chat.test' },
    window: { postMessage: () => {} }, Map, Float32Array,
  });
  return worlds;
}

test('the instance page bridge carries the shell\'s fixed platform, read-only', async () => {
  for (const platform of ['darwin', 'win32', 'linux']) {
    const bridge = (await exposed(platform)).get('dillaDesktop');
    assert.equal(bridge.platform, platform);
    assert.ok(Object.isFrozen(bridge));
    assert.throws(() => { 'use strict'; bridge.platform = 'other'; });
  }
});

test('a subframe gets no bridge and so no platform', async () => {
  assert.equal((await exposed('darwin', false)).size, 0);
});
