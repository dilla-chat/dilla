import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { decode, encode } from '../cbor';
import { fromHex, toHex } from '../hex';
import {
  BOT_FIRST_ANSWER_MS, BOT_REPLY_HOLD_MS, BotCommandRelay, createBotCommands, decodeBotAnswer, encodeBotAnswer,
  type BotAnswerEvent, type BotCommandPort, type BotCommandSubmit, type BotRelayDeps, type BotRunItem,
} from './commands';

const fid = (byte: number): Uint8Array => new Uint8Array(16).fill(byte);
const COMMUNITY = toHex(fid(0x0e));
const REQUEST = toHex(fid(0x10));
const PUBLICATION = new Uint8Array([7, 7, 7]);

async function collect(items: AsyncIterable<BotRunItem>): Promise<BotRunItem[]> {
  const out: BotRunItem[] = [];
  for await (const item of items) out.push(item);
  return out;
}

const text = (s: string): Uint8Array => new TextEncoder().encode(s);

/** The page side's port, faked: it records each submit and lets the test push answers as the worker would. */
function fakePort() {
  const submits: BotCommandSubmit[] = [];
  const listeners = new Map<string, Set<(a: BotAnswerEvent) => void>>();
  let refetches = 0;
  let next = 0x20;
  const submit = vi.fn((req: BotCommandSubmit) => { submits.push(req); return port.submitError === null ? Promise.resolve() : Promise.reject(port.submitError); });
  const refetchCatalog = vi.fn(() => { refetches++; return Promise.resolve(); });
  const forgotten: string[] = [];
  const port: BotCommandPort & { submits: BotCommandSubmit[]; answer(a: Partial<BotAnswerEvent> & { seq: number }): void; refetches(): number;
    submitError: Error | null; submitMock: typeof submit; refetchMock: typeof refetchCatalog; forgotten: string[]; listening(id: string): number;
    catalogReady?: (communityId: string) => boolean } = {
    submits,
    submitError: null,
    forgotten,
    forget: (requestId) => { forgotten.push(requestId); },
    listening: (id) => listeners.get(id)?.size ?? 0,
    submit,
    submitMock: submit,
    refetchMock: refetchCatalog,
    answers: (requestId, fn) => {
      const set = listeners.get(requestId) ?? new Set();
      set.add(fn); listeners.set(requestId, set);
      return () => { set.delete(fn); };
    },
    refetchCatalog,
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (h) => { clearTimeout(h as ReturnType<typeof setTimeout>); },
    newRequestId: () => toHex(fid(next++)),
    answer: (a) => {
      const event: BotAnswerEvent = { requestId: a.requestId ?? submits.at(-1)!.requestId, seq: a.seq, status: a.status ?? 200,
        body: a.body ?? text('ok'), final: a.final ?? false };
      for (const fn of [...(listeners.get(event.requestId) ?? [])]) fn(event);
    },
    refetches: () => refetches,
  };
  return port;
}

