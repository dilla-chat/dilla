import { arr, bin, decode, encode, u64, type CborValue } from '../cbor';
import { CoreError, type ExpectedGroup, type Id, type WelcomeOutcome } from '../core-port';
import type { Frame } from '../gateway/frames';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { SYNC } from './engine';
import { SyncError } from './errors';
import type { SyncInternals } from './internals';

const groupKey = (g: Id): string => `g:${toHex(g)}`;
const same = (a: Id, b: Id): boolean => toHex(a) === toHex(b);
function reportUnexpected(s: SyncInternals, group: Id): void {
  const hex = toHex(group);
  if (s.unexpectedReported.has(hex)) return;
  s.unexpectedReported.add(hex);
  s.deps.onUnexpectedWelcome(group);
}

async function applyWelcomeRows(s: SyncInternals, raw: Uint8Array, rows: CborValue[], expected: ExpectedGroup[]): Promise<void> {
  const expectedGroups = new Set(expected.map((item) => toHex(item.groupId)));
  const genericRows: CborValue[] = [];
  for (const row of rows) {
    const fields = arr(row, 7);
    const welcomeId = u64(fields[0]);
    const group = bin(fields[1], 16);
    if (expectedGroups.has(toHex(group))) {
      genericRows.push(row);
      continue;
    }

    let proof: { payload: Uint8Array; signature: Uint8Array };
    try {
      proof = await s.deps.routes.getInteractionAdmissionContext(group);
    } catch {
      // Ordinary unexpected Welcomes and unavailable/stale interaction proofs both stay on the
      // existing unexpected path. Its outcome 3 preserves the row and cannot admit the group.
      genericRows.push(row);
      continue;
    }
    if (s.stopped()) return;

    let outcomes: WelcomeOutcome[];
    try {
      const now = BigInt(Math.floor(s.deps.now() / 1000));
      outcomes = s.deps.core.humanInteractionWelcomeApply(encode([row]), encode([proof.payload, proof.signature]), now);
    } catch {
      // A successful proof lookup identifies a kind-3 candidate. Any local verification or
      // storage refusal leaves the Welcome queued for recovery; generic apply could delete it.
      reportUnexpected(s, group);
      continue;
    }
    const outcome = outcomes[0];
    if (outcomes.length !== 1 || outcome?.welcomeId !== welcomeId || !same(outcome.groupId, group)
      || (outcome.outcome !== 0 && outcome.outcome !== 1)) {
      reportUnexpected(s, group);
      continue;
    }

    try { await s.deps.routes.deleteWelcome(welcomeId); } catch { /* accepted receipt makes the next poll safe */ }
    if (outcome.outcome === 0 && !s.stopped()) void s.queues.run(groupKey(group), () => s.activate(group)).catch(() => undefined);
  }

  if (genericRows.length > 0) {
    const body = genericRows.length === rows.length ? raw : encode(genericRows);
    await judgeWelcomes(s, s.deps.core.welcomesApply(body, expected));
  }
}

