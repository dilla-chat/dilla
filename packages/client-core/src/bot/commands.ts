// Bot v2 (task 20): sealed Bot commands and their streamed answers.
//
// The worker half (BotCommandRelay) signs and seals a command to the community's verified command key in the core, sends it
// over the gateway socket (POST /v1/communities/{community}/bot/commands, body [request_id, sealed]) and opens each op 62
// bot_reply with the one-time reply secret the core keeps behind a handle. The page half (createBotCommands) turns one
// command into a run: its answers in order, a timeout when no first answer comes in 5 s, Retry with the same request id,
// and one catalog refetch and resend when the Bot refuses a stale key. Nothing here polls: an answer arrives as a frame.
//
// The answer plaintext the Bot seals is CBOR [status(uint ≤ 65535), body(bstr)]; `final` travels outside the seal (op 62).
import type { CoreClient } from '../bridge';
import { arr, bin, decode, encode, u53 } from '../cbor';
import type { BotCommandRequest, CorePort } from '../core-port';
import type { BotReplyFrame } from '../gateway/frames';
import { fromHex, toHex } from '../hex';

/** The spec's sealed-request timeout: with no first answer in this long, the run ends with { kind: 'timeout' }. */
export const BOT_FIRST_ANSWER_MS = 5_000;
/** The spec's offline reply drop: a reply secret is forgotten this long after the last answer (or the send). */
export const BOT_REPLY_HOLD_MS = 120_000;
/** The Bot's refusal of a command sealed to a key it no longer holds (410 E_BOT_KEY_STALE). */
export const BOT_KEY_STALE_STATUS = 410;
/** Reply keys kept per request id: one per send of it (a retry seals afresh; the Bot may answer either). */
const HANDLES_PER_REQUEST = 4;
/** Answers the worker keeps in the `botAnswers` slice for the page to read. */
export const BOT_ANSWER_LOG = 32;

export type BotArg = string | number | boolean | Uint8Array | null;
export interface BotAnswer { status: number; body: Uint8Array; final: boolean }
export type BotRunItem = BotAnswer | { kind: 'timeout' } | { kind: 'refused'; code: string; status: number };
export interface BotCommandRun { requestId: string; answers: AsyncIterable<BotRunItem>; retry(): BotCommandRun }
export interface BotCommandSubmit { communityId: string; channelId: string | null; operation: number; args: readonly BotArg[]; requestId: string }
export interface BotAnswerEvent { requestId: string; seq: number; status: number; body: Uint8Array; final: boolean }
/** One answer as the `botAnswers` slice carries it (no bytes in a slice: the body is hex); `n` rises by one per answer. */
export interface BotAnswerView { n: number; requestId: string; seq: number; status: number; body: string; final: boolean }

export class BotCommandError extends Error {
  constructor(readonly code: string, readonly status = 0) { super(code); this.name = 'BotCommandError'; }
}

export function decodeBotAnswer(plain: Uint8Array): { status: number; body: Uint8Array } {
  const [status, body] = arr(decode(plain), 2);
  const s = u53(status);
  if (s > 0xffff) throw new BotCommandError('E_BOT_ANSWER');
  return { status: s, body: bin(body) };
}
export function encodeBotAnswer(answer: { status: number; body: Uint8Array }): Uint8Array { return encode([answer.status, answer.body]); }

const codeOf = (e: unknown): string => (typeof e === 'object' && e !== null && 'code' in e && typeof e.code === 'string' ? e.code : 'E_BOT_SEND');
const statusOf = (e: unknown): number => (typeof e === 'object' && e !== null && 'status' in e && typeof e.status === 'number' ? e.status : 0);

// ---------------------------------------------------------------------------------------------------------------------
// The worker half.

export interface BotRelayDeps {
  core(): Pick<CorePort, 'botCommandSeal' | 'botReplyOpen' | 'botReplyForget'> | null;
  catalog: { publication(communityId: string): Uint8Array | null; reverify(communityId: string): boolean };
  /** The socket route POST /v1/communities/{community}/bot/commands; a refusal throws (409 E_BOT_DUPLICATE, 503 E_BOT_OFFLINE, 403). */
  post(communityId: string, body: Uint8Array, idem: Uint8Array): Promise<void>;
  /** Unix seconds by the instance's clock (sent_at; the Bot refuses one more than 2 min off). */
  nowS(): bigint;
  random(bytes: number): Uint8Array;
  setTimer(fn: () => void, ms: number): number;
  clearTimer(handle: number): void;
  emit(answer: BotAnswerEvent): void;
}

interface Pending { requestId: Uint8Array; handles: number[]; seen: Set<number>; timer: number | null }

export class BotCommandRelay {
  private readonly pending = new Map<string, Pending>();

  constructor(private readonly deps: BotRelayDeps) {}

