// W-Shell: the shell wires the channel bits, roles, lost access and the role flows (web3.5, flow 09).
import { afterEach, describe, it, expect } from 'vitest';
import { rowMenuItem } from '../test/row-menu.ts';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { PERM, type ChannelSummary, type CommunityAccess, type MemberSummary, type RoleSummary } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient } from '../test/fake-client.ts';
import { account, ME } from '../test/fixtures.ts';
import { Shell } from './Shell.tsx';

const A = 'a1'.repeat(16);
const GEN = 'c3'.repeat(16);
const LOBBY = 'd4'.repeat(16);
const OWNER = 'ee'.repeat(16);
const BOB = '28'.repeat(16);
const EVERYONE = 'ab'.repeat(16);
const GAME = '71'.repeat(16);
const ch = (id: string, name: string, position: number): ChannelSummary =>
  ({ id, communityId: A, kind: 0, mode: 0, name, topic: '', parentId: null, position, slowmode: 0, announcement: false, group: 'none' });
const role = (id: string, name: string, position: number, allow = 0): RoleSummary =>
  ({ id, name, color: 0, position, hoist: false, mentionable: true, allow, deny: 0, members: 0 });
const BASE = PERM.viewChannel | PERM.sendMessages | PERM.readHistory;
const ROLES = [role(EVERYONE, '@everyone', 0, BASE), role(GAME, 'Game Night', 3)];
const MEMBERS: MemberSummary[] = [
  { userId: ME.id, username: 'ada', display: 'Ada L', kind: 0, roleIds: [] },
  { userId: BOB, username: 'bob', display: 'Bob', kind: 0, roleIds: [] },
];
const access = (over: Partial<CommunityAccess> = {}): CommunityAccess => ({ top: 5, owner: false, manageCommunity: false, manageChannels: false,
  kickMembers: false, banMembers: false, createInvite: false, manageRoles: false, loaded: true, overwritesLoaded: true, everyoneId: EVERYONE, ...over });

function setup(opts: { acc?: Partial<CommunityAccess>; base?: number; overwrites?: Record<string, { kind: 0 | 1; targetId: string; allow: number; deny: number }[]>;
  start?: string; extra?: ChannelSummary[] } = {}) {
  window.history.replaceState(null, '', `/c/${A}/${opts.start ?? GEN}`);
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('connection', { status: 'online', generation: '7' });
  fake.set('communities', [{ id: A, name: 'Midgard', owner: OWNER }]);
  fake.set(`channels:${A}`, [ch(GEN, 'general', 0), ch(LOBBY, 'lobby', 1), ...(opts.extra ?? [])]);
  fake.set(`members:${A}`, MEMBERS);
  fake.set(`presence:${A}`, { [ME.id]: 'online', [BOB]: 'online' });
  fake.set(`roles:${A}`, opts.base === undefined ? ROLES : [role(EVERYONE, '@everyone', 0, opts.base), ROLES[1]]);
  fake.set(`overwrites:${A}`, opts.overwrites ?? {});
  fake.set(`permissions:${A}`, access(opts.acc));
  fake.set('removals', []);
  fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', items: [], hasEarlier: false });
  fake.set(`timeline:${LOBBY}`, { channelId: LOBBY, group: 'active', items: [], hasEarlier: false });
  const user = userEvent.setup();
  render(<div className="d-root"><CoreProvider client={fake}><Shell /></CoreProvider></div>);
  return { fake, user };
}
it('keeps account presence in the user panel, no select in the sidebar, and no connection state in a status bar', () => {
  setup();
  expect(screen.queryByRole('combobox', { name: 'Your status' })).toBeNull();
  expect(screen.getByRole('button', { name: /your status: online$/ }).closest('.d-user-panel')).not.toBeNull();
  expect(screen.queryByText('online', { selector: '.d-chunk__v' })).toBeNull();
  expect(screen.queryByRole('region', { name: 'Connection' })).toBeNull();
});
const box = () => screen.getByRole('textbox', { name: 'message #general' });
// Waits in real time outside act, so React flushes each announcer step instead of batching them.
const pause = (ms = 450) => {
  const until = Date.now() + ms;
  return waitFor(() => { if (Date.now() < until) throw new Error('waiting'); }, { timeout: ms + 1000, interval: 25 });
};
// Every non-empty text the shell's announcer shows, in order.
function listen(): string[] {
  const seen: string[] = [];
  const node = screen.getByTestId('shell-announcer');
  const note = () => { if (node.textContent) seen.push(node.textContent); };
  new MutationObserver(note).observe(node, { childList: true, characterData: true, subtree: true });
  note();
  return seen;
}
const DENY_SEND = (id: string) => ({ [id]: [{ kind: 0 as const, targetId: EVERYONE, allow: 0, deny: PERM.sendMessages }] });
const announcer = () => screen.getByTestId('shell-announcer');
const wide = () => {
  window.matchMedia = ((query: string) => ({ matches: true, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
    addListener: () => undefined, removeListener: () => undefined, onchange: null, dispatchEvent: () => false }));
};
afterEach(() => { Reflect.deleteProperty(window, 'matchMedia'); });

