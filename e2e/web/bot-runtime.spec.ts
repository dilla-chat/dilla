import { openInviteInClient } from './support/invite';
import { expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import {
  FIREFOX_FAVICON_LINE, WAIT, addServerVia, completeSignUp, composerBox, copy, isFirefoxBrowserFaviconViolation, makeInviteLink, messageRow, newAccount,
  openChannel, reactionChip, rowAction, sendText, signIn, signUp, test, makeRole, roleButton, ensureMembersOpen,
} from './support/app';
import { restoreDesktopWindow } from './support/electron.ts';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { e2ePort } from './support/ports';
import { markBotChannel, watchBearer } from './support/bot-channel';

const BOT_RUNTIME_PORT = Number(process.env.DILLA_E2E_BOT_RUNTIME_PORT ?? e2ePort(8491));
const HOST: HostConfig = { port: BOT_RUNTIME_PORT, control: BOT_RUNTIME_PORT + 1, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true };
const ROLE_PANEL_PORT = Number(process.env.DILLA_E2E_ROLE_PANEL_PORT ?? HOST.port);
const ROLE_PANEL_HOST: HostConfig = { ...HOST, port: ROLE_PANEL_PORT, control: ROLE_PANEL_PORT + 1, advanceClockWithWall: true };
const SERVER = 'Runtime Bot Acceptance';
const RECOVERY_SERVER = 'Bot Moderation Log';

test.use({ baseURL: `http://127.0.0.1:${ROLE_PANEL_PORT}` });

test('the Firefox web guard exempts only its browser-chrome favicon refusal', () => {
  const favicon = {
    directive: 'img-src', blockedURI: 'http://127.0.0.1:8491/favicon.ico', sourceFile: 'resource', sample: '',
  };
  expect(isFirefoxBrowserFaviconViolation('firefox-web', favicon)).toBe(true);
  expect(isFirefoxBrowserFaviconViolation('chromium-web', favicon)).toBe(false);
  expect(isFirefoxBrowserFaviconViolation('firefox-web', { ...favicon, sourceFile: 'http://127.0.0.1:8491/i/invite' })).toBe(false);
  expect(isFirefoxBrowserFaviconViolation('firefox-web', { ...favicon, blockedURI: 'http://127.0.0.1:8491/avatar.png' })).toBe(false);
  expect(FIREFOX_FAVICON_LINE.test('[JavaScript Error: "Content-Security-Policy: The page.s settings blocked the loading of a resource (img-src) at http://127.0.0.1:8491/favicon.ico . {file: "resource:///modules/FaviconLoader.sys.mjs" line: 52}]')).toBe(true);
});

test('a bot enabled through server settings reaches ready through the native runtime', async ({ page, instanceName }) => {
  let host: StartedHost | null = null;
  try {
    host = await startTestHost(HOST);
    const account = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await expect(settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })).toBeVisible();
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await expect(settings.getByText(copy('admin.bot.none'), { exact: true })).toBeVisible();
    // Keep the capability mask empty for this baseline; the rendered chooser is source-current.
    await expect(settings.getByRole('switch', { name: copy('serverSettings.bot.cap.roles'), exact: true })).not.toHaveAttribute('aria-disabled', 'true');
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(account.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();

    const runtimeCredentialFile = host.runtimeCredentialFile;
    if (runtimeCredentialFile === undefined) throw new Error('test host did not expose its protected runtime credential path');
    await expect.poll(async () => {
      const token = readFileSync(runtimeCredentialFile, 'utf8').trim();
      // The runtime credential's list is the Bot socket's alone under a supervisor; the host serves it on its control listener.
      const response = await fetch(`${host!.control}/debug/bot/installations`, {
        headers: { authorization: `Bearer ${token}`, accept: 'application/cbor' },
      });
      if (!response.ok) return `HTTP ${response.status}`;
      const rows = arr(decode(new Uint8Array(await response.arrayBuffer())));
      return rows.length === 1 ? arr(rows[0])[2] : `count ${rows.length}`;
    }, { timeout: 90_000, intervals: [1_000, 2_000] }).toBe(3n);

    await expect.poll(async () => {
      await page.keyboard.press('Escape');
      await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
      const refreshed = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
      await refreshed.getByRole('navigation', { name: copy('admin.nav'), exact: true })
        .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
      return refreshed.locator('.dw-bot-settings').getByRole('status').innerText();
    }, { timeout: 90_000, intervals: [1_000, 2_000, 3_000] }).toBe(copy('admin.bot.status.ready'));
  } finally {
    await host?.stop();
  }
});

