// v0.1.0-web3.7, slow mode and announcement channels from an empty instance. Like admin.spec.ts this suite starts a test host of
// its own with no account, no server and no channel, and runs everything through the shipping clients: two browsers, one owner
// and one member. Nothing here seeds state or calls the instance's API on anyone's behalf (GET /v1/instance for the name the
// onboarding shows, and the host's clock on the control port, are the only requests the spec makes itself).
//
// Two posts go around the UI, through Björn's own core worker (ruling R27, support/worker-port.ts): a post into the announcement
// channel he may not post in, which the host refuses with 403, and the same post relabelled as class 1 on its way out (the
// lying sender), which the host cannot see and the receiver's core drops.
import { expect, type Locator, type Page } from '@playwright/test';
import { addServerVia,
  CLASS, TEST_TIMEOUT, WAIT, makeInviteLink, newChannelButton, completeSignUp, copy, escapeRegExp, expectAccessible, expectComposerReady, newAccount, openChannel,
  railItem, railNav, channelRow, composerBox, messageRow, logRegion, reactionChip, rowAction, sendText, signUp, test, channelRowMenuItem,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { launchSecond, secondProfileDir } from './support/second-harness';
import { holdUploads, installWorkerPort, portCall, portSlice } from './support/worker-port';
import type { AccountState, ChannelSummary, SearchState, TimelineState } from '../../packages/client-core/src/state/types';
import { arr, decode } from '../../packages/client-core/src/cbor/index';
import { rmSync } from 'node:fs';

/** Its own ports (E3 reservation). */
const EMPTY: HostConfig = { port: 8501, control: 8502, webRoot: 'packages/web/dist' };
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

function started(): StartedHost {
  if (host === null) throw new Error('the empty test host did not start');
  return host;
}
const button = (scope: Page | Locator, key: string, vars?: Record<string, string>) =>
  scope.getByRole('button', { name: copy(key, vars), exact: true });
const dialogOf = (page: Page, key: string, vars?: Record<string, string>) =>
  page.getByRole('dialog', { name: copy(key, vars), exact: true });

/** Moves the host's fake clock forward (POST /debug/clock on the control port; the helper in support/app.ts names port 8464). */
async function advanceHostClock(h: StartedHost, seconds: number): Promise<void> {
  const res = await fetch(`${h.control}/debug/clock`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
  });
  expect(res.status, await res.text()).toBe(204);
}
/** The slow-mode notice with its time left open: `slow.wait` with any time in it (the visible, ticking copy; the status beside it speaks it). */
function waitNotice(page: Page): Locator {
  const [pre, post] = copy('slow.wait', { time: '\u0001' }).split('\u0001');
  return page.locator('[aria-hidden="true"]').getByText(new RegExp(`^${escapeRegExp(pre ?? '')}.+${escapeRegExp(post ?? '')}$`));
}
const channelIdOf = (page: Page): string => /\/([0-9a-f]{32})$/.exec(new URL(page.url()).pathname)?.[1] ?? '';
/** What every item of a channel's timeline slice holds, whatever its state (an unreadable placeholder counts), as `state body`. */
async function itemsIn(page: Page, channelId: string): Promise<string[]> {
  const slice = await portSlice<TimelineState>(page, `timeline:${channelId}`);
  return (slice?.items ?? []).map((i) => `${i.state} ${i.body}`);
}