describe('sendBotCommand (the page side)', () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });

  it('Enter sends one sealed request and shows the first answer', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [60, 'tea'] });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    expect(port.submits).toEqual([{ communityId: COMMUNITY, channelId: null, operation: 80, args: [60, 'tea'], requestId: run.requestId }]);
    port.answer({ seq: 0, body: text('Saved'), final: true });
    expect(await all).toEqual([{ status: 200, body: text('Saved'), final: true }]);
  });

  it('two streamed answers render in order', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 12, args: ['song'] });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, body: text('Searching…') });
    port.answer({ seq: 0, body: text('Searching…') });      // a replayed duplicate is dropped
    port.answer({ seq: 1, body: text('Now playing: song'), final: true });
    expect((await all).map(a => 'body' in a ? new TextDecoder().decode(a.body) : a.kind)).toEqual(['Searching…', 'Now playing: song']);
  });

  it('no answer in 5 s ends with a timeout; Retry reuses the request id', async () => {
    const port = fakePort();
    const commands = createBotCommands(port);
    const run = commands.sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(BOT_FIRST_ANSWER_MS - 1);
    let settled = false;
    void all.then(() => { settled = true; });
    await vi.advanceTimersByTimeAsync(0);
    expect(settled).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    expect(await all).toEqual([{ kind: 'timeout' }]);
    const again = run.retry();
    expect(again.requestId).toBe(REQUEST);
    const second = collect(again.answers);
    await vi.advanceTimersByTimeAsync(0);
    expect(port.submits.map(s => s.requestId)).toEqual([REQUEST, REQUEST]);
    port.answer({ seq: 0, final: true });
    expect(await second).toHaveLength(1);
  });

  it('a stale key refetches once and resends with the same id', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, status: 410, body: text('E_BOT_KEY_STALE'), final: true });
    await vi.advanceTimersByTimeAsync(0);
    expect(port.refetches()).toBe(1);
    expect(port.submits.map(s => s.requestId)).toEqual([REQUEST, REQUEST]);
    // The resend's answer is the run's answer; the stale refusal never shows.
    port.answer({ seq: 0, body: text('Saved'), final: true });
    expect(await all).toEqual([{ status: 200, body: text('Saved'), final: true }]);
  });

  it('a second stale answer ends with the refusal, and never loops', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, status: 410, final: true });
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, status: 410, final: true });
    expect(await all).toEqual([{ kind: 'refused', code: 'E_BOT_KEY_STALE', status: 410 }]);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(port.refetches()).toBe(1);
    expect(port.submits).toHaveLength(2);
  });

  // Task 21b: dillad refuses a command sealed to a key the Bot no longer opens (410 E_BOT_KEY_STALE on the POST itself,
  // since such a seal could never be answered). The run reads the catalog once and resends the same request id.
  it('a send dillad refuses as sealed to a stale key refetches once and resends with the same id', async () => {
    const port = fakePort();
    port.submitError = Object.assign(new Error('E_BOT_KEY_STALE'), { code: 'E_BOT_KEY_STALE', status: 410 });
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    expect(port.refetches()).toBe(1);
    expect(port.submits.map(s => s.requestId)).toEqual([REQUEST, REQUEST]);
    // A second stale refusal ends the run; it never loops.
    await vi.advanceTimersByTimeAsync(0);
    expect(await all).toEqual([{ kind: 'refused', code: 'E_BOT_KEY_STALE', status: 410 }]);
    expect(port.submits).toHaveLength(2);
    // After one refetch the resend gets through and is answered.
    const ok = fakePort();
    ok.submitError = Object.assign(new Error('E_BOT_KEY_STALE'), { code: 'E_BOT_KEY_STALE', status: 410 });
    ok.refetchMock.mockImplementation(() => { ok.submitError = null; return Promise.resolve(); });
    const second = createBotCommands(ok).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    const answers = collect(second.answers);
    await vi.advanceTimersByTimeAsync(0);
    expect(ok.submits.map(s => s.requestId)).toEqual([REQUEST, REQUEST]);
    ok.answer({ seq: 0, body: text('Saved'), final: true });
    expect(await answers).toEqual([{ status: 200, body: text('Saved'), final: true }]);
  });

  // Final review I2: right after a Bot start its first answer can be a retryable refusal (503 E_BOT_OFFLINE, 401
  // E_BOT_SIGNATURE). While the catalog says ready, the run resends the same request id once, after 1 s.
  for (const [status, code] of [[503, 'E_BOT_OFFLINE'], [401, 'E_BOT_SIGNATURE']] as const) {
    it(`a ${status} answer while the catalog says ready is resent once with the same id`, async () => {
      const port = fakePort();
      port.catalogReady = () => true;
      const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
      const all = collect(run.answers);
      await vi.advanceTimersByTimeAsync(0);
      port.answer({ seq: 0, status, body: text(code), final: true });
      await vi.advanceTimersByTimeAsync(999);
      expect(port.submits).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(port.submits.map(s => s.requestId)).toEqual([REQUEST, REQUEST]);
      port.answer({ seq: 0, body: text('Saved'), final: true });
      expect(await all).toEqual([{ status: 200, body: text('Saved'), final: true }]);
    });
  }

  it('a second retryable refusal ends the run with it, and never loops', async () => {
    const port = fakePort();
    port.catalogReady = () => true;
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, status: 503, body: text('E_BOT_OFFLINE'), final: true });
    await vi.advanceTimersByTimeAsync(1_000);
    port.answer({ seq: 0, status: 503, body: text('E_BOT_OFFLINE'), final: true });
    expect(await all).toEqual([{ status: 503, body: text('E_BOT_OFFLINE'), final: true }]);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(port.submits).toHaveLength(2);
  });

  it('a 503 that is not the Bot’s start-up refusal (a handler’s own E_UNAVAILABLE) is the answer, with no resend', async () => {
    const port = fakePort();
    port.catalogReady = () => true;
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, status: 503, body: text('E_UNAVAILABLE'), final: true });
    expect(await all).toEqual([{ status: 503, body: text('E_UNAVAILABLE'), final: true }]);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(port.submits).toHaveLength(1);
  });

  it('a retryable refusal while the catalog is not ready is the answer, with no resend', async () => {
    const port = fakePort();
    port.catalogReady = () => false;
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, status: 503, body: text('E_BOT_OFFLINE'), final: true });
    expect(await all).toEqual([{ status: 503, body: text('E_BOT_OFFLINE'), final: true }]);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(port.submits).toHaveLength(1);
  });

  it('a refused send ends the run with the refusal', async () => {
    const port = fakePort();
    port.submitError = Object.assign(new Error('E_BOT_OFFLINE'), { code: 'E_BOT_OFFLINE', status: 503 });
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 80, args: [] });
    expect(await collect(run.answers)).toEqual([{ kind: 'refused', code: 'E_BOT_OFFLINE', status: 503 }]);
  });

  it('nothing polls', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 81, args: [] });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(await all).toEqual([{ kind: 'timeout' }]);
    // One request in 60 s: no reads, no resends, no status checks.
    expect(port.submitMock).toHaveBeenCalledTimes(1);
    expect(port.refetchMock).not.toHaveBeenCalled();
  });
});