test('literal music command text never executes through the native runtime', async ({ page, instanceName, browser, guard }) => {
  let host: StartedHost | null = null;
  let secondContext: import('@playwright/test').BrowserContext | null = null;
  try {
    host = await startTestHost({ ...HOST, advanceClockWithWall: true });
    const account = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await expect(settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })).toBeVisible();
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await expect(settings.getByText(copy('admin.bot.none'), { exact: true })).toBeVisible();
    await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.music'), exact: true }).click();
    await expect(settings.getByRole('switch', { name: copy('serverSettings.bot.cap.roles'), exact: true })).not.toHaveAttribute('aria-disabled', 'true');
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(account.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();

    const runtimeCredentialFile = host.runtimeCredentialFile;
    if (runtimeCredentialFile === undefined) throw new Error('test host did not expose its protected runtime credential path');
    await expect.poll(async () => {
      const token = readFileSync(runtimeCredentialFile, 'utf8').trim();
      // The runtime credential's list is the Bot socket's alone under a supervisor; the host serves it on its control listener.
      const response = await fetch(`${host!.control}/debug/bot/installations`, {
        headers: { authorization: `Bearer ${token}`, accept: 'application/cbor' },
      });
      if (!response.ok) return `HTTP ${response.status}`;
      const rows = arr(decode(new Uint8Array(await response.arrayBuffer())));
      return rows.length === 1 ? arr(rows[0])[2] : `count ${rows.length}`;
    }, { timeout: 90_000, intervals: [1_000, 2_000] }).toBe(3n);

    await expect.poll(async () => {
      await page.keyboard.press('Escape');
      await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
      const refreshed = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
      await refreshed.getByRole('navigation', { name: copy('admin.nav'), exact: true })
        .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
      return refreshed.locator('.dw-bot-settings').getByRole('status').innerText();
    }, { timeout: 90_000, intervals: [1_000, 2_000, 3_000] }).toBe(copy('admin.bot.status.ready'));
    await page.keyboard.press('Escape');
    await openChannel(page, SERVER, 'general');

    const inviteLink = await makeInviteLink(page, SERVER);
    secondContext = await browser.newContext({ baseURL: host.url });
    await guard.attach(secondContext);
    const secondPage = await secondContext.newPage();
    await openInviteInClient(secondPage, inviteLink);
    await completeSignUp(secondPage, newAccount('Bea', host.control), instanceName);
    await openChannel(secondPage, SERVER, 'general');
    await openChannel(page, SERVER, 'general');
    for (const text of ['/music', '/play northern lights']) {
      await composerBox(page, 'general').fill(`/${text}`);
      await page.getByRole('textbox', { name: 'Command', exact: true }).press('Enter');
      await expect(messageRow(page, 'general', text)).toHaveAttribute('data-state', 'ok');
    }
    await expect(page.getByRole('log').locator('.d-message-row [data-kind=bot]')).toHaveCount(0);
    await expect(secondPage.getByRole('log').locator('.d-message-row [data-kind=bot]')).toHaveCount(0);

  } finally {
    await secondContext?.close();
    await host?.stop();
  }
});

