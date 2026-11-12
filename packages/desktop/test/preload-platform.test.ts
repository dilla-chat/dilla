import { test } from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';

// The page's own device label ("Desktop · macOS") reads the bridge's fixed platform. It is a display hint, never a trust
// signal: the device's custody tier stays in its signed credential.
async function exposed(platform: string, mainFrame = true, argv: string[] = []): Promise<Map<string, any>> {
  const worlds = new Map<string, any>();
  const compiled = await build({ entryPoints: [fileURLToPath(new URL('../src/preload.ts', import.meta.url))],
    write: false, bundle: true, platform: 'node', format: 'cjs', external: ['electron'] });
  vm.runInNewContext(compiled.outputFiles[0].text, {
    require: (name: string) => {
      assert.equal(name, 'electron');
      return { contextBridge: { exposeInMainWorld: (key: string, value: any) => worlds.set(key, value) },
        ipcRenderer: { on: () => {}, send: () => {}, invoke: () => Promise.resolve(), removeListener: () => {} } };
    },
    process: { isMainFrame: mainFrame, platform, argv: ['electron', ...argv] }, Intl, location: { protocol: 'https:', origin: 'https://chat.test' },
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

test('the bridge carries the system region the main process put on the command line', async () => {
  assert.equal((await exposed('darwin', true, ['--dilla-locale=sv-SE'])).get('dillaDesktop').locale, 'sv-SE');
  assert.equal((await exposed('darwin', true, ['--dilla-locale=en-US'])).get('dillaDesktop').locale, 'en-US');
});

test('a missing or malformed region leaves the locale unset, so the page falls back to its own', async () => {
  assert.equal((await exposed('linux')).get('dillaDesktop').locale, undefined);
  assert.equal((await exposed('linux', true, ['--dilla-locale='])).get('dillaDesktop').locale, undefined);
  assert.equal((await exposed('linux', true, ['--dilla-locale=not a locale!'])).get('dillaDesktop').locale, undefined);
});
