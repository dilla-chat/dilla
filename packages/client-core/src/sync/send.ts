import { CoreError, type Id, type OutboxRow, type SendRequest } from '../core-port';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { resyncGroup } from './channel';
import { SYNC } from './engine';
import { LEAF_VERDICTS, SyncError } from './errors';
import type { SyncInternals } from './internals';

function owner(s: SyncInternals, msgId: Id): Id | undefined {
  const id = toHex(msgId);
  return s.deps.core.groups().find((g) => s.deps.core.outbox(g.groupId).some((x) => toHex(x.msgId) === id))?.groupId;
}
/** Queues any envelope type (L-TS-34): the core's refusal propagates synchronously and nothing else runs. */
export function sendRequest(s: SyncInternals, g: Id, request: SendRequest): Id {
  if (s.stopped()) throw new SyncError('E_SYNC_STOPPED');
  const id = s.deps.core.sendPrepare(g, request, BigInt(Math.floor(s.deps.now() / 1000)));
  s.queued.add(toHex(id));
  s.deps.onOutboxChanged(g); s.requestDrain(g); return id;
}
export function sendMessage(s: SyncInternals, g: Id, text: string): Id {
  return sendRequest(s, g, { type: 0, replyTo: null, body: text, attachments: [], threadId: null });
}
export function retrySend(s: SyncInternals, msgId: Id): void {
  const g = owner(s, msgId);
  const failed = g === undefined ? undefined : s.deps.core.outbox(g).find((x) => toHex(x.msgId) === toHex(msgId));
  s.deps.core.sendRetry(msgId);
  s.count425.delete(toHex(msgId)); s.slowed.delete(toHex(msgId));
  if (g === undefined) return;
  const key = `${toHex(g)}:${toHex(msgId)}`;
  s.reframedOnce.delete(key); s.trustRetried.delete(key);
  // A person's retry asks for the group's duties again, as a ready does: a membership refused earlier in
  // this ready is tried once more, and a commit-quiet group commits once more.
  s.resyncTried.delete(toHex(g)); s.quiet.delete(toHex(g));
  s.deps.onOutboxChanged(g);
  // A row a leaf verdict failed is sealed again only after the group's leaves were read again (I1); that refresh is
  // its one, so a second refusal fails it.
  if (failed !== undefined && LEAF_VERDICTS.has(failed.error)) { refreshTrustThenDrain(s, g, key); return; }
  s.requestDrain(g);
}
/** Holds the row (drainOne waits while the refresh is out), refreshes the group's trust once, then drains again. */
function refreshTrustThenDrain(s: SyncInternals, g: Id, key: string): void {
  s.trustRetried.set(key, 'pending');
  const job = s.deps.refreshGroupTrust(g).catch(() => undefined).then(() => {
    if (s.trustRetried.get(key) === 'pending') s.trustRetried.set(key, 'done');
    if (!s.stopped()) s.requestDrain(g);
  });
  // Not run in the group's lane: the refresh needs none, and catch-up and commits must not wait behind it.
  if (s.deps.trackJob !== undefined) s.deps.trackJob(job); else void job;
}
export function discardSend(s: SyncInternals, msgId: Id): void {
  const g = owner(s, msgId);
  // A message id no outbox holds calls nothing (web-1 pre-flight ruling 1(iv)); the core would refuse it E_CORE_NOT_FOUND.
  if (g === undefined) return;
  const row = s.deps.core.outbox(g).find((x) => toHex(x.msgId) === toHex(msgId));
  s.deps.core.sendDiscard(msgId);
  // The row's attachment references are the controller's to delete (L-TS-34).
  if (row !== undefined) s.deps.onDiscarded(g, row);
  s.reframedOnce.delete(`${toHex(g)}:${toHex(msgId)}`); s.trustRetried.delete(`${toHex(g)}:${toHex(msgId)}`);
  s.slowed.delete(toHex(msgId)); s.queued.delete(toHex(msgId));
  s.deps.onOutboxChanged(g);
}
async function afterLostResponse(s: SyncInternals, g: Id, msgId: Id): Promise<void> {
  await s.catchUpNow(g, 'catch-up');
  const row = s.deps.core.outbox(g).find((x) => toHex(x.msgId) === toHex(msgId));
  if (row?.state !== 1) { s.deps.onOutboxChanged(g); s.requestDrain(g); return; }
  if (!s.echoWait.has(toHex(g))) armEchoWait(s, g, msgId);
}
/** Waits echoWaitMs, catches up once more, and requeues the row only when that catch-up reached the server
 *  and did not see the upload; a catch-up that did not reach it proves nothing, so the row stays in flight
 *  and the wait starts again (TS-01: a resend after it would store a second copy). */
