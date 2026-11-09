// Bot v2 Task 22: Bot channels from the admin UI. A text channel is marked in its settings; the Bot joins it (the instance
// proposes the Bot into its group and the marking admin's device commits the Add) and posts there: a new member's welcome,
// and a webhook's posts with their "via webhook" label. A webhook into a channel that is no longer a Bot channel is 409.
// Linux-only (playwright.desktop.config.ts): it needs the native Bot runtime.
import { openInviteInClient } from './support/invite';
import { expect, type Locator, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { addServerVia, completeSignUp, copy, makeInviteLink, messageRow, newAccount, openChannel, signUp, test } from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { e2ePort } from './support/ports';
import { settingsEditing } from '../../packages/web/src/strings/settingsEditing';

const PORT = Number(process.env.DILLA_E2E_BOT_CHANNELS_PORT ?? e2ePort(8493));
const HOST: HostConfig = { port: PORT, control: PORT + 1, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true };
const SERVER = 'Bot Channels';

test.use({ baseURL: `http://127.0.0.1:${PORT}` });

const navItem = (settings: Locator, key: string) =>
  settings.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy(key), exact: true });

async function openServerSettings(page: Page): Promise<Locator> {
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click()
    .then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  return page.getByRole('dialog', { name: copy('admin.title'), exact: true });
}

/** With DILLA_SHOTS set (admin.spec's convention), the page at 1240×820, at the 900×600 minimum and in the 390-wide phone shell. */
async function shoot(page: Page, name: string): Promise<void> {
  const dir = process.env.DILLA_SHOTS;
  if (dir === undefined || dir === '') return;
  const original = page.viewportSize();
  for (const [width, height] of [[1240, 820], [900, 600], [390, 844]] as const) {
    await page.setViewportSize({ width, height });
    await page.evaluate(() => new Promise<void>(resolve => { requestAnimationFrame(() => { requestAnimationFrame(() => { resolve(); }); }); }));
    await page.screenshot({ path: `${dir}/${name}-${String(width)}x${String(height)}.png` });
  }
  if (original !== null) await page.setViewportSize(original);
}

/** Creates the server and enables its Bot with `capability` switched on, then waits for the installation to be ready. */
async function serverWithReadyBot(page: Page, host: StartedHost, instanceName: string, capability: string): Promise<void> {
  const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
  await addServerVia(page, 'create');
  const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  const settings = await openServerSettings(page);
  await navItem(settings, 'admin.nav.bot').click();
  await settings.getByRole('switch', { name: copy(capability), exact: true }).click();
  await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
  const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
  await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(owner.password);
  await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
  const credential = host.runtimeCredentialFile;
  if (credential === undefined) throw new Error('test host did not expose its runtime credential path');
  await expect.poll(async () => {
    const token = readFileSync(credential, 'utf8').trim();
    const response = await fetch(`${host.control}/debug/bot/installations`, { headers: { authorization: `Bearer ${token}`, accept: 'application/cbor' } });
    if (!response.ok) return `HTTP ${response.status}`;
    const rows = arr(decode(new Uint8Array(await response.arrayBuffer())));
    return rows.length === 1 ? arr(rows[0])[2] : `count ${rows.length}`;
  }, { timeout: 90_000, intervals: [1_000, 2_000] }).toBe(3n);
  await page.keyboard.press('Escape');
  await expect(settings).toBeHidden();
}

/** Marks or unmarks `channel` as a Bot channel through its settings dialog (Server settings → Channels → edit). */
async function setBotChannel(page: Page, channel: string, on: boolean): Promise<void> {
  const settings = await openServerSettings(page);
  await navItem(settings, 'admin.nav.channels').click();
  // The text channel's row (a voice channel may share the name).
  await settings.locator('li').filter({ hasText: `#${channel}` })
    .getByRole('button', { name: copy('admin.channels.editLabel', { name: channel }), exact: true }).click();
  const edit = page.getByRole('dialog', { name: copy('admin.channel.titleText', { name: channel }), exact: true });
  const toggle = edit.getByRole('switch', { name: copy('bot.channel.toggle'), exact: true });
  if ((await toggle.getAttribute('aria-checked')) !== String(on)) await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', String(on));
  if (on) await shoot(page, 'channel-settings-bot-toggle');
  await edit.getByRole('button', { name: copy('admin.cancel'), exact: true }).click();
  // The channel list carries the Bot badge once the mark is back.
  const row = settings.locator('li').filter({ hasText: `#${channel}` });
  if (on) await expect(row.getByText(copy('bot.channel.badge'), { exact: true })).toBeVisible({ timeout: 15_000 });
  else await expect(row.getByText(copy('bot.channel.badge'), { exact: true })).toHaveCount(0, { timeout: 15_000 });
  await page.keyboard.press('Escape');
  await expect(settings).toBeHidden();
}

