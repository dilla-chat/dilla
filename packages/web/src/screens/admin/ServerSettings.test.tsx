import { describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { PhoneLayoutProvider } from '@dilla/ui';
import * as layout from '../../layout.ts';
import { PERM, type ChannelSummary, type CommunityAccess, type MemberSummary, type RoleSummary } from '@dilla/client-core';
import { encode, fromHex } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { ServerSettings, type AdminTab } from './ServerSettings.tsx';

const SERVER = 'a1'.repeat(16);
const OTHER_SERVER = 'b2'.repeat(16);
const BOB = '28'.repeat(16);
const CAROL = '3c'.repeat(16);
const NAME = 'Björn';
const NFD = 'Björn';
type FakeInstallation = { id: string; communityId: string; state: number; revision: string; userId: string | null; deviceId: string | null;
  capabilities: number; updatedAt: string };

export const FULL: CommunityAccess = { top: 100, owner: true, manageCommunity: true, manageChannels: true, kickMembers: true,
  banMembers: true, createInvite: true, manageRoles: true, loaded: true, overwritesLoaded: true, everyoneId: 'ab'.repeat(16) };
const NONE: CommunityAccess = { ...FULL, top: 0, owner: false, manageCommunity: false, manageChannels: false, kickMembers: false,
  banMembers: false, createInvite: false, manageRoles: false };

function setup(access: CommunityAccess | undefined, over: { name?: string; tab?: AdminTab | null; members?: MemberSummary[] | null;
  missingCommunity?: boolean; phone?: boolean;
  selfRoles?: { id: string; name: string; position: number; selfSelected: boolean }[] | ((communityId: string) => { id: string; name: string; position: number; selfSelected: boolean }[]);
  roles?: RoleSummary[]; channels?: ChannelSummary[]; installation?: FakeInstallation | null | ((communityId: string) => FakeInstallation | null);
  interaction?: boolean | (() => boolean); upload?: 'accepted' | 'pending' | 'unknown';
  uploadHandler?: (requestId: string) => Promise<unknown> } = {}) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', over.missingCommunity ? [] : [{ id: SERVER, name: over.name ?? NAME, owner: access?.owner === true ? ME.id : BOB }]);
  fake.handler = command => Promise.resolve(command.m === 'listSelfRoles' ? (typeof over.selfRoles === 'function' ? over.selfRoles(command.communityId) : over.selfRoles ?? [])
    : command.m === 'getBotInstallation' ? (typeof over.installation === 'function' ? over.installation(command.communityId) : over.installation ?? null)
    : command.m === 'humanInteractionRequest' ? over.uploadHandler?.(command.requestId) ?? { requestId: command.requestId, upload: over.upload ?? 'accepted' }
    : command.m === 'readBotInteractionResponses' ? (() => {
      const sent = fake.callsOf('humanInteractionRequest').at(-1);
      const sentChannel = sent?.args[0];
      const interaction = over.interaction;
      const responseReady = typeof interaction === 'function' ? interaction() : interaction === true;
      return { through: '1', blocked: false, responses: responseReady && sent !== undefined && typeof sentChannel === 'string' ? [{
        deliveryId: '1', requestId: sent.requestId, communityId: SERVER, operation: 31, channelId: null, status: 0,
        revision: '1', data: encode([fromHex(sentChannel), 9n]), sourceSeq: '1',
      }] : [] };
    })()
    : command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
  if (access !== undefined) fake.set(`permissions:${SERVER}`, access);
  if (over.roles !== undefined) fake.set(`roles:${SERVER}`, over.roles);
  if (over.channels !== undefined) fake.set(`channels:${SERVER}`, over.channels);
  if (over.members !== null) {
    fake.set(`members:${SERVER}`, over.members ?? [
      { userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] },
      { userId: BOB, username: 'bob', display: 'Bob B', kind: 0, roleIds: [] },
      { userId: CAROL, username: 'carol', display: '', kind: 0, roleIds: [] },
    ]);
  }
  fake.set(`presence:${SERVER}`, { [ME.id]: 'online', [BOB]: 'online', [CAROL]: 'online' });
  fake.set('removals', []);
  const onClose = vi.fn();
  const onTab = vi.fn();
  const announce = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><PhoneLayoutProvider value={over.phone ?? false}><CoreProvider client={fake}>
    <ServerSettings communityId={SERVER} tab={over.tab === undefined ? 'overview' : over.tab} onTab={onTab} onClose={onClose} announce={announce} />
  </CoreProvider></PhoneLayoutProvider></div>);
  return { fake, user, view, onClose, onTab, announce };
}
/** The pane beside the tab list: "delete server" is both a tab and a button, so a query for the button looks only here. */
const pane = () => within(document.querySelector<HTMLElement>('.d-settings-frame__body')!);
// Signal task 10 fix round 1: one workspace nav, "You" then this server; the server's own items are the ones after "You".
const USER_ITEMS = new Set(['profile', 'signin', 'devices', 'notifications', 'voice', 'keyboard', 'appearance', 'privacy', 'diagnostics']
  .map(id => (en as Readonly<Record<string, string>>)[`workspace.${id}`]));
const navNames = () => within(screen.getByRole('navigation', { name: en['admin.nav'] })).queryAllByRole('button')
  .map(b => b.textContent).filter(name => !USER_ITEMS.has(name ?? ''));

