/**
 * The group-half model of CorePort. It is held to the Rust core by `parity.test.ts` (L-CORE-10);
 * change a rule here only together with the fixture.
 *
 * Test support for src/sync: a modelled delivery service (ModelDs), a core whose group half
 * follows L-CORE-07..09 over a readable stand-in wire (ModelCore), a scripted gateway
 * (FakeGateway) and a manual clock. Nothing here does any cryptography: an application message
 * is a CBOR array that names its sender, which is enough to test ordering, adoption, commits and
 * joins. Words of the copy lint (scripts/check-ui-copy.mjs) appear nowhere in this file.
 */
import { arr, bin, decode, encode, str, u53, u64, type CborInput, type CborValue } from '../../cbor';
import { decodeBoundedArray } from '../../state/bounded';
import {
  CoreError,
  type ApplyResult,
  type CorePort,
  type ExpectedGroup,
  type GroupInfo,
  type Id,
  type IdentityInfo,
  type OutboxRow,
  type AttachmentDescriptor,
  type PinRow,
  type PurgeRow,
  type SendRequest,
  type ThreadRow,
  type TimelineRow,
  type WelcomeOutcome,
  type ActivityRow,
  type OwnDeviceList,
  type SealedObjects,
  type SignedLists,
  type PendingLeaf, type TrustRecord, type SearchHit, type TrustBlock, trustBlock,
  type CallMediaEpoch, readCallMediaEpoch,
  type HumanInteractionEventPage,
  type HumanInteractionEvent,
} from '../../core-port';
import { FAILED_LEAF } from '../errors';
import type { Frame } from '../../gateway/frames';
import type { GatewayEvent, ReadyInfo } from '../../gateway/gateway';
import { toHex } from '../../hex';
import { DillaHttpError } from '../../http/errors';
import { CHANNEL_NAME_MAX_BYTES, COMMUNITY_NAME_MAX_BYTES, OWNER_TOP, nameProblem, type NewOverwrite, type OverwriteRow, type RoleRow, type Routes } from '../../http/routes';
import { mentionsMe } from '../../state/mentions';
import { entryKey, fakeDskPub, fakeListBlob, fakeListNames, readFakeList, type FakeList, type FakeListEntry } from '../../testing/fake-list';

/** The plain message request web-2a sent with a bare body (L-TS-30). */
export function textRequest(body: string): SendRequest {
  return { type: 0, replyTo: null, body, attachments: [], threadId: null };
}

export interface Peer {
  device: Id;
  user: Id;
}

/** A 16-byte id: tag in byte 0, n (0..65535) in bytes 14-15. */
export function idOf(tag: number, n: number): Id {
  const b = new Uint8Array(16);
  b[0] = tag;
  b[14] = (n >> 8) & 0xff;
  b[15] = n & 0xff;
  return b;
}

export const ME: Peer = { device: idOf(0xd0, 1), user: idOf(0xe0, 1) };
export const PEER: Peer = { device: idOf(0xd0, 2), user: idOf(0xe0, 2) };
export const THIRD: Peer = { device: idOf(0xd0, 3), user: idOf(0xe0, 3) };
export const FOURTH: Peer = { device: idOf(0xd0, 4), user: idOf(0xe0, 4) };
export const COMMUNITY = idOf(0xc0, 1);
export const CHANNEL = idOf(0xc1, 1);
export const CHANNEL_2 = idOf(0xc1, 2);

export function at<T>(a: readonly T[], i: number): T {
  const v = a[i];
  if (v === undefined) throw new Error(`index ${String(i)} out of range`);
  return v;
}

function sameOverwrite(o: OverwriteRow, channelId: Uint8Array, kind: 0 | 1, targetId: Uint8Array): boolean {
  return same(o.channelId, channelId) && o.kind === kind && same(o.targetId, targetId);
}
function same(a: Uint8Array, b: Uint8Array): boolean {
  return toHex(a) === toHex(b);
}

function bySeq(a: { seq: bigint }, b: { seq: bigint }): number {
  return a.seq < b.seq ? -1 : a.seq > b.seq ? 1 : 0;
}

/** The DillaHttpError constructor of the L-HTTP-20…45 header (task 13); this is its only call site. */
export function httpError(status: number, code: string, retryAfterMs: number | null = null, extra: CborValue[] = []): DillaHttpError {
  return new DillaHttpError({ status, code, detail: '', retryAfterMs, extra });
}

/** The CoreError constructor of L-TS-03 (task 15); this is its only call site. */
export function coreError(code: string, detail = ''): CoreError {
  return new CoreError(code, detail);
}

/** Lets every pending microtask chain run to its end (no fake here waits on a real timer). */
export async function settle(): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise<void>((resolve) => {
      globalThis.setTimeout(resolve, 0);
    });
  }
}

export function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve: () => void = () => undefined;
  const promise = new Promise<void>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** The stand-in tree hash of a group at an epoch: the group id in bytes 0-15, the epoch big-endian in bytes 24-31. */
function treeHashOf(groupId: Id, epoch: bigint): Uint8Array {
  const h = new Uint8Array(32);
  h.set(groupId, 0);
  new DataView(h.buffer).setBigUint64(24, epoch);
  return h;
}

/**
 * The stand-in commitment C of an outbox row's envelope: 0xcc, then the message id. The real C is
 * keyed by the envelope's k_f, fixed when the row is prepared, so it names one outbox row and every
 * upload of it; the message id does the same here.
 */
function commitmentFor(msgId: Id): Uint8Array {
  const c = new Uint8Array(32);
  c[0] = 0xcc;
  c.set(msgId, 1);
  return c;
}

/** The envelope fields of an application message beyond its body (L-TS-31). */
export interface WireOptions {
  type?: number;
  replyTo?: Id | null;
  attachments?: AttachmentDescriptor[];
  /** web3.7: the envelope's thread_id; a non-null one is the stand-in's ninth element. */
  threadId?: Id | null;
}

/** The msg id of the parity fixture's peer target (L-CORE-39). */
export const FOLD_TARGET: Uint8Array = new Uint8Array(16).fill(0x71);
/** A msg id no stored row carries: the target of a fold that is never held. */
export const FOLD_UNHELD: Uint8Array = new Uint8Array(16).fill(0xee);

/** The stand-in wire shared by ModelCore and ModelDs. */
export const wire = {
  /** The 8-element stand-in of an envelope: ['app', user, device, msgId, body, type, replyTo, attachments], and since
   *  web3.7 a ninth element, threadId, when the envelope carries one. */
  app: (from: Peer, msgId: Id, body: string, opts: WireOptions = {}): Uint8Array => encode([
    'app', from.user, from.device, msgId, body, opts.type ?? 0, opts.replyTo ?? null,
    (opts.attachments ?? []).map((a) => [a.blobId, a.key, a.nonce, a.size, a.mime, a.w, a.h, a.thumb, a.name]),
    ...(opts.threadId === undefined || opts.threadId === null ? [] : [opts.threadId]),
  ]),
  commit: (newEpoch: bigint, committer: Id, adds: readonly Id[], removes: readonly Id[]): Uint8Array =>
    encode(['commit', newEpoch, committer, adds, removes]),
  prop: (ref: Id, op: 'add' | 'remove', device: Id): Uint8Array => encode(['prop', ref, op, device]),
  /** A Welcome into `epoch`; `treeHash` is the joined state's (a forged Welcome passes another). */
  welcome: (groupId: Id, epoch: bigint, treeHash: Uint8Array = treeHashOf(groupId, epoch)): Uint8Array =>
    encode(['welcome', groupId, epoch, treeHash]),
  treeHash: treeHashOf,
  commitment: commitmentFor,
  info: (groupId: Id, epoch: bigint): Uint8Array => encode(['info', groupId, epoch]),
  tree: (groupId: Id, epoch: bigint): Uint8Array => encode(['tree', groupId, epoch]),
  binding: (communityId: Id | null, channelId: Id): Uint8Array => encode(['binding', communityId, channelId]),
  /** A call group's binding (kind 1, media version 1): the text binding with the kind appended. */
  callBinding: (communityId: Id | null, channelId: Id): Uint8Array => encode(['binding', communityId, channelId, 1]),
};

/**
 * The commitment an application blob carries (the stand-in of its 32-byte authenticated_data):
 * wire.commitment of its message id, or null for a blob that is not an application message.
 */
export function commitmentOf(blob: Uint8Array): Uint8Array | null {
  try {
    const v = arr(decode(blob));
    if ((v.length !== 8 && v.length !== 9) || str(at(v, 0)) !== 'app') return null;
    return commitmentFor(bin(at(v, 3), 16));
  } catch {
    return null;
  }
}

export class ManualClock {
  private t = 1_700_000_000_000;
  private ids = 0;
  private readonly timers = new Map<number, { at: number; fn: () => void }>();

  readonly now = (): number => this.t;

  readonly setTimeout = (fn: () => void, ms: number): number => {
    this.ids += 1;
    this.timers.set(this.ids, { at: this.t + ms, fn });
    return this.ids;
  };

  readonly clearTimeout = (id: number): void => {
    this.timers.delete(id);
  };

  get pending(): number {
    return this.timers.size;
  }

  /** Moves time forward by ms, firing due timers in order and settling after each. */
  async advance(ms: number): Promise<void> {
    const end = this.t + ms;
    for (;;) {
      let nextId = -1;
      let nextAt = Number.POSITIVE_INFINITY;
      for (const [id, tm] of this.timers) {
        if (tm.at <= end && tm.at < nextAt) {
          nextId = id;
          nextAt = tm.at;
        }
      }
      if (nextId < 0) break;
      const tm = this.timers.get(nextId);
      this.timers.delete(nextId);
      this.t = nextAt;
      tm?.fn();
      await settle();
    }
    this.t = end;
    await settle();
  }
}

export class FakeGateway {
  readonly acks: { group: string; round: bigint }[] = [];
  readonly frames: Frame[] = [];
  private readonly listeners = new Set<(e: GatewayEvent) => void>();

  readonly subscribe = (listener: (e: GatewayEvent) => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  readonly commitAck = (groupId: Uint8Array, round: bigint): void => {
    this.acks.push({ group: toHex(groupId), round });
  };

  get listenerCount(): number {
    return this.listeners.size;
  }

  emit(e: GatewayEvent): void {
    for (const l of [...this.listeners]) l(e);
  }

  ready(info: ReadyInfo): void {
    this.emit({ type: 'ready', info });
  }

  frame(f: Frame): void {
    this.frames.push(f);
    this.emit({ type: 'frame', frame: f });
  }
}

// ---------------------------------------------------------------------------------------------
// ModelCore: the group half of CorePort (L-CORE-07..09) over the stand-in wire.
// ---------------------------------------------------------------------------------------------

interface MProp {
  op: 'add' | 'remove';
  device: Id;
}

interface MGroup {
  groupId: Id;
  /** app_groups.kind: 0 text, 1 call (L-CORE-50, L-CORE-51). */
  kind: 0 | 1;
  /** This device's own leaf index in a call group (the roster's own_leaf of call_media_epoch). */
  ownLeaf: number;
  communityId: Id | null;
  targetId: Id;
  state: 0 | 1 | 2 | 3 | 4;
  epoch: bigint;
  joinedEpoch: bigint;
  joinEpoch: bigint;
  preJoinEpoch: bigint;
  nextSeq: bigint;
  ackedSeq: bigint;
  ackedEpoch: bigint;
  proposals: Map<string, MProp>;
  pending: { epoch: bigint; removesMe: boolean } | null;
  fromResync: boolean;
  /** app_groups.was_gone: joining over a row that was in state 4. */
  wasGone: boolean;
  /** app_groups.max_epoch: the highest epoch held; only ever raised. */
  maxEpoch: bigint;
  rows: Map<bigint, MRow>;
  /** app_pending_history: seq → the evidence of a row held unreadable by its sender leaf's verdict. */
  held: Map<bigint, Held>;
  /** app_reactions as the model holds it: target hex → (user hex + U+0000 + emoji) → the present reaction. */
  reactions: Map<string, Map<string, { user: Id; emoji: string; seq: bigint }>>;
  /** app_pins: target hex → the pin. */
  pins: Map<string, { target: Id; seq: bigint; byUser: Id }>;
}

/**
 * A stored app_messages row (fold rows included): web-1's twelve fields and the v3 columns. `hasEnvelope` stands in
 * for the core's `envelope` column: true for a readable stored row, false once it is NULL (FACTS-SECURITY-03).
 */
interface MRow extends Omit<TimelineRow, 'editedSeq' | 'reply' | 'reactions' | 'pinned' | 'attachments' | 'mention' | 'thread'> {
  replyTo: Id | null;
  /** app_messages.thread_id (schema 5): the envelope's thread_id of a type-0 row, null on every other row. */
  threadId: Id | null;
  editBody: string | null;
  editSeq: bigint;
  mention: boolean;
  attachments: AttachmentDescriptor[];
  hasEnvelope: boolean;
}

/** The row that started a refold (L-CORE-33): the seq, type and authenticated sender of the inserted row. */
interface FoldTrigger {
  seq: bigint;
  ty: number;
  senderUser: Id;
  senderDevice: Id;
}

interface MOut {
  msgId: Id;
  groupId: Id;
  body: string;
  created: bigint;
  state: 0 | 1 | 2;
  error: string;
  epoch: bigint | null;
  type: SendRequest['type'];
  replyTo: Id | null;
  attachments: AttachmentDescriptor[];
  threadId: Id | null;
}

/** The 8-element stand-in of an envelope, decoded; null for a malformed one (a field of the wrong kind or length). */
function readApp(blob: Uint8Array): { user: Id; device: Id; msgId: Id; body: string; type: number; replyTo: Id | null;
  attachments: AttachmentDescriptor[]; threadId: Id | null; } | null {
  try {
    const v = arr(decode(blob));
    if (v.length !== 8 && v.length !== 9) return null;
    if (str(at(v, 0)) !== 'app') return null;
    const type = u64(at(v, 5));
    if (type > 6n) return null;
    const dim = (x: CborValue): number | null => (x === null ? null : u53(x));
    const attachments = arr(at(v, 7)).map((x): AttachmentDescriptor => {
      const a = arr(x, 9);
      return { blobId: bin(at(a, 0), 32), key: bin(at(a, 1), 32), nonce: bin(at(a, 2), 12), size: u53(at(a, 3)), mime: str(at(a, 4)),
        w: dim(at(a, 5)), h: dim(at(a, 6)), thumb: at(a, 7) === null ? null : bin(at(a, 7)), name: str(at(a, 8)) };
    });
    return { user: bin(at(v, 1), 16), device: bin(at(v, 2), 16), msgId: bin(at(v, 3), 16), body: str(at(v, 4)), type: Number(type),
      replyTo: at(v, 6) === null ? null : bin(at(v, 6), 16), attachments,
      threadId: v.length === 9 && at(v, 8) !== null ? bin(at(v, 8), 16) : null };
  } catch {
    return null;
  }
}

/** The first 120 Unicode scalar values of a shown body, line breaks as spaces (fold::excerpt). */
function excerpt(body: string): string {
  return Array.from(body).slice(0, 120).join('').replace(/[\n\r]/g, ' ');
}

/** Byte order of the UTF-8 encodings (SQLite's BLOB/TEXT order for the emoji column). */
function utf8Order(a: string, b: string): number {
  const x = new TextEncoder().encode(a);
  const y = new TextEncoder().encode(b);
  for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i] - y[i];
  return x.length - y.length;
}

const utf8Length = (s: string): number => new TextEncoder().encode(s).length;

export interface CoreCall {
  m: string;
  g: string;
}

export interface ApplyCall {
  g: string;
  through: bigint;
  handshakes: number;
  messages: number;
}

type HsOutcome = 'ok' | 'skip' | 'changed' | 'stop' | 'gone';

/** A leaf as the trust double judges it: its credential's user, device, key, tier and UMK. */
interface MCandidate { userId: Id; deviceId: Id; dskPub: Uint8Array; tier: 0 | 1; umk: Uint8Array }
/** A current leaf of a group, with the signed-list version and head its last ingest cached. `forced` is a test's stand-in
 *  for a leaf whose credential no list can vouch for (a forged or failed leaf): its verdict, whatever the lists say. */
interface MLeaf extends MCandidate { groupId: Id; leafIndex: number; version: bigint; head: Uint8Array | null; forced?: string }
/** A leaf of a call group's served tree (the stand-in tree names none; a test sets them, ModelCore.setCallTree). */
export interface CallTreeLeaf { leafIndex: number; user: Id; device: Id; tier?: 0 | 1; code?: string }
/** The group-authenticated evidence of a message held unreadable by its sender leaf's verdict (app_pending_history). */
type Held = NonNullable<ReturnType<typeof readApp>>;

