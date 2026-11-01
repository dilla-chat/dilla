import { describe, expect, it, vi } from 'vitest';
import { decode, encode } from '../cbor';
import type { CoreClient } from '../bridge';
import { createVoiceMusicAdapter, musicPlayheadMs, VoiceMusicAcceptedUnconfirmedError, voiceMusicAdapter, type VoiceMusicState } from './music.ts';

const COMMUNITY = 'ab'.repeat(16);
const CHANNEL = 'cd'.repeat(16);
const ITEM = Uint8Array.from({ length: 16 }, (_, i) => i + 1);

function snapshot(revision: bigint, paused: boolean, canControl = true) {
  return encode([1n, revision, ['ef'.repeat(16), 'youtube', 'abcdefghijk', 'Track', 'Artist', '', 120_000n, ITEM], [], 0n,
    paused ? 1n : 0n, 75n, 0n, 0n, canControl ? 1n : 0n]);
}

function response(request: Record<string, unknown>, deliveryId: string, status: number, revision: string | null, data: Uint8Array) {
  return { deliveryId, requestId: request.requestId as string, communityId: COMMUNITY, operation: request.operation as number,
    channelId: CHANNEL, status, revision, data, sourceSeq: deliveryId };
}

function fixture() {
  const commands: Record<string, unknown>[] = [];
  const events: ReturnType<typeof response>[] = [];
  let id = 0;
  const client = {
    call: vi.fn((command: Record<string, unknown>) => {
      commands.push(command);
      if (command.m === 'getBotCommandStatus') return Promise.resolve({ installationId: 'ab'.repeat(16), botUserId: null, state: 3, revision: '7' });
      if (command.m === 'humanInteractionRequest') return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
      if (command.m === 'readBotInteractionResponses') {
        const operations = command.operations as number[]; const requestIds = command.requestIds as string[];
        const matching = events.filter(row => operations.includes(row.operation) && requestIds.includes(row.requestId));
        for (let i = events.length - 1; i >= 0; i--) if (matching.includes(events[i])) events.splice(i, 1);
        return Promise.resolve({ responses: matching, through: null, blocked: false });
      }
      return Promise.resolve(null);
    }),
    get: (key: string) => key === 'account' ? { phase: 'ready', user: { id: 'cd'.repeat(16) }, deviceId: 'ef'.repeat(16), instance: { id: '12'.repeat(16) } }
      : key === 'communities' ? [{ id: COMMUNITY }] : key === `permissions:${COMMUNITY}` ? { viewChannel: true } : undefined,
    subscribe: () => () => undefined,
    onCallKeys: () => () => undefined,
    dispose: () => undefined,
  } as unknown as CoreClient;
  return {
    client, commands,
    adapter: createVoiceMusicAdapter(client, COMMUNITY),
    getRequest(operation: number) { return [...commands].reverse().find(command => command.m === 'humanInteractionRequest' && command.operation === operation)!; },
    answer(request: Record<string, unknown>, status: number, revision: string | null, data: Uint8Array) {
      events.push(response(request, String(++id), status, revision, data));
    },
  };
}

