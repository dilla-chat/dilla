// Bot v2 task 6: once the gateway socket is up, no authenticated app request goes over HTTP. The REST form of every
// moved route is deleted, so a request that still went over HTTP would be a 404; this spec watches the whole session so a
// call site the route tests missed (a worker path, a rarely-taken branch) fails here, not in the field.
//
// It records every HTTP request of the context (the renderer's and the core worker's, which Playwright reports at the
// context) over one session: sign up, join a server, open a channel, send, react, open settings, make a role, make an
// invite, upload an image, open a DM, join and leave voice (the voice leg runs on a host of its own, with the in-process
// SFU). Each /v1 request must be a pre-gateway route (any time:
// session establishment can repeat), a blob, GIF media or backup transfer (binary bodies above a frame), or an HTTP-only
// route made before the socket was first opened (sign-up and sign-in).
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { expect, type BrowserContext, type Page } from '@playwright/test';
import {
  WAIT, addServerVia, attachInput, composerBox, copy, dmsNav, drawImage, homeTile, instanceInvite, joinCommunity, makeInviteLink,
  makeRole, openChannel, closeSettings, railItem, rowAction, rowById, reactionChip, selectRole, sendText, settingsGear,
  messageRow, signUp, tag, test, expectComposerReady, trayEntries, TEST_TIMEOUT,
} from './support/app';
import { HTTP_ONLY_ROUTES, PRE_GATEWAY_ROUTES, socketRouteFor } from '../../packages/client-core/src/http/socket-routes';
import { joinVoice } from '../voice/support/voice';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { chromiumMediaArgs, writeFixtures } from '../media/support/fixtures';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { e2ePort } from './support/ports';

test.describe.configure({ timeout: TEST_TIMEOUT });
// The fake microphone for the voice leg (a launch option, so it applies to the whole file).
test.use({
  permissions: ['microphone'],
  launchOptions: { args: chromiumMediaArgs(writeFixtures(join(tmpdir(), 'dilla-media-fixtures'))) },
});

/** Binary transfers that stay on HTTP: a blob (above a frame), a GIF's media bytes and a backup chunk. */
const TRANSFERS = [
  { method: 'GET', pattern: '/v1/channels/{id}/blobs/{blob_id}' },
  { method: 'PUT', pattern: '/v1/channels/{id}/blobs/{blob_id}' },
  { method: 'DELETE', pattern: '/v1/channels/{id}/blobs/{blob_id}' },
  { method: 'POST', pattern: '/v1/channels/{id}/blobs/{blob_id}/confirm' },
  { method: 'GET', pattern: '/v1/communities/{id}/gifs/media/{gif_id}' },
  { method: 'GET', pattern: '/v1/backups' },
  { method: 'GET', pattern: '/v1/backups/{kind}/{chunk_seq}' },
  { method: 'PUT', pattern: '/v1/backups/{kind}/{chunk_seq}' },
  { method: 'DELETE', pattern: '/v1/backups/{kind}/{chunk_seq}' },
] as const;

interface Seen { method: string; path: string; at: number }

const button = (page: Page, key: string, vars?: Record<string, string>) => page.getByRole('button', { name: copy(key, vars), exact: true });

/** Records every /v1 HTTP request of the context (the renderer's and the worker's) and when the gateway socket was first opened. */
function record(context: BrowserContext, page: Page) {
  const seen: Seen[] = [];
  let socketOpenedAt = Infinity;
  context.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname.startsWith('/v1/')) seen.push({ method: request.method(), path: url.pathname, at: Date.now() });
  });
  page.on('websocket', (ws) => { if (ws.url().endsWith('/gateway')) socketOpenedAt = Math.min(socketOpenedAt, Date.now()); });
  return {
    /** Fails on any /v1 HTTP request that is not allowed while the socket is up. */
    check(): void {
      // The recorder saw the worker: its gateway ticket mint is an HTTP request.
      expect(seen.some((r) => r.method === 'POST' && r.path === '/v1/gateway/ticket'), 'the worker\'s ticket request was recorded').toBe(true);
      expect(Number.isFinite(socketOpenedAt), 'the gateway socket was opened').toBe(true);
      const offenders = seen.filter((r) => {
        if (socketRouteFor(r.method, r.path, HTTP_ONLY_ROUTES) === null) return true; // not an HTTP route at all
        if (socketRouteFor(r.method, r.path, PRE_GATEWAY_ROUTES) !== null) return false; // session establishment repeats
        if (socketRouteFor(r.method, r.path, TRANSFERS) !== null) return false; // blob, GIF media and backup transfers
        return r.at >= socketOpenedAt; // anything else HTTP-only (sign-up, sign-in) must precede the socket
      });
      expect(offenders.map((r) => `${r.method} ${r.path}`), 'HTTP requests made while the socket was up').toEqual([]);
    },
  };
}

