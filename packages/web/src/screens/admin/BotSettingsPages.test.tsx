import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { en } from '../../strings/index.ts';
import { encodeWelcomeSettings } from './welcome-settings.ts';
import { BotPage, BotSettingsProvider } from './BotSettingsPages.tsx';
import { ServerSettings } from './ServerSettings.tsx';

const COMMUNITY = 'a1'.repeat(16);

describe('bot settings pages', () => {
  it('mounts only the active bot page and does not read inactive page settings', async () => {
    const client = new FakeClient();
    client.set('account', account());
    client.set('factors', { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
    client.set('communities', [{ id: COMMUNITY, name: 'Midgard', owner: 'ff'.repeat(16) }]);
    client.set(`permissions:${COMMUNITY}`, { loaded: true, manageCommunity: true, manageRoles: true, manageChannels: true,
      kickMembers: true, banMembers: true, moderateMembers: true, createInvite: true, owner: true, top: 100,
      everyoneId: 'ab'.repeat(16), overwritesLoaded: true });
    client.handler = command => command.m === 'getBotInstallation' ? Promise.resolve(null) : Promise.resolve(null);

    render(<CoreProvider client={client}><BotSettingsProvider communityId={COMMUNITY}>
      <BotPage communityId={COMMUNITY} serverName="Midgard" announce={() => undefined} />
    </BotSettingsProvider></CoreProvider>);

    expect(await screen.findByRole('heading', { name: 'Bot' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Welcome messages' })).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Automatic moderation' })).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Moderation log' })).not.toBeInTheDocument();
    expect(client.callsOf('getBotJoinRole')).toHaveLength(0);
    expect(client.callsOf('getBotSpotifyConfig')).toHaveLength(0);
  });

  it('opens an audit-only moderation log without installation management reads', async () => {
    const client = new FakeClient();
    client.set('account', account());
    client.set('communities', [{ id: COMMUNITY, name: 'Midgard', owner: 'ff'.repeat(16) }]);
    client.set(`permissions:${COMMUNITY}`, { loaded: true, viewAuditLog: true, manageCommunity: false, manageRoles: false,
      manageChannels: false, kickMembers: false, banMembers: false, createInvite: false, owner: false, top: 0,
      everyoneId: '', overwritesLoaded: true });
    render(<CoreProvider client={client}><ServerSettings communityId={COMMUNITY} tab="moderationLog"
      onTab={() => undefined} onClose={() => undefined} announce={() => undefined} /></CoreProvider>);
    expect(await screen.findByRole('heading', { name: en['workspace.moderationLog'] })).toBeInTheDocument();
    expect(client.callsOf('getBotInstallation')).toHaveLength(0);
  });

  it('keeps an accepted welcome request owned and polls it across page changes', async () => {
    const client = new FakeClient();
    client.set('account', account());
    client.set('factors', { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
    client.set('communities', [{ id: COMMUNITY, name: 'Midgard', owner: 'ff'.repeat(16) }]);
    client.set(`permissions:${COMMUNITY}`, { loaded: true, manageCommunity: true, manageRoles: true, manageChannels: true,
      kickMembers: true, banMembers: true, moderateMembers: true, createInvite: true, owner: true, top: 100,
      everyoneId: 'ab'.repeat(16), overwritesLoaded: true });
    const installation = { id: 'b2'.repeat(16), communityId: COMMUNITY, state: 3, revision: '7', userId: null,
      deviceId: null, capabilities: 1 << 1, updatedAt: '100' };
    let responseReady = false;
    client.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(installation);
      if (command.m === 'humanInteractionRequest') return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
      if (command.m === 'readBotInteractionResponses') return Promise.resolve(responseReady ? { through: '2', blocked: false, responses: [{
        deliveryId: '1', sourceSeq: '1', requestId: 'previous-request', communityId: COMMUNITY,
        operation: 1, channelId: null, status: 0, revision: '7', data: new Uint8Array(),
      }, {
        deliveryId: '2', sourceSeq: '2', requestId: client.callsOf('humanInteractionRequest').at(-1)?.requestId ?? '',
        communityId: COMMUNITY, operation: 1, channelId: null, status: 0, revision: '8',
        data: encodeWelcomeSettings({ channelId: 'cc'.repeat(16), template: 'Hello {member}!', serverName: 'Midgard' }),
      }] } : { through: null, blocked: false, responses: [] });
      return Promise.resolve(null);
    };
    client.set(`channels:${COMMUNITY}`, [{ id: 'cc'.repeat(16), communityId: COMMUNITY, name: 'general', kind: 0, mode: 0,
      topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' }]);
    const page = (kind: 'welcome' | 'bot') => <CoreProvider client={client}>
      <ServerSettings communityId={COMMUNITY} tab={kind} onTab={() => undefined} onClose={() => undefined} announce={() => undefined} />
    </CoreProvider>;
    const user = userEvent.setup();
    const view = render(page('welcome'));
    expect(screen.getByRole('dialog', { name: 'Server settings' })).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: en['admin.bot.welcomeSettings.load'] }));
    await waitFor(() => expect(client.callsOf('humanInteractionRequest')).toHaveLength(1));

    view.rerender(page('bot'));
    expect(screen.queryByRole('button', { name: en['admin.bot.welcomeSettings.load'] })).not.toBeInTheDocument();
    responseReady = true;
    await waitFor(() => expect(client.callsOf('ackBotInteractionResponses')).toHaveLength(1));

    view.rerender(page('welcome'));
    expect(await screen.findByDisplayValue('Hello {member}!')).toBeInTheDocument();
    expect(client.callsOf('humanInteractionRequest')).toHaveLength(1);
  });
});
