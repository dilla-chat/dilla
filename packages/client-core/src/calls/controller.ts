// The worker's call controller (L-TS-56, L-TS-57): it joins, keys, leaves, pauses and rotates this device's call, keeps
// who is in voice (step 7) and the incoming DM rings (step 8). Every key, the join epoch's included, leaves the worker
// only as a `callKeys` push with its buffer transferred (L-TS-52); no command result and no slice carries a key byte.
import { CoreError, isTrustBlock, type ApplyResult, type CallMediaEpoch, type CorePort, type Id } from '../core-port';
import { callRingOf, Op, voiceStateOf, type Frame } from '../gateway/frames';
import { fromHex, toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import type { CallStats, Routes } from '../http/routes';
import { isMuted } from '../state/settings';
import { CALL_IDLE, type CallJoinResult, type CallKeys, type CallPhase, type CallState, type SliceTypes,
  type VoiceParticipant, type WorkerError } from '../state/types';
import type { FromWorker } from '../worker/protocol';
import { joinCall, JoinRefused, type JoinRoutes } from './join';
import { Rings } from './ring';
import { statusOf, sweepStaleCalls } from './sweep';
import { VoiceBook, type VoiceRoutes } from './voice';

/** L-TS-56 step 5: a fresh leaf secret every hour of a call, and a conflicted rotation tried again a minute later. */
export const CALL_ROTATE_MS = 3_600_000;
export const CALL_ROTATE_RETRY_MS = 60_000;
/** WB client m7: a rotation that fails (refused, conflicted, or its confirm throws) is tried again a minute later this many
 *  times in a row; then it waits for the next hourly tick, so a fault that persists is not retried every minute. */
export const CALL_ROTATE_RETRIES = 5;
/** L-TS-56 step 1: the list refresh of a call's pending leaves waits at most this long for the whole batch. */
export const WAIT_LIST_MS = 10_000;
/** web3.7 3a-client: a join waits at most this long for the boot sweep of leftover call groups (calls/sweep.ts). */
export const SWEEP_JOIN_WAIT_MS = 5000;

export type CallRoutes = VoiceRoutes & JoinRoutes & Pick<Routes, 'shareCall' | 'unshareCall' | 'postCallRtt' | 'postProposal'
  | 'declineCall' | 'postCommit' | 'endCallIfAlone'>;

/** What the call controller reads from, and publishes through, the worker's composition root. */
export interface CallDeps {
  core(): CorePort | null;
  routes: CallRoutes;
  post(message: FromWorker, transfer: Transferable[]): void;
  publish<K extends 'call' | 'voice' | 'rings'>(name: K, value: SliceTypes[K]): void;
  errorOf(e: unknown): WorkerError;
  now(): number;
  /** A monotonic clock (ms): the rejoin's 30 s bound (task-10 fix review N2). */
  monotonic(): number;
  setTimeout(fn: () => void, ms: number): number;
  clearTimeout(id: number): void;
  randomId(): Id;
  me(): { userId: Id; deviceId: Id } | null;
  instance(): { policyVersion: bigint; externalSenderPub: Uint8Array } | null;
  settings(): Record<string, string>;
  nameOf(userHex: string): string;
  participant(userHex: string, deviceHex: string, flags: number, channelHex: string): VoiceParticipant;
  voiceChannels(communityHex: string): string[];
  /** A known channel (any kind) or DM. */
  known(channelHex: string): boolean;
  /** A known voice channel's community, or a known DM (community null, with its participants); undefined otherwise. */
  callTarget(channelHex: string): { communityId: string | null; members: string[] } | undefined;
  communityListed(communityHex: string): boolean;
  /** The worker's trust refresh of a group's leaves (the existing one, L-TS-56: no second implementation). */
  refreshLeaves(groupId: Id): Promise<void>;
  /** Runs a job on the sync engine's lane of the group, so a rotation never interleaves with an apply. */
  serial(groupId: Id, job: () => Promise<void>): Promise<void>;
  /** An own commit of a call group (the rotation) was confirmed: reported like any other group change. */
  onCommitted(groupId: Id, result: ApplyResult): void;
  /** After a join's answer: the engine takes the group up (held frames, catch-up). */
  afterJoin(groupId: Id): void;
  /** A call of this id begins or ends here: what the worker kept for it (the last voice_self) goes (review M3). */
  callEnded(callHex: string): void;
  track(job: Promise<unknown>): void;
  /** Disposable WSS sample; unavailable/congested transports drop it without HTTP fallback. */
  sendStats?(callId: Id, report: CallStats): void;
}

/** A refusal of the call controller's own; the worker's error mapping reads its code. */
export class CallRefusal extends CoreError {}

interface Live {
  channel: string; channelId: Id; dm: boolean; members: string[]; groupId: Id; callId: Id; vdec: string;
  postedEpoch: bigint; leaving: boolean;
  /** Re-review N-m1: a hold made the page drop the key of postedEpoch (the pipeline then bars that epoch as a sender), so
   *  the hold ends only with a newer epoch's key. */
  keysDropped: boolean;
  /** The gate passed again at the unchanged epoch of such a hold: the rotation that moves the epoch on may run. */
  rotateOut: boolean;
  outgoing: { ringing: string[]; declined: string[]; notRung: string[] } | null;
  /** The users of the last keyed roster: the candidates of a pause's changedUser (review M4). */
  rosterUsers: string[];
  /** The devices of the last keyed roster: a leave from a roster of this device alone ends the call (concern 2). */
  rosterDevices: string[];
  /** The leave in flight (one per call): a second leave awaits it and sends nothing (task 8b review I1). */
  leaveRun: Promise<void> | null;
}
/** A join in flight (review I1): a leave or a sign-out cancels it with 'leave' (the self-Remove is sent), a reset with
 *  'reset' (forgotten locally, nothing sent), a drop of its server with 'dropped' (torn down as a leave, then phase
 *  removed); `done` settles once the join has torn down or finished. web3.7 3a-client: `waiting` while it waits for the
 *  boot sweep (it holds no group yet), and `wake` ends that wait at once when it is cancelled. */
/** Review I3: a channel's sweep requests: the signal they carry and those still in flight. */
interface SweepChannel { abort: AbortController; inflight: Set<Promise<unknown>> }
interface JoinToken { cancelled: 'leave' | 'reset' | 'dropped' | null; done: Promise<void>; channel: string; waiting: boolean; wake: (() => void) | null }

const IN_A_CALL: readonly CallPhase[] = ['joining', 'in', 'paused', 'leaving'];

export class CallController {
  private readonly book: VoiceBook;
  private readonly rings: Rings;
  private state: CallState = CALL_IDLE;
  private live: Live | null = null;
  private joining: JoinToken | null = null;
  private rotateTimer: number | null = null;
  /** Rotations failed in a row since the last one that committed (WB client m7). */
  private rotateFailures = 0;
  /** The channel whose blocked phase was seen held by that channel's own send block (its text group): that phase goes idle
   *  once the block clears (integration card), so the page can offer the join again. */
  private blockSeen: string | null = null;
  /** web3.7 3a-client: this ready's boot sweep (running, or done), null before it and once a re-ready may sweep again. */
  private sweeping: Promise<void> | null = null;
  /** The sweep in progress is the one of this generation; a stop (sign-out, the core detached) moves it on. */
  private sweepGen = 0;
  /** Review I3: the running sweep's requests by channel hex: their signal and the requests still in flight. */
  private sweepChannels = new Map<string, SweepChannel>();

  constructor(private readonly deps: CallDeps) {
    this.book = new VoiceBook({
      routes: deps.routes,
      publish: (value) => { deps.publish('voice', value); },
      participant: (user, device, flags, channel) => deps.participant(user, device, flags, channel),
      voiceChannels: (community) => deps.voiceChannels(community),
      known: (channel) => deps.known(channel),
    });
    this.rings = new Rings({
      now: () => deps.now(), setTimeout: (fn, ms) => deps.setTimeout(fn, ms), clearTimeout: (id) => { deps.clearTimeout(id); },
      publish: (rings) => { deps.publish('rings', rings); },
      ownUser: () => { const me = deps.me(); return me === null ? null : toHex(me.userId); },
      dmMembers: (channel) => { const t = deps.callTarget(channel); return t === undefined || t.communityId !== null ? undefined : t.members; },
      blocked: (channel) => this.channelBlocked(channel),
      busy: () => IN_A_CALL.includes(this.state.phase),
      quiet: (channel) => isMuted(deps.settings(), channel),
      nameOf: (user) => deps.nameOf(user),
    });
  }

  /** The three slices' initial values (L-TS-51), published when the worker is ready. */
  /** The member lists changed: the voice slice's names are read again (web-3 task 10, concern 4). */
  membersChanged(): void { this.book.renamed(); }

  // ---- web3.7 3a-client: the boot sweep of leftover call groups ----

  /** Leaves every call group a page of this browser left behind (calls/sweep.ts), once per ready: a second call while one
   *  runs, or after it ran, starts nothing (the worker drops the memo with the core, reset, and on sign-out, shutdown).
   *  A sweep that found the instance unreachable (status 0) is deferred: the next call (the first online) sweeps again.
   *  A join waits for it, at most SWEEP_JOIN_WAIT_MS. A failing sweep never fails a join. */
  startSweep(): void {
    if (this.sweeping !== null) return;
    this.sweepGen += 1;
    const gen = this.sweepGen;
    const stopped = (): boolean => this.sweepGen !== gen;
    let offline = false;
    const routes = this.deps.routes;
    const channels = new Map<string, SweepChannel>();
    this.sweepChannels = channels;
    /** A request of the sweep, counted in flight under the channel whose signal it carries. */
    const tracked = <T>(signal: AbortSignal | undefined, request: Promise<T>): Promise<T> => {
      const entry = [...channels.values()].find((c) => c.abort.signal === signal);
      if (entry === undefined) return request;
      entry.inflight.add(request);
      void request.catch(() => undefined).finally(() => { entry.inflight.delete(request); });
      return request;
    };
    const run = sweepStaleCalls({
      core: () => this.deps.core(),
      me: () => this.deps.me(),
      routes: {
        getChannelCall: async (channel, signal) => {
          try { return await tracked(signal, routes.getChannelCall(channel, signal)); }
          catch (e) { if (statusOf(e) === 0) offline = true; throw e; }
        },
        endCallIfAlone: (callId, signal) => tracked(signal, routes.endCallIfAlone(callId, signal)),
        postProposal: (groupId, body, signal) => tracked(signal, routes.postProposal(groupId, body, signal)),
      },
      signal: (channelId) => {
        const key = toHex(channelId);
        let entry = channels.get(key);
        if (entry === undefined) {
          entry = { abort: new AbortController(), inflight: new Set() };
          // A stop that came first: every later request of this sweep is cancelled at once.
          if (stopped()) entry.abort.abort();
          channels.set(key, entry);
        }
        return entry.abort.signal;
      },
      serial: async <T>(groupId: Id, job: () => Promise<T>): Promise<T> => {
        let out: { value: T } | null = null;
        await this.deps.serial(groupId, async () => { out = { value: await job() }; });
        if (out === null) throw new CallRefusal('E_INTERNAL', 'the lane ran nothing');
        return (out as { value: T }).value;
      },
      heldGroup: () => this.heldGroup(),
      stopped,
    }).catch(() => undefined).then(() => {
      if (!stopped() && offline) this.sweeping = null;
    });
    this.sweeping = run;
  }

  /** The boot sweep stops where it is and sends nothing more (review I3: its requests in flight are aborted, retries
   *  included); the next startSweep sweeps again. */
  private stopSweep(): void {
    this.sweepGen += 1;
    this.sweeping = null;
    for (const entry of this.sweepChannels.values()) entry.abort.abort();
  }

  /** The group the sweep never touches: the live call's, else the channel's call group of a join past its sweep wait. */
  private heldGroup(): Id | null {
    if (this.live !== null) return this.live.groupId;
    const joining = this.joining;
    if (joining === null || joining.waiting) return null;
    try {
      return this.deps.core()?.groups().find((g) => g.kind === 1 && g.state !== 4 && toHex(g.targetId) === joining.channel)?.groupId ?? null;
    } catch {
      return null;
    }
  }

  /** web3.7 3a-client: the join's wait for the boot sweep, at most SWEEP_JOIN_WAIT_MS, ended at once by a cancel. Review
   *  I3: a wait the bound ends holds the channel's group from then on, aborts the sweep's requests for the channel and
   *  waits for them to settle (at once, once aborted), so the join's own read of the call comes after them. */
  private async awaitSweep(token: JoinToken): Promise<void> {
    const sweeping = this.sweeping;
    if (sweeping === null) return;
    let timer: number | null = null;
    let bound = false;
    await Promise.race([sweeping, new Promise<void>((resolve) => {
      token.wake = resolve;
      timer = this.deps.setTimeout(() => { bound = true; resolve(); }, SWEEP_JOIN_WAIT_MS);
    })]);
    if (timer !== null) this.deps.clearTimeout(timer);
    token.wake = null;
    if (!bound || token.cancelled !== null) return;
    token.waiting = false;
    const entry = this.sweepChannels.get(token.channel);
    if (entry === undefined) return;
    entry.abort.abort();
    await Promise.allSettled([...entry.inflight]);
  }

  /** A join in flight is cancelled (`reset` overrides an earlier reason; the others keep the first), and its wait for
   *  the boot sweep ends now. */
  private cancelJoining(token: JoinToken, reason: 'leave' | 'reset' | 'dropped'): void {
    if (reason === 'reset') token.cancelled = 'reset';
    else token.cancelled ??= reason;
    token.wake?.();
  }

  publishInitial(): void {
    this.deps.publish('call', this.state);
    this.deps.publish('voice', this.book.current());
    this.deps.publish('rings', this.rings.list());
  }

  // ---- step 1: the join ----

  async join(channel: string, vdec: string): Promise<CallJoinResult> {
    const target = this.deps.callTarget(channel);
    if (target === undefined) throw new CallRefusal('E_BAD_INPUT', 'not a voice channel or a DM');
    if (!(['idle', 'blocked', 'removed', 'failed'] as CallPhase[]).includes(this.state.phase)) throw new CallRefusal('E_CALL_ACTIVE', '');
    // Re-review note 1: a blocked call's leave may still be in flight (its self-Remove and forget); a join now could be
    // overtaken by it, so it is refused until the leave is done.
    if (this.live !== null) throw new CallRefusal('E_CALL_ACTIVE', 'a leave is in flight');
    const core = this.deps.core();
    const instance = this.deps.instance();
    if (core === null || instance === null) throw new CallRefusal('E_NOT_READY', '');
    const dm = target.communityId === null;
    // Decision 4: a DM call is refused while the DM's text group is send-blocked, before anything reaches the instance.
    if (dm) {
      const text = this.textGroup(channel);
      const code = text === null ? null : core.sendBlockCode(text);
      if (code !== null) {
        this.setState({ ...CALL_IDLE, phase: 'blocked', channelId: channel, error: this.deps.errorOf(new CallRefusal(code, '')),
          changedUser: code === 'E_UMK_CHANGED' ? this.changedUser(text === null ? [] : [text], target.members) : null });
        throw new CallRefusal(code, '');
      }
    }
    this.setState({ ...CALL_IDLE, phase: 'joining', channelId: channel });
    // Review M3: a voice_self kept from an earlier call of this channel is not re-sent into this one.
    this.deps.callEnded(channel);
    const channelId = fromHex(channel);
    let finish = (): void => {};
    const token: JoinToken = { cancelled: null, done: new Promise<void>((resolve) => { finish = resolve; }), channel, waiting: true, wake: null };
    this.joining = token;
    try {
      // web3.7 3a-client: before anything reaches the instance, the boot sweep leaves what a page of this browser left
      // behind (a rejoin after a reload waits for it), bounded by SWEEP_JOIN_WAIT_MS.
      await this.awaitSweep(token);
      token.waiting = false;
      if (token.cancelled !== null) throw this.cancelled(token, channel);
      return await this.joinOnce(token, core, instance, target, channel, channelId, vdec);
    } finally {
      if (this.joining === token) this.joining = null;
      finish();
    }
  }

  private async joinOnce(token: JoinToken, core: CorePort, instance: { policyVersion: bigint; externalSenderPub: Uint8Array },
    target: { communityId: string | null; members: string[] }, channel: string, channelId: Id, vdec: string): Promise<CallJoinResult> {
    const dm = target.communityId === null;
    let joined;
    try {
      joined = await joinCall({
        core, routes: this.deps.routes, policyVersion: instance.policyVersion, externalSenderPub: instance.externalSenderPub,
        randomId: () => this.deps.randomId(), refreshLeaves: (g) => this.deps.refreshLeaves(g),
        wait: (ms) => new Promise<void>((resolve) => { this.deps.setTimeout(resolve, ms); }),
        now: () => this.deps.monotonic(),
        cancelled: () => token.cancelled !== null,
        rejoining: () => {
          if (token.cancelled === null && this.joining === token && this.state.phase === 'joining') this.setState({ ...this.state, rejoining: true });
        },
      }, { channelId, communityId: target.communityId === null ? null : fromHex(target.communityId), vdec });
    } catch (e) {
      const cause = e instanceof JoinRefused ? e.cause : e;
      if (e instanceof JoinRefused && e.leave !== null) await this.teardown(token, e.leave, e.alone ? channelId : null);
      if (token.cancelled !== null) throw this.cancelled(token, channel);
      const phase = e instanceof JoinRefused ? e.phase : 'failed';
      const code = this.deps.errorOf(cause).code;
      // Ruling I2 and review M4: a refused join names a changed contact only among this call's own participants — the
      // leaf of the call tree the core's refusal names first (concern 3 of web-3 task 10: a leaf that never entered the
      // room), else the DM's members or the room's participants.
      const candidates = dm ? target.members : e instanceof JoinRefused ? e.users : [];
      this.setState({ ...CALL_IDLE, phase, channelId: channel, error: this.deps.errorOf(cause),
        changedUser: code === 'E_UMK_CHANGED' ? this.changedUser([], candidates, reportedUser(cause)) : null });
      throw cause;
    }
    const { groupId, media, started } = joined;
    // Review I1: a leave, sign-out, forget or revocation during the join: the key is zero-filled here and never posted,
    // the group is left (or, revoked, forgotten locally) and nothing is armed for it.
    if (token.cancelled !== null) {
      new Uint8Array(media.baseKey).fill(0);
      await this.teardown(token, groupId);
      throw this.cancelled(token, channel);
    }
    const own = toHex(this.deps.me()?.userId ?? new Uint8Array(16));
    const others = target.members.filter((u) => u !== own);
    // WB product walk I6: the instance answers whom it rang; an instance that does not (six elements) rang everyone.
    const rung = started.rung === null ? null : new Set(started.rung.map((u) => toHex(u)));
    const waiting = others.filter((u) => !this.book.usersIn(channel).has(u));
    const outgoing = dm && started.created ? { ringing: waiting.filter((u) => rung === null || rung.has(u)), declined: [],
      notRung: waiting.filter((u) => rung !== null && !rung.has(u)) } : null;
    this.live = { channel, channelId, dm, members: target.members, groupId, callId: started.callId, vdec, postedEpoch: media.epoch,
      leaving: false, keysDropped: false, rotateOut: false, outgoing, rosterUsers: [], rosterDevices: [], leaveRun: null };
    // The join epoch's key: a push posted before this command's ret, its buffer transferred (L-TS-52).
    this.postKeys(this.live, media);
    this.setIn(media);
    this.rings.remove(channel);
    this.rotateFailures = 0;
    this.armRotation(CALL_ROTATE_MS);
    this.deps.afterJoin(groupId);
    return { callId: toHex(started.callId), groupId: toHex(groupId), livekitUrl: started.livekitUrl, token: started.token,
      iceServers: started.iceServers, caps: started.caps, epoch: media.epoch.toString() };
  }

  // ---- step 2: the keys of later epochs ----

  /** A batch applied in a group: for the call group, a new epoch is keyed, and a gone own leaf is phase removed. */
  onGroupChanged(groupId: Id, result: ApplyResult): void {
    const live = this.live;
    if (live === null || toHex(live.groupId) !== toHex(groupId)) return;
    if (live.leaving) return;
    if (result.state === 4 || result.state === 3) {
      // Step 3: the own leaf is gone (another member committed its Remove), or (review M6) the group's state is lost: a
      // call group is never resynced, so the call ends here as removed.
      this.lost(live);
      return;
    }
    if (result.epochChanged) this.rekey(true);
  }

  /** The engine lost this device's membership of a call group (a refused read, a lost state): the call is removed. */
  onMembershipLost(groupId: Id): void {
    const live = this.live;
    if (live === null || live.leaving || toHex(live.groupId) !== toHex(groupId)) return;
    this.lost(live);
  }

  /** web3.4 C4 fix round 1: a server is gone from this account (a kick, a ban, its delete, a leave on another device), and
   *  with it these channels. A call in one ends here at once through the removed phase, as a lost membership does: the
   *  page stops its media and drops its keys on that phase, and the group is forgotten. The voice book forgets the
   *  channels, so a later rejoin shows only what the instance says then. */
  channelsDropped(channels: readonly string[]): void {
    if (channels.length === 0) return;
    const gone = new Set(channels);
    const live = this.live;
    if (live !== null && !live.leaving && gone.has(live.channel)) this.lost(live, true);
    // C4 fix round 2: a join in flight into one stops at its next step; its teardown leaves the group, as a leave's does.
    else if (this.joining !== null && live === null && this.state.channelId !== null && gone.has(this.state.channelId)) {
      this.cancelJoining(this.joining, 'dropped');
    }
    for (const channel of gone) this.book.forget(channel);
  }

  /** The removal of a call whose server is gone (E_NOT_FOUND, 404): the page shows voice.removed. */
  private goneError(): WorkerError {
    return this.deps.errorOf(new DillaHttpError({ status: 404, code: 'E_NOT_FOUND', detail: 'the server is gone', retryAfterMs: null, extra: [] }));
  }

  /** `dropped` (C4 fix round 1): the channel's server is gone from this account. The channel is not read again (it may no
   *  longer be seen: the removal says E_NOT_FOUND at once), and the group is forgotten on its lane, so an own commit the
   *  engine is posting (an op 17 Remove) finishes first. */
  private lost(live: Live, dropped = false): void {
    this.stopRotation();
    this.live = null;
    if (dropped) {
      const groupId = live.groupId;
      this.deps.track(this.deps.serial(groupId, () => {
        try { this.deps.core()?.callGroupForget(groupId); } catch { /* forgotten already */ }
        return Promise.resolve();
      }));
    } else {
      try { this.deps.core()?.callGroupForget(live.groupId); } catch { /* forgotten already */ }
    }
    this.deps.callEnded(toHex(live.callId));
    if (dropped) {
      this.setState({ ...CALL_IDLE, phase: 'removed', channelId: live.channel, error: this.goneError() });
      return;
    }
    this.setState({ ...CALL_IDLE, phase: 'removed', channelId: live.channel });
    // Product walk I8: the own entry leaves the voice slice now (the instance's leave reaches it later, or never once the
    // membership is gone), and the channel's call is read again: a 404 means this device may no longer see the channel,
    // so its entry goes and the removal says so (E_NOT_FOUND), which the page shows instead of a join that cannot work.
    const own = this.deps.me();
    const ownHex = own === null ? null : toHex(own.deviceId);
    if (ownHex !== null) this.book.dropDevice(live.channel, ownHex);
    this.deps.track((async () => {
      try {
        await this.book.loadChannel(live.channelId);
        if (ownHex !== null) this.book.dropDevice(live.channel, ownHex);
      } catch (e) {
        const error = this.deps.errorOf(e);
        if (error.code !== 'E_NOT_FOUND') return;
        this.book.forget(live.channel);
        if (this.state.phase === 'removed' && this.state.channelId === live.channel) this.setState({ ...this.state, error });
      }
    })());
  }

  /** After every trust refresh: a pause may end (the changed contact verified), a pending leaf may now validate, and a
   *  block the refresh found mid-epoch is acted on. */
  onTrust(): void { this.rekey(false); }

  private rekey(fromEpoch: boolean): void {
    const live = this.live;
    const core = this.deps.core();
    // The block check: a device that is leaving, blocked or paused-and-refused posts no key.
    if (live === null || core === null || live.leaving || (this.state.phase !== 'in' && this.state.phase !== 'paused')) return;
    let media: CallMediaEpoch;
    try {
      media = core.callMediaEpoch(live.groupId);
    } catch (e) {
      const code = e instanceof CoreError ? e.code : 'E_INTERNAL';
      // Wave 2 security review m1: any refusal (a changed identity, a pending leaf, a core error) withdraws the hold's
      // way-out rotation; it is armed again only once the gate passes at the unchanged epoch. keysDropped stays.
      live.rotateOut = false;
      // RV-3: a changed identity pauses this device; it stays in the call group and the room.
      if (code === 'E_UMK_CHANGED') {
        const text = live.dm ? this.textGroup(live.channel) : null;
        this.setState({ ...this.state, phase: 'paused', error: this.deps.errorOf(e),
          changedUser: this.changedUser(text === null ? [live.groupId] : [live.groupId, text], live.dm ? live.members : live.rosterUsers) });
        return;
      }
      // Any other gate refusal (unlisted, credential, tier, binding): blocked, and the device leaves.
      if (isTrustBlock(code)) {
        this.setState({ ...this.state, phase: 'blocked', error: this.deps.errorOf(e), changedUser: null });
        this.deps.track(this.leave(true));
        return;
      }
      // A pending leaf: no key until a refresh validates it (the refresh ends in onTrust, which keys then).
      if (code === 'E_DEVICE_LIST_STALE' && fromEpoch) this.deps.track(this.deps.refreshLeaves(live.groupId));
      // WB security I1: once the group has moved past the epoch whose key the page holds, that key is one a leaf the new
      // epoch's commit removed still holds. So a new epoch the gate refuses as pending, or for any other reason (an
      // unexpected core error fails closed), holds the call: phase paused with no changed contact and the refusal's
      // code, so the page sends and plays nothing and drops its keys, until the current epoch's key is posted (setIn ends
      // it; that epoch is always newer than the page's). Within the epoch already keyed, the page's key is that epoch's
      // own, which only leaves the gate passed hold: nothing changes there.
      if (fromEpoch || this.epochMoved(live)) {
        live.keysDropped = true;
        this.setState({ ...this.state, phase: 'paused', error: this.deps.errorOf(e), changedUser: null });
      }
      return;
    }
    if (media.epoch === live.postedEpoch) {
      // The page holds this epoch's key already; nothing is posted, and a pause on it ends.
      new Uint8Array(media.baseKey).fill(0);
      // Re-review N-m1: unless a hold made the page drop it (the fail-closed hold at an unchanged epoch): then the call
      // stays held, never "in" and silent, and a rotation moves the epoch on now; that epoch's key ends the hold.
      if (live.keysDropped) {
        if (!live.rotateOut) { live.rotateOut = true; this.armRotation(0); }
        return;
      }
      if (this.state.phase === 'paused') this.setIn(media);
      return;
    }
    live.keysDropped = false;
    live.rotateOut = false;
    live.postedEpoch = media.epoch;
    this.postKeys(live, media);
    this.setIn(media);
  }

  /** The call group's epoch is not the one whose key was posted last (a read that fails answers yes: fail closed). */
  private epochMoved(live: Live): boolean {
    try { return this.deps.core()?.groupRow(live.groupId)?.epoch !== live.postedEpoch; } catch { return true; }
  }

  private postKeys(live: Live, media: CallMediaEpoch): void {
    const keys: CallKeys = { groupId: toHex(live.groupId), epoch: media.epoch.toString(), baseKey: media.baseKey, selfLeaf: media.ownLeaf,
      roster: media.roster.map((r) => ({ leaf: r.leaf, deviceId: toHex(r.deviceId), userId: toHex(r.userId) })) };
    this.deps.post({ t: 'callKeys', callId: toHex(live.callId), keys }, [media.baseKey]);
  }

  private setIn(media: CallMediaEpoch): void {
    const live = this.live;
    if (live === null) return;
    live.rosterUsers = media.roster.map((r) => toHex(r.userId));
    live.rosterDevices = media.roster.map((r) => toHex(r.deviceId));
    this.setState({ phase: 'in', channelId: live.channel, callId: toHex(live.callId), epoch: media.epoch.toString(), code: media.code,
      error: null, roster: media.roster.map((r) => ({ userId: toHex(r.userId), deviceId: toHex(r.deviceId), leaf: r.leaf })),
      outgoing: this.outgoingNames(live), changedUser: null, rejoining: false });
  }

  // ---- step 4: the leave ----

  /** `lost` (WB client I3): the page's room ended on its own; the call ends in phase removed (E_DISCONNECTED), so the
   *  page and every other reader see a defined end rather than a silent idle. */
  async leave(blocked = false, lost = false): Promise<null> {
    // Review I1: a join in flight is cancelled and this answers once it has torn down (its self-Remove sent).
    const joining = this.joining;
    if (joining !== null && this.live === null) {
      this.cancelJoining(joining, 'leave');
      await joining.done;
      return null;
    }
    const live = this.live;
    if (live === null) {
      // Task 8b review I1: the page's leave after the worker's own blocked or removed end keeps that phase, with its error
      // and changedUser (plan :403, "stays blocked when leaving because of a block"); only a live call's leave idles.
      if (!blocked && this.state.phase !== 'blocked' && this.state.phase !== 'removed') this.setState({ ...CALL_IDLE });
      return null;
    }
    // A leave already running (the worker's own blocked leave, or an earlier callLeave): wait for it, never a second
    // self-Remove, and its outcome stands.
    if (live.leaveRun !== null) { await live.leaveRun; return null; }
    live.leaveRun = this.leaveLive(live, blocked, lost);
    await live.leaveRun;
    return null;
  }

  private async leaveLive(live: Live, blocked: boolean, lost = false): Promise<void> {
    // Never a rotation after this point: call_rotate would cancel the posted self-Remove (task 5, progress note).
    live.leaving = true;
    this.stopRotation();
    if (!blocked) this.setState({ ...this.state, phase: 'leaving' });
    // Web-3 task 10, concern 2: nobody can commit the Remove of the only leaf, and the instance refuses this device's
    // resync while that Remove stands (guard 1), so a person alone in the call ends it as they leave, while still its
    // leaf: its room and group close, and a rejoin registers a fresh group at once. A blocked leave too (f10app m7): a
    // blocked device is still a current leaf, and ending a call it is alone in exposes nothing. The end is the
    // conditional form only (f10app I1): the instance ends the call only while this device is its group's only current
    // leaf, so a device that has just joined (its commit landed, not yet keyed here nor in the room) keeps its call;
    // then (409) this device just leaves.
    if (await this.alone(live)) {
      try { await this.deps.routes.endCallIfAlone(live.callId); } catch { /* the room's departure timeout ends it instead */ }
    }
    await this.leaveGroup(live.groupId);
    if (this.live === live) this.live = null;
    this.deps.callEnded(toHex(live.callId));
    this.setState(blocked ? { ...this.state, phase: 'blocked' }
      : lost ? { ...CALL_IDLE, phase: 'removed', channelId: live.channel, error: this.deps.errorOf(new CallRefusal('E_DISCONNECTED', '')) }
        : { ...CALL_IDLE });
  }

  /** Whether this device is the call's only one: the last keyed roster holds no other leaf, and the room's snapshot read
   *  now shows no other device (one joining since the roster was keyed). A read that fails answers no. */
  private async alone(live: Live): Promise<boolean> {
    const me = this.deps.me();
    if (me === null) return false;
    const own = toHex(me.deviceId);
    if (live.rosterDevices.length === 0 || live.rosterDevices.some((d) => d !== own)) return false;
    try {
      const read = await this.deps.routes.getChannelCall(live.channelId);
      if (read.groupId !== null && toHex(read.groupId) !== toHex(live.groupId)) return false;
      return read.participants.every((p) => toHex(p.deviceId) === own);
    } catch {
      return false;
    }
  }

  /** The self-Remove (a 409 E_REMOVE_PENDING is fine, any failure leaves the group to the instance) and the forget, on the
   *  group's lane (review M1): a rotation in flight commits first, and the Remove is built on the epoch it left. */
  private async leaveGroup(groupId: Id): Promise<void> {
    await this.deps.serial(groupId, async () => {
      const core = this.deps.core();
      if (core === null) return;
      try { await this.deps.routes.postProposal(groupId, core.callLeaveProposal(groupId)); }
      catch { /* E_REMOVE_PENDING, or the instance removes the leaf when the call ends */ }
      try { core.callGroupForget(groupId); } catch { /* forgotten already */ }
    });
  }

  /** A cancelled or refused join's group: left with its self-Remove, or (a reset: the session is gone) forgotten locally.
   *  `aloneIn` (WB client I4) is the channel of a group whose tree holds only this device: nobody could commit its
   *  self-Remove, so the call is ended conditionally (its group closes, the next join registers afresh) and the group is
   *  forgotten without one. Should another device have joined meanwhile (409), the self-Remove is sent after all. */
  private async teardown(token: JoinToken, groupId: Id, aloneIn: Id | null = null): Promise<void> {
    if (token.cancelled === 'reset') {
      try { this.deps.core()?.callGroupForget(groupId); } catch { /* forgotten already */ }
      return;
    }
    if (aloneIn !== null) {
      // The call id of a call is its channel's id (protocol/09 § Voice). A start that never reached the instance left no
      // call to end (404): the group stays with this one leaf until the idle close (RV-2), and the next join resyncs it.
      // Re-review N-m3: only a 404 is "no call to end"; any other failure leaves open whether another device joined, so
      // the self-Remove goes out as for a 409 (a self-Remove is a member proposal: it never locks the rejoin, guard 1).
      let ended: boolean;
      try { ended = await this.deps.routes.endCallIfAlone(aloneIn); }
      catch (e) { ended = this.deps.errorOf(e).status === 404; }
      if (ended) {
        await this.deps.serial(groupId, () => { try { this.deps.core()?.callGroupForget(groupId); } catch { /* forgotten */ } return Promise.resolve(); });
        return;
      }
    }
    await this.leaveGroup(groupId);
  }

  private cancelled(token: JoinToken, channel: string): CallRefusal {
    if (token.cancelled === 'leave') this.setState({ ...CALL_IDLE });
    // C4 fix round 2: the channel's server was dropped while joining; the page says the call was removed.
    if (token.cancelled === 'dropped') this.setState({ ...CALL_IDLE, phase: 'removed', channelId: channel, error: this.goneError() });
    return new CallRefusal('E_NO_CALL', 'the call was left while it was joining');
  }

  // ---- step 5: the rotation ----

  private armRotation(ms: number): void {
    this.stopRotation();
    this.rotateTimer = this.deps.setTimeout(() => { this.rotateTimer = null; this.deps.track(this.rotate()); }, ms);
  }

  /** A failed rotation of the live call: a minute later, at most CALL_ROTATE_RETRIES times in a row, then hourly again. */
  private retryRotation(live: Live): void {
    if (this.live !== live || live.leaving) return;
    this.rotateFailures += 1;
    if (this.rotateFailures > CALL_ROTATE_RETRIES) { this.rotateFailures = 0; this.armRotation(CALL_ROTATE_MS); return; }
    this.armRotation(CALL_ROTATE_RETRY_MS);
  }

  private stopRotation(): void {
    if (this.rotateTimer !== null) { this.deps.clearTimeout(this.rotateTimer); this.rotateTimer = null; }
  }

  private async rotate(): Promise<void> {
    const live = this.live;
    const core = this.deps.core();
    if (live === null || core === null || live.leaving) return;
    // RV-3: while paused nothing is sent; the rotation waits for the next tick. Re-review N-m1: a hold whose keys the page
    // dropped, with the gate passing again at the unchanged epoch, rotates to a fresh epoch, the only way out of it.
    const heldOut = this.state.phase === 'paused' && live.rotateOut;
    if (this.state.phase === 'paused' && !heldOut) { this.armRotation(CALL_ROTATE_MS); return; }
    if (this.state.phase !== 'in' && !heldOut) return;
    await this.deps.serial(live.groupId, async () => {
      // A leave that started while this waited for the lane (blocked or not, it sets `leaving` first): never a rotation,
      // since call_rotate would cancel the posted self-Remove. This is the one in-lane guard against it.
      if (this.live !== live || live.leaving) return;
      // Review M1: a pause that landed while this waited for the lane holds the rotation too (not the N-m1 way out).
      if (this.state.phase === 'paused' && !live.rotateOut) { this.armRotation(CALL_ROTATE_MS); return; }
      let body: Uint8Array;
      try { body = core.callRotate(live.groupId); }
      catch { this.retryRotation(live); return; }
      try {
        await this.deps.routes.postCommit(live.groupId, body);
      } catch {
        try { core.commitAbort(live.groupId); } catch { /* nothing pending */ }
        this.retryRotation(live);
        return;
      }
      // WB client m7: a confirm that throws must not stop the hourly rotation for the rest of the call (the job's
      // rejection was swallowed and nothing re-armed it). The instance has accepted the commit, so its pending state is
      // kept: the engine adopts the commit as this device's own when it arrives (aborting it would make that commit a
      // stranger's, and a call group that loses its state ends). The rotation is armed again, bounded.
      let result: ApplyResult;
      try { result = core.commitConfirm(live.groupId); }
      catch {
        this.retryRotation(live);
        return;
      }
      this.rotateFailures = 0;
      if (this.live === live && !live.leaving) this.armRotation(CALL_ROTATE_MS);
      this.deps.onCommitted(live.groupId, result);
    });
  }

  // ---- step 6: share, refresh, stats ----

  async share(on: boolean): Promise<null> {
    const live = this.requireLive();
    if (on && this.state.phase === 'paused') throw new CallRefusal('E_UNAVAILABLE', 'paused');
    if (on) await this.deps.routes.shareCall(live.callId); else await this.deps.routes.unshareCall(live.callId);
    return null;
  }

  async refresh(): Promise<{ token: string; iceServers: CallJoinResult['iceServers'] }> {
    const live = this.requireLive();
    const started = await this.deps.routes.startCall(live.channelId, live.vdec);
    return { token: started.token, iceServers: started.iceServers };
  }

  stats(report: CallStats): Promise<null> {
    const live = this.requireLive();
    this.deps.sendStats?.(live.callId, report);
    return Promise.resolve(null);
  }

  /** Signal task 8c: this device's RTT (ms) or a clear (null), for the stage's per-person ping. True once the instance stored
   *  it; false when it refused by rate (429) or by room (404, a reconnect not landed yet), so the page does not count it as sent
   *  and its next sample tries again (Task 11). */
  async rtt(ms: number | null): Promise<boolean> {
    const live = this.requireLive();
    try { await this.deps.routes.postCallRtt(live.callId, ms); }
    catch (e) {
      if (e instanceof DillaHttpError && (e.status === 429 || e.status === 404)) return false;
      throw e;
    }
    return true;
  }

  private requireLive(): Live {
    if (this.live === null || this.live.leaving) throw new CallRefusal('E_NO_CALL', '');
    return this.live;
  }

  // ---- steps 7 and 8: voice presence and rings ----

  /** loadVoice: exactly one id (the shape rules checked it); an unknown channel or community is refused. */
  async loadVoice(communityId: string | null, channelId: string | null): Promise<null> {
    if (channelId !== null) {
      if (this.deps.callTarget(channelId) === undefined) throw new CallRefusal('E_BAD_INPUT', 'unknown channel');
      await this.book.loadChannel(fromHex(channelId));
    } else if (communityId !== null) {
      if (!this.deps.communityListed(communityId)) throw new CallRefusal('E_BAD_INPUT', 'unknown community');
      await this.book.loadCommunity(fromHex(communityId));
    }
    this.shrinkRinging();
    return null;
  }

  /** callDecline: the ring goes here whatever the instance answers (a 404 is expected after the ring ended). */
  async decline(callId: string, ringTs: number): Promise<null> {
    // Review M2: only the ring this decline answers (L-HTTP-104 acts on the matching expires_ts alone).
    this.rings.removeCall(callId, ringTs);
    try { await this.deps.routes.declineCall(fromHex(callId), ringTs); }
    catch (e) { if (!(e instanceof DillaHttpError)) throw e; }
    return null;
  }

  /** A gateway frame of ops 50 and 53; a frame that does not read is dropped (L-GW-33). */
  onFrame(frame: Frame): void {
    if (frame.op === Op.voiceState) {
      let f;
      try { f = voiceStateOf(frame); } catch { return; }
      this.book.frame(f);
      this.shrinkRinging();
      return;
    }
    if (frame.op !== Op.callRing) return;
    let f;
    try { f = callRingOf(frame); } catch { return; }
    if (f.state !== 2) { this.rings.frame(f); return; }
    // State 2: a participant declined the call this device opened (R7).
    const live = this.live;
    const user = toHex(f.userId);
    if (live?.outgoing == null || live.channel !== toHex(f.channelId) || !live.members.includes(user)
      || user === toHex(this.deps.me()?.userId ?? new Uint8Array(16))) return;
    live.outgoing = { ringing: live.outgoing.ringing.filter((u) => u !== user), notRung: live.outgoing.notRung,
      declined: live.outgoing.declined.includes(user) ? live.outgoing.declined : [...live.outgoing.declined, user] };
    this.setState({ ...this.state, outgoing: this.outgoingNames(live) });
  }

  private shrinkRinging(): void {
    const live = this.live;
    if (live?.outgoing == null) return;
    const inCall = this.book.usersIn(live.channel);
    const ringing = live.outgoing.ringing.filter((u) => !inCall.has(u));
    const notRung = live.outgoing.notRung.filter((u) => !inCall.has(u));
    if (ringing.length === live.outgoing.ringing.length && notRung.length === live.outgoing.notRung.length) return;
    live.outgoing = { ...live.outgoing, ringing, notRung };
    this.setState({ ...this.state, outgoing: this.outgoingNames(live) });
  }

  /** After every sendBlocks publish (integration cards, task 9 review m8 and concern 1): a ring whose DM is send-blocked
   *  now ends, and a blocked phase held by its channel's own send block goes idle once that block cleared. A block only of
   *  the (left and forgotten) call group cannot be seen to clear here, so it stays; removed stays as it is. */
  onSendBlocks(): void {
    // Attacker: a contact whose identity changed while their ring shows; decision 4 extended to rings mid-ring.
    for (const r of this.rings.list()) if (this.channelBlocked(r.channelId)) this.rings.remove(r.channelId);
    const s = this.state;
    if (s.phase !== 'blocked' || s.channelId === null) return;
    if (this.textBlocked(s.channelId)) { this.blockSeen = s.channelId; return; }
    if (this.blockSeen === s.channelId && this.live === null && this.joining === null) this.setState({ ...CALL_IDLE });
  }

  // ---- step 9: resign and wipe ----

  /** WB client m5: a join in flight stops at its next step (its teardown sends the self-Remove), as a leave stops it.
   *  web3.7 3a-client: a join waiting for the boot sweep stops at once. */
  cancelJoin(): void {
    if (this.joining !== null && this.live === null) this.cancelJoining(this.joining, 'leave');
  }

  /** resign: the call is left (its self-Remove posted) and the rings cleared before the store is released. web3.7
   *  3a-client: a sign-out, a wipe or a resign stops the boot sweep first; it sends nothing more. */
  async shutdown(): Promise<void> {
    this.stopSweep();
    try { await this.leave(); } catch { /* the instance removes the leaf when the call ends */ }
    this.rings.clear();
    this.setState({ ...CALL_IDLE });
  }

  /** Everything this account's calls left in the worker goes with the account (wipe, release); nothing is sent. */
  reset(): void {
    // Review I1: a join in flight is cancelled; its group is forgotten locally and nothing is sent or posted.
    if (this.joining !== null) this.cancelJoining(this.joining, 'reset');
    // web3.7 3a-client: the core is detached; the sweep stops, and the next ready sweeps again (contract 6).
    this.stopSweep();
    this.stopRotation();
    this.live = null;
    this.state = CALL_IDLE;
    this.rings.clear();
    this.book.clear();
    this.deps.publish('call', this.state);
  }

  isCallGroup(groupId: Id): boolean {
    return this.deps.core()?.groupRow(groupId)?.kind === 1;
  }

  // ---- helpers ----

  private setState(next: CallState): void {
    if (next.phase !== 'blocked' || next.channelId === null) this.blockSeen = null;
    else if (this.textBlocked(next.channelId)) this.blockSeen = next.channelId;
    this.state = next;
    this.deps.publish('call', next);
    this.rings.refreshBusy();
  }

  private outgoingNames(live: Live): CallState['outgoing'] {
    return live.outgoing === null ? null
      : { ringing: live.outgoing.ringing.map((u) => this.deps.nameOf(u)), declined: live.outgoing.declined.map((u) => this.deps.nameOf(u)),
        notRung: live.outgoing.notRung.map((u) => this.deps.nameOf(u)) };
  }

  /** The active text group of a DM (kind 0), never its call group (L-CORE-51). */
  private textGroup(channel: string): Id | null {
    return this.deps.core()?.groups().find((g) => g.kind === 0 && g.state === 2 && toHex(g.targetId) === channel)?.groupId ?? null;
  }

  /** The channel's own send block: its active text group (a DM's, or a channel's) is send-blocked (the sendBlocks map). */
  private textBlocked(channel: string): boolean {
    const core = this.deps.core();
    const text = core === null ? null : this.textGroup(channel);
    if (core === null || text === null) return false;
    // A gate the core cannot read counts as held (fail closed: the phase never idles on an error).
    try { return core.sendBlockCode(text) !== null; } catch { return true; }
  }

  /** A ring's decision 4: the DM's text group, or its call group when this device holds it, is send-blocked. */
  private channelBlocked(channel: string): boolean {
    const core = this.deps.core();
    if (core === null) return true;
    for (const g of core.groups()) {
      if (g.state !== 2 || toHex(g.targetId) !== channel) continue;
      try { if (core.sendBlockCode(g.groupId) !== null) return true; } catch { return true; }
    }
    return false;
  }

  /** Ruling I2: the changed contact, the first by user id: a changed leaf of the groups in question, else a contact among
   *  the candidates (this call's own participants: the DM's members, the call's roster or snapshot) whose key changed;
   *  null when none is known (review M4: never a contact of another conversation). `reported` is the leaf of the refused
   *  join's tree the core named (its refusal's detail), taken first while the trust state shows that contact changed. */
  private changedUser(groups: readonly Id[], candidates: readonly string[], reported: string | null = null): string | null {
    const core = this.deps.core();
    const me = this.deps.me();
    if (core === null) return null;
    const own = me === null ? '' : toHex(me.userId);
    const inGroups = new Set(groups.map(toHex));
    try {
      const leaves = core.pendingLeaves().filter((l) => l.reason === 'changed' && inGroups.has(toHex(l.groupId)))
        .map((l) => toHex(l.userId)).filter((u) => u !== own).sort();
      if (leaves.length > 0) return leaves[0] ?? null;
      const changed = core.trustState().filter((r) => r.changedUmk !== null || r.blocked === 'E_UMK_CHANGED').map((r) => toHex(r.userId))
        .filter((u) => u !== own);
      if (reported !== null && reported !== own && changed.includes(reported)) return reported;
      const rows = changed.filter((u) => candidates.includes(u)).sort();
      return rows[0] ?? null;
    } catch {
      return null;
    }
  }
}

/** The changed contact a core refusal names (CoreError.changedUser), else null. */
function reportedUser(cause: unknown): string | null {
  return cause instanceof CoreError ? cause.changedUser : null;
}