export async function judgeWelcomes(s: SyncInternals, outcomes: WelcomeOutcome[]): Promise<void> {
  for (const o of outcomes) {
    if (o.outcome === 3) {
      // Once per group until the expected set changes (CORE-ENGINE-01): a kept Welcome is served on every poll.
      reportUnexpected(s, o.groupId);
      continue;
    }
    if (o.outcome === 1 && s.row(o.groupId)?.state !== 2) continue;
    try { await s.deps.routes.deleteWelcome(o.welcomeId); } catch { /* later poll can retry */ }
    if (o.outcome === 0) void s.queues.run(groupKey(o.groupId), () => s.activate(o.groupId)).catch(() => undefined);
  }
}
export async function welcomeStep(s: SyncInternals): Promise<void> {
  let after = 0n;
  try {
    for (;;) {
      if (s.stopped()) return;
      const page = await s.deps.routes.getWelcomes(after);
      if (page.count === 0) return;
      await applyWelcomeRows(s, page.raw, arr(decode(page.raw)), s.expected());
      if (page.lastId === null || page.count < 64) return;
      after = page.lastId;
    }
  } catch { /* next ready or open retries */ }
}
export async function welcomeFrame(s: SyncInternals, f: Frame): Promise<void> {
  const g = f.groupId; if (g === null || s.stopped()) return;
  try {
    const p = arr(f.payload, 6);
    const id = u64(p[0]); const epoch = u64(p[1]); const seq = u64(p[2]);
    const blob = bin(p[3]); const tree = bin(p[4]); const hash = bin(p[5], 32);
    const body = encode([[id, g, epoch, seq, blob, tree, hash]]);
    await applyWelcomeRows(s, body, arr(decode(body)), s.expected());
  } catch (e) { if (!(e instanceof CoreError)) return; }
}
export async function externalJoin(s: SyncInternals, eg: ExpectedGroup, budget: { joins: number }): Promise<void> {
  for (;;) {
    if (s.stopped()) throw new SyncError('E_SYNC_STOPPED');
    const info = await s.deps.routes.getGroupInfo(eg.groupId);
    const tree = await s.deps.routes.getGroupTree(eg.groupId);
    let body: Uint8Array;
    try { body = s.deps.core.groupJoinExternal(eg, info, tree); }
    catch (e) {
      if (e instanceof CoreError && e.code === 'E_CORE_INPUT') {
        budget.joins += 1;
        if (budget.joins < SYNC.commitRetryMax) continue;
      }
      throw e;
    }
    budget.joins += 1;
    let answer: { seq: bigint; epoch: bigint };
    try {
      answer = await s.deps.routes.postResync(eg.groupId, body);
    } catch (e) {
      if (e instanceof DillaHttpError && s.row(eg.groupId)?.state === 1) s.deps.core.groupDiscard(eg.groupId);
      throw e;
    }
    s.deps.core.groupJoined(eg.groupId, answer.seq);
    const hex = toHex(eg.groupId);
    s.recoveryGeneration.set(hex, (s.recoveryGeneration.get(hex) ?? 0n) + 1n);
    await s.activate(eg.groupId);
    return;
  }
}
export async function resyncGroup(s: SyncInternals, g: Id, fromOpen: boolean): Promise<boolean> {
  const hex = toHex(g);
  if (!fromOpen && s.resyncTried.has(hex)) return false;
  const row = s.row(g); if (row === undefined) return false;
  s.resyncTried.add(hex);
  s.reportMembership(g, 'resyncing');
  try {
    await externalJoin(s, { groupId: g, communityId: row.communityId, channelId: row.targetId, policyVersion: s.deps.instance.policyVersion }, { joins: 0 });
    return true;
  } catch (e) {
    // A refusal before the join leaves an active row in state 2; it is never discarded (group_discard of
    // an active row deletes its history), and the not-member report below is what the page shows.
    if (s.row(g)?.state === 1) s.deps.core.groupDiscard(g);
    const snap = s.snapshot(g); if (snap !== null) s.deps.onGroupChanged(g, snap);
    s.reportMembership(g, 'not-member');
    if (row.state === 2) failOutbox(s, g, codeOf(e));
    return false;
  }
}
function codeOf(e: unknown): string {
  return e instanceof DillaHttpError || e instanceof CoreError || e instanceof SyncError ? e.code : 'E_INTERNAL';
}
/** A refused resync of a group that was active: its queued and in-flight messages will not leave, so they
 *  fail (the page offers retry and discard) instead of showing "sending…" for ever. */
