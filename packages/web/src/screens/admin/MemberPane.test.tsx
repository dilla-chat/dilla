import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { CommunityAccess, MemberSummary, RoleSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { MemberPane } from './MemberPane.tsx';
import { reasonProblem } from './Moderation.tsx';

const id = (n: number) => n.toString(16).padStart(2, '0').repeat(16);
const CID = id(9), OWNER = id(1), ME = id(2), BOB = id(3), CAL = id(4);
const MOD: RoleSummary = { id: id(10), name: 'mods', color: 0, position: 5, hoist: true, mentionable: false, allow: 0, deny: 0, members: 0 };
const m = (userId: string, username: string, roleIds: string[] = []): MemberSummary => ({ userId, username, display: '', kind: 0, roleIds });
const EVERYONE = 'ab'.repeat(16);
const access = (over: Partial<CommunityAccess> = {}): CommunityAccess => ({ top: 5, owner: false, manageCommunity: false, manageChannels: false,
  kickMembers: true, banMembers: true, createInvite: false, manageRoles: false, loaded: true, overwritesLoaded: true, everyoneId: EVERYONE, ...over });

function setup(acc: CommunityAccess | undefined = access()) {
  const fake = new FakeClient();
  fake.set('communities', [{ id: CID, name: 'Midgard', owner: OWNER }]);
  fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada', [MOD.id]), m(BOB, 'bob'), m(CAL, 'carol', [MOD.id])]);
  fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'online', [CAL]: 'online' });
  fake.set(`roles:${CID}`, [MOD]);
  if (acc) fake.set(`permissions:${CID}`, acc);
  const announce = vi.fn();
  const onMessage = vi.fn();
  const onCompare = vi.fn();
  const view = render(<div className="d-root"><CoreProvider client={fake}>
    <MemberPane communityId={CID} selfId={ME} onMessage={onMessage} onCompare={onCompare} announce={announce} /></CoreProvider></div>);
  return { fake, announce, onMessage, onCompare, view, user: userEvent.setup() };
}
/** A member's row button: its name starts with the presence word, then the display name (and the owner word). */
const row = (who: string) => screen.getByRole('button', { name: new RegExp(`\\b${who}\\b`) });
const card = () => screen.getByRole('dialog');
/** Task 9: the card's row (Message, then the glyph buttons by name). */
const cardActions = () => within(card()).queryAllByRole('button').filter(b => b.matches('.d-profile-card__action, .d-profile-card__icon-action'))
  .map(b => b.getAttribute('aria-label') ?? b.textContent);
/** Task 9: the moderation items of a row's person menu (right-click), which is closed again. */
function modActions(who: string): string[] {
  fireEvent.contextMenu(row(who), { clientX: 10, clientY: 10 });
  const menu = screen.getByRole('menu');
  const out = within(menu).getAllByRole('menuitem').filter(b => b.dataset.danger === 'true').map(b => b.textContent ?? '');
  fireEvent.keyDown(menu, { key: 'Escape' });
  return out;
}
/** Moderation is in the row's person menu (task 9, the prototype); message, compare and roles are on the card. */
async function pick(user: ReturnType<typeof userEvent.setup>, who: string, item: string) {
  const menu: Record<string, string> = { kick: `Kick ${who}`, ban: `Ban ${who}`, 'Text timeout': 'Text timeout', 'Warn member': 'Warn member' };
  const inMenu = menu[item];
  if (inMenu !== undefined) {
    fireEvent.contextMenu(row(who), { clientX: 10, clientY: 10 });
    await user.click(within(screen.getByRole('menu')).getByRole('menuitem', { name: inMenu }));
    return;
  }
  await user.click(row(who));
  await user.click(within(card()).getByRole('button', { name: item }));
}

