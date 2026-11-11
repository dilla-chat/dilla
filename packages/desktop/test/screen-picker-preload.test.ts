import { test } from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';

// Task 5: the picker window's preload exposes only the picker: receive tiles, choose one, or cancel.
async function fixture(mainFrame = true, href = 'dilla-internal://picker/') {
  const listeners = new Map<string, (...args: any[]) => void>();
  const sent: any[][] = [];
  const exposed = new Map<string, any>();
  const compiled = await build({ entryPoints: [fileURLToPath(new URL('../src/screen-picker-preload.ts', import.meta.url))],
    write: false, bundle: true, platform: 'node', format: 'cjs', external: ['electron'] });
  vm.runInNewContext(compiled.outputFiles[0].text, {
    require: (name: string) => {
      assert.equal(name, 'electron');
      return { contextBridge: { exposeInMainWorld: (key: string, value: any) => exposed.set(key, value) },
        ipcRenderer: { on: (channel: string, fn: any) => listeners.set(channel, fn), send: (...args: any[]) => sent.push(args),
          removeListener: (channel: string, fn: any) => { if (listeners.get(channel) === fn) listeners.delete(channel); } } };
    },
    process: { isMainFrame: mainFrame }, location: { href },
  });
  return { exposed, sent, emit: (value: unknown) => listeners.get('dilla:screen-picker-sources')?.({}, value), listening: () => listeners.has('dilla:screen-picker-sources') };
}
const tile = { id: '7c9e6679-7425-40de-944b-e07fc1f90ae7', name: 'Built-in', kind: 'screen', thumbnail: 'data:image/jpeg;base64,AQI=', icon: null };

test('the picker preload exposes nothing outside its own top-level picker document', async () => {
  for (const [mainFrame, href] of [[false, 'dilla-internal://picker/'], [true, 'https://chat.test/'], [true, 'dilla-internal://capture/']] as const) {
    assert.equal((await fixture(mainFrame, href)).exposed.size, 0);
  }
  const f = await fixture();
  assert.deepEqual([...f.exposed.keys()], ['dillaPicker']);
  assert.deepEqual(Object.keys(f.exposed.get('dillaPicker')).sort(), ['cancel', 'choose', 'onSources']);
  assert.ok(Object.isFrozen(f.exposed.get('dillaPicker')));
});

test('only well-formed tiles reach the page, and the listener can unsubscribe', async () => {
  const f = await fixture();
  const seen: any[] = [];
  const off = f.exposed.get('dillaPicker').onSources((tiles: any) => seen.push(tiles));
  f.emit([tile]);
  f.emit([{ ...tile, kind: 'tab' }]);
  f.emit([{ ...tile, id: 'screen:1:0' }]);
  f.emit([{ ...tile, thumbnail: 'https://evil.test/x.png' }]);
  f.emit([{ ...tile, extra: 1, name: 'x'.repeat(257) }]);
  f.emit('all');
  f.emit(Array.from({ length: 65 }, () => tile));
  assert.equal(seen.length, 1);
  assert.equal(seen[0][0].name, 'Built-in');
  off();
  assert.equal(f.listening(), false);
});

test('choose sends one tile id; cancel sends null; nothing else crosses', async () => {
  const f = await fixture();
  const picker = f.exposed.get('dillaPicker');
  picker.choose(tile.id);
  picker.choose({ id: tile.id });
  picker.choose('x'.repeat(65));
  picker.cancel();
  assert.deepEqual(JSON.parse(JSON.stringify(f.sent)), [['dilla:screen-picker-choice', { id: tile.id }], ['dilla:screen-picker-choice', { id: null }]]);
});
