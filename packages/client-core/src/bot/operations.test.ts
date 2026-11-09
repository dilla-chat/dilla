import { describe, expect, it } from 'vitest';
import { decode, encode } from '../cbor';
import { fromHex } from '../hex';
import type { BotAnswer, BotCommandPort, BotCommandSubmit } from './commands';
import { createBotCommands } from './commands';
import { appOperationAnswer, BotOperationRefusedError, BotOperationUncertainError, createBotOperationsOn, sealedOperation, supportedV1Operations,
  v1Operation } from './operations';

const COMMUNITY = 'ab'.repeat(16);
const VOICE = 'cd'.repeat(16);
const ITEM = '12'.repeat(16);

/** A port whose Bot answers each submit with `answer(submit)`; null leaves it unanswered. */
function fakePort(answer: (s: BotCommandSubmit) => BotAnswer | null | 'refuse') {
  const submits: BotCommandSubmit[] = [];
  const listeners = new Map<string, (a: { requestId: string; seq: number; status: number; body: Uint8Array; final: boolean }) => void>();
  const timers: { fn: () => void; ms: number }[] = [];
  let n = 0;
  const port: BotCommandPort = {
    submit: (s) => {
      submits.push(s);
      const a = answer(s);
      if (a === 'refuse') return Promise.reject(Object.assign(new Error('E_BOT_OFFLINE'), { code: 'E_BOT_OFFLINE', status: 503 }));
      if (a !== null) queueMicrotask(() => listeners.get(s.requestId)?.({ requestId: s.requestId, seq: 0, ...a }));
      return Promise.resolve();
    },
    answers: (requestId, l) => { listeners.set(requestId, l); return () => listeners.delete(requestId); },
    refetchCatalog: () => Promise.resolve(),
    forget: () => undefined,
    setTimer: (fn, ms) => { timers.push({ fn, ms }); return timers.length - 1; },
    clearTimer: (h) => { const t = timers[h as number]; if (t !== undefined) t.fn = () => undefined; },
    newRequestId: () => (++n).toString(16).padStart(32, '0'),
  };
  return { port, submits, fire: () => { for (const t of [...timers]) t.fn(); } };
}

const ok = (revision: bigint | null, data: Uint8Array): BotAnswer => ({ status: 200, body: encode([revision, data]), final: true });
const refusal = (status: number, code: string): BotAnswer => ({ status, body: new TextEncoder().encode(code), final: true });

