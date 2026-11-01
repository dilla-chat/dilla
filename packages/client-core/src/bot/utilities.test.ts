import { describe, expect, it, vi } from 'vitest';
import type { CoreClient } from '../bridge';
import { BotInteractionUncertainError } from './interactions.ts';
import { decode, encode } from '../cbor/index.ts';
import {
  BotUtilitiesError, BotUtilitiesProviderUnavailableError, BotUtilitiesStatusError, BotUtilitiesUncertainMutationError, createBotUtilities, decodeBotUtilityCapabilities, decodePollClose, decodePollCreate, decodePollGet, decodePollListPage, decodePollResponse, decodePollVote,
  decodePollVotersPage, decodeReminderCancel, decodeReminderCreate, decodeReminderDismiss, decodeReminderListPage,
  encodePollCreatePayload, encodePollSelection, encodeReminderCreatePayload,
  type PollSummary,
} from './utilities.ts';
import { normalizeReminderTimezone } from './utility-time.ts';

const id = (byte: number) => new Uint8Array(16).fill(byte);
const hex = (byte: number) => byte.toString(16).padStart(2, '0').repeat(16);
const pollSummary = (overrides: Record<number, unknown> = {}) => [
  id(1), id(2), null, id(3), 'Lunch?', ['Soup', 'Salad'], 0n,
  1_800_000_000n, 1_800_003_600n, 0n, [2n, 4n], [], 1n,
].map((value, index) => overrides[index] === undefined ? value : overrides[index]);
const reminderSummary = (overrides: Record<number, unknown> = {}) => [
  id(4), 'Call Åsa', 1_800_000_060n, 'Europe/Stockholm', id(2), id(5), 0n, 1n,
].map((value, index) => overrides[index] === undefined ? value : overrides[index]);
const data = (value: unknown) => encode(value as never);

const expectedPoll: PollSummary = {
  id: hex(1), channelId: hex(2), threadId: null, creatorId: hex(3), question: 'Lunch?', answers: ['Soup', 'Salad'],
  multi: false, createdAt: 1_800_000_000n, dueAt: 1_800_003_600n, closed: false, counts: [2n, 4n], myChoices: [], revision: 1n,
};

