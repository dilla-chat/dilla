import { test as base, expect, _electron, type ElectronApplication, type Page } from '@playwright/test';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import type { DesktopBridge } from '../../src/contract.ts';
import { nativeTrace } from '../support/trace.ts';

interface NativePrompt {
 title: string; message: string; detail?: string; buttons: string[];
 defaultId: number; cancelId: number; owner: number | null;
}
interface CaptureResult {
 status: 'allowed' | 'denied'; error?: string; tracks: string[]; trusted: boolean; active: boolean;
}

let desktop: ElectronApplication | undefined;
let profile: string;
let instance: string;
let stopTrace: (() => Promise<void>) | undefined;
let nativeStderr = '';
const network: string[] = [];
const server = createServer((req, res) => {
 network.push(req.url!);
 res.writeHead(404, { 'content-type': 'text/html' });
 res.end('<script>globalThis.remoteCodeExecuted = true</script>untrusted instance response');
});
const test = base.extend<{ twoInstances: boolean }>({ twoInstances: [false, { option: true }] });

test.beforeAll(async () => {
 await new Promise<void>(done => server.listen(0, '127.0.0.1', done));
 instance = `http://127.0.0.1:${(server.address() as { port: number }).port}/`;
});
test.afterAll(async () => { server.closeAllConnections(); await new Promise<void>(done => server.close(() => done())); });
test.beforeEach(async ({ twoInstances }, info) => {
 network.length = 0;
 profile = await mkdtemp(join(tmpdir(), 'dilla-native-permissions-'));
 if (twoInstances) await writeFile(join(profile, 'desktop.json'), JSON.stringify({ version: 1, instances: [instance, instance + 'team/'], autostart: false }));
 // Fake devices make availability deterministic without opening physical hardware.
 // There is intentionally no fake UI/grant flag, permission handler replacement,
 // getUserMedia replacement, or OS privacy configuration change in this suite.
 desktop = await _electron.launch({ chromiumSandbox: true, args: [resolve('.'), `--profile=${profile}`, `--instance=${instance}`, '--allow-loopback', '--use-fake-device-for-media-stream'] });
 nativeStderr = ''; desktop.process().stderr?.on('data', chunk => { nativeStderr = (nativeStderr + String(chunk)).slice(-12000); });
 stopTrace = await nativeTrace(desktop.context(), info);
 await desktop.evaluate(({ BrowserWindow }) => { (globalThis as any).__permissionCrashes = []; for (const win of BrowserWindow.getAllWindows()) win.webContents.on('render-process-gone', (_event, details) => { (globalThis as any).__permissionCrashes.push(details); }); });
 const page = await desktop.firstWindow();
 await page.waitForLoadState('load');
 await expect(page.locator('#root')).not.toBeEmpty();
 await desktop.evaluate(({ dialog }) => {
  (globalThis as any).__permissionPrompts = [];
  (globalThis as any).__permissionAnswer = 0;
  // Only the human's native dialog answer is substituted. The shipping Session
  // handlers, owner checks, permission cache and Chromium media APIs stay real.
  dialog.showMessageBox = (async (...args: any[]) => {
   const options = args.at(-1), owner = args.length === 2 ? args[0].id : null;
   if (!['Dilla permission', 'Share your screen', 'Connect to a Dilla instance'].includes(options.title)) throw new Error(`Unexpected native dialog: ${options.title}`);
   (globalThis as any).__permissionPrompts.push({ title: options.title, message: options.message, detail: options.detail, buttons: options.buttons, defaultId: options.defaultId, cancelId: options.cancelId, owner });
   return { response: (globalThis as any).__permissionAnswer, checkboxChecked: false };
  }) as typeof dialog.showMessageBox;
 });
});
test.afterEach(async () => {
 try {
  if (desktop && test.info().status !== 'passed') {
   const crashes = await desktop.evaluate(() => (globalThis as any).__permissionCrashes).catch(() => []);
   await test.info().attach('native-permission-diagnostics', { body: JSON.stringify({ crashes, stderr: nativeStderr }), contentType: 'application/json' });
   console.log('NATIVE_PERMISSION_DIAGNOSTICS', JSON.stringify({ crashes, stderr: nativeStderr }));
  }
  await stopTrace?.();
 }
 finally {
  stopTrace = undefined;
  const owned = desktop; desktop = undefined;
  if (owned) {
   const child = owned.process();
   const exited = child.exitCode !== null || child.signalCode !== null ? Promise.resolve() : new Promise<void>(done => child.once('exit', () => done()));
   // Quit guards intentionally protect the app. Teardown exits only this owned
   // Electron process instead of asking the product to approve a normal quit.
   try { await owned.evaluate(() => { setImmediate(() => process.exit(0)); }); }
   catch (error) { if (child.exitCode === null && child.signalCode === null) throw error; }
   await exited;
  }
  // Chromium can release Windows profile files shortly after its parent exits.
  await rm(profile, { recursive: true, force: true, maxRetries: 8, retryDelay: 100 });
 }
});

