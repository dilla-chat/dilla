import { expect, type BrowserContext, type Page, type Worker } from '@playwright/test';
import { arr, decode, encode } from '../../packages/client-core/src/cbor';
import { decodeFrame } from '../../packages/client-core/src/gateway/frames';
import { addServerVia, copy, instanceInvite, joinCommunity, openChannel, signUp, test, WAIT } from './support/app';
import { installWorkerPort, portCall, portSlice } from './support/worker-port';
import { removeHostData, startTestHost, type StartedHost } from './support/host';

interface Probe { offset: number; packets: number[][]; socket: WebSocket | null }

// Playwright's clock controls document timers, not dedicated-worker clocks. Advance the worker's
// wall/monotonic readings explicitly for input coalescing; its actual socket and heartbeat timers
// remain real. No product test RPC, packet sender or protocol capability is added for this probe.
async function probe(page: Page): Promise<Worker> {
  const worker = page.workers().find(w => /\/assets\/entry-[\w-]+\.js$/.test(w.url()));
  if (!worker) throw new Error('the shipping core worker did not start');
  await worker.evaluate(() => {
    const root = globalThis as unknown as { __realtime: Probe };
    root.__realtime = { offset: 0, packets: [], socket: null };
    const wall = Date.now.bind(Date);
    const mono = performance.now.bind(performance);
    Date.now = () => wall() + root.__realtime.offset;
    Object.defineProperty(performance, 'now', { configurable: true, value: () => mono() + root.__realtime.offset });
    const send = WebSocket.prototype.send;
    WebSocket.prototype.send = function(data) {
      root.__realtime.socket = this;
      if (data instanceof ArrayBuffer) root.__realtime.packets.push(Array.from(new Uint8Array(data)));
      else if (ArrayBuffer.isView(data)) root.__realtime.packets.push(Array.from(new Uint8Array(data.buffer, data.byteOffset, data.byteLength)));
      send.call(this, data);
    };
  });
  return worker;
}

function requests(context: BrowserContext) {
  const seen = { version: 0, activity: 0, stats: 0 };
  context.on('request', request => {
    const path = new URL(request.url()).pathname;
    if (path === '/app-version.json') seen.version++;
    if (path === '/v1/accounts/me/presence/activity') seen.activity++;
    if (/^\/v1\/calls\/[^/]+\/stats$/.test(path)) seen.stats++;
  });
  return seen;
}
async function advance(page: Page, worker: Worker, ms: number) {
  await worker.evaluate(value => { (globalThis as unknown as { __realtime: Probe }).__realtime.offset += value; }, ms);
  await page.clock.runFor(ms);
}
async function ops(worker: Worker): Promise<number[]> {
  return (await worker.evaluate(() => (globalThis as unknown as { __realtime: Probe }).__realtime.packets))
    .map(bytes => decodeFrame(new Uint8Array(bytes)).op);
}
async function notice(worker: Worker, entry: string) {
  const bytes = Array.from(encode([56, 0, null, [entry]]));
  await worker.evaluate(value => {
    const socket = (globalThis as unknown as { __realtime: Probe }).__realtime.socket;
    if (!socket) throw new Error('the probe has not seen an outbound frame');
    socket.dispatchEvent(new MessageEvent('message', { data: new Uint8Array(value).buffer }));
  }, bytes);
}

const connection = (page: Page) => portSlice<{ status: string; wireVersion?: number; appBuild?: string }>(page, 'connection');