  /** Seals and sends one command; resolves once the instance accepted it (202). A retry passes the same request id. */
  async send(req: BotCommandSubmit): Promise<void> {
    const core = this.deps.core();
    if (core === null) throw new BotCommandError('E_CORE_STATE');
    const sealedOnce = (): { sealed: Uint8Array; replyHandle: number } => {
      const publication = this.usablePublication(req.communityId);
      const request: BotCommandRequest = { requestId: fromHex(req.requestId), communityId: fromHex(req.communityId),
        channelId: req.channelId === null ? null : fromHex(req.channelId), operation: req.operation,
        args: req.args.map(a => (typeof a === 'boolean' ? (a ? 1 : 0) : a)), sentAt: this.deps.nowS() };
      return core.botCommandSeal(publication, request);
    };
    let seal: { sealed: Uint8Array; replyHandle: number };
    try { seal = sealedOnce(); }
    catch (e) {
      // The core seals only to a key it verified this session, before its not_after: check it again once.
      if (codeOf(e) !== 'E_BOT_KEY_STALE' && codeOf(e) !== 'E_BOT_STALE') throw e;
      this.deps.catalog.reverify(req.communityId);
      seal = sealedOnce();
    }
    const entry = this.pending.get(req.requestId) ?? { requestId: fromHex(req.requestId), handles: [], seen: new Set<number>(), timer: null };
    // A resend starts the stream over for this device: answers the page missed (it timed out) are wanted again.
    entry.seen.clear();
    entry.handles.push(seal.replyHandle);
    while (entry.handles.length > HANDLES_PER_REQUEST) this.forgetHandle(entry.handles.shift()!);
    this.pending.set(req.requestId, entry);
    this.arm(req.requestId, entry);
    try {
      await this.deps.post(req.communityId, encode([entry.requestId, seal.sealed]), this.deps.random(16));
    } catch (e) {
      // Refused: nothing reached the Bot under this seal, so its reply secret goes now.
      entry.handles = entry.handles.filter(h => h !== seal.replyHandle);
      this.forgetHandle(seal.replyHandle);
      if (entry.handles.length === 0) this.drop(req.requestId);
      throw e;
    }
  }

  /** op 62: opens the answer with the request's own reply secret; a replayed, unknown or unopenable one is dropped. */
  onReply(frame: BotReplyFrame): void {
    const requestId = toHex(frame.requestId);
    const entry = this.pending.get(requestId);
    const core = this.deps.core();
    if (entry === undefined || core === null || entry.seen.has(frame.seq)) return;
    let plain: Uint8Array | null = null;
    for (const handle of [...entry.handles].reverse()) {
      try { plain = core.botReplyOpen(handle, frame.requestId, frame.seq, frame.sealed); break; } catch { /* another send's key */ }
    }
    if (plain === null) return;
    let answer: { status: number; body: Uint8Array };
    try { answer = decodeBotAnswer(plain); } catch { return; }
    entry.seen.add(frame.seq);
    this.deps.emit({ requestId, seq: frame.seq, status: answer.status, body: answer.body, final: frame.final });
    if (frame.final) this.drop(requestId);
    else this.arm(requestId, entry);
  }

  /** Sign-out, revocation: every reply secret goes. */
  forgetAll(): void {
    for (const id of [...this.pending.keys()]) this.drop(id);
  }

  private usablePublication(communityId: string): Uint8Array {
    let publication = this.deps.catalog.publication(communityId);
    if (publication === null && this.deps.catalog.reverify(communityId)) publication = this.deps.catalog.publication(communityId);
    // No key the core accepted: the commands are greyed (offline reason) and nothing is sealed.
    if (publication === null) throw new BotCommandError('E_BOT_OFFLINE', 503);
    return publication;
  }

  private arm(requestId: string, entry: Pending): void {
    if (entry.timer !== null) this.deps.clearTimer(entry.timer);
    entry.timer = this.deps.setTimer(() => { entry.timer = null; this.drop(requestId); }, BOT_REPLY_HOLD_MS);
  }

  private drop(requestId: string): void {
    const entry = this.pending.get(requestId);
    if (entry === undefined) return;
    this.pending.delete(requestId);
    if (entry.timer !== null) this.deps.clearTimer(entry.timer);
    for (const handle of entry.handles) this.forgetHandle(handle);
    entry.handles = [];
  }

