import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { EventEmitter } from 'node:events';
import vm from 'node:vm';
import { transform } from 'esbuild';

// Execute the shipping main-process callbacks; only Electron/native operations
// are substituted so document changes can race unresolved OS operations.
async function fixture() {
 const source = await readFile(new URL('../src/main.ts', import.meta.url), 'utf8');
 const section = (from: string, to: string) => source.slice(source.indexOf(from), source.indexOf(to));
 const code = section('const captures =', 'const startupUsability =')
  + section("ipcMain.handle('dilla:app-audio'", "ipcMain.on('dilla:pcm-ack'")
  + section("ipcMain.handle('dilla:command'", "ipcMain.handle('dilla:manager'");
 const handlers = new Map<string, (...args: any[]) => any>();
 const native: any[] = [], pickers: any[] = [], guards = new Map();
 class Contents extends EventEmitter {
  mainFrame = { url: 'https://chat.test/' };
  destroyed = false;
  messages: any[][] = [];
  isDestroyed() { return this.destroyed; }
  send(...args: any[]) { this.messages.push(args); }
 }
 class Owner extends EventEmitter {
  webContents = new Contents(); destroyed = false; visible = true; focused = true;
  isDestroyed() { return this.destroyed; }
  isVisible() { return this.visible; }
  isFocused() { return this.focused; }
 }
 const win = new Owner(), instance = 'https://chat.test/', windows = new Map([[instance, win]]);
 const event = { sender: win.webContents, senderFrame: win.webContents.mainFrame };
 const globals = {
  windows, __dirname: '/immutable', AbortController,
  ipcMain: { handle: (name: string, handler: (...args: any[]) => any) => handlers.set(name, handler) },
  owner: (input: typeof event) => { if (input.sender !== win.webContents || input.senderFrame !== win.webContents.mainFrame || win.destroyed || win.webContents.destroyed) throw Error('Untrusted owner'); return { instance, win }; },
  ScreenCaptureBroker: class {}, startupActivated: true, askingQuit: false, quitting: false,
  existsSync: () => true, audioExecutable: '/immutable/dilla-audio',
  refreshMenu() {}, randomUUID: () => `capture-${native.length}`,
  validateCommand: (value: unknown) => value,
  updater: { guards, setGuard: (key: string, value: unknown) => guards.set(key, value) },
  NativeAudio: class {
   stopped = false; started = false; pcm: any; ended: any; enumerate: any;
   constructor() { native.push(this); }
   sources() { return new Promise(resolve => { this.enumerate = () => resolve([{ id: '123:456', label: 'Chosen application' }]); }); }
   start(_source: unknown, pcm: any, ended: any) { this.started = true; this.pcm = pcm; this.ended = ended; }
   stop() { this.stopped = true; }
  },
  dialog: { showMessageBox: (_win: Owner, options: any) => new Promise(resolve => { pickers.push({ options, choose: () => resolve({ response: 1 }) }); }) },
 };
 const context = vm.createContext(globals);
 vm.runInContext((await transform(code, { loader: 'ts' })).code, context);
 const flush = () => new Promise<void>(resolve => setImmediate(resolve));
 const request = () => handlers.get('dilla:app-audio')!(event);
 const stop = () => vm.runInContext('stopAppAudio("https://chat.test/")', context);
 const command = (value: unknown) => handlers.get('dilla:command')!(event, value);
 return { win, native, pickers, flush, request, stop, command };
}

