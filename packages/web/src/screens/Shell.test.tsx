import { afterEach, beforeEach, describe, it, expect, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ChannelSummary, DmSummary, MemberSummary, TimelineItem, TimelineState } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account, ME, timelineItem } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { openRowMenu } from '../test/row-menu.ts';
import { formatTime } from '../strings/index.ts';
import { en } from '../strings/en.ts';
import { joinErrorState, replaceHistoryState } from '../router.ts';
import { browser } from '../browser.ts';
import { Shell } from './Shell.tsx';
import type { ShellUpdateDeps } from './Shell.tsx';
import { toolbarButton } from '../test/toolbar.ts';

const A = 'a1'.repeat(16);
const B = 'b2'.repeat(16);
const VOICE = 'e5'.repeat(16);
const GEN = 'c3'.repeat(16);
const CAT = '17'.repeat(16);
const RAND = 'd4'.repeat(16);
const READ = 'f6'.repeat(16);
const PEER = '28'.repeat(16);
const PEER_DEV = '29'.repeat(16);
const BOT = '39'.repeat(16);
const STRANGER = '4a'.repeat(16);
const NOW = Math.floor(Date.now() / 1000);

function ch(id: string, name: string, over: Partial<ChannelSummary> = {}): ChannelSummary {
  return { id, communityId: A, kind: 0, mode: 0, name, topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'none', ...over };
}
const CHANNELS: ChannelSummary[] = [
  ch(VOICE, 'lounge', { kind: 1, position: 0, group: 'unsupported' }),
  ch(GEN, 'general', { position: 1, topic: 'say hi' }),
  ch(CAT, 'more', { kind: 2, position: 2, group: 'unsupported' }),
  ch(RAND, 'random', { parentId: CAT, position: 0 }),
  ch(READ, 'lobby', { parentId: CAT, position: 1, mode: 1, group: 'unsupported' }),
];
const MEMBERS: MemberSummary[] = [
  { userId: ME.id, username: 'ada', display: 'Ada L', kind: 0, roleIds: [] },
  { userId: PEER, username: 'bob', display: '', kind: 0, roleIds: [] },
  { userId: BOT, username: 'helper', display: 'Helper', kind: 1, roleIds: [] },
];
function item(over: Partial<TimelineItem> & { key: string }): TimelineItem {
  return timelineItem({ senderUser: PEER, senderDevice: PEER_DEV, ts: NOW, ...over });
}
function timeline(over: Partial<TimelineState> = {}): TimelineState {
  return { channelId: GEN, group: 'active', items: [], hasEarlier: false, ...over };
}
// `state` is the history entry's state (task 23 carries a refused join in it, pre-flight ruling (g)).
function setup(path: string, opts: { channels?: boolean; communities?: { id: string; name: string; owner: string }[]; state?: unknown; before?(fake: FakeClient): void;
  update?: ShellUpdateDeps } = {}) {
  replaceHistoryState(opts.state ?? null, '', path);
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('connection', { status: 'online', generation: '7' });
  fake.set('communities', opts.communities ?? [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }, { id: B, name: 'Valhalla', owner: 'ee'.repeat(16) }]);
  if (opts.channels !== false) fake.set(`channels:${A}`, CHANNELS);
  fake.set(`afk:${A}`, { channelId: null, timeoutSeconds: 300, pending: false });
  fake.set(`members:${A}`, MEMBERS);
  opts.before?.(fake);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Shell update={opts.update} /></CoreProvider></div>);
  return { fake, user, view };
}
const path = () => window.location.pathname;

/** The rail's "+" opens the add dialog; its choice (join a server, new server) opens the existing flow. */
async function addServer(user: ReturnType<typeof userEvent.setup>, choice: 'join a server' | 'new server') {
  await user.click(within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: 'add a server' }));
  await user.click(within(screen.getByRole('dialog', { name: 'add a server' })).getByRole('button', { name: choice }));
}
/** The `@` tile: the rail's first item, direct messages and Home. */
const homeTile = () => within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: /^direct messages/ });
const composer = (name = 'general') => screen.getByRole('textbox', { name: `message #${name}` });
const sendButton = () => screen.getByRole('button', { name: 'send' });

it('checkpoints an unsent draft before refreshing to a new UI build', async () => {
  let changed!: (entry: string) => void;
  const save = vi.fn<(...args: [unknown, unknown, readonly File[]]) => Promise<void>>().mockResolvedValue();
  const reload = vi.fn();
  const update: ShellUpdateDeps = { currentEntry: '/assets/old.js',
    watch: options => { changed = entry => options.changed(entry); return () => undefined; },
    store: { save, load: () => Promise.resolve(null), remove: () => Promise.resolve() }, reload };
  const { fake, user } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  fake.handler = command => command.m === 'checkpointFiles' ? Promise.resolve([]) : Promise.resolve(null);
  await user.type(composer(), 'keep me');
  act(() => changed('/assets/new.js'));
  await waitFor(() => expect(reload).toHaveBeenCalledOnce());
  expect(fake.callsOf('checkpointFiles')).toContainEqual({ m: 'checkpointFiles', channelId: GEN });
  expect(save.mock.calls[0]?.[1]).toMatchObject({ conversationId: GEN, draft: { channelId: GEN, text: 'keep me' } });
});

it('keeps the reminder inbox entry mounted from DM and root routes without a selected community', async () => {
  const { user, view } = setup('/', { communities: [] });
  expect(await screen.findByRole('button', { name: 'Reminders' })).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'Reminders' }));
  expect(await screen.findByRole('dialog', { name: 'Reminders' })).toBeInTheDocument();
  view.unmount();
  replaceHistoryState(null, '', `/dm/${'ab'.repeat(16)}`);
  const fake = new FakeClient();
  fake.set('account', account()); fake.set('connection', { status: 'online', generation: '7' }); fake.set('communities', []);
  fake.set('dms', []);
  const dmView = render(<div className="d-root"><CoreProvider client={fake}><Shell /></CoreProvider></div>);
  expect(await screen.findByRole('button', { name: 'Reminders' })).toBeInTheDocument();
  dmView.unmount();
});

it('shows my text timeout expiry and blocks the channel composer', async () => {
  const expires = NOW + 600;
  const { fake } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.handler = command => command.m === 'getMemberTimeout' ? Promise.resolve(String(expires)) : Promise.resolve(null);
  } });

  const reason = `You cannot post messages in this community until ${formatTime(expires)}.`;
  expect(fake.callsOf('getMemberTimeout')).toEqual([{ m: 'getMemberTimeout', communityId: A, userId: ME.id }]);
  expect(await screen.findByText(reason)).toBeInTheDocument();
  expect(composer()).toHaveAttribute('aria-disabled', 'true');
});

it('refreshes my timeout when it expires', async () => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(NOW * 1000));
  let reads = 0;
  const expires = NOW + 60;
  const { fake, view } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.handler = command => command.m === 'getMemberTimeout'
      ? Promise.resolve(++reads === 1 ? String(expires) : null) : Promise.resolve(null);
  } });
  try {
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(screen.getByText(`You cannot post messages in this community until ${formatTime(expires)}.`)).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fake.callsOf('getMemberTimeout')).toHaveLength(2);
    expect(screen.queryByText(`You cannot post messages in this community until ${formatTime(expires)}.`)).not.toBeInTheDocument();
    expect(composer()).not.toHaveAttribute('aria-disabled', 'true');
  } finally {
    view.unmount();
    vi.useRealTimers();
  }
});

it('reads my timeout again when the selected community changes', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.handler = () => Promise.resolve(null);
  } });
  await waitFor(() => expect(fake.callsOf('getMemberTimeout')).toHaveLength(1));
  await user.click(within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: 'Valhalla' }));
  await waitFor(() => expect(fake.callsOf('getMemberTimeout')).toHaveLength(2));
  expect(fake.callsOf('getMemberTimeout').map(call => call.communityId)).toEqual([A, B]);
});

it('checkpoints selected files even when the current conversation has no text draft', async () => {
  let changed!: (entry: string) => void;
  const file = new File(['attachment'], 'only-file.txt', { type: 'text/plain' });
  const save = vi.fn<(...args: [unknown, unknown, readonly File[]]) => Promise<void>>().mockResolvedValue();
  const reload = vi.fn();
  const update: ShellUpdateDeps = { currentEntry: '/assets/old.js',
    watch: options => { changed = entry => options.changed(entry); return () => undefined; },
    store: { save, load: () => Promise.resolve(null), remove: () => Promise.resolve() }, reload };
  const { fake } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  fake.handler = command => command.m === 'checkpointFiles' ? Promise.resolve([file]) : Promise.resolve(null);
  await waitFor(() => expect(changed).toBeTypeOf('function'));
  act(() => changed('/assets/new.js'));
  await waitFor(() => expect(reload).toHaveBeenCalledOnce());
  expect(save.mock.calls[0]?.[1]).toMatchObject({ conversationId: GEN });
  expect(save.mock.calls[0]?.[2]).toEqual([file]);
});

it('locks the app while checkpointing and waits for a click after a failed save', async () => {
  let changed!: (entry: string) => void;
  const save = vi.fn<(...args: [unknown, unknown, readonly File[]]) => Promise<void>>()
    .mockRejectedValueOnce(new Error('store unavailable')).mockResolvedValue();
  const reload = vi.fn();
  const update: ShellUpdateDeps = { currentEntry: '/assets/old.js',
    watch: options => { changed = entry => options.changed(entry); return () => undefined; },
    store: { save, load: () => Promise.resolve(null), remove: () => Promise.resolve() }, reload };
  const { fake } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  fake.handler = command => command.m === 'checkpointFiles' ? Promise.resolve([]) : Promise.resolve(null);
  await waitFor(() => expect(changed).toBeTypeOf('function'));
  act(() => changed('/assets/new.js'));
  await waitFor(() => expect(screen.getByText(en['update.failed'])).toBeInTheDocument());
  expect(document.querySelector<HTMLElement>('.d-root')?.inert).toBe(false);
  await new Promise(resolve => setTimeout(resolve, 50));
  expect(save).toHaveBeenCalledTimes(1);
  expect(reload).not.toHaveBeenCalled();
  await userEvent.setup().click(screen.getByRole('button', { name: en['update.retry'] }));
  await waitFor(() => expect(reload).toHaveBeenCalledOnce());
  expect(save).toHaveBeenCalledTimes(2);
});

it('retries a failed checkpoint restore before starting the update watcher', async () => {
  const load = vi.fn().mockRejectedValueOnce(new Error('temporary IDB failure'))
    .mockResolvedValue({ state: { draft: { channelId: GEN, text: 'still saved' } }, files: [] });
  const watch = vi.fn(() => () => undefined);
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch,
    store: { save: () => Promise.resolve(), load, remove: () => Promise.resolve() }, reload: () => undefined };
  setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  await waitFor(() => expect(screen.getByText(en['update.restoreFailed'])).toBeInTheDocument());
  expect(watch).not.toHaveBeenCalled();
  await userEvent.setup().click(screen.getByRole('button', { name: en['update.retry'] }));
  await waitFor(() => expect(composer()).toHaveValue('still saved'));
  await waitFor(() => expect(watch).toHaveBeenCalledOnce());
});

it('loads older messages before restoring a saved scroll anchor', async () => {
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch: () => () => undefined,
    store: { save: () => Promise.resolve(), load: () => Promise.resolve({ state: { view: {
      main: { fromBottom: 120, anchor: { seq: '42', offset: 10 } }, thread: null, focus: null } }, files: [] }),
      remove: () => Promise.resolve() }, reload: () => undefined };
  const { fake } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline({ hasEarlier: true })) });
  fake.handler = command => {
    if (command.m === 'loadEarlier') fake.set(`timeline:${GEN}`, timeline({ hasEarlier: false,
      items: [item({ key: 's42', seq: '42', msgId: '42'.repeat(16), body: 'older' })] }));
    return Promise.resolve(null);
  };
  await waitFor(() => expect(fake.callsOf('loadEarlier')).toContainEqual({ m: 'loadEarlier', channelId: GEN }));
  await screen.findByText('older');
});

it('waits for an unsaved dialog to close before applying an update', async () => {
  let changed!: (entry: string) => void;
  const reload = vi.fn();
  const update: ShellUpdateDeps = { currentEntry: '/assets/old.js',
    watch: options => { changed = entry => options.changed(entry); return () => undefined; },
    store: { save: () => Promise.resolve(), load: () => Promise.resolve(null), remove: () => Promise.resolve() }, reload };
  const { fake, user } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  fake.handler = command => command.m === 'checkpointFiles' ? Promise.resolve([]) : Promise.resolve(null);
  await addServer(user, 'new server');
  await user.type(screen.getByRole('textbox', { name: 'Server name' }), 'unfinished');
  act(() => changed('/assets/new.js'));
  expect(screen.getByText(en['update.finishDialog'])).toBeInTheDocument();
  expect(reload).not.toHaveBeenCalled();
  await user.click(within(screen.getByRole('dialog', { name: 'Start a server' })).getByRole('button', { name: 'Close' }));
  await waitFor(() => expect(reload).toHaveBeenCalledOnce());
});

it('restores the draft after a refreshed tab boots and clears the checkpoint', async () => {
  const remove = vi.fn(() => Promise.resolve());
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch: () => () => undefined,
    store: { save: () => Promise.resolve(), load: () => Promise.resolve({ state: { draft: { channelId: GEN, text: 'kept across reload' } }, files: [] }), remove },
    reload: () => undefined };
  setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  await waitFor(() => expect(composer()).toHaveValue('kept across reload'));
  expect(remove).toHaveBeenCalledOnce();
});

it('final review M3: a draft restored after an update reload sends no typing signal until the person edits it', async () => {
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch: () => () => undefined,
    store: { save: () => Promise.resolve(), load: () => Promise.resolve({ state: { draft: { channelId: GEN, text: 'kept across reload' } }, files: [] }),
      remove: () => Promise.resolve() },
    reload: () => undefined };
  const { fake, user } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  await waitFor(() => expect(composer()).toHaveValue('kept across reload'));
  await new Promise(r => setTimeout(r, 50));
  // Typing clears away (Q1): a restored draft with nobody at the keyboard must not.
  expect(fake.callsOf('typing').filter(c => c.on)).toEqual([]);
  await user.type(composer(), '!');
  await waitFor(() => expect(fake.callsOf('typing')).toContainEqual({ m: 'typing', channelId: GEN, on: true }));
});

it('restores selected files before clearing a refreshed tab checkpoint', async () => {
  const file = new File(['attachment'], 'kept.txt', { type: 'text/plain' });
  const remove = vi.fn(() => Promise.resolve());
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch: () => () => undefined,
    store: { save: () => Promise.resolve(), load: () => Promise.resolve({ state: { conversationId: GEN,
      draft: { channelId: RAND, text: 'draft elsewhere' } }, files: [file] }), remove },
    reload: () => undefined };
  const { fake } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  await waitFor(() => expect(fake.callsOf('attachFiles')).toContainEqual({ m: 'attachFiles', channelId: GEN, files: [file] }));
  await waitFor(() => expect(remove).toHaveBeenCalledOnce());
});