describe('authenticated voice music adapter', () => {
  it('retries an unavailable subscribed channel so a late Bot Welcome can recover', async () => {
    vi.useFakeTimers();
    try {
      const f = fixture();
      f.client.call = vi.fn((command: Record<string, unknown>) => {
        f.commands.push(command);
        if (command.m === 'getBotCommandStatus') return Promise.resolve({ installationId: 'ab'.repeat(16), botUserId: null, state: 3, revision: '7' });
        if (command.m === 'humanInteractionRequest') return Promise.reject(new Error('private group not ready'));
        return Promise.resolve({ responses: [], through: null, blocked: false });
      });
      const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
      await vi.advanceTimersByTimeAsync(5_500);
      expect(f.commands.filter(command => command.m === 'humanInteractionRequest' && command.operation === 10).length).toBeGreaterThanOrEqual(2);
      unsubscribe();
    } finally {
      vi.useRealTimers();
    }
  });
  it('keeps the unavailable fallback until it receives an authenticated state snapshot', async () => {
    expect(voiceMusicAdapter.state(CHANNEL)).toEqual({ status: 'unavailable', reason: 'authenticated-transport-missing' });
    await expect(voiceMusicAdapter.pause(CHANNEL, true)).rejects.toThrow(/transport is unavailable/);
  });

  it('decodes a Bot state response and acks only after applying it', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));
    const state = f.adapter.state(CHANNEL);
    expect(state).toMatchObject({ status: 'available', track: { id: 'ef'.repeat(16), title: 'Track', artist: 'Artist' },
      queue: [], positionMs: 0, volume: 75, paused: false, shuffle: false, repeat: 'off',
      permissions: { control: true, manageQueue: true } });
    unsubscribe();
  });

  it('exposes the provider and the requester the Bot recorded for a queued track', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    const ada = '0a'.repeat(16);
    f.answer(f.getRequest(10), 0, '1', encode([1n, 1n, ['ef'.repeat(16), 'youtube', 'abcdefghijk', 'Track', 'Artist', '', 120_000n, ada],
      [['12'.repeat(16), 'youtube', 'bcdefghijkl', 'Next', 'Other', '', 60_000n, 'search']], 30_000n, 0n, 75n, 0n, 0n, 1n]));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));
    const state = f.adapter.state(CHANNEL);
    if (state.status !== 'available') throw new Error('unavailable');
    expect(state.track).toMatchObject({ provider: 'youtube', requesterId: ada, durationMs: 120_000 });
    expect(state.queue[0]?.track.requesterId).toBeUndefined();
    expect(state.positionMs).toBe(30_000);
    unsubscribe();
  });

  it('Ruling AI: runs the playhead on from the Bot stamp and its own receipt time, never from its clock against the Bot\'s', async () => {
    vi.useFakeTimers({ now: 1_000_000 });
    try {
      const f = fixture();
      const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
      await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
      // The Bot's clock runs 5 minutes ahead of this device; the track has played 2 s since its 30 s stamp.
      f.answer(f.getRequest(10), 0, '1', encode([1n, 1n, ['ef'.repeat(16), 'youtube', 'abcdefghijk', 'Track', 'Artist', '', 120_000n, ''], [],
        30_000n, 0n, 75n, 0n, 0n, 1n, 1_298_000n, 1_300_000n]));
      await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));
      const state = f.adapter.state(CHANNEL) as Extract<VoiceMusicState, { status: 'available' }>;
      expect(state.clock?.elapsedMs).toBe(2_000);
      const arrived = state.clock!.receivedAtMs;
      expect(musicPlayheadMs(state, arrived)).toBe(32_000);
      expect(musicPlayheadMs(state, arrived + 5_000)).toBe(37_000);
      expect(musicPlayheadMs(state, arrived + 500_000)).toBe(120_000);
      expect(musicPlayheadMs({ ...state, paused: true }, arrived + 5_000)).toBe(30_000);
      unsubscribe();
    } finally { vi.useRealTimers(); }
  });

  it('fix round 2: decodes 11 and 14 slot snapshots and longer track rows, ignoring what it does not know', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    const track = ['ef'.repeat(16), 'youtube', 'abcdefghijk', 'Track', 'Artist', '', 120_000n, '', 'future'];
    f.answer(f.getRequest(10), 0, '1', encode([1n, 1n, track, [], 1_000n, 0n, 75n, 0n, 0n, 1n, 500n]));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));
    const eleven = f.adapter.state(CHANNEL) as Extract<VoiceMusicState, { status: 'available' }>;
    expect(eleven.track?.title).toBe('Track');
    expect(eleven.clock).toBeUndefined();
    unsubscribe();
    const g = fixture();
    const off = g.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(g.getRequest(10)).toBeDefined());
    g.answer(g.getRequest(10), 0, '1', encode([1n, 1n, track, [], 1_000n, 0n, 75n, 0n, 0n, 1n, 9_000n, 10_000n, 0n, 'later']));
    await vi.waitFor(() => expect(g.adapter.state(CHANNEL).status).toBe('available'));
    expect((g.adapter.state(CHANNEL) as Extract<VoiceMusicState, { status: 'available' }>).clock?.elapsedMs).toBe(1_000);
    off();
  });

  it('an older Bot without the stamp keeps the static position', () => {
    const state: Extract<VoiceMusicState, { status: 'available' }> = { status: 'available', track: { id: 'a', title: 'T', artist: 'A', durationMs: 60_000 },
      queue: [], positionMs: 12_000, volume: 50, paused: false, shuffle: false, repeat: 'off', permissions: { control: true, manageQueue: true } };
    expect(musicPlayheadMs(state, Date.now() + 9_000)).toBe(12_000);
  });

  it('announces through operation 25 on the voice channel, naming the text channel at revision 0', async () => {
    const f = fixture();
    const TEXT = '9a'.repeat(16);
    const done = f.adapter.announce(CHANNEL, TEXT);
    await vi.waitFor(() => expect(f.getRequest(25)).toBeDefined());
    const request = f.getRequest(25);
    expect(request).toMatchObject({ channelId: CHANNEL, args: [TEXT], revision: '0' });
    f.answer(request, 0, null, encode([]));
    await expect(done).resolves.toBeUndefined();
    await expect(f.adapter.announce(CHANNEL, 'nope')).rejects.toThrow(/announcement channel/);
  });

  it('searches YouTube through authenticated operation 11 and returns stable result IDs', async () => {
    const f = fixture();
    const search = f.adapter.search(CHANNEL, 'harbor north');
    await vi.waitFor(() => expect(f.getRequest(11)).toBeDefined());
    const request = f.getRequest(11);
    expect(request.args).toEqual(['harbor north']);
    expect(request.revision).toBeNull();
    f.answer(request, 0, null, encode([1n, [['yt:abcdefghijk', 'youtube', 'abcdefghijk', 'North', 'Harbor', '', 120_000n, '']]]));

    await expect(search).resolves.toEqual([{
      id: 'yt:abcdefghijk', title: 'North', artist: 'Harbor', durationMs: 120_000,
      input: 'https://www.youtube.com/watch?v=abcdefghijk',
    }]);
  });

  it('enqueues one YouTube input with the current revision and waits for confirmed state', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));

    const enqueue = f.adapter.enqueue(CHANNEL, 'https://www.youtube.com/watch?v=abcdefghijk');
    await vi.waitFor(() => expect(f.getRequest(12)).toBeDefined());
    expect(f.getRequest(12).args).toEqual(['https://www.youtube.com/watch?v=abcdefghijk']);
    expect(f.getRequest(12).revision).toBe('1');
    f.answer(f.getRequest(12), 0, '2', encode([]));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10)).toHaveLength(2));
    const reads = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10);
    f.answer(reads[1], 0, '2', snapshot(2n, false));
    await expect(enqueue).resolves.toBeUndefined();
    unsubscribe();
  });

  it('uses the command attempt ID for one enqueue mutation and clears the queue with operation 23', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));

    const attemptId = '12'.repeat(16);
    const enqueue = f.adapter.enqueue(CHANNEL, 'https://www.youtube.com/watch?v=abcdefghijk', { requestId: attemptId });
    await vi.waitFor(() => expect(f.getRequest(12)).toBeDefined());
    expect(f.getRequest(12).requestId).toBe(attemptId);
    f.answer(f.getRequest(12), 0, '2', encode([]));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10)).toHaveLength(2));
    let reads = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10);
    f.answer(reads[1], 0, '2', snapshot(2n, false));
    await enqueue;

    const clear = f.adapter.clearQueue(CHANNEL, { requestId: '34'.repeat(16) });
    await vi.waitFor(() => expect(f.getRequest(23)).toBeDefined());
    expect(f.getRequest(23)).toMatchObject({ requestId: '34'.repeat(16), args: [], revision: '2' });
    f.answer(f.getRequest(23), 0, '3', encode([]));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10)).toHaveLength(3));
    reads = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10);
    f.answer(reads[2], 0, '3', snapshot(3n, false));
    await clear;
    unsubscribe();
  });

  it('passes Spotify links and URIs unchanged through operation 12', async () => {
    for (const input of [
      'https://open.spotify.com/track/1234567890123456789012',
      'https://open.spotify.com/album/1234567890123456789012',
      'https://open.spotify.com/playlist/1234567890123456789012',
      'spotify:track:1234567890123456789012',
      'spotify:album:1234567890123456789012',
      'spotify:playlist:1234567890123456789012',
    ]) {
      const f = fixture();
      const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
      await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
      f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
      await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));

      const enqueue = f.adapter.enqueue(CHANNEL, input);
      await vi.waitFor(() => expect(f.getRequest(12)).toBeDefined());
      expect(f.getRequest(12).args).toEqual([input]);
      f.answer(f.getRequest(12), 0, '2', encode([]));
      await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10)).toHaveLength(2));
      const reads = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10);
      f.answer(reads[1], 0, '2', snapshot(2n, false));
      await expect(enqueue).resolves.toBeUndefined();
      unsubscribe();
    }
  });

  it('rejects status 5 without a revision as definite unavailable', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));
    const enqueue = f.adapter.enqueue(CHANNEL, 'https://youtu.be/abcdefghijk', { requestId: '12'.repeat(16) });
    await vi.waitFor(() => expect(f.getRequest(12)).toBeDefined());
    const rejected = expect(enqueue).rejects.toMatchObject({ code: 'unavailable' });
    f.answer(f.getRequest(12), 5, null, encode([]));
    await rejected;
    unsubscribe();
  });


  it('uses neutral unavailable copy for pause without a current call', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));
    const pause = f.adapter.pause(CHANNEL, true);
    await vi.waitFor(() => expect(f.getRequest(13)).toBeDefined());
    const rejected = expect(pause).rejects.toMatchObject({ code: 'operation-unavailable', message: "The Bot could not do that right now. Try again." });
    f.answer(f.getRequest(13), 5, null, encode([]));
    await rejected;
    unsubscribe();
  });

  it.each([null, '2'])('classifies durable status 5 with revision %s when restoring an attempt', async revision => {
    const f = fixture();
    const original = f.client.call.bind(f.client);
    f.client.call = vi.fn((command: Record<string, unknown>) => {
      if (command.m === 'readBotInteractionResponses') return Promise.resolve({ responses: [{ requestId: '56'.repeat(16), operation: 12, status: 5, revision, data: encode([]) }], through: null });
      return original(command as never);
    });
    const enqueue = f.adapter.enqueue(CHANNEL, 'https://youtu.be/abcdefghijk', { requestId: '56'.repeat(16) });
    await expect(enqueue).rejects.toMatchObject(revision === null ? { code: 'unavailable' } : { requestId: '56'.repeat(16) });
    expect(f.commands.filter(command => command.operation === 12)).toHaveLength(0);
  });

  it('keeps status 5 with a revision uncertain because the mutation may have persisted', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));

    const enqueue = f.adapter.enqueue(CHANNEL, 'spotify:track:1234567890123456789012');
    await vi.waitFor(() => expect(f.getRequest(12)).toBeDefined());
    f.answer(f.getRequest(12), 5, '1', encode([]));
    await expect(enqueue).rejects.toBeInstanceOf(VoiceMusicAcceptedUnconfirmedError);
    unsubscribe();
  });

  it('explains when Spotify playback is already active in another voice channel', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));

    const enqueue = f.adapter.enqueue(CHANNEL, 'spotify:track:1234567890123456789012');
    await vi.waitFor(() => expect(f.getRequest(12)).toBeDefined());
    f.answer(f.getRequest(12), 6, '1', encode([]));
    await expect(enqueue).rejects.toThrow(/Spotify.*(active|busy)|another voice channel/i);
    unsubscribe();
  });

  it('does not resolve a mutation on upload receipt and waits for the operation and resulting snapshot', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: false }));

    let settled = false;
    const command = Promise.resolve(f.adapter.pause(CHANNEL, true)).then(() => { settled = true; });
    await vi.waitFor(() => expect(f.getRequest(13)).toBeDefined());
    expect(f.getRequest(13)?.revision).toBe('0');
    await new Promise(resolve => setTimeout(resolve, 20));
    expect(settled).toBe(false);

    f.answer(f.getRequest(13), 0, '2', encode([]));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10)).toHaveLength(2));
    const reads = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10);
    f.answer(reads[1], 0, '2', snapshot(2n, true));
    await command;
    expect(settled).toBe(true);
    expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: true });
    unsubscribe();
  });

  it('surfaces a Bot rejection without changing the confirmed snapshot', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));
    const command = Promise.resolve(f.adapter.pause(CHANNEL, true));
    await vi.waitFor(() => expect(f.getRequest(13)).toBeDefined());
    f.answer(f.getRequest(13), 2, '1', encode([]));
    await expect(command).rejects.toThrow(/permission/);
    expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: false });
    unsubscribe();
  });

  it('keeps controls read-only when the authenticated snapshot denies the current actor DJ access', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available',
      permissions: { control: false, manageQueue: false } }));
    unsubscribe();
  });

  it('refreshes the revision after a conflict before rejecting, so the next control uses current state', async () => {
    const f = fixture();
    const unsubscribe = f.adapter.subscribe(CHANNEL, () => undefined);
    await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
    f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
    await vi.waitFor(() => expect(f.adapter.state(CHANNEL).status).toBe('available'));

    const conflict = Promise.resolve(f.adapter.setVolume(CHANNEL, 75));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 18)).toHaveLength(1));
    const firstMutation = f.commands.find(call => call.m === 'humanInteractionRequest' && call.operation === 18)!;
    expect(firstMutation.revision).toBe('1');
    f.answer(firstMutation, 4, '2', encode([]));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10)).toHaveLength(2));
    const reads = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10);
    f.answer(reads[1], 0, '2', snapshot(2n, false));
    await expect(conflict).rejects.toThrow(/state changed/);
    expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: false });

    const retry = Promise.resolve(f.adapter.setVolume(CHANNEL, 75));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 18)).toHaveLength(2));
    const secondMutation = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 18)[1];
    expect(secondMutation.revision).toBe('2');
    f.answer(secondMutation, 0, '3', encode([]));
    await vi.waitFor(() => expect(f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10)).toHaveLength(3));
    const updatedReads = f.commands.filter(call => call.m === 'humanInteractionRequest' && call.operation === 10);
    f.answer(updatedReads[2], 0, '3', snapshot(3n, true));
    await retry;
    expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: true });
    unsubscribe();
  });
});

