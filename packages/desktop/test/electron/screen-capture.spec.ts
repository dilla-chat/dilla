import { test as base, expect, _electron, type ElectronApplication, type Page } from '@playwright/test';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { randomUUID } from 'node:crypto';
import { nativeTrace } from '../support/trace.ts';
import type { FailClosedResult, VideoObservation } from '../../../media/harness/failclosed.ts';

// The source is a real separate application window. Its pixels never enter the
// capture API, broker or RTP harness except through the operating system capturer.
let actor: ElectronApplication | undefined, desktop: ElectronApplication | undefined;
let otherInstance: Page | undefined;
const test = base.extend<{ twoInstances: boolean }>({ twoInstances: [false, { option: true }] });
let page: Page, directory: string, profile: string, title: string;
let stopTrace: (() => Promise<void>) | undefined;
type ValidationWindow = Window & { validation: { capture(): Promise<MediaStream>; screen(options?: { source?: string; receiver?: boolean; ms?: number }): Promise<ScreenResult> } };
type ScreenResult = FailClosedResult & { audioTracks: number };

async function exitOwned(app: ElectronApplication | undefined): Promise<void> {
 if (!app) return;
 const child = app.process();
 const exited = child.exitCode !== null || child.signalCode !== null ? Promise.resolve() : new Promise<void>(done => child.once('exit', () => done()));
 const requested = app.evaluate(() => { setImmediate(() => process.exit(0)); }).catch(error => { if (child.exitCode === null && child.signalCode === null) throw error; });
 await Promise.race([requested, exited]);
 await exited;
}
function app(): ElectronApplication { if (!desktop) throw new Error('Desktop fixture is not running'); return desktop; }
async function focusOwner(): Promise<void> {
 const window = await app().browserWindow(page);
 try {
  await window.evaluate(window => { window.show(); window.focus(); }); await page.bringToFront();
  await expect.poll(() => window.evaluate(window => window.isFocused() && window.isVisible())).toBe(true);
 } finally { await window.dispose(); }
}
async function brokers(): Promise<number[]> {
 return app().evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().filter(win => win.webContents.getURL() === 'dilla-internal://capture/').map(win => win.id));
}