// Bot v2 Task 23: the moderation log's pages are sealed Bot reads (160, 164), answered in the socket frame, with no
// per-device interaction group or response journal. Each device reads them itself, before and after a Bot restart.
test('the moderation log reads through sealed Bot commands on each device, across a Bot restart', async ({ page, instanceName, browser, guard }) => {
  let host: StartedHost | null = null;
  let secondContext: import('@playwright/test').BrowserContext | null = null;
  try {
    host = await startTestHost(HOST);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(RECOVERY_SERVER);
    await page.keyboard.press('Enter');

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.warnings'), exact: true }).click();
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(owner.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
    await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') })).toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });

    const readLog = async (dialog: import('@playwright/test').Locator): Promise<void> => {
      await dialog.getByRole('navigation', { name: copy('admin.nav'), exact: true })
        .getByRole('button', { name: copy('workspace.moderationLog'), exact: true }).click();
      await dialog.getByRole('button', { name: copy('admin.bot.moderationLog.loadAutomatic'), exact: true }).click();
      await expect(dialog.getByText(copy('admin.bot.moderationLog.empty'), { exact: true })).toBeVisible({ timeout: 90_000 });
      await dialog.getByRole('button', { name: copy('admin.bot.moderationLog.load'), exact: true }).click();
      await expect(dialog.getByText(copy('admin.bot.moderationLog.empty'), { exact: true })).toBeVisible({ timeout: 90_000 });
    };
    await readLog(settings);

    await host.restartBotRuntime();
    await readLog(settings);

    // Device 2 signs in from the same recovery key and reads the log itself.
    secondContext = await browser.newContext({ baseURL: host.url });
    await guard.attach(secondContext);
    const secondPage = await secondContext.newPage();
    await signIn(secondPage, owner, instanceName);
    await expect(secondPage.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
      .getByRole('button', { name: RECOVERY_SERVER, exact: true })).toBeVisible({ timeout: 90_000 });
    await secondPage.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => secondPage.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    await readLog(secondPage.getByRole('dialog', { name: copy('admin.title'), exact: true }));
  } finally {
    await secondContext?.close();
    await host?.stop();
  }
});

test('a Roles-enabled bot applies the configured join role on join and rejoin', async ({ page, instanceName, browser, guard }) => {
  let host: StartedHost | null = null;
  let secondContext: import('@playwright/test').BrowserContext | null = null;
  const joinRoleName = 'Arrival Crew';
  try {
    host = await startTestHost(HOST);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    let settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    const nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await nav.getByRole('button', { name: copy('roles.tab'), exact: true }).click();
    // Signal task 10c: the inline role editor.
    await makeRole(page, settings, joinRoleName);
    // The role's button is named "{name}, {count} members" (Signal task 10c).
    const roleRow = roleButton(settings, joinRoleName);
    await expect(roleRow).toHaveAccessibleName(copy('serverSettings.roles.row.other', { name: joinRoleName, count: 0 }));

    await nav.getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    const rolesCapability = settings.getByRole('switch', { name: copy('serverSettings.bot.cap.roles'), exact: true });
    await expect(rolesCapability).toBeEnabled();
    await rolesCapability.check();
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(owner.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();

    const runtimeCredentialFile = host.runtimeCredentialFile;
    if (runtimeCredentialFile === undefined) throw new Error('test host did not expose its protected runtime credential path');
    await expect.poll(async () => {
      const token = readFileSync(runtimeCredentialFile, 'utf8').trim();
      // The runtime credential's list is the Bot socket's alone under a supervisor; the host serves it on its control listener.
      const response = await fetch(`${host!.control}/debug/bot/installations`, {
        headers: { authorization: `Bearer ${token}`, accept: 'application/cbor' },
      });
      if (!response.ok) return `HTTP ${response.status}`;
      const rows = arr(decode(new Uint8Array(await response.arrayBuffer())));
      return rows.length === 1 ? arr(rows[0])[2] : `count ${rows.length}`;
    }, { timeout: 90_000, intervals: [1_000, 2_000] }).toBe(3n);
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') })).toHaveText(copy('admin.bot.status.ready'), { timeout: 30_000 });
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('workspace.joinRoles'), exact: true }).click();

    const joinRoleSelect = settings.getByRole('combobox', { name: copy('admin.bot.joinRole.label'), exact: true });
    await expect(joinRoleSelect).toBeVisible();
    const selectedJoinRole = await joinRoleSelect.selectOption({ label: joinRoleName });
    const saveJoinRole = settings.locator('.d-settings-frame__footer').getByRole('button', { name: copy('admin.overview.save'), exact: true });
    await expect(saveJoinRole).toBeEnabled();
    await saveJoinRole.click();
    await expect(saveJoinRole).toHaveCount(0, { timeout: WAIT });
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('workspace.joinRoles'), exact: true }).click();
    await expect(settings.getByRole('combobox', { name: copy('admin.bot.joinRole.label'), exact: true }))
      .toHaveValue(selectedJoinRole[0] ?? '');
    await settings.getByRole('button', { name: copy('settings.close'), exact: true }).click();
    await expect(settings).toBeHidden();
    const inviteLink = await makeInviteLink(page, SERVER);

    secondContext = await browser.newContext({ baseURL: host.url });
    await guard.attach(secondContext);
    const secondPage = await secondContext.newPage();
    await openInviteInClient(secondPage, inviteLink);
    await completeSignUp(secondPage, newAccount('Bea', host.control), instanceName);
    await expect.poll(async () => {
      const openSettings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
      if (await openSettings.isVisible()) {
        await openSettings.getByRole('button', { name: copy('settings.close'), exact: true }).click();
        await expect(openSettings).toBeHidden();
      }
      await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
      settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
      await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
        .getByRole('button', { name: copy('roles.tab'), exact: true }).click();
      return roleRow.getAttribute('aria-label');
    }, { timeout: 90_000, intervals: [1_000, 2_000] }).toContain(copy('roles.row.one'));

    await secondPage.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => secondPage!.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const secondSettings = secondPage.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await secondSettings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.leave'), exact: true }).click();
    await secondSettings.getByRole('group').getByRole('button', { name: copy('admin.leave.open'), exact: true }).click();
    const leaveDialog = secondPage.getByRole('dialog', { name: copy('admin.leave.title', { server: SERVER }), exact: true });
    await leaveDialog.getByRole('button', { name: copy('admin.leave.confirm'), exact: true }).click();
    await expect(leaveDialog).toBeHidden();
    await expect(secondPage.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
      .getByRole('button', { name: SERVER, exact: true })).toHaveCount(0);
    await expect(roleRow).toHaveAttribute('aria-label', new RegExp(copy('roles.row.other', { count: 0 })));

    await addServerVia(secondPage, 'join');
    const joinDialog = secondPage.getByRole('dialog', { name: copy('join.title'), exact: true });
    await joinDialog.getByLabel(copy('join.invite'), { exact: true }).fill(inviteLink);
    await joinDialog.getByRole('button', { name: copy('join.submit'), exact: true }).click();
    await expect(joinDialog).toBeHidden();
    await expect(secondPage.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
      .getByRole('button', { name: SERVER, exact: true })).toBeVisible();
    await expect.poll(() => roleRow.getAttribute('aria-label'), { timeout: 90_000, intervals: [1_000, 2_000] })
      .toContain(copy('roles.row.one'));
  } finally {
    await secondContext?.close();
    await host?.stop();
  }
});

