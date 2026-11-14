// The join of a call (L-TS-56 step 1), modelled on sync/channel.ts externalJoin: find the channel's call group
// (L-HTTP-100), register one or join it by external commit, settle pending leaves before anything is posted (RV-1),
// derive the join epoch's key through the core's gate, and start the call. Nothing is posted for a tree the gate refuses,
// and the join commit is posted only after call_join_check passes.
import { CoreError, isTrustBlock, type CallMediaEpoch, type CorePort, type ExpectedGroup, type Id } from '../core-port';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import type { CallStarted, ChannelCall, Routes } from '../http/routes';

export type JoinRoutes = Pick<Routes, 'getChannelCall' | 'postGroup' | 'getGroupInfo' | 'getGroupTree' | 'postResync' | 'startCall'>;

export interface JoinDeps {
  core: CorePort;
  routes: JoinRoutes;
  policyVersion: bigint;
  externalSenderPub: Uint8Array;
  randomId(): Id;
  /** The worker's trust refresh of the group's leaves (bounded by WAIT_LIST_MS); it never throws. */
  refreshLeaves(groupId: Id): Promise<void>;
  /** Resolves after `ms` on the worker's clock (the rejoin's wait for its own Remove). */
  wait(ms: number): Promise<void>;
  /** A monotonic clock (ms; performance.now in the worker, task-10 fix review N2): the rejoin's wait is bounded by
   *  elapsed time, not by a count of its steps, and a wall-clock step neither stretches nor cuts it. */
  now(): number;
  /** True once the join was cancelled (a leave, sign-out or revocation): a wait stops there. */
  cancelled(): boolean;
  /** The join has begun waiting for this device's earlier Remove (the page says so in its status region). */
  rejoining(): void;
}

/**
 * A rejoin while this device's own Remove is still outstanding (web-3 task 10, concern 2): the instance refuses the
 * resync of a Remove's target with 409 E_REMOVE_PENDING (R25's guard 1, kept whole; its own code since the fix round,
 * so every other refusal, a 403 included, fails at once), and the join waits until another member has committed the
 * Remove (the device then joins afresh) or the call has ended and its group closed (the device registers a fresh one).
 * Each step, REJOIN_STEP_MS apart, reads the call and the group's GroupInfo (two requests: at most one a second) and
 * tries the join again only when either has changed; the wait ends REJOIN_WAIT_MS after the first refusal, on the
 * worker's clock, which outlasts the room's departure timeout (20 s) and its webhook. A person alone ends the call as
 * they leave (controller.leaveLive), so only a leave that could not (a page that went away) waits here that long.
 */
export const REJOIN_STEP_MS = 2_000;
export const REJOIN_WAIT_MS = 30_000;
/** The most reads of a call group's GroupInfo and tree (a pair each) one external join makes when the core finds the
 *  pair disagreeing (a commit landed between the two reads). */
export const JOIN_READS_MAX = 3;

export interface JoinTarget { channelId: Id; communityId: Id | null; vdec: string }
export interface Joined { groupId: Id; media: CallMediaEpoch; started: CallStarted }

/** A join that ended without a call. `leave` names an active call group this device must leave (a self-Remove and a
 *  forget); `cause` is the error to answer the command with. */
export class JoinRefused extends Error {
  /** The users the last call read named in the call (review M4: the only candidates of a changedUser). */
  users: string[] = [];
  /** WB client I4: `leave` is a group whose tree holds only this device (it registered it, or the join epoch's roster
   *  is its own leaf alone). Nobody can commit a self-Remove there, so the controller ends it conditionally instead. */
  constructor(readonly code: string, readonly phase: 'blocked' | 'failed', readonly cause: unknown, readonly leave: Id | null,
    readonly alone = false) {
    super(code);
    this.name = 'JoinRefused';
  }
}

/** Task-10 fix review N1: a join cancelled (a leave, sign-out or revocation) posts nothing more. `leave` is the group it
 *  already joined, if any (the controller sends its self-Remove). */
function stopped(leave: Id | null): JoinRefused {
  return new JoinRefused('E_NO_CALL', 'failed', new CoreError('E_NO_CALL', 'the join was cancelled'), leave);
}

const codeOf = (e: unknown): string => (e instanceof CoreError || e instanceof DillaHttpError ? e.code : 'E_INTERNAL');
const isHttp = (e: unknown, status: number, code: string): boolean => e instanceof DillaHttpError && e.status === status && e.code === code;
/** A gate refusal: a trust block (E_BINDING included) is `blocked`; anything else, a pending list included, `failed`. */
function refused(e: unknown, leave: Id | null): JoinRefused {
  const code = codeOf(e);
  return new JoinRefused(code, isTrustBlock(code) ? 'blocked' : 'failed', e, leave);
}

export async function joinCall(d: JoinDeps, t: JoinTarget): Promise<Joined> {
  const seen: { read: ChannelCall | null } = { read: null };
  try {
    return await joinLoop(d, t, seen);
  } catch (e) {
    if (e instanceof JoinRefused) e.users = [...new Set((seen.read?.participants ?? []).map((p) => toHex(p.userId)))];
    throw e;
  }
}

