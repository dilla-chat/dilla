import { afterEach, expect, it, vi } from 'vitest';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import * as Core from '@dilla/client-core';
import type { PollSummary } from '@dilla/client-core';
import { A, GEN, MEMBERS, PEER, P1, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';
import { ME } from '../../test/fixtures.ts';
import { PHONE_QUERY } from '../../layout.ts';

const BOT = '9f'.repeat(16), POLL = 'ab'.repeat(16), INSTALL = 'cd'.repeat(16);
const stamp = 1_800_003_600;
const fallback = `Raid?\nBridge\nHarbour\nVote with /poll vote.\nCloses: <t:${stamp}:R>\n[dilla-poll:v1:${POLL}]`;
const poll: PollSummary = { id: POLL, channelId: GEN, threadId: null, creatorId: PEER, question: 'Raid?', answers: ['Bridge', 'Harbour'], multi: false,
  counts: [0n, 0n], myChoices: [], closed: false, revision: 1n, createdAt: 1_800_000_000n, dueAt: BigInt(stamp) };
function open() {
  let resolve!: (row: PollSummary) => void;
  const read = new Promise<PollSummary>(done => { resolve = done; });
  vi.spyOn(Core, 'createBotUtilities').mockReturnValue({ binding: { installationId: INSTALL, botUserId: BOT },
    capabilities: () => Promise.resolve({ installationId: INSTALL, botUserId: BOT }), pollGet: () => read, pollRead: async () => ({ poll: await read, voters: null }),
    pollVoters: () => Promise.resolve({ voters: [], nextCursor: null }) } as never);
  const mounted = renderConversation({ items: [peerRow(P1, '1', fallback, { senderUser: BOT, bot: true })], before: fake => {
    fake.set(`members:${A}`, [...MEMBERS, { userId: BOT, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]);
  } });
  return { resolve, ...mounted };
}
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });
it('shows no raw text while the poll loads, then the verified card, naming its creator', async () => {
  const f = open();
  // Pending: the card says "Loading poll…" and the row holds none of the post's raw text.
  expect(within(rowOf(P1)).getByRole('status')).toHaveTextContent('Loading poll…');
  expect(rowOf(P1)).not.toHaveTextContent('Vote with /poll vote.');
  expect(rowOf(P1)).not.toHaveTextContent('Closes:');
  await act(() => { f.resolve(poll); return Promise.resolve(); });
  await screen.findByRole('button', { name: /Bridge/ });
  expect(rowOf(P1)).toHaveTextContent('bob asked:');
  expect(rowOf(P1)).not.toHaveTextContent('Glenn asked:');
  expect(rowOf(P1)).not.toHaveTextContent('Vote with /poll vote.');
  expect(rowOf(P1)).not.toHaveTextContent('Closes:');
  expect(rowOf(P1).querySelector('.d-tag')).toHaveAttribute('data-kind', 'bot');
});
it('shows the fallback with local time once verification gives up, when the returned poll belongs to another channel', async () => {
  vi.useFakeTimers();
  try {
    const f = open(); await act(() => { f.resolve({ ...poll, channelId: ME.id }); return Promise.resolve(); });
    const local = new Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }).format(new Date(stamp * 1000));
    expect(rowOf(P1)).not.toHaveTextContent('Vote with /poll vote.');
    expect(within(rowOf(P1)).getByRole('status')).toHaveTextContent('Loading poll…');
    await act(async () => { await vi.advanceTimersByTimeAsync(17_500); });
    expect(rowOf(P1)).toHaveTextContent('Vote with /poll vote.');
    expect(rowOf(P1)).toHaveTextContent(`Closes: ${local}`);
    expect(rowOf(P1)).not.toHaveTextContent('<t:');
    expect(within(rowOf(P1)).queryByRole('status')).toBeNull();
    expect(screen.queryByRole('button', { name: /Bridge/ })).toBeNull();
    expect(rowOf(P1)).not.toHaveTextContent('asked:');
  } finally { vi.useRealTimers(); }
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


