import { useNativeViewport } from '../support/viewport.ts';
import { captureNativeDownload } from '../support/download.ts';
import { test as base, expect, _electron, type ElectronApplication } from '@playwright/test';
import { createServer } from 'node:http';
import { mkdtemp, rm, writeFile, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createHash } from 'node:crypto';

let app: ElectronApplication;
let profile: string;
let instance: string;
const test = base.extend<{ instanceSuffix: string; multiInstance: boolean }>({ instanceSuffix: ['', { option: true }], multiInstance: [false, { option: true }] });
const network: string[] = [];
const server = createServer((req, res) => {
  network.push(req.url!);
  if (req.url === '/team/v1/echo') {
    let body = ''; req.setEncoding('utf8'); req.on('data', chunk => { body += chunk; });
    req.on('end', () => { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ body, method: req.method })); });
    return;
  }
  res.writeHead(404, { 'content-type': 'text/html' });
  res.end('<script>globalThis.remoteCodeExecuted = true</script>remote client');
});
server.on('upgrade', (req, socket) => {
  const accept = createHash('sha1').update(String(req.headers['sec-websocket-key']) + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
  socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ' + accept + '\r\n\r\n');
  const path = Buffer.from(req.url!);
  socket.write(Buffer.concat([Buffer.from([0x81, path.length]), path]));
  socket.on('error', () => {});
  socket.on('data', () => { socket.end(); });
});

test.beforeAll(async () => {
  await new Promise<void>(done => server.listen(0, '127.0.0.1', done));
  instance = `http://127.0.0.1:${(server.address() as { port: number }).port}/`;
});
test.afterAll(async () => { await new Promise<void>(done => server.close(() => done())); });
test.beforeEach(async ({ instanceSuffix, multiInstance }) => {
  profile = await mkdtemp(join(tmpdir(), 'dilla-electron-'));
  if (multiInstance) await writeFile(join(profile, 'desktop.json'), JSON.stringify({ version: 1, instances: [instance, instance + 'team/'], autostart: false }));
  app = await _electron.launch({ chromiumSandbox: true, args: [resolve('.'), `--profile=${profile}`, `--instance=${instance}${instanceSuffix}`, '--allow-loopback'], timeout: 30_000 });
  const page = await app.firstWindow(); await page.waitForLoadState('load'); await useNativeViewport(app, page);
});
test.afterEach(async () => { await app?.close(); await rm(profile, { recursive: true, force: true }); });

test('runs the installed React client in an isolated sandbox without executing server HTML', async () => {
  const page = await app.firstWindow();
  await page.waitForURL(instance);
  await expect(page.locator('#root')).not.toBeEmpty();
  const info = await page.evaluate(() => (window as any).dillaDesktop.info());
  expect(info.instance).toBe(instance);
  await expect(page.getByRole('button', { name: 'Close login window', exact: true })).toBeVisible();
  await page.evaluate(() => (window as any).dillaDesktop.send({ type: 'window-mode', mode: 'workspace', height: null }));
  await page.evaluate(() => { document.documentElement.dataset.desktopWindow = 'workspace'; });
  await expect(page.getByRole('button', { name: 'Dilla menu', exact: true })).toBeVisible();
  expect(await page.locator('.dw-desktop-titlebar__menu').evaluate(node => Math.round(node.getBoundingClientRect().height))).toBe(28);
  expect(await page.locator('.d-splash, .d-onboarding-frame').first().evaluate(node => node.getBoundingClientRect().top)).toBe(0);
  await page.screenshot({ path: 'test-results/integrated-titlebar.png' });
  expect(info.capabilities.applicationAudio).toBe(true);
  expect(await page.evaluate(() => ({ node: typeof (window as any).require, process: typeof (window as any).process, remote: (window as any).remoteCodeExecuted })))
    .toEqual({ node: 'undefined', process: 'undefined', remote: undefined });
  const preferences = await app.evaluate(({ BrowserWindow }) => (BrowserWindow.getAllWindows()[0].webContents as any).getLastWebPreferences());
  expect(preferences.sandbox).toBe(true);
  expect(preferences.contextIsolation).toBe(true);
  expect(preferences.nodeIntegration).toBe(false);
  expect(network).not.toContain('/');
  expect(network.some(path => path.startsWith('/assets/'))).toBe(false);
  await page.evaluate(() => { const script = document.createElement('script'); script.src = '/v1/remote-code.js'; document.head.appendChild(script); });
  await expect.poll(() => page.evaluate(() => (window as any).remoteCodeExecuted)).toBeUndefined();
  await expect(page.locator('#root')).not.toBeEmpty();
});

test('focused copy writes text to the system clipboard while clipboard reads and hidden writes stay denied', async () => {
 const page = await app.firstWindow();
 await page.evaluate(() => {
  const button = document.createElement('button'); button.textContent = 'Copy acceptance text';
  button.onclick = () => { void navigator.clipboard.writeText('dilla clipboard acceptance').then(() => { button.dataset.copied = 'true'; }, () => { button.dataset.copied = 'false'; }); };
  document.body.appendChild(button);
 });
 await page.getByRole('button', { name: 'Copy acceptance text', exact: true }).click();
 await expect(page.getByRole('button', { name: 'Copy acceptance text', exact: true })).toHaveAttribute('data-copied', 'true');
 expect(await app.evaluate(({ clipboard }) => clipboard.readText())).toBe('dilla clipboard acceptance');
 expect(await page.evaluate(async () => { try { await navigator.clipboard.readText(); return 'allowed'; } catch { return 'denied'; } })).toBe('denied');
 await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].hide());
 expect(await page.evaluate(async () => { try { await navigator.clipboard.writeText('hidden write'); return 'allowed'; } catch { return 'denied'; } })).toBe('denied');
 expect(await app.evaluate(({ clipboard }) => clipboard.readText())).toBe('dilla clipboard acceptance');
});

