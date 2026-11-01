// Flow 07 (v0.1.0-web3.1) in the shell: a fresh instance is usable from here alone. The rail starts a server, the
// sidebar adds a channel and copies an invite link, the empty states say how to begin.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ChannelSummary, Command, CommunitySummary, MemberSummary } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account, ME } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { Shell } from './Shell.tsx';
import { PHONE_QUERY } from '../layout.ts';
import { t } from '../strings/index.ts';
import { replaceHistoryState } from '../router.ts';

const A = 'a1'.repeat(16);
const NEW = '5b'.repeat(16);
const GEN = 'c3'.repeat(16);
const VGEN = 'c4'.repeat(16);
const RAND = 'd4'.repeat(16);
const PEER = '28'.repeat(16);
const OTHER_OWNER = 'ee'.repeat(16);

const ch = (id: string, communityId: string, name: string, over: Partial<ChannelSummary> = {}): ChannelSummary =>
  ({ id, communityId, kind: 0, mode: 0, name, topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active', ...over });
const ME_MEMBER: MemberSummary = { userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] };
const PEER_MEMBER: MemberSummary = { userId: PEER, username: 'bob', display: '', kind: 0, roleIds: [] };

/** A worker double that answers the create commands as the controller does: the slice first, then the answer. */
function worker(fake: FakeClient) {
  return (c: Command): Promise<unknown> => {
    if (c.m === 'createCommunity') {
      fake.set('communities', [...(fake.get('communities') ?? []), { id: NEW, name: c.name, owner: ME.id }]);
      return Promise.resolve({ communityId: NEW });
    }
    if (c.m === 'createChannel') {
      const id = c.kind === 'voice' ? VGEN : (fake.get(`channels:${c.communityId}`) ?? []).some(x => x.id === GEN) ? RAND : GEN;
      const list = fake.get(`channels:${c.communityId}`) ?? [];
      fake.set(`channels:${c.communityId}`, [...list, ch(id, c.communityId, c.name, { kind: c.kind === 'voice' ? 1 : 0, position: list.length, group: 'none' })]);
      return Promise.resolve({ channelId: id });
    }
    if (c.m === 'createInvite') return Promise.resolve({ url: 'https://dilla.test/i/7KQ2', code: '7KQ2', expires: 1_900_000_000 });
    if (c.m === 'selectCommunity' && fake.get(`members:${c.communityId}`) === undefined) fake.set(`members:${c.communityId}`, [ME_MEMBER]);
    return Promise.resolve(null);
  };
}

function setup(path: string, opts: { communities?: CommunitySummary[]; members?: MemberSummary[] } = {}) {
  replaceHistoryState(null, '', path);
  const fake = new FakeClient();
  fake.handler = worker(fake);
  fake.set('account', account());
  fake.set('connection', { status: 'online', generation: '7' });
  fake.set('communities', opts.communities ?? []);
  fake.set('dms', []);
  for (const c of opts.communities ?? []) {
    fake.set(`channels:${c.id}`, [ch(GEN, c.id, 'general')]);
    fake.set(`members:${c.id}`, opts.members ?? [ME_MEMBER, PEER_MEMBER]);
  }
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Shell /></CoreProvider></div>);
  return { fake, user, view };
}
const path = () => window.location.pathname;

/** The rail's "+" opens the add dialog; its choice (join a server, new server) opens the existing flow. */
async function addServer(user: ReturnType<typeof userEvent.setup>, choice: 'join a server' | 'new server') {
  await user.click(within(screen.getByRole('navigation', { name: 'servers' })).getByRole('button', { name: 'add a server' }));
  await user.click(within(screen.getByRole('dialog', { name: 'add a server' })).getByRole('button', { name: choice }));
}
const rail = () => screen.getByRole('navigation', { name: 'servers' });

afterEach(() => { vi.useRealTimers(); });

