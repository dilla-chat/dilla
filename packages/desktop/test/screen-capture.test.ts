import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { EventEmitter } from 'node:events';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import { build } from 'esbuild';

// Exercise the controller, substituting only native Electron objects unavailable in node.
async function fixture(response = 1, directory = '/immutable/dist', autoStarted = true) {
  let clock = 0, nextTimer = 1;
  const timers = new Map<number, { at: number; interval: number; fn: () => void }>();
  const later = (fn: () => void, ms: number, interval = 0) => { const id = nextTimer++; timers.set(id, { at: clock + ms, interval, fn }); return id; };
  const advance = (ms: number) => {
    const end = clock + ms;
    for (;;) {
      const due = [...timers.entries()].filter(([, timer]) => timer.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) break;
      const [id, timer] = due; clock = timer.at;
      if (timer.interval) timer.at += timer.interval; else timers.delete(id);
      timer.fn();
    }
    clock = end;
  };
  class Clock extends Date { static now() { return clock; } }
  const ipc = new EventEmitter();
  const windows: Window[] = [];
  const sessions: any[] = [];
  let sourcesCalls = 0;
  let dialogOptions: any;
  let dialogResolve: ((value: { response: number }) => void) | undefined;
  let holdDialog = false, failNextStart = false, holdSources = false;
  let sourcesResolve: ((value: { id: string; name: string }[]) => void) | undefined;
  const nativeSources = [{ id: 'screen:123:0', name: 'Actual screen' }, { id: 'window:456:0', name: 'Selected window' }];
  class Contents extends EventEmitter {
    mainFrame = { url: 'https://chat.test/' };
    destroyed = false;
    getURL() { return this.mainFrame.url; }
    isDestroyed() { return this.destroyed; }
    setWindowOpenHandler(handler: any) { this.popup = handler; }
    popup: any;
    posts: any[] = [];
    sends: any[] = [];
    postMessage(channel: string, data: any, ports: any[]) {
      this.posts.push({ channel, data, ports });
      if (channel === 'dilla:screen-broker-start' && autoStarted) queueMicrotask(() => ipc.emit('dilla:screen-broker-status',
        { sender: this, senderFrame: this.mainFrame }, { type: failNextStart ? 'failed' : 'started', id: data.id }));
    }
    send(...args: any[]) { this.sends.push(args); }
  }
  class Window extends EventEmitter {
    webContents = new Contents();
    destroyed = false;
    options: any;
    constructor(options?: any) { super(); this.options = options; windows.push(this); }
    isDestroyed() { return this.destroyed; }
    async loadURL(url: string) {
      this.webContents.mainFrame.url = url;
      this.webContents.emit('did-start-navigation', {}, this.webContents.mainFrame.url, false, true);
    }
    destroy() { this.destroyed = true; this.webContents.destroyed = true; this.emit('closed'); }
  }
  class Port { closed = false; close() { this.closed = true; } }
  const electron = {
    ipcMain: ipc, BrowserWindow: Window,
    MessageChannelMain: class { port1 = new Port(); port2 = new Port(); },
    desktopCapturer: { getSources: async (options: any) => {
      sourcesCalls++;
      assert.deepEqual(JSON.parse(JSON.stringify(options)), { types: ['screen', 'window'], thumbnailSize: { width: 0, height: 0 }, fetchWindowIcons: false });
      return holdSources ? new Promise<{ id: string; name: string }[]>(resolve => { sourcesResolve = resolve; }) : nativeSources;
    } },
    dialog: { showMessageBox: async (_owner: any, options: any) => {
      dialogOptions = options;
      return holdDialog ? new Promise<{ response: number }>(resolve => { dialogResolve = resolve; }) : { response };
    } },
    session: { fromPartition: (partition: string) => {
      assert.ok(!partition.startsWith('persist:'));
      const ses: any = new EventEmitter();
      ses.protocol = { handle: (_scheme: string, handler: any) => { ses.handler = handler; }, unhandle: () => {} };
      ses.setPermissionCheckHandler = (handler: any) => { ses.check = handler; };
      ses.setPermissionRequestHandler = (handler: any) => { ses.permission = handler; };
      ses.setDisplayMediaRequestHandler = (handler: any) => { ses.display = handler; };
      ses.webRequest = { onBeforeRequest: (_filter: any, handler: any) => { ses.network = handler; } };
      ses.clearStorageData = async () => { ses.cleared = true; };
      ses.clearCache = async () => {};
      ses.closeAllConnections = async () => {};
      sessions.push(ses); return ses;
    } },
  };
  const compiled = await build({ entryPoints: [fileURLToPath(new URL('../src/screen-capture.ts', import.meta.url))], bundle: true,
    write: false, platform: 'node', format: 'cjs', external: ['electron'] });
  const require = createRequire(import.meta.url);
  const module = { exports: {} as any };
  vm.runInNewContext(compiled.outputFiles[0].text, { module, exports: module.exports,
    require: (name: string) => name === 'electron' ? electron : require(name),
    setTimeout: (fn: () => void, ms: number) => later(fn, ms), clearTimeout: (id: number) => timers.delete(id),
    setInterval: (fn: () => void, ms: number) => later(fn, ms, ms), clearInterval: (id: number) => timers.delete(id),
    AbortController, URL, Response, Date: Clock, console });
  const ended: { instance: string; id: string }[] = [];
  const controller = new module.exports.ScreenCaptureBroker({ directory, onEnded: (instance: string, id: string) => ended.push({ instance, id }) });
  const owner = new Window();
  return { controller, owner, windows, sessions, ended, ipc, advance, get sourcesCalls() { return sourcesCalls; },
    get dialogOptions() { return dialogOptions; }, hold: () => { holdDialog = true; },
    choose: (response: number) => dialogResolve?.({ response }), failStart: () => { failNextStart = true; },
    holdSources: () => { holdSources = true; }, sourcesReady: () => sourcesResolve?.(nativeSources), newOwner: () => new Window() };
}
const options = { width: 1920, height: 1080, frameRate: 30 };