function application(): ElectronApplication { if (!desktop) throw new Error('Electron fixture is not running'); return desktop; }
async function prompts(): Promise<NativePrompt[]> { return application().evaluate(() => (globalThis as any).__permissionPrompts); }
async function focus(page: Page): Promise<void> {
 const win = await application().browserWindow(page);
 try {
  await win.evaluate(win => { win.show(); win.focus(); });
  await page.bringToFront();
  await expect.poll(() => win.evaluate(win => win.isVisible() && win.isFocused())).toBe(true);
 } finally { await win.dispose(); }
}
async function capture(page: Page, kind: 'microphone' | 'camera' | 'screen'): Promise<CaptureResult> {
 await focus(page);
 await page.evaluate(kind => {
  const button = document.createElement('button'); button.textContent = `Request ${kind} for permission acceptance`;
  Object.assign(button.style, { position: 'fixed', inset: '10px', zIndex: '2147483647' });
  (globalThis as any).__permissionResult = undefined;
  button.onclick = event => {
   const trusted = event.isTrusted, active = navigator.userActivation.isActive;
   const request = kind === 'screen' ? navigator.mediaDevices.getDisplayMedia({ video: true, audio: false })
    : navigator.mediaDevices.getUserMedia(kind === 'microphone' ? { audio: true, video: false } : { audio: false, video: true });
   void request.then(stream => {
    const tracks = stream.getTracks().map(track => track.kind);
    stream.getTracks().forEach(track => track.stop());
    (globalThis as any).__permissionResult = { status: 'allowed', tracks, trusted, active };
   }, error => { (globalThis as any).__permissionResult = { status: 'denied', error: error.name, tracks: [], trusted, active }; });
  };
  document.body.append(button);
 }, kind);
 await page.getByRole('button', { name: `Request ${kind} for permission acceptance`, exact: true }).click();
 await page.waitForFunction(() => (globalThis as any).__permissionResult !== undefined);
 return page.evaluate(() => (globalThis as any).__permissionResult);
}
async function assertNoMediaSurface(page: Page): Promise<void> {
 expect(await page.evaluate(async () => ({
  labeledDevices: (await navigator.mediaDevices.enumerateDevices()).filter(device => device.label !== '').map(device => device.kind),
  attachedStreams: [...document.querySelectorAll('audio, video')].filter(media => (media as HTMLMediaElement).srcObject !== null).length,
  remoteCodeExecuted: (globalThis as any).remoteCodeExecuted,
  node: typeof (globalThis as any).require,
 }))).toEqual({ labeledDevices: [], attachedStreams: 0, remoteCodeExecuted: undefined, node: 'undefined' });
}

for (const kind of ['microphone', 'camera'] as const) {
 // A regression that ignores the native Deny answer or grants media by default
 // returns real fake-device tracks and fails this acceptance assertion.
 test(`the shipping permission boundary refuses ${kind} capture after native Deny`, async () => {
  const page = await application().firstWindow();
  await assertNoMediaSurface(page);
  expect(await capture(page, kind)).toEqual({ status: 'denied', error: 'NotAllowedError', tracks: [], trusted: true, active: true });
  expect(await prompts()).toEqual([{ title: 'Dilla permission', message: `Allow ${new URL(instance).host} to use microphone or camera?`, buttons: ['Deny', 'Allow'], defaultId: 0, cancelId: 0, owner: await application().evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].id) }]);
  await assertNoMediaSurface(page);
  expect(network).not.toContain('/');
 });
}

