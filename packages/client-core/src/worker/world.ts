import { expect, vi } from 'vitest';
import type { BootOutcome } from '../account/boot';
import { Enrol } from '../account/enrol';
import { Session } from '../account/session';
import type { Signup } from '../account/signup';
import { encode, type CborInput } from '../cbor';
import { CoreError, type ActivityRow, type ApplyResult, type AttachmentDescriptor, type CorePort, type GroupInfo, type HumanInteractionEvent, type HumanInteractionEventPage, type IdentityInfo, type ManualRemovalEvidence, type ManualWarningEvidence, type OutboxRow, type OwnDeviceList, type PinRow, type PurgeRow, type SealedObjects, type SendRequest, type SessionRecord, type ThreadRow, type SignedLists, type TimelineRow, type TrustBlock, type TrustRecord } from '../core-port';
import { Gateway, type GatewayDeps, type GatewayEvent, type ReadyInfo } from '../gateway/gateway';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { OWNER_TOP, Routes, type DeviceRow, type GifPage, type GifStatus, type AFKSettings, type AFKSnapshot, type AccountMe, type ProfilePatch, type MemberRow, type BanRow, type Factors, type InviteRow, type Instance, type NewOverwrite, type NewRole, type OverwriteRow, type RolePatch, type RoleRow } from '../http/routes';
import type { AccountState, BadgeState, ChannelSummary, ConnectionState, NoticesState, TimelineState } from '../state/types';
import type { SyncDeps, SyncEngine } from '../sync/engine';
import { Controller, type ControllerDeps, type ControllerParts } from './controller';
import type { LockLike, LockManagerLike } from './leader';
import type { FromWorker } from './protocol';

/** tsc does not narrow a find or filter callback, so the slice messages are named by a type predicate. */
export type SliceMessage = Extract<FromWorker, { t: 'slice' }>;

export const INSTANCE_ID = new Uint8Array(16).fill(0x42);
export const INSTANCE_HEX = '42'.repeat(16);
export const LOCK = `dilla-core:${INSTANCE_HEX}`;
// GET /v1/instance: [wire, e2ee, media, instance_id, generation, name, registration_mode, auth_methods,
// policy_version, external_sender_key_id, external_sender_pub] (interfaces.md A3, protocol/09).
export const INSTANCE_DOC: CborInput = [[1], [1], [1], INSTANCE_ID, 7, 'dilla.test', 0, [0], 1, new Uint8Array(16).fill(1), new Uint8Array(32).fill(2)];
// GET /v1/instance/limits, eleven elements.
export const LIMITS: CborInput = [131_072, 104_857_600, 4, 2, 32, 8, 10_737_418_240, 30_000, 131_584, 30, 30];

export function cbor(value: CborInput, status = 200): Response {
  return new Response(encode(value).slice(), { status, headers: { 'content-type': 'application/cbor' } });
}

export function fakeFetch(paths: string[]): typeof fetch {
  // eslint-disable-next-line @typescript-eslint/no-unnecessary-type-assertion, @typescript-eslint/require-await -- the brief's double, kept as written
  return (async (input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    paths.push(path);
    if (path === '/v1/instance') return cbor(INSTANCE_DOC);
    if (path === '/v1/instance/limits') return cbor(LIMITS);
    // web3.6: enterReady's factor gate reads the factors; this account owes nothing.
    if (path === '/v1/auth/factors') return cbor([0, 1_700_000_000, [], 10, 1]);
    return cbor(['E_NOT_FOUND', 'not modelled by this test', null], 404);
  }) as typeof fetch;
}

/** Web Locks as one origin sees them: another tab may hold a lock; a plain request waits for it. */
export class FakeLocks implements LockManagerLike {
  readonly requests: { name: string; ifAvailable: boolean }[] = [];
  private readonly held = new Set<string>();
  private readonly waiting = new Map<string, (() => void)[]>();
  isHeld(name: string): boolean { return this.held.has(name); }

  holdElsewhere(name: string): () => void {
    this.held.add(name);
    return () => {
      this.held.delete(name);
      this.waiting.get(name)?.shift()?.();
    };
  }

  async request(name: string, options: { ifAvailable?: boolean }, callback: (lock: LockLike | null) => Promise<void>): Promise<void> {
    this.requests.push({ name, ifAvailable: options.ifAvailable === true });
    if (this.held.has(name)) {
      if (options.ifAvailable === true) return callback(null);
      await new Promise<void>((resolve) => {
        const queue = this.waiting.get(name) ?? [];
        queue.push(resolve);
        this.waiting.set(name, queue);
      });
    }
    this.held.add(name);
    try { await callback({ name }); }
    finally {
      this.held.delete(name);
      this.waiting.get(name)?.shift()?.();
    }
  }
}

/** Only what boot, the signup phases and the reset path call; nothing else is reached in these tests. */
export function stubCore(): { core: CorePort; resets: () => number } {
  let phase: IdentityInfo['phase'] = 0;
  let resets = 0;
  const core = {
    identity: (): IdentityInfo => ({
      phase, instanceId: phase === 0 ? null : INSTANCE_ID, userId: null,
      deviceId: phase === 0 ? null : new Uint8Array(16).fill(9), username: '', listPublished: false,
    }),
    signupBegin: (): string => { phase = 1; return 'ABCD'.repeat(13); },
    signupReset: (): void => { phase = 0; resets += 1; },
    trustGeneration: (): bigint => 0n,
    close: (): void => {},
  };
  return { core: core as unknown as CorePort, resets: () => resets };
}

export function harness(over: Partial<ControllerDeps> = {}) {
  const posted: FromWorker[] = [];
  const paths: string[] = [];
  const locks = new FakeLocks();
  const deps: ControllerDeps = {
    origin: 'http://127.0.0.1:8453',
    fetch: fakeFetch(paths),
    WebSocket: class {} as unknown as typeof WebSocket,
    locks,
    now: () => 1_700_000_000_000,
    random: () => 0,
    setTimeout: (fn, ms) => setTimeout(fn, ms) as unknown as number,
    clearTimeout: (handle) => clearTimeout(handle),
    sleep: () => Promise.resolve(),
    boot: () => Promise.resolve({ kind: 'unsupported', reason: 'not set by this test' }),
    resetDevice: () => Promise.resolve(),
    post: (m) => { posted.push(m); },
    testHooks: false,
    ...over,
  };
  const controller = new Controller(deps);
  const last = <T>(name: string): T | undefined =>
    [...posted].reverse().find((m): m is SliceMessage => m.t === 'slice' && m.name === name)?.value as T | undefined;
  return {
    controller, posted, paths, locks, last,
    account: () => last<AccountState>('account'),
    connection: () => last<ConnectionState>('connection'),
    ret: (id: number) => posted.find((m) => m.t === 'ret' && m.id === id),
    call: (id: number, command: unknown) => controller.handle({ t: 'call', id, command }),
  };
}