it('reopens the same thread with its unsent reply after an update', async () => {
  const root = 'fa'.repeat(16);
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch: () => () => undefined,
    store: { save: () => Promise.resolve(), load: () => Promise.resolve({ state: { draft: { channelId: GEN, text: '' },
      side: { channelId: GEN, view: 'thread', rootMsgId: root }, threadDraft: { channelId: GEN, rootMsgId: root, text: 'reply survives' } }, files: [] }),
      remove: () => Promise.resolve() }, reload: () => undefined };
  const { fake } = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  await waitFor(() => expect(fake.callsOf('openThread')).toContainEqual({ m: 'openThread', channelId: GEN, rootMsgId: root }));
  expect(screen.getByRole('textbox', { name: en['thread.composer'] })).toHaveValue('reply survives');
});
it('restores the reply target and member pane after the channel list arrives', async () => {
  const reply = { msgId: 'ab'.repeat(16), author: 'Bob', excerpt: 'Earlier' };
  const remove = vi.fn(() => Promise.resolve());
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch: () => () => undefined,
    store: { save: () => Promise.resolve(), load: () => Promise.resolve({ state: { conversationId: GEN, draft: { channelId: GEN, text: 'later' },
      membersOpen: true, ui: { reply, editing: null, pinsOpen: false, jump: null, notice: null, trayError: null } }, files: [] }),
      remove }, reload: () => undefined };
  const { fake } = setup(`/c/${A}/${GEN}`, { channels: false, update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  await waitFor(() => expect(remove).toHaveBeenCalledOnce());
  act(() => fake.set(`channels:${A}`, CHANNELS));
  await waitFor(() => expect(composer()).toHaveValue('later'));
  await waitFor(() => expect(document.querySelector('.d-reply-chip__label')).toHaveTextContent(/replying to Bob/i));
});
it('reopens the search dialog with its query after an update', async () => {
  const update: ShellUpdateDeps = { currentEntry: '/assets/new.js', watch: () => () => undefined,
    store: { save: () => Promise.resolve(), load: () => Promise.resolve({ state: { search: { open: true, query: 'lantern', scope: 'all' } }, files: [] }),
      remove: () => Promise.resolve() }, reload: () => undefined };
  setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
  await waitFor(() => expect(screen.getByRole('combobox', { name: 'Search channels, people and messages' })).toHaveValue('lantern'));
});
it('opens scoped search with a modifier and sends the typed query', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`);
  await user.keyboard('{Control>}/{/Control}');
  const input = screen.getByRole('combobox', { name: 'Search channels, people and messages' });
  await user.type(input, 'lamp');
  // Task 9 fix round 1: the scope is implicit (everywhere, the open conversation's hits first).
  await waitFor(() => expect(fake.callsOf('search')).toContainEqual({ m: 'search', scope: 'all', id: null, query: 'lamp', cursor: null }));
  expect(input).toHaveFocus();
});

// Ruling X (Jonas 2026-10-27): the desktop channel header has no search; the top bar's search is the one, as Control+K is.
it('has no search in the desktop channel header; the top bar opens it', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`);
  const header = (await screen.findByRole('heading', { level: 2, name: 'general' })).closest('header')!;
  expect(within(header).queryByRole('button', { name: 'search' })).toBeNull();
  expect(within(header).queryByRole('button', { name: 'more' })).toBeNull();
  await user.click(screen.getByRole('button', { name: en['topbar.search'] }));
  const input = screen.getByRole('combobox', { name: 'Search channels, people and messages' });
  await user.type(input, 'lamp');
  await waitFor(() => expect(fake.callsOf('search').some(c => c.query === 'lamp')).toBe(true));
});
// Walk 7: the browser's own offline event shows the link banner at once; the gateway status stays online.
it('shows the link banner when the browser goes offline, and empties it when it returns', async () => {
  const { fake } = setup(`/c/${A}/${GEN}`);
  await screen.findByRole('heading', { level: 2, name: 'general' });
  expect(fake.get('connection')?.status).toBe('online');
  const line = () => document.querySelector('.d-link-banner');
  expect(line()).toBeEmptyDOMElement();
  const online = vi.spyOn(window.navigator, 'onLine', 'get').mockReturnValue(false);
  try {
    act(() => { window.dispatchEvent(new Event('offline')); });
    expect(line()).toHaveTextContent(en['nav.link.offline']);
    expect(screen.queryByText(en['shell.banner.offline'])).toBeNull();
    online.mockReturnValue(true);
    act(() => { window.dispatchEvent(new Event('online')); });
    expect(line()).toBeEmptyDOMElement();
  } finally { online.mockRestore(); }
});
// Task 9 fix round 1 (Jonas): the palette is the prototype's: channels, people and actions with nothing typed, no scope control.
it('lists channels, people and actions with nothing typed, draws no scope control, and opens a channel result', async () => {
  const { user } = setup(`/c/${A}/${GEN}`);
  await user.keyboard('{Control>}k{/Control}');
  const search = screen.getByRole('search');
  expect(within(search).queryByRole('button', { name: 'this community' })).toBeNull();
  expect(within(search).getByRole('group', { name: 'Channels' })).toBeInTheDocument();
  expect(within(search).getByRole('group', { name: 'Actions' })).toBeInTheDocument();
  expect(within(search).getByRole('option', { name: /Open settings/ })).toBeInTheDocument();
});

// Merge of lanes 9 and 10: settings search moved to ⌘K as the palette's Settings group, only for a query; a pick opens its section.
it('lists matching settings under Settings for a query, none with nothing typed, and opens the section', async () => {
  const { user } = setup(`/c/${A}/${GEN}`);
  await user.keyboard('{Control>}k{/Control}');
  const search = screen.getByRole('search');
  expect(within(search).queryByRole('group', { name: 'Settings' })).toBeNull();
  await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'voice');
  const group = within(search).getByRole('group', { name: 'Settings' });
  expect(within(group).getAllByRole('option').length).toBeLessThanOrEqual(8);
  await user.click(within(group).getAllByRole('option', { name: /Voice/ })[0]);
  await waitFor(() => expect(window.location.pathname).toBe('/settings/voice'));
  expect(screen.queryByRole('search')).toBeNull();
});

it('takes a picked community search hit to its channel and focuses the row', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${RAND}`, timeline({ channelId: RAND, items: [item({ key: 'found', seq: '5', body: 'lamp' })] }));
  } });
  await user.keyboard('{Control>}k{/Control}');
  await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'lamp');
  act(() => fake.set('search', { requestId: 4, hits: [{ channelId: RAND, communityId: A, seq: '5', snippet: 'lamp', rank: '0', threadRoot: null }], cursor: null }));
  await user.click(screen.getByRole('option', { name: /lamp/ }));
  await waitFor(() => expect(window.location.pathname).toBe(`/c/${A}/${RAND}`));
  await waitFor(() => expect(document.querySelector('.d-message-row[data-seq="5"]')).toHaveFocus());
});

it('focuses the destination row when the previous channel has the same sequence', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline({ items: [item({ key: 'old-five', seq: '5', body: 'old' })] }));
    client.set(`timeline:${RAND}`, timeline({ channelId: RAND, items: [item({ key: 'target-five', seq: '5', body: 'lamp' })] }));
  } });
  await user.keyboard('{Control>}k{/Control}');
  await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'lamp');
  act(() => fake.set('search', { requestId: 4, hits: [{ channelId: RAND, communityId: A, seq: '5', snippet: 'lamp', rank: '0', threadRoot: null }], cursor: null }));
  await user.click(screen.getByRole('option', { name: /lamp/ }));
  await waitFor(() => expect(window.location.pathname).toBe(`/c/${A}/${RAND}`));
  await waitFor(() => expect(screen.getByText('lamp', { selector: '.d-message-row__body' }).closest('.d-message-row')).toHaveFocus());
});

it('accepts the worker request id after invalidation and loads earlier pages for a picked row', async () => {
  const older = item({ key: 'older', seq: '5', body: 'lamp' });
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set('search', { requestId: 0, hits: [], cursor: null });
    client.set(`timeline:${RAND}`, timeline({ channelId: RAND, items: [item({ key: 'newer', seq: '20', body: 'later' })], hasEarlier: true }));
  } });
  fake.handler = command => {
    if (command.m === 'loadEarlier' && command.channelId === RAND) fake.set(`timeline:${RAND}`, timeline({ channelId: RAND, items: [older, item({ key: 'newer', seq: '20', body: 'later' })], hasEarlier: false }));
    return Promise.resolve(null);
  };
  await user.keyboard('{Control>}k{/Control}');
  await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'lamp');
  act(() => fake.set('search', { requestId: 9, hits: [{ channelId: RAND, communityId: A, seq: '5', snippet: 'lamp', rank: '0', threadRoot: null }], cursor: null }));
  await user.click(screen.getByRole('option', { name: /lamp/ }));
  await waitFor(() => expect(fake.callsOf('loadEarlier')).toContainEqual({ m: 'loadEarlier', channelId: RAND }));
  await waitFor(() => expect(document.querySelector('.d-message-row[data-seq="5"]')).toHaveFocus());
});

it('reports a search hit unavailable if its channel never loads a timeline', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`);
  await user.keyboard('{Control>}k{/Control}');
  await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'gone');
  act(() => fake.set('search', { requestId: 5, hits: [{ channelId: RAND, communityId: A, seq: '5', snippet: 'gone', rank: '0', threadRoot: null }], cursor: null }));
  await user.click(screen.getByRole('option', { name: /gone/ }));
  await waitFor(() => expect(screen.getByText('That message is no longer available')).toBeInTheDocument(), { timeout: 2500 });
  expect(screen.getByRole('combobox', { name: 'Search channels, people and messages' })).toHaveFocus();
});

it('takes a keyboard-picked DM hit to its DM route and focuses the row', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    withDms(client);
    client.set(`timeline:${DM}`, timeline({ channelId: DM, items: [item({ key: 'dm-found', seq: '8', body: 'lamp' })] }));
  } });
  await user.keyboard('{Control>}k{/Control}');
  const input = screen.getByRole('combobox', { name: 'Search channels, people and messages' });
  await user.type(input, 'lamp');
  act(() => fake.set('search', { requestId: 4, hits: [{ channelId: DM, communityId: null, seq: '8', snippet: 'lamp', rank: '0', threadRoot: null }], cursor: null }));
  await user.keyboard('{Enter}');
  await waitFor(() => expect(window.location.pathname).toBe(`/dm/${DM}`));
  await waitFor(() => expect(document.querySelector('.d-message-row[data-seq="8"]')).toHaveFocus());
});

it('sends typing off when leaving the visible channel', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => client.set(`timeline:${GEN}`, timeline()) });
  await user.type(composer(), 'draft');
  await user.click(screen.getByRole('button', { name: 'random' }));
  await waitFor(() => expect(fake.callsOf('typing')).toContainEqual({ m: 'typing', channelId: GEN, on: false }));
});
it('keeps ordinary drafts separate while switching between channels', async () => {
  const { user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set(`timeline:${RAND}`, timeline({ channelId: RAND }));
  } });
  await user.type(composer(), 'general draft');
  await user.click(screen.getByRole('button', { name: 'random' }));
  await user.type(composer('random'), 'random draft');
  await user.click(screen.getByRole('button', { name: 'general' }));
  expect(composer()).toHaveValue('general draft');
  await user.click(screen.getByRole('button', { name: 'random' }));
  expect(composer('random')).toHaveValue('random draft');
});
it('returns focus to where search was opened when Escape closes it', async () => {
  const { user } = setup(`/c/${A}/${GEN}`, { before: client => client.set(`timeline:${GEN}`, timeline()) });
  await user.click(composer());
  await user.keyboard('{Control>}k{/Control}');
  expect(screen.getByRole('combobox', { name: 'Search channels, people and messages' })).toHaveFocus();
  await user.keyboard('{Escape}');
  expect(screen.queryByRole('combobox', { name: 'Search channels, people and messages' })).toBeNull();
  expect(composer()).toHaveFocus();
});

// W-06: the palette is a modal dialog, so the shell beneath it is inert; M-01: no second palette, none under a modal.
it('opens search as a modal dialog, once, and never under another open dialog', async () => {
  const { user } = setup(`/c/${A}/${GEN}`, { before: client => client.set(`timeline:${GEN}`, timeline()) });
  await user.click(composer());
  await user.keyboard('{Control>}k{/Control}');
  const dialog = screen.getByRole('dialog', { name: 'Search channels, people and messages' });
  expect(dialog.tagName).toBe('DIALOG');
  expect(dialog).toHaveAttribute('open');
  const input = screen.getByRole('combobox', { name: 'Search channels, people and messages' });
  await user.keyboard('{Control>}k{/Control}');
  expect(screen.getAllByRole('dialog', { name: 'Search channels, people and messages' })).toHaveLength(1);
  await user.keyboard('{Escape}');
  expect(input).not.toBeInTheDocument();
  expect(composer()).toHaveFocus();
  const other = document.createElement('dialog');
  other.setAttribute('open', '');
  document.body.append(other);
  try {
    await user.keyboard('{Control>}k{/Control}');
    expect(screen.queryByRole('combobox', { name: 'Search channels, people and messages' })).toBeNull();
  } finally { other.remove(); }
});

it('opens no search under the Settings dialog', async () => {
  const { user } = setup('/settings/devices');
  await user.keyboard('{Control>}k{/Control}');
  expect(screen.queryByRole('combobox', { name: 'Search channels, people and messages' })).toBeNull();
});

// Task 9 fix round 1: the message search starts at two characters and runs everywhere (no visible scope).
it('says searching while the worker answers, then the empty text', async () => {
  let answer!: (value: null) => void;
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => client.set(`timeline:${GEN}`, timeline()) });
  fake.handler = command => command.m === 'search' && command.scope === 'all'
    ? new Promise(resolve => { answer = resolve; }) : Promise.resolve(null);
  await user.keyboard('{Control>}k{/Control}');
  await user.type(screen.getByRole('combobox', { name: 'Search channels, people and messages' }), 'zq');
  act(() => fake.set('search', { requestId: 3, hits: [], cursor: null }));
  expect(screen.getByRole('search')).toHaveTextContent('Searching your messages');
  await act(async () => { answer(null); await Promise.resolve(); });
  expect(screen.getByRole('search')).toHaveTextContent('Nothing in this browser matches “zq”.');
  expect(screen.queryByRole('button', { name: 'this conversation' })).toBeNull();
});

// The docs shoot's shell-not-member image showed "(E_NOT_READY)" in the error banner: a typing signal the worker refuses
// (the channel no longer viewable, or offline) is a hint that was not sent, never an error the person must read.
it('a refused typing signal shows no error banner', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.handler = command => (command.m === 'typing' ? Promise.reject(refusal({ code: 'E_NOT_READY' })) : Promise.resolve(null));
  } });
  await user.type(composer(), 'anyone still here?');
  await waitFor(() => expect(fake.callsOf('typing').length).toBeGreaterThan(0));
  await act(async () => { await Promise.resolve(); });
  expect(screen.queryByText(/E_NOT_READY/)).toBeNull();
  expect(document.body).not.toHaveTextContent('E_NOT_READY');
});

it('renews typing while the draft keeps changing, within the server’s hold', async () => {
  const clock = vi.spyOn(Date, 'now').mockReturnValue(1_000_000);
  try {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => client.set(`timeline:${GEN}`, timeline()) });
    await user.type(composer(), 'ab');
    expect(fake.callsOf('typing')).toEqual([{ m: 'typing', channelId: GEN, on: true }]);
    clock.mockReturnValue(1_005_000);
    await user.type(composer(), 'c');
    expect(fake.callsOf('typing')).toEqual([{ m: 'typing', channelId: GEN, on: true }, { m: 'typing', channelId: GEN, on: true }]);
  } finally { clock.mockRestore(); }
});

