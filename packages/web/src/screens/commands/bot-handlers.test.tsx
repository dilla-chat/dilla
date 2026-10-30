import { createElement, useEffect } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { decode, encode, fromHex, PERM, parseCommand, type CoreClient, type CommandContext as Binding } from '@dilla/client-core';
import type { CommandExecutionContext } from './command-types.ts';
import { useBotCommandProvider } from './bot-handlers.ts';

const COMMUNITY = '11'.repeat(16); const TEXT = '22'.repeat(16); const VOICE = '33'.repeat(16); const OTHER_VOICE = '34'.repeat(16); const USER = '44'.repeat(16);
const INSTALLATION = '55'.repeat(16); const ITEM = '66'.repeat(16); const OTHER_ITEM = '67'.repeat(16); const CURRENT_ITEM = '65'.repeat(16); const PARENT = '77'.repeat(16);
type Wire = Record<string, unknown>;
function musicSnapshot(revision: number, paused: boolean, queue: { id: string; provider: string; canonical: string; title: string; artist: string }[] = [],
  state: { positionMs?: number; volume?: number; shuffle?: boolean; repeat?: number; control?: boolean; current?: { id: string; provider: string; canonical: string; title: string; artist: string } | null } = {}) {
  const current = state.current;
  return encode([1n, BigInt(revision), current === null ? null : [current?.id ?? CURRENT_ITEM, current?.provider ?? 'youtube', current?.canonical ?? 'mnopqrstuvw',
    current?.title ?? 'Current track', current?.artist ?? 'Listener', '', 120_000n, ''],
    queue.map(row => [row.id, row.provider, row.canonical, row.title, row.artist, '', 120_000n, '']),
    BigInt(state.positionMs ?? 0), paused ? 1n : 0n, BigInt(state.volume ?? 70), state.shuffle ? 1n : 0, BigInt(state.repeat ?? 0), state.control === false ? 0n : 1n]);
}
function fixture(options: { failRegistrationOnce?: boolean; failResume?: boolean; invalidRefreshAfterEnqueue?: boolean; seededQueue?: boolean; seedSchedule?: boolean;
  malformedAnnouncementReceipt?: boolean; ambiguousOperations?: number[]; unknownUploads?: number[]; disabledSchedule?: boolean; invalidMusicReceipt?: boolean;
  djControl?: boolean; readFailureOperations?: number[] } = {}) {
  let musicRevision = 1; let paused = true; let badSnapshotOnce = false; let positionMs = 0; let volume = 70; let shuffle = false; let repeat = 0; let installationId = INSTALLATION; let botUserId: string | null = null; let installRevision = '9'; let botState = 3;
  let currentTrack: { id: string; provider: string; canonical: string; title: string; artist: string } | null = { id: CURRENT_ITEM, provider: 'youtube', canonical: 'mnopqrstuvw', title: 'Current track', artist: 'Listener' };
  let queue: { id: string; provider: string; canonical: string; title: string; artist: string }[] = options.seededQueue
    ? [{ id: ITEM, provider: 'youtube', canonical: 'abcdefghijk', title: 'Queued track', artist: 'Listener' },
      { id: OTHER_ITEM, provider: 'youtube', canonical: 'zyxwvutsrqp', title: 'Second track', artist: 'Listener' }] : [];
  let joinedVoice = VOICE;
  let announcementRevision = 1; const schedules: unknown[][] = options.seedSchedule
    ? [[fromHex('99'.repeat(16)), fromHex(TEXT), 'Launch plan', 'one_off', 'Europe/Stockholm', '2099-01-01', 12n, 0n, 0n, options.disabledSchedule ? 0n : 1n, 1n]] : [];
  let failRegistrationOnce = options.failRegistrationOnce === true;
  const registeredPanels = new Map<string, Wire>();
  const requests: Wire[] = []; const responses: { deliveryId: string; requestId: string; communityId: string; operation: number; channelId: string | null; status: number; revision: string | null; data: Uint8Array }[] = [];
  const dispatchedOperations = new Set<number>();
  let delivery = 0;
  const client = {
    // eslint-disable-next-line @typescript-eslint/require-await -- models CoreClient.call while resolving each worker command.
    call: vi.fn(async (command: Wire): Promise<unknown> => {
      if (command.m === 'getBotCommandStatus') return { installationId, botUserId, state: botState, revision: installRevision };
      if (command.m === 'listSelfRoles') return [{ id: '88'.repeat(16), name: 'Reader' }];
      if (command.m === 'registerRolePanel') { requests.push(command); if (failRegistrationOnce) { failRegistrationOnce = false; throw new Error('temporary registration failure'); }
        registeredPanels.set(String(command.requestId), command); return null; }
      if (command.m === 'humanInteractionRequest') {
        requests.push(command);
        const operation = command.operation as number; const id = command.requestId as string;
        dispatchedOperations.add(operation);
        let status = 0; let revision: string | null = null; let data: Uint8Array = encode([]);
        if ([12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23].includes(operation)) {
          if (options.djControl === false) status = 2;
          else if (String(command.revision) !== String(musicRevision)) status = 4;
        }
        if (operation === 10) { revision = String(musicRevision); data = badSnapshotOnce ? Uint8Array.of(0xff) : musicSnapshot(musicRevision, paused, queue,
          { positionMs, volume, shuffle, repeat, current: currentTrack, control: options.djControl !== false }); badSnapshotOnce = false; }
        if (operation === 11) data = encode([1n, [['yt:abcdefghijk', 'youtube', 'abcdefghijk', 'First result', 'Listener', '', 120_000n, '']]]);
        if (operation === 12 && status === 0) { queue.push({ id: ITEM, provider: 'youtube', canonical: 'abcdefghijk', title: 'First result', artist: 'Listener' }); musicRevision++; revision = String(musicRevision); if (options.invalidRefreshAfterEnqueue) badSnapshotOnce = true; }
        if (operation === 13 && status === 0) { paused = true; musicRevision++; revision = String(musicRevision); }
        if (operation === 14 && options.failResume) status = 2;
        else if (operation === 14 && status === 0) { paused = false; musicRevision++; revision = String(musicRevision); }
        if (operation === 15 && status === 0) { currentTrack = queue.shift() ?? null; musicRevision++; revision = String(musicRevision); }
        if (operation === 16 && status === 0) { queue = []; currentTrack = null; paused = true; positionMs = 0; musicRevision++; revision = String(musicRevision); }
        if (operation === 17 && status === 0) { positionMs = Number((command.args as unknown[])[0]); musicRevision++; revision = String(musicRevision); }
        if (operation === 18 && status === 0) { volume = Number((command.args as unknown[])[0]); musicRevision++; revision = String(musicRevision); }
        if (operation === 19 && status === 0) { shuffle = !shuffle; musicRevision++; revision = String(musicRevision); }
        if (operation === 20 && status === 0) { repeat = Number((command.args as unknown[])[0]); musicRevision++; revision = String(musicRevision); }
        if (operation === 21 && status === 0) { const id = (command.args as unknown[])[0]; queue = queue.filter(row => row.id !== id); musicRevision++; revision = String(musicRevision); }
        if (operation === 22 && status === 0) {
          const [id, beforeId] = command.args as [string, string | null]; const row = queue.find(value => value.id === id);
          if (row !== undefined) { queue = queue.filter(value => value.id !== id); const index = beforeId === null ? queue.length : queue.findIndex(value => value.id === beforeId);
            queue.splice(index < 0 ? queue.length : index, 0, row); }
          musicRevision++; revision = String(musicRevision);
        }
        if (operation === 23 && status === 0) { queue = []; musicRevision++; revision = String(musicRevision); }
        if (operation === 50) { revision = String(announcementRevision); data = encode([1n, 0n, BigInt(schedules.length), schedules as never]); }
        if (operation === 51) {
          const value = decode((command.args as unknown[])[1] as Uint8Array) as unknown[];
          if ((command.args as unknown[])[0] === null) schedules.push(value.slice(1));
          else { const id = value[1] as Uint8Array; const index = schedules.findIndex(row => row[0] instanceof Uint8Array && Array.from(row[0]).join(',') === Array.from(id).join(',')); if (index >= 0) schedules[index] = value.slice(1); }
          announcementRevision++; revision = String(announcementRevision);
        }
        if (operation === 52) { const id = (command.args as unknown[])[0] as Uint8Array; const index = schedules.findIndex(row => row[0] instanceof Uint8Array && Array.from(row[0]).join(',') === Array.from(id).join(',')); if (index >= 0) schedules.splice(index, 1); announcementRevision++; revision = String(announcementRevision); }
        if (operation === 31) data = encode([fromHex(TEXT), 99n]);
        if (operation === 51 && options.malformedAnnouncementReceipt) data = Uint8Array.of(0xff);
        if (operation === 13 && options.invalidMusicReceipt) data = Uint8Array.of(0xff);
        if (options.ambiguousOperations?.includes(operation)) status = 5;
        responses.push({ deliveryId: String(++delivery), requestId: id, communityId: COMMUNITY, operation, channelId: command.channelId as string | null,
          status, revision, data });
        return { requestId: id, upload: options.unknownUploads?.includes(operation) ? 'unknown' : 'accepted' };
      }
      if (command.m === 'readBotInteractionResponses') {
        const operations = command.operations as number[]; const requestIds = command.requestIds as string[];
        const readFailures = options.readFailureOperations ?? [];
        const failedOperation = readFailures.find(operation => operations.includes(operation) && dispatchedOperations.has(operation));
        if (failedOperation !== undefined) { options.readFailureOperations = readFailures.filter(operation => operation !== failedOperation); throw new Error(`response read failed for ${failedOperation}`); }
        return { responses: responses.filter(row => operations.includes(row.operation) && requestIds.includes(row.requestId)), through: null, blocked: false };
      }
      if (command.m === 'ackBotInteractionResponses') return null;
      return null;
    }),
    get: (key: string) => key === 'account' ? { phase: 'ready', user: { id: USER }, deviceId: 'aa'.repeat(16), instance: { id: 'bb'.repeat(16) } }
      : key === 'communities' ? [{ id: COMMUNITY }] : key === `permissions:${COMMUNITY}` ? { loaded: true, manageCommunity: true, manageRoles: true }
      : key === `channels:${COMMUNITY}` ? [{ id: TEXT, communityId: COMMUNITY, kind: 0, name: 'announcements' }, { id: VOICE, communityId: COMMUNITY, kind: 1, name: 'voice' },
        { id: OTHER_VOICE, communityId: COMMUNITY, kind: 1, name: 'other voice' }]
          : key === 'call' ? { channelId: joinedVoice } : undefined,
    subscribe: () => () => undefined, onCallKeys: () => () => undefined, dispose: () => undefined,
  } as unknown as CoreClient;
  return { client, requests, schedules, registeredPanels, get musicState() { return { queue, currentTrack, paused, positionMs, volume, shuffle, repeat }; },
    setInstallation(value: string) { installationId = value; }, setBotUser(value: string) { botUserId = value; },
    setInstallRevision(value: string) { installRevision = value; }, setBotState(value: number) { botState = value; }, setJoinedVoice(value: string) { joinedVoice = value; },
    getJoinedVoice() { return joinedVoice; }, reorderQueue() { queue = [...queue].reverse(); musicRevision++; } };
}
function commandContext(client: CoreClient, over: Partial<CommandExecutionContext> = {}): CommandExecutionContext {
  const binding: Binding = { communityId: COMMUNITY, channelId: TEXT, bits: PERM.manageCommunity | PERM.manageRoles };
  return { requestId: PARENT, binding, getCurrentBinding: () => binding, isCurrent: () => true, client,
    getCall: () => ({ channelId: VOICE, session: {} } as never), call: {} as never, navigate: vi.fn(), openThread: vi.fn(), openSearch: vi.fn(),
    items: [], selfId: USER, encodeMessage: text => text, confirm: () => Promise.resolve(true), openInvite: () => Promise.resolve(true), openGif: vi.fn(), openModerationLog: vi.fn(), ...over };
}
function mountProvider(client: CoreClient, capture: { value?: ReturnType<typeof useBotCommandProvider> }) {
  function Probe() { const value = useBotCommandProvider(client, COMMUNITY); useEffect(() => { capture.value = value; }); return createElement('div', null, value.dialogs); }
  return render(createElement(Probe));
}

