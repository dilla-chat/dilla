// W-Shell-B: threads in the aside (one aside) and as a phone sheet, slow mode in the composer, the channel rows (flow 11).
import { afterEach, describe, expect, it, vi } from 'vitest';
import { rowMenuItem } from '../test/row-menu.ts';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { PERM, type ChannelSummary, type CommunityAccess, type MemberSummary, type RoleSummary, type TimelineItem } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient } from '../test/fake-client.ts';
import { account, ME, timelineItem } from '../test/fixtures.ts';
import { PHONE_QUERY } from '../layout.ts';
import { en, formatTime, t } from '../strings/index.ts';
import { Shell } from './Shell.tsx';
import { ComposerArea } from './conversation/ComposerArea.tsx';
import { replaceHistoryState } from '../router.ts';

// Every composer renders as before; the spy records the props the shell (and the thread view) hand it.
vi.mock('./conversation/ComposerArea.tsx', async importOriginal => {
  const actual = await importOriginal<typeof import('./conversation/ComposerArea.tsx')>();
  return { ...actual, ComposerArea: vi.fn(actual.ComposerArea) };
});

const A = 'a1'.repeat(16);
const GEN = 'c3'.repeat(16);
const NEWS = 'd4'.repeat(16);
const SLOW = 'd5'.repeat(16);
const OWNER = 'ee'.repeat(16);
const BOB = '28'.repeat(16);
const EVERYONE = 'ab'.repeat(16);
const ROOT = '71'.repeat(16);
const ROOT2 = '72'.repeat(16);
const ch = (id: string, name: string, position: number, slowmode = 0): ChannelSummary =>
  ({ id, communityId: A, kind: 0, mode: 0, name, topic: '', parentId: null, position, slowmode, announcement: false, group: 'none' });
const role = (allow: number): RoleSummary =>
  ({ id: EVERYONE, name: '@everyone', color: 0, position: 0, hoist: false, mentionable: true, allow, deny: 0, members: 0 });
const BASE = PERM.viewChannel | PERM.sendMessages | PERM.readHistory;
const MEMBERS: MemberSummary[] = [
  { userId: ME.id, username: 'ada', display: 'Ada L', kind: 0, roleIds: [] },
  { userId: BOB, username: 'bob', display: 'Bob', kind: 0, roleIds: [] },
];
const access = (): CommunityAccess => ({ top: 5, owner: false, manageCommunity: false, manageChannels: false,
  kickMembers: false, banMembers: false, createInvite: false, manageRoles: false, loaded: true, overwritesLoaded: true, everyoneId: EVERYONE });
const rootRow = (id: string, seq: string, body: string, count: number): TimelineItem =>
  timelineItem({ key: `s${seq}`, msgId: id, seq, body, senderUser: BOB, senderDevice: BOB, ts: 1_700_000_000, thread: { count, lastTs: 1_700_000_100 } });

const WIDTH = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetWidth');
// jsdom has no layout: a wide shell needs a width for the aside to be a column (AppShell measures its root).
function media(phone: boolean) {
  Object.defineProperty(HTMLElement.prototype, 'offsetWidth', { configurable: true, get: () => (phone ? 390 : 1600) });
  window.matchMedia = vi.fn((query: string) => ({
    matches: query === PHONE_QUERY ? phone : !phone && query === '(min-width: 64rem)', media: query,
    addEventListener: () => undefined, removeEventListener: () => undefined,
  })) as unknown as typeof window.matchMedia;
}
afterEach(() => {
  if (WIDTH !== undefined) Object.defineProperty(HTMLElement.prototype, 'offsetWidth', WIDTH);
  Reflect.deleteProperty(window, 'matchMedia');
  // eslint-disable-next-line no-restricted-properties -- clears the tab memory the shell writes
  window.sessionStorage.clear();
  vi.restoreAllMocks();
});

