import { expect } from '@playwright/test';
import { addServerVia, completeSignUp, copy, makeInviteLink, newAccount, openChannel, railItem, signUp, test } from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';

const HOST: HostConfig = {
  port: 8495, control: 8496, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true,
};
const SERVER = 'Moderation Evidence';
const OWNER = 'Ada';
const TARGET = 'Bea';
const REASON = 'manual warning evidence from Chromium';

test.use({ baseURL: `http://127.0.0.1:${String(HOST.port)}` });

test('a warning made in the web client can be retrieved from Bot settings', async ({ page, browser, guard, instanceName }) => {
  let host: StartedHost | null = null;
  let targetContext: import('@playwright/test').BrowserContext | null = null;
  try {
    host = await startTestHost(HOST);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, OWNER, host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/);
    await openChannel(page, SERVER, 'general');

    const invite = await makeInviteLink(page, SERVER);
    targetContext = await browser.newContext({ baseURL: host.url });
    await guard.attach(targetContext);
    const targetPage = await targetContext.newPage();
    await targetPage.goto(new URL(invite).pathname);
    await targetPage.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
    await completeSignUp(targetPage, newAccount(TARGET, host.control), instanceName);
    await expect(railItem(targetPage, SERVER)).toBeVisible();
    await openChannel(targetPage, SERVER, 'general');

    // Enable the bot through the same server settings flow used by a moderator.
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    let settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    const nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await nav.getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.warnings'), exact: true }).click();
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(owner.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();

    // The open Bot panel polls the native runtime until the installation is ready.
    await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') })).toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });
    await settings.getByRole('button', { name: copy('settings.close'), exact: true }).click();
    await expect(settings).toBeHidden();

    // The owner warns Bea through her row's person menu (a right-click; Signal task 9); the reason is the evidence we retrieve below.
    const members = page.getByRole('complementary', { name: copy('members.region', { server: SERVER }), exact: true });
    const targetRow = members.getByRole('listitem').filter({ hasText: TARGET });
    await expect(targetRow).toBeVisible();
    await targetRow.locator('.d-member-list__open').click({ button: 'right' });
    await page.getByRole('menu', { name: copy('members.menu.label', { name: TARGET }), exact: true })
      .getByRole('menuitem', { name: copy('mod.warn.menu'), exact: true }).click();
    const warning = page.getByRole('dialog', { name: copy('mod.warn.title', { name: TARGET, server: SERVER }), exact: true });
    await warning.getByRole('textbox', { name: copy('mod.warn.reason'), exact: true }).fill(REASON);
    await warning.getByRole('button', { name: copy('mod.warn.confirm'), exact: true }).click();
    await expect(warning).toBeHidden();
    const warningNotice = page.getByText(copy('mod.warn.done', { name: TARGET }), { exact: true })
      .or(page.getByText(copy('mod.warn.evidencePending', { name: TARGET }), { exact: true }));
    await expect(warningNotice).toBeVisible();

    // Retrieve through the actual Bot settings panel and assert the returned evidence is rendered.
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('workspace.moderationLog'), exact: true }).click();
    const loadEvidence = settings.getByRole('button', { name: /Load manual evidence|Sending request/u });
    await loadEvidence.click();
    const returned = settings.locator('article').filter({ hasText: REASON });
    await expect.poll(async () => {
      if (await returned.isVisible()) return true;
      const failed = settings.getByText(copy('admin.bot.moderationLog.error'), { exact: true });
      if (await failed.isVisible()) {
        await loadEvidence.click();
      }
      return false;
    }, { timeout: 90_000, intervals: [1_000, 2_000] }).toBe(true);
    await expect(returned).toContainText('warn');
  } finally {
    await targetContext?.close();
    await host?.stop();
  }
});