describe('utility request payloads', () => {
  it('encodes a strict poll creation tuple using byte IDs', () => {
    const result = encodePollCreatePayload({
      channelId: hex(2), threadId: hex(5), question: 'Lunch?', answers: ['Soup', 'Salad'], multi: true, durationSeconds: 3600,
    });
    expect(result).toEqual(encode([1n, id(2), id(5), 'Lunch?', ['Soup', 'Salad'], 1n, 3600n]));
    expect(result.length).toBeLessThanOrEqual(1800);
  });

  it.each([
    { question: ' \u0085 ', answers: ['A', 'B'] },
    { question: 'Q', answers: ['A', 'A'] },
    { question: 'Q', answers: ['A'] },
    { question: 'Q', answers: Array.from({ length: 11 }, (_, i) => `A${i}`) },
    { question: 'Q', answers: ['é'.repeat(101), 'B'] },
  ])('rejects malformed poll question or answers %#', ({ question, answers }) => {
    expect(() => encodePollCreatePayload({ channelId: hex(2), question, answers, multi: false, durationSeconds: 3600 })).toThrow(BotUtilitiesError);
  });

  it('rejects non-preset poll durations, malformed IDs, and threads without channels', () => {
    expect(() => encodePollCreatePayload({ channelId: hex(2), question: 'Q', answers: ['A', 'B'], multi: false, durationSeconds: 7200 })).toThrow(BotUtilitiesError);
    expect(() => encodePollCreatePayload({ channelId: 'bad', question: 'Q', answers: ['A', 'B'], multi: false, durationSeconds: 3600 })).toThrow(BotUtilitiesError);
    expect(() => encodePollCreatePayload({ channelId: hex(2), threadId: hex(0), question: 'Q', answers: ['A', 'B'], multi: false, durationSeconds: 3600 })).toThrow(BotUtilitiesError);
  });

  it('reports aggregate CBOR byte budget for a too-large poll payload instead of truncating', () => {
    try {
      encodePollCreatePayload({ channelId: hex(2), question: '😀'.repeat(300), answers: Array.from({ length: 10 }, (_, i) => `${i}${'😀'.repeat(99)}`), multi: false, durationSeconds: 3600 });
      throw new Error('expected payload overflow');
    } catch (error) {
      expect(error).toBeInstanceOf(BotUtilitiesError);
      expect((error as Error).message).toMatch(/bytes.*1800/u);
    }
  });

  it('encodes bounded reminder creation payload and optional source IDs', () => {
    expect(encodeReminderCreatePayload({
      whenKind: 0, when: 60, text: 'Call Åsa', timezone: 'Europe/Stockholm', sourceChannelId: hex(2), sourceThreadId: hex(5),
    })).toEqual(encode([1n, 0n, 60n, 'Call Åsa', 'Europe/Stockholm', id(2), id(5)]));
  });

  it.each([
    { whenKind: 0, when: 59 },
    { whenKind: 0, when: 31_536_001 },
    { whenKind: 1, when: 0 },
    { whenKind: 1, when: 253_402_300_800 },
  ])('rejects reminder time outside protocol bounds %#', ({ whenKind, when }) => {
    expect(() => encodeReminderCreatePayload({ whenKind: whenKind as 0 | 1, when, text: 'x', timezone: 'UTC' })).toThrow(BotUtilitiesError);
  });

  it('checks UTF-8 text limits, timezone, and source thread relationship', () => {
    expect(() => encodeReminderCreatePayload({ whenKind: 0, when: 60, text: 'é'.repeat(751), timezone: 'UTC' })).toThrow(BotUtilitiesError);
    expect(() => encodeReminderCreatePayload({ whenKind: 0, when: 60, text: 'x', timezone: 'Local' })).toThrow(BotUtilitiesError);
    expect(() => encodeReminderCreatePayload({ whenKind: 0, when: 60, text: 'x', timezone: 'UTC', sourceThreadId: hex(5) })).toThrow(BotUtilitiesError);
  });

  it('canonicalizes valid timezone aliases and rejects fixed offsets before encoding', () => {
    expect(normalizeReminderTimezone('europe/stockholm')).toBe('Europe/Stockholm');
    expect(normalizeReminderTimezone('PST')).toBe('America/Los_Angeles');
    expect(normalizeReminderTimezone('UTC')).toBe('UTC');
    for (const timezone of ['+01:00', '-0530', '+05']) {
      expect(() => encodeReminderCreatePayload({ whenKind: 0, when: 60, text: 'x', timezone })).toThrow(BotUtilitiesError);
    }
    const payload = encodeReminderCreatePayload({ whenKind: 0, when: 60, text: 'x', timezone: 'europe/stockholm' });
    expect(decode(payload)).toEqual([1n, 0n, 60n, 'x', 'Europe/Stockholm', null, null]);
  });

  it('encodes unique bounded poll selections and permits withdrawal as an empty selection', () => {
    expect(encodePollSelection([3, 0, 2])).toEqual(encode([3n, 0n, 2n]));
    expect(encodePollSelection([])).toEqual(encode([]));
    expect(() => encodePollSelection([1, 1])).toThrow(BotUtilitiesError);
    expect(() => encodePollSelection([10])).toThrow(BotUtilitiesError);
  });
});