function failOutbox(s: SyncInternals, g: Id, code: string): void {
  const hex = toHex(g);
  const echo = s.echoWait.get(hex); if (echo !== undefined) { s.cancelTimer(echo.timer); s.echoWait.delete(hex); }
  const epoch = s.epochWait.get(hex); if (epoch !== undefined) { s.cancelTimer(epoch); s.epochWait.delete(hex); }
  const slow = s.slowWait.get(hex); if (slow !== undefined) { s.cancelTimer(slow.timer); s.slowWait.delete(hex); }
  let changed = false;
  for (const r of s.deps.core.outbox(g)) {
    if (r.state === 2) continue;
    s.deps.core.sendFail(r.msgId, code); s.count425.delete(toHex(r.msgId)); s.slowed.delete(toHex(r.msgId));
    s.reframedOnce.delete(`${hex}:${toHex(r.msgId)}`); changed = true;
  }
  if (changed) s.deps.onOutboxChanged(g);
}
export async function openChannelFlow(
  s: SyncInternals, ch: { communityId: Id | null; channelId: Id; textGroupId: Id | null },
): Promise<{ groupId: Id; state: 0 | 1 | 2 | 3 | 4 }> {
  let textGroupId = ch.textGroupId;
  const budget = { joins: 0 };
  let registrations = 0;
  if (ch.communityId === null && textGroupId === null) textGroupId = await dmGroupOf(s, ch.channelId);
  if (textGroupId !== null) s.addExpected({ groupId: textGroupId, communityId: ch.communityId, channelId: ch.channelId, policyVersion: s.deps.instance.policyVersion });
  for (;;) {
    if (s.stopped()) throw new SyncError('E_SYNC_STOPPED');
    const local = s.deps.core.groups().find((r) => same(r.targetId, ch.channelId) && r.kind === 0 && r.state !== 4);
    if (local !== undefined) {
      if (local.state === 2) return { groupId: local.groupId, state: 2 };
      // The state was read on the channel lane; a job on the group lane runs only after the job ahead of it
      // (a background resync or join in flight), so each job re-reads it and acts only if it still holds,
      // otherwise the loop looks again (state 2 returns, state 3 resyncs).
      const g = local.groupId;
      if (local.state === 3) {
        const ran = await s.queues.run(groupKey(g), async () => {
          if (s.row(g)?.state !== 3) return false;
          await resyncGroup(s, g, true);
          return true;
        });
        if (ran) return { groupId: g, state: s.row(g)?.state ?? 4 };
        continue;
      }
      // A row still in state 0 or 1 once the lane is free is a registration or join an earlier page never finished.
      await s.queues.run(groupKey(g), () => {
        const state = s.row(g)?.state;
        if (state === 0 || state === 1) s.deps.core.groupDiscard(g);
        return Promise.resolve();
      });
      continue;
    }
    if (textGroupId !== null) {
      await s.pollWelcomes();
      if (s.row(textGroupId)?.state === 2) return { groupId: textGroupId, state: 2 };
      const eg = { groupId: textGroupId, communityId: ch.communityId, channelId: ch.channelId, policyVersion: s.deps.instance.policyVersion };
      try {
        await s.queues.run(groupKey(textGroupId), async () => {
          if (s.row(eg.groupId)?.state === 2) return;
          await externalJoin(s, eg, budget);
        });
        return { groupId: textGroupId, state: 2 };
      } catch (e) {
        if (e instanceof DillaHttpError && e.status === 425 && e.code === 'E_COMMIT_REQUIRED' && budget.joins < SYNC.commitRetryMax) {
          // Defensive: a later delivery service may propose Adds at community join; today's join uses an external commit.
          await s.wait(SYNC.membershipWaitMs);
          if (s.stopped()) throw new SyncError('E_SYNC_STOPPED');
          continue;
        }
        if (e instanceof DillaHttpError && e.status === 409 && e.code === 'E_COMMIT_CONFLICT' && budget.joins < SYNC.commitRetryMax) continue;
        throw e;
      }
    }
    if (registrations >= SYNC.registerRetryMax) throw new SyncError('E_REGISTER_RACE');
    registrations += 1;
    const g = crypto.getRandomValues(new Uint8Array(16));
    try {
      await s.queues.run(groupKey(g), async () => {
        const body = s.deps.core.groupCreate(g, ch.communityId, ch.channelId, s.deps.instance.policyVersion, s.deps.instance.externalSenderPub);
        let result: { nextSeq: bigint };
        try { result = await s.deps.routes.postGroup(body); }
        catch (e) { if (s.row(g)?.state === 0) s.deps.core.groupDiscard(g); throw e; }
        s.deps.core.groupRegistered(g, result.nextSeq);
        await s.activate(g);
      });
      return { groupId: g, state: 2 };
    } catch (e) {
      if (!(e instanceof DillaHttpError && e.status === 409 && e.code === 'E_GROUP_EXISTS')) throw e;
      if (ch.communityId === null) textGroupId = await dmGroupOf(s, ch.channelId);
      else {
        const rows = await s.deps.routes.listChannels(ch.communityId);
        const channel = rows.find((r) => same(r.id, ch.channelId));
        if (channel === undefined) throw new SyncError('E_CHANNEL_GONE');
        textGroupId = channel.textGroupId;
      }
      if (textGroupId !== null) s.addExpected({ groupId: textGroupId, communityId: ch.communityId, channelId: ch.channelId, policyVersion: s.deps.instance.policyVersion });
    }
  }
}

async function dmGroupOf(s: SyncInternals, channelId: Id): Promise<Id | null> {
  try { return (await s.deps.routes.getChannel(channelId)).textGroupId; }
  catch (e) {
    if (e instanceof DillaHttpError && e.status === 404 && e.code === 'E_NOT_FOUND') throw new SyncError('E_CHANNEL_GONE');
    throw e;
  }
}