function setup(o: { phone?: boolean; base?: number; path?: string; timeout?: string | null } = {}) {
  media(o.phone === true);
  replaceHistoryState(null, '', o.path ?? `/c/${A}/${GEN}`);
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('connection', { status: 'online', generation: '7' });
  fake.set('communities', [{ id: A, name: 'Midgard', owner: OWNER }]);
  fake.set(`channels:${A}`, [ch(GEN, 'general', 0), ch(NEWS, 'news', 1), ch(SLOW, 'slowpoke', 2, 30)]);
  fake.set(`members:${A}`, MEMBERS);
  fake.set(`roles:${A}`, [role(o.base ?? BASE)]);
  fake.set(`overwrites:${A}`, { [NEWS]: [{ kind: 0, targetId: EVERYONE, allow: 0, deny: PERM.sendMessages }] });
  fake.set(`permissions:${A}`, access());
  fake.set('removals', []);
  fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false, items: [rootRow(ROOT, '1', 'the first root', 2)] });
  fake.set(`timeline:${NEWS}`, { channelId: NEWS, group: 'active', items: [], hasEarlier: false });
  fake.set(`timeline:${SLOW}`, { channelId: SLOW, group: 'active', items: [], hasEarlier: false });
  fake.set(`threads:${GEN}`, [
    { rootMsgId: ROOT, rootSeq: '1', count: 2, lastTs: 5, excerpt: 'the first root' },
    { rootMsgId: ROOT2, rootSeq: '4', count: 1, lastTs: 9, excerpt: 'the second root' },
  ]);
  fake.set(`thread:${GEN}`, { channelId: GEN, rootMsgId: ROOT, root: rootRow(ROOT, '1', 'the first root', 2), items: [], hasEarlier: false });
  if (o.timeout !== undefined) fake.handler = command => command.m === 'getMemberTimeout' ? Promise.resolve(o.timeout) : Promise.resolve(null);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Shell /></CoreProvider></div>);
  return { fake, user, view };
}
const members = () => screen.queryByRole('complementary', { name: 'members of Midgard' });
const threadPane = () => screen.queryByRole('complementary', { name: en['thread.title'] });
const listPane = () => screen.queryByRole('complementary', { name: en['thread.list'] });
const toggle = () => screen.getByRole('button', { name: /^Members/ });
const wait = (fake: FakeClient) => fake.set(`slow:${GEN}`, { until: Date.now() + 60_000, queued: false });