test('native picker cancellation creates no capture window, session or port', async () => {
  const f = await fixture(0);
  assert.equal(await f.controller.request('https://chat.test/', f.owner, options), null);
  assert.equal(f.windows.length, 1);
  assert.equal(f.sessions.length, 0);
  assert.equal(f.owner.webContents.posts.length, 0);
  assert.equal(f.dialogOptions.cancelId, 0);
  assert.equal(f.dialogOptions.defaultId, 0);
});

test('selected source remains private, owning video-only permission and lifecycle are enforced', async () => {
  const f = await fixture(2);
  try {
    const source = await f.controller.request('https://chat.test/', f.owner, options);
    assert.equal(source.displaySurface, 'window');
    assert.match(source.id, /^[0-9a-f-]{36}$/);
    const broker = f.windows[1], ses = f.sessions[0];
    assert.equal(broker.options.show, false);
    assert.equal(broker.options.webPreferences.sandbox, true);
    assert.equal(broker.options.webPreferences.contextIsolation, true);
    assert.equal(broker.options.webPreferences.nodeIntegration, false);
    const port = f.owner.webContents.posts[0];
    assert.equal(port.channel, 'dilla:screen-port');
    assert.deepEqual(JSON.parse(JSON.stringify(port.data)), { id: source.id });
    assert.equal(broker.webContents.posts[0].data.sourceId, 'window:456:0');
    const details = { isMainFrame: true, requestingUrl: 'dilla-internal://capture/', mediaTypes: ['video'] };
    const grant = (wc: any, permission: string, value: any) => { let result: any; ses.permission(wc, permission, (v: any) => { result = v; }, value); return result; };
    assert.equal(grant(broker.webContents, 'display-capture', details), true);
    assert.equal(grant(f.owner.webContents, 'display-capture', details), false);
    assert.equal(grant(broker.webContents, 'display-capture', { ...details, mediaTypes: ['audio', 'video'] }), false);
    assert.equal(grant(broker.webContents, 'media', details), false);
    let canceled: any;
    ses.network({ url: 'https://evil.test/script.js' }, (value: any) => { canceled = value.cancel; });
    assert.equal(canceled, true);
    await assert.rejects(f.controller.configure('https://other.test/', source.id, options), /capture/i);
    await f.controller.configure('https://chat.test/', source.id, { width: 1280, height: 720, frameRate: 15 });
    assert.equal(broker.webContents.sends.at(-1)[0], 'dilla:screen-broker-configure');
    f.controller.stop('https://other.test/', source.id);
    assert.equal(broker.destroyed, false);
    f.controller.stop('https://chat.test/', source.id);
    assert.equal(broker.destroyed, true);
    assert.deepEqual(f.ended, [{ instance: 'https://chat.test/', id: source.id }]);
    assert.equal(grant(broker.webContents, 'display-capture', details), false);
    assert.equal(f.ipc.listenerCount('dilla:screen-broker-status'), 0);
  } finally { f.controller.stopAll(); }
});

