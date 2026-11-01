// v0.1.0-web3.1, the regression test of the create-flow incident: a fresh instance could not be used from the web client,
// and every suite hid it by seeding its servers through dilla-testhost. This suite starts a test host of its own with no
// account, no server and no channel, and gets from there to two people talking using the shipping client alone: the
// bootstrap invite, "new server", "invite link", a sign-up through that link, a reply, a direct message through the picker,
// and a second channel both of them use. Nothing here seeds state or calls the instance's API on anyone's behalf: the one
// request the spec makes itself is GET /v1/instance, for the name the onboarding shows.
// The create flow's controls are found by role and accessible name (flow 07's copy keys in en.ts) only.
import { expect, type Locator, type Page } from '@playwright/test';
import {
  TEST_TIMEOUT, WAIT, completeSignUp, composerBox, copy, dmsNav, expectAccessible, expectComposerReady, channelRow, homeTile,
  messageRow, newAccount, newChannelButton, openChannel, openInviteDialog, railItem, railNav, sendText, signUp, tag, test,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { installWorkerPort, portSlice } from './support/worker-port';
import type { ChannelSummary } from '../../packages/client-core/src/state/types';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { rmSync } from 'node:fs';

/** Its own ports: the suite's shared host (8463/8464) has servers in it by now. */
const EMPTY: HostConfig = { port: 8477, control: 8478, webRoot: 'packages/web/dist' };
const SERVER = 'Midgard Crew';
const OWNER = 'Ada';
const FRIEND = 'Björn';
/** The task's exact values (task-4 brief, step 2). */
const HELLO = 'hello from the owner';
const SECOND_CHANNEL = 'plans';
/** The whole-branch walk's long names (I2): a typed sentence, stored as its slug, and 100 bytes in one word. */
const LONG_TYPED = 'Fika på fredag och veckans planering';
const LONG_SLUG = 'fika-på-fredag-och-veckans-planering';
const HUNDRED_BYTES = 'midsommar-'.repeat(10);

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
const dmLog = (page: Page, name: string) => page.getByRole('log', { name: copy('shell.dm.log', { name }), exact: true });
const dmRow = (page: Page, name: string, text: string) => dmLog(page, name).locator('.d-message-row').filter({ hasText: text });

test('from an empty instance: start a server, invite a friend through a link, reply, DM and add a channel', async ({
  page, context, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = started();
  await installWorkerPort(context);

  // 1. The first person on the instance: the bootstrap invite, then a shell that says how to begin.
  // The host prints its instance invites comma-separated; the first is the one an operator would hand on.
  await signUp(page, h.invite.split(',')[0], instance, OWNER, h.control);
  const empty = page.getByRole('region', { name: copy('shell.noServer.title'), exact: true });
  await expect(empty).toBeVisible({ timeout: WAIT });
  // The control main's client lacked (dbe836b): the spec fails here against it.
  const newServer = railNav(page).getByRole('button', { name: copy('layout.rail.add'), exact: true });
  const chooseCreate = () => page.getByRole('dialog', { name: copy('layout.add.title'), exact: true })
    .getByRole('button', { name: copy('layout.add.create'), exact: true }).click();
  await expect(newServer).toBeVisible();
  await expect(button(empty, 'shell.noServer.create')).toBeVisible();
  await expect(button(empty, 'shell.noServer.action')).toBeVisible();
  await expectAccessible(page, 'a fresh instance, no server');

  // Flow 07: a dialog takes focus in on open and gives it back to its opener on Escape (a real browser's focus, which
  // the ui suite's jsdom could only stub).
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  const serverName = start.getByRole('textbox', { name: copy('create.server.name'), exact: true });
  await newServer.focus();
  await page.keyboard.press('Enter');
  await chooseCreate();
  await expect(serverName).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(start).toBeHidden();
  await expect(newServer).toBeFocused();
  // The same for the join flow opened from the add dialog: Escape returns focus to the "+" tile.
  await newServer.focus();
  await page.keyboard.press('Enter');
  await page.getByRole('dialog', { name: copy('layout.add.title'), exact: true })
    .getByRole('button', { name: copy('layout.add.join'), exact: true }).click();
  const joinDialog = page.getByRole('dialog', { name: copy('join.title'), exact: true });
  await expect(joinDialog).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(joinDialog).toBeHidden();
  await expect(newServer).toBeFocused();

  // 2. "new server", a name, and #general opens with focus in its composer. The server also has a voice general.
  await newServer.click();
  await chooseCreate();
  await expect(serverName).toBeFocused();
  await expectAccessible(page, 'start a server');
  await page.keyboard.type(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
  const serverId = new URL(page.url()).pathname.split('/')[2] ?? '';
  await expect(railItem(page, SERVER)).toHaveAttribute('aria-current', 'page');
  await expect(button(page, 'voice.open', { name: 'general' })).toBeVisible();
  await expectComposerReady(page, 'general');
  await expect(composerBox(page, 'general')).toBeFocused();
  await sendText(page, 'general', HELLO);

  // 3. The invite link, read from the dialog's field. The shell tries to copy it as the dialog opens; Firefox may refuse a
  // copy that a network round trip separated from the click, and the dialog then says so. Either way the link stands in
  // the field, which is what a person copies by hand, so the clipboard is never read here. An answer slower than a second is
  // not copied by itself at all (flow 07, "invite: answer slow"): the status stays empty and the link waits in the field
  // with an enabled "copy link". So the dialog must end in one of three states: copied, blocked, or (after the second in
  // which a copy by itself would have spoken) the link with an enabled "copy link" and nothing said.
  // web3.8 (item 11): the link is made under server settings → Invites.
  // Signal task 10c: the link shows once in the Invites card, with Copy; no dialog opens and nothing is copied by itself.
  const inviteCard = await openInviteDialog(page, SERVER);
  const linkField = inviteCard.getByRole('textbox', { name: copy('serverSettings.invites.link'), exact: true });
  const LINK = /^https?:\/\/[^/\s]+\/i\/[^/\s]+$/;
  await expect(linkField).toBeVisible({ timeout: WAIT });
  await expect.poll(() => linkField.inputValue(), { timeout: WAIT }).toMatch(LINK);
  await expect(button(inviteCard, 'serverSettings.invites.copy')).toBeEnabled();
  const link = await linkField.inputValue();
  expect(link).toMatch(LINK);
  await expectAccessible(page, 'the invite link');
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog', { name: copy('admin.title'), exact: true })).toBeHidden();

  // 4. The second person, in a browser of their own, through that link: the instance's invite page, its "open dilla"
  // link, then the sign-up, which lands them in the server. The link names the instance's public domain; this host
  // serves it on loopback, so its path is opened here.
  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, secondDir);
  try {
    await installWorkerPort(second);
    const page2 = second.pages()[0] ?? (await second.newPage());
    await page2.goto(new URL(link).pathname);
    const openLink = page2.getByRole('link', { name: 'Open dilla in this browser', exact: true });
    await expect(openLink).toHaveAttribute('href', /^\/welcome\?invite=[^&\s]+$/);
    // The guard watches the web client from its first page on. The invite page before it is the instance's static HTML
    // under `default-src 'none'`: Firefox asks it for /favicon.ico on its own and reports the refusal, which is that CSP
    // working, not the client's (it is not this suite's to judge).
    await guard.attach(second);
    await openLink.click();
    await completeSignUp(page2, newAccount(FRIEND, h.control), instance);

    // 5. The second person lands in the server, with #general (review focus 5). "hello from the owner" went out before
    // their browser was in #general's MLS group, so it is not readable here: the log says so instead of looking empty
    // (shell.log.empty, flow 03 "messages from before this browser joined"). The conversation they see starts at their
    // join: the owner's next message reaches them, and their reply reaches the owner.
    await expect(railItem(page2, SERVER)).toBeVisible({ timeout: WAIT });
    await openChannel(page2, SERVER, 'general');
    const generalLog = page2.getByRole('log', { name: copy('shell.log.label', { channel: 'general' }), exact: true });
    await expect(generalLog).toContainText(copy('shell.log.empty'));
    await expect(messageRow(page2, 'general', HELLO)).toHaveCount(0);
    const welcome = `welcome to ${SERVER} ${tag()}`;
    await sendText(page, 'general', welcome);
    await expect(messageRow(page2, 'general', welcome)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    const reply = `thanks for the link ${tag()}`;
    await sendText(page2, 'general', reply);
    await expect(messageRow(page, 'general', reply)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // 6. A direct message to the owner, through "message someone".
    await homeTile(page2).click();
    await button(page2, 'shell.dms.new').click();
    const picker = page2.getByRole('dialog', { name: copy('shell.dms.newTitle'), exact: true });
    const pick = picker.getByRole('button', { name: copy('shell.dms.open'), exact: true });
    await expect(pick).toHaveCount(1);
    await expect(pick).toHaveAccessibleDescription(OWNER);
    await pick.click();
    await expect(page2).toHaveURL(/\/dm\/[0-9a-f]{32}$/, { timeout: WAIT });
    const dmBox = page2.getByRole('textbox', { name: copy('shell.dm.composer', { name: OWNER }), exact: true });
    await expect(dmBox).toBeEnabled({ timeout: WAIT });
    const dmText = `just between us ${tag()}`;
    await dmBox.fill(dmText);
    await dmBox.press('Enter');
    await expect(dmRow(page2, OWNER, dmText)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    await homeTile(page).click();
    await dmsNav(page).getByRole('button', { name: new RegExp(`^${FRIEND}(, |$)`) }).click();
    await expect(dmRow(page, FRIEND, dmText)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // 7. The owner adds a text channel and both use it. The second person's open tab, standing on the server, learns of
    // it from op 54 community_changed within about a second: no reload, no focus, and well inside the 15 s focus throttle.
    await railItem(page2, SERVER).click();
    await expect(channelRow(page2, 'general')).toBeVisible({ timeout: WAIT });
    await railItem(page, SERVER).click();
    await newChannelButton(page).click();
    const newChannel = page.getByRole('dialog', { name: copy('create.channel.title', { server: SERVER }), exact: true });
    await expect(newChannel.getByRole('textbox', { name: copy('create.channel.name'), exact: true })).toBeFocused();
    const kind = newChannel.getByRole('radiogroup', { name: copy('create.channel.kind'), exact: true });
    await expect(kind.getByRole('radio', { name: copy('create.channel.text'), exact: true })).toBeChecked();
    await expectAccessible(page, 'new channel');
    await page.keyboard.type(SECOND_CHANNEL);
    await page.keyboard.press('Enter');
    await expect(newChannel).toBeHidden({ timeout: WAIT });
    await expectComposerReady(page, SECOND_CHANNEL);
    const plansId = new URL(page.url()).pathname.split('/')[3] ?? '';
    await expect(channelRow(page2, SECOND_CHANNEL)).toBeVisible({ timeout: 5_000 });
    // The sidebar update can precede the MLS Welcome. Send only after Björn's
    // browser has joined the new group, or the first message is unrecoverable there.
    await expect.poll(async () => (await portSlice<ChannelSummary[]>(page2, `channels:${serverId}`))
      ?.find((c) => c.id === plansId)?.group, { timeout: WAIT }).toBe('active');
    await openChannel(page2, SERVER, SECOND_CHANNEL);
    const inPlans = `the first words in #${SECOND_CHANNEL} ${tag()}`;
    await sendText(page, SECOND_CHANNEL, inPlans);
    await expect(messageRow(page2, SECOND_CHANNEL, inPlans)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    const answer = `and an answer in #${SECOND_CHANNEL} ${tag()}`;
    await sendText(page2, SECOND_CHANNEL, answer);
    await expect(messageRow(page, SECOND_CHANNEL, answer)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // 8. The second person is a member, not the owner: "server settings" yes (it opens on Invites), the list's "+" and the
    // sidebar's old "invite link" no.
    await expect(button(page2, 'admin.open')).toBeVisible();
    await expect(newChannelButton(page2)).toHaveCount(0);
    await expect(button(page2, 'invite.open')).toHaveCount(0);

    // 9. The whole-branch walk. The owner's actions (server settings in the header, the "+" in the channel list; web3.8) are
    // all visible and none is clipped: each fits inside its row and the group fits the 240 px sidebar (flow 07); a name
    // the server already has for that kind is refused before any request (walk M2); a channel the owner just made says
    // only "nothing here yet." (walk M1); and a long name never widens the page (walk I2): at 1280 "pinned" stays on
    // screen, at 360 the page is no wider than the viewport.
    // WS8: the header holds only the server name and its settings gear (no text buttons, no actions group).
    await expect(page.getByRole('group', { name: copy('create.actions'), exact: true })).toHaveCount(0);
    const gear = button(page, 'admin.open');
    await expect(gear).toBeVisible();
    await expect(newChannelButton(page)).toBeVisible();
    const gearFit = await gear.evaluate(b => {
      const r = b.getBoundingClientRect();
      const sidebar = b.closest('.d-app-shell__sidebar')?.getBoundingClientRect();
      return { left: r.left, right: r.right, width: r.width, clipped: b.scrollWidth > b.clientWidth + 1,
        sidebar: sidebar === undefined ? null : { left: sidebar.left, right: sidebar.right, width: sidebar.width } };
    });
    // Signal §5.1: the sidebar's default width is the prototype's 244 (--sidebar-w).
    expect(gearFit.sidebar?.width ?? Infinity, `the sidebar is 244 px (${JSON.stringify(gearFit.sidebar)})`).toBeCloseTo(244, 0);
    expect(gearFit.left).toBeGreaterThanOrEqual((gearFit.sidebar?.left ?? 0) - 0.5);
    expect(gearFit.right).toBeLessThanOrEqual((gearFit.sidebar?.right ?? 0) + 0.5);
    expect(gearFit.width).toBeGreaterThanOrEqual(24);
    expect(gearFit.clipped, 'the gear shows its whole icon').toBe(false);
    // The list's "+" fits inside the sidebar too, unclipped (it replaced the old header buttons).
    const plusFit = await newChannelButton(page).evaluate(b => {
      const r = b.getBoundingClientRect();
      const sidebar = b.closest('.d-app-shell__sidebar')?.getBoundingClientRect();
      return { left: r.left, right: r.right, width: r.width, clipped: b.scrollWidth > b.clientWidth + 1,
        sidebar: sidebar === undefined ? null : { left: sidebar.left, right: sidebar.right } };
    });
    expect(plusFit.sidebar, 'the "+" is inside the sidebar').not.toBeNull();
    expect(plusFit.left).toBeGreaterThanOrEqual((plusFit.sidebar?.left ?? 0) - 0.5);
    expect(plusFit.right).toBeLessThanOrEqual((plusFit.sidebar?.right ?? 0) + 0.5);
    expect(plusFit.width).toBeGreaterThanOrEqual(24);
    expect(plusFit.clipped, 'the "+" shows its whole icon').toBe(false);
    const nameField = newChannel.getByRole('textbox', { name: copy('create.channel.name'), exact: true });
    await newChannelButton(page).click();
    await nameField.fill(SECOND_CHANNEL);
    await nameField.press('Enter');
    await expect(newChannel.getByRole('alert')).toHaveText(copy('create.channel.error.takenText', { name: SECOND_CHANNEL }));
    for (const [typed, slug] of [[LONG_TYPED, LONG_SLUG], [HUNDRED_BYTES, HUNDRED_BYTES]] as const) {
      if (!(await newChannel.isVisible())) await newChannelButton(page).click();
      await nameField.fill(typed);
      await nameField.press('Enter');
      await expect(newChannel).toBeHidden({ timeout: WAIT });
      await expectComposerReady(page, slug);
      const log = page.getByRole('log', { name: copy('shell.log.label', { channel: slug }), exact: true });
      await expect(log).toContainText(copy('shell.log.emptyMade'));
      await expect(log).not.toContainText(copy('shell.log.empty'));
      // Ruling X: the header's Members button (the last header control; pins moved to the row's context menu) stands in the viewport.
      await expect(page.locator('.d-channel-header [data-members-toggle]')).toBeInViewport({ ratio: 1 });
      const size = page.viewportSize();
      await page.setViewportSize({ width: 360, height: 740 });
      try {
        await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth), { message: `${slug} at 360 px` })
          .toBeLessThanOrEqual(360);
      } finally {
        if (size !== null) await page.setViewportSize(size);
      }
    }
    await guard.collectWorkers();
  } finally {
    await second.close();
    rmSync(secondDir, { recursive: true, force: true });
  }
});