test('a human publishes a bot-authored self-service role panel and its reaction grant keeps its own source', async ({ page, instanceName, browser, guard }) => {
  let host: StartedHost | null = null;
  let secondContext: import('@playwright/test').BrowserContext | null = null;
  let secondPage: import('@playwright/test').Page | null = null;
  const roleName = 'Readers';
  try {
    host = await startTestHost(ROLE_PANEL_HOST);
    const bearerOf = watchBearer(page);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    let settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    let nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await nav.getByRole('button', { name: copy('roles.tab'), exact: true }).click();
    // Signal task 10c: the inline role editor; "show separately" is in the role's menu.
    await makeRole(page, settings, roleName, { hoist: true });

    await nav.getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.roles'), exact: true }).click();
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(owner.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();

    const runtimeCredentialFile = host.runtimeCredentialFile;
    if (runtimeCredentialFile === undefined) throw new Error('test host did not expose its protected runtime credential path');
    await expect.poll(async () => {
      const token = readFileSync(runtimeCredentialFile, 'utf8').trim();
      // The runtime credential's list is the Bot socket's alone under a supervisor; the host serves it on its control listener.
      const response = await fetch(`${host!.control}/debug/bot/installations`, {
        headers: { authorization: `Bearer ${token}`, accept: 'application/cbor' },
      });
      if (!response.ok) return `HTTP ${response.status}`;
      const rows = arr(decode(new Uint8Array(await response.arrayBuffer())));
      return rows.length === 1 ? arr(rows[0])[2] : `count ${rows.length}`;
    }, { timeout: 90_000, intervals: [1_000, 2_000] }).toBe(3n);

    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await nav.getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') })).toHaveText(copy('admin.bot.status.ready'), { timeout: 30_000 });

    // Approve a plain role using the owner UI; only approved, eligible roles can appear in the panel publisher.
    await nav.getByRole('button', { name: copy('admin.nav.selfRoles'), exact: true }).click();
    const approval = settings.getByRole('group').filter({ hasText: copy('admin.bot.selfRoles.configure') });
    const approvalCheckbox = approval.getByLabel(roleName, { exact: true });
    await expect(approvalCheckbox).not.toBeChecked();
    await approvalCheckbox.click();
    await expect(approvalCheckbox).toBeChecked();
    await expect(settings.locator('.d-sr-only[role="status"]')).toHaveText(copy('admin.bot.selfRoles.saved'));

    // Join the second human before publishing so the acceptance exercises live encrypted delivery.
    await page.keyboard.press('Escape');
    await expect(settings).toBeHidden();
    const inviteLink = await makeInviteLink(page, SERVER);
    secondContext = await browser.newContext({ baseURL: host.url });
    await guard.attach(secondContext);
    secondPage = await secondContext.newPage();
    await openInviteInClient(secondPage, inviteLink);
    await completeSignUp(secondPage, newAccount('Bea', host.control), instanceName);
    await openChannel(secondPage, SERVER, 'general');

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await nav.getByRole('button', { name: copy('admin.nav.selfRoles'), exact: true }).click();

    // Bot v2 Task 17: the Bot posts only into Bot channels; the panel's target is marked first.
    await markBotChannel(page, host.url, bearerOf, 'general');
    await settings.getByLabel(`${copy('admin.bot.rolePanel.emoji')} for ${roleName}`, { exact: true }).fill('👍');
    await settings.getByRole('combobox', { name: copy('admin.bot.rolePanel.channel'), exact: true }).selectOption({ label: 'general' });
    await settings.getByRole('button', { name: copy('admin.bot.rolePanel.publish'), exact: true }).click();
    const selfRoles = settings.getByRole('region', { name: copy('admin.bot.selfRoles.title'), exact: true });
    await expect(selfRoles.getByRole('status').filter({ hasText: copy('admin.bot.rolePanel.pending') })).toHaveText(copy('admin.bot.rolePanel.pending'));
    await expect(selfRoles.getByRole('status').filter({ hasText: copy('admin.bot.rolePanel.published') })).toHaveText(copy('admin.bot.rolePanel.published'), { timeout: 90_000 });
    await page.keyboard.press('Escape');
    await expect(settings).toBeHidden();

    const panel = messageRow(page, 'general', '👍 Readers');
    await expect(panel).toBeVisible({ timeout: 30_000 });
    await expect(panel).toContainText('React with the matching emoji to choose a role:');
    await expect(panel).toContainText('👍 Readers');
    await expect(panel.getByText('bot', { exact: true })).toBeVisible();
    await expect(panel).toHaveAttribute('data-state', 'ok');

    if (secondPage === null) throw new Error('second member was not joined before panel publish');
    const secondPanel = messageRow(secondPage, 'general', '👍 Readers');
    await expect(secondPanel).toBeVisible({ timeout: 30_000 });
    await rowAction(secondPanel, 'react');
    const emojiPicker = secondPage.getByRole('dialog', { name: copy('shell.emoji.label'), exact: true });
    await emojiPicker.getByRole('button', { name: copy('shell.emoji.01'), exact: true }).click();
    await expect(reactionChip(secondPanel, copy('shell.emoji.01'), 1)).toHaveAttribute('aria-pressed', 'true', { timeout: 30_000 });
    const members = secondPage.getByRole('complementary', { name: copy('members.region', { server: SERVER }), exact: true });
    const memberRole = (count: number) => members.getByRole('heading', { name: copy('members.group', { name: roleName, count }), exact: true });
    // A second desktop app can leave a window too narrow for the member pane (a tiling window manager shrinks it), and
    // the shell then closes the pane (moderation-evidence.spec, admin.spec and roles.spec do the same).
    await restoreDesktopWindow(secondPage);
    await ensureMembersOpen(secondPage, members);
    await expect(memberRole(1)).toBeVisible({ timeout: 30_000 });

    // The panel reaction and the member's ordinary self-service choice are separate grants.
    await secondPage.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => secondPage!.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const memberSettings = secondPage.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await memberSettings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.selfRoles'), exact: true }).click();
    let selfRole = memberSettings.getByRole('group').getByLabel(roleName, { exact: true });
    await expect(selfRole).toBeVisible();
    await expect(selfRole).not.toBeChecked();
    await selfRole.click();
    await expect(selfRole).toBeChecked();
    await secondPage.keyboard.press('Escape');
    await rowAction(secondPanel, 'react');
    const removePicker = secondPage.getByRole('dialog', { name: copy('shell.emoji.label'), exact: true });
    await removePicker.getByRole('button', { name: copy('shell.emoji.01'), exact: true }).click();
    await expect(reactionChip(secondPanel, copy('shell.emoji.01'), 1)).toHaveCount(0);
    await secondPage.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => secondPage!.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const settingsAfterRemoval = secondPage.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await settingsAfterRemoval.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.selfRoles'), exact: true }).click();
    selfRole = settingsAfterRemoval.getByRole('group').getByLabel(roleName, { exact: true });
    await expect(selfRole).toBeChecked();
    await expect(memberRole(1)).toBeVisible({ timeout: 30_000 });
    await selfRole.click();
    await expect(selfRole).not.toBeChecked();
    await expect(memberRole(1)).toHaveCount(0, { timeout: 30_000 });
  } finally {
    await secondContext?.close();
    await host?.stop();
  }
});
