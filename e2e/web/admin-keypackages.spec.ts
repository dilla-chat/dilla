import { expect, type Locator, type Page } from '@playwright/test';
import { addServerVia, chooseBotCapabilities, composerBox, copy, enableBotButton, openChannel, signIn, signUp } from './support/app';
import { prepareBotRuntimeBinaries, startTestHost, type HostConfig, type StartedHost } from './support/host';
import { test } from './support/second';
import { e2ePort } from './support/ports';

// An instance administrator's devices must be able to publish KeyPackages, or the Bot can never add them to its
// interaction groups and their pickers lack every Bot command (v0.1.0-bot1 shipped that bug; CI had no admin account).
// adminInvite makes the first account an instance admin through the signup path, as `dillad init`'s invite does.
const PORT = Number(process.env.DILLA_E2E_ADMIN_KP_PORT ?? e2ePort(8583));
const HOST: HostConfig = { port: PORT, control: PORT + 1, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true, adminInvite: true };
const COMMUNITY = 'Admin KeyPackages';
const GENERAL = 'general';
const BOT_WAIT = 90_000;

test.use({ baseURL: `http://127.0.0.1:${String(PORT)}` });
test.use({ actionTimeout: BOT_WAIT });
test.describe.configure({ timeout: 360_000 });

test.beforeAll(async () => {
  test.setTimeout(360_000);
  await prepareBotRuntimeBinaries();
});

/** The Bot page's Enable, as slash-commands.spec does it, up to the Bot's own "ready" status. */
async function enableBot(page: Page, password: string): Promise<void> {
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  const nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
  await expect(nav).toBeVisible({ timeout: BOT_WAIT });
  await nav.getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
  await chooseBotCapabilities(settings, ['polls', 'reminders']);
  await enableBotButton(settings).click();
  const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
  await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(password);
  await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
  await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') }))
    .toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });
  await settings.getByRole('button', { name: copy('settings.close'), exact: true }).click();
  await expect(settings).toBeHidden();
}

/** Types "/" in the composer and narrows the picker to `command`; the option only exists once the Bot has registered it. */
async function pickerOption(page: Page, command: string): Promise<Locator> {
  await composerBox(page, GENERAL).fill('/');
  const region = page.getByRole('region', { name: 'Command composer', exact: true });
  await region.getByRole('textbox', { name: 'Command', exact: true }).fill(`/${command}`);
  return region.getByRole('option', { name: new RegExp(`^/${command},`) });
}

test('an instance admin enrols a second device by recovery key and finds the Bot commands there', async ({ page, second, instanceName }) => {
  let host: StartedHost | null = null;
  try {
    host = await startTestHost(HOST);
    const admin = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const dialog = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await dialog.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(COMMUNITY);
    await page.keyboard.press('Enter');
    await openChannel(page, COMMUNITY, GENERAL);
    await enableBot(page, admin.password);
    await expect(await pickerOption(page, 'remind')).toBeVisible({ timeout: BOT_WAIT });

    // The second device joins with the password and the recovery key: it publishes its own KeyPackages, the Bot adds it,
    // and the commands the Bot registered reach its picker.
    const b = (await second.open()).page;
    await signIn(b, admin, instanceName);
    await openChannel(b, COMMUNITY, GENERAL);
    await expect(await pickerOption(b, 'remind')).toBeVisible({ timeout: BOT_WAIT });
  } finally {
    // Cancel the app's background sync before its host disappears.
    await page.goto('about:blank').catch(() => undefined);
    await host?.stop();
  }
});
