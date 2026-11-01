import { arr, bin, decode, encode, opt, str, u53, u64, CborError, type CborValue } from './cbor';
import { decodePendingLeaves, decodeTrustState } from './state/trust';
import { decodeSearchHits, validateSearch } from './state/search';

/** The changed contact a call-gate refusal names (calls.rs `refusal`: `E_UMK_CHANGED: <user id hex>`), decoded once here
 *  at the core's boundary, never by reading `detail` elsewhere (web-3 task 10, concern 3). */
const CHANGED_USER = /^E_UMK_CHANGED: ([0-9a-f]{32})$/;
/** A store a newer client wrote (schema.rs `refuse_newer`, error.rs `E_STORE_NEWER`): the app schema it found and the
 *  highest this build supports, decoded once here at the core's boundary from the error's message (C4x, web3.6). */
const STORE_NEWER = /^E_STORE_NEWER: found (\d{1,10}), supported (\d{1,10})$/;
export interface StoreVersions { found: number; supported: number; }
/** The two schema numbers of an E_STORE_NEWER refusal, from core_open's plain Error or a CoreError; null otherwise. */
export function storeNewerOf(err: unknown): StoreVersions | null {
  if (!(err instanceof Error)) return null;
  const match = STORE_NEWER.exec(err.message);
  return match ? { found: Number(match[1]), supported: Number(match[2]) } : null;
}

export class CoreError extends Error {
  readonly code: string;
  readonly detail: string;
  /** The user (hex) an E_UMK_CHANGED refusal of the call gate names as the changed contact; null otherwise. */
  readonly changedUser: string | null;
  constructor(code: string, detail = '') {
    super(detail === '' ? code : `${code}: ${detail}`);
    this.name = 'CoreError'; this.code = code; this.detail = detail;
    this.changedUser = CHANGED_USER.exec(this.message)?.[1] ?? null;
  }
  static from(err: unknown): CoreError {
    if (err instanceof CoreError) return err;
    if (err instanceof Error) {
      const match = /^(E_[A-Z0-9_]+)(?:: ([\s\S]*))?$/.exec(err.message);
      return match ? new CoreError(match[1] ?? '', match[2] ?? '') : new CoreError('E_CORE_WASM', err.message);
    }
    return new CoreError('E_CORE_WASM', String(err));
  }
}
export type Id = Uint8Array;
export interface IdentityInfo { phase: 0 | 1 | 2 | 3; instanceId: Id | null; userId: Id | null; deviceId: Id | null; username: string; listPublished: boolean; }
export interface SessionRecord { token: string; expires: bigint; idleExpires: bigint; }
export const GroupState = { registering: 0, joining: 1, active: 2, needsResync: 3, gone: 4 } as const;
export interface GroupInfo { groupId: Id; kind: number; communityId: Id | null; targetId: Id; state: 0 | 1 | 2 | 3 | 4; epoch: bigint; nextSeq: bigint; proposalsPending: number; pendingCommit: boolean; }
export interface ApplyResult { state: 0 | 1 | 2 | 3 | 4; epoch: bigint; nextSeq: bigint; newSeqs: bigint[]; proposalsPending: number; epochChanged: boolean; ownAdopted: boolean; }
export interface WelcomeOutcome { welcomeId: bigint; groupId: Id; outcome: 0 | 1 | 2 | 3; reason: string; }
export interface ExpectedGroup { groupId: Id; communityId: Id | null; channelId: Id; policyVersion: bigint; }
export interface ActivityRow { groupId: Id; unread: number; mentions: number; lastSeq: bigint; lastTs: bigint; lastReadSeq: bigint; }
export interface DeviceEntryInfo { deviceId: Id; dskPub: Uint8Array; tier: 0 | 1; addedAt: bigint; revokedAt: bigint | null; }
export interface OwnDeviceList { version: bigint; published: boolean; entries: DeviceEntryInfo[]; }
export interface SealedObjects { root: Uint8Array | null; state: Uint8Array | null; stateUploaded: boolean; }
/** What enrol_complete and device_list_revoke sign: the next list's PUT body, the re-sealed state object, and the PUT
 *  body of an interrupted publication the core found in the state object and signed on, to send first
 *  (BACKUPS-RECOVERY-02), else null. */
export interface SignedLists { deviceListBody: Uint8Array; stateSealed: Uint8Array; interrupted: Uint8Array | null; }
export interface AttachmentDescriptor { blobId: Uint8Array; key: Uint8Array; nonce: Uint8Array; size: number; mime: string;
  w: number | null; h: number | null; thumb: Uint8Array | null; name: string; }
/** `threadId` (web3.7) names the held type-0 root a message joins; absent or null is no thread, and only type 0 may
 *  carry one (the core refuses it on 1..6 with E_CORE_INPUT). */
export interface SendRequest { type: 0 | 1 | 2 | 3 | 4 | 5 | 6; replyTo: Id | null; body: string; attachments: AttachmentDescriptor[]; threadId: Id | null; }
export interface OutboxRow { msgId: Id; state: 0 | 1 | 2; error: string; created: bigint; body: string; type: 0 | 1 | 2 | 3 | 4 | 5 | 6; replyTo: Id | null;
  attachments: { blobId: Uint8Array; size: number; mime: string; name: string }[]; }
/** `threadRoot` (web3.7 fix round 1, M3): the root of the thread the pinned row shows in, null for a channel row. */
export interface PinRow { targetSeq: bigint; msgId: Id; pinnedSeq: bigint; byUser: Id; author: Id | null; excerpt: string; targetTs: bigint; threadRoot: Id | null; }
export interface PurgeRow { groupId: Id; seq: bigint; channelId: Id; blobIds: Uint8Array[]; }
export interface PendingLeaf { groupId: Id; leafIndex: number; userId: Id; cachedVersion: bigint; reason: string; }
/** The stored verdict that blocks a user's leaf (and with it sending in its group); E_UMK_CHANGED wins (ABI 8).
 *  E_BINDING (web-3, ABI 9) is the call gate's refusal of a DM call group bound to a community its text group is not
 *  (client/calls.rs dm_text_id): a block of its own, never folded into E_CREDENTIAL. */
export type TrustBlock = 'E_UMK_CHANGED' | 'E_CREDENTIAL' | 'E_DEVICE_UNLISTED' | 'E_DEVICE_REVOKED' | 'E_TIER_MISMATCH' | 'E_BINDING';
const TRUST_BLOCKS: readonly string[] = ['E_UMK_CHANGED', 'E_CREDENTIAL', 'E_DEVICE_UNLISTED', 'E_DEVICE_REVOKED', 'E_TIER_MISMATCH', 'E_BINDING'];
/** Whether a refusal code is a trust block: the call gate's hard refusals (task 6 blocks or pauses on these). */
export function isTrustBlock(code: string): code is TrustBlock { return TRUST_BLOCKS.includes(code); }
/** The core's block_code: a known verdict as it is, any other stored code as a failed identity check (fail closed). */
export function trustBlock(code: string): TrustBlock {
  return TRUST_BLOCKS.includes(code) ? code as TrustBlock : 'E_CREDENTIAL';
}
/**
 * One trust_state row (ABI 8). `blocked` is the stored verdict of this user's leaves still in an active group, read from
 * the store (right after a reload too). The own user's row is always present: its own credential's key, verified, never
 * changed; its `blocked` reports an own-user leaf under another key (E_UMK_CHANGED) or an unlisted own device.
 */
export interface TrustRecord { userId: Id; umk: Uint8Array; firstSeen: bigint; verified: boolean; changedUmk: Uint8Array | null; blocked: TrustBlock | null; }
export interface SearchScope { kind: 'channel' | 'community' | 'all'; id: Id | null; }
export interface SearchCursor { rank: bigint; groupId: Id; seq: bigint; }
/** `threadRoot` (web3.7 fix round 1, M3): the root of the thread the hit shows in, null for a channel row. */
export interface SearchHit { groupId: Id; seq: bigint; snippet: string; rank: bigint; threadRoot: Id | null; }
/** `threadRoot` (web3.7 fix round 1, M3): the root of the thread the quoted row shows in, null for a channel row. */
export interface ReplyInfo { replyTo: Id; targetSeq: bigint | null; targetUser: Id | null; excerpt: string; state: 0 | 1 | 2; threadRoot: Id | null; }
export interface ReactionInfo { emoji: string; count: number; mine: boolean; }
export interface AttachmentInfo { index: number; size: number; mime: string; w: number | null; h: number | null; hasThumb: boolean; name: string; }
/** `thread` (web3.7) is the summary of a held thread root with at least one shown reply: the reply count and the latest
 *  reply's `recvTs`; null on every other row. */