describe('the shell wires the channel bits', () => {
  it('offers no @everyone without the mentionEveryone bit', async () => {
    const { user } = setup();
    await user.type(box(), '@eve');
    expect(screen.queryByRole('listbox')).toBeNull();
  });
  it('offers @everyone with the bit, and a role from the roles slice', async () => {
    const { user } = setup({ base: BASE | PERM.mentionEveryone });
    await user.type(box(), '@eve');
    expect(within(screen.getByRole('listbox')).getAllByRole('option')).toHaveLength(1);
    await user.clear(box());
    await user.type(box(), '@ga');
    expect(within(screen.getByRole('listbox')).getByRole('option')).toHaveTextContent('Game Night');
  });
});

describe('a channel I may not send in', () => {
  it('keeps the composer, the draft and the focus, and announces it once', async () => {
    const { fake, user } = setup();
    await user.type(box(), 'half a thought');
    expect(box()).toHaveFocus();
    act(() => { fake.publish(`overwrites:${A}`, { [GEN]: [{ kind: 0, targetId: EVERYONE, allow: 0, deny: PERM.sendMessages }] }); });
    expect(box()).toHaveValue('half a thought');
    expect(box()).toHaveFocus();
    expect(box()).toHaveAttribute('aria-disabled', 'true');
    expect(await screen.findByText('You cannot send messages in #general.', { selector: 'p[role="status"]' })).toBeInTheDocument();
    expect(announcer()).toHaveTextContent('You cannot send messages in #general.');
    act(() => { fake.publish(`channels:${A}`, [ch(GEN, 'general', 0), ch(LOBBY, 'lobby', 1)]); });
    expect(announcer()).toHaveTextContent('You cannot send messages in #general.');
  });
});

describe('the channel edit dialog', () => {
  it('speaks its saves through the shell announcer', async () => {
    const { fake, user } = setup({ acc: { manageChannels: true, manageRoles: true }, overwrites: { [GEN]: [{ kind: 0, targetId: GAME, allow: 0, deny: 0 }] } });
    fake.handler = () => Promise.resolve(null);
    await rowMenuItem(user, 'general', 'edit channel');
    const dialog = screen.getByRole('dialog', { name: 'Edit #general' });
    await user.click(within(dialog).getByRole('button', { name: /^Game Night/ }));
    await user.click(within(within(dialog).getByRole('radiogroup', { name: 'Send messages' })).getByRole('radio', { name: 'Deny' }));
    expect(await screen.findByText('Permissions of #general saved.', { selector: '[data-testid="shell-announcer"]' })).toBeInTheDocument();
  });
});

describe('the pins panel', () => {
  it('states the pin rule once the bits are known', async () => {
    const { user } = setup();
    await rowMenuItem(user, 'general', 'pinned');
    expect(screen.getByText('A pin shows while the person who pinned it may pin in this channel.')).toBeInTheDocument();
  });
});

describe('the cannot-send memo', () => {
  it('is kept while the permissions slice is re-read, so it is told once', async () => {
    const { fake } = setup({ overwrites: DENY_SEND(GEN) });
    const seen = listen();
    await pause();
    const had = fake.get(`permissions:${A}`);
    act(() => { fake.publish(`permissions:${A}`, undefined as never); });
    await pause();
    act(() => { fake.publish(`permissions:${A}`, had as CommunityAccess); });
    await pause();
    expect(seen).toEqual(['You cannot send messages in #general.']);
  });
  it('is told again on returning to the channel', async () => {
    const { user } = setup({ overwrites: { ...DENY_SEND(GEN), ...DENY_SEND(LOBBY) } });
    const seen = listen();
    await pause();
    await user.click(screen.getByRole('button', { name: /lobby/ }));
    await pause();
    await user.click(screen.getByRole('button', { name: /general/ }));
    await pause();
    expect(seen).toEqual(['You cannot send messages in #general.', 'You cannot send messages in #lobby.', 'You cannot send messages in #general.']);
  });
});