it('names typing people once each, in one polite status above the composer', () => {
  const { fake } = setup(`/c/${A}/${GEN}`, { before: client => client.set(`timeline:${GEN}`, timeline()) });
  act(() => fake.set(`typing:${GEN}`, [{ userId: PEER, name: 'bob', expiresAt: NOW + 10 }, { userId: PEER, name: 'bob', expiresAt: NOW + 9 }]));
  const status = screen.getByText('bob is typing…');
  expect(status).toHaveAttribute('role', 'status');
  expect(document.querySelector('.d-app-shell__main')).toContainElement(status);
  act(() => fake.set(`typing:${GEN}`, [{ userId: PEER, name: 'bob', expiresAt: NOW + 10 }, { userId: BOT, name: 'Helper', expiresAt: NOW + 10 }]));
  expect(screen.getAllByText(/typing/)).toHaveLength(1);
  expect(screen.getByText('Several people are typing…')).toHaveAttribute('role', 'status');
});

it('says only in the unblocked conversation that a validated Remove restored sending', async () => {
  const removed = 'You can send in this conversation after this member leaves. Their changed identity is still unverified elsewhere.';
  const { user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [{ userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false,
      changedUmk: 'cc'.repeat(32), removedIn: [GEN] }]);
  } });
  expect(screen.getByText(removed)).toHaveAttribute('role', 'status');
  await user.click(screen.getByRole('button', { name: 'random' }));
  expect(screen.queryByText(removed)).toBeNull();
});

// Flow 05 "changed": the alert stands in the conversation the changed contact is in, cannot be dismissed, and its action
// leads to Privacy's comparison. Task 10's browser run found it only inside Settings.
it('stands a changed contact of this conversation as an alert that leads to Privacy', async () => {
  const changed = 'This contact’s identity changed. Compare the number before sending.';
  const { user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [
      { userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'cc'.repeat(32) },
      { userId: STRANGER, name: 'eve', shared: true, umk: 'ee'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'dd'.repeat(32) },
    ]);
  } });
  const alert = screen.getByRole('alert');
  expect(alert).toHaveTextContent(changed);
  expect(alert).toHaveTextContent('bob');
  expect(screen.queryByText('eve')).toBeNull();
  expect(within(alert).queryByRole('button', { name: /dismiss/i })).toBeNull();
  await user.click(within(alert).getByRole('button', { name: 'Compare with bob' }));
  expect(path()).toBe('/settings/privacy');
});

// Requirement 1 of task 10 (L-CORE-40), W-01: the core's send gate per conversation (the sendBlocks slice) blocks the
// composer, and the composer says why: the changed-key copy for E_UMK_CHANGED, privacy.leafFailed for any failed leaf.
it('blocks the composer from the core\'s send block, and says why', async () => {
  const changed = 'This contact’s identity changed. Compare the number before sending.';
  const failed = 'A device in this conversation failed the identity check. Sending is paused here until it’s removed.';
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [
      { userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'cc'.repeat(32),
        blocked: 'E_UMK_CHANGED' },
    ]);
    client.set('sendBlocks', { [GEN]: { code: 'E_UMK_CHANGED', own: false } });
  } });
  expectBlocked();
  const reason = document.querySelector('.d-composer__reason');
  expect(reason).toHaveTextContent(changed);
  expect(composer('general').getAttribute('aria-describedby')?.split(' ')).toContain(reason?.id);
  expect(screen.getByRole('button', { name: en['shell.composer.attach'] })).toBeDisabled();
  await user.type(composer('general'), 'held back{Enter}');
  expect(fake.callsOf('send')).toEqual([]);
  for (const code of ['E_DEVICE_UNLISTED', 'E_TIER_MISMATCH', 'E_CREDENTIAL'] as const) {
    act(() => fake.set('sendBlocks', { [GEN]: { code, own: false } }));
    expectBlocked();
    expect(document.querySelector('.d-composer__reason')).toHaveTextContent(failed);
    expect(document.body).not.toHaveTextContent(code);
  }
  // The core lifted the block (verified, or a validated Remove here): sending resumes.
  act(() => fake.set('sendBlocks', {}));
  expect(composer('general')).not.toHaveAttribute('aria-disabled');
  expect(screen.getByRole('button', { name: en['shell.composer.attach'] })).toBeEnabled();
});

it('blocks only the conversation the core names', () => {
  setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('sendBlocks', { [RAND]: { code: 'E_DEVICE_UNLISTED', own: false } });
  } });
  expect(composer('general')).not.toHaveAttribute('aria-disabled');
});

// R1: privacy.changed asks for a comparison only when a changed member of this conversation has a key to compare.
it('says there is nothing to compare under the composer when the changed contact has no key', () => {
  const noKey = 'This contact’s identity changed, but no device list shows the new key, so there is nothing to compare yet. Sending is paused where their changed device is a member until it’s removed.';
  setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [{ userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false,
      changedUmk: null, blocked: 'E_UMK_CHANGED' }]);
    client.set('sendBlocks', { [GEN]: { code: 'E_UMK_CHANGED', own: false } });
  } });
  expectBlocked();
  expect(document.querySelector('.d-composer__reason')).toHaveTextContent(noKey);
  expect(document.querySelector('.d-composer__reason')).not.toHaveTextContent('Compare the number');
});

// R1: a block held by an own-user leaf names the own account and Devices, never a contact's number to compare.
it('says the own account is the reason under the composer when only own devices block the conversation', () => {
  const ownChanged = 'A device using your account under a different identity is in some of your conversations. Sending is paused there. Open Devices and remove any device you don’t recognise.';
  const ownUnlisted = 'A device that is not on your device list is using your account in some conversations. Sending is paused there. Remove it in Devices.';
  const { fake } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set('account', account({ ownTrust: 'changed' }));
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', []);
    client.set('sendBlocks', { [GEN]: { code: 'E_UMK_CHANGED', own: true } });
  } });
  expectBlocked();
  const reason = () => document.querySelector('.d-composer__reason');
  expect(reason()).toHaveTextContent(ownChanged);
  expect(reason()).not.toHaveTextContent('Compare');
  act(() => {
    fake.set('account', account({ ownTrust: 'unlisted' }));
    fake.set('sendBlocks', { [GEN]: { code: 'E_DEVICE_UNLISTED', own: true } });
  });
  expectBlocked();
  expect(reason()).toHaveTextContent(ownUnlisted);
});

// R2: a pin, reaction, edit or delete the core refuses for a trust reason says the blocked copy, never the raw code.
it('shows a trust refusal of a pin as the conversation\'s blocked copy, not a raw code', async () => {
  const changed = 'This contact’s identity changed. Compare the number before sending.';
  const failed = 'A device in this conversation failed the identity check. Sending is paused here until it’s removed.';
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', msgId: '11'.repeat(16), seq: '1', body: 'pin me' })] }));
    client.set('trust', [
      { userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'cc'.repeat(32),
        blocked: 'E_UMK_CHANGED' },
    ]);
    client.set('sendBlocks', { [GEN]: { code: 'E_UMK_CHANGED', own: false } });
  } });
  fake.handler = c => c.m === 'pin' ? Promise.reject(refusal({ code: 'E_UMK_CHANGED' })) : Promise.resolve(null);
  const row = () => screen.getByText('pin me').closest<HTMLElement>('.d-message-row')!;
  const shown = screen.getAllByText(changed).length;
  await user.click(toolbarButton(within(row()).getByRole('toolbar', { name: en['shell.message.toolbar'] }), 'pin'));
  await waitFor(() => expect(fake.callsOf('pin')).toHaveLength(1));
  // The alert and the composer already say it; the refusal adds its own dismissable banner with the same text.
  await waitFor(() => expect(screen.getAllByText(changed).length).toBe(shown + 1));
  expect(document.body).not.toHaveTextContent('E_UMK_CHANGED');
  expect(document.body).not.toHaveTextContent('That did not work');
  // Any other trust code reads as the failed-device copy.
  fake.handler = c => c.m === 'pin' ? Promise.reject(refusal({ code: 'E_DEVICE_UNLISTED' })) : Promise.resolve(null);
  act(() => fake.set('sendBlocks', { [GEN]: { code: 'E_DEVICE_UNLISTED', own: false } }));
  await user.click(toolbarButton(within(row()).getByRole('toolbar', { name: en['shell.message.toolbar'] }), 'pin'));
  await waitFor(() => expect(fake.callsOf('pin')).toHaveLength(2));
  await waitFor(() => expect(screen.getAllByText(failed).length).toBeGreaterThan(1));
  expect(document.body).not.toHaveTextContent('E_DEVICE_UNLISTED');
});

// A send the core refuses for a trust reason says what the composer would say, never "That did not work (E_…)".
it('shows a trust refusal of a send as the composer reason, not a raw code', async () => {
  const failed = 'A device in this conversation failed the identity check. Sending is paused here until it’s removed.';
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('sendBlocks', {});
  } });
  fake.handler = c => c.m === 'send' ? Promise.reject(refusal({ code: 'E_DEVICE_UNLISTED' })) : Promise.resolve(null);
  await user.type(composer('general'), 'hello{Enter}');
  await waitFor(() => expect(document.querySelector('.d-composer__reason')).toHaveTextContent(failed));
  expect(document.body).not.toHaveTextContent('E_DEVICE_UNLISTED');
  // The next published gate replaces the page's guess.
  act(() => fake.set('sendBlocks', {}));
  expect(composer('general')).not.toHaveAttribute('aria-disabled');
});

// C1: blocked E_UMK_CHANGED with no changedUmk is a leaf under a key no validated list presents: nothing to compare.
it('stands a changed contact with no key to compare without Compare, and says a Remove clears it', () => {
  const noKey = 'This contact’s identity changed, but no device list shows the new key, so there is nothing to compare yet. Sending is paused where their changed device is a member until it’s removed.';
  setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [{ userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false,
      changedUmk: null, blocked: 'E_UMK_CHANGED' }]);
  } });
  const alert = screen.getByRole('alert');
  expect(alert).toHaveTextContent('bob');
  expect(alert).toHaveTextContent(noKey);
  expect(alert).toHaveAttribute('data-tone', 'danger');
  expect(within(alert).queryByRole('button')).toBeNull();
});

it('leaves the composer open where the changed contact is not a member', () => {
  setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [
      { userId: STRANGER, name: 'eve', shared: true, umk: 'ee'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'dd'.repeat(32) },
    ]);
  } });
  expect(composer('general')).not.toHaveAttribute('aria-disabled');
});

// Settings' Privacy section carries the same alert and action; the conversation's copy under the modal would repeat the
// announcement and give a second, unreachable "Compare with" button.
it('leaves the changed alert to Privacy while Settings covers the conversation', () => {
  setup('/settings/privacy', { state: { settingsFrom: `/c/${A}/${GEN}` }, before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [{ userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false,
      changedUmk: 'cc'.repeat(32) }]);
  } });
  const settings = screen.getByRole('dialog', { name: /settings/i });
  expect(within(settings).getByRole('alert')).toHaveTextContent('This contact’s identity changed.');
  expect(within(settings).getByRole('button', { name: 'Compare with bob' })).toBeInTheDocument();
  expect(document.querySelector('.dw-trust-alerts')).toBeNull();
});

it('stands no changed alert where a validated Remove restored sending', () => {
  setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set('trust', [{ userId: PEER, name: 'bob', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false,
      changedUmk: 'cc'.repeat(32), removedIn: [GEN] }]);
  } });
  expect(screen.queryByRole('alert')).toBeNull();
});

// A blocked composer stays focusable: aria-disabled on the textarea, the native attribute on the button only (L-UI-13).
function expectBlocked(name = 'general') {
  expect(composer(name)).toHaveAttribute('aria-disabled', 'true');
  expect(sendButton()).toBeDisabled();
}
const opens = (fake: FakeClient) => fake.calls.filter(c => c.m === 'openChannel' || c.m === 'closeChannel');

describe('selection', () => {
  it('takes / to the first server and its first readable text channel', () => {
    const { fake } = setup('/', { channels: false });
    expect(path()).toBe(`/c/${A}`);
    expect(fake.callsOf('selectCommunity')).toEqual([{ m: 'selectCommunity', communityId: A }]);
    expect(fake.callsOf('openChannel')).toEqual([]);
    act(() => fake.set(`channels:${A}`, CHANNELS));
    expect(path()).toBe(`/c/${A}/${GEN}`);
    expect(fake.callsOf('openChannel')).toEqual([{ m: 'openChannel', channelId: GEN }]);
  });
  it('replaces a route to an unknown server', () => {
    setup(`/c/${'ff'.repeat(16)}`);
    expect(path()).toBe(`/c/${A}/${GEN}`);
  });
  it('replaces a route to an unknown channel', () => {
    setup(`/c/${A}/${'ee'.repeat(16)}`);
    expect(path()).toBe(`/c/${A}/${GEN}`);
  });
  it('lists channels in order, the category as a folding header before its children', () => {
    setup(`/c/${A}/${GEN}`);
    const list = screen.getByRole('navigation', { name: 'channels' });
    const text = list.textContent ?? '';
    expect(text).toContain('Midgard');
    const at = (s: string) => text.indexOf(s);
    expect(within(list).getByRole('button', { name: 'more' })).toHaveAttribute('aria-expanded', 'true');
    expect(at('more')).toBeLessThan(at('random'));
    expect(at('lounge')).toBeLessThan(at('more'));
    expect(at('general')).toBeLessThan(at('random'));
    expect(at('random')).toBeLessThan(at('lobby'));
    // web3.8 (item 12): a voice channel stands in its position among the text rows, under no heading.
    expect(at('lounge')).toBeLessThan(at('general'));
    expect(within(list).queryByRole('heading', { name: 'voice' })).toBeNull();
    expect(within(list).getByRole('img', { name: 'Readable by this server' })).toBeInTheDocument();
  });
  it('switches channels and servers, closing what it leaves', async () => {
    const { user, fake } = setup(`/c/${A}/${GEN}`);
    await user.click(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: /^random/ }));
    expect(path()).toBe(`/c/${A}/${RAND}`);
    expect(opens(fake)).toEqual([
      { m: 'openChannel', channelId: GEN }, { m: 'closeChannel', channelId: GEN }, { m: 'openChannel', channelId: RAND },
    ]);
    await user.click(within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: 'Valhalla' }));
    expect(path()).toBe(`/c/${B}`);
    expect(fake.callsOf('selectCommunity')).toEqual([
      { m: 'selectCommunity', communityId: A }, { m: 'selectCommunity', communityId: B },
    ]);
    expect(opens(fake).at(-1)).toEqual({ m: 'closeChannel', channelId: RAND });
  });
  it('shows an unsupported channel without opening it', () => {
    const { fake } = setup(`/c/${A}/${READ}`);
    expect(screen.getByRole('heading', { name: 'This channel does not open here yet' })).toBeInTheDocument();
    expect(screen.getByText('Channels this server can read arrive in a later version of the web client.')).toBeInTheDocument();
    expect(screen.queryByRole('log')).toBeNull();
    expect(composer('lobby')).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'send' })).toBeDisabled();
    expect(screen.getByText('this channel does not open here yet')).toBeInTheDocument();
    expect(fake.callsOf('openChannel')).toEqual([]);
  });
  it('shows a voice channel as its voice view, with no timeline and no composer (web-3)', () => {
    const { fake } = setup(`/c/${A}/${VOICE}`);
    expect(within(screen.getByRole('main')).getByRole('button', { name: 'Join voice' })).toBeInTheDocument();
    expect(within(screen.getByRole('main')).getByText('Nobody is here yet')).toBeInTheDocument();
    expect(screen.queryByRole('log')).toBeNull();
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(fake.callsOf('openChannel')).toEqual([]);
    expect(fake.callsOf('loadVoice')).toEqual([{ m: 'loadVoice', communityId: A, channelId: null }]);
  });
  it('marks a readable channel in its header', () => {
    setup(`/c/${A}/${READ}`);
    expect(screen.getByText('Channels this server can read arrive in a later version of the web client.')).toBeInTheDocument();
    expect(screen.getAllByRole('img', { name: 'Readable by this server' }).length).toBeGreaterThanOrEqual(2);
  });
  // Pre-flight ruling (c): a server whose channels did not load says so in the sidebar and can be retried.
  it('shows a server that did not load and tries again', async () => {
    let attempts = 0;
    const { fake, user } = setupWith(c => {
      if (c.m !== 'selectCommunity') return Promise.resolve(null);
      attempts += 1;
      return attempts === 1 ? Promise.reject(refusal({ code: 'E_NETWORK' })) : Promise.resolve(null);
    });
    const sidebar = await screen.findByRole('region', { name: 'Midgard' });
    expect(sidebar).toHaveTextContent('This server did not load (E_NETWORK).');
    expect(screen.queryByText('loading channels…')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'try again' }));
    expect(fake.callsOf('selectCommunity')).toEqual([
      { m: 'selectCommunity', communityId: A }, { m: 'selectCommunity', communityId: A },
    ]);
    expect(screen.queryByText('This server did not load (E_NETWORK).')).toBeNull();
    expect(screen.getByText('loading channels…')).toBeInTheDocument();
    act(() => fake.set(`channels:${A}`, CHANNELS));
    expect(path()).toBe(`/c/${A}/${GEN}`);
  });
});

