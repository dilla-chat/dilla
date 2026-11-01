import { decodeRemovalProof } from '../bot/protocol';
import type { CorePort, Id, RemovalFeedEntry } from '../core-port';
import type { Routes } from '../http/routes';
import { ingestAccountKind, readDeviceListHistory, signedHeadList } from '../state/trust';

const PAGE_LIMIT = 250;
const PAGE_BUDGET = 8;

/** Reconcile the authenticated server feed before a caller publishes a newly applied timeline. */
export async function recoverModerationRemovals(core: CorePort, routes: Pick<Routes,
  'getModerationRemovals' | 'getDeviceList' | 'getDeviceListHistory'>, groupId: Id,
  onError?: (code: string) => void): Promise<void> {
  let cursor = core.removalCursor(groupId);
  for (let pageNo = 0; pageNo < PAGE_BUDGET; pageNo++) {
    const page = await routes.getModerationRemovals(groupId, cursor, PAGE_LIMIT);
    if (page.rows.length === 0) {
      if (page.nextRevision !== cursor) throw new Error('E_REMOVAL_FEED: empty page advanced cursor');
      return;
    }
    if (page.rows.length > PAGE_LIMIT || page.nextRevision !== cursor + BigInt(page.rows.length)) {
      throw new Error('E_REMOVAL_FEED: invalid revision range');
    }
    const actors = new Map<string, RemovalFeedEntry>();
    let expected = cursor;
    for (const row of page.rows) {
      expected += 1n;
      if (row.revision !== expected) throw new Error('E_REMOVAL_FEED: revision gap');
      const proof = decodeRemovalProof(row.proof);
      if (toHex(proof.groupId) !== toHex(groupId)) throw new Error('E_REMOVAL_FEED: proof group mismatch');
      const actor = toHex(proof.actorUser);
      if (!actors.has(actor)) actors.set(actor, row);
    }
    // Refresh each previously trusted actor against the signed history chain before proof verification.
    for (const entry of actors.values()) {
      const cachedVersion = core.removalActorHistoryVersion(groupId, entry);
      const actor = decodeRemovalProof(entry.proof).actorUser;
      const { history, head } = await readDeviceListHistory(routes, actor, cachedVersion);
      ingestAccountKind(core, actor, head, onError);
      core.refreshRemovalActorHistory(groupId, entry, history, signedHeadList(head));
    }
    for (const row of page.rows) {
      const persisted = core.ingestRemoval(groupId, row);
      if (persisted !== row.revision) throw new Error('E_REMOVAL_FEED: core cursor mismatch');
    }
    cursor = page.nextRevision;
    if (page.rows.length < PAGE_LIMIT) return;
  }
  throw new Error('E_REMOVAL_SYNC_LIMIT: feed remains ahead after bounded recovery');
}

function toHex(bytes: Uint8Array): string {
  let out = '';
  for (const b of bytes) out += b.toString(16).padStart(2, '0');
  return out;
}