export class ModelCore implements CorePort {
  /** A current leaf of a modelled group. `umk` is the key its credential carries (fakeDskPub(user) unless a test sets it). */
  private readonly leaves = new Map<string, MLeaf>();
  private readonly trustPins = new Map<string, TrustRecord>();
  addPendingLeaf(groupId: Id, leafIndex: number, userId: Id, deviceId: Id, dskPub: Uint8Array, tier: 0 | 1): void {
    if (groupId.length !== 16 || userId.length !== 16 || deviceId.length !== 16 || dskPub.length !== 32 ||
      !Number.isInteger(leafIndex) || leafIndex < 0 || leafIndex > 0xffff_ffff || (tier !== 0 && tier !== 1))
      throw coreError('E_CORE_INPUT');
    this.leaves.set(`${toHex(groupId)}/${leafIndex}`, { groupId: groupId.slice(), leafIndex,
      userId: userId.slice(), deviceId: deviceId.slice(), dskPub: dskPub.slice(), tier, umk: fakeDskPub(userId), version: 0n, head: null });
  }
  /** app_device_lists: user hex → the newest signed list an ingest accepted (trust.rs ingest_leaf), with the UMK it is under. */
  private readonly userLists = new Map<string, { version: bigint; list: FakeList; head: Uint8Array; umk: Uint8Array }>();
  /** app_quarantine: `${group hex}/${leaf key hex}` → the stored verdict, keyed by the leaf identity (ruling D1'). */
  private readonly verdicts = new Map<string, string>();
  /** meta trust_generation (trust.rs bump_generation). */
  private generation = 0n;
  /** The KeyPackages an Add proposal can carry: device hex → its leaf (the credential the core reads from it). */
  private readonly keyPackageLeaves = new Map<string, MCandidate>();
  /** This device's own credential key: the own pin (F3), verified from signup on and never changed. */
  private get ownUmk(): Uint8Array { return fakeDskPub(this.me.user); }
  pendingLeaves(): PendingLeaf[] {
    if (this.leaves.size > 4096) throw coreError('E_CORE_LIMIT');
    return [...this.leaves.values()].map((leaf) => ({ groupId: leaf.groupId, leafIndex: leaf.leafIndex,
      userId: leaf.userId, cachedVersion: leaf.version,
      reason: this.trustPins.get(toHex(leaf.userId))?.changedUmk != null ? 'changed' : leaf.version === 0n ? 'unknown' : 'ready-refresh' }));
  }
  /** Test hook: a KeyPackage of `deviceId` that an Add proposal of the model DS may carry; `umk` is its credential's key. */
  addKeyPackage(userId: Id, deviceId: Id, dskPub: Uint8Array, tier: 0 | 1, umk: Uint8Array = fakeDskPub(userId)): void {
    this.keyPackageLeaves.set(toHex(deviceId), { userId: userId.slice(), deviceId: deviceId.slice(), dskPub: dskPub.slice(), tier, umk: umk.slice() });
  }
  /**
   * Test hook: the verdict a transition audit (trust.rs audit_member) or a changed-key ingest writes for one current
   * leaf, through the same apply_verdict rule; null is a valid leaf. `E_UMK_CHANGED` for a contact is a validated list
   * under a new key (0xc4…): the pin takes it as its pending change and the leaf carries it. For the own user it is a
   * leaf under a foreign key (F3): the own pin never takes a change.
   */
  setLeafVerdict(groupId: Id, leafIndex: number, code: string | null): void {
    const leaf = this.leaves.get(`${toHex(groupId)}/${leafIndex}`);
    if (leaf === undefined) throw coreError('E_CORE_NOT_FOUND');
    let touched = this.applyVerdict(leaf, code);
    if (code === 'E_UMK_CHANGED') {
      const changed = new Uint8Array(32).fill(0xc4);
      leaf.umk = changed;
      const user = toHex(leaf.userId);
      if (!same(leaf.userId, this.me.user)) {
        const pin = this.trustPins.get(user) ?? { userId: leaf.userId, umk: fakeDskPub(leaf.userId), firstSeen: 0n, verified: false, changedUmk: null, blocked: null };
        if (pin.changedUmk === null) { this.trustPins.set(user, { ...pin, changedUmk: changed }); touched = true; }
        const cached = this.userLists.get(user);
        if (cached !== undefined) cached.umk = changed;
      }
    }
    if (touched) this.generation += 1n;
  }
  /** Test hook: a validated Remove of a current leaf (sync.rs clear_removed_blocks): the leaf and its verdict go. */
  removeLeaf(groupId: Id, leafIndex: number): void {
    const key = `${toHex(groupId)}/${leafIndex}`;
    const leaf = this.leaves.get(key);
    if (leaf === undefined) throw coreError('E_CORE_NOT_FOUND');
    this.leaves.delete(key);
    if (this.verdicts.delete(`${toHex(groupId)}/${toHex(leaf.dskPub)}`)) this.generation += 1n;
  }
  /** trust.rs leaf_verdict: the pin first (rule 2), then the cached signed list (rule 3). */
  private judge(leaf: { userId: Id; deviceId: Id; dskPub: Uint8Array; tier: 0 | 1; umk: Uint8Array; forced?: string }): string | null {
    if (leaf.forced !== undefined) return leaf.forced;
    const pin = this.trustPins.get(toHex(leaf.userId));
    if (pin !== undefined) {
      if (pin.changedUmk !== null) {
        if (same(pin.changedUmk, leaf.umk)) return 'E_UMK_CHANGED';
        // While a change is pending, a leaf of the pinned key is pending: an unverified list vouches for nobody.
        return same(pin.umk, leaf.umk) ? 'E_DEVICE_LIST_STALE' : 'E_CREDENTIAL';
      }
      // A leaf of the own user under another key than this device's own (F3 ruling).
      if (!same(pin.umk, leaf.umk)) return same(leaf.userId, this.me.user) ? 'E_UMK_CHANGED' : 'E_CREDENTIAL';
    }
    const cached = this.userLists.get(toHex(leaf.userId));
    if (cached === undefined) return 'E_DEVICE_LIST_STALE';
    if (!same(cached.umk, leaf.umk)) return 'E_CREDENTIAL';
    const entry = cached.list.entries.find((item) => same(item.deviceId, leaf.deviceId) && item.revokedAt === null);
    if (entry === undefined) return this.revoked(cached.list.entries, leaf) ? 'E_DEVICE_REVOKED' : 'E_DEVICE_UNLISTED';
    if (!same(entryKey(entry), leaf.dskPub)) return 'E_CREDENTIAL';
    if ((entry.tier ?? 1) !== leaf.tier) return 'E_TIER_MISMATCH';
    return pin !== undefined ? null : 'E_DEVICE_LIST_STALE';
  }
  /** device_list.rs check_leaf: the list names this device under this key, revoked (E_DEVICE_REVOKED, not unlisted). */
  private revoked(entries: readonly FakeListEntry[], leaf: { deviceId: Id; dskPub: Uint8Array }): boolean {
    return entries.some((item) => same(item.deviceId, leaf.deviceId) && item.revokedAt !== null && same(entryKey(item), leaf.dskPub));
  }
  /** trust.rs apply_verdict: whether a stored verdict changed. A changed-key block is never overwritten or cleared here. */
  private applyVerdict(leaf: { groupId: Id; dskPub: Uint8Array }, code: string | null): boolean {
    const key = `${toHex(leaf.groupId)}/${toHex(leaf.dskPub)}`;
    const stored = this.verdicts.get(key);
    if (code === null) {
      if (stored === undefined || stored === 'E_UMK_CHANGED') return false;
      this.verdicts.delete(key);
      return true;
    }
    if (code === 'E_DEVICE_LIST_STALE' || stored === code || stored === 'E_UMK_CHANGED') return false;
    this.verdicts.set(key, code);
    return true;
  }
  /** trust.rs member_blocks: each current leaf of `groupId` with a stored verdict, as block_code answers it. */
  private memberBlocks(groupId: Id): { userId: Id; code: TrustBlock }[] {
    const out: { userId: Id; code: TrustBlock }[] = [];
    for (const leaf of this.leaves.values()) {
      if (!same(leaf.groupId, groupId)) continue;
      const code = this.verdicts.get(`${toHex(groupId)}/${toHex(leaf.dskPub)}`);
      if (code !== undefined) out.push({ userId: leaf.userId, code: trustBlock(code) });
    }
    return out;
  }
  /** trust.rs send_block over the group's current leaves: a changed-key block wins; a leaf that left does not count. */
  private sendBlock(groupId: Id): TrustBlock | null {
    const codes = this.memberBlocks(groupId).map((b) => b.code);
    return codes.includes('E_UMK_CHANGED') ? 'E_UMK_CHANGED' : codes[0] ?? null;
  }
  /** trust.rs send_block_code: the live tree of a held group, else any stored verdict row of the group (fail closed). */
  sendBlockCode(groupId: Id): TrustBlock | null {
    if (groupId.length !== 16) throw coreError('E_CORE_INPUT');
    if (this.gs.has(toHex(groupId))) return this.sendBlock(groupId);
    const rows = [...this.verdicts].filter(([key]) => key.startsWith(`${toHex(groupId)}/`)).map(([, code]) => trustBlock(code));
    return rows.includes('E_UMK_CHANGED') ? 'E_UMK_CHANGED' : rows[0] ?? null;
  }
  /** trust.rs send_block_own: the block's code is held by own-user leaves only; never for a group whose state is not held. */
  sendBlockOwn(groupId: Id): boolean {
    if (groupId.length !== 16) throw coreError('E_CORE_INPUT');
    if (!this.gs.has(toHex(groupId))) return false;
    const code = this.sendBlock(groupId);
    return code !== null && this.memberBlocks(groupId).filter((b) => b.code === code).every((b) => same(b.userId, this.me.user));
  }
  /** The Add candidates of a proposals body, by position (trust.rs candidate_leaves); unknown KeyPackages are not modelled. */
  private candidates(groupId: Id, proposalsBody: Uint8Array): { index: bigint; leaf: MCandidate }[] {
    this.must(groupId);
    const out: { index: bigint; leaf: MCandidate }[] = [];
    for (const [index, row] of arr(decode(proposalsBody)).entries()) {
      const p = arr(row, 5);
      if (u64(at(p, 4)) === 1n) continue;
      const v = arr(decode(bin(at(p, 3))), 4);
      if (str(at(v, 2)) !== 'add') continue;
      const leaf = this.keyPackageLeaves.get(toHex(bin(at(v, 3), 16)));
      if (leaf !== undefined) out.push({ index: BigInt(index), leaf });
    }
    return out;
  }
  proposedAdds(groupId: Id, proposalsBody: Uint8Array): { index: bigint; userId: Id; cachedVersion: bigint }[] {
    return this.candidates(groupId, proposalsBody).map(({ index, leaf }) =>
      ({ index, userId: leaf.userId, cachedVersion: this.userLists.get(toHex(leaf.userId))?.version ?? 0n }));
  }
  deviceListIngestAdd(groupId: Id, proposalsBody: Uint8Array, index: bigint, requestedUser: Id, history: Uint8Array,
    headList: Uint8Array, now: bigint): { version: bigint; changed: boolean; quarantined: number } {
    const candidate = this.candidates(groupId, proposalsBody).find((c) => c.index === index);
    if (candidate === undefined) throw coreError('E_DEVICE_LIST_STALE');
    const cached = this.userLists.get(toHex(candidate.leaf.userId));
    try {
      const validated = this.validateHistory(candidate.leaf, requestedUser, history, headList,
        cached === undefined ? null : { version: cached.version, head: cached.head });
      return this.ingested(candidate.leaf.userId, validated, headList, now);
    } catch (e) {
      // trust.rs device_list_ingest_add (ruling 3): a fetched history that fails the candidate is its verdict, kept by
      // its leaf identity (no generation bump), so commitBuild leaves the Add out instead of answering stale again.
      if (e instanceof CoreError && FAILED_LEAF.has(e.code) && same(candidate.leaf.userId, requestedUser))
        this.applyVerdict({ groupId, dskPub: candidate.leaf.dskPub }, e.code);
      throw e;
    }
  }
  /** Task S1 part 2: the model records the attestations it is handed; the Rust core judges them. */
  readonly accountKinds: { userId: Id; attestation: Uint8Array }[] = [];
  accountKindIngest(userId: Id, attestation: Uint8Array): void {
    this.accountKinds.push({ userId, attestation });
  }
  deviceListIngest(groupId: Id, leafIndex: number, requestedUser: Id, history: Uint8Array, headList: Uint8Array,
    now: bigint): { version: bigint; changed: boolean; quarantined: number } {
    const leaf = this.leaves.get(`${toHex(groupId)}/${leafIndex}`);
    if (leaf === undefined) throw coreError('E_DEVICE_LIST_STALE');
    const validated = this.validateHistory(leaf, requestedUser, history, headList,
      leaf.version > 0n && leaf.head !== null ? { version: leaf.version, head: leaf.head } : null);
    leaf.version = validated.version;
    leaf.head = headList.slice();
    return this.ingested(leaf.userId, validated, headList, now);
  }
  /** trust.rs validate_history_from for the double's lists: every refusal writes nothing. */
  private validateHistory(leaf: MCandidate, requestedUser: Id, history: Uint8Array,
    headList: Uint8Array, anchorRow: { version: bigint; head: Uint8Array } | null): { version: bigint; list: FakeList } {
    if (!same(leaf.userId, requestedUser)) throw coreError('E_CREDENTIAL');
    if (history.length > 4_194_304) throw coreError('E_DEVICE_LIST_STALE');
    let rows: CborValue[];
    try { rows = decodeBoundedArray(history, 1024); } catch { throw coreError('E_DEVICE_LIST_STALE'); }
    if (rows.length < 1 || rows.length > 1024) throw coreError('E_DEVICE_LIST_STALE');
    let version = anchorRow?.version ?? 0n;
    let final: CborValue[] | null = null;
    let finalList: FakeList | null = null;
    for (const [index, value] of rows.entries()) {
      let row: CborValue[];
      try { row = arr(value, 4); } catch { throw coreError('E_DEVICE_LIST_STALE'); }
      const rowVersion = u64(row[0]);
      const anchor = index === 0 && anchorRow !== null;
      if (anchor) {
        if (rowVersion !== anchorRow.version || !same(bin(row[1]), anchorRow.head)) throw coreError('E_DEVICE_LIST_STALE');
      } else if (rowVersion !== version + 1n) throw coreError('E_DEVICE_LIST_STALE');
      const list = readFakeList(bin(row[1]));
      if (list === null) throw coreError('E_DEVICE_LIST_STALE');
      if (!same(list.userId, leaf.userId)) throw coreError('E_CREDENTIAL');
      // This fixed marker is only a double's stand-in; client_trust.rs checks real signatures and hashes.
      const sig = bin(row[2], 64);
      const prev = bin(row[3], 32);
      if (!sig.every((byte) => byte === Number(rowVersion & 255n))) throw coreError('E_CREDENTIAL');
      if (!prev.every((byte) => byte === Number((rowVersion - 1n) & 255n))) throw coreError('E_DEVICE_LIST_STALE');
      if (!anchor) version = rowVersion;
      final = row;
      finalList = list;
    }
    // Like trust.rs, head_list is the signed list itself (the native peer's head.blob), never the served row around it.
    if (final === null || finalList === null || !same(bin(final[1]), headList)) throw coreError('E_DEVICE_LIST_STALE');
    const entry = finalList.entries.find((item) => same(item.deviceId, leaf.deviceId) && item.revokedAt === null);
    if (entry === undefined) throw coreError(this.revoked(finalList.entries, leaf) ? 'E_DEVICE_REVOKED' : 'E_DEVICE_UNLISTED');
    if (!same(entryKey(entry), leaf.dskPub)) throw coreError('E_CREDENTIAL');
    if ((entry.tier ?? 1) !== leaf.tier) throw coreError('E_TIER_MISMATCH');
    // The own account's key is this device's own credential's; no served list changes it (the stand-in lists carry no
    // UMK, so the leaf's own key stands for the history's).
    if (same(leaf.userId, this.me.user) && !same(leaf.umk, this.ownUmk)) throw coreError('E_UMK_CHANGED');
    return { version, list: finalList };
  }
  /** trust.rs ingest_leaf's write unit: the pin, the cached list, and every current leaf of the user judged again. */
  private ingested(userId: Id, validated: { version: bigint; list: FakeList }, headList: Uint8Array,
    now: bigint): { version: bigint; changed: boolean; quarantined: number } {
    const user = toHex(userId);
    if (!this.trustPins.has(user)) this.trustPins.set(user, { userId: userId.slice(), umk: fakeDskPub(userId),
      firstSeen: now, verified: false, changedUmk: null, blocked: null });
    const pin = this.trustPins.get(user);
    this.userLists.set(user, { version: validated.version, list: validated.list, head: headList.slice(),
      umk: (pin?.changedUmk ?? pin?.umk ?? fakeDskPub(userId)).slice() });
    let touched = false;
    let quarantined = 0;
    for (const leaf of this.leaves.values()) {
      if (toHex(leaf.userId) !== user || this.isOwn(leaf)) continue;
      const verdict = this.judge(leaf);
      touched = this.applyVerdict(leaf, verdict) || touched;
      if (verdict !== null && verdict !== 'E_DEVICE_LIST_STALE') quarantined += 1;
    }
    if (touched) this.generation += 1n;
    return { version: validated.version, changed: false, quarantined };
  }
  private isOwn(leaf: { userId: Id; deviceId: Id }): boolean {
    return same(leaf.userId, this.me.user) && same(leaf.deviceId, this.me.device);
  }
  /** The current leaf a held row's sender is, if the double models it. */
  private leafOf(groupId: Id, userId: Id, deviceId: Id): MLeaf | undefined {
    return [...this.leaves.values()].find((leaf) => same(leaf.groupId, groupId) && same(leaf.userId, userId) && same(leaf.deviceId, deviceId));
  }
  /**
   * sync.rs revalidate_pending: rows of this leaf that waited unreadable turn readable at the same seq once the exact leaf
   * validates (trust.rs validate_pending_sender); the generation moves when any does.
   */
  revalidatePending(groupId: Id, leafIndex: number, limit: number): number {
    if (!Number.isInteger(limit) || limit < 1 || limit > 256) throw coreError('E_CORE_INPUT', 'pending limit must be 1..=256');
    const leaf = this.leaves.get(`${toHex(groupId)}/${leafIndex}`);
    if (leaf === undefined) throw coreError('E_CORE_NOT_FOUND');
    const g = this.gs.get(toHex(groupId));
    if (g === undefined || this.judge(leaf) !== null) return 0;
    let released = 0;
    for (const [seq, held] of [...g.held].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
      if (!same(held.user, leaf.userId) || !same(held.device, leaf.deviceId)) continue;
      const row = g.rows.get(seq);
      g.held.delete(seq);
      if (row === undefined || row.status !== 1) continue;
      this.insertReadable(g, seq, row.epoch, row.recvTs, held);
      released += 1;
    }
    if (released > 0) this.generation += 1n;
    return released;
  }
  /** trust_state (ABI 8): every pin, the own one always, with the stored verdict of the user's leaves in active groups. */
  trustState(): TrustRecord[] {
    const blocks: { userId: Id; code: TrustBlock }[] = [];
    for (const g of this.gs.values()) if (g.state === 2) blocks.push(...this.memberBlocks(g.groupId));
    return [...this.trustPins.values()].sort((a, b) => toHex(a.userId).localeCompare(toHex(b.userId))).map((pin) => {
      const codes = blocks.filter((b) => same(b.userId, pin.userId)).map((b) => b.code);
      return { ...pin, blocked: codes.includes('E_UMK_CHANGED') ? 'E_UMK_CHANGED' : codes[0] ?? null };
    });
  }
  trustGeneration(): bigint { return this.generation; }
  /** trust.rs trust_verify: approves only the comparison key its cached list is under, then releases held messages. */
  trustVerify(userId: Id, umk: Uint8Array): void {
    if (userId.length !== 16 || umk.length !== 32) throw coreError('E_CORE_INPUT');
    const user = toHex(userId);
    const cached = this.userLists.get(user);
    if (cached === undefined || !same(cached.umk, umk)) throw coreError('E_DEVICE_LIST_STALE');
    const pin = this.trustPins.get(user);
    if (pin === undefined || !same(pin.changedUmk ?? pin.umk, umk)) throw coreError('E_DEVICE_LIST_STALE');
    const leaves = [...this.leaves.values()].filter((leaf) => same(leaf.userId, userId));
    const listed = (leaf: MLeaf): boolean => cached.list.entries.some((e) => same(e.deviceId, leaf.deviceId) && e.revokedAt === null &&
      same(entryKey(e), leaf.dskPub) && (e.tier ?? 1) === leaf.tier);
    // The verified key must still hold a current leaf that its signed list names.
    if (!leaves.some((leaf) => same(leaf.umk, umk) && listed(leaf))) throw coreError('E_DEVICE_LIST_STALE');
    this.trustPins.set(user, { ...pin, umk: umk.slice(), verified: true, changedUmk: null });
    for (const leaf of leaves) {
      const key = `${toHex(leaf.groupId)}/${toHex(leaf.dskPub)}`;
      if ((same(leaf.umk, umk) || listed(leaf)) && this.verdicts.get(key) === 'E_UMK_CHANGED') this.verdicts.delete(key);
    }
    for (const leaf of leaves) if (!this.isOwn(leaf)) this.applyVerdict(leaf, this.judge(leaf));
    this.generation += 1n;
    for (const leaf of leaves) if (!this.isOwn(leaf)) this.revalidatePending(leaf.groupId, leaf.leafIndex, 256);
  }
  /** trust.rs safety_number_for: digits for exactly the comparison key, with a user of a current shared group. */
  safetyNumberFor(userId: Id, expectedUmk: Uint8Array): string {
    if (userId.length !== 16 || expectedUmk.length !== 32) throw coreError('E_CORE_INPUT');
    const user = toHex(userId);
    const cached = this.userLists.get(user);
    const pin = this.trustPins.get(user);
    if (cached === undefined || pin === undefined || !same(pin.changedUmk ?? pin.umk, cached.umk)) throw coreError('E_CORE_NOT_FOUND');
    if (!same(cached.umk, expectedUmk)) throw coreError('E_DEVICE_LIST_STALE');
    if (![...this.leaves.values()].some((leaf) => same(leaf.userId, userId))) throw coreError('E_CORE_NOT_FOUND');
    // A stand-in for safety_number(own UMK, key): 60 digits in groups of five, the same for both sides of a pair.
    const digits = Array.from({ length: 60 }, (_, i) => String((this.ownUmk[i % 32] ^ cached.umk[(i * 7) % 32]) % 10)).join('');
    return digits.match(/.{5}/g)?.join(' ') ?? '';
  }
  search(): SearchHit[] { return []; }

  // --- web-3 call groups (L-CORE-50..52; client/calls.rs, groups.rs group_create_kind / group_join_kind) ---

  /** The leaves the served tree of a call group brings, by group hex: the stand-in tree names none, so a test sets them. */
  private readonly callTrees = new Map<string, CallTreeLeaf[]>();
  /** The CBOR buffer the last call_media_epoch handed over, by group hex: the adapter zero-fills it (L-TS-55). */
  private readonly epochBytes = new Map<string, Uint8Array>();
  /** The hex of the last base key derived, by group hex: a test looks for it where no key may be. */
  private readonly baseKeys = new Map<string, string>();

  /** Test hook: the leaves (besides this device's own) of call group `groupId`'s served tree. A `code` forces that leaf's
   *  verdict (a forged or failed leaf); else the trust rules judge it against the cached lists. A held, active group gains
   *  them at once, as the audit of the transition that added them would (trust.rs audit_member). */
  setCallTree(groupId: Id, leaves: readonly CallTreeLeaf[]): void {
    this.callTrees.set(toHex(groupId), leaves.map((l) => ({ ...l })));
    const g = this.gs.get(toHex(groupId));
    if (g !== undefined && g.state === 2) this.installTree(g);
  }
  /** Test hook: one more leaf with a forced failing verdict (an unlisted, forged device) in call group `groupId`'s tree:
   *  held and active, the leaf and its stored verdict are there at once (the next epoch's audit wrote them). */
  addFailedLeaf(groupId: Id, code: string): void {
    const tree = this.callTrees.get(toHex(groupId)) ?? [];
    const g = this.gs.get(toHex(groupId));
    const taken = new Set([...tree.map((l) => l.leafIndex), g?.ownLeaf ?? -1]);
    let leafIndex = 0;
    while (taken.has(leafIndex)) leafIndex += 1;
    const n = 0x40 + tree.length;
    this.setCallTree(groupId, [...tree, { leafIndex, user: idOf(0xf0, n), device: idOf(0xf1, n), code }]);
  }
  /** Test hook: the cached signed list and the pin of `userId`, as an earlier ingest of that row left them (`blob` is the
   *  signed list ModelDs served at `version`). Its leaves in later joined trees validate against it. */
  trustList(userId: Id, version: bigint, blob: Uint8Array): void {
    const list = readFakeList(blob);
    if (list === null) throw coreError('E_CORE_INPUT', 'not a fake list');
    const user = toHex(userId);
    if (!this.trustPins.has(user)) this.trustPins.set(user, { userId: userId.slice(), umk: fakeDskPub(userId), firstSeen: 0n, verified: false, changedUmk: null, blocked: null });
    const pin = this.trustPins.get(user);
    this.userLists.set(user, { version, list, head: blob.slice(), umk: (pin?.changedUmk ?? pin?.umk ?? fakeDskPub(userId)).slice() });
  }
  /** Test hook: `userId`'s identity changed (a validated list under a new key): every current leaf of the user in a held
   *  group carries the new key and the stored E_UMK_CHANGED verdict (setLeafVerdict's rule, RV-3). */
  changeIdentity(userId: Id): void {
    for (const leaf of [...this.leaves.values()]) {
      if (same(leaf.userId, userId) && this.gs.get(toHex(leaf.groupId))?.state === 2) this.setLeafVerdict(leaf.groupId, leaf.leafIndex, 'E_UMK_CHANGED');
    }
  }
  /** Test hook: a held group's send block (send_block_code answers `code`): a leaf of a stranger with that stored verdict. */
  blockGroup(groupId: Id, code: string): void {
    const g = this.must(groupId);
    const n = 0x80 + [...this.leaves.values()].filter((l) => same(l.groupId, groupId)).length;
    const leaf: MLeaf = { groupId: g.groupId, leafIndex: 1000 + n, userId: idOf(0xf2, n), deviceId: idOf(0xf3, n),
      dskPub: fakeDskPub(idOf(0xf3, n)), tier: 1, umk: fakeDskPub(idOf(0xf2, n)), version: 0n, head: null, forced: code };
    this.leaves.set(`${toHex(groupId)}/${leaf.leafIndex}`, leaf);
    if (this.applyVerdict(leaf, code)) this.generation += 1n;
  }
  /** Test hook: a current leaf's stored verdict becomes `code` (a forced one: the lists no longer decide it) without its
   *  user's pin taking a change, as a transition audit that found the leaf under another key stores it. */
  forceVerdict(groupId: Id, leafIndex: number, code: string): void {
    const leaf = this.leaves.get(`${toHex(groupId)}/${leafIndex}`);
    if (leaf === undefined) throw coreError('E_CORE_NOT_FOUND');
    leaf.forced = code;
    if (this.applyVerdict(leaf, code)) this.generation += 1n;
  }
  /** Test hook: clears what blockGroup set on the group. */
  unblockGroup(groupId: Id): void {
    for (const [key, leaf] of [...this.leaves]) {
      if (!same(leaf.groupId, groupId) || leaf.leafIndex < 1000) continue;
      this.leaves.delete(key);
      this.verdicts.delete(`${toHex(groupId)}/${toHex(leaf.dskPub)}`);
      this.generation += 1n;
    }
  }
  /** Test hook: a group this device already holds, active at `epoch` (a DM's text group joined earlier). */
  seedGroup(groupId: Id, kind: 0 | 1, communityId: Id | null, targetId: Id, epoch = 1n, nextSeq = 1n): void {
    const g = this.fresh(groupId, communityId, targetId, 2, kind);
    g.epoch = epoch; g.joinedEpoch = epoch; g.maxEpoch = epoch; g.nextSeq = nextSeq;
    this.gs.set(toHex(groupId), g);
  }
  lastBaseKeyHex(groupId: Id): string { return this.baseKeys.get(toHex(groupId)) ?? ''; }
  /** The last epoch this group's call_media_epoch handed to the worker: a test reads whether its key buffer was
   *  transferred (detached) or zero-filled. */
  lastMedia(groupId: Id): CallMediaEpoch | undefined { return this.medias.get(toHex(groupId)); }
  private readonly medias = new Map<string, CallMediaEpoch>();
  lastEpochBytes(groupId: Id): Uint8Array { return this.epochBytes.get(toHex(groupId)) ?? new Uint8Array(0); }

  callGroupCreate(groupId: Id, communityId: Id | null, channelId: Id): Uint8Array {
    this.log('callGroupCreate', groupId);
    const prev = this.gs.get(toHex(groupId));
    if (prev !== undefined) throw coreError('E_CORE_STATE', `group exists in state ${String(prev.state)}`);
    const holder = this.holder(channelId, groupId, 1);
    if (holder !== undefined) throw coreError('E_CORE_STATE', `channel has group ${toHex(holder.groupId)}`);
    this.gs.set(toHex(groupId), this.fresh(groupId, communityId, channelId, 0, 1));
    return encode([groupId, wire.callBinding(communityId, channelId), wire.info(groupId, 0n), wire.tree(groupId, 0n)]);
  }

