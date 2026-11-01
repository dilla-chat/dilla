import { act, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { GEN, P1, peerRow, renderConversation, rowOf, timeline } from '../../test/conversation.tsx';
import { builtinHandlers } from './builtin.ts';
import type { CommandExecutionContext } from './command-types.ts';

describe('built-in shrug rendering', () => {
  it('shows the classic shrug arm and underscores through the production message renderer', async () => {
    const { fake } = renderConversation();
    const binding = { communityId: null, channelId: GEN };
    const context: CommandExecutionContext = {
      requestId: `shrug-${crypto.randomUUID()}`, binding, client: fake,
      call: {} as never, getCall: () => context.call, getCurrentBinding: () => binding, isCurrent: () => true,
      navigate: () => undefined, openThread: () => undefined, openSearch: () => undefined,
      items: [], selfId: null, encodeMessage: text => text, confirm: () => Promise.resolve(false),
      openInvite: () => Promise.resolve(false), openGif: () => undefined, openModerationLog: () => undefined,
    };
    const result = await builtinHandlers.shrug!({ commandId: 'shrug', args: { text: '*waves*' } }, context);
    expect(result.status).toBe('success');
    const sent = fake.callsOf('send')[0];
    expect(sent?.text).toBeDefined();
    act(() => fake.set(`timeline:${GEN}`, timeline([peerRow(P1, '1', sent?.text ?? '')])));
    await waitFor(() => expect(rowOf(P1).querySelector('.d-message-row__body')?.textContent).toBe('¯\\_(ツ)_/¯ waves'));
    expect(rowOf(P1).querySelector('.d-message-row__body em')?.textContent).toBe('waves');
  });
});
