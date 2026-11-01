// Task S1: a sender is shown as a Bot (BOT tag, poll card, music card) only when the server's member row is of account
// kind 1. The credential's kind (item.bot) is signed by the sender's own SSK, so a modified client can claim it; alone it
// is never enough.
import { describe, expect, it } from 'vitest';
import type { MemberSummary } from '@dilla/client-core';
import { A, MEMBERS, P1, P2, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';

const GLENN = '9f'.repeat(16);
const POLL = 'ab'.repeat(16);
const BOT_MEMBER: MemberSummary = { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] };

describe('bot identity comes from the server member kind', () => {
  it('a human whose credential claims bot kind gets no BOT tag and no poll card', () => {
    const body = `Lunch?\n[dilla-poll:v1:${POLL}]`;
    renderConversation({
      // P1: a human member (PEER, kind 0) whose device credential claims bot kind; P2: the community's Bot.
      items: [peerRow(P1, '1', body, { bot: true }), peerRow(P2, '2', body, { bot: true, senderUser: GLENN })],
      before: fake => fake.set(`members:${A}`, [...MEMBERS, BOT_MEMBER]),
    });
    expect(rowOf(P1).querySelector('.d-tag[data-kind="bot"]')).toBeNull();
    // The marker is not read as a poll: it stays plain text, and no poll card is mounted.
    expect(rowOf(P1)).toHaveTextContent(`[dilla-poll:v1:${POLL}]`);
    expect(rowOf(P2).querySelector('.d-tag')).toHaveAttribute('data-kind', 'bot');
    expect(rowOf(P2)).not.toHaveTextContent('[dilla-poll:v1:');
  });

  it('a sender the member list does not name as a Bot is not a Bot', () => {
    renderConversation({
      items: [peerRow(P1, '1', 'hello', { bot: true, senderUser: GLENN })],
      before: fake => fake.set(`members:${A}`, MEMBERS),
    });
    expect(rowOf(P1).querySelector('.d-tag[data-kind="bot"]')).toBeNull();
  });

  it('a Bot account whose credential does not claim bot kind is not tagged either', () => {
    renderConversation({
      items: [peerRow(P1, '1', 'hello', { bot: false, senderUser: GLENN })],
      before: fake => fake.set(`members:${A}`, [...MEMBERS, BOT_MEMBER]),
    });
    expect(rowOf(P1).querySelector('.d-tag[data-kind="bot"]')).toBeNull();
  });
});
