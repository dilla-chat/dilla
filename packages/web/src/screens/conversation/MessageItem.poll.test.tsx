import { afterEach, expect, it, vi } from 'vitest';
import { act, screen, waitFor } from '@testing-library/react';
import * as Core from '@dilla/client-core';
import type { PollSummary } from '@dilla/client-core';
import { A, GEN, MEMBERS, PEER, P1, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';
import { ME } from '../../test/fixtures.ts';

const BOT = '9f'.repeat(16), POLL = 'ab'.repeat(16), INSTALL = 'cd'.repeat(16);
const stamp = 1_800_003_600;
const fallback = `Raid?\nBridge\nHarbour\nVote with /poll vote.\nCloses: <t:${stamp}:R>\n[dilla-poll:v1:${POLL}]`;
const poll: PollSummary = { id: POLL, channelId: GEN, threadId: null, creatorId: PEER, question: 'Raid?', answers: ['Bridge', 'Harbour'], multi: false,
  counts: [0n, 0n], myChoices: [], closed: false, revision: 1n, createdAt: 1_800_000_000n, dueAt: BigInt(stamp) };
function open() {
  let resolve!: (row: PollSummary) => void;
  const read = new Promise<PollSummary>(done => { resolve = done; });
  vi.spyOn(Core, 'createBotUtilities').mockReturnValue({ binding: { installationId: INSTALL, botUserId: BOT },
    capabilities: () => Promise.resolve({ installationId: INSTALL, botUserId: BOT }), pollGet: () => read,
    pollVoters: () => Promise.resolve({ voters: [], nextCursor: null }) } as never);
  const mounted = renderConversation({ items: [peerRow(P1, '1', fallback, { senderUser: BOT, bot: true })], before: fake => {
    fake.set(`members:${A}`, [...MEMBERS, { userId: BOT, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]);
  } });
  return { resolve, ...mounted };
}
afterEach(() => vi.restoreAllMocks());
it('keeps the fallback with local time until the verified poll card renders, then names its creator', async () => {
  const f = open();
  const local = new Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }).format(new Date(stamp * 1000));
  expect(rowOf(P1)).toHaveTextContent('Vote with /poll vote.');
  expect(rowOf(P1)).toHaveTextContent(`Closes: ${local}`);
  expect(rowOf(P1)).not.toHaveTextContent('<t:');
  await act(() => { f.resolve(poll); return Promise.resolve(); });
  await screen.findByRole('button', { name: /Bridge/ });
  expect(rowOf(P1)).toHaveTextContent('bob asked:');
  expect(rowOf(P1)).not.toHaveTextContent('Glenn asked:');
  expect(rowOf(P1)).not.toHaveTextContent('Vote with /poll vote.');
  expect(rowOf(P1)).not.toHaveTextContent('Closes:');
  expect(rowOf(P1).querySelector('.d-tag')).toHaveAttribute('data-kind', 'bot');
});
it('retains the fallback when the returned poll belongs to another channel', async () => {
  const f = open(); await act(() => { f.resolve({ ...poll, channelId: ME.id }); return Promise.resolve(); });
  await waitFor(() => expect(rowOf(P1)).toHaveTextContent('Vote with /poll vote.'));
  expect(screen.queryByRole('button', { name: /Bridge/ })).toBeNull();
  expect(rowOf(P1)).not.toHaveTextContent('asked:');
});

it('keeps row identity and keyboard focus when a verified poll is edited', async () => {
  const f = open(); await act(() => { f.resolve(poll); return Promise.resolve(); });
  await screen.findByRole('button', { name: /Bridge/ });
  const original = rowOf(P1); act(() => original.focus());
  act(() => f.fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false,
    items: [peerRow(P1, '1', fallback, { senderUser: BOT, bot: true, edited: true })] }));
  expect(rowOf(P1)).toBe(original);
  expect(rowOf(P1)).toHaveFocus();
  expect(rowOf(P1)).not.toHaveTextContent('bob asked:');
  expect(rowOf(P1)).toHaveTextContent('Vote with /poll vote.');
});