describe('lost access', () => {
  it('announces, moves to the first listed channel, keeps the draft and closes the channel dialog', async () => {
    const { fake, user } = setup({ acc: { manageChannels: true } });
    await user.type(box(), 'still here');
    await rowMenuItem(user, 'general', 'edit channel');
    expect(screen.getByRole('dialog', { name: 'Edit #general' })).toBeInTheDocument();
    act(() => { fake.publish(`channels:${A}`, [ch(LOBBY, 'lobby', 1)]); });
    expect(await screen.findByText('#general is no longer available to you.', { selector: '[data-testid="shell-announcer"]' })).toBeInTheDocument();
    expect(window.location.pathname).toBe(`/c/${A}/${LOBBY}`);
    expect(screen.queryByRole('dialog')).toBeNull();
    act(() => { fake.publish(`channels:${A}`, [ch(GEN, 'general', 0), ch(LOBBY, 'lobby', 1)]); });
    expect(screen.queryByRole('dialog')).toBeNull();
    await user.click(screen.getByRole('button', { name: /general/ }));
    expect(box()).toHaveValue('still here');
  });
  it('says nothing when the whole server leaves the list', async () => {
    const { fake } = setup();
    const seen = listen();
    act(() => { fake.publish('communities', []); fake.publish(`channels:${A}`, []); });
    await pause();
    expect(seen).toEqual([]);
  });
  it('words a deleted channel the same neutral way, without saying it was hidden', async () => {
    const { fake } = setup();
    const seen = listen();
    act(() => { fake.publish(`channels:${A}`, [ch(LOBBY, 'lobby', 1)]); });
    await pause();
    expect(seen).toEqual(['#general is no longer available to you.']);
    expect(seen[0]).not.toMatch(/see|hidden|deleted/);
  });
  it('names a lost voice channel by its bare name, never with a text channel’s # (E-AFK round 2)', async () => {
    const VOICE = '5e'.repeat(16);
    const lounge: ChannelSummary = { ...ch(VOICE, 'lounge', 2), kind: 1 };
    const { fake } = setup({ start: VOICE, extra: [lounge] });
    const seen = listen();
    await pause();
    act(() => { fake.publish(`channels:${A}`, [ch(GEN, 'general', 0), ch(LOBBY, 'lobby', 1)]); });
    await pause();
    expect(seen).toEqual(['lounge is no longer available to you.']);
  });
  it('tells the lost channel first and the new channel being read-only after it, both', async () => {
    const { fake } = setup({ overwrites: DENY_SEND(LOBBY) });
    const seen = listen();
    act(() => { fake.publish(`channels:${A}`, [ch(LOBBY, 'lobby', 1)]); });
    await pause(600);
    expect(seen).toEqual(['#general is no longer available to you.', 'You cannot send messages in #lobby.']);
  });
});

describe('the role flows', () => {
  it('opens the assign dialog from the member card; closing returns focus to the member row', async () => {
    wide();
    const { user } = setup({ acc: { manageRoles: true } });
    await user.click(screen.getByRole('button', { name: /\bBob\b/ }));
    await user.click(within(screen.getByRole('dialog', { name: 'Bob' })).getByRole('button', { name: 'edit roles' }));
    const dialog = await screen.findByRole('dialog', { name: 'Roles of Bob' });
    await user.keyboard('{Escape}');
    expect(dialog).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /\bBob\b/ })).toHaveFocus();
  });
  it('in the narrow drawer, closing the assign dialog returns to the member row and closing the drawer returns to the toggle', async () => {
    window.matchMedia = ((query: string) => ({ matches: false, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
      addListener: () => undefined, removeListener: () => undefined, onchange: null, dispatchEvent: () => false }));
    const { user } = setup({ acc: { manageRoles: true } });
    await user.click(screen.getByRole('button', { name: /^Members/ }));
    await user.click(screen.getByRole('button', { name: /\bBob\b/ }));
    await user.click(within(screen.getByRole('dialog', { name: 'Bob' })).getByRole('button', { name: 'edit roles' }));
    await screen.findByRole('dialog', { name: 'Roles of Bob' });
    await user.keyboard('{Escape}');
    expect(screen.getByRole('button', { name: /\bBob\b/ })).toHaveFocus();
    await user.click(screen.getByRole('button', { name: 'close members' }));
    expect(screen.getByRole('button', { name: /^Members/ })).toHaveFocus();
  });
  it('the roles tab of server settings routes with manageRoles', async () => {
    const { user } = setup({ acc: { manageRoles: true } });
    await user.click(screen.getByRole('button', { name: 'server settings' })); await user.click(await screen.findByRole('menuitem', { name: 'Server settings' })); // Signal task 10 fix round 2: the ⌄ is a menu
    const dialog = screen.getByRole('dialog', { name: /settings/i });
    expect(within(dialog).getByRole('button', { name: 'Members', current: 'page' })).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Roles' }));
    expect(within(dialog).getByRole('button', { name: 'Roles', current: 'page' })).toBeInTheDocument();
    expect(within(dialog).getByRole('heading', { name: 'Roles' })).toBeInTheDocument();
  });
});