describe('MemberPane', () => {
  it('restores a settings member row inside its own scope when the covered shell has a duplicate row', () => {
    const fake = new FakeClient();
    fake.set('communities', [{ id: CID, name: 'Midgard', owner: OWNER }]);
    fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada'), m(BOB, 'bob'), m(CAL, 'carol')]);
    fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'online', [CAL]: 'online' });
    fake.set(`roles:${CID}`, []);
    const background = document.createElement('button');
    background.dataset.focusKey = `member:${BOB}`;
    background.textContent = 'covered bob';
    const host = document.createElement('div');
    document.body.append(background, host);
    const view = render(<div className="d-root"><CoreProvider client={fake}><MemberPane communityId={CID} selfId={ME}
      onMessage={vi.fn()} onCompare={vi.fn()} announce={vi.fn()} focusScope="settings" /></CoreProvider></div>, { container: host });
    const scoped = host.querySelector<HTMLElement>('.d-root > div')!;
    const before = within(scoped).getByRole('button', { name: /\bbob\b/ });
    act(() => before.focus());
    act(() => fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'offline', [CAL]: 'online' }));
    expect(within(scoped).getByRole('button', { name: /\bbob\b/ })).toHaveFocus();
    expect(background).not.toHaveFocus();
    view.unmount(); background.remove(); host.remove();
  });
  // S3: the members aside has its own 48 bar (MemberList's `label`), so its hairline sits on the header's line.
  it('draws the members bar, a heading named for the list', () => {
    setup();
    const bar = screen.getByRole('heading', { level: 2, name: 'Members' });
    expect(bar.closest('.d-member-list__bar')).not.toBeNull();
    expect(screen.getByRole('region', { name: 'Members' })).toContainElement(bar);
  });
  it('waits for presence, then places offline members at the bottom with a spoken label', () => {
    const { fake } = setup();
    act(() => fake.set(`presence:${CID}`, null));
    expect(screen.getByRole('status')).toHaveTextContent('Loading members');
    act(() => fake.set(`presence:${CID}`, { [OWNER]: 'offline', [ME]: 'online', [BOB]: 'dnd', [CAL]: 'idle' }));
    expect(screen.getByRole('heading', { name: 'offline — 1' })).toBeInTheDocument();
    expect(row('bob')).toHaveAccessibleName('bob, do not disturb');
  });
  it('orders a hoisted role section, then online, then offline, each with its count', () => {
    const { fake } = setup();
    act(() => fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'offline', [CAL]: 'online' }));
    const titles = screen.getAllByRole('heading', { level: 3 }).map(h => h.textContent);
    expect(titles).toEqual(['mods — 2', 'online — 1', 'offline — 1']);
  });
  it('keeps keyboard focus on a member row when presence moves it', () => {
    const { fake } = setup();
    const before = row('bob');
    act(() => before.focus());
    act(() => fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'offline', [CAL]: 'online' }));
    const after = row('bob');
    expect(after).not.toBe(before);
    expect(after).toHaveFocus();
  });
  it('lists groups with a crown for the owner and no tags: owner is a spoken word, "you" is not in the list', async () => {
    const { view } = setup();
    expect(screen.getByRole('heading', { name: 'mods — 2' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'online — 2' })).toBeInTheDocument();
    expect(row('olof')).toHaveAccessibleName('olof, owner, online');
    expect(screen.queryByText('you')).toBeNull();
    await expectNoAxeViolations(view.container);
  });
  it('a click opens the card with the member, the roles in order with their colours, and the right actions', async () => {
    const { user } = setup();
    expect(screen.queryByRole('dialog')).toBeNull();
    await user.click(row('carol'));
    expect(card()).toHaveAccessibleName('carol');
    expect(card().querySelector('.d-profile-card__status')).toHaveTextContent('@carol');
    expect(within(card()).getByRole('list', { name: 'roles' })).toHaveTextContent('mods');
    expect(cardActions()).toEqual(['Message', 'compare safety numbers']);
    expect(row('carol')).toHaveAttribute('aria-expanded', 'true');
  });
  it('lists several roles highest first with their colour swatches', async () => {
    const { user, fake } = setup();
    const red: RoleSummary = { ...MOD, id: id(11), name: 'reds', color: 0xE5484D, position: 7, hoist: false };
    act(() => {
      fake.set(`roles:${CID}`, [MOD, red]);
      fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada', [MOD.id]), m(BOB, 'bob', [MOD.id, red.id]), m(CAL, 'carol')]);
    });
    await user.click(row('bob'));
    const items = within(within(card()).getByRole('list', { name: 'roles' })).getAllByRole('listitem');
    expect(items.map(i => i.textContent)).toEqual(['reds', 'mods']);
    expect(items[0]?.querySelector('[style*="background"], .d-role-dot')).not.toBeNull();
  });
  // Task 9: the prototype's card names the owner with the crown after the name and draws no "you" mark.
  it('shows the owner crown on the owner card only', async () => {
    const { user } = setup();
    await user.click(row('ada'));
    expect(within(card()).queryByRole('img', { name: 'owner' })).toBeNull();
    await user.click(row('olof'));
    expect(within(card()).getByRole('img', { name: 'owner' })).toBeInTheDocument();
    await user.click(row('bob'));
    expect(within(card()).queryByRole('img', { name: 'owner' })).toBeNull();
  });
  it('the viewer\'s own card has no message, compare, kick or ban', async () => {
    const { user } = setup(access({ manageRoles: true }));
    await user.click(row('ada'));
    expect(cardActions()).toEqual(['edit roles']);
  });
  it('offers kick and ban on bob, and none on the owner or an equal rank', async () => {
    const { user } = setup();
    await user.click(row('bob'));
    expect(cardActions()).toEqual(['Message', 'compare safety numbers']);
    await user.keyboard('{Escape}');
    expect(modActions('bob')).toEqual(['Kick bob', 'Ban bob']);
    await user.click(row('olof'));
    expect(cardActions()).toEqual(['Message', 'compare safety numbers']);
    await user.keyboard('{Escape}');
    expect(modActions('olof')).toEqual([]);
    expect(modActions('carol')).toEqual([]);
  });
  it('a member without the kick permission sees no kick, and without ban no ban', () => {
    setup(access({ kickMembers: false }));
    expect(modActions('bob')).toEqual(['Ban bob']);
  });
  it('shows no kick or ban until permissions have loaded', async () => {
    const { user } = setup(access({ loaded: false }));
    await user.click(row('bob'));
    expect(cardActions()).toEqual(['Message', 'compare safety numbers']);
    await user.keyboard('{Escape}');
    expect(modActions('bob')).toEqual([]);
  });
  it('clicking the open member again closes the card, another row switches it, Escape closes and returns focus', async () => {
    const { user } = setup();
    await user.click(row('bob'));
    await user.click(row('bob'));
    expect(screen.queryByRole('dialog')).toBeNull();
    await user.click(row('bob'));
    await user.click(row('carol'));
    expect(card()).toHaveAccessibleName('carol');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(row('carol')).toHaveFocus();
  });
  // Task 9: a right-click opens the prototype's person menu in place of the card; Enter on the open row keeps the card.
  it('Enter on the open member\'s row keeps the card open (only a primary click toggles); a right-click swaps it for the person menu', async () => {
    const { user } = setup();
    await user.click(row('bob'));
    act(() => row('bob').focus());
    await user.keyboard('{Enter}');
    expect(card()).toHaveAccessibleName('bob');
    fireEvent.contextMenu(row('bob'), { clientX: 10, clientY: 10 });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('menu', { name: 'actions for bob' })).toBeInTheDocument();
  });
  it('I1: a card follows its member\'s row when a presence change re-mounts it, and a click on the new row closes it', async () => {
    const { user, fake } = setup();
    await user.click(row('bob'));
    const old = row('bob');
    act(() => fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'offline', [CAL]: 'online' }));
    expect(old.isConnected).toBe(false);
    await waitFor(() => expect(row('bob')).toHaveAttribute('aria-expanded', 'true'));
    expect(card()).toHaveAccessibleName('bob');
    await user.click(row('bob'));
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('I2: a member who leaves closes the card, and a rejoin does not bring it back', async () => {
    const { user, fake } = setup();
    await user.click(row('bob'));
    act(() => fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada', [MOD.id]), m(CAL, 'carol', [MOD.id])]));
    expect(screen.queryByRole('dialog')).toBeNull();
    act(() => fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada', [MOD.id]), m(BOB, 'bob'), m(CAL, 'carol', [MOD.id])]));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(row('bob')).toHaveAttribute('aria-expanded', 'false');
  });
  it('Enter on a focused row opens the card; Shift+F10 opens the person menu, whose Profile opens the card', async () => {
    const { user } = setup();
    act(() => row('bob').focus());
    await user.keyboard('{Enter}');
    expect(card()).toHaveAccessibleName('bob');
    await user.keyboard('{Escape}');
    act(() => row('carol').focus());
    await user.keyboard('{Shift>}{F10}{/Shift}');
    const menu = screen.getByRole('menu', { name: 'actions for carol' });
    expect(within(menu).getAllByRole('menuitem').map(b => b.textContent)).toEqual(['Profile', 'Message', 'Copy username']);
    await user.click(within(menu).getByRole('menuitem', { name: 'Profile' }));
    expect(card()).toHaveAccessibleName('carol');
  });
  it('has no axe violations with the card open', async () => {
    const { user } = setup();
    await user.click(row('bob'));
    await expectNoAxeViolations(document.body);
  });
  it('hands message and compare to the shell', async () => {
    const { user, onMessage, onCompare } = setup();
    await pick(user, 'bob', 'Message');
    await pick(user, 'bob', 'compare safety numbers');
    expect(onMessage).toHaveBeenCalledWith(BOB);
    expect(onCompare).toHaveBeenCalledWith(BOB);
  });
});