// A shell whose calls are answered by `handler` from the first render on, on /c/A with no channels yet.
function setupWith(handler: FakeClient['handler']) {
  replaceHistoryState(null, '', `/c/${A}`);
  const fake = new FakeClient();
  fake.handler = handler;
  fake.set('account', account());
  fake.set('connection', { status: 'online', generation: '7' });
  fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }, { id: B, name: 'Valhalla', owner: 'ee'.repeat(16) }]);
  fake.set(`members:${A}`, MEMBERS);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Shell /></CoreProvider></div>);
  return { fake, user, view };
}

describe('timeline', () => {
  it('uses neutral reason copy on unreadable rows without exposing runtime codes', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [
      item({ key: 'changed', state: 'cannot-read', reason: 'E_UMK_CHANGED', body: '' }),
      item({ key: 'unlisted', state: 'cannot-read', reason: 'E_DEVICE_UNLISTED', body: '' }),
      item({ key: 'revoked', state: 'cannot-read', reason: 'E_DEVICE_REVOKED', body: '' }),
    ] })));
    const log = screen.getByRole('log', { name: 'messages in #general' });
    expect(log).toHaveTextContent('This contact’s identity changed. Compare the number before sending.');
    // A message from a device never added names the action that makes it readable, not only the state; one from a device
    // the sender removed says it stays shut, and never asks for an action that cannot help.
    const unlisted = 'Sent from a device that is not on the sender’s device list. Ask them to add it to their account; the message opens once they do.';
    const revoked = 'Sent from a device the sender removed from their account. It will not open.';
    expect(log).toHaveTextContent(unlisted);
    expect(log).toHaveTextContent(revoked);
    expect(log.textContent?.split(unlisted)).toHaveLength(2);
    for (const code of ['E_UMK_CHANGED', 'E_DEVICE_UNLISTED', 'E_DEVICE_REVOKED']) expect(log).not.toHaveTextContent(code);
  });
  it('shows names, tags and every row state, and never an unreadable body', async () => {
    const { fake, user, view } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [
      item({ key: 's1', body: 'hello from bob' }),
      item({ key: 's2', senderUser: BOT, bot: true, body: 'beep' }),
      item({ key: 's3', senderUser: ME.id, own: true, web: true, body: 'hi bob' }),
      item({ key: 's4', senderUser: STRANGER, body: 'who am i' }),
      item({ key: 's5', state: 'cannot-read', reason: 'E_SENDER_MISMATCH', senderUser: null, body: 'SECRET-PLAINTEXT' }),
      item({ key: 's6', state: 'deleted', body: 'GONE-TEXT' }),
      item({ key: `o${'ab'.repeat(16)}`, state: 'pending', senderUser: ME.id, own: true, web: true, body: 'on its way', msgId: 'ab'.repeat(16) }),
      item({ key: `o${'cd'.repeat(16)}`, state: 'failed', reason: 'E_TOO_LARGE', senderUser: ME.id, own: true, web: true, body: 'too big', msgId: 'cd'.repeat(16) }),
    ] })));
    const log = screen.getByRole('log', { name: 'messages in #general' });
    expect(log).toHaveAttribute('aria-live', 'polite');
    for (const text of ['hello from bob', 'beep', 'hi bob', 'who am i', 'on its way', 'too big']) expect(within(log).getByText(text)).toBeInTheDocument();
    // s1 and the deleted s6 (item()'s default sender is bob): a deleted row keeps its sender (L-CORE-08 rule 2) and
    // MessageRow always renders its author (task 21), so bob is named twice.
    expect(within(log).getAllByText('bob')).toHaveLength(2);
    expect(within(log).getByText('Helper')).toBeInTheDocument();
    expect(within(log).getAllByText('Ada L')).toHaveLength(3);
    expect(within(log).getByText('4a4a4a4a')).toBeInTheDocument();
    expect(within(log).getByText('device 29292929')).toBeInTheDocument();
    // MP4 removed the device tag from grouped rows: Ada's s3 and the pending row head runs of their own (s4 to s6 sit between),
    // the failed row is grouped under the pending one and carries none. (Caused by MP4, not by MP1's member-list change.)
    expect(within(log).getAllByText('web')).toHaveLength(2);
    expect(within(log).getAllByText('bot')).toHaveLength(1);
    // Each row names its time once in its head (heard) and once in its gutter (seen; Task 7, Signal §5.5): 8 rows, 16 times.
    expect(within(log).getAllByText(formatTime(NOW))).toHaveLength(16);
    expect(within(log).getByText('This message could not be read on this device.')).toBeInTheDocument();
    expect(within(log).queryByText('E_SENDER_MISMATCH')).toBeNull();
    expect(screen.queryByText(/SECRET-PLAINTEXT/)).toBeNull();
    expect(within(log).getByText('message deleted')).toBeInTheDocument();
    expect(screen.queryByText(/GONE-TEXT/)).toBeNull();
    expect(within(log).getByText('sending…')).toBeInTheDocument();
    expect(within(log).getByText('not sent')).toBeInTheDocument();
    expect(within(log).getByText('E_TOO_LARGE')).toBeInTheDocument();
    await user.click(within(log).getByRole('button', { name: 'retry' }));
    await user.click(within(log).getByRole('button', { name: 'discard' }));
    expect(fake.callsOf('retrySend')).toEqual([{ m: 'retrySend', msgId: 'cd'.repeat(16) }]);
    expect(fake.callsOf('discardSend')).toEqual([{ m: 'discardSend', msgId: 'cd'.repeat(16) }]);
    await expectNoAxeViolations(view.container);
  });
  it('says when it is loading, when it is empty, and loads earlier messages', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    expect(screen.getByText('loading messages…')).toBeInTheDocument();
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    expect(screen.getByText('nothing here yet. Messages sent before this browser joined are not shown.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'load earlier' })).toBeNull();
    act(() => fake.set(`timeline:${GEN}`, timeline({ hasEarlier: true, items: [item({ key: 's9', body: 'x' })] })));
    await user.click(screen.getByRole('button', { name: 'load earlier' }));
    expect(fake.callsOf('loadEarlier')).toEqual([{ m: 'loadEarlier', channelId: GEN }]);
  });
  // A11Y-DESIGN-01: a polite log announces rows added to it, but not the rows it mounts with. Each channel and its
  // first slice get a log of their own, so history is never read out as new messages.
  it('mounts a new log with its rows when the channel changes', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'in general' })] })));
    act(() => fake.set(`timeline:${RAND}`, timeline({ channelId: RAND, items: [item({ key: 'r1', body: 'in random' })] })));
    const before = screen.getByRole('log', { name: 'messages in #general' });
    await user.click(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: /^random/ }));
    const after = screen.getByRole('log', { name: 'messages in #random' });
    expect(after).not.toBe(before);
    expect(before.isConnected).toBe(false);
    expect(within(after).getByText('in random')).toBeInTheDocument();
    expect(after).not.toHaveAttribute('aria-busy');
  });
  it('starts at the newest row after switching timelines with overlapping row keys', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [
      item({ key: 's1', msgId: '11'.repeat(16), seq: '1', body: 'general first' }),
      item({ key: 's2', msgId: '12'.repeat(16), seq: '2', body: 'general last' }),
    ] })));
    act(() => fake.set(`timeline:${RAND}`, timeline({ channelId: RAND, items: [
      item({ key: 's1', msgId: '21'.repeat(16), seq: '1', body: 'random first' }),
      item({ key: 's3', msgId: '23'.repeat(16), seq: '3', body: 'random last' }),
    ] })));
    act(() => screen.getByText('general first').closest<HTMLElement>('.d-message-row')?.focus());
    expect(screen.getByText('general first').closest('.d-message-row')).toHaveAttribute('data-active', 'true');
    await user.click(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: /^random/ }));
    expect(screen.getByText('random last').closest('.d-message-row')).toHaveAttribute('data-active', 'true');
    expect(screen.getByText('random first').closest('.d-message-row')).not.toHaveAttribute('data-active', 'true');
  });
  it('closes a row emoji picker before switching to a timeline with the same row key', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [
      item({ key: 's1', msgId: '11'.repeat(16), seq: '1', body: 'general row' }),
    ] })));
    act(() => fake.set(`timeline:${RAND}`, timeline({ channelId: RAND, items: [
      item({ key: 's1', msgId: '21'.repeat(16), seq: '1', body: 'random row' }),
    ] })));
    const row = screen.getByText('general row').closest<HTMLElement>('.d-message-row')!;
    await user.click(within(row).getByRole('button', { name: 'react' }));
    expect(screen.getByRole('dialog', { name: 'pick a reaction' })).toBeInTheDocument();
    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
    expect(path()).toBe(`/c/${A}/${RAND}`);
    expect(screen.queryByRole('dialog', { name: 'pick a reaction' })).toBeNull();
    expect(screen.getByRole('log', { name: 'messages in #random' })).toHaveFocus();
    expect(fake.callsOf('react')).toEqual([]);
  });
  it('hands focus on when Alt+Arrow switches channels from the composer', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    act(() => fake.set(`timeline:${RAND}`, timeline({ channelId: RAND })));
    const before = composer();
    await user.click(before);
    expect(before).toHaveFocus();
    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
    expect(path()).toBe(`/c/${A}/${RAND}`);
    expect(before.isConnected).toBe(false);
    expect(document.activeElement).not.toBe(document.body);
    expect([screen.getByRole('log', { name: 'messages in #random' }), composer('random')])
      .toContain(document.activeElement);
  });
  it('remounts the composer after dismissing a mention query and switching channels', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    act(() => fake.set(`timeline:${RAND}`, timeline({ channelId: RAND })));
    const before = composer();
    await user.type(composer(), '@');
    const first = within(screen.getByRole('listbox', { name: 'people to mention' })).getAllByRole('option')[0];
    await user.keyboard('{ArrowDown}');
    expect(first).toHaveAttribute('aria-selected', 'false');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox', { name: 'people to mention' })).toBeNull();
    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
    expect(path()).toBe(`/c/${A}/${RAND}`);
    const after = composer('random');
    expect(after).not.toBe(before);
    expect(before.isConnected).toBe(false);
    await user.click(after);
    await user.type(composer('random'), '@');
    expect(within(screen.getByRole('listbox', { name: 'people to mention' })).getAllByRole('option')[0])
      .toHaveAttribute('aria-selected', 'true');
  });
  it('mounts a new log when the first slice arrives, and keeps focus in it', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    const loading = screen.getByRole('log', { name: 'messages in #general' });
    expect(loading).toHaveAttribute('aria-busy', 'true');
    act(() => loading.focus());
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'first' })] })));
    const ready = screen.getByRole('log', { name: 'messages in #general' });
    expect(ready).not.toBe(loading);
    expect(within(ready).getByText('first')).toBeInTheDocument();
    expect(ready).toHaveFocus();
    // Later slices of the same channel are added to the same log, and so are announced.
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'first' }), item({ key: 's2', body: 'second' })] })));
    expect(screen.getByRole('log', { name: 'messages in #general' })).toBe(ready);
  });
  it('is busy before its first slice and while earlier messages load', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    const log = () => screen.getByRole('log', { name: 'messages in #general' });
    expect(log()).toHaveAttribute('aria-busy', 'true');
    act(() => fake.set(`timeline:${GEN}`, timeline({ hasEarlier: true, items: [item({ key: 's9', body: 'x' })] })));
    expect(log()).not.toHaveAttribute('aria-busy');
    let finish: (v: null) => void = () => {};
    fake.handler = c => (c.m === 'loadEarlier' ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(null));
    await user.click(screen.getByRole('button', { name: 'load earlier' }));
    expect(log()).toHaveAttribute('aria-busy', 'true');
    await act(async () => { finish(null); await Promise.resolve(); });
    expect(log()).not.toHaveAttribute('aria-busy');
  });
});

describe('a channel that fails to open', () => {
  it('says so and can be retried', async () => {
    const { fake, user } = setup('/', { channels: false });
    let attempts = 0;
    fake.handler = c => {
      if (c.m !== 'openChannel') return Promise.resolve(null);
      attempts += 1;
      return attempts === 1 ? Promise.reject(refusal({ code: 'E_COMMIT_CONFLICT', status: 409 })) : Promise.resolve(null);
    };
    act(() => fake.set(`channels:${A}`, CHANNELS));
    expect(await screen.findByRole('heading', { name: 'This channel did not open' })).toBeInTheDocument();
    expect(screen.getByText('Something went wrong while opening it (E_COMMIT_CONFLICT).')).toBeInTheDocument();
    expect(screen.queryByRole('log')).toBeNull();
    expectBlocked();
    expect(screen.getByText('this channel did not open')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'try again' }));
    expect(fake.callsOf('openChannel')).toEqual([{ m: 'openChannel', channelId: GEN }, { m: 'openChannel', channelId: GEN }]);
    expect(screen.getByRole('log', { name: 'messages in #general' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'This channel did not open' })).toBeNull();
  });
});