export interface ThreadSummary { count: number; lastTs: bigint; }
export interface TimelineRow { seq: bigint; epoch: bigint; recvTs: bigint; status: 0 | 1 | 2; reason: string; senderUser: Id | null; senderDevice: Id; senderKind: number | null; senderTier: number | null; msgId: Id | null; type: number | null; body: string;
  editedSeq: bigint; reply: ReplyInfo | null; reactions: ReactionInfo[]; pinned: boolean; attachments: AttachmentInfo[]; mention: boolean; thread: ThreadSummary | null; }
/** One `threads` row (web3.7): a thread of the group, its held root's seq and excerpt (null and '' when the root is not
 *  held), its shown reply count and the latest reply's `recvTs`. */
export interface ThreadRow { rootMsgId: Id; rootSeq: bigint | null; count: number; lastTs: bigint; excerpt: string; }
export interface RemovalFeedEntry { revision: bigint; proof: Uint8Array; signature: Uint8Array; acceptedAt: bigint; }
/** Protected local outbox entry for a human removal's op61 evidence. State 1 means POST outcome unknown. */
export interface ManualRemovalEvidence {
  proofRequestId: Id; evidenceRequestId: Id; actorUserId: Id; actorDeviceId: Id; communityId: Id; groupId: Id;
  channelId: Id; messageId: Id; targetSequence: bigint; reason: string; excerpt: string; signedBody: Uint8Array;
  state: 0 | 1 | 2; createdAt: bigint; expires: bigint; evidenceExpires: bigint;
}
/** Protected local outbox for a manual warning: the reason is staged before POST and tied to its accepted receipt. */
export interface ManualWarningEvidence {
  requestId: Id; evidenceRequestId: Id; actorUserId: Id; actorDeviceId: Id; communityId: Id; targetUserId: Id;
  reason: string; state: 0 | 1; createdAt: bigint; actionId: Id; revision: bigint; acceptedAt: bigint; evidenceExpires: bigint;
}
export interface HumanInteractionEvent {
  deliveryId: bigint; groupId: Id; communityId: Id; requestId: Id; sourceSeq: bigint; epoch: bigint; receivedAt: bigint;
  senderUser: Id; senderDevice: Id; messageId: Id; body: string; operation: number; channelId: Id | null;
}
export interface HumanInteractionEventPage { events: HumanInteractionEvent[]; next: bigint; highWater: bigint; }
export interface CorePort {
  pause(): void; resume(): Promise<void>; close(): void;
  identity(): IdentityInfo;
  signupBegin(instanceId: Id): string;
  signupRequest(invite: string, username: string, display: string, password: string | null): Uint8Array;
  signupComplete(userId: Id, username: string, now: bigint, instanceSignerPub: Uint8Array): Uint8Array;
  instanceSignerKeyMatches(observedKey: Uint8Array): void;
  instanceSignerKeyPin(instanceId: Id, observedKey: Uint8Array): void;
  signupReset(): void;
  deviceListBody(): Uint8Array; deviceListPublished(): void;
  /** BACKUPS-RECOVERY-02: an unpublished candidate is dropped; the accepted list's own body becomes the candidate. */
  deviceListDrop(): void;
  sessionSign(nonce: Uint8Array, purpose: 0 | 1): Uint8Array;
  signRemoval(requestId: Id, groupId: Id, targetSeq: bigint, expires: bigint, now: bigint): Uint8Array;
  prepareManualRemovalEvidence?(proofRequestId: Id, evidenceRequestId: Id, communityId: Id, groupId: Id, channelId: Id,
    messageId: Id, targetSeq: bigint, reason: string, excerpt: string, createdAt: bigint, expires: bigint, now: bigint): Uint8Array;
  manualRemovalEvidence?(): ManualRemovalEvidence[];
  ensureManualRemovalEvidenceRequest?(proofRequestId: Id, candidateRequestId: Id, now: bigint): { requestId: Id; expires: bigint };
  markManualRemovalSubmitted?(requestId: Id): void;
  markManualRemovalAccepted?(requestId: Id): void;
  completeManualRemovalEvidence?(requestId: Id, now: bigint): void;
  rejectUnsubmittedManualRemoval?(requestId: Id, now: bigint): void;
  rejectManualRemoval?(requestId: Id, now: bigint): void;
  prepareManualWarningEvidence?(requestId: Id, evidenceRequestId: Id, communityId: Id, targetUserId: Id,
    reason: string, createdAt: bigint, now: bigint): void;
  manualWarningEvidence?(): ManualWarningEvidence[];
  markManualWarningAccepted?(requestId: Id, actionId: Id, revision: bigint, acceptedAt: bigint): void;
  ensureManualWarningEvidenceRequest?(requestId: Id, candidateRequestId: Id, now: bigint): { requestId: Id; expires: bigint };
  completeManualWarningEvidence?(requestId: Id, now: bigint): void;
  sessionStore(s: SessionRecord): void; session(): SessionRecord | null; sessionClear(): void;
  keyPackages(count: number, lastResort: boolean): Uint8Array;
  groups(): GroupInfo[];
  /** Deterministic CBOR [group_info_bytes, tail_array, ratchet_tree_bytes] for repairing server history. */
  groupHealSnapshot(groupId: Id): Uint8Array;
  groupCreate(groupId: Id, communityId: Id | null, channelId: Id, policyVersion: bigint, externalSenderPub: Uint8Array): Uint8Array;
  groupRegistered(groupId: Id, nextSeq: bigint): void;
  groupDiscard(groupId: Id): void;
  groupJoinExternal(g: ExpectedGroup, infoBody: Uint8Array, treeBody: Uint8Array): Uint8Array;
  groupJoined(groupId: Id, seq: bigint): void;
  welcomesApply(welcomesBody: Uint8Array, expected: ExpectedGroup[]): WelcomeOutcome[];
  /** The core verifies this opaque signed context and derives the expected kind-3 binding itself. */
  humanInteractionWelcomeApply(welcomesBody: Uint8Array, currentContextProof: Uint8Array, now: bigint): WelcomeOutcome[];
  humanInteractionSendPrepare(requestId: Id, communityId: Id, groupId: Id, requestBody: Uint8Array, now: bigint): Id;
  humanInteractionEventsRead(after: bigint, limit: number): HumanInteractionEventPage;
  humanInteractionReceiptsRead(communityId: Id, installationId: Id, botUserId: Id, requestIds: readonly Id[]): HumanInteractionEvent[];
  humanInteractionEventsAck(through: bigint): void;
  humanInteractionEventsAckSelected(deliveryIds: readonly bigint[]): void;
  groupApply(groupId: Id, handshakes: Uint8Array, messages: Uint8Array, through: bigint): ApplyResult;
  commitBuild(groupId: Id, proposalsBody: Uint8Array): Uint8Array;
  commitConfirm(groupId: Id): ApplyResult; commitAbort(groupId: Id): void;
  cursorBody(groupId: Id): Uint8Array | null; cursorAcked(groupId: Id, lastSeq: bigint, lastEpoch: bigint): void;
  messageDeleted(groupId: Id, seq: bigint): ApplyResult;
  removalCursor(groupId: Id): bigint;
  removalActorHistoryVersion(groupId: Id, entry: RemovalFeedEntry): bigint;
  refreshRemovalActorHistory(groupId: Id, entry: RemovalFeedEntry, history: Uint8Array, headList: Uint8Array): bigint;
  ingestRemoval(groupId: Id, entry: RemovalFeedEntry): bigint;
  sendPrepare(groupId: Id, request: SendRequest, now: bigint): Id;
  sendEncrypt(msgId: Id): { groupId: Id; messageBody: Uint8Array };
  sendConfirm(msgId: Id, response: Uint8Array): { groupId: Id; seq: bigint };
  sendRequeue(msgId: Id): void; sendFail(msgId: Id, error: string): void; sendRetry(msgId: Id): void; sendDiscard(msgId: Id): void;
  outbox(groupId: Id): OutboxRow[];
  timeline(groupId: Id, beforeSeq: bigint, limit: number): TimelineRow[];
  /** web3.7 (R25): the held root (null when it is not held) and the thread's replies, newest first, below `beforeSeq`
   *  (0n = from the newest), at most `limit` (1..=200). */
  threadTimeline(groupId: Id, rootMsgId: Id, beforeSeq: bigint, limit: number): { root: TimelineRow | null; rows: TimelineRow[] };
  /** web3.7: the group's threads, at most 50, the latest reply first. */
  threads(groupId: Id): ThreadRow[];
  groupRow(groupId: Id): GroupInfo | null;
  markRead(groupId: Id, seq: bigint, now: bigint): void;
  activity(): ActivityRow[];
  pins(groupId: Id): PinRow[];
  attachmentGet(groupId: Id, seq: bigint, index: number): AttachmentDescriptor;
  purges(): PurgeRow[];
  purgeDone(groupId: Id, seq: bigint): void;
  ownRolesSet(communityId: Id, roleIds: Id[]): void;
  /** web3.5 R1 (ABI 10): the group's role view: who may pin and who may send @everyone/@here there now, and the server's
   *  roles anyone may mention (a role token from a member without the broadcast right counts only for these); 0..=4096
   *  ids each. */
  authoritySet(groupId: Id, pinners: Id[], broadcasters: Id[], mentionable: Id[]): void;
  settings(): Record<string, string>; settingPut(k: string, v: string): void; settingDelete(k: string): void;
  sealedObjects(): SealedObjects; stateSealedUploaded(): void;
  /** BACKUPS-RECOVERY-03: whether this device's own sealed state object carries the list it accepted as the newest. */
  stateSealedCurrent(): boolean;
  enrolBegin(instanceId: Id): { deviceId: Id; dskPub: Uint8Array };
  enrolSessionSign(nonce: Uint8Array, login: Uint8Array): Uint8Array;
  enrolRegistered(userId: Id): void;
  /** REGISTRATION-DEVICES-02: the typed key's form (the core's normaliser and strict parse); E_RECOVERY_KEY otherwise. */
  recoveryKeyCheck(recoveryKey: string): void;
  enrolComplete(input: { recoveryKey: string; rootSealed: Uint8Array; stateSealed: Uint8Array; listBody: Uint8Array; username: string; now: bigint }): SignedLists;
  enrolReset(): void;
  deviceListRevoke(input: { recoveryKey: string; rootSealed: Uint8Array; stateSealed: Uint8Array; listBody: Uint8Array; deviceIds: Id[]; now: bigint }): SignedLists;
  ownDeviceListUpdate(historyBody: Uint8Array): { version: bigint; listed: boolean };
  ownDeviceList(): OwnDeviceList;
  pendingLeaves(): PendingLeaf[];
  proposedAdds(groupId: Id, proposalsBody: Uint8Array): { index: bigint; userId: Id; cachedVersion: bigint }[];
  deviceListIngestAdd(groupId: Id, proposalsBody: Uint8Array, index: bigint, requestedUser: Id,
    history: Uint8Array, headList: Uint8Array, now: bigint): { version: bigint; changed: boolean; quarantined: number };
  deviceListIngest(groupId: Id, leafIndex: number, requestedUser: Id, history: Uint8Array, headList: Uint8Array, now: bigint): { version: bigint; changed: boolean; quarantined: number };
  /** Task S1 part 2: store the instance-signed account-kind attestation for one user before its history is ingested. */
  accountKindIngest(userId: Id, attestation: Uint8Array): void;
  revalidatePending(groupId: Id, leafIndex: number, limit: number): number;
  trustState(): TrustRecord[];
  trustGeneration(): bigint;
  trustVerify(userId: Id, umk: Uint8Array): void;
  /** Pairwise digits for exactly the key the page will approve (the pending changed key, else the pinned one);
   *  E_DEVICE_LIST_STALE when `expectedUmk` is not that key, E_CORE_NOT_FOUND with no cached list or no shared group. */
  safetyNumberFor(userId: Id, expectedUmk: Uint8Array): string;
  /** The code send_prepare/send_encrypt refuse with in this group, or null when sending is open (ABI 8). */
  sendBlockCode(groupId: Id): TrustBlock | null;
  /** True when that block is held only by own-user leaves (R1: the page names the own account, not a contact). */
  sendBlockOwn(groupId: Id): boolean;
  search(scope: SearchScope, query: string, cursor: SearchCursor | null, limit: number): SearchHit[];
  // ---- web-3 call groups (L-WASM-34, L-TS-55); pendingLeaves and deviceListIngest serve call groups too (L-CORE-52) ----
  /** group_create for a call group: CBOR [group_id, binding, group_info, ratchet_tree] for POST /v1/groups. */
  callGroupCreate(groupId: Id, communityId: Id | null, channelId: Id, policyVersion: bigint, externalSenderPub: Uint8Array): Uint8Array;
  /** The external join of a call group, gated in the core; the commit comes back unposted, the group staged (state 1). */
  callGroupJoinExternal(g: ExpectedGroup, infoBody: Uint8Array, treeBody: Uint8Array): Uint8Array;
  /** The current epoch's media key, refused by the call gate before any key byte is derived. */
  callMediaEpoch(groupId: Id): CallMediaEpoch;
  /** The full call gate on a staged call group; the worker posts the join commit only when this passes (RV-1). */
  callJoinCheck(groupId: Id): void;
  /** CBOR [epoch, proposal]: a Remove of this device's own leaf, for POST /v1/groups/{id}/proposal. */
  callLeaveProposal(groupId: Id): Uint8Array;
  /** An empty commit with an UpdatePath in commit_build's body shape; it cancels a pending own leave. */
  callRotate(groupId: Id): Uint8Array;
  /** Deletes the call group's state and sets its row to gone. */
  callGroupForget(groupId: Id): void;
}