test('mark a channel, enable welcome, a new member is welcomed there', async ({ page, instanceName, browser, guard }) => {
  let host: StartedHost | null = null;
  let second: import('@playwright/test').BrowserContext | null = null;
  try {
    host = await startTestHost(HOST);
    await serverWithReadyBot(page, host, instanceName, 'serverSettings.bot.cap.welcome');
    await setBotChannel(page, 'general', true);

    // The Bot page lists the Bot channel with its badge, and the health line.
    const settings = await openServerSettings(page);
    await navItem(settings, 'admin.nav.bot').click();
    await expect(settings.getByRole('heading', { name: copy('bot.channels.title'), exact: true })).toBeVisible();
    await expect(settings.getByRole('group', { name: /^#general/ })).toBeVisible();
    await expect(settings.getByText(copy('bot.health.ok'), { exact: true })).toBeVisible({ timeout: 30_000 });
    await settings.getByRole('heading', { name: copy('bot.channels.title'), exact: true }).scrollIntoViewIfNeeded();
    await shoot(page, 'bot-page-channels-health');

    // Welcome posts into the Bot channel the picker offers.
    await navItem(settings, 'workspace.welcome').click();
    await settings.getByRole('button', { name: copy('admin.bot.welcomeSettings.load'), exact: true }).click();
    const target = settings.getByRole('combobox', { name: copy('admin.bot.welcomeSettings.channel'), exact: true });
    await expect(target).toBeVisible({ timeout: 30_000 });
    await target.selectOption({ label: 'general' });
    await settings.getByRole('textbox', { name: copy('admin.bot.welcomeSettings.template'), exact: true }).fill('Welcome {member} to {server}!');
    // The settings frame's own Save (settings/edit-state.tsx, settingsEditing.save) holds the page's form.
    await page.getByRole('button', { name: settingsEditing.save, exact: true }).first().click();
    await expect(settings.getByText(copy('admin.bot.welcomeSettings.saved'), { exact: true }).first()).toBeVisible({ timeout: 30_000 });
    await page.keyboard.press('Escape');
    await expect(settings).toBeHidden();

    const invite = await makeInviteLink(page, SERVER);
    second = await browser.newContext({ baseURL: host.url });
    await guard.attach(second);
    const bea = await second.newPage();
    await openInviteInClient(bea, invite);
    await completeSignUp(bea, newAccount('Bea', host.control), instanceName);
    await openChannel(page, SERVER, 'general');
    const welcome = messageRow(page, 'general', `Welcome Bea to ${SERVER}!`);
    await expect(welcome).toBeVisible({ timeout: 60_000 });
    await expect(welcome.getByText('bot', { exact: true })).toBeVisible();
    await shoot(page, 'channel-bot-badge-welcome');
  } finally {
    await second?.close();
    await host?.stop();
  }
});

test('a webhook POST appears in the channel', async ({ page, instanceName, request }) => {
  let host: StartedHost | null = null;
  try {
    host = await startTestHost(HOST);
    await serverWithReadyBot(page, host, instanceName, 'serverSettings.bot.cap.welcome');
    await setBotChannel(page, 'general', true);

    const settings = await openServerSettings(page);
    await navItem(settings, 'admin.nav.bot').click();
    await settings.getByRole('button', { name: copy('bot.webhooks.create'), exact: true }).click();
    const dialog = page.getByRole('dialog', { name: copy('bot.webhooks.newTitle'), exact: true });
    await dialog.getByRole('textbox', { name: copy('bot.webhooks.name'), exact: true }).fill('CI');
    await dialog.getByRole('button', { name: copy('bot.webhooks.create'), exact: true }).click();
    const shown = page.getByRole('dialog', { name: copy('bot.webhooks.addressTitle'), exact: true });
    await expect(shown.getByText(copy('bot.webhooks.urlOnce'), { exact: true })).toBeVisible();
    const address = await shown.getByRole('textbox', { name: 'CI', exact: true }).inputValue();
    await shoot(page, 'webhook-url-once');
    await shown.getByRole('button', { name: copy('bot.webhooks.done'), exact: true }).click();
    await expect(shown).toBeHidden();
    // The address is gone once its dialog closed.
    await expect(page.locator(`input[value="${address}"]`)).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(settings).toBeHidden();

    // The instance answers the public route on its own listener; the address names its domain.
    const path = new URL(address).pathname;
    await openChannel(page, SERVER, 'general');
    // The Bot joins the channel's group just after the mark (the marking device commits its Add); until then the instance
    // answers 503 with Retry-After, which a sender retries.
    await expect.poll(async () => (await request.post(`${host!.url}${path}`, { data: { text: 'Build 812 passed' } })).status(),
      { timeout: 60_000, intervals: [2_000] }).toBe(204);
    const row = messageRow(page, 'general', 'Build 812 passed');
    await expect(row).toBeVisible({ timeout: 5_000 });
    await expect(row).toContainText(copy('message.viaWebhook', { name: 'CI' }));
    await expect(row).not.toContainText('[dilla-webhook:');
    await shoot(page, 'webhook-post-label');

    // A webhook into a channel that is not a Bot channel any more is refused.
    await setBotChannel(page, 'general', false);
    const refused = await request.post(`${host.url}${path}`, { data: { text: 'after the unmark' } });
    expect(refused.status()).toBe(409);
  } finally {
    await host?.stop();
  }
});