it.each([[13, true], [15, false]] as const)('sends immediate control %s without a state read and accepts its authenticated snapshot', async (operation, paused) => {
  const f = fixture();
  const command = operation === 13 ? f.adapter.pause(CHANNEL, true) : f.adapter.skip(CHANNEL);
  await vi.waitFor(() => expect(f.getRequest(operation)).toBeDefined());
  expect(f.commands.filter(row => row.m === 'humanInteractionRequest').map(row => row.operation)).toEqual([operation]);
  expect(f.getRequest(operation).revision).toBe('0');
  f.answer(f.getRequest(operation), 0, '3', snapshot(3n, paused));
  await command;
  expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused });
  expect(f.commands.filter(row => row.m === 'humanInteractionRequest').map(row => row.operation)).toEqual([operation]);
});

it('keeps a newer control snapshot when an older control response arrives late', async () => {
  const f = fixture();
  const skip = f.adapter.skip(CHANNEL);
  const pause = f.adapter.pause(CHANNEL, true);
  await vi.waitFor(() => { expect(f.getRequest(15)).toBeDefined(); expect(f.getRequest(13)).toBeDefined(); });
  f.answer(f.getRequest(13), 0, '3', snapshot(3n, true));
  await pause;
  f.answer(f.getRequest(15), 0, '2', snapshot(2n, false));
  await skip;
  expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: true });
});