describe('a fresh instance (flow 07)', () => {
  it('says how to start: start a server or join one, in the main pane', async () => {
    const { user, view } = setup('/');
    const region = screen.getByRole('region', { name: 'You are not in a server yet' });
    expect(region).toHaveTextContent('Start a server and invite your friends, or join one with an invite someone sent you.');
    expect(within(region).getAllByRole('button').map(b => b.textContent)).toEqual(['Start a server', 'Join a server']);
    await expectNoAxeViolations(view.container);
    await user.click(within(region).getByRole('button', { name: 'Start a server' }));
    expect(screen.getByRole('dialog', { name: 'Start a server' })).toBeInTheDocument();
  });

  it('starts the rail with the @ tile and ends it with the one add tile, which opens a dialog of join and new server', async () => {
    const { user } = setup('/');
    const buttons = within(rail()).getAllByRole('button');
    expect(buttons.map(b => b.getAttribute('aria-label'))).toEqual(['direct messages', 'add a server']);
    expect(rail().textContent).not.toMatch(/join|new/);
    await user.click(buttons[1]);
    const dialog = screen.getByRole('dialog', { name: 'add a server' });
    expect(within(dialog).getAllByRole('button').map(b => b.textContent)).toEqual(expect.arrayContaining(['join a server', 'new server']));
  });

  it('starts a server from the rail: it is created with #general and a voice general, opened on #general, focus in its composer', async () => {
    const { fake, user } = setup('/');
    await addServer(user, 'new server');
    await user.type(screen.getByRole('textbox', { name: 'Server name' }), 'Midgard Crew{Enter}');
    await waitFor(() => expect(path()).toBe(`/c/${NEW}/${GEN}`));
    expect(fake.calls.filter(c => c.m.startsWith('create'))).toEqual([
      { m: 'createCommunity', name: 'Midgard Crew' },
      { m: 'createChannel', communityId: NEW, kind: 'text', name: 'general' },
      { m: 'createChannel', communityId: NEW, kind: 'voice', name: 'general' },
    ]);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(within(rail()).getByRole('button', { name: 'Midgard Crew' })).toHaveAttribute('aria-current', 'page');
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'message #general' })).toHaveFocus());
  });

  it('on a phone a new server opens #general with nothing focused in the composer and the tab bar shown', async () => {
    window.matchMedia = vi.fn((query: string) => ({
      matches: query === PHONE_QUERY, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
    })) as unknown as typeof window.matchMedia;
    try {
      const { user } = setup('/');
      await addServer(user, 'new server');
      await user.type(screen.getByRole('textbox', { name: 'Server name' }), 'Midgard Crew{Enter}');
      await waitFor(() => expect(path()).toBe(`/c/${NEW}/${GEN}`));
      const box = await screen.findByRole('textbox', { name: 'message #general' });
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
      expect(box).not.toHaveFocus();
      expect(document.activeElement?.closest('.d-composer')).toBeNull();
      expect(screen.getByRole('navigation', { name: t('nav.tabs.label') })).toBeVisible();
    } finally {
      Reflect.deleteProperty(window, 'matchMedia');
    }
  });

  // Review Focus 2 in the shell: the server stays selected and usable behind the notice, and after the dialog closes.
  it('a voice channel that fails keeps the new server selected, with #general listed and open to use', async () => {
    const { fake, user } = setup('/');
    const answer = worker(fake);
    fake.handler = c => (c.m === 'createChannel' && c.kind === 'voice' ? Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503 })) : answer(c));
    await addServer(user, 'new server');
    await user.type(screen.getByRole('textbox', { name: 'Server name' }), 'Midgard Crew{Enter}');
    const dialog = await screen.findByRole('dialog', { name: 'Start a server' });
    await waitFor(() => expect(within(dialog).getByRole('alert')).toHaveTextContent('voice channel general was not added (E_UNAVAILABLE)'));
    expect(within(rail()).getByRole('button', { name: 'Midgard Crew' })).toHaveAttribute('aria-current', 'page');
    // Behind the dialog the shell already opened the server's first channel, #general.
    expect(path()).toBe(`/c/${NEW}/${GEN}`);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(path()).toBe(`/c/${NEW}/${GEN}`);
    expect(within(screen.getByRole('navigation', { name: 'channels' })).getByText('general', { selector: '.d-chrow__name' })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'message #general' })).toBeInTheDocument();
    expect(fake.callsOf('openChannel').map(c => c.channelId)).toContain(GEN);
    expect(fake.callsOf('createCommunity')).toHaveLength(1);
    expect(fake.callsOf('createChannel').filter(c => c.kind === 'text')).toHaveLength(1);
  });

  // Step 7: Escape closes each create dialog through the native <dialog>, and nothing else takes focus from its opener.
  it('Escape closes a create dialog and focus goes back to what opened it', async () => {
    // The browser's own rule, which jsdom lacks: close() hands focus back to the element focused at showModal().
    const opened = new WeakMap<HTMLDialogElement, Element | null>();
    const show = vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (this: HTMLDialogElement) {
      opened.set(this, document.activeElement); this.setAttribute('open', '');
    });
    const close = vi.spyOn(HTMLDialogElement.prototype, 'close').mockImplementation(function (this: HTMLDialogElement) {
      this.removeAttribute('open');
      const back = opened.get(this);
      if (back instanceof HTMLElement && back.isConnected) back.focus();
    });
    try {
      const { user } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: ME.id }] });
      const tile = within(rail()).getByRole('button', { name: 'add a server' });
      // The add dialog's choice closes it (focus back on the "+" tile) and opens the flow, so the tile is the flow's opener too.
      for (const [opener, title, field] of [
        [tile, 'Start a server', 'Server name'],
        [screen.getByRole('button', { name: 'new channel' }), 'New channel in Midgard', 'Channel name'],
      ] as const) {
        opener.focus();
        await user.keyboard('{Enter}');
        if (opener === tile) await user.click(within(screen.getByRole('dialog', { name: 'add a server' })).getByRole('button', { name: 'new server' }));
        expect(screen.getByRole('dialog', { name: title })).toBeInTheDocument();
        expect(screen.getByRole('textbox', { name: field })).toHaveFocus();
        await user.keyboard('{Escape}');
        expect(screen.queryByRole('dialog')).toBeNull();
        expect(opener).toHaveFocus();
      }
    } finally {
      show.mockRestore();
      close.mockRestore();
    }
  });

  it('with no server, the direct messages say why there is no one to message', async () => {
    const { user } = setup('/');
    const sidebar = screen.getByRole('navigation', { name: 'direct messages' });
    expect(within(rail()).getByRole('button', { name: 'direct messages' })).toHaveAttribute('aria-current', 'page');
    await user.click(within(sidebar).getByRole('button', { name: 'message someone' }));
    const dialog = screen.getByRole('dialog', { name: 'Message someone' });
    expect(dialog).toHaveTextContent('There is no one to message yet: direct messages are with people you share a server with. Start a server and invite someone, or join one.');
    expect(within(dialog).queryByRole('list')).toBeNull();
  });
});