describe('threads on a desktop shell (one aside)', () => {
  it('replaces the member list while a thread is open, labels the aside thread.title, and restores the members on close', async () => {
    const { fake, user } = setup();
    expect(await screen.findByRole('complementary', { name: 'members of Midgard' })).toBeInTheDocument();
    expect(toggle()).toHaveAttribute('aria-pressed', 'true');
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
    expect(members()).toBeNull();
    expect(document.querySelectorAll('aside')).toHaveLength(1);
    expect(fake.callsOf('openThread')).toEqual([{ m: 'openThread', channelId: GEN, rootMsgId: ROOT }]);
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(threadPane()).toBeNull());
    expect(await screen.findByRole('complementary', { name: 'members of Midgard' })).toBeInTheDocument();
    expect(toggle()).toHaveAttribute('aria-pressed', 'true');
    expect(fake.callsOf('closeThread')).toHaveLength(1);
  });

  it('keeps the main log and composer first in the DOM, the thread pane after them', async () => {
    const { user } = setup();
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    const logs = document.querySelectorAll('[role="log"]');
    expect(logs).toHaveLength(2);
    expect(logs[0].closest('main')).not.toBeNull();
    expect(logs[1].closest('aside')).not.toBeNull();
    const composers = document.querySelectorAll('.d-composer textarea');
    expect(composers[0].closest('main')).not.toBeNull();
    expect(composers[1].closest('aside')).not.toBeNull();
  });

  it('closes the thread when the members toggle is pressed, and shows the members', async () => {
    const { user } = setup();
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    expect(toggle()).toHaveAttribute('aria-pressed', 'false');
    await user.click(toggle());
    await waitFor(() => expect(threadPane()).toBeNull());
    expect(members()).not.toBeNull();
  });

  it('closes the thread on a channel switch, and does not bring it back on return', async () => {
    const { user } = setup();
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    await user.click(screen.getByRole('button', { name: 'news' }));
    await waitFor(() => expect(threadPane()).toBeNull());
    await user.click(await screen.findByRole('button', { name: 'general' }));
    await screen.findByRole('button', { name: '2 replies' });
    expect(threadPane()).toBeNull();
  });

  it('opens a thread from the toolbar item of a message', async () => {
    const { user } = setup();
    const message = (await screen.findByText('the first root')).closest<HTMLElement>('.d-message-row')!;
    message.focus();
    await user.click(within(message).getByRole('button', { name: en['thread.open'] }));
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
  });

  it('opens a thread from a reply quote or a pin that carries threadRoot', async () => {
    const { fake, user } = setup();
    const quoting = timelineItem({ key: 's9', msgId: '90'.repeat(16), seq: '9', body: 'quoting', senderUser: BOB, senderDevice: BOB, ts: 1_700_000_200,
      reply: { msgId: ROOT2, state: 'ok', senderUser: BOB, excerpt: 'the second root', seq: '4', threadRoot: ROOT } });
    act(() => fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false, items: [rootRow(ROOT, '1', 'the first root', 2), quoting] }));
    const line = (await screen.findByText('quoting')).closest<HTMLElement>('.d-message-row')!;
    await user.click(within(line).getByRole('button', { name: /the second root/ }));
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
    expect(fake.callsOf('openThread').at(-1)).toEqual({ m: 'openThread', channelId: GEN, rootMsgId: ROOT });
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(threadPane()).toBeNull());
    act(() => fake.set(`pins:${GEN}`, [{ msgId: ROOT2, seq: '4', senderUser: BOB, excerpt: 'the second root', pinnedBy: BOB, pinnedSeq: '5', ts: 1_700_000_000, threadRoot: ROOT }]));
    await rowMenuItem(user, 'general', en['shell.pins.open']);
    await user.click(await screen.findByRole('button', { name: en['shell.pins.jump'] }));
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
    expect(fake.callsOf('openThread')).toHaveLength(2);
  });

  it('opens a thread from a search hit that carries threadRoot', async () => {
    const { fake, user } = setup();
    await screen.findByRole('button', { name: '2 replies' });
    await user.keyboard('{Control>}k{/Control}');
    await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'reply');
    act(() => fake.set('search', { requestId: 4, hits: [{ channelId: GEN, communityId: A, seq: '4', snippet: 'a reply', rank: '1', threadRoot: ROOT }], cursor: null }));
    const hit = await screen.findByRole('option', { name: /a reply/ });
    await user.click(hit);
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
    expect(fake.callsOf('openThread').at(-1)).toEqual({ m: 'openThread', channelId: GEN, rootMsgId: ROOT });
  });
});

describe('the thread list', () => {
  it('opens from the header button, lists the threads, opens one in its place, and Escape closes the list returning focus to the button', async () => {
    const { fake, user } = setup();
    await user.click(await screen.findByRole('button', { name: en['layout.header.threads'] }));
    const aside = await screen.findByRole('complementary', { name: en['thread.list'] });
    expect(members()).toBeNull();
    expect(fake.callsOf('loadThreads')).toEqual([{ m: 'loadThreads', channelId: GEN }]);
    await user.click(within(aside).getByRole('button', { name: /the second root/ }));
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
    expect(fake.callsOf('openThread').at(-1)).toEqual({ m: 'openThread', channelId: GEN, rootMsgId: ROOT2 });
    expect(listPane()).toBeNull();
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(threadPane()).toBeNull());
    await user.click(screen.getByRole('button', { name: en['layout.header.threads'] }));
    await screen.findByRole('complementary', { name: en['thread.list'] });
    await user.keyboard('{Escape}');
    await waitFor(() => expect(listPane()).toBeNull());
    expect(screen.getByRole('button', { name: en['layout.header.threads'] })).toHaveFocus();
  });
});