describe('utility response decoders', () => {
  it('decodes the exact capability tuple and binds installation and bot IDs', () => {
    const caps = decodeBotUtilityCapabilities(data([1n, id(7), id(8), 1n, [3n, 70n, 71n, 72n, 73n, 74n, 75n, 80n, 81n, 82n, 83n, 84n]]));
    expect(caps).toEqual({ installationId: hex(7), botUserId: hex(8), version: 1, supportedOperations: [3, 70, 71, 72, 73, 74, 75, 80, 81, 82, 83, 84] });
    expect(() => decodeBotUtilityCapabilities(data([1n, id(7), id(8), 1n, [3n]]), { installationId: hex(9) })).toThrow(BotUtilitiesError);
  });

  it('decodes the report of a Bot with Music on, which includes the music announce operation (25)', () => {
    // internal/bot/protocol/codec.go OpMusicState..OpMusicAnnounce (10..25): the Bot reports these to each member when
    // Music is on (internal/bot/service/command_capabilities.go). Rejecting 25 hid every Bot command from the picker.
    const music = [10n, 11n, 12n, 13n, 14n, 15n, 16n, 17n, 18n, 19n, 20n, 21n, 22n, 23n, 25n];
    const caps = decodeBotUtilityCapabilities(data([1n, id(7), id(8), 1n, [3n, ...music, 70n, 71n, 72n, 73n, 74n, 75n, 80n, 81n, 82n, 83n, 84n]]));
    expect(caps.supportedOperations).toContain(25);
    expect(caps.supportedOperations).toContain(70);
    expect(caps.supportedOperations).toContain(80);
  });

  it('drops an operation this client does not know and keeps the rest of the report', () => {
    const caps = decodeBotUtilityCapabilities(data([1n, id(7), id(8), 1n, [3n, 70n, 199n, 255n]]));
    expect(caps.supportedOperations).toEqual([3, 70]);
  });

  it('rejects duplicate, unsorted, or malformed capability entries', () => {
    for (const value of [
      [1n, id(7), id(8), 1n, [70n, 70n]],
      [1n, id(7), id(8), 1n, [71n, 70n]],
      [1n, id(7), id(8), 1n, [256n]],
      [1n, id(7), id(8), 1n, ['70']],
      [2n, id(7), id(8), 1n, [3n]],
    ]) expect(() => decodeBotUtilityCapabilities(data(value))).toThrow(BotUtilitiesError);
  });

  it('decodes poll summaries and correlates poll ID, channel, thread, and envelope revision', () => {
    expect(decodePollGet(data([1n, pollSummary()]), { pollId: hex(1), channelId: hex(2), threadId: null, responseRevision: '1' })).toEqual(expectedPoll);
    expect(() => decodePollGet(data([1n, pollSummary()]), { pollId: hex(9) })).toThrow(BotUtilitiesError);
    expect(() => decodePollGet(data([1n, pollSummary()]), { channelId: hex(9) })).toThrow(BotUtilitiesError);
    expect(() => decodePollGet(data([1n, pollSummary()]), { responseRevision: '2' })).toThrow(BotUtilitiesError);
    expect(() => decodePollResponse(data([1n, pollSummary()]), { myChoices: [0] })).toThrow(BotUtilitiesError);
    expect(() => decodePollResponse(data([1n, pollSummary()]), { closed: true })).toThrow(BotUtilitiesError);
    expect(decodePollVote(data([1n, pollSummary({ 11: [0n] })]), { pollId: hex(1), choices: [0], responseRevision: 1n }).myChoices).toEqual([0]);
    expect(() => decodePollVote(data([1n, pollSummary()]), { pollId: hex(1), choices: [0] })).toThrow(BotUtilitiesError);
    expect(() => decodePollClose(data([1n, pollSummary({ 9: 1n })]), { pollId: hex(1) })).not.toThrow();
  });

  it('accepts the Go poll-create empty-array choice shape and rejects null', () => {
    // Go pollSummary(nil) uses uintsToAny(nil), whose verified CBOR wire shape is 0x80 ([]).
    const goPollCreateFixture = data([1n, pollSummary({ 11: [] })]);
    expect(decodePollCreate(goPollCreateFixture, hex(1), { channelId: hex(2), responseRevision: '1' }).myChoices).toEqual([]);
    expect(() => decodePollCreate(data([1n, pollSummary({ 11: null })]), hex(1), { channelId: hex(2), responseRevision: '1' }))
      .toThrow(BotUtilitiesError);
  });

  it('rejects inconsistent poll summaries including count length, IDs, states, and revision', () => {
    for (const summary of [
      pollSummary({ 1: null }),
      pollSummary({ 6: 2n }),
      pollSummary({ 10: [1n] }),
      pollSummary({ 11: [2n] }),
      pollSummary({ 12: 0n }),
      pollSummary({ 0: id(0) }),
    ]) expect(() => decodePollGet(data([1n, summary]))).toThrow(BotUtilitiesError);
  });

  it('decodes poll pages with opaque cursor bound to community, channel and thread', () => {
    const cursor = encode([1n, id(9), id(2), id(5), new TextEncoder().encode(`dilla.bot.poll.v1/${hex(1)}`)]);
    const page = decodePollListPage(data([1n, [pollSummary({ 2: id(5) })], cursor]), { communityId: hex(9), channelId: hex(2), threadId: hex(5) });
    expect(page.items[0].threadId).toBe(hex(5));
    expect(page.nextCursor).toEqual(cursor);
    expect(() => decodePollListPage(data([1n, [], cursor]), { communityId: hex(9), channelId: hex(2), threadId: null })).toThrow(BotUtilitiesError);
  });

  it('decodes named voter pages and binds their cursor to poll and option', () => {
    const cursor = encode([1n, id(1), 1n, new TextEncoder().encode(`dilla.bot.poll-vote.v1/${hex(1)}/${hex(4)}`)]);
    expect(decodePollVotersPage(data([1n, [id(3), id(4)], cursor]), { pollId: hex(1), option: 1 })).toEqual({ voters: [hex(3), hex(4)], nextCursor: cursor });
    expect(() => decodePollVotersPage(data([1n, [id(3)], cursor]), { pollId: hex(2), option: 1 })).toThrow(BotUtilitiesError);
  });

  it('decodes reminder summaries including redacted sources and correlates requested IDs/revision', () => {
    expect(decodeReminderCreate(data([1n, reminderSummary()]), { reminderId: hex(4), responseRevision: '1' })).toMatchObject({
      id: hex(4), text: 'Call Åsa', dueUnixSeconds: 1_800_000_060n, timezone: 'Europe/Stockholm', sourceChannelId: hex(2), sourceThreadId: hex(5), state: 'scheduled', revision: 1n,
    });
    expect(decodeReminderCreate(data([1n, reminderSummary({ 4: null, 5: null })]), { reminderId: hex(4) })).toMatchObject({ sourceChannelId: null, sourceThreadId: null });
    expect(() => decodeReminderCreate(data([1n, reminderSummary()]), { reminderId: hex(9) })).toThrow(BotUtilitiesError);
    expect(() => decodeReminderCreate(data([1n, reminderSummary({ 6: 1n })]))).toThrow(BotUtilitiesError);
    expect(decodeReminderCancel(data([1n, reminderSummary({ 6: 2n, 7: 2n })]), hex(4), '2').state).toBe('cancelled');
    expect(decodeReminderDismiss(data([1n, reminderSummary({ 6: 3n, 7: 2n })]), hex(4), 2n).state).toBe('dismissed');
    expect(() => decodeReminderCancel(data([1n, reminderSummary({ 6: 3n })]), hex(4))).toThrow(BotUtilitiesError);
  });

  it('rejects invalid reminder summary ranges and a thread without its source channel', () => {
    for (const summary of [
      reminderSummary({ 1: '' }),
      reminderSummary({ 2: 0n }),
      reminderSummary({ 3: 'Local' }),
      reminderSummary({ 4: null }),
      reminderSummary({ 6: 4n }),
      reminderSummary({ 7: 0n }),
    ]) expect(() => decodeReminderCreate(data([1n, summary]))).toThrow(BotUtilitiesError);
  });

  it('requires bounded versions and correctly correlates page entries and cursor owner', () => {
    const cursor = new TextEncoder().encode(`${hex(6)}/00000000001800000060/${hex(4)}`);
    const page = decodeReminderListPage(data([1n, [reminderSummary()], cursor]), { ownerId: hex(6) });
    expect(page.items).toHaveLength(1);
    expect(page.nextCursor).toEqual(cursor);
    expect(() => decodeReminderListPage(data([1n, [reminderSummary()], cursor]), { ownerId: hex(7) })).toThrow(BotUtilitiesError);
    expect(() => decodeReminderListPage(data([1n, [reminderSummary()], new Uint8Array(257)]), { ownerId: hex(6) })).toThrow(BotUtilitiesError);
  });
});


