import { act, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { encodeBotStatus, encodeMusicProviderStatus, fromHex } from '@dilla/client-core';
import { en, t } from '../../strings/index.ts';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { installSealedBot, type SealedReply } from '../../test/sealed-bot.ts';
import { MusicProviderStatus } from './MusicProviderStatus.tsx';

const cid = '11'.repeat(16); const installationId = '22'.repeat(16);
const ALL = [10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25];

/** Bot v2 Task 21b: the status is the sealed app operation 124; `reply` answers it (null leaves it unanswered). */
function setup(options: { status?: number; wrongInstallation?: boolean; age?: number; offered?: number[]; reply?: () => SealedReply; offline?: boolean } = {}) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: cid, name: 'Music test', owner: ME.id }]);
  fake.set(`permissions:${cid}`, { loaded: true, manageCommunity: true, top: 0, owner: false, manageRoles: false, manageChannels: false, createInvite: false, banMembers: false, kickMembers: false, everyoneId: '', overwritesLoaded: true });
  fake.handler = command => command.m === 'sendBotCommand' && options.offline === true
    ? Promise.reject(refusal({ code: 'E_BOT_OFFLINE', status: 503 })) : Promise.resolve(null);
  const bot = installSealedBot(fake, cid, () => options.reply !== undefined ? options.reply() : { status: options.status ?? 0, revision: null,
    data: encodeMusicProviderStatus({ version: 1, installationId: fromHex(options.wrongInstallation ? '33'.repeat(16) : installationId),
      observedAt: BigInt(Math.floor(Date.now() / 1000) - (options.age ?? 0)), youtubePlayback: 'configured', youtubePlaybackReason: 'none' }) },
  { offered: options.offered ?? ALL });
  if (options.offline === true) {
    const sealed = fake.handler;
    fake.handler = command => command.m === 'sendBotCommand' ? Promise.reject(refusal({ code: 'E_BOT_OFFLINE', status: 503 })) : sealed(command);
  }
  const view = render(<CoreProvider client={fake}><MusicProviderStatus communityId={cid} installationId={installationId} /></CoreProvider>);
  return { fake, bot, view };
}
afterEach(() => vi.useRealTimers());
describe('music provider status', () => {
  it('reads the runtime evidence through one sealed command, with no interaction request', async () => {
    const { fake, bot } = setup();
    expect(await screen.findByText('ready')).toBeVisible();
    expect(bot.calls.map(call => [call.sealed, call.operation, call.channelId, call.args, call.revision])).toEqual([[124, 24, null, [], null]]);
    expect(fake.calls.some(call => call.m === 'humanInteractionRequest' || call.m === 'readBotInteractionResponses')).toBe(false);
  });
  it('does not send to a Bot whose catalog does not offer provider status', async () => {
    const { bot } = setup({ offered: [10] });
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(bot.calls).toEqual([]);
    expect(screen.queryByText('ready')).not.toBeInTheDocument();
  });
  it('does not equate a wrong installation snapshot with configured playback', async () => {
    setup({ wrongInstallation: true });
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(screen.queryByText('ready')).not.toBeInTheDocument();
  });
  it('removes the observation and stops protected reads when access is revoked', async () => {
    const { fake } = setup(); await screen.findByText('ready');
    act(() => fake.publish(`permissions:${cid}`, { ...fake.get(`permissions:${cid}`)!, manageCommunity: false }));
    expect(screen.queryByText('ready')).not.toBeInTheDocument();
  });
  it('shows older observations as stale', async () => {
    setup({ age: 70 });
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('out of date'));
    expect(screen.queryByText('ready')).not.toBeInTheDocument();
  });
  it('shows a refused status as unavailable', async () => {
    setup({ status: 2 });
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(screen.queryByText('ready')).not.toBeInTheDocument();
  });
  it('shows a Bot the instance cannot reach as unavailable and allows Check again', async () => {
    const { fake } = setup({ offline: true });
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled();
    const sends = fake.callsOf('sendBotCommand').length;
    act(() => screen.getByRole('button', { name: 'Check again' }).click());
    await waitFor(() => expect(fake.callsOf('sendBotCommand')).toHaveLength(sends + 1));
  });
  it('says the Bot has not reported yet when the status goes unanswered, offers Check again and does not poll', async () => {
    vi.useFakeTimers();
    const { bot } = setup({ reply: () => null });
    // The send and its one automatic resend each wait 5 s for an answer.
    await act(async () => { await vi.advanceTimersByTimeAsync(9_000); });
    expect(screen.getByRole('status')).toHaveTextContent('Checking provider status…');
    await act(async () => { await vi.advanceTimersByTimeAsync(1_500); });
    expect(screen.getByRole('status')).toHaveTextContent('The bot answered, but has not reported provider status yet. Check again.');
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled();
    expect(screen.queryByText('ready')).not.toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(bot.calls).toHaveLength(2);
  });
  it('keeps a completed observation until its freshness window expires', async () => {
    vi.useFakeTimers();
    setup();
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(screen.getByText('ready')).toBeVisible();
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(screen.getByRole('status')).toHaveTextContent('out of date');
    expect(screen.queryByText('ready')).not.toBeInTheDocument();
  });
});