describe('sealed Bot operations (Task 21b)', () => {
  it('maps every v1 operation of the app surfaces to its sealed operation and back', () => {
    expect([71, 10, 84].map(sealedOperation)).toEqual([91, 92, 93]);
    expect([11, 21, 24, 31, 50, 70, 73, 83].map(sealedOperation)).toEqual([111, 121, 124, 131, 150, 170, 173, 183]);
    for (const op of [10, 11, 21, 31, 50, 71, 73, 84]) expect(v1Operation(sealedOperation(op))).toBe(op);
    expect(supportedV1Operations([{ operation: 91 }, { operation: 173 }, { operation: 80 }, { operation: 92 }])).toEqual([10, 71, 73]);
  });

  it('a mutation sends its revision first and its ids as bytes, on the voice channel, and reads the answer', async () => {
    const f = fakePort(() => ok(8n, encode([])));
    const ops = createBotOperationsOn(createBotCommands(f.port), COMMUNITY);
    const result = await ops.request({ operation: 21, channelId: VOICE, args: [ITEM], revision: '7' });
    expect(f.submits).toHaveLength(1);
    const sent = f.submits[0];
    expect(sent).toMatchObject({ communityId: COMMUNITY, channelId: VOICE, operation: 121 });
    expect(sent.args).toEqual([7, fromHex(ITEM)]);
    expect(result).toMatchObject({ requestId: sent.requestId, status: 0, revision: '8' });
    expect(decode(result.data)).toEqual([]);
  });

  it('a read sends the v1 arguments as they are and keeps a null revision', async () => {
    const f = fakePort(() => ok(null, encode([1n])));
    const ops = createBotOperationsOn(createBotCommands(f.port), COMMUNITY);
    const result = await ops.request({ operation: 84, args: [null] });
    expect(f.submits[0]).toMatchObject({ channelId: null, operation: 93, args: [null] });
    expect(result.revision).toBeNull();
  });

  it('a refusal comes back as the v1 status; an unsupported or offline Bot is a refused send', async () => {
    const statuses: [number, string, number][] = [[400, 'E_INVALID', 1], [403, 'E_FORBIDDEN', 2], [408, 'E_BOT_STALE', 3], [409, 'E_CONFLICT', 4],
      [503, 'E_UNAVAILABLE', 5], [429, 'E_BUSY', 6], [404, 'E_NOT_FOUND', 7], [500, 'E_INTERNAL', 9]];
    for (const [status, code, v1] of statuses) {
      const f = fakePort(() => refusal(status, code));
      const result = await createBotOperationsOn(createBotCommands(f.port), COMMUNITY).request({ operation: 73, args: [ITEM, encode([0n])], revision: '3' });
      expect(result.status).toBe(v1);
    }
    for (const answer of [refusal(404, 'E_BOT_UNSUPPORTED'), refusal(503, 'E_BOT_OFFLINE'), 'refuse' as const]) {
      const f = fakePort(() => answer);
      await expect(createBotOperationsOn(createBotCommands(f.port), COMMUNITY).request({ operation: 72, args: [VOICE, null, null] }))
        .rejects.toBeInstanceOf(BotOperationRefusedError);
    }
  });

  it('no answer in time is uncertain; read() resends the same request id and returns what the Bot answers then', async () => {
    let answered = false;
    const f = fakePort(() => (answered ? ok(4n, encode([])) : null));
    const ops = createBotOperationsOn(createBotCommands(f.port), COMMUNITY);
    const pending = ops.request({ operation: 74, args: [ITEM], revision: '3', requestId: '9'.repeat(32) });
    const settled = expect(pending).rejects.toBeInstanceOf(BotOperationUncertainError);
    for (let round = 0; round < 2; round++) { for (let i = 0; i < 10; i++) await Promise.resolve(); f.fire(); }
    await settled;
    expect(await ops.read('8'.repeat(32), 74)).toBeNull();
    answered = true;
    const read = await ops.read('9'.repeat(32), 74);
    expect(read).toMatchObject({ requestId: '9'.repeat(32), status: 0, revision: '4' });
    expect(f.submits.map(s => s.requestId)).toEqual(['9'.repeat(32), '9'.repeat(32), '9'.repeat(32)]);
    expect(f.submits[2].args).toEqual(f.submits[0].args);
  });

  // Task 21b fix round 1: a command can be lost on its way (the Bot's connection was re-established while it was relayed).
  // The first answer that does not come is followed by one resend of the same request, which the Bot answers once.
  it('resends an unanswered request once with its id before it is uncertain', async () => {
    let sends = 0;
    const f = fakePort(() => (++sends === 1 ? null : ok(2n, encode([]))));
    const ops = createBotOperationsOn(createBotCommands(f.port), COMMUNITY);
    const pending = ops.request({ operation: 71, args: [ITEM], requestId: '7'.repeat(32) });
    await Promise.resolve(); await Promise.resolve();
    f.fire();
    await expect(pending).resolves.toMatchObject({ status: 0, revision: '2' });
    expect(f.submits.map(s => s.requestId)).toEqual(['7'.repeat(32), '7'.repeat(32)]);
  });

  it('decodes only a well-formed answer', () => {
    expect(() => appOperationAnswer({ status: 200, body: encode(['x', new Uint8Array()]), final: true })).toThrow();
    expect(appOperationAnswer({ status: 200, body: encode([null, Uint8Array.of(1)]), final: true })).toEqual({ status: 0, revision: null, data: Uint8Array.of(1) });
    // A sealed read may answer a third element (poll.get's voters).
    expect(appOperationAnswer({ status: 200, body: encode([7n, Uint8Array.of(1), [1n]]), final: true })).toEqual({ status: 0, revision: '7', data: Uint8Array.of(1), extra: [1n] });
    expect(() => appOperationAnswer({ status: 200, body: encode([7n, Uint8Array.of(1), 1n, 2n]), final: true })).toThrow();
  });
});
