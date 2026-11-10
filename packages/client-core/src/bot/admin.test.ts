import { describe, expect, it } from 'vitest';
import { encode } from '../cbor';
import { fromHex } from '../hex';
import type { BotAnswer, BotCommandPort, BotCommandSubmit } from './commands';
import { createBotCommands } from './commands';
import { BOT_STATUS_READ, decodeBotStatus, decodeBotWebhooks, decodeWebhookLabel, encodeBotStatus } from './admin';
import { createBotOperationsOn, sealedOperation } from './operations';

const COMMUNITY = 'ab'.repeat(16);
const RECEIPT = '61'.repeat(16);

function fakePort(answer: (s: BotCommandSubmit) => BotAnswer) {
  const submits: BotCommandSubmit[] = [];
  const listeners = new Map<string, (a: { requestId: string; seq: number; status: number; body: Uint8Array; final: boolean }) => void>();
  let n = 0;
  const port: BotCommandPort = {
    submit: (s) => {
      submits.push(s);
      const a = answer(s);
      queueMicrotask(() => listeners.get(s.requestId)?.({ requestId: s.requestId, seq: 0, ...a }));
      return Promise.resolve();
    },
    answers: (requestId, l) => { listeners.set(requestId, l); return () => listeners.delete(requestId); },
    refetchCatalog: () => Promise.resolve(),
    forget: () => undefined,
    setTimer: () => 0,
    clearTimer: () => undefined,
    newRequestId: () => (++n).toString(16).padStart(32, '0'),
  };
  return { port, submits };
}
const ok = (revision: bigint | null, data: Uint8Array): BotAnswer => ({ status: 200, body: encode([revision, data]), final: true });

describe('the admin surfaces on sealed commands (Task 22)', () => {
  it('welcome settings are app operations 101 and 102; the save sends its revision first', async () => {
    expect([1, 2, 61].map(sealedOperation)).toEqual([101, 102, 161]);
    const f = fakePort(() => ok(4n, encode([])));
    const ops = createBotOperationsOn(createBotCommands(f.port), COMMUNITY);
    await ops.request({ operation: 1, args: [5], revision: null });
    const doc = encode([1n]);
    await ops.request({ operation: 2, args: [5, doc], revision: '4' });
    expect(f.submits.map(s => s.operation)).toEqual([101, 102]);
    expect(f.submits[0].args).toEqual([5]);
    expect(f.submits[1].args).toEqual([4, 5, doc]);
  });

  it('moderation evidence is 161: revision 0 first, the receipt id as 16 bytes', async () => {
    const f = fakePort(() => ok(null, new Uint8Array()));
    const ops = createBotOperationsOn(createBotCommands(f.port), COMMUNITY);
    const result = await ops.request({ operation: 61, args: [RECEIPT, 'spam', 'buy now'], revision: '0' });
    expect(f.submits[0].operation).toBe(161);
    expect(f.submits[0].args).toEqual([0, fromHex(RECEIPT), 'spam', 'buy now']);
    expect(result.status).toBe(0);
  });

  it('the Bot page status read (94) is sent as it is, with no arguments', async () => {
    expect(sealedOperation(BOT_STATUS_READ)).toBe(94);
    const data = encodeBotStatus({ needsReAdd: ['70'.repeat(16)], musicStarts: [{ firstFrameMs: 640, cold: true }] });
    const f = fakePort(() => ok(null, data));
    const ops = createBotOperationsOn(createBotCommands(f.port), COMMUNITY);
    const result = await ops.request({ operation: BOT_STATUS_READ, args: [] });
    expect(f.submits[0]).toMatchObject({ operation: 94, args: [] });
    expect(decodeBotStatus(result.data)).toEqual({ needsReAdd: ['70'.repeat(16)], musicStarts: [{ firstFrameMs: 640, cold: true }] });
  });

  it('decodes the status strictly: a version, 16-byte ids, at most 20 starts', () => {
    expect(() => decodeBotStatus(encode([2n, [], []]))).toThrow();
    expect(() => decodeBotStatus(encode([1n, [Uint8Array.of(1)], []]))).toThrow();
    expect(() => decodeBotStatus(encode([1n, [], Array.from({ length: 21 }, () => [1n, 0n])]))).toThrow();
    expect(() => decodeBotStatus(encode([1n, [], [[1n, 2n]]]))).toThrow();
  });

  it('decodes the webhook list rows', () => {
    const wid = fromHex('77'.repeat(16)); const channel = fromHex('88'.repeat(16));
    const rows = decodeBotWebhooks(encode([[wid, channel, 'CI', null, 1n, 1_800_000_000n, null]]));
    expect(rows).toEqual([{ webhookId: '77'.repeat(16), channelId: '88'.repeat(16), name: 'CI', avatar: null, allowEveryone: true,
      createdAt: 1_800_000_000, rotatedAt: null }]);
  });

  it('reads the webhook label line and leaves every other text alone', () => {
    const label = `[dilla-webhook:v1:${'77'.repeat(16)}:Terraria%3A Hardcore:${'a7'.repeat(32)}]`;
    expect(decodeWebhookLabel(`boss down\n${label}`)).toEqual({ text: 'boss down', webhookId: '77'.repeat(16), name: 'Terraria: Hardcore',
      avatarId: 'a7'.repeat(32) });
    expect(decodeWebhookLabel(`hi\n[dilla-webhook:v1:${'77'.repeat(16)}:CI]`)).toMatchObject({ text: 'hi', name: 'CI', avatarId: null });
    // A defanged marker, a label that is not the last line, and a malformed id are not labels.
    expect(decodeWebhookLabel(`(dilla-webhook:v1:${'77'.repeat(16)}:CI]`)).toBeNull();
    expect(decodeWebhookLabel(`[dilla-webhook:v1:${'77'.repeat(16)}:CI]\nmore`)).toBeNull();
    expect(decodeWebhookLabel('x\n[dilla-webhook:v1:zz:CI]')).toBeNull();
  });
});
