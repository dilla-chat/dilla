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
  let tick!: () => void; let clock = Math.round(performance.now()); vi.spyOn(performance, 'now').mockImplementation(() => clock); const interval = window.setInterval.bind(window);
  vi.spyOn(window, 'setInterval').mockImplementation(((fn: () => void, ms: number) => { if (ms === 30_000) { tick = fn; return 900001; } return interval(fn, ms); }) as never);
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
  vi.spyOn(Core, 'createBotUtilities').mockReturnValue({ binding: { installationId, botUserId: authorId }, capabilities: capabilitiesSpy, pollGet, pollVote, pollVoters } as never);
  render(<PollCard {...{ client, communityId, channelId, selfId, authorId }} threadId={null} body={`[dilla-poll:v1:${id}]`} bot edited={false} deleted={false} bits={0} />);
  return { client, capabilities: capabilitiesSpy, pollVote, pollVoters, pollGet, tick: () => { clock += 30_000; tick(); }, refreshNow: () => tick(), get poll() { return poll; }, setPoll(over: Partial<PollSummary>) { poll = { ...poll, ...over }; }, close() { poll = { ...poll, closed: true }; } };
}
afterEach(() => vi.restoreAllMocks());
describe('Signal poll adapter', () => {
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
  it('verifies from the shared Bot list: no capability read of its own, and none for a second card', async () => {
    const f = fixture(); await screen.findByText('Raid?');
    // One load, made by the shared store (not three by the card).
    expect(f.capabilities).toHaveBeenCalledTimes(1);
    expect(commandBotSnapshot(f.client, communityId).botUserId).toBe(authorId);
    render(<PollCard {...{ client: f.client, communityId, channelId, selfId, authorId }} threadId={null} body={`[dilla-poll:v1:${id}]`} bot edited={false} deleted={false} bits={0} />);
    await waitFor(() => expect(screen.getAllByText('Raid?')).toHaveLength(2));
    expect(f.capabilities).toHaveBeenCalledTimes(1);
  });
  it('single choice switches to [1] and clicking your pick withdraws []', async () => { const f = fixture(); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [1] }))); await userEvent.click(screen.getByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenLastCalledWith(expect.objectContaining({ choices: [] }))); });
  it('multi toggles within the set and counts distinct voters across every page', async () => { const f = fixture({ multi: true, counts: [2n, 2n] }); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [0, 1] }))); await userEvent.click(screen.getByRole('button', { name: /Bridge/ })); await waitFor(() => expect(f.pollVote).toHaveBeenLastCalledWith(expect.objectContaining({ choices: [1] })));  await waitFor(() => expect(screen.getByText('Open for 24 hours · pick any · 2 voters')).toBeInTheDocument()); expect(f.pollVoters).toHaveBeenCalledWith(expect.objectContaining({ option: 0, cursor: new Uint8Array([1]) })); expect(screen.queryByRole('img', { name: 'other' })).not.toBeInTheDocument(); });
  it('uses dueAt minus createdAt, rounds vote percentages and previews first-page voters per answer', async () => { const f = fixture(); expect(await screen.findByText('Open for 24 hours · pick one · 3 voters')).toBeInTheDocument(); expect(screen.getByText('33%')).toBeInTheDocument(); await waitFor(() => expect(screen.getAllByRole('img', { name: 'Ada' })).toHaveLength(1)); expect(f.pollVoters).toHaveBeenCalledWith(expect.objectContaining({ option: 1, cursor: null })); });
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
it('walks once per cadence across two vote revisions and keeps the last count while the next walk loads', async () => {
  const f = fixture({ multi: true, counts: [2n, 2n] });
  await screen.findByText('Open for 24 hours · pick any · 2 voters'); const calls = f.pollVoters.mock.calls.length;
  await userEvent.click(screen.getByRole('button', { name: /Harbour/ }));
  await waitFor(() => expect(screen.getByRole('button', { name: /Harbour/ })).toHaveAttribute('aria-disabled', 'false'));
  await userEvent.click(screen.getByRole('button', { name: /Bridge/ }));
  await waitFor(() => expect(f.pollVote).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(screen.getByRole('button', { name: /Bridge/ })).toHaveAttribute('aria-disabled', 'false'));
  const reads = f.pollGet.mock.calls.length; f.refreshNow();
  await waitFor(() => expect(f.pollGet).toHaveBeenCalledTimes(reads + 1)); await act(async () => { await Promise.resolve(); });
  expect(f.pollVoters).toHaveBeenCalledTimes(calls);
  let release!: (value: { voters: string[]; nextCursor: null }) => void;
  f.pollVoters.mockImplementationOnce(() => new Promise(resolve => { release = resolve; }));
  f.tick(); await waitFor(() => expect(release).toBeDefined());
  expect(screen.getByText('Open for 24 hours · pick any · 2 voters')).toBeInTheDocument();
  expect(screen.queryByRole('img', { name: 'Ada' })).toBeNull();
  expect(screen.getByRole('img', { name: 'Bea' })).toBeInTheDocument();
  release({ voters: [selfId, 'new1', 'new2'], nextCursor: null });
  await screen.findByText('Open for 24 hours · pick any · 4 voters');
});
it('shows the distinct count read so far with a plus at the multi-choice budget cap', async () => {
  const f = fixture({ multi: true, counts: [1000n, 1000n] }); let p = 0;
  f.pollVoters.mockImplementation(() => { const n = ++p; return Promise.resolve({ voters: Array.from({ length: 20 }, (_, i) => `${n}-${i}`), nextCursor: new Uint8Array([n]) }); });
  expect(await screen.findByText('Open for 24 hours · pick any · 200+ voters')).toBeInTheDocument(); expect(f.pollVoters).toHaveBeenCalledTimes(10);
});
it('keeps the last distinct count after a voter read fails', async () => {
  const f = fixture({ multi: true, counts: [2n, 2n] }); await screen.findByText('Open for 24 hours · pick any · 2 voters');
  f.setPoll({ revision: 2n }); f.pollVoters.mockRejectedValueOnce(new Error('read failed')); f.tick();
  await screen.findByText('Could not load voter names.'); expect(screen.getByText('Open for 24 hours · pick any · 2 voters')).toBeInTheDocument();
});

it('drops your own withdrawn avatar immediately while keeping other voters until the next walk', async () => {
  const f = fixture({ myChoices: [0], counts: [1n, 1n] });
  await waitFor(() => expect(screen.getByRole('img', { name: 'Ada' })).toBeInTheDocument());
  await userEvent.click(screen.getByRole('button', { name: /Bridge/ }));
  await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [] })));
  await waitFor(() => expect(screen.queryByRole('img', { name: 'Ada' })).toBeNull());
  expect(screen.getByRole('img', { name: 'Bea' })).toBeInTheDocument();
});


it('keeps a voter read error across a successful periodic refresh', async () => {
  const f = fixture({ multi: true, counts: [2n, 2n] });
  await screen.findByText('Open for 24 hours · pick any · 2 voters');
  f.setPoll({ revision: 2n }); f.pollVoters.mockRejectedValueOnce(new Error('read failed')); f.tick();
  await screen.findByText('Could not load voter names.');
  f.setPoll({ revision: 3n }); const reads = f.pollGet.mock.calls.length; f.tick();
  await waitFor(() => expect(f.pollGet).toHaveBeenCalledTimes(reads + 1));
  await act(async () => { await Promise.resolve(); });
  expect(screen.getByRole('alert')).toHaveTextContent('Could not load voter names.');
});