describe('role assignment', () => {
  it('offers roles only with manageRoles, opens the dialog and closes it with done', async () => {
    const { user } = setup(access({ manageRoles: true }));
    await user.click(row('bob'));
    expect(cardActions()).toEqual(['Message', 'compare safety numbers', 'edit roles']);
    await user.click(within(card()).getByRole('button', { name: 'edit roles' }));
    const dialog = screen.getByRole('dialog', { name: 'Roles of bob' });
    expect(within(dialog).getByRole('checkbox', { name: 'mods' })).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });
  it('has no roles item without manageRoles', async () => {
    const { user } = setup();
    await user.click(row('bob'));
    expect(within(card()).queryByRole('button', { name: 'edit roles' })).toBeNull();
  });
});

describe('KickFlow', () => {
  it('kicks, announces and closes', async () => {
    const { user, fake, announce } = setup();
    await pick(user, 'bob', 'kick');
    const dialog = screen.getByRole('dialog', { name: 'Kick bob from Midgard?' });
    await user.click(within(dialog).getByRole('button', { name: 'Kick' }));
    await waitFor(() => expect(announce).toHaveBeenCalledWith('bob was kicked.'));
    expect(fake.callsOf('kickMember')).toEqual([{ m: 'kickMember', communityId: CID, userId: BOB }]);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });
  it.each([
    ['E_FORBIDDEN', 'You cannot kick bob. Their highest role is at or above yours, or your roles do not allow kicking.'],
    ['E_NOT_FOUND', 'bob is no longer in Midgard.'],
    ['E_NETWORK', 'That did not work (E_NETWORK). Try again.'],
  ])('maps %s to its copy and keeps the dialog open', async (code, text) => {
    const { user, fake } = setup();
    fake.handler = () => Promise.reject(refusal({ code }));
    await pick(user, 'bob', 'kick');
    await user.click(screen.getByRole('button', { name: 'Kick' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
  });
});

describe('focus and reopening', () => {
  it('Cancel returns focus to the row trigger', async () => {
    const { user } = setup();
    await pick(user, 'bob', 'kick');
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(row('bob')).toHaveFocus());
  });
  it('a successful kick leaves focus on the member region, not body', async () => {
    const { user, fake } = setup();
    await pick(user, 'bob', 'kick');
    fake.handler = () => { fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada', [MOD.id]), m(CAL, 'carol', [MOD.id])]); return Promise.resolve(null); };
    await user.click(screen.getByRole('button', { name: 'Kick' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await waitFor(() => expect(document.body).not.toHaveFocus());
    expect(document.activeElement).toBe(document.querySelector('.d-member-list')?.parentElement);
  });
  it('reopening on another member does not show the busy label of the closed dialog', async () => {
    const { user, fake } = setup();
    fake.handler = () => new Promise(() => undefined);
    await pick(user, 'bob', 'kick');
    await user.click(screen.getByRole('button', { name: 'Kick' }));
    expect(screen.getByRole('button', { name: 'Kicking…' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await pick(user, 'bob', 'kick');
    expect(screen.getByRole('button', { name: 'Kick' })).toBeEnabled();
  });
});

describe('BanFlow', () => {
  it('sends the reason and a day by default', async () => {
    const { user, fake, announce } = setup();
    await pick(user, 'bob', 'ban');
    await user.type(screen.getByRole('textbox', { name: 'Reason' }), 'spam');
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    await waitFor(() => expect(announce).toHaveBeenCalledWith('bob was banned.'));
    expect(fake.callsOf('banMember')).toEqual([{ m: 'banMember', communityId: CID, userId: BOB, reason: 'spam', seconds: 86400 }]);
  });
  it('sends the chosen length: a week, and forever as null', async () => {
    const { user, fake } = setup();
    await pick(user, 'bob', 'ban');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Ban length' }), '7 days');
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    await waitFor(() => expect(fake.callsOf('banMember')).toHaveLength(1));
    expect(fake.callsOf('banMember')[0]?.seconds).toBe(604800);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await pick(user, 'bob', 'ban');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Ban length' }), 'until lifted');
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    await waitFor(() => expect(fake.callsOf('banMember')).toHaveLength(2));
    expect(fake.callsOf('banMember')[1]?.seconds).toBeNull();
  });
  it('refuses a long or control-character reason locally, with no call', async () => {
    const { user, fake } = setup();
    await pick(user, 'bob', 'ban');
    const reason = screen.getByRole('textbox', { name: 'Reason' });
    await user.click(reason);
    await user.paste('x'.repeat(513));
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('That reason is too long. A reason is up to 512 characters (fewer for some letters).');
    await user.clear(reason);
    await user.paste('bad\u0007bell');
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('A reason cannot hold control characters');
    expect(fake.callsOf('banMember')).toHaveLength(0);
  });
  it.each([
    [{ code: 'E_FORBIDDEN' }, 'You cannot ban bob.'],
    [{ code: 'E_BAD_INPUT', detail: 'unknown community', status: 400 }, 'That did not work (E_BAD_INPUT). Try again.'],
    [{ code: 'E_NOT_FOUND', status: 404 }, 'That did not work (E_NOT_FOUND). Try again.'],
    [{ code: 'E_INVALID_REQUEST', status: 400 }, 'The ban length was refused.'],
    [{ code: 'E_NETWORK' }, 'That did not work (E_NETWORK). Try again.'],
  ])('maps %o to its copy', async (init, text) => {
    const { user, fake } = setup();
    fake.handler = () => Promise.reject(refusal(init));
    await pick(user, 'bob', 'ban');
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
  });
  it('refuses a reason by its bytes: 257 x e-acute is too long, 200 passes', async () => {
    const { user, fake } = setup();
    await pick(user, 'bob', 'ban');
    const reason = screen.getByRole('textbox', { name: 'Reason' });
    await user.click(reason);
    await user.paste('é'.repeat(257));
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('too long');
    expect(fake.callsOf('banMember')).toHaveLength(0);
    await user.clear(reason);
    await user.paste('é'.repeat(200));
    await user.click(screen.getByRole('button', { name: 'Ban' }));
    await waitFor(() => expect(fake.callsOf('banMember')).toHaveLength(1));
  });
  it('counts a carriage return as a control character', () => {
    expect(reasonProblem('a\rb')).toBe('control');
    expect(reasonProblem('a\nb\tc')).toBeNull();
  });
  it('one call for a double click while the first is pending', async () => {
    const { user, fake } = setup();
    fake.handler = () => new Promise(() => undefined);
    await pick(user, 'bob', 'ban');
    const ban = screen.getByRole('button', { name: 'Ban' });
    // Both clicks land before React renders the disabled button.
    act(() => { ban.click(); ban.click(); });
    expect(fake.callsOf('banMember')).toHaveLength(1);
  });
  it('has no axe violations open', async () => {
    const { user } = setup();
    await pick(user, 'bob', 'ban');
    await expectNoAxeViolations(document.body);
  });
});

describe('TimeoutFlow', () => {
  it('sets a bounded text timeout and offers a separate clear action', async () => {
    const { user, fake, announce } = setup(access({ moderateMembers: true }));
    const before = Math.floor(Date.now() / 1000);
    await pick(user, 'bob', 'Text timeout');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(fake.callsOf('setMemberTimeout')).toHaveLength(1));
    const set = fake.callsOf('setMemberTimeout')[0];
    expect(set).toMatchObject({ m: 'setMemberTimeout', communityId: CID, userId: BOB });
    expect(set?.requestId).toMatch(/^[0-9a-f]{32}$/);
    expect(Number(set?.expires)).toBeGreaterThanOrEqual(before + 600);
    expect(Number(set?.expires)).toBeLessThanOrEqual(Math.floor(Date.now() / 1000) + 600);
    expect(announce).toHaveBeenCalledWith('Text timeout updated for bob.');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await pick(user, 'bob', 'Text timeout');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Action' }), 'Clear existing timeout');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(fake.callsOf('clearMemberTimeout')).toHaveLength(1));
    expect(fake.callsOf('clearMemberTimeout')[0]).toMatchObject({ communityId: CID, userId: BOB });
  });
  it('warns an eligible lower-ranked member with a reason and reports the case receipt', async () => {
    const { fake, user, announce } = setup(access({ moderateMembers: true }));
    fake.handler = command => command.m === 'warnMember'
      ? Promise.resolve({ requestId: '12'.repeat(16), actionId: id(44), revision: '9', acceptedAt: 1_800_000_000, evidence: 'accepted' })
      : Promise.resolve(null);
    await pick(user, 'bob', 'Warn member');
    await user.type(screen.getByRole('textbox', { name: 'Reason' }), 'Please follow the channel rules.');
    await user.click(screen.getByRole('button', { name: 'Warn' }));
    await waitFor(() => expect(fake.callsOf('warnMember')).toHaveLength(1));
    expect(fake.callsOf('warnMember')[0]).toMatchObject({ userId: BOB, reason: 'Please follow the channel rules.' });
    expect(announce).toHaveBeenCalledWith('Warning sent to the moderation log for bob.');
  });
});

describe('role assignment focus', () => {
  it('returns focus when the member list reloads while the dialog closes', async () => {
    const { user, fake } = setup(access({ manageRoles: true, top: 6 }));
    await pick(user, 'bob', 'edit roles');
    act(() => fake.set(`presence:${CID}`, null));
    expect(screen.getByRole('status')).toHaveTextContent('Loading members');
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    act(() => fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'online', [CAL]: 'online' }));
    expect(row('bob')).toHaveFocus();
  });
  it('Done after a grant that moves the member into a hoisted group returns focus to his re-mounted menu trigger', async () => {
    const { user, fake } = setup(access({ manageRoles: true, top: 6 }));
    fake.handler = c => {
      if (c.m === 'grantRole') fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada', [MOD.id]), m(BOB, 'bob', [MOD.id]), m(CAL, 'carol', [MOD.id])]);
      return Promise.resolve(null);
    };
    const before = row('bob');
    await pick(user, 'bob', 'edit roles');
    await user.click(within(screen.getByRole('dialog')).getByRole('checkbox', { name: 'mods' }));
    await waitFor(() => expect(row('bob')).not.toBe(before));
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(row('bob')).toHaveFocus();
  });
});

describe('task 9 fix round 1', () => {
  it('the person menu offers Mention only with a composer to write in, and hands the username over', async () => {
    const fake = new FakeClient();
    fake.set('communities', [{ id: CID, name: 'Midgard', owner: OWNER }]);
    fake.set(`members:${CID}`, [m(OWNER, 'olof'), m(ME, 'ada', [MOD.id]), m(BOB, 'bob'), m(CAL, 'carol', [MOD.id])]);
    fake.set(`presence:${CID}`, { [OWNER]: 'online', [ME]: 'online', [BOB]: 'online', [CAL]: 'online' });
    fake.set(`roles:${CID}`, [MOD]);
    fake.set(`permissions:${CID}`, access());
    const onMention = vi.fn();
    const pane = (mention?: (u: string) => void) => <div className="d-root"><CoreProvider client={fake}>
      <MemberPane communityId={CID} selfId={ME} onMessage={vi.fn()} onCompare={vi.fn()} announce={vi.fn()} onMention={mention} /></CoreProvider></div>;
    const view = render(pane());
    const user = userEvent.setup();
    expect(modActions('bob')).toEqual(['Kick bob', 'Ban bob']);
    fireEvent.contextMenu(row('bob'), { clientX: 10, clientY: 10 });
    expect(within(screen.getByRole('menu')).queryByRole('menuitem', { name: 'Mention' })).toBeNull();
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    view.rerender(pane(onMention));
    fireEvent.contextMenu(row('bob'), { clientX: 10, clientY: 10 });
    await user.click(within(screen.getByRole('menu')).getByRole('menuitem', { name: 'Mention' }));
    expect(onMention).toHaveBeenCalledExactlyOnceWith('bob');
    fireEvent.contextMenu(row('ada'), { clientX: 10, clientY: 10 });
    expect(within(screen.getByRole('menu')).queryByRole('menuitem', { name: 'Mention' })).toBeNull();
  });
  it('the card carries the WEB tag for a person', async () => {
    const { user } = setup();
    await user.click(row('bob'));
    expect(card().querySelector('.d-profile-card__client')).toHaveTextContent('WEB');
  });
});