describe('ServerSettings tabs', () => {
  it('keeps loading feedback and the live announcer accessible on the phone index', () => {
    const phone = vi.spyOn(layout, 'usePhoneLayout').mockReturnValue(true);
    try {
      const { fake } = setup({ ...NONE, loaded: false }, { phone: true, tab: null });
      const dialog = screen.getByRole('dialog', { name: en['admin.title'] });
      expect(within(dialog).getByText(en['admin.loading'])).toBeVisible();
      expect(within(dialog).getByRole('status').closest('[hidden]')).toBeNull();
      expect(fake.callsOf('getBotInstallation')).toHaveLength(0);
      act(() => fake.set(`permissions:${SERVER}`, NONE));
      expect(within(dialog).queryByText(en['admin.loading'])).toBeNull();
      expect(within(dialog).getByRole('button', { name: en['workspace.members'] })).toBeVisible();
    } finally { phone.mockRestore(); }
  });
  it('uses registry visibility for ordinary members and keeps the audit-log gate separate', () => {
    setup({ ...NONE, loaded: true, viewAuditLog: false });
    expect(navNames()).toContain('Members');
    expect(navNames()).toContain('Self-assignable roles');
    expect(navNames()).not.toContain('Moderation log');
  });
  it('mounts the member workspace for ordinary members without manager actions', async () => {
    const { user } = setup(NONE, { tab: 'members' });
    expect(screen.getByRole('heading', { name: 'Members', level: 2 })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /Bob B/ }));
    const profile = screen.getByRole('dialog', { name: 'Bob B' });
    const actions = within(profile).queryAllByRole('button').filter(button => button.matches('.d-profile-card__action, .d-profile-card__icon-action')).map(button => button.getAttribute('aria-label') ?? button.textContent);
    expect(actions).toEqual(['Message', 'compare safety numbers']);
  });
  it('shows moderation log from its own audit permission', () => {
    setup({ ...NONE, viewAuditLog: true });
    expect(navNames()).toContain('Moderation log');
    expect(navNames()).not.toContain('Bot');
  });
  it('publishes a bot-authored role panel and registers only its accepted exact message', async () => {
    const roleId = 'c3'.repeat(16); const channelId = 'd4'.repeat(16);
    const { fake, user, announce } = setup(FULL, { tab: 'selfRoles',
      selfRoles: [{ id: roleId, name: 'Readers', position: 2, selfSelected: false }],
      roles: [{ id: roleId, name: 'Readers', position: 2, allow: PERM.viewChannel, deny: 0, color: 0, hoist: false, mentionable: false, members: 0 }],
      channels: [{ id: channelId, communityId: SERVER, name: 'general', kind: 0, mode: 0, topic: '', parentId: null,
        position: 0, slowmode: 0, announcement: false, group: 'active' }],
      installation: { id: 'f6'.repeat(16), communityId: SERVER, state: 3, revision: '0', userId: null, deviceId: null, capabilities: 4, updatedAt: '' },
      interaction: true });
    const emoji = await screen.findByRole('textbox', { name: 'Emoji for Readers' });
    await user.clear(emoji); await user.type(emoji, '📚');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Text channel' }), channelId);
    await user.click(screen.getByRole('button', { name: 'Publish role panel' }));
    await waitFor(() => expect(fake.callsOf('registerRolePanel')).toHaveLength(1));
    const sent = fake.callsOf('humanInteractionRequest')[0];
    expect(sent).toMatchObject({ operation: 31, channelId: null });
    expect(fake.callsOf('registerRolePanel')[0]).toMatchObject({ communityId: SERVER, channelId, messageSeq: '9', options: [{ emoji: '📚', roleId }] });
    expect(document.querySelector('.d-settings-frame .d-sr-only[role="status"]')).toHaveTextContent('The bot published and registered the role panel.');
    expect(announce).not.toHaveBeenCalledWith('The bot published and registered the role panel.');
  });
  it('rejects duplicate reaction mappings before sending or changing role policy', async () => {
    const one = 'c3'.repeat(16); const two = 'c4'.repeat(16); const channelId = 'd4'.repeat(16);
    const { fake, user } = setup(FULL, { tab: 'selfRoles', selfRoles: [
      { id: one, name: 'Readers', position: 2, selfSelected: false }, { id: two, name: 'Writers', position: 3, selfSelected: false },
    ], channels: [{ id: channelId, communityId: SERVER, name: 'general', kind: 0, mode: 0, topic: '', parentId: null,
      position: 0, slowmode: 0, announcement: false, group: 'active' }],
    installation: { id: 'f6'.repeat(16), communityId: SERVER, state: 3, revision: '0', userId: null, deviceId: null, capabilities: 4, updatedAt: '' } });
    await user.type(await screen.findByRole('textbox', { name: 'Emoji for Readers' }), '✅');
    await user.type(screen.getByRole('textbox', { name: 'Emoji for Writers' }), '✅');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Text channel' }), channelId);
    await user.click(screen.getByRole('button', { name: 'Publish role panel' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Choose a text channel');
    expect(fake.callsOf('humanInteractionRequest')).toHaveLength(0);
    expect(fake.callsOf('configureSelfRoles')).toHaveLength(0);
  });
  it.each(['pending', 'unknown'] as const)('keeps polling after an %s upload until its late accepted response', async upload => {
    const roleId = 'c3'.repeat(16); const channelId = 'd4'.repeat(16); let responseReady = false;
    const { fake, user } = setup(FULL, { tab: 'selfRoles', selfRoles: [{ id: roleId, name: 'Readers', position: 2, selfSelected: false }],
      channels: [{ id: channelId, communityId: SERVER, name: 'general', kind: 0, mode: 0, topic: '', parentId: null,
        position: 0, slowmode: 0, announcement: false, group: 'active' }],
      installation: { id: 'f6'.repeat(16), communityId: SERVER, state: 3, revision: '0', userId: null, deviceId: null, capabilities: 4, updatedAt: '' },
      interaction: () => responseReady, upload });
    await user.type(await screen.findByRole('textbox', { name: 'Emoji for Readers' }), '📚');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Text channel' }), channelId);
    await user.click(screen.getByRole('button', { name: 'Publish role panel' }));
    await screen.findByText('The send result is uncertain. Retry with the same request to safely continue.');
    expect(fake.callsOf('humanInteractionRequest')).toHaveLength(1);
    responseReady = true;
    await waitFor(() => expect(fake.callsOf('registerRolePanel')).toHaveLength(1));
    expect(fake.callsOf('humanInteractionRequest')).toHaveLength(1);
    expect(fake.callsOf('registerRolePanel')[0]).toMatchObject({ channelId, messageSeq: '9', options: [{ emoji: '📚', roleId }] });
  });
  it('shows a deterministic local send-preparation error without calling it an uncertain upload', async () => {
    const roleId = 'c3'.repeat(16); const channelId = 'd4'.repeat(16);
    const { fake, user } = setup(FULL, { tab: 'selfRoles', selfRoles: [{ id: roleId, name: 'Readers', position: 2, selfSelected: false }],
      channels: [{ id: channelId, communityId: SERVER, name: 'general', kind: 0, mode: 0, topic: '', parentId: null,
        position: 0, slowmode: 0, announcement: false, group: 'active' }],
      installation: { id: 'f6'.repeat(16), communityId: SERVER, state: 3, revision: '0', userId: null, deviceId: null, capabilities: 4, updatedAt: '' },
      uploadHandler: () => Promise.reject(refusal({ code: 'E_CORE_INPUT' })) });
    await user.type(await screen.findByRole('textbox', { name: 'Emoji for Readers' }), '📚');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Text channel' }), channelId);
    await user.click(screen.getByRole('button', { name: 'Publish role panel' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not publish the role panel (E_CORE_INPUT). Try again.');
    expect(screen.queryByText('The send result is uncertain. Retry with the same request to safely continue.')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry publishing' })).not.toBeInTheDocument();
    expect(fake.callsOf('readBotInteractionResponses')).toHaveLength(0);
    expect(fake.callsOf('registerRolePanel')).toHaveLength(0);
  });
  it('shows a bounded error code when polling the bot response fails', async () => {
    const roleId = 'c3'.repeat(16); const channelId = 'd4'.repeat(16);
    const { fake, user } = setup(FULL, { tab: 'selfRoles', selfRoles: [{ id: roleId, name: 'Readers', position: 2, selfSelected: false }],
      channels: [{ id: channelId, communityId: SERVER, name: 'general', kind: 0, mode: 0, topic: '', parentId: null,
        position: 0, slowmode: 0, announcement: false, group: 'active' }],
      installation: { id: 'f6'.repeat(16), communityId: SERVER, state: 3, revision: '0', userId: null, deviceId: null, capabilities: 4, updatedAt: '' } });
    const handle = fake.handler;
    fake.handler = command => command.m === 'readBotInteractionResponses'
      ? Promise.reject(refusal({ code: 'E_UNAVAILABLE' })) : handle(command);
    await user.type(await screen.findByRole('textbox', { name: 'Emoji for Readers' }), '📚');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Text channel' }), channelId);
    await user.click(screen.getByRole('button', { name: 'Publish role panel' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not publish the role panel (E_UNAVAILABLE). Try again.');
    expect(fake.callsOf('humanInteractionRequest')).toHaveLength(1);
  });
  it('does not restore a delayed upload into a different community or retry it there', async () => {
    const roleId = 'c3'.repeat(16); const channelA = 'd4'.repeat(16); const channelB = 'd5'.repeat(16);
    let resolveUpload!: (value: { requestId: string; upload: 'pending' }) => void;
    const { fake, user, view } = setup(FULL, { tab: 'selfRoles',
      selfRoles: () => [{ id: roleId, name: 'Readers', position: 2, selfSelected: false }],
      channels: [{ id: channelA, communityId: SERVER, name: 'general', kind: 0, mode: 0, topic: '', parentId: null,
        position: 0, slowmode: 0, announcement: false, group: 'active' }],
      installation: communityId => ({ id: 'f6'.repeat(16), communityId, state: 3, revision: '0', userId: null, deviceId: null, capabilities: 4, updatedAt: '' }),
      uploadHandler: requestId => new Promise(resolve => { resolveUpload = value => resolve({ ...value, requestId }); }) });
    fake.publish(`communities`, [{ id: SERVER, name: NAME, owner: ME.id }, { id: OTHER_SERVER, name: 'Other', owner: ME.id }]);
    fake.set(`permissions:${OTHER_SERVER}`, FULL);
    fake.set(`roles:${OTHER_SERVER}`, [{ id: roleId, name: 'Readers', position: 2, allow: PERM.viewChannel, deny: 0, color: 0, hoist: false, mentionable: false, members: 0 }]);
    fake.set(`channels:${OTHER_SERVER}`, [{ id: channelB, communityId: OTHER_SERVER, name: 'other-general', kind: 0, mode: 0, topic: '', parentId: null,
      position: 0, slowmode: 0, announcement: false, group: 'active' }]);
    await user.type(await screen.findByRole('textbox', { name: 'Emoji for Readers' }), '📚');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Text channel' }), channelA);
    await user.click(screen.getByRole('button', { name: 'Publish role panel' }));
    await waitFor(() => expect(fake.callsOf('humanInteractionRequest')).toHaveLength(1));
    const oldRequestId = fake.callsOf('humanInteractionRequest')[0].requestId;
    view.rerender(<div className="d-root"><CoreProvider client={fake}>
      <ServerSettings communityId={OTHER_SERVER} tab="selfRoles" onTab={vi.fn()} onClose={vi.fn()} announce={vi.fn()} />
    </CoreProvider></div>);
    resolveUpload({ requestId: oldRequestId, upload: 'pending' });
    await screen.findByRole('heading', { name: en['admin.bot.rolePanel.title'] });
    expect(screen.queryByRole('button', { name: 'Retry publishing' })).not.toBeInTheDocument();
    await user.type(await screen.findByRole('textbox', { name: 'Emoji for Readers' }), '🖋️');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Text channel' }), channelB);
    await user.click(screen.getByRole('button', { name: 'Publish role panel' }));
    await waitFor(() => expect(fake.callsOf('humanInteractionRequest')).toHaveLength(2));
    expect(fake.callsOf('humanInteractionRequest')[1]).toMatchObject({ communityId: OTHER_SERVER });
    expect(fake.callsOf('humanInteractionRequest')[1].args[0]).toBe(channelB);
    expect(fake.callsOf('humanInteractionRequest')[1].requestId).not.toBe(oldRequestId);
  });
  it('shows every tab to an owner, with the delete tab last', () => {
    setup(FULL);
    expect(navNames()).toEqual(['Overview', 'Channels', 'GIFs', 'Members', 'Roles', 'Invites', 'Self-assignable roles', 'Bot', 'Welcome messages', 'Join roles', 'Music', 'Automoderation', 'Bans', 'Ownership / Leave']);
  });
  it('shows the roles tab only with manageRoles, after channels', () => {
    setup({ ...NONE, manageChannels: true, manageRoles: true });
    expect(navNames()).toEqual(['Channels', 'Members', 'Roles', 'Self-assignable roles', 'Ownership / Leave']);
  });
  it('shows no roles tab without manageRoles', () => {
    setup({ ...NONE, manageChannels: true });
    expect(navNames()).not.toContain('Roles');
    expect(navNames()).toContain('Self-assignable roles');
  });
  it('mounts the roles panel for the roles tab', () => {
    setup(FULL, { tab: 'roles' });
    expect(screen.getByRole('heading', { name: en['roles.title'] })).toBeInTheDocument();
  });
  it('shows only the tabs a person has the permission for; the danger tab is always there', () => {
    setup({ ...NONE, manageChannels: true, banMembers: true });
    expect(navNames()).toEqual(['Channels', 'Members', 'Self-assignable roles', 'Bans', 'Ownership / Leave']);
  });
  it('shows overview for manageCommunity and invites for createInvite while audit log stays separately gated', () => {
    setup({ ...NONE, manageCommunity: true, createInvite: true });
    expect(navNames()).toContain('Overview');
    expect(navNames()).toContain('GIFs');
    expect(navNames()).toContain('Invites');
    expect(navNames()).not.toContain('Moderation log');
  });
  it('shows the bot tab only to manageCommunity members while self-service roles are member-accessible', () => {
    setup({ ...NONE, manageChannels: true });
    expect(navNames()).not.toContain('Bot');
    expect(navNames()).toContain('Self-assignable roles');
  });
  it('lets an ordinary member opt into an approved self-service role', async () => {
    const roleId = 'c3'.repeat(16);
    const { fake, user } = setup(NONE, { tab: 'selfRoles', selfRoles: [{ id: roleId, name: 'Readers', position: 2, selfSelected: false }] });
    const checkbox = await screen.findByRole('checkbox', { name: 'Readers' });
    await user.click(checkbox);
    await waitFor(() => expect(fake.callsOf('selectSelfRole')).toHaveLength(1));
    expect(fake.callsOf('selectSelfRole')[0]).toMatchObject({ communityId: SERVER, roleId, enabled: true });
    expect(screen.getByRole('checkbox', { name: 'Readers' })).toBeChecked();
  });
  it('hides self-service policy choices that exceed safe bits or the manager rank', async () => {
    const role = (id: string, name: string, position: number, allow: number): RoleSummary => ({ id, name, position, allow,
      deny: 0, color: 0, hoist: false, mentionable: false, members: 0 });
    setup({ ...NONE, top: 10, manageRoles: true }, { tab: 'selfRoles', roles: [
      role('c1'.repeat(16), 'Safe', 5, PERM.viewChannel), role('c2'.repeat(16), 'Too high', 10, 0),
      role('c3'.repeat(16), 'Unsafe', 4, PERM.manageRoles),
    ] });
    await screen.findByText(en['admin.bot.selfRoles.configure']);
    expect(screen.getByRole('checkbox', { name: 'Safe' })).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: 'Too high' })).not.toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: 'Unsafe' })).not.toBeInTheDocument();
  });
  it('shows no tab as forbidden while the permissions are loading', () => {
    setup({ ...NONE, loaded: false });
    expect(navNames()).toEqual([]);
    expect(screen.getByRole('dialog', { name: en['admin.title'] })).toBeInTheDocument();
  });
  it('clears a revoked section and moves to an allowed workspace with an explanation', async () => {
    const { fake, announce } = setup(FULL, { tab: 'channels' });
    expect(screen.getByRole('heading', { name: 'Channels', level: 2 })).toBeInTheDocument();
    fake.set(`permissions:${SERVER}`, { ...NONE, manageRoles: false });
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Members', level: 2 })).toBeInTheDocument());
    expect(navNames()).not.toContain('Channels');
    expect(screen.getByRole('status')).toHaveTextContent(en['workspace.permissionChanged']);
    expect(announce).not.toHaveBeenCalledWith(en['workspace.permissionChanged']);
  });
  it('clears a draft immediately when the permission snapshot becomes unloaded', async () => {
    const { fake, user } = setup(FULL);
    await user.type(screen.getByRole('textbox', { name: en['admin.overview.name'] }), ' draft');
    expect(screen.getByRole('button', { name: 'Save' })).toBeInTheDocument();
    act(() => fake.set(`permissions:${SERVER}`, { ...FULL, loaded: false }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Save' })).toBeNull());
    expect(screen.queryByRole('textbox', { name: en['admin.overview.name'] })).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent(en['workspace.permissionChanged']);
  });
  it('closes a direct-linked workspace when its loaded community list excludes the server', async () => {
    const { onClose } = setup(FULL, { missingCommunity: true });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(document.querySelector('dialog.d-settings-frame')).not.toHaveAttribute('open');
  });
  it('closes and clears when a removal notice arrives before the community list changes', async () => {
    const { fake, onClose } = setup(FULL);
    act(() => fake.set('removals', [{ id: 1, communityId: SERVER, name: NAME }]));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(document.querySelector('dialog.d-settings-frame')).not.toHaveAttribute('open');
  });
  it('opens the first permitted tab when the asked-for tab is not allowed', () => {
    setup(NONE, { tab: 'overview' });
    expect(screen.getByRole('heading', { name: 'Members', level: 2 })).toBeInTheDocument();
  });
  it('opens the first allowed section for a desktop null route', () => {
    setup(NONE, { tab: null });
    expect(screen.getByRole('heading', { name: 'Members', level: 2 })).toBeInTheDocument();
  });
  it('keeps GIF settings in their own section', () => {
    const overview = setup(FULL);
    expect(overview.view.container.querySelector('.dw-gifs-settings')).toBeNull();
    overview.view.unmount();
    const { view } = setup(FULL, { tab: 'gifs' });
    expect(view.container.querySelector('.d-settings-frame__body h2')).toHaveTextContent('GIFs');
  });
  it('asks to switch tabs through onTab', async () => {
    const { user, onTab } = setup(FULL);
    await user.click(screen.getByRole('button', { name: 'Bans' }));
    expect(onTab).toHaveBeenCalledWith('bans');
  });
  it('closes through onClose', async () => {
    const { user, onClose } = setup(FULL);
    await user.click(screen.getByRole('button', { name: en['settings.close'] }));
    expect(onClose).toHaveBeenCalled();
  });
  it('has no axe violations', async () => {
    const { view } = setup(FULL);
    await expectNoAxeViolations(view.container);
  });
});

describe('the overview section', () => {
  it('keeps only the AFK timeout dirty after a rename succeeds and the timeout fails', async () => {
    const { fake, user } = setup(FULL);
    fake.handler = command => command.m === 'getAFKSettings' ? Promise.resolve({ channelId: null, timeoutSeconds: 300 })
      : command.m === 'setAFK' ? Promise.reject(refusal({ code: 'E_NETWORK' })) : Promise.resolve(null);
    const name = screen.getByRole('textbox', { name: en['admin.overview.name'] });
    await user.clear(name); await user.type(name, 'Midgard');
    await user.click(within(screen.getByRole('radiogroup', { name: en['admin.overview.afk'] })).getByRole('radio', { name: en['admin.overview.afk.900'] }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(en['workspace.overview.partial'])).toBeInTheDocument();
    expect(fake.callsOf('renameCommunity')).toHaveLength(1);
    expect(fake.callsOf('setAFK')).toHaveLength(1);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(fake.callsOf('setAFK')).toHaveLength(2));
    expect(fake.callsOf('renameCommunity')).toHaveLength(1);
  });
  const field = () => screen.getByRole('textbox', { name: en['admin.overview.name'] });
  const rename = async (user: ReturnType<typeof userEvent.setup>, to: string) => {
    await user.clear(field());
    if (to !== '') await user.type(field(), to);
    await user.click(screen.getByRole('button', { name: 'Save' }));
  };
  // Task 11 (Jonas, Q7): the prototype's segmented 1m / 5m / 15m / 30m / 1h, its label and hint, in the same card as Leave server.
  it('has one timeout row with the five choices and no separate save button', async () => {
    const { fake } = setup({ ...NONE, manageCommunity: true });
    fake.handler = command => Promise.resolve(command.m === 'getAFKSettings' ? { channelId: 'fe'.repeat(16), timeoutSeconds: 900 } : null);
    fake.publish(`afk:${SERVER}`, { channelId: null, timeoutSeconds: 900, pending: false });
    const timeout = screen.getByRole('radiogroup', { name: en['admin.overview.afk'] });
    await waitFor(() => expect(within(timeout).getByRole('radio', { name: '15m' })).toHaveAttribute('aria-checked', 'true'));
    expect(within(timeout).getAllByRole('radio').map(o => o.textContent)).toEqual(['1m', '5m', '15m', '30m', '1h']);
    expect(screen.queryByRole('combobox')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
    expect(document.querySelectorAll('.d-settings-frame__footer')).toHaveLength(0);
    expect((timeout.closest('.d-settings-row') as HTMLElement).querySelector('.d-settings-row__label')).toHaveTextContent(en['admin.overview.afk']);
    expect(screen.getByText(en['admin.overview.afkHint'])).toBeInTheDocument();
    expect(screen.getByText(en['admin.overview.leaveHint'])).toBeInTheDocument();
  });
  it('saves the changed timeout with the name in one press, without a channel', async () => {
    const { fake, user } = setup({ ...NONE, manageCommunity: true });
    fake.handler = command => Promise.resolve(command.m === 'getAFKSettings' ? { channelId: 'fe'.repeat(16), timeoutSeconds: 300 } : null);
    fake.publish(`afk:${SERVER}`, { channelId: null, timeoutSeconds: 300, pending: false });
    const timeout = screen.getByRole('radiogroup', { name: en['admin.overview.afk'] });
    await waitFor(() => expect(within(timeout).getByRole('radio', { name: '5m' })).toHaveAttribute('aria-checked', 'true'));
    await user.click(within(timeout).getByRole('radio', { name: '15m' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(fake.callsOf('setAFK')).toEqual([{ m: 'setAFK', communityId: SERVER, timeoutSeconds: 900 }]));
    expect(fake.callsOf('setAFK')[0]).not.toHaveProperty('channelId');
    expect(fake.callsOf('renameCommunity')).toHaveLength(0);
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Save' })).toBeNull());
    expect(screen.queryByRole('button', { name: 'Reset' })).toBeNull();
  });
  it('keeps a clean overview out of the save flow', async () => {
    const { fake } = setup({ ...NONE, manageCommunity: true });
    const timeout = screen.getByRole('radiogroup', { name: en['admin.overview.afk'] });
    await waitFor(() => expect(within(timeout).getAllByRole('radio')[0]).not.toHaveAttribute('aria-disabled'));
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
    expect(fake.callsOf('renameCommunity')).toEqual([]);
    expect(fake.callsOf('setAFK')).toEqual([]);
  });
  it('lets a member leave from the Overview card, and tells an owner to hand the server over first', async () => {
    const { fake, user } = setup({ ...NONE, manageCommunity: true });
    await user.click(screen.getByRole('button', { name: en['admin.leave.confirm'] }));
    const dialog = await screen.findByRole('dialog', { name: /^Leave .*\?$/ });
    await user.click(within(dialog).getByRole('button', { name: en['admin.leave.confirm'] }));
    await waitFor(() => expect(fake.callsOf('leaveCommunity')).toEqual([{ m: 'leaveCommunity', communityId: SERVER }]));
  });
  // The shared Save action appears only for a dirty draft and lives in the frame footer.
  it('draws the name as a settings row and saves from the frame footer', async () => {
    const { user } = setup(FULL);
    const row = field().closest('.d-settings-row') as HTMLElement;
    expect(row.querySelector('.d-settings-row__label')).toHaveTextContent(en['admin.overview.name']);
    await user.type(field(), ' new');
    const save = screen.getByRole('button', { name: 'Save' });
    expect(save.closest('.d-settings-frame__footer')).not.toBeNull();
    expect(save.closest('.d-settings-frame__body')).toBeNull();
    expect(document.querySelectorAll('.d-settings-frame__footer')).toHaveLength(1);
  });
  it('renames the server with the trimmed name and clears the shared dirty footer', async () => {
    const { fake, user, announce } = setup(FULL);
    await rename(user, '  Midgard ');
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Save' })).toBeNull());
    expect(announce).not.toHaveBeenCalledWith(en['admin.overview.saved']);
    expect(fake.callsOf('renameCommunity')).toEqual([{ m: 'renameCommunity', communityId: SERVER, name: 'Midgard' }]);
  });
  it('says a conflicting change (409) with the conflict copy', async () => {
    const { fake, user } = setup(FULL);
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 409 }));
    await rename(user, 'Midgard');
    expect(await screen.findByText(en['admin.overview.error.conflict'])).toBeInTheDocument();
  });
  it('says a refusal by its copy, and a blank name without asking the server', async () => {
    const { fake, user } = setup(FULL);
    fake.handler = () => Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 }));
    await rename(user, 'Midgard');
    expect(await screen.findByText(en['admin.overview.error.forbidden'])).toBeInTheDocument();
    fake.calls.length = 0;
    await rename(user, '');
    expect(await screen.findByText(en['admin.overview.error.empty'])).toBeInTheDocument();
    expect(fake.calls).toEqual([]);
  });
  it('shows a code for any other failure', async () => {
    const { fake, user } = setup(FULL);
    fake.handler = () => Promise.reject(refusal({ code: 'E_NETWORK' }));
    await rename(user, 'Midgard');
    expect(await screen.findByText('Could not save (E_NETWORK). Try again.')).toBeInTheDocument();
  });
});

describe('the danger section', () => {
  // S7: the danger actions are settings rows, label left and button right.
  it('draws the owner actions and the member leave as settings rows', () => {
    setup(FULL, { tab: 'danger' });
    expect(document.querySelectorAll('.d-settings-frame__body .d-settings-row')).toHaveLength(2);
    expect(pane().getByRole('group', { name: en['admin.nav.delete'] })).toContainElement(pane().getByRole('button', { name: en['admin.nav.delete'] }));
  });
  it('draws a member leave as one settings row', () => {
    setup(NONE, { tab: 'danger' });
    expect(document.querySelectorAll('.d-settings-frame__body .d-settings-row')).toHaveLength(1);
  });
  const open = async (user: ReturnType<typeof userEvent.setup>, name: string) => {
    await user.click(pane().getByRole('button', { name }));
    return screen.findByRole('dialog', { name: new RegExp(name === 'hand over server' ? 'Hand' : name === 'leave server' ? 'Leave' : 'Delete') });
  };
  it('tells the owner why they cannot leave, and offers delete and hand over', () => {
    setup(FULL, { tab: 'danger' });
    expect(screen.getByText(`You own ${NAME}, so you cannot leave it. Hand it to another member or delete it.`)).toBeInTheDocument();
    expect(pane().getByRole('button', { name: 'delete server' })).toBeInTheDocument();
    expect(pane().getByRole('button', { name: 'hand over server' })).toBeInTheDocument();
    expect(pane().queryByRole('button', { name: 'leave server' })).toBeNull();
  });
  it('lets a non-owner leave through a confirm, then closes', async () => {
    const { fake, user, onClose, announce } = setup(NONE, { tab: 'danger' });
    expect(screen.queryByText(/so you cannot leave it/)).toBeNull();
    const dialog = await open(user, 'leave server');
    await user.click(within(dialog).getByRole('button', { name: en['admin.leave.confirm'] }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(fake.callsOf('leaveCommunity')).toEqual([{ m: 'leaveCommunity', communityId: SERVER }]);
    expect(announce).toHaveBeenCalledWith(`You left ${NAME}.`);
  });
  it('keeps the leave confirm open with a coded refusal', async () => {
    const { fake, user, onClose } = setup(NONE, { tab: 'danger' });
    fake.handler = () => Promise.reject(refusal({ code: 'E_NETWORK' }));
    const dialog = await open(user, 'leave server');
    await user.click(within(dialog).getByRole('button', { name: en['admin.leave.confirm'] }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('Could not leave (E_NETWORK). Try again.');
    expect(onClose).not.toHaveBeenCalled();
  });
  it('enables delete only when the typed name equals the server name under NFC (NFD "Bjorn" matches)', async () => {
    const { fake, user, onClose } = setup(FULL, { tab: 'danger' });
    const dialog = await open(user, 'delete server');
    const confirm = within(dialog).getByRole('button', { name: en['admin.delete.confirm'] });
    expect(confirm).toBeDisabled();
    const typed = within(dialog).getByRole('textbox', { name: en['admin.delete.confirmName'] });
    await user.type(typed, 'Bjorn');
    expect(confirm).toBeDisabled();
    expect(within(dialog).getByText(en['admin.delete.mismatch'])).toBeInTheDocument();
    await user.clear(typed);
    await user.type(typed, ` ${NFD} `);
    expect(NFD).not.toBe(NAME);
    expect(confirm).toBeEnabled();
    await user.click(confirm);
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(fake.callsOf('deleteCommunity')).toEqual([{ m: 'deleteCommunity', communityId: SERVER }]);
  });
  it('says a delete refusal by its copy and stays open', async () => {
    const { fake, user } = setup(FULL, { tab: 'danger' });
    fake.handler = () => Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 }));
    const dialog = await open(user, 'delete server');
    await user.type(within(dialog).getByRole('textbox', { name: en['admin.delete.confirmName'] }), NAME);
    await user.click(within(dialog).getByRole('button', { name: en['admin.delete.confirm'] }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(en['admin.delete.error.forbidden']);
  });
  it('hands over to the chosen member (other members only), after the typed name', async () => {
    const { fake, user, announce } = setup(FULL, { tab: 'danger' });
    const dialog = await open(user, 'hand over server');
    const select = within(dialog).getByRole('combobox', { name: en['admin.transfer.member'] });
    expect(within(select).getAllByRole('option').map(o => o.textContent)).toEqual(['Bob B', 'carol']);
    await user.selectOptions(select, 'carol');
    const confirm = within(dialog).getByRole('button', { name: en['admin.transfer.confirm'] });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox', { name: en['admin.transfer.confirmName'] }), NFD);
    await user.click(confirm);
    await waitFor(() => expect(fake.callsOf('transferOwnership')).toEqual([{ m: 'transferOwnership', communityId: SERVER, userId: CAROL }]));
    expect(document.querySelector('.d-settings-frame .d-sr-only[role="status"]')).toHaveTextContent(`carol now owns ${NAME}.`);
    expect(announce).not.toHaveBeenCalledWith(`carol now owns ${NAME}.`);
  });
  it('says a member who left (404) is gone', async () => {
    const { fake, user } = setup(FULL, { tab: 'danger' });
    fake.handler = () => Promise.reject(refusal({ code: 'E_NOT_FOUND', status: 404 }));
    const dialog = await open(user, 'hand over server');
    await user.type(within(dialog).getByRole('textbox', { name: en['admin.transfer.confirmName'] }), NAME);
    await user.click(within(dialog).getByRole('button', { name: en['admin.transfer.confirm'] }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(`That member is no longer in ${NAME}. Choose another.`);
    expect(fake.callsOf('transferOwnership')[0]?.userId).toBe(BOB);
  });
  it('says there is no one to hand over to', () => {
    const { fake } = setup(FULL, { tab: 'danger' });
    fake.publish(`members:${SERVER}`, [{ userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] }]);
    expect(screen.getByText(en['admin.transfer.none'])).toBeInTheDocument();
    expect(pane().queryByRole('button', { name: 'hand over server' })).toBeNull();
  });
});

describe('fix round 1: loading, closing, minors', () => {
  it('says the settings are loading, with text, while the permissions are read', () => {
    setup({ ...NONE, loaded: false });
    expect(screen.getByText(en['admin.loading'])).toBeInTheDocument();
  });
  it('keeps its live status region inside the active modal', () => {
    setup(FULL);
    expect(document.querySelector('.d-settings-frame [role="status"]')).not.toBeNull();
  });
  it.each([
    ['leave', NONE, 'leave server', 'Leave', en['admin.leave.confirm'], `You left ${NAME}.`],
    ['delete', FULL, 'delete server', 'Delete', en['admin.delete.confirm'], `${NAME} was deleted.`],
  ] as const)('%s: a community dropped before the command resolves still announces and closes', async (_k, access, button, title, confirm, message) => {
    const { fake, user, onClose, announce } = setup(access, { tab: 'danger' });
    fake.handler = () => { fake.publish('communities', []); return Promise.resolve(null); };
    await user.click(pane().getByRole('button', { name: button }));
    const dialog = await screen.findByRole('dialog', { name: new RegExp(title) });
    const typed = within(dialog).queryByRole('textbox');
    if (typed !== null) await user.type(typed, NAME);
    await user.click(within(dialog).getByRole('button', { name: confirm }));
    await waitFor(() => expect(announce).toHaveBeenCalledWith(message));
    expect(onClose).toHaveBeenCalled();
    // The frame is closed through its own close path (which hands focus back), not torn down: the last known server stays.
    const frame = document.querySelector('dialog.d-settings-frame');
    expect(frame).not.toBeNull();
    expect(frame).not.toHaveAttribute('open');
  });
  it('does not say there is no one to hand over to before the members are read', () => {
    setup(FULL, { tab: 'danger', members: null });
    expect(screen.queryByText(en['admin.transfer.none'])).toBeNull();
  });
  it('leaves bots out of the hand over list: the server refuses them', async () => {
    const { user } = setup(FULL, { tab: 'danger', members: [
      { userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] },
      { userId: BOB, username: 'robo', display: '', kind: 1, roleIds: [] },
      { userId: CAROL, username: 'carol', display: '', kind: 0, roleIds: [] },
    ] });
    await user.click(pane().getByRole('button', { name: 'hand over server' }));
    const dialog = await screen.findByRole('dialog', { name: /Hand/ });
    expect(within(within(dialog).getByRole('combobox')).getAllByRole('option').map(o => o.textContent)).toEqual(['carol']);
  });
  it('says there is no one when the only other member is a bot', () => {
    setup(FULL, { tab: 'danger', members: [
      { userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] },
      { userId: BOB, username: 'robo', display: '', kind: 1, roleIds: [] },
    ] });
    expect(screen.getByText(en['admin.transfer.none'])).toBeInTheDocument();
  });
  it('does not overwrite a name being typed when the server is renamed elsewhere, but follows when not typing', async () => {
    const { fake, user } = setup(FULL);
    const field = screen.getByRole('textbox', { name: en['admin.overview.name'] });
    fake.publish('communities', [{ id: SERVER, name: 'Elsewhere', owner: ME.id }]);
    expect(field).toHaveValue('Elsewhere');
    await user.type(field, ' mine');
    fake.publish('communities', [{ id: SERVER, name: 'Again', owner: ME.id }]);
    expect(field).toHaveValue('Elsewhere mine');
  });
});
