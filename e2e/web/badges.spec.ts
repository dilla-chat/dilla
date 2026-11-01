import { expect, type Page } from '@playwright/test';
import {
  CLASS, HEX32, PUBLIC_URL, TEST_TIMEOUT, WAIT, channelRow, clickNotification, closeSettings, copy, dmComposer,
  dmLog, dmsNav, expectAccessible, expectComposerReady, expectShell, homeTile, instanceInvite, joinCommunity, messageRow,
  notificationsShown, openChannel, openSettings, permissionRequests, railItem, recordNotifications, sendText,
  setNotifyDefault, shownName, signUp, tag, test, type Peer,
} from './support/app';

test.describe.configure({ timeout: TEST_TIMEOUT });

/** The browser's user id as the peer's MLS state authenticates it: one message into channel 1. */
async function browserUser(page: Page, peer: Peer, channel: string): Promise<string> {
  const hello = `hello ${tag()}`;
  await sendText(page, channel, hello);
  const user = (await peer.waitFor(hello)).sender_user;
  expect(user).toMatch(HEX32);
  return user;
}

/** A row's accessible name: the bare name while quiet, else `{name}, unread {unread}, mentions {mentions}` (ruling 34). */
function rowLabel(name: string, unread: number, mentions: number): string {
  return unread === 0 && mentions === 0 ? name : copy('shell.channels.rowLabel', { name, unread, mentions });
}
/** A muted row's accessible name: `{name}, muted, mentions {mentions}` (ruling 15). */
function mutedLabel(name: string, mentions: number): string {
  return copy('shell.channels.rowLabelMuted', { name, mentions });
}

const tagsShown = async (page: Page): Promise<string[]> => (await notificationsShown(page)).map((n) => n.tag);

test('unopened channels badge their unread messages and mentions, opening reads them, and a mute hides the count but not the mention', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({ channels: 3 });
  const [one, two, three] = peer.channelNames;
  const [id1, id2, id3] = setup.channel_ids;
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, one);

  // Join-all: the browser holds the groups of the two channels it never opened.
  await peer.waitForDevices(2, id1);
  await peer.waitForDevices(2, id2);
  await peer.waitForDevices(2, id3);

  const row1 = channelRow(page, one);
  const row2 = channelRow(page, two);
  const row3 = channelRow(page, three);
  const rail = railItem(page, peer.communityName);
  // Quiet rows are named by their bare names (rowLabel answers the name at 0 and 0).
  for (const [row, name] of [[row1, one], [row2, two], [row3, three]] as const) {
    await expect(row).toHaveAccessibleName(name, { timeout: WAIT });
    await expect(row).toHaveAttribute('data-unread', '0');
  }
  const user = await browserUser(page, peer, one);
  await expect(row1).toHaveAccessibleName(one);

  await peer.send(`plain in two ${tag()}`, id2);
  await expect(row2).toHaveAttribute('data-unread', '1', { timeout: WAIT });
  await expect(row2).toHaveAccessibleName(rowLabel(two, 1, 0));
  expect(rowLabel(two, 1, 0)).toBe(`${two}, unread 1, mentions 0`);
  await expect(rail).toHaveAttribute('data-unread', '1', { timeout: WAIT });

  await peer.send(`<@${user}> look here ${tag()}`, id3);
  await expect(row3).toHaveAttribute('data-mentions', '1', { timeout: WAIT });
  await expect(row3).toHaveAttribute('data-unread', '1');
  await expect(row3).toHaveAccessibleName(rowLabel(three, 1, 1));
  await expect(rail).toHaveAttribute('data-unread', '2', { timeout: WAIT });
  await expect(rail).toHaveAttribute('data-mentions', '1');
  await expectAccessible(page, 'shell with badges');

  // Opening channel two reads it (markRead on open).
  await channelRow(page, two).click();
  await expectComposerReady(page, two);
  await expect(row2).toHaveAccessibleName(two, { timeout: WAIT });
  await expect(row2).toHaveAttribute('data-unread', '0');
  await expect(rail).toHaveAttribute('data-unread', '1', { timeout: WAIT });

  // Mute channel three: its count goes, its mention stays.
  // Signal task 10 fix rounds 1–2: Settings → Notifications keeps the prototype's rows; a channel mutes from its row's menu
  // (Mute is a menuitemcheckbox, which keeps the menu open: Escape closes it).
  await channelRow(page, three).click({ button: 'right' });
  const rowMenu = page.getByRole('menu', { name: copy('nav.channel.menuOf', { name: three }), exact: true });
  await rowMenu.getByRole('menuitemcheckbox', { name: copy('notify.mute'), exact: true }).click();
  await expect(rowMenu.getByRole('menuitemcheckbox', { name: copy('notify.mute'), exact: true })).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
  await page.keyboard.press('Escape');
  await expect(row3).toHaveAttribute('data-muted', 'true', { timeout: WAIT });
  await expect(row3).toHaveAttribute('data-unread', '0');
  await expect(row3).toHaveAttribute('data-mentions', '1');
  await expect(row3).toHaveAccessibleName(mutedLabel(three, 1));
  expect(mutedLabel(three, 1)).toBe(`${three}, muted, mentions 1`);

  // Device-local and kept: a reload brings back the read marker and the mute.
  await page.reload();
  await expectShell(page);
  await expect(channelRow(page, two)).toHaveAccessibleName(two, { timeout: WAIT });
  await expect(channelRow(page, three)).toHaveAccessibleName(mutedLabel(three, 1), { timeout: WAIT });
  await expect(channelRow(page, three)).toHaveAttribute('data-unread', '0');
});