test.describe('instance permission ownership', () => {
 test.use({ twoInstances: true });
 test('a grant is reusable by its owner but cannot authorize another complete instance URL', async () => {
  await expect.poll(() => application().windows().length).toBe(2);
  const pages = application().windows();
  const first = pages[0], second = pages[1];
  await Promise.all(pages.map(page => expect(page.locator('#root')).not.toBeEmpty()));
  await focus(first);
  await application().evaluate(() => { (globalThis as any).__permissionAnswer = 1; });
  // Permission only: no Notification object or OS notification is created.
  expect(await first.evaluate(() => Notification.requestPermission())).toBe('granted');
  expect(await prompts()).toHaveLength(1);
  await application().evaluate(() => { (globalThis as any).__permissionAnswer = 0; });
  expect(await first.evaluate(() => Notification.requestPermission())).toBe('granted');
  expect(await prompts()).toHaveLength(1);
  await focus(second);
  expect(await second.evaluate(() => Notification.requestPermission())).toBe('denied');
  expect(await prompts()).toHaveLength(2);
  expect(await first.evaluate(() => Notification.permission)).toBe('granted');
  expect(await second.evaluate(() => Notification.permission)).toBe('denied');
  expect(await first.evaluate(async () => (await (window as any).dillaDesktop.info()).instance))
   .not.toBe(await second.evaluate(async () => (await (window as any).dillaDesktop.info()).instance));
 });
});

test('another sandboxed webContents cannot borrow an owning window permission grant', async () => {
 const owner = await application().firstWindow();
 await focus(owner);
 await application().evaluate(() => { (globalThis as any).__permissionAnswer = 1; });
 expect(await owner.evaluate(() => Notification.requestPermission())).toBe('granted');
 expect(await prompts()).toHaveLength(1);
 const otherWindow = application().waitForEvent('window');
 await application().evaluate(async ({ BrowserWindow }) => {
  const owner = BrowserWindow.getAllWindows()[0];
  // Hostile fixture deliberately reuses the owner's Session. Production never
  // creates this window; ownership must still defeat an accidental/hostile peer.
  const other = new BrowserWindow({ show: true, webPreferences: { session: owner.webContents.session, sandbox: true, contextIsolation: true, nodeIntegration: false } });
  await other.loadURL(owner.webContents.getURL());
 });
 const other = await otherWindow;
 await other.waitForLoadState('load');
 expect(await other.evaluate(() => Notification.requestPermission())).toBe('denied');
 expect(await capture(other, 'microphone')).toEqual({ status: 'denied', error: 'NotAllowedError', tracks: [], trusted: true, active: true });
 // No extra prompt: an unrelated webContents is rejected before a user grant.
 expect(await prompts()).toHaveLength(1);
 await assertNoMediaSurface(other);
 expect(await owner.evaluate(() => Notification.permission)).toBe('granted');
});

test('screen capture without a real user gesture is refused without opening a picker', async () => {
 const page = await application().firstWindow();
 await focus(page);
 // Playwright evaluates assertions with CDP activation. Run the request at
 // document startup after a real reload, before any renderer evaluation/click.
 await page.addInitScript(() => {
  window.addEventListener('DOMContentLoaded', () => {
   const active = navigator.userActivation.isActive;
   void navigator.mediaDevices.getDisplayMedia({ video: true }).then(stream => {
    const tracks = stream.getTracks().map(track => track.kind); stream.getTracks().forEach(track => track.stop());
    (globalThis as any).__ungesturedDisplayResult = { active, status: 'allowed', tracks };
   }, error => { (globalThis as any).__ungesturedDisplayResult = { active, status: 'denied', error: error.name, tracks: [] }; });
  }, { once: true });
 });
 await page.reload();
 await page.waitForFunction(() => (globalThis as any).__ungesturedDisplayResult !== undefined);
 const result = await page.evaluate(() => (globalThis as any).__ungesturedDisplayResult);
 expect(result).toMatchObject({ active: false, status: 'denied', tracks: [] });
 // Chromium can reject activation first; Electron's callback(null) reports
 // cancellation as AbortError after the shipping handler refuses the request.
 expect(['InvalidStateError', 'NotAllowedError', 'AbortError']).toContain(result.error);
 expect(await prompts()).toEqual([]);
 await assertNoMediaSurface(page);
});

