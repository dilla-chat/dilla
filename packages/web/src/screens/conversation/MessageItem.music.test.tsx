// Fix round 1 (Jonas 2026-10-28): Glenn's public "started music" message reads the prototype's line and carries the live card.
import { describe, expect, it } from 'vitest';
import { within } from '@testing-library/react';
import { A, CHANNEL, MEMBERS, P1, P2, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';
import { ME } from '../../test/fixtures.ts';

const HARBOUR = '4e'.repeat(16);
const GLENN = '9f'.repeat(16);
const ITEM = '6d'.repeat(16);

describe('a "started music" message', () => {
  it('reads "{name} started music in {room}" with the player card, from a bot only', () => {
    const marker = `Music started.\n[dilla-music:v1:${HARBOUR}:${ME.id}:${ITEM}]`;
    renderConversation({
      items: [peerRow(P1, '1', marker, { bot: true, senderUser: GLENN }), peerRow(P2, '2', marker)],
      before: fake => {
        fake.set(`channels:${A}`, [CHANNEL, { ...CHANNEL, id: HARBOUR, kind: 1, name: 'harbour' }]);
        fake.set(`members:${A}`, [...MEMBERS, { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]);
      },
    });
    const bot = rowOf(P1);
    expect(bot).toHaveTextContent('Ada L started music in harbour');
    expect(bot).not.toHaveTextContent('dilla-music');
    const card = within(bot).getByRole('group', { name: 'Music player' });
    // The Bot is not in harbour in this fixture: the card says it left voice.
    expect(card).toHaveTextContent('left voice');
    expect(within(rowOf(P2)).queryByRole('group', { name: 'Music player' })).toBeNull();
  });

  it('security review M1: no card for a bot-flagged member who is not the managed Bot account, nor for an edited message', () => {
    const marker = `Music started.\n[dilla-music:v1:${HARBOUR}:${ME.id}:${ITEM}]`;
    renderConversation({
      // P1: a human member whose device credential claims bot kind; P2: the Bot's own message, edited.
      items: [peerRow(P1, '1', marker, { bot: true }), peerRow(P2, '2', marker, { bot: true, senderUser: GLENN, edited: true })],
      before: fake => fake.set(`members:${A}`, [...MEMBERS, { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]),
    });
    for (const id of [P1, P2]) {
      expect(within(rowOf(id)).queryByRole('group', { name: 'Music player' })).toBeNull();
      expect(rowOf(id)).not.toHaveTextContent('started music in');
    }
  });

  it('security review L3: a room this viewer cannot see reads "a voice channel", never its id', () => {
    renderConversation({
      items: [peerRow(P1, '1', `Music started.\n[dilla-music:v1:${HARBOUR}:${ME.id}:${ITEM}]`, { bot: true, senderUser: GLENN })],
      before: fake => fake.set(`members:${A}`, [...MEMBERS, { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]),
    });
    expect(rowOf(P1)).toHaveTextContent('Ada L started music in a voice channel');
    expect(rowOf(P1)).not.toHaveTextContent(HARBOUR.slice(0, 8));
  });

  it('Task S1: the room name follows the channel list when it arrives after the message', () => {
    const { fake } = renderConversation({
      items: [peerRow(P1, '1', `Music started.\n[dilla-music:v1:${HARBOUR}:${ME.id}:${ITEM}]`, { bot: true, senderUser: GLENN })],
      before: f => f.set(`members:${A}`, [...MEMBERS, { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]),
    });
    expect(rowOf(P1)).toHaveTextContent('Ada L started music in a voice channel');
    fake.publish(`channels:${A}`, [CHANNEL, { ...CHANNEL, id: HARBOUR, kind: 1, name: 'harbour' }]);
    expect(rowOf(P1)).toHaveTextContent('Ada L started music in harbour');
  });
});
