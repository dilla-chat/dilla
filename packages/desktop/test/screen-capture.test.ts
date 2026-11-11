import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, mkdtemp, writeFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { EventEmitter } from 'node:events';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import { build } from 'esbuild';

// Exercise the controller, substituting only native Electron objects unavailable in node.
async function fixture(response = 1, directory = '/immutable/dist', autoStarted = true, portal = true) {
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
  const jpegQualities: number[] = [];
  const image = (bytes: number[]) => ({ isEmpty: () => bytes.length === 0, toDataURL: () => `data:image/png;base64,${Buffer.from(bytes).toString('base64')}`,
    toJPEG: (quality: number) => { jpegQualities.push(quality); return Buffer.from(bytes); } });
  let pickerSources: any[] = [
    { id: 'screen:1:0', name: 'Built-in', display_id: '1', thumbnail: image([1, 2]), appIcon: null },
    { id: 'window:77:0', name: 'Studio notes', display_id: '', thumbnail: image([3]), appIcon: image([4]) },
  ];
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
    shown = false;
    show() { this.shown = true; }
  }
  class Port { closed = false; close() { this.closed = true; } }
  const electron = {
    ipcMain: ipc, BrowserWindow: Window,
    MessageChannelMain: class { port1 = new Port(); port2 = new Port(); },
    desktopCapturer: { getSources: async (options: any) => {
      sourcesCalls++;
      if (!portal) {
        assert.deepEqual(JSON.parse(JSON.stringify(options)), { types: ['screen', 'window'], thumbnailSize: { width: 320, height: 180 }, fetchWindowIcons: true });
        return pickerSources;
      }
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
      ses.partition = partition; sessions.push(ses); return ses;
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
    AbortController, URL, Response, Date: Clock, console, Buffer, Uint8Array });
  const ended: { instance: string; id: string }[] = [];
  const controller = new module.exports.ScreenCaptureBroker({ directory, portal, onEnded: (instance: string, id: string) => ended.push({ instance, id }) });
  const owner = new Window();
  return { controller, owner, windows, sessions, ended, ipc, advance, get sourcesCalls() { return sourcesCalls; },
    get dialogOptions() { return dialogOptions; }, hold: () => { holdDialog = true; },
    choose: (response: number) => dialogResolve?.({ response }), failStart: () => { failNextStart = true; },
    holdSources: () => { holdSources = true; }, sourcesReady: () => sourcesResolve?.(nativeSources), newOwner: () => new Window(),
    setSources: (list: any[]) => { pickerSources = list; }, timers: () => timers.size, jpegQualities };
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

// Task 5: the thumbnail picker (macOS, Windows, Linux X11) runs in a main-owned sandboxed window. Thumbnails and titles go
// only there; main takes the choice from that window alone and maps its opaque tile id back to the native source.
const settle = async () => { for (let i = 0; i < 4; i++) await new Promise(resolve => setImmediate(resolve)); };
async function opened(f: Awaited<ReturnType<typeof fixture>>) {
  await settle();
  const win = f.windows[1];
  const sends = () => win.webContents.sends.filter((args: any[]) => args[0] === 'dilla:screen-picker-sources').map((args: any[]) => args[1]);
  const choose = (id: unknown, sender: any = win.webContents, frame: any = sender.mainFrame) =>
    f.ipc.emit('dilla:screen-picker-choice', { sender, senderFrame: frame }, { id });
  return { win, sends, choose, last: () => sends().at(-1) };
}
const noSourceData = (owner: any) => {
  assert.deepEqual(owner.webContents.sends, [], 'the account renderer is sent nothing while picking');
  assert.equal(owner.webContents.posts.length, 0);
};

test('the picker is a sandboxed modal child window on its own session, and only it receives tiles', async () => {
  const f = await fixture(1, '/immutable/dist', true, false);
  try {
    const request = f.controller.request('a', f.owner, options);
    const p = await opened(f);
    assert.equal(f.dialogOptions, undefined);
    const prefs = p.win.options.webPreferences;
    assert.equal(p.win.options.parent, f.owner);
    assert.equal(p.win.options.modal, true);
    assert.equal(prefs.sandbox, true);
    assert.equal(prefs.contextIsolation, true);
    assert.equal(prefs.nodeIntegration, false);
    assert.equal(prefs.devTools, false);
    assert.match(prefs.preload, /screen-picker-preload\.cjs$/);
    assert.equal(prefs.session, f.sessions[0]);
    assert.equal(p.win.webContents.getURL(), 'dilla-internal://picker/');
    assert.equal(p.win.shown, true);
    const shown = p.last();
    assert.deepEqual([...shown].map((s: any) => Object.keys(s).sort().join(',')), ['icon,id,kind,name,thumbnail', 'icon,id,kind,name,thumbnail']);
    assert.deepEqual([...shown].map((s: any) => [s.kind, s.name]), [['screen', 'Built-in'], ['window', 'Studio notes']]);
    assert.ok([...shown].every((s: any) => /^[0-9a-f-]{36}$/.test(s.id)), 'a native source id never leaves main');
    assert.equal(shown[0].thumbnail, 'data:image/jpeg;base64,AQI=', 'thumbnails are JPEG');
    assert.ok(f.jpegQualities.length > 0 && f.jpegQualities.every(q => q === 70));
    assert.equal(shown[0].icon, null);
    assert.equal(shown[1].icon, 'data:image/png;base64,BA==');
    noSourceData(f.owner);
    p.choose(shown[1].id);
    const source = await request;
    assert.equal(source.displaySurface, 'window');
    assert.equal(p.win.destroyed, true);
    assert.equal(f.windows[2].webContents.posts[0].data.sourceId, 'window:77:0');
    assert.equal(f.ipc.listenerCount('dilla:screen-picker-choice'), 0);
  } finally { f.controller.stopAll(); }
});

test('the picker page is served from the internal scheme with a strict CSP and nothing else loads', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'dilla-picker-protocol-'));
  const f = await fixture(1, directory, true, false);
  try {
    await mkdir(join(directory, 'screen-picker'));
    await writeFile(join(directory, 'screen-picker', 'screen-picker.html'), '<!doctype html><title>picker</title>');
    await writeFile(join(directory, 'screen-picker', 'screen-picker.js'), 'picker script');
    await writeFile(join(directory, 'other.js'), 'must not load');
    const request = f.controller.request('a', f.owner, options);
    const p = await opened(f);
    const serve = f.sessions[0].handler;
    const html = await serve({ method: 'GET', url: 'dilla-internal://picker/' });
    assert.equal(html.status, 200);
    const csp = html.headers.get('Content-Security-Policy');
    for (const rule of ["default-src 'none'", "script-src 'self'", "connect-src 'none'", "img-src 'self' data:", "frame-ancestors 'none'"]) assert.ok(csp.includes(rule), rule);
    assert.equal(await (await serve({ method: 'GET', url: 'dilla-internal://picker/screen-picker.js' })).text(), 'picker script');
    for (const url of ['dilla-internal://picker/../other.js', 'dilla-internal://picker/%2e%2e/other.js', 'dilla-internal://capture/', 'dilla-internal://picker/x.exe',
      'dilla-internal://picker/?x=1', 'https://picker/', 'file:///etc/passwd']) assert.equal((await serve({ method: 'GET', url })).status, 404, url);
    assert.equal((await serve({ method: 'POST', url: 'dilla-internal://picker/' })).status, 404);
    let cancel: any;
    f.sessions[0].network({ url: 'https://evil.test/' }, (value: any) => { cancel = value.cancel; });
    assert.equal(cancel, true);
    let granted: any;
    f.sessions[0].permission(p.win.webContents, 'display-capture', (value: boolean) => { granted = value; }, {});
    assert.equal(granted, false);
    p.choose(null);
    assert.equal(await request, null);
    assert.equal((await serve({ method: 'GET', url: 'dilla-internal://picker/' })).status, 404);
  } finally { f.controller.stopAll(); await rm(directory, { recursive: true, force: true }); }
});