test.beforeEach(async ({ twoInstances }, info) => {
 directory = await mkdtemp(join(tmpdir(), 'dilla-screen-actor-'));
 profile = await mkdtemp(join(tmpdir(), 'dilla-screen-owner-'));
 if (twoInstances) await writeFile(join(profile, 'desktop.json'), JSON.stringify({ version: 1, instances: ['http://127.0.0.1:1/', 'http://127.0.0.1:1/team/'], autostart: false }));
 title = `Dilla native checkerboard ${randomUUID()}`;
 await writeFile(join(directory, 'package.json'), JSON.stringify({ main: 'main.cjs' }));
 await writeFile(join(directory, 'main.cjs'), `const { app, BrowserWindow } = require('electron');
 const path = require('node:path'), fs = require('node:fs');
 fs.mkdirSync(path.join(__dirname, 'profile'), { recursive: true }); app.setPath('userData', path.join(__dirname, 'profile'));
 app.enableSandbox(); app.on('window-all-closed', () => {}); app.whenReady().then(async () => {
  const window = new BrowserWindow({ x: 0, y: 0, width: 600, height: 440, title: ${JSON.stringify(title)},
   webPreferences: { sandbox: true, contextIsolation: true, nodeIntegration: false, backgroundThrottling: false } });
  await window.loadFile('checker.html');
 });`);
 await writeFile(join(directory, 'checker.html'), `<!doctype html><title>${title}</title><style>html,body{margin:0;width:100%;height:100%;overflow:hidden}canvas{width:100%;height:100%;display:block}</style><canvas width="600" height="400"></canvas><script>
 const canvas = document.querySelector('canvas'), context = canvas.getContext('2d'); let count = 0;
 function draw() { const phase = Math.floor(count / 6) % 2;
  for(let y=0;y<8;y++) for(let x=0;x<12;x++) { context.fillStyle = (x+y+phase)%2 ? '#e62250' : '#10d7cf'; context.fillRect(x*50,y*50,50,50); }
  count++; window.actorFrames = count; requestAnimationFrame(draw);
 } draw();</script>`);
 actor = await _electron.launch({ chromiumSandbox: true, args: [directory] });
 const actorPage = await actor.firstWindow(); await actorPage.waitForFunction(() => (window as any).actorFrames > 2);
 desktop = await _electron.launch({ chromiumSandbox: true, args: [resolve('.'), `--profile=${profile}`, '--instance=http://127.0.0.1:1/', '--allow-loopback', '--allow-loopback-in-peer-connection', '--media-validation'] });
 stopTrace = await nativeTrace(desktop.context(), info);
 page = await desktop.firstWindow(); await page.waitForFunction(() => 'validation' in window);
 otherInstance = undefined;
 if (twoInstances) {
  await expect.poll(() => app().windows().length).toBe(2); otherInstance = app().windows()[1];
  await otherInstance.waitForFunction(() => 'validation' in window);
  const other = await app().browserWindow(otherInstance); try { await other.evaluate(window => window.hide()); } finally { await other.dispose(); }
 }
 await page.evaluate(() => (window as any).dillaDesktop.send({ type: 'window-mode', mode: 'workspace', height: null }));
 const owner = await desktop.browserWindow(page);
 try { await owner.evaluate(window => window.setBounds({ x: 650, y: 0, width: 900, height: 650 })); } finally { await owner.dispose(); }
 await desktop.evaluate(({ dialog }, title) => {
  (globalThis as any).__screenSelections = [];
  (globalThis as any).__screenNetworkPermissions = [];
  dialog.showMessageBox = (async (...args: any[]) => {
   const options = args.at(-1);
   if (options.title === 'Dilla permission' && [
    'Allow 127.0.0.1:1 to use local network connections for calls?',
    'Allow 127.0.0.1:1 to use connections to a local Dilla service?',
   ].includes(options.message)) {
    if (JSON.stringify(options.buttons) !== '["Deny","Allow"]' || options.defaultId !== 0 || options.cancelId !== 0) throw new Error('Unexpected native network consent');
    (globalThis as any).__screenNetworkPermissions.push(options.message);
    return { response: 1, checkboxChecked: false };
   }
   if (options.title !== 'Share your screen') throw new Error(`Unexpected native dialog: ${options.title}`);
   const matches = options.buttons.flatMap((name: string, index: number) => name === title ? [index] : []);
   if (matches.length !== 1 || matches[0] === 0) throw new Error('The real actor window must be offered exactly once; no screen fallback');
   (globalThis as any).__screenSelections.push({ title: options.buttons[matches[0]], cancelFirst: options.buttons[0] === 'Cancel' });
   return { response: matches[0], checkboxChecked: false };
  }) as typeof dialog.showMessageBox;
 }, title);
 await focusOwner();
});
test.afterEach(async () => {
 try { await stopTrace?.(); }
 finally {
  stopTrace = undefined;
  const owner = desktop, source = actor; desktop = undefined; actor = undefined;
  try { await exitOwned(owner); } finally { await exitOwned(source); }
  await Promise.all([directory, profile].filter(Boolean).map(path => rm(path, { recursive: true, force: true, maxRetries: 8, retryDelay: 100 })));
 }
});

