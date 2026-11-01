import { arr, bin, CborError, decode, encode, u64, type CborInput } from '../cbor';
import { CoreError, type ApplyResult, type CorePort, type ExpectedGroup, type GroupInfo, type Id, type OutboxRow, type SendRequest } from '../core-port';
import { Op, type Frame } from '../gateway/frames';
import type { Gateway, GatewayEvent, ReadyInfo } from '../gateway/gateway';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import type { Instance, Routes } from '../http/routes';
import { openChannelFlow, resyncGroup, welcomeFrame, welcomeStep } from './channel';
import { FAILED_LEAF, SyncError } from './errors';
import { SerialQueues } from './group-queue';
import { JoinAll } from './joinall';
import type { SyncInternals } from './internals';
import { discardSend, drainOne, retrySend, sendMessage, sendRequest } from './send';
import { ingestAccountKind, readDeviceListHistory, signedHeadList } from '../state/trust';
import { recoverModerationRemovals } from './removals';

export type SyncRoutes = Pick<Routes, 'listChannels' | 'postGroup' | 'getGroupInfo' | 'getGroupTree' | 'getHandshakes' | 'getMessages'
  | 'getGroupHealState' | 'postGroupHeal'
  | 'getProposals' | 'postCommit' | 'postMessage' | 'postCursor' | 'postResync' | 'getWelcomes' | 'deleteWelcome' | 'getChannel'
  | 'getInteractionAdmissionContext'
  | 'getDeviceList' | 'getDeviceListHistory' | 'getModerationRemovals'>;
export type SyncGateway = Pick<Gateway, 'subscribe' | 'commitAck'>;
export interface SyncDeps {
  core: CorePort; routes: SyncRoutes; gateway: SyncGateway; instance: Instance; deviceId: Id;
  now(): number; random(): number; setTimeout(fn: () => void, ms: number): number; clearTimeout(id: number): void;
  onGroupChanged(groupId: Id, result: ApplyResult): void; onOutboxChanged(groupId: Id): void;
  onTrustChanged(): void;
  /** A refusal of the account-kind ingest worth a diagnostics entry (the controller records it). */
  onError?: (code: string) => void;
  trackJob?(job: Promise<unknown>): void;
  /** 'member' (WB fix wave): a group reported not-member was read to the end again (the DS serves group reads to
   *  current leaves only), so the device holds its leaf after all. A frame applied to the group proves no such thing. */
  onMembership(groupId: Id, status: 'resyncing' | 'not-member' | 'member'): void;
  onJoinAll(progress: { done: number; total: number; failed: number }): void;
  /** A Welcome for an unexpected group; the controller reloads DMs. */
  onUnexpectedWelcome(groupId: Id): void;
  /** Called by drainOne before sendEncrypt for an outbox row with attachments, every attempt. Rejecting with a DillaHttpError
   *  404 fails the row with 'E_ATTACHMENT_MISSING'; any other rejection fails it with its code (the person may retry).
   *  (Pre-flight ruling F5: a DillaHttpError of status 0, 429 or 5xx is transient and leaves the row queued for the next ready.) */
  beforeSend(groupId: Id, row: OutboxRow): Promise<void>;
  /** Called by discardSend after sendDiscard with the row as the outbox held it. */
  onDiscarded(groupId: Id, row: OutboxRow): void;
  /** Reads the group's leaves' signed lists again (the controller's trust refresh). drainOne calls it once for a queued row
   *  that send_encrypt refused by a leaf verdict, and retrySend before a verdict-failed row is tried again. */
  refreshGroupTrust(groupId: Id): Promise<void>;
  /** CB2: a 429 requeued the group's row and the group waits until untilMs (ms since the epoch) before it posts again
   *  (queued: a post waits for it). untilMs 0 with queued false clears it: the waiting row was confirmed. */
  onSlow(groupId: Id, untilMs: number, queued: boolean): void;
  /** CB2: an upload of this device was confirmed; type is the row's envelope type. */
  onPosted(groupId: Id, type: number, row?: OutboxRow, confirmedSeq?: bigint): void;
}
export const SYNC = {
  handshakePage: 512, messagePage: 256, commitRetryMax: 5, commitJitterMs: 400,
  membershipWaitMs: 2000, echoWaitMs: 5000, registerRetryMax: 3, cursorDebounceMs: 30000, joinAllConcurrency: 1, joinAllRetryMs: 60000,
} as const;
export interface PageInfo { count: number; lastSeq: bigint | null; }
export function throughOf(ms: PageInfo, hs: PageInfo, carry: bigint | null): bigint {
  const max = (a: bigint, b: bigint): bigint => a > b ? a : b;
  const m = ms.lastSeq;
  const h = hs.lastSeq;
  const boundM = ms.count === SYNC.messagePage ? (m ?? 0n) : max(m ?? 0n, carry ?? 0n);
  const boundH = hs.count === SYNC.handshakePage ? (h ?? 0n) : max(h ?? 0n, m ?? 0n);
  return boundM < boundH ? boundM : boundH;
}
const key = (g: Id): string => `g:${toHex(g)}`;
const HELD_MAX = 512;