/** One epoch of a call group (L-TS-55). `baseKey` is a fresh 16-byte buffer the worker transfers once; nothing else
 *  holds the key bytes (the decoded buffer they came in is zero-filled). */
export interface CallMediaEpoch { epoch: bigint; baseKey: ArrayBuffer; ownLeaf: number; roster: { leaf: number; deviceId: Id; userId: Id }[]; code: string }
const CALL_CODE = /^(\d{5} ){5}\d{5}$/;
const CALL_ROSTER_MAX = 1024;
/**
 * The adapter of L-TS-55 over call_media_epoch's CBOR [epoch, base_key(16), own_leaf, [[leaf, device_id, user_id]],
 * code]: the key is copied into a fresh ArrayBuffer and the whole decoded buffer is zero-filled, whether or not it
 * decodes. The facade's own Vec is already zeroized (L-WASM-34); this clears the copy wasm-bindgen handed out.
 */
export function readCallMediaEpoch(bytes: Uint8Array): CallMediaEpoch {
  // WB security m1: the decoded key copy is zero-filled on every path, an error after it was decoded included.
  let key: Uint8Array | null = null;
  try {
    const a = arr(decode(bytes), 5);
    key = bin(field(a, 1), 16);
    const roster = arr(field(a, 3));
    if (roster.length > CALL_ROSTER_MAX) throw new CborError('E_CBOR_SHAPE', 'roster limit');
    const code = str(field(a, 4));
    if (!CALL_CODE.test(code)) throw new CborError('E_CBOR_RANGE', 'call code');
    const out: CallMediaEpoch = {
      epoch: u64(field(a, 0)), baseKey: new ArrayBuffer(16), ownLeaf: u53(field(a, 2)),
      roster: roster.map((row) => { const r = arr(row, 3); return { leaf: u53(field(r, 0)), deviceId: bin(field(r, 1), 16), userId: bin(field(r, 2), 16) }; }),
      code,
    };
    new Uint8Array(out.baseKey).set(key);
    return out;
  } catch (err) {
    if (err instanceof CborError || err instanceof RangeError) throw new CoreError('E_CORE_DECODE', `callMediaEpoch: ${err.message}`);
    throw err;
  } finally {
    key?.fill(0);
    bytes.fill(0);
  }
}

