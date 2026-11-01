import { test as base, expect, _electron, type ElectronApplication } from '@playwright/test';
import { createServer } from 'node:http';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
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
});
test.afterEach(async () => { await app?.close(); await rm(profile, { recursive: true, force: true }); });

test('runs the installed React client in an isolated sandbox without executing server HTML', async () => {
  const page = await app.firstWindow();
  await page.waitForURL(instance);
  await expect(page.locator('#root')).not.toBeEmpty();
  const info = await page.evaluate(() => (window as any).dillaDesktop.info());
  expect(info.instance).toBe(instance);
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
  await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].close());
  expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().map(win => ({ visible: win.isVisible(), destroyed: win.isDestroyed() }))))
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
