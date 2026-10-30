import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import * as Core from '@dilla/client-core';
import type { PollSummary } from '@dilla/client-core';
import { FakeClient } from '../../test/fake-client.ts';
import { PollCard } from './PollCard.tsx';
const communityId = '11'.repeat(16), channelId = '22'.repeat(16), selfId = '33'.repeat(16), authorId = '44'.repeat(16), installationId = '55'.repeat(16), id = '66'.repeat(16);
function fixture(over: Partial<PollSummary> = {}) {
  const client = new FakeClient(); client.set('account', { instance: { id: 'instance' }, user: { id: selfId }, deviceId: 'device' } as never);
  client.set('communities', [{ id: communityId }] as never); client.set(`channels:${communityId}`, [{ id: channelId, kind: 0, group: 'active' }] as never);
  client.set(`members:${communityId}`, [{ userId: selfId, display: 'Ada' }] as never);
  let poll: PollSummary = { id, channelId, threadId: null, creatorId: selfId, question: 'Raid?', answers: ['Bridge', 'Harbour'], multi: false, counts: [1n, 2n], myChoices: [0], closed: false, revision: 1n, createdAt: 100n, dueAt: 86500n, ...over };
  const pollVote = vi.fn((input: { choices: number[] }) => { poll = { ...poll, myChoices: input.choices, revision: poll.revision + 1n }; return Promise.resolve(poll); });
  const pollVoters = vi.fn(({ option, cursor }: { option: number; cursor?: Uint8Array | null }) => Promise.resolve({ voters: cursor ? ['other'] : [selfId], nextCursor: poll.multi && option === 0 && !cursor ? new Uint8Array([1]) : null }));
  vi.spyOn(Core, 'createBotUtilities').mockReturnValue({ binding: { installationId, botUserId: authorId }, capabilities: () => Promise.resolve({ installationId, botUserId: authorId }), pollGet: () => Promise.resolve(poll), pollVote, pollVoters } as never);
  render(<PollCard {...{ client, communityId, channelId, selfId, authorId }} threadId={null} body={`[dilla-poll:v1:${id}]`} bot edited={false} deleted={false} bits={0} />);
  return { pollVote, pollVoters, close() { poll = { ...poll, closed: true }; } };
}
afterEach(() => vi.restoreAllMocks());
describe('Signal poll adapter', () => {
  it('single choice switches to [1] and clicking your pick withdraws []', async () => { const f = fixture(); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [1] }))); await userEvent.click(screen.getByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenLastCalledWith(expect.objectContaining({ choices: [] }))); });
  it('multi toggles within the set and counts distinct voters across every page', async () => { const f = fixture({ multi: true }); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await waitFor(() => expect(f.pollVote).toHaveBeenCalledWith(expect.objectContaining({ choices: [0, 1] }))); await userEvent.click(screen.getByRole('button', { name: /Bridge/ })); await waitFor(() => expect(f.pollVote).toHaveBeenLastCalledWith(expect.objectContaining({ choices: [1] })));  await waitFor(() => expect(screen.getByText('Open for 24 hours · pick any · 2 voters')).toBeInTheDocument()); expect(f.pollVoters).toHaveBeenCalledWith(expect.objectContaining({ option: 0, cursor: new Uint8Array([1]) })); expect(screen.queryByRole('img', { name: 'other' })).not.toBeInTheDocument(); });
  it('uses dueAt minus createdAt, rounds vote percentages and previews first-page voters per answer', async () => { const f = fixture(); expect(await screen.findByText('Open for 24 hours · pick one · 3 voters')).toBeInTheDocument(); expect(screen.getByText('33%')).toBeInTheDocument(); await waitFor(() => expect(screen.getAllByRole('img', { name: 'Ada' })).toHaveLength(2)); expect(f.pollVoters).toHaveBeenCalledWith(expect.objectContaining({ option: 1, cursor: null })); });
  it('refreshes to the closed card when a vote races a close', async () => { const f = fixture(); f.pollVote.mockImplementationOnce((input) => { f.close(); return Promise.reject(new Core.BotUtilitiesStatusError(73, 4, (input as unknown as { requestId: string }).requestId, null)); }); await userEvent.click(await screen.findByRole('button', { name: /Harbour/ })); await screen.findByText('CLOSED'); await userEvent.click(screen.getByRole('button', { name: /Bridge/ })); expect(f.pollVote).toHaveBeenCalledOnce(); });
});