function armEchoWait(s: SyncInternals, g: Id, msgId: Id): void {
  const hex = toHex(g); const msgHex = toHex(msgId);
  const timer = s.armTimer(SYNC.echoWaitMs, () => {
    void s.queues.run(`g:${hex}`, async () => {
      const reached = await s.catchUpNow(g, 'catch-up');
      const fresh = s.deps.core.outbox(g).find((x) => toHex(x.msgId) === msgHex);
      if (fresh?.state === 1 && !reached) {
        // A group that left state 2 drains its in-flight row again when it is activated (drainOne).
        s.echoWait.delete(hex); if (!s.stopped() && s.row(g)?.state === 2) armEchoWait(s, g, msgId);
        return;
      }
      if (fresh?.state === 1) { s.deps.core.sendRequeue(msgId); s.deps.onOutboxChanged(g); }
      s.echoWait.delete(hex); s.requestDrain(g);
    }).catch(() => undefined);
  });
  s.echoWait.set(hex, { msgHex, timer });
}
/** FX-core (walk #2): the outbox in queue order. The core lists it by (created second, msg_id), and a msg_id is random,
 *  so two rows queued within one second can come back swapped: a requeued post would then upload after a later one.
 *  The rows this engine queued keep the order it queued them in. A row it did not queue was queued before it started,
 *  so it is older than all of those and keeps the core's place, ahead of them. */
function inQueueOrder(s: SyncInternals, rows: readonly OutboxRow[]): OutboxRow[] {
  const rank = new Map<string, number>();
  for (const h of s.queued) rank.set(h, rank.size);
  return rows.map((x, at) => ({ x, at, r: rank.get(toHex(x.msgId)) ?? -1 }))
    .sort((a, b) => a.r - b.r || a.at - b.at).map((e) => e.x);
}
/** A fold whose target is a post (type 0) still queued in the outbox. */
function targetsQueuedPost(rows: readonly OutboxRow[], fold: OutboxRow): boolean {
  const target = fold.replyTo;
  if (target === null) return false;
  const t = toHex(target);
  return rows.some((x) => x.type === 0 && x.state === 0 && toHex(x.msgId) === t);
}
/** CB2 (R38): while a slow-mode wait is armed the group posts nothing; the first queued fold (types 1..6) still drains,
 *  unless a 429 requeued it: then every fold behind it waits for the timer too, so folds keep their queue order (two edits
 *  of one message must reach the host in order). A fold whose target is a post still queued waits with that post. */