describe('a profile card from a thread row (MP7, N1)', () => {
  const withMention = (fake: FakeClient) => fake.set(`thread:${GEN}`, { channelId: GEN, rootMsgId: ROOT, root: rootRow(ROOT, '1', 'the first root', 2), hasEarlier: false,
    items: [timelineItem({ key: 's2', msgId: '73'.repeat(16), seq: '2', body: `hey <@${BOB}>`, senderUser: BOB, senderDevice: BOB, ts: 1_700_000_050 })] });
  it('on a phone the card sits outside the inert scopes, holds focus, and Escape returns focus to the pill in the sheet', async () => {
    const { fake, user } = setup({ phone: true });
    withMention(fake);
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    const sheet = await screen.findByRole('complementary', { name: en['thread.title'] });
    const pill = await within(sheet).findByRole('button', { name: '@Bob' });
    await user.click(pill);
    const card = await screen.findByRole('dialog', { name: 'Bob' });
    expect(card.closest('[inert]')).toBeNull();
    expect(card.contains(document.activeElement)).toBe(true);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Bob' })).toBeNull();
    expect(pill).toHaveFocus();
    expect(threadPane()).not.toBeNull();
  });
  it('a card action works from a thread row (message opens the DM)', async () => {
    const { fake, user } = setup({ phone: true });
    withMention(fake);
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    const sheet = await screen.findByRole('complementary', { name: en['thread.title'] });
    await user.click(await within(sheet).findByRole('button', { name: '@Bob' }));
    await user.click(within(await screen.findByRole('dialog', { name: 'Bob' })).getByRole('button', { name: 'Message' }));
    expect(screen.queryByRole('dialog', { name: 'Bob' })).toBeNull();
  });
});

describe('threads on a phone (a sheet)', () => {
  it('opens full screen as a history entry, closes on the system back, keeps the tab bar and returns focus to the root row', async () => {
    const { fake, user } = setup({ phone: true });
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    const sheet = await screen.findByRole('complementary', { name: en['thread.title'] });
    expect(sheet.querySelector('.d-thread-pane')).toHaveAttribute('data-phone', 'true');
    expect(window.history.state).toMatchObject({ phoneSheet: 'thread' });
    expect(within(sheet).getAllByRole('button', { name: en['thread.close'] })).toHaveLength(1);
    expect(screen.getByRole('navigation', { name: t('nav.tabs.label') })).toBeInTheDocument();
    act(() => { window.history.back(); });
    await waitFor(() => expect(threadPane()).toBeNull());
    expect(fake.callsOf('closeThread')).toHaveLength(1);
    // A pointer click leaves focus where it was (the chip keeps still), so focus comes back to the root's row.
    await waitFor(() => expect(document.querySelector(`.d-message-row[data-msg-id="${ROOT}"]`)).toHaveFocus());
  });

  it('closes on Escape and by its own back control, popping its entry', async () => {
    const { user } = setup({ phone: true });
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    const length = window.history.length;
    await user.keyboard('{Escape}');
    await waitFor(() => expect(threadPane()).toBeNull());
    expect(window.history.length).toBe(length);
    expect((window.history.state as { phoneSheet?: unknown } | null)?.phoneSheet).toBeUndefined();
    await user.click(screen.getByRole('button', { name: '2 replies' }));
    await user.click(await screen.findByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(threadPane()).toBeNull());
    expect(window.history.length).toBe(length);
  });

  it('opens the thread list from the channel menu as a sheet, and a row opens its thread in the same entry', async () => {
    const { user } = setup({ phone: true });
    // Signal (Ruling U): Threads is a header button on a phone too.
    await user.click(await screen.findByRole('button', { name: en['layout.header.threads'] }));
    const sheet = await screen.findByRole('complementary', { name: en['thread.list'] });
    expect(window.history.state).toMatchObject({ phoneSheet: 'thread' });
    const length = window.history.length;
    await user.click(within(sheet).getByRole('button', { name: /the first root/ }));
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
    expect(listPane()).toBeNull();
    expect(window.history.length).toBe(length);
    act(() => { window.history.back(); });
    await waitFor(() => expect(threadPane()).toBeNull());
  });
});