// Signal task 10 fix round 2–3: the device-wide notify default (it governs direct messages) is the DM list head's menu.
test('the DM list head\u2019s menu sets the device-wide notify default, and it is kept', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await homeTile(page).click();
  const trigger = page.getByRole('button', { name: copy('signalSettings.menu.dms'), exact: true });
  const modes = page.getByRole('group', { name: copy('notify.title'), exact: true });
  const every = modes.getByRole('menuitemradio', { name: copy('signalSettings.menu.everything'), exact: true });
  await trigger.click();
  await expect(modes.getByRole('menuitemradio', { name: copy('signalSettings.menu.dmsMentions'), exact: true })).toHaveAttribute('aria-checked', 'true');
  await expectAccessible(page, 'the DM list head menu');
  await every.click();
  await expect(trigger).toBeFocused();
  await trigger.click();
  await expect(every).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
  await page.keyboard.press('Escape');
  await page.reload();
  await expectShell(page);
  await homeTile(page).click();
  await trigger.click();
  await expect(every).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
});

test('a direct message opened from the DMs tab reaches the peer, and the reply badges it until it is read', async ({ page, context, peer, instanceName }, testInfo) => {
  const chromium = testInfo.project.name === 'chromium-web';
  if (chromium) {
    await context.grantPermissions(['notifications'], { origin: PUBLIC_URL });
    await recordNotifications(context);
  }
  const setup = await peer.setup();
  await peer.register();
  const channel = peer.channelName;
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, channel);
  const deviceA = (await peer.waitForDevices(2)).find((d) => d !== setup.device_id);
  expect(deviceA).toMatch(HEX32);

  await homeTile(page).click();
  await expect(homeTile(page)).toHaveAttribute('aria-current', 'page');
  await expect(dmsNav(page).getByText(copy('shell.dms.empty'), { exact: true })).toBeVisible({ timeout: WAIT });
  await page.getByRole('button', { name: copy('shell.dms.new'), exact: true }).click();
  const picker = page.getByRole('dialog', { name: copy('shell.dms.newTitle'), exact: true });
  await expect(picker).toBeVisible({ timeout: WAIT });
  await expectAccessible(page, 'message someone');
  // Task 20's picker: one <li> per member, holding the shown name and a `shell.dms.open` button.
  await picker.getByRole('listitem').filter({ hasText: shownName(setup) })
    .getByRole('button', { name: copy('shell.dms.open'), exact: true }).click();
  await expect(page).toHaveURL(/\/dm\/[0-9a-f]{32}$/, { timeout: WAIT });
  const dmId = new URL(page.url()).pathname.split('/')[2];
  await expect(dmComposer(page)).toBeEnabled({ timeout: WAIT });
  const dm = await peer.waitForDm(dmId);
  expect(dm.group_id).toMatch(HEX32);

  // Off the DM: back to the channel, then the peer replies.
  await openChannel(page, peer.communityName, channel);
  const reply = `dm reply ${tag()}`;
  await peer.sendDm(dmId, reply);
  await expect(homeTile(page)).toHaveAttribute('data-unread', '1', { timeout: WAIT });
  if (chromium) {
    await expect.poll(() => tagsShown(page), { timeout: WAIT }).toEqual([`dilla:${dmId}`]);
    const [shown] = await notificationsShown(page);
    expect(shown.body).toBe(reply);
    expect([copy('notify.title.dm', { sender: shownName(setup) }), copy('notify.title.dm', { sender: setup.user_id.slice(0, 8) })]).toContain(shown.title);
  }

  await homeTile(page).click();
  const dmRow = dmsNav(page).locator(CLASS.channelRow);
  await expect(dmRow).toHaveCount(1, { timeout: WAIT });
  await expect(dmRow).toHaveAttribute('data-unread', '1', { timeout: WAIT });
  await dmRow.click();
  await expect(page).toHaveURL(new RegExp(`/dm/${dmId}$`), { timeout: WAIT });
  await expect(dmLog(page).locator(CLASS.messageRow).filter({ hasText: reply })).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  await expect(dmRow).toHaveAttribute('data-unread', '0', { timeout: WAIT });

  const answer = `dm answer ${tag()}`;
  await dmComposer(page).fill(answer);
  await dmComposer(page).press('Enter');
  await expect(dmLog(page).locator(CLASS.messageRow).filter({ hasText: answer })).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const got = await peer.waitForDmMessage(dmId, answer);
  expect(got.sender_device).toBe(deviceA);
  expect(got.tier).toBe(1);
});