describe('a server’s actions (flow 07)', () => {
  it('the header holds the server settings gear alone (leave lives in settings); the owner has the "+" in the list header, a member none', () => {
    const { view } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: ME.id }] });
    const head = document.querySelector('.d-channel-list__head')!;
    expect(within(head as HTMLElement).getAllByRole('button').map(b => b.getAttribute('aria-label') ?? b.textContent)).toEqual(['new channel', 'server settings']);
    const panel = screen.getByRole('navigation', { name: 'channels' });
    expect(within(panel).getByRole('button', { name: 'new channel' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'invite link' })).toBeNull();
    view.unmount();
    setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: OTHER_OWNER }] });
    expect(within(document.querySelector('.d-channel-list__head') as HTMLElement).getAllByRole('button').map(b => b.getAttribute('aria-label') ?? b.textContent)).toEqual(['server settings']);
    expect(screen.queryByRole('button', { name: 'leave server' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'new channel' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'invite link' })).toBeNull();
  });

  it('the "+" is not on the direct messages list behind the @ tile', async () => {
    const { user } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: ME.id }] });
    expect(screen.getByRole('button', { name: 'new channel' })).toBeInTheDocument();
    await user.click(within(rail()).getByRole('button', { name: 'direct messages' }));
    expect(screen.queryByRole('button', { name: 'new channel' })).toBeNull();
  });

  // Product walk M1: in a channel this browser just made, nothing was sent before it joined, so the empty log says only
  // that nothing is here yet. A channel made elsewhere keeps the full sentence.
  it('an empty channel this browser just made says only that nothing is here yet', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: ME.id }] });
    act(() => fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', items: [], hasEarlier: false }));
    expect(screen.getByText('nothing here yet. Messages sent before this browser joined are not shown.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'new channel' }));
    await user.type(screen.getByRole('textbox', { name: 'Channel name' }), 'Random{Enter}');
    await waitFor(() => expect(path()).toBe(`/c/${A}/${RAND}`));
    act(() => fake.set(`timeline:${RAND}`, { channelId: RAND, group: 'active', items: [], hasEarlier: false }));
    await waitFor(() => expect(screen.getByText('nothing here yet.')).toBeInTheDocument());
    expect(screen.queryByText(/Messages sent before this browser joined/)).toBeNull();
  });

  it('adds a channel and opens it', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: ME.id }] });
    await user.click(screen.getByRole('button', { name: 'new channel' }));
    expect(screen.getByRole('dialog', { name: 'New channel in Midgard' })).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: 'Channel name' }), 'Random{Enter}');
    await waitFor(() => expect(path()).toBe(`/c/${A}/${RAND}`));
    expect(fake.callsOf('createChannel')).toEqual([{ m: 'createChannel', communityId: A, kind: 'text', name: 'random' }]);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  // Review M6: a new channel's composer takes focus once, on arrival. If the person goes elsewhere first, the wish is
  // dropped, so the composer does not take focus later when they come back.
  it('a pending composer focus is dropped when the person goes elsewhere before the channel shows', async () => {
    const LOBBY = 'e1'.repeat(16);
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: ME.id }] });
    act(() => fake.set(`channels:${A}`, [ch(GEN, A, 'general'), ch(LOBBY, A, 'lobby', { position: 1 })]));
    const answer = worker(fake);
    // The instance makes the channel, but the list that holds it has not arrived yet.
    fake.handler = c => (c.m === 'createChannel' ? Promise.resolve({ channelId: RAND }) : answer(c));
    await user.click(screen.getByRole('button', { name: 'new channel' }));
    await user.type(screen.getByRole('textbox', { name: 'Channel name' }), 'Random{Enter}');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await user.click(screen.getByRole('button', { name: /lobby/ }));
    await waitFor(() => expect(path()).toBe(`/c/${A}/${LOBBY}`));
    act(() => fake.set(`channels:${A}`, [ch(GEN, A, 'general'), ch(LOBBY, A, 'lobby', { position: 1 }), ch(RAND, A, 'random', { position: 2 })]));
    const row = screen.getByRole('button', { name: /random/ });
    await user.click(row);
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'message #random' })).toBeInTheDocument());
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole('textbox', { name: 'message #random' })).not.toHaveFocus();
  });

  it('server settings → Invites copies an invite link and says so; the link shows in the dialog', async () => {
    const { fake, user } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: OTHER_OWNER }] });
    act(() => fake.set(`permissions:${A}`, { top: 0, owner: false, manageCommunity: false, manageChannels: false, kickMembers: false, banMembers: false,
      createInvite: true, manageRoles: false, loaded: true, overwritesLoaded: true, everyoneId: 'ef'.repeat(16) }));
    await user.click(screen.getByRole('button', { name: 'server settings' })); await user.click(await screen.findByRole('menuitem', { name: 'Server settings' })); // Signal task 10 fix round 2: the ⌄ is a menu
    const settings = await screen.findByRole('dialog', { name: 'Server settings' });
    expect(within(settings).getByRole('button', { name: 'Members', current: 'page' })).toBeInTheDocument();
    await user.click(within(settings).getByRole('button', { name: 'Invites' }));
    // Signal task 10c: the link shows once in the Invites card with Copy (the prototype), no dialog.
    await user.click(await within(settings).findByRole('button', { name: 'Make invite link' }));
    const link = await within(settings).findByRole('textbox', { name: 'Invite link' });
    expect(link).toHaveValue('https://dilla.test/i/7KQ2');
    expect(fake.callsOf('createInvite')).toEqual([{ m: 'createInvite', communityId: A, maxUses: 10, ttlSeconds: 604_800 }]);
    await user.click(within(settings).getByRole('button', { name: 'Copy' }));
    await expect(navigator.clipboard.readText()).resolves.toBe('https://dilla.test/i/7KQ2');
    await waitFor(() => expect(within(settings).queryByRole('textbox', { name: 'Invite link' })).not.toBeInTheDocument());
  });

  it('a server with only you: the picker says to invite people first', async () => {
    const { user } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: ME.id }], members: [ME_MEMBER] });
    await user.click(within(rail()).getByRole('button', { name: 'direct messages' }));
    await user.click(screen.getByRole('button', { name: 'message someone' }));
    expect(screen.getByRole('dialog', { name: 'Message someone' }))
      .toHaveTextContent('No one else is in this server yet. Invite people first, then message them here.');
  });

  it('reads the server’s lists again when the window regains focus, at most once in 15 s', () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(1_900_000_000_000);
    const { fake } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: OTHER_OWNER }] });
    const selects = () => fake.callsOf('selectCommunity').length;
    const before = selects();
    act(() => { window.dispatchEvent(new Event('focus')); });
    expect(selects()).toBe(before);   // the load at mount was just now
    vi.setSystemTime(1_900_000_016_000);
    act(() => { window.dispatchEvent(new Event('focus')); });
    expect(selects()).toBe(before + 1);
    act(() => { window.dispatchEvent(new Event('focus')); });
    expect(selects()).toBe(before + 1);
  });

  // WB m1: a focus inside the 15 s window is not dropped. One trailing read runs when the window ends, so a tab that stays
  // in front still learns of another member's new channel (until the instance pushes the change itself).
  it('a focus inside the 15 s window schedules one trailing read at its end', () => {
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] });
    vi.setSystemTime(1_900_000_000_000);
    const { fake, view } = setup(`/c/${A}/${GEN}`, { communities: [{ id: A, name: 'Midgard', owner: OTHER_OWNER }] });
    const selects = () => fake.callsOf('selectCommunity').length;
    const before = selects();
    act(() => { vi.advanceTimersByTime(5_000); });
    act(() => { window.dispatchEvent(new Event('focus')); });
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    expect(selects()).toBe(before);
    act(() => { vi.advanceTimersByTime(9_999); });
    expect(selects()).toBe(before);
    act(() => { vi.advanceTimersByTime(1); });
    expect(selects()).toBe(before + 1);
    // A trailing read still pending when the shell goes away does not run.
    act(() => { vi.advanceTimersByTime(1_000); });
    act(() => { window.dispatchEvent(new Event('focus')); });
    expect(selects()).toBe(before + 1);
    view.unmount();
    act(() => { vi.advanceTimersByTime(60_000); });
    expect(selects()).toBe(before + 1);
  });
});