test('legacy desktop getUserMedia cannot capture a valid screen source without a user gesture or picker consent', async () => {
 const page = await application().firstWindow();
 await focus(page);
 const sourceID = await application().evaluate(async ({ desktopCapturer }) => {
  const sources = await desktopCapturer.getSources({ types: ['screen'], thumbnailSize: { width: 0, height: 0 } });
  return sources[0]?.id;
 });
 expect(sourceID, 'The negative acceptance must address an actual capturable desktop source').toBeTruthy();
 await page.addInitScript(sourceID => {
  window.addEventListener('DOMContentLoaded', () => {
   const active = navigator.userActivation.isActive;
   const constraints = { audio: false, video: { mandatory: { chromeMediaSource: 'desktop', chromeMediaSourceId: sourceID } } } as unknown as MediaStreamConstraints;
   void navigator.mediaDevices.getUserMedia(constraints).then(stream => {
    const tracks = stream.getTracks().map(track => track.kind); stream.getTracks().forEach(track => track.stop());
    (globalThis as any).__legacyDesktopResult = { active, status: 'allowed', tracks };
   }, error => { (globalThis as any).__legacyDesktopResult = { active, status: 'denied', error: error.name, message: error.message, tracks: [] }; });
  }, { once: true });
 }, sourceID);
 await page.reload();
 await page.waitForFunction(() => (globalThis as any).__legacyDesktopResult !== undefined);
 const result = await page.evaluate(() => (globalThis as any).__legacyDesktopResult);
 const nativePrompts = await prompts();
 await test.info().attach('legacy-desktop-permission-result', { body: JSON.stringify({ result, nativePrompts }, null, 2), contentType: 'application/json' });
 console.log('LEGACY_DESKTOP_PERMISSION_RESULT', JSON.stringify({ result, nativePrompts }));
 expect(result).toMatchObject({ active: false, status: 'denied', tracks: [] });
 expect(['NotAllowedError', 'AbortError']).toContain(result.error);
 expect(nativePrompts).toEqual([]);
 await assertNoMediaSurface(page);
});

test('a trusted direct getDisplayMedia call cannot bypass the native broker picker', async () => {
 const page = await application().firstWindow();
 const result = await capture(page, 'screen');
 expect(result).toMatchObject({ status: 'denied', tracks: [], trusted: true, active: true });
 expect(['NotAllowedError', 'AbortError']).toContain(result.error);
 expect(await prompts()).toEqual([]);
 await assertNoMediaSurface(page);
});

test('legacy desktop video without a source ID and system audio remain denied in the account renderer', async () => {
 const page = await application().firstWindow(); await focus(page);
 page.on('console', message => { if (message.text().startsWith('LEGACY_UNSELECTED_START')) console.log(message.text()); });
 await page.addInitScript(() => {
  window.addEventListener('DOMContentLoaded', () => {
   void (async () => {
    const results = [];
    for (const constraints of [
     { audio: false, video: { mandatory: { chromeMediaSource: 'desktop' } } },
     // Electron's legacy system-audio API requires accompanying desktop video.
     { audio: { mandatory: { chromeMediaSource: 'desktop' } }, video: { mandatory: { chromeMediaSource: 'desktop' } } },
    ]) {
     const active = navigator.userActivation.isActive;
     console.info('LEGACY_UNSELECTED_START', JSON.stringify(constraints));
     try {
      const stream = await navigator.mediaDevices.getUserMedia(constraints as unknown as MediaStreamConstraints);
      const tracks = stream.getTracks().map(track => track.kind); stream.getTracks().forEach(track => track.stop());
      results.push({ active, status: 'allowed', tracks });
     } catch (error) { results.push({ active, status: 'denied', tracks: [], error: (error as DOMException).name }); }
    }
    (globalThis as any).__legacyUnselectedResults = results;
   })();
  }, { once: true });
 });
 await page.reload();
 await page.waitForFunction(() => (globalThis as any).__legacyUnselectedResults !== undefined);
 const results = await page.evaluate(() => (globalThis as any).__legacyUnselectedResults);
 expect(results).toHaveLength(2);
 for (const result of results) {
  expect(result).toMatchObject({ active: false, status: 'denied', tracks: [] });
  expect(['NotAllowedError', 'AbortError']).toContain(result.error);
 }
 expect(await prompts()).toEqual([]);
 await assertNoMediaSurface(page);
});