it('phone poll votes toggle the row toolbar and a closed winner retains the revealed stripe state', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(Date.now());
  vi.stubGlobal('matchMedia', (query: string) => ({ matches: query === PHONE_QUERY, media: query, addEventListener() {}, removeEventListener() {} }));
  let record = { ...poll, creatorId: ME.id, multi: true };
  vi.spyOn(Core, 'createBotUtilities').mockReturnValue({ binding: { installationId: INSTALL, botUserId: BOT },
    capabilities: () => Promise.resolve({ installationId: INSTALL, botUserId: BOT }), pollGet: () => Promise.resolve(record), pollRead: () => Promise.resolve({ poll: record, voters: null }),
    pollVoters: () => Promise.resolve({ voters: [], nextCursor: null }),
    pollVote: ({ choices }: { choices: number[] }) => { record = { ...record, myChoices: choices, counts: [choices.includes(0) ? 1n : 0n, choices.includes(1) ? 1n : 0n], revision: record.revision + 1n }; return Promise.resolve(record); },
    pollClose: () => { record = { ...record, closed: true, counts: [2n, 1n], revision: record.revision + 1n }; return Promise.resolve(record); },
  } as never);
  const mounted = renderConversation({ items: [peerRow(P1, '1', fallback, { senderUser: BOT, bot: true })], before: fake => {
    fake.set(`members:${A}`, [...MEMBERS, { userId: BOT, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]);
  } });
  const row = rowOf(P1);
  const log = screen.getByRole('log');
  const bridge = await within(row).findByRole('button', { name: /Bridge/ });
  // The reader scrolls the poll into view, then a vote grows/re-pins the log within the same 800ms window.
  fireEvent.wheel(log);
  await mounted.user.click(bridge);
  await waitFor(() => expect(within(row).getByRole('button', { name: /Bridge/ })).toHaveAttribute('aria-disabled', 'false'));
  expect(within(row).getByRole('button', { name: /Bridge/ })).toHaveAttribute('aria-pressed', 'true');
  fireEvent.scroll(log); // layout adjustment after the optimistic/verified poll update, without new scroll intent
  expect(within(row).getByRole('toolbar')).toBeInTheDocument();
  expect(row).toHaveAttribute('data-revealed', 'true');
  await mounted.user.click(within(row).getByRole('button', { name: /Harbour/ }));
  await waitFor(() => expect(within(row).getByRole('button', { name: /Harbour/ })).toHaveAttribute('aria-disabled', 'false'));
  expect(within(row).queryByRole('toolbar')).toBeNull();
  expect(row).not.toHaveAttribute('data-revealed');
  await mounted.user.click(within(row).getByText('Raid?'));
  await mounted.user.click(within(row).getByRole('button', { name: 'Close poll' }));
  await within(row).findByText('CLOSED');
  expect(within(row).getByRole('button', { name: /Bridge/ })).toHaveAttribute('data-winner', 'true');
  expect(within(row).getByRole('toolbar')).toBeInTheDocument();
  expect(row).toHaveAttribute('data-revealed', 'true');
  fireEvent.wheel(log); fireEvent.scroll(log);
  expect(within(row).queryByRole('toolbar')).toBeNull();
  await mounted.user.click(within(row).getByText('Raid?'));
  expect(row).toHaveAttribute('data-revealed', 'true');
  fireEvent.touchStart(log); fireEvent.touchMove(log); fireEvent.scroll(log); fireEvent.touchEnd(log);
  expect(within(row).queryByRole('toolbar')).toBeNull();
  await mounted.user.click(within(row).getByText('Raid?'));
  fireEvent.scroll(log); // a new reveal also supersedes the previous touch scroll
  expect(row).toHaveAttribute('data-revealed', 'true');
  fireEvent.keyDown(row, { key: 'Escape' });
  expect(within(row).queryByRole('toolbar')).toBeNull();
  expect(row).not.toHaveAttribute('data-revealed');
});
