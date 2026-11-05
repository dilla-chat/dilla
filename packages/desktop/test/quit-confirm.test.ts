import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { transform } from 'esbuild';
import { UpdateCoordinator } from '../src/update.ts';

const main = await readFile(new URL('../src/main.ts', import.meta.url), 'utf8');

async function harness(guards: Array<{ activeCall: boolean; unsavedDraft: boolean } | null>, answer: number) {
  const updater = new UpdateCoordinator({ check: async () => null, download: async () => {}, install: async () => {} });
  const shown: any[] = [];
  const windows = new Map<string, any>(guards.map((_, i) => [`https://i${i}.example/`, { isDestroyed: () => false, webContents: { send() {} } }]));
  const context: any = vm.createContext({
    updater, windows, setTimeout, clearTimeout, quitProbe: null, randomUUID: () => 'x',
    dialog: { showMessageBox: async (options: any) => { shown.push(options); return { response: answer }; } },
  });
  const body = main.slice(main.indexOf('async function collectQuitGuards('), main.indexOf('\nasync function requestQuit('));
  vm.runInContext((await transform(body, { loader: 'ts' })).code, context);
  const run = vm.runInContext('collectQuitGuards()', context) as Promise<boolean>;
  // Answer the probe: every window reports its snapshot.
  [...windows.keys()].forEach((instance, i) => { if (guards[i]) updater.setGuard(instance, guards[i]!); });
  vm.runInContext('quitProbe.done()', context);
  return { result: await run, shown, updater };
}

test('a call or unsent text asks once; Quit quits and releases the guard', async () => {
  const { result, shown, updater } = await harness([{ activeCall: true, unsavedDraft: true }], 1);
  assert.equal(result, true);
  assert.equal(shown.length, 1);
  assert.equal(shown[0].title, 'Quit Dilla?');
  assert.equal(shown[0].message, "Quitting leaves your call and discards text you haven't sent.");
  assert.deepEqual([...shown[0].buttons], ['Cancel', 'Quit']);
  assert.equal(shown[0].defaultId, 0);
  assert.equal(updater.protected(), false);
});

test('Cancel stays open and keeps the guard', async () => {
  const { result, shown, updater } = await harness([{ activeCall: true, unsavedDraft: false }], 0);
  assert.equal(result, false);
  assert.equal(shown.length, 1);
  assert.equal(updater.protected(), true);
});

test('an idle app quits without a prompt; an unresponsive renderer still blocks', async () => {
  const idle = await harness([{ activeCall: false, unsavedDraft: false }], 1);
  assert.equal(idle.result, true); assert.equal(idle.shown.length, 0);
  const hung = await harness([null], 1);
  assert.equal(hung.result, false); assert.equal(hung.shown.length, 1); assert.notEqual(hung.shown[0].title, 'Quit Dilla?');
});

test('the unload guard is overridden while quitting and permission grants are persisted', () => {
  const handler = main.slice(main.indexOf("'will-prevent-unload'"), main.indexOf("'will-attach-webview'"));
  assert.match(handler, /if \(quitting\) \{ event\.preventDefault\(\); return; \}/);
  assert.match(main, /preferences\.grants\(instance\)/);
  assert.match(main, /preferences\.grant\(instance, permission\)/);
});