test('replaced captures survive stale stop and navigation tears down only their owning session', async () => {
  const f = await fixture();
  try {
    const old = await f.controller.request('a', f.owner, options);
    const current = await f.controller.request('a', f.owner, options);
    f.controller.stop('a', old.id);
    assert.equal(f.windows[2].destroyed, false);
    f.owner.webContents.emit('did-start-navigation', {}, 'https://other.test/', false, true);
    assert.equal(f.windows[2].destroyed, true);
    assert.deepEqual(f.ended.map(row => row.id), [old.id, current.id]);
  } finally { f.controller.stopAll(); }
});

test('owner navigation while native selection is open aborts without capturing late selection', async () => {
  const f = await fixture(); f.hold();
  const request = f.controller.request('a', f.owner, options);
  await new Promise(resolve => setImmediate(resolve));
  f.owner.webContents.emit('did-start-navigation', {}, 'https://other.test/', false, true);
  f.choose(1);
  assert.equal(await request, null);
  assert.equal(f.windows.length, 1);
  assert.equal(f.sessions.length, 0);
  assert.equal(f.dialogOptions.signal.aborted, true);
});

test('simultaneous native selections are rejected and cannot receive another request port', async () => {
  const f = await fixture(); f.hold();
  const request = f.controller.request('a', f.owner, options);
  await new Promise(resolve => setImmediate(resolve));
  await assert.rejects(f.controller.request('a', f.owner, options), /already open/);
  f.choose(0);
  assert.equal(await request, null);
  assert.equal(f.windows.length, 1);
});

test('configuration flood keeps one native operation and only the latest pending options', async () => {
  const f = await fixture();
  try {
    const source = await f.controller.request('a', f.owner, options);
    const broker = f.windows[1];
    for (let width = 100; width <= 500; width++) await f.controller.configure('a', source.id, { width, height: 100, frameRate: 30 });
    assert.equal(broker.webContents.sends.length, 1);
    f.ipc.emit('dilla:screen-broker-status', { sender: broker.webContents, senderFrame: broker.webContents.mainFrame }, { id: source.id, type: 'configured' });
    assert.equal(broker.webContents.sends.length, 2);
    assert.equal(broker.webContents.sends[1][1].options.width, 500);
  } finally { f.controller.stopAll(); }
});

test('broker status from another renderer or child frame cannot stop the owning capture', async () => {
  const f = await fixture();
  try {
    const source = await f.controller.request('a', f.owner, options);
    const broker = f.windows[1];
    f.ipc.emit('dilla:screen-broker-status', { sender: f.owner.webContents, senderFrame: broker.webContents.mainFrame }, { id: source.id, type: 'failed' });
    f.ipc.emit('dilla:screen-broker-status', { sender: broker.webContents, senderFrame: { url: broker.webContents.mainFrame.url } }, { id: source.id, type: 'failed' });
    assert.equal(broker.destroyed, false);
    f.ipc.emit('dilla:screen-broker-status', { sender: broker.webContents, senderFrame: broker.webContents.mainFrame }, { id: source.id, type: 'failed' });
    assert.equal(broker.destroyed, true);
  } finally { f.controller.stopAll(); }
});