test('a choice from any webContents other than the picker window is refused', async () => {
  const f = await fixture(1, '/immutable/dist', true, false);
  const request = f.controller.request('a', f.owner, options);
  const p = await opened(f);
  const tile = p.last()[0].id;
  p.choose(tile, f.owner.webContents);
  p.choose(tile, f.newOwner().webContents);
  p.choose(tile, p.win.webContents, { url: 'dilla-internal://picker/' });
  p.choose(null, f.owner.webContents);
  await settle();
  assert.equal(p.win.destroyed, false, 'the picker stays open');
  assert.equal(f.windows.filter(w => w.options?.webPreferences?.preload?.endsWith('screen-capture-preload.cjs')).length, 0);
  assert.equal(f.sessions.length, 1);
  p.choose(null);
  assert.equal(await request, null);
});

test('an unknown or stale source id is refused and grants nothing', async () => {
  const f = await fixture(1, '/immutable/dist', true, false);
  const request = f.controller.request('a', f.owner, options);
  const p = await opened(f);
  const first = p.last();
  for (const bad of ['screen:1:0', 'nope', crypto.randomUUID(), 7, undefined, 'x'.repeat(300)]) p.choose(bad);
  f.ipc.emit('dilla:screen-picker-choice', { sender: p.win.webContents, senderFrame: p.win.webContents.mainFrame }, { id: first[0].id, extra: 1 });
  // A refresh drops the window; its tile id is stale from then on, while the screen keeps its id.
  f.setSources([{ id: 'screen:1:0', name: 'Built-in', thumbnail: { isEmpty: () => false, toJPEG: () => Buffer.from([9]) }, appIcon: null }]);
  f.advance(2000);
  await settle();
  assert.equal(p.last().length, 1);
  assert.equal(p.last()[0].id, first[0].id);
  p.choose(first[1].id);
  await settle();
  assert.equal(p.win.destroyed, false);
  assert.equal(f.sessions.length, 1, 'no capture session was created');
  p.choose(null);
  assert.equal(await request, null);
  // A later request never accepts tile ids handed to an earlier one.
  const next = f.controller.request('a', f.owner, options);
  await settle();
  const q = { win: f.windows.at(-1)! };
  f.ipc.emit('dilla:screen-picker-choice', { sender: q.win.webContents, senderFrame: q.win.webContents.mainFrame }, { id: first[0].id });
  await settle();
  assert.equal(q.win.destroyed, false);
  f.ipc.emit('dilla:screen-picker-choice', { sender: q.win.webContents, senderFrame: q.win.webContents.mainFrame }, { id: null });
  assert.equal(await next, null);
  noSourceData(f.owner);
});