/** The worker side, faked around a core whose seal and open are recorded. */
function fakeRelay(over: { publication?: Uint8Array | null; sealError?: string[]; refreshed?: Uint8Array | null } = {}) {
  const emitted: BotAnswerEvent[] = [];
  const posts: { communityId: string; body: Uint8Array; idem: Uint8Array }[] = [];
  const sealErrors = [...(over.sealError ?? [])];
  let handle = 0;
  const forgotten: number[] = [];
  const core = {
    botCommandSeal: vi.fn<(publication: Uint8Array, request: unknown) => { sealed: Uint8Array; replyHandle: number }>(() => {
      const code = sealErrors.shift();
      if (code !== undefined) throw Object.assign(new Error(code), { code });
      handle += 1;
      return { sealed: new Uint8Array([0xa0, handle]), replyHandle: handle };
    }),
    // A reply "opens" when its first byte names the handle it was sealed to.
    botReplyOpen: vi.fn((h: number, _request: Uint8Array, _seq: number, sealed: Uint8Array) => {
      if (sealed[0] !== h) throw Object.assign(new Error('E_BOT_REPLY'), { code: 'E_BOT_REPLY' });
      return sealed.subarray(1);
    }),
    botReplyForget: vi.fn((h: number) => { forgotten.push(h); }),
  };
  const publication = over.publication === undefined ? PUBLICATION : over.publication;
  let usable = publication;
  const catalog = { publication: vi.fn(() => usable), reverify: vi.fn(() => usable !== null), invalidate: vi.fn(() => undefined),
    refresh: vi.fn(() => { usable = over.refreshed === undefined ? usable : over.refreshed; return Promise.resolve(); }) };
  const post = vi.fn((communityId: string, body: Uint8Array, idem: Uint8Array) => { posts.push({ communityId, body, idem }); return Promise.resolve(); });
  const deps: BotRelayDeps = {
    core: () => core, catalog,
    post,
    nowS: () => 1_758_659_000n,
    random: (n: number) => crypto.getRandomValues(new Uint8Array(n)),
    setTimer: (fn, ms) => setTimeout(fn, ms) as unknown as number,
    clearTimer: (h) => { clearTimeout(h); },
    emit: (a) => { emitted.push(a); },
  };
  return { relay: new BotCommandRelay(deps), core, catalog, deps, emitted, posts, forgotten, post };
}
/** An answer sealed to `handle` (the fake core opens it when the first byte names its handle); `final` is the sealed one. */
const sealedReply = (handle: number, status: number, body: string, final = false): Uint8Array =>
  new Uint8Array([handle, ...encodeBotAnswer({ status, body: text(body), final })]);