test('protected reload needs an explicit native confirmation and cancelling retains the document', async () => {
 const page = await app.firstWindow();
 page.on('dialog', () => {}); // Electron's native unload handler owns this decision.
 await page.evaluate(() => {
  (window as any).__beforeReload = true;
  window.addEventListener('beforeunload', event => { event.preventDefault(); event.returnValue = ''; });
 });
 await app.evaluate(({ BrowserWindow, dialog }) => {
  (globalThis as any).__reloadPrompts = 0;
  dialog.showMessageBoxSync = ((_win: unknown, options: any) => {
   if (options.title !== 'Reload Dilla?') throw new Error('Unexpected native prompt');
   (globalThis as any).__reloadPrompts++; return 0;
  }) as typeof dialog.showMessageBoxSync;
  BrowserWindow.getAllWindows()[0].webContents.reload();
 });
 await expect.poll(() => app.evaluate(() => (globalThis as any).__reloadPrompts)).toBe(1);
 expect(await page.evaluate(() => (window as any).__beforeReload)).toBe(true);
 const loaded = page.waitForEvent('load');
 await app.evaluate(({ BrowserWindow, dialog }) => {
  dialog.showMessageBoxSync = (() => 1) as typeof dialog.showMessageBoxSync;
  BrowserWindow.getAllWindows()[0].webContents.reload();
 });
 await loaded;
 expect(await page.evaluate(() => (window as any).__beforeReload)).toBeUndefined();
});

test('rejects malicious IPC and remote document navigation and keeps storage across reload', async () => {
  const page = await app.firstWindow();
  await page.waitForURL(instance);
  const error = await page.evaluate(async () => {
    try { await (window as any).dillaDesktop.send({ type: 'exec', command: 'touch /tmp/unsafe' }); return ''; }
    catch (e) { return String(e); }
  });
  expect(error).toContain('Unknown desktop command');
  await page.evaluate(() => localStorage.setItem('desktop-persistence', 'kept'));
  await page.reload();
  expect(await page.evaluate(() => localStorage.getItem('desktop-persistence'))).toBe('kept');
  await page.evaluate(() => { location.href = '/v1/remote-document'; });
  // Playwright clears its cached URL on a navigation that Electron cancels before commit.
  // Check both Chromium's live document and Electron's native window URL.
  expect(await page.evaluate(() => location.href)).toBe(instance);
  expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].webContents.getURL())).toBe(instance);
  expect(await page.evaluate(() => (window as any).dillaDesktop.info())).toMatchObject({ instance });
});