test('cancelling the typed native screen picker returns null without creating a broker', async () => {
 const page = await application().firstWindow(); await focus(page);
 await page.evaluate(() => {
  const button = document.createElement('button'); button.textContent = 'Choose desktop screen for acceptance';
  Object.assign(button.style, { position: 'fixed', inset: '10px', zIndex: '2147483647' });
  button.onclick = event => {
   void (window as unknown as { dillaDesktop: DesktopBridge }).dillaDesktop.requestScreenCapture!({ width: 640, height: 480, frameRate: 15 }).then((source: unknown) => {
    (globalThis as any).__typedScreenResult = { source, trusted: event.isTrusted };
   }, error => { (globalThis as any).__typedScreenError = String(error); });
  }; document.body.append(button);
 });
 await page.getByRole('button', { name: 'Choose desktop screen for acceptance', exact: true }).click();
 await page.waitForFunction(() => (globalThis as any).__typedScreenResult !== undefined || (globalThis as any).__typedScreenError !== undefined);
 expect(await page.evaluate(() => (globalThis as any).__typedScreenError)).toBeUndefined();
 expect(await page.evaluate(() => (globalThis as any).__typedScreenResult)).toEqual({ source: null, trusted: true });
 const shown = await prompts();
 expect(shown).toHaveLength(1);
 expect(shown[0]).toMatchObject({ title: 'Share your screen', defaultId: 0, cancelId: 0 });
 expect(shown[0].buttons[0]).toBe('Cancel');
 expect(shown[0].buttons.length).toBeGreaterThan(1);
 expect(await application().evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().filter(win => win.webContents.getURL() === 'dilla-internal://capture/').length)).toBe(0);
 await assertNoMediaSurface(page);
});

test('an unfamiliar native invite is refused before connecting or persisting its host', async () => {
 // A separate listening endpoint makes any contact observable even if the
 // attempted HTTPS handshake never reaches an HTTP request handler.
 const target = createServer(); let connections = 0;
 target.on('connection', socket => { connections++; socket.destroy(); });
 await new Promise<void>(done => target.listen(0, '127.0.0.1', done));
 try {
  const port = (target.address() as { port: number }).port;
  const beforeURL = application().windows()[0].url();
  const unfamiliar = `https://127.0.0.1:${port}/`;
  // Exercise the shipping native second-instance event, not OS protocol
  // registration. parseInvite's existing grammar always selects HTTPS.
  await application().evaluate(({ app }, port) => { app.emit('second-instance', {}, ['dilla', `dilla://invite/127.0.0.1:${port}/Code_123`], '', {}); }, port);
  await expect.poll(prompts).toHaveLength(1);
  expect((await prompts())[0]).toMatchObject({ title: 'Connect to a Dilla instance', message: `Connect to 127.0.0.1:${port}?`, detail: `Instance: ${unfamiliar}\nOnly continue if you recognize this address.`, buttons: ['Cancel', 'Connect'], defaultId: 0, cancelId: 0, owner: null });
  // Drain the completed async native invite handler before inspecting effects.
  await application().evaluate(() => new Promise<void>(done => setImmediate(done)));
  expect(application().windows()).toHaveLength(1);
  expect(application().windows()[0].url()).toBe(beforeURL);
  const saved = JSON.parse(await readFile(join(profile, 'desktop.json'), 'utf8'));
  expect(saved.instances).toEqual([instance]);
  expect(saved.instances).not.toContain(unfamiliar);
  expect(connections).toBe(0);
 } finally { target.closeAllConnections(); await new Promise<void>(done => target.close(() => done())); }
});