export interface CoreHandle {
      capacity(): number; reserve_capacity(n: number): Promise<void>;
      pause(): void; resume(): Promise<void>; close(): void;
      identity(): Uint8Array;
      signup_begin(instance_id: Uint8Array): string;
      signup_request(invite: string, username: string, display: string, password?: string | null): Uint8Array;
      signup_complete(user_id: Uint8Array, username: string, now: bigint, instance_signer_pub: Uint8Array): Uint8Array;
      instance_signer_key_matches(observed_key: Uint8Array): void;
      instance_signer_key_pin(instance_id: Uint8Array, observed_key: Uint8Array): void;
      signup_reset(): void;
      device_list_body(): Uint8Array; device_list_published(): void; device_list_drop(): void;
      session_sign(nonce: Uint8Array, purpose: number): Uint8Array;
      /** Present after rebuilding the WASM package from a facade that includes moderator removals. */
      sign_removal?(request_id: Uint8Array, group_id: Uint8Array, target_sequence: bigint, expires: bigint, now: bigint): Uint8Array;
      prepare_manual_removal_evidence?(proof_request_id: Uint8Array, evidence_request_id: Uint8Array, community_id: Uint8Array,
        group_id: Uint8Array, channel_id: Uint8Array, message_id: Uint8Array, target_sequence: bigint, reason: string,
        excerpt: string, created_at: bigint, expires: bigint, now: bigint): Uint8Array;
      manual_removal_evidence?(): Uint8Array;
      ensure_manual_removal_evidence_request?(proof_request_id: Uint8Array, candidate_request_id: Uint8Array, now: bigint): Uint8Array;
      mark_manual_removal_submitted?(request_id: Uint8Array): void;
      mark_manual_removal_accepted?(request_id: Uint8Array): void;
      complete_manual_removal_evidence?(request_id: Uint8Array, now: bigint): void;
      reject_unsubmitted_manual_removal?(request_id: Uint8Array, now: bigint): void;
      reject_manual_removal?(request_id: Uint8Array, now: bigint): void;
      prepare_manual_warning_evidence?(request_id: Uint8Array, evidence_request_id: Uint8Array, community_id: Uint8Array,
        target_user_id: Uint8Array, reason: string, created_at: bigint, now: bigint): void;
      manual_warning_evidence?(): Uint8Array;
      mark_manual_warning_accepted?(request_id: Uint8Array, action_id: Uint8Array, revision: bigint, accepted_at: bigint): void;
      ensure_manual_warning_evidence_request?(request_id: Uint8Array, candidate_request_id: Uint8Array, now: bigint): Uint8Array;
      complete_manual_warning_evidence?(request_id: Uint8Array, now: bigint): void;
      session_store(token: string, expires: bigint, idle_expires: bigint): void;
      session(): Uint8Array; session_clear(): void;
      key_packages(count: number, last_resort: boolean): Uint8Array;
      sealed_objects(): Uint8Array;
      groups(): Uint8Array;
      group_heal_snapshot?(group_id: Uint8Array): Uint8Array;
      group_create(group_id: Uint8Array, community_id: Uint8Array | null | undefined, channel_id: Uint8Array, policy_version: bigint, external_sender_pub: Uint8Array): Uint8Array;
      group_registered(group_id: Uint8Array, next_seq: bigint): void;
      group_discard(group_id: Uint8Array): void;
      group_join_external(group_id: Uint8Array, community_id: Uint8Array | null | undefined, channel_id: Uint8Array, policy_version: bigint, info_body: Uint8Array, tree_body: Uint8Array): Uint8Array;
      group_joined(group_id: Uint8Array, seq: bigint): void;
      welcomes_apply(welcomes_body: Uint8Array, expected: Uint8Array): Uint8Array;
      human_interaction_welcome_apply(welcomes_body: Uint8Array, current_context_proof: Uint8Array, now: bigint): Uint8Array;
      human_interaction_send_prepare(request_id: Uint8Array, community_id: Uint8Array, group_id: Uint8Array, request_body: Uint8Array, now: bigint): Uint8Array;
      human_interaction_events_read(after: bigint, limit: bigint): Uint8Array;
      human_interaction_receipts_read(community_id: Uint8Array, installation_id: Uint8Array, bot_user_id: Uint8Array, request_ids: Uint8Array): Uint8Array;
      human_interaction_events_ack(through: bigint): void;
      human_interaction_events_ack_selected(delivery_ids: Uint8Array): void;
      group_apply(group_id: Uint8Array, handshakes: Uint8Array, messages: Uint8Array, through: bigint): Uint8Array;
      commit_build(group_id: Uint8Array, proposals_body: Uint8Array): Uint8Array;
      commit_confirm(group_id: Uint8Array): Uint8Array;
      commit_abort(group_id: Uint8Array): void;
      cursor_body(group_id: Uint8Array): Uint8Array;
      cursor_acked(group_id: Uint8Array, last_seq: bigint, last_epoch: bigint): void;
      message_deleted(group_id: Uint8Array, seq: bigint): Uint8Array;
      removal_cursor(group_id: Uint8Array): bigint;
      removal_actor_history_version(group_id: Uint8Array, revision: bigint, proof: Uint8Array, signature: Uint8Array, accepted_at: bigint): bigint;
      refresh_removal_actor_history(group_id: Uint8Array, revision: bigint, proof: Uint8Array, signature: Uint8Array, accepted_at: bigint, history: Uint8Array, head_list: Uint8Array): bigint;
      ingest_removal(group_id: Uint8Array, revision: bigint, proof: Uint8Array, signature: Uint8Array, accepted_at: bigint): bigint;
      send_prepare(group_id: Uint8Array, request: Uint8Array, now: bigint): Uint8Array;
      send_encrypt(msg_id: Uint8Array): Uint8Array;
      send_confirm(msg_id: Uint8Array, response: Uint8Array): Uint8Array;
      send_requeue(msg_id: Uint8Array): void; send_fail(msg_id: Uint8Array, error: string): void;
      send_retry(msg_id: Uint8Array): void; send_discard(msg_id: Uint8Array): void;
      outbox(group_id: Uint8Array): Uint8Array;
      timeline(group_id: Uint8Array, before_seq: bigint, limit: number): Uint8Array;
      thread_timeline(group_id: Uint8Array, root_msg_id: Uint8Array, before_seq: bigint, limit: number): Uint8Array;
      threads(group_id: Uint8Array): Uint8Array;
      group_row(group_id: Uint8Array): Uint8Array;
      mark_read(group_id: Uint8Array, seq: bigint, now: bigint): void;
      activity(): Uint8Array;
      settings(): Uint8Array; setting_put(k: string, v: string): void; setting_delete(k: string): void;
      enrol_begin(instance_id: Uint8Array): Uint8Array;
      enrol_session_sign(nonce: Uint8Array, login: Uint8Array): Uint8Array;
      enrol_registered(user_id: Uint8Array): void;
      recovery_key_check(recovery_key: string): void;
      enrol_complete(recovery_key: string, root_sealed: Uint8Array, state_sealed: Uint8Array, list_body: Uint8Array, username: string, now: bigint): Uint8Array;
      enrol_reset(): void;
      device_list_revoke(recovery_key: string, root_sealed: Uint8Array, state_sealed: Uint8Array, list_body: Uint8Array, device_ids: Uint8Array, now: bigint): Uint8Array;
      own_device_list_update(history_body: Uint8Array): Uint8Array;
      own_device_list(): Uint8Array;
      pending_leaves(): Uint8Array;
      proposed_adds(group_id: Uint8Array, proposals_body: Uint8Array): Uint8Array;
      device_list_ingest_add(group_id: Uint8Array, proposals_body: Uint8Array, index: bigint, requested_user: Uint8Array,
        history_body: Uint8Array, head_list: Uint8Array, now: bigint): Uint8Array;
      device_list_ingest(group_id: Uint8Array, leaf_index: number, requested_user: Uint8Array, history_body: Uint8Array, head_list: Uint8Array, now: bigint): Uint8Array;
      account_kind_ingest(user_id: Uint8Array, attestation: Uint8Array): void;
      revalidate_pending(group_id: Uint8Array, leaf_index: number, limit: number): number;
      trust_state(): Uint8Array;
      trust_generation(): bigint;
      trust_verify(user_id: Uint8Array, expected_umk: Uint8Array): void;
      safety_number_for(user_id: Uint8Array, expected_umk: Uint8Array): string;
      send_block_code(group_id: Uint8Array): string | undefined;
      send_block_own(group_id: Uint8Array): boolean;
      search(scope_kind: number, scope_id: Uint8Array | null, query: string, before: Uint8Array | null, limit: number): Uint8Array;
      state_sealed_uploaded(): void;
      state_sealed_current(): boolean;
      pins(group_id: Uint8Array): Uint8Array;
      attachment_get(group_id: Uint8Array, seq: bigint, index: number): Uint8Array;
      purges(): Uint8Array;
      purge_done(group_id: Uint8Array, seq: bigint): void;
      own_roles_set(community_id: Uint8Array, role_ids: Uint8Array): void;
      authority_set(group_id: Uint8Array, pinners: Uint8Array, broadcasters: Uint8Array, mentionable: Uint8Array): void;
      call_group_create(group_id: Uint8Array, community_id: Uint8Array | null | undefined, channel_id: Uint8Array, policy_version: bigint, external_sender_pub: Uint8Array): Uint8Array;
      call_group_join_external(group_id: Uint8Array, community_id: Uint8Array | null | undefined, channel_id: Uint8Array, policy_version: bigint, info_body: Uint8Array, tree_body: Uint8Array): Uint8Array;
      call_media_epoch(group_id: Uint8Array): Uint8Array;
      call_join_check(group_id: Uint8Array): void;
      call_leave_proposal(group_id: Uint8Array): Uint8Array;
      call_rotate(group_id: Uint8Array): Uint8Array;
      call_group_forget(group_id: Uint8Array): void;
    }
    export interface StoreOpenConfigLike { directory: string; db_name: string; kek_hex: string; }
    export interface CoreWasmModule {
      core_open(cfg: StoreOpenConfigLike): Promise<CoreHandle>;
      StoreOpenConfig: new (directory: string, db_name: string, kek_hex: string) => StoreOpenConfigLike;
      is_sah_contention(err: unknown): boolean;
      probe_persistence(): Promise<string>;
    }

