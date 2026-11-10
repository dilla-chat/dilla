// Bot v2 Task 22: the Bot page's health, command key and Bot channels.
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { encodeBotStatus, type BotCatalogView, type ChannelSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { installSealedBot } from '../../test/sealed-bot.ts';
import { en, formatDayTime, t } from '../../strings/index.ts';
import { BotHealth } from './BotHealth.tsx';

const SERVER = 'a1'.repeat(16);
const BOTS = 'c3'.repeat(16);
const GENERAL = 'c4'.repeat(16);
const QUEST = 'c5'.repeat(16);
const ACCESS = { loaded: true, manageCommunity: true, top: 100, owner: true, manageRoles: true, manageChannels: true, createInvite: true,
  banMembers: true, kickMembers: true, everyoneId: 'ab'.repeat(16), overwritesLoaded: true };
const channel = (id: string, name: string): ChannelSummary =>
  ({ id, communityId: SERVER, kind: 0, mode: 0, name, topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' });

function setup(over: { catalog?: Partial<BotCatalogView>; needsReAdd?: string[]; manage?: boolean } = {}) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set(`permissions:${SERVER}`, { ...ACCESS, manageCommunity: over.manage ?? true });
  fake.set(`channels:${SERVER}`, [channel(BOTS, 'bots'), channel(GENERAL, 'general'), channel(QUEST, 'quests')]);
  fake.handler = command => Promise.resolve(command.m === 'reAddBot' ? 'added' : null);
  const bot = installSealedBot(fake, SERVER, () => ({ status: 0, revision: null,
    data: encodeBotStatus({ needsReAdd: over.needsReAdd ?? [], musicStarts: [] }) }), { offered: [94] });
  fake.set(`botCatalog:${SERVER}`, { ...fake.get(`botCatalog:${SERVER}`)!, fingerprint: '3f2a 91c0 7b44 e1d8', botChannels: [BOTS, QUEST],
    status: { state: 'ready', since: 1_800_000_000 }, ...over.catalog });
  const user = userEvent.setup();
  const view = render(<CoreProvider client={fake}><BotHealth communityId={SERVER} announce={vi.fn()} /></CoreProvider>);
  return { fake, bot, user, view };
}

describe('the Bot page: health, command key and Bot channels (Task 22)', () => {
  it('the Bot page lists Bot channels and the key fingerprint', () => {
    setup();
    expect(screen.getByText(en['bot.health.ok'])).toBeVisible();
    expect(screen.getByText(t('bot.key.fingerprint', { fingerprint: '3f2a 91c0 7b44 e1d8' }))).toBeVisible();
    const list = screen.getByRole('group', { name: /^#bots/ });
    expect(within(list).getByText(en['bot.channel.badge'])).toBeVisible();
    expect(screen.getByRole('group', { name: /^#quests/ })).toBeVisible();
    expect(screen.queryByText('#general')).toBeNull();
  });

  it('shows the empty line with no Bot channels', () => {
    setup({ catalog: { botChannels: [] } });
    expect(screen.getByText(en['bot.channels.empty'])).toBeVisible();
  });

  it('health shows Commands failing since <time> when degraded', () => {
    setup({ catalog: { status: { state: 'degraded', since: 1_800_000_000 } } });
    expect(screen.getByText(t('bot.health.failing', { time: formatDayTime(1_800_000_000) }))).toBeVisible();
    expect(screen.queryByText(en['bot.health.ok'])).toBeNull();
  });

  it('health shows the offline time when the Bot is offline', () => {
    setup({ catalog: { status: { state: 'offline', since: 1_800_000_060 } } });
    expect(screen.getByText(t('bot.health.offline', { time: formatDayTime(1_800_000_060) }))).toBeVisible();
  });

  // Fix round 1 (L4): with no server time the line says the state alone, never "since " followed by nothing.
  it.each([['degraded', 'bot.health.failingNow'], ['offline', 'bot.health.offlineNow']] as const)('health with no since shows the plain %s line', (state, key) => {
    setup({ catalog: { status: { state, since: null } } });
    expect(screen.getByText(en[key])).toBeVisible();
    expect(screen.queryByText(/since\s*$/u)).toBeNull();
  });

  it('a Bot channel the Bot needs re-adding to says so, and the button adds it again', async () => {
    const { fake, bot, user } = setup({ needsReAdd: [QUEST] });
    const row = await screen.findByRole('group', { name: /^#quests/ });
    await waitFor(() => expect(within(row).getByText(en['bot.channels.needsReAdd'])).toBeVisible());
    expect(within(screen.getByRole('group', { name: /^#bots/ })).queryByText(en['bot.channels.needsReAdd'])).toBeNull();
    expect(bot.calls.map(c => c.sealed)).toContain(94);
    await user.click(within(row).getByRole('button', { name: en['bot.channels.reAdd'] }));
    expect(fake.callsOf('reAddBot')).toEqual([{ m: 'reAddBot', communityId: SERVER, channelId: QUEST }]);
  });

  it('a member who does not manage the community sees no re-add and sends no status read', () => {
    const { bot } = setup({ needsReAdd: [QUEST], manage: false });
    expect(screen.queryByRole('button', { name: en['bot.channels.reAdd'] })).toBeNull();
    expect(bot.calls).toEqual([]);
  });
});
