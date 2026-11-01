import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { PERM, type ChannelSummary, type CommunityAccess, type MemberSummary, type OverwriteSummary, type RoleSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { ChannelPermissions } from './ChannelPermissions.tsx';

const SERVER = 'a1'.repeat(16);
const CH = 'c1'.repeat(16);
const EVERYONE = 'e0'.repeat(16);
const PLAYERS = 'b2'.repeat(16);
const MODS = 'b1'.repeat(16);
const COUNCIL = 'b3'.repeat(16);
const BOB = '28'.repeat(16);
const OWNER = 'ee'.repeat(16);

const role = (id: string, name: string, position: number, allow = 0): RoleSummary =>
  ({ id, name, color: 0, position, hoist: false, mentionable: false, allow, deny: 0, members: 0 });
const BASE = PERM.viewChannel | PERM.sendMessages | PERM.attachFiles | PERM.addReactions | PERM.readHistory;
const ROLES = [role(EVERYONE, '@everyone', 0, BASE), role(PLAYERS, 'Players', 2), role(MODS, 'Moderators', 5, PERM.pinMessages | PERM.manageRoles),
  role(COUNCIL, 'Council', 9)];
const MEMBERS: MemberSummary[] = [
  { userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [MODS] },
  { userId: BOB, username: 'bob', display: 'Bob', kind: 0, roleIds: [PLAYERS] },
];
const ACCESS: CommunityAccess = { top: 5, owner: false, manageCommunity: false, manageChannels: true, kickMembers: false, banMembers: false,
  createInvite: false, manageRoles: true, loaded: true, everyoneId: EVERYONE, overwritesLoaded: true };
const CHANNEL: ChannelSummary = { id: CH, communityId: SERVER, kind: 0, mode: 0, name: 'lobby', topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' };
const ow = (kind: 0 | 1, targetId: string, allow: number, deny: number): OverwriteSummary => ({ kind, targetId, allow, deny });

function setup(entries: OverwriteSummary[] = [], access: Partial<CommunityAccess> = {}, without: string[] = []) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: SERVER, name: 'Midgard', owner: OWNER }]);
  fake.set(`channels:${SERVER}`, [CHANNEL]);
  fake.set(`members:${SERVER}`, MEMBERS);
  fake.set(`roles:${SERVER}`, ROLES);
  fake.set(`permissions:${SERVER}`, { ...ACCESS, ...access });
  fake.set(`overwrites:${SERVER}`, entries.length === 0 ? {} : { [CH]: entries });
  for (const name of without) fake.set(name as `roles:${string}`, undefined as never);
  const announce = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}>
    <ChannelPermissions channelId={CH} communityId={SERVER} announce={announce} />
  </CoreProvider></div>);
  return { fake, user, view, announce };
}
const ALLOW_MODS = ow(0, MODS, PERM.viewChannel, 0);
const toggle = () => screen.getByRole('switch', { name: 'private channel' });
const disclosure = (name: string) => screen.getByRole('button', { name: new RegExp(`^${name}`) });

