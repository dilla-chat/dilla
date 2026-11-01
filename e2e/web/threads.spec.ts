// web3.7 E4: public threads from an empty host, through the shipping UI only (flow 11, states 11 and 12). The browser is one
// member; the testkit's web-driver peer is the other, sends thread replies through its `thread_id`, and owns the server. The
// 390 x 844 case is Chromium only (Firefox has no mobile emulation, R23). Its own host on 8503/8504.
import { expect, type Locator, type Page } from '@playwright/test';
import { addServerVia,
  CLASS, COPY, HEX32, TEST_TIMEOUT, WAIT, channelRow, composerBox, copy, deleteDialog, editorBox, expectAccessible,
  logRegion, openChannel, pinsDialog, railItem, railNav, rowById, rowToolbar, sendText, signUp, tag, test, isPeerMessage,
  type PeerMessage, type PeerReceived, type PeerSetup, channelRowMenuItem, expectRowMenuAction, rowAction
} from './support/app';
import { WebDriver } from './support/driver';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const HOST: HostConfig = { port: 8503, control: 8504, webRoot: 'packages/web/dist' };
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 3 } as const;
const GENERAL = 'general';
const SECOND = 'general-2';

let host: StartedHost | null = null;
let instance = '';

test.describe.configure({ mode: 'serial', timeout: TEST_TIMEOUT });
test.use({ baseURL: `http://127.0.0.1:${String(HOST.port)}` });

