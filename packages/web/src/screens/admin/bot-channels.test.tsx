// Bot v2 Task 22: a text channel marked as a Bot channel in its settings shows the Bot badge, and its member list holds the
// Bot; a channel that is not one leaves the Bot out (the Bot is in Bot channels' groups only).
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import type { BotCatalogView, ChannelSummary, MemberSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { en } from '../../strings/index.ts';
import { ChannelEditFlow } from './ChannelEditFlow.tsx';
import { ChannelsSection } from './ChannelsSection.tsx';
import { MemberPane } from './MemberPane.tsx';

const SERVER = 'a1'.repeat(16);
const LOBBY = 'c3'.repeat(16);
const GENERAL = 'c4'.repeat(16);
const GLENN = '9f'.repeat(16);
const ACCESS = { loaded: true, manageCommunity: true, top: 100, owner: true, manageRoles: false, manageChannels: true, createInvite: true,
  banMembers: true, kickMembers: true, everyoneId: 'ab'.repeat(16), overwritesLoaded: true };
const channel = (id: string, name: string): ChannelSummary =>
  ({ id, communityId: SERVER, kind: 0, mode: 0, name, topic: '', parentId: null, position: id === LOBBY ? 0 : 1, slowmode: 0, announcement: false, group: 'active' });
const catalog = (botChannels: string[]): BotCatalogView => ({ revision: '1', commands: [], keyId: null, fingerprint: null,
  status: { state: 'ready', since: null }, botChannels, botUserId: GLENN });
const MEMBERS: MemberSummary[] = [
  { userId: ME.id, username: 'ada', display: 'Ada', kind: 0, roleIds: [] },
  { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] },
];

function setup(manage = true) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: SERVER, name: 'Midgard', owner: ME.id }]);
  fake.set(`permissions:${SERVER}`, { ...ACCESS, manageCommunity: manage });
  fake.set(`channels:${SERVER}`, [channel(LOBBY, 'lobby'), channel(GENERAL, 'general')]);
  fake.set(`members:${SERVER}`, MEMBERS);
  fake.set(`roles:${SERVER}`, []);
  fake.set(`presence:${SERVER}`, { [ME.id]: 'online', [GLENN]: 'online' });
  fake.set(`botCatalog:${SERVER}`, catalog([]));
  fake.handler = command => {
    if (command.m === 'markBotChannel') fake.publish(`botCatalog:${SERVER}`, catalog(command.on ? [command.channelId] : []));
    return Promise.resolve(null);
  };
  return { fake, user: userEvent.setup() };
}

describe('Bot channels (Task 22)', () => {
  it('marking a channel shows the Bot badge and lists the Bot as a member', async () => {
    const { fake, user } = setup();
    render(<CoreProvider client={fake}><div className="d-root">
      <ChannelsSection communityId={SERVER} serverName="Midgard" announce={vi.fn()} />
    </div></CoreProvider>);
    const lobbyRow = () => screen.getByText('#lobby').closest('li')!;
    expect(within(lobbyRow()).queryByText(en['bot.channel.badge'])).toBeNull();
    await user.click(screen.getByRole('button', { name: en['admin.channels.editLabel'].replace('{name}', 'lobby') }));
    const toggle = within(screen.getByRole('dialog')).getByRole('switch', { name: en['bot.channel.toggle'] });
    expect(toggle).toHaveAttribute('aria-checked', 'false');
    await user.click(toggle);
    expect(fake.callsOf('markBotChannel')).toEqual([{ m: 'markBotChannel', communityId: SERVER, channelId: LOBBY, on: true }]);
    expect(toggle).toHaveAttribute('aria-checked', 'true');
    expect(within(lobbyRow()).getByText(en['bot.channel.badge'])).toBeVisible();
    expect(within(screen.getByText('#general').closest('li')!).queryByText(en['bot.channel.badge'])).toBeNull();

    // The member list of the marked channel holds the Bot; another text channel's does not.
    const pane = render(<CoreProvider client={fake}><MemberPane communityId={SERVER} selfId={ME.id} channelId={LOBBY}
      onMessage={vi.fn()} onCompare={vi.fn()} announce={vi.fn()} /></CoreProvider>);
    expect(within(pane.container).getByText('Glenn')).toBeVisible();
    pane.rerender(<CoreProvider client={fake}><MemberPane communityId={SERVER} selfId={ME.id} channelId={GENERAL}
      onMessage={vi.fn()} onCompare={vi.fn()} announce={vi.fn()} /></CoreProvider>);
    expect(within(pane.container).queryByText('Glenn')).toBeNull();
  });

  it('only a member who manages the community gets the toggle', () => {
    const { fake } = setup(false);
    render(<CoreProvider client={fake}><div className="d-root">
      <ChannelEditFlow channel={channel(LOBBY, 'lobby')} categories={[]} announce={vi.fn()} onClose={vi.fn()} />
    </div></CoreProvider>);
    expect(within(screen.getByRole('dialog')).queryByRole('switch', { name: en['bot.channel.toggle'] })).toBeNull();
  });
});