export const opened = (core: CorePort) => (instance: Instance): Promise<BootOutcome> =>
  Promise.resolve({ kind: 'opened', core, instance });


// ---- The ready phase, with the five composed parts (ControllerParts) replaced by doubles. ----

export const USER = new Uint8Array(16).fill(0x0a);
export const DEVICE = new Uint8Array(16).fill(0x0b);
export const COMMUNITY = new Uint8Array(16).fill(0xc0);
export const CHANNEL = new Uint8Array(16).fill(0xc1);
export const GROUP = new Uint8Array(16).fill(0xc2);
export const STRANGER = new Uint8Array(16).fill(0x5a);
export const NEW_COMMUNITY = new Uint8Array(16).fill(0x5c);
export const ROLE_EVERYONE = new Uint8Array(16).fill(0x5d);
export const INSTANCE: Instance = {
  instanceId: INSTANCE_ID, generation: 7n, name: 'dilla.test', registrationMode: 0, authMethods: [0],
  policyVersion: 1n, externalSenderPub: new Uint8Array(32).fill(2),
};
export const READY_INFO: ReadyInfo = {
  deviceId: DEVICE, userId: USER, generation: 7n, versions: { wire: 2n, e2ee: 1n, media: 1n }, keypackagesRemaining: 32, groups: [],
  heartbeatMs: 30_000, maxFrameBytes: 131_584, backoffMs: 1_000, backoffJitterMs: 500,
};
export const refusal = (status: number, code: string, retryAfterMs: number | null = null): DillaHttpError =>
  new DillaHttpError({ status, code, detail: '', retryAfterMs, extra: [] });
export const SUBMIT = {
  m: 'signupSubmit', invite: 'friends-code', username: 'web', display: 'Web', password: null, recoveryKeyAcknowledged: true,
} as const;

export function textGroup(state: 0 | 1 | 2 | 3 | 4): GroupInfo {
  return { groupId: GROUP, kind: 0, communityId: COMMUNITY, targetId: CHANNEL, state, epoch: 1n, nextSeq: 1n, proposalsPending: 0, pendingCommit: false };
}

export function applied(state: 0 | 1 | 2 | 3 | 4): ApplyResult {
  return { state, epoch: 2n, nextSeq: 5n, newSeqs: [], proposalsPending: 0, epochChanged: true, ownAdopted: false };
}

export function strangerRow(seq: number): TimelineRow {
  return {
    seq: BigInt(seq), epoch: 1n, recvTs: 1_700_000_000n, status: 0, reason: '', senderUser: STRANGER,
    senderDevice: new Uint8Array(16).fill(0x5b), senderKind: 0, senderTier: 0, msgId: new Uint8Array(16).fill(0x30 + seq),
    type: 0, body: `from a stranger ${seq}`, editedSeq: 0n, reply: null, reactions: [], pinned: false, attachments: [], mention: false,
    thread: null,
  };
}

export class GatewayDouble {
  starts = 0;
  stops = 0;
  readonly status = 'idle';
  private readonly listeners: ((e: GatewayEvent) => void)[] = [];
  constructor(private readonly log: string[] = []) {}
  start(): void { this.starts += 1; this.log.push('gateway.start'); }
  stop(): void { this.stops += 1; this.log.push('gateway.stop'); }
  subscribe(listener: (e: GatewayEvent) => void): () => void { this.listeners.push(listener); return () => {}; }
  commitAck(): void {}
  sendCallStats = vi.fn(() => false);
  sendPresenceActivity = vi.fn(() => false);
  // eslint-disable-next-line @typescript-eslint/no-unused-vars -- the arguments make the double match the gateway
  sendTyping = vi.fn((_channel: Uint8Array, _on: boolean): void => {});
  // eslint-disable-next-line @typescript-eslint/no-unused-vars -- the arguments make the double match the gateway
  sendVoiceSelf = vi.fn((_call: Uint8Array, _flags: 0 | 1 | 2 | 3): void => {});
  emit(e: GatewayEvent): void { for (const l of this.listeners) l(e); }
}

export const DM_CHANNEL = new Uint8Array(16).fill(0xd1);
export const DM_GROUP = new Uint8Array(16).fill(0xd2);
export const OTHER_DEVICE = new Uint8Array(16).fill(0x0c);
export const ORPHAN_GROUP = new Uint8Array(16).fill(0xe2);
export const ORPHAN_CHANNEL = new Uint8Array(16).fill(0xe1);
export const RECOVERY_KEY = 'QRST'.repeat(13);
export const PASSWORD = 'correct horse battery staple';
export const ROOT = new Uint8Array([1, 2, 3]);
export const STATE_OBJECT = new Uint8Array([4, 5, 6]);
export const LOCAL_STATE = new Uint8Array([4, 5, 7]);
export const LIST_RAW = new Uint8Array([0x84, 0x01, 0x41, 0x09]);
export const FETCHED = { root: ROOT, state: STATE_OBJECT, listBody: LIST_RAW };
export const NOW_S = 1_700_000_000n;