it('keeps a newer control snapshot when an older state read arrives late', async () => {
  const f = fixture();
  const refresh = f.adapter.refresh(CHANNEL);
  await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
  const pause = f.adapter.pause(CHANNEL, true);
  await vi.waitFor(() => expect(f.getRequest(13)).toBeDefined());
  f.answer(f.getRequest(13), 0, '3', snapshot(3n, true));
  await pause;
  f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
  await refresh;
  expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: true });
});

it('exposes the total when only a queue prefix fits the authenticated snapshot', async () => {
  const f = fixture();
  const refresh = f.adapter.refresh(CHANNEL);
  await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
  const row = decode(snapshot(1n, true));
  if (!Array.isArray(row)) throw new Error('invalid fixture');
  f.answer(f.getRequest(10), 0, '1', encode([...row, 0n, 0n, 25n]));
  await refresh;
  expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', queue: [], queueTotal: 25 });
});


it('binds two skips to the same visible item without fetching another state', async () => {
  const f = fixture();
  const refresh = f.adapter.refresh(CHANNEL);
  await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
  f.answer(f.getRequest(10), 0, '1', snapshot(1n, false));
  await refresh;
  const first = f.adapter.skip(CHANNEL);
  const second = f.adapter.skip(CHANNEL);
  await vi.waitFor(() => expect(f.commands.filter(row => row.m === 'humanInteractionRequest' && row.operation === 15)).toHaveLength(2));
  const skips = f.commands.filter(row => row.m === 'humanInteractionRequest' && row.operation === 15);
  expect(skips.map(row => row.args)).toEqual([['ef'.repeat(16)], ['ef'.repeat(16)]]);
  for (const row of skips) f.answer(row, 0, '2', snapshot(2n, false));
  await Promise.all([first, second]);
  expect(f.commands.filter(row => row.m === 'humanInteractionRequest' && row.operation === 10)).toHaveLength(1);
});