  private forgetHandle(handle: number): void {
    try { this.deps.core()?.botReplyForget(handle); } catch { /* already evicted */ }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// The page half.

export interface BotCommandPort {
  submit(req: BotCommandSubmit): Promise<void>;
  answers(requestId: string, listener: (answer: BotAnswerEvent) => void): () => void;
  refetchCatalog(communityId: string): Promise<void>;
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(handle: unknown): void;
  newRequestId(): string;
}

/** A single-consumer queue that is an AsyncIterable and ends once. */
class Queue<T> implements AsyncIterable<T> {
  private readonly items: T[] = [];
  private ended = false;
  private wake: (() => void) | null = null;
  push(item: T): void { if (!this.ended) { this.items.push(item); this.wake?.(); } }
  end(): void { this.ended = true; this.wake?.(); }
  async *[Symbol.asyncIterator](): AsyncIterator<T> {
    for (;;) {
      const next = this.items.shift();
      if (next !== undefined) { yield next; continue; }
      if (this.ended) return;
      await new Promise<void>(resolve => { this.wake = resolve; });
      this.wake = null;
    }
  }
}

export function createBotCommands(port: BotCommandPort): {
  sendBotCommand(req: { communityId: string; channelId: string | null; operation: number; args: readonly BotArg[]; requestId?: string }): BotCommandRun;
} {
  const sendBotCommand = (req: { communityId: string; channelId: string | null; operation: number; args: readonly BotArg[]; requestId?: string }): BotCommandRun => {
    const requestId = req.requestId ?? port.newRequestId();
    const submit: BotCommandSubmit = { communityId: req.communityId, channelId: req.channelId, operation: req.operation, args: req.args, requestId };
    const queue = new Queue<BotRunItem>();
    let closed = false;
    let answered = false;
    let staleResent = false;
    let lastSeq = -1;
    let first: unknown = null;
    let idle: unknown = null;
    const clear = (): void => {
      if (first !== null) { port.clearTimer(first); first = null; }
      if (idle !== null) { port.clearTimer(idle); idle = null; }
    };
    const close = (item?: BotRunItem): void => {
      if (closed) return;
      closed = true;
      clear();
      unsubscribe();
      if (item !== undefined) queue.push(item);
      queue.end();
    };
    const armFirst = (): void => { first = port.setTimer(() => { first = null; if (!answered) close({ kind: 'timeout' }); }, BOT_FIRST_ANSWER_MS); };
    const send = (): void => {
      void port.submit(submit).catch((e: unknown) => { close({ kind: 'refused', code: codeOf(e), status: statusOf(e) }); });
    };
    const unsubscribe = port.answers(requestId, (a) => {
      if (closed) return;
      if (a.status === BOT_KEY_STALE_STATUS) {
        // Review Focus 2: refetch the catalog once and resend with the same request id; a second stale answer is the end.
        if (staleResent) { close({ kind: 'refused', code: 'E_BOT_KEY_STALE', status: a.status }); return; }
        staleResent = true; answered = false; lastSeq = -1;
        clear();
        void port.refetchCatalog(req.communityId).catch(() => undefined).then(() => { if (!closed) { armFirst(); send(); } });
        return;
      }
      if (a.seq <= lastSeq) return;
      lastSeq = a.seq;
      answered = true;
      clear();
      queue.push({ status: a.status, body: a.body, final: a.final });
      if (a.final) { close(); return; }
      idle = port.setTimer(() => { idle = null; close(); }, BOT_REPLY_HOLD_MS);
    });
    armFirst();
    send();
    return { requestId, answers: queue, retry: () => sendBotCommand({ ...req, requestId }) };
  };
  return { sendBotCommand };
}

const randomRequestId = (): string => {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  if (bytes.every(b => b === 0)) bytes[15] = 1;
  return toHex(bytes);
};

/** The page's port over the core worker: `sendBotCommand` to seal and send, the `botAnswers` slice for the answers and
 *  `syncBotCatalog` for the one refetch a stale key asks for. */
export function botCommandPort(client: CoreClient): BotCommandPort {
  return {
    submit: async (req) => { await client.call({ m: 'sendBotCommand', ...req, args: [...req.args] }); },
    answers: (requestId, listener) => {
      let seen = Math.max(0, ...(client.get('botAnswers') ?? []).map(a => a.n));
      return client.subscribe('botAnswers', () => {
        for (const a of client.get('botAnswers') ?? []) {
          if (a.n <= seen) continue;
          seen = a.n;
          if (a.requestId === requestId) listener({ requestId, seq: a.seq, status: a.status, body: fromHex(a.body), final: a.final });
        }
      });
    },
    refetchCatalog: async (communityId) => { await client.call({ m: 'syncBotCatalog', communityId }); },
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (handle) => { clearTimeout(handle as ReturnType<typeof setTimeout>); },
    newRequestId: randomRequestId,
  };
}

const ports = new WeakMap<CoreClient, ReturnType<typeof createBotCommands>>();
/** sendBotCommand over this page's core client. */
export function botCommands(client: CoreClient): ReturnType<typeof createBotCommands> {
  let commands = ports.get(client);
  if (commands === undefined) { commands = createBotCommands(botCommandPort(client)); ports.set(client, commands); }
  return commands;
}