test('closing a window keeps the client alive for background work', async () => {
  const page = await app.firstWindow();
  await page.waitForURL(instance);
  await expect.poll(() => app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].isVisible())).toBe(true);
  await page.getByRole('button', { name: 'Close login window', exact: true }).click();
  await expect.poll(() => app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().map(win => ({ visible: win.isVisible(), destroyed: win.isDestroyed() }))))
    .toEqual([{ visible: false, destroyed: false }]);
});

test.describe('path-scoped instance', () => {
  test.use({ instanceSuffix: 'team/' });
  test('forwards real API requests and POST bodies to the selected base path', async () => {
    const page = await app.firstWindow();
    await page.waitForURL(instance);
    await expect(page.locator('#root')).not.toBeEmpty();
    expect(await page.evaluate(() => (window as any).dillaDesktop.info())).toMatchObject({ instance: instance + 'team/' });
    const echoed = await page.evaluate(async () => (await fetch('/v1/echo', { method: 'POST', body: 'preserved body' })).json());
    expect(echoed).toEqual({ body: 'preserved body', method: 'POST' });
    expect(network).toContain('/team/v1/echo');
    await expect.poll(() => network.includes('/team/v1/instance')).toBe(true);
    const socketPath = await page.evaluate(async () => {
      const info = await (window as any).dillaDesktop.info();
      // The core receives this same base before boot; its gateway appends /gateway to the base.
      const url = info.instance.replace(/\/$/, '').replace(/^http/, 'ws') + '/gateway?ticket=public-test';
      return new Promise<string>((resolve, reject) => {
        const socket = new WebSocket(url);
        socket.onmessage = event => { resolve(String(event.data)); socket.close(); };
        socket.onerror = () => { reject(new Error('gateway connection failed')); };
      });
    });
    expect(socketPath).toBe('/team/gateway?ticket=public-test');
  });
});

test.describe('multiple instances', () => {
  test.use({ multiInstance: true });
  test('isolates storage for two complete URLs on the same host', async () => {
    await expect.poll(() => app.windows().length).toBe(2);
    const [first, second] = app.windows();
    await expect(first.locator('#root')).not.toBeEmpty(); await expect(second.locator('#root')).not.toBeEmpty();
    await first.evaluate(() => localStorage.setItem('private-to-this-instance', 'kept'));
    expect(await second.evaluate(() => localStorage.getItem('private-to-this-instance'))).toBe(null);
    const identities = await Promise.all([first, second].map(page => page.evaluate(async () => (await (window as any).dillaDesktop.info()).instance)));
    expect(new Set(identities)).toEqual(new Set([instance, instance + 'team/']));
  });
});


