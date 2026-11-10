import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import * as Core from '@dilla/client-core';
import type { PollSummary } from '@dilla/client-core';
import { FakeClient } from '../../test/fake-client.ts';
import { PollCard } from './PollCard.tsx';
import { commandBotSnapshot } from './command-bot-store.ts';
const communityId = '11'.repeat(16), channelId = '22'.repeat(16), selfId = '33'.repeat(16), authorId = '44'.repeat(16), installationId = '55'.repeat(16), id = '66'.repeat(16);
// The clock starts on a whole millisecond: from a fractional start, 30 s later minus the start can fall a hair under 30 000 and skip the walk.
function fixture(over: Partial<PollSummary> = {}, capabilities: () => Promise<unknown> = () => Promise.resolve({ installationId, botUserId: authorId, supportedOperations: [3, 70] })) {
  let clock = Math.round(performance.now()); vi.spyOn(performance, 'now').mockImplementation(() => clock);
  const interval = vi.spyOn(window, 'setInterval');
  const client = new FakeClient(); const base = client.handler;
  client.handler = command => command.m === 'getBotCommandStatus' ? Promise.resolve({ installationId, botUserId: authorId, state: 3, revision: '1' }) : base(command);
  client.set('account', { instance: { id: 'instance' }, user: { id: selfId }, deviceId: 'device' } as never);
  client.set('communities', [{ id: communityId }] as never); client.set(`channels:${communityId}`, [{ id: channelId, kind: 0, group: 'active' }] as never);
  client.set(`members:${communityId}`, [{ userId: selfId, display: 'Ada' }, { userId: 'other', display: 'Bea' }] as never);
  let poll: PollSummary = { id, channelId, threadId: null, creatorId: selfId, question: 'Raid?', answers: ['Bridge', 'Harbour'], multi: false, counts: [1n, 2n], myChoices: [0], closed: false, revision: 1n, createdAt: 100n, dueAt: 86500n, ...over };
  const pollVote = vi.fn((input: { choices: number[] }) => { poll = { ...poll, myChoices: input.choices, revision: poll.revision + 1n }; return Promise.resolve(poll); });
  const pollGet = vi.fn(() => Promise.resolve(poll));
  const pollVoters = vi.fn(({ option, cursor }: { option: number; cursor?: Uint8Array | null }) => Promise.resolve({ voters: cursor || option === 1 ? ['other'] : [selfId], nextCursor: poll.multi && option === 0 && !cursor ? new Uint8Array([1]) : null }));
  const capabilitiesSpy = vi.fn(capabilities);
  // Task 21b fix round 1: the card reads the poll and its voters in one sealed poll.get. The voters it answers: you in the
  // first answer, Bea in the second (and in the first too for a multi-choice poll), each only where the answer has votes.
  let voters: { previews: string[][]; distinct: number; capped: boolean } | 'none' | null = null;
  const votersOf = (p: PollSummary) => {
    if (voters === 'none') return null;
    if (voters !== null) return voters;
    const mine = (option: number) => p.myChoices?.includes(option) === true ? [selfId] : [];
    const previews = p.counts.map((count, option) => count === 0n ? [] : [...mine(option), ...(option === 1 || p.multi ? ['other'] : [])]);
    return { previews, distinct: new Set(previews.flat()).size, capped: false };
  };
  const pollRead = vi.fn(async (...args: Parameters<typeof pollGet>) => { const read = await pollGet(...args); return { poll: read, voters: votersOf(read) }; });
  vi.spyOn(Core, 'createBotUtilities').mockReturnValue({ binding: { installationId, botUserId: authorId }, capabilities: capabilitiesSpy, pollGet, pollRead, pollVote, pollVoters } as never);
  render(<PollCard {...{ client, communityId, channelId, selfId, authorId }} threadId={null} body={`[dilla-poll:v1:${id}]`} bot edited={false} deleted={false} bits={0} />);
  // The worker's cue for a channel hint: its counter in the botHints slice rises.
  let hints = 0; const hint = () => { client.set('botHints', { [`1:${channelId}`]: ++hints }); };
  return { client, interval, capabilities: capabilitiesSpy, pollVote, pollVoters, pollGet, pollRead, setVoters(value: typeof voters) { voters = value; }, tick: () => { clock += 30_000; hint(); }, refreshNow: hint, get poll() { return poll; }, setPoll(over: Partial<PollSummary>) { poll = { ...poll, ...over }; }, close() { poll = { ...poll, closed: true }; } };
}
afterEach(() => vi.restoreAllMocks());
describe('Signal poll adapter', () => {
  it('updates on a channel hint and has no interval', async () => {
    const f = fixture();
    await waitFor(() => expect(f.pollGet).toHaveBeenCalledTimes(1));
    await act(async () => { await new Promise(resolve => setTimeout(resolve, 20)); });
    expect(f.pollGet).toHaveBeenCalledTimes(1);
    expect(f.client.calls).toContainEqual({ m: 'watchBotScope', kind: 1, channelId, on: true });
    f.setPoll({ counts: [4n, 2n], revision: 2n });
    await act(async () => { f.tick(); await Promise.resolve(); });
    await waitFor(() => expect(f.pollGet).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/6 voters/)).toBeInTheDocument();
    // A hint for another channel is not this card's.
    await act(async () => { f.client.set('botHints', { [`1:${'99'.repeat(16)}`]: 1 }); await new Promise(resolve => setTimeout(resolve, 20)); });
    expect(f.pollGet).toHaveBeenCalledTimes(2);
    expect(f.interval.mock.calls.filter(([, ms]) => ms === 30_000)).toEqual([]);
  });
  it('shows a Loading poll… skeleton before verification and retries a failed verification at 2 s, 5 s and 10 s', async () => {
    vi.useFakeTimers();
    try {
      const f = fixture(); f.pollGet.mockRejectedValue(new Error('down'));
      expect(screen.getByRole('status')).toHaveTextContent('Loading poll…');
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      expect(f.pollGet).toHaveBeenCalledTimes(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); }); expect(f.pollGet).toHaveBeenCalledTimes(2);
      await act(async () => { await vi.advanceTimersByTimeAsync(5_000); }); expect(f.pollGet).toHaveBeenCalledTimes(3);
      await act(async () => { await vi.advanceTimersByTimeAsync(10_000); }); expect(f.pollGet).toHaveBeenCalledTimes(4);
      // The retries ran out: the card steps aside and the post's own text stands, instead of "Loading poll…" forever.
      expect(screen.queryByRole('status')).toBeNull();
      await act(async () => { await vi.advanceTimersByTimeAsync(9_000); }); expect(f.pollGet).toHaveBeenCalledTimes(4);
    // Restore the spies while the fake timers are still installed: fixture() spied on window.setInterval then, and restoring
    // afterwards would put the fake one back for every later test.
    } finally { vi.restoreAllMocks(); vi.useRealTimers(); }
  });
  it('shows the post text at once, without the retry chain, when a loaded Bot is not the post\'s author', async () => {
    vi.useFakeTimers();
    try {
      const f = fixture({}, () => Promise.resolve({ installationId, botUserId: 'ee'.repeat(16), supportedOperations: [3, 70] }));
      expect(screen.getByRole('status')).toHaveTextContent('Loading poll…');
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      expect(screen.queryByRole('status')).toBeNull(); expect(f.pollGet).not.toHaveBeenCalled();
      // The shared list was reloaded once to cover a newly installed Bot, and no quick retry follows.
      expect(f.capabilities).toHaveBeenCalledTimes(2);
      await act(async () => { await vi.advanceTimersByTimeAsync(20_000); });
      expect(f.capabilities).toHaveBeenCalledTimes(2);
    } finally { vi.restoreAllMocks(); vi.useRealTimers(); }
  });
  it('shows the post text at once when the community has no ready Bot', async () => {
    vi.useFakeTimers();
    try {
      const f = fixture({}, () => Promise.reject(new Core.BotUtilitiesProviderUnavailableError()));
      expect(screen.getByRole('status')).toHaveTextContent('Loading poll…');
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      expect(screen.queryByRole('status')).toBeNull();
      await act(async () => { await vi.advanceTimersByTimeAsync(20_000); });
      expect(f.capabilities).toHaveBeenCalledTimes(1);
    } finally { vi.restoreAllMocks(); vi.useRealTimers(); }
  });
  // Final review M1: "the installation changed while capabilities were loading" is transient, not "no ready Bot".
  it('an installation change during verification refreshes the shared list once and retries, instead of giving up', async () => {
    vi.useFakeTimers();
    try {
      const changed = () => new Core.BotUtilitiesProviderUnavailableError(3, '77'.repeat(16), 'The Bot installation changed while capabilities were loading.');
      const f = fixture(); f.pollGet.mockRejectedValueOnce(changed());
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      expect(f.pollGet).toHaveBeenCalledTimes(1);
      expect(f.capabilities).toHaveBeenCalledTimes(2); // the shared list reloaded once
      expect(screen.getByRole('status')).toHaveTextContent('Loading poll…');
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
      expect(f.pollGet).toHaveBeenCalledTimes(2);
      expect(screen.getByText('Raid?')).toBeInTheDocument();
    } finally { vi.restoreAllMocks(); vi.useRealTimers(); }
  });
  it('an installation change while the shared list loads is a failure the card retries, not a refusal', async () => {
    vi.useFakeTimers();
    try {
      let attempt = 0;
      const f = fixture({}, () => {
        attempt += 1;
        return attempt === 1 ? Promise.reject(new Core.BotUtilitiesProviderUnavailableError(3, '77'.repeat(16), 'The Bot installation changed while capabilities were loading.'))
          : Promise.resolve({ installationId, botUserId: authorId, supportedOperations: [3, 70] });
      });
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      expect(commandBotSnapshot(f.client, communityId).unavailable).toBe(false);
      expect(screen.getByRole('status')).toHaveTextContent('Loading poll…');
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
      expect(screen.getByText('Raid?')).toBeInTheDocument();
    } finally { vi.restoreAllMocks(); vi.useRealTimers(); }
  });
  it('verifies from the shared Bot list: no capability read of its own, and none for a second card', async () => {
    const f = fixture(); await screen.findByText('Raid?');
    // One load, made by the shared store (not three by the card).
    expect(f.capabilities).toHaveBeenCalledTimes(1);
    expect(commandBotSnapshot(f.client, communityId).botUserId).toBe(authorId);
    render(<PollCard {...{ client: f.client, communityId, channelId, selfId, authorId }} threadId={null} body={`[dilla-poll:v1:${id}]`} bot edited={false} deleted={false} bits={0} />);
    await waitFor(() => expect(screen.getAllByText('Raid?')).toHaveLength(2));
    expect(f.capabilities).toHaveBeenCalledTimes(1);
  });
  it('pauses its hint refresh while the window is hidden and refreshes once when it is shown again, only if overdue', async () => {
    vi.useFakeTimers();
    let state: DocumentVisibilityState = 'visible';
    const spy = vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state);
    try {
      const f = fixture();
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      const reads = f.pollGet.mock.calls.length;
      const setVisibility = (next: DocumentVisibilityState) => { state = next; document.dispatchEvent(new Event('visibilitychange')); };
      setVisibility('hidden');
      await act(async () => { f.tick(); f.tick(); await vi.advanceTimersByTimeAsync(0); });
      expect(f.pollGet).toHaveBeenCalledTimes(reads);
      await act(async () => { setVisibility('visible'); await vi.advanceTimersByTimeAsync(0); });
      expect(f.pollGet).toHaveBeenCalledTimes(reads + 1);
      await act(async () => { setVisibility('hidden'); setVisibility('visible'); await vi.advanceTimersByTimeAsync(0); });
      expect(f.pollGet).toHaveBeenCalledTimes(reads + 1);
      await act(async () => { f.tick(); await vi.advanceTimersByTimeAsync(0); });
      expect(f.pollGet).toHaveBeenCalledTimes(reads + 2);
    } finally { spy.mockRestore(); vi.restoreAllMocks(); vi.useRealTimers(); }
  });
  it('single choice switches to [1] and clicking your pick withdraws []', async () => { const f = fixture(); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [1] }))); await userEvent.click(screen.getByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenLastCalledWith(expect.objectContaining({ choices: [] }))); });
  it('multi toggles within the set and counts distinct voters across every page', async () => { const f = fixture({ multi: true, counts: [2n, 2n] }); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [0, 1] }))); await userEvent.click(screen.getByRole('button', { name: /Bridge/ })); await waitFor(() => expect(f.pollVote).toHaveBeenLastCalledWith(expect.objectContaining({ choices: [1] })));  await waitFor(() => expect(screen.getByText('Open for 24 hours · pick any · 2 voters')).toBeInTheDocument()); expect(f.pollVoters).not.toHaveBeenCalled(); expect(screen.queryByRole('img', { name: 'other' })).not.toBeInTheDocument(); });
  it('uses dueAt minus createdAt, rounds vote percentages and previews first-page voters per answer', async () => { const f = fixture(); expect(await screen.findByText('Open for 24 hours · pick one · 3 voters')).toBeInTheDocument(); expect(screen.getByText('33%')).toBeInTheDocument(); await waitFor(() => expect(screen.getAllByRole('img', { name: 'Ada' })).toHaveLength(1)); expect(f.pollRead).toHaveBeenCalledTimes(1); expect(f.pollVoters).not.toHaveBeenCalled(); });
  it('refreshes to the closed card when a vote races a close', async () => { const f = fixture(); f.pollVote.mockImplementationOnce((input) => { f.close(); return Promise.reject(new Core.BotUtilitiesStatusError(73, 4, (input as unknown as { requestId: string }).requestId, null)); }); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await screen.findByText('CLOSED'); await userEvent.click(screen.getByRole('button', { name: /Bridge/ })); expect(f.pollVote).toHaveBeenCalledOnce(); });
});