it('guards a skip of an empty visible state instead of advancing a newly started track', async () => {
  const f = fixture();
  const refresh = f.adapter.refresh(CHANNEL);
  await vi.waitFor(() => expect(f.getRequest(10)).toBeDefined());
  const value = decode(snapshot(1n, true)) as unknown[]; value[2] = null;
  f.answer(f.getRequest(10), 0, '1', encode(value as never)); await refresh;
  const skip = f.adapter.skip(CHANNEL);
  await vi.waitFor(() => expect(f.getRequest(15)).toBeDefined());
  expect(f.getRequest(15).args).toEqual(['']);
  f.answer(f.getRequest(15), 0, '1', encode(value as never)); await skip;
});

it.each([13, 15])('reconciles the saved receipt for immediate operation %s when a reload resend conflicts with its original body', async operation => {
  const f = fixture(); const requestId = '89'.repeat(16);
  f.answer({ requestId, operation }, 0, '3', snapshot(3n, operation === 13));
  const call = f.client.call.bind(f.client);
  const retryCall = vi.fn((command: Parameters<CoreClient['call']>[0]) => command.m === 'humanInteractionRequest'
    ? Promise.reject(new Error('interaction request id conflicts with a prior request')) : call(command));
  f.client.call = retryCall;
  await (operation === 13 ? f.adapter.pause(CHANNEL, true, { requestId }) : f.adapter.skip(CHANNEL, { requestId }));
  expect(retryCall.mock.calls.filter(([row]) => row.m === 'humanInteractionRequest')).toHaveLength(1);
  expect(f.commands.filter(row => row.m === 'humanInteractionRequest')).toEqual([]);
  expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: operation === 13 });
});

