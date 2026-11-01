import { test } from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';

test('UI remounts and repeated startup waits share one fixed native request', async () => {
 let bridge: any, release!: () => void;
 const pending = new Promise<void>(done => { release = done; }), calls: unknown[][] = [];
 const compiled = await build({ entryPoints: [fileURLToPath(new URL('../src/preload.ts', import.meta.url))],
  write: false, bundle: true, platform: 'node', format: 'cjs', external: ['electron'] });
 vm.runInNewContext(compiled.outputFiles[0].text, {
  require: (name: string) => {
   assert.equal(name, 'electron');
   return { contextBridge: { exposeInMainWorld: (name: string, value: unknown) => { assert.equal(name, 'dillaDesktop'); bridge = value; } },
    ipcRenderer: { on() {}, invoke: (...args: unknown[]) => { calls.push(args); return pending; } } };
  },
  process: { isMainFrame: true }, location: { protocol: 'https:', origin: 'https://chat.test' },
 });
 const first = bridge.waitForStartup(), next = bridge.waitForStartup('ignored-renderer-input');
 assert.equal(first, next); assert.deepEqual(calls, [['dilla:startup-ready']]);
 release(); await first; await bridge.waitForStartup(); assert.equal(calls.length, 1);
});