function call<T>(fn: () => T): T {
  try { return fn(); } catch (err) { throw CoreError.from(err); }
}
function decoded<T>(method: string, bytes: Uint8Array, read: (v: CborValue) => T): T {
  try { return read(decode(bytes)); }
  catch (err) {
    if (err instanceof CborError || err instanceof RangeError) throw new CoreError('E_CORE_DECODE', `${method}: ${err.message}`);
    throw err;
  }
}
function field(a: CborValue[], i: number): CborValue { return a[i] ?? null; }
function oneOf<T extends number>(v: CborValue, values: readonly T[]): T {
  const n = u53(v);
  if (!values.includes(n as T)) throw new CborError('E_CBOR_RANGE', `unexpected value ${n}`);
  return n as T;
}
function readTrustIngest(bytes: Uint8Array): { version: bigint; changed: boolean; quarantined: number } {
  try {
    const a = arr(decode(bytes), 3);
    const changed = u64(a[1]);
    if (changed !== 0n && changed !== 1n) throw new CborError('E_CBOR_RANGE', 'changed bit');
    return { version: u64(a[0]), changed: changed === 1n, quarantined: u53(a[2]) };
  } catch (error) { throw new CoreError('E_CORE_WASM', String(error)); }
}
function readIdentity(v: CborValue): IdentityInfo {
  const a = arr(v, 6);
  return { phase: oneOf(field(a, 0), [0, 1, 2, 3]), instanceId: opt(field(a, 1), (x) => bin(x, 16)),
    userId: opt(field(a, 2), (x) => bin(x, 16)), deviceId: opt(field(a, 3), (x) => bin(x, 16)),
    username: str(field(a, 4)), listPublished: u64(field(a, 5)) === 1n };
}
function readSession(v: CborValue): SessionRecord | null {
  if (v === null) return null;
  const a = arr(v, 3);
  return { token: str(field(a, 0)), expires: u64(field(a, 1)), idleExpires: u64(field(a, 2)) };
}
function readGroup(v: CborValue): GroupInfo {
  const a = arr(v, 9);
  return { groupId: bin(field(a, 0), 16), kind: u53(field(a, 1)), communityId: opt(field(a, 2), (x) => bin(x, 16)),
    targetId: bin(field(a, 3), 16), state: oneOf(field(a, 4), [0, 1, 2, 3, 4]), epoch: u64(field(a, 5)),
    nextSeq: u64(field(a, 6)), proposalsPending: u53(field(a, 7)), pendingCommit: u64(field(a, 8)) === 1n };
}
function readApply(v: CborValue): ApplyResult {
  const a = arr(v, 6); const flags = u64(field(a, 5));
  return { state: oneOf(field(a, 0), [0, 1, 2, 3, 4]), epoch: u64(field(a, 1)), nextSeq: u64(field(a, 2)),
    newSeqs: arr(field(a, 3)).map(u64), proposalsPending: u53(field(a, 4)),
    epochChanged: (flags & 1n) !== 0n, ownAdopted: (flags & 2n) !== 0n };
}
function readWelcome(v: CborValue): WelcomeOutcome {
  const a = arr(v, 4);
  return { welcomeId: u64(field(a, 0)), groupId: bin(field(a, 1), 16), outcome: oneOf(field(a, 2), [0, 1, 2, 3]), reason: str(field(a, 3)) };
}
function readOutbox(v: CborValue): OutboxRow {
  const a = arr(v, 8);
  return { msgId: bin(field(a, 0), 16), state: oneOf(field(a, 1), [0, 1, 2]), error: str(field(a, 2)), created: u64(field(a, 3)), body: str(field(a, 4)),
    type: oneOf(field(a, 5), [0, 1, 2, 3, 4, 5, 6]), replyTo: opt(field(a, 6), (x) => bin(x, 16)),
    attachments: arr(field(a, 7)).map((x) => { const b = arr(x, 4); return { blobId: bin(field(b, 0), 32), size: u53(field(b, 1)),
      mime: str(field(b, 2)), name: str(field(b, 3)) }; }) };
}
/** A pin row: 8 elements since web3.7 fix round 1 (the 8th the thread root); 7 from a core before it. */
function readPin(v: CborValue): PinRow {
  const a = arr(v);
  if (a.length !== 7 && a.length !== 8) throw new CborError('E_CBOR_SHAPE', `pin row of ${String(a.length)} elements`);
  return { targetSeq: u64(field(a, 0)), msgId: bin(field(a, 1), 16), pinnedSeq: u64(field(a, 2)), byUser: bin(field(a, 3), 16),
    author: opt(field(a, 4), (x) => bin(x, 16)), excerpt: str(field(a, 5)), targetTs: u64(field(a, 6)),
    threadRoot: opt(field(a, 7), (x) => bin(x, 16)) };
}
function readDescriptor(v: CborValue): AttachmentDescriptor {
  const a = arr(v, 9);
  return { blobId: bin(field(a, 0), 32), key: bin(field(a, 1), 32), nonce: bin(field(a, 2), 12), size: u53(field(a, 3)),
    mime: str(field(a, 4)), w: opt(field(a, 5), u53), h: opt(field(a, 6), u53), thumb: opt(field(a, 7), bin), name: str(field(a, 8)) };
}
function readPurge(v: CborValue): PurgeRow {
  const a = arr(v, 4);
  return { groupId: bin(field(a, 0), 16), seq: u64(field(a, 1)), channelId: bin(field(a, 2), 16),
    blobIds: arr(field(a, 3)).map((x) => bin(x, 32)) };
}
/** A timeline row: 19 elements since web3.7 (the 19th the thread summary); 18 from a core before it, read as no thread. */
function readTimeline(v: CborValue): TimelineRow {
  const a = arr(v);
  if (a.length !== 18 && a.length !== 19) throw new CborError('E_CBOR_SHAPE', `timeline row of ${String(a.length)} elements`);
  return { seq: u64(field(a, 0)), epoch: u64(field(a, 1)), recvTs: u64(field(a, 2)), status: oneOf(field(a, 3), [0, 1, 2]),
    reason: str(field(a, 4)), senderUser: opt(field(a, 5), (x) => bin(x, 16)), senderDevice: bin(field(a, 6), 16),
    senderKind: opt(field(a, 7), u53), senderTier: opt(field(a, 8), u53), msgId: opt(field(a, 9), (x) => bin(x, 16)),
    type: opt(field(a, 10), u53), body: str(field(a, 11)), editedSeq: u64(field(a, 12)),
    reply: opt(field(a, 13), readReply), reactions: arr(field(a, 14)).map(readReaction),
    pinned: oneOf(field(a, 15), [0, 1]) === 1, attachments: arr(field(a, 16)).map(readAttachmentInfo),
    mention: oneOf(field(a, 17), [0, 1]) === 1, thread: opt(field(a, 18), readThreadSummary) };
}
function readThreadSummary(v: CborValue): ThreadSummary {
  const a = arr(v, 2);
  return { count: u53(field(a, 0)), lastTs: u64(field(a, 1)) };
}
function readThreadTimeline(v: CborValue): { root: TimelineRow | null; rows: TimelineRow[] } {
  const a = arr(v, 2);
  return { root: opt(field(a, 0), readTimeline), rows: arr(field(a, 1)).map(readTimeline) };
}
function readThread(v: CborValue): ThreadRow {
  const a = arr(v, 5);
  return { rootMsgId: bin(field(a, 0), 16), rootSeq: opt(field(a, 1), u64), count: u53(field(a, 2)), lastTs: u64(field(a, 3)),
    excerpt: str(field(a, 4)) };
}
/** A reply reference: 6 elements since web3.7 fix round 1 (the 6th the thread root); 5 from a core before it. */
function readReply(v: CborValue): ReplyInfo {
  const a = arr(v);
  if (a.length !== 5 && a.length !== 6) throw new CborError('E_CBOR_SHAPE', `reply of ${String(a.length)} elements`);
  return { replyTo: bin(field(a, 0), 16), targetSeq: opt(field(a, 1), u64), targetUser: opt(field(a, 2), (x) => bin(x, 16)),
    excerpt: str(field(a, 3)), state: oneOf(field(a, 4), [0, 1, 2]), threadRoot: opt(field(a, 5), (x) => bin(x, 16)) };
}
function readReaction(v: CborValue): ReactionInfo {
  const a = arr(v, 3);
  return { emoji: str(field(a, 0)), count: u53(field(a, 1)), mine: oneOf(field(a, 2), [0, 1]) === 1 };
}
function readAttachmentInfo(v: CborValue): AttachmentInfo {
  const a = arr(v, 7);
  return { index: u53(field(a, 0)), size: u53(field(a, 1)), mime: str(field(a, 2)), w: opt(field(a, 3), u53),
    h: opt(field(a, 4), u53), hasThumb: oneOf(field(a, 5), [0, 1]) === 1, name: str(field(a, 6)) };
}
function readActivity(v: CborValue): ActivityRow {
  const a = arr(v, 6);
  return { groupId: bin(field(a, 0), 16), unread: u53(field(a, 1)), mentions: u53(field(a, 2)),
    lastSeq: u64(field(a, 3)), lastTs: u64(field(a, 4)), lastReadSeq: u64(field(a, 5)) };
}
function readEntry(v: CborValue): DeviceEntryInfo {
  const a = arr(v, 5);
  return { deviceId: bin(field(a, 0), 16), dskPub: bin(field(a, 1), 32), tier: oneOf(field(a, 2), [0, 1]),
    addedAt: u64(field(a, 3)), revokedAt: opt(field(a, 4), u64) };
}
function readOwnList(v: CborValue): OwnDeviceList {
  const a = arr(v, 3);
  return { version: u64(field(a, 0)), published: oneOf(field(a, 1), [0, 1]) === 1,
    entries: arr(field(a, 2)).map(readEntry) };
}
function readSealed(v: CborValue): SealedObjects {
  const a = arr(v, 3);
  return { root: opt(field(a, 0), bin), state: opt(field(a, 1), bin), stateUploaded: oneOf(field(a, 2), [0, 1]) === 1 };
}
function readSigned(v: CborValue): SignedLists {
  const a = arr(v, 3);
  return { deviceListBody: bin(field(a, 0)), stateSealed: bin(field(a, 1)), interrupted: opt(field(a, 2), bin) };
}