test('no app request uses HTTP while the socket is up', async ({ page, context, peer, instanceName }) => {
  const recorder = record(context, page);

  // Sign up, and join the peer's server (the peer is its owner and the page's one contact).
  const setup = await peer.setup();
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_ids[0]);

  // Send, and react to the peer's message.
  const text = `socket only ${tag()}`;
  await sendText(page, peer.channelName, text);
  await peer.waitFor(text);
  const theirs = `from the peer ${tag()}`;
  const sent = await peer.send(theirs);
  const row = messageRow(page, peer.channelName, theirs);
  await expect(row).toBeVisible({ timeout: WAIT });
  const target = rowById(page, peer.channelName, sent.msg_id);
  await rowAction(target, 'react');
  const grid = page.getByRole('dialog', { name: copy('shell.emoji.label'), exact: true });
  await grid.getByRole('button', { name: copy('shell.emoji.01'), exact: true }).click();
  await expect(reactionChip(target, copy('shell.emoji.01'), 1)).toBeVisible({ timeout: WAIT });

  // Settings.
  await settingsGear(page).click();
  await closeSettings(page);

  // A server of the page's own: a role change, an invite, an image and the voice channel.
  const own = `Own ${tag()}`;
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(own);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(railItem(page, own)).toHaveAttribute('aria-current', 'page', { timeout: WAIT });
  await expectComposerReady(page, 'general');

  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click();
  await page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click();
  const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy('roles.tab'), exact: true }).click();
  const role = `Crew ${tag()}`;
  await makeRole(page, settings, role, { perms: ['pin'] });
  await selectRole(settings, role);
  // The Bot admin page reads the installation, its command status and its settings.
  const botTab = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy('admin.nav.bot'), exact: true });
  await botTab.click();
  await expect(botTab).toHaveAttribute('aria-current', 'page', { timeout: WAIT });
  await page.waitForLoadState('networkidle');
  // The GIF settings: read the status, then set a key so the composer offers the picker.
  await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy('workspace.gifs'), exact: true }).click();
  const gifKey = settings.getByRole('textbox', { name: copy('gif.settings.key'), exact: true });
  await expect(gifKey).toBeVisible({ timeout: WAIT });
  await gifKey.fill(`key${tag()}abcd`);
  await settings.getByRole('button', { name: copy('gif.settings.save'), exact: true }).click();
  await expect(settings.getByText(copy('gif.settings.saved'), { exact: true })).toBeVisible({ timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(settings).toBeHidden({ timeout: WAIT });
  await makeInviteLink(page, own);

  const drawn = await drawImage(page, { width: 160, height: 120, type: 'image/png', seed: 6 });
  await attachInput(page).setInputFiles([{ name: 'drawn.png', mimeType: 'image/png', buffer: drawn.bytes }]);
  await expect(trayEntries(page).first()).toHaveAttribute('data-phase', 'ready', { timeout: WAIT });
  const caption = `an image ${tag()}`;
  await composerBox(page, 'general').fill(caption);
  await composerBox(page, 'general').press('Enter');
  await expect(messageRow(page, 'general', caption)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

  // The GIF picker: open it and search once (an instance with no Giphy key answers a refusal, which is fine).
  await page.getByRole('button', { name: copy('gif.button'), exact: true }).click();
  const gifSearch = page.getByRole('searchbox', { name: copy('gif.search'), exact: true }).or(page.getByRole('textbox', { name: copy('gif.search'), exact: true }));
  await expect(gifSearch.first()).toBeVisible({ timeout: WAIT });
  await gifSearch.first().fill('cat');
  await page.waitForLoadState('networkidle');
  await page.keyboard.press('Escape');

  // A DM with the peer (a co-member of the peer's server).
  await railItem(page, peer.communityName).click();
  await homeTile(page).click();
  await button(page, 'shell.dms.new').click();
  const picker = page.getByRole('dialog', { name: copy('shell.dms.newTitle'), exact: true });
  await picker.getByRole('button', { name: copy('shell.dms.open'), exact: true }).first().click();
  await expect(page).toHaveURL(/\/dm\/[0-9a-f]{32}$/, { timeout: WAIT });
  await expect(dmsNav(page)).toBeVisible();

  recorder.check();
});

// Voice runs on a host of its own: the shared host has no in-process SFU, so a join there fails before it connects.
test.describe('voice', () => {
  const EMPTY: HostConfig = { port: e2ePort(8531), control: e2ePort(8532), webRoot: 'packages/web/dist', sfu: { port: e2ePort(7897), udpPort: e2ePort(7899) } };
  let host: StartedHost | null = null;
  let instance = '';
  test.use({ baseURL: `http://127.0.0.1:${String(EMPTY.port)}` });
  test.beforeAll(async () => {
    host = await startTestHost(EMPTY);
    const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
    expect(res.status, 'GET /v1/instance').toBe(200);
    const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
    if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
    instance = name;
  });
  test.afterAll(async () => { await host?.stop(); });

  test('joining and leaving voice goes over the socket', async ({ page, context, browserName }) => {
    test.skip(browserName !== 'chromium', 'the fake microphone is a Chromium switch');
    const h = host;
    if (h === null) throw new Error('the empty test host did not start');
    const recorder = record(context, page);
    await signUp(page, h.invite.split(',')[0], instance, 'Ada', h.control);
    const own = `Voice ${tag()}`;
    await addServerVia(page, 'create');
    const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(own);
    await page.keyboard.press('Enter');
    await expect(start).toBeHidden({ timeout: WAIT });
    await expectComposerReady(page, 'general');
    await joinVoice(page, own, 'general');
    await page.locator('.dw-voice-stage [data-control="leave"]').click();
    await expect(page.locator('[data-call-phase]').first()).not.toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
    recorder.check();
  });
});