describe('composer', () => {
  it.each([
    ['active', null],
    ['none', 'joining this channel…'],
    ['joining', 'joining this channel…'],
    ['resync', 'catching up with this channel…'],
    ['not-member', 'you cannot post in this channel and new messages will not arrive. Reload, or ask the host.'],
  ] as const)('in state %s', (group, reason) => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ group })));
    if (reason === null) {
      expect(composer()).not.toHaveAttribute('aria-disabled');
    } else {
      expect(composer()).toHaveAttribute('aria-disabled', 'true');
      expect(screen.getByRole('button', { name: 'send' })).toBeDisabled();
      expect(screen.getByText(reason)).toBeInTheDocument();
      expect(composer()).toHaveAccessibleDescription(expect.stringContaining(reason));
    }
  });
  it('sends what was typed', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    fake.handler = c => Promise.resolve(c.m === 'send' ? { msgId: 'ab'.repeat(16) } : null);
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    await user.type(composer(), 'hello{Enter}');
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: 'hello' }]);
  });
  // WEB-APP-01: while a send is in flight a second Enter, a click on send or a held Enter posts nothing more.
  it('sends once while a send is in flight', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    let finish: (v: unknown) => void = () => {};
    fake.handler = c => (c.m === 'send' ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(null));
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    await user.type(composer(), 'hello{Enter}{Enter}');
    await user.click(sendButton());
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: 'hello' }]);
    // Walk 3: the box is empty the moment Enter is pressed, so nothing typed next can join the sent text.
    expect(composer()).toHaveValue('');
    await act(async () => { finish({ msgId: 'ab'.repeat(16) }); await Promise.resolve(); });
    expect(composer()).toHaveValue('');
    // The in-flight mark is cleared: the next message goes out.
    fake.handler = c => Promise.resolve(c.m === 'send' ? { msgId: 'cd'.repeat(16) } : null);
    await user.type(composer(), 'again{Enter}');
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: 'hello' }, { m: 'send', channelId: GEN, text: 'again' }]);
  });
  // Walk 3: a key that lands right after Enter starts the next message; it never joins the one just sent.
  it('clears the draft at once, so the next keys do not repeat the sent text', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    fake.handler = c => (c.m === 'send' ? new Promise(() => {}) : Promise.resolve(null));
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    await user.type(composer(), 'Release night is Friday{Enter}Bring snacks{Enter}');
    expect(fake.callsOf('send')).toEqual([
      { m: 'send', channelId: GEN, text: 'Release night is Friday' },
      { m: 'send', channelId: GEN, text: 'Bring snacks' },
    ]);
    expect(composer()).toHaveValue('');
  });
  it('puts the text back when the send is refused and nothing new was typed', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    fake.handler = c => (c.m === 'send' ? Promise.reject(refusal({ code: 'E_NOT_READY', detail: 'x' })) : Promise.resolve(null));
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    await user.type(composer(), 'hello{Enter}');
    await screen.findByRole('alert');
    expect(composer()).toHaveValue('hello');
  });
  // A11Y-DESIGN-04: past the 4000-byte budget the counter says by how much, never a negative "left".
  it('says how far a message is over the budget and sends nothing', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    fireEvent.change(composer(), { target: { value: 'x'.repeat(3990) } });
    expect(screen.getByText('10 left', { selector: '.d-composer__counter' })).toBeInTheDocument();
    fireEvent.change(composer(), { target: { value: 'x'.repeat(4070) } });
    expect(screen.getByText('70 bytes over the limit', { selector: '.d-composer__counter' })).toBeInTheDocument();
    expect(screen.queryByText(/-\d+ left/)).toBeNull();
    expect(sendButton()).toHaveAttribute('aria-disabled', 'true');
    await user.click(sendButton());
    expect(fake.callsOf('send')).toEqual([]);
  });
  it('shows a failed command and lets it be dismissed', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    fake.handler = c => (c.m === 'send' ? Promise.reject(refusal({ code: 'E_NOT_READY', detail: 'the phase is loading' })) : Promise.resolve(null));
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    await user.type(composer(), 'hello{Enter}');
    expect(await screen.findByRole('alert')).toHaveTextContent('That did not work (E_NOT_READY).');
    await user.click(screen.getByRole('button', { name: 'dismiss' }));
    expect(screen.queryByRole('alert')).toBeNull();
  });
  // Pre-flight ruling (d): the shell holds the composer's text; a send that resolves clears it, a refused one keeps it.
  it('clears the text once a send resolves and keeps it when the send is refused', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    let refuse = false;
    fake.handler = c => {
      if (c.m !== 'send') return Promise.resolve(null);
      return refuse ? Promise.reject(refusal({ code: 'E_BAD_INPUT', detail: 'body' })) : Promise.resolve({ msgId: 'ab'.repeat(16) });
    };
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    await user.type(composer(), 'hello{Enter}');
    expect(composer()).toHaveValue('');
    refuse = true;
    await user.type(composer(), 'keep me{Enter}');
    // Pre-flight ruling (f): the banner says what to do, and never shows the server's detail.
    expect(await screen.findByRole('alert')).toHaveTextContent('That did not work (E_BAD_INPUT). Try again, or reload the page.');
    expect(screen.getByRole('alert')).not.toHaveTextContent('body');
    expect(composer()).toHaveValue('keep me');
    expect(fake.callsOf('send')).toEqual([
      { m: 'send', channelId: GEN, text: 'hello' }, { m: 'send', channelId: GEN, text: 'keep me' },
    ]);
  });
});

// A11Y-DESIGN-05: a focused control that removes itself hands focus on, never to <body>.
describe('focus after a control removes itself', () => {
  const F1 = 'cd'.repeat(16);
  const F2 = 'ef'.repeat(16);
  const failed = (msgId: string, body: string) =>
    item({ key: `o${msgId}`, state: 'failed', reason: 'E_NETWORK', senderUser: ME.id, own: true, web: true, body, msgId });
  it('goes from a retried or discarded row to the next failed row, else to the log', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'x' }), failed(F1, 'one'), failed(F2, 'two')] })));
    const log = screen.getByRole('log');
    const [, retry2] = within(log).getAllByRole('button', { name: 'retry' });
    act(() => within(log).getAllByRole('button', { name: 'retry' })[0].focus());
    await user.keyboard('{Enter}');
    expect(fake.callsOf('retrySend')).toEqual([{ m: 'retrySend', msgId: F1 }]);
    expect(retry2).toHaveFocus();
    // The worker turns the first row pending: focus stays where it went.
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [
      item({ key: 's1', body: 'x' }), { ...failed(F1, 'one'), state: 'pending', reason: '' }, failed(F2, 'two'),
    ] })));
    expect(retry2).toHaveFocus();
    act(() => within(log).getByRole('button', { name: 'discard' }).focus());
    await user.keyboard('{Enter}');
    expect(fake.callsOf('discardSend')).toEqual([{ m: 'discardSend', msgId: F2 }]);
    expect(screen.getByRole('log')).toHaveFocus();
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'x' }), { ...failed(F1, 'one'), state: 'pending', reason: '' }] })));
    expect(screen.getByRole('log')).toHaveFocus();
  });
  it('goes from a discarded row to the same action of the next failed row', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [failed(F1, 'one'), failed(F2, 'two')] })));
    const [discard1, discard2] = within(screen.getByRole('log')).getAllByRole('button', { name: 'discard' });
    act(() => discard1.focus());
    await user.keyboard('{Enter}');
    expect(discard2).toHaveFocus();
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [failed(F2, 'two')] })));
    expect(discard2).toHaveFocus();
  });
  it('goes from the dismissed command error to the composer', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    fake.handler = c => (c.m === 'send' ? Promise.reject(refusal({ code: 'E_NOT_READY' })) : Promise.resolve(null));
    act(() => fake.set(`timeline:${GEN}`, timeline()));
    await user.type(composer(), 'hello{Enter}');
    await screen.findByRole('alert');
    act(() => screen.getByRole('button', { name: 'dismiss' }).focus());
    await user.keyboard('{Enter}');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(composer()).toHaveFocus();
  });
  it('goes from the main pane’s try again to the log, and back to try again when it fails again', async () => {
    const { fake, user } = setup('/', { channels: false });
    let refuse = true;
    fake.handler = c => (c.m === 'openChannel' && refuse ? Promise.reject(refusal({ code: 'E_COMMIT_CONFLICT', status: 409 })) : Promise.resolve(null));
    act(() => fake.set(`channels:${A}`, CHANNELS));
    await screen.findByRole('heading', { name: 'This channel did not open' });
    act(() => screen.getByRole('button', { name: 'try again' }).focus());
    await user.keyboard('{Enter}');
    await waitFor(() => expect(screen.getByRole('button', { name: 'try again' })).toHaveFocus());
    expect(fake.callsOf('openChannel')).toHaveLength(2);
    refuse = false;
    await user.keyboard('{Enter}');
    expect(screen.getByRole('log', { name: 'messages in #general' })).toHaveFocus();
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'x' })] })));
    expect(screen.getByRole('log', { name: 'messages in #general' })).toHaveFocus();
  });
  it('goes from the sidebar’s try again to the channel list, and back to try again when it fails again', async () => {
    let attempts = 0;
    const { fake, user } = setupWith(c => {
      if (c.m !== 'selectCommunity') return Promise.resolve(null);
      attempts += 1;
      return attempts <= 2 ? Promise.reject(refusal({ code: 'E_NETWORK' })) : Promise.resolve(null);
    });
    await screen.findByRole('region', { name: 'Midgard' });
    act(() => screen.getByRole('button', { name: 'try again' }).focus());
    await user.keyboard('{Enter}');
    await waitFor(() => expect(screen.getByRole('button', { name: 'try again' })).toHaveFocus());
    await user.keyboard('{Enter}');
    expect(fake.callsOf('selectCommunity')).toHaveLength(3);
    act(() => fake.set(`channels:${A}`, CHANNELS));
    expect(path()).toBe(`/c/${A}/${GEN}`);
    const general = within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: /^general/ });
    expect(general).toHaveFocus();
    expect(general).toHaveAttribute('tabindex', '0');
  });
});

describe('servers and the join dialog', () => {
  it('invites a person without servers to join one', async () => {
    const { user, view } = setup('/', { communities: [] });
    expect(path()).toBe('/');
    expect(screen.getByRole('heading', { name: 'You are not in a server yet' })).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
    await user.click(screen.getByRole('button', { name: 'Join a server' }));
    expect(screen.getByRole('dialog', { name: 'Join a server' })).toBeInTheDocument();
  });
  it('joins from the rail, closes the dialog and opens the server', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    fake.handler = c => Promise.resolve(c.m === 'joinCommunity' ? { communityId: B } : null);
    await addServer(user, 'join a server');
    await user.type(screen.getByRole('textbox', { name: 'Invite' }), 'https://dilla.test/i/ABCD-EFGH');
    await user.click(screen.getByRole('button', { name: 'Join' }));
    expect(fake.callsOf('joinCommunity')).toEqual([{ m: 'joinCommunity', invite: 'ABCD-EFGH' }]);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(path()).toBe(`/c/${B}`);
  });
  it('keeps a just-joined server selected until the server list catches up', async () => {
    const NEW = '5b'.repeat(16);
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    fake.handler = c => Promise.resolve(c.m === 'joinCommunity' ? { communityId: NEW } : null);
    await addServer(user, 'join a server');
    await user.type(screen.getByRole('textbox', { name: 'Invite' }), 'ABCD-EFGH');
    await user.click(screen.getByRole('button', { name: 'Join' }));
    expect(path()).toBe(`/c/${NEW}`);
    expect(fake.callsOf('selectCommunity').map(c => c.communityId)).toEqual([A]);
    act(() => fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }, { id: B, name: 'Valhalla', owner: 'ee'.repeat(16) }, { id: NEW, name: 'Asgard', owner: 'ee'.repeat(16) }]));
    expect(path()).toBe(`/c/${NEW}`);
    expect(fake.callsOf('selectCommunity').map(c => c.communityId)).toEqual([A, NEW]);
  });
  it('opens the join dialog prefilled when an invite link is followed while signed in', () => {
    // channels: false keeps the default-channel redirect out of this test: the route stops at the first server.
    setup('/welcome?invite=ABCD', { channels: false });
    expect(screen.getByRole('dialog', { name: 'Join a server' })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Invite' })).toHaveValue('ABCD');
    expect(path()).toBe(`/c/${A}`);
    expect(window.location.search).toBe('');
  });
  it('opens the same dialog without a server and leaves the address at /', () => {
    setup('/welcome?invite=ABCD', { communities: [] });
    expect(screen.getByRole('dialog', { name: 'Join a server' })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Invite' })).toHaveValue('ABCD');
    expect(path()).toBe('/');
    expect(window.location.search).toBe('');
  });
  // Pre-flight ruling (g): the join refused right after signup (task 23) rides on the history entry and is shown on open.
  it('shows the join refused at signup in the dialog it opens', () => {
    const joinError = { code: 'E_INVITE_INVALID', detail: '', status: 410, retryAfterMs: null };
    setup('/welcome?invite=ABCD', { communities: [], state: joinErrorState(joinError) });
    expect(screen.getByRole('textbox', { name: 'Invite' })).toHaveValue('ABCD');
    expect(screen.getByRole('textbox', { name: 'Invite' })).toHaveAccessibleDescription(expect.stringContaining('This invite is expired, used up or unknown.'));
    expect(path()).toBe('/');
    expect(window.history.state).not.toHaveProperty('joinError');
  });
});

// BACKUPS-RECOVERY-04: E_ROOT_MISMATCH is an alert, not a log line: it stands in the shell while the worker reports it.
describe('a recovery root this account did not seal', () => {
  const TEXT = 'The recovery data stored for this account on dilla.test is not this account’s. The recovery key will not work until the operator resets it.';
  it('shows a persistent danger banner while the account slice reports it, and none otherwise', async () => {
    const { fake, view } = setup(`/c/${A}/${GEN}`, { before: f => f.set('account', account({ rootMismatch: true })) });
    const alert = screen.getByText(TEXT).closest('[role="alert"]');
    expect(alert).not.toBeNull();
    expect(alert).toHaveAttribute('data-tone', 'danger');
    expect(within(alert as HTMLElement).queryByRole('button')).toBeNull();
    await expectNoAxeViolations(view.container);
    act(() => fake.set('account', account({ rootMismatch: false })));
    expect(screen.queryByText(TEXT)).toBeNull();
  });
});

// Privacy lists no own row, so what the core reports about this account's own key stands here, persistent like a
// changed contact's alert: a different own identity, or a device under this account that the own list does not name.
describe('the own account’s identity', () => {
  const CHANGED = 'A device using your account under a different identity is in some of your conversations. Sending is paused there. Open Devices and remove any device you don’t recognise.';
  const UNLISTED = 'A device that is not on your device list is using your account in some conversations. Sending is paused there. Remove it in Devices.';
  it('shows a persistent danger banner whose action opens Devices, and none otherwise', async () => {
    const { fake, view, user } = setup(`/c/${A}/${GEN}`, { before: f => f.set('account', account({ ownTrust: 'changed' })) });
    const alert = screen.getByText(CHANGED).closest('[role="alert"]') as HTMLElement;
    expect(alert).not.toBeNull();
    expect(alert).toHaveAttribute('data-tone', 'danger');
    expect(within(alert).queryByRole('button', { name: /dismiss/i })).toBeNull();
    expect(within(alert).queryByRole('button', { name: /compare/i })).toBeNull();
    await expectNoAxeViolations(view.container);
    act(() => fake.set('account', account({ ownTrust: 'unlisted' })));
    expect(screen.queryByText(CHANGED)).toBeNull();
    const unlisted = screen.getByText(UNLISTED).closest('[role="alert"]') as HTMLElement;
    expect(unlisted).toHaveAttribute('data-tone', 'danger');
    await user.click(within(unlisted).getByRole('button', { name: 'Open devices' }));
    expect(path()).toBe('/settings/devices');
    act(() => fake.set('account', account({ ownTrust: null })));
    expect(screen.queryByText(UNLISTED)).toBeNull();
  });
});