test('desktop notifications follow the default and the channel on screen', async ({ page, context, peer, instanceName }, testInfo) => {
  test.skip(testInfo.project.name !== 'chromium-web', 'Q30: notifications are proven on Chromium; Firefox asks only from a gesture, WebKit is untestable');
  await recordNotifications(context);

  const setup = await peer.setup({ channels: 3 });
  const [one, two, three] = peer.channelNames;
  const [id1, id2, id3] = setup.channel_ids;
  const title = (channel: string) => copy('notify.title.channel', { channel, server: peer.communityName });
  const onChannel = (id: string) => new RegExp(`/c/${setup.community_id}/${id}$`);
  const account = await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, one);
  await peer.waitForDevices(2, id1);
  await peer.waitForDevices(2, id2);
  await peer.waitForDevices(2, id3);
  const user = await browserUser(page, peer, one);

  // The browser's permission is shown; nothing asked for one.
  const settings = await openSettings(page, 'notifications');
  await context.grantPermissions(['notifications'], { origin: PUBLIC_URL });
  // Signal task 10 fix round 1 (README §5.9): the permission is asked from the Desktop notifications switch.
  const desktop = settings.getByRole('switch', { name: copy('notify.desktop.label'), exact: true });
  await desktop.click();
  await expect(desktop).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
  await expect(settings.getByRole('radio', { name: copy('notify.mode.mentions'), exact: true })).toHaveAttribute('aria-checked', 'true');
  await closeSettings(page);
  await expect(page).toHaveURL(onChannel(id1), { timeout: WAIT });
  expect(await permissionRequests(page)).toBe(1);

  // (a) DMs and mentions: a plain message stays a badge, a mention notifies.
  const plain = `plain ${tag()}`;
  await peer.send(plain, id2);
  await expect(channelRow(page, two)).toHaveAttribute('data-unread', '1', { timeout: WAIT });
  const mention = `<@${user}> look ${tag()}`;
  const mentionShown = mention.replace(`<@${user}>`, `@${account.display}`);
  expect(mentionShown).not.toContain('<@');
  await peer.send(mention, id3);
  await expect.poll(() => tagsShown(page), { timeout: WAIT }).toContain(`dilla:${id3}`);
  expect(await notificationsShown(page)).toEqual([{ title: title(three), body: mentionShown, tag: `dilla:${id3}`, closed: false }]);

  // (b) Everything: the channel on screen stays quiet, another one notifies, and the click opens it.
  await setNotifyDefault(page, 'everything');
  await expect(page).toHaveURL(onChannel(id1), { timeout: WAIT });
  const onScreen = `on screen ${tag()}`;
  await peer.send(onScreen, id1);
  await expect(messageRow(page, one, onScreen)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const second = `plain again ${tag()}`;
  await peer.send(second, id2);
  await expect.poll(() => tagsShown(page), { timeout: WAIT }).toContain(`dilla:${id2}`);
  expect(await notificationsShown(page)).toEqual([
    { title: title(three), body: mentionShown, tag: `dilla:${id3}`, closed: false },
    { title: title(two), body: second, tag: `dilla:${id2}`, closed: false },
  ]);
  await clickNotification(page, 1);
  await expect(page).toHaveURL(onChannel(id2), { timeout: WAIT });
  expect((await notificationsShown(page))[1].closed).toBe(true);
  await expectComposerReady(page, two);

  // (c) Nothing: a mention only counts; the control after switching back proves it raised nothing.
  await setNotifyDefault(page, 'nothing');
  await expect(page).toHaveURL(onChannel(id2), { timeout: WAIT });
  await peer.send(`<@${user}> quiet ${tag()}`, id3);
  await expect(channelRow(page, three)).toHaveAttribute('data-mentions', '2', { timeout: WAIT });
  await setNotifyDefault(page, 'everything');
  const control = `control ${tag()}`;
  await peer.send(control, id1);
  await expect.poll(() => tagsShown(page), { timeout: WAIT }).toContain(`dilla:${id1}`);
  expect((await notificationsShown(page)).map((n) => [n.tag, n.body])).toEqual([
    [`dilla:${id3}`, mentionShown],
    [`dilla:${id2}`, second],
    [`dilla:${id1}`, control],
  ]);
});

