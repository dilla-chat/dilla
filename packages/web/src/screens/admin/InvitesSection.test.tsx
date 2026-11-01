import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { Command, CommunityAccess, InviteSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { InvitesSection } from './InvitesSection.tsx';

const SERVER = 'a1'.repeat(16);
const BOB = '28'.repeat(16);
const INV1 = 'f1'.repeat(16);
const INV2 = 'f2'.repeat(16);
const INV3 = 'f3'.repeat(16);
const NOW = Math.floor(Date.now() / 1000);

const INVITES: InviteSummary[] = [
  { id: INV1, createdBy: BOB, maxUses: 10, uses: 3, created: NOW - 60, expires: NOW + 5 * 86_400 + 600 },
  { id: INV2, createdBy: ME.id, maxUses: 1, uses: 0, created: NOW - 60, expires: NOW + 3 * 3600 + 60 },
  // Task 10c: "∞" and "never" are 0 on the wire.
  { id: INV3, createdBy: ME.id, maxUses: 0, uses: 12, created: NOW - 60, expires: 0 },
];

const EVERYONE = 'ab'.repeat(16);
const access = (over: Partial<CommunityAccess> = {}): CommunityAccess => ({ top: 0, owner: false, manageCommunity: false, manageChannels: false,
  kickMembers: false, banMembers: false, createInvite: true, manageRoles: false, loaded: true, overwritesLoaded: true, everyoneId: EVERYONE, ...over });

function setup(options: { invites?: InviteSummary[] | null; handler?: (command: Command) => Promise<unknown>; manager?: boolean } = {}) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: SERVER, name: 'Midgard', owner: ME.id }]);
  fake.set(`members:${SERVER}`, [
    { userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] },
    { userId: BOB, username: 'bob', display: 'Bob B', kind: 0, roleIds: [] },
  ]);
  if (options.invites !== null) fake.set(`invites:${SERVER}`, options.invites ?? INVITES);
  fake.set(`permissions:${SERVER}`, access({ manageCommunity: options.manager ?? true }));
  fake.handler = options.handler ?? (command => Promise.resolve(command.m === 'createInvite'
    ? { url: 'https://dilla.test/i/abc', code: 'abc', expires: 0 } : null));
  const announce = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}>
    <InvitesSection communityId={SERVER} serverName="Midgard" announce={announce} />
  </CoreProvider></div>);
  return { fake, user, view, announce };
}
const make = (user: ReturnType<typeof userEvent.setup>) => user.click(screen.getByRole('button', { name: en['serverSettings.invites.make'] }));
const radio = (group: string, name: string) => within(screen.getByRole('radiogroup', { name: group })).getByRole('radio', { name });
const expiresGroup = en['serverSettings.invites.expires'];
const usesGroup = en['serverSettings.invites.uses'];