describe('slow mode in the composer', () => {
  it('shows the countdown from the slow:<channel> slice', async () => {
    const { fake } = setup();
    await screen.findByRole('textbox', { name: 'message #general' });
    expect(document.querySelector('.d-composer__notice')).toBeNull();
    act(() => wait(fake));
    expect(document.querySelector('.d-composer__notice')).toHaveTextContent(/Slow mode is on/);
  });

  it('shows no countdown for a person with the bypass bit (bit 19)', async () => {
    const { fake } = setup({ base: BASE | PERM.bypassSlowmode });
    await screen.findByRole('textbox', { name: 'message #general' });
    act(() => wait(fake));
    expect(document.querySelector('.d-composer__notice')).toBeNull();
  });

  it('speaks a queued send through the shell announcer', async () => {
    const { fake } = setup();
    await screen.findByRole('textbox', { name: 'message #general' });
    const note = screen.getByTestId('shell-announcer');
    act(() => fake.set(`slow:${GEN}`, { until: Date.now() + 60_000, queued: true }));
    await waitFor(() => expect(note).toHaveTextContent(en['slow.queued']));
  });

  it('says only some people can post in an announcement channel', async () => {
    setup({ path: `/c/${A}/${NEWS}` });
    expect(await screen.findAllByText(en['announce.readOnly'].replace('{name}', 'news'))).toHaveLength(1);
  });
});

describe('the channel rows', () => {
  it('lists an announcement channel with the announcement kind and a slow channel with the badge', async () => {
    setup();
    const row = (name: string) => screen.getByText(name, { selector: '.d-chrow__name' }).closest<HTMLElement>('.d-chrow')!;
    await screen.findByText('news', { selector: '.d-chrow__name' });
    expect(row('news')).toHaveAttribute('data-kind', 'announcement');
    expect(row('slowpoke')).toHaveAttribute('data-kind', 'text');
    expect(within(row('slowpoke')).getByText(en['slow.badge'])).toBeInTheDocument();
    expect(row('general')).toHaveAttribute('data-kind', 'text');
    expect(row('general').querySelector('.d-chrow__badge')).toBeNull();
    expect(row('news').querySelector('.d-chrow__badge')).toBeNull();
  });
});

const rowOfRoot = () => document.querySelector<HTMLElement>(`.d-message-row[data-msg-id="${ROOT}"]`);
const threadBox = () => screen.getByRole('textbox', { name: en['thread.composer'] });
const pinFor = (fake: FakeClient) => act(() => fake.set(`pins:${GEN}`, [{ msgId: ROOT2, seq: '4', senderUser: BOB, excerpt: 'the second root', pinnedBy: BOB, pinnedSeq: '5', ts: 1_700_000_000, threadRoot: ROOT }]));
async function searchThread(fake: FakeClient, user: ReturnType<typeof userEvent.setup>) {
  await screen.findByRole('button', { name: '2 replies' });
  await user.keyboard('{Control>}k{/Control}');
  await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'reply');
  act(() => fake.set('search', { requestId: 4, hits: [{ channelId: GEN, communityId: A, seq: '4', snippet: 'a reply', rank: '1', threadRoot: ROOT }], cursor: null }));
  await user.click(await screen.findByRole('option', { name: /a reply/ }));
  return screen.findByRole('complementary', { name: en['thread.title'] });
}