test('cancelling the picker grants nothing and closes it', async () => {
  const f = await fixture(1, '/immutable/dist', true, false);
  const request = f.controller.request('a', f.owner, options);
  const p = await opened(f);
  p.choose(null);
  assert.equal(await request, null);
  assert.equal(p.win.destroyed, true);
  assert.equal(f.windows.length, 2);
  assert.equal(f.sessions.length, 1);
  assert.equal(f.sessions[0].cleared, true);
  noSourceData(f.owner);
});

test('closing the picker window cancels', async () => {
  const f = await fixture(1, '/immutable/dist', true, false);
  const request = f.controller.request('a', f.owner, options);
  const p = await opened(f);
  p.win.destroy();
  assert.equal(await request, null);
  assert.equal(f.windows.length, 2);
  assert.equal(f.ipc.listenerCount('dilla:screen-picker-choice'), 0);
});

test('the picker deadline cancels after two minutes', async () => {
  const f = await fixture(1, '/immutable/dist', true, false);
  const request = f.controller.request('a', f.owner, options);
  const p = await opened(f);
  f.advance(119_000);
  await settle();
  assert.equal(p.win.destroyed, false);
  f.advance(1000);
  assert.equal(await request, null);
  assert.equal(p.win.destroyed, true);
  assert.equal(f.windows.length, 2);
  const calls = f.sourcesCalls;
  f.advance(10_000); await settle();
  assert.equal(f.sourcesCalls, calls);
});