describe('the invites section (Signal task 10c)', () => {
  it('reads the live list when it opens', async () => {
    const { fake } = setup();
    await waitFor(() => expect(fake.callsOf('loadInvites')).toEqual([{ m: 'loadInvites', communityId: SERVER }]));
  });
  it('offers the prototype options; "never" and "∞" only to someone who manages the server', () => {
    setup();
    expect(within(screen.getByRole('radiogroup', { name: expiresGroup })).getAllByRole('radio').map(r => r.textContent)).toEqual(['1 hour', '1 day', '7 days', 'never']);
    expect(within(screen.getByRole('radiogroup', { name: usesGroup })).getAllByRole('radio').map(r => r.textContent)).toEqual(['1', '10', '∞']);
    expect(radio(expiresGroup, '7 days')).toHaveAttribute('aria-checked', 'true');
    expect(radio(usesGroup, '10')).toHaveAttribute('aria-checked', 'true');
  });
  it('a member without Manage server sees and makes only the finite links', () => {
    setup({ manager: false });
    expect(within(screen.getByRole('radiogroup', { name: expiresGroup })).getAllByRole('radio').map(r => r.textContent)).toEqual(['1 hour', '1 day', '7 days']);
    expect(within(screen.getByRole('radiogroup', { name: usesGroup })).getAllByRole('radio').map(r => r.textContent)).toEqual(['1', '10']);
  });
  it('makes a link for 7 days and 10 uses by default and shows it once with its note', async () => {
    const { fake, user } = setup();
    await make(user);
    await waitFor(() => expect(fake.callsOf('createInvite')).toEqual([{ m: 'createInvite', communityId: SERVER, maxUses: 10, ttlSeconds: 604_800 }]));
    expect(await screen.findByRole('textbox', { name: en['serverSettings.invites.link'] })).toHaveValue('https://dilla.test/i/abc');
    expect(screen.getByText('It can be used 10 times and expires in 7 days. The link is shown only once.')).toBeInTheDocument();
  });
  it.each([
    ['1 hour', '1', 3600, 1, 'It can be used 1 time and expires in 1 hour.'],
    ['1 day', '10', 86_400, 10, 'It can be used 10 times and expires in 1 day.'],
    ['never', '∞', 0, 0, 'It can be used any number of times and expires never.'],
  ] as const)('maps %s and %s uses to ttl %i and %i uses', async (ttlLabel, usesLabel, ttl, uses, note) => {
    const { fake, user } = setup();
    await user.click(radio(expiresGroup, ttlLabel));
    await user.click(radio(usesGroup, usesLabel));
    await make(user);
    await waitFor(() => expect(fake.callsOf('createInvite')[0]).toMatchObject({ maxUses: uses, ttlSeconds: ttl }));
    expect(await screen.findByText(new RegExp(note.replace('.', '\\.')))).toBeInTheDocument();
  });
  it('Copy copies the link, says so and files it away', async () => {
    const { user, announce } = setup();
    await make(user);
    await user.click(await screen.findByRole('button', { name: en['serverSettings.invites.copy'] }));
    await expect(navigator.clipboard.readText()).resolves.toBe('https://dilla.test/i/abc');
    await waitFor(() => expect(announce).toHaveBeenCalledWith(en['serverSettings.invites.copied']));
    expect(screen.queryByRole('textbox', { name: en['serverSettings.invites.link'] })).toBeNull();
    expect(screen.getByRole('button', { name: en['serverSettings.invites.make'] })).toBeInTheDocument();
  });
  it('says a refused make by its copy', async () => {
    const { user } = setup({ handler: c => (c.m === 'createInvite' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : Promise.resolve(null)) });
    await make(user);
    expect(await screen.findByRole('alert')).toHaveTextContent(en['invite.error.forbidden']);
  });
  it('lists the live links: masked code, uses (∞) and expiry (never)', () => {
    setup();
    expect(screen.getByRole('heading', { name: 'Active links — 3' })).toBeInTheDocument();
    const rows = within(screen.getByRole('list', { name: en['admin.invites.title'] })).getAllByRole('listitem');
    expect(rows.map(r => r.textContent)).toEqual([
      `${window.location.host}/i/••••3 / 10 usesexpires in 5 daysRevoke`,
      `${window.location.host}/i/••••0 / 1 usesexpires in 3 hoursRevoke`,
      `${window.location.host}/i/••••12 / ∞ usesexpires neverRevoke`,
    ]);
  });
  it('revokes a link and announces it', async () => {
    const { fake, user, announce } = setup();
    await user.click(screen.getByRole('button', { name: 'Revoke the link by Bob B that expires in 5 days' }));
    await waitFor(() => expect(fake.callsOf('revokeInvite')).toEqual([{ m: 'revokeInvite', communityId: SERVER, inviteId: INV1 }]));
    await waitFor(() => expect(announce).toHaveBeenCalledWith(en['admin.invites.revoked']));
  });
  it('says a revoke refusal (403) by its copy', async () => {
    const { user } = setup({ handler: c => (c.m === 'revokeInvite' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : Promise.resolve(null)) });
    await user.click(screen.getByRole('button', { name: 'Revoke the link by Bob B that expires in 5 days' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.invites.error.forbidden']);
  });
  it('without Manage server only the links this account made offer revoke', () => {
    setup({ manager: false });
    expect(screen.getAllByRole('button', { name: /^Revoke the link by / }).map(b => b.getAttribute('aria-label')))
      .toEqual(['Revoke the link by ada that expires in 3 hours', 'Revoke the link by ada that expires never']);
  });
  it('calls a maker who is no longer a member a former member', () => {
    setup({ invites: [{ id: INV1, createdBy: 'ab'.repeat(16), maxUses: 10, uses: 0, created: 1, expires: 0 }] });
    expect(screen.getByRole('button', { name: `Revoke the link by ${en['admin.member.former']} that expires never` })).toBeInTheDocument();
  });
  it('sends one createInvite when make is pressed again before the first answers', async () => {
    const { fake, user } = setup();
    let release: (v: unknown) => void = () => undefined;
    fake.handler = command => (command.m === 'createInvite' ? new Promise(r => { release = r; }) : Promise.resolve(null));
    await make(user);
    await user.click(screen.getByRole('button', { name: en['serverSettings.invites.making'] }));
    expect(fake.callsOf('createInvite')).toHaveLength(1);
    release({ url: 'https://dilla.test/i/z', code: 'z', expires: 0 });
    await screen.findByDisplayValue('https://dilla.test/i/z');
  });
  it('has no axe violations', async () => {
    const { view, user } = setup();
    await make(user);
    await screen.findByRole('textbox', { name: en['serverSettings.invites.link'] });
    await expectNoAxeViolations(view.container);
  });
});
