// Manual text timeout acceptance from an empty testhost. Every account, server, role, voice channel and DM is made through
// the shipping Chromium clients; the timeout itself is applied and cleared through the member card.
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { expect, type Locator, type Page } from '@playwright/test';
import {
  addServerVia, TEST_TIMEOUT, WAIT, completeSignUp, composerBox, copy, dmComposer, dmLog, makeInviteLink,
  messageRow, newAccount, newChannelButton, openChannel, railItem, rowAction, rowToolbar, signUp, tag, test, expectRowMenuAction,
 makeRole,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { chromiumMediaArgs, writeFixtures } from '../media/support/fixtures';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const EMPTY: HostConfig = { port: 8531, control: 8532, webRoot: 'packages/web/dist', sfu: { port: 7896, udpPort: 7898 } };
const SERVER = 'Timeout Test';
const OWNER = 'Ada';
const MEMBER = 'Bea';
const ROLE = 'Pin Crew';

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({
  baseURL: `http://127.0.0.1:${String(EMPTY.port)}`,
  launchOptions: { args: chromiumMediaArgs(writeFixtures(join(tmpdir(), 'dilla-timeout-media'))) },
});

test.beforeAll(async () => {
  host = await startTestHost(EMPTY);
  const res = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  instance = name;
});
test.afterAll(async () => { await host?.stop(); });

function started(): StartedHost {
  if (host === null) throw new Error('the empty test host did not start');
  return host;
}
const button = (scope: Page | Locator, key: string, vars?: Record<string, string>) =>
  scope.getByRole('button', { name: copy(key, vars), exact: true });
const dialog = (page: Page, key: string, vars?: Record<string, string>) =>
  page.getByRole('dialog', { name: copy(key, vars), exact: true });
const members = (page: Page) => page.getByRole('complementary', { name: copy('members.region', { server: SERVER }), exact: true });
const memberRow = (page: Page, name: string) => members(page).getByRole('listitem').filter({ hasText: name });
const memberTrigger = (page: Page, name: string) => memberRow(page, name).locator('.d-member-list__open');
const roleRow = (settings: Locator, name: string) => settings.getByRole('list', { name: copy('roles.title'), exact: true }).getByRole('listitem').filter({ hasText: name });
const announce = (page: Page) => page.getByTestId('shell-announcer');

async function makeVoiceChannel(page: Page): Promise<void> {
  await newChannelButton(page).click();
  const create = dialog(page, 'create.channel.title', { server: SERVER });
  await create.getByRole('radio', { name: copy('create.channel.voice'), exact: true }).click();
  await create.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill('lounge');
  await page.keyboard.press('Enter');
  await expect(create).toBeHidden({ timeout: WAIT });
}

async function setSwitch(toggle: Locator): Promise<void> {
  if (await toggle.getAttribute('aria-checked') !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', 'true');
}

async function expectWriteRefused(row: Locator): Promise<void> {
  const pending = row.locator('.d-message-row__pending');
  await expect(pending).toHaveAttribute('data-tone', 'failed', { timeout: WAIT });
  await expect(pending).toContainText(copy('shell.message.notSaved'));
}

async function discardRefusedWrite(row: Locator): Promise<void> {
  await row.locator('.d-message-row__pending').getByRole('button', { name: copy('shell.message.discard'), exact: true }).click();
  await expect(row.locator('.d-message-row__pending')).toHaveCount(0, { timeout: WAIT });
}

test('manual text timeout blocks community writes and recovers while read, voice, DMs and own deletion continue', async ({
  page, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  test.skip(browserName !== 'chromium', 'the in-process voice acceptance uses Chromium fake media');
  await page.context().grantPermissions(['microphone']);
  const h = started();

  // Ada creates the community and voice channel in the browser. Bea joins through an invite made by the owner UI.
  await signUp(page, h.invite.split(',')[0] ?? '', instance, OWNER, h.control);
  await addServerVia(page, 'create');
  const create = dialog(page, 'create.server.title');
  await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(create).toBeHidden({ timeout: WAIT });
  await openChannel(page, SERVER, 'general');
  await makeVoiceChannel(page);
  await openChannel(page, SERVER, 'general');
  const invite = await makeInviteLink(page, SERVER);

  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, secondDir);
  try {
    await second.grantPermissions(['microphone']);
    const member = second.pages()[0] ?? await second.newPage();
    await member.goto(new URL(invite).pathname);
    await member.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
    await guard.attach(second);
    await completeSignUp(member, newAccount(MEMBER, h.control), instance);
    await expect(railItem(member, SERVER)).toBeVisible({ timeout: WAIT });
    await openChannel(member, SERVER, 'general');
    await expect(memberRow(page, MEMBER)).toBeVisible({ timeout: WAIT });
    await expect(memberRow(member, OWNER)).toBeVisible({ timeout: WAIT });

    // Give Bea pin rights through the real roles panel so a later pin refusal is attributable to the timeout.
    await button(page, 'admin.open').click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    let settings = dialog(page, 'admin.title');
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('roles.tab'), exact: true }).click();
    // Signal task 10c: the inline role editor; each switch saves at once.
    await makeRole(page, settings, ROLE, { perms: ['pin', 'react'] });
    await page.keyboard.press('Escape');
    await expect(settings).toBeHidden();
    await memberTrigger(page, MEMBER).click();
    await page.getByRole('dialog', { name: MEMBER, exact: true })
      .getByRole('button', { name: copy('layout.profile.manageRoles'), exact: true }).click();
    const assign = dialog(page, 'roles.assign.title', { name: MEMBER });
    const pinCrew = assign.getByRole('checkbox', { name: ROLE, exact: true });
    await pinCrew.click();
    await expect(pinCrew).toBeChecked({ timeout: WAIT });
    await button(assign, 'roles.assign.close').click();
    await expect(assign).toBeHidden();

    // Bea writes two messages before timeout; they provide real own-message targets for edit and permitted deletion.
    const ownForEdit = `edit target ${tag()}`;
    const ownForDelete = `delete target ${tag()}`;
    await composerBox(member, 'general').fill(ownForEdit);
    await member.keyboard.press('Enter');
    const editRow = messageRow(member, 'general', ownForEdit);
    await expect(editRow).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    await composerBox(member, 'general').fill(ownForDelete);
    await member.keyboard.press('Enter');
    const deleteRow = messageRow(member, 'general', ownForDelete);
    await expect(deleteRow).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    const ownerText = `read during timeout ${tag()}`;
    await composerBox(page, 'general').fill(ownerText);
    await page.keyboard.press('Enter');
    const ownerRow = messageRow(member, 'general', ownerText);
    await expect(ownerRow).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // The owner applies a ten minute timeout through Bea's row's person menu (a right-click; Signal task 9).
    await memberTrigger(page, MEMBER).click({ button: 'right' });
    await page.getByRole('menu', { name: copy('members.menu.label', { name: MEMBER }), exact: true })
      .getByRole('menuitem', { name: copy('mod.timeout.menu'), exact: true }).click();
    let timeout = dialog(page, 'mod.timeout.title', { name: MEMBER });
    await timeout.getByLabel(copy('mod.timeout.duration'), { exact: true }).selectOption('tenMinutes');
    await timeout.getByRole('button', { name: copy('mod.timeout.confirm'), exact: true }).click();
    await expect(timeout).toBeHidden({ timeout: WAIT });
    await expect(announce(page)).toHaveText(copy('mod.timeout.done', { name: MEMBER }), { timeout: WAIT });

    // The target's live gateway wakeup refreshes advisory state without reloading the open page.
    // Reading continues, while the composer and each actual server write fail closed.
    await expect(ownerRow).toBeVisible({ timeout: WAIT });
    await expect(composerBox(member, 'general')).toBeDisabled({ timeout: WAIT });

    await rowAction(ownerRow, 'react');
    await member.getByRole('dialog', { name: copy('shell.emoji.label'), exact: true })
      .getByRole('button', { name: copy('shell.emoji.01'), exact: true }).click();
    await expectWriteRefused(ownerRow);
    await discardRefusedWrite(ownerRow);

    await rowAction(editRow, 'edit');
    await member.getByRole('textbox', { name: copy('shell.edit.label'), exact: true }).fill(`edited while timed out ${tag()}`);
    await member.getByRole('button', { name: copy('shell.edit.save'), exact: true }).click();
    await expectWriteRefused(editRow);
    await expect(editRow).toContainText(ownForEdit);
    await discardRefusedWrite(editRow);

    await expectRowMenuAction(editRow, 'pin', true);
    await rowAction(editRow, 'pin');
    await expectWriteRefused(editRow);
    await expect(editRow).not.toHaveAttribute('data-pinned', 'true');
    await discardRefusedWrite(editRow);

    // A timed out member can still remove their own message.
    await rowAction(deleteRow, 'delete');
    const confirmDelete = dialog(member, 'shell.delete.title');
    await confirmDelete.getByRole('button', { name: copy('shell.delete.confirm'), exact: true }).click();
    await expect(confirmDelete).toBeHidden({ timeout: WAIT });
    await expect(messageRow(member, 'general', ownForDelete)).toHaveCount(0, { timeout: WAIT });
    await expect(messageRow(page, 'general', ownForDelete)).toHaveCount(0, { timeout: WAIT });

    // DMs are outside it too: Bea opens Ada's profile action and sends her a direct message.
    await memberTrigger(member, OWNER).click();
    await member.getByRole('dialog', { name: OWNER, exact: true })
      .getByRole('button', { name: copy('members.menu.message'), exact: true }).click();
    const dmText = `direct while timed out ${tag()}`;
    await dmComposer(member).fill(dmText);
    await member.keyboard.press('Enter');
    await expect(dmLog(member).getByText(dmText, { exact: true })).toBeVisible({ timeout: WAIT });

    // Clearing the timeout through the same moderator flow restores community posting immediately.
    await memberTrigger(page, MEMBER).click({ button: 'right' });
    await page.getByRole('menu', { name: copy('members.menu.label', { name: MEMBER }), exact: true })
      .getByRole('menuitem', { name: copy('mod.timeout.menu'), exact: true }).click();
    timeout = dialog(page, 'mod.timeout.title', { name: MEMBER });
    await timeout.getByLabel(copy('mod.timeout.duration'), { exact: true }).selectOption('clear');
    await timeout.getByRole('button', { name: copy('mod.timeout.confirm'), exact: true }).click();
    await expect(timeout).toBeHidden({ timeout: WAIT });
    await openChannel(member, SERVER, 'general');
    await expect(composerBox(member, 'general')).toBeEnabled({ timeout: WAIT });
    const recovery = `posting restored ${tag()}`;
    await composerBox(member, 'general').fill(recovery);
    await member.keyboard.press('Enter');
    await expect(messageRow(page, 'general', recovery)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // Voice is outside the community text restriction.
    const voice = member.getByRole('button', { name: copy('voice.open', { name: 'lounge' }), exact: true });
    await voice.click();
    await expect(member.locator('[data-call-phase]').first()).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
  } finally {
    await second.close();
  }
});