test('main watchdog destroys a capture page that stops reporting health', async () => {
  const f = await fixture();
  const source = await f.controller.request('a', f.owner, options);
  f.advance(4999);
  assert.equal(f.windows[1].destroyed, false);
  f.advance(1);
  assert.equal(f.windows[1].destroyed, true);
  assert.deepEqual(f.ended, [{ instance: 'a', id: source.id }]);
});

test('capture startup cannot live on heartbeat messages without producing a first frame', async () => {
  const f = await fixture(1, '/immutable/dist', false);
  const request = f.controller.request('a', f.owner, options);
  const rejected = assert.rejects(request, /Screen capture failed/);
  await new Promise(resolve => setImmediate(resolve));
  const broker = f.windows[1], id = broker.webContents.posts[0].data.id;
  for (let i = 0; i < 3; i++) {
    f.advance(4000);
    f.ipc.emit('dilla:screen-broker-status', { sender: broker.webContents, senderFrame: broker.webContents.mainFrame }, { id, type: 'heartbeat' });
  }
  assert.equal(broker.destroyed, false);
  f.advance(3000);
  await rejected;
  assert.equal(broker.destroyed, true);
});

test('private protocol serves only the two immutable resources with a deny-by-default CSP', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'dilla-capture-protocol-'));
  const f = await fixture(1, directory);
  try {
    await writeFile(join(directory, 'screen-capture.html'), '<!doctype html><title>capture</title>');
    await writeFile(join(directory, 'screen-capture-broker.js'), 'fixed script');
    await writeFile(join(directory, 'other.js'), 'must not load');
    await f.controller.request('a', f.owner, options);
    const serve = f.sessions[0].handler;
    const html = await serve({ method: 'GET', url: 'dilla-internal://capture/' });
    assert.equal(html.status, 200);
    assert.match(html.headers.get('Content-Security-Policy'), /default-src 'none'/);
    assert.match(html.headers.get('Content-Security-Policy'), /connect-src 'none'/);
    assert.equal(await (await serve({ method: 'GET', url: 'dilla-internal://capture/screen-capture-broker.js' })).text(), 'fixed script');
    for (const url of ['dilla-internal://capture/other.js', 'dilla-internal://manager/', 'dilla-internal://capture/?source=screen:1:0',
      'dilla-internal://capture/%2e%2e/other.js', 'https://capture/', 'file:///etc/passwd']) {
      assert.equal((await serve({ method: 'GET', url })).status, 404);
    }
    assert.equal((await serve({ method: 'POST', url: 'dilla-internal://capture/' })).status, 404);
    f.controller.stopAll();
    assert.equal((await serve({ method: 'GET', url: 'dilla-internal://capture/' })).status, 404);
  } finally { f.controller.stopAll(); await rm(directory, { recursive: true, force: true }); }
});

test('replacement picker and Cancel preserve the existing capture and its native permission', async () => {
  const f = await fixture();
  try {
    const old = await f.controller.request('a', f.owner, options);
    const oldBroker = f.windows[1], oldSession = f.sessions[0];
    f.hold();
    const replacement = f.controller.request('a', f.owner, options);
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(oldBroker.destroyed, false);
    let allowed = false;
    oldSession.permission(oldBroker.webContents, 'display-capture', (value: boolean) => { allowed = value; },
      { isMainFrame: true, requestingUrl: 'dilla-internal://capture/', mediaTypes: ['video'] });
    assert.equal(allowed, true);
    await f.controller.configure('a', old.id, { width: 1280, height: 720, frameRate: 15 });
    assert.equal(oldBroker.webContents.sends.at(-1)[0], 'dilla:screen-broker-configure');
    f.choose(0);
    assert.equal(await replacement, null);
    assert.equal(oldBroker.destroyed, false);
    assert.equal(f.sessions.length, 1);
    assert.equal(f.ended.length, 0);
  } finally { f.controller.stopAll(); }
});