describe('BotCommandRelay (the worker side)', () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });

  it('seals to the verified publication and posts [request_id, sealed] once', async () => {
    const f = fakeRelay();
    await f.relay.send({ communityId: COMMUNITY, channelId: toHex(fid(0x21)), operation: 80, args: [60, 'tea', true, null], requestId: REQUEST });
    expect(f.core.botCommandSeal).toHaveBeenCalledTimes(1);
    const [publication, request] = f.core.botCommandSeal.mock.calls[0];
    expect(publication).toEqual(PUBLICATION);
    expect(request).toEqual({ requestId: fid(0x10), communityId: fid(0x0e), channelId: fid(0x21), operation: 80, args: [60, 'tea', 1, null],
      sentAt: 1_758_659_000n });
    expect(f.posts).toHaveLength(1);
    expect(decode(f.posts[0].body)).toEqual([fid(0x10), new Uint8Array([0xa0, 1])]);
    expect(f.posts[0].idem).toHaveLength(16);
  });

  it('a catalog whose key signature fails is shown greyed, not used: nothing is sealed', async () => {
    const f = fakeRelay({ publication: null });
    await expect(f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST })).rejects.toMatchObject({ code: 'E_BOT_OFFLINE' });
    expect(f.catalog.reverify).toHaveBeenCalledWith(COMMUNITY);
    expect(f.core.botCommandSeal).not.toHaveBeenCalled();
    expect(f.posts).toHaveLength(0);
  });

  it('a stale or expired key drops this device’s verdict, refetches the catalog once and checks again (M2)', async () => {
    // The core refuses the held key; checking again locally still fails; the catalog read brings the rotated key.
    const rotated = new Uint8Array([9]);
    const f = fakeRelay({ sealError: ['E_BOT_STALE'], refreshed: rotated });
    f.catalog.reverify.mockReturnValueOnce(false);
    await f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST });
    expect(f.catalog.invalidate).toHaveBeenCalledWith(COMMUNITY);
    expect(f.catalog.refresh).toHaveBeenCalledTimes(1);
    expect(f.core.botCommandSeal.mock.calls[1][0]).toEqual(rotated);
    // Still refused after all that: the refusal goes to the page, with no loop.
    const g = fakeRelay({ sealError: ['E_BOT_KEY_STALE', 'E_BOT_KEY_STALE'] });
    g.catalog.reverify.mockReturnValueOnce(false);
    await expect(g.relay.send({ communityId: COMMUNITY, channelId: null, operation: 80, args: [], requestId: REQUEST })).rejects.toMatchObject({ code: 'E_BOT_KEY_STALE' });
    expect(g.catalog.refresh).toHaveBeenCalledTimes(1);
    expect(g.posts).toHaveLength(0);
  });

  it('ends the stream on the sealed final only; the envelope’s final is not trusted (#3)', async () => {
    const f = fakeRelay();
    await f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 12, args: ['x'], requestId: REQUEST });
    // The server says final, the Bot did not: the stream goes on.
    f.relay.onReply({ requestId: fid(0x10), seq: 0, sealed: sealedReply(1, 200, 'Searching…', false), final: true });
    f.relay.onReply({ requestId: fid(0x10), seq: 0, sealed: sealedReply(1, 200, 'Searching…', false), final: false });
    // The server says not final, the Bot did: it ends.
    f.relay.onReply({ requestId: fid(0x10), seq: 1, sealed: sealedReply(1, 200, 'Now playing', true), final: false });
    expect(f.emitted.map(a => [a.seq, new TextDecoder().decode(a.body), a.final])).toEqual([[0, 'Searching…', false], [1, 'Now playing', true]]);
    expect(f.core.botReplyOpen.mock.calls[0][1]).toEqual(fid(0x10));
    expect(f.forgotten).toEqual([1]);
    f.relay.onReply({ requestId: fid(0x10), seq: 2, sealed: sealedReply(1, 200, 'late', true), final: true });
    expect(f.emitted).toHaveLength(2);
  });

  it('a retry keeps only the newest reply key: an answer sealed to the old one is dropped (#4)', async () => {
    const f = fakeRelay();
    await f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    f.relay.onReply({ requestId: fid(0x10), seq: 0, sealed: sealedReply(1, 200, 'first', false), final: false });
    await f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    expect(f.forgotten).toEqual([1]);
    expect(toHex(f.posts[0].idem)).not.toBe(toHex(f.posts[1].idem));
    f.relay.onReply({ requestId: fid(0x10), seq: 1, sealed: sealedReply(1, 200, 'old key', true), final: true });
    expect(f.emitted.map(a => new TextDecoder().decode(a.body))).toEqual(['first']);
    // The newest send's stream starts over: its seq 0 is accepted.
    f.relay.onReply({ requestId: fid(0x10), seq: 0, sealed: sealedReply(2, 200, 'list', true), final: true });
    expect(f.emitted.map(a => new TextDecoder().decode(a.body))).toEqual(['first', 'list']);
    expect(f.forgotten).toEqual([1, 2]);
  });

  it('forgets the reply handle 2 min after the last answer, or when the page closes the run (M5)', async () => {
    const f = fakeRelay();
    await f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 12, args: [], requestId: REQUEST });
    await vi.advanceTimersByTimeAsync(BOT_REPLY_HOLD_MS - 1000);
    f.relay.onReply({ requestId: fid(0x10), seq: 0, sealed: sealedReply(1, 200, 'Searching…'), final: false });
    await vi.advanceTimersByTimeAsync(BOT_REPLY_HOLD_MS - 1);
    expect(f.forgotten).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(f.forgotten).toEqual([1]);
    await f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 12, args: [], requestId: REQUEST });
    f.relay.forget(REQUEST);
    expect(f.forgotten).toEqual([1, 2]);
  });

  it('a reply for no request of this device, or one that does not open, is dropped', async () => {
    const f = fakeRelay();
    f.relay.onReply({ requestId: fid(0x33), seq: 0, sealed: sealedReply(1, 200, 'x', true), final: true });
    await f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 12, args: [], requestId: REQUEST });
    f.relay.onReply({ requestId: fid(0x10), seq: 0, sealed: sealedReply(9, 200, 'x', true), final: true });
    expect(f.emitted).toEqual([]);
  });

  it('a send the instance refused forgets its reply handle at once', async () => {
    const f = fakeRelay();
    f.post.mockRejectedValueOnce(Object.assign(new Error('E_BOT_DUPLICATE'), { code: 'E_BOT_DUPLICATE', status: 409 }));
    await expect(f.relay.send({ communityId: COMMUNITY, channelId: null, operation: 12, args: [], requestId: REQUEST })).rejects.toMatchObject({ code: 'E_BOT_DUPLICATE' });
    expect(f.forgotten).toEqual([1]);
  });
});