it.each([{ multi: false, answer: 1 }, { multi: false, answer: 0 }, { multi: true, answer: 1 }])('only the pending answer retries an uncertain vote ($multi, $answer)', async ({ multi, answer }) => {
  const f = fixture({ multi, counts: [2n, 2n] }); f.pollVote.mockRejectedValueOnce(new Error('lost reply'));
  await userEvent.click(await screen.findByRole('button', { name: answer === 0 ? /Bridge/ : /Harbour/ }));
  await screen.findByText('Your vote may not have counted. Tap it again to retry.');
  const reads = f.pollGet.mock.calls.length; f.tick(); await waitFor(() => expect(f.pollGet).toHaveBeenCalledTimes(reads + 1)); await act(async () => { await Promise.resolve(); });
  expect(screen.getByRole('alert')).toHaveTextContent('Your vote may not have counted. Tap it again to retry.');
  const pending = screen.getByRole('button', { name: answer === 0 ? /Bridge/ : /Harbour/ });
  const other = screen.getByRole('button', { name: answer === 0 ? /Harbour/ : /Bridge/ });
  expect(other).toHaveAttribute('aria-disabled', 'true'); await userEvent.click(other); expect(f.pollVote).toHaveBeenCalledOnce();
  await userEvent.click(pending); await waitFor(() => expect(f.pollVote).toHaveBeenCalledTimes(2));
  expect(f.pollVote.mock.calls[1][0]).toEqual(f.pollVote.mock.calls[0][0]);
});
it('retains optimistic pressed state through a 30 second refresh while voting', async () => {
  const f = fixture(); let release!: (poll: PollSummary) => void;
  f.pollVote.mockImplementationOnce(() => new Promise(resolve => { release = resolve; }));
  const answer = await screen.findByRole('button', { name: /Harbour/ }); await userEvent.click(answer);
  const reads = f.pollGet.mock.calls.length; f.tick();
  await act(async () => { await Promise.resolve(); });
  expect(f.pollGet).toHaveBeenCalledTimes(reads);
  expect(answer).toHaveAttribute('aria-disabled', 'true');
  expect(answer).toHaveAttribute('aria-pressed', 'true');
  f.setPoll({ revision: 2n, myChoices: [1] }); release(f.poll);
  await waitFor(() => expect(answer).toHaveAttribute('aria-disabled', 'false')); expect(answer).toHaveAttribute('aria-pressed', 'true');
});
// Task 21b fix round 1: the voters come with the poll in one sealed read per hint; nothing reads them per answer.
it('takes the voters from each hint\'s poll read, with no voter reads of its own', async () => {
  const f = fixture({ multi: true, counts: [2n, 2n] });
  await screen.findByText('Open for 24 hours · pick any · 2 voters');
  await userEvent.click(screen.getByRole('button', { name: /Harbour/ }));
  await waitFor(() => expect(screen.getByRole('button', { name: /Harbour/ })).toHaveAttribute('aria-disabled', 'false'));
  f.setVoters({ previews: [[selfId, 'new1'], ['other', 'new2']], distinct: 4, capped: false });
  const reads = f.pollRead.mock.calls.length; f.tick();
  await waitFor(() => expect(f.pollRead).toHaveBeenCalledTimes(reads + 1));
  await screen.findByText('Open for 24 hours · pick any · 4 voters');
  expect(f.pollVoters).not.toHaveBeenCalled();
});
it('shows a capped distinct count with a plus', async () => {
  const f = fixture({ multi: true, counts: [1000n, 1000n] });
  f.setVoters({ previews: [['other'], ['other']], distinct: 2000, capped: true });
  f.tick();
  expect(await screen.findByText('Open for 24 hours · pick any · 2000+ voters')).toBeInTheDocument(); expect(f.pollVoters).not.toHaveBeenCalled();
});
it('keeps the last voters when a read answers none (an older Bot)', async () => {
  const f = fixture({ multi: true, counts: [2n, 2n] }); await screen.findByText('Open for 24 hours · pick any · 2 voters');
  f.setPoll({ revision: 2n }); f.setVoters('none'); const reads = f.pollRead.mock.calls.length; f.tick();
  await waitFor(() => expect(f.pollRead).toHaveBeenCalledTimes(reads + 1)); await act(async () => { await Promise.resolve(); });
  expect(screen.getByText('Open for 24 hours · pick any · 2 voters')).toBeInTheDocument();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('drops your own withdrawn avatar immediately while keeping other voters', async () => {
  const f = fixture({ myChoices: [0], counts: [1n, 1n] });
  await waitFor(() => expect(screen.getByRole('img', { name: 'Ada' })).toBeInTheDocument());
  await userEvent.click(screen.getByRole('button', { name: /Bridge/ }));
  await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [] })));
  await waitFor(() => expect(screen.queryByRole('img', { name: 'Ada' })).toBeNull());
  expect(screen.getByRole('img', { name: 'Bea' })).toBeInTheDocument();
});