async function startAction(action: 'screen' | 'capture', options?: { source?: string; receiver?: boolean; ms?: number }): Promise<void> {
 await focusOwner();
 await page.evaluate(({ action, options }) => {
  (globalThis as any).__screenResult = undefined; (globalThis as any).__screenError = undefined;
  const observePort = (event: MessageEvent) => {
   if (event.source === window && event.data?.type === 'dilla-screen-capture-port' && typeof event.data.id === 'string') {
    (globalThis as any).__ownedCaptureID = event.data.id; window.removeEventListener('message', observePort);
   }
  }; window.addEventListener('message', observePort);
  const button = document.createElement('button'); button.textContent = 'Start native screen acceptance';
  Object.assign(button.style, { position: 'fixed', left: '10px', top: '10px', zIndex: '2147483647' });
  button.onclick = event => {
   (globalThis as any).__screenTrustedClick = event.isTrusted;
   if (action === 'screen') {
    void (window as unknown as ValidationWindow).validation.screen(options).then(result => { (globalThis as any).__screenResult = result; }, error => { (globalThis as any).__screenError = String(error); });
   } else {
    void (window as unknown as ValidationWindow).validation.capture().then(stream => {
     (globalThis as any).__capturedScreen = stream;
     (globalThis as any).__screenResult = { audioTracks: stream.getAudioTracks().length, videoTracks: stream.getVideoTracks().length, settings: stream.getVideoTracks()[0].getSettings() };
    }, error => { (globalThis as any).__screenError = String(error); });
   }
  }; document.body.append(button);
 }, { action, options });
 await page.getByRole('button', { name: 'Start native screen acceptance', exact: true }).click();
}
async function completed<T>(): Promise<T> {
 await page.waitForFunction(() => (globalThis as any).__screenResult !== undefined || (globalThis as any).__screenError !== undefined);
 const outcome = await page.evaluate(() => ({ result: (globalThis as any).__screenResult, error: (globalThis as any).__screenError, trusted: (globalThis as any).__screenTrustedClick }));
 expect(outcome.error).toBeUndefined(); expect(outcome.trusted).toBe(true);
 await test.info().attach('native-screen-result', { body: JSON.stringify(outcome.result, null, 2), contentType: 'application/json' });
 console.log('NATIVE_SCREEN_RESULT', JSON.stringify(outcome.result));
 const networkPrompts = await app().evaluate(() => (globalThis as any).__screenNetworkPermissions as string[]);
 for (const message of networkPrompts) expect(['Allow 127.0.0.1:1 to use local network connections for calls?', 'Allow 127.0.0.1:1 to use connections to a local Dilla service?']).toContain(message);
 await test.info().attach('native-screen-network-consent', { body: JSON.stringify(networkPrompts), contentType: 'application/json' });
 console.log('NATIVE_SCREEN_NETWORK_PERMISSION_PROMPTS', JSON.stringify(networkPrompts));
 return outcome.result as T;
}
async function assertBrokerIsolation(): Promise<void> {
 await expect.poll(brokers).toHaveLength(1);
 const state = await app().evaluate(async ({ BrowserWindow }) => {
  const broker = BrowserWindow.getAllWindows().find(win => win.webContents.getURL() === 'dilla-internal://capture/')!;
  const preferences = (broker.webContents as any).getLastWebPreferences();
  const surface = await broker.webContents.executeJavaScript(`(async () => {
   const worker = await new Promise(resolve => {
    let attempted, timer;
    const finish = result => { clearTimeout(timer); window.removeEventListener('securitypolicyviolation', denied); attempted?.terminate(); resolve(result); };
    const denied = event => { if (event.effectiveDirective === 'worker-src') finish('blocked-by-csp'); };
    window.addEventListener('securitypolicyviolation', denied);
    timer = setTimeout(() => finish('no-policy-event'), 1500);
    try {
     attempted = new Worker('data:text/javascript,postMessage("broker-worker-executed")');
     attempted.onmessage = () => finish('executed'); attempted.onerror = event => event.preventDefault();
    } catch (error) { if (error.name !== 'SecurityError') finish(error.name); }
   });
   return { desktop: typeof window.dillaDesktop, core: typeof window.dillaCore, worker, storage: localStorage.getItem('owner-device-secret'), node: typeof window.require };
  })()`);
  return { visible: broker.isVisible(), focusable: broker.isFocusable(), sandbox: preferences.sandbox, contextIsolation: preferences.contextIsolation, nodeIntegration: preferences.nodeIntegration, surface,
   visibleOwners: BrowserWindow.getAllWindows().filter(win => win.isVisible()).length };
 });
 expect(state).toEqual({ visible: false, focusable: false, sandbox: true, contextIsolation: true, nodeIntegration: false,
  surface: { desktop: 'undefined', core: 'undefined', worker: 'blocked-by-csp', storage: null, node: 'undefined' }, visibleOwners: 1 });
}
function assertPattern(observation: VideoObservation): void {
 expect(observation.frames).toBeGreaterThan(2); expect(observation.spanMs).toBeGreaterThan(4000);
 expect(observation.width).toBeGreaterThan(16); expect(observation.width).toBeLessThanOrEqual(640);
 expect(observation.height).toBeGreaterThan(16); expect(observation.height).toBeLessThanOrEqual(480);
 expect(observation.patternedFrames).toBeGreaterThan(2); expect(observation.distinctSamples).toBeGreaterThan(1);
 expect(observation.maxRedFraction).toBeGreaterThan(0.1); expect(observation.maxCyanFraction).toBeGreaterThan(0.1);
}
async function assertDirectCaptureDenied(): Promise<void> {
 const results = await page.evaluate(async () => {
  const outcomes = [];
  for (const getStream of [() => navigator.mediaDevices.getDisplayMedia({ video: true }), () => navigator.mediaDevices.getUserMedia({ video: { mandatory: { chromeMediaSource: 'desktop' } } } as unknown as MediaStreamConstraints)]) {
   try { const stream = await getStream(); const tracks = stream.getTracks().map(track => track.kind); stream.getTracks().forEach(track => track.stop()); outcomes.push({ allowed: true, tracks }); }
   catch (error) { outcomes.push({ allowed: false, tracks: [], error: (error as DOMException).name }); }
  } return outcomes;
 });
 for (const outcome of results) expect(outcome).toMatchObject({ allowed: false, tracks: [] });
}