const COMMUNITY = hex(9);
const INSTALLATION = hex(7);
const BOT_USER = hex(8);
const USER = hex(6);
const REQUEST_ID = hex(10);
const statusRow = (installationId = INSTALLATION, botUserId: string | null = BOT_USER) => ({ installationId, botUserId, state: 3, revision: '9' });
const capabilityData = (operations: number[] = [3, 70, 71, 72, 73, 74, 75, 80, 81, 82, 83, 84], installationId = INSTALLATION, botUserId = BOT_USER) =>
  data([1n, bytesFromHexForTest(installationId), bytesFromHexForTest(botUserId), 1n, operations.map(BigInt)]);
function bytesFromHexForTest(value: string): Uint8Array {
  return Uint8Array.from({ length: 16 }, (_, index) => Number.parseInt(value.slice(index * 2, index * 2 + 2), 16));
}
function fakeUtilityCore(options: {
  status?: () => unknown;
  response?: (operation: number, requestId: string, command: Record<string, unknown>) => { status?: number; revision?: string | null; data: Uint8Array };
  upload?: (command: Record<string, unknown>) => string;
  failReadAfterUpload?: number[];
  enforceRequestIdentity?: boolean;
}) {
  const calls: Record<string, unknown>[] = [];
  const identities = new Map<string, string>();
  const sentOperations = new Set<number>();
  const client = {
    get: (name: string) => name === 'account' ? { phase: 'ready', user: { id: USER, username: 'member' }, deviceId: hex(5) }
      : name === 'communities' ? [{ id: COMMUNITY }] : name === `permissions:${COMMUNITY}` ? { loaded: true } : undefined,
    subscribe: () => () => {},
    call: vi.fn((command: Record<string, unknown>) => {
      calls.push(command);
      if (command.m === 'getBotCommandStatus') return options.status?.() ?? statusRow();
      if (command.m === 'humanInteractionRequest') {
        sentOperations.add(Number(command.operation));
        if (options.enforceRequestIdentity) {
          const requestId = String(command.requestId);
          const fingerprint = JSON.stringify([command.operation, command.channelId, command.args, command.revision]);
          const prior = identities.get(requestId);
          if (prior !== undefined && prior !== fingerprint) throw new Error('request ID reused for a different operation body');
          identities.set(requestId, fingerprint);
        }
        return { requestId: command.requestId, upload: options.upload?.(command) ?? 'accepted' };
      }
      if (command.m === 'readBotInteractionResponses') {
        const operation = (command.operations as number[])[0];
        const requestId = (command.requestIds as string[])[0];
        if (options.failReadAfterUpload?.includes(operation) && sentOperations.has(operation)) {
          options.failReadAfterUpload = options.failReadAfterUpload.filter(value => value !== operation);
          throw new Error(`response read failed after upload for ${operation}`);
        }
        const result = options.response?.(operation, requestId, command) ?? { data: capabilityData(), revision: null };
        return { responses: [{ deliveryId: '1', requestId, operation, status: result.status ?? 0, revision: result.revision ?? null, data: result.data }], through: hex(4) };
      }
      if (command.m === 'ackBotInteractionResponses') return null;
      throw new Error(`Unexpected command ${String(command.m)}`);
    }),
  } as unknown as CoreClient;
  return { client, calls };
}