describe('connection', () => {
  it('shows the link banner in the banner slot, with no status bar', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    const line = () => document.querySelector('.d-link-banner');
    expect(screen.queryByRole('region', { name: 'connection' })).toBeNull();
    expect(line()).toHaveAttribute('role', 'status');
    expect(line()).toBeEmptyDOMElement();
    expect(screen.queryByRole('alert')).toBeNull();
    act(() => fake.set('connection', { status: 'connecting', generation: null }));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(line()).toHaveTextContent('Connecting to dilla.test…');
    act(() => fake.set('connection', { status: 'offline', generation: '7' }));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(line()).toHaveTextContent('Offline. Reconnecting…');
    expect(screen.queryByRole('region', { name: 'connection' })).toBeNull();
    act(() => fake.set('connection', { status: 'online', generation: '8' }));
    expect(line()).toBeEmptyDOMElement();
  });

  it('shows the newer-client banner, not the link banner, while the server refuses the client version', () => {
    const OFFLINE = 'Connection lost. Reconnecting…';
    const TOO_OLD = 'This server needs a newer client. Reload the page.';
    const { fake } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set('connection', { status: 'offline', generation: '7', reason: 'version' }));
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(screen.getByRole('alert')).toHaveTextContent(TOO_OLD);
    expect(screen.queryByText(OFFLINE)).toBeNull();
    expect(document.querySelector('.d-link-banner')).toBeNull();
    act(() => fake.set('connection', { status: 'offline', generation: '7' }));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(document.querySelector('.d-link-banner')).toHaveTextContent('Offline. Reconnecting…');
    expect(screen.queryByText(TOO_OLD)).toBeNull();
  });

  it('offers a reload button on the newer-client banner, and only there', async () => {
    const reload = vi.spyOn(browser, 'reload').mockImplementation(() => undefined);
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    expect(screen.queryByRole('button', { name: en['reload.action'] })).toBeNull();
    act(() => fake.set('connection', { status: 'offline', generation: '7', reason: 'version' }));
    await user.click(within(screen.getByRole('alert')).getByRole('button', { name: en['reload.action'] }));
    expect(reload).toHaveBeenCalledTimes(1);
    act(() => fake.set('connection', { status: 'offline', generation: '7' }));
    expect(screen.queryByRole('button', { name: en['reload.action'] })).toBeNull();
    reload.mockRestore();
  });
});

describe('the newer-client reload keeps unsent work', () => {
  function wired() {
    const save = vi.fn<(...args: [unknown, unknown, readonly File[]]) => Promise<void>>().mockResolvedValue();
    const reload = vi.fn();
    const update: ShellUpdateDeps = { currentEntry: '/assets/old.js', watch: () => () => undefined,
      store: { save, load: () => Promise.resolve(null), remove: () => Promise.resolve() }, reload };
    const ctx = setup(`/c/${A}/${GEN}`, { update, before: client => client.set(`timeline:${GEN}`, timeline()) });
    ctx.fake.handler = command => command.m === 'checkpointFiles' ? Promise.resolve([]) : Promise.resolve(null);
    return { ...ctx, save, reload };
  }

  it('checkpoints the typed draft, then reloads', async () => {
    const { fake, user, save, reload } = wired();
    await user.type(composer(), 'keep me');
    act(() => fake.set('connection', { status: 'offline', generation: '7', reason: 'version' }));
    await user.click(screen.getByRole('button', { name: en['reload.action'] }));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledOnce();
    expect(save.mock.calls[0]?.[1]).toMatchObject({ conversationId: GEN, draft: { channelId: GEN, text: 'keep me' } });
  });

  it('control: with no draft it is a plain reload of an empty checkpoint', async () => {
    const { fake, user, save, reload } = wired();
    act(() => fake.set('connection', { status: 'offline', generation: '7', reason: 'version' }));
    await user.click(screen.getByRole('button', { name: en['reload.action'] }));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    expect(JSON.stringify(save.mock.calls[0]?.[1])).not.toContain('keep me');
  });
});

describe('keyboard', () => {
  it('moves between channels with Alt+Arrow, clamped at both ends', async () => {
    const { user } = setup(`/c/${A}/${GEN}`);
    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
    expect(path()).toBe(`/c/${A}/${RAND}`);
    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
    expect(path()).toBe(`/c/${A}/${READ}`);
    for (let i = 0; i < 4; i++) await user.keyboard('{Alt>}{ArrowUp}{/Alt}');
    expect(path()).toBe(`/c/${A}/${VOICE}`);
    await user.keyboard('{ArrowDown}');
    expect(path()).toBe(`/c/${A}/${VOICE}`);
  });
  it('returns from the log to the composer on Escape', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'x' })] })));
    act(() => screen.getByRole('log').focus());
    expect(screen.getByRole('log')).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(composer()).toHaveFocus();
  });
  it('returns to a blocked composer too, so its reason is heard', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ group: 'not-member', items: [item({ key: 's1', body: 'x' })] })));
    act(() => screen.getByRole('log').focus());
    await user.keyboard('{Escape}');
    expect(composer()).toHaveFocus();
    expect(composer()).toHaveAttribute('aria-disabled', 'true');
  });
  it('tabs through skip link, top bar, rail, channels, status, the header actions, the active row and the composer in that order', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'x' })] })));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('link', { name: 'skip to messages' }));
    // Signal (README §5.2): the top bar's search, bell and gear come first, above the rail.
    const bar = document.querySelector<HTMLElement>('.d-topbar');
    if (bar === null) throw new Error('no top bar');
    await user.tab();
    expect(document.activeElement).toBe(within(bar).getByRole('button', { name: en['topbar.search'] }));
    await user.tab();
    expect(document.activeElement).toBe(within(bar).getByRole('button', { name: 'Reminders' }));
    await user.tab();
    expect(document.activeElement).toBe(within(bar).getByRole('button', { name: en['workspace.open'] }));
    await user.tab();
    expect(screen.getByRole('navigation', { name: 'servers' })).toContainElement(document.activeElement as HTMLElement);
    // Web3.8 (WS8): one gear beside the server's name; leave and delete live in server settings. There are no sidebar tabs.
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'server settings' }));
    await user.tab();
    expect(screen.getByRole('navigation', { name: 'channels' })).toContainElement(document.activeElement as HTMLElement);
    // Web3.8 (item 12): the voice rows are in the one roving list; its one stop is the active row, so no second stop follows.
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /^general/ }));
    await user.tab();
    // web3.8: the sidebar splitter is its own stop between the channel list and the header.
    expect(document.activeElement).toBe(screen.getByRole('separator', { name: en['pane.resize.sidebar'] }));
    // web3.8 (item 13): the user panel follows the splitter: the name block (the status menu), then its toolbar (one stop:
    // mute, deafen). Signal §5.4: no gear (the top bar holds it).
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /your status: online$/ }));
    await user.tab();
    expect(document.activeElement).toBe(within(screen.getByRole('toolbar', { name: 'your account' })).getByRole('button', { name: 'mute' }));
    await user.tab();
    const header = document.querySelector<HTMLElement>('.d-channel-header');
    if (header === null) throw new Error('no header');
    // Signal §5.5 (Ruling X): Threads, Members and nothing else (pins and edit channel are in the row's context menu).
    expect(document.activeElement).toBe(within(header).getByRole('button', { name: en['layout.header.threads'] }));
    await user.tab();
    expect(document.activeElement).toBe(within(header).getByRole('button', { name: /^Members/ }));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('article'));
    await user.tab();
    // MP7: the author name is the row's first inner stop.
    expect(document.activeElement).toBe(screen.getByRole('article').querySelector('.d-message-row__author-open'));
    // Task 7 (Signal §5.5): the input row (textarea, send), then the footer's tools (attach, emoji, </>).
    await user.tab();
    expect(composer()).toHaveFocus();
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: en['shell.composer.send'] }));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: en['shell.composer.attach'] }));
  });
  // Requirement 11: closing the join dialog puts focus in the composer.
  it('moves focus to the composer when the join dialog closes', async () => {
    const { user } = setup(`/c/${A}/${GEN}`);
    await addServer(user, 'join a server');
    await user.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(composer()).toHaveFocus();
  });
});

const DM = '9a'.repeat(16);
const DMS: DmSummary[] = [{ id: DM, kind: 3, members: [ME.id, PEER], name: 'bob', group: 'active' }];
const withDms = (fake: FakeClient) => fake.set('dms', DMS);

describe('sidebar actions and pane widths (web3.8)', () => {
  const OWNED = [{ id: A, name: 'Midgard', owner: ME.id }];
  const clean = () => {
    // eslint-disable-next-line no-restricted-properties -- the test clears the one key the shell writes
    window.localStorage.clear();
  };
  beforeEach(clean);
  afterEach(clean);

  it('the owner sees "+" on each category header when the server has categories, and in the sidebar head when it has none', () => {
    const { view } = setup(`/c/${A}/${GEN}`, { communities: OWNED });
    const header = document.querySelector('.d-channel-list__head') as HTMLElement;
    expect(within(header).queryByRole('button', { name: en['invite.open'] })).toBeNull();
    expect(within(header).queryByRole('button', { name: en['create.channel.open'] })).toBeNull();
    expect(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: 'new channel in more' })).toBeInTheDocument();
    view.unmount();
    setup(`/c/${A}/${GEN}`, { communities: OWNED, before: fake => fake.set(`channels:${A}`, CHANNELS.filter(c => c.parentId === null && c.kind !== 2)) });
    const plus = within(document.querySelector('.d-channel-list__head') as HTMLElement).getByRole('button', { name: en['create.channel.open'] });
    expect(plus).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: en['invite.open'] })).toBeNull();
  });

  it('a member without manage channels sees no "+"; the DM list behind the @ tile has none either', async () => {
    const { user } = setup(`/c/${A}/${GEN}`);
    expect(screen.queryByRole('button', { name: en['create.channel.open'] })).toBeNull();
    await user.click(homeTile());
    expect(screen.queryByRole('button', { name: en['create.channel.open'] })).toBeNull();
  });

  it('"server settings" for a member with only create invite opens Invites (R20)', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`permissions:${A}`, { top: 0, owner: false, manageCommunity: false, manageChannels: false, kickMembers: false,
      banMembers: false, createInvite: true, manageRoles: false, loaded: true, overwritesLoaded: true, everyoneId: 'ef'.repeat(16) }));
    await user.click(screen.getByRole('button', { name: 'server settings' })); await user.click(await screen.findByRole('menuitem', { name: 'Server settings' })); // Signal task 10 fix round 2: the ⌄ is a menu
    const dialog = await screen.findByRole('dialog', { name: /settings/i });
    expect(within(dialog).getByRole('button', { name: 'Members', current: 'page' })).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Invites' }));
    expect(within(dialog).getByRole('button', { name: 'Invites', current: 'page' })).toBeInTheDocument();
  });

  it('ArrowRight on the sidebar separator writes dilla.paneWidths, and a remount reads it back', async () => {
    const { user, view } = setup(`/c/${A}/${GEN}`);
    const sep = screen.getByRole('separator', { name: en['pane.resize.sidebar'] });
    const before = Number(sep.getAttribute('aria-valuenow'));
    // Nothing is stored until a drag or a key reports a width: not on mount, not before the first key.
    // eslint-disable-next-line no-restricted-properties -- the test reads the one key the shell writes
    expect(window.localStorage.getItem('dilla.paneWidths')).toBeNull();
    act(() => sep.focus());
    await user.keyboard('{ArrowRight}');
    const after = Number(sep.getAttribute('aria-valuenow'));
    expect(after).toBeGreaterThan(before);
    // eslint-disable-next-line no-restricted-properties -- the test reads the one key the shell writes
    expect(JSON.parse(window.localStorage.getItem('dilla.paneWidths') ?? 'null')).toEqual({ sidebar: after });
    view.unmount();
    setup(`/c/${A}/${GEN}`);
    expect(screen.getByRole('separator', { name: en['pane.resize.sidebar'] })).toHaveAttribute('aria-valuenow', String(after));
  });

  it('a remount with no change writes nothing', () => {
    const { view } = setup(`/c/${A}/${GEN}`);
    view.unmount();
    setup(`/c/${A}/${GEN}`);
    // eslint-disable-next-line no-restricted-properties -- the test reads the one key the shell writes
    expect(window.localStorage.getItem('dilla.paneWidths')).toBeNull();
  });
});