describe('the answer plaintext', () => {
  it('is [status, body, final] and nothing else', () => {
    expect(decodeBotAnswer(encode([200, text('hi'), 1]))).toEqual({ status: 200, body: text('hi'), final: true });
    expect(decodeBotAnswer(encode([200, text('hi'), 0]))).toEqual({ status: 200, body: text('hi'), final: false });
    expect(() => decodeBotAnswer(encode([200, text('hi')]))).toThrow();
    expect(() => decodeBotAnswer(encode([200, 'hi', 1]))).toThrow();
    expect(() => decodeBotAnswer(encode([70000, text('hi'), 1]))).toThrow();
    expect(() => decodeBotAnswer(encode([200, text('hi'), 2]))).toThrow();
    expect(toHex(encodeBotAnswer({ status: 0, body: fromHex('80'), final: true }))).toBe('8300418001');
  });
});

describe('fix round 1: the page side', () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });

  it('a first answer that arrives after the timeout, before Retry, is shown (M4)', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    const late = vi.fn();
    run.onLate(late);
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(await all).toEqual([{ kind: 'timeout' }]);
    port.answer({ seq: 0, body: text('Saved'), final: true });
    expect(late).toHaveBeenCalledWith({ status: 200, body: text('Saved'), final: true });
    expect(port.listening(REQUEST)).toBe(0);
    // Retry after that still works as a resend of the same id (the Bot de-duplicates).
    run.retry();
    await vi.advanceTimersByTimeAsync(0);
    expect(port.submits.map(s => s.requestId)).toEqual([REQUEST, REQUEST]);
  });

  it('Retry ends the timed-out run: a later answer goes to the new run only', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    const late = vi.fn();
    run.onLate(late);
    await vi.advanceTimersByTimeAsync(5_000);
    const again = run.retry();
    const second = collect(again.answers);
    await vi.advanceTimersByTimeAsync(0);
    port.answer({ seq: 0, body: text('list'), final: true });
    expect(await second).toEqual([{ status: 200, body: text('list'), final: true }]);
    expect(late).not.toHaveBeenCalled();
  });

  it('stop ends a timed-out run’s listening and keeps the reply secret (N1)', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    const late = vi.fn();
    run.onLate(late);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(port.listening(REQUEST)).toBe(1);
    run.stop();
    expect(port.listening(REQUEST)).toBe(0);
    port.answer({ seq: 0, final: true });
    expect(late).not.toHaveBeenCalled();
    expect(port.forgotten).toEqual([]);
  });

  it('onDone fires once when a timed-out run’s 2-minute late window ends (task 20b, M-B)', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    const done = vi.fn();
    run.onDone(done);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(done).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(BOT_REPLY_HOLD_MS);
    expect(done).toHaveBeenCalledTimes(1);
    expect(port.listening(REQUEST)).toBe(0);
    run.stop();
    expect(done).toHaveBeenCalledTimes(1);
    // A listener added after the end is told at once.
    const later = vi.fn();
    run.onDone(later);
    expect(later).toHaveBeenCalledTimes(1);
  });

  it('cancel ends the run with no item and asks the worker to forget the reply secret (M5)', async () => {
    const port = fakePort();
    const run = createBotCommands(port).sendBotCommand({ communityId: COMMUNITY, channelId: null, operation: 81, args: [], requestId: REQUEST });
    const all = collect(run.answers);
    await vi.advanceTimersByTimeAsync(0);
    run.cancel();
    expect(await all).toEqual([]);
    expect(port.forgotten).toEqual([REQUEST]);
    expect(port.listening(REQUEST)).toBe(0);
    port.answer({ seq: 0, final: true });
    await vi.advanceTimersByTimeAsync(10_000);
    expect(port.submits).toHaveLength(1);
  });
});
