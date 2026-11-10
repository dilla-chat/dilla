import { describe, expect, it } from 'vitest';
import type { CoreClient } from '../bridge';
import { encode } from '../cbor';
import { decodeModerationPage, ModerationPageError, readModerationPage } from './moderation-log';
import type { BotOperationRequest, BotOperations } from './operations';

const community = '11'.repeat(16);
const hex = (n: number): string => n.toString(16).padStart(2, '0').repeat(16);

function fake(data: Uint8Array, status = 0): { ops: BotOperations; sent: BotOperationRequest[] } {
  const sent: BotOperationRequest[] = [];
  return {
    sent,
    ops: {
      request: (input) => { sent.push(input); return Promise.resolve({ requestId: hex(9), status, revision: null, data }); },
      read: () => Promise.resolve(null),
    },
  };
}

describe('moderation log pages (Bot v2 Task 23)', () => {
  it('reads automatic cases as sealed operation 60 with no cursor', async () => {
    const row = [community, hex(2), hex(3), hex(4), hex(5), 2n, 'spam', 'remove', 'buy now', 1_800_000_000n];
    const { ops, sent } = fake(encode([1n, [row], null]));
    const page = await readModerationPage({} as CoreClient, community, 60, null, ops);
    expect(sent).toEqual([{ operation: 60, args: [null] }]);
    expect(page).toEqual({ status: 0, nextCursor: null, cases: [{ key: `${hex(5)}:2`, channelId: hex(3), actorId: hex(4), messageId: hex(5),
      revision: '2', reason: 'spam', action: 'remove', excerpt: 'buy now', receivedAt: '1800000000' }] });
  });

  it('reads manual evidence as operation 64 and passes the cursor on as bytes', async () => {
    const evidence = new Uint8Array(16).fill(7);
    const { ops, sent } = fake(encode([1n, [[evidence, 'warn', 'rude', '', 1_800_000_001n]], new Uint8Array([0xab, 0xcd])]));
    const page = await readModerationPage({} as CoreClient, community, 64, '0102', ops);
    expect(sent[0]).toEqual({ operation: 64, args: [new Uint8Array([1, 2])] });
    expect(page.nextCursor).toBe('abcd');
    expect(page.cases[0]).toMatchObject({ key: `manual:${'07'.repeat(16)}`, action: 'warn', reason: 'rude' });
  });

  it('returns a refusal as its status with no rows', async () => {
    const { ops } = fake(new Uint8Array(), 2);
    expect(await readModerationPage({} as CoreClient, community, 60, null, ops)).toEqual({ status: 2, cases: [], nextCursor: null });
  });

  it('refuses a case from another community and a malformed page', () => {
    const row = ['22'.repeat(16), hex(2), hex(3), hex(4), hex(5), 2n, 'spam', 'remove', 'x', 1n];
    expect(() => decodeModerationPage(60, encode([1n, [row], null]), community)).toThrow(ModerationPageError);
    expect(() => decodeModerationPage(64, encode([2n, [], null]), community)).toThrow(ModerationPageError);
  });
});