it.each(['missing', 'failed'] as const)('keeps an immediate retry uncertain when its prior receipt is %s', async outcome => {
  const f = fixture(); const requestId = '90'.repeat(16); const call = f.client.call.bind(f.client);
  f.client.call = vi.fn((command: Parameters<CoreClient['call']>[0]) => {
    if (command.m === 'humanInteractionRequest') return Promise.reject(new Error('interaction request id conflicts with a prior request'));
    if (command.m === 'readBotInteractionResponses' && outcome === 'failed') return Promise.reject(new Error('receipt read failed'));
    return call(command);
  });
  await expect(f.adapter.skip(CHANNEL, { requestId })).rejects.toMatchObject({ requestId });
  expect(f.commands.some(row => row.m === 'humanInteractionRequest' && row.operation === 10)).toBe(false);
});

it.each(['upload-unresolved', 'response-read-failed'] as const)('keeps the first immediate %s uncertain until the person reconciles the same attempt', async failure => {
  const f = fixture(); const requestId = '91'.repeat(16); const call = f.client.call.bind(f.client);
  let uploads = 0; let reads = 0;
  f.client.call = vi.fn((command: Parameters<CoreClient['call']>[0]) => {
    if (command.m === 'humanInteractionRequest') {
      uploads++;
      f.answer(command, 0, '3', snapshot(3n, true));
      if (failure === 'upload-unresolved') return Promise.resolve({ requestId, upload: 'unknown' });
    }
    if (command.m === 'readBotInteractionResponses') {
      reads++;
      if (failure === 'response-read-failed' && reads === 1) return Promise.reject(new Error('response read failed'));
    }
    return call(command);
  });
  await expect(f.adapter.pause(CHANNEL, true, { requestId })).rejects.toMatchObject({ requestId });
  expect(reads).toBe(failure === 'response-read-failed' ? 1 : 0);
  await expect(f.adapter.pause(CHANNEL, true, { requestId })).resolves.toBeUndefined();
  expect(uploads).toBe(1);
  expect(f.adapter.state(CHANNEL)).toMatchObject({ status: 'available', paused: true });
  expect(f.commands.some(row => row.m === 'humanInteractionRequest' && row.operation === 10)).toBe(false);
});