async function joinLoop(d: JoinDeps, t: JoinTarget, seen: { read: ChannelCall | null }): Promise<Joined> {
  let reread = { exists: true, closed: true, ended: true };
  let waitSince: number | null = null;
  let read: ChannelCall = await d.routes.getChannelCall(t.channelId);
  seen.read = read;
  for (;;) {
    seen.read = read;
    forgetStale(d.core, t.channelId, read.groupId);
    let groupId: Id;
    let created = false;
    if (read.groupId === null) {
      groupId = d.randomId();
      let body: Uint8Array;
      try { body = d.core.callGroupCreate(groupId, t.communityId, t.channelId, d.policyVersion, d.externalSenderPub); }
      catch (e) { throw refused(e, null); }
      if (d.cancelled()) { d.core.groupDiscard(groupId); throw stopped(null); }
      let nextSeq: bigint;
      try { ({ nextSeq } = await d.routes.postGroup(body)); }
      catch (e) {
        d.core.groupDiscard(groupId);
        // Another device registered the channel's call group first: read the call once more and join theirs.
        if (isHttp(e, 409, 'E_GROUP_EXISTS') && reread.exists) {
          reread = { ...reread, exists: false };
          read = await d.routes.getChannelCall(t.channelId);
          continue;
        }
        throw refused(e, null);
      }
      d.core.groupRegistered(groupId, nextSeq);
      created = true;
    } else {
      groupId = read.groupId;
      const tried: { info: Uint8Array | null } = { info: null };
      try {
        await joinExisting(d, t, groupId, tried);
      } catch (e) {
        // The call ended as this join was built (the last member's DELETE ?if_alone=1 closed its group): the instance
        // refuses the external commit for the closed group (409 E_GROUP_CLOSED). That group is final, so the call is read
        // once more and, when it names no group, a fresh one is registered through the same steps (and the same core
        // gate) as any first joiner's. Once only: a second refusal fails the join.
        if (e instanceof JoinRefused && isHttp(e.cause, 409, 'E_GROUP_CLOSED') && reread.ended && !d.cancelled()) {
          reread = { ...reread, ended: false };
          read = await d.routes.getChannelCall(t.channelId);
          continue;
        }
        if (!(e instanceof JoinRefused && isHttp(e.cause, 409, 'E_REMOVE_PENDING')) || tried.info === null || d.cancelled()) throw e;
        if (waitSince === null) { waitSince = d.now(); d.rejoining(); }
        read = await waitForRemove(d, t, e, waitSince, groupId, tried.info);
        continue;
      }
    }
    const media = await mediaEpoch(d, groupId);
    if (d.cancelled()) { new Uint8Array(media.baseKey).fill(0); throw stopped(groupId); }
    try {
      return { groupId, media, started: await d.routes.startCall(t.channelId, t.vdec) };
    } catch (e) {
      // Any failure zero-fills the held key and posts nothing.
      new Uint8Array(media.baseKey).fill(0);
      // L-HTTP-105: the start raced the idle close of the group; it is gone at the instance, so it is forgotten here and
      // the call is read once more (a fresh registration, or a newer group).
      if (isHttp(e, 503, 'E_UNAVAILABLE') && reread.closed) {
        reread = { ...reread, closed: false };
        d.core.callGroupForget(groupId);
        read = await d.routes.getChannelCall(t.channelId);
        continue;
      }
      const alone = created || (media.roster.length === 1 && media.roster[0]?.leaf === media.ownLeaf);
      throw new JoinRefused(codeOf(e), 'failed', e, groupId, alone);
    }
  }
}

/** The rejoin's wait (REJOIN_WAIT_MS from `since`): each step reads the call, and while it still names the refused
 *  group, its GroupInfo; it answers the call read once either has changed (the call ended or moved on, or a commit
 *  landed: perhaps the Remove), and throws `refusal` once the bound has passed or the join is cancelled. */
async function waitForRemove(d: JoinDeps, t: JoinTarget, refusal: JoinRefused, since: number, groupId: Id,
  info: Uint8Array): Promise<ChannelCall> {
  for (;;) {
    const left = since + REJOIN_WAIT_MS - d.now();
    if (left <= 0 || d.cancelled()) throw refusal;
    await d.wait(Math.min(REJOIN_STEP_MS, left));
    if (d.cancelled()) throw refusal;
    const read = await d.routes.getChannelCall(t.channelId);
    // N1: a leave during a read stops the wait there; the join goes no further.
    if (d.cancelled()) throw refusal;
    if (read.groupId === null || toHex(read.groupId) !== toHex(groupId)) return read;
    const now = await d.routes.getGroupInfo(groupId);
    if (d.cancelled()) throw refusal;
    if (toHex(now) !== toHex(info)) return read;
  }
}