describe('direct messages', () => {
  it('has no sidebar tabs: the @ tile is the rail’s first item and shows the DM list, a server shows its channels', async () => {
    const { user, view } = setup(`/c/${A}/${GEN}`, { before: withDms });
    expect(screen.queryByRole('tablist')).toBeNull();
    expect(screen.queryByRole('tab')).toBeNull();
    const sidebar = screen.getByRole('navigation', { name: 'channels' });
    expect(within(sidebar).getByRole('heading', { level: 1, name: 'Midgard' })).toBeInTheDocument();
    expect(within(sidebar).getByRole('button', { name: 'general' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'message someone' })).toBeNull();
    const rail = screen.getByRole('navigation', { name: 'servers' });
    const first = within(rail).getAllByRole('button')[0];
    expect(first).toBe(homeTile());
    expect(first).toHaveAccessibleName('direct messages');
    expect(first).not.toHaveAttribute('aria-current');
    await user.click(first);
    expect(path()).toBe('/dm');
    expect(first).toHaveAttribute('aria-current', 'page');
    expect(first).toHaveFocus();
    expect(within(rail).getByRole('button', { name: 'Midgard' })).not.toHaveAttribute('aria-current');
    const list = screen.getByRole('navigation', { name: 'direct messages' });
    // FB1: the Home sidebar is titled for the DMs, not for the server that was selected before.
    expect(within(list).getByRole('heading', { level: 1, name: 'Direct messages' })).toBeInTheDocument();
    expect(within(list).queryByRole('heading', { name: 'Midgard' })).toBeNull();
    expect(within(list).getByRole('button', { name: 'message someone' })).toBeInTheDocument();
    expect(within(list).getByRole('button', { name: 'bob' })).toHaveAttribute('data-kind', 'dm');
    expect(screen.queryByRole('tablist')).toBeNull();
    await expectNoAxeViolations(view.container);
    await user.click(within(rail).getByRole('button', { name: 'Midgard' }));
    expect(path()).toMatch(new RegExp(`^/c/${A}`));
    expect(screen.getByRole('navigation', { name: 'channels' })).toBeInTheDocument();
    expect(first).not.toHaveAttribute('aria-current');
    await user.click(homeTile());
    await user.click(within(screen.getByRole('navigation', { name: 'direct messages' })).getByRole('button', { name: 'bob' }));
    expect(path()).toBe(`/dm/${DM}`);
  });
  // WORKER-WEB-02: DMs belong to no server, so they stay reachable when no server is listed.
  it('with no server, the sidebar opens on the DMs and says there is no server in the @ tile’s empty list', async () => {
    const { user, view } = setup('/', { communities: [], before: withDms });
    const sidebar = screen.getByRole('navigation', { name: 'direct messages' });
    expect(within(sidebar).getByRole('heading', { level: 1, name: 'Direct messages' })).toBeInTheDocument();
    expect(homeTile()).toHaveAttribute('aria-current', 'page');
    expect(within(sidebar).getByRole('button', { name: 'bob' })).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
    await user.click(within(sidebar).getByRole('button', { name: 'message someone' }));
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });
  it('with no server, a DM route keeps the sidebar and the other DMs', () => {
    setup(`/dm/${DM}`, { communities: [], before: withDms });
    expect(homeTile()).toHaveAttribute('aria-current', 'page');
    expect(within(screen.getByRole('navigation', { name: 'direct messages' })).getByRole('button', { name: 'bob' }))
      .toHaveAttribute('aria-current', 'page');
  });
  // A11Y-DESIGN-02: a server that did not load is said in the channels list; the @ tile stays reachable and lists the DMs.
  it('a server that did not load says so, and the @ tile still lists the DMs', async () => {
    const { fake, user } = setupWith(c => (c.m === 'selectCommunity' ? Promise.reject(refusal({ code: 'E_NETWORK' })) : Promise.resolve(null)));
    act(() => fake.set('dms', DMS));
    const panel = await screen.findByRole('navigation', { name: 'channels' });
    await waitFor(() => expect(within(panel).getByRole('region', { name: 'Midgard' })).toHaveTextContent('This server did not load (E_NETWORK).'));
    await user.click(homeTile());
    expect(within(screen.getByRole('navigation', { name: 'direct messages' })).getByRole('button', { name: 'bob' })).toBeInTheDocument();
    await user.click(within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: 'Midgard' }));
    expect(await screen.findByRole('button', { name: 'try again' })).toBeInTheDocument();
  });
  it('says when there are none', async () => {
    const { user } = setup(`/c/${A}/${GEN}`, { before: f => f.set('dms', []) });
    await user.click(homeTile());
    expect(screen.getByText('no direct messages yet')).toBeInTheDocument();
  });
  it('opens a DM with its own log and composer, sends into it and closes it when left', async () => {
    const { fake, user, view } = setup(`/dm/${DM}`, { before: client => {
      withDms(client);
      client.handler = command => command.m === 'getMemberTimeout' ? Promise.resolve(String(NOW + 600)) : Promise.resolve(null);
    } });
    expect(fake.callsOf('openChannel')).toEqual([{ m: 'openChannel', channelId: DM }]);
    expect(homeTile()).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('heading', { level: 2, name: 'bob' })).toHaveTextContent('bob');
    expect(document.querySelector('.d-channel-header__title')).toHaveTextContent(/^bob$/);
    expect(document.querySelector('.d-channel-header__title [data-icon="at-sign"]')).not.toBeNull();
    act(() => fake.set(`timeline:${DM}`, timeline({ channelId: DM, items: [item({ key: 's1', body: 'hey' })] })));
    expect(screen.getByRole('log', { name: 'messages with bob' })).toHaveTextContent('hey');
    await expectNoAxeViolations(view.container);
    // shell.dm.composer is `message @{name}` (pre-flight row 1.10): the name is the composer's untransformed target.
    expect(document.querySelector('.d-composer__target')).toHaveTextContent('@bob');
    expect(fake.callsOf('getMemberTimeout')).toEqual([]);
    expect(screen.getByRole('textbox', { name: 'message @bob' })).not.toHaveAttribute('aria-disabled', 'true');
    await user.type(screen.getByRole('textbox', { name: 'message @bob' }), 'hi bob{Enter}');
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: DM, text: 'hi bob' }]);
    await user.click(within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: 'Midgard' }));
    await user.click(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: /^random/ }));
    expect(opens(fake)).toEqual([
      { m: 'openChannel', channelId: DM }, { m: 'closeChannel', channelId: DM },
      // The server tile opens its first channel on the way (general), which the next pick closes.
      { m: 'openChannel', channelId: GEN }, { m: 'closeChannel', channelId: GEN }, { m: 'openChannel', channelId: RAND },
    ]);
  });
  describe('the profile card in a DM (EOD E3)', () => {
    const open = async (body: string, how: 'author' | 'pill') => {
      const { fake, user } = setup(`/dm/${DM}`, { before: withDms });
      act(() => fake.set(`timeline:${DM}`, timeline({ channelId: DM, items: [item({ key: 's1', seq: '1', msgId: 'a1'.repeat(16), body })] })));
      const log = screen.getByRole('log', { name: 'messages with bob' });
      await user.click(await within(log).findByRole('button', { name: how === 'author' ? 'bob' : '@bob' }));
      return { fake, user, card: await screen.findByRole('dialog', { name: 'bob' }) };
    };
    it('opens from the author name with message and compare safety numbers only, outside inert scopes', async () => {
      const { card } = await open('hi', 'author');
      expect(card.closest('[inert]')).toBeNull();
      expect(within(card).getByText('bob')).toBeInTheDocument();
      expect(within(card).queryByText('owner')).toBeNull();
      expect(within(card).queryByRole('list')).toBeNull();
      expect(within(card).getAllByRole('button').filter(b => b.matches('.d-profile-card__action, .d-profile-card__icon-action')).map(b => b.getAttribute('aria-label') ?? b.textContent)).toEqual(['Message', 'compare safety numbers']);
    });
    it('opens from a mention pill', async () => {
      await open(`hello <@${PEER}>`, 'pill');
    });
    it('opens from the avatar', async () => {
      const { fake, user } = setup(`/dm/${DM}`, { before: withDms });
      act(() => fake.set(`timeline:${DM}`, timeline({ channelId: DM, items: [item({ key: 's1', seq: '1', msgId: 'a1'.repeat(16), body: 'hi' })] })));
      await user.click(document.querySelector<HTMLElement>('.d-message-row__avatar')!);
      expect(await screen.findByRole('dialog', { name: 'bob' })).toBeInTheDocument();
    });
    it('Escape closes the card and focus returns to the opener', async () => {
      const { user } = await open('hi', 'author');
      const opener = within(screen.getByRole('log')).getByRole('button', { name: 'bob' });
      await user.keyboard('{Escape}');
      expect(screen.queryByRole('dialog')).toBeNull();
      expect(opener).toHaveFocus();
    });
    it('a peer no shared server lists is titled with the DM name and shown offline', async () => {
      const CAROL = 'cc'.repeat(16);
      const { fake, user } = setup(`/dm/${DM}`, { before: f => f.set('dms', [{ id: DM, kind: 3, members: [ME.id, CAROL], name: 'carol', group: 'active' }]) });
      act(() => fake.set(`timeline:${DM}`, timeline({ channelId: DM, items: [item({ key: 's1', seq: '1', msgId: 'a1'.repeat(16), body: 'hi', senderUser: CAROL, senderDevice: CAROL })] })));
      await user.click(await within(screen.getByRole('log')).findByRole('button', { name: /^(carol|cccccccc)/ }));
      const card = await screen.findByRole('dialog', { name: 'carol' });
      // No shared server gives a presence snapshot, so the card says offline, as the member list does for an unknown person.
      expect(card).toHaveTextContent('offline');
    });
    it('a third party in a 1:1 DM is a former member, never titled with the peer', async () => {
      const { fake, user } = setup(`/dm/${DM}`, { before: withDms });
      act(() => fake.set(`timeline:${DM}`, timeline({ channelId: DM, items: [item({ key: 's1', seq: '1', msgId: 'a1'.repeat(16), body: 'yo', senderUser: STRANGER, senderDevice: STRANGER })] })));
      await user.click(await within(screen.getByRole('log')).findByRole('button', { name: /^4a4a4a4a/ }));
      const card = await screen.findByRole('dialog', { name: 'former member' });
      expect(card).not.toHaveTextContent('bob');
    });
    it('a member in a group DM is named from the members, an unknown one is a former member, never the group name', async () => {
      const GROUP = '9b'.repeat(16);
      const { fake, user } = setup(`/dm/${GROUP}`, { before: f => f.set('dms', [{ id: GROUP, kind: 4, members: [ME.id, PEER, STRANGER], name: 'the crew', group: 'active' }]) });
      act(() => fake.set(`timeline:${GROUP}`, timeline({ channelId: GROUP, items: [
        item({ key: 's1', seq: '1', msgId: 'a1'.repeat(16), body: 'hi' }),
        item({ key: 's2', seq: '2', msgId: 'a2'.repeat(16), body: 'yo', senderUser: STRANGER, senderDevice: STRANGER })] })));
      const log = screen.getByRole('log');
      await user.click(await within(log).findByRole('button', { name: 'bob' }));
      expect(await screen.findByRole('dialog', { name: 'bob' })).toBeInTheDocument();
      await user.keyboard('{Escape}');
      await user.click(await within(log).findByRole('button', { name: /^4a4a4a4a/ }));
      const card = await screen.findByRole('dialog', { name: 'former member' });
      expect(card).not.toHaveTextContent('the crew');
    });
    it('compare opens the safety numbers for that person', async () => {
      const { user, card } = await open('hi', 'author');
      await user.click(within(card).getByRole('button', { name: 'compare safety numbers' }));
      expect(path()).toBe('/settings/privacy');
    });
  });
  it('leaves a DM it does not know for the first server', () => {
    setup(`/dm/${'7b'.repeat(16)}`, { before: withDms });
    expect(path()).toBe(`/c/${A}/${GEN}`);
  });
  it('starts a DM with a member of this server', async () => {
    const { fake, user, view } = setup(`/c/${A}/${GEN}`, { before: withDms });
    fake.handler = c => Promise.resolve(c.m === 'openDm' ? { channelId: DM } : null);
    await user.click(homeTile());
    await user.click(screen.getByRole('button', { name: 'message someone' }));
    const dialog = screen.getByRole('dialog', { name: 'Message someone' });
    expect(within(dialog).getByText('Pick a member of this server.')).toBeInTheDocument();
    expect(within(dialog).getByText('bob')).toBeInTheDocument();
    expect(within(dialog).queryByText('Ada L')).toBeNull();
    expect(within(dialog).queryByText('Helper')).toBeNull();
    expect(within(dialog).getByRole('button', { name: 'message' })).toHaveAccessibleDescription('bob');
    await expectNoAxeViolations(view.container);
    await user.click(within(dialog).getByRole('button', { name: 'message' }));
    expect(fake.callsOf('openDm')).toEqual([{ m: 'openDm', userId: PEER }]);
    expect(path()).toBe(`/dm/${DM}`);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  // Product walk M12: the picker listed a removed member and missed one who joined after the page loaded.
  it('reads the server\'s members again when the picker opens, and lists what that read says', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { before: withDms });
    await user.click(homeTile());
    const before = fake.callsOf('selectCommunity').length;
    await user.click(screen.getByRole('button', { name: 'message someone' }));
    expect(fake.callsOf('selectCommunity').slice(before)).toEqual([{ m: 'selectCommunity', communityId: A }]);
    const dialog = screen.getByRole('dialog', { name: 'Message someone' });
    act(() => fake.set(`members:${A}`, [{ userId: '5e'.repeat(16), username: 'sven', display: 'Sven', kind: 0, roleIds: [] }]));
    expect(within(dialog).getByText('Sven')).toBeInTheDocument();
    expect(within(dialog).queryByText('bob')).toBeNull();
  });
  // Fix round 3 (m4): the picker's refresh is a background read: its failure keeps the cached list and says nothing.
  it('a failed member refresh on opening the picker shows no banner and keeps the list', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { before: withDms });
    await user.click(homeTile());
    fake.handler = c => (c.m === 'selectCommunity' ? Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503 })) : Promise.resolve(null));
    const before = fake.callsOf('selectCommunity').length;
    await user.click(screen.getByRole('button', { name: 'message someone' }));
    const dialog = screen.getByRole('dialog', { name: 'Message someone' });
    await waitFor(() => expect(fake.callsOf('selectCommunity').length).toBeGreaterThan(before));
    await act(async () => { await Promise.resolve(); });
    expect(within(dialog).getByText('bob')).toBeInTheDocument();
    expect(screen.queryByText(/That did not work/)).toBeNull();
  });

  it('shows a refused DM in its dialog', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { before: withDms });
    fake.handler = c => (c.m === 'openDm' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : Promise.resolve(null));
    await user.click(homeTile());
    await user.click(screen.getByRole('button', { name: 'message someone' }));
    await user.click(screen.getByRole('button', { name: 'message' }));
    expect(within(screen.getByRole('dialog', { name: 'Message someone' })).getByRole('alert'))
      .toHaveTextContent('That did not work (E_FORBIDDEN). Try again, or reload the page.');
    expect(path()).toBe('/dm');
  });
});

describe('badges', () => {
  const HALL = 'b9'.repeat(16);
  const badged = (badges: Record<string, { unread: number; mentions: number }>, settings: Record<string, string> = {}) => (fake: FakeClient) => {
    fake.set(`channels:${B}`, [ch(HALL, 'hall', { communityId: B })]);
    fake.set('badges', badges);
    fake.set('settings', settings);
    withDms(fake);
  };
  it('badges rows, the rail and the @ tile, and names them by their counts', async () => {
    const { view } = setup(`/c/${A}/${GEN}`, { before: badged({ [RAND]: { unread: 3, mentions: 0 }, [HALL]: { unread: 2, mentions: 1 }, [DM]: { unread: 1, mentions: 0 } }) });
    const list = screen.getByRole('navigation', { name: 'channels' });
    expect(within(list).getByRole('button', { name: 'random, unread 3, mentions 0' })).toHaveAttribute('data-unread', '3');
    expect(within(list).getByRole('button', { name: 'general' })).toHaveAttribute('data-unread', '0');
    const rail = screen.getByRole('navigation', { name: 'servers' });
    expect(within(rail).getByRole('button', { name: 'Midgard, unread 3, mentions 0' })).toBeInTheDocument();
    expect(within(rail).getByRole('button', { name: 'Valhalla, unread 2, mentions 1' })).toBeInTheDocument();
    expect(screen.queryByRole('tab')).toBeNull();
    expect(homeTile()).toHaveAccessibleName('direct messages, unread 1, mentions 0');
    expect(homeTile()).toHaveAttribute('data-unread', '1');
    await expectNoAxeViolations(view.container);
  });
  it('hides a muted channel’s unread count and keeps its mentions', () => {
    setup(`/c/${A}/${GEN}`, { before: badged({ [RAND]: { unread: 3, mentions: 1 } }, { [`mute.channel.${RAND}`]: '1' }) });
    expect(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: 'random, muted, mentions 1' })).toHaveAttribute('data-unread', '0');
    expect(within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: 'Midgard, unread 0, mentions 1' })).toBeInTheDocument();
  });
  it('follows the badge slice', () => {
    const { fake } = setup(`/c/${A}/${GEN}`, { before: badged({}) });
    expect(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: /^random$/ })).toBeInTheDocument();
    act(() => fake.set('badges', { [RAND]: { unread: 1, mentions: 0 } }));
    expect(within(screen.getByRole('navigation', { name: 'channels' })).getByRole('button', { name: 'random, unread 1, mentions 0' })).toBeInTheDocument();
  });
});