test('a direct message the peer opens appears, badges and notifies without a reload', async ({ page, context, peer, instanceName }, testInfo) => {
  const chromium = testInfo.project.name === 'chromium-web';
  if (chromium) {
    await context.grantPermissions(['notifications'], { origin: PUBLIC_URL });
    await recordNotifications(context);
  }
  const setup = await peer.setup();
  await peer.register();
  const channel = peer.channelName;
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, channel);
  await expect(homeTile(page)).not.toHaveAttribute('aria-current', 'page');
  const user = await browserUser(page, peer, channel);

  // The peer opens the DM: it registers the DM's group (null community), the instance proposes the browser's
  // device, and the peer's sync_dm commits that Add before anything is sent, so the browser's Welcome covers it.
  const opened = await peer.openDm(user);
  expect(opened.channelId).toMatch(HEX32);
  expect(opened.groupId).toMatch(HEX32);
  expect(opened.created).toBe(true);
  expect(await peer.waitForDmMembers(opened.channelId, 2)).toBe(2);
  await peer.sendDm(opened.channelId, 'dm from peer');

  // No reload anywhere: the unexpected Welcome reloads the DM list in the worker (L-TS-22 rule 7).
  await expect(homeTile(page)).toHaveAttribute('data-unread', '1', { timeout: WAIT });
  await homeTile(page).click();
  const dmRow = dmsNav(page).locator(CLASS.channelRow);
  await expect(dmRow).toHaveCount(1, { timeout: WAIT });
  await expect(dmRow).toHaveAccessibleName(rowLabel(setup.username, 1, 0), { timeout: WAIT });
  await expect(dmRow).toHaveAttribute('data-unread', '1');
  if (chromium) {
    await expect.poll(() => tagsShown(page), { timeout: WAIT }).toEqual([`dilla:${opened.channelId}`]);
    expect(await notificationsShown(page)).toEqual([
      { title: copy('notify.title.dm', { sender: setup.username }), body: 'dm from peer', tag: `dilla:${opened.channelId}`, closed: false },
    ]);
  }
  await dmRow.click();
  await expect(page).toHaveURL(new RegExp(`/dm/${opened.channelId}$`), { timeout: WAIT });
  await expect(dmLog(page).locator(CLASS.messageRow).filter({ hasText: 'dm from peer' })).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
});