test('one picker per owner: a second request while it is open is refused', async () => {
  const f = await fixture(1, '/immutable/dist', true, false);
  const request = f.controller.request('a', f.owner, options);
  const p = await opened(f);
  await assert.rejects(f.controller.request('a', f.owner, options), /already open/);
  assert.equal(f.windows.length, 2);
  p.choose(null);
  assert.equal(await request, null);
});

test('the refresh timer stops when the picker closes, by cancel, choice, window close, owner navigation or stop', async () => {
  for (const close of ['cancel', 'choose', 'close', 'navigate', 'stop'] as const) {
    const f = await fixture(1, '/immutable/dist', true, false);
    try {
      const request = f.controller.request('a', f.owner, options);
      const p = await opened(f);
      f.advance(2000); await settle();
      f.advance(2000); await settle();
      assert.equal(f.sourcesCalls, 3, 'it refreshes every two seconds while open');
      assert.equal(p.sends().length, 3);
      if (close === 'cancel') p.choose(null);
      else if (close === 'choose') p.choose(p.last()[0].id);
      else if (close === 'close') p.win.destroy();
      else if (close === 'navigate') f.owner.webContents.emit('did-start-navigation', {}, 'https://other.test/', false, true);
      else f.controller.stop('a');
      await request;
      assert.equal(p.win.destroyed, true, close);
      const calls = f.sourcesCalls;
      f.advance(10_000); await settle();
      f.advance(200_000); await settle();
      assert.equal(f.sourcesCalls, calls, `${close}: no refresh after the picker closed`);
      assert.equal(f.ipc.listenerCount('dilla:screen-picker-choice'), 0);
    } finally { f.controller.stopAll(); }
  }
});

test('one fixed picker partition: two consecutive pickers both work and the second serves files', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'dilla-picker-twice-'));
  const f = await fixture(1, directory, true, false);
  try {
    await mkdir(join(directory, 'screen-picker'));
    await writeFile(join(directory, 'screen-picker', 'screen-picker.html'), '<!doctype html><title>picker</title>');
    const first = f.controller.request('a', f.owner, options);
    const p1 = await opened(f);
    assert.equal(f.sessions[0].partition, 'dilla-picker');
    p1.choose(null);
    assert.equal(await first, null);
    const serve = f.sessions[0].handler;
    assert.equal((await serve({ method: 'GET', url: 'dilla-internal://picker/' })).status, 404, 'nothing is served between pickers');
    const second = f.controller.request('a', f.owner, options);
    await settle();
    const win = f.windows.at(-1)!;
    assert.notEqual(win, p1.win);
    assert.equal(f.sessions.filter(s => s.partition === 'dilla-picker').length, 1, 'the partition is reused, not recreated');
    assert.equal(win.options.webPreferences.session, f.sessions[0]);
    assert.equal((await f.sessions[0].handler({ method: 'GET', url: 'dilla-internal://picker/' })).status, 200);
    const tiles = win.webContents.sends.filter((args: any[]) => args[0] === 'dilla:screen-picker-sources').at(-1)[1];
    f.ipc.emit('dilla:screen-picker-choice', { sender: win.webContents, senderFrame: win.webContents.mainFrame }, { id: tiles[0].id });
    const source = await second;
    assert.equal(source.displaySurface, 'monitor');
    assert.equal(f.windows.at(-1)!.webContents.posts[0].data.sourceId, 'screen:1:0');
  } finally { f.controller.stopAll(); await rm(directory, { recursive: true, force: true }); }
});

test('Wayland keeps the system portal: zero-size enumeration and no picker window', async () => {
  const f = await fixture(2);
  try {
    const source = await f.controller.request('a', f.owner, options);
    assert.equal(source.displaySurface, 'window');
    assert.equal(f.dialogOptions.title, 'Share your screen');
    assert.ok(f.windows.every(w => !String(w.options?.webPreferences?.preload ?? '').includes('screen-picker')));
  } finally { f.controller.stopAll(); }
});