describe('where focus lands (fix round 2)', () => {
  it('lands in the thread composer when a thread is picked from the list', async () => {
    const { user } = setup();
    await user.click(await screen.findByRole('button', { name: en['layout.header.threads'] }));
    const aside = await screen.findByRole('complementary', { name: en['thread.list'] });
    await user.click(within(aside).getByRole('button', { name: /the second root/ }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    await waitFor(() => expect(threadBox()).toHaveFocus());
  });

  it('returns to the root row when a thread opened from a search hit closes', async () => {
    const { fake, user } = setup();
    await searchThread(fake, user);
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(threadPane()).toBeNull());
    await waitFor(() => expect(rowOfRoot()).toHaveFocus());
  });

  it('returns to the opening message when a pointer opened the thread with the main composer focused', async () => {
    const { user } = setup();
    const box = await screen.findByRole('textbox', { name: 'message #general' });
    box.focus();
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(threadPane()).toBeNull());
    await waitFor(() => expect(rowOfRoot()).toHaveFocus());
  });

  it('returns to the root row on a desktop when a thread opened from a pin closes', async () => {
    const { fake, user } = setup();
    await screen.findByRole('button', { name: '2 replies' });
    pinFor(fake);
    await rowMenuItem(user, 'general', en['shell.pins.open']);
    await user.click(await screen.findByRole('button', { name: en['shell.pins.jump'] }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(threadPane()).toBeNull());
    await waitFor(() => expect(rowOfRoot()).toHaveFocus());
  });
});

describe('phone: a thread from a search hit, a pin and a quote', () => {
  it('opens from a search hit, replacing an open members sheet so it does not come back', async () => {
    const { fake, user } = setup({ phone: true });
    await user.click(await screen.findByRole('button', { name: /^Members/ }));
    await screen.findByRole('complementary', { name: 'members of Midgard' });
    await searchThread(fake, user);
    expect(members()).toBeNull();
    act(() => { window.history.back(); });
    await waitFor(() => expect(threadPane()).toBeNull());
    expect(members()).toBeNull();
    expect((window.history.state as { phoneSheet?: unknown } | null)?.phoneSheet).toBeUndefined();
  });

  it('opens a thread sheet from a search hit', async () => {
    const { fake, user } = setup({ phone: true });
    const sheet = await searchThread(fake, user);
    expect(sheet.querySelector('.d-thread-pane')).toHaveAttribute('data-phone', 'true');
    expect(window.history.state).toMatchObject({ phoneSheet: 'thread' });
  });

  // Ruling X: on a phone the pins are in the channel row's context menu (a long press; here its right-click twin) on the list.
  it('opens a thread sheet from a pin', async () => {
    const { fake, user } = setup({ phone: true, path: `/c/${A}` });
    pinFor(fake);
    await rowMenuItem(user, 'general', en['shell.pins.open']);
    await screen.findByRole('button', { name: '2 replies' });
    await user.click(await screen.findByRole('button', { name: en['shell.pins.jump'] }));
    const sheet = await screen.findByRole('complementary', { name: en['thread.title'] });
    expect(sheet.querySelector('.d-thread-pane')).toHaveAttribute('data-phone', 'true');
    expect(window.history.state).toMatchObject({ phoneSheet: 'thread' });
  });

  it('opens a thread sheet from a reply quote', async () => {
    const { fake, user } = setup({ phone: true });
    const quoting = timelineItem({ key: 's9', msgId: '90'.repeat(16), seq: '9', body: 'quoting', senderUser: BOB, senderDevice: BOB, ts: 1_700_000_200,
      reply: { msgId: ROOT2, state: 'ok', senderUser: BOB, excerpt: 'the second root', seq: '4', threadRoot: ROOT } });
    act(() => fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false, items: [rootRow(ROOT, '1', 'the first root', 2), quoting] }));
    const line = (await screen.findByText('quoting')).closest<HTMLElement>('.d-message-row')!;
    await user.click(within(line).getByRole('button', { name: /the second root/ }));
    const sheet = await screen.findByRole('complementary', { name: en['thread.title'] });
    expect(sheet.querySelector('.d-thread-pane')).toHaveAttribute('data-phone', 'true');
    expect(window.history.state).toMatchObject({ phoneSheet: 'thread' });
  });
});