export class SyncEngine implements SyncInternals {
  readonly queues: SerialQueues;
  readonly echoWait = new Map<string, { msgHex: string; timer: number }>();
  readonly epochWait = new Map<string, number>();
  readonly slowWait = new Map<string, { timer: number; until: number }>();
  readonly slowed = new Set<string>();
  readonly queued = new Set<string>();
  readonly count425 = new Map<string, number>();
  readonly reframedOnce = new Set<string>();
  readonly trustRetried = new Map<string, 'pending' | 'done'>();
  readonly resyncTried = new Set<string>();
  readonly recoveryGeneration = new Map<string, bigint>();
  readonly quiet = new Set<string>();
  readonly unexpectedReported = new Set<string>();
  /** Groups whose last membership report was not-member (WB fix wave): a confirmed catch-up withdraws it. */
  private readonly lostReported = new Set<string>();
  private expectedList: ExpectedGroup[] = [];
  private readonly joinAll = new JoinAll(this, (p) => { this.deps.onJoinAll(p); });
  private readonly opened = new Map<string, ExpectedGroup>();
  private ready: ReadyInfo | null = null;
  private readonly held = new Map<string, Frame[]>();
  /** Active groups stay unable to drain queued sends until their current ready's heal check succeeds. */
  private readonly healBlocked = new Set<string>();
  private readonly volunteer = new Map<string, number>();
  private readonly cursorTimer = new Map<string, number>();
  private readonly timers = new Set<number>();
  private readonly waiters = new Set<() => void>();
  private unsubscribe: (() => void) | null = null;
  private isStopped = false;

