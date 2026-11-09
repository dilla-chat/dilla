// Bot v2 Task 22: a webhook post is the Bot's message ending in its label line. It shows "via webhook <name>" only when
// the community's pinned Bot user wrote it; the avatar comes only from the instance's member route, else the placeholder.
import { waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { BotCatalogView, MemberSummary } from '@dilla/client-core';
import { A, MEMBERS, P1, P2, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';
import { t } from '../../strings/index.ts';

const GLENN = '9f'.repeat(16);
const HOOK = '77'.repeat(16);
const AVATAR = 'a7'.repeat(32);
const BOT_MEMBER: MemberSummary = { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] };
const catalog = (botUserId: string | null): BotCatalogView => ({ revision: '1', commands: [], keyId: null, fingerprint: null,
  status: { state: 'ready', since: null }, botChannels: [], botUserId });

describe('webhook posts (Task 22)', () => {
  it('a webhook post shows via webhook <name>', () => {
    renderConversation({
      items: [peerRow(P1, '1', `Build 812 passed\n[dilla-webhook:v1:${HOOK}:CI%3A main]`, { bot: true, senderUser: GLENN })],
      before: fake => { fake.set(`members:${A}`, [...MEMBERS, BOT_MEMBER]); fake.set(`botCatalog:${A}`, catalog(GLENN)); },
    });
    expect(rowOf(P1)).toHaveTextContent(t('message.viaWebhook', { name: 'CI: main' }));
    expect(rowOf(P1)).toHaveTextContent('Build 812 passed');
    expect(rowOf(P1)).not.toHaveTextContent('[dilla-webhook:');
  });

  it('the label is plain text from anyone but the pinned Bot user', () => {
    const body = `hi\n[dilla-webhook:v1:${HOOK}:Fake]`;
    renderConversation({
      // P1 is a human; P2 is a Bot account the server names, but not the Bot user this device pinned.
      items: [peerRow(P1, '1', body), peerRow(P2, '2', body, { bot: true, senderUser: GLENN })],
      before: fake => { fake.set(`members:${A}`, [...MEMBERS, BOT_MEMBER]); fake.set(`botCatalog:${A}`, catalog('8e'.repeat(16))); },
    });
    for (const row of [rowOf(P1), rowOf(P2)]) {
      expect(row).not.toHaveTextContent(t('message.viaWebhook', { name: 'Fake' }));
      expect(row).toHaveTextContent('[dilla-webhook:');
    }
  });

  it('the avatar is read from the instance route only, and a 404 keeps the placeholder', async () => {
    const opened = renderConversation({
      items: [peerRow(P1, '1', `deploy\n[dilla-webhook:v1:${HOOK}:CD:${AVATAR}]`, { bot: true, senderUser: GLENN })],
      // The default handler answers getWebhookAvatar with null: the instance's 404 (its in-memory cache lost the image).
      before: fake => { fake.set(`members:${A}`, [...MEMBERS, BOT_MEMBER]); fake.set(`botCatalog:${A}`, catalog(GLENN)); },
    });
    await waitFor(() => expect(opened.fake.callsOf('getWebhookAvatar')).toEqual([{ m: 'getWebhookAvatar', communityId: A, avatarId: AVATAR }]));
    expect(rowOf(P1).querySelector('img')).toBeNull();
    expect(rowOf(P1).querySelector('.d-message-row__avatar')).not.toBeNull();
  });
});