describe('the shell wires slow mode into the composers', () => {
  const channelComposer = () => {
    const calls = vi.mocked(ComposerArea).mock.calls.map(c => c[0]).filter(p => p.channelId === GEN && p.threadId === undefined);
    return calls.at(-1)!;
  };
  it('hands the channel composer the slice, the bypass bit, the announcement flag and the announcer', async () => {
    const { fake } = setup({ base: BASE | PERM.bypassSlowmode });
    await screen.findByRole('textbox', { name: 'message #general' });
    const slice = { until: Date.now() + 60_000, queued: true };
    act(() => fake.set(`slow:${GEN}`, slice));
    expect(channelComposer().slow).toEqual(slice);
    expect(channelComposer().bypass).toBe(true);
    expect(channelComposer().announcement).toBe(false);
    expect(typeof channelComposer().announce).toBe('function');
  });
  it('says bypass false without the bit, and announcement true for an announcement channel', async () => {
    setup({ path: `/c/${A}/${NEWS}` });
    await screen.findByRole('textbox', { name: 'message #news' });
    const last = vi.mocked(ComposerArea).mock.calls.map(c => c[0]).filter(p => p.channelId === NEWS).at(-1)!;
    expect(last.bypass).toBe(false);
    expect(last.announcement).toBe(true);
  });
  it('counts down in the thread composer too', async () => {
    const { fake, user } = setup();
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    const aside = await screen.findByRole('complementary', { name: en['thread.title'] });
    expect(aside.querySelector('.d-composer__notice')).toBeNull();
    act(() => wait(fake));
    expect(aside.querySelector('.d-composer__notice')).toHaveTextContent(/Slow mode is on/);
  });
  it('passes the community timeout to both channel and thread composers', async () => {
    const expires = Math.floor(Date.now() / 1000) + 60;
    const { fake, user } = setup({ timeout: String(expires) });
    const reason = `You cannot post messages in this community until ${formatTime(expires)}.`;
    await user.click(await screen.findByRole('button', { name: '2 replies' }));
    await screen.findByRole('complementary', { name: en['thread.title'] });
    await waitFor(() => {
      const composers = vi.mocked(ComposerArea).mock.calls.map(c => c[0]).filter(p => p.channelId === GEN);
      expect(composers.some(p => p.threadId === undefined && p.blocked === reason)).toBe(true);
      expect(composers.some(p => p.threadId !== undefined && p.blocked === reason)).toBe(true);
    });
    expect(fake.callsOf('getMemberTimeout')).toEqual([{ m: 'getMemberTimeout', communityId: A, userId: ME.id }]);
  });
});

void render;

describe('the channel header of an announcement channel (walk 17)', () => {
  it('shows the megaphone in place of # for an announcement channel, and # for a plain one', async () => {
    setup({ path: `/c/${A}/${NEWS}` });
    const head = await screen.findByRole('heading', { level: 2, name: 'news' });
    // web3.9 (C3): the megaphone is a sibling of the name inside the title, not inside the heading.
    const title = head.closest('.d-channel-header__title')!;
    expect(title.querySelector('[data-icon="megaphone"]')).not.toBeNull();
    expect(head.querySelector('[data-icon="megaphone"]')).toBeNull();
    expect(title.textContent).toBe('news');
    cleanup();
    setup({ path: `/c/${A}/${GEN}` });
    const plain = await screen.findByRole('heading', { level: 2, name: 'general' });
    expect(plain.closest('.d-channel-header__title')!.querySelector('[data-icon="megaphone"]')).toBeNull();
    expect(plain.closest('.d-channel-header__title')!.textContent).toBe('general');
    expect(plain.closest('.d-channel-header__title')!.querySelector('[data-icon="hash"]')).not.toBeNull();
  });
});