export function wrapCore(handle: CoreHandle): CorePort {
  return {
    pause: () => call(() => handle.pause()),
    resume: () => call(async () => { try { await handle.resume(); } catch (err) { throw CoreError.from(err); } }),
    close: () => call(() => handle.close()),
    identity: () => call(() => decoded('identity', handle.identity(), readIdentity)),
    signupBegin: (instanceId) => call(() => handle.signup_begin(instanceId)),
    signupRequest: (invite, username, display, password) => call(() => handle.signup_request(invite, username, display, password)),
    signupComplete: (userId, username, now, instanceSignerPub) => call(() => handle.signup_complete(userId, username, now, instanceSignerPub)),
    instanceSignerKeyMatches: (observedKey) => call(() => handle.instance_signer_key_matches(observedKey)),
    instanceSignerKeyPin: (instanceId, observedKey) => call(() => handle.instance_signer_key_pin(instanceId, observedKey)),
    signupReset: () => call(() => handle.signup_reset()),
    deviceListBody: () => call(() => handle.device_list_body()),
    deviceListPublished: () => call(() => handle.device_list_published()),
    deviceListDrop: () => call(() => handle.device_list_drop()),
    sessionSign: (nonce, purpose) => call(() => handle.session_sign(nonce, purpose)),
    signRemoval: (requestId, groupId, targetSeq, expires, now) => call(() => {
      if (handle.sign_removal === undefined) throw new CoreError('E_CORE_STATE', 'moderation signing is not available in this WASM build');
      return handle.sign_removal(requestId, groupId, targetSeq, expires, now);
    }),
    prepareManualRemovalEvidence: (proofRequestId, evidenceRequestId, communityId, groupId, channelId, messageId, targetSeq, reason, excerpt, createdAt, expires, now) => call(() => {
      if (handle.prepare_manual_removal_evidence === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      return handle.prepare_manual_removal_evidence(proofRequestId, evidenceRequestId, communityId, groupId, channelId, messageId, targetSeq, reason, excerpt, createdAt, expires, now);
    }),
    manualRemovalEvidence: () => call(() => {
      if (handle.manual_removal_evidence === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      return decoded('manualRemovalEvidence', handle.manual_removal_evidence(), (v) => arr(v).map((row) => {
        const a = arr(row, 16); const state = u53(a[12] ?? null);
        if (state > 2) throw new CoreError('E_CORE_DECODE', 'manual removal evidence state is invalid');
        return { proofRequestId: bin(a[0] ?? null, 16), evidenceRequestId: bin(a[1] ?? null, 16),
          actorUserId: bin(a[2] ?? null, 16), actorDeviceId: bin(a[3] ?? null, 16), communityId: bin(a[4] ?? null, 16),
          groupId: bin(a[5] ?? null, 16), channelId: bin(a[6] ?? null, 16), messageId: bin(a[7] ?? null, 16),
          targetSequence: u64(a[8] ?? null), reason: str(a[9] ?? null), excerpt: str(a[10] ?? null), signedBody: bin(a[11] ?? null),
          state: state as 0 | 1 | 2, createdAt: u64(a[13] ?? null), expires: u64(a[14] ?? null), evidenceExpires: u64(a[15] ?? null) };
      }));
    }),
    ensureManualRemovalEvidenceRequest: (proofRequestId, candidateRequestId, now) => call(() => {
      if (handle.ensure_manual_removal_evidence_request === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      return decoded('ensureManualRemovalEvidenceRequest', handle.ensure_manual_removal_evidence_request(proofRequestId, candidateRequestId, now), (v) => {
        const a = arr(v, 2); return { requestId: bin(a[0] ?? null, 16), expires: u64(a[1] ?? null) };
      });
    }),
    markManualRemovalSubmitted: (requestId) => call(() => {
      if (handle.mark_manual_removal_submitted === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.mark_manual_removal_submitted(requestId);
    }),
    markManualRemovalAccepted: (requestId) => call(() => {
      if (handle.mark_manual_removal_accepted === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.mark_manual_removal_accepted(requestId);
    }),
    completeManualRemovalEvidence: (requestId, now) => call(() => {
      if (handle.complete_manual_removal_evidence === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.complete_manual_removal_evidence(requestId, now);
    }),
    rejectUnsubmittedManualRemoval: (requestId, now) => call(() => {
      if (handle.reject_unsubmitted_manual_removal === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.reject_unsubmitted_manual_removal(requestId, now);
    }),
    rejectManualRemoval: (requestId, now) => call(() => {
      if (handle.reject_manual_removal === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.reject_manual_removal(requestId, now);
    }),
    prepareManualWarningEvidence: (requestId, evidenceRequestId, communityId, targetUserId, reason, createdAt, now) => call(() => {
      if (handle.prepare_manual_warning_evidence === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.prepare_manual_warning_evidence(requestId, evidenceRequestId, communityId, targetUserId, reason, createdAt, now);
    }),
    manualWarningEvidence: () => call(() => {
      if (handle.manual_warning_evidence === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      return decoded('manualWarningEvidence', handle.manual_warning_evidence(), (v) => arr(v).map((row) => {
        const a = arr(row, 13); const state = u53(a[7] ?? null);
        if (state > 1) throw new CoreError('E_CORE_DECODE', 'manual warning evidence state is invalid');
        return { requestId: bin(a[0] ?? null, 16), evidenceRequestId: bin(a[1] ?? null, 16),
          actorUserId: bin(a[2] ?? null, 16), actorDeviceId: bin(a[3] ?? null, 16), communityId: bin(a[4] ?? null, 16),
          targetUserId: bin(a[5] ?? null, 16), reason: str(a[6] ?? null), state: state as 0 | 1, createdAt: u64(a[8] ?? null),
          actionId: bin(a[9] ?? null, 16), revision: u64(a[10] ?? null), acceptedAt: u64(a[11] ?? null), evidenceExpires: u64(a[12] ?? null) };
      }));
    }),
    markManualWarningAccepted: (requestId, actionId, revision, acceptedAt) => call(() => {
      if (handle.mark_manual_warning_accepted === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.mark_manual_warning_accepted(requestId, actionId, revision, acceptedAt);
    }),
    ensureManualWarningEvidenceRequest: (requestId, candidateRequestId, now) => call(() => {
      if (handle.ensure_manual_warning_evidence_request === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      return decoded('ensureManualWarningEvidenceRequest', handle.ensure_manual_warning_evidence_request(requestId, candidateRequestId, now), (v) => {
        const a = arr(v, 2); return { requestId: bin(a[0] ?? null, 16), expires: u64(a[1] ?? null) };
      });
    }),
    completeManualWarningEvidence: (requestId, now) => call(() => {
      if (handle.complete_manual_warning_evidence === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
      handle.complete_manual_warning_evidence(requestId, now);
    }),
    sessionStore: (s) => call(() => handle.session_store(s.token, s.expires, s.idleExpires)),
    session: () => call(() => decoded('session', handle.session(), readSession)),
    sessionClear: () => call(() => handle.session_clear()),
    keyPackages: (count, lastResort) => call(() => handle.key_packages(count, lastResort)),
    groups: () => call(() => decoded('groups', handle.groups(), (v) => arr(v).map(readGroup))),
    groupHealSnapshot: (groupId) => call(() => {
      if (handle.group_heal_snapshot === undefined) throw new CoreError('E_CORE_STATE', 'group healing is not available in this WASM build');
      return handle.group_heal_snapshot(groupId);
    }),
    groupCreate: (groupId, communityId, channelId, policyVersion, externalSenderPub) => call(() => handle.group_create(groupId, communityId, channelId, policyVersion, externalSenderPub)),
    groupRegistered: (groupId, nextSeq) => call(() => handle.group_registered(groupId, nextSeq)),
    groupDiscard: (groupId) => call(() => handle.group_discard(groupId)),
    groupJoinExternal: (g, infoBody, treeBody) => call(() => handle.group_join_external(g.groupId, g.communityId, g.channelId, g.policyVersion, infoBody, treeBody)),
    groupJoined: (groupId, seq) => call(() => handle.group_joined(groupId, seq)),
    welcomesApply: (welcomesBody, expected) => call(() => decoded('welcomesApply', handle.welcomes_apply(welcomesBody,
      encode(expected.map((e) => [e.groupId, e.communityId, e.channelId, e.policyVersion]))), (v) => arr(v).map(readWelcome))),
    humanInteractionWelcomeApply: (welcomesBody, currentContextProof, now) => call(() => decoded(
      'humanInteractionWelcomeApply',
      handle.human_interaction_welcome_apply(welcomesBody, currentContextProof, now),
      (v) => arr(v).map(readWelcome),
    )),
    humanInteractionSendPrepare: (requestId, communityId, groupId, requestBody, now) => call(() => decoded(
      'humanInteractionSendPrepare',
      handle.human_interaction_send_prepare(requestId, communityId, groupId, requestBody, now),
      (v) => bin(field(arr(v, 1), 0), 16),
    )),
    humanInteractionEventsRead: (after, limit) => call(() => decoded(
      'humanInteractionEventsRead', handle.human_interaction_events_read(after, BigInt(limit)), (v) => {
        const page = arr(v, 3);
        return {
          events: arr(page[0]).map((item) => {
            const row = arr(item, 13);
            return {
              deliveryId: u64(row[0]), groupId: bin(row[1], 16), communityId: bin(row[2], 16),
              requestId: bin(row[3], 16), sourceSeq: u64(row[4]), epoch: u64(row[5]), receivedAt: u64(row[6]),
              senderUser: bin(row[7], 16), senderDevice: bin(row[8], 16), messageId: bin(row[9], 16), body: str(row[10]),
              operation: u53(row[11]), channelId: opt(row[12], v => bin(v, 16)),
            };
          }),
          next: u64(page[1]), highWater: u64(page[2]),
        };
      },
    )),
    humanInteractionReceiptsRead: (communityId, installationId, botUserId, requestIds) => call(() => decoded(
      'humanInteractionReceiptsRead', handle.human_interaction_receipts_read(communityId, installationId, botUserId, encode(requestIds)),
      (v) => arr(v).map((item) => {
        const row = arr(item, 13);
        return {
          deliveryId: u64(row[0]), groupId: bin(row[1], 16), communityId: bin(row[2], 16),
          requestId: bin(row[3], 16), sourceSeq: u64(row[4]), epoch: u64(row[5]), receivedAt: u64(row[6]),
          senderUser: bin(row[7], 16), senderDevice: bin(row[8], 16), messageId: bin(row[9], 16), body: str(row[10]),
          operation: u53(row[11]), channelId: opt(row[12], v => bin(v, 16)),
        };
      }),
    )),
    humanInteractionEventsAck: (through) => call(() => handle.human_interaction_events_ack(through)),
    humanInteractionEventsAckSelected: (deliveryIds) => call(() => handle.human_interaction_events_ack_selected(encode(deliveryIds))),
    groupApply: (groupId, handshakes, messages, through) => call(() => decoded('groupApply', handle.group_apply(groupId, handshakes, messages, through), readApply)),
    commitBuild: (groupId, proposalsBody) => call(() => handle.commit_build(groupId, proposalsBody)),
    commitConfirm: (groupId) => call(() => decoded('commitConfirm', handle.commit_confirm(groupId), readApply)),
    commitAbort: (groupId) => call(() => handle.commit_abort(groupId)),
    cursorBody: (groupId) => call(() => {
      const bytes = handle.cursor_body(groupId);
      return decoded('cursorBody', bytes, (v) => v === null ? null : bytes);
    }),
    cursorAcked: (groupId, lastSeq, lastEpoch) => call(() => handle.cursor_acked(groupId, lastSeq, lastEpoch)),
    messageDeleted: (groupId, seq) => call(() => decoded('messageDeleted', handle.message_deleted(groupId, seq), readApply)),
    removalCursor: (groupId) => call(() => handle.removal_cursor(groupId)),
    removalActorHistoryVersion: (groupId, entry) => call(() => handle.removal_actor_history_version(groupId, entry.revision, entry.proof, entry.signature, entry.acceptedAt)),
    refreshRemovalActorHistory: (groupId, entry, history, headList) => call(() => handle.refresh_removal_actor_history(groupId, entry.revision, entry.proof, entry.signature, entry.acceptedAt, history, headList)),
    ingestRemoval: (groupId, entry) => call(() => handle.ingest_removal(groupId, entry.revision, entry.proof, entry.signature, entry.acceptedAt)),
    sendPrepare: (groupId, request, now) => call(() => decoded('sendPrepare', handle.send_prepare(groupId,
      encode([request.type, request.replyTo, request.body, request.attachments.map((a) => [a.blobId, a.key, a.nonce, a.size, a.mime, a.w, a.h, a.thumb, a.name]),
        request.threadId ?? null]),
      now), (v) => bin(field(arr(v, 1), 0), 16))),
    sendEncrypt: (msgId) => call(() => decoded('sendEncrypt', handle.send_encrypt(msgId), (v) => {
      const a = arr(v, 2); return { groupId: bin(field(a, 0), 16), messageBody: bin(field(a, 1)) };
    })),
    sendConfirm: (msgId, response) => call(() => decoded('sendConfirm', handle.send_confirm(msgId, response), (v) => {
      const a = arr(v, 2); return { groupId: bin(field(a, 0), 16), seq: u64(field(a, 1)) };
    })),
    sendRequeue: (msgId) => call(() => handle.send_requeue(msgId)),
    sendFail: (msgId, error) => call(() => handle.send_fail(msgId, error)),
    sendRetry: (msgId) => call(() => handle.send_retry(msgId)),
    sendDiscard: (msgId) => call(() => handle.send_discard(msgId)),
    outbox: (groupId) => call(() => decoded('outbox', handle.outbox(groupId), (v) => arr(v).map(readOutbox))),
    timeline: (groupId, beforeSeq, limit) => call(() => decoded('timeline', handle.timeline(groupId, beforeSeq, limit), (v) => arr(v).map(readTimeline))),
    threadTimeline: (groupId, rootMsgId, beforeSeq, limit) => call(() => decoded('threadTimeline',
      handle.thread_timeline(groupId, rootMsgId, beforeSeq, limit), readThreadTimeline)),
    threads: (groupId) => call(() => decoded('threads', handle.threads(groupId), (v) => arr(v).map(readThread))),
    groupRow: (groupId) => call(() => decoded('groupRow', handle.group_row(groupId), (v) => v === null ? null : readGroup(v))),
    markRead: (groupId, seq, now) => call(() => handle.mark_read(groupId, seq, now)),
    activity: () => call(() => decoded('activity', handle.activity(), (v) => arr(v).map(readActivity))),
    pins: (groupId) => call(() => decoded('pins', handle.pins(groupId), (v) => arr(v).map(readPin))),
    attachmentGet: (groupId, seq, index) => call(() => decoded('attachmentGet', handle.attachment_get(groupId, seq, index), readDescriptor)),
    purges: () => call(() => decoded('purges', handle.purges(), (v) => arr(v).map(readPurge))),
    purgeDone: (groupId, seq) => call(() => handle.purge_done(groupId, seq)),
    ownRolesSet: (communityId, roleIds) => call(() => {
      if (roleIds.length > 64 || roleIds.some((id) => id.length !== 16))
        throw new CoreError('E_CORE_INPUT', 'role_ids must be 0..=64 ids of 16 bytes');
      const joined = new Uint8Array(roleIds.length * 16);
      roleIds.forEach((id, index) => joined.set(id, index * 16));
      handle.own_roles_set(communityId, joined);
    }),
    authoritySet: (groupId, pinners, broadcasters, mentionable) => call(() => {
      const join = (ids: Id[]): Uint8Array => {
        if (ids.length > 4096 || ids.some((id) => id.length !== 16))
          throw new CoreError('E_CORE_INPUT', 'pinners, broadcasters and mentionable must be 0..=4096 ids of 16 bytes');
        const joined = new Uint8Array(ids.length * 16);
        ids.forEach((id, index) => joined.set(id, index * 16));
        return joined;
      };
      const p = join(pinners);
      const b = join(broadcasters);
      const m = join(mentionable);
      handle.authority_set(groupId, p, b, m);
    }),
    settings: () => call(() => decoded('settings', handle.settings(), (v) => Object.fromEntries(arr(v).map((row) => {
      const a = arr(row, 2); return [str(field(a, 0)), str(field(a, 1))];
    })))),
    settingPut: (k, v) => call(() => handle.setting_put(k, v)),
    settingDelete: (k) => call(() => handle.setting_delete(k)),
    sealedObjects: () => call(() => decoded('sealedObjects', handle.sealed_objects(), readSealed)),
    stateSealedUploaded: () => call(() => handle.state_sealed_uploaded()),
    stateSealedCurrent: () => call(() => handle.state_sealed_current()),
    enrolBegin: (instanceId) => call(() => decoded('enrolBegin', handle.enrol_begin(instanceId), (v) => {
      const a = arr(v, 2); return { deviceId: bin(field(a, 0), 16), dskPub: bin(field(a, 1), 32) };
    })),
    enrolSessionSign: (nonce, login) => call(() => handle.enrol_session_sign(nonce, login)),
    enrolRegistered: (userId) => call(() => handle.enrol_registered(userId)),
    recoveryKeyCheck: (recoveryKey) => call(() => handle.recovery_key_check(recoveryKey)),
    enrolComplete: (input) => call(() => decoded('enrolComplete', handle.enrol_complete(input.recoveryKey, input.rootSealed,
      input.stateSealed, input.listBody, input.username, input.now), readSigned)),
    enrolReset: () => call(() => handle.enrol_reset()),
    deviceListRevoke: (input) => call(() => {
      if (input.deviceIds.length < 1 || input.deviceIds.length > 64 || input.deviceIds.some((deviceId) => deviceId.length !== 16))
        throw new CoreError('E_CORE_INPUT', 'device ids must be 1..64 ids of 16 bytes');
      const ids = new Uint8Array(input.deviceIds.length * 16);
      input.deviceIds.forEach((deviceId, index) => ids.set(deviceId, index * 16));
      return decoded('deviceListRevoke', handle.device_list_revoke(input.recoveryKey, input.rootSealed,
        input.stateSealed, input.listBody, ids, input.now), readSigned);
    }),
    ownDeviceListUpdate: (historyBody) => call(() => decoded('ownDeviceListUpdate', handle.own_device_list_update(historyBody), (v) => {
      const a = arr(v, 2); return { version: u64(field(a, 0)), listed: oneOf(field(a, 1), [0, 1]) === 1 };
    })),
    ownDeviceList: () => call(() => decoded('ownDeviceList', handle.own_device_list(), readOwnList)),
    pendingLeaves: () => call(() => decodePendingLeaves(handle.pending_leaves())),
    proposedAdds: (groupId, proposalsBody) => call(() => decoded('proposedAdds', handle.proposed_adds(groupId, proposalsBody), (v) =>
      arr(v).map((row) => { const a = arr(row, 3); return { index: u64(a[0]), userId: bin(a[1], 16), cachedVersion: u64(a[2]) }; }))),
    deviceListIngestAdd: (groupId, proposalsBody, index, requestedUser, history, headList, now) => call(() =>
      readTrustIngest(handle.device_list_ingest_add(groupId, proposalsBody, index, requestedUser, history, headList, now))),
    deviceListIngest: (groupId, leafIndex, requestedUser, history, headList, now) => call(() =>
      readTrustIngest(handle.device_list_ingest(groupId, leafIndex, requestedUser, history, headList, now))),
    accountKindIngest: (userId, attestation) => call(() => handle.account_kind_ingest(userId, attestation)),
    revalidatePending: (groupId, leafIndex, limit) => call(() => handle.revalidate_pending(groupId, leafIndex, limit)),
    trustState: () => call(() => decodeTrustState(handle.trust_state())),
    trustGeneration: () => call(() => handle.trust_generation()),
    trustVerify: (userId, umk) => call(() => {
      if (userId.length !== 16 || umk.length !== 32) throw new CoreError('E_CORE_INPUT');
      handle.trust_verify(userId, umk);
    }),
    safetyNumberFor: (userId, expectedUmk) => call(() => {
      if (userId.length !== 16 || expectedUmk.length !== 32) throw new CoreError('E_CORE_INPUT');
      return handle.safety_number_for(userId, expectedUmk);
    }),
    sendBlockCode: (groupId) => call(() => {
      if (groupId.length !== 16) throw new CoreError('E_CORE_INPUT');
      const code = handle.send_block_code(groupId);
      return code === undefined || code === null ? null : trustBlock(code);
    }),
    sendBlockOwn: (groupId) => call(() => {
      if (groupId.length !== 16) throw new CoreError('E_CORE_INPUT');
      return handle.send_block_own(groupId) === true;
    }),
    search: (scope, query, cursor, limit) => call(() => {
      validateSearch(scope, query, cursor, limit);
      const kind = scope.kind === 'channel' ? 0 : scope.kind === 'community' ? 1 : 2;
      return decodeSearchHits(handle.search(kind, scope.id, query,
        cursor === null ? null : encode([cursor.rank, cursor.groupId, cursor.seq]), limit));
    }),
    callGroupCreate: (groupId, communityId, channelId, policyVersion, externalSenderPub) => call(() =>
      handle.call_group_create(groupId, communityId, channelId, policyVersion, externalSenderPub)),
    callGroupJoinExternal: (g, infoBody, treeBody) => call(() =>
      handle.call_group_join_external(g.groupId, g.communityId, g.channelId, g.policyVersion, infoBody, treeBody)),
    callMediaEpoch: (groupId) => call(() => readCallMediaEpoch(handle.call_media_epoch(groupId))),
    callJoinCheck: (groupId) => call(() => handle.call_join_check(groupId)),
    callLeaveProposal: (groupId) => call(() => handle.call_leave_proposal(groupId)),
    callRotate: (groupId) => call(() => handle.call_rotate(groupId)),
    callGroupForget: (groupId) => call(() => handle.call_group_forget(groupId)),
  };
}
