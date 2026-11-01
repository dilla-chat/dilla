import { arr, bin, decode, encode, str, u53, u64, type CborValue } from '../cbor';
import { decodeBoundedArray } from './bounded';
import { CoreError, trustBlock, type Id, type PendingLeaf, type TrustRecord } from '../core-port';
import type { Routes } from '../http/routes';

type ReaderRoutes = Pick<Routes, 'getDeviceList' | 'getDeviceListHistory'>;
const MAX_VERSIONS = 1024;
const MAX_BYTES = 4_194_304;
const PAGE = 64;
const MAX_REPLY_ROWS = 4096;
const equal = (a: Uint8Array, b: Uint8Array): boolean => a.length === b.length && a.every((v, i) => v === b[i]);
const stale = (): never => { throw new CoreError('E_DEVICE_LIST_STALE'); };
const malformed = (error: unknown): never => { throw new CoreError('E_CORE_WASM', String(error)); };

export function decodePendingLeaves(bytes: Uint8Array): PendingLeaf[] {
  try {
    const rows = decodeBoundedArray(bytes, MAX_REPLY_ROWS);
    return rows.map((value) => {
      const row = arr(value, 5);
      const leafIndex = u53(row[1]);
      if (leafIndex > 0xffff_ffff) return malformed('leaf index');
      const reason = str(row[4]);
      if (reason !== 'unknown' && reason !== 'changed' && reason !== 'ready-refresh') return malformed('leaf reason');
      return { groupId: bin(row[0], 16), leafIndex, userId: bin(row[2], 16), cachedVersion: u64(row[3]), reason };
    });
  } catch (error) { return malformed(error); }
}

export function decodeTrustState(bytes: Uint8Array): TrustRecord[] {
  try {
    const rows = decodeBoundedArray(bytes, MAX_REPLY_ROWS);
    return rows.map((value) => {
      const row = arr(value, 6);
      const verified = u53(row[3]);
      if (verified !== 0 && verified !== 1) return malformed('verified bit');
      return { userId: bin(row[0], 16), umk: bin(row[1], 32), firstSeen: u64(row[2]), verified: verified === 1,
        changedUmk: row[4] === null ? null : bin(row[4], 32), blocked: row[5] === null ? null : trustBlock(str(row[5])) };
    });
  } catch (error) { return malformed(error); }
}

/** The candidate head row: [version, list, ssk_signature, prev_hash], plus on wire 4 the account-kind attestation. */
function headRow(head: Uint8Array): CborValue[] {
  const row = arr(decode(head));
  if (row.length !== 4 && row.length !== 5) throw new RangeError('E_CBOR_SHAPE: device-list head');
  return row;
}

/**
 * The core's `head_list`: the signed DeviceList inside the reader's candidate head row [version, list, ssk_signature,
 * prev_hash(, attestation)]. The core decodes and caches exactly these bytes (trust.rs `bounded_list`), as the native peer
 * passes them.
 */
export function signedHeadList(head: Uint8Array): Uint8Array {
  try { return bin(headRow(head)[1]); } catch { return stale(); }
}

/**
 * Task S1 fix round 1: the four-element list body [version, list, ssk_signature, prev_hash] of a served head, the one
 * form every core decoder of a list body takes (enrolComplete, deviceListRevoke, the history chain). A wire-4 head's
 * fifth element, the account-kind attestation, is no part of it; any other head is returned as served.
 */
export function chainRow(head: Uint8Array): Uint8Array {
  // Only a five-element head is rewritten; anything else is passed as served and the core's decoder judges it.
  let row: CborValue[];
  try { row = arr(decode(head)); } catch { return head; }
  return row.length === 5 ? encode(row.slice(0, 4)) : head;
}

/** Task S1 part 2: the instance-signed account-kind attestation a wire-4 head carries as its fifth element, else null. */
export function headAttestation(head: Uint8Array): Uint8Array | null {
  try { const row = headRow(head); return row.length === 5 ? bin(row[4]) : null; } catch { return stale(); }
}

/**
 * Task S1 part 2: hand the head's account-kind attestation to the core before the user's history is ingested, so the
 * ingest's leaf verdicts judge the credential's kind against it. The core stores one only once it verifies (instance key,
 * instance id, user id; it binds no key, Ruling AW). Fix round 2: this never throws. Any refusal (a malformed attestation, a core with no pinned
 * instance key) is handled exactly like a withheld attestation: nothing is stored and the history ingest still runs, so a
 * key change is still recorded and the removal feed never stalls; a user without a verified attestation stays pending once
 * one was seen. No refusal ever surfaces as a leaf verdict (E_CREDENTIAL is the core's verdict on a verified attestation
 * of another kind), so a server cannot steer which verdict the client shows. Task 11: a refusal other than the expected
 * two (E_CORE_INPUT, the server's bytes; E_CORE_STATE, no pinned instance key yet), such as a storage failure, is still
 * handled as withheld but goes to `onError` (the worker's diagnostics) rather than vanishing.
 */