/** The external join: a hard verdict refuses at the core's join and nothing is posted; a pending leaf is settled by one
 *  bounded list refresh, and the commit is posted only after call_join_check passes (RV-1). `tried.info` is the
 *  GroupInfo the commit was built on (the rejoin's wait compares it). A commit of another member can land at any step
 *  (bob committing the Remove a reloaded page's boot sweep proposed, as its rejoin starts): the reads and the post are
 *  repeated, at most JOIN_READS_MAX times, while the group has moved under them. */
async function joinExisting(d: JoinDeps, t: JoinTarget, groupId: Id, tried: { info: Uint8Array | null }): Promise<void> {
  const eg: ExpectedGroup = { groupId, communityId: t.communityId, channelId: t.channelId, policyVersion: d.policyVersion };
  for (let reads = 1; ; reads++) {
    const info = await d.routes.getGroupInfo(groupId);
    tried.info = info;
    const tree = await d.routes.getGroupTree(groupId);
    const again = (): boolean => reads < JOIN_READS_MAX && !d.cancelled();
    let body: Uint8Array;
    try {
      body = d.core.callGroupJoinExternal(eg, info, tree);
    } catch (e) {
      // The two reads are not one: a commit that lands between them (another member committing a Remove, a rotation)
      // serves a GroupInfo and a tree of different epochs, which the core refuses as E_CORE_INPUT before anything is
      // staged. Both are read again, a bounded number of times, as the text group's external join does.
      if (codeOf(e) === 'E_CORE_INPUT' && again()) continue;
      throw refused(e, null);
    }
    try {
      try { d.core.callJoinCheck(groupId); }
      catch (e) {
        if (codeOf(e) !== 'E_DEVICE_LIST_STALE') throw e;
        await d.refreshLeaves(groupId);
        d.core.callJoinCheck(groupId);
      }
    } catch (e) {
      discard(d.core, groupId);
      throw refused(e, null);
    }
    if (d.cancelled()) { discard(d.core, groupId); throw stopped(null); }
    let seq: bigint;
    try { ({ seq } = await d.routes.postResync(groupId, body)); }
    catch (e) {
      discard(d.core, groupId);
      // The same commit, landing after the tree read: the external commit was built on an epoch the group has left. A
      // refusal is final only for the group it was built on, so when GroupInfo has changed since (a commit, or any
      // proposal or message the group sequenced), both are read again.
      if (again() && isStaleCommit(e) && await groupMovedSince(d, groupId, info)) continue;
      throw refused(e, null);
    }
    d.core.groupJoined(groupId, seq);
    return;
  }
}

/** The instance's refusals of a commit built on an epoch it has left: dillad answers 422 E_COMMIT_INVALID (rule
 *  structural, its resync takes the epoch to commit against from the group itself), a lost race 409 E_COMMIT_CONFLICT.
 *  409 E_REMOVE_PENDING is neither: the rejoin's own wait takes it; nor is 409 E_GROUP_CLOSED, which joinLoop answers
 *  with a fresh registration. */
function isStaleCommit(e: unknown): boolean {
  return e instanceof DillaHttpError && ((e.status === 422 && e.code === 'E_COMMIT_INVALID') || (e.status === 409 && e.code === 'E_COMMIT_CONFLICT'));
}

/** True when the group's GroupInfo is no longer `info`: a commit landed after it was read, or anything else the group
 *  sequenced did (GET /info carries NextSeq, one space for proposals, messages and commits; a retry for such a change
 *  is bounded and harmless). A failed read says nothing. */
async function groupMovedSince(d: JoinDeps, groupId: Id, info: Uint8Array): Promise<boolean> {
  try { return toHex(await d.routes.getGroupInfo(groupId)) !== toHex(info); }
  catch { return false; }
}

/** The join epoch's key through the gate; a pending leaf is read once more, any refusal leaves the joined group. */
async function mediaEpoch(d: JoinDeps, groupId: Id): Promise<CallMediaEpoch> {
  try { return d.core.callMediaEpoch(groupId); }
  catch (e) {
    if (codeOf(e) !== 'E_DEVICE_LIST_STALE') throw refused(e, groupId);
  }
  await d.refreshLeaves(groupId);
  try { return d.core.callMediaEpoch(groupId); }
  catch (e) { throw refused(e, groupId); }
}

function discard(core: CorePort, groupId: Id): void {
  try { if (core.groupRow(groupId)?.state === 1) core.groupDiscard(groupId); } catch { /* nothing left to discard */ }
}

/** A call group of this channel that this device still holds, other than the one the instance names now, is gone at the
 *  instance (closed, or the call moved on): its state goes, so the core's one-group-per-channel rule lets the next in. */
function forgetStale(core: CorePort, channelId: Id, current: Id | null): void {
  const channel = toHex(channelId);
  for (const g of core.groups()) {
    if (g.kind !== 1 || g.state === 4 || toHex(g.targetId) !== channel || (current !== null && toHex(g.groupId) === toHex(current))) continue;
    try {
      if (g.state === 0 || g.state === 1) core.groupDiscard(g.groupId);
      else core.callGroupForget(g.groupId);
    } catch { /* the core refuses; the join below reports it */ }
  }
}