test('successful replacement retains old capture until its exact stop and bounds handover to two', async () => {
  const f = await fixture();
  try {
    const old = await f.controller.request('a', f.owner, options);
    const current = await f.controller.request('a', f.owner, options);
    assert.equal(f.windows[1].destroyed, false);
    assert.equal(f.windows[2].destroyed, false);
    assert.equal(f.ended.length, 0);
    await assert.rejects(f.controller.request('a', f.owner, options), /handover|limit|two/i);
    assert.equal(f.sessions.length, 2);
    f.controller.stop('other', old.id);
    assert.equal(f.windows[1].destroyed, false);
    f.controller.stop('a', old.id);
    assert.equal(f.windows[1].destroyed, true);
    assert.equal(f.windows[2].destroyed, false);
    f.controller.stop('a', old.id);
    assert.equal(f.windows[2].destroyed, false);
    assert.deepEqual(f.ended, [{ instance: 'a', id: old.id }]);
    await f.controller.configure('a', current.id, options);
    f.controller.stop('a', current.id);
    assert.equal(f.windows[2].destroyed, true);
  } finally { f.controller.stopAll(); }
});

test('failed replacement acquisition closes only its new broker and preserves the published capture', async () => {
  const f = await fixture();
  try {
    const old = await f.controller.request('a', f.owner, options);
    f.failStart();
    await assert.rejects(f.controller.request('a', f.owner, options), /Screen capture failed/);
    assert.equal(f.windows[1].destroyed, false);
    assert.equal(f.windows[2].destroyed, true);
    assert.equal(f.ended.length, 1);
    assert.notEqual(f.ended[0].id, old.id);
    await f.controller.configure('a', old.id, options);
  } finally { f.controller.stopAll(); }
});

test('unkeyed instance stop releases both handover captures without affecting other instances', async () => {
  const f = await fixture();
  try {
    await f.controller.request('a', f.owner, options);
    await f.controller.request('a', f.owner, options);
    const otherOwner = f.newOwner();
    const other = await f.controller.request('b', otherOwner, options);
    assert.equal(f.windows[1].destroyed, false);
    assert.equal(f.windows[2].destroyed, false);
    f.controller.stop('a');
    assert.equal(f.windows[1].destroyed, true);
    assert.equal(f.windows[2].destroyed, true);
    assert.equal(f.windows[4].destroyed, false);
    assert.equal(f.ended.length, 2);
    await f.controller.configure('b', other.id, options);
  } finally { f.controller.stopAll(); }
});

test('interactive native enumeration remains valid after more than fifteen seconds of user selection', async () => {
  const f = await fixture(); f.holdSources();
  try {
    const request = f.controller.request('a', f.owner, options);
    f.advance(60_000);
    f.sourcesReady();
    const source = await request;
    assert.equal(source.displaySurface, 'monitor');
    assert.equal(f.windows[1].destroyed, false);
  } finally { f.controller.stopAll(); }
});

test('aborted interactive enumeration retains its exclusive slot until the native portal settles', { timeout: 1000 }, async () => {
  const f = await fixture(); f.holdSources();
  const request = f.controller.request('a', f.owner, options);
  f.controller.stop('a');
  assert.equal(await request, null);
  await assert.rejects(f.controller.request('a', f.owner, options), /already open/);
  f.advance(60_000);
  await assert.rejects(f.controller.request('a', f.owner, options), /already open/);
  f.sourcesReady();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.windows.length, 1);
  assert.equal(f.sessions.length, 0);
  f.hold();
  const next = f.controller.request('a', f.owner, options);
  f.sourcesReady();
  await new Promise(resolve => setImmediate(resolve));
  f.choose(0);
  assert.equal(await next, null);
});

test('owner navigation and crash release both capture windows during handover', async () => {
  for (const event of ['did-start-navigation', 'render-process-gone']) {
    const f = await fixture();
    try {
      await f.controller.request('a', f.owner, options);
      await f.controller.request('a', f.owner, options);
      assert.equal(f.windows[1].destroyed, false);
      assert.equal(f.windows[2].destroyed, false);
      f.owner.webContents.emit(event, {}, 'https://other.test/', false, true);
      assert.equal(f.windows[1].destroyed, true);
      assert.equal(f.windows[2].destroyed, true);
      assert.equal(f.ended.length, 2);
      assert.equal(f.ipc.listenerCount('dilla:screen-broker-status'), 0);
    } finally { f.controller.stopAll(); }
  }
});