export function ingestAccountKind(core: { accountKindIngest(userId: Id, attestation: Uint8Array): void }, userId: Id, head: Uint8Array,
  onError?: (code: string) => void): void {
  let attestation: Uint8Array | null;
  try { attestation = headAttestation(head); } catch { return; }
  if (attestation === null) return;
  try { core.accountKindIngest(userId, attestation); }
  catch (e) {
    // Withheld: the ingest runs without it.
    const code = e instanceof CoreError ? e.code : 'E_CORE_UNKNOWN';
    if (code !== 'E_CORE_INPUT' && code !== 'E_CORE_STATE') onError?.(code);
  }
}

/**
 * The rows of a history read by readDeviceListHistory from `version` on, or null when it holds none. The core anchors an
 * ingest on the list it caches (trust.rs ingest_leaf: the first row is the cached version), so once one leaf's ingest
 * cached a newer version, the user's other leaves are ingested from that version, not from where the read began.
 */
export function historyFrom(history: Uint8Array, version: bigint): Uint8Array | null {
  try {
    const rows = decodeBoundedArray(history, MAX_VERSIONS).filter((row) => u64(arr(row, 4)[0]) >= version);
    return rows.length === 0 ? null : encode(rows);
  } catch { return null; }
}

/** The server supplies bytes; the Rust core authenticates all signatures and leaf evidence. */
export async function readDeviceListHistory(routes: ReaderRoutes, userId: Id, cachedVersion: bigint): Promise<{ head: Uint8Array; history: Uint8Array }> {
  if (!(userId instanceof Uint8Array) || userId.length !== 16 || cachedVersion < 0n) stale();
  let budgetBytes = 0;
  let budgetRows = 0;
  try {
    for (let attempt = 0; attempt < 2; attempt++) {
      const candidate = await routes.getDeviceList(userId);
      if (candidate === null || candidate.raw.length > 1_048_576 || candidate.version < cachedVersion) return stale();
      // The history pages carry the four-element row; a wire-4 head adds the attestation, which is no part of the chain.
      const candidateRow = headRow(candidate.raw).slice(0, 4);
      const candidateChainRow = encode(candidateRow);
      if (u64(candidateRow[0]) !== candidate.version || !equal(bin(candidateRow[1]), candidate.blob)) stale();
      bin(candidateRow[2], 64); bin(candidateRow[3], 32);
      if (candidate.version - cachedVersion > BigInt(MAX_VERSIONS)) stale();
      // Rust validates the cached signed row as the first history anchor before accepting newer rows.
      let after = cachedVersion > 0n ? cachedVersion - 1n : 0n;
      const rows: CborValue[] = [];
      if (candidate.version === cachedVersion) {
        rows.push(candidateRow);
        after = candidate.version;
        budgetRows += 1;
        budgetBytes += candidate.raw.length;
        if (budgetRows > MAX_VERSIONS || budgetBytes > MAX_BYTES) stale();
      }
      let overranCandidate = false;
      while (after < candidate.version) {
        const page = await routes.getDeviceListHistory(userId, after);
        budgetBytes += page.raw.length;
        if (page.raw.length > 1_048_576 || budgetBytes > MAX_BYTES || page.count < 1 || page.count > PAGE) stale();
        const got = decodeBoundedArray(page.raw, PAGE);
        if (got.length !== page.count || budgetRows + got.length > MAX_VERSIONS) stale();
        for (const value of got) {
          const row = arr(value, 4);
          const version = u64(row[0]);
          if (version !== after + 1n) stale();
          if (version > candidate.version) { overranCandidate = true; break; }
          bin(row[1]); bin(row[2], 64); bin(row[3], 32);
          rows.push(row);
          after = version;
        }
        budgetRows += got.length;
        if (overranCandidate) break;
        if (page.lastVersion !== after) stale();
      }
      if (rows.length < 1 || !equal(encode(rows[rows.length - 1]), candidateChainRow)) stale();
      const head = await routes.getDeviceList(userId);
      if (overranCandidate && (head === null || head.version <= candidate.version)) stale();
      if (head !== null && head.version === candidate.version && equal(head.raw, candidate.raw)) {
        const history = encode(rows);
        if (history.length > MAX_BYTES) stale();
        return { head: candidate.raw, history };
      }
    }
  } catch { return stale(); }
  return stale();
}