test('short startup content keeps an opaque theme-matched background across window resizes', async () => {
  const page = await app.firstWindow();
  await expect(page.locator('.d-splash')).toBeVisible();
  // Reproduce a small content surface with unused space around it.
  await page.locator('.d-splash').evaluate(node => { (node as HTMLElement).style.minHeight = '120px'; });
  for (const theme of ['signal']) {
    await page.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
    const nativeColor = await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].getBackgroundColor());
    const colors = await page.evaluate(nativeColor => {
      const canvas = document.createElement('canvas'), context = canvas.getContext('2d')!;
      // Electron returns #AARRGGBB; CSS uses #RRGGBBAA.
      const color = nativeColor.length === 9 ? '#' + nativeColor.slice(3) + nativeColor.slice(1, 3) : nativeColor;
      context.fillStyle = color; context.fillRect(0, 0, 1, 1);
      const native = [...context.getImageData(0, 0, 1, 1).data];
      context.clearRect(0, 0, 1, 1); context.fillStyle = getComputedStyle(document.body).backgroundColor; context.fillRect(0, 0, 1, 1);
      return { native, document: [...context.getImageData(0, 0, 1, 1).data] };
    }, nativeColor);
    expect(colors.native).toEqual(colors.document);
    expect(colors.native[3]).toBe(255);
    for (const size of [{ width: 900, height: 600 }, { width: 1240, height: 820 }]) {
      await page.setViewportSize(size);
      const samples = await page.evaluate(async () => {
        const samples: number[][] = [];
        for (let i = 0; i < 10; i++) {
          await new Promise(requestAnimationFrame);
          samples.push([innerHeight, document.documentElement.scrollHeight, document.body.getBoundingClientRect().bottom, document.getElementById('root')!.getBoundingClientRect().bottom]);
        }
        return samples;
      });
      expect(new Set(samples.map(sample => JSON.stringify(sample))).size).toBe(1);
      expect(samples[0].slice(1)).toEqual([size.height, size.height, size.height]);
    }
  }
});


test('downloads a decrypted local attachment while refusing blob document navigation', async () => {
  const page = await app.firstWindow();
  await expect(page.locator('#root')).not.toBeEmpty();
  const download = await captureNativeDownload(app, page, () => page.evaluate(() => {
    const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob(['decrypted local attachment'], { type: 'text/plain' })); a.download = 'attachment.txt'; document.body.append(a); a.click(); a.remove();
  }));
  expect(await readFile(download.path, 'utf8')).toBe('decrypted local attachment');
  expect(download.filename).toBe('attachment.txt');
  await page.evaluate(() => { location.href = URL.createObjectURL(new Blob(['<script>globalThis.blobDocumentExecuted=true</script>'], { type: 'text/html' })); });
  expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].webContents.getURL())).toBe(instance);
  expect(await page.evaluate(() => (globalThis as any).blobDocumentExecuted)).toBeUndefined();
});

test('a desktop resize fills the real native content area and cannot shrink into a mobile window', async () => {
 const page = await app.firstWindow(); await expect(page.locator('#root')).not.toBeEmpty();
 await page.evaluate(() => (window as any).dillaDesktop.send({ type: 'window-mode', mode: 'workspace', height: null }));
 await page.setViewportSize({ width: 900, height: 600 });
 expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].getContentSize())).toEqual([900, 600]);
 expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].getMinimumSize()[0])).toBeGreaterThanOrEqual(800);
 await page.setViewportSize({ width: 360, height: 400 });
 expect(await page.evaluate(() => ({ width: innerWidth, height: innerHeight }))).toEqual({ width: 900, height: 600 });
 expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].getContentSize())).toEqual([900, 600]);
});


test('startup and sign-in use one compact window which expands only for the workspace', async () => {
 const page = await app.firstWindow(); await expect(page.locator('#root')).not.toBeEmpty();
 const id = await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].id);
 expect(await app.evaluate(({ BrowserWindow }) => ({ size: BrowserWindow.getAllWindows()[0].getContentSize(), resizable: BrowserWindow.getAllWindows()[0].isResizable() }))).toEqual({ size: [400, 280], resizable: false });
 await page.evaluate(() => (window as any).dillaDesktop.send({ type: 'window-mode', mode: 'authentication', height: 500 }));
 expect(await page.evaluate(() => [innerWidth, innerHeight])).toEqual([400, 500]);
 await page.evaluate(() => (window as any).dillaDesktop.send({ type: 'window-mode', mode: 'workspace', height: null }));
 expect(await app.evaluate(({ BrowserWindow }) => ({ id: BrowserWindow.getAllWindows()[0].id, count: BrowserWindow.getAllWindows().length, resizable: BrowserWindow.getAllWindows()[0].isResizable() }))).toEqual({ id, count: 1, resizable: true });
 await page.waitForFunction(() => innerWidth >= 900 && innerHeight >= 600);
 await page.screenshot({ path: 'test-results/workspace-native-bounds.png' });
});
