// web3.8, E2 / Bot v2 Task 1: a client older than the host is told so, without a reconnect loop, and keeps its work.
// The current client speaks wire 6 alone. A host started with `-wire 7` offers only a newer wire, so the client cannot identify:
// its hello has nothing in common and every wire it offers is newer (the instance's own "older than all" rule, read from the
// hello). The desktop app shows `update.required` with its update button (`update.requiredAction`) and does not reload itself;
// a browser tab reloads once to the host's current client (checkpointing the draft first, WS3) and, still refused, shows
// `update.required` with `reload.action`. A plain version refusal (the server's close 4006 naming E_VERSION) keeps the older
// state: `shell.banner.clientTooOld` with `reload.action`, whose press checkpoints the draft before the page reloads. The
// community list is a socket read (Bot v2), so a tab refused from its start shows the banner over an empty shell; the draft
// is checked on a tab refused while open (its host restarts and the next socket is refused).
// Ada's account lives in /tmp/dw/host-8515, which the three hosts of this suite share (`keepData`).
import { expect, type Page, type TestInfo } from '@playwright/test';
import { addServerVia, TEST_TIMEOUT, WAIT, copy, composerBox, expectAccessible, expectComposerReady, railNav, signUp, test } from './support/app';
import { hostArgs, removeHostData, startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { e2ePort } from './support/ports';
import { desktopE2E } from './support/electron';

const WIRE6: HostConfig = { port: e2ePort(8515), control: e2ePort(8516), webRoot: 'packages/web/dist', keepData: true, wire: 6 };
const WIRE7: HostConfig = { ...WIRE6, wire: 7 };
const SERVER = 'Midgard Crew';
const DRAFT = 'a half-written thought that must outlive the reload';
const QUIET_MS = 4_000;
const BACKOFF_WAIT = WAIT + 60_000;

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({ baseURL: `http://127.0.0.1:${String(WIRE6.port)}` });

test.beforeAll(async () => {
  removeHostData(WIRE6.port);
  host = await startTestHost(WIRE6);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
});
test.afterAll(async () => { await host?.stop(); removeHostData(WIRE6.port); });

/** Stops the running host and starts `next` on the same ports and the same data. */
async function swapHost(next: HostConfig): Promise<void> {
  await host?.stop();
  host = null;
  host = await startTestHost(next);
}

const banner = (page: Page) => page.getByText(copy('shell.banner.clientTooOld'), { exact: true });
const reloadButton = (page: Page) => page.getByRole('button', { name: copy('reload.action'), exact: true });
const updateBanner = (page: Page) => page.getByText(copy('update.required'), { exact: true });
const updateButton = (page: Page) => page.getByRole('button', { name: copy('update.requiredAction'), exact: true });

const isChromium = (info: TestInfo) => desktopE2E() || info.project.name.startsWith('chromium');

/** The gateway ticket the core worker fetches before it opens each socket (POST /v1/gateway/ticket). */
const TICKET = /\/v1\/gateway\/ticket$/;

/** Runs in each of the page's workers while refusing (chromium only): every WebSocket that gets its first listener from then on
 *  also closes itself 4006 naming E_VERSION once it is open, which the gateway reads as the server's own plain version refusal.
 *  Playwright's WebSocket routes do not reach a worker's sockets. The patch is on the prototype, so it also covers a WebSocket
 *  class the worker took before it. */
function refuseSockets(): void {
  const proto = WebSocket.prototype as WebSocket & { dillaRefusing?: true };
  if (proto.dillaRefusing === true) return;
  proto.dillaRefusing = true;
  const add = proto.addEventListener;
  const armed = new WeakSet<WebSocket>();
  proto.addEventListener = function (this: WebSocket, ...args: Parameters<WebSocket['addEventListener']>): void {
    if (!armed.has(this)) {
      armed.add(this);
      add.call(this, 'open', () => { this.close(4006, 'E_VERSION'); });
    }
    add.apply(this, args);
  } as WebSocket['addEventListener'];
}

/** Counts the gateway sockets the page opens (workers' included: the page event sees them), its `load` events, and the gateway
 *  tickets let through only after the refusal patch was in every worker. */
async function watch(page: Page): Promise<{ sockets: () => number; loads: () => number; patched: () => number; refuse: (on: boolean) => Promise<void> }> {
  let sockets = 0;
  let loads = 0;
  let patched = 0;
  page.on('load', () => { loads += 1; });
  page.on('websocket', (ws) => { if (ws.url().endsWith('/gateway')) sockets += 1; });
  // The worker opens its gateway socket only with a ticket, so the ticket is held until the patch is in: the patch can never land
  // after the socket exists. (The patch used to be run over the DevTools protocol while the new worker waited for its debugger,
  // but Playwright resumes the worker too, so on a slow runner the worker could take the unpatched WebSocket first.)
  const refuse = async (on: boolean): Promise<void> => {
    const transport = desktopE2E() ? page.context() : page;
    if (!on) { await transport.unroute(TICKET); return; }
    await transport.route(TICKET, async (route) => {
      for (const worker of page.workers()) await worker.evaluate(refuseSockets);
      patched += 1;
      await route.continue();
    });
  };
  return { sockets: () => sockets, loads: () => loads, patched: () => patched, refuse };
}

async function expectNoReconnect(page: Page, sockets: () => number): Promise<void> {
  const opened = sockets();
  await page.waitForTimeout(QUIET_MS);
  expect(sockets(), `gateway sockets opened in ${String(QUIET_MS)} ms after the version refusal`).toBe(opened);
}

test('a tab older than the host is refused two ways: banner, no reconnect, and Reload keeps the draft', async ({ page }) => {
  expect(hostArgs(WIRE6, '/tmp/dilla-version-reload-wire6'), 'the baseline host offers only wire 6').toContain('-wire=6');
  expect(hostArgs(WIRE7, '/tmp/dilla-version-reload-wire7'), 'the replacement host offers only wire 7').toContain('-wire=7');
  const h = host;
  if (h === null) throw new Error('the test host did not start');
  await page.setViewportSize({ width: 1280, height: 800 });
  const seen = await watch(page);
  await signUp(page, h.invite.split(',')[0], instance, 'Ada', h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  await expect(banner(page)).toHaveCount(0);
  await composerBox(page, 'general').fill(DRAFT);

  // The wire-6 host goes, a wire-7 host takes its ports. Assert the replacement host advertises the newer wire before
  // waiting for the client's own refusal from the hello.
  const loadsBeforeSwap = seen.loads();
  await swapHost(WIRE7);
  const replacement = await fetch(`${h.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(replacement.status, 'replacement GET /v1/instance').toBe(200);
  const replacementDocument = arr(decode(new Uint8Array(await replacement.arrayBuffer())));
  expect(replacementDocument[0], 'replacement host advertises only the newer wire 7').toEqual([7n]);
  // The tab was backing off (up to 30 s) while no host answered, so the next attempt can be that far away.
  if (desktopE2E()) {
    // The desktop app: "Update dilla to continue" with the update button, and no reload of its own.
    await expect(updateBanner(page)).toBeVisible({ timeout: BACKOFF_WAIT });
    await expect(updateButton(page)).toBeVisible();
    await expect(banner(page)).toHaveCount(0);
    await expectNoReconnect(page, seen.sockets);
    expect(seen.loads(), 'the desktop app does not reload itself').toBe(loadsBeforeSwap);
    await expect(composerBox(page, 'general')).toHaveValue(DRAFT);
    await expectAccessible(page, 'newer host: desktop update banner');
  } else {
    // A browser tab reloads once by itself, keeping the draft; refused again, it stops and says why.
    await expect.poll(seen.loads, { timeout: BACKOFF_WAIT, message: 'the tab reloaded once to the newer client' }).toBe(loadsBeforeSwap + 1);
    await expect(updateBanner(page)).toBeVisible({ timeout: WAIT });
    await expect(reloadButton(page)).toBeVisible();
    await expect(banner(page)).toHaveCount(0);
    await expect(composerBox(page, 'general')).toHaveValue(DRAFT, { timeout: WAIT });
    await expectNoReconnect(page, seen.sockets);
    expect(seen.loads(), 'no second automatic reload').toBe(loadsBeforeSwap + 1);
    await expectAccessible(page, 'newer host: browser update banner after one reload');
  }

  // A host that offers wire 6 again loads the app normally: no banner.
  await swapHost(WIRE6);
  await page.reload();
  await expectComposerReady(page, 'general');
  await expect(banner(page)).toHaveCount(0);
  await expect(updateBanner(page)).toHaveCount(0);
  await expectAccessible(page, 'wire 6 again: no banner');

  // The plain version refusal: the server closes the socket 4006 naming E_VERSION (chromium only: the close is injected into
  // the page's workers before the gateway ticket is let through). The older banner, Reload keeps the draft, no reconnect loop.
  if (!isChromium(test.info())) return;
  await seen.refuse(true);
  const before = seen.sockets();
  await page.reload();
  await expect.poll(seen.patched, { timeout: WAIT, message: 'the reloaded page\'s ticket waited for the refusal patch' }).toBeGreaterThan(0);
  await expect(banner(page)).toBeVisible({ timeout: WAIT });
  await expect(reloadButton(page)).toBeVisible();
  await expect(updateBanner(page)).toHaveCount(0);
  await expectNoReconnect(page, seen.sockets);
  expect(seen.sockets() - before, 'one socket after the reload, refused once').toBe(1);
  await expectAccessible(page, 'version refused: banner shown');

  // A tab refused while it is open: the page loads normally, the host restarts (the open socket is lost) and the next
  // socket is refused. The banner comes over the channel, and its Reload checkpoints the draft first; with the refusal
  // lifted, the page loads once more with the draft in the composer and no banner.
  await seen.refuse(false);
  await page.reload();
  await expectComposerReady(page, 'general');
  await expect(banner(page)).toHaveCount(0);
  await composerBox(page, 'general').fill(DRAFT);
  await seen.refuse(true);
  const live = seen.sockets();
  await swapHost(WIRE6);
  await expect(banner(page)).toBeVisible({ timeout: BACKOFF_WAIT });
  await expect(updateBanner(page)).toHaveCount(0);
  await expectNoReconnect(page, seen.sockets);
  expect(seen.sockets() - live, 'one socket after the host came back, refused once').toBe(1);
  await seen.refuse(false);
  const loadsBefore = seen.loads();
  await reloadButton(page).click();
  await expect.poll(seen.loads, { timeout: WAIT }).toBe(loadsBefore + 1);
  await expect(composerBox(page, 'general')).toHaveValue(DRAFT, { timeout: WAIT });
  await expect(banner(page)).toHaveCount(0);
});