function foldBesideWait(s: SyncInternals, rows: readonly OutboxRow[]): OutboxRow | undefined {
  for (const x of rows) {
    if (x.state !== 0 || x.type === 0) continue;
    if (s.slowed.has(toHex(x.msgId))) return undefined;
    if (!targetsQueuedPost(rows, x)) return x;
  }
  return undefined;
}
export async function drainOne(s: SyncInternals, g: Id): Promise<void> {
  const hex = toHex(g);
  if (s.stopped() || !s.canSendInGroup(g) || s.echoWait.has(hex) || s.row(g)?.state !== 2) return;
  const rows = inQueueOrder(s, s.deps.core.outbox(g));
  const inFlight = rows.find((x) => x.state === 1);
  if (inFlight !== undefined) { await afterLostResponse(s, g, inFlight.msgId); return; }
  const row: OutboxRow | undefined = s.slowWait.has(hex) ? foldBesideWait(s, rows) : rows.find((x) => x.state === 0);
  if (row === undefined) return;
  const msgId = row.msgId; const msgHex = toHex(msgId);
  const reframeKey = `${hex}:${msgHex}`;
  // The row waits for the trust refresh its leaf-verdict refusal started; that refresh drains the group again.
  if (s.trustRetried.get(reframeKey) === 'pending') return;
  if (row.attachments.length > 0) {
    // Ruling 7 (Q17): every attempt confirms the row's references before its upload, so a sent attachment is never pending.
    try { await s.deps.beforeSend(g, row); }
    catch (e) {
      if (s.stopped()) return;
      // Pre-flight ruling F5: a confirm that did not reach a verdict (no answer, 401, 429, 5xx) leaves the row queued, as a
      // text row stays queued across network loss; the next ready drains it again (no timer of its own).
      if (e instanceof DillaHttpError && (e.status === 0 || e.status === 401 || e.status === 429 || e.status >= 500)) return;
      // A discard or a retry that ran meanwhile owns the row now.
      if (!s.deps.core.outbox(g).some((x) => toHex(x.msgId) === msgHex && x.state === 0)) { s.requestDrain(g); return; }
      const code = e instanceof DillaHttpError ? (e.status === 404 ? 'E_ATTACHMENT_MISSING' : e.code)
        : e instanceof CoreError ? e.code : 'E_INTERNAL';
      s.deps.core.sendFail(msgId, code); s.reframedOnce.delete(reframeKey); s.slowed.delete(msgHex); s.deps.onOutboxChanged(g); s.requestDrain(g);
      return;
    }
    if (s.stopped()) return;
    if (!s.deps.core.outbox(g).some((x) => toHex(x.msgId) === msgHex && x.state === 0)) { s.requestDrain(g); return; }
  }
  let messageBody: Uint8Array;
  try {
    messageBody = s.deps.core.sendEncrypt(msgId).messageBody;
  }
  catch (e) {
    if (e instanceof CoreError && e.code === 'E_CORE_STATE') {
      const group = s.row(g);
      if (group?.pendingCommit) return;
      if ((group?.proposalsPending ?? 0) > 0) {
        if (s.quiet.has(hex)) return;
        const ok = await s.commitNow(g, 0);
        if (!ok) s.quiet.add(hex);
        s.requestDrain(g);
      }
      return;
    }
    // I1: send_encrypt refused before it changed the row (state 0). A device that joined before the list naming it was
    // ingested fails against the cached list until a refresh ingests the newer one: the row stays queued for one refresh.
    // A second refusal fails it; a changed key (E_UMK_CHANGED) is never retried.
    if (e instanceof CoreError && LEAF_VERDICTS.has(e.code) && !s.trustRetried.has(reframeKey)) {
      refreshTrustThenDrain(s, g, reframeKey);
      return;
    }
    if (e instanceof CoreError) {
      s.deps.core.sendFail(msgId, e.code); s.reframedOnce.delete(reframeKey); s.slowed.delete(msgHex); s.trustRetried.delete(reframeKey);
      s.deps.onOutboxChanged(g); s.requestDrain(g); return;
    }
    throw e;
  }
  try {
    const answer = await s.deps.routes.postMessage(g, messageBody, msgId);
    if (s.stopped()) return;
    let confirmed: { groupId: Id; seq: bigint };
    try { confirmed = s.deps.core.sendConfirm(msgId, answer.raw); }
    catch (e) {
      if (!(e instanceof CoreError)) throw e;
      if (!s.deps.core.outbox(g).some((x) => toHex(x.msgId) === msgHex && x.state !== 2)) s.reframedOnce.delete(reframeKey);
      s.deps.onOutboxChanged(g); s.count425.delete(msgHex); s.requestCatchUp(g); s.requestDrain(g); return;
    }
    s.reframedOnce.delete(reframeKey); s.trustRetried.delete(reframeKey); s.deps.onOutboxChanged(g); s.count425.delete(msgHex);
    s.queued.delete(msgHex);
    // The clear first, so the countdown onPosted starts is not cleared after it.
    if (s.slowed.delete(msgHex) && row.type === 0 && !s.slowWait.has(hex)) s.deps.onSlow(g, 0, false);
    s.deps.onPosted(g, row.type, row, confirmed.seq);
    s.requestDrain(g);
  } catch (e) {
    if (s.stopped()) return;
    const status = e instanceof DillaHttpError ? e.status : 0;
    const code = e instanceof DillaHttpError ? e.code : 'E_NETWORK';
    if (status === 422 && code === 'E_COMMIT_INVALID' && e instanceof DillaHttpError && e.extra[0] === 'epoch' && !s.reframedOnce.has(reframeKey)) {
      // The instance rejected this upload before storing it. Re-frame the same outbox row at the
      // current epoch, once; this is separate from the lost-response path, where storage is unknown.
      const reached = await s.catchUpNow(g, 'catch-up');
      const current = s.deps.core.outbox(g).find((x) => toHex(x.msgId) === msgHex);
      if (!reached || current?.state !== 1 || s.row(g)?.state !== 2) {
        if (current?.state === 1) { s.deps.core.sendFail(msgId, code); s.reframedOnce.delete(reframeKey); s.slowed.delete(msgHex); }
        s.deps.onOutboxChanged(g); s.requestDrain(g);
        return;
      }
      s.deps.core.sendRequeue(msgId);
      s.reframedOnce.add(reframeKey);
      s.deps.onOutboxChanged(g); s.requestDrain(g);
      return;
    }
    if (status === 429) {
      // CB2 (R6): every 429 requeues the row and arms one timer for the group; the detail is never read. The lane is
      // released at once, so frames, catch-up and the folds beside the wait go on (R38).
      s.deps.core.sendRequeue(msgId); s.deps.onOutboxChanged(g);
      s.slowed.add(msgHex);
      const wait = e instanceof DillaHttpError && e.retryAfterMs !== null ? e.retryAfterMs : 1000;
      const until = s.deps.now() + wait;
      // The later deadline wins: a fold's short 429 never cuts a post's wait short.
      const old = s.slowWait.get(hex);
      if (old === undefined || until > old.until) {
        if (old !== undefined) s.cancelTimer(old.timer);
        s.slowWait.set(hex, { timer: s.armTimer(wait, () => { s.slowWait.delete(hex); s.requestDrain(g); }), until });
      }
      // Only a post's wait is the person's: a fold's 429 is waited out silently. The countdown is the deadline the
      // group actually waits for, so it never ends before the post can go.
      if (row.type === 0) s.deps.onSlow(g, Math.max(until, old?.until ?? 0), true);
      s.requestDrain(g);
      return;
    }
    if (status === 425) {
      s.deps.core.sendRequeue(msgId); s.deps.onOutboxChanged(g);
      const n = (s.count425.get(msgHex) ?? 0) + 1; s.count425.set(msgHex, n);
      if (n >= SYNC.commitRetryMax) {
        s.deps.core.sendFail(msgId, 'E_COMMIT_REQUIRED'); s.reframedOnce.delete(reframeKey); s.slowed.delete(msgHex); s.deps.onOutboxChanged(g); s.requestDrain(g);
      } else {
        const old = s.epochWait.get(hex); if (old !== undefined) s.cancelTimer(old);
        s.epochWait.set(hex, s.armTimer(SYNC.membershipWaitMs, () => {
          s.epochWait.delete(hex); s.requestCommit(g, 0); s.requestDrain(g);
        }));
      }
    } else if (status === 403 && code === 'E_LEAF_NOT_CURRENT') {
      s.deps.core.sendRequeue(msgId); s.deps.onOutboxChanged(g);
      if (!s.resyncTried.has(hex)) await resyncGroup(s, g, false);
    } else if (status === 401) {
      s.deps.core.sendRequeue(msgId); s.deps.onOutboxChanged(g);
    } else if (status === 0 || status === 500 || status === 502 || status === 504) {
      // 502/504 come from the operator's proxy (L-HTTP-10) and say nothing about whether dillad stored it.
      await afterLostResponse(s, g, msgId);
    } else {
      s.deps.core.sendFail(msgId, code); s.reframedOnce.delete(reframeKey); s.slowed.delete(msgHex); s.deps.onOutboxChanged(g); s.requestDrain(g);
    }
  }
}
