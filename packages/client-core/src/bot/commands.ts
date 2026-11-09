// Bot v2 (task 20): sealed Bot commands and their streamed answers.
//
// The worker half (BotCommandRelay) signs and seals a command to the community's verified command key in the core, sends it
// over the gateway socket (POST /v1/communities/{community}/bot/commands, body [request_id, sealed]) and opens each op 62
// bot_reply with the one-time reply secret the core keeps behind a handle. The page half (createBotCommands) turns one
// command into a run: its answers in order, a timeout when no first answer comes in 5 s, Retry with the same request id,
// and one catalog refetch and resend when the Bot refuses a stale key. Nothing here polls: an answer arrives as a frame.
//
// The answer plaintext the Bot seals is CBOR [status(uint ≤ 65535), body(bstr), final(uint 0|1)]. Only the sealed `final`
// ends a stream: op 62's own `final` is the server's and is not trusted (fix round 1, automated review #3).
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
/** The Bot's retryable refusals right after it starts (503 E_BOT_OFFLINE, 401 E_BOT_SIGNATURE): while the catalog says
 *  ready, a run resends the same request id once, this long after the refusal (final review I2). */
export const BOT_TRANSIENT_REFUSALS: ReadonlyMap<number, string> = new Map([[401, 'E_BOT_SIGNATURE'], [503, 'E_BOT_OFFLINE']]);
export const BOT_TRANSIENT_RESEND_MS = 1_000;
/** Answers the worker keeps in the `botAnswers` slice for the page to read. */
export const BOT_ANSWER_LOG = 32;

export type BotArg = string | number | boolean | Uint8Array | null;
export interface BotAnswer { status: number; body: Uint8Array; final: boolean }
export type BotRunItem = BotAnswer | { kind: 'timeout' } | { kind: 'refused'; code: string; status: number };
export interface BotCommandRun {
  requestId: string;
  answers: AsyncIterable<BotRunItem>;
  /** Sends the same request id again (a new seal); this run stops listening. */
  retry(): BotCommandRun;
  /** Ends this run's listening (also after a timeout) and keeps the worker's reply secret: a new run of the same request
   *  id (the host's Retry) takes over its answers (fix round 2, N1). */
  stop(): void;
  /** The composer closed: the run ends with no item and the worker forgets its reply secret. */
  cancel(): void;
  /** After a timeout: the first answer that still arrives (before a retry or cancel, within 2 min) goes here. */
  onLate(listener: (answer: BotAnswer) => void): void;
  /** Called once when the run takes nothing more: its last answer, a stop, a cancel, or the end of the 2-minute late window
   *  after a timeout (task 20b, M-B: the host drops its run then). A listener added after the end is called at once. */
  onDone(listener: () => void): void;
}
export interface BotCommandSubmit { communityId: string; channelId: string | null; operation: number; args: readonly BotArg[]; requestId: string }
export interface BotAnswerEvent { requestId: string; seq: number; status: number; body: Uint8Array; final: boolean }
/** One answer as the `botAnswers` slice carries it (no bytes in a slice: the body is hex); `n` rises by one per answer. */
export interface BotAnswerView { n: number; requestId: string; seq: number; status: number; body: string; final: boolean }

export class BotCommandError extends Error {
  constructor(readonly code: string, readonly status = 0) { super(code); this.name = 'BotCommandError'; }
}

export function decodeBotAnswer(plain: Uint8Array): BotAnswer {
  const [status, body, final] = arr(decode(plain), 3);
  const s = u53(status); const f = u53(final);
  if (s > 0xffff || f > 1) throw new BotCommandError('E_BOT_ANSWER');
  return { status: s, body: bin(body), final: f === 1 };
}
export function encodeBotAnswer(answer: BotAnswer): Uint8Array { return encode([answer.status, answer.body, answer.final ? 1 : 0]); }

const codeOf = (e: unknown): string => (typeof e === 'object' && e !== null && 'code' in e && typeof e.code === 'string' ? e.code : 'E_BOT_SEND');
const statusOf = (e: unknown): number => (typeof e === 'object' && e !== null && 'status' in e && typeof e.status === 'number' ? e.status : 0);
/** A Bot answer that is one of its start-up refusals: the status and, as the body, exactly that status's code. */
function isTransientRefusal(status: number, body: Uint8Array): boolean {
  const code = BOT_TRANSIENT_REFUSALS.get(status);
  return code !== undefined && body.length === code.length && body.every((b, i) => b === code.charCodeAt(i));
}
const STALE_SEAL: ReadonlySet<string> = new Set(['E_BOT_KEY_STALE', 'E_BOT_STALE']);

// ---------------------------------------------------------------------------------------------------------------------
// The worker half.