for (const phase of ['enumeration', 'picker']) for (const reason of ['navigation', 'crash', 'closed', 'destroyed', 'hidden', 'stop', 'call-ended']) {
 test(`${reason} during ${phase} invalidates the original audio selection until the OS operation settles`, async () => {
  const f = await fixture();
  if (reason === 'call-ended') await f.command({ type: 'guard', activeCall: true, unsavedDraft: false });
  const pending = f.request();
  if (phase === 'picker') { f.native[0].enumerate(); await f.flush(); }
  if (reason === 'navigation') f.win.webContents.emit('did-start-navigation', {}, 'https://chat.test/', false, true);
  else if (reason === 'crash') f.win.webContents.emit('render-process-gone');
  else if (reason === 'closed') { f.win.destroyed = true; f.win.emit('closed'); }
  else if (reason === 'destroyed') { f.win.webContents.destroyed = true; f.win.webContents.emit('destroyed'); }
  else if (reason === 'hidden') { f.win.visible = false; f.win.emit('hide'); }
  else if (reason === 'call-ended') await f.command({ type: 'guard', activeCall: false, unsavedDraft: false });
  else f.stop();
  if (phase === 'picker') assert.equal(f.pickers[0].options.signal?.aborted, true);
  if (!['closed', 'destroyed', 'hidden'].includes(reason)) { void f.request().catch(() => {}); await f.flush(); assert.equal(f.native.length, 1, 'the unresolved OS operation retains its exclusive slot'); }
  if (phase === 'enumeration') f.native[0].enumerate(); else f.pickers[0].choose();
  assert.equal(await pending, null);
  assert.equal(f.native[0].started, false);
  assert.equal(f.pickers.length, phase === 'picker' ? 1 : 0);
 });
}

test('only the visible focused original document can open or finish an application selection', async () => {
 const f = await fixture();
 f.win.visible = false; void f.request().catch(() => {}); await f.flush(); assert.equal(f.native.length, 0, 'hidden owners cannot enumerate capture sources');
 f.win.visible = true; f.win.focused = false; await assert.rejects(f.request(), /unavailable/i);
 f.win.focused = true;
 const pending = f.request(); await assert.rejects(f.request(), /selection|unavailable/i);
 f.native[0].enumerate(); await f.flush();
 f.win.webContents.mainFrame = { url: 'https://chat.test/' };
 f.pickers[0].choose(); assert.equal(await pending, null);
 assert.equal(f.native[0].started, false);
});

test('handover keeps the new selection alive and stale ended callbacks cannot stop it', async () => {
 const f = await fixture();
 const first = f.request(); f.native[0].enumerate(); await f.flush(); f.pickers[0].choose(); const old = await first;
 const next = f.request(); f.native[1].enumerate(); await f.flush(); f.pickers[1].choose(); const current = await next;
 assert.ok(old && current); assert.equal(f.native[0].stopped, true); assert.equal(f.native[1].stopped, false);
 f.native[0].ended(); assert.equal(f.native[1].stopped, false);
 const before = f.win.webContents.messages.length;
 f.win.webContents.emit('did-start-navigation', {}, 'https://chat.test/', false, true);
 f.native[1].pcm(new Float32Array(960));
 assert.equal(f.native[1].stopped, true);
 assert.equal(f.win.webContents.messages.slice(before).some(message => message[0] === 'dilla:pcm'), false);
});

test('the old renderer track ending cannot cancel a pending replacement selection', async () => {
 const f = await fixture();
 const first = f.request(); f.native[0].enumerate(); await f.flush(); f.pickers[0].choose(); const old = await first;
 const next = f.request(); f.native[1].enumerate(); await f.flush();
 f.native[0].ended();
 await f.command({ type: 'stop-app-audio', id: old.id });
 assert.equal(f.pickers[1].options.signal.aborted, false);
 f.pickers[1].choose(); assert.ok(await next);
 assert.equal(f.native[1].started, true); f.stop();
});

for (const reason of ['closed', 'crash', 'destroyed']) test(`an active capture stops on owner ${reason} but survives closing to tray`, async () => {
 const f = await fixture();
 const pending = f.request(); f.native[0].enumerate(); await f.flush(); f.pickers[0].choose(); await pending;
 f.win.visible = false; f.win.emit('hide');
 assert.equal(f.native[0].stopped, false);
 if (reason === 'closed') { f.win.destroyed = true; f.win.emit('closed'); }
 else if (reason === 'destroyed') { f.win.webContents.destroyed = true; f.win.webContents.emit('destroyed'); }
 else f.win.webContents.emit('render-process-gone');
 assert.equal(f.native[0].stopped, true);
 const before = f.win.webContents.messages.length;
 f.native[0].pcm(new Float32Array(960));
 assert.equal(f.win.webContents.messages.length, before);
});