  /** group_join_kind for a call group: the text join's checks with kind 1, then the served tree's audit and the join gate
   *  (a pending leaf tolerated, ruling RV-1); every other refusal rolls the whole join back. */
  callGroupJoinExternal(eg: ExpectedGroup, infoBody: Uint8Array, treeBody: Uint8Array): Uint8Array {
    this.log('callGroupJoinExternal', eg.groupId);
    const info = arr(decode(infoBody), 4);
    const epoch = u64(at(info, 0));
    if (epoch !== u64(at(arr(decode(treeBody), 3), 0))) throw coreError('E_CORE_INPUT', 'info_body and tree_body disagree');
    if (!same(bin(at(arr(decode(bin(at(info, 1))), 3), 1), 16), eg.groupId)) throw coreError('E_CORE_INPUT', 'GroupInfo group id does not match');
    const prev = this.gs.get(toHex(eg.groupId));
    if (prev !== undefined && (prev.state === 0 || prev.state === 1)) throw coreError('E_CORE_STATE', `group state ${String(prev.state)}`);
    if (prev !== undefined && !this.boundTo(prev, eg.communityId, eg.channelId, 1)) throw coreError('E_CORE_STATE', 'group is bound to another channel');
    const holder = this.holder(eg.channelId, eg.groupId, 1);
    if (holder !== undefined) throw coreError('E_CORE_STATE', `channel has group ${toHex(holder.groupId)}`);
    if (prev !== undefined && epoch < this.floorOf(prev)) throw coreError('E_CORE_INPUT', 'GroupInfo would rejoin at or below an epoch this device has held');
    const g = this.fresh(eg.groupId, eg.communityId, eg.channelId, 1, 1);
    if (prev !== undefined) { g.maxEpoch = prev.maxEpoch; g.nextSeq = prev.nextSeq; g.wasGone = prev.state === 4; }
    g.joinEpoch = epoch + 1n;
    g.epoch = g.joinEpoch;
    this.gs.set(toHex(eg.groupId), g);
    this.installTree(g);
    const refused = this.callGate(g, true);
    if (refused !== null) {
      const detail = this.gateDetail(g, refused);
      // The unit of the join rolls back: no row, no verdict rows and no group state of the refused join survive.
      this.dropCallLeaves(eg.groupId);
      if (prev !== undefined) this.gs.set(toHex(eg.groupId), prev); else this.gs.delete(toHex(eg.groupId));
      throw coreError(refused, detail);
    }
    return encode([wire.commit(epoch + 1n, this.me.device, [this.me.device], []), wire.info(eg.groupId, epoch + 1n)]);
  }

  callMediaEpoch(groupId: Id): CallMediaEpoch {
    this.log('callMediaEpoch', groupId);
    const g = this.callRow(groupId);
    if (g.state !== 2) throw coreError('E_CORE_STATE', `group state ${String(g.state)}`);
    const refused = this.callGate(g, false);
    if (refused !== null) throw coreError(refused, this.gateDetail(g, refused));
    // A stand-in key and code, distinct per group and epoch and never zero.
    const key = Uint8Array.from({ length: 16 }, (_, i) => ((g.groupId[i] ?? 0) ^ Number((g.epoch * 37n + BigInt(i) * 11n) & 0xffn)) | 0x01);
    const digits = Array.from({ length: 30 }, (_, i) => String(((key[i % 16] ?? 0) + i) % 10)).join('');
    const roster = [{ leaf: g.ownLeaf, device: this.me.device, user: this.me.user },
      ...[...this.leaves.values()].filter((l) => same(l.groupId, groupId)).map((l) => ({ leaf: l.leafIndex, device: l.deviceId, user: l.userId }))]
      .sort((a, b) => a.leaf - b.leaf);
    const bytes = encode([g.epoch, key, g.ownLeaf, roster.map((r) => [r.leaf, r.device, r.user]), digits.match(/.{5}/g)?.join(' ') ?? '']);
    this.baseKeys.set(toHex(groupId), toHex(key));
    this.epochBytes.set(toHex(groupId), bytes);
    key.fill(0);
    // The wasm adapter's own decoder: the key is copied out and these bytes are zero-filled (L-TS-55).
    const media = readCallMediaEpoch(bytes);
    this.medias.set(toHex(groupId), media);
    return media;
  }

  callJoinCheck(groupId: Id): void {
    this.log('callJoinCheck', groupId);
    const g = this.callRow(groupId);
    if (g.state !== 1) throw coreError('E_CORE_STATE', `group state ${String(g.state)}`);
    const refused = this.callGate(g, false);
    if (refused !== null) throw coreError(refused, this.gateDetail(g, refused));
  }

  /** calls.rs `refusal`: an E_UMK_CHANGED names the first user by id among this tree's other leaves whose pin holds a
   *  changed key (hex), else nothing (web-3 task 10, concern 3). */
  private gateDetail(g: MGroup, code: string): string {
    if (code !== 'E_UMK_CHANGED') return '';
    const own = toHex(this.me.user);
    const users = [...new Set([...this.leaves.values()].filter((l) => same(l.groupId, g.groupId)).map((l) => toHex(l.userId)))]
      .filter((u) => u !== own).sort();
    return users.find((u) => (this.trustPins.get(u)?.changedUmk ?? null) !== null) ?? '';
  }

  callLeaveProposal(groupId: Id): Uint8Array {
    this.log('callLeaveProposal', groupId);
    const g = this.callRow(groupId);
    if (g.state !== 2) throw coreError('E_CORE_STATE', `group state ${String(g.state)}`);
    this.msgs += 1;
    const ref = idOf(0x5f, this.msgs);
    // OpenMLS keeps its own leave in the pending proposal store: call_rotate takes it out again.
    g.proposals.set(toHex(ref), { op: 'remove', device: this.me.device });
    return encode([g.epoch, wire.prop(ref, 'remove', this.me.device)]);
  }

  /** commit_build_inner with `rotate`: this device's own pending Remove is withdrawn (OpenMLS refuses a self-removal),
   *  then an empty commit of whatever else is queued. */
  callRotate(groupId: Id): Uint8Array {
    this.log('callRotate', groupId);
    const g = this.callRow(groupId);
    for (const [ref, p] of [...g.proposals]) if (p.op === 'remove' && same(p.device, this.me.device)) g.proposals.delete(ref);
    return this.commitBuild(groupId, encode([]));
  }

  callGroupForget(groupId: Id): void {
    this.log('callGroupForget', groupId);
    const g = this.callRow(groupId);
    if (g.state === 4) return;
    g.state = 4; g.epoch = 0n; g.proposals.clear(); g.pending = null; g.fromResync = false; g.wasGone = false;
    this.dropCallLeaves(groupId);
  }

  /** calls.rs call_row: E_CORE_NOT_FOUND for an unknown id, E_CORE_INPUT for a group that is not a call group. */
  private callRow(groupId: Id): MGroup {
    const g = this.gs.get(toHex(groupId));
    if (g === undefined) throw coreError('E_CORE_NOT_FOUND', '');
    if (g.kind !== 1) throw coreError('E_CORE_INPUT', 'not a call group');
    return g;
  }

  /** The audit of a call group's served tree: each leaf joins `leaves` with the verdict the trust rules give it. */
  private installTree(g: MGroup): void {
    const tree = this.callTrees.get(toHex(g.groupId)) ?? [];
    const taken = new Set(tree.map((l) => l.leafIndex));
    let own = 0;
    while (taken.has(own)) own += 1;
    g.ownLeaf = own;
    for (const t of tree) {
      const key = `${toHex(g.groupId)}/${t.leafIndex}`;
      if (this.leaves.has(key)) continue;
      const cached = this.userLists.get(toHex(t.user));
      const leaf: MLeaf = { groupId: g.groupId, leafIndex: t.leafIndex, userId: t.user.slice(), deviceId: t.device.slice(),
        dskPub: fakeDskPub(t.device), tier: t.tier ?? 1, umk: cached?.umk.slice() ?? fakeDskPub(t.user),
        version: cached?.version ?? 0n, head: cached?.head.slice() ?? null, ...(t.code === undefined ? {} : { forced: t.code }) };
      this.leaves.set(key, leaf);
      if (this.applyVerdict(leaf, this.judge(leaf))) this.generation += 1n;
    }
  }

  private dropCallLeaves(groupId: Id): void {
    for (const [key, leaf] of [...this.leaves]) {
      if (!same(leaf.groupId, groupId)) continue;
      this.leaves.delete(key);
      if (this.verdicts.delete(`${toHex(groupId)}/${toHex(leaf.dskPub)}`)) this.generation += 1n;
    }
  }

  /** calls.rs gate_verdict, in the ledger's order; null when the gate passes. `staleOk` is the join's tolerance of a
   *  pending leaf (call_join_check settles it before anything is posted). */
  private callGate(g: MGroup, staleOk: boolean): string | null {
    // 1. The send block: a stored failed or changed-key verdict of a leaf of this tree.
    const block = this.sendBlock(g.groupId);
    if (block !== null) return block;
    // 2. Every other leaf validates against its cached signed list now; pending refuses (RV-1).
    for (const leaf of [...this.leaves.values()].filter((l) => same(l.groupId, g.groupId)).sort((a, b) => a.leafIndex - b.leafIndex)) {
      if (this.isOwn(leaf)) continue;
      const verdict = this.judge(leaf);
      if (verdict === null || (verdict === 'E_DEVICE_LIST_STALE' && staleOk)) continue;
      return verdict;
    }
    // 3. The text group held for the call's target: a community that disagrees is E_BINDING (review M3); a DM's text
    // group's send block refuses (decision 4).
    const text = this.holder(g.targetId, g.groupId, 0);
    if (text !== undefined) {
      const textCommunity = text.communityId === null ? null : toHex(text.communityId);
      const callCommunity = g.communityId === null ? null : toHex(g.communityId);
      if (textCommunity !== callCommunity) return 'E_BINDING';
      if (textCommunity === null) {
        const textBlock = this.sendBlock(text.groupId);
        if (textBlock !== null) return textBlock;
      }
    }
    return null;
  }

  /** Every group-scoped call except the reads groups/outbox/timeline, in call order. */
  readonly calls: CoreCall[] = [];
  readonly applyCalls: ApplyCall[] = [];
  private readonly gs = new Map<string, MGroup>();
  private readonly out = new Map<string, MOut>();
  private msgs = 0;
  private readonly readState = new Map<string, { lastReadSeq: bigint; lastReadAt: bigint }>();
  private readonly settingsMap = new Map<string, string>();
  /** app_purges: `${group hex}/${seq}` → the purge row (L-CORE-38). */
  private readonly purgeRows = new Map<string, PurgeRow>();
  /** app_roles: community hex → the own role ids (L-CORE-35). */
  private readonly roles = new Map<string, Id[]>();
  /** app_authority / app_authority_known / app_authority_mentionable (web3.5 R1): group hex → user hexes allowed
   *  pin_messages / mention_everyone, and the role hexes anyone may mention. */
  private readonly authority = new Map<string, { pinners: Set<string>; broadcasters: Set<string>; mentionable: Set<string> }>();

  readonly instancePinChecks: Uint8Array[] = [];

  constructor(readonly me: Peer, private readonly instance?: { instanceId: Id; externalSenderPub: Uint8Array }) {
    // F3: the own pin is this device's own credential key, verified from signup on; it never takes a pending change.
    this.trustPins.set(toHex(me.user), { userId: me.user.slice(), umk: fakeDskPub(me.user), firstSeen: 0n, verified: true, changedUmk: null, blocked: null });
  }