describe('the channel permissions section', () => {
  it('asks before making the channel private, and sends setChannelPrivate(true)', async () => {
    const { fake, user } = setup([ALLOW_MODS]);
    fake.handler = c => {
      if (c.m === 'setChannelPrivate') fake.set(`overwrites:${SERVER}`, { [CH]: [ow(0, EVERYONE, 0, PERM.viewChannel)] });
      return Promise.resolve(null);
    };
    expect(toggle()).toHaveAttribute('aria-checked', 'false');
    await user.click(toggle());
    const dialog = screen.getByRole('dialog', { name: 'Make #lobby private?' });
    expect(dialog).toHaveTextContent('People who are not given access leave the channel now.');
    await user.click(within(dialog).getByRole('button', { name: 'Make private' }));
    await waitFor(() => expect(fake.callsOf('setChannelPrivate')).toEqual([{ m: 'setChannelPrivate', channelId: CH, private: true }]));
    await waitFor(() => expect(toggle()).toHaveAttribute('aria-checked', 'true'));
  });
  it('asks before opening a private channel to everyone, and sends setChannelPrivate(false)', async () => {
    const { fake, user } = setup([ow(0, EVERYONE, 0, PERM.viewChannel)]);
    expect(toggle()).toHaveAttribute('aria-checked', 'true');
    await user.click(toggle());
    const dialog = screen.getByRole('dialog', { name: 'Open #lobby to everyone?' });
    expect(dialog).toHaveTextContent('Everyone in Midgard joins the channel.');
    await user.click(within(dialog).getByRole('button', { name: 'Open to everyone' }));
    await waitFor(() => expect(fake.callsOf('setChannelPrivate')).toEqual([{ m: 'setChannelPrivate', channelId: CH, private: false }]));
  });
  // Review 9: the instance answers 400 E_INVALID_REQUEST for the lockout and for a bad body, kind or bit alike, so its
  // refusal is worded by its code; only the page's own check (below) says "would hide".
  it('words the instance’s 400 by its code, keeps confirm enabled, and returns focus to the toggle on cancel', async () => {
    const { fake, user } = setup([ALLOW_MODS]);
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 400 }));
    await user.click(toggle());
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Could not save the permissions (E_INVALID_REQUEST). Try again.');
    expect(alert).not.toHaveTextContent('That change would hide the channel from you.');
    expect(screen.getByRole('button', { name: 'Make private' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(toggle()).toHaveFocus());
  });
  it('does not ask the instance when making private would hide the channel from me, and says so at once', async () => {
    const { fake, user } = setup();
    await user.click(toggle());
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('That change would hide the channel from you.');
    expect(fake.callsOf('setChannelPrivate')).toEqual([]);
  });
  it('lets the owner make it private without an allowing entry', async () => {
    const { fake, user } = setup([], { owner: true });
    await user.click(toggle());
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    await waitFor(() => expect(fake.callsOf('setChannelPrivate')).toHaveLength(1));
  });
  it('lets an administrator make it private without an allowing entry', async () => {
    const { fake, user } = setup();
    fake.publish(`roles:${SERVER}`, ROLES.map(r => (r.id === MODS ? { ...r, allow: r.allow | PERM.administrator } : r)));
    await user.click(toggle());
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    await waitFor(() => expect(fake.callsOf('setChannelPrivate')).toHaveLength(1));
  });
  it('lets someone with an allowing entry of their own make it private', async () => {
    const { fake, user } = setup([ow(1, ME.id, PERM.viewChannel, 0)]);
    await user.click(toggle());
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    await waitFor(() => expect(fake.callsOf('setChannelPrivate')).toHaveLength(1));
  });
  it('maps 403 and 404 refusals of the toggle', async () => {
    const { fake, user } = setup([ALLOW_MODS]);
    fake.handler = () => Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 }));
    await user.click(toggle());
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('You cannot change this.');
    fake.handler = () => Promise.reject(refusal({ code: 'E_NOT_FOUND', status: 404 }));
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('This channel no longer exists.'));
  });
  it('maps the worker’s own E_BAD_INPUT to the unread copy, and not to the hidden one', async () => {
    const { fake, user } = setup([ow(0, PLAYERS, 0, 0)]);
    fake.handler = () => Promise.reject(refusal({ code: 'E_BAD_INPUT' }));
    await user.click(disclosure('Players'));
    await user.click(within(screen.getByRole('radiogroup', { name: 'Send messages' })).getByRole('radio', { name: 'Deny' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not read who can see this channel.');
  });
  it('words an overwrite’s 400 E_INVALID_REQUEST by its code, not as the lockout', async () => {
    const { fake, user } = setup([ow(0, PLAYERS, 0, 0)]);
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 400 }));
    await user.click(disclosure('Players'));
    await user.click(within(screen.getByRole('radiogroup', { name: 'Send messages' })).getByRole('radio', { name: 'Deny' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not save the permissions (E_INVALID_REQUEST). Try again.');
  });
  it('maps a 400 that is not E_INVALID_REQUEST to the generic copy', async () => {
    const { fake, user } = setup([ow(0, PLAYERS, 0, 0)]);
    fake.handler = () => Promise.reject(refusal({ code: 'E_OTHER', status: 400 }));
    await user.click(disclosure('Players'));
    await user.click(within(screen.getByRole('radiogroup', { name: 'Send messages' })).getByRole('radio', { name: 'Deny' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not save the permissions (E_OTHER). Try again.');
  });
  it('says the read failed once the access is read but the overwrites are not, with no private toggle', () => {
    setup([], { overwritesLoaded: false });
    expect(screen.queryByRole('switch')).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('Could not read who can see this channel. Close this and open it again.');
    expect(screen.queryByText('No role or person has settings of their own here yet.')).toBeNull();
  });
  it('shows the loading line, never public or none, while the access is unread, absent, or the overwrites are absent', () => {
    for (const [access, without] of [[{ loaded: false, overwritesLoaded: false }, []], [{}, [`permissions:${SERVER}`]], [{}, [`overwrites:${SERVER}`]]] as const) {
      const { view } = setup([], access, [...without]);
      expect(screen.queryByRole('switch')).toBeNull();
      expect(screen.getByRole('status')).toHaveTextContent('Loading roles…');
      expect(screen.queryByText('No role or person has settings of their own here yet.')).toBeNull();
      view.unmount();
    }
  });
  it('applies two quick changes in order, the second reading what the first wrote', async () => {
    const { fake, user } = setup([ow(0, PLAYERS, 0, 0)]);
    let release: () => void = () => undefined;
    const first = new Promise<void>(r => { release = r; });
    fake.handler = async c => {
      if (c.m !== 'setOverwrite') return null;
      if (fake.callsOf('setOverwrite').length === 1) await first;
      fake.set(`overwrites:${SERVER}`, { [CH]: [ow(0, PLAYERS, c.allow, c.deny)] });
      return null;
    };
    await user.click(disclosure('Players'));
    await user.click(within(screen.getByRole('radiogroup', { name: 'Send messages' })).getByRole('radio', { name: 'Deny' }));
    await user.click(within(screen.getByRole('radiogroup', { name: 'Attach files' })).getByRole('radio', { name: 'Deny' }));
    expect(fake.callsOf('setOverwrite')).toHaveLength(1);
    release();
    await waitFor(() => expect(fake.callsOf('setOverwrite')).toHaveLength(2));
    expect(fake.callsOf('setOverwrite').map(c => c.deny)).toEqual([PERM.sendMessages, PERM.sendMessages | PERM.attachFiles]);
  });
  it('returns focus to the switch after a confirm that succeeds', async () => {
    const { fake, user } = setup([ALLOW_MODS]);
    fake.handler = c => {
      if (c.m === 'setChannelPrivate') fake.set(`overwrites:${SERVER}`, { [CH]: [ow(0, EVERYONE, 0, PERM.viewChannel)] });
      return Promise.resolve(null);
    };
    await user.click(toggle());
    await user.click(screen.getByRole('button', { name: 'Make private' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(toggle()).toHaveFocus();
  });
  it('moves focus to the new entry’s disclosure after add', async () => {
    const { user } = setup([ow(0, PLAYERS, 0, 0)]);
    await user.selectOptions(screen.getByRole('combobox', { name: 'Role or person' }), 'Bob');
    await user.click(screen.getByRole('button', { name: 'add a role or person' }));
    expect(disclosure('Bob')).toHaveFocus();
  });
  it('lists @everyone first, then roles highest first, then members', () => {
    setup([ow(1, BOB, 0, PERM.sendMessages), ow(0, PLAYERS, PERM.pinMessages, 0), ow(0, COUNCIL, 0, PERM.attachFiles), ow(0, EVERYONE, 0, PERM.viewChannel)]);
    const names = within(screen.getByRole('list', { name: 'Who has settings here' })).getAllByRole('button', { name: /^(@everyone|Council|Players|Bob)/ })
      .map(b => b.textContent);
    expect(names).toEqual(['@everyone', 'Council', 'Players', 'Bob member']);
  });
  it('says there are none with no entries', () => {
    setup();
    expect(screen.getByText('No role or person has settings of their own here yet.')).toBeInTheDocument();
  });
  it('shows only the channel bits in the tri grid, never the server-wide ones', async () => {
    const { user } = setup([ow(0, PLAYERS, 0, 0)]);
    await user.click(disclosure('Players'));
    expect(disclosure('Players')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('radiogroup', { name: 'View channels' })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Send messages' })).toBeInTheDocument();
    for (const hidden of ['kick members', 'ban members', 'manage roles', 'manage server', 'administrator', 'view audit log', 'manage nicknames', en['perm.moderate']]) {
      expect(screen.queryByRole('radiogroup', { name: hidden })).toBeNull();
    }
  });
  it('sends setOverwrite with the changed bit and announces it', async () => {
    const { fake, user, announce } = setup([ow(0, PLAYERS, PERM.pinMessages, 0)]);
    await user.click(disclosure('Players'));
    expect(within(screen.getByRole('radiogroup', { name: 'Pin messages' })).getByRole('radio', { name: 'Allow' })).toHaveAttribute('aria-checked', 'true');
    await user.click(within(screen.getByRole('radiogroup', { name: 'Send messages' })).getByRole('radio', { name: 'Deny' }));
    await waitFor(() => expect(fake.callsOf('setOverwrite')).toEqual([
      { m: 'setOverwrite', channelId: CH, kind: 0, targetId: PLAYERS, allow: PERM.pinMessages, deny: PERM.sendMessages }]));
    await waitFor(() => expect(announce).toHaveBeenCalledWith('Permissions of #lobby saved.'));
  });
  it('removes an entry with setOverwrite 0/0, and has no remove for @everyone', async () => {
    const { fake, user } = setup([ow(0, EVERYONE, 0, PERM.viewChannel), ow(0, PLAYERS, PERM.pinMessages, 0)]);
    fake.handler = c => {
      if (c.m === 'setOverwrite') fake.set(`overwrites:${SERVER}`, { [CH]: [ow(0, EVERYONE, 0, PERM.viewChannel)] });
      return Promise.resolve(null);
    };
    expect(screen.getAllByRole('button', { name: /^remove the settings for/ })).toHaveLength(1);
    await user.click(screen.getByRole('button', { name: 'remove the settings for Players' }));
    await waitFor(() => expect(fake.callsOf('setOverwrite')).toEqual([{ m: 'setOverwrite', channelId: CH, kind: 0, targetId: PLAYERS, allow: 0, deny: 0 }]));
    await waitFor(() => expect(screen.queryByRole('button', { name: /^Players/ })).toBeNull());
  });
  it('disables the rows of a role at or above mine, and bits I lack, with the forbidden reason', async () => {
    const { user } = setup([ow(0, COUNCIL, 0, 0), ow(0, PLAYERS, 0, 0)]);
    await user.click(disclosure('Council'));
    const sendGroup = screen.getByRole('radiogroup', { name: 'Send messages' });
    expect(sendGroup).toHaveAttribute('aria-disabled', 'true');
    expect(sendGroup).toHaveAccessibleDescription(expect.stringContaining('You cannot change this.'));
    await user.click(disclosure('Council'));
    await user.click(disclosure('Players'));
    expect(screen.getByRole('radiogroup', { name: 'Send messages' })).not.toHaveAttribute('aria-disabled');
    // I do not hold manage messages here, so I cannot set it.
    expect(screen.getByRole('radiogroup', { name: 'Manage messages' })).toHaveAttribute('aria-disabled', 'true');
  });
  it('offers the roles and people below me with no entry, and sends nothing until a value changes', async () => {
    const { fake, user } = setup([ow(0, PLAYERS, 0, 0)]);
    const select = screen.getByRole('combobox', { name: 'Role or person' });
    expect(within(select).getAllByRole('option').map(o => o.textContent)).toEqual(['@everyone', 'Bob']);
    await user.selectOptions(select, 'Bob');
    await user.click(screen.getByRole('button', { name: 'add a role or person' }));
    expect(disclosure('Bob')).toBeInTheDocument();
    expect(fake.callsOf('setOverwrite')).toEqual([]);
  });
  it('maps an unknown failure to its code', async () => {
    const { fake, user } = setup([ow(0, PLAYERS, 0, 0)]);
    fake.handler = () => Promise.reject(refusal({ code: 'E_NETWORK' }));
    await user.click(disclosure('Players'));
    await user.click(within(screen.getByRole('radiogroup', { name: 'Send messages' })).getByRole('radio', { name: 'Allow' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not save the permissions (E_NETWORK). Try again.');
  });
  it('has no serious axe violations with an entry open', async () => {
    const { view, user } = setup([ow(0, EVERYONE, 0, PERM.viewChannel), ow(0, PLAYERS, PERM.pinMessages, 0)]);
    await user.click(disclosure('Players'));
    await expectNoAxeViolations(view.container);
  });
});

describe('ChannelPermissions heading order', () => {
  it('puts the grid’s group headings under "who has settings here" (h4), as h5', async () => {
    const { user } = setup([ow(0, PLAYERS, 0, 0)]);
    await user.click(disclosure('Players'));
    const levels = screen.getAllByRole('heading').map(h => Number(h.tagName.slice(1)));
    expect(levels.slice(0, 2)).toEqual([3, 4]);
    expect(levels.slice(2).length).toBeGreaterThan(0);
    expect(levels.slice(2).every(l => l === 5)).toBe(true);
  });
});
