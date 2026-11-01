import { act, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { encode, encodeMusicProviderStatus, fromHex } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { MusicProviderStatus } from './MusicProviderStatus.tsx';
import { noteSpotifyReturn } from '../../settings/spotify-return.ts';

const cid = '11'.repeat(16); const installationId = '22'.repeat(16);
/** When each operation's answer becomes readable, in ms after that operation was sent; null never answers. */
type AnswerTiming = { 0?: number | null; 24?: number | null };
function setup(status = 0, wrongInstallation = false, age = 0, empty = false, legacy = false, capabilityData?: Uint8Array, linked = true, pending?: 'send' | 'read', timing: AnswerTiming = {}) {
  const sentAt: Record<number, number> = {};
  const unanswered = (op: number) => {
    const delay = timing[op as 0 | 24];
    return delay === null || (delay !== undefined && Date.now() < (sentAt[op] ?? 0) + delay);
  };
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: cid, name: 'Music test', owner: ME.id }]);
  fake.set(`permissions:${cid}`, { loaded: true, manageCommunity: true, top: 0, owner: false, manageRoles: false, manageChannels: false, createInvite: false, banMembers: false, kickMembers: false, everyoneId: '', overwritesLoaded: true });
  let requestId = '';
  let operation = 0;
  fake.handler = command => Promise.resolve((() => {
    if (pending === 'send' && command.m === 'humanInteractionRequest' || pending === 'read' && command.m === 'readBotInteractionResponses') return new Promise<never>(() => {});
    if (command.m === 'getBotCommandStatus') return { installationId, botUserId: null, state: 3, revision: '1' };
    if (command.m === 'humanInteractionRequest') { requestId = command.requestId; operation = command.operation; sentAt[operation] = Date.now(); return { requestId, upload: 'accepted' }; }
    if (command.m === 'readBotInteractionResponses') return { responses: empty || unanswered(operation) ? [] : [{ requestId, communityId: cid, operation, status: operation === 0 ? 0 : status, deliveryId: operation === 0 ? '8' : '9', channelId: null, revision: null,
      data: operation === 0 ? capabilityData ?? encode(legacy ? [1, fromHex('44'.repeat(16)), 0, 1, 0] : [2, fromHex('44'.repeat(16)), 0, 1, 0, [24]])
        : encodeMusicProviderStatus({ version: 1, installationId: fromHex(wrongInstallation ? '33'.repeat(16) : installationId), observedAt: BigInt(Math.floor(Date.now() / 1000) - age),
        spotifyAccount: linked ? 'linked' : 'not_linked', spotifyAccountReason: linked ? 'none' : 'missing_credentials', spotifyPlayback: 'missing_requirements', spotifyPlaybackReason: 'helper_missing',
        youtubePlayback: 'configured', youtubePlaybackReason: 'none' }) }], through: 'ee'.repeat(16), blocked: false };
    if (command.m === 'ackBotInteractionResponses') return null;
    return null;
  })());
  const view = render(<CoreProvider client={fake}><MusicProviderStatus communityId={cid} installationId={installationId} /></CoreProvider>);
  return { fake, view };
}
afterEach(() => vi.useRealTimers());
// Signal task 10c: the status renders as the Music page's YouTube row and the Spotify account row.
describe('music provider status', () => {
  it('uses runtime evidence and acknowledges the exact leased capability and status responses', async () => {
    const { fake } = setup();
    expect(await screen.findByText(/^Linked · /)).toBeVisible();
    expect(screen.getByText(/Spotify playback setup: Missing requirements/)).toBeVisible();
    expect(fake.callsOf('readBotInteractionResponses').map(call => [call.operations, call.requestIds])).toEqual([
      [[0], [fake.callsOf('humanInteractionRequest')[0].requestId]], [[24], [fake.callsOf('humanInteractionRequest')[1].requestId]],
    ]);
    expect(fake.callsOf('ackBotInteractionResponses')).toHaveLength(2);
    expect(fake.callsOf('humanInteractionRequest').map(call => call.operation)).toEqual([0, 24]);
    expect(fake.callsOf('humanInteractionRequest')[1]).toMatchObject({ operation: 24, args: [], revision: null, channelId: null });
  });
  it('does not send an unknown operation to an older runtime without a capability advertisement', async () => {
    const { fake } = setup(0, false, 0, false, true);
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(fake.callsOf('humanInteractionRequest').map(call => call.operation)).toEqual([0]);
    expect(fake.callsOf('ackBotInteractionResponses')).toHaveLength(1);
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
  });
  it('rejects malformed capability evidence before requesting provider status', async () => {
    const { fake } = setup(0, false, 0, false, false, encode([2, fromHex('44'.repeat(16)), 0, 1, 0, '24']));
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(fake.callsOf('humanInteractionRequest').map(call => call.operation)).toEqual([0]);
  });
  it('does not equate unsupported responses or wrong installation snapshots with linked', async () => {
    setup(0, true);
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
  });
  it('removes the observation and stops protected reads when access is revoked', async () => {
    const { fake } = setup(); await screen.findByText(/^Linked · /);
    act(() => fake.publish(`permissions:${cid}`, { ...fake.get(`permissions:${cid}`)!, manageCommunity: false }));
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
  });
  it('shows older observations as stale and unknown with their timestamp', async () => {
    setup(0, false, 70);
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('out of date'));
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
    expect(screen.getByText(/ · Observed /)).toBeVisible();
  });
  it('shows unsupported runtime responses as unavailable', async () => {
    setup(1);
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unavailable'));
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
  });
  it('bounds pending reads to thirty seconds and allows retry', async () => {
    vi.useFakeTimers();
    const { fake } = setup(0, false, 0, true);
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(screen.getByRole('status')).toHaveTextContent('unavailable');
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled();
    const reads = fake.callsOf('readBotInteractionResponses').length;
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(fake.callsOf('readBotInteractionResponses')).toHaveLength(reads);
  });
  it.each(['send', 'read'] as const)('expires even when the worker %s call never settles', async pending => {
    vi.useFakeTimers();
    setup(0, false, 0, false, false, undefined, true, pending);
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(screen.getByRole('status')).toHaveTextContent('unavailable');
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled();
  });
  // CT 144, 2026-10-30: operation 0 was answered after 25 s and operation 24 had 5 s left of a
  // shared deadline. Each answer has its own thirty seconds, measured from its own send.
  it('gives operation 24 its own thirty seconds after a slow operation 0', async () => {
    vi.useFakeTimers();
    const { fake } = setup(0, false, 0, false, false, undefined, true, undefined, { 0: 25_000, 24: 20_000 });
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(fake.callsOf('humanInteractionRequest').map(call => call.operation)).toEqual([0, 24]);
    expect(screen.getByRole('status')).not.toHaveTextContent('unavailable');
    await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
    expect(screen.getByText(/^Linked · /)).toBeVisible();
  });
  it('shows that the bot answered and offers Check again when operation 24 is not answered', async () => {
    vi.useFakeTimers();
    const { fake } = setup(0, false, 0, false, false, undefined, true, undefined, { 24: null });
    await act(async () => { await vi.advanceTimersByTimeAsync(29_000); });
    expect(screen.getByRole('status')).toHaveTextContent('Checking provider status…');
    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(screen.getByRole('status')).toHaveTextContent('The bot answered, but has not reported provider status yet. Check again.');
    expect(screen.getByRole('status')).not.toHaveTextContent('unavailable');
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled();
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
    const reads = fake.callsOf('readBotInteractionResponses').length;
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(fake.callsOf('readBotInteractionResponses')).toHaveLength(reads);
  });
  it('still reports unavailable when operation 0 is not answered within its own thirty seconds', async () => {
    vi.useFakeTimers();
    const { fake } = setup(0, false, 0, false, false, undefined, true, undefined, { 0: null });
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(screen.getByRole('status')).toHaveTextContent('unavailable');
    expect(fake.callsOf('humanInteractionRequest').map(call => call.operation)).toEqual([0]);
  });
  it('keeps a completed observation until its freshness window expires', async () => {
    vi.useFakeTimers();
    setup();
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(screen.getByText(/^Linked · /)).toBeVisible();
    await act(async () => { await vi.advanceTimersByTimeAsync(31_000); });
    expect(screen.getByRole('status')).toHaveTextContent('out of date');
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
  });
  it('reconciles an OAuth return for at most sixty seconds without treating delivery as Linked', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    noteSpotifyReturn(cid);
    const { fake } = setup(0, false, 0, false, false, undefined, false);
    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    expect(screen.getByText(/^Not linked · /)).toBeVisible();
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
    expect(fake.callsOf('humanInteractionRequest').filter(call => call.operation === 0)).toHaveLength(1);
    expect(fake.callsOf('humanInteractionRequest').filter(call => call.operation === 24).length).toBeGreaterThan(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    const requests = fake.callsOf('humanInteractionRequest').length;
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(fake.callsOf('humanInteractionRequest')).toHaveLength(requests);
    expect(screen.queryByText(/^Linked · /)).not.toBeInTheDocument();
  });

});