  constructor(readonly deps: SyncDeps) { this.queues = new SerialQueues((job) => deps.trackJob?.(job)); }
  stopped(): boolean { return this.isStopped; }
  /** Every membership report goes through here, so the engine knows which groups it last reported not-member. */
  reportMembership(g: Id, status: 'resyncing' | 'not-member' | 'member'): void {
    if (status === 'not-member') this.lostReported.add(toHex(g)); else this.lostReported.delete(toHex(g));
    this.deps.onMembership(g, status);
  }
  start(): void {
    if (this.unsubscribe !== null || this.isStopped) return;
    this.unsubscribe = this.deps.gateway.subscribe((e) => { this.onEvent(e); });
  }
  stop(): void {
    if (this.isStopped) return;
    this.joinAll.stop();
    this.isStopped = true;
    this.unsubscribe?.(); this.unsubscribe = null;
    for (const id of this.timers) this.deps.clearTimeout(id);
    this.timers.clear(); this.volunteer.clear(); this.cursorTimer.clear(); this.epochWait.clear(); this.slowWait.clear(); this.slowed.clear(); this.queued.clear(); this.echoWait.clear(); this.reframedOnce.clear(); this.trustRetried.clear();
    this.queues.clear();
    for (const wake of this.waiters) wake();
    this.waiters.clear();
  }
  /** Resolves after ms, or at once when the engine stops (its timers are cleared); the caller checks stopped(). */
  wait(ms: number): Promise<void> {
    if (this.isStopped) return Promise.resolve();
    return new Promise((resolve) => {
      const wake = (): void => { this.waiters.delete(wake); resolve(); };
      this.waiters.add(wake);
      this.armTimer(ms, wake);
    });
  }
  /** CORE-ENGINE-01: a set that gained a group starts a pass (rule 7: the welcome step first); the same set again
   *  neither starts one nor cancels an armed retry (rule 4: a refused join waits for the next ready or
   *  joinAllRetryMs). Any change lets a kept unexpected Welcome be reported once more. */
  setExpected(groups: ExpectedGroup[]): void {
    const before = new Set(this.expectedList.map((g) => toHex(g.groupId)));
    const after = new Set(groups.map((g) => toHex(g.groupId)));
    const gained = [...after].some((id) => !before.has(id));
    this.expectedList = [...groups];
    if (gained || [...before].some((id) => !after.has(id))) this.unexpectedReported.clear();
    if (gained && this.ready !== null && !this.isStopped) this.joinAll.request();
  }
  setChannels(channels: ExpectedGroup[]): void { this.setExpected(channels); }
  expected(): ExpectedGroup[] {
    const byId = new Map<string, ExpectedGroup>();
    for (const g of [...this.expectedList, ...this.opened.values()]) byId.set(toHex(g.groupId), g);
    return [...byId.values()];
  }
  addExpected(g: ExpectedGroup): void { this.opened.set(toHex(g.groupId), g); }
  row(g: Id): GroupInfo | undefined { return this.deps.core.groupRow(g) ?? undefined; }
  snapshot(g: Id): ApplyResult | null {
    const r = this.row(g);
    return r === undefined ? null : {
      state: r.state, epoch: r.epoch, nextSeq: r.nextSeq, newSeqs: [], proposalsPending: r.proposalsPending,
      epochChanged: false, ownAdopted: false,
    };
  }
  armTimer(ms: number, fn: () => void): number {
    const id = this.deps.setTimeout(() => { this.timers.delete(id); if (!this.isStopped) fn(); }, ms);
    this.timers.add(id);
    return id;
  }
  cancelTimer(id: number): void { this.deps.clearTimeout(id); this.timers.delete(id); }
  private onEvent(e: GatewayEvent): void {
    if (this.isStopped) return;
    if (e.type === 'ready') {
      this.ready = e.info; this.quiet.clear(); this.resyncTried.clear();
      this.healBlocked.clear();
      const readyGroups = this.deps.core.groups();
      for (const g of readyGroups) if (g.state === 2) this.healBlocked.add(toHex(g.groupId));
      for (const g of e.info.groups) if (g.proposalsOutstanding > 0 && this.row(g.groupId)?.state === 2) this.armVolunteer(g.groupId);
      void this.queues.run('w', async () => {
        try { await welcomeStep(this); } catch { /* later ready retries */ }
        if (this.isStopped) return;
        for (const g of readyGroups) {
          if (g.state === 2) {
            try {
              await this.ensureGroupHealed(g, e.info.generation);
              this.healBlocked.delete(toHex(g.groupId));
              if (!this.isStopped && this.row(g.groupId)?.state === 2) {
                this.requestCatchUp(g.groupId); this.requestDrain(g.groupId);
              }
            } catch (err) {
              if (err instanceof DillaHttpError && err.status === 404 && !this.isStopped) this.requestCatchUp(g.groupId);
              // A 404 catch-up can still identify a removed member; every other failed repair holds this group until next ready.
            }
          }
          else if (g.state === 3 && !this.resyncTried.has(toHex(g.groupId))) {
            this.queues.coalesce(key(g.groupId), 'resync', async () => { await this.resync(g.groupId); });
          }
        }
        if (!this.isStopped) this.joinAll.request();
      }).catch(() => undefined);
      return;
    }
    if (e.type !== 'frame') return;
    const f = e.frame; const g = f.groupId;
    if (g === null || f.op === Op.epochChanged) return;
    if (f.op === Op.commitNeeded) {
      let round: bigint;
      try { round = u64(f.payload[3] ?? null); } catch { return; }
      if (this.row(g)?.state !== 2) return;
      this.deps.gateway.commitAck(g, round); this.disarmVolunteer(g); this.quiet.delete(toHex(g)); this.requestCommit(g, 0);
    } else if (f.op === Op.handshake || f.op === Op.messageCt) {
      void this.queues.run(key(g), () => this.frameJob(f)).catch(() => undefined);
    } else if (f.op === Op.messageDeleted) {
      void this.queues.run(key(g), () => this.deletedJob(f)).catch(() => undefined);
    } else if (f.op === Op.welcome) {
      void this.queues.run('w', () => welcomeFrame(this, f)).catch(() => undefined);
    }
  }
  private async ensureGroupHealed(group: GroupInfo, generation: bigint): Promise<void> {
    const status = await this.deps.routes.getGroupHealState(group.groupId);
    if (status.generation !== generation) throw new Error('E_SYNC_HEAL_GENERATION');
    if (!status.needsHeal) return;
    const snapshot = this.deps.core.groupHealSnapshot(group.groupId);
    const parts = arr(decode(snapshot), 3);
    bin(parts[0]); arr(parts[1]); bin(parts[2]);
    const result = await this.deps.routes.postGroupHeal(group.groupId, snapshot);
    const current = this.row(group.groupId);
    if (current?.state !== 2 || result.epoch !== current.epoch || result.nextSeq !== current.nextSeq) throw new Error('E_SYNC_HEAL_MISMATCH');
    const confirmed = await this.deps.routes.getGroupHealState(group.groupId);
    if (confirmed.generation !== generation || confirmed.needsHeal || confirmed.epoch !== result.epoch || confirmed.nextSeq !== result.nextSeq) {
      throw new Error('E_SYNC_HEAL_MISMATCH');
    }
  }
  private async frameJob(f: Frame): Promise<void> {
    const g = f.groupId;
    if (g === null || this.isStopped) return;
    const r = this.row(g);
    if (r === undefined || r.state === 3 || r.state === 4) return;
    if (r.state === 0 || r.state === 1) {
      const h = this.held.get(toHex(g)) ?? []; h.push(f); if (h.length > HELD_MAX) h.shift(); this.held.set(toHex(g), h); return;
    }
    let seq: bigint; let hs: Uint8Array; let ms: Uint8Array;
    try {
      if (f.op === Op.handshake) {
        const p = arr(f.payload, 5); seq = u64(p[0]); u64(p[1]); u64(p[2]);
        if (p[3] !== null) u64(p[3]);
        bin(p[4]); hs = encode([p]); ms = encode([]);
      } else {
        // web3.7: a 7th element is the upload class (0, 1 or null), carried as the row's 9th; six elements (a web3.6
        // host) keep the 8-element row.
        const p = arr(f.payload); if (p.length !== 6 && p.length !== 7) throw new CborError('E_CBOR_SHAPE', 'message.ct has 6 or 7 elements');
        seq = u64(p[0]); u64(p[1]); bin(p[2], 16); bin(p[3]); bin(p[4], 32); u64(p[5]);
        const row: CborInput[] = [p[0]!, p[1]!, p[2]!, p[3]!, null, p[4]!, p[5]!, 0];
        if (p.length === 7) {
          if (p[6] !== null && p[6] !== 0n && p[6] !== 1n) throw new CborError('E_CBOR_SHAPE', 'class is 0, 1 or null');
          row.push(p[6] ?? null);
        }
        hs = encode([]); ms = encode([row]);
      }
    } catch { this.requestCatchUp(g); return; }
    if (seq < r.nextSeq) return;
    if (seq > r.nextSeq) { await this.catchUpNow(g, 'catch-up'); return; }
    try {
      const result = this.deps.core.groupApply(g, hs, ms, seq);
      await this.applyHook(g, result, 'frame');
      if (result.state === 2) this.armCursor(g);
    } catch (e) {
      if (e instanceof CoreError) { this.reportCurrent(g); this.requestCatchUp(g); }
      else throw e;
    }
  }
  private async deletedJob(f: Frame): Promise<void> {
    await Promise.resolve();
    const g = f.groupId; if (g === null || this.isStopped) return;
    let seq: bigint;
    try { const p = arr(f.payload, 2); seq = u64(p[0]); u64(p[1]); } catch { return; }
    const row = this.row(g); if (row === undefined || row.state === 4) return;
    try {
      const result = this.deps.core.messageDeleted(g, seq);
      if (result.newSeqs.length > 0) this.deps.onGroupChanged(g, result);
    } catch (e) { if (e instanceof CoreError) this.reportCurrent(g); else throw e; }
  }
  private armCursor(g: Id): void {
    const hex = toHex(g); if (this.cursorTimer.has(hex)) return;
    this.cursorTimer.set(hex, this.armTimer(SYNC.cursorDebounceMs, () => {
      this.cursorTimer.delete(hex);
      void this.queues.run(key(g), () => this.cursorStep(g)).catch(() => undefined);
    }));
  }
  /** A core refusal may have atomically quarantined earlier text before returning the error. */
  private reportCurrent(g: Id): void {
    const current = this.snapshot(g);
    if (current !== null) this.deps.onGroupChanged(g, current);
  }
  private disarmCursor(g: Id): void {
    const h = toHex(g); const id = this.cursorTimer.get(h); if (id !== undefined) this.cancelTimer(id); this.cursorTimer.delete(h);
  }
  private async cursorStep(g: Id): Promise<void> {
    if (this.isStopped || this.row(g)?.state !== 2) return;
    try {
      const body = this.deps.core.cursorBody(g); if (body === null) return;
      const p = arr(decode(body), 2);
      await this.deps.routes.postCursor(g, body);
      if (!this.isStopped) this.deps.core.cursorAcked(g, u64(p[0]), u64(p[1]));
    } catch { /* next catch-up retries */ }
  }
  /**
   * Catches the group up to what the server holds. Resolves true only when it read the group to the end
   * with the group still active, so the caller may conclude that a row the server stored would have been
   * seen; false when it stopped early, met a refusal (HTTP layer retries spent, network down), or ended
   * in a resync, which restarts above the resync commit and can never see an earlier row (TS-01).
   */
  async catchUpNow(g: Id, source: 'catch-up' | 'commit'): Promise<boolean> {
    this.disarmCursor(g);
    let carry: bigint | null = null;
    try {
      for (;;) {
        if (this.isStopped) return false;
        const r = this.row(g); if (r?.state !== 2) return false;
        const from = r.nextSeq;
        const ms = await this.deps.routes.getMessages(g, from, SYNC.messagePage);
        if (this.isStopped) return false;
        const hs = await this.deps.routes.getHandshakes(g, from, SYNC.handshakePage);
        if (this.isStopped) return false;
        const through = throughOf(ms, hs, carry);
        if (through >= from) await this.applyHook(g, this.deps.core.groupApply(g, hs.raw, ms.raw, through), source);
        carry = hs.lastSeq;
        if (this.row(g)?.state !== 2) return false;
        if (ms.count < SYNC.messagePage && hs.count < SYNC.handshakePage && !(hs.lastSeq !== null && hs.lastSeq > through)) break;
      }
      // The DS answered every read of the group: this device holds its leaf, whatever an earlier refusal reported.
      if (this.lostReported.has(toHex(g))) this.reportMembership(g, 'member');
      await this.cursorStep(g);
      return true;
    } catch (e) {
      if (e instanceof CoreError) this.reportCurrent(g);
      if ((this.membershipLost(g, e) || (e instanceof DillaHttpError && e.status === 410 && e.code === 'E_PRUNED'))
        && !this.resyncTried.has(toHex(g))) {
        await this.resync(g);
      } else if (this.membershipLost(g, e)) {
        this.relost(g);
      }
      return false;
    }
  }
  /** A group read answering 404 E_NOT_FOUND for an active group: the device holds no leaf any more (a kick
   *  or a removal it never saw; protocol/02 invariant 5), which only an external join can repair. */
  private membershipLost(g: Id, e: unknown): boolean {
    return e instanceof DillaHttpError && e.status === 404 && e.code === 'E_NOT_FOUND' && this.row(g)?.state === 2;
  }
  async applyHook(g: Id, r: ApplyResult, source: 'frame' | 'catch-up' | 'commit'): Promise<void> {
    const group = this.row(g);
    if (group?.kind === 0 && group.communityId !== null) await recoverModerationRemovals(this.deps.core, this.deps.routes, g, this.deps.onError);
    this.deps.onGroupChanged(g, r);
    const hex = toHex(g);
    if (r.ownAdopted) {
      const rows = this.deps.core.outbox(g);
      for (const id of this.count425.keys()) if (!rows.some((x) => toHex(x.msgId) === id)) this.count425.delete(id);
      for (const id of this.reframedOnce) {
        if (id.startsWith(`${hex}:`) && !rows.some((x) => `${hex}:${toHex(x.msgId)}` === id && x.state !== 2)) this.reframedOnce.delete(id);
      }
      this.deps.onOutboxChanged(g);
      const wait = this.echoWait.get(hex);
      if (wait !== undefined && !rows.some((x) => toHex(x.msgId) === wait.msgHex && x.state === 1)) {
        this.cancelTimer(wait.timer); this.echoWait.delete(hex); this.requestDrain(g);
      }
    }
    if (r.epochChanged) {
      const id = this.epochWait.get(hex);
      if (id !== undefined) { this.cancelTimer(id); this.epochWait.delete(hex); this.requestDrain(g); }
      else if (!this.echoWait.has(hex) && this.deps.core.outbox(g).some((x) => x.state === 0)) this.requestDrain(g);
    }
    if (r.state === 3 && !this.resyncTried.has(hex)) await this.resync(g);
    if (source !== 'commit' && r.state === 2) {
      if (r.proposalsPending > 0) this.armVolunteer(g); else this.disarmVolunteer(g);
    }
  }
  requestCatchUp(g: Id): void { if (!this.isStopped) this.queues.coalesce(key(g), 'catch-up', async () => { await this.catchUpNow(g, 'catch-up'); }); }
  requestDrain(g: Id): void { if (!this.isStopped && !this.healBlocked.has(toHex(g))) this.queues.coalesce(key(g), 'drain', () => drainOne(this, g)); }
  canSendInGroup(g: Id): boolean { return !this.healBlocked.has(toHex(g)); }
  requestCommit(g: Id, attempt: number): void {
    if (!this.isStopped) this.queues.coalesce(key(g), 'commit', async () => { await this.commitNow(g, attempt); });
  }
  async commitNow(g: Id, attempt: number): Promise<boolean> {
    const row = this.row(g); if (row?.state !== 2 || this.isStopped) return false;
    let built = false;
    try {
      const proposals = await this.deps.routes.getProposals(g);
      if (this.isStopped) return false;
      const active = arr(decode(proposals)).some((x) => u64(arr(x, 5)[4]) === 0n);
      if (!active && row.proposalsPending === 0) return false;
      for (const candidate of this.deps.core.proposedAdds(g, proposals)) {
        const { head, history } = await readDeviceListHistory(this.deps.routes, candidate.userId, candidate.cachedVersion);
        if (this.isStopped) return false;
        try {
          ingestAccountKind(this.deps.core, candidate.userId, head, this.deps.onError);
          this.deps.core.deviceListIngestAdd(g, proposals, candidate.index, candidate.userId, history, signedHeadList(head), BigInt(Math.floor(this.deps.now() / 1000)));
        } catch (e) {
          // Controller ruling 3: a candidate whose leaf fails is left out of this commit by the core (commit_build), never
          // a refusal that stalls this device's commits. A stale or unreadable history still refuses: the next attempt
          // fetches it again.
          if (!(e instanceof CoreError) || !FAILED_LEAF.has(e.code)) throw e;
        }
        this.deps.onTrustChanged();
      }
      const body = this.deps.core.commitBuild(g, proposals); built = true;
      await this.deps.routes.postCommit(g, body);
      if (this.isStopped) return false;
      const result = this.deps.core.commitConfirm(g);
      await this.applyHook(g, result, 'commit');
      this.disarmVolunteer(g); this.quiet.delete(toHex(g)); this.requestDrain(g);
      return true;
    } catch (e) {
      // Re-review N-m4: only a commit this attempt built is aborted. A core refusal before the build (commit_build's "a
      // commit is pending" while a call rotation's accepted commit awaits its echo) leaves that pending commit alone:
      // aborting it would make its echo a stranger's commit and end the call as removed.
      if (built) {
        try { this.deps.core.commitAbort(g); } catch { /* no pending state */ }
      }
      if (e instanceof DillaHttpError) {
        if (e.status === 409 && e.code === 'E_COMMIT_CONFLICT') {
          await this.catchUpNow(g, 'commit');
          if ((this.row(g)?.proposalsPending ?? 0) > 0) {
            if (attempt < SYNC.commitRetryMax) this.armTimer(this.deps.random() * SYNC.commitJitterMs, () => this.requestCommit(g, attempt + 1));
            else this.quiet.add(toHex(g));
          }
        } else if (e.status === 425 || e.status === 422) {
          await this.catchUpNow(g, 'commit');
          if (e.status === 422) this.quiet.add(toHex(g));
        } else if (((e.status === 403 && e.code === 'E_LEAF_NOT_CURRENT') || this.membershipLost(g, e)) && !this.resyncTried.has(toHex(g))) {
          await this.resync(g);
        } else if (this.membershipLost(g, e)) {
          this.relost(g);
        }
      }
      this.requestDrain(g);
      return false;
    }
  }
  /** A 404 on a group whose resync was already tried (it is not tried again before the next ready): reported not-member
   *  again unless that is still the last report, since nothing else would say it once a confirmed read withdrew it. */
  private relost(g: Id): void {
    if (!this.lostReported.has(toHex(g))) this.reportMembership(g, 'not-member');
  }
  private armVolunteer(g: Id): void {
    const hex = toHex(g); if (this.volunteer.has(hex) || this.quiet.has(hex)) return;
    const id = this.armTimer((this.ready?.backoffMs ?? 300) + this.deps.random() * (this.ready?.backoffJitterMs ?? 300), () => {
      this.volunteer.delete(hex); this.requestCommit(g, 0);
    });
    this.volunteer.set(hex, id);
  }
  private disarmVolunteer(g: Id): void {
    const hex = toHex(g); const id = this.volunteer.get(hex); if (id !== undefined) this.cancelTimer(id); this.volunteer.delete(hex);
  }
  async activate(g: Id): Promise<void> {
    const snap = this.snapshot(g); if (snap !== null) this.deps.onGroupChanged(g, snap);
    const frames = this.held.get(toHex(g)) ?? []; this.held.delete(toHex(g));
    const ordered: { frame: Frame; seq: bigint }[] = [];
    for (const frame of frames) {
      try { ordered.push({ frame, seq: u64(frame.payload[0] ?? null) }); }
      catch { this.requestCatchUp(g); }
    }
    ordered.sort((a, b) => a.seq < b.seq ? -1 : a.seq > b.seq ? 1 : 0);
    for (const { frame } of ordered) await this.frameJob(frame);
    this.requestCatchUp(g); this.requestDrain(g);
  }
  /**
   * The engine's resync of a group whose state is lost. A call group (kind 1) is never re-joined here: its join is the
   * call controller's alone (the gated join, L-TS-56 step 1), so a lost call group is reported as not-member and the call
   * ends as removed (task 6 review M6).
   */
  private async resync(g: Id): Promise<boolean> {
    if (this.row(g)?.kind === 1) {
      this.resyncTried.add(toHex(g));
      this.reportMembership(g, 'not-member');
      return false;
    }
    const resynced = await resyncGroup(this, g, false);
    if (resynced) this.healBlocked.delete(toHex(g));
    return resynced;
  }
  /**
   * The account joined the community again (web3.4: a kick, a lifted ban or a leave, then a join in the same tab). A
   * resync is tried once per ready, and one refused while the account was out stood as not-member until the next ready (a
   * reload), so the community's text groups may resync again from here: each one last reported not-member, or still in
   * state 3, resyncs at once on its lane (also after a refusal that was in flight meanwhile); a loss no read has met yet
   * resyncs when one does.
   */
  rejoined(communityId: Id): void {
    if (this.isStopped) return;
    const community = toHex(communityId);
    const lost = (g: Id): boolean => {
      const row = this.row(g);
      return row?.state === 3 || (row?.state === 2 && this.lostReported.has(toHex(g)));
    };
    for (const row of this.deps.core.groups()) {
      if (row.kind !== 0 || row.communityId === null || toHex(row.communityId) !== community) continue;
      const g = row.groupId;
      this.resyncTried.delete(toHex(g));
      if (!lost(g)) continue;
      this.queues.coalesce(key(g), 'resync', async () => {
        if (lost(g) && await resyncGroup(this, g, true)) this.healBlocked.delete(toHex(g));
      });
    }
  }
  /** Runs a job on the group's lane, serialised with every apply, catch-up and commit of that group (review M7). */
  runOnGroup<T>(g: Id, job: () => Promise<T>): Promise<T> { return this.queues.run(key(g), job); }
  async pollWelcomes(): Promise<void> { await this.queues.run('w', () => welcomeStep(this)).catch(() => undefined); }
  openChannel(ch: { communityId: Id | null; channelId: Id; textGroupId: Id | null }): Promise<{ groupId: Id; state: 0 | 1 | 2 | 3 | 4 }> {
    if (this.isStopped) return Promise.reject(new SyncError('E_SYNC_STOPPED'));
    return this.queues.run(`c:${toHex(ch.channelId)}`, async () => {
      const opened = await openChannelFlow(this, ch);
      const group = this.row(opened.groupId);
      if (opened.state === 2 && group?.kind === 0 && group.communityId !== null && !this.isStopped) {
        let observedGeneration = 0n;
        try {
          await this.queues.run(key(opened.groupId), () => {
            if (this.isStopped) throw new SyncError('E_SYNC_STOPPED');
            observedGeneration = this.recoveryGeneration.get(toHex(opened.groupId)) ?? 0n;
            return recoverModerationRemovals(this.deps.core, this.deps.routes, opened.groupId, this.deps.onError);
          });
        } catch (e) {
          // A device can rejoin the community before its background catch-up has noticed the
          // Remove. The group row is still state 2, but the member-only removal feed refuses it
          // with the same 404 as messages and handshakes. Resync this channel once on its lane,
          // then read the feed again before allowing the caller to publish the timeline.
          if (!this.membershipLost(opened.groupId, e)) throw e;
          const resynced = await this.queues.run(key(opened.groupId), async () => {
            if (this.isStopped) throw new SyncError('E_SYNC_STOPPED');
            // A proposal/catch-up already on this lane may have replaced our
            // leaf after the failed read. Its 404 then describes an old local
            // generation; retry the read without another cursor-moving join.
            if ((this.recoveryGeneration.get(toHex(opened.groupId)) ?? 0n) !== observedGeneration) return true;
            const joined = await resyncGroup(this, opened.groupId, true);
            if (joined) this.healBlocked.delete(toHex(opened.groupId));
            return joined;
          });
          if (!resynced) throw e;
          await this.queues.run(key(opened.groupId), () => {
            if (this.isStopped) throw new SyncError('E_SYNC_STOPPED');
            return recoverModerationRemovals(this.deps.core, this.deps.routes, opened.groupId, this.deps.onError);
          });
        }
      }
      return opened;
    });
  }
  sendRequest(g: Id, request: SendRequest): Id { return sendRequest(this, g, request); }
  send(g: Id, text: string): Id { return sendMessage(this, g, text); }
  retry(msgId: Id): void { retrySend(this, msgId); }
  discard(msgId: Id): void { discardSend(this, msgId); }
}
export { SyncError } from './errors';