test('wire 5 stays free of periodic version/activity/stats HTTP for fifteen idle minutes and reconciles reconnects', async ({ page, context, peer, instanceName }) => {
  await installWorkerPort(context);
  await page.clock.install();
  const seen = requests(context);
  const setup = await peer.setup(); await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup); await openChannel(page, peer.communityName, peer.channelName);
  await expect.poll(() => connection(page)).toMatchObject({ status: 'online', wireVersion: 5 });
  await expect.poll(async () => (await connection(page))?.appBuild).toMatch(/^\/assets\/.+\.js$/);
  await page.waitForLoadState('networkidle');
  const current = (await connection(page))!.appBuild!;
  await context.route('**/app-version.json', route => route.fulfill({ json: { entry: current } }));
  const worker = await probe(page);
  await page.clock.pauseAt(new Date(Date.now() + 1000));
  const before = { ...seen };
  await advance(page, worker, 900_000);
  expect(seen).toEqual(before);
  expect((await ops(worker)).filter(op => op === 57 || op === 58)).toEqual([]);
  // Foreground reconciliation is allowed, but foreground is not fabricated input.
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect.poll(() => seen.version).toBe(before.version + 1);
  expect((await ops(worker)).filter(op => op === 57)).toHaveLength(0);
  for (let i = 0; i < 3; i++) {
    await advance(page, worker, 5000);
    await page.keyboard.press('ArrowRight'); await page.keyboard.press('ArrowRight');
    await expect.poll(async () => (await ops(worker)).filter(op => op === 57).length).toBe(i + 1);
  }
  expect(seen.activity).toBe(before.activity);
  expect(seen.stats).toBe(before.stats);
  // Three real socket reconnects each reconcile once; no historical input is emitted on ready.
  const afterInput = seen.version;
  for (let i = 0; i < 3; i++) {
    await worker.evaluate(() => { (globalThis as unknown as { __realtime: Probe }).__realtime.socket!.close(4000, 'reconnect probe'); });
    await expect.poll(() => connection(page)).toMatchObject({ status: 'offline' });
    await expect.poll(() => connection(page), { timeout: WAIT }).toMatchObject({ status: 'online', wireVersion: 5 });
    await expect.poll(() => seen.version).toBe(afterInput + i + 1);
  }
  expect((await ops(worker)).filter(op => op === 57)).toHaveLength(3);
  // The notice remains a verified hint; repeated/malformed hints do not reload an unsaved form.
  await addServerVia(page, 'create');
  const dialog = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await dialog.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill('unfinished form');
  await context.unroute('**/app-version.json');
  await context.route('**/app-version.json', route => route.fulfill({ json: { entry: '/assets/next-build.js' } }));
  const beforeHint = seen.version;
  await notice(worker, '/assets/../bad.js');
  await notice(worker, '/assets/next-build.js'); await notice(worker, '/assets/next-build.js');
  await expect.poll(() => seen.version).toBe(beforeHint + 1);
  await expect(page.getByText(copy('update.finishDialog'), { exact: true })).toBeVisible();
  await expect(dialog.getByRole('textbox', { name: copy('create.server.name'), exact: true })).toHaveValue('unfinished form');
  await page.clock.runFor(900_000);
  expect(seen.version).toBe(beforeHint + 1);
});

test.describe('no HTTP transport fallback', () => {
  const port = 8921;
  let host: StartedHost;
  let instanceName: string;
  test.use({ baseURL: `http://127.0.0.1:${String(port)}` });
  test.beforeAll(async () => {
    removeHostData(port); host = await startTestHost({ port, control: port + 1, webRoot: 'packages/web/dist', wire: 4 });
    const response = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
    const value = arr(decode(new Uint8Array(await response.arrayBuffer())))[5];
    if (typeof value !== 'string') throw new Error('the legacy host has no name');
    instanceName = value;
  });
  test.afterAll(async () => { await host?.stop(); removeHostData(port); });
  test('an older connection never starts version polling or HTTP activity fallback', async ({ page, context }) => {
    await installWorkerPort(context); await page.clock.install(); const seen = requests(context);
    await signUp(page, host.invite.split(',')[0], instanceName, 'Ada', host.control);
    await expect.poll(() => connection(page)).toMatchObject({ status: 'online', wireVersion: 4 });
    await page.waitForLoadState('networkidle');
    const worker = await probe(page);
    await page.clock.pauseAt(new Date(Date.now() + 1000));
    const before = { ...seen };
    for (let i = 0; i < 3; i++) {
      await advance(page, worker, 300_000);
      expect(seen.version).toBe(before.version);
    }
    await page.keyboard.press('ArrowRight');
    await advance(page, worker, 5000);
    expect(await portCall(page, { m: 'presenceActivity' })).toMatchObject({ ok: true });
    expect(seen.activity).toBe(before.activity);
    expect((await ops(worker)).filter(op => op === 57 || op === 58)).toEqual([]);
  });
});