describe('reading', () => {
  let shown: DocumentVisibilityState = 'visible';
  beforeEach(() => {
    shown = 'visible';
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => shown });
  });
  afterEach(() => { delete (document as Partial<Record<'visibilityState', unknown>>).visibilityState; });

  it('marks the open channel read while it shows something unread, once per change', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'a' })] })));
    expect(fake.callsOf('markRead')).toEqual([]);
    act(() => fake.set('badges', { [GEN]: { unread: 1, mentions: 0 } }));
    expect(fake.callsOf('markRead')).toEqual([{ m: 'markRead', channelId: GEN }]);
    act(() => fake.set('badges', { [GEN]: { unread: 1, mentions: 0 } }));
    expect(fake.callsOf('markRead')).toHaveLength(1);
    act(() => fake.set('badges', { [GEN]: { unread: 0, mentions: 0 } }));
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'a' }), item({ key: 's2', body: 'b' })] })));
    act(() => fake.set('badges', { [GEN]: { unread: 1, mentions: 0 } }));
    expect(fake.callsOf('markRead')).toHaveLength(2);
  });
  it('waits while the page is hidden and marks when it is shown', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    shown = 'hidden';
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'a' })] })));
    act(() => fake.set('badges', { [GEN]: { unread: 2, mentions: 1 } }));
    expect(fake.callsOf('markRead')).toEqual([]);
    shown = 'visible';
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    expect(fake.callsOf('markRead')).toEqual([{ m: 'markRead', channelId: GEN }]);
  });
  // Added to the brief's tests (red proof (c)): the effect's dependency list already skips a republished badge with
  // the same counts, so only a change of another dependency (the page hidden and shown again) reaches the ref check.
  it('does not mark again when the page is shown again with nothing new', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'a' })] })));
    act(() => fake.set('badges', { [GEN]: { unread: 1, mentions: 0 } }));
    expect(fake.callsOf('markRead')).toEqual([{ m: 'markRead', channelId: GEN }]);
    shown = 'hidden';
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    shown = 'visible';
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    expect(fake.callsOf('markRead')).toHaveLength(1);
  });
  it('never marks a channel that is not open', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    act(() => fake.set(`timeline:${GEN}`, timeline({ items: [item({ key: 's1', body: 'a' })] })));
    act(() => fake.set('badges', { [RAND]: { unread: 5, mentions: 1 } }));
    expect(fake.callsOf('markRead')).toEqual([]);
  });
  it('marks an open DM too', () => {
    const { fake } = setup(`/dm/${DM}`, { before: withDms });
    act(() => fake.set(`timeline:${DM}`, timeline({ channelId: DM, items: [item({ key: 's1', body: 'a' })] })));
    act(() => fake.set('badges', { [DM]: { unread: 1, mentions: 0 } }));
    expect(fake.callsOf('markRead')).toEqual([{ m: 'markRead', channelId: DM }]);
  });
});

describe('devices and settings', () => {
  it('no longer asks for the device list on mount: Diagnostics and You do', () => {
    const { fake } = setup(`/c/${A}/${GEN}`);
    expect(fake.callsOf('refreshDevices')).toEqual([]);
    expect(screen.queryByRole('region', { name: 'connection' })).toBeNull();
  });
  it('opens settings from the top bar’s gear, remembering the channel', async () => {
    const { user } = setup(`/c/${A}/${GEN}`);
    await user.click(screen.getByRole('button', { name: en['workspace.open'] }));
    expect(path()).toBe('/settings');
    expect(window.history.state).toMatchObject({ settingsFrom: `/c/${A}/${GEN}` });
    expect(typeof (window.history.state as Record<string, unknown>).dillaHistoryIndex).toBe('number');
  });
  it('keeps the shell where it was while settings is open over it', () => {
    const { fake } = setup('/settings/notifications', { state: { settingsFrom: `/c/${A}/${RAND}` } });
    expect(path()).toBe('/settings/notifications');
    expect(fake.callsOf('selectCommunity')).toEqual([{ m: 'selectCommunity', communityId: A }]);
    expect(fake.callsOf('openChannel')).toEqual([{ m: 'openChannel', channelId: RAND }]);
  });
  it('goes nowhere under a deep link to settings', () => {
    setup('/settings/devices');
    expect(path()).toBe('/settings/devices');
  });
});

describe('the channel header actions (web3.9)', () => {
  const OWNED = [{ id: A, name: 'Midgard', owner: ME.id }];
  const header = () => document.querySelector('.d-channel-header') as HTMLElement;

  it('Signal §5.5 (Ruling X): Threads (icon and label) and Members (icon, the count, named "Members, {n}"), nothing else', () => {
    setup(`/c/${A}/${GEN}`, { communities: OWNED });
    const bar = within(header());
    const threads = bar.getByRole('button', { name: en['layout.header.threads'] });
    expect(threads).toHaveTextContent(en['layout.header.threads']);
    expect(threads.querySelector('svg[data-icon="messages-square"]')).not.toBeNull();
    const members = bar.getByRole('button', { name: /^Members, \d+$/ });
    expect(members.querySelector('svg[data-icon="users"]')).not.toBeNull();
    expect(members.textContent).toMatch(/^\d+$/);
    expect(header().querySelector('[data-foldable]')).toBeNull();
    for (const name of [en['shell.pins.open'], en['search.open'], en['admin.channel.open']]) expect(bar.queryByRole('button', { name })).toBeNull();
    expect(header()).not.toHaveAttribute('data-overflow');
    expect(bar.queryByRole('button', { name: en['layout.header.more'] })).toBeNull();
    expect(bar.getAllByRole('button')).toHaveLength(2);
  });

  it('the channel row\'s context menu holds pins and edit channel (Ruling X), and pins opens them', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: OWNED });
    const menu = await openRowMenu(user, 'general');
    // Signal task 10 fix round 1: Mute moved from Settings → Notifications into the row's menu.
    expect(within(menu).getAllByRole('menuitem').map(i => i.textContent)).toEqual([en['shell.pins.open'], en['admin.channel.open']]);
    expect(within(menu).getByRole('menuitemcheckbox', { name: en['notify.mute'] })).toBeInTheDocument();
    expect(within(menu).getAllByRole('menuitemradio').map(i => i.textContent)).toEqual([en['signalSettings.menu.notifyDefault'], en['signalSettings.menu.notifyAll'], en['signalSettings.menu.notifyMentions'], en['signalSettings.menu.notifyNothing']]);
    await user.click(within(menu).getByRole('menuitem', { name: en['shell.pins.open'] }));
    await waitFor(() => expect(fake.callsOf('loadPins')).toHaveLength(1));
  });
  // Signal task 10 fix round 1: muting a channel lives in its row's menu (Settings → Notifications keeps the prototype's rows).
  it('the row menu mutes a channel with mute.channel.<hex>', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: OWNED });
    const menu = await openRowMenu(user, 'general');
    await user.click(within(menu).getByRole('menuitemcheckbox', { name: en['notify.mute'] }));
    await waitFor(() => expect(fake.callsOf('setSetting').at(-1)).toMatchObject({ key: `mute.channel.${GEN}`, value: '1' }));
  });
  // Signal task 10 fix round 2 (Jonas): the channel's own notify mode is in its row menu; default deletes it.
  it('the row menu sets the channel\u2019s notify mode with notify.channel.<hex>', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: OWNED });
    await user.click(within(await openRowMenu(user, 'general')).getByRole('menuitemradio', { name: en['signalSettings.menu.notifyNothing'] }));
    await waitFor(() => expect(fake.callsOf('setSetting').at(-1)).toMatchObject({ key: `notify.channel.${GEN}`, value: 'nothing' }));
    await user.click(within(await openRowMenu(user, 'general')).getByRole('menuitemradio', { name: en['signalSettings.menu.notifyDefault'] }));
    await waitFor(() => expect(fake.callsOf('setSetting').at(-1)).toMatchObject({ key: `notify.channel.${GEN}`, value: null }));
  });
  // Signal task 10 fix round 2 (Jonas): the server's ⌄ menu holds its settings, Mute server and the server's notify mode.
  it('the server ⌄ menu mutes the server and sets its notify mode', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: OWNED });
    await user.click(await screen.findByRole('button', { name: en['admin.open'] }));
    // The current mode is the checked radio of the labelled group (no server value yet: default).
    const modes = await screen.findByRole('group', { name: en['notify.title'] });
    expect(within(modes).getByRole('menuitemradio', { name: en['signalSettings.menu.notifyDefault'] })).toHaveAttribute('aria-checked', 'true');
    expect(within(modes).getByRole('menuitemradio', { name: en['signalSettings.menu.notifyAll'] })).toHaveAttribute('aria-checked', 'false');
    await user.click(await screen.findByRole('menuitemcheckbox', { name: en['signalSettings.menu.muteServer'] }));
    await waitFor(() => expect(fake.callsOf('setSetting').at(-1)).toMatchObject({ key: `mute.community.${A}`, value: '1' }));
    await user.click(await screen.findByRole('menuitemradio', { name: en['signalSettings.menu.notifyAll'] }));
    await waitFor(() => expect(fake.callsOf('setSetting').at(-1)).toMatchObject({ key: `notify.community.${A}`, value: 'all' }));
  });
  it('the DM list head\u2019s menu sets the device-wide default that governs direct messages', async () => {
    const { fake, user } = setup('/dm', { communities: OWNED });
    await user.click(await screen.findByRole('button', { name: en['signalSettings.menu.dms'] }));
    const everything = await screen.findByRole('menuitemradio', { name: en['signalSettings.menu.everything'] });
    expect(screen.getByRole('menuitemradio', { name: en['signalSettings.menu.dmsMentions'] })).toHaveAttribute('aria-checked', 'true');
    await user.click(everything);
    await waitFor(() => expect(fake.callsOf('setSetting').at(-1)).toMatchObject({ key: 'notify.default', value: 'everything' }));
  });
  it('pins from the row of another channel open that channel, then its pins', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: OWNED });
    const menu = await openRowMenu(user, 'random');
    await user.click(within(menu).getByRole('menuitem', { name: en['shell.pins.open'] }));
    await waitFor(() => expect(path()).toBe(`/c/${A}/${RAND}`));
    expect(await screen.findByRole('dialog', { name: /random/ })).toBeInTheDocument();
    expect(fake.callsOf('loadPins').length).toBeGreaterThan(0);
  });

  it('a member without manage channels has no edit action, in the bar or the row menu', async () => {
    const { user } = setup(`/c/${A}/${GEN}`);
    const menu = await openRowMenu(user, 'general');
    expect(within(menu).getAllByRole('menuitem').map(i => i.textContent)).toEqual([en['shell.pins.open']]);
    await user.keyboard('{Escape}');
    expect(within(header()).queryByRole('button', { name: en['admin.channel.open'] })).toBeNull();
  });
});


it('selects a builtin member through the shipping Shell provider and executes dm', async () => {
  const { fake, user } = setup(`/c/${A}/${GEN}`, { before: core => {
    core.set(`timeline:${GEN}`, timeline());
    core.handler = command => command.m === 'openDm' ? Promise.resolve({ channelId: 'ac'.repeat(16) }) : Promise.resolve(null);
  } });
  fireEvent.change(composer(), { target: { value: '/dm' } });
  const member = await screen.findByRole('combobox', { name: 'user' });
  await user.type(member, 'bob');
  await user.click(await screen.findByRole('option', { name: 'bob' }));
  await user.click(screen.getByRole('button', { name: /run command/i }));
  await waitFor(() => expect(fake.callsOf('openDm')).toEqual([{ m: 'openDm', userId: PEER }]));
});

// Signal (README §5.2): on a desktop the reminders entry is the top bar's bell on every route, and the only one.
for (const [label, route] of [
  ['channel', `/c/${A}/${GEN}`],
  ['DM', `/dm/${DM}`],
  ['desktop voice', `/c/${A}/${VOICE}`],
  ['channel list', `/c/${A}`],
  ['DM list', '/dm'],
] as const) {
  it(`keeps the reminder inbox in the top bar on the ${label} route`, async () => {
    const { user } = setup(route, { before: fake => { withDms(fake); if (label === 'channel list') fake.set(`channels:${A}`, []); } });
    const trigger = await screen.findByRole('button', { name: 'Reminders' });
    expect(screen.getAllByRole('button', { name: 'Reminders' })).toHaveLength(1);
    expect(trigger.closest('.d-topbar')).not.toBeNull();
    expect(trigger.parentElement).not.toBe(document.querySelector('.d-app-shell__main'));
    // README §6.7: the bell opens the non-modal reminders popover under it (Due / Upcoming); Escape gives focus back to the bell.
    await user.click(trigger);
    const popover = screen.getByRole('dialog', { name: 'Reminders' });
    expect(popover).toHaveAttribute('aria-modal', 'false');
    expect(trigger.closest('.d-topbar__bell')).toContainElement(popover);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(within(popover).getByRole('tab', { name: /^Upcoming/ })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Reminders' })).toBeNull();
    expect(trigger).toHaveFocus();
    // A second press on the bell toggles it shut.
    await user.click(trigger);
    await user.click(trigger);
    expect(screen.queryByRole('dialog', { name: 'Reminders' })).toBeNull();
  });
}

describe('the top bar', () => {
  it('search opens the palette, as Control+K does', async () => {
    const { user } = setup(`/c/${A}/${GEN}`);
    await user.click(screen.getByRole('button', { name: en['topbar.search'] }));
    expect(await screen.findByRole('combobox', { name: 'Search channels, people and messages' })).toBeInTheDocument();
  });
  it('the gear opens Settings', async () => {
    const { user } = setup(`/c/${A}/${GEN}`);
    await user.click(within(document.querySelector<HTMLElement>('.d-topbar')!).getByRole('button', { name: en['workspace.open'] }));
    await waitFor(() => expect(path()).toBe('/settings'));
  });
  it('shows the Ctrl K chord off Apple platforms', () => {
    setup(`/c/${A}/${GEN}`);
    expect(screen.getByRole('button', { name: en['topbar.search'] })).toHaveTextContent(/Ctrl K$/);
  });
  it('has no voice chip outside a call', () => {
    setup(`/c/${A}/${GEN}`);
    expect(document.querySelector('.d-topbar__voice')).toBeNull();
  });
});

// Task 9 fix round 2: the member menu's Mention appends "@username " (a space before it when the draft does not end in one), puts
// the caret at the end and focuses the composer; the narrow desktop's drawer over the conversation (jsdom's layout) closes first,
// and the focus waits until the composer is no longer inert.
async function mentionBob() {
  const r = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set(`presence:${A}`, { [ME.id]: 'online', [PEER]: 'online', [BOT]: 'online' });
  } });
  await r.user.type(composer(), 'hi');
  await r.user.click(screen.getByRole('button', { name: /^Members/ }));
  const aside = screen.getByRole('complementary', { name: 'members of Midgard' });
  fireEvent.contextMenu(within(aside).getByRole('button', { name: /\bbob\b/ }), { clientX: 10, clientY: 10 });
  await r.user.click(within(screen.getByRole('menu', { name: 'actions for bob' })).getByRole('menuitem', { name: 'Mention' }));
  return r;
}
it('Mention from the member menu closes the drawer, appends with a separator and focuses the composer, caret at the end', async () => {
  await mentionBob();
  await waitFor(() => expect(composer()).toHaveFocus());
  const box = composer() as HTMLTextAreaElement;
  expect(box.value).toBe('hi @bob ');
  expect(box.selectionStart).toBe(box.value.length);
  expect(screen.queryByRole('complementary', { name: 'members of Midgard' })).toBeNull();
  expect(box.closest('[inert]')).toBeNull();
});
it('Mention is not offered while the composer cannot send (a text timeout)', async () => {
  const expires = NOW + 600;
  const { user } = setup(`/c/${A}/${GEN}`, { before: client => {
    client.set(`timeline:${GEN}`, timeline());
    client.set(`presence:${A}`, { [ME.id]: 'online', [PEER]: 'online', [BOT]: 'online' });
    client.handler = command => command.m === 'getMemberTimeout' ? Promise.resolve(String(expires)) : Promise.resolve(null);
  } });
  await screen.findByText(`You cannot post messages in this community until ${formatTime(expires)}.`);
  await user.click(screen.getByRole('button', { name: /^Members/ }));
  const aside = screen.getByRole('complementary', { name: 'members of Midgard' });
  fireEvent.contextMenu(within(aside).getByRole('button', { name: /\bbob\b/ }), { clientX: 10, clientY: 10 });
  const menu = screen.getByRole('menu', { name: 'actions for bob' });
  expect(within(menu).getByRole('menuitem', { name: 'Message' })).toBeInTheDocument();
  expect(within(menu).queryByRole('menuitem', { name: 'Mention' })).toBeNull();
});