// Bot v2 Task 22: the Bot page's status read (94) carries the last 20 music starts; the music page lists them.
describe('music start timings (Task 22)', () => {
  it('music timings list the last 20 starts', async () => {
    const fake = new FakeClient();
    fake.set('account', account());
    fake.set(`permissions:${cid}`, { loaded: true, manageCommunity: true, top: 0, owner: false, manageRoles: false, manageChannels: false, createInvite: false, banMembers: false, kickMembers: false, everyoneId: '', overwritesLoaded: true });
    const starts = Array.from({ length: 20 }, (_, i) => ({ firstFrameMs: 100 + i, cold: i === 0 }));
    const bot = installSealedBot(fake, cid, call => call.sealed === 94
      ? { status: 0, revision: null, data: encodeBotStatus({ needsReAdd: [], musicStarts: starts }) }
      : { status: 0, revision: null, data: encodeMusicProviderStatus({ version: 1, installationId: fromHex(installationId),
        observedAt: BigInt(Math.floor(Date.now() / 1000)), youtubePlayback: 'configured', youtubePlaybackReason: 'none' }) },
    { offered: [24, 94] });
    render(<CoreProvider client={fake}><MusicProviderStatus communityId={cid} installationId={installationId} /></CoreProvider>);
    const row = await screen.findByRole('group', { name: en['bot.music.timings'] });
    const items = await within(row).findAllByRole('listitem');
    expect(items).toHaveLength(20);
    expect(items[0]).toHaveTextContent(t('bot.music.startCold', { ms: 100 }));
    expect(items[19]).toHaveTextContent(t('bot.music.start', { ms: 119 }));
    expect(bot.calls.map(c => c.sealed).sort()).toEqual([124, 94].sort());
  });

  it('says so when no music has started', async () => {
    const fake = new FakeClient();
    fake.set('account', account());
    fake.set(`permissions:${cid}`, { loaded: true, manageCommunity: true, top: 0, owner: false, manageRoles: false, manageChannels: false, createInvite: false, banMembers: false, kickMembers: false, everyoneId: '', overwritesLoaded: true });
    installSealedBot(fake, cid, call => call.sealed === 94 ? { status: 0, revision: null, data: encodeBotStatus({ needsReAdd: [], musicStarts: [] }) } : null,
      { offered: [94] });
    render(<CoreProvider client={fake}><MusicProviderStatus communityId={cid} installationId={installationId} /></CoreProvider>);
    expect(await screen.findByText(en['bot.music.noStarts'])).toBeVisible();
  });
});