// `now` is an addition to the brief's double (pre-flight ruling (a)): a test of the notice floor moves the clock.
// `realAccount` (WORKER-WEB-01) composes the real Session and Enrol over these doubles, so the session record the
// core keeps decides what ensure() does.
// web3.6 C2: `owed` is the account's second-factor debt as GET /v1/auth/factors answers it; `realGateway` builds the real
// Gateway over `WebSocket`, and `httpTicket` sends postTicket through the real Routes and HttpClient over `fetch`, so a
// refused ticket reaches the controller's HttpDeps.onRefusal as it does in the worker.
export function world(opts: {
  phase?: 0 | 1 | 2 | 3; enrolUser?: boolean; realRoutes?: boolean; realAccount?: boolean; fetch?: typeof fetch; now?: () => number;
  /** web3.8 C1 fix m3: the monotonic clock (deps.monotonic), apart from the wall clock `now`. */
  monotonic?: () => number;
  owed?: boolean; realGateway?: boolean; WebSocket?: typeof WebSocket; httpTicket?: boolean;
  /** S2 review M5: getInstance through the real Routes and HttpClient over `fetch` (the bearer it sends is observable). */
  httpInstance?: boolean;
} = {}) {
  // An account that is already registered (phase 2) is already a member of the community, so that a
  // selectCommunity of it passes pre-flight ruling 1(v); a signup or sign-in world starts outside it.
  const calls: string[] = [];
  const state = {
    phase: opts.phase ?? 2, enrolUser: opts.enrolUser === true ? USER : null as Uint8Array | null, username: 'web',
    groups: [] as GroupInfo[], rows: [] as TimelineRow[], activity: [] as ActivityRow[], settings: {} as Record<string, string>,
    // web3.7: each thread's held root (null when not held) and replies, newest first, by the root's msg id in hex; the
    // threads the group lists. A test sets the rows as the core would read them (the thread rule already applied).
    threadRows: new Map<string, { root: TimelineRow | null; rows: TimelineRow[] }>(), threadList: [] as ThreadRow[],
    ownList: { version: 1n, published: true, entries: [{ deviceId: DEVICE, dskPub: new Uint8Array(32), tier: 1, addedAt: 1n, revokedAt: null }] } as OwnDeviceList,
    resets: 0, joined: (opts.phase ?? 2) === 2,
    dms: [] as { channelId: Uint8Array; kind: 3 | 4; members: Uint8Array[] }[],
    session: null as SessionRecord | null, listPublished: null as boolean | null,
    outbox: [] as OutboxRow[], pins: [] as PinRow[], purges: [] as PurgeRow[], roleIds: [] as Uint8Array[],
    manualEvidence: [] as ManualRemovalEvidence[],
    manualWarnings: [] as ManualWarningEvidence[],
    descriptors: new Map<string, AttachmentDescriptor>(), blobs: new Map<string, Uint8Array>(),
    // v0.1.0-web3.1: the servers and channels this account created, as the instance would list them after the create.
    created: [] as { id: Uint8Array; name: string }[],
    newChannels: [] as { communityId: Uint8Array; row: Awaited<ReturnType<Routes['listChannels']>>[number] }[],
    owed: opts.owed === true,
  };
  /* eslint-disable @typescript-eslint/no-unused-vars -- the typed parameters give the doubles their call signatures */
  const core = {
    identity: (): IdentityInfo => ({
      phase: state.phase, instanceId: INSTANCE_ID,
      userId: state.phase === 2 ? USER : state.phase === 3 ? state.enrolUser : null,
      deviceId: DEVICE, username: state.phase === 2 ? state.username : '', listPublished: state.listPublished ?? state.phase === 2,
    }),
    signupBegin: (): string => { state.phase = 1; return 'ABCD'.repeat(13); },
    signupReset: (): void => { state.phase = 0; state.resets += 1; },
    groups: (): GroupInfo[] => state.groups,
    groupRow: (g: Uint8Array): GroupInfo | null => state.groups.find((x) => toHex(x.groupId) === toHex(g)) ?? null,
    removalCursor: vi.fn((_groupId: Uint8Array) => 0n),
    removalActorHistoryVersion: vi.fn(() => 1n),
    refreshRemovalActorHistory: vi.fn(() => 1n),
    ingestRemoval: vi.fn((_groupId: Uint8Array, _entry: unknown) => 1n),
    timeline: (): TimelineRow[] => state.rows,
    signRemoval: vi.fn((_requestId: Uint8Array, _groupId: Uint8Array, _seq: bigint, _expires: bigint, _now: bigint) =>
      encode([new Uint8Array([1]), new Uint8Array(64).fill(7)])),
    prepareManualRemovalEvidence: vi.fn((proofRequestId: Uint8Array, evidenceRequestId: Uint8Array, communityId: Uint8Array,
      groupId: Uint8Array, channelId: Uint8Array, messageId: Uint8Array, targetSequence: bigint, reason: string, excerpt: string,
      createdAt: bigint, expires: bigint) => {
      const signedBody = encode([new Uint8Array([1]), new Uint8Array(64).fill(7)]);
      state.manualEvidence.push({ proofRequestId, evidenceRequestId, actorUserId: USER, actorDeviceId: DEVICE, communityId,
        groupId, channelId, messageId, targetSequence, reason, excerpt, signedBody, state: 0, createdAt, expires, evidenceExpires: 0n });
      return signedBody;
    }),
    manualRemovalEvidence: vi.fn(() => state.manualEvidence.filter((row) => row.state < 3)),
    ensureManualRemovalEvidenceRequest: vi.fn((proofRequestId: Uint8Array, candidateRequestId: Uint8Array, now: bigint) => {
      const row = state.manualEvidence.find((item) => toHex(item.proofRequestId) === toHex(proofRequestId));
      if (row === undefined || row.state !== 2) throw new Error('missing accepted removal');
      if (row.evidenceExpires === 0n) row.evidenceExpires = now + 30n;
      else if (row.evidenceExpires <= now) { row.evidenceRequestId = candidateRequestId; row.evidenceExpires = now + 30n; }
      return { requestId: row.evidenceRequestId, expires: row.evidenceExpires };
    }),
    markManualRemovalSubmitted: vi.fn((requestId: Uint8Array) => {
      const row = state.manualEvidence.find((item) => toHex(item.proofRequestId) === toHex(requestId)); if (row !== undefined && row.state === 0) row.state = 1;
    }),
    markManualRemovalAccepted: vi.fn((requestId: Uint8Array) => {
      const row = state.manualEvidence.find((item) => toHex(item.proofRequestId) === toHex(requestId)); if (row !== undefined && row.state < 3) row.state = 2;
    }),
    completeManualRemovalEvidence: vi.fn((requestId: Uint8Array) => {
      const index = state.manualEvidence.findIndex((item) => toHex(item.proofRequestId) === toHex(requestId)); if (index >= 0) state.manualEvidence.splice(index, 1);
    }),
    rejectUnsubmittedManualRemoval: vi.fn((requestId: Uint8Array) => {
      const index = state.manualEvidence.findIndex((item) => toHex(item.proofRequestId) === toHex(requestId) && item.state === 0); if (index >= 0) state.manualEvidence.splice(index, 1);
    }),
    rejectManualRemoval: vi.fn((requestId: Uint8Array) => {
      const index = state.manualEvidence.findIndex((item) => toHex(item.proofRequestId) === toHex(requestId) && item.state < 2); if (index >= 0) state.manualEvidence.splice(index, 1);
    }),
    prepareManualWarningEvidence: vi.fn((requestId: Uint8Array, evidenceRequestId: Uint8Array, communityId: Uint8Array, targetUserId: Uint8Array, reason: string, createdAt: bigint) => {
      state.manualWarnings.push({ requestId, evidenceRequestId, actorUserId: USER, actorDeviceId: DEVICE, communityId, targetUserId,
        reason, state: 0, createdAt, actionId: new Uint8Array(16), revision: 0n, acceptedAt: 0n, evidenceExpires: 0n });
    }),
    manualWarningEvidence: vi.fn(() => state.manualWarnings.filter(row => row.state < 2)),
    markManualWarningAccepted: vi.fn((requestId: Uint8Array, actionId: Uint8Array, revision: bigint, acceptedAt: bigint) => {
      const row = state.manualWarnings.find(item => toHex(item.requestId) === toHex(requestId));
      if (row !== undefined) { row.actionId = actionId; row.revision = revision; row.acceptedAt = acceptedAt; row.state = 1; }
    }),
    ensureManualWarningEvidenceRequest: vi.fn((requestId: Uint8Array, candidateRequestId: Uint8Array, now: bigint) => {
      const row = state.manualWarnings.find(item => toHex(item.requestId) === toHex(requestId));
      if (row === undefined || row.state !== 1) throw new Error('missing accepted warning');
      if (row.evidenceExpires === 0n) row.evidenceExpires = now + 30n;
      else if (row.evidenceExpires <= now) { row.evidenceRequestId = candidateRequestId; row.evidenceExpires = now + 30n; }
      return { requestId: row.evidenceRequestId, expires: row.evidenceExpires };
    }),
    completeManualWarningEvidence: vi.fn((requestId: Uint8Array) => {
      const index = state.manualWarnings.findIndex(item => toHex(item.requestId) === toHex(requestId));
      if (index >= 0) state.manualWarnings.splice(index, 1);
    }),
    humanInteractionSendPrepare: vi.fn((requestId: Uint8Array, _communityId: Uint8Array, _groupId: Uint8Array,
      _requestBody: Uint8Array, _now: bigint) => requestId),
    humanInteractionEventsRead: vi.fn((_after: bigint, _limit: number): HumanInteractionEventPage => ({ events: [], next: 0n, highWater: 0n })),
    humanInteractionReceiptsRead: vi.fn((_communityId: Uint8Array, _installationId: Uint8Array, _botUserId: Uint8Array, _requestIds: readonly Uint8Array[]): HumanInteractionEvent[] => []),
    humanInteractionEventsAck: vi.fn((_through: bigint) => {}),
    humanInteractionEventsAckSelected: vi.fn((_deliveryIds: readonly bigint[]) => {}),
    sendEncrypt: vi.fn((_msgId: Uint8Array) => ({ groupId: new Uint8Array(16).fill(0x52), messageBody: new Uint8Array([0xa1]) })),
    sendConfirm: vi.fn((_msgId: Uint8Array, _response: Uint8Array) => ({ groupId: new Uint8Array(16).fill(0x52), seq: 1n })),
    sendRetry: vi.fn((_msgId: Uint8Array) => {}),
    sendFail: vi.fn((_msgId: Uint8Array, _error: string) => {}),
    instanceSignerKeyMatches: vi.fn((_key: Uint8Array): void => {}),
    instanceSignerKeyPin: vi.fn((_instanceId: Uint8Array, _key: Uint8Array): void => {}),
    threadTimeline: vi.fn((_g: Uint8Array, root: Uint8Array, beforeSeq: bigint, limit: number) => {
      const t = state.threadRows.get(toHex(root)) ?? { root: null, rows: [] };
      return { root: t.root, rows: t.rows.filter((r) => beforeSeq === 0n || r.seq < beforeSeq).slice(0, limit) };
    }),
    threads: vi.fn((_g: Uint8Array): ThreadRow[] => state.threadList),
    outbox: (): OutboxRow[] => state.outbox,
    activity: (): ActivityRow[] => state.activity,
    markRead: vi.fn((_g: Uint8Array, _seq: bigint, _now: bigint): void => { calls.push('core.markRead'); }),
    settings: (): Record<string, string> => ({ ...state.settings }),
    settingPut: vi.fn((k: string, v: string): void => { state.settings[k] = v; }),
    settingDelete: vi.fn((k: string): void => { state.settings = Object.fromEntries(Object.entries(state.settings).filter(([key]) => key !== k)); }),
    ownDeviceList: (): OwnDeviceList => state.ownList,
    deviceListRevoke: vi.fn((_input: unknown): SignedLists => {
      calls.push('core.deviceListRevoke'); return { deviceListBody: new Uint8Array([7]), stateSealed: new Uint8Array([8]), interrupted: null };
    }),
    deviceListDrop: vi.fn((): void => { calls.push('core.deviceListDrop'); state.listPublished = false; }),
    stateSealedUploaded: vi.fn((): void => { calls.push('core.stateSealedUploaded'); }),
    // An addition to the brief's double (task-14 fix round 1): the state object this device sealed, as the core keeps it.
    sealedObjects: vi.fn((): SealedObjects => ({ root: ROOT, state: LOCAL_STATE, stateUploaded: true })),
    deviceListPublished: vi.fn((): void => { calls.push('core.deviceListPublished'); state.listPublished = true; }),
    deviceListBody: (): Uint8Array => new Uint8Array([7]),
    enrolComplete: vi.fn((_input: unknown): SignedLists => {
      calls.push('core.enrolComplete'); state.phase = 2; state.listPublished = false;
      return { deviceListBody: new Uint8Array([7]), stateSealed: new Uint8Array([8]), interrupted: null };
    }),
    session: (): SessionRecord | null => state.session,
    sessionStore: vi.fn((s: SessionRecord): void => { state.session = s; }),
    sessionClear: vi.fn((): void => { calls.push('core.sessionClear'); state.session = null; }),
    sessionSign: (_nonce: Uint8Array, _purpose: 0 | 1): Uint8Array => new Uint8Array([0x5e]),
    recoveryKeyCheck: vi.fn((_key: string): void => { calls.push('core.recoveryKeyCheck'); }),
    pins: vi.fn((_g: Uint8Array): PinRow[] => state.pins),
    purges: vi.fn((): PurgeRow[] => state.purges),
    purgeDone: vi.fn((_g: Uint8Array, seq: bigint): void => {
      calls.push(`core.purgeDone(${String(seq)})`); state.purges = state.purges.filter((p) => p.seq !== seq);
    }),
    ownRolesSet: vi.fn((_c: Uint8Array, _ids: Uint8Array[]): void => {}),
    authoritySet: vi.fn((_g: Uint8Array, _pinners: Uint8Array[], _broadcasters: Uint8Array[], _mentionable: Uint8Array[]): void => {}),
    trustVerify: vi.fn((_u: Uint8Array, _k: Uint8Array): void => {}),
    safetyNumberFor: vi.fn((_u: Uint8Array, _k: Uint8Array): string => Array.from({ length: 12 }, () => '12345').join(' ')),
    sendBlockCode: vi.fn((_g: Uint8Array): TrustBlock | null => null),
    sendBlockOwn: vi.fn((_g: Uint8Array): boolean => false),
    keyPackages: vi.fn((_count: number, _last: boolean): Uint8Array => new Uint8Array([1])),
    search: vi.fn((_scope: unknown, _query: string, _cursor: unknown, _limit: number) => [] as {
      groupId: Uint8Array; seq: bigint; snippet: string; rank: bigint; threadRoot: Uint8Array | null }[]),
    resume: vi.fn((): Promise<void> => Promise.resolve()),
    attachmentGet: vi.fn((_g: Uint8Array, seq: bigint, index: number): AttachmentDescriptor => {
      const d = state.descriptors.get(`${String(seq)}:${String(index)}`);
      if (d === undefined) throw new CoreError('E_CORE_NOT_FOUND', 'no such attachment');
      return d;
    }),
    pause: vi.fn((): void => { calls.push('core.pause'); }),
    close: vi.fn((): void => {}),
    pendingLeaves: vi.fn(() => [] as { groupId: Uint8Array; leafIndex: number; userId: Uint8Array; cachedVersion: bigint; reason: string }[]),
    proposedAdds: vi.fn(() => []),
    deviceListIngestAdd: vi.fn(() => ({ version: 1n, changed: false, quarantined: 0 })),
    deviceListIngest: vi.fn((_group: Uint8Array, _leaf: number, _user: Uint8Array, _history: Uint8Array, _head: Uint8Array, _now: bigint) =>
      ({ version: 1n, changed: true, quarantined: 1 })),
    accountKindIngest: vi.fn((_user: Uint8Array, _attestation: Uint8Array) => undefined),
    revalidatePending: vi.fn(() => 0),
    trustState: vi.fn((): TrustRecord[] => []),
    trustGeneration: vi.fn(() => 0n),
  };
  const routes = {
    negotiateSession: vi.fn(() => Promise.resolve({ wire: 3, e2ee: 1, media: 1, botAvailable: true })),
    getInstance: vi.fn(() => Promise.resolve(INSTANCE)),
    getLimits: vi.fn(() => Promise.resolve({
      maxCiphertextBytes: 131_072, keypackagesPerDevice: 32, keypackageRefillThreshold: 8, heartbeatMs: 30_000, maxFrameBytes: 131_584,
    })),
    getInvite: vi.fn((_code: string) => Promise.resolve({ communityId: null as Uint8Array | null, communityName: null as string | null, expires: 0n })),
    joinCommunity: vi.fn((_id: Uint8Array, _invite: string) => { state.joined = true; return Promise.resolve(); }),
    listCommunities: vi.fn(() => { calls.push('listCommunities'); return Promise.resolve([
      ...(state.joined ? [{ id: COMMUNITY, name: 'friends', owner: USER, policyVersion: 1n }] : []),
      ...state.created.map((c) => ({ id: c.id, name: c.name, owner: USER, policyVersion: 1n })),
    ]); }),
    listChannels: vi.fn((id: Uint8Array) => { calls.push('listChannels'); return Promise.resolve([
      ...(toHex(id) === toHex(COMMUNITY) ? [{
        id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: null, name: 'general', topic: '', position: 0, slowmode: 0, seq: 1n, textGroupId: GROUP,
      }] : []),
      ...state.newChannels.filter((c) => toHex(c.communityId) === toHex(id)).map((c) => c.row),
    ]); }),
    getAFK: vi.fn((_id: Uint8Array): Promise<AFKSnapshot> => Promise.resolve({ channelId: null, timeoutSeconds: 300, pending: false, away: [] })),
    gifStatus: vi.fn((_c: Uint8Array): Promise<GifStatus> => Promise.resolve({ on: false, rating: 'pg-13' })),
    gifSetKey: vi.fn((_c: Uint8Array, _key: string): Promise<void> => Promise.resolve()),
    gifClearKey: vi.fn((_c: Uint8Array): Promise<void> => Promise.resolve()),
    gifSetRating: vi.fn((_c: Uint8Array, _rating: string): Promise<void> => Promise.resolve()),
    gifSearch: vi.fn((_c: Uint8Array, _q: string, _offset: number): Promise<GifPage> => Promise.resolve({ items: [], next: null })),
    gifMedia: vi.fn((_c: Uint8Array, _id: string, _variant: 'preview' | 'full'): Promise<Uint8Array> => Promise.resolve(new Uint8Array([0x47, 0x49, 0x46, 0x38, 0x39, 0x61]))),
    getAFKSettings: vi.fn((_id: Uint8Array): Promise<AFKSettings> => Promise.resolve({ channelId: null, timeoutSeconds: 300 })),
    setAFKSettings: vi.fn(() => Promise.resolve()),
    sendAFKActivity: vi.fn(() => Promise.resolve()),
    putAFKAway: vi.fn((_id: Uint8Array) => Promise.resolve()),
    createCommunity: vi.fn((name: string) => {
      calls.push('createCommunity');
      state.created.push({ id: NEW_COMMUNITY, name });
      return Promise.resolve({ id: NEW_COMMUNITY, roleEveryone: ROLE_EVERYONE, policyVersion: 1n });
    }),
    createChannel: vi.fn((communityId: Uint8Array, c: { kind: 'text' | 'voice'; name: string; position: number; overwrites?: readonly NewOverwrite[] }) => {
      calls.push('createChannel');
      const id = new Uint8Array(16).fill(0x60 + state.newChannels.length);
      state.newChannels.push({ communityId, row: { id, kind: c.kind === 'voice' ? 1 : 0, mode: 0, visibility: 0, parentId: null,
        name: c.name, topic: '', position: c.position, slowmode: 0, seq: 0n, textGroupId: null } });
      return Promise.resolve({ id, mode: 0, visibility: 0 });
    }),
    createInvite: vi.fn((_communityId: Uint8Array, _invite: { maxUses: number; ttlSeconds: number }) => Promise.resolve({
      inviteId: new Uint8Array(16).fill(0x71), code: 'AB12CD34', url: 'https://dilla.test/i/AB12CD34', expires: NOW_S + 604_800n,
    })),
    listMembers: vi.fn((_id: Uint8Array): Promise<MemberRow[]> => {
      calls.push('listMembers');
      return Promise.resolve([{ userId: USER, username: 'web', display: 'Web', kind: 0 as const, nick: '', roleIds: state.roleIds }]);
    }),
    applyRolePanelReaction: vi.fn((_community: Uint8Array, _channel: Uint8Array, _messageSeq: bigint, _group: Uint8Array,
      _eventSeq: bigint, _emoji: string, _enabled: boolean) => Promise.resolve()),
    getPresenceSnapshot: vi.fn((_id: Uint8Array) => Promise.resolve([{ userId: USER, status: 1 }])),
    setPresenceChoice: vi.fn((_choice: number) => Promise.resolve()),
    patchMe: vi.fn((_patch: ProfilePatch) => Promise.resolve()),
    sendPresenceActivity: vi.fn(() => Promise.resolve()),
    // wave 1 (web3.4): the admin routes. None of them pushes into `calls`, which the older suites read in order.
    getPermissions: vi.fn((_id: Uint8Array) => Promise.resolve({ bits: (1n << 30n) - 1n, top: OWNER_TOP })),
    postModerationRemoval: vi.fn((_groupId: Uint8Array, _body: Uint8Array) => Promise.resolve()),
    postMessage: vi.fn((_groupId: Uint8Array, _body: Uint8Array) => Promise.resolve({ raw: encode([1n]), seq: 1n })),
    getBotInstallation: vi.fn((_id: Uint8Array) => Promise.resolve(null)),
    getBotCommandStatus: vi.fn((_id: Uint8Array) => Promise.resolve({ installationId: new Uint8Array(16).fill(0x73), botUserId: USER, state: 3, revision: 1n })),
    armAutoModerationGrant: vi.fn((_group: Uint8Array, _requestId: Uint8Array, _community: Uint8Array, _revision: bigint,
      _expires: bigint, _hash: Uint8Array, _stepUp: string) => Promise.resolve({ token: 'opaque-grant-token', expires: NOW_S + 60n })),
    configureBotInstallation: vi.fn((_id: Uint8Array, _requestId: Uint8Array, _revision: bigint | null, _capabilities: number, _stepUp: string) =>
      Promise.resolve({ id: new Uint8Array(16).fill(0x73), communityId: COMMUNITY, state: 0 as const, revision: 1n,
        userId: null, deviceId: null, capabilities: 0, updatedAt: NOW_S })),
    disableBotInstallation: vi.fn((_id: Uint8Array, _requestId: Uint8Array, _revision: bigint) =>
      Promise.resolve({ id: new Uint8Array(16).fill(0x73), communityId: COMMUNITY, state: 4 as const, revision: 2n,
        userId: null, deviceId: null, capabilities: 0, updatedAt: NOW_S })),
    listRoles: vi.fn((_id: Uint8Array) => Promise.resolve([] as RoleRow[])),
    patchCommunity: vi.fn((_id: Uint8Array, _patch: { name: string }) => Promise.resolve(2n)),
    deleteCommunity: vi.fn((_id: Uint8Array) => Promise.resolve()),
    leaveCommunity: vi.fn((_id: Uint8Array) => Promise.resolve()),
    transferOwnership: vi.fn((_id: Uint8Array, _user: Uint8Array) => Promise.resolve()),
    createCategory: vi.fn((_id: Uint8Array, _c: { name: string; position: number }) =>
      Promise.resolve({ id: new Uint8Array(16).fill(0x90), mode: 0, visibility: 0 })),
    patchChannel: vi.fn((_id: Uint8Array, _patch: { name?: string; topic?: string; parent?: Uint8Array | 'top'; position?: number }) => Promise.resolve()),
    deleteChannel: vi.fn((_id: Uint8Array) => Promise.resolve()),
    kickMember: vi.fn((_id: Uint8Array, _user: Uint8Array) => Promise.resolve()),
    putBan: vi.fn((_id: Uint8Array, _user: Uint8Array, _ban: { reason: string; expires: bigint | null }) => Promise.resolve()),
    deleteBan: vi.fn((_id: Uint8Array, _user: Uint8Array) => Promise.resolve()),
    listBans: vi.fn((_id: Uint8Array) => Promise.resolve([] as BanRow[])),
    listInvites: vi.fn((_id: Uint8Array) => Promise.resolve([] as InviteRow[])),
    getMemberTimeout: vi.fn((_communityId: Uint8Array, _userId: Uint8Array): Promise<bigint | null> => Promise.resolve(null)),
    revokeInvite: vi.fn((_id: Uint8Array) => Promise.resolve()),
    // wave 2a (flow 09): roles and overwrites; like the admin routes, none of them pushes into `calls`.
    listOverwrites: vi.fn((_id: Uint8Array) => Promise.resolve([] as OverwriteRow[])),
    createRole: vi.fn((_id: Uint8Array, _role: NewRole) => Promise.resolve({ roleId: new Uint8Array(16).fill(0x91) })),
    patchRole: vi.fn((_id: Uint8Array, _patch: RolePatch) => Promise.resolve()),
    deleteRole: vi.fn((_id: Uint8Array) => Promise.resolve()),
    grantRole: vi.fn((_c: Uint8Array, _user: Uint8Array, _role: Uint8Array) => Promise.resolve()),
    revokeRole: vi.fn((_c: Uint8Array, _user: Uint8Array, _role: Uint8Array) => Promise.resolve()),
    putOverwrite: vi.fn((_ch: Uint8Array, _kind: 0 | 1, _target: Uint8Array, _bits: { allow: number; deny: number }) => Promise.resolve()),
    deleteOverwrite: vi.fn((_ch: Uint8Array, _kind: 0 | 1, _target: Uint8Array) => Promise.resolve()),
    putBlob: vi.fn((_c: Uint8Array, blobId: Uint8Array, stored: Uint8Array) => {
      calls.push(`putBlob(${toHex(blobId).slice(0, 8)})`); state.blobs.set(toHex(blobId), stored);
      return Promise.resolve({ created: true, size: stored.length });
    }),
    confirmBlob: vi.fn((_c: Uint8Array, blobId: Uint8Array) => { calls.push(`confirmBlob(${toHex(blobId).slice(0, 8)})`); return Promise.resolve(); }),
    getBlob: vi.fn((_c: Uint8Array, blobId: Uint8Array): Promise<Uint8Array | null> => Promise.resolve(state.blobs.get(toHex(blobId)) ?? null)),
    deleteBlob: vi.fn((_c: Uint8Array, blobId: Uint8Array) => { calls.push(`deleteBlob(${toHex(blobId).slice(0, 8)})`); return Promise.resolve(); }),
    deleteGroupMessage: vi.fn((_g: Uint8Array, seq: bigint): Promise<'deleted' | 'gone'> => {
      calls.push(`deleteGroupMessage(${String(seq)})`); return Promise.resolve('deleted');
    }),
    postTicket: vi.fn(() => Promise.resolve({ ticket: 'ticket', expires: 0n })),
    postKeyPackages: vi.fn((_body: Uint8Array) => Promise.resolve()),
    // web3.6: the account's factors (owed while state.owed), and the passkey sign-in ceremony's begin.
    getFactors: vi.fn((): Promise<Factors> => {
      return Promise.resolve({ owed: state.owed, totp: state.owed ? null : 1_700_000_000, passkeys: [], backupCodesLeft: state.owed ? 0 : 10,
        hasPassword: true });
    }),
    passkeyLoginBegin: vi.fn(() => Promise.resolve({ ceremonyId: new Uint8Array(16).fill(0xce), options: '{"publicKey":{}}' })),
    getAccountMe: vi.fn((): Promise<AccountMe> => Promise.resolve({ userId: USER, username: 'web', display: 'Web', kind: 0, flags: 0n, presenceChoice: 1 })),
    listDms: vi.fn(() => { calls.push('listDms'); return Promise.resolve(state.dms); }),
    getChannel: vi.fn((_id: Uint8Array) => Promise.resolve({
      id: DM_CHANNEL, kind: 3, mode: 0, visibility: 0, parentId: null, name: '', topic: '', position: 0, seq: 1n, textGroupId: DM_GROUP,
    })),
    postDm: vi.fn((_recipients: Uint8Array[]) => {
      state.dms = [{ channelId: DM_CHANNEL, kind: 3, members: [USER, STRANGER] }];
      return Promise.resolve({ channelId: DM_CHANNEL, created: true });
    }),
    listDevices: vi.fn((): Promise<DeviceRow[]> => { calls.push('listDevices'); return Promise.resolve([
      { id: OTHER_DEVICE, tier: 1 as const, signerTier: 1 as const, verifiedAt: null, revokedAt: null, lastSeen: 1_699_999_000n, label: null },
      { id: DEVICE, tier: 1 as const, signerTier: 1 as const, verifiedAt: null, revokedAt: null, lastSeen: 1_700_000_000n, label: 'work laptop' },
    ]); }),
    patchDevice: vi.fn((_id: Uint8Array, _label: string | null) => { calls.push('patchDevice'); return Promise.resolve(); }),
    deleteSessions: vi.fn((_id: Uint8Array) => { calls.push('deleteSessions'); return Promise.resolve(); }),
    deleteDevice: vi.fn((_id: Uint8Array) => { calls.push('deleteDevice'); return Promise.resolve(); }),
    // The return type is spelled out so the tests' replacements (null, or another Uint8Array) typecheck.
    getBackup: vi.fn((kind: 0 | 1): Promise<{ object: Uint8Array; created: bigint } | null> => {
      calls.push(`getBackup(${kind})`); return Promise.resolve({ object: kind === 0 ? ROOT : STATE_OBJECT, created: 1n });
    }),
    putBackup: vi.fn((kind: 0 | 1, _object: Uint8Array) => { calls.push(`putBackup(${kind})`); return Promise.resolve({ blobId: new Uint8Array(32), size: 3, created: false }); }),
    getDeviceList: vi.fn((_user: Uint8Array) => {
      calls.push('getDeviceList');
      const raw = toHex(_user) === toHex(STRANGER) ? encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]) : LIST_RAW;
      return Promise.resolve({ version: 1n, blob: new Uint8Array([9]), raw });
    }),
    getDeviceListHistory: vi.fn((_user: Uint8Array, _after: bigint) => Promise.resolve({
      raw: encode([[1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]]), count: 1, lastVersion: 1n,
    })),
    putDeviceList: vi.fn((_user: Uint8Array, _body: Uint8Array) => { calls.push('putDeviceList'); return Promise.resolve(); }),
    postChallenge: vi.fn((_device: Uint8Array) => Promise.resolve({ nonce: new Uint8Array(32), expires: NOW_S + 60n })),
    postSession: vi.fn((_device: Uint8Array, _body: Uint8Array) => {
      calls.push('postSession');
      return Promise.resolve({
        token: 'enrolled', scope: 0, userId: USER, deviceId: DEVICE, expires: NOW_S + 604_800n,
        idleExpires: NOW_S + 43_200n, generation: 7n,
      });
    }),
  };
  const session = {
    token: (): string => 'session-token',
    ensure: vi.fn(() => { calls.push('session.ensure'); return Promise.resolve(true); }),
    establish: vi.fn(() => { calls.push('session.establish'); return Promise.resolve(true); }),
  };
  const signup = {
    begin: (_instanceId: Uint8Array): string[] => { core.signupBegin(); return Array.from({ length: 13 }, () => 'ABCD'); },
    submit: vi.fn((_input: unknown) => { state.phase = 2; return Promise.resolve(); }),
    resume: vi.fn((): Promise<0 | 2 | 'revoked'> => Promise.resolve(2)),
  };
  const enrol = {
    login: vi.fn((_username: string, _password: string): Promise<{ needs: 0 | 1 | 2 }> => { calls.push('enrol.login'); return Promise.resolve({ needs: 0 }); }),
    totp: vi.fn((_code: string) => { calls.push('enrol.totp'); return Promise.resolve(); }),
    passkey: vi.fn((_ceremonyId: Uint8Array, _response: string): Promise<{ needs: 0 | 1 }> => { calls.push('enrol.passkey'); return Promise.resolve({ needs: 0 }); }),
    backupCode: vi.fn((_code: string) => { calls.push('enrol.backupCode'); return Promise.resolve(); }),
    register: vi.fn((_instanceId: Uint8Array) => { calls.push('enrol.register'); state.phase = 3; state.enrolUser = USER; return Promise.resolve({ userId: USER }); }),
    fetch: vi.fn((_userId: Uint8Array) => { calls.push('enrol.fetch'); return Promise.resolve(FETCHED); }),
    complete: vi.fn((_key: string, _fetched: unknown, _username: string) => { calls.push('enrol.complete'); state.phase = 2; return Promise.resolve(); }),
    reset: vi.fn((): void => { calls.push('enrol.reset'); state.phase = 0; state.enrolUser = null; }),
  };
  const account = {
    publishDeviceList: vi.fn((_core: CorePort, _routes: Routes) => { calls.push('publishDeviceList'); return Promise.resolve(); }),
    ensureBackups: vi.fn((_core: CorePort, _routes: Routes) => { calls.push('ensureBackups'); return Promise.resolve(); }),
    refreshOwnDeviceList: vi.fn((_core: CorePort, _routes: Routes, _user: Uint8Array) => {
      calls.push('refreshOwnDeviceList'); return Promise.resolve({ version: 1n, listed: true });
    }),
    repairBackupState: vi.fn((_core: CorePort, _routes: Routes) => { calls.push('repairBackupState'); return Promise.resolve(false); }),
  };
  const gateway = new GatewayDouble(calls);
  // An addition to the brief's double (pre-flight ruling (a)): a test mints a ticket through the deps the controller passed.
  const gatewayDeps: { value: GatewayDeps | null } = { value: null };
  const sync = {
    deps: null as SyncDeps | null,
    builds: 0,                                   // parts.sync calls: 1 at ready, +1 for each resume() after a refused wipe call
    start: vi.fn(() => { calls.push('sync.start'); }), stop: vi.fn(() => { calls.push('sync.stop'); }),
    stopped: vi.fn(() => false),
    setChannels: vi.fn(), setExpected: vi.fn((_groups: unknown) => {}), send: vi.fn(), retry: vi.fn(), discard: vi.fn(),
    sendRequest: vi.fn((_g: Uint8Array, _r: SendRequest): Uint8Array => new Uint8Array(16).fill(0x77)),
    runOnGroup: vi.fn((_g: Uint8Array, send: () => Promise<unknown>) => send()),
    pollWelcomes: vi.fn(() => Promise.resolve()),
    openChannel: vi.fn((_c: unknown) => Promise.resolve({ groupId: GROUP, state: 2 as const })),
    rejoined: vi.fn((_communityId: Uint8Array) => {}),
  };
  /* eslint-enable @typescript-eslint/no-unused-vars */
  const wiped: string[] = [];
  const parts: Partial<ControllerParts> = {
    ...(opts.realRoutes === true ? {} : { routes: (http) => {
      const real = new Routes(http);
      if (opts.httpTicket === true) routes.postTicket.mockImplementation(() => real.postTicket());
      if (opts.httpInstance === true) routes.getInstance.mockImplementation(() => real.getInstance());
      return routes as unknown as Routes;
    } }),
    session: opts.realAccount === true ? (d) => new Session(d) : () => session as unknown as Session,
    signup: () => signup as unknown as Signup,
    enrol: opts.realAccount === true ? (d) => new Enrol(d) : () => enrol as unknown as Enrol,
    gateway: (deps) => { gatewayDeps.value = deps; return opts.realGateway === true ? new Gateway(deps) : gateway as unknown as Gateway; },
    sync: (deps) => { sync.deps = deps; sync.builds += 1; return sync as unknown as SyncEngine; },
    publishDeviceList: account.publishDeviceList,
    ensureBackups: account.ensureBackups,
    refreshOwnDeviceList: account.refreshOwnDeviceList,
    repairBackupState: account.repairBackupState,
  };
  const bootCount = { value: 0 };
  const h = harness({
    boot: (instance) => { bootCount.value += 1; return Promise.resolve({ kind: 'opened', core: core as unknown as CorePort, instance }); },
    resetDevice: (instance) => { calls.push('resetDevice'); wiped.push(toHex(instance.instanceId)); return Promise.resolve(); },
    ...(opts.fetch ? { fetch: opts.fetch } : {}),
    ...(opts.now ? { now: opts.now } : {}),
    ...(opts.monotonic ? { monotonic: opts.monotonic } : {}),
    ...(opts.WebSocket ? { WebSocket: opts.WebSocket } : {}),
    parts,
  });
  // The brief's literal put `account` (the three account-part doubles) after `...h`, which shadowed h.account()
  // (the account slice) that the same tests call; one callable object serves both spellings.
  const accountView = Object.assign((): AccountState | undefined => h.account(), account);
  return {
    ...h, state, calls, core, routes, session, signup, enrol, account: accountView, gateway, gatewayDeps, sync, wiped, parts,
    channels: () => h.last<ChannelSummary[]>(`channels:${toHex(COMMUNITY)}`),
    timeline: () => h.last<TimelineState>(`timeline:${toHex(CHANNEL)}`),
    badges: () => h.last<Record<string, BadgeState>>('badges'),
    notices: () => h.last<NoticesState>('notices'),
    get boots(): number { return bootCount.value; },
  };
}
export type World = ReturnType<typeof world>;

/** Starts the worker of a world and waits until its account is ready. */
export async function toReady(w: World): Promise<void> {
  w.call(1, { m: 'start' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
}