export interface BotRelayDeps {
  core(): Pick<CorePort, 'botCommandSeal' | 'botReplyOpen' | 'botReplyForget'> | null;
  catalog: {
    publication(communityId: string): Uint8Array | null;
    reverify(communityId: string): boolean;
    invalidate(communityId: string): void;
    refresh(communityId: string): Promise<void>;
  };
  /** The socket route POST /v1/communities/{community}/bot/commands; a refusal throws (409 E_BOT_DUPLICATE, 503 E_BOT_OFFLINE, 403). */
  post(communityId: string, body: Uint8Array, idem: Uint8Array): Promise<void>;
  /** Unix seconds by the instance's clock (sent_at; the Bot refuses one more than 2 min off). */
  nowS(): bigint;
  random(bytes: number): Uint8Array;
  setTimer(fn: () => void, ms: number): number;
  clearTimer(handle: number): void;
  emit(answer: BotAnswerEvent): void;
}

/** One request id's newest send: its reply secret's handle and the seqs accepted under it. */
interface Pending { requestId: Uint8Array; handle: number; seen: Set<number>; timer: number | null }

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
      // The core no longer holds the key as verified (expired, or evicted): this device drops its verdict too, checks again,
      // and when that fails reads the catalog once (a rotated key) and checks again. Never more than that.
      if (!STALE_SEAL.has(codeOf(e))) throw e;
      this.deps.catalog.invalidate(req.communityId);
      if (!this.deps.catalog.reverify(req.communityId)) await this.deps.catalog.refresh(req.communityId);
      seal = sealedOnce();
    }
    // Only the newest send's reply secret is kept: answers sealed to an earlier one are dropped (automated review #4), and a
    // resend starts the stream over for this device.
    this.drop(req.requestId);
    const entry: Pending = { requestId: fromHex(req.requestId), handle: seal.replyHandle, seen: new Set<number>(), timer: null };
    this.pending.set(req.requestId, entry);
    this.arm(req.requestId, entry);
    try {
      await this.deps.post(req.communityId, encode([entry.requestId, seal.sealed]), this.deps.random(16));
    } catch (e) {
      // Refused: nothing reached the Bot under this seal, so its reply secret goes now.
      if (this.pending.get(req.requestId) === entry) this.drop(req.requestId);
      throw e;
    }
  }

  /** op 62: opens the answer with the newest send's reply secret; a replayed, unknown or unopenable one is dropped. The
   *  stream ends on the sealed `final` only. */
  onReply(frame: BotReplyFrame): void {
    const requestId = toHex(frame.requestId);
    const entry = this.pending.get(requestId);
    const core = this.deps.core();
    if (entry === undefined || core === null || entry.seen.has(frame.seq)) return;
    let answer: BotAnswer;
    try { answer = decodeBotAnswer(core.botReplyOpen(entry.handle, frame.requestId, frame.seq, frame.sealed)); } catch { return; }
    entry.seen.add(frame.seq);
    this.deps.emit({ requestId, seq: frame.seq, status: answer.status, body: answer.body, final: answer.final });
    if (answer.final) this.drop(requestId);
    else this.arm(requestId, entry);
  }

  /** The page closed the run: its reply secret goes now. */
  forget(requestId: string): void { this.drop(requestId); }

  /** Sign-out, revocation: every reply secret goes. */
  forgetAll(): void {
    for (const id of [...this.pending.keys()]) this.drop(id);
  }

  private usablePublication(communityId: string): Uint8Array {
    let publication = this.deps.catalog.publication(communityId);
    if (publication === null && this.deps.catalog.reverify(communityId)) publication = this.deps.catalog.publication(communityId);
    // No key this device verified: the commands are greyed and nothing is sealed.
    if (publication === null) throw new BotCommandError('E_BOT_OFFLINE', 503);
    return publication;
  }

  private arm(requestId: string, entry: Pending): void {
    if (entry.timer !== null) this.deps.clearTimer(entry.timer);
    entry.timer = this.deps.setTimer(() => { entry.timer = null; if (this.pending.get(requestId) === entry) this.drop(requestId); }, BOT_REPLY_HOLD_MS);
  }

  private drop(requestId: string): void {
    const entry = this.pending.get(requestId);
    if (entry === undefined) return;
    this.pending.delete(requestId);
    if (entry.timer !== null) this.deps.clearTimer(entry.timer);
    try { this.deps.core()?.botReplyForget(entry.handle); } catch { /* already evicted */ }
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// The page half.

export interface BotCommandPort {
  submit(req: BotCommandSubmit): Promise<void>;
  answers(requestId: string, listener: (answer: BotAnswerEvent) => void): () => void;
  refetchCatalog(communityId: string): Promise<void>;
  /** Whether the community's held catalog status is ready: only then is a retryable refusal resent. */
  catalogReady?(communityId: string): boolean;
  /** The run is over for the page: the worker may forget the reply secret now. */
  forget(requestId: string): void;
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

type SendInput = { communityId: string; channelId: string | null; operation: number; args: readonly BotArg[]; requestId?: string };

export function createBotCommands(port: BotCommandPort): { sendBotCommand(req: SendInput): BotCommandRun } {
  const sendBotCommand = (req: SendInput): BotCommandRun => {
    const requestId = req.requestId ?? port.newRequestId();
    const submit: BotCommandSubmit = { communityId: req.communityId, channelId: req.channelId, operation: req.operation, args: req.args, requestId };
    const queue = new Queue<BotRunItem>();
    /** open: the run's answers flow; late: it timed out, a first answer still shows; done: nothing more. */
    let phase: 'open' | 'late' | 'done' = 'open';
    let answered = false;
    let staleResent = false;
    let transientResent = false;
    let resend: unknown = null;
    let lastSeq = -1;
    let first: unknown = null;
    let idle: unknown = null;
    let late: ((answer: BotAnswer) => void) | null = null;
    let ended: (() => void) | null = null;
    const clear = (): void => {
      if (first !== null) { port.clearTimer(first); first = null; }
      if (idle !== null) { port.clearTimer(idle); idle = null; }
      if (resend !== null) { port.clearTimer(resend); resend = null; }
    };
    const finish = (forget: boolean): void => {
      if (phase === 'done') return;
      phase = 'done';
      clear();
      unsubscribe();
      queue.end();
      if (forget) port.forget(requestId);
      const listener = ended;
      ended = null;
      listener?.();
    };
    const close = (item?: BotRunItem): void => {
      if (phase !== 'open') return;
      if (item !== undefined) queue.push(item);
      finish(false);
    };
    const armFirst = (): void => {
      first = port.setTimer(() => {
        first = null;
        if (answered || phase !== 'open') return;
        // No first answer in 5 s: the run ends, but listens on (up to 2 min) for an answer that is only late (M4).
        queue.push({ kind: 'timeout' });
        queue.end();
        phase = 'late';
        idle = port.setTimer(() => { idle = null; finish(false); }, BOT_REPLY_HOLD_MS);
      }, BOT_FIRST_ANSWER_MS);
    };
    const send = (): void => {
      void port.submit(submit).catch((e: unknown) => {
        // dillad refuses a command sealed to a key the Bot no longer opens (Task 21b): read the catalog once (the new key)
        // and resend the same request id, as for a stale answer; a second refusal ends the run.
        if (codeOf(e) === 'E_BOT_KEY_STALE' && !staleResent && phase === 'open') {
          staleResent = true;
          clear();
          void port.refetchCatalog(req.communityId).catch(() => undefined).then(() => { if (phase === 'open') { armFirst(); send(); } });
          return;
        }
        close({ kind: 'refused', code: codeOf(e), status: statusOf(e) });
      });
    };
    const unsubscribe = port.answers(requestId, (a) => {
      if (phase === 'done') return;
      if (phase === 'late') {
        if (a.status === BOT_KEY_STALE_STATUS) return;
        late?.({ status: a.status, body: a.body, final: a.final });
        if (a.final) finish(false);
        return;
      }
      if (a.status === BOT_KEY_STALE_STATUS) {
        // Review Focus 2: refetch the catalog once and resend with the same request id; a second stale answer is the end.
        if (staleResent) { close({ kind: 'refused', code: 'E_BOT_KEY_STALE', status: a.status }); return; }
        staleResent = true; answered = false; lastSeq = -1;
        clear();
        void port.refetchCatalog(req.communityId).catch(() => undefined).then(() => { if (phase === 'open') { armFirst(); send(); } });
        return;
      }
      if (a.final && a.seq === 0 && !transientResent && isTransientRefusal(a.status, a.body)
        && port.catalogReady?.(req.communityId) === true) {
        // The Bot was only just starting (its feed not yet bound, the sender's device list mid-ingest): send the same
        // request id once more, a moment later; the Bot runs a request it refused this way on the new seal.
        transientResent = true; answered = false; lastSeq = -1;
        clear();
        resend = port.setTimer(() => { resend = null; if (phase === 'open') { armFirst(); send(); } }, BOT_TRANSIENT_RESEND_MS);
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
    return {
      requestId,
      answers: queue,
      retry: () => { finish(false); return sendBotCommand({ ...req, requestId }); },
      stop: () => { finish(false); },
      cancel: () => { finish(true); },
      onLate: (listener) => { late = listener; },
      onDone: (listener) => { if (phase === 'done') listener(); else ended = listener; },
    };
  };
  return { sendBotCommand };
}

const randomRequestId = (): string => {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  if (bytes.every(b => b === 0)) bytes[15] = 1;
  return toHex(bytes);
};

/** The page's port over the core worker: `sendBotCommand` to seal and send, the `botAnswers` slice for the answers,
 *  `syncBotCatalog` for the one refetch a stale key asks for and `forgetBotCommand` when a run closes. */
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
    catalogReady: (communityId) => client.get(`botCatalog:${communityId}`)?.status.state === 'ready',
    forget: (requestId) => { void client.call({ m: 'forgetBotCommand', requestId }).catch(() => undefined); },
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