test('a selected native application window stays video-only and encrypted through the real media worker', async () => {
 await page.evaluate(() => localStorage.setItem('owner-device-secret', 'must-stay-in-owner'));
 await startAction('screen', { ms: 5000 });
 await assertBrokerIsolation();
 const result = await completed<ScreenResult>();
 expect(result.audioTracks).toBe(0); expect(result.transport?.sender).toBe('connected'); expect(result.transport?.receiver).toBe('connected');
 const encrypted = Object.values(result.sender?.encrypted ?? {}).flatMap(Object.values).reduce((sum, count) => sum + count, 0);
 const decrypted = Object.values(result.receiver?.decrypted ?? {}).reduce((sum, count) => sum + count, 0);
 expect(encrypted).toBeGreaterThan(10); expect(decrypted).toBeGreaterThan(10);
 expect(result.bytesSent).toBeGreaterThan(0); expect(result.rendered).toBeGreaterThan(0); expect(result.errors).toEqual([]); expect(result.sender?.passedThrough).toBe(0);
 expect(result.senderTrackEnded).toBe(false); expect(result.nativeVideo).toBeDefined();
 assertPattern(result.nativeVideo!.input); assertPattern(result.nativeVideo!.receiver);
 expect(result.nativeVideo!.observationMs).toBeGreaterThanOrEqual(5000);
 await expect.poll(brokers).toHaveLength(0);
 expect(await app().evaluate(() => (globalThis as any).__screenSelections)).toEqual([{ title, cancelFirst: true }]);
 await assertDirectCaptureDenied();
});

test('an unbound native screen source is blocked while its real input remains live', async () => {
 await startAction('screen', { source: 'unknown', receiver: false, ms: 5000 });
 await assertBrokerIsolation();
 const result = await completed<ScreenResult>();
 expect(result.audioTracks).toBe(0); expect(result.bytesSent).toBe(0); expect(result.rendered).toBe(0);
 expect(result.sender?.dropped.blocked).toBeGreaterThan(10); expect(result.sender?.passedThrough).toBe(0);
 expect(result.unpublished).toHaveLength(1); expect(result.senderTrackEnded).toBe(false);
 assertPattern(result.nativeVideo!.input); expect(result.nativeVideo!.receiver.frames).toBe(0);
 await expect.poll(brokers).toHaveLength(0);
});

test.describe('capture instance ownership', () => {
 test.use({ twoInstances: true });
 test('a foreign instance cannot stop or reconfigure the capture and stopping its track destroys the broker', async () => {
  await startAction('capture');
  const capture = await completed<{ audioTracks: number; videoTracks: number; settings: MediaTrackSettings }>();
  expect(capture).toMatchObject({ audioTracks: 0, videoTracks: 1, settings: { displaySurface: 'window' } });
  await assertBrokerIsolation();
  const id = await page.evaluate(() => (globalThis as any).__ownedCaptureID as string);
  expect(id).toMatch(/^[0-9a-f-]{36}$/);
  expect(otherInstance).toBeDefined();
  await otherInstance!.evaluate(async id => { await (window as any).dillaDesktop.send({ type: 'stop-screen-capture', id }); }, id);
  expect(await brokers()).toHaveLength(1);
  expect(await otherInstance!.evaluate(async id => {
   try { await (window as any).dillaDesktop.send({ type: 'configure-screen-capture', id, options: { width: 320, height: 240, frameRate: 10 } }); return ''; }
   catch (error) { return String(error); }
  }, id)).toContain('Screen capture unavailable');
  expect(await page.evaluate(() => (globalThis as any).__capturedScreen.getVideoTracks()[0].readyState)).toBe('live');
  await page.evaluate(() => (globalThis as any).__capturedScreen.getTracks().forEach((track: MediaStreamTrack) => track.stop()));
  await expect.poll(brokers).toHaveLength(0);
 });
});

test('owner navigation releases its native broker', async () => {
 await startAction('capture'); await completed(); await assertBrokerIsolation();
 await page.reload();
 await expect.poll(brokers).toHaveLength(0);
 expect(await app().evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().filter(win => win.isVisible()).length)).toBe(1);
});

test('closing the selected source ends its consumer track and destroys the broker', async () => {
 await startAction('capture'); await completed(); await assertBrokerIsolation();
 await actor!.evaluate(({ BrowserWindow }) => { BrowserWindow.getAllWindows()[0].destroy(); });
 await expect.poll(brokers).toHaveLength(0);
 await expect.poll(() => page.evaluate(() => (globalThis as any).__capturedScreen.getVideoTracks()[0].readyState)).toBe('ended');
});