describe('Bot slash command provider', () => {
  it('plays the first worker search result, then resumes with distinct stable child request IDs', async () => {
    const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client);
    const result = await capture.value!.handlers.play!({ commandId: 'play', args: { 'query-or-url': 'harbor song' } }, context);
    expect(result).toMatchObject({ status: 'success', private: true });
    const writes = f.requests.filter(row => row.m === 'humanInteractionRequest' && [11, 12, 14].includes(row.operation as number));
    expect(writes.map(row => row.operation)).toEqual([11, 12, 14]);
    expect(writes[1]?.args).toEqual(['https://www.youtube.com/watch?v=abcdefghijk']);
    expect(writes.map(row => row.requestId)).toHaveLength(new Set(writes.map(row => row.requestId)).size);
    expect(writes.every(row => row.requestId !== context.requestId)).toBe(true);
    expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 12)).toHaveLength(1);
  });

  it('refuses music controls without a current joined call and sends no Bot interaction', async () => {
    const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const result = await capture.value!.handlers.pause!({ commandId: 'pause', args: {} }, commandContext(f.client, { getCall: () => ({ channelId: VOICE, session: null } as never) }));
    expect(result).toMatchObject({ status: 'failure', private: true, message: 'Join a voice channel first. Glenn plays where you are.' });
    expect(f.requests.filter(row => row.m === 'humanInteractionRequest')).toHaveLength(0);
  });

  it('allows an authenticated DJ control without Manage Community while still requiring the live same call', async () => {
    const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); context.getCurrentBinding = () => ({ ...context.binding, bits: 0 });
    expect(await capture.value!.handlers.pause!({ commandId: 'pause', args: {} }, context)).toMatchObject({ status: 'success' });
    expect(f.requests.some(row => row.operation === 13)).toBe(true);
    const changed = commandContext(f.client, { getCall: () => ({ channelId: TEXT, session: {} } as never) });
    expect(await capture.value!.handlers.pause!({ commandId: 'pause', args: {} }, changed)).toMatchObject({ status: 'failure' });
  });

  it('refuses a caller absent from the authenticated DJ snapshot', async () => {
    const f = fixture({ djControl: false }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    expect(await capture.value!.handlers.pause!({ commandId: 'pause', args: {} }, commandContext(f.client))).toMatchObject({ status: 'failure' });
    expect(f.requests.filter(row => row.operation === 13)).toHaveLength(0);
    expect(f.musicState.paused).toBe(true);
  });

  it.each([
    ['pause', {}, 13, []], ['resume', {}, 14, []], ['skip', {}, 15, []], ['stop', {}, 16, []],
    ['seek', { position: '1:02' }, 17, [62_000]], ['volume', { level: '42' }, 18, [42]], ['shuffle', { mode: 'on' }, 19, []],
    ['repeat', { mode: 'all' }, 20, [2]], ['queue remove', { item: ITEM }, 21, [ITEM]], ['queue move', { item: ITEM, position: '2' }, 22, [ITEM, null]],
    ['queue clear', {}, 23, []],
  ] as const)('runs %s with its exact worker operation and arguments', async (commandId, args, operation, wireArgs) => {
    const f = fixture({ seededQueue: commandId === 'skip' || commandId.startsWith('queue ') }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const provider = capture.value;
    if (provider === undefined) throw new Error('Bot provider did not mount.');
    const handler = provider.handlers[commandId];
    if (handler === undefined) throw new Error(`Missing handler for ${commandId}.`);
    const result = await handler({ commandId, args }, commandContext(f.client));
    expect(result).toMatchObject({ status: 'success', private: true });
    const mutation = f.requests.find(row => row.m === 'humanInteractionRequest' && row.operation === operation);
    expect(mutation?.args).toEqual(wireArgs);
    expect(mutation?.revision).toBe('1');
    if (commandId === 'pause') expect(f.musicState.paused).toBe(true);
    if (commandId === 'resume') expect(f.musicState.paused).toBe(false);
    if (commandId === 'skip') { expect(f.musicState.currentTrack?.id).toBe(ITEM); expect(f.musicState.queue.map(row => row.id)).toEqual([OTHER_ITEM]); }
    if (commandId === 'queue remove') expect(f.musicState.queue.map(row => row.id)).toEqual([OTHER_ITEM]);
    if (commandId === 'stop' || commandId === 'queue clear') expect(f.musicState.queue).toEqual([]);
    if (commandId === 'seek') expect(f.musicState.positionMs).toBe(62_000);
    if (commandId === 'volume') expect(f.musicState.volume).toBe(42);
    if (commandId === 'shuffle') expect(f.musicState.shuffle).toBe(true);
    if (commandId === 'repeat') expect(f.musicState.repeat).toBe(2);
    if (commandId === 'queue move') expect(f.musicState.queue.map(row => row.id)).toEqual([OTHER_ITEM, ITEM]);
  });

  it('does not enqueue a second time when accepted enqueue refresh is uncertain, and reconciles using the same request id', async () => {
    const f = fixture({ invalidRefreshAfterEnqueue: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invocation = { commandId: 'play', args: { 'query-or-url': 'https://youtube.com/watch?v=abcdefghijk' } };
    expect(await capture.value!.handlers.play!(invocation, context)).toMatchObject({ status: 'uncertain', private: true });
    expect(await capture.value!.handlers.play!(invocation, context)).toMatchObject({ status: 'success', private: true });
    const enqueueRequests = f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 12);
    expect(enqueueRequests).toHaveLength(1);
    expect(enqueueRequests[0]?.args).toEqual(['https://youtube.com/watch?v=abcdefghijk']);
  });

  it('keeps status 5 uncertain and reconciles the same enqueue instead of uploading a second effect', async () => {
    const f = fixture({ ambiguousOperations: [12] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invocation = { commandId: 'play', args: { 'query-or-url': 'https://youtube.com/watch?v=abcdefghijk' } };
    expect(await capture.value!.handlers.play!(invocation, context)).toMatchObject({ status: 'uncertain' });
    expect(await capture.value!.handlers.play!(invocation, context)).toMatchObject({ status: 'uncertain' });
    expect(f.requests.filter(row => row.operation === 12)).toHaveLength(1);
    expect(f.musicState.queue).toHaveLength(1);
  });

  it('preserves an uncertain enqueue across DJ and call loss, then reconciles with the same parent ID', async () => {
    const options = { unknownUploads: [12], djControl: true }; const f = fixture(options); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client);
    const invoke = { commandId: 'play', args: { 'query-or-url': 'https://youtube.com/watch?v=abcdefghijk' } };
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain' });
    options.djControl = false;
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain' });
    options.djControl = true;
    context.getCall = () => ({ channelId: VOICE, session: null } as never);
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain' });
    context.getCall = () => ({ channelId: VOICE, session: {} } as never);
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 12)).toHaveLength(1);
    expect(f.musicState.queue).toHaveLength(1);
  });

  it('preserves an uncertain enqueue across Bot provider downtime and recovers on the same parent ID', async () => {
    const f = fixture({ unknownUploads: [12] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'play', args: { 'query-or-url': 'https://youtube.com/watch?v=abcdefghijk' } };
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain' });
    f.setBotState(4);
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain' });
    f.setBotState(3);
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 12)).toHaveLength(1);
    expect(f.musicState.queue).toHaveLength(1);
  });

  it('keeps an unknown enqueue bound to its original voice channel while the user joins another channel', async () => {
    const f = fixture({ unknownUploads: [12] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client, { getCall: () => ({ channelId: f.getJoinedVoice(), session: {} } as never) });
    const invoke = { commandId: 'play', args: { 'query-or-url': 'https://youtube.com/watch?v=abcdefghijk' } };
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain', private: true });
    f.setJoinedVoice(OTHER_VOICE);
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain', private: true });
    expect(f.requests.filter(row => row.operation === 12)).toHaveLength(1);
    f.setJoinedVoice(VOICE);
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'success', private: true });
    expect(f.requests.filter(row => row.operation === 12)).toHaveLength(1);
    expect(f.musicState.queue).toHaveLength(1);
  });

  it('keeps the enqueue uncertain when the worker response read fails after upload acceptance', async () => {
    const f = fixture({ readFailureOperations: [12] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'play', args: { 'query-or-url': 'https://youtube.com/watch?v=abcdefghijk' } };
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'uncertain' });
    expect(f.musicState.queue).toHaveLength(1);
    expect(await capture.value!.handlers.play!(invoke, context)).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 12)).toHaveLength(1);
    expect(f.musicState.queue).toHaveLength(1);
  });

  it('keeps an accepted music operation uncertain when its receipt is malformed', async () => {
    const f = fixture({ invalidMusicReceipt: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'pause', args: {} };
    expect(await capture.value!.handlers.pause!(invoke, context)).toMatchObject({ status: 'uncertain' });
    expect(await capture.value!.handlers.pause!(invoke, context)).toMatchObject({ status: 'uncertain' });
    expect(f.requests.filter(row => row.operation === 13)).toHaveLength(1);
    expect(f.musicState.paused).toBe(true);
  });

  it('reconciles a durable result after a capability revision change and refreshes current state', async () => {
    const f = fixture({ unknownUploads: [13] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'pause', args: {} };
    expect(await capture.value!.handlers.pause!(invoke, context)).toMatchObject({ status: 'uncertain' });
    f.setInstallRevision('10');
    expect(await capture.value!.handlers.pause!(invoke, context)).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 13)).toHaveLength(1);
    expect(f.requests.filter(row => row.operation === 10).length).toBeGreaterThan(1);
    expect(f.musicState.paused).toBe(true);
    expect(await capture.value!.handlers.queue!({ commandId: 'queue', args: {} }, commandContext(f.client, { requestId: 'ac'.repeat(16) })))
      .toMatchObject({ status: 'success' });
  });

  it('reuses the original immutable queue move body when the queue changes before reconciliation', async () => {
    const f = fixture({ seededQueue: true, unknownUploads: [22] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invocation = { commandId: 'queue move', args: { item: ITEM, position: '2' } };
    expect(await capture.value!.handlers['queue move']!(invocation, context)).toMatchObject({ status: 'uncertain' });
    const first = f.requests.find(row => row.operation === 22)!;
    f.reorderQueue();
    expect(await capture.value!.handlers['queue move']!(invocation, context)).toMatchObject({ status: 'success' });
    const writes = f.requests.filter(row => row.operation === 22);
    expect(writes).toHaveLength(1);
    expect(writes[0]?.args).toEqual(first.args);
    expect(f.musicState.queue.map(row => row.id)).toEqual([ITEM, OTHER_ITEM]);
  });

  it('reports accepted enqueue as partial success when resume is refused and never repeats enqueue', async () => {
    const f = fixture({ failResume: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invocation = { commandId: 'play', args: { 'query-or-url': 'https://youtube.com/watch?v=abcdefghijk' } };
    const result = await capture.value!.handlers.play!(invocation, context);
    expect(result).toMatchObject({ status: 'success', private: true });
    expect(result.message).toMatch(/already in the queue/u);
    await capture.value!.handlers.play!(invocation, context);
    expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 12)).toHaveLength(1);
    expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 14)).toHaveLength(1);
  });

  it('lists announcements from an authenticated response and keeps output private', async () => {
    const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const result = await capture.value!.handlers['announcement list']!({ commandId: 'announcement list', args: {} }, commandContext(f.client));
    expect(result).toMatchObject({ status: 'success', private: true, message: 'No scheduled announcements.' });
    const reads = f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 50);
    expect(reads.length).toBeGreaterThan(0);
    expect(new Set(reads.map(row => row.requestId)).size).toBe(reads.length);
  });

  it('debounces play autocomplete 250 ms, skips short queries, and returns the selected worker result input', async () => {
    vi.useFakeTimers();
    try {
      const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
      mountProvider(f.client, capture); await vi.waitFor(() => expect(capture.value).toBeDefined());
      const provider = capture.value!; const binding: Binding = { communityId: COMMUNITY, channelId: TEXT };
      const option = { name: 'query-or-url', kind: 'text' as const, description: 'query', required: true, freeText: true };
      await expect(provider.suggest('play', option, 'a', binding)).resolves.toEqual([]);
      expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 11)).toHaveLength(0);
      const suggestions = provider.suggest('play', option, 'harbor', binding);
      await vi.advanceTimersByTimeAsync(249);
      expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 11)).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(1);
      await expect(suggestions).resolves.toEqual([{ id: 'yt:abcdefghijk', value: 'https://www.youtube.com/watch?v=abcdefghijk', label: 'First result', description: 'Listener' }]);
      expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 11)).toHaveLength(1);
    } finally { vi.useRealTimers(); }
  });

  it('resolves an announcement suggestion by saved stable ID as well as its title', async () => {
    const f = fixture({ seedSchedule: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const provider = capture.value!; const binding: Binding = { communityId: COMMUNITY, channelId: TEXT };
    const option = { name: 'announcement', kind: 'announcement' as const, description: 'announcement', required: true };
    await expect(provider.suggest('announcement edit', option, '99'.repeat(16), binding)).resolves.toEqual([
      expect.objectContaining({ id: '99'.repeat(16), value: '99'.repeat(16), label: 'Launch plan' }),
    ]);
  });

  it('edits a selected announcement with the current global revision and preserves its recurrence', async () => {
    const f = fixture({ seedSchedule: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const pending = capture.value!.handlers['announcement edit']!({ commandId: 'announcement edit', args: { announcement: '99'.repeat(16), text: 'Revised plan' } }, commandContext(f.client));
    await screen.findByRole('dialog', { name: 'Edit scheduled announcement' });
    expect(screen.getByLabelText('Repeat')).toHaveValue('one_off');
    expect(screen.getByLabelText('Date')).toHaveValue('2099-01-01');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await pending).toMatchObject({ status: 'success', private: true, message: 'Announcement updated.' });
    const save = f.requests.find(row => row.m === 'humanInteractionRequest' && row.operation === 51)!;
    expect(save.revision).toBe('1');
    expect((save.args as unknown[])[0]).toEqual(fromHex('99'.repeat(16)));
    const value = decode((save.args as unknown[])[1] as Uint8Array) as unknown[];
    expect(value.slice(3, 6)).toEqual(['Revised plan', 'one_off', 'Europe/Stockholm']);
    expect(f.schedules[0]?.[2]).toBe('Revised plan');
    expect(f.schedules[0]?.[3]).toBe('one_off');
  });

  it('keeps a disabled recurring announcement disabled when editing only its text', async () => {
    const f = fixture({ seedSchedule: true, disabledSchedule: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const pending = capture.value!.handlers['announcement edit']!({ commandId: 'announcement edit', args: { announcement: '99'.repeat(16), text: 'Edit only' } }, commandContext(f.client));
    await screen.findByRole('dialog', { name: 'Edit scheduled announcement' }); await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await pending).toMatchObject({ status: 'success' });
    const payload = decode((f.requests.find(row => row.operation === 51)!.args as unknown[])[1] as Uint8Array) as unknown[];
    expect(payload[10]).toBe(0n);
    expect(f.schedules[0]?.[9]).toBe(0n);
  });

  it('confirms announcement deletion and binds the delete to schedule and document revisions', async () => {
    const f = fixture({ seedSchedule: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client, { confirm: () => Promise.resolve(true) });
    const result = await capture.value!.handlers['announcement delete']!({ commandId: 'announcement delete', args: { announcement: '99'.repeat(16) } }, context);
    expect(result).toMatchObject({ status: 'success', private: true, message: 'Scheduled announcement deleted.' });
    const deletion = f.requests.find(row => row.m === 'humanInteractionRequest' && row.operation === 52)!;
    expect(deletion).toMatchObject({ revision: '1', args: [fromHex('99'.repeat(16)), 1] });
    expect(f.schedules).toHaveLength(0);
    const listed = await capture.value!.handlers['announcement list']!({ commandId: 'announcement list', args: {} },
      commandContext(f.client, { requestId: 'ad'.repeat(16) }));
    expect(listed).toMatchObject({ status: 'success', message: 'No scheduled announcements.' });
  });

  it('opens a focused role panel editor and uses separate publication and registration IDs', async () => {
    const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const pending = capture.value!.handlers['rolepanel create']!({ commandId: 'rolepanel create', args: {} }, commandContext(f.client));
    const checkbox = await screen.findByRole('checkbox', { name: 'Reader' }); await user.click(checkbox);
    await user.type(screen.getByLabelText('Reaction for Reader'), '📚');
    await user.click(screen.getByRole('button', { name: 'Publish panel' }));
    const result = await pending;
    expect(result).toMatchObject({ status: 'success', private: true });
    const publication = f.requests.find(row => row.m === 'humanInteractionRequest' && row.operation === 31)!;
    const registration = f.requests.find(row => row.m === 'registerRolePanel')!;
    expect(publication.requestId).not.toBe(registration.requestId);
    expect(registration).toMatchObject({ communityId: COMMUNITY, channelId: TEXT, messageSeq: '99', options: [{ emoji: '📚', roleId: '88'.repeat(16) }] });
    expect(f.registeredPanels.get(String(registration.requestId))).toEqual(registration);
  });

  it('recovers a published role panel by retrying registration without publishing another message', async () => {
    const f = fixture({ failRegistrationOnce: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client);
    const first = capture.value!.handlers['rolepanel create']!({ commandId: 'rolepanel create', args: {} }, context);
    await user.click(await screen.findByRole('checkbox', { name: 'Reader' }));
    await user.type(screen.getByLabelText('Reaction for Reader'), '📚');
    await user.click(screen.getByRole('button', { name: 'Publish panel' }));
    expect(await first).toMatchObject({ status: 'uncertain', private: true });
    context.getCurrentBinding = () => ({ ...context.binding, bits: 0 });
    expect(await capture.value!.handlers['rolepanel create']!({ commandId: 'rolepanel create', args: {} }, context)).toMatchObject({ status: 'uncertain' });
    context.getCurrentBinding = () => context.binding;
    const retry = await capture.value!.handlers['rolepanel create']!({ commandId: 'rolepanel create', args: {} }, context);
    expect(retry).toMatchObject({ status: 'success', private: true });
    expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 31)).toHaveLength(1);
    const registrations = f.requests.filter(row => row.m === 'registerRolePanel');
    expect(registrations).toHaveLength(2);
    expect(registrations[0]).toEqual(registrations[1]);
    expect(f.registeredPanels.size).toBe(1);
  });

  it('retains a published role panel attempt when its accepted receipt is malformed', async () => {
    const f = fixture(); const call = f.client.call.bind(f.client);
    f.client.call = (async (command: Wire) => {
      const value = await call(command as never);
      if (command.m === 'readBotInteractionResponses') return { ...(value as object), responses: (value as { responses: Wire[] }).responses
        .map(row => row.operation === 31 ? { ...row, data: Uint8Array.of(0xff) } : row) };
      return value;
    });
    const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {}; const user = userEvent.setup();
    mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined()); const context = commandContext(f.client);
    const pending = capture.value!.handlers['rolepanel create']!({ commandId: 'rolepanel create', args: {} }, context);
    await user.click(await screen.findByRole('checkbox', { name: 'Reader' }));
    await user.type(screen.getByLabelText('Reaction for Reader'), '📚'); await user.click(screen.getByRole('button', { name: 'Publish panel' }));
    expect(await pending).toMatchObject({ status: 'uncertain' });
    expect(await capture.value!.handlers['rolepanel create']!({ commandId: 'rolepanel create', args: {} }, context)).toMatchObject({ status: 'uncertain' });
    expect(f.requests.filter(row => row.operation === 31)).toHaveLength(1);
    expect(f.requests.filter(row => row.m === 'registerRolePanel')).toHaveLength(0);
  });

  it('sends a schedule only after the announcement editor submits valid data', async () => {
    const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const pending = capture.value!.handlers['announcement create']!({ commandId: 'announcement create', args: { text: 'Meet at noon' } }, commandContext(f.client));
    await screen.findByRole('dialog', { name: 'Schedule an announcement' });
    expect(f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 51)).toHaveLength(0);
    const destination = screen.getByLabelText('Destination channel'); fireEvent.change(destination, { target: { value: TEXT } });
    await user.click(screen.getByRole('button', { name: 'Save' }));
    const result = await pending;
    expect(result).toMatchObject({ status: 'success', private: true, message: 'Announcement scheduled.' });
    const save = f.requests.find(row => row.m === 'humanInteractionRequest' && row.operation === 51)!;
    expect(save.args).toHaveLength(2); expect(save.revision).toBe('1');
    expect((decode((save.args as unknown[])[1] as Uint8Array) as unknown[])[3]).toBe('Meet at noon');
    const listed = await capture.value!.handlers['announcement list']!({ commandId: 'announcement list', args: {} },
      commandContext(f.client, { requestId: 'ae'.repeat(16) }));
    expect(listed).toMatchObject({ status: 'success' });
    expect(listed.message).toContain('Meet at noon');
  });

  it('keeps the original announcement request and body when an accepted save has a malformed receipt', async () => {
    const f = fixture({ malformedAnnouncementReceipt: true }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invocation = { commandId: 'announcement create', args: { text: 'Original schedule body' } };
    const pending = capture.value!.handlers['announcement create']!(invocation, context);
    await screen.findByRole('dialog', { name: 'Schedule an announcement' });
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await pending).toMatchObject({ status: 'uncertain', private: true });
    const first = f.requests.find(row => row.m === 'humanInteractionRequest' && row.operation === 51)!;
    const originalBody = (first.args as unknown[])[1] as Uint8Array;
    expect(await capture.value!.handlers['announcement create']!(invocation, context)).toMatchObject({ status: 'uncertain', private: true });
    const saves = f.requests.filter(row => row.m === 'humanInteractionRequest' && row.operation === 51);
    expect(saves).toHaveLength(1);
    expect(saves.map(row => (row.args as unknown[])[1])).toEqual([originalBody]);
    expect(f.schedules).toHaveLength(1);
  });

  it('keeps status 5 announcement saves uncertain and does not upload a second schedule', async () => {
    const f = fixture({ ambiguousOperations: [51] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'announcement create', args: { text: 'Status five' } };
    const pending = capture.value!.handlers['announcement create']!(invoke, context);
    await screen.findByRole('dialog', { name: 'Schedule an announcement' }); await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await pending).toMatchObject({ status: 'uncertain' });
    expect(await capture.value!.handlers['announcement create']!(invoke, context)).toMatchObject({ status: 'uncertain' });
    expect(f.requests.filter(row => row.operation === 51)).toHaveLength(1);
    expect(f.schedules).toHaveLength(1);
  });

  it('keeps an accepted announcement attempt uncertain across Manage Community loss and retries the same ID', async () => {
    const f = fixture({ readFailureOperations: [51] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'announcement create', args: { text: 'Stable schedule' } };
    const pending = capture.value!.handlers['announcement create']!(invoke, context);
    await screen.findByRole('dialog', { name: 'Schedule an announcement' }); await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await pending).toMatchObject({ status: 'uncertain' });
    context.getCurrentBinding = () => ({ ...context.binding, bits: 0 });
    expect(await capture.value!.handlers['announcement create']!(invoke, context)).toMatchObject({ status: 'uncertain' });
    context.getCurrentBinding = () => context.binding;
    expect(await capture.value!.handlers['announcement create']!(invoke, context)).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 51)).toHaveLength(1);
    expect(f.schedules).toHaveLength(1);
  });

  it('reconciles announcement response-read failure without publishing another schedule', async () => {
    const f = fixture({ readFailureOperations: [51] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'announcement create', args: { text: 'Read recovery' } };
    const pending = capture.value!.handlers['announcement create']!(invoke, context);
    await screen.findByRole('dialog', { name: 'Schedule an announcement' }); await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await pending).toMatchObject({ status: 'uncertain' });
    expect(await capture.value!.handlers['announcement create']!(invoke, context)).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 51)).toHaveLength(1);
    expect(f.schedules).toHaveLength(1);
  });

  it('keeps a pending announcement uncertain when the Bot becomes unavailable before scope preflight', async () => {
    const f = fixture({ readFailureOperations: [51] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'announcement create', args: { text: 'Scope recovery' } };
    const pending = capture.value!.handlers['announcement create']!(invoke, context);
    await screen.findByRole('dialog', { name: 'Schedule an announcement' }); await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await pending).toMatchObject({ status: 'uncertain', private: true });
    f.setBotState(4);
    expect(await capture.value!.handlers['announcement create']!(invoke, context)).toMatchObject({ status: 'uncertain', private: true });
    expect(f.requests.filter(row => row.operation === 51)).toHaveLength(1);
    f.setBotState(3);
    const recovered = await capture.value!.handlers['announcement create']!(invoke, context);
    expect(recovered).toMatchObject({ status: 'success', private: true });
    expect(f.requests.filter(row => row.operation === 51)).toHaveLength(1);
    expect(f.schedules).toHaveLength(1);
  });

  it('reconciles role panel response-read failure without publishing twice', async () => {
    const f = fixture({ readFailureOperations: [31] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
    const user = userEvent.setup(); mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined());
    const context = commandContext(f.client); const invoke = { commandId: 'rolepanel create', args: {} };
    const pending = capture.value!.handlers['rolepanel create']!(invoke, context);
    await user.click(await screen.findByRole('checkbox', { name: 'Reader' })); await user.type(screen.getByLabelText('Reaction for Reader'), '📚');
    await user.click(screen.getByRole('button', { name: 'Publish panel' }));
    expect(await pending).toMatchObject({ status: 'uncertain' });
    expect(await capture.value!.handlers['rolepanel create']!(invoke, context)).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 31)).toHaveLength(1);
    expect(f.requests.filter(row => row.m === 'registerRolePanel')).toHaveLength(1);
  });
});


