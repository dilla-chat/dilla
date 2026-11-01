// v0.1.0-web3.5, private channels from an empty instance. Like admin.spec.ts this suite starts a test host of its own with no account,
// no server and no channel, and runs a server through the shipping clients only: two browsers, one owner (Ada) and one
// member (Björn). Nothing here seeds state or calls the instance's API on anyone's behalf (GET /v1/instance for the name
// the onboarding shows is the one request the spec makes itself). Controls are found by role and accessible name, with the
// copy keys of flow 09 (strings/{roles,perm}.ts, the shell's pins and mention keys) through copy().
// Every dialog's focus return is asserted with toBeFocused(), which jsdom cannot prove through portals and the inert drawer.
import { expect, type Locator, type Page } from '@playwright/test';
import { addServerVia,
  CLASS, TEST_TIMEOUT, WAIT, makeInviteLink, newChannelButton, completeSignUp, copy, expectAccessible, expectComposerReady, newAccount, openChannel, railItem, railNav,
  channelRow, messageRow, sendText, signUp, tag, test, openSettings as openAppSettings, closeSettings as closeAppSettings, composerBox, channelRowMenuItem,
 makeRole as makeRoleInline,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { rmSync } from 'node:fs';

/** Its own ports (E2 reservation). */
const EMPTY: HostConfig = { port: 8493, control: 8494, webRoot: 'packages/web/dist' };
const SERVER = 'Midgard Crew';
const OWNER = 'Ada';
const FRIEND = 'Björn';
const STAFF = 'Staff';
const KEEPERS = 'Keepers';
const ROOM = 'staff-room';

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

function started(): StartedHost {
  if (host === null) throw new Error('the empty test host did not start');
  return host;
}
const button = (scope: Page | Locator, key: string, vars?: Record<string, string>) =>
  scope.getByRole('button', { name: copy(key, vars), exact: true });
const dialogOf = (page: Page, key: string, vars?: Record<string, string>) =>
  page.getByRole('dialog', { name: copy(key, vars), exact: true });
const membersRegion = (page: Page) => page.getByRole('complementary', { name: copy('members.region', { server: SERVER }), exact: true });
const memberRow = (page: Page, name: string) => membersRegion(page).getByRole('listitem').filter({ hasText: name });
const memberTrigger = (page: Page, name: string) =>
  memberRow(page, name).locator('.d-member-list__open');
/** The shell's polite live region: it holds the latest announcement until the next one replaces it. */
const announcer = (page: Page) => page.getByTestId('shell-announcer');
const switchOf = (scope: Locator, key: string) => scope.getByRole('switch', { name: copy(key), exact: true });

/** Opens "server settings" in the sidebar and returns the dialog, with the permissions read. */
async function openSettings(page: Page): Promise<Locator> {
  await button(page, 'admin.open').click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const dialog = dialogOf(page, 'admin.title');
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await expect(dialog.getByRole('navigation', { name: copy('admin.nav'), exact: true })).toBeVisible({ timeout: WAIT });
  return dialog;
}
const navItem = (dialog: Locator, key: string) =>
  dialog.getByRole('navigation', { name: copy('admin.nav'), exact: true }).getByRole('button', { name: copy(key), exact: true });
/** Escape closes the settings dialog and focus goes back to "server settings", never to the page. */
async function closeSettings(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await expect(dialogOf(page, 'admin.title')).toBeHidden();
  await expect(button(page, 'admin.open')).toBeFocused();
}
/** Opens the member's action menu from its row and picks `item`. */
async function memberAction(page: Page, name: string, item: string): Promise<void> {
  await memberTrigger(page, name).click();
  await page.getByRole('dialog', { name }).getByRole('button', { name: copy(item), exact: true }).click();
}
async function setSwitch(toggle: Locator, on: boolean): Promise<void> {
  if ((await toggle.getAttribute('aria-checked')) !== String(on)) await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', String(on));
}

/** Ticks (or unticks) `role` for `name` in the assign dialog, waits for the announcement, and presses Done. */
async function assign(page: Page, name: string, role: string, on: boolean): Promise<void> {
  await memberAction(page, name, 'layout.profile.manageRoles');
  const dialog = dialogOf(page, 'roles.assign.title', { name });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  const box = dialog.getByRole('checkbox', { name: role, exact: true });
  await box.click();
  if (on) await expect(box).toBeChecked({ timeout: WAIT });
  else await expect(box).not.toBeChecked({ timeout: WAIT });
  await expect(announcer(page)).toHaveText(copy(on ? 'roles.assign.given' : 'roles.assign.taken', { role, name }), { timeout: WAIT });
  await button(dialog, 'roles.assign.close').click();
  await expect(dialog).toBeHidden();
}
/** Makes a role from the roles tab with the given permission switches on. */
async function makeRole(page: Page, name: string, rights: string[]): Promise<void> {
  const settings = await openSettings(page);
  await navItem(settings, 'roles.tab').click();
  // Signal task 10c: the inline role editor (every change saves at once).
  await makeRoleInline(page, settings, name, { perms: rights.map(right => right.replace(/^perm\./, '')) });
  await closeSettings(page);
}
/** The channel header's "edit channel" opens channel settings; the private switch lives in its permissions section. */
async function openChannelSettings(page: Page, name: string): Promise<{ dialog: Locator; priv: Locator; opener: Locator }> {
  // Ruling X: "edit channel" is in the channel row's context menu; focus returns to the row.
  const opener = channelRow(page, name);
  await channelRowMenuItem(page, name, 'admin.channel.open');
  const dialog = dialogOf(page, 'admin.channel.titleText', { name });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  const priv = switchOf(dialog, 'chperm.private');
  await expect(priv).toBeVisible({ timeout: WAIT });
  return { dialog, priv, opener };
}

test('private channels run from an empty instance: create, gain, lose, make private and public, the lockout and a muted server', async ({
  page, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = started();

  // 1. Ada starts "Midgard Crew", Björn joins through her invite, and Ada makes the role "Staff".
  await signUp(page, h.invite.split(',')[0], instance, OWNER, h.control);
  await addServerVia(page, 'create');
  const start = dialogOf(page, 'create.server.title');
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
  await expectComposerReady(page, 'general');

  // web3.8 (item 11): the link is made under server settings → Invites.
  const link = await makeInviteLink(page, SERVER);

  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, secondDir);
  try {
    const page2 = second.pages()[0] ?? (await second.newPage());
    await page2.goto(new URL(link).pathname);
    const openLink = page2.getByRole('link', { name: 'Open dilla in this browser', exact: true });
    await expect(openLink).toHaveAttribute('href', /^\/welcome\?invite=[^&\s]+$/);
    await guard.attach(second);
    await openLink.click();
    await completeSignUp(page2, newAccount(FRIEND, h.control), instance);
    await expect(railItem(page2, SERVER)).toBeVisible({ timeout: WAIT });
    await openChannel(page2, SERVER, 'general');
    await expect(memberRow(page, FRIEND)).toBeVisible({ timeout: WAIT });
    await makeRole(page, STAFF, []);

    // 2. Ada makes the text channel "staff-room", private, opened to Staff. Björn's list lacks it (a public "lounge" made
    //    after it shows that his list had caught up). Ada posts "before".
    const newChannel = newChannelButton(page);
    await newChannel.click();
    const made = dialogOf(page, 'create.channel.title', { server: SERVER });
    await expect(made).toBeVisible({ timeout: WAIT });
    await made.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill(ROOM);
    await setSwitch(switchOf(made, 'chperm.create.private'), true);
    const staffBox = made.getByRole('checkbox', { name: STAFF, exact: true });
    await staffBox.click();
    await expect(staffBox).toBeChecked();
    await expectAccessible(page, 'the private channel create');
    // Escape closes the dialog and focus goes back to "new channel"; the dialog then starts over.
    await page.keyboard.press('Escape');
    await expect(made).toBeHidden();
    await expect(newChannel).toBeFocused();
    await newChannel.click();
    await expect(made).toBeVisible({ timeout: WAIT });
    await made.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill(ROOM);
    await setSwitch(switchOf(made, 'chperm.create.private'), true);
    await made.getByRole('checkbox', { name: STAFF, exact: true }).click();
    await page.keyboard.press('Enter');
    await expect(made).toBeHidden({ timeout: WAIT });
    await expect(channelRow(page, ROOM)).toBeVisible({ timeout: WAIT });
    await newChannel.click();
    await made.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill('lounge');
    await page.keyboard.press('Enter');
    await expect(made).toBeHidden({ timeout: WAIT });
    await expect(channelRow(page2, 'lounge')).toBeVisible({ timeout: WAIT });
    await expect(channelRow(page2, ROOM)).toHaveCount(0);
    await openChannel(page, SERVER, ROOM);
    await sendText(page, ROOM, 'before');

    // 3. Ada gives Björn Staff. The room appears for him; "before" is not shown; "after" is; a reload keeps both.
    await assign(page, FRIEND, STAFF, true);
    await expect(memberTrigger(page, FRIEND)).toBeFocused();
    await expect(channelRow(page2, ROOM)).toBeVisible({ timeout: WAIT });
    await openChannel(page2, SERVER, ROOM);
    await sendText(page, ROOM, 'after');
    await expect(messageRow(page2, ROOM, 'after')).toBeVisible({ timeout: WAIT });
    await expect(page2.getByText('before', { exact: true })).toHaveCount(0);
    await page2.reload();
    await openChannel(page2, SERVER, ROOM);
    await expect(messageRow(page2, ROOM, 'after')).toBeVisible({ timeout: WAIT });
    await expect(page2.getByText('before', { exact: true })).toHaveCount(0);

    // 4. With the room open, Ada takes Staff from Björn: he is told, and the room leaves his list.
    await assign(page, FRIEND, STAFF, false);
    await expect(announcer(page2)).toHaveText(copy('chperm.lostAccess', { name: `#${ROOM}` }), { timeout: WAIT });
    await expect(channelRow(page2, ROOM)).toHaveCount(0, { timeout: WAIT });
    await expect(composerBox(page2, ROOM)).toHaveCount(0);
    await expect(channelRow(page2, 'general')).toBeVisible();

    // 5. Ada makes #general private from its settings and then public again. The confirm closes alone on Escape and focus
    //    returns to the switch each time.
    await openChannel(page, SERVER, 'general');
    await openChannel(page2, SERVER, 'lounge');
    const gen = await openChannelSettings(page, 'general');
    await expectAccessible(page, 'the channel permissions section');
    await gen.priv.click();
    const askPrivate = dialogOf(page, 'chperm.makePrivate.title', { name: 'general' });
    await expect(askPrivate).toBeVisible({ timeout: WAIT });
    await expect(button(askPrivate, 'chperm.cancel')).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(askPrivate).toBeHidden();
    await expect(gen.dialog).toBeVisible();
    await expect(gen.priv).toBeFocused();
    await expect(gen.priv).toHaveAttribute('aria-checked', 'false');
    await gen.priv.click();
    await button(askPrivate, 'chperm.makePrivate.confirm').click();
    await expect(askPrivate).toBeHidden({ timeout: WAIT });
    await expect(gen.priv).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
    await expect(gen.priv).toBeFocused();
    await expect(channelRow(page2, 'general')).toHaveCount(0, { timeout: WAIT });
    await gen.priv.click();
    const askPublic = dialogOf(page, 'chperm.makePublic.title', { name: 'general' });
    await expect(askPublic).toBeVisible({ timeout: WAIT });
    await button(askPublic, 'chperm.makePublic.confirm').click();
    await expect(askPublic).toBeHidden({ timeout: WAIT });
    await expect(gen.priv).toHaveAttribute('aria-checked', 'false', { timeout: WAIT });
    await expect(gen.priv).toBeFocused();
    await expect(channelRow(page2, 'general')).toBeVisible({ timeout: WAIT });
    await page.keyboard.press('Escape');
    await expect(gen.dialog).toBeHidden();
    await expect(gen.opener).toBeFocused();

    // 5b. The lockout. Björn holds "Keepers" (manage channels and manage roles, not administrator): he may not make #general
    //     private while no role he holds is allowed to see it. The refusal is shown, the channel stays public.
    await makeRole(page, KEEPERS, ['perm.manageChannels', 'perm.manageRoles']);
    await assign(page, FRIEND, KEEPERS, true);
    await openChannel(page2, SERVER, 'general');
    const mine = await openChannelSettings(page2, 'general');
    await mine.priv.click();
    const lock = dialogOf(page2, 'chperm.makePrivate.title', { name: 'general' });
    await expect(lock).toBeVisible({ timeout: WAIT });
    await button(lock, 'chperm.makePrivate.confirm').click();
    await expect(lock.getByRole('alert')).toHaveText(copy('chperm.error.hidden'), { timeout: WAIT });
    await expect(lock).toBeVisible();
    await expect(button(lock, 'chperm.makePrivate.confirm')).not.toBeFocused();
    await button(lock, 'chperm.cancel').click();
    await expect(lock).toBeHidden();
    await expect(mine.priv).toBeFocused();
    await expect(mine.priv).toHaveAttribute('aria-checked', 'false');
    await page2.keyboard.press('Escape');
    await expect(mine.dialog).toBeHidden();
    await expect(mine.opener).toBeFocused();
    // Ada still sees #general public: nothing was changed.
    await expect(channelRow(page, 'general')).toBeVisible();

    // 6. Björn mutes "Midgard Crew" from the server's ⌄ menu (Signal task 10 fix round 2; Settings → Notifications keeps the
    // prototype's rows): his #general row reads as muted.
    await button(page2, 'admin.open').click();
    const muteServer = page2.getByRole('menuitemcheckbox', { name: copy('signalSettings.menu.muteServer'), exact: true });
    await expect(muteServer).toHaveAttribute('aria-checked', 'false', { timeout: WAIT });
    await muteServer.click();
    await expect(muteServer).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
    await page2.keyboard.press('Escape');
    await expect(channelRow(page2, 'general')).toHaveAccessibleName(copy('shell.channels.rowLabelMuted', { name: 'general', mentions: 0 }), { timeout: WAIT });
    await guard.collectWorkers();
  } finally {
    await second.close();
    rmSync(secondDir, { recursive: true, force: true });
  }
});
