// web3.8, WS4 (item 11): the sidebar's actions moved. From an empty host, Ada (the owner) starts "Midgard Crew" and makes a
// channel through the "+" in the channel list's header; Björn (a member with the default @everyone: create invite, no manage
// channels) has no "+", opens server settings, selects Invites, makes a link and copies it. At 390x844 the header has the search icon
// and the gear (no menu), a member reaches leave under settings, and the list header has the "+" for Ada only.
import { expect, type Page } from '@playwright/test';
import { addServerVia,
  TEST_TIMEOUT, WAIT, completeSignUp, composerBox, copy, expectAccessible, expectComposerReady, makeInviteLink, newAccount, newChannelButton,
  railItem, railNav, signUp, test,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const EMPTY: HostConfig = { port: 8523, control: 8524, webRoot: 'packages/web/dist' };
const SERVER = 'Midgard Crew';
const OWNER = 'Ada';
const FRIEND = 'Björn';

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
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

const button = (page: Page, key: string) => page.getByRole('button', { name: copy(key), exact: true });
test('the owner adds a channel with the "+"; a member makes a link under server settings → Invites; a phone has a gear and a search icon, no menu', async ({
  page, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = host;
  if (h === null) throw new Error('the empty test host did not start');
  await page.setViewportSize({ width: 1280, height: 800 });

  // 1. Ada starts the server and adds a channel through the "+" in the list header. The server header holds no invite
  //    link and no "+ channel" any more.
  await signUp(page, h.invite.split(',')[0], instance, OWNER, h.control);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
  await expectComposerReady(page, 'general');
  await expect(button(page, 'invite.open')).toHaveCount(0);
  await expect(page.getByRole('group', { name: copy('create.actions'), exact: true }).getByRole('button', { name: copy('create.channel.open'), exact: true })).toHaveCount(0);
  const plus = newChannelButton(page);
  await expect(plus).toBeVisible({ timeout: WAIT });
  await plus.click();
  const made = page.getByRole('dialog', { name: copy('create.channel.title', { server: SERVER }), exact: true });
  await made.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill('plans');
  await page.keyboard.press('Enter');
  await expect(made).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'plans');
  await expect(composerBox(page, 'plans')).toBeVisible();

  // 2. Björn joins through a link Ada makes under server settings → Invites (the helper is that path).
  const link = await makeInviteLink(page, SERVER);
  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, secondDir);
  try {
    const page2 = second.pages()[0] ?? (await second.newPage());
    await page2.setViewportSize({ width: 1280, height: 800 });
    await page2.goto(new URL(link).pathname);
    await page2.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
    await completeSignUp(page2, newAccount(FRIEND, h.control), instance);
    await expect(railItem(page2, SERVER)).toBeVisible({ timeout: WAIT });
    await railItem(page2, SERVER).click();
    await expect(page2).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });

    // Björn has no "+", and the sidebar has no invite link. Settings opens the first permitted section;
    // the grouped navigation keeps Invites available to a member with create-invite permission.
    await expect(button(page2, 'admin.open')).toBeVisible({ timeout: WAIT });
    await expect(newChannelButton(page2)).toHaveCount(0);
    await expect(button(page2, 'invite.open')).toHaveCount(0);
    await expect(button(page2, 'admin.leave.open')).toHaveCount(0);
    await button(page2, 'admin.open').click().then(() => page2.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const settings = page2.getByRole('dialog', { name: copy('admin.title'), exact: true });
    const nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await expect(nav).toBeVisible({ timeout: WAIT });
    await expect(nav.getByRole('button', { name: copy('workspace.members'), exact: true })).toHaveAttribute('aria-current', 'page');
    await nav.getByRole('button', { name: copy('admin.nav.invites'), exact: true }).click();
    await expect(nav.getByRole('button', { name: copy('admin.nav.invites'), exact: true })).toHaveAttribute('aria-current', 'page');
    await expect(page2).toHaveURL(/\/settings\/invites$/, { timeout: WAIT });
    await expectAccessible(page2, 'server settings, Invites, for a member');

    // He makes a link and copies it; in Chromium the clipboard is read back and compared to the URL the dialog shows.
    if (browserName === 'chromium') await second.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: h.url });
    // Signal task 10c: the link shows once in the Invites card with Copy; a member without Manage server is offered
    // no "never" and no "∞".
    await expect(settings.getByRole('radio', { name: copy('serverSettings.invites.ttl.never'), exact: true })).toHaveCount(0);
    await expect(settings.getByRole('radio', { name: copy('serverSettings.invites.uses.unlimited'), exact: true })).toHaveCount(0);
    await settings.getByRole('button', { name: copy('serverSettings.invites.make'), exact: true }).click();
    const field = settings.getByRole('textbox', { name: copy('serverSettings.invites.link'), exact: true });
    await expect(field).toBeVisible({ timeout: WAIT });
    await expect.poll(() => field.inputValue(), { timeout: WAIT }).toMatch(/^https?:\/\/[^/\s]+\/i\/[^/\s]+$/);
    const shown = await field.inputValue();
    await settings.getByRole('button', { name: copy('serverSettings.invites.copy'), exact: true }).click();
    if (browserName === 'chromium') {
      await expect(field).toBeHidden({ timeout: WAIT });
      expect(await page2.evaluate(() => navigator.clipboard.readText()), 'the clipboard holds the shown URL').toBe(shown);
    }
    await page2.keyboard.press('Escape');
    await expect(settings).toBeHidden();

    // 3. At 390x844 (Ruling AB): the head has the ⌄ (server settings) alone, no search, no menu; the list header has the "+" for Ada only.
    await page.setViewportSize({ width: 390, height: 844 });
    await page2.setViewportSize({ width: 390, height: 844 });
    for (const p of [page, page2]) {
      const backButton = button(p, 'nav.back');
      await expect(backButton).toBeVisible({ timeout: WAIT });
      await backButton.click();
      await expect(button(p, 'admin.open')).toBeVisible({ timeout: WAIT });
      await expect(p.locator('.d-channel-list__head').getByRole('button', { name: copy('search.open'), exact: true })).toHaveCount(0);
      await expect(button(p, 'nav.server.menu')).toHaveCount(0);
      await expect(button(p, 'admin.leave.open')).toHaveCount(0);
    }
    // A member reaches leave through the gear: server settings → leave server.
    await button(page2, 'admin.open').click().then(() => page2.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const phoneSettings = page2.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await expect(phoneSettings.getByRole('button', { name: copy('admin.nav.leave'), exact: true })).toBeVisible({ timeout: WAIT });
    await page2.keyboard.press('Escape');
    await expect(phoneSettings).toBeHidden();
    await expect(button(page2, 'admin.open')).toBeFocused();
    await expect(newChannelButton(page)).toBeVisible();
    await expect(newChannelButton(page2)).toHaveCount(0);

    // 4. Accessible: the owner's phone list with its "+".
    await expectAccessible(page, 'the phone servers list with the new channel "+"');
  } finally {
    await second.close();
  }
});