  // The account half is task 15's; the engine never calls it.
  pause(): void {
    this.calls.push({ m: 'pause', g: '' });
  }
  async resume(): Promise<void> {
    await Promise.resolve();
  }
  close(): void {
    this.calls.push({ m: 'close', g: '' });
  }
  identity(): IdentityInfo {
    return { phase: 2, instanceId: this.instance?.instanceId ?? null, userId: this.me.user, deviceId: this.me.device, username: 'model', listPublished: true };
  }
  instanceSignerKeyMatches(observedKey: Uint8Array): void {
    this.instancePinChecks.push(observedKey.slice());
    if (this.instance === undefined) throw coreError('E_CORE_STATE', 'not modelled');
    if (!same(this.instance.externalSenderPub, observedKey)) throw coreError('E_CORE_STATE', 'instance external-sender key changed');
  }
  instanceSignerKeyPin(instanceId: Uint8Array, observedKey: Uint8Array): void {
    this.instancePinChecks.push(observedKey.slice());
    if (this.instance === undefined || !same(this.instance.instanceId, instanceId)
      || !same(this.instance.externalSenderPub, observedKey)) throw coreError('E_CORE_STATE', 'instance pin mismatch');
  }
  signupBegin(): string {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  signupRequest(): Uint8Array {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  signupComplete(): Uint8Array {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  signupReset(): void {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  deviceListBody(): Uint8Array {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  deviceListPublished(): void {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  deviceListDrop(): void {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  sessionSign(): Uint8Array {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  signRemoval(): Uint8Array {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  sessionStore(): void {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  session(): null {
    return null;
  }
  sessionClear(): void {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  keyPackages(): Uint8Array {
    throw coreError('E_CORE_STATE', 'not modelled');
  }
  sealedObjects(): SealedObjects { throw coreError('E_CORE_STATE', 'not modelled'); }
  stateSealedUploaded(): void { throw coreError('E_CORE_STATE', 'not modelled'); }
  stateSealedCurrent(): boolean { throw coreError('E_CORE_STATE', 'not modelled'); }
  enrolBegin(): { deviceId: Id; dskPub: Uint8Array } { throw coreError('E_CORE_STATE', 'not modelled'); }
  enrolSessionSign(): Uint8Array { throw coreError('E_CORE_STATE', 'not modelled'); }
  enrolRegistered(): void { throw coreError('E_CORE_STATE', 'not modelled'); }
  recoveryKeyCheck(): void { throw coreError('E_CORE_STATE', 'not modelled'); }
  enrolComplete(): SignedLists { throw coreError('E_CORE_STATE', 'not modelled'); }
  enrolReset(): void { throw coreError('E_CORE_STATE', 'not modelled'); }
  deviceListRevoke(): SignedLists { throw coreError('E_CORE_STATE', 'not modelled'); }
  ownDeviceListUpdate(): { version: bigint; listed: boolean } { throw coreError('E_CORE_STATE', 'not modelled'); }
  ownDeviceList(): OwnDeviceList { throw coreError('E_CORE_STATE', 'not modelled'); }

  // --- groups (L-CORE-07) ---

  groups(): GroupInfo[] {
    return [...this.gs.values()]
      .sort((a, b) => (toHex(a.groupId) < toHex(b.groupId) ? -1 : 1))
      .map((g) => this.info(g));
  }
  groupHealSnapshot(groupId: Id): Uint8Array {
    this.log('groupHealSnapshot', groupId);
    const g = this.must(groupId);
    if (g.state !== 2) throw coreError('E_CORE_STATE', `group state ${String(g.state)}`);
    return encode([wire.info(g.groupId, g.epoch), [], wire.tree(g.groupId, g.epoch)]);
  }
  groupRow(groupId: Id): GroupInfo | null {
    const g = this.gs.get(toHex(groupId));
    return g === undefined ? null : this.info(g);
  }
  markRead(groupId: Id, seq: bigint, now: bigint): void {
    const g = this.must(groupId);
    if (g.state === 4) throw coreError('E_CORE_STATE', 'group state 4');
    const key = toHex(groupId);
    const previous = this.readState.get(key)?.lastReadSeq ?? 0n;
    const clamped = seq < g.nextSeq ? seq : g.nextSeq - 1n;
    this.readState.set(key, { lastReadSeq: clamped > previous ? clamped : previous, lastReadAt: now });
  }
  activity(): ActivityRow[] {
    return [...this.gs.values()].filter((g) => g.state === 2 || g.state === 3)
      .sort((a, b) => toHex(a.groupId).localeCompare(toHex(b.groupId)))
      .map((g) => {
        const lastReadSeq = this.readState.get(toHex(g.groupId))?.lastReadSeq ?? 0n;
        const rows = [...g.rows.values()].filter((r) => r.status === 0 && r.type === 0 &&
          // The core's `sender_user <> ?1` excludes a NULL sender, as SQL does (CORE-ENGINE-03).
          r.senderUser !== null && !same(r.senderUser, this.me.user) && r.seq > lastReadSeq).sort(bySeq);
        const last = rows[rows.length - 1];
        // The stored flag, fixed when the row arrived (L-CORE-35), never the body as it reads now.
        return { groupId: g.groupId, unread: rows.length, mentions: rows.filter((r) => r.mention).length,
          lastSeq: last?.seq ?? 0n, lastTs: last?.recvTs ?? 0n, lastReadSeq };
      });
  }
  settings(): Record<string, string> {
    return Object.fromEntries([...this.settingsMap].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0));
  }
  settingPut(k: string, v: string): void {
    const enc = new TextEncoder();
    if (enc.encode(k).length < 1 || enc.encode(k).length > 128 || enc.encode(v).length > 1024) throw coreError('E_CORE_INPUT');
    this.settingsMap.set(k, v);
  }
  settingDelete(k: string): void {
    const length = new TextEncoder().encode(k).length;
    if (length < 1 || length > 128) throw coreError('E_CORE_INPUT');
    this.settingsMap.delete(k);
  }

  groupCreate(groupId: Id, communityId: Id | null, channelId: Id): Uint8Array {
    this.log('groupCreate', groupId);
    for (const g of this.gs.values()) {
      if (same(g.targetId, channelId) && g.state !== 4 && g.kind === 0) throw coreError('E_CORE_STATE', 'a group exists for the channel');
    }
    this.gs.set(toHex(groupId), this.fresh(groupId, communityId, channelId, 0));
    return encode([groupId, wire.binding(communityId, channelId), wire.info(groupId, 0n), wire.tree(groupId, 0n)]);
  }

  groupRegistered(groupId: Id, nextSeq: bigint): void {
    this.log('groupRegistered', groupId);
    const g = this.must(groupId);
    if (g.state !== 0) throw coreError('E_CORE_STATE', `state ${String(g.state)}`);
    g.state = 2;
    g.nextSeq = nextSeq;
  }

  groupDiscard(groupId: Id): void {
    this.log('groupDiscard', groupId);
    const g = this.must(groupId);
    if (g.state !== 0 && g.state !== 1) throw coreError('E_CORE_STATE', `group state ${String(g.state)}`);
    g.proposals.clear();
    g.pending = null;
    // A staged call group's audit rows go with it (the joined tree is not held any more).
    if (g.kind === 1) this.dropCallLeaves(groupId);
    if (g.fromResync) {
      // As the core writes it (groups.rs: state 3, epoch 0): the old group state was already replaced (CORE-ENGINE-03).
      g.state = 3;
      g.epoch = 0n;
      g.fromResync = false;
    } else if (g.wasGone) {
      // A rejoin of a gone row that did not complete: back to gone with its timeline, outbox,
      // nextSeq and floor (history outranks the discard of a fresh join).
      g.state = 4;
      g.epoch = 0n;
      g.wasGone = false;
    } else {
      this.gs.delete(toHex(groupId));
      for (const [k, o] of this.out) if (same(o.groupId, groupId)) this.out.delete(k);
    }
  }

  /** L-CORE-07 `group_join_external`: the checks in the core's order; every refusal writes nothing. */
  groupJoinExternal(eg: ExpectedGroup, infoBody: Uint8Array, treeBody: Uint8Array): Uint8Array {
    this.log('groupJoinExternal', eg.groupId);
    const info = arr(decode(infoBody), 4);
    const epoch = u64(at(info, 0));
    if (epoch !== u64(at(arr(decode(treeBody), 3), 0))) throw coreError('E_CORE_INPUT', 'info_body and tree_body disagree');
    if (!same(bin(at(arr(decode(bin(at(info, 1))), 3), 1), 16), eg.groupId)) {
      throw coreError('E_CORE_INPUT', 'GroupInfo group id does not match');
    }
    const prev = this.gs.get(toHex(eg.groupId));
    if (prev !== undefined && (prev.state === 0 || prev.state === 1)) throw coreError('E_CORE_STATE', `group state ${String(prev.state)}`);
    if (prev !== undefined && !this.boundTo(prev, eg.communityId, eg.channelId)) {
      throw coreError('E_CORE_STATE', 'group is bound to another channel');
    }
    const holder = this.holder(eg.channelId, eg.groupId);
    if (holder !== undefined) throw coreError('E_CORE_STATE', `channel has group ${toHex(holder.groupId)}`);
    // The join lands at epoch + 1: refused when that is not above the highest epoch held.
    if (prev !== undefined && epoch < this.floorOf(prev)) {
      throw coreError('E_CORE_INPUT', 'GroupInfo would rejoin at or below an epoch this device has held');
    }
    const g = this.fresh(eg.groupId, eg.communityId, eg.channelId, 1);
    if (prev !== undefined) {
      // The row is updated, never recreated: its timeline, folds, outbox, cursor and floor stay.
      g.rows = prev.rows;
      g.reactions = prev.reactions;
      g.pins = prev.pins;
      g.nextSeq = prev.nextSeq;
      g.ackedSeq = prev.ackedSeq;
      g.ackedEpoch = prev.ackedEpoch;
      g.fromResync = prev.state === 2 || prev.state === 3;
      g.wasGone = prev.state === 4;
      // The floor is not written here: the stale epoch was written when it was reached, and the
      // joined epoch is recorded only by groupJoined.
      g.maxEpoch = prev.maxEpoch;
      g.preJoinEpoch = prev.epoch;
    }
    g.joinEpoch = epoch + 1n;
    g.epoch = g.joinEpoch;
    this.gs.set(toHex(eg.groupId), g);
    return encode([wire.commit(epoch + 1n, this.me.device, [this.me.device], []), wire.info(eg.groupId, epoch + 1n)]);
  }

  groupJoined(groupId: Id, seq: bigint): void {
    this.log('groupJoined', groupId);
    const g = this.must(groupId);
    if (g.state !== 1) throw coreError('E_CORE_STATE', `group state ${String(g.state)}`);
    g.state = 2;
    g.epoch = g.joinEpoch;
    g.joinedEpoch = g.joinEpoch;
    if (seq + 1n > g.nextSeq) g.nextSeq = seq + 1n; // never lowered
    if (g.joinEpoch > g.maxEpoch) g.maxEpoch = g.joinEpoch;
    g.fromResync = false;
    g.wasGone = false;
    g.proposals.clear();
    g.pending = null;
  }

  welcomesApply(welcomesBody: Uint8Array, expected: ExpectedGroup[]): WelcomeOutcome[] {
    const out: WelcomeOutcome[] = [];
    for (const row of arr(decode(welcomesBody))) {
      const w = arr(row, 7);
      const welcomeId = u64(at(w, 0));
      const groupId = bin(at(w, 1), 16);
      const labelEpoch = u64(at(w, 2));
      const commitSeq = u64(at(w, 3));
      const labelTreeHash = bin(at(w, 6), 32);
      this.log('welcomesApply', groupId);
      const refuse = (reason: string): void => {
        out.push({ welcomeId, groupId, outcome: 2, reason });
      };
      const exp = expected.find((e) => same(e.groupId, groupId));
      if (exp === undefined) {
        out.push({ welcomeId, groupId, outcome: 3, reason: '' });
        continue;
      }
      const prev = this.gs.get(toHex(groupId));
      // Outcome 1 only for a working membership: a row in state 0, 1 or 2 (L-CORE-07).
      if (prev !== undefined && (prev.state === 0 || prev.state === 1 || prev.state === 2)) {
        out.push({ welcomeId, groupId, outcome: 1, reason: '' });
        continue;
      }
      // The core's order (groups.rs, mls/group.rs join_from_welcome_labelled): the row's binding, the
      // channel holder, the parse, then on the staged Welcome the binding and the group id
      // (E_BINDING; the stand-in wire carries no binding, so only the id is checked here), the
      // served label (E_CORE_INPUT), and last the floor. Each refusal stores nothing and leaves the
      // floor as it was.
      if (prev !== undefined && !this.boundTo(prev, exp.communityId, exp.channelId)) {
        refuse('E_CORE_STATE');
        continue;
      }
      if (this.holder(exp.channelId, groupId) !== undefined) {
        refuse('E_CORE_STATE');
        continue;
      }
      let joined: CborValue[];
      try {
        joined = arr(decode(bin(at(w, 4))), 4);
        if (str(at(joined, 0)) !== 'welcome') throw new Error('not a welcome');
      } catch {
        refuse('E_CORE_INPUT');
        continue;
      }
      if (!same(bin(at(joined, 1), 16), groupId)) {
        refuse('E_BINDING');
        continue;
      }
      const epoch = u64(at(joined, 2));
      // The joined state must be the one the server labelled (3f26670).
      if (epoch !== labelEpoch || !same(bin(at(joined, 3), 32), labelTreeHash)) {
        refuse('E_CORE_INPUT');
        continue;
      }
      if (prev !== undefined && epoch <= this.floorOf(prev)) {
        refuse('E_CORE_INPUT');
        continue;
      }
      // A row in state 3 or 4 is updated (its stale group state goes; its rows, outbox, cursor and
      // binding stay; nextSeq is never lowered).
      const g = this.fresh(groupId, exp.communityId, exp.channelId, 2);
      g.nextSeq = commitSeq + 1n;
      if (prev !== undefined) {
        g.rows = prev.rows;
        g.reactions = prev.reactions;
        g.pins = prev.pins;
        g.ackedSeq = prev.ackedSeq;
        g.ackedEpoch = prev.ackedEpoch;
        g.maxEpoch = prev.maxEpoch;
        if (prev.nextSeq > g.nextSeq) g.nextSeq = prev.nextSeq;
      }
      g.epoch = epoch;
      g.joinedEpoch = epoch;
      if (epoch > g.maxEpoch) g.maxEpoch = epoch;
      this.gs.set(toHex(groupId), g);
      out.push({ welcomeId, groupId, outcome: 0, reason: '' });
    }
    return out;
  }

  humanInteractionWelcomeApply(welcomesBody: Uint8Array, currentContextProof: Uint8Array, _now: bigint): WelcomeOutcome[] {
    void welcomesBody;
    void currentContextProof;
    void _now;
    this.log('humanInteractionWelcomeApply', new Uint8Array(16));
    throw new CoreError('E_CORE_STATE', 'human interaction Welcome context is not modelled');
  }

  humanInteractionSendPrepare(requestId: Id, communityId: Id, groupId: Id, requestBody: Uint8Array, now: bigint): Id {
    void requestId; void communityId; void groupId; void requestBody; void now;
    throw new CoreError('E_CORE_STATE', 'human interaction sends are not modelled');
  }
  humanInteractionEventsRead(after: bigint, limit: number): HumanInteractionEventPage {
    void after; void limit;
    throw new CoreError('E_CORE_STATE', 'human interaction events are not modelled');
  }
  humanInteractionReceiptsRead(communityId: Id, installationId: Id, botUserId: Id, requestIds: readonly Id[]): HumanInteractionEvent[] {
    void communityId; void installationId; void botUserId; void requestIds;
    throw new CoreError('E_CORE_STATE', 'human interaction receipts are not modelled');
  }
  humanInteractionEventsAck(through: bigint): void {
    void through;
    throw new CoreError('E_CORE_STATE', 'human interaction events are not modelled');
  }
  humanInteractionEventsAckSelected(deliveryIds: readonly bigint[]): void {
    void deliveryIds;
    throw new CoreError('E_CORE_STATE', 'human interaction events are not modelled');
  }

  // --- ordered processing (L-CORE-08) ---

  groupApply(groupId: Id, handshakes: Uint8Array, messages: Uint8Array, through: bigint): ApplyResult {
    this.log('groupApply', groupId);
    const g = this.must(groupId);
    const hs = arr(decode(handshakes)).map((r) => arr(r, 5));
    // web3.7: nine elements, the 9th the upload class; eight from a host that predates it (wire.rs decode_message_rows).
    const ms = arr(decode(messages)).map((r) => {
      const a = arr(r);
      if (a.length !== 8 && a.length !== 9) throw coreError('E_CORE_INPUT', `messages row has ${String(a.length)} elements`);
      return a;
    });
    this.applyCalls.push({ g: toHex(groupId), through, handshakes: hs.length, messages: ms.length });
    if (g.state !== 2) throw coreError('E_CORE_STATE', `state ${String(g.state)}`);
    const rows = new Map<bigint, { seq: bigint; hs: CborValue[] | null; msg: CborValue[] | null }>();
    for (const a of hs) {
      const seq = u64(at(a, 0));
      if (rows.has(seq)) throw coreError('E_CORE_INPUT', 'a seq appears twice');
      rows.set(seq, { seq, hs: a, msg: null });
    }
    for (const a of ms) {
      const seq = u64(at(a, 0));
      if (rows.has(seq)) throw coreError('E_CORE_INPUT', 'a seq appears twice');
      rows.set(seq, { seq, hs: null, msg: a });
    }
    const ordered = [...rows.values()].filter((r) => r.seq >= g.nextSeq && r.seq <= through).sort(bySeq);
    const newSeqs: bigint[] = [];
    let changed = false;
    let adopted = false;
    let stopped = false;
    for (const r of ordered) {
      if (r.hs !== null) {
        const o: HsOutcome = this.applyHandshake(g, r.hs);
        if (o === 'stop') {
          g.state = 3;
          stopped = true;
          break;
        }
        if (o === 'changed' || o === 'gone') changed = true;
        if (o === 'gone') {
          g.nextSeq = r.seq + 1n;
          stopped = true;
          break;
        }
      } else if (r.msg !== null && g.kind === 0) {
        // L-CORE-52 (sync.rs): a call group's application messages are skipped, never decrypted or folded.
        const m = this.applyMessage(g, r.msg);
        if (m.inserted) newSeqs.push(r.seq);
        if (m.adopted) adopted = true;
      }
      g.nextSeq = r.seq + 1n;
    }
    if (!stopped && through + 1n > g.nextSeq) g.nextSeq = through + 1n;
    return this.result(g, newSeqs, changed, adopted);
  }

  commitBuild(groupId: Id, proposalsBody: Uint8Array): Uint8Array {
    this.log('commitBuild', groupId);
    const g = this.must(groupId);
    if (g.state !== 2) throw coreError('E_CORE_STATE', `state ${String(g.state)}`);
    if (g.pending !== null) throw coreError('E_CORE_STATE', 'a commit is pending');
    const queued = new Map(g.proposals);
    for (const row of arr(decode(proposalsBody))) {
      const p = arr(row, 5);
      if (u64(at(p, 4)) === 1n) continue;
      const v = arr(decode(bin(at(p, 3))), 4);
      const ref = toHex(bin(at(v, 1), 16));
      if (!queued.has(ref)) queued.set(ref, { op: str(at(v, 2)) === 'add' ? 'add' : 'remove', device: bin(at(v, 3), 16) });
    }
    // sync.rs commit_build (ruling 3, drop_queued_adds): a queued Add whose leaf fails is left out of this commit; one
    // whose signed history is unknown refuses, writing nothing. A KeyPackage the double does not model is committed.
    for (const [ref, p] of queued) {
      const leaf = p.op === 'add' ? this.keyPackageLeaves.get(toHex(p.device)) : undefined;
      if (leaf === undefined) continue;
      const judged = this.judge(leaf);
      // trust.rs validate_proposed_add: a verdict recorded by device_list_ingest_add answers where the list is unknown.
      const recorded = [...this.verdicts].find(([key]) => key.endsWith(`/${toHex(leaf.dskPub)}`))?.[1];
      const verdict = judged === 'E_DEVICE_LIST_STALE' && recorded !== undefined ? recorded : judged;
      if (verdict === 'E_DEVICE_LIST_STALE') throw coreError(verdict, '');
      if (verdict !== null) queued.delete(ref);
    }
    g.proposals.clear();
    for (const [ref, p] of queued) g.proposals.set(ref, p);
    const adds = [...g.proposals.values()].filter((p) => p.op === 'add').map((p) => p.device);
    const removes = [...g.proposals.values()].filter((p) => p.op === 'remove').map((p) => p.device);
    g.pending = { epoch: g.epoch + 1n, removesMe: removes.some((d) => same(d, this.me.device)) };
    const welcomes = adds.map((d) => [d, wire.welcome(g.groupId, g.epoch + 1n)]);
    return encode([g.epoch, wire.commit(g.epoch + 1n, this.me.device, adds, removes), wire.info(g.groupId, g.epoch + 1n), welcomes, null]);
  }

  commitConfirm(groupId: Id): ApplyResult {
    this.log('commitConfirm', groupId);
    const g = this.must(groupId);
    if (g.state !== 2) throw coreError('E_CORE_STATE', `state ${String(g.state)}`);
    this.mergePending(g);
    g.proposals.clear();
    return this.result(g, [], true, false);
  }

  commitAbort(groupId: Id): void {
    this.log('commitAbort', groupId);
    this.must(groupId).pending = null;
  }

  cursorBody(groupId: Id): Uint8Array | null {
    this.log('cursorBody', groupId);
    const g = this.must(groupId);
    return g.nextSeq - 1n <= g.ackedSeq ? null : encode([g.nextSeq - 1n, g.epoch]);
  }

  cursorAcked(groupId: Id, lastSeq: bigint, lastEpoch: bigint): void {
    this.log('cursorAcked', groupId);
    const g = this.must(groupId);
    if (lastSeq > g.nextSeq - 1n) throw coreError('E_CORE_INPUT', 'cursor exceeds next seq');
    if (lastSeq <= g.ackedSeq) return;
    g.ackedSeq = lastSeq;
    g.ackedEpoch = lastEpoch;
  }

  /** Op 21 for a stored row (L-CORE-08 `message_deleted`): status 2, body ''; any state but 4. */
  messageDeleted(groupId: Id, seq: bigint): ApplyResult {
    this.log('messageDeleted', groupId);
    const g = this.must(groupId);
    if (g.state === 4) throw coreError('E_CORE_STATE', 'state 4');
    const row = g.rows.get(seq);
    if (row === undefined || row.status === 2) return this.result(g, [], false, false);
    g.rows.set(seq, this.deletedRow(row));
    this.refoldDeleted(g, row);
    return this.result(g, [seq], false, false);
  }
  removalCursor(groupId: Id): bigint { this.must(groupId); return 0n; }
  removalActorHistoryVersion(): bigint { throw coreError('E_CORE_STATE'); }
  refreshRemovalActorHistory(): bigint { throw coreError('E_CORE_STATE'); }
  ingestRemoval(): bigint { throw coreError('E_CORE_STATE'); }

  // --- sending and reading (L-CORE-09) ---

  sendPrepare(groupId: Id, request: SendRequest, now: bigint): Id {
    this.log('sendPrepare', groupId);
    // L-CORE-36 in the core's order (messages.rs send_prepare, wire::decode_send_request, Envelope::validate); every
    // refusal writes nothing.
    const { type, replyTo, body, attachments } = request;
    const threadId = request.threadId ?? null;
    const count = (n: unknown): boolean => typeof n === 'number' && Number.isSafeInteger(n) && n >= 0;
    const bytes = (b: unknown, length?: number): boolean => b instanceof Uint8Array && (length === undefined || b.length === length);
    const wellFormed = typeof body === 'string' && Array.isArray(attachments) && (replyTo === null || bytes(replyTo, 16)) &&
      (threadId === null || bytes(threadId, 16)) &&
      attachments.every((a) => bytes(a.blobId, 32) && bytes(a.key, 32) && bytes(a.nonce, 12) && count(a.size) &&
        typeof a.mime === 'string' && typeof a.name === 'string' && (a.w === null || count(a.w)) && (a.h === null || count(a.h)) &&
        (a.thumb === null || bytes(a.thumb)));
    if (!wellFormed) throw coreError('E_CORE_INPUT', 'request is malformed');
    if (!Number.isInteger(type) || type < 0 || type > 6) throw coreError('E_CORE_INPUT', 'type is out of range');
    if (type !== 0 && attachments.length > 0) throw coreError('E_CORE_INPUT', 'only a message carries attachments');
    if (type !== 0 && threadId !== null) throw coreError('E_CORE_INPUT', 'only a message starts or joins a thread');
    const g = this.must(groupId);
    // L-CORE-52 (messages.rs): sending stays text-only; a call group carries no messages.
    if (g.kind !== 0) throw coreError('E_CORE_INPUT', 'not a text group');
    // FX-core (messages.rs send_prepare): threads exist only in community channel groups.
    if (threadId !== null && g.communityId === null) throw coreError('E_CORE_INPUT', 'threads are for channel groups only');
    if (g.state !== 2) throw coreError('E_CORE_STATE', `group state ${String(g.state)}`);
    // messages.rs send_prepare: a changed-key or failed leaf still in the group blocks queuing (and again at send_encrypt).
    const block = this.sendBlock(groupId);
    if (block !== null) throw coreError(block, '');
    const target = replyTo === null ? null : this.resolve(g, replyTo);
    if (type === 0) {
      if (body.trim() === '' && attachments.length === 0) throw coreError('E_CORE_INPUT', 'body is empty');
      if (replyTo !== null && target === null) throw coreError('E_CORE_NOT_FOUND', 'target is not held');
      if (threadId !== null) {
        // messages.rs thread_root: the first type-0 row with the id decides; without one, another type or nothing.
        const named = [...g.rows.values()].filter((r) => r.msgId !== null && same(r.msgId, threadId));
        if (this.threadRoot(g, threadId) === null) {
          if (named.length === 0) throw coreError('E_CORE_NOT_FOUND', 'target is not held');
          if (named.some((r) => r.type === 0)) throw coreError('E_CORE_INPUT', 'the thread root is a thread reply');
          throw coreError('E_CORE_INPUT', 'the thread root is not a message');
        }
      }
    } else {
      if (replyTo === null) throw coreError('E_CORE_INPUT', 'reply_to is required');
      if (target === null) throw coreError('E_CORE_NOT_FOUND', 'target is not held');
      if (target.status !== 0) throw coreError('E_CORE_STATE', 'the target is deleted');
      if (type <= 2 && (target.senderUser === null || !same(target.senderUser, this.me.user))) {
        throw coreError('E_CORE_INPUT', 'only the author may edit or delete');
      }
      if (type === 1 && body.trim() === '') throw coreError('E_CORE_INPUT', 'body is empty');
      if ((type === 3 || type === 4) && (body === '' || utf8Length(body) > 32)) throw coreError('E_CORE_INPUT', 'emoji must be 1..=32 bytes');
      if ((type === 2 || type === 5 || type === 6) && body !== '') throw coreError('E_CORE_INPUT', 'body must be empty');
    }
    // Envelope::validate: the limits are UTF-8 bytes, not characters (envelope/mod.rs:17-31, 375-409).
    if (((type === 0 || type === 1) && utf8Length(body) > 4000) || attachments.length > 4 ||
      attachments.some((a) => utf8Length(a.mime) > 255 || (a.thumb !== null && a.thumb.length > 8192) || utf8Length(a.name) > 255)) {
      throw coreError('E_ENVELOPE_LIMIT');
    }
    this.msgs += 1;
    const msgId = idOf(0x6d, this.msgs);
    this.out.set(toHex(msgId), { msgId, groupId, body, created: now, state: 0, error: '', epoch: null, type, replyTo,
      attachments: attachments.map((a) => ({ ...a })), threadId });
    return msgId;
  }

  sendEncrypt(msgId: Id): { groupId: Id; messageBody: Uint8Array } {
    const o = this.out.get(toHex(msgId));
    if (o === undefined) throw coreError('E_CORE_NOT_FOUND');
    this.log('sendEncrypt', o.groupId);
    const g = this.must(o.groupId);
    if (o.state !== 0) throw coreError('E_CORE_STATE', 'the row is not queued');
    if (g.state !== 2) throw coreError('E_CORE_STATE', `state ${String(g.state)}`);
    for (const x of this.out.values()) {
      if (same(x.groupId, o.groupId) && x.state === 1) throw coreError('E_CORE_STATE', 'a message is in flight');
    }
    // The core frames no application message while a proposal or a commit is pending (L-CORE-09).
    if (g.proposals.size > 0) throw coreError('E_CORE_STATE', 'proposals are pending');
    if (g.pending !== null) throw coreError('E_CORE_STATE', 'a commit is pending');
    // messages.rs send_encrypt: the gate holds again there, for a row queued before the verdict arrived.
    const block = this.sendBlock(o.groupId);
    if (block !== null) throw coreError(block, '');
    o.state = 1;
    o.epoch = g.epoch;
    const framed = wire.app(this.me, o.msgId, o.body, { type: o.type, replyTo: o.replyTo, attachments: o.attachments, threadId: o.threadId });
    return { groupId: o.groupId, messageBody: encode([g.epoch, framed]) };
  }

  sendConfirm(msgId: Id, response: Uint8Array): { groupId: Id; seq: bigint } {
    const r = arr(decode(response), 3);
    const seq = u64(at(r, 0));
    const recvTs = u64(at(r, 2));
    const o = this.out.get(toHex(msgId));
    if (o !== undefined) {
      this.log('sendConfirm', o.groupId);
      if (o.state !== 1) throw coreError('E_CORE_STATE', `outbox row in state ${String(o.state)}`);
      const g = this.must(o.groupId);
      const held = g.rows.get(seq);
      if (held !== undefined) {
        // The deleted marker of this upload (its deletion was read first) completes the row; any
        // other row contradicts the answer: the row is failed (kept) and the call still throws.
        if (held.status === 2 && same(held.senderDevice, this.me.device) && (held.msgId === null || same(held.msgId, msgId))) {
          g.rows.set(seq, { ...held, msgId });
          this.out.delete(toHex(msgId));
          return { groupId: o.groupId, seq };
        }
        o.state = 2;
        o.error = 'E_CORE_STATE';
        throw coreError('E_CORE_STATE', 'message seq exists');
      }
      g.rows.set(seq, this.ownRow(seq, o.epoch ?? g.epoch, recvTs, o));
      this.refoldInsert(g, seq, o.type, o.msgId, o.replyTo, this.me.user, this.me.device);
      this.out.delete(toHex(msgId));
      return { groupId: o.groupId, seq };
    }
    for (const g of this.gs.values()) {
      for (const row of g.rows.values()) {
        if (row.msgId !== null && same(row.msgId, msgId)) {
          this.log('sendConfirm', g.groupId);
          return { groupId: g.groupId, seq: row.seq };
        }
      }
    }
    throw coreError('E_CORE_NOT_FOUND');
  }

  sendRequeue(msgId: Id): void {
    const o = this.mustOut(msgId, 'sendRequeue');
    if (o.state !== 1) throw coreError('E_CORE_STATE', `outbox state ${String(o.state)}`);
    o.state = 0;
  }

  sendFail(msgId: Id, error: string): void {
    const o = this.mustOut(msgId, 'sendFail');
    if (o.state === 2) throw coreError('E_CORE_STATE', 'outbox state 2');
    if (new TextEncoder().encode(error).length > 64) throw coreError('E_CORE_INPUT', 'error longer than 64 bytes');
    o.state = 2;
    o.error = error;
  }

  sendRetry(msgId: Id): void {
    const o = this.mustOut(msgId, 'sendRetry');
    if (o.state !== 2) throw coreError('E_CORE_STATE', `outbox state ${String(o.state)}`);
    o.state = 0;
    o.error = '';
  }

  sendDiscard(msgId: Id): void {
    const o = this.mustOut(msgId, 'sendDiscard');
    if (o.state === 1) throw coreError('E_CORE_STATE', 'outbox state 1');
    this.out.delete(toHex(msgId));
  }

  outbox(groupId: Id): OutboxRow[] {
    return [...this.out.values()]
      .filter((o) => same(o.groupId, groupId))
      .sort((a, b) => (a.created !== b.created ? (a.created < b.created ? -1 : 1) : toHex(a.msgId) < toHex(b.msgId) ? -1 : 1))
      // L-CORE-37: the attachment summary only; no key, nonce or thumbnail leaves the outbox.
      .map((o) => ({ msgId: o.msgId, state: o.state, error: o.error, created: o.created, body: o.body, type: o.type, replyTo: o.replyTo,
        attachments: o.attachments.map((a) => ({ blobId: a.blobId, size: a.size, mime: a.mime, name: a.name })) }));
  }

  /** L-CORE-37 `pins`: every pin whose target resolves, newest pin first. */
  pins(groupId: Id): PinRow[] {
    const g = this.must(groupId);
    const out: PinRow[] = [];
    for (const p of [...g.pins.values()].sort((a, b) => (a.seq > b.seq ? -1 : a.seq < b.seq ? 1 : 0))) {
      const t = this.resolve(g, p.target);
      if (t === null) continue;
      out.push({ targetSeq: t.seq, msgId: p.target, pinnedSeq: p.seq, byUser: p.byUser, author: t.senderUser,
        excerpt: excerpt(t.editBody ?? t.body), targetTs: t.recvTs, threadRoot: this.shownThreadRoot(g, t) });
    }
    return out;
  }

  /** L-CORE-38 `attachment_get`: the full descriptor of a held status-0 type-0 row's attachment. */
  attachmentGet(groupId: Id, seq: bigint, index: number): AttachmentDescriptor {
    const row = this.gs.get(toHex(groupId))?.rows.get(seq);
    const a = row !== undefined && row.status === 0 && row.type === 0 && row.hasEnvelope ? row.attachments[index] : undefined;
    if (a === undefined) throw coreError('E_CORE_NOT_FOUND', 'no such attachment');
    return { ...a };
  }

  /** L-CORE-38 `purges`: every recorded purge, ordered by group then seq. */
  purges(): PurgeRow[] {
    return [...this.purgeRows.values()]
      .sort((a, b) => (toHex(a.groupId) !== toHex(b.groupId) ? (toHex(a.groupId) < toHex(b.groupId) ? -1 : 1) : bySeq(a, b)))
      .map((p) => ({ ...p, blobIds: [...p.blobIds] }));
  }

  /** L-CORE-38 `purge_done`: removes one purge row, absent or not. */
  purgeDone(groupId: Id, seq: bigint): void {
    this.log('purgeDone', groupId);
    this.purgeRows.delete(`${toHex(groupId)}/${String(seq)}`);
  }

  /** L-CORE-35 `own_roles_set`: replaces the community's own role ids. */
  ownRolesSet(communityId: Id, roleIds: Id[]): void {
    if (roleIds.length > 64 || roleIds.some((r) => !(r instanceof Uint8Array) || r.length !== 16)) {
      throw coreError('E_CORE_INPUT', 'role_ids must be 0..=64 ids of 16 bytes');
    }
    // INSERT OR IGNORE: a repeated id is held once.
    this.roles.set(toHex(communityId), [...new Map(roleIds.map((r) => [toHex(r), r.slice()])).values()]);
  }

  /** web3.5 R1 `authority_set`: replaces the group's role view, refolds every pinned or unpinned target and recomputes
   *  the mention flag of other users' readable rows naming a <@…> token: every such row on a first view or when the
   *  mentionable roles changed, else only those of senders whose broadcast bit changed (W2a review finding 1). The group
   *  need not be held; a held DM group is refused with E_CORE_STATE. */
  authoritySet(groupId: Id, pinners: Id[], broadcasters: Id[], mentionable: Id[]): void {
    const bad = (ids: Id[]): boolean => ids.length > 4096 || ids.some((r) => !(r instanceof Uint8Array) || r.length !== 16);
    if (bad(pinners) || bad(broadcasters) || bad(mentionable)) {
      throw coreError('E_CORE_INPUT', 'pinners, broadcasters and mentionable must be 0..=4096 ids of 16 bytes');
    }
    const g = this.gs.get(toHex(groupId));
    // messages.rs (98bc249): a DM group has no roles; nothing is written.
    if (g !== undefined && g.communityId === null) throw coreError('E_CORE_STATE', 'a DM group has no role view');
    const before = this.authority.get(toHex(groupId));
    const next = { pinners: new Set(pinners.map(toHex)), broadcasters: new Set(broadcasters.map(toHex)), mentionable: new Set(mentionable.map(toHex)) };
    this.authority.set(toHex(groupId), next);
    if (g === undefined) return;
    const targets = new Map<string, Id>();
    for (const r of g.rows.values()) if (r.type !== null && (r.type === 5 || r.type === 6) && r.replyTo !== null) targets.set(toHex(r.replyTo), r.replyTo);
    for (const target of targets.values()) this.refold(g, target, null);
    const everySender = before === undefined || before.mentionable.size !== next.mentionable.size
      || [...next.mentionable].some((role) => !before.mentionable.has(role));
    for (const r of [...g.rows.values()]) {
      if (r.status !== 0 || r.type !== 0 || r.senderUser === null || same(r.senderUser, this.me.user)) continue;
      if (!r.body.includes('<@')) continue;
      const sender = toHex(r.senderUser);
      if (!everySender && before.broadcasters.has(sender) === next.broadcasters.has(sender)) continue;
      const mention = this.mentionOf(g, r.body, r.senderUser);
      if (mention !== r.mention) g.rows.set(r.seq, { ...r, mention });
    }
  }

  /** A pin or unpin counts from any member while the group's view is unknown, else only from a pinner (bit 3). */
  private mayPin(g: MGroup, user: Id | null): boolean {
    const view = this.authority.get(toHex(g.groupId));
    return view === undefined || (user !== null && view.pinners.has(toHex(user)));
  }

  /** messages.rs mentions_me_gated: <@everyone>/<@here> and own-role tokens count from a broadcaster (bit 18) or while the
   *  view is unknown; from anyone else only a direct mention and a token of an own role the view marks mentionable. */
  private mentionOf(g: MGroup, body: string, sender: Id): boolean {
    const view = this.authority.get(toHex(g.groupId));
    const me = toHex(this.me.user);
    const roles = this.roleHexes(g);
    if (view === undefined || view.broadcasters.has(toHex(sender))) return mentionsMe(body, me, roles);
    return body.includes(`<@${me}>`) || roles.some((role) => view.mentionable.has(role) && body.includes(`<@${role}>`));
  }

  /** L-CORE-34: displayable rows (type null or 0) with the fold applied; web3.7: a reply whose root is held before it
   *  shows in its thread only (messages.rs timeline_sql, HELD_ROOT). */
  timeline(groupId: Id, beforeSeq: bigint, limit: number): TimelineRow[] {
    const g = this.must(groupId);
    const rows = [...g.rows.values()]
      .filter((r) => (r.type === null || r.type === 0) && this.shownThreadRoot(g, r) === null &&
        (beforeSeq === 0n || r.seq < beforeSeq)).sort(bySeq);
    return rows.slice(Math.max(0, rows.length - limit)).map((r) => this.view(g, r));
  }

  /** web3.7 (R25): the held root and the thread's type-0 replies after it, newest first (messages.rs thread_timeline). */
  threadTimeline(groupId: Id, rootMsgId: Id, beforeSeq: bigint, limit: number): { root: TimelineRow | null; rows: TimelineRow[] } {
    const g = this.must(groupId);
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) throw coreError('E_CORE_INPUT', 'limit out of range');
    const root = this.threadRoot(g, rootMsgId);
    const after = root?.seq ?? 0n;
    const rows = [...g.rows.values()]
      .filter((r) => r.type === 0 && r.threadId !== null && same(r.threadId, rootMsgId) && r.seq > after &&
        (beforeSeq === 0n || r.seq < beforeSeq))
      .sort(bySeq).reverse().slice(0, limit);
    return { root: root === null ? null : this.view(g, root), rows: rows.map((r) => this.view(g, r)) };
  }

  /** web3.7: the group's threads under a held first root, at most 50, the latest reply first (messages.rs THREADS_SQL;
   *  fix round 1, M1; fix round 2: a fold reusing the root's id changes nothing). */
  threads(groupId: Id): ThreadRow[] {
    const g = this.must(groupId);
    const out: (ThreadRow & { id: string })[] = [];
    for (const root of g.rows.values()) {
      if (root.msgId === null || this.threadRoot(g, root.msgId) !== root) continue;
      const msgId = root.msgId;
      const summary = this.summary(g, root);
      if (summary === null) continue;
      out.push({ rootMsgId: msgId, rootSeq: root.seq, count: summary.count, lastTs: summary.lastTs,
        excerpt: root.status === 0 ? excerpt(root.editBody ?? root.body) : '', id: toHex(msgId) });
    }
    return out
      .sort((a, b) => (a.lastTs === b.lastTs ? a.id.localeCompare(b.id) : a.lastTs > b.lastTs ? -1 : 1))
      .slice(0, 50)
      .map((t): ThreadRow => ({ rootMsgId: t.rootMsgId, rootSeq: t.rootSeq, count: t.count, lastTs: t.lastTs, excerpt: t.excerpt }));
  }

  /** The thread root of `msgId`: the first type-0 row carrying it, when it carries no thread id (fix round 1, I1). */
  private threadRoot(g: MGroup, msgId: Id): MRow | null {
    let t: MRow | null = null;
    for (const r of g.rows.values()) {
      if (r.type === 0 && r.msgId !== null && same(r.msgId, msgId) && (t === null || r.seq < t.seq)) t = r;
    }
    return t !== null && t.threadId === null ? t : null;
  }

  /** The root of the thread `r` shows in: its thread id when that root is held before it, else null (shown_thread_root). */
  private shownThreadRoot(g: MGroup, r: MRow): Id | null {
    if (r.type !== 0 || r.threadId === null) return null;
    const root = this.threadRoot(g, r.threadId);
    return root !== null && root.seq < r.seq ? r.threadId : null;
  }

  /** The 19-element view of one stored row (messages.rs view_of). */
  private view(g: MGroup, r: MRow): TimelineRow {
    const ok = r.status === 0;
    let reply: TimelineRow['reply'] = null;
    if (ok && r.replyTo !== null) {
      const t = this.resolve(g, r.replyTo);
      reply = t === null
        ? { replyTo: r.replyTo, targetSeq: null, targetUser: null, excerpt: '', state: 1, threadRoot: null }
        : { replyTo: r.replyTo, targetSeq: t.seq, targetUser: t.senderUser, excerpt: t.status === 0 ? excerpt(t.editBody ?? t.body) : '',
          state: t.status === 0 ? 0 : 2, threadRoot: this.shownThreadRoot(g, t) };
    }
    // Reactions and the pin belong to a target only: a row that repeats a msg id never shows them.
    const target = ok && r.type === 0 && r.msgId !== null && this.resolve(g, r.msgId)?.seq === r.seq;
    return {
      seq: r.seq, epoch: r.epoch, recvTs: r.recvTs, status: r.status, reason: r.reason, senderUser: r.senderUser,
      senderDevice: r.senderDevice, senderKind: r.senderKind, senderTier: r.senderTier, msgId: r.msgId, type: r.type,
      body: ok ? r.editBody ?? r.body : '', editedSeq: r.editSeq, reply,
      reactions: target && r.msgId !== null ? this.chips(g, r.msgId) : [],
      pinned: target && r.msgId !== null && g.pins.has(toHex(r.msgId)),
      attachments: ok && r.type === 0
        ? r.attachments.map((a, index) => ({ index, size: a.size, mime: a.mime, w: a.w, h: a.h, hasThumb: a.thumb !== null, name: a.name }))
        : [],
      mention: r.mention,
      thread: this.summary(g, r),
    };
  }

  /** A root's thread summary: the count and latest recvTs of its shown type-0 replies after it; null on any other row. */
  private summary(g: MGroup, r: MRow): TimelineRow['thread'] {
    if (r.msgId === null || this.threadRoot(g, r.msgId) !== r) return null;
    const msgId = r.msgId;
    const replies = [...g.rows.values()].filter((x) => x.type === 0 && x.status === 0 && x.threadId !== null && same(x.threadId, msgId) &&
      x.seq > r.seq);
    if (replies.length === 0) return null;
    return { count: replies.length, lastTs: replies.reduce((m, x) => (x.recvTs > m ? x.recvTs : m), 0n) };
  }

  /**
   * A model-only probe of a stored row, fold rows included (FACTS-SECURITY-03): its stored body (never the folded
   * edit) and whether the envelope column still holds bytes. Logs no call; it is not a CorePort member.
   */
  storedRow(groupId: Id, seq: bigint): { type: number | null; status: 0 | 1 | 2; body: string; hasEnvelope: boolean } | null {
    const r = this.gs.get(toHex(groupId))?.rows.get(seq);
    return r === undefined ? null : { type: r.type, status: r.status, body: r.body, hasEnvelope: r.hasEnvelope };
  }

  // --- test accessors ---

  group(groupId: Id): GroupInfo | undefined {
    const g = this.gs.get(toHex(groupId));
    return g === undefined ? undefined : this.info(g);
  }

  /** The group's epoch floor (app_groups.max_epoch joined with the stored state's epoch), for the model tests. */
  floor(groupId: Id): bigint | undefined {
    const g = this.gs.get(toHex(groupId));
    return g === undefined ? undefined : this.floorOf(g);
  }

  /** Bodies of the readable rows, ascending by seq. */
  bodies(groupId: Id): string[] {
    const g = this.gs.get(toHex(groupId));
    if (g === undefined) return [];
    return [...g.rows.values()].filter((r) => r.status === 0).sort(bySeq).map((r) => r.body);
  }

  /** The unreadable rows, ascending by seq. */
  unreadable(groupId: Id): { seq: bigint; reason: string }[] {
    const g = this.gs.get(toHex(groupId));
    if (g === undefined) return [];
    return [...g.rows.values()].filter((r) => r.status === 1).sort(bySeq).map((r) => ({ seq: r.seq, reason: r.reason }));
  }

  resetLog(): void {
    this.calls.length = 0;
    this.applyCalls.length = 0;
  }

  // --- internals ---

  private log(m: string, groupId: Id): void {
    this.calls.push({ m, g: toHex(groupId) });
  }

  private must(groupId: Id): MGroup {
    const g = this.gs.get(toHex(groupId));
    if (g === undefined) throw coreError('E_CORE_NOT_FOUND');
    return g;
  }

  private mustOut(msgId: Id, m: string): MOut {
    const o = this.out.get(toHex(msgId));
    if (o === undefined) throw coreError('E_CORE_NOT_FOUND');
    this.log(m, o.groupId);
    return o;
  }

  private fresh(groupId: Id, communityId: Id | null, targetId: Id, state: 0 | 1 | 2, kind: 0 | 1 = 0): MGroup {
    return {
      groupId, kind, ownLeaf: 0, communityId, targetId, state, epoch: 0n, joinedEpoch: 0n, joinEpoch: 0n, preJoinEpoch: 0n, nextSeq: 1n, ackedSeq: 0n, ackedEpoch: 0n,
      proposals: new Map(), pending: null, fromResync: false, wasGone: false, maxEpoch: 0n, rows: new Map(), held: new Map(),
      reactions: new Map(), pins: new Map(),
    };
  }

  private info(g: MGroup): GroupInfo {
    return {
      groupId: g.groupId, kind: g.kind, communityId: g.communityId, targetId: g.targetId, state: g.state,
      epoch: g.state === 4 ? 0n : g.epoch, nextSeq: g.nextSeq, proposalsPending: g.proposals.size, pendingCommit: g.pending !== null,
    };
  }

  private result(g: MGroup, newSeqs: bigint[], epochChanged: boolean, ownAdopted: boolean): ApplyResult {
    return {
      state: g.state, epoch: g.state === 4 ? 0n : g.epoch, nextSeq: g.nextSeq, newSeqs,
      proposalsPending: g.proposals.size, epochChanged, ownAdopted,
    };
  }

  /** Rule 1 of L-CORE-08 for one handshake row [seq, epoch, kind, sender, blob]. */
  private applyHandshake(g: MGroup, a: CborValue[]): HsOutcome {
    const framed = u64(at(a, 1));
    const kind = u64(at(a, 2));
    let v: CborValue[];
    try {
      v = arr(decode(bin(at(a, 4))));
    } catch {
      return kind === 0n ? 'skip' : 'stop';
    }
    if (framed < g.epoch) return 'skip';
    const tag = str(at(v, 0));
    if (tag === 'prop') {
      if (framed !== g.epoch) return 'skip';
      const ref = toHex(bin(at(v, 1), 16));
      if (!g.proposals.has(ref)) g.proposals.set(ref, { op: str(at(v, 2)) === 'add' ? 'add' : 'remove', device: bin(at(v, 3), 16) });
      return 'ok';
    }
    if (tag !== 'commit' || framed !== g.epoch) return kind === 0n ? 'skip' : 'stop';
    if (same(bin(at(v, 2), 16), this.me.device)) {
      // An own commit: the pending one (OwnPendingCommit), or one this core no longer holds.
      if (g.pending === null) return 'stop';
      return this.mergePending(g);
    }
    // A staged merge clears any pending commit (openmls 0.9.0 processing.rs:462-463).
    g.proposals.clear();
    g.pending = null;
    if (arr(at(v, 4)).some((d) => same(bin(d, 16), this.me.device))) {
      // Removed: the group state goes without a merge, and the floor is not written (the last
      // epoch held was written when it was reached).
      g.state = 4;
      return 'gone';
    }
    g.epoch = u64(at(v, 1));
    this.raiseFloor(g, g.epoch);
    return 'changed';
  }

  private mergePending(g: MGroup): 'changed' | 'gone' {
    const p = g.pending;
    g.pending = null;
    g.proposals.clear();
    if (p === null) return 'changed';
    g.epoch = p.epoch;
    this.raiseFloor(g, g.epoch);
    if (p.removesMe) {
      g.state = 4;
      return 'gone';
    }
    return 'changed';
  }

  /** Rules 2-5 of L-CORE-08 (as amended) for one message row [seq, epoch, uploader, blob, commitment, franking, recv_ts, deleted]. */
  private applyMessage(g: MGroup, a: CborValue[]): { inserted: boolean; adopted: boolean } {
    const seq = u64(at(a, 0));
    const epoch = u64(at(a, 1));
    const uploader = bin(at(a, 2), 16);
    const blob = at(a, 3);
    const served = at(a, 4) === null ? null : bin(at(a, 4), 32);
    const recvTs = u64(at(a, 6));
    const deleted = u64(at(a, 7)) === 1n;
    const bare = (status: 1 | 2, reason: string): MRow => ({
      seq, epoch, recvTs, status, reason, senderUser: null, senderDevice: uploader, senderKind: null, senderTier: null,
      msgId: null, type: null, body: '', replyTo: null, threadId: null, editBody: null, editSeq: 0n, mention: false, attachments: [],
      hasEnvelope: false,
    });
    const stored = g.rows.get(seq);
    // A live row at a stored seq is skipped before anything else: not listed, nothing processed.
    if (!deleted && stored !== undefined) return { inserted: false, adopted: false };
    if (deleted) {
      if (stored !== undefined) {
        g.rows.set(seq, this.deletedRow(stored));
        this.refoldDeleted(g, stored);
        return { inserted: true, adopted: false };
      }
      // The deleted upload of an unresolved outbox row: the server stored it, so the row is done. The marker keeps
      // the outbox row's type and reply_to, and refolds what it named (sync.rs apply_message).
      const o = same(uploader, this.me.device) && served !== null ? this.unresolvedWith(g, served) : undefined;
      if (o !== undefined) {
        const marker: MRow = {
          ...bare(2, ''), senderUser: this.me.user, senderDevice: this.me.device, senderKind: 0, senderTier: 1, msgId: o.msgId, type: o.type,
          replyTo: o.replyTo, threadId: o.type === 0 ? o.threadId : null,
        };
        g.rows.set(seq, marker);
        this.refoldDeleted(g, marker);
        this.out.delete(toHex(o.msgId));
        return { inserted: true, adopted: true };
      }
      g.rows.set(seq, bare(2, ''));
      return { inserted: true, adopted: false };
    }
    if (blob === null) {
      g.rows.set(seq, bare(1, 'E_PRUNED'));
      return { inserted: true, adopted: false };
    }
    if (same(uploader, this.me.device)) {
      // Adopted only as the upload of the outbox row (any state) whose commitment the row carries:
      // the served field, else the blob's; when both are there and differ it carries none.
      const carried = commitmentOf(bin(blob));
      const c = served !== null && carried !== null && !same(served, carried) ? null : served ?? carried;
      const o = c === null ? undefined : this.unresolvedWith(g, c);
      if (o !== undefined) {
        g.rows.set(seq, this.ownRow(seq, epoch, recvTs, o));
        this.refoldInsert(g, seq, o.type, o.msgId, o.replyTo, this.me.user, this.me.device);
        this.out.delete(toHex(o.msgId));
        return { inserted: true, adopted: true };
      }
      g.rows.set(seq, bare(1, 'E_OWN_UNKNOWN'));
      return { inserted: true, adopted: false };
    }
    // A malformed stand-in is a row that cannot be read, as a ciphertext the group refuses.
    const v = readApp(bin(blob));
    if (v === null || epoch > g.epoch || epoch < g.joinedEpoch) {
      g.rows.set(seq, bare(1, 'E_CORE_MLS'));
      return { inserted: true, adopted: false };
    }
    // Envelope::decode fails inside process_message, before the sender check: the protocol code is stored
    // (client/error.rs:49, sync.rs:653-658; ruled: peer-client 10).
    if (v.type !== 0 && (v.replyTo === null || v.attachments.length > 0)) {
      g.rows.set(seq, bare(1, 'E_ENVELOPE_SHAPE'));
      return { inserted: true, adopted: false };
    }
    if (!same(v.device, uploader)) {
      g.rows.set(seq, bare(1, 'E_SENDER_MISMATCH'));
      return { inserted: true, adopted: false };
    }
    // sync.rs validate_cached_sender: a sender leaf with a failed verdict leaves its message unreadable, the evidence held
    // until that exact leaf validates (revalidate_pending, trust_verify). The double holds only definite verdicts: a
    // leaf whose list is unknown (E_DEVICE_LIST_STALE) is shown, as before ABI 8.
    const leaf = this.leafOf(g.groupId, v.user, v.device);
    if (leaf !== undefined) {
      const judged = this.judge(leaf);
      const code = this.verdicts.get(`${toHex(g.groupId)}/${toHex(leaf.dskPub)}`) ?? (judged === 'E_DEVICE_LIST_STALE' ? null : judged);
      if (code !== null) {
        g.rows.set(seq, bare(1, code));
        g.held.set(seq, v);
        return { inserted: true, adopted: false };
      }
    }
    this.insertReadable(g, seq, epoch, recvTs, v);
    return { inserted: true, adopted: false };
  }

  /** A readable row of another sender (sync.rs insert_message + refold_insert), as received or released at its seq. */
  private insertReadable(g: MGroup, seq: bigint, epoch: bigint, recvTs: bigint, v: Held): void {
    g.rows.set(seq, {
      seq, epoch, recvTs, status: 0, reason: '', senderUser: v.user, senderDevice: v.device, senderKind: 0, senderTier: 0,
      // FX-core (messages.rs insert_message): a thread id is stored in a community channel group only.
      msgId: v.msgId, type: v.type, body: v.body, replyTo: v.replyTo, threadId: v.type === 0 && g.communityId !== null ? v.threadId : null,
      editBody: null, editSeq: 0n,
      // L-CORE-35: only a type-0 row of another user, against the roles known now; recomputed later only by authoritySet.
      mention: v.type === 0 && !same(v.user, this.me.user) && this.mentionOf(g, v.body, v.user),
      attachments: v.type === 0 ? v.attachments : [], hasEnvelope: true,
    });
    this.refoldInsert(g, seq, v.type, v.msgId, v.replyTo, v.user, v.device);
  }

  private ownRow(seq: bigint, epoch: bigint, recvTs: bigint, o: MOut): MRow {
    return {
      seq, epoch, recvTs, status: 0, reason: '', senderUser: this.me.user, senderDevice: this.me.device, senderKind: 0, senderTier: 1,
      msgId: o.msgId, type: o.type, body: o.body, replyTo: o.replyTo, threadId: o.type === 0 ? o.threadId : null, editBody: null, editSeq: 0n,
      mention: false, attachments: o.attachments.map((x) => ({ ...x })), hasEnvelope: true,
    };
  }

  /** The row after the delivery service deleted it: status 2, nothing readable, its envelope dropped. */
  private deletedRow(row: MRow): MRow {
    return { ...row, status: 2, reason: '', body: '', attachments: [], hasEnvelope: false };
  }

  private roleHexes(g: MGroup): string[] {
    return g.communityId === null ? [] : (this.roles.get(toHex(g.communityId)) ?? []).map(toHex);
  }

  /** refold_insert: a status-0 insert refolds its target (type 0: its own msg id; 1..=6: reply_to) with itself as trigger. */
  private refoldInsert(g: MGroup, seq: bigint, ty: number, msgId: Id, replyTo: Id | null, senderUser: Id, senderDevice: Id): void {
    const target = ty === 0 ? msgId : replyTo;
    if (target !== null) this.refold(g, target, { seq, ty, senderUser, senderDevice });
  }

  /** refold_deleted: a row that became status 2 refolds what it named (type 0: its msg id; 1..=6: reply_to), no trigger. */
  private refoldDeleted(g: MGroup, row: MRow): void {
    const target = row.type === 0 ? row.msgId : row.type !== null ? row.replyTo : null;
    if (target !== null) this.refold(g, target, null);
  }

  /**
   * fold::resolve: the lowest-seq type-0 row with status 0 or 2 carrying `msgId`. lead-fold-spoofing (2026-10-07): a
   * type-0 row whose msg id a stored row of lower seq already names (a fold's or a reply's reply_to) is a repeat and
   * never a target; honest clients name only a msg id they received, so an honest target precedes every row naming it.
   */
  private resolve(g: MGroup, msgId: Id): MRow | null {
    let t: MRow | null = null;
    for (const r of g.rows.values()) {
      if (r.type === 0 && (r.status === 0 || r.status === 2) && r.msgId !== null && same(r.msgId, msgId) && (t === null || r.seq < t.seq)) t = r;
    }
    if (t === null) return null;
    const seq = t.seq;
    for (const r of g.rows.values()) if (r.seq < seq && r.replyTo !== null && same(r.replyTo, msgId)) return null;
    return t;
  }

  /** The status-0 fold rows of `type` that name `target`, sequenced after the target when one is given (lead-fold-spoofing). */
  private foldsOf(g: MGroup, target: Id, types: readonly number[], after: bigint): MRow[] {
    return [...g.rows.values()].filter((r) => r.status === 0 && r.type !== null && types.includes(r.type) && r.replyTo !== null &&
      same(r.replyTo, target) && r.seq > after).sort(bySeq);
  }

  /** Blanks a stored row's body and drops its envelope (the core's `body = '', envelope = NULL`). */
  private blank(g: MGroup, r: MRow): void {
    g.rows.set(r.seq, { ...r, body: '', hasEnvelope: false });
  }

  /**
   * fold::refold (L-CORE-33 as amended by S1/S11 and lead-fold-spoofing): recomputes the target's derived state from
   * the stored rows. Nothing here refuses; a rule breach is ignored.
   */
  private refold(g: MGroup, target: Id, trigger: FoldTrigger | null): void {
    const key = toHex(target);
    const t = this.resolve(g, target);
    if (t === null) {
      // Step 1, not held: a user's own delete blanks only that user's edits (every call but a type-3..6 insert).
      if (trigger !== null && trigger.ty >= 3) return;
      const deleters = new Map<string, Id>();
      for (const r of this.foldsOf(g, target, [2], -1n)) if (r.senderUser !== null) deleters.set(toHex(r.senderUser), r.senderUser);
      for (const user of deleters.values()) {
        for (const r of this.foldsOf(g, target, [1], -1n)) if (r.senderUser !== null && same(r.senderUser, user)) this.blank(g, r);
      }
      return;
    }
    // A fold counts only when sequenced after its target (lead-fold-spoofing); a repeat of a held type 0 refolds nothing.
    if (trigger !== null && trigger.seq < t.seq) return;
    if (trigger !== null && trigger.ty === 0 && trigger.seq !== t.seq) return;
    const author = t.senderUser;
    // S1(1): a delete by anyone but the author changes nothing; nor does one of a target already deleted.
    if (trigger !== null && trigger.ty === 2 && (author === null || !same(trigger.senderUser, author) || t.status === 2)) return;
    // S1(3): a fold arriving for a deleted target: an edit is blanked; nothing else is touched.
    if (t.status === 2 && trigger !== null && trigger.ty !== 0) {
      if (trigger.ty === 1) {
        const r = g.rows.get(trigger.seq);
        if (r !== undefined) this.blank(g, r);
      }
      return;
    }
    // Step 2: deleted by the delivery service, or by its author after it.
    const authorDeleted = author !== null && this.foldsOf(g, target, [2], t.seq).some((r) => r.senderUser !== null && same(r.senderUser, author));
    if (t.status === 2 || authorDeleted) {
      // The purge (task 3): only this device's own delete of its own user's readable message.
      if (trigger !== null && trigger.ty === 2 && same(trigger.senderDevice, this.me.device) && same(trigger.senderUser, this.me.user) &&
        author !== null && same(author, this.me.user) && t.status === 0) {
        const purgeKey = `${toHex(g.groupId)}/${String(t.seq)}`;
        if (!this.purgeRows.has(purgeKey)) {
          this.purgeRows.set(purgeKey, { groupId: g.groupId, seq: t.seq, channelId: g.targetId, blobIds: t.attachments.map((x) => x.blobId) });
        }
      }
      g.rows.set(t.seq, { ...t, status: 2, body: '', reason: '', editBody: null, editSeq: 0n, attachments: [], hasEnvelope: false });
      // S11: the author's edits (any status) are blanked; another user's ignored edits keep their stored words.
      if (author !== null) {
        for (const r of [...g.rows.values()]) {
          if (r.type === 1 && r.replyTo !== null && same(r.replyTo, target) && r.senderUser !== null && same(r.senderUser, author)) this.blank(g, r);
        }
      }
      g.reactions.delete(key);
      g.pins.delete(key);
      return;
    }
    // Step 3: the author's highest-seq edit; the mention flag is untouched.
    const edits = author === null ? [] : this.foldsOf(g, target, [1], t.seq).filter((r) => r.senderUser !== null && same(r.senderUser, author));
    const edit = edits[edits.length - 1];
    g.rows.set(t.seq, { ...t, editBody: edit?.body ?? null, editSeq: edit?.seq ?? 0n });
    // Step 4: per (user, emoji) the highest-seq add or remove decides.
    const latest = new Map<string, MRow>();
    for (const r of this.foldsOf(g, target, [3, 4], t.seq)) if (r.senderUser !== null) latest.set(`${toHex(r.senderUser)}\u0000${r.body}`, r);
    const present = new Map<string, { user: Id; emoji: string; seq: bigint }>();
    for (const [k, r] of latest) if (r.type === 3 && r.senderUser !== null) present.set(k, { user: r.senderUser, emoji: r.body, seq: r.seq });
    if (present.size > 0) g.reactions.set(key, present);
    else g.reactions.delete(key);
    // Step 5: the highest-seq pin or unpin that counts (web3.5 R1: from a pinner once the view is known) decides.
    const pin = this.foldsOf(g, target, [5, 6], t.seq).filter((r) => this.mayPin(g, r.senderUser)).pop();
    if (pin !== undefined && pin.type === 5 && pin.senderUser !== null) g.pins.set(key, { target: target.slice(), seq: pin.seq, byUser: pin.senderUser });
    else g.pins.delete(key);
  }

  /** The reaction chips of a target (messages.rs timeline): the 20 most counted, shown by first seq then emoji bytes. */
  private chips(g: MGroup, target: Id): TimelineRow['reactions'] {
    const by = new Map<string, { emoji: string; users: number; mine: boolean; first: bigint }>();
    for (const r of g.reactions.get(toHex(target))?.values() ?? []) {
      const c = by.get(r.emoji) ?? { emoji: r.emoji, users: 0, mine: false, first: r.seq };
      c.users += 1;
      c.mine ||= same(r.user, this.me.user);
      if (r.seq < c.first) c.first = r.seq;
      by.set(r.emoji, c);
    }
    const order = (a: { first: bigint; emoji: string }, b: { first: bigint; emoji: string }): number =>
      a.first !== b.first ? (a.first < b.first ? -1 : 1) : utf8Order(a.emoji, b.emoji);
    return [...by.values()]
      .sort((a, b) => (a.users !== b.users ? b.users - a.users : order(a, b)))
      .slice(0, 20)
      .sort(order)
      .map((c) => ({ emoji: c.emoji, count: c.users, mine: c.mine }));
  }

  /** The matching outbox row of L-CORE-08: the group's first row (created, msgId), in any state, whose commitment is c. */
  private unresolvedWith(g: MGroup, c: Uint8Array): MOut | undefined {
    return [...this.out.values()]
      .filter((o) => same(o.groupId, g.groupId))
      .sort((x, y) => (x.created !== y.created ? (x.created < y.created ? -1 : 1) : toHex(x.msgId) < toHex(y.msgId) ? -1 : 1))
      .find((o) => same(wire.commitment(o.msgId), c));
  }

  /** The row is the group of `kind` of channelId in communityId (a join never rebinds a row; groups.rs bound_to). */
  private boundTo(g: MGroup, communityId: Id | null, channelId: Id, kind: 0 | 1 = 0): boolean {
    return g.kind === kind && (g.communityId === null ? communityId === null : communityId !== null && same(g.communityId, communityId)) &&
      same(g.targetId, channelId);
  }

  /** Another non-gone group of `kind` that holds channelId (groups.rs holder, L-CORE-51: a DM's text group and its call
   *  group share the target and never collide). */
  private holder(channelId: Id, except: Id, kind: 0 | 1 = 0): MGroup | undefined {
    return [...this.gs.values()].find((x) => !same(x.groupId, except) && same(x.targetId, channelId) && x.state !== 4 && x.kind === kind);
  }

  /** The epoch floor of L-CORE-07: maxEpoch, and the epoch of the group state still stored (states 2 and 3). */
  private floorOf(g: MGroup): bigint {
    const stored = g.state === 2 || g.state === 3 ? g.epoch : 0n;
    return stored > g.maxEpoch ? stored : g.maxEpoch;
  }

  private raiseFloor(g: MGroup, epoch: bigint): void {
    if (epoch > g.maxEpoch) g.maxEpoch = epoch;
  }
}

// ---------------------------------------------------------------------------------------------
// ModelDs: the delivery-service routes of L-HTTP-31, 33..43 and the fan-out of ops 16, 17, 19, 20.
// ---------------------------------------------------------------------------------------------

export type RouteName = 'listChannels' | 'postGroup' | 'getGroupInfo' | 'getGroupTree' | 'getHandshakes' | 'getMessages'
  | 'getGroupHealState' | 'postGroupHeal'
  | 'getProposals' | 'postCommit' | 'postMessage' | 'postCursor' | 'postResync' | 'getWelcomes' | 'deleteWelcome'
  | 'getInteractionAdmissionContext'
  | 'getChannel' | 'postDm' | 'listDms' | 'putDeviceList' | 'getDeviceList' | 'getDeviceListHistory' | 'deleteGroupMessage'
  | 'getModerationRemovals'
  // web-3 calls (L-HTTP-100..104): the call routes of 09 § Voice as the instance answers them.
  | 'getChannelCall' | 'getCommunityCalls' | 'startCall' | 'shareCall' | 'unshareCall' | 'postCallStats' | 'postCallRtt' | 'postProposal' | 'declineCall'
  | 'endCallIfAlone'
  // v0.1.0-web3.1: the create flow (internal/api communities.go, channels.go, invites.go), so a suite creates its server
  // and its channels through the client path.
  | 'listCommunities' | 'createCommunity' | 'createChannel' | 'createInvite'
  // web3.4: the caller's own permissions and the roles list, as the instance resolves them for the model's servers.
  | 'getPermissions' | 'listRoles'
  // wave 2a (flow 09): roles and channel overwrites, ruled by the owner alone in the model.
  | 'listOverwrites' | 'createRole' | 'patchRole' | 'deleteRole' | 'grantRole' | 'revokeRole' | 'putOverwrite' | 'deleteOverwrite'
  // web3.7: the channel patch, so a slow mode set through the client path is served back in the list.
  | 'patchChannel';
export type ModelRoutes = Pick<Routes, RouteName>;
/** The @everyone base of a new server (internal/api everyoneRoleAllow): view, send, attach, react, history, connect, speak, video,
 *  screen share and create invite. Every defined permission bit is below 2^30. */
export const MODEL_EVERYONE_ALLOW = 0b1111_1111_0011n;
export const MODEL_PERM_ALL = (1n << 30n) - 1n;
/** fail: throw before acting; lose: act, then throw a network error; before/after: run fn around acting; gate: wait first. */
export type Injection = { fail: Error } | { lose: true } | { before: () => void } | { after: () => void } | { gate: Promise<void> };

export interface DsCall {
  dev: string;
  route: RouteName;
  group: string;
  from: bigint | null;
}

export interface DsView {
  epoch: bigint;
  members: string[];
  messages: { seq: bigint; epoch: bigint; uploader: string; body: string }[];
  commits: { seq: bigint; kind: number; committer: string }[];
}

interface DsEntry {
  seq: bigint;
  epoch: bigint;
  hs: { kind: number; sender: bigint | null } | null;
  uploader: Id | null;
  blob: Uint8Array | null;
  /** mls_app_messages.commitment_c: read from the upload, kept when the message is deleted; null for handshakes. */
  commitment: Uint8Array | null;
  deleted: boolean;
  franking: Uint8Array;
  recvTs: bigint;
}

interface DsProposal {
  ref: Id;
  blob: Uint8Array;
  epoch: bigint;
  void: boolean;
}

interface DsGroup {
  id: Id;
  /** mls_groups.kind: 0 text, 1 call. */
  kind: 0 | 1;
  /** A call group the instance closed (L-HTTP-105): L-HTTP-100 no longer names it and a start answers 403. */
  closed: boolean;
  channelId: Id;
  epoch: bigint;
  next: bigint;
  log: DsEntry[];
  members: Set<string>;
  proposals: DsProposal[];
  prunedBelow: bigint;
}

interface DsWelcome {
  id: bigint;
  dev: string;
  groupId: Id;
  epoch: bigint;
  commitSeq: bigint;
  blob: Uint8Array;
}

export class ModelDs {
  /**
   * Rows of the parity cases (L-CORE-10) at a given seq and epoch, named after the fixture's `what`
   * values (static, so they do not collide with the instance methods that append to a group's log).
   */
  static readonly row = {
    /** peer-message: a type-0 message by PEER, framed in `epoch`. */
    peerMessage: (seq: bigint, epoch: bigint): CborInput[] => [
      seq, epoch, PEER.device, wire.app(PEER, idOf(0x7e, Number(seq)), `parity ${String(seq)}`), new Uint8Array(32), new Uint8Array(32),
      1_700_000_000n + seq, 0,
    ],
    /**
     * peer-commit: a self-update commit by PEER (kind 1), framed in `epoch`. Stand-in convention
     * (CORE-ENGINE-02): element 1 is the pre-commit epoch here and in appendCommit, while dillad and the
     * Rust relay label a commit row with the post-commit epoch; the Rust core reads the epoch from the
     * handshake blob itself and the engine never reads the label, so no rule may key on it without
     * changing both.
     */
    peerCommit: (seq: bigint, epoch: bigint): CborInput[] => [seq, epoch, 1, 1n, wire.commit(epoch + 1n, PEER.device, [], [])],
    /** bad-commit: a kind-1 row whose blob no core can process. */
    badCommit: (seq: bigint, epoch: bigint): CborInput[] => [seq, epoch, 1, 1n, new Uint8Array([0xff])],
    /**
     * own-echo: a message row uploaded by ME, carrying the private message `blob` of ME's sendEncrypt
     * and, in element 4, the commitment the delivery service read from that upload.
     */
    ownEcho: (seq: bigint, epoch: bigint, blob: Uint8Array): CborInput[] => [
      seq, epoch, ME.device, blob, commitmentOf(blob), new Uint8Array(32), 1_700_000_000n + seq, 0,
    ],
    /** pruned: blob null, deleted 0. */
    pruned: (seq: bigint, epoch: bigint): CborInput[] => [seq, epoch, PEER.device, null, null, new Uint8Array(32), 1_700_000_000n + seq, 0],
    /** deleted: deleted 1. */
    deleted: (seq: bigint, epoch: bigint): CborInput[] => [seq, epoch, PEER.device, null, null, new Uint8Array(32), 1_700_000_000n + seq, 1],
    /** add-proposal: an Add of THIRD by the instance's external sender (kind 0, sender null). */
    addProposal: (seq: bigint, epoch: bigint): CborInput[] => [seq, epoch, 0, null, wire.prop(idOf(0x5e, Number(seq)), 'add', THIRD.device)],
    /** Any application row of `from` (commitment and franking 32 zero bytes, recv_ts 1_700_000_000 + seq, deleted 0). */
    app: (seq: bigint, epoch: bigint, from: Peer, msgId: Id, body: string, opts: WireOptions = {}): CborInput[] => [
      seq, epoch, from.device, wire.app(from, msgId, body, opts), new Uint8Array(32), new Uint8Array(32), 1_700_000_000n + seq, 0,
    ],
    /** The fold parity rows (L-CORE-39): PEER's target FOLD_TARGET, and PEER's folds with msg id idOf(0x7f, seq). */
    peerTarget: (seq: bigint, epoch: bigint): CborInput[] => ModelDs.row.app(seq, epoch, PEER, FOLD_TARGET, 'the target'),
    peerEdit: (seq: bigint, epoch: bigint, target: Id): CborInput[] =>
      ModelDs.row.app(seq, epoch, PEER, idOf(0x7f, Number(seq)), 'edited by the peer', { type: 1, replyTo: target }),
    peerReact: (seq: bigint, epoch: bigint, target: Id): CborInput[] =>
      ModelDs.row.app(seq, epoch, PEER, idOf(0x7f, Number(seq)), '👍', { type: 3, replyTo: target }),
    peerUnreact: (seq: bigint, epoch: bigint, target: Id): CborInput[] =>
      ModelDs.row.app(seq, epoch, PEER, idOf(0x7f, Number(seq)), '👍', { type: 4, replyTo: target }),
    peerDelete: (seq: bigint, epoch: bigint, target: Id): CborInput[] =>
      ModelDs.row.app(seq, epoch, PEER, idOf(0x7f, Number(seq)), '', { type: 2, replyTo: target }),
    peerPin: (seq: bigint, epoch: bigint, target: Id): CborInput[] =>
      ModelDs.row.app(seq, epoch, PEER, idOf(0x7f, Number(seq)), '', { type: 5, replyTo: target }),
    peerReactUnheld: (seq: bigint, epoch: bigint): CborInput[] =>
      ModelDs.row.app(seq, epoch, PEER, idOf(0x7f, Number(seq)), '👍', { type: 3, replyTo: FOLD_UNHELD }),
  };

  readonly calls: DsCall[] = [];
  readonly healNeeded = new Map<string, boolean>();
  /** `${device hex}/${group hex}` -> the last acknowledged [last_seq, last_epoch]. */
  readonly cursors = new Map<string, readonly [bigint, bigint]>();
  /** Device hexes whose external commits are refused with 403 E_FORBIDDEN. */
  readonly denyJoin = new Set<string>();
  /** Device hexes that are the target of an outstanding instance Remove: their resync is refused with 409
   *  E_REMOVE_PENDING (R25's guard 1) until a member commits it. */
  readonly removeTarget = new Set<string>();
  /**
   * Device hexes the community's ACL does not admit (a kicked user's devices until the user joins
   * again): getGroupInfo, getGroupTree and postResync answer 404 E_NOT_FOUND, as the delivery
   * service's requireReader does (internal/ds/registry.go).
   */
  readonly aclDeny = new Set<string>();
  /** `${group hex}/${device hex}` of leaves a kick took: getProposals answers 404 E_NOT_FOUND too (requireMember). */
  private readonly removed = new Set<string>();
  private readonly groups = new Map<string, DsGroup>();
  /** A created channel carries its kind, name and position; a seeded one (addChannel) is listed as `channel-<i>`. */
  private readonly channels = new Map<string, { communityId: Id; channelId: Id; created?: { kind: 0 | 1; name: string; position: number }; slowmode?: number; topic?: string }>();
  /** Created servers: community hex → its name, its owner's and its members' user hexes. */
  private readonly communities = new Map<string, { id: Id; name: string; owner: string; members: Set<string> }>();
  private readonly owners = new Map<string, string>();
  /** Wave 2a: the roles (besides @everyone) and channel overwrites of each created server, by community hex. */
  private readonly roleRows = new Map<string, RoleRow[]>();
  private readonly overwriteRows = new Map<string, OverwriteRow[]>();
  private readonly devices = new Map<string, Id>();
  private readonly users = new Map<string, Id>();
  private readonly keyPackageDevs = new Set<string>();
  private readonly lists = new Map<string, { version: bigint; blob: Uint8Array; sig: Uint8Array; prev: Uint8Array }[]>();
  private readonly revokedDevs = new Set<string>();
  private readonly dms = new Map<string, { channelId: Id; users: Id[] }>();
  private readonly welcomes: DsWelcome[] = [];
  private readonly sinks = new Map<string, (f: Frame) => void>();
  private readonly parked = new Map<string, (f: Frame) => void>();
  private readonly drops = new Map<string, number>();
  private readonly injections = new Map<RouteName, Injection[]>();
  private readonly messageHooks = new Map<string, () => void>();
  private readonly proposalHooks = new Map<string, () => void>();
  private counter = 0;
  private frameN = 0n;
  private welcomeIds = 0n;
  private clockS = 1_700_000_000n;

  // --- test controls ---

  addChannel(communityId: Id, channelId: Id): void {
    this.channels.set(toHex(channelId), { communityId, channelId });
  }
  /** `user` joins a created server (POST /v1/communities/{id}/join as the instance admits it). */
  addMember(communityId: Id, user: Id): void {
    this.communities.get(toHex(communityId))?.members.add(toHex(user));
  }
  own(user: Id, device: Id): void {
    this.owners.set(toHex(device), toHex(user));
    this.devices.set(toHex(device), device);
    this.users.set(toHex(user), user);
    this.keyPackageDevs.add(toHex(device));
  }
  publishList(user: Id, entries: readonly { device: Id; revokedAt?: bigint | null }[]): bigint {
    const key = toHex(user);
    const version = BigInt((this.lists.get(key)?.length ?? 0) + 1);
    const blob = fakeListBlob(user, entries.map((entry) => ({ deviceId: entry.device, revokedAt: entry.revokedAt ?? null })), this.clockS);
    // The ModelCore's stand-in signature and hash markers, so a published list is one the double's ingest accepts.
    this.storeList(key, version, blob, new Uint8Array(64).fill(Number(version & 255n)), new Uint8Array(32).fill(Number((version - 1n) & 255n)));
    return version;
  }
  addDm(channelId: Id, participants: readonly Peer[]): void {
    participants.forEach((p) => this.own(p.user, p.device));
    const users = [...new Map(participants.map((p) => [toHex(p.user), p.user])).values()]
      .sort((a, b) => toHex(a).localeCompare(toHex(b)));
    this.dms.set(toHex(channelId), { channelId, users });
  }
  private unlisted(device: string): boolean {
    const owner = this.owners.get(device);
    if (owner === undefined) return false;
    const rows = this.lists.get(owner);
    if (rows === undefined || rows.length === 0) return false;
    const list = readFakeList(at(rows, rows.length - 1).blob);
    const id = this.devices.get(device);
    // Invariant 4 judges the (device_id, dsk_pub) pair; every ModelDs device holds its default key.
    return list === null || id === undefined || !fakeListNames(list, id, fakeDskPub(id));
  }
  private storeList(user: string, version: bigint, blob: Uint8Array, sig: Uint8Array, prev: Uint8Array): void {
    const rows = this.lists.get(user) ?? [];
    rows.push({ version, blob, sig, prev });
    this.lists.set(user, rows);
    const list = readFakeList(blob);
    if (list === null) return;
    for (const entry of list.entries) {
      const key = toHex(entry.deviceId);
      if (entry.revokedAt !== null && this.owners.get(key) === user && !this.revokedDevs.has(key)) {
        this.revokedDevs.add(key);
        for (const g of this.groups.values()) if (g.members.has(key)) this.propose(g.id, 'remove', entry.deviceId);
      }
    }
  }

  attach(dev: Id, sink: (f: Frame) => void): void {
    this.sinks.set(toHex(dev), sink);
  }

  /** Stops fan-out to dev (frames are lost, as for a dropped connection) until reattach. */
  detach(dev: Id): void {
    const k = toHex(dev);
    const s = this.sinks.get(k);
    if (s !== undefined) {
      this.parked.set(k, s);
      this.sinks.delete(k);
    }
  }

  reattach(dev: Id): void {
    const k = toHex(dev);
    const s = this.parked.get(k);
    if (s !== undefined) {
      this.sinks.set(k, s);
      this.parked.delete(k);
    }
  }

  /** Loses the next count frames addressed to dev. */
  dropNext(dev: Id, count: number): void {
    this.drops.set(toHex(dev), count);
  }

  /** Queues a fault for the next call of route (injections of one route are used in order). */
  inject(route: RouteName, injection: Injection): void {
    const list = this.injections.get(route) ?? [];
    list.push(injection);
    this.injections.set(route, list);
  }

  /**
   * One-shot: runs fn right after the next getMessages for groupId has produced its answer and
   * before any further request is served (a row stored between the two reads of a catch-up round).
   */
  afterNextMessagesRead(groupId: Id, fn: () => void): void {
    this.messageHooks.set(toHex(groupId), fn);
  }

  /** One-shot race: remove a leaf after its proposal read and before its commit upload. */
  afterNextProposalsRead(groupId: Id, fn: () => void): void {
    this.proposalHooks.set(toHex(groupId), fn);
  }

  resetCalls(): void {
    this.calls.length = 0;
  }

  callsOf(dev: Id, route: RouteName): DsCall[] {
    const d = toHex(dev);
    return this.calls.filter((c) => c.dev === d && c.route === route);
  }

  welcomesFor(dev: Id): { id: bigint; groupId: string }[] {
    const d = toHex(dev);
    return this.welcomes.filter((w) => w.dev === d).map((w) => ({ id: w.id, groupId: toHex(w.groupId) }));
  }

  view(groupId: Id): DsView {
    const g = this.must(groupId);
    const messages: DsView['messages'] = [];
    const commits: DsView['commits'] = [];
    for (const e of g.log) {
      if (e.hs === null) {
        if (e.blob === null || e.uploader === null) continue;
        messages.push({ seq: e.seq, epoch: e.epoch, uploader: toHex(e.uploader), body: str(at(arr(decode(e.blob), 8), 4)) });
      } else if (e.hs.kind !== 0) {
        let committer = '';
        try {
          committer = e.blob === null ? '' : toHex(bin(at(arr(decode(e.blob), 5), 2), 16));
        } catch {
          committer = '';
        }
        commits.push({ seq: e.seq, kind: e.hs.kind, committer });
      }
    }
    return { epoch: g.epoch, members: [...g.members].sort(), messages, commits };
  }

  // --- peers and the instance ---

  peerCreate(peer: Peer, channelId: Id): Id {
    this.counter += 1;
    const id = idOf(0x9a, this.counter);
    this.groups.set(toHex(id), this.newGroup(id, channelId, toHex(peer.device)));
    return id;
  }

  // --- web-3 calls (09 § Voice, L-HTTP-100..105, L-GW-32, L-GW-33) ---

  /** livekit.max_voice_participants as the start's E_CALL_FULL reads it. */
  maxVoiceParticipants = 25;
  /** WB product walk I6: users (hex) a DM start does not ring, as the instance's pair rule would hold them back. */
  readonly ringHeld = new Set<string>();
  /** channel hex → its live call: a call id is the channel id (internal/ds/state.go callIDOf). */
  private readonly liveCalls = new Map<string, { callId: Id; channelId: Id; groupId: Id; participants: Map<string, { user: Id; device: Id; flags: number }> }>();
  /** Voice channels by channel hex → their community (the start and the snapshot need a channel the caller may view). */
  private readonly voiceChannels = new Map<string, Id>();

  addVoiceChannel(communityId: Id, channelId: Id): void {
    this.voiceChannels.set(toHex(channelId), communityId);
  }
  /** A call group peer registered for the channel (kind 1), with peer's device its only member. */
  peerCreateCall(peer: Peer, channelId: Id): Id {
    this.counter += 1;
    const id = idOf(0x9c, this.counter);
    this.groups.set(toHex(id), this.newGroup(id, channelId, toHex(peer.device), 1));
    return id;
  }
  /** L-HTTP-105: the idle close; L-HTTP-100 no longer names the group and a start on it answers 403 E_LEAF_NOT_CURRENT. */
  closeCallGroup(groupId: Id): void {
    this.must(groupId).closed = true;
    for (const [key, call] of this.liveCalls) if (same(call.groupId, groupId)) this.liveCalls.delete(key);
  }
  /** A device's voice_state as the SFU reports it (flags 0: it left), fanned out as op 50 to every attached device. */
  voice(channelId: Id, peer: Peer, flags: number): void {
    const call = this.liveCalls.get(toHex(channelId));
    if (call === undefined) return;
    if (flags === 0) call.participants.delete(toHex(peer.device));
    else call.participants.set(toHex(peer.device), { user: peer.user, device: peer.device, flags });
    for (const dev of this.sinks.keys()) this.emitEphemeral(dev, 50, [peer.user, peer.device, call.callId, BigInt(flags)]);
  }
  /** One op 53 call_ring to `to`'s attached device (L-GW-33): the instance alone sends it. */
  ring(to: Id, callId: Id, channelId: Id, user: Id, state: 0 | 1 | 2, expiresTs: number): void {
    this.emitEphemeral(toHex(to), 53, [callId, channelId, user, BigInt(state), BigInt(expiresTs)]);
  }
  /** The live call of the channel ends (every ring stops; the call group closes as endCall does). */
  endCall(channelId: Id): void {
    const call = this.liveCalls.get(toHex(channelId));
    if (call === undefined) return;
    this.liveCalls.delete(toHex(channelId));
    this.closeCallGroup(call.groupId);
  }
  /** The newest row of `user`'s signed list as served (a test seeds a device's cache with it). */
  listRow(user: Id): { version: bigint; blob: Uint8Array } | undefined {
    const rows = this.lists.get(toHex(user));
    const last = rows?.[rows.length - 1];
    return last === undefined ? undefined : { version: last.version, blob: last.blob };
  }

  private mayView(d: string, channelId: Id): boolean {
    const dm = this.dms.get(toHex(channelId));
    if (dm !== undefined) return dm.users.some((user) => toHex(user) === this.owners.get(d));
    return this.voiceChannels.has(toHex(channelId)) || this.channels.has(toHex(channelId));
  }
  private snapshot(channelId: Id): { groupId: Id | null; callId: Id | null; participants: { userId: Id; deviceId: Id; flags: number }[] } {
    const call = this.liveCalls.get(toHex(channelId));
    const participants = call === undefined ? [] : [...call.participants.values()]
      .sort((a, b) => toHex(a.user).localeCompare(toHex(b.user)) || toHex(a.device).localeCompare(toHex(b.device)))
      .map((p) => ({ userId: p.user, deviceId: p.device, flags: p.flags }));
    return { groupId: call?.groupId ?? this.openGroupOf(channelId, 1)?.id ?? null, callId: call?.callId ?? null, participants };
  }
  private startCall(d: string, channelId: Id): { created: boolean; callId: Id; groupId: Id; livekitUrl: string; token: string;
    iceServers: [string[], string, string][]; caps: [number, number, number]; rung: Id[] | null } {
    if (!this.mayView(d, channelId)) throw httpError(404, 'E_NOT_FOUND');
    const live = this.liveCalls.get(toHex(channelId));
    const g = live === undefined ? this.openGroupOf(channelId, 1) : this.groups.get(toHex(live.groupId));
    // The leaf gate: a token only for a current leaf of the call group's current epoch.
    if (g === undefined || g.closed || !g.members.has(d)) throw httpError(403, 'E_LEAF_NOT_CURRENT');
    if (live !== undefined && live.participants.size >= this.maxVoiceParticipants) throw httpError(409, 'E_CALL_FULL');
    if (live === undefined) this.liveCalls.set(toHex(channelId), { callId: channelId, channelId, groupId: g.id, participants: new Map() });
    // A start that opens a DM call answers whom it rang (09 § Voice, WB product walk I6).
    const dm = this.dms.get(toHex(channelId));
    const rung = live === undefined && dm !== undefined
      ? dm.users.filter((u) => toHex(u) !== this.owners.get(d) && !this.ringHeld.has(toHex(u))) : null;
    return { created: live === undefined, callId: channelId, groupId: g.id, livekitUrl: 'wss://model.test/rtc', token: `token-${d.slice(0, 8)}`,
      iceServers: [[['turn:model.test:3478?transport=udp'], 'user', 'credential']], caps: [64_000, 2_500_000, 1], rung };
  }
  private liveCall(callId: Id): { callId: Id; channelId: Id; groupId: Id } {
    const call = [...this.liveCalls.values()].find((c) => same(c.callId, callId));
    if (call === undefined) throw httpError(404, 'E_NOT_FOUND');
    return call;
  }
  /** POST /v1/groups/{id}/proposal by a member (L-HTTP-103): stored and fanned out as a handshake; a second Remove of the
   *  same leaf is 409 E_REMOVE_PENDING. */
  private postProposal(d: string, id: Id, body: Uint8Array): { seq: bigint } {
    const g = this.must(id);
    if (!g.members.has(d)) throw httpError(403, 'E_LEAF_NOT_CURRENT');
    const v = arr(decode(body), 2);
    if (u64(at(v, 0)) !== g.epoch) throw httpError(409, 'E_COMMIT_CONFLICT');
    const blob = bin(at(v, 1));
    const p = arr(decode(blob), 4);
    const removes = (x: DsProposal): boolean => {
      const q = arr(decode(x.blob), 4);
      return x.epoch === g.epoch && !x.void && str(at(q, 2)) === 'remove' && same(bin(at(q, 3), 16), bin(at(p, 3), 16));
    };
    if (str(at(p, 2)) === 'remove' && g.proposals.some(removes)) throw httpError(409, 'E_REMOVE_PENDING');
    g.proposals.push({ ref: bin(at(p, 1), 16), blob, epoch: g.epoch, void: false });
    const e: DsEntry = { seq: g.next, epoch: g.epoch, hs: { kind: 0, sender: 0n }, uploader: null, blob, commitment: null, deleted: false,
      franking: new Uint8Array(32), recvTs: this.tick() };
    g.next += 1n;
    g.log.push(e);
    for (const m of [...g.members]) this.emit(m, 16, g.id, [e.seq, e.epoch, 0n, 0n, blob]);
    return { seq: e.seq };
  }
  private emitEphemeral(dev: string, op: number, payload: CborValue[]): void {
    const sink = this.sinks.get(dev);
    if (sink !== undefined) sink({ op, n: 0n, groupId: null, payload });
  }

  /** An external commit (kind 2, sender null) by peer that adds it, framed at the current epoch. */
  peerJoin(groupId: Id, peer: Peer): bigint {
    const g = this.must(groupId);
    return this.appendCommit(g, 2, null, wire.commit(g.epoch + 1n, peer.device, [peer.device], []), [peer.device], []);
  }

  /** Model shortcut: makes dev a member without a commit (the core models no roster). */
  join(groupId: Id, dev: Id): void {
    this.must(groupId).members.add(toHex(dev));
  }

  /** Model shortcut: takes dev's leaf away without a commit (its next upload meets 403). */
  evict(groupId: Id, dev: Id): void {
    this.must(groupId).members.delete(toHex(dev));
  }

  /**
   * The kick (internal/api/communities.go removeMember): the member row goes, so the ACL refuses the
   * device, and its leaf goes, so every requireMember read (messages, handshakes, proposals) answers
   * 404 E_NOT_FOUND. No frame tells the device: the gateway's roster no longer holds it.
   */
  kick(groupId: Id, dev: Id): void {
    this.must(groupId).members.delete(toHex(dev));
    this.removed.add(`${toHex(groupId)}/${toHex(dev)}`);
    this.aclDeny.add(toHex(dev));
  }

  /** The kicked user joins the community again: the ACL admits the device; its leaf stays gone. */
  readmit(dev: Id): void {
    this.aclDeny.delete(toHex(dev));
  }

  peerSend(groupId: Id, from: Peer, body: string): bigint {
    this.counter += 1;
    return this.appendMessage(this.must(groupId), from.device, wire.app(from, idOf(0x7e, this.counter), body)).seq;
  }

  /** An instance proposal (sender null) for the current epoch. */
  propose(groupId: Id, op: 'add' | 'remove', device: Id): bigint {
    const g = this.must(groupId);
    this.counter += 1;
    const ref = idOf(0x5e, this.counter);
    const blob = wire.prop(ref, op, device);
    g.proposals.push({ ref, blob, epoch: g.epoch, void: false });
    const e: DsEntry = {
      seq: g.next, epoch: g.epoch, hs: { kind: 0, sender: null }, uploader: null, blob, commitment: null, deleted: false,
      franking: new Uint8Array(32), recvTs: this.tick(),
    };
    g.next += 1n;
    g.log.push(e);
    for (const m of [...g.members]) this.emit(m, 16, g.id, [e.seq, e.epoch, 0n, null, blob]);
    return e.seq;
  }

  /** A member commit covering every outstanding proposal plus adds; Welcomes go to every added device. */
  peerCommit(groupId: Id, committer: Peer, adds: readonly Id[] = []): bigint {
    const g = this.must(groupId);
    const allAdds: Id[] = [...adds];
    const removes: Id[] = [];
    for (const p of g.proposals) {
      if (p.epoch !== g.epoch || p.void) continue;
      const v = arr(decode(p.blob), 4);
      (str(at(v, 2)) === 'add' ? allAdds : removes).push(bin(at(v, 3), 16));
    }
    const blob = wire.commit(g.epoch + 1n, committer.device, allAdds, removes);
    const seq = this.appendCommit(g, 1, 0n, blob, allAdds, removes);
    for (const a of allAdds) this.addWelcome(toHex(a), g, seq, wire.welcome(g.id, g.epoch));
    return seq;
  }

  /** A commit no core can process (its blob is not CBOR). */
  garbageCommit(groupId: Id): bigint {
    return this.appendCommit(this.must(groupId), 1, 0n, new Uint8Array([0xff]), [], []);
  }

  /**
   * Deletes the stored message at seq as the delivery service does: the blob goes, seq, epoch, uploader,
   * commitment, franking tag and recv_ts stay, later pages serve it with deleted = 1, and op 21 fans out.
   */
  deleteMessage(groupId: Id, seq: bigint): void {
    const g = this.must(groupId);
    const e = g.log.find((x) => x.seq === seq && x.hs === null);
    if (e === undefined || e.deleted) return;
    e.blob = null;
    e.deleted = true;
    const deletedAt = this.tick();
    for (const m of [...g.members]) this.emit(m, 21, g.id, [seq, deletedAt]);
  }

  /**
   * DELETE /v1/groups/{id}/messages/{seq} as `deviceId` (L-HTTP-80): 404 for an unknown group, a device that is not a
   * member (checked first) or no message at seq; 403 when the uploader's user is not the caller's; else the blob goes,
   * the row is tombstoned and op 21 fans out to every member, also for a row already deleted (dillad's no-op update,
   * internal/ds/message.go:179-193). Attacker statement: the 404 hits only a device no longer in the group.
   */
  deleteAs(groupId: Id, deviceId: Id, seq: bigint): 204 | 403 | 404 {
    const g = this.groups.get(toHex(groupId));
    if (!g || !g.members.has(toHex(deviceId))) return 404;
    const row = g.log.find((x) => x.seq === seq && x.hs === null);
    if (!row || !row.uploader) return 404;
    const uploader = toHex(row.uploader);
    const caller = toHex(deviceId);
    if ((this.owners.get(uploader) ?? uploader) !== (this.owners.get(caller) ?? caller)) return 403;
    row.blob = null;
    row.deleted = true;
    const deletedAt = this.tick();
    for (const member of g.members) this.emit(member, 21, g.id, [seq, deletedAt]);
    return 204;
  }

  /** Sends mls.commit_needed to dev. */
  elect(groupId: Id, dev: Id, round: bigint): void {
    const g = this.must(groupId);
    const refs = g.proposals.filter((p) => p.epoch === g.epoch && !p.void).map((p) => p.ref);
    this.emit(toHex(dev), 17, g.id, [g.epoch, refs, 2000n, round]);
  }

  prune(groupId: Id, below: bigint): void {
    this.must(groupId).prunedBelow = below;
  }

  // --- routes ---

  routesFor(dev: Id): ModelRoutes {
    const d = toHex(dev);
    return {
      listChannels: (communityId) => this.call(d, 'listChannels', null, null, () => this.channelRows(communityId)),
      listCommunities: () => this.call(d, 'listCommunities', null, null, () => [...this.communities.values()]
        .filter((c) => c.members.has(this.owners.get(d) ?? ''))
        .map((c) => ({ id: c.id, name: c.name, owner: this.users.get(c.owner)!, policyVersion: 1n }))),
      createCommunity: (name) => this.call(d, 'createCommunity', null, null, () => this.createCommunity(d, name)),
      // Slot 6 of the patch body is the slow mode; the owner alone rules it, as every other channel change in the model.
      patchChannel: (channelId, patch) => this.call(d, 'patchChannel', channelId, null, () => {
        this.ownerOfChannel(d, channelId);
        const known = this.channels.get(toHex(channelId))!;
        if (patch.slowmode !== undefined) known.slowmode = patch.slowmode;
        if (patch.topic !== undefined) known.topic = patch.topic;
      }),
      createChannel: (communityId, channel) => this.call(d, 'createChannel', null, null, () => this.createChannel(d, communityId, channel)),
      createInvite: (communityId) => this.call(d, 'createInvite', null, null, () => {
        this.memberOf(d, communityId);
        this.counter += 1;
        const code = `MODEL${String(this.counter)}`;
        return { inviteId: idOf(0x1e, this.counter), code, url: `https://model.test/i/${code}`, expires: this.clockS + 604_800n };
      }),
      // The owner holds every permission at the top rank; any other member holds the @everyone base at rank 0.
      getPermissions: (communityId) => this.call(d, 'getPermissions', null, null, () => {
        const c = this.memberOf(d, communityId);
        return c.owner === this.owners.get(d) ? { bits: MODEL_PERM_ALL, top: OWNER_TOP } : { bits: MODEL_EVERYONE_ALLOW, top: 0 };
      }),
      listRoles: (communityId) => this.call(d, 'listRoles', null, null, () => {
        this.memberOf(d, communityId);
        const everyone = communityId.slice();
        everyone[0] = 0xc7;
        return [{ id: everyone, name: '@everyone', color: 0, position: 0, allow: Number(MODEL_EVERYONE_ALLOW), deny: 0, hoist: false, mentionable: false },
          ...(this.roleRows.get(toHex(communityId)) ?? [])];
      }),
      listOverwrites: (communityId) => this.call(d, 'listOverwrites', null, null, () => {
        this.memberOf(d, communityId);
        return this.overwriteRows.get(toHex(communityId)) ?? [];
      }),
      createRole: (communityId, role) => this.call(d, 'createRole', null, null, () => {
        this.ownerOf(d, communityId);
        this.counter += 1;
        const roleId = idOf(0xc8, this.counter);
        const rows = this.roleRows.get(toHex(communityId)) ?? [];
        rows.push({ id: roleId, name: role.name, color: role.color, position: role.position, allow: role.allow, deny: role.deny,
          hoist: role.hoist, mentionable: role.mentionable });
        this.roleRows.set(toHex(communityId), rows);
        return { roleId };
      }),
      patchRole: (roleId, patch) => this.call(d, 'patchRole', null, null, () => {
        const row = this.roleOf(d, roleId);
        Object.assign(row, Object.fromEntries(Object.entries(patch).filter(([, v]) => v !== undefined)));
      }),
      deleteRole: (roleId) => this.call(d, 'deleteRole', null, null, () => {
        const row = this.roleOf(d, roleId);
        for (const [c, rows] of this.roleRows) this.roleRows.set(c, rows.filter((r) => r !== row));
      }),
      grantRole: (communityId) => this.call(d, 'grantRole', null, null, () => { this.ownerOf(d, communityId); }),
      revokeRole: (communityId) => this.call(d, 'revokeRole', null, null, () => { this.ownerOf(d, communityId); }),
      putOverwrite: (channelId, kind, targetId, bits) => this.call(d, 'putOverwrite', null, null, () => {
        const communityId = this.ownerOfChannel(d, channelId);
        const rows = (this.overwriteRows.get(toHex(communityId)) ?? []).filter((o) => !sameOverwrite(o, channelId, kind, targetId));
        rows.push({ channelId, kind, targetId, allow: bits.allow, deny: bits.deny });
        this.overwriteRows.set(toHex(communityId), rows);
      }),
      deleteOverwrite: (channelId, kind, targetId) => this.call(d, 'deleteOverwrite', null, null, () => {
        const communityId = this.ownerOfChannel(d, channelId);
        const rows = this.overwriteRows.get(toHex(communityId)) ?? [];
        if (!rows.some((o) => sameOverwrite(o, channelId, kind, targetId))) throw httpError(404, 'E_NOT_FOUND');
        this.overwriteRows.set(toHex(communityId), rows.filter((o) => !sameOverwrite(o, channelId, kind, targetId)));
      }),
      postGroup: (body) => this.call(d, 'postGroup', null, null, () => this.postGroup(d, body)),
      getGroupInfo: (id) =>
        this.call(d, 'getGroupInfo', id, null, () => {
          const g = this.must(id);
          if (this.aclDeny.has(d)) throw httpError(404, 'E_NOT_FOUND');
          return encode([g.epoch, wire.info(g.id, g.epoch), new Uint8Array(32), g.next]);
        }),
      getGroupTree: (id) =>
        this.call(d, 'getGroupTree', id, null, () => {
          const g = this.must(id);
          if (this.aclDeny.has(d)) throw httpError(404, 'E_NOT_FOUND');
          return encode([g.epoch, wire.tree(g.id, g.epoch), new Uint8Array(32)]);
        }),
      getGroupHealState: (id) => this.call(d, 'getGroupHealState', id, null, () => {
        const g = this.must(id);
        if (!g.members.has(d) || this.aclDeny.has(d)) throw httpError(404, 'E_NOT_FOUND');
        return { epoch: g.epoch, nextSeq: g.next, generation: 1n, needFromSeq: g.next, needsHeal: this.healNeeded.get(toHex(id)) ?? false };
      }),
      postGroupHeal: (id, snapshot) => this.call(d, 'postGroupHeal', id, null, () => {
        const g = this.must(id);
        if (!g.members.has(d)) throw httpError(404, 'E_NOT_FOUND');
        const a = arr(decode(snapshot), 3);
        if (!(a[0] instanceof Uint8Array) || !Array.isArray(a[1]) || !(a[2] instanceof Uint8Array)) throw httpError(400, 'E_INVALID_REQUEST');
        this.healNeeded.set(toHex(id), false);
        return { epoch: g.epoch, nextSeq: g.next };
      }),
      getHandshakes: (id, from, limit) => this.call(d, 'getHandshakes', id, from, () => this.page(d, id, from, limit, true)),
      getMessages: (id, from, limit) =>
        this.call(d, 'getMessages', id, from, () => {
          const answer = this.page(d, id, from, limit, false);
          const hook = this.messageHooks.get(toHex(id));
          if (hook !== undefined) {
            this.messageHooks.delete(toHex(id));
            hook();
          }
          return answer;
        }),
      getModerationRemovals: (id, after) => this.call(d, 'getModerationRemovals', id, after, () => ({ rows: [], nextRevision: after })),
      getProposals: (id) =>
        this.call(d, 'getProposals', id, null, () => {
          const g = this.must(id);
          if (!g.members.has(d)) throw httpError(404, 'E_NOT_FOUND');
          const answer = encode(g.proposals.filter((p) => p.epoch === g.epoch).map((p) => [p.ref, 0, null, p.blob, p.void ? 1 : 0]));
          const hook = this.proposalHooks.get(toHex(id));
          if (hook !== undefined) {
            this.proposalHooks.delete(toHex(id));
            hook();
          }
          return answer;
        }),
      postCommit: (id, body) => this.call(d, 'postCommit', id, null, () => this.postCommit(d, id, body)),
      postMessage: (id, body) => this.call(d, 'postMessage', id, null, () => this.postMessage(dev, id, body)),
      deleteGroupMessage: (id, seq) => this.call(d, 'deleteGroupMessage', id, null, () => {
        const status = this.deleteAs(id, dev, seq);
        if (status === 403) throw httpError(403, 'E_NOT_UPLOADER');
        return status === 204 ? 'deleted' : 'gone';
      }),
      postCursor: (id, body) =>
        this.call(d, 'postCursor', id, null, () => {
          const v = arr(decode(body), 2);
          this.cursors.set(`${d}/${toHex(id)}`, [u64(at(v, 0)), u64(at(v, 1))]);
        }),
      postResync: (id, body) => this.call(d, 'postResync', id, null, () => this.postResync(dev, id, body)),
      getWelcomes: (after) => this.call(d, 'getWelcomes', null, after, () => this.welcomePage(d, after)),
      getInteractionAdmissionContext: (groupId) => this.call(d, 'getInteractionAdmissionContext', groupId, null, () => {
        throw httpError(403, 'E_FORBIDDEN');
      }),
      deleteWelcome: (welcomeId) =>
        this.call(d, 'deleteWelcome', null, null, () => {
          const i = this.welcomes.findIndex((w) => w.dev === d && w.id === welcomeId);
          if (i >= 0) this.welcomes.splice(i, 1);
        }),
      getChannel: (id) => this.call(d, 'getChannel', id, null, () => {
        const dm = this.dms.get(toHex(id));
        if (dm !== undefined) {
          if (!dm.users.some((user) => toHex(user) === this.owners.get(d))) throw httpError(404, 'E_NOT_FOUND');
          return { id, kind: dm.users.length === 2 ? 3 : 4, mode: 0, visibility: 0, parentId: null,
            name: '', topic: '', position: 0, slowmode: 0, seq: 0n, textGroupId: this.openGroupOf(id)?.id ?? null };
        }
        const channel = this.channels.get(toHex(id));
        if (channel === undefined) throw httpError(404, 'E_NOT_FOUND');
        return this.channelRows(channel.communityId).find((row) => same(row.id, id))!;
      }),
      postDm: (recipients) => this.call(d, 'postDm', null, null, () => {
        const owner = this.owners.get(d);
        if (owner === undefined) throw httpError(400, 'E_INVALID_REQUEST');
        const users = [...new Set([owner, ...recipients.map(toHex)])].sort();
        if (users.length < 2) throw httpError(400, 'E_INVALID_REQUEST');
        for (const dm of this.dms.values()) if (dm.users.map(toHex).join('/') === users.join('/'))
          return { channelId: dm.channelId, created: false };
        if (users.some((user) => !this.users.has(user))) throw httpError(404, 'E_NOT_FOUND');
        const channelId = idOf(0xd3, this.dms.size + 1);
        this.dms.set(toHex(channelId), { channelId, users: users.map((user) => this.users.get(user)!) });
        return { channelId, created: true };
      }),
      listDms: () => this.call(d, 'listDms', null, null, () => [...this.dms.values()]
        .filter((dm) => dm.users.some((user) => toHex(user) === this.owners.get(d)))
        .map((dm) => ({ channelId: dm.channelId, kind: dm.users.length === 2 ? 3 as const : 4 as const, members: dm.users }))),
      putDeviceList: (userId, body) => this.call(d, 'putDeviceList', null, null, () => {
        const user = toHex(userId);
        if (this.owners.get(d) !== user) throw httpError(403, 'E_FORBIDDEN');
        const a = arr(decode(body), 4);
        const version = u64(at(a, 0));
        if (version !== BigInt((this.lists.get(user)?.length ?? 0) + 1)) throw httpError(409, 'E_INVALID_REQUEST');
        this.storeList(user, version, bin(at(a, 1)), bin(at(a, 2)), bin(at(a, 3)));
      }),
      getDeviceList: (userId) => this.call(d, 'getDeviceList', null, null, () => {
        const rows = this.lists.get(toHex(userId));
        const last = rows?.[rows.length - 1];
        return last === undefined ? null : { version: last.version, blob: last.blob,
          raw: encode([last.version, last.blob, last.sig, last.prev]) };
      }),
      getDeviceListHistory: (userId, after) => this.call(d, 'getDeviceListHistory', null, after, () => {
        const rows = (this.lists.get(toHex(userId)) ?? []).filter((row) => row.version > after).slice(0, 64);
        return { raw: encode(rows.map((row) => [row.version, row.blob, row.sig, row.prev])),
          count: rows.length, lastVersion: rows.length ? at(rows, rows.length - 1).version : null };
      }),
      getChannelCall: (channelId) => this.call(d, 'getChannelCall', null, null, () => {
        if (!this.mayView(d, channelId)) throw httpError(404, 'E_NOT_FOUND');
        return this.snapshot(channelId);
      }),
      getCommunityCalls: (communityId) => this.call(d, 'getCommunityCalls', null, null, () => [...this.voiceChannels]
        .filter(([channel, community]) => same(community, communityId) && this.liveCalls.has(channel))
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([channel]) => { const s = this.snapshot(this.liveCalls.get(channel)!.channelId);
          return { channelId: this.liveCalls.get(channel)!.channelId, callId: s.callId!, participants: s.participants }; })),
      startCall: (channelId) => this.call(d, 'startCall', null, null, () => this.startCall(d, channelId)),
      shareCall: (callId) => this.call(d, 'shareCall', null, null, () => { this.liveCall(callId); }),
      unshareCall: () => this.call(d, 'unshareCall', null, null, () => undefined),
      // L-HTTP-102's conditional DELETE (?if_alone=1): a current leaf of the call's open group ends it only while it is the
      // group's only member (409 E_CALL_NOT_ALONE otherwise, false); true once it has ended.
      endCallIfAlone: (callId) => this.call(d, 'endCallIfAlone', null, null, () => {
        const call = [...this.liveCalls.values()].find((c) => same(c.callId, callId));
        if (call === undefined) return true;
        const g = this.groups.get(toHex(call.groupId));
        if (g !== undefined && !g.closed && !g.members.has(d)) throw httpError(403, 'E_LEAF_NOT_CURRENT');
        if (g !== undefined && !g.closed && g.members.size !== 1) return false;
        this.endCall(call.channelId);
        return true;
      }),
      postCallStats: (callId) => this.call(d, 'postCallStats', null, null, () => { this.liveCall(callId); }),
      postCallRtt: (callId) => this.call(d, 'postCallRtt', null, null, () => { this.liveCall(callId); }),
      postProposal: (id, body) => this.call(d, 'postProposal', id, null, () => this.postProposal(d, id, body)),
      declineCall: (callId) => this.call(d, 'declineCall', null, null, () => {
        const call = this.liveCall(callId);
        const dm = this.dms.get(toHex(call.channelId));
        if (dm === undefined) throw httpError(400, 'E_INVALID_REQUEST');
        if (!this.mayView(d, call.channelId)) throw httpError(404, 'E_NOT_FOUND');
      }),
    };
  }

  private async call<T>(dev: string, route: RouteName, group: Id | null, from: bigint | null, run: () => T): Promise<T> {
    this.calls.push({ dev, route, group: group === null ? '' : toHex(group), from });
    if (this.revokedDevs.has(dev)) throw httpError(401, 'E_UNAUTHENTICATED');
    const injection = this.injections.get(route)?.shift();
    await (injection !== undefined && 'gate' in injection ? injection.gate : Promise.resolve());
    if (injection !== undefined && 'fail' in injection) throw injection.fail;
    if (injection !== undefined && 'before' in injection) injection.before();
    const out = run();
    if (injection !== undefined && 'after' in injection) injection.after();
    if (injection !== undefined && 'lose' in injection) throw httpError(0, 'E_NETWORK');
    return out;
  }

  /** POST /v1/communities: the caller owns the new server and is its first member (communities.go create). */
  private createCommunity(d: string, name: string): { id: Id; roleEveryone: Id; policyVersion: bigint } {
    const owner = this.owners.get(d);
    if (owner === undefined || nameProblem(name, COMMUNITY_NAME_MAX_BYTES) !== null) throw httpError(400, 'E_INVALID_REQUEST');
    this.counter += 1;
    const id = idOf(0xc8, this.counter);
    this.communities.set(toHex(id), { id, name, owner, members: new Set([owner]) });
    return { id, roleEveryone: idOf(0xc7, this.counter), policyVersion: 1n };
  }
  /** A created server the caller is a member of; 404 otherwise, so a non-member does not learn that it exists. */
  private memberOf(d: string, communityId: Id): { owner: string } {
    const c = this.communities.get(toHex(communityId));
    if (c === undefined || !c.members.has(this.owners.get(d) ?? '')) throw httpError(404, 'E_NOT_FOUND');
    return c;
  }
  /** POST /v1/communities/{id}/channels: only the owner holds ManageChannels (no role grants it in the model); a voice
   *  channel is also a channel calls may start in. */
  private createChannel(d: string, communityId: Id, channel: { kind: 'text' | 'voice'; name: string; position: number; overwrites?: readonly NewOverwrite[] }):
    { id: Id; mode: number; visibility: number } {
    const c = this.memberOf(d, communityId);
    if (c.owner !== this.owners.get(d)) throw httpError(403, 'E_FORBIDDEN');
    if (nameProblem(channel.name, CHANNEL_NAME_MAX_BYTES) !== null) throw httpError(400, 'E_INVALID_REQUEST');
    this.counter += 1;
    const channelId = idOf(0xcb, this.counter);
    const kind = channel.kind === 'voice' ? 1 : 0;
    this.channels.set(toHex(channelId), { communityId, channelId, created: { kind, name: channel.name, position: channel.position } });
    if (kind === 1) this.voiceChannels.set(toHex(channelId), communityId);
    // The ninth element: the channel is private from birth.
    if (channel.overwrites !== undefined && channel.overwrites.length > 0) {
      const rows = this.overwriteRows.get(toHex(communityId)) ?? [];
      for (const o of channel.overwrites) rows.push({ channelId, kind: o.kind, targetId: o.targetId, allow: o.allow, deny: o.deny });
      this.overwriteRows.set(toHex(communityId), rows);
    }
    return { id: channelId, mode: 0, visibility: 0 };
  }
  /** Only the owner holds ManageRoles in the model. */
  private ownerOf(d: string, communityId: Id): void {
    const c = this.memberOf(d, communityId);
    if (c.owner !== this.owners.get(d)) throw httpError(403, 'E_FORBIDDEN');
  }
  private ownerOfChannel(d: string, channelId: Id): Id {
    const known = this.channels.get(toHex(channelId));
    if (known === undefined) throw httpError(404, 'E_NOT_FOUND');
    this.ownerOf(d, known.communityId);
    return known.communityId;
  }
  private roleOf(d: string, roleId: Id): RoleRow {
    for (const [c, rows] of this.roleRows) {
      const row = rows.find((r) => same(r.id, roleId));
      if (row !== undefined) { this.ownerOf(d, this.communities.get(c)!.id); return row; }
    }
    throw httpError(404, 'E_NOT_FOUND');
  }

  private channelRows(communityId: Uint8Array): {
    id: Uint8Array; kind: number; mode: number; visibility: number; parentId: Uint8Array | null; name: string; topic: string;
    position: number; slowmode: number; seq: bigint; textGroupId: Uint8Array | null;
  }[] {
    return [...this.channels.values()]
      .filter((c) => same(c.communityId, communityId))
      .map((c, i) => ({
        id: c.channelId, kind: c.created?.kind ?? 0, mode: 0, visibility: 0, parentId: null, name: c.created?.name ?? `channel-${String(i)}`,
        topic: c.topic ?? '', position: c.created?.position ?? i, slowmode: c.slowmode ?? 0, seq: 1n,
        textGroupId: c.created?.kind === 1 ? null : this.openGroupOf(c.channelId)?.id ?? null,
      }));
  }

  private postGroup(d: string, body: Uint8Array): { nextSeq: bigint } {
    const v = arr(decode(body), 4);
    const id = bin(at(v, 0), 16);
    const binding = arr(decode(bin(at(v, 1))));
    if (binding.length !== 3 && binding.length !== 4) throw httpError(400, 'E_INVALID_REQUEST');
    const channelId = bin(at(binding, 2), 16);
    const kind = binding.length === 4 && u64(at(binding, 3)) === 1n ? 1 : 0;
    if (this.unlisted(d)) throw httpError(400, 'E_INVALID_REQUEST');
    if (this.openGroupOf(channelId, kind) !== undefined) throw httpError(409, 'E_GROUP_EXISTS');
    this.groups.set(toHex(id), this.newGroup(id, channelId, d, kind));
    const dm = this.dms.get(toHex(channelId));
    // A DM's text group adds every participant device; a call group is joined by external commit only.
    if (dm !== undefined && kind === 0) {
      const g = this.must(id);
      for (const user of dm.users) {
        for (const [device, owner] of this.owners) {
          if (owner === toHex(user) && !g.members.has(device) && !this.revokedDevs.has(device) && this.keyPackageDevs.has(device))
            this.propose(id, 'add', this.devices.get(device)!);
        }
      }
    }
    return { nextSeq: 1n };
  }

  private page(d: string, id: Id, from: bigint, limit: number, handshakes: boolean): { raw: Uint8Array; count: number; lastSeq: bigint | null } {
    const g = this.must(id);
    if (!g.members.has(d)) throw httpError(404, 'E_NOT_FOUND');
    if (from < g.prunedBelow) throw httpError(410, 'E_PRUNED');
    const rows = g.log.filter((e) => e.seq >= from && (e.hs !== null) === handshakes).slice(0, limit);
    const raw = encode(
      rows.map((e) =>
        e.hs !== null
          ? [e.seq, e.epoch, e.hs.kind, e.hs.sender, e.blob]
          : [e.seq, e.epoch, e.uploader, e.blob, e.commitment, e.franking, e.recvTs, e.deleted ? 1 : 0],
      ),
    );
    const last = rows[rows.length - 1];
    return { raw, count: rows.length, lastSeq: last === undefined ? null : last.seq };
  }

  private postCommit(d: string, id: Id, body: Uint8Array): { seq: bigint; epoch: bigint } {
    const g = this.must(id);
    if (!g.members.has(d)) throw httpError(403, 'E_LEAF_NOT_CURRENT');
    const v = arr(decode(body), 5);
    if (u64(at(v, 0)) !== g.epoch) throw httpError(409, 'E_COMMIT_CONFLICT');
    const commit = bin(at(v, 1));
    const c = arr(decode(commit), 5);
    const adds = arr(at(c, 3)).map((x) => bin(x, 16));
    const removes = arr(at(c, 4)).map((x) => bin(x, 16));
    const seq = this.appendCommit(g, 1, 0n, commit, adds, removes);
    for (const w of arr(at(v, 3))) {
      const p = arr(w, 2);
      this.addWelcome(toHex(bin(at(p, 0), 16)), g, seq, bin(at(p, 1)));
    }
    return { seq, epoch: g.epoch };
  }

  private postMessage(dev: Id, id: Id, body: Uint8Array): { raw: Uint8Array; seq: bigint } {
    const g = this.must(id);
    if (!g.members.has(toHex(dev))) throw httpError(403, 'E_LEAF_NOT_CURRENT');
    if (g.proposals.some((p) => p.epoch === g.epoch && !p.void)) throw httpError(425, 'E_COMMIT_REQUIRED', 2000);
    const v = arr(decode(body), 2);
    const blob = bin(at(v, 1));
    if (blob.length > 131072) throw httpError(413, 'E_TOO_LARGE');
    if (u64(at(v, 0)) !== g.epoch) throw httpError(422, 'E_COMMIT_INVALID', null, ['epoch']);
    const e = this.appendMessage(g, dev, blob);
    return { raw: encode([e.seq, e.franking, e.recvTs]), seq: e.seq };
  }

  private postResync(dev: Id, id: Id, body: Uint8Array): { seq: bigint; epoch: bigint } {
    const g = this.must(id);
    if (this.aclDeny.has(toHex(dev))) throw httpError(404, 'E_NOT_FOUND');
    if (this.denyJoin.has(toHex(dev))) throw httpError(403, 'E_FORBIDDEN');
    if (this.removeTarget.has(toHex(dev))) throw httpError(409, 'E_REMOVE_PENDING');
    if (this.unlisted(toHex(dev))) throw httpError(422, 'E_COMMIT_INVALID', null, ['external_joiner']);
    const v = arr(decode(body), 2);
    const commit = bin(at(v, 0));
    if (u64(at(arr(decode(commit), 5), 1)) !== g.epoch + 1n) throw httpError(409, 'E_COMMIT_CONFLICT');
    const seq = this.appendCommit(g, 2, null, commit, [dev], []);
    return { seq, epoch: g.epoch };
  }

  private welcomePage(d: string, after: bigint): { raw: Uint8Array; count: number; lastId: bigint | null } {
    const rows = this.welcomes.filter((w) => w.dev === d && w.id > after).slice(0, 64);
    const last = rows[rows.length - 1];
    return {
      raw: encode(rows.map((w) => [w.id, w.groupId, w.epoch, w.commitSeq, w.blob, wire.tree(w.groupId, w.epoch), wire.treeHash(w.groupId, w.epoch)])),
      count: rows.length,
      lastId: last === undefined ? null : last.id,
    };
  }

  private newGroup(id: Id, channelId: Id, member: string, kind: 0 | 1 = 0): DsGroup {
    return { id, kind, closed: false, channelId, epoch: 0n, next: 1n, log: [], members: new Set([member]), proposals: [], prunedBelow: 0n };
  }

  /** The channel's open group of `kind` (a call group: the newest, L-HTTP-100); a DM's text group and its call group share
   *  the channel and never collide. */
  private openGroupOf(channelId: Id, kind: 0 | 1 = 0): DsGroup | undefined {
    let found: DsGroup | undefined;
    for (const g of this.groups.values()) {
      if (!same(g.channelId, channelId) || g.kind !== kind || g.closed) continue;
      if (kind === 0) return g;
      found = g;
    }
    return found;
  }

  private must(id: Id): DsGroup {
    const g = this.groups.get(toHex(id));
    if (g === undefined) throw httpError(404, 'E_NOT_FOUND');
    return g;
  }

  private tick(): bigint {
    this.clockS += 1n;
    return this.clockS;
  }

  private appendMessage(g: DsGroup, uploader: Id, blob: Uint8Array): DsEntry {
    // The commitment is read from the upload itself (protocol/02: the stored value of C), never sent beside it.
    const e: DsEntry = {
      seq: g.next, epoch: g.epoch, hs: null, uploader, blob, commitment: commitmentOf(blob), deleted: false,
      franking: new Uint8Array(32).fill(Number(g.next % 256n)), recvTs: this.tick(),
    };
    g.next += 1n;
    g.log.push(e);
    for (const m of [...g.members]) this.emit(m, 19, g.id, [e.seq, e.epoch, uploader, blob, e.franking, e.recvTs]);
    return e;
  }

  /**
   * Appends a commit framed at the current epoch, advances the epoch, voids its proposals, fans out op 16.
   * The row is labelled with the pre-commit epoch, unlike dillad (see ModelDs.row.peerCommit, CORE-ENGINE-02).
   */
  private appendCommit(g: DsGroup, kind: number, sender: bigint | null, blob: Uint8Array, adds: readonly Id[], removes: readonly Id[]): bigint {
    const e: DsEntry = {
      seq: g.next, epoch: g.epoch, hs: { kind, sender }, uploader: null, blob, commitment: null, deleted: false,
      franking: new Uint8Array(32), recvTs: this.tick(),
    };
    g.next += 1n;
    g.log.push(e);
    for (const p of g.proposals) if (p.epoch === g.epoch) p.void = true;
    g.epoch += 1n;
    const audience = new Set(g.members);
    for (const a of adds) {
      g.members.add(toHex(a));
      audience.add(toHex(a));
      this.removed.delete(`${toHex(g.id)}/${toHex(a)}`);
    }
    for (const r of removes) g.members.delete(toHex(r));
    for (const m of audience) this.emit(m, 16, g.id, [e.seq, e.epoch, BigInt(kind), sender, blob]);
    return e.seq;
  }

  private addWelcome(dev: string, g: DsGroup, commitSeq: bigint, blob: Uint8Array): void {
    this.welcomeIds += 1n;
    const w: DsWelcome = { id: this.welcomeIds, dev, groupId: g.id, epoch: g.epoch, commitSeq, blob };
    this.welcomes.push(w);
    // Labelled truthfully: the epoch after the commit that added dev and that epoch's tree hash.
    this.emit(dev, 20, g.id, [w.id, w.epoch, commitSeq, blob, wire.tree(g.id, g.epoch), wire.treeHash(g.id, g.epoch)]);
  }

  private emit(dev: string, op: number, groupId: Id, payload: CborValue[]): void {
    const sink = this.sinks.get(dev);
    if (sink === undefined) return;
    const left = this.drops.get(dev) ?? 0;
    if (left > 0) {
      this.drops.set(dev, left - 1);
      return;
    }
    this.frameN += 1n;
    sink({ op, n: this.frameN, groupId, payload });
  }
}