const pollSummaryForId = (idValue: string, channelId = hex(2), threadId: string | null = null, myChoices: bigint[] | null = null, revision = 1n, closed = 0n) => data([1n, [
  bytesFromHexForTest(idValue), bytesFromHexForTest(channelId), threadId === null ? null : bytesFromHexForTest(threadId), id(3),
  'Lunch?', ['Soup', 'Salad'], 0n, 1_800_000_000n, 1_800_003_600n, closed, [0n, 0n], myChoices, revision,
]]);
const reminderSummaryForId = (idValue: string) => data([1n, [
  bytesFromHexForTest(idValue), 'Call Åsa', 1_800_000_060n, 'Europe/Stockholm', null, null, 0n, 1n,
]]);

describe('createBotUtilities facade', () => {
  it('binds returned capabilities to live installation and bot identity', async () => {
    const f = fakeUtilityCore({ response: () => ({ data: capabilityData() }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    expect(await api.capabilities()).toEqual({
      version: 1, installationId: INSTALLATION, botUserId: BOT_USER,
      supportedOperations: [3, 70, 71, 72, 73, 74, 75, 80, 81, 82, 83, 84],
    });
    expect(api.binding).toEqual({ installationId: INSTALLATION, botUserId: BOT_USER });
  });

  it('rejects capabilities from a different current installation or Bot user', async () => {
    const f = fakeUtilityCore({ response: operation => ({ data: operation === 3 ? capabilityData([3], hex(11), BOT_USER) : capabilityData() }) });
    await expect(createBotUtilities(f.client, COMMUNITY).capabilities()).rejects.toBeInstanceOf(BotUtilitiesError);
    const wrongBot = fakeUtilityCore({ response: operation => ({ data: operation === 3 ? capabilityData([3], INSTALLATION, hex(11)) : capabilityData() }) });
    await expect(createBotUtilities(wrongBot.client, COMMUNITY).capabilities()).rejects.toBeInstanceOf(BotUtilitiesError);
  });

  it('clears a prior card binding when capability refresh no longer matches live status', async () => {
    let capabilityReads = 0;
    const f = fakeUtilityCore({ response: operation => ({ data: operation === 3
      ? (++capabilityReads === 1 ? capabilityData() : capabilityData([3], hex(11), BOT_USER)) : capabilityData() }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    await api.capabilities();
    expect(api.binding).toEqual({ installationId: INSTALLATION, botUserId: BOT_USER });
    await expect(api.capabilities()).rejects.toBeInstanceOf(BotUtilitiesError);
    expect(api.binding).toBeNull();
  });

  it('does not dispatch an operation when the live installation changes after its capability check', async () => {
    let statusReads = 0;
    const f = fakeUtilityCore({
      status: () => statusRow(++statusReads <= 4 ? INSTALLATION : hex(11)),
      response: operation => ({ data: operation === 3 ? capabilityData() : pollSummaryForId(hex(1)), revision: operation === 3 ? null : '1' }),
    });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.pollGet(hex(1), { channelId: hex(2) })).rejects.toMatchObject({ code: 'binding-mismatch' });
    expect(statusReads).toBe(5);
    expect(api.binding).toBeNull();
    expect(f.calls.some(call => call.m === 'humanInteractionRequest' && call.operation === 71)).toBe(false);
  });

  it('withholds a read result and clears its card binding when installation changes after dispatch', async () => {
    let statusReads = 0;
    const f = fakeUtilityCore({
      status: () => statusRow(++statusReads <= 5 ? INSTALLATION : hex(11)),
      response: operation => ({ data: operation === 3 ? capabilityData() : pollSummaryForId(hex(1), hex(2), null, []), revision: operation === 3 ? null : '1' }),
    });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.pollGet(hex(1), { channelId: hex(2) })).rejects.toMatchObject({ constructor: BotInteractionUncertainError });
    expect(statusReads).toBe(6);
    expect(api.binding).toBeNull();
  });

  it('creates a poll with the exact payload, stable request ID, revision zero, and correlated channel/thread', async () => {
    const thread = hex(12);
    const f = fakeUtilityCore({ response: (operation, requestId) => ({
      data: operation === 3 ? capabilityData() : pollSummaryForId(requestId, hex(2), thread, []), revision: operation === 3 ? null : '1',
    }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.pollCreate({ requestId: REQUEST_ID, channelId: hex(2), threadId: thread, question: 'Lunch?', answers: ['Soup', 'Salad'], multi: false, durationSeconds: 3600 }))
      .resolves.toMatchObject({ id: REQUEST_ID, channelId: hex(2), threadId: thread });
    const sent = f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 70)!;
    expect(sent).toMatchObject({ requestId: REQUEST_ID, revision: '0', channelId: null });
    expect((sent.args as unknown[])[0]).toEqual(encode([1n, id(2), id(12), 'Lunch?', ['Soup', 'Salad'], 0n, 3600n]));
  });

  it('does not dispatch an operation absent from current capabilities', async () => {
    const f = fakeUtilityCore({ response: operation => ({ data: capabilityData(operation === 3 ? [3, 70, 71, 72, 73, 75, 80, 81, 82, 83, 84] : []) }) });
    await expect(createBotUtilities(f.client, COMMUNITY).pollClose({ requestId: REQUEST_ID, pollId: hex(1), revision: '1', channelId: hex(2) }))
      .rejects.toBeInstanceOf(BotUtilitiesProviderUnavailableError);
    expect(f.calls.some(call => call.m === 'humanInteractionRequest' && call.operation === 74)).toBe(false);
  });

  it('maps provider unavailable status to a typed error and leaves conflict for caller refetch', async () => {
    const unavailable = fakeUtilityCore({ response: (operation) => ({ status: operation === 3 ? 0 : 5, revision: null, data: capabilityData() }) });
    await expect(createBotUtilities(unavailable.client, COMMUNITY).reminderList()).rejects.toBeInstanceOf(BotUtilitiesProviderUnavailableError);
    const conflict = fakeUtilityCore({ response: operation => ({ status: operation === 3 ? 0 : 4, revision: operation === 3 ? null : '2', data: operation === 3 ? capabilityData() : new Uint8Array() }) });
    await expect(createBotUtilities(conflict.client, COMMUNITY).pollVote({ requestId: REQUEST_ID, pollId: hex(1), channelId: hex(2), choices: [0], revision: '1' }))
      .rejects.toMatchObject({ constructor: BotUtilitiesStatusError, status: 4, operation: 73, revision: '2' });
  });

  it('treats status 5 after accepted poll create as uncertain and never resends its stable request ID', async () => {
    const f = fakeUtilityCore({ response: operation => ({
      status: operation === 3 ? 0 : 5, revision: null, data: operation === 3 ? capabilityData() : new Uint8Array(),
    }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    const input = { requestId: REQUEST_ID, channelId: hex(2), question: 'Lunch?', answers: ['Soup', 'Salad'], multi: false, durationSeconds: 3600 } as const;
    await expect(api.pollCreate(input)).rejects.toMatchObject({
      constructor: BotUtilitiesUncertainMutationError, uncertain: true, requestId: REQUEST_ID, operation: 70, status: 5,
    });
    await expect(api.pollCreate(input)).rejects.toMatchObject({ requestId: REQUEST_ID, operation: 70 });
    expect(f.calls.filter(call => call.m === 'humanInteractionRequest' && call.operation === 70)).toHaveLength(1);
  });

  it('passes uncertain request identity through and never resends after an uncertain upload', async () => {
    const f = fakeUtilityCore({ upload: command => command.operation === 73 ? 'pending' : 'accepted' });
    const api = createBotUtilities(f.client, COMMUNITY);
    for (let attempt = 0; attempt < 2; attempt++) {
      const error = await api.pollVote({ requestId: REQUEST_ID, pollId: hex(1), channelId: hex(2), choices: [0], revision: '1' }).catch((value: unknown): unknown => value);
      expect(error).toBeInstanceOf(BotUtilitiesUncertainMutationError);
      if (error instanceof BotUtilitiesUncertainMutationError) {
        expect(error.requestId).toBe(REQUEST_ID);
        expect(error.cause).toBeInstanceOf(BotInteractionUncertainError);
      }
    }
    expect(f.calls.filter(call => call.m === 'humanInteractionRequest' && call.operation === 73)).toHaveLength(1);
  });

  it('retains a utility mutation ID after a raw response-read error and reconciles through read()', async () => {
    const f = fakeUtilityCore({ failReadAfterUpload: [73], response: operation => ({
      data: operation === 3 ? capabilityData() : pollSummaryForId(hex(1), hex(2), null, [0n], 2n),
      revision: operation === 3 ? null : '2',
    }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.pollVote({ requestId: REQUEST_ID, pollId: hex(1), channelId: hex(2), choices: [0], revision: '1' }))
      .rejects.toMatchObject({ constructor: BotUtilitiesUncertainMutationError, requestId: REQUEST_ID, operation: 73 });
    expect(f.calls.filter(call => call.m === 'humanInteractionRequest' && call.operation === 73)).toHaveLength(1);
    await expect(api.read(REQUEST_ID, 73)).resolves.toMatchObject({ status: 0, requestId: REQUEST_ID });
    expect(f.calls.filter(call => call.m === 'humanInteractionRequest' && call.operation === 73)).toHaveLength(1);
  });

  it('reads and paginates polls with exact channel/thread operation shapes', async () => {
    const thread = hex(12);
    const pageData = data([1n, [[bytesFromHexForTest(hex(1)), id(2), id(12), id(3), 'Lunch?', ['Soup', 'Salad'], 0n,
      1_800_000_000n, 1_800_003_600n, 0n, [0n, 0n], [], 1n]], null]);
    const f = fakeUtilityCore({ response: (operation) => ({
      data: operation === 3 ? capabilityData() : operation === 71 ? pollSummaryForId(hex(1), hex(2), thread, []) : pageData,
      revision: operation === 3 || operation === 72 ? null : '1',
    }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.pollGet(hex(1), { channelId: hex(2), threadId: thread })).resolves.toMatchObject({ id: hex(1), threadId: thread });
    await expect(api.pollList({ channelId: hex(2), threadId: thread })).resolves.toMatchObject({ items: [{ id: hex(1), threadId: thread }], nextCursor: null });
    expect(f.calls.filter(call => call.m === 'humanInteractionRequest' && call.operation === 71)[0].args).toEqual([id(1)]);
    expect(f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 72)).toMatchObject({ args: [id(2), id(12), null] });
  });

  it('sends exact replacement vote and close mutation revisions and decodes their updated summaries', async () => {
    const f = fakeUtilityCore({ response: (operation) => ({
      data: operation === 3 ? capabilityData() : pollSummaryForId(hex(1), hex(2), null, operation === 73 ? [1n] : [], 2n, operation === 74 ? 1n : 0n),
      revision: operation === 3 ? null : '2',
    }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.pollVote({ requestId: REQUEST_ID, pollId: hex(1), channelId: hex(2), choices: [1], revision: '1' }))
      .resolves.toMatchObject({ id: hex(1), myChoices: [1], revision: 2n });
    await expect(api.pollClose({ requestId: hex(11), pollId: hex(1), channelId: hex(2), revision: '2' }))
      .resolves.toMatchObject({ id: hex(1), closed: true, revision: 2n });
    const vote = f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 73)!;
    expect(vote).toMatchObject({ requestId: REQUEST_ID, revision: '1', args: [id(1), encode([1n])] });
    expect(f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 74)).toMatchObject({ requestId: hex(11), revision: '2', args: [id(1)] });
  });

  it('checks backing poll context before returning named voters', async () => {
    const voterPage = data([1n, [id(4)], null]);
    const f = fakeUtilityCore({ enforceRequestIdentity: true, response: (operation) => ({
      data: operation === 3 ? capabilityData() : operation === 71 ? pollSummaryForId(hex(1), hex(2), null, []) : voterPage,
      revision: operation === 3 || operation === 75 ? null : '1',
    }) });
    await expect(createBotUtilities(f.client, COMMUNITY).pollVoters({ pollId: hex(1), channelId: hex(2), option: 1, requestId: REQUEST_ID }))
      .resolves.toEqual({ voters: [hex(4)], nextCursor: null });
    const backing = f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 71)!;
    const voters = f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 75)!;
    expect(backing.requestId).not.toBe(REQUEST_ID);
    expect(voters).toMatchObject({ requestId: REQUEST_ID, args: [id(1), 1, null] });
    const fingerprints = new Map<string, string>();
    for (const call of [backing, voters]) {
      const requestId = call.requestId as string;
      const fingerprint = JSON.stringify([call.operation, call.channelId, call.args, call.revision]);
      if (fingerprints.has(requestId)) expect(fingerprints.get(requestId)).toBe(fingerprint);
      fingerprints.set(requestId, fingerprint);
    }
  });

  it.each([
    ['poll create', 70], ['poll vote', 73], ['poll close', 74],
    ['reminder create', 80], ['reminder cancel', 82], ['reminder dismiss', 83],
  ])('reports accepted malformed %s result as uncertain with original identity', async (_label, operation) => {
    const f = fakeUtilityCore({ response: op => ({
      data: op === 3 ? capabilityData() : new Uint8Array([0xff]), revision: op === 3 ? null : '2',
    }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    const methods: Record<number, () => Promise<unknown>> = {
      70: () => api.pollCreate({ requestId: REQUEST_ID, channelId: hex(2), question: 'Q', answers: ['A', 'B'], multi: false, durationSeconds: 3600 }),
      73: () => api.pollVote({ requestId: REQUEST_ID, pollId: hex(1), channelId: hex(2), choices: [0], revision: '1' }),
      74: () => api.pollClose({ requestId: REQUEST_ID, pollId: hex(1), channelId: hex(2), revision: '1' }),
      80: () => api.reminderCreate({ requestId: REQUEST_ID, whenKind: 0, when: 60, text: 'Call', timezone: 'UTC' }),
      82: () => api.reminderCancel({ requestId: REQUEST_ID, reminderId: hex(1), revision: '1' }),
      83: () => api.reminderDismiss({ requestId: REQUEST_ID, reminderId: hex(1), revision: '1' }),
    };
    const error = await methods[operation]().catch((value: unknown): unknown => value);
    expect(error).toBeInstanceOf(BotUtilitiesUncertainMutationError);
    if (error instanceof BotUtilitiesUncertainMutationError) {
      expect(error).toMatchObject({ uncertain: true, requestId: REQUEST_ID, operation, status: 0 });
      expect(error.cause).toBeInstanceOf(Error);
    }
    expect(f.calls.filter(call => call.m === 'humanInteractionRequest' && call.operation === operation)).toHaveLength(1);
  });

  it('lists, cancels, dismisses and pages due reminders with account-scoped ownership', async () => {
    const makeRow = (state: bigint) => [id(4), 'Call Åsa', 1_800_000_060n, 'Europe/Stockholm', null, null, state, 2n];
    const f = fakeUtilityCore({ response: (operation) => {
      if (operation === 3) return { data: capabilityData() };
      if (operation === 81) return { data: data([1n, [makeRow(0n)], null]) };
      if (operation === 84) return { data: data([1n, [makeRow(1n)], null]) };
      return { data: data([1n, makeRow(operation === 82 ? 2n : 3n)]), revision: '2' };
    } });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.reminderList()).resolves.toMatchObject({ items: [{ id: hex(4), state: 'scheduled' }] });
    await expect(api.reminderDue()).resolves.toMatchObject({ items: [{ id: hex(4), state: 'due' }] });
    await expect(api.reminderCancel({ requestId: hex(13), reminderId: hex(4), revision: '1' })).resolves.toMatchObject({ state: 'cancelled', revision: 2n });
    await expect(api.reminderDismiss({ requestId: hex(14), reminderId: hex(4), revision: '1' })).resolves.toMatchObject({ state: 'dismissed', revision: 2n });
    expect(f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 81)).toMatchObject({ args: [null] });
    expect(f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 84)).toMatchObject({ args: [null] });
  });

  it('correlates reminder create IDs and preserves the summary binding', async () => {
    const f = fakeUtilityCore({ response: (operation, requestId) => ({
      data: operation === 3 ? capabilityData() : reminderSummaryForId(requestId), revision: operation === 3 ? null : '1',
    }) });
    const api = createBotUtilities(f.client, COMMUNITY);
    await expect(api.reminderCreate({ requestId: REQUEST_ID, whenKind: 0, when: 60, text: 'Call Åsa', timezone: 'Europe/Stockholm' }))
      .resolves.toMatchObject({ id: REQUEST_ID, state: 'scheduled' });
    const sent = f.calls.find(call => call.m === 'humanInteractionRequest' && call.operation === 80)!;
    expect(sent).toMatchObject({ requestId: REQUEST_ID, revision: '0' });
  });

  it('rejects a create response carrying another source context, while allowing deliberate redaction', async () => {
    const wrongSource = fakeUtilityCore({ response: (operation, requestId) => ({
      data: operation === 3 ? capabilityData() : data([1n, [bytesFromHexForTest(requestId), 'Call', 1_800_000_060n, 'UTC', id(13), null, 0n, 1n]]),
      revision: operation === 3 ? null : '1',
    }) });
    const rejected = await createBotUtilities(wrongSource.client, COMMUNITY).reminderCreate({
      requestId: REQUEST_ID, whenKind: 0, when: 60, text: 'Call', timezone: 'UTC', sourceChannelId: hex(2),
    }).catch((error: unknown): unknown => error);
    expect(rejected).toMatchObject({ code: 'uncertain-mutation', operation: 80, requestId: REQUEST_ID });

    const redacted = fakeUtilityCore({ response: (operation, requestId) => ({
      data: operation === 3 ? capabilityData() : reminderSummaryForId(requestId), revision: operation === 3 ? null : '1',
    }) });
    await expect(createBotUtilities(redacted.client, COMMUNITY).reminderCreate({
      requestId: REQUEST_ID, whenKind: 0, when: 60, text: 'Call Åsa', timezone: 'Europe/Stockholm', sourceChannelId: hex(2),
    })).resolves.toMatchObject({ sourceChannelId: null, sourceThreadId: null });
  });
});