describe('historical receipt identity across changed capability revisions', () => {
  it.each(['music', 'announcement', 'rolepanel'] as const)('settles %s after a same-bot revision change, refusing identity replacements', async kind => {
    for (const change of ['revision', 'installation', 'bot'] as const) {
      const operation = kind === 'music' ? 13 : kind === 'announcement' ? 51 : 31;
      const f = fixture({ readFailureOperations: [operation] }); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {};
      const view = mountProvider(f.client, capture); await waitFor(() => expect(capture.value).toBeDefined()); const user = userEvent.setup();
      const context = commandContext(f.client); const commandId = kind === 'music' ? 'pause' : kind === 'announcement' ? 'announcement create' : 'rolepanel create';
      const invoke: { commandId: string; args: Record<string, string> } = { commandId, args: kind === 'announcement' ? { text: 'Historical receipt' } : {} };
      const handler = capture.value!.handlers[commandId]!;
      const first = handler(invoke, context);
      if (kind === 'announcement') { await screen.findByRole('dialog', { name: 'Schedule an announcement' }); await user.click(screen.getByRole('button', { name: 'Save' })); }
      if (kind === 'rolepanel') { await user.click(await screen.findByRole('checkbox', { name: 'Reader' })); await user.type(screen.getByLabelText('Reaction for Reader'), '📚'); await user.click(screen.getByRole('button', { name: 'Publish panel' })); }
      expect(await first).toMatchObject({ status: 'uncertain' });
      f.setInstallRevision('10');
      if (change === 'installation') f.setInstallation('ba'.repeat(16));
      if (change === 'bot') f.setBotUser('bc'.repeat(16));
      expect(await handler(invoke, context)).toMatchObject({ status: change === 'revision' ? 'success' : 'uncertain' });
      expect(f.requests.filter(row => row.operation === operation)).toHaveLength(1);
      if (kind === 'rolepanel') expect(f.requests.filter(row => row.m === 'registerRolePanel')).toHaveLength(change === 'revision' ? 1 : 0);
      view.unmount();
    }
  });
  it.each([['90', 90_000], ['1:02', 62_000], ['0:01:02', 62_000]] as const)('runs registry-parsed /seek %s with the shared grammar', async (position, milliseconds) => {
    const parsed = parseCommand(`/seek ${position}`); expect(parsed.type).toBe('invocation'); if (parsed.type !== 'invocation') throw new Error('Invalid seek parse');
    const f = fixture(); const capture: { value?: ReturnType<typeof useBotCommandProvider> } = {}; const view = mountProvider(f.client, capture);
    await waitFor(() => expect(capture.value).toBeDefined());
    expect(await capture.value!.handlers.seek!(parsed.invocation, commandContext(f.client))).toMatchObject({ status: 'success' });
    expect(f.requests.find(row => row.operation === 17)?.args).toEqual([milliseconds]); view.unmount();
  });
});