test('slow mode and an announcement channel from an empty instance: the wait, the queue, the read-only notice, a refused post and a lying class', async ({
  page, context, guard, playwright, browserName, launchOptions, headless, channel, viewport,
}, testInfo) => {
  const h = started();
  await installWorkerPort(context);

  // 1. Ada signs up with the bootstrap invite and starts "Midgard Crew"; Björn joins through her invite.
  await signUp(page, h.invite.split(',')[0], instance, OWNER, h.control);
  await addServerVia(page, 'create');
  const start = dialogOf(page, 'create.server.title');
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
  const generalId = channelIdOf(page);
  const serverId = new URL(page.url()).pathname.split('/')[2] ?? '';
  expect(generalId).not.toBe('');
  expect(serverId).toMatch(/^[0-9a-f]{32}$/);
  await expectComposerReady(page, 'general');

  // web3.8 (item 11): the link is made under server settings → Invites.
  const link = await makeInviteLink(page, SERVER);

  const secondDir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
  const second = await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL: h.url }, secondDir);
  try {
    await installWorkerPort(second);
    const uploads = await holdUploads(second);
    const page2 = second.pages()[0] ?? (await second.newPage());
    await page2.goto(new URL(link).pathname);
    const openLink = page2.getByRole('link', { name: 'Open dilla in this browser', exact: true });
    await expect(openLink).toHaveAttribute('href', /^\/welcome\?invite=[^&\s]+$/);
    await guard.attach(second);
    await openLink.click();
    await completeSignUp(page2, newAccount(FRIEND, h.control), instance);
    await expect(railItem(page2, SERVER)).toBeVisible({ timeout: WAIT });
    await openChannel(page2, SERVER, 'general');

    // 2. Ada opens #general's settings and sets slow mode to 10 seconds.
    await channelRowMenuItem(page, 'general', 'admin.channel.open');
    const edit = dialogOf(page, 'admin.channel.titleText', { name: 'general' });
    const select = edit.getByRole('combobox', { name: copy('slow.label'), exact: true });
    await expect(select).toBeVisible({ timeout: WAIT });
    await select.selectOption({ label: copy('slow.s10') });
    await expectAccessible(page, 'channel settings with the slow mode select');
    await button(edit, 'admin.channel.save').click();
    await expect(edit).toBeHidden({ timeout: WAIT });

    await expect(channelRow(page2, 'general')).toHaveAccessibleName(new RegExp(escapeRegExp(copy('slow.badge'))), { timeout: WAIT });

    // 3. Björn sends "one": it arrives on Ada's side, and his composer counts down from 10 seconds.
    const noticeOf = waitNotice(page2);
    await expect(noticeOf).toHaveCount(0);
    await sendText(page2, 'general', 'one');
    await expect(messageRow(page, 'general', 'one')).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    await expect(noticeOf).toBeVisible({ timeout: WAIT });
    const shown = (await noticeOf.textContent()) ?? '';
    const [pre, post] = copy('slow.wait', { time: '\u0001' }).split('\u0001');
    expect(shown.slice((pre ?? '').length, shown.length - (post ?? '').length), 'the time left in the notice').toMatch(/^0:(10|0\d)$/);
    // Ada is the owner and bypasses slow mode: her composer shows no wait.
    await expect(waitNotice(page)).toHaveCount(0);

    // 4. Björn sends "two" at once: it queues, does not reach Ada within 2 s, and arrives once the wait is over.
    const box = composerBox(page2, 'general');
    await box.fill('two');
    await box.press('Enter');
    await expect(page2.getByText(copy('slow.queued'), { exact: false }).first()).toBeVisible({ timeout: WAIT });
    await expect(messageRow(page2, 'general', 'two')).toHaveAttribute('data-state', 'pending');
    await page.waitForTimeout(2_000);
    await expect(messageRow(page, 'general', 'two')).toHaveCount(0);
    await advanceHostClock(h, 10);
    await expect(messageRow(page, 'general', 'two')).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    await expect(messageRow(page2, 'general', 'two')).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // 5. Ada sends twice in a row with no countdown, and Björn gets both.
    await sendText(page, 'general', 'ada one');
    await sendText(page, 'general', 'ada two');
    await expect(waitNotice(page)).toHaveCount(0);
    await expect(messageRow(page2, 'general', 'ada two')).toHaveAttribute('data-state', 'ok', { timeout: WAIT });

    // 6. Ada starts the announcement channel "news" with no role ticked; Björn reads it, cannot post, and can react.
    await newChannelButton(page).click();
    const create = dialogOf(page, 'create.channel.title', { server: SERVER });
    await create.getByRole('radiogroup', { name: copy('create.channel.kind'), exact: true })
      .getByRole('radio', { name: copy('announce.kind'), exact: true }).click();
    await create.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill('news');
    await button(create, 'create.channel.submit').click();
    await expect(create).toBeHidden({ timeout: WAIT });
    await expectComposerReady(page, 'news');
    const newsId = channelIdOf(page);
    expect(newsId).not.toBe('');
    await expect(channelRow(page2, 'news')).toBeVisible({ timeout: WAIT });
    // The row can arrive before the MLS Welcome. Opening it during that gap can
    // race the creator's commit and show E_COMMIT_INVALID instead of read-only mode.
    await expect.poll(async () => (await portSlice<ChannelSummary[]>(page2, `channels:${serverId}`))
      ?.find((c) => c.id === newsId)?.group, { timeout: WAIT }).toBe('active');
    await channelRow(page2, 'news').click();
    await expect(page2.getByText(copy('announce.readOnly', { name: 'news' }), { exact: true }).first()).toBeVisible({ timeout: WAIT });
    await expect(composerBox(page2, 'news')).toBeDisabled();
    await expectAccessible(page2, 'the announcement channel as a member');
    await sendText(page, 'news', 'headline');
    const headline = messageRow(page2, 'news', 'headline');
    await expect(headline).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    await rowAction(headline, 'react');
    const grid = page2.getByRole('dialog', { name: copy('shell.emoji.label'), exact: true });
    await grid.getByRole('button', { name: copy('shell.emoji.01'), exact: true }).click();
    await expect(grid).toBeHidden();
    await expect(reactionChip(headline, copy('shell.emoji.01'), 1)).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });
    const onAdasSide = messageRow(page, 'news', 'headline');
    await expect(reactionChip(onAdasSide, copy('shell.emoji.01'), 1)).toHaveAttribute('aria-pressed', 'false', { timeout: WAIT });

    // 7. A post crafted outside the UI (R27), through Björn's own core worker. The host refuses it: 403, so the row fails with
    // E_FORBIDDEN and Ada never sees it. The control, the same call into #general (where he may post), arrives on Ada's side.
    await channelRow(page, 'general').click();
    await expectComposerReady(page, 'general');
    const crafted = await portCall(page2, { m: 'send', channelId: newsId, text: 'crafted' });
    expect(crafted.ok, 'the worker takes the send').toBe(true);
    await expect.poll(async () => {
      const slice = await portSlice<TimelineState>(page2, `timeline:${newsId}`);
      const item = slice?.items.find((i) => i.body === 'crafted');
      return item === undefined ? 'absent' : `${item.state} ${item.reason}`;
    }, { timeout: WAIT }).toBe('failed E_FORBIDDEN');
    await expect.poll(() => uploads.seen.at(-1), { timeout: WAIT, message: 'the host answered the upload' }).toEqual({ status: 403, relabelled: false });
    await advanceHostClock(h, 10);
    // The worker sends into a channel it has open; #general is not the one Björn's page shows, so the call opens it first.
    expect((await portCall(page2, { m: 'openChannel', channelId: generalId })).error ?? null).toBeNull();
    const control = await portCall(page2, { m: 'send', channelId: generalId, text: 'crafted control' });
    expect(control.error ?? null, 'the worker takes the control send').toBeNull();
    await expect(messageRow(page, 'general', 'crafted control')).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
    await expect.poll(() => uploads.seen.at(-1), { timeout: WAIT, message: 'the control upload went through' }).toEqual({ status: 200, relabelled: false });

    // 8. The lying sender: the same post, with its class byte turned from 0 (a post) to 1 (a reaction, pin or edit), so that the
    // host checks nothing. It answers 200. Ada's core finds that the class disagrees with what it decrypts and drops the row:
    // her timeline, her #news badge and mentions and her search stay as they were. The lie reaches her twice: live (op 19), and
    // by catch-up while her page is away (the messages list, with the class as its 9th element). Björn's reaction after each
    // lie is the marker: the core takes a group's rows in order, so once it shows on the headline the lie has been taken.
    // The lie mentions Ada, so that a row that slipped through would also count a mention.
    const adaId = (await portSlice<AccountState>(page, 'account'))?.user?.id ?? '';
    expect(adaId).toMatch(/^[0-9a-f]{32}$/);
    const lieSent = async (text: string): Promise<void> => {
      await advanceHostClock(h, 10);
      const before = uploads.seen.length;
      uploads.relabel = true;
      const lie = await portCall(page2, { m: 'send', channelId: newsId, text: `${text} <@${adaId}>` });
      expect(lie.error ?? null, 'the worker takes the lying send').toBeNull();
      await expect.poll(() => uploads.seen.length, { timeout: WAIT }).toBeGreaterThan(before);
      uploads.relabel = false;
      expect(uploads.seen.at(-1), 'the host cannot tell and accepts it').toEqual({ status: 200, relabelled: true });
    };
    const markerSent = async (emojiKey: string): Promise<void> => {
      await rowAction(headline, 'react');
      await grid.getByRole('button', { name: copy(emojiKey), exact: true }).click();
      await expect(grid).toBeHidden();
      await expect(reactionChip(headline, copy(emojiKey), 1)).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });
    };
    const markerSeen = async (p: Page, reactions: number): Promise<void> => {
      await portCall(p, { m: 'openChannel', channelId: newsId });
      await expect.poll(async () => {
        const slice = await portSlice<TimelineState>(p, `timeline:${newsId}`);
        return slice?.items.find((i) => i.body === 'headline')?.reactions.length ?? 0;
      }, { timeout: WAIT }).toBe(reactions);
    };
    /** Ada's side shows nothing of the lies: every item of the timeline, both badges, and a search (beside one that hits). */
    const nothingOfTheLies = async (p: Page, when: string): Promise<void> => {
      const row = channelRow(p, 'news');
      expect.soft(await row.getAttribute('data-unread'), `Ada's #news unread badge ${when}`).toBe('0');
      expect.soft(await row.getAttribute('data-mentions'), `Ada's #news mention badge ${when}`).toBe('0');
      expect.soft(await itemsIn(p, newsId), `every item of Ada's timeline of #news ${when}`).toEqual(['ok headline']);
      const found = async (query: string): Promise<number> => {
        const reply = await portCall(p, { m: 'search', scope: 'channel', id: newsId, query, cursor: null });
        expect(reply.error ?? null, `search for ${query}`).toBeNull();
        return (await portSlice<SearchState>(p, 'search'))?.hits.length ?? -1;
      };
      expect.soft(await found('headline'), `a search for the headline finds it ${when}`).toBe(1);
      expect.soft(await found('relabelled'), `a search for the lie finds nothing ${when}`).toBe(0);
    };

    await expect(channelRow(page, 'news')).toHaveAttribute('data-unread', '0');
    await lieSent('relabelled');
    await markerSent('shell.emoji.02');
    await markerSeen(page, 2);
    await nothingOfTheLies(page, 'live');

    // The same after a reload: Ada's core has the dropped row in its store and reads it again.
    await page.reload();
    await expect(railItem(page, SERVER)).toBeVisible({ timeout: WAIT });
    await expectComposerReady(page, 'general');
    await markerSeen(page, 2);
    await nothingOfTheLies(page, 'after a reload');
    await channelRow(page, 'news').click();
    await expect.soft(logRegion(page, 'news').locator(CLASS.messageRow)).toHaveCount(1);
    await expect.soft(messageRow(page, 'news', 'relabelled')).toHaveCount(0);
    await expect.soft(messageRow(page, 'news', 'crafted')).toHaveCount(0);

    // And by catch-up: Ada's page is away while a second lie and its marker are uploaded, then she comes back and her core reads
    // both from the host's message list, where the class is the 9th element of the row.
    const away = new URL(page.url()).pathname.replace(/[0-9a-f]{32}$/, generalId);
    await page.goto('about:blank');
    await lieSent('relabelled again');
    await markerSent('shell.emoji.03');
    await page.goto(away);
    await expect(railItem(page, SERVER)).toBeVisible({ timeout: WAIT });
    await expectComposerReady(page, 'general');
    await markerSeen(page, 3);
    await nothingOfTheLies(page, 'after catch-up');
    await channelRow(page, 'news').click();
    await expect.soft(logRegion(page, 'news').locator(CLASS.messageRow)).toHaveCount(1);
    await expect.soft(messageRow(page, 'news', 'relabelled')).toHaveCount(0);

    // The control for the badge: with Ada in #news, Björn's honest post in #general badges #general for her.
    await advanceHostClock(h, 10);
    await channelRow(page2, 'general').click();
    await expectComposerReady(page2, 'general');
    await sendText(page2, 'general', 'honest');
    await expect(channelRow(page, 'general')).toHaveAttribute('data-unread', '1', { timeout: WAIT });

    // 9. The pages stay accessible: channel settings with the select was checked at step 2, #news as a member at step 6.
    await expectAccessible(page, 'the announcement channel as its owner');
    await guard.collectWorkers();
  } finally {
    await second.close();
    rmSync(secondDir, { recursive: true, force: true });
  }
});