test.beforeAll(async () => {
  host = await startTestHost(HOST);
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

/** The native peer of this spec's host: the server's owner. Not support/app.ts's `Peer`, which is bound to the suite's host. */
class ThreadPeer {
  readonly inbox: PeerReceived[] = [];
  community = '';
  private constructor(private readonly driver: WebDriver, private readonly seed: number) {}

  static async start(h: StartedHost): Promise<ThreadPeer> {
    const seed = 1 + (Number.parseInt(tag(), 16) % 0xff_fffe);
    const driver = await WebDriver.start(h.url, { DILLA_TESTKIT_INVITE: h.invite, DILLA_TESTKIT_CONTROL: h.control }, seed);
    return new ThreadPeer(driver, seed);
  }

  /** A server with two channels, #general and #general-2; the peer is in both groups. */
  async setup(): Promise<PeerSetup> {
    this.community = `e2e-${this.seed.toString(16)}`;
    const s = await this.driver.setup({ community: this.community, channel: GENERAL, channels: 2 });
    await this.driver.register();
    return s;
  }
  send(body: string, channelId: string, o: { threadId?: string; type?: number; replyTo?: string } = {}): Promise<{ seq: number; msg_id: string }> {
    return this.driver.send(body, channelId, o);
  }
  private async sync(channelId: string): Promise<void> { this.inbox.push(...(await this.driver.sync(channelId)).received); }
  async waitForRow(match: (m: PeerMessage) => boolean, what: string, channelId: string): Promise<PeerMessage> {
    let found: PeerMessage | undefined;
    await expect.poll(async () => {
      await this.sync(channelId);
      found = [...this.inbox].reverse().filter(isPeerMessage).find(match);
      return found !== undefined;
    }, { timeout: WAIT, intervals: [250, 500, 1000, 2000], message: `the peer never saw ${what}` }).toBe(true);
    if (found === undefined) throw new Error(`the peer never saw ${what}`);
    return found;
  }
  waitFor(body: string, channelId: string): Promise<PeerMessage> { return this.waitForRow((m) => m.type === 0 && m.body === body, JSON.stringify(body), channelId); }
  async waitForDevices(count: number, channelId: string): Promise<void> {
    await expect.poll(async () => { await this.sync(channelId); return (await this.driver.members(channelId)).devices.length; },
      { timeout: WAIT, intervals: [250, 500, 1000, 2000] }).toBe(count);
  }
  close(): Promise<void> { return this.driver.close(); }
}

async function join(page: Page, community: string, setup: PeerSetup): Promise<void> {
  await addServerVia(page, 'join');
  const dialog = page.getByRole('dialog', { name: copy(COPY.joinTitle), exact: true });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await dialog.getByLabel(copy(COPY.joinInvite), { exact: true }).fill(setup.invite_code);
  await dialog.getByRole('button', { name: copy(COPY.joinSubmit), exact: true }).click();
  await expect(dialog).toBeHidden({ timeout: WAIT });
  await expect(railItem(page, community)).toBeVisible({ timeout: WAIT });
}

/** The browser (Ada) signs up with the bootstrap invite and joins the peer's (Björn's) server; both channels hold both devices. */
async function enter(page: Page, peer: ThreadPeer, display = 'Ada') {
  const h = started();
  const setup = await peer.setup();
  const account = await signUp(page, h.invite.split(',')[0], instance, display, h.control);
  await join(page, peer.community, setup);
  await openChannel(page, peer.community, GENERAL);
  for (const id of setup.channel_ids) await peer.waitForDevices(2, id);
  return { setup, account, general: setup.channel_ids[0], second: setup.channel_ids[1] };
}

const threadPane = (page: Page) => page.getByRole('complementary', { name: copy('thread.title'), exact: true });
const threadComposer = (page: Page) => page.getByRole('textbox', { name: copy('thread.composer'), exact: true });
const chip = (row: Locator, key: 'thread.summary.one' | 'thread.summary.other', count?: number) =>
  row.getByRole('button', { name: copy(key, count === undefined ? undefined : { count }), exact: true });
const paneRow = (page: Page, text: string) => threadPane(page).locator(CLASS.messageRow).filter({ hasText: text });
const openFromToolbar = async (row: Locator) => {
  await row.focus();
  await rowToolbar(row).getByRole('button', { name: copy('thread.open'), exact: true }).click();
};

test('a reply in a thread shows its count on both sides, the pane sits in the aside, and a reload keeps it', async ({ page }) => {
  const peer = await ThreadPeer.start(started());
  try {
    const { general } = await enter(page, peer);
    await test.step('Ada posts, then opens the thread from the toolbar: the pane is in the aside and takes focus', async () => {
      const plan = `plan ${tag()}`;
      await sendText(page, GENERAL, plan);
      const root = await peer.waitFor(plan, general);
      expect(root.msg_id).toMatch(HEX32);
      const rootRow = rowById(page, GENERAL, root.msg_id);
      await openFromToolbar(rootRow);
      await expect(threadPane(page)).toBeVisible({ timeout: WAIT });
      await expect(page.locator('.d-app-shell')).not.toHaveAttribute('data-layout', 'phone');
      await expect(threadPane(page).getByRole('heading', { name: copy('thread.title'), exact: true })).toBeVisible();
      await expect(paneRow(page, plan)).toHaveCount(1);
      // The aside is a column beside the channel, not on top of it.
      // Measured together and polled: Firefox can report the log before the grid has narrowed it for the new column.
      await expect.poll(async () => {
        const log = await logRegion(page, GENERAL).boundingBox();
        const aside = await threadPane(page).boundingBox();
        return aside!.x - (log!.x + log!.width);
      }, { message: 'the thread pane starts where the channel ends', timeout: WAIT }).toBeGreaterThanOrEqual(-1);
      await expect(threadComposer(page)).toBeFocused();
      await expectAccessible(page, 'thread pane in the aside');

      // Björn replies through the thread id: it shows in the pane and as a chip on the root, never in the channel.
      const ok = `ok ${tag()}`;
      await peer.send(ok, general, { threadId: root.msg_id });
      await expect(paneRow(page, ok)).toBeVisible({ timeout: WAIT });
      await expect(chip(rootRow, 'thread.summary.one')).toBeVisible({ timeout: WAIT });
      await expect(logRegion(page, GENERAL)).not.toContainText(ok);
      // Bjorn's reply is not Ada's: her toolbar offers no edit or delete on it.
      await paneRow(page, ok).focus();
      await expect(rowToolbar(paneRow(page, ok))).toBeVisible();
      await expectRowMenuAction(paneRow(page, ok), 'edit', false);
      await expectRowMenuAction(paneRow(page, ok), 'delete', false);

      // MP7 (N1): a thread row's author name opens the profile card; it is live (not inert) and takes focus, and Escape returns focus to the name.
      const author = paneRow(page, ok).locator('.d-message-row__author-open');
      await author.click();
      const authorCard = page.getByRole('dialog').filter({ has: page.locator('.d-profile-card__close') });
      await expect(authorCard).toBeVisible();
      await expect(authorCard.locator(':focus')).toHaveCount(1);
      await page.keyboard.press('Escape');
      await expect(authorCard).toBeHidden();
      await expect(author).toBeFocused();

      // Ada replies from the pane: Björn receives it, and both counts read 2.
      const go = `go ${tag()}`;
      await threadComposer(page).fill(go);
      await threadComposer(page).press('Enter');
      await expect(paneRow(page, go)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
      const received = await peer.waitFor(go, general);
      expect(received.sender_user, 'the reply is Ada’s own').toBe(root.sender_user);
      await expect(chip(rootRow, 'thread.summary.other', 2)).toBeVisible({ timeout: WAIT });
      await expect(logRegion(page, GENERAL)).not.toContainText(go);

      // Closing from the keyboard-opened chip returns focus to the chip.
      await threadPane(page).getByRole('button', { name: copy('thread.close'), exact: true }).click();
      await expect(threadPane(page)).toBeHidden();
      await chip(rootRow, 'thread.summary.other', 2).focus();
      await page.keyboard.press('Enter');
      await expect(threadPane(page)).toBeVisible({ timeout: WAIT });
      await expect(paneRow(page, ok)).toBeVisible({ timeout: WAIT });
      await threadPane(page).getByRole('button', { name: copy('thread.close'), exact: true }).click();
      await expect(threadPane(page)).toBeHidden();
      await expect(chip(rootRow, 'thread.summary.other', 2)).toBeFocused();

      // A real pointer click on the chip opens it (pointerdown must not move the chip out from under the pointer).
      await chip(rootRow, 'thread.summary.other', 2).click();
      await expect(threadPane(page)).toBeVisible({ timeout: WAIT });
      await expect(paneRow(page, go)).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(threadPane(page)).toBeHidden();

      // Later messages make the root row not the log's tab stop: the case where a pointerdown on the chip focuses a row.
      const later = `later ${tag()}`;
      await peer.send(later, general);
      await expect(logRegion(page, GENERAL)).toContainText(later, { timeout: WAIT });
      // A reload keeps the chip and the thread.
      await page.reload();
      await expect(composerBox(page, GENERAL)).toBeEnabled({ timeout: WAIT });
      const again = rowById(page, GENERAL, root.msg_id);
      await expect(chip(again, 'thread.summary.other', 2)).toBeVisible({ timeout: WAIT });
      await expect(logRegion(page, GENERAL)).not.toContainText(ok);
      await chip(again, 'thread.summary.other', 2).click();
      await expect(paneRow(page, ok)).toBeVisible({ timeout: WAIT });
      await expect(paneRow(page, go)).toBeVisible({ timeout: WAIT });
    });
  } finally { await peer.close(); }
});

test('a mention in a thread badges the channel, and the reply stays out of the channel', async ({ page }) => {
  const peer = await ThreadPeer.start(started());
  try {
    const { general, second } = await enter(page, peer);
    const plan = `plan ${tag()}`;
    await sendText(page, GENERAL, plan);
    const root = await peer.waitFor(plan, general);
    const ada = root.sender_user;
    // Ada sits in the other channel, so the mention in #general's thread has to badge #general's row.
    await channelRow(page, SECOND).click();
    await expect(composerBox(page, SECOND)).toBeEnabled({ timeout: WAIT });
    await expect(channelRow(page, GENERAL)).toHaveAttribute('data-mentions', '0');
    // Control: a mention-free thread reply in a channel Ada is not viewing gives no mention badge.
    await peer.send(`plain ${tag()}`, general, { threadId: root.msg_id });
    await expect(channelRow(page, GENERAL)).toHaveAttribute('data-unread', /[1-9]/, { timeout: WAIT });
    await expect(channelRow(page, GENERAL)).toHaveAttribute('data-mentions', '0');
    const note = `ok ${tag()}`;
    await peer.send(`<@${ada}> ${note}`, general, { threadId: root.msg_id });
    await expect(channelRow(page, GENERAL)).toHaveAttribute('data-mentions', '1', { timeout: WAIT });
    // Control: a mention in the other channel adds to that row, not to #general's.
    await peer.send(`<@${ada}> side ${tag()}`, second);
    await expect(channelRow(page, GENERAL)).toHaveAttribute('data-mentions', '1');
    await channelRow(page, GENERAL).click();
    await expect(composerBox(page, GENERAL)).toBeEnabled({ timeout: WAIT });
    await expect(chip(rowById(page, GENERAL, root.msg_id), 'thread.summary.other', 2)).toBeVisible({ timeout: WAIT });
    await expect(logRegion(page, GENERAL)).not.toContainText(note);
    await expect(channelRow(page, GENERAL)).toHaveAttribute('data-mentions', '0', { timeout: WAIT });
  } finally { await peer.close(); }
});

test('Ada edits and deletes her own reply in the thread', async ({ page }) => {
  const peer = await ThreadPeer.start(started());
  try {
    const { general } = await enter(page, peer);
    const plan = `plan ${tag()}`;
    await sendText(page, GENERAL, plan);
    const root = await peer.waitFor(plan, general);
    await openFromToolbar(rowById(page, GENERAL, root.msg_id));
    await expect(threadComposer(page)).toBeFocused({ timeout: WAIT });
    const first = `first ${tag()}`;
    await threadComposer(page).fill(first);
    await threadComposer(page).press('Enter');
    const mine = paneRow(page, first);
    await expect(mine).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    const sent = await peer.waitFor(first, general);

    await mine.focus();
    await rowAction(mine, 'edit');
    await expect(editorBox(page)).toBeFocused();
    await expect(editorBox(page)).toHaveValue(first);
    const edited = `edited ${tag()}`;
    await editorBox(page).fill(edited);
    await page.keyboard.press('Enter');
    const shown = paneRow(page, edited);
    await expect(shown).toHaveAttribute('data-edited', 'true', { timeout: WAIT });
    await expect(shown).not.toContainText(first);
    await peer.waitForRow((m) => m.type === 1 && m.reply_to === sent.msg_id && m.body === edited, 'the edit of the thread reply', general);

    const byId = threadPane(page).locator(`${CLASS.messageRow}[data-msg-id="${sent.msg_id}"]`);
    await byId.focus();
    await rowAction(byId, 'delete');
    await expect(deleteDialog(page)).toBeVisible();
    await deleteDialog(page).getByRole('button', { name: copy('shell.delete.confirm'), exact: true }).click();
    await expect(byId).toHaveAttribute('data-state', 'deleted', { timeout: WAIT });
    await expect(byId).not.toContainText(edited);
    await peer.waitForRow((m) => m.type === 2 && m.reply_to === sent.msg_id, 'the delete of the thread reply', general);
    // The channel never listed the reply, edited or not, and the root survives.
    await expect(logRegion(page, GENERAL)).not.toContainText(edited);
    await expect(rowById(page, GENERAL, root.msg_id)).toContainText(plan);
  } finally { await peer.close(); }
});

test('a thread opens from a search hit', async ({ page }) => {
  const peer = await ThreadPeer.start(started());
  try {
    const { general } = await enter(page, peer);
    const plan = `plan ${tag()}`;
    await sendText(page, GENERAL, plan);
    const root = await peer.waitFor(plan, general);
    const needle = `quokka${tag()}`;
    await peer.send(`${needle} in a thread`, general, { threadId: root.msg_id });
    const decoy = await peer.send(`decoy ${tag()}`, general);
    await peer.send(`decoyreply ${tag()}`, general, { threadId: decoy.msg_id });
    await expect(chip(rowById(page, GENERAL, root.msg_id), 'thread.summary.one')).toBeVisible({ timeout: WAIT });
    await expect(chip(rowById(page, GENERAL, decoy.msg_id), 'thread.summary.one')).toBeVisible({ timeout: WAIT });
    // The reply is not a row of the channel, so a search hit cannot focus it there: it opens the thread.
    await expect(threadPane(page)).toHaveCount(0);
    await page.keyboard.press('ControlOrMeta+K');
    const search = page.getByRole('combobox', { name: copy('search.placeholder') });
    await search.fill(needle);
    const hit = page.getByRole('option', { name: new RegExp(needle) });
    await expect(hit).toBeVisible({ timeout: WAIT });
    await hit.click();
    await expect(threadPane(page)).toBeVisible({ timeout: WAIT });
    await expect(paneRow(page, needle)).toBeVisible({ timeout: WAIT });
    await expect(paneRow(page, plan)).toBeVisible();
    await expect(threadPane(page).getByText(/decoy/)).toHaveCount(0);
  } finally { await peer.close(); }
});

test('a thread opens from the pin of a reply', async ({ page }) => {
  const peer = await ThreadPeer.start(started());
  try {
    const { general } = await enter(page, peer);
    const plan = `plan ${tag()}`;
    await sendText(page, GENERAL, plan);
    const root = await peer.waitFor(plan, general);
    const needle = `quokka${tag()}`;
    const reply = await peer.send(`${needle} in a thread`, general, { threadId: root.msg_id });
    const decoy = await peer.send(`decoy ${tag()}`, general);
    await peer.send(`decoyreply ${tag()}`, general, { threadId: decoy.msg_id });
    await expect(chip(rowById(page, GENERAL, root.msg_id), 'thread.summary.one')).toBeVisible({ timeout: WAIT });
    await expect(chip(rowById(page, GENERAL, decoy.msg_id), 'thread.summary.one')).toBeVisible({ timeout: WAIT });
    // A pin of the reply: the pins dialog's jump opens the thread too.
    await peer.send('', general, { type: 5, replyTo: reply.msg_id });
    await channelRowMenuItem(page, GENERAL, 'shell.pins.open');
    const pins = pinsDialog(page, GENERAL);
    await expect(pins).toBeVisible({ timeout: WAIT });
    await expect(pins).toContainText(needle, { timeout: WAIT });
    await pins.getByRole('button', { name: copy('shell.pins.jump'), exact: true }).click();
    await expect(pins).toBeHidden();
    await expect(threadPane(page)).toBeVisible({ timeout: WAIT });
    await expect(paneRow(page, needle)).toBeVisible({ timeout: WAIT });
    await expect(paneRow(page, plan)).toBeVisible();
    await expect(threadPane(page).getByText(/decoy/)).toHaveCount(0);
  } finally { await peer.close(); }
});

test('on a phone the thread is a full-screen sheet, the system back closes it and the channel remains', async ({ browser, guard }) => {
  test.skip(test.info().project.name !== 'chromium-web', 'Chromium only: no mobile emulation elsewhere');
  const peer = await ThreadPeer.start(started());
  const context = await browser.newContext({ ...PHONE, baseURL: `http://127.0.0.1:${String(HOST.port)}` });
  await guard.attach(context);
  const page = await context.newPage();
  try {
    const { general } = await enter(page, peer, 'Björn');
    // Messages sent after Björn joined; the peer starts the thread.
    const plan = `plan ${tag()}`;
    const root = await peer.send(plan, general);
    const row = rowById(page, GENERAL, root.msg_id);
    await expect(row).toContainText(plan, { timeout: WAIT });
    await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/);
    const channelUrl = page.url();
    await openFromToolbar(row);
    const sheet = threadPane(page);
    await expect(sheet).toBeVisible({ timeout: WAIT });
    const box = await sheet.boundingBox();
    expect(box!.x, 'the sheet starts at the left edge').toBeLessThanOrEqual(1);
    expect(box!.width, 'the sheet fills the width').toBeGreaterThanOrEqual(388);
    // Task 11 (Jonas, Q4): the sheet covers the whole screen, the tab bar included (the prototype's phone thread).
    expect(box!.y + box!.height, 'the sheet reaches the bottom of the screen').toBeGreaterThanOrEqual(840);
    // Fix round 2: the tab bar under it is covered (the sheet is what a tap there hits) and inert (out of the tab order).
    const bar = page.locator('.d-app-shell__tabbar');
    const barBox = (await bar.boundingBox())!;
    expect(await page.evaluate(([x, y]) => document.elementFromPoint(x, y)?.closest('.d-app-shell__aside') !== null,
      [barBox.x + barBox.width / 2, barBox.y + barBox.height / 2]), 'the sheet covers the tab bar').toBe(true);
    await expect(bar, 'the covered tab bar is inert').toHaveAttribute('inert', '');
    // The sheet is an AppShell overlay: it takes focus to its first control, the back button (flow 11 says the composer; see the report).
    await expect(sheet.getByRole('button', { name: copy('thread.close'), exact: true })).toBeFocused();
    // Task 7: the textarea sits in the box's input row over the footer, so the composer (its form) is measured.
    const composer = await threadComposer(page).locator('xpath=ancestor::form[1]').boundingBox();
    // Task 7: the Signal composer keeps the prototype's 10px bottom padding on a phone, so its box ends within 16px of the sheet's end.
    expect(composer!.y + composer!.height, 'the thread composer sits at the bottom of the sheet').toBeGreaterThanOrEqual(box!.y + box!.height - 16);
    const ok = `ok ${tag()}`;
    await peer.send(ok, general, { threadId: root.msg_id });
    await expect(sheet.getByText(ok)).toBeVisible({ timeout: WAIT });
    await expectAccessible(page, 'thread sheet on a phone');
    await page.goBack();
    await expect(sheet).toBeHidden({ timeout: WAIT });
    expect(page.url(), 'back closes the sheet and the channel view remains').toBe(channelUrl);
    await expect(composerBox(page, GENERAL)).toBeVisible();
    await expect(chip(row, 'thread.summary.one')).toBeVisible({ timeout: WAIT });
    await expect.poll(() => row.evaluate((r) => r.contains(document.activeElement)), { timeout: WAIT, message: 'focus returns to the opener row' }).toBe(true);
    // A tap on the chip opens it again.
    await chip(row, 'thread.summary.one').tap();
    await expect(sheet).toBeVisible({ timeout: WAIT });
    await page.goBack();
    await expect(sheet).toBeHidden({ timeout: WAIT });
    await expect.poll(() => row.evaluate((r) => r.contains(document.activeElement)), { timeout: WAIT, message: 'focus returns to the opener row' }).toBe(true);
  } finally {
    await context.close();
    await peer.close();
  }
});
