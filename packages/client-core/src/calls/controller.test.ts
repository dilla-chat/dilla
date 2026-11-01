// The worker's call controller (L-TS-56, L-TS-57) over the whole worker: the Controller with ModelCore holding call
// groups, ModelDs answering the delivery-service and call routes (L-HTTP-100..105), the real sync engine, and a scripted
// gateway. Every FromWorker message is recorded in order with its transfer list; a transferred buffer is detached as a
// structured clone with a transfer list detaches it.
import { describe, expect, it } from 'vitest';
import type { BootOutcome } from '../account/boot';
import type { Enrol } from '../account/enrol';
import type { Session } from '../account/session';
import type { Signup } from '../account/signup';
import type { CallKeys, CallJoinResult, CallState, RingsState, SendBlocksState, VoiceState, DmSummary } from '../state/types';
import type { CborValue } from '../cbor';
import { CoreError, type Id } from '../core-port';
import { fakeDskPub } from '../testing/fake-list';
import type { Frame } from '../gateway/frames';
import type { Gateway } from '../gateway/gateway';
import { fromHex, toHex } from '../hex';
import type { Routes } from '../http/routes';
import { DillaHttpError } from '../http/errors';
import { INSTANCE, readyInfo } from '../sync/testing/harness';
import { COMMUNITY, FakeGateway, ManualClock, ME, ModelCore, ModelDs, PEER, THIRD, FOURTH, idOf, settle, type Peer } from '../sync/testing/model';
import { COMMUNITY_RELOAD_DEBOUNCE_MS, Controller, MEMBER_REREAD_MIN_MS, type ControllerParts } from '../worker/controller';
import { CALL_ROTATE_MS, CALL_ROTATE_RETRIES } from './controller';
import { JOIN_READS_MAX } from './join';
import type { FromWorker } from '../worker/protocol';

export const VOICE = idOf(0xc5, 1);
export const DM = idOf(0xd5, 1);
const NAMES = new Map<string, string>([[toHex(ME.user), 'Me'], [toHex(PEER.user), 'bob'], [toHex(THIRD.user), 'carol'], [toHex(FOURTH.user), 'dave']]);
/** Members who joined the server after a page loaded its list: the instance lists them from then on. */
const LATE: Peer[] = [];
/** web3.4 C4: false once this account was removed from the seeded server; the instance no longer lists it. */
const LISTED = { value: true };

/** The route labels the call tests read; the engine's own catch-up reads (messages, handshakes, cursors) are not listed. */
const LABELS: Record<string, string> = {
  getChannelCall: 'GET channel call', getCommunityCalls: 'GET community calls', postGroup: 'POST group', getGroupInfo: 'GET group info',
  getGroupTree: 'GET group tree', postResync: 'POST resync', startCall: 'POST calls', postProposal: 'POST proposal', postCommit: 'POST commit',
  declineCall: 'POST decline', getDeviceList: 'GET device list', shareCall: 'POST share', unshareCall: 'DELETE share', postCallStats: 'POST stats', postCallRtt: 'POST rtt',
  endCallIfAlone: 'DELETE call', listMembers: 'GET members',
};

interface Posted { message: FromWorker; transfer: Transferable[] }
interface Answer { label: string; error: DillaHttpError }
interface Hold { label: string; gate: Promise<void>; reached: () => void }

export interface HarnessOptions {
  voiceChannel?: boolean;
  callGroupBy?: 'peer';
  /** A DM with bob ('dm' / true), or a group DM with bob and carol ('group'). */
  dm?: boolean | 'group';
  /** More DMs with bob, carol and dave in turn (for the ring bound). */
  extraDms?: number;
  /** v0.1.0-web3.1: no seeded server; the model lists what a test creates through the create commands. */
  created?: boolean;
  /** web3.7 3a-client: runs before the worker starts (a leftover call group seeded, a boot read held or refused). */
  before?: (w: { ds: ModelDs; model: ModelCore; server: { hold(label: string): { release: () => void; reached: Promise<void> };
    refuseAll(label: string, status: number, code: string): void } }) => Promise<void> | void;
}

export async function harness(opts: HarnessOptions = {}) {
  const clock = new ManualClock();
  const ds = new ModelDs();
  const model = new ModelCore(ME, INSTANCE);
  for (const p of [ME, PEER, THIRD, FOURTH]) ds.own(p.user, p.device);
  for (const p of [ME, PEER, THIRD, FOURTH]) ds.publishList(p.user, [{ device: p.device }]);
  const seed = (p: Peer): void => { const row = ds.listRow(p.user); if (row !== undefined) model.trustList(p.user, row.version, row.blob); };
  seed(PEER);   // carol's list is not cached: her leaves are pending until a refresh reads it
  ds.addVoiceChannel(COMMUNITY, VOICE);
  let callGroup: Id = new Uint8Array(16);
  if (opts.callGroupBy === 'peer') {
    callGroup = ds.peerCreateCall(PEER, VOICE);
    model.setCallTree(callGroup, [{ leafIndex: 0, user: PEER.user, device: PEER.device }]);
  }
  // DMs: the channel at the instance, its text group held by this device (joined earlier).
  const dms: { channel: Id; text: Id; members: Peer[] }[] = [];
  const addDm = (channel: Id, members: Peer[]): void => {
    ds.addDm(channel, [ME, ...members]);
    const text = ds.peerCreate(members[0], channel);
    ds.join(text, ME.device);
    for (const m of members.slice(1)) ds.join(text, m.device);
    model.seedGroup(text, 0, null, channel, 0n);
    dms.push({ channel, text, members });
  };
  if (opts.dm !== undefined && opts.dm !== false) addDm(DM, opts.dm === 'group' ? [PEER, THIRD] : [PEER]);
  for (let i = 0; i < (opts.extraDms ?? 0); i++) addDm(idOf(0xd6, i + 1), [[PEER, THIRD, FOURTH][i % 3]]);

  // The gateway: ModelDs fans out to it; a test pushes frames of its own.
  const fake = new FakeGateway();
  ds.attach(ME.device, (f) => { fake.frame(f); });
  const voiceSelfSent: { callId: string; flags: number }[] = [];
  const statsTransport = { owned: false, sent: [] as { callId: string; report: unknown }[] };
  const gatewayPart = {
    sendPresenceActivity: () => false, sendCallStats: (callId: Id, report: unknown) => {
      if (statsTransport.owned) statsTransport.sent.push({ callId: toHex(callId), report });
      return statsTransport.owned;
    },
    status: 'ready', start: (): void => {}, stop: (): void => {}, subscribe: fake.subscribe, commitAck: fake.commitAck,
    sendTyping: (): void => {}, sendVoiceSelf: (call: Uint8Array, flags: number): void => { voiceSelfSent.push({ callId: toHex(call), flags }); },
  };

  // The routes: ModelDs for the delivery service and the calls, stubs for the account half, every call recorded by label.
  const base = ds.routesFor(ME.device) as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  const channelRows = [{ id: VOICE, kind: 1, mode: 0, visibility: 0, parentId: null, name: 'lounge', topic: '', position: 0, seq: 1n, textGroupId: null }];
  const stubs: Record<string, (...a: unknown[]) => Promise<unknown>> = {
    ...(opts.created === true ? {} : {
      listCommunities: () => Promise.resolve(LISTED.value ? [{ id: COMMUNITY, name: 'friends', owner: PEER.user, policyVersion: 1n }] : []),
      listChannels: () => Promise.resolve(opts.voiceChannel === true ? channelRows : []),
    }),
    getInstance: () => Promise.resolve(INSTANCE),
    getLimits: () => Promise.resolve({ maxCiphertextBytes: 131_072, keypackagesPerDevice: 32, keypackageRefillThreshold: 0, heartbeatMs: 30_000, maxFrameBytes: 131_584 }),
    listMembers: () => Promise.resolve([ME, PEER, THIRD, FOURTH, ...LATE].map((p) => ({ userId: p.user, username: NAMES.get(toHex(p.user)) ?? '',
      display: NAMES.get(toHex(p.user)) ?? '', kind: 0 as const, nick: '', roleIds: [] }))),
    postTicket: () => Promise.resolve({ ticket: 'ticket', expires: 1_700_000_030n }),
    postKeyPackages: () => Promise.resolve(0),
    getAccountMe: () => Promise.resolve({ userId: ME.user, username: 'me', display: 'Me', kind: 0, flags: 0n }),
    // web3.6: enterReady's factor gate; this account owes nothing.
    getFactors: () => Promise.resolve({ owed: false, totp: 1_700_000_000, passkeys: [], backupCodesLeft: 10, hasPassword: true }),
  };
  const requests: { route: string; args: unknown[] }[] = [];
  const answers: Answer[] = [];
  /** Labels every request of which is refused until allowed again (an instance that cannot be reached). */
  const refusing = new Map<string, DillaHttpError>();
  const holds: Hold[] = [];
  const replies: { label: string; value: unknown }[] = [];
  const names =new Set([...Object.keys(base), ...Object.keys(stubs)]);
  const routes: Record<string, (...a: unknown[]) => Promise<unknown>> = {};
  for (const name of names) {
    routes[name] = async (...args: unknown[]): Promise<unknown> => {
      // An unlabelled hold (the engine's own reads): waits, unrecorded.
      const raw = holds.findIndex((x) => x.label === `raw:${name}`);
      if (raw >= 0) { const [hold] = holds.splice(raw, 1); hold.reached(); await hold.gate; }
      const label = LABELS[name];
      if (label !== undefined) {
        requests.push({ route: label, args });
        const h = holds.findIndex((x) => x.label === label);
        if (h >= 0) { const [hold] = holds.splice(h, 1); hold.reached(); await hold.gate; }
        const always = refusing.get(label);
        if (always !== undefined) throw always;
        const a = answers.findIndex((x) => x.label === label);
        if (a >= 0) { const [answer] = answers.splice(a, 1); throw answer.error; }
        const r = replies.findIndex((x) => x.label === label);
        if (r >= 0) { const [value] = replies.splice(r, 1); return value.value; }
      }
      return (stubs[name] ?? base[name])(...args);
    };
  }
  const server = {
    requests,
    /** The next request of `label` is refused with this status and code. */
    answer(label: string, status: number, code = status === 404 ? 'E_NOT_FOUND' : status === 409 ? 'E_COMMIT_CONFLICT' : 'E_UNAVAILABLE',
      retryAfterMs: number | null = null): void {
      answers.push({ label, error: new DillaHttpError({ status, code, detail: '', retryAfterMs, extra: [] }) });
    },
    /** The next request of `label` waits until release(); `reached` resolves once it is waiting. */
    hold(label: string): { release: () => void; reached: Promise<void> } {
      let release = (): void => {};
      let reached = (): void => {};
      const gate = new Promise<void>((r) => { release = r; });
      const reachedP = new Promise<void>((r) => { reached = r; });
      holds.push({ label, gate, reached });
      return { release, reached: reachedP };
    },
    /** The next request of `label` is answered with `value` instead of the model's answer. */
    reply(label: string, value: unknown): void { replies.push({ label, value }); },
    /** The next call of a route method by name (labelled or not) waits until release(), unrecorded. */
    holdRaw(method: string): { release: () => void; reached: Promise<void> } { return server.hold(`raw:${method}`); },
    routes(): string[] { return requests.map((r) => r.route); },
    reset(): void { requests.length = 0; },
    /** Every request of `label` is refused with this status and code until allow(label). */
    refuseAll(label: string, status: number, code: string): void {
      refusing.set(label, new DillaHttpError({ status, code, detail: '', retryAfterMs: null, extra: [] }));
    },
    allow(label: string): void { refusing.delete(label); },
  };

  const posted: Posted[] = [];
  /** Whether a session can be established again (false: a revoked socket ends in the revoked phase). */
  const sessionState = { ok: true };
  const session = { token: (): string => 'token', ensure: (): Promise<boolean> => Promise.resolve(true),
    establish: (): Promise<boolean> => Promise.resolve(sessionState.ok) };
  const parts: Partial<ControllerParts> = {
    routes: () => routes as unknown as Routes,
    session: () => session as unknown as Session,
    signup: () => ({}) as unknown as Signup,
    enrol: () => ({}) as unknown as Enrol,
    gateway: () => gatewayPart as unknown as Gateway,
    publishDeviceList: () => Promise.resolve(),
    ensureBackups: () => Promise.resolve(),
    refreshOwnDeviceList: () => Promise.resolve({ version: 1n, listed: true }),
    repairBackupState: () => Promise.resolve(false),
  };
  let released = 0;
  /** The wall clock's step from the monotonic one (a test moves it: NTP, a resumed laptop). */
  const wall = { skew: 0 };
  const controller = new Controller({
    origin: 'http://127.0.0.1:8453', fetch: (() => Promise.reject(new Error('no fetch'))),
    WebSocket: class {} as unknown as typeof WebSocket,
    locks: { request: async (name, _o, cb) => { await cb({ name }); } },
    now: () => clock.now() + wall.skew, monotonic: clock.now, random: () => 0.5, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout, sleep: () => Promise.resolve(),
    boot: (instance): Promise<BootOutcome> => Promise.resolve({ kind: 'opened', core: model, instance }),
    resetDevice: () => Promise.resolve(),
    post: (message, transfer = []) => {
      posted.push({ message, transfer });
      if (message.t === 'released') released += 1;
      // What postMessage does with a transfer list: the receiver gets the buffer, the sender's copy is detached.
      structuredClone(message, { transfer });
    },
    testHooks: false,
    parts,
  });
  let nextId = 1;
  const ret = (id: number): Extract<FromWorker, { t: 'ret' }> | undefined =>
    posted.map((p) => p.message).find((m): m is Extract<FromWorker, { t: 'ret' }> => m.t === 'ret' && m.id === id);
  const send = (command: unknown): number => { const id = nextId++; controller.handle({ t: 'call', id, command }); return id; };
  async function call(command: unknown): Promise<unknown> {
    const id = send(command);
    for (let i = 0; i < 400; i++) {
      const r = ret(id);
      if (r !== undefined) {
        if (r.ok) return r.value;
        throw Object.assign(new Error(r.error.code), r.error);
      }
      await settle();
    }
    throw new Error(`no answer to ${JSON.stringify(command)}`);
  }
  const slices = (): Extract<FromWorker, { t: 'slice' }>[] =>
    posted.map((p) => p.message).filter((m): m is Extract<FromWorker, { t: 'slice' }> => m.t === 'slice');
  function slice(name: 'call'): CallState;
  function slice(name: 'voice'): VoiceState;
  function slice(name: 'rings'): RingsState;
  function slice(name: 'sendBlocks'): SendBlocksState;
  function slice(name: 'dms'): DmSummary[];
  function slice(name: string): unknown;
  function slice(name: string): unknown { return slices().filter((s) => s.name === name).at(-1)?.value; }

  await opts.before?.({ ds, model, server });
  await call({ m: 'start' });
  for (let i = 0; i < 100 && (slice('account') as { phase?: string } | undefined)?.phase !== 'ready'; i++) await settle();
  fake.emit({ type: 'status', status: 'ready', closeCode: null });
  fake.ready(readyInfo(ME));
  await settle();
  await clock.advance(0);

  return {
    controller, model, ds, clock, server, call, send, ret, slice, voiceSelfSent, sessionState, wall,
    /** The gateway reports a revoked socket (the worker re-establishes its session, or ends in revoked). */
    revokeSocket: (): void => { fake.emit({ type: 'revoked' }); },
    /** The socket goes down, then is ready again (the browser back online). */
    reconnect: (): void => {
      fake.emit({ type: 'status', status: 'waiting', closeCode: null });
      fake.emit({ type: 'status', status: 'ready', closeCode: null });
    },
    /** The socket goes down (and stays down until online()). */
    offline: (): void => { fake.emit({ type: 'status', status: 'waiting', closeCode: null }); },
    online: (): void => { fake.emit({ type: 'status', status: 'ready', closeCode: null }); },
    statsTransport, callGroup, voice: toHex(VOICE), dm: toHex(DM), dms, gateway: { push: (f: Frame): void => { fake.frame(f); } },
    posted: (): Posted[] => [...posted],
    pushes: (t: FromWorker['t']): Posted[] => posted.filter((p) => p.message.t === t),
    clearPosted: (): void => { posted.length = 0; },
    released: (): number => released,
    slices: { all: (): unknown[] => slices().map((s) => s.value) },
    /** A commit by bob in the group (a self-update), fanned out to this device: the engine applies it. */
    async deliverHandshakes(group: Id, by: Peer = PEER): Promise<void> {
      ds.peerCommit(group, by);
      await settle();
      await clock.advance(0);
    },
  };
}
export type World = Awaited<ReturnType<typeof harness>>;

describe('call harness startup', () => {
  it('enters ready only after checking the model instance signer pin', async () => {
    const w = await harness();
    expect(w.slice('account')).toMatchObject({ phase: 'ready' });
    expect(w.model.instancePinChecks).toEqual([INSTANCE.externalSenderPub]);
  });
});

/** A voice_state frame as the gateway hands it over (op 50). */
export function voiceFrame(peer: Peer, call: Id, flags: number): Frame {
  return { op: 50, n: 0n, groupId: null, payload: [peer.user, peer.device, call, BigInt(flags)] as CborValue[] };
}

export const CALL_KEYS = (p: Posted): CallKeys => (p.message as { keys: CallKeys }).keys;
export type { CallJoinResult };

describe('voice presence (L-GW-32, L-TS-56 step 7)', () => {
  // Web-3 task 10, concern 4: a participant the loaded member list lacks is looked up (the list is read again once per
  // unknown user) and the voice slice then names them, never leaving the id's first 8 characters on the tile.
  it('a participant who joined the server after the member list loaded is named once the list is read again', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    const late: Peer = { user: idOf(0xe8, 1), device: idOf(0xd8, 1) };
    LATE.push(late);
    NAMES.set(toHex(late.user), 'erin');
    try {
      w.gateway.push(voiceFrame(late, VOICE, 1));
      const nameOf = () => w.slice('voice')[w.voice]?.participants.find((p) => p.userId === toHex(late.user))?.name;
      for (let i = 0; i < 40 && nameOf() !== 'erin'; i++) await settle();
      expect(nameOf()).toBe('erin');
    } finally {
      LATE.length = 0;
      NAMES.delete(toHex(late.user));
    }
  });

  // f10app m3. Attacker: the instance, voice_state's only sender, streams frames for ever-new user ids to make the worker
  // re-read the member list back to back and grow its looked-up set without bound.
  const flood = (w: World, from: number, n: number): void => {
    for (let i = from; i < from + n; i++) w.gateway.push(voiceFrame({ user: idOf(0xe6, i + 1), device: idOf(0xd6, i + 1) }, VOICE, 1));
  };
  const memberReads = (w: World): number => w.server.routes().filter((r) => r === 'GET members').length;

  it('voice-driven member reads are paced: the read owed after a flood waits 5 s after the last one ended', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    flood(w, 0, 10);
    await settled(w);
    expect(memberReads(w)).toBe(1);
    flood(w, 10, 10);
    await settled(w);
    await w.clock.advance(4_000);
    await settled(w);
    expect(memberReads(w)).toBe(1);
    await w.clock.advance(1_000);
    await settled(w);
    expect(memberReads(w)).toBe(2);
  });

  it('at most VOICE_LOOKUPS_MAX users a community are looked up from voice frames; beyond it a flood reads nothing', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    const quiet = async (): Promise<void> => { for (let i = 0; i < 4; i++) { await settled(w); await w.clock.advance(6_000); } await settled(w); };
    flood(w, 0, 200);
    await quiet();
    const under = memberReads(w);
    expect(under).toBeGreaterThan(0);
    // 56 more users fit under the bound (256): they are looked up.
    flood(w, 200, 100);
    await quiet();
    const atBound = memberReads(w);
    expect(atBound).toBeGreaterThan(under);
    // Beyond it, a flood of new users reads nothing more.
    flood(w, 300, 300);
    await quiet();
    expect(memberReads(w)).toBe(atBound);
  });

  it('loadVoice applies frames received during the snapshot read after it, in order', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    // A live call in the lounge with X (bob's device) in it.
    await w.ds.routesFor(PEER.device).startCall(VOICE, '');
    w.ds.voice(VOICE, PEER, 1);
    await w.call({ m: 'loadVoice', communityId: null, channelId: w.voice });
    expect(w.slice('voice')[w.voice]?.participants.map((p) => [p.deviceId, p.flags])).toEqual([[toHex(PEER.device), 1]]);
    // The next snapshot read is held; meanwhile X joins again (1), leaves (0) and Y (carol) joins with flags 3.
    const held = w.server.hold('GET channel call');
    const id = w.send({ m: 'loadVoice', communityId: null, channelId: w.voice });
    await held.reached;
    w.gateway.push(voiceFrame(PEER, VOICE, 1));
    w.gateway.push(voiceFrame(PEER, VOICE, 0));
    w.gateway.push(voiceFrame(THIRD, VOICE, 3));
    // The snapshot answered names X only ([.., [[X, 1]]]): it was taken before the frames.
    held.release();
    for (let i = 0; i < 20 && w.ret(id) === undefined; i++) await settle();
    expect(w.ret(id)).toMatchObject({ ok: true });
    const call = w.slice('voice')[w.voice];
    expect(call?.participants.map((p) => [p.userId, p.deviceId, p.flags, p.name])).toEqual([[toHex(THIRD.user), toHex(THIRD.device), 3, 'carol']]);
  });

  it('loadVoice of a community lists each live call and drops a channel whose call ended; own device is marked', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    await w.ds.routesFor(PEER.device).startCall(VOICE, '');
    w.ds.voice(VOICE, PEER, 1);
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    await w.call({ m: 'loadVoice', communityId: toHex(COMMUNITY), channelId: null });
    expect(w.slice('voice')[w.voice]?.participants.map((p) => [p.name, p.own, p.web, p.bot])).toEqual([['bob', false, false, false]]);
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.slice('voice')[w.voice]?.participants.find((p) => p.own)).toMatchObject({ userId: toHex(ME.user), web: true });
    w.ds.endCall(VOICE);
    await w.call({ m: 'loadVoice', communityId: toHex(COMMUNITY), channelId: null });
    expect(w.slice('voice')[w.voice]).toBeUndefined();
  });

  it('refuses loadVoice of a channel or community it does not know, and drops a frame of an unknown call', async () => {
    const w = await harness({ voiceChannel: true });
    await expect(w.call({ m: 'loadVoice', communityId: null, channelId: 'ab'.repeat(16) })).rejects.toMatchObject({ code: 'E_BAD_INPUT' });
    await expect(w.call({ m: 'loadVoice', communityId: 'ab'.repeat(16), channelId: null })).rejects.toMatchObject({ code: 'E_BAD_INPUT' });
    w.gateway.push(voiceFrame(PEER, idOf(0x77, 1), 1));
    expect(w.slice('voice')).toEqual({});
  });
});

describe('sendBlocks stays a text-group map (L-TS-57)', () => {
  // Attacker: the instance or a member poisons a DM's call group (it shares the DM text group's target id). The block
  // must stay with the call: the DM's composer is not paused by it, nor does the call group pass as the DM's text group.
  it('a call group never enters sendBlocks, even in a DM sharing its target id', async () => {
    const w = await harness({ dm: true });
    const dmCall = idOf(0x9e, 1);
    w.model.seedGroup(dmCall, 1, null, DM);
    const before = w.slice('dms').find((d) => d.id === w.dm);
    expect(before?.group).toBe('active');
    w.model.blockGroup(dmCall, 'E_DEVICE_UNLISTED');
    await w.call({ m: 'refreshTrust' });
    expect(w.slice('sendBlocks')).toEqual({});
    expect(w.slice('dms').find((d) => d.id === w.dm)).toEqual(before);
    // The control: the DM's text group blocked is in the map, under the DM's channel id.
    w.model.blockGroup(w.dms[0].text, 'E_UMK_CHANGED');
    await w.call({ m: 'refreshTrust' });
    expect(w.slice('sendBlocks')).toEqual({ [w.dm]: { code: 'E_UMK_CHANGED', own: false } });
  });
});

// ---------------------------------------------------------------------------------------------
// Task 6b: the worker joins, keys, leaves, pauses and rings calls (L-TS-56).
// ---------------------------------------------------------------------------------------------

const settled = async (w: World, n = 4): Promise<void> => { for (let i = 0; i < n; i++) { await settle(); await w.clock.advance(0); } };
const JOIN = (w: World): { m: 'callJoin'; channelId: string; vdec: string } => ({ m: 'callJoin', channelId: w.voice, vdec: 'vp8' });
const codeOf = async (p: Promise<unknown>): Promise<string> => p.then(() => 'ok', (e: { code?: string }) => e.code ?? 'thrown');
/** A ring frame (op 53) as the gateway hands it over. */
function ring(state: 0 | 1 | 2, o: { call?: Id; channel?: Id; user?: Peer; expires?: number } = {}): Frame {
  return { op: 53, n: 0n, groupId: null,
    payload: [o.call ?? o.channel ?? DM, o.channel ?? DM, (o.user ?? PEER).user, BigInt(state), BigInt(o.expires ?? 1_700_000_030)] as CborValue[] };
}
/** In the lounge's call: bob's call group joined, the log and the posts cleared. */
async function joined(opts: HarnessOptions = {}): Promise<World & { result: CallJoinResult }> {
  const w = await harness({ voiceChannel: true, callGroupBy: 'peer', ...opts });
  const result = await w.call(JOIN(w)) as CallJoinResult;
  await settled(w);
  w.clearPosted();
  w.server.reset();
  return Object.assign(w, { result });
}
const keysPosted = (w: World): CallKeys[] => w.pushes('callKeys').map(CALL_KEYS);
/** The last call-path request; the worker's trust refresh reads lists at its own times beside it. */
const lastCallRoute = (w: World): string | undefined => w.server.routes().filter((r) => r !== 'GET device list').at(-1);

describe('callJoin (L-TS-56 step 1)', () => {
  it('joins an existing call group and pushes the join epoch key once, before the ret, never in a slice or a ret', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    const r = await w.call({ m: 'callJoin', channelId: w.voice, vdec: 'vp8' }) as CallJoinResult;
    const posted = w.posted();                                   // every FromWorker message in order, with its transfer list
    const k = posted.findIndex((m) => m.message.t === 'callKeys');
    const ret = posted.findIndex((m) => m.message.t === 'ret' && m.message.ok && (m.message.value as CallJoinResult | null)?.callId === r.callId);
    expect(k).toBeGreaterThanOrEqual(0); expect(k).toBeLessThan(ret);
    expect(posted[k].transfer).toEqual([ (posted[k].message as { keys: CallKeys }).keys.baseKey ]);
    expect(posted.filter((m) => m.message.t === 'callKeys')).toHaveLength(1);
    expect(JSON.stringify(r)).not.toMatch(/baseKey/);
    expect(w.server.requests.map((q) => q.route)).toEqual(['GET channel call', 'GET group info', 'GET group tree', 'POST resync', 'POST calls']);
    expect(JSON.stringify(w.slices.all())).not.toContain(w.model.lastBaseKeyHex(w.callGroup));
    expect(w.model.lastEpochBytes(w.callGroup).every((b) => b === 0)).toBe(true);   // the adapter zero-filled the decoded bytes
    expect(w.slice('call')).toMatchObject({ phase: 'in', epoch: r.epoch, code: expect.stringMatching(/^(\d{5} ){5}\d{5}$/) as string });
    // The key the page receives is the core's, for the join epoch, with the roster of the tree.
    const keys = CALL_KEYS(posted[k]);
    expect(keys).toMatchObject({ groupId: toHex(w.callGroup), epoch: r.epoch, selfLeaf: 1 });
    expect(keys.roster.map((x) => x.userId)).toEqual([toHex(PEER.user), toHex(ME.user)]);
    expect(r).toMatchObject({ groupId: toHex(w.callGroup), callId: w.voice, token: expect.any(String) as string });
  });

  // RV-1 (progress note of task 5): the core cannot stop a worker that posts a staged join without call_join_check, so the
  // worker must never do so. The double's join accepts anything the gate tolerates; its check refuses.
  it('never posts a staged join without call_join_check first', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    w.model.callJoinCheck = (): void => { throw new CoreError('E_CREDENTIAL', ''); };
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_CREDENTIAL');
    expect(w.server.routes()).toEqual(['GET channel call', 'GET group info', 'GET group tree']);
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', error: { code: 'E_CREDENTIAL' } });
    expect(w.model.group(w.callGroup)).toBeUndefined();
    expect(w.pushes('callKeys')).toHaveLength(0);
  });

  // A commit that lands between the two reads (bob committing the Remove a reloaded page's boot sweep proposed, as its
  // rejoin starts) serves a GroupInfo and a tree of different epochs. The core refuses that pair (E_CORE_INPUT), and
  // the join reads both again, as the text group's external join does, rather than failing the call.
  it('a commit landing between the GroupInfo and tree reads makes the join read both again, not fail', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    const held = w.server.hold('GET group tree');
    const joined = w.call(JOIN(w));
    await held.reached;
    w.ds.peerCommit(w.callGroup, PEER);
    held.release();
    await joined;
    expect(w.server.routes()).toEqual(['GET channel call', 'GET group info', 'GET group tree', 'GET group info', 'GET group tree',
      'POST resync', 'POST calls']);
    expect(w.slice('call').phase).toBe('in');
  });

  // The same commit, landing after both reads and before the resync is posted: the external commit was built on an epoch
  // the group has left. The instance refuses it (dillad: 422 E_COMMIT_INVALID, rule structural, since its resync takes the
  // epoch to commit against from the group itself; the model: 409 E_COMMIT_CONFLICT). The group has moved on, so the
  // refusal says nothing of the join: it reads GroupInfo and tree again and builds the commit on the new epoch.
  it.each([
    ['422 E_COMMIT_INVALID (dillad: structural)', 422, 'E_COMMIT_INVALID'],
    ['409 E_COMMIT_CONFLICT (a commit that lost the epoch)', 409, 'E_COMMIT_CONFLICT'],
  ] as const)('a commit landing between the tree read and the resync (%s) makes the join read both again, not fail', async (_name, status, code) => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    const held = w.server.hold('POST resync');
    w.server.answer('POST resync', status, code);
    const joined = w.call(JOIN(w));
    await held.reached;
    w.ds.peerCommit(w.callGroup, PEER);
    held.release();
    await joined;
    expect(w.server.routes()).toEqual(['GET channel call', 'GET group info', 'GET group tree', 'POST resync',
      'GET group info', 'GET group info', 'GET group tree', 'POST resync', 'POST calls']);
    expect(w.slice('call').phase).toBe('in');
  });

  it('a resync refused while the group has not moved fails the join at once, without a second commit', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    w.server.answer('POST resync', 422, 'E_COMMIT_INVALID');
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_COMMIT_INVALID');
    expect(w.server.routes()).toEqual(['GET channel call', 'GET group info', 'GET group tree', 'POST resync', 'GET group info']);
    expect(w.slice('call').phase).toBe('failed');
    expect(w.model.group(w.callGroup)).toBeUndefined();
  });

  it('a group that moves under every commit fails the join after a bounded number of reads', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    for (let i = 0; i < 5; i++) w.ds.inject('postResync', { before: () => { w.ds.peerCommit(w.callGroup, PEER); } });
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_COMMIT_CONFLICT');
    // The documented bound (join.ts, JOIN_READS_MAX), written as a literal: raising it is a decision this test must show.
    expect(JOIN_READS_MAX).toBe(3);
    expect(w.server.routes().filter((r) => r === 'POST resync').length).toBe(3);
    expect(w.slice('call').phase).toBe('failed');
  });

  it('a GroupInfo and tree that keep disagreeing fail the join after a bounded number of reads', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    w.model.callGroupJoinExternal = (): Uint8Array => { throw new CoreError('E_CORE_INPUT', 'info_body and tree_body disagree'); };
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_CORE_INPUT');
    expect(w.server.routes().filter((r) => r === 'GET group tree').length).toBe(JOIN_READS_MAX);
    expect(w.server.routes()).not.toContain('POST resync');
    expect(w.slice('call').phase).toBe('failed');
  });

  it('registers the call group when none is open and re-reads once on E_GROUP_EXISTS', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    // The first read finds no open group (the peer registered meanwhile): POST group meets theirs.
    w.server.reply('GET channel call', { groupId: null, callId: null, participants: [] });
    w.server.answer('POST group', 409, 'E_GROUP_EXISTS');
    await w.call(JOIN(w));
    expect(w.server.routes()).toEqual(['GET channel call', 'POST group', 'GET channel call', 'GET group info', 'GET group tree', 'POST resync', 'POST calls']);
    expect(w.slice('call').phase).toBe('in');
    expect(w.model.groups().filter((g) => g.kind === 1 && g.state === 2).map((g) => toHex(g.groupId))).toEqual([toHex(w.callGroup)]);
  });

  it('a call it opens in a voice channel with no group registers one and keys its only leaf', async () => {
    const w = await harness({ voiceChannel: true });
    w.server.reset();
    const r = await w.call(JOIN(w)) as CallJoinResult;
    expect(w.server.routes()).toEqual(['GET channel call', 'POST group', 'POST calls']);
    expect(CALL_KEYS(w.pushes('callKeys')[0]).roster).toEqual([{ leaf: 0, deviceId: toHex(ME.device), userId: toHex(ME.user) }]);
    expect(w.slice('call')).toMatchObject({ phase: 'in', outgoing: null, callId: r.callId });
  });

  it('a voice channel created through the client path has no call group until its first call registers one', async () => {
    const w = await harness({ created: true });
    const { communityId } = await w.call({ m: 'createCommunity', name: 'Midgard Crew' }) as { communityId: string };
    const { channelId } = await w.call({ m: 'createChannel', communityId, kind: 'voice', name: 'general' }) as { channelId: string };
    w.server.reset();
    await w.call({ m: 'callJoin', channelId, vdec: 'vp8' });
    expect(w.server.routes()).toEqual(['GET channel call', 'POST group', 'POST calls']);
    expect(w.slice('call')).toMatchObject({ phase: 'in', channelId });
  });

  it('after the instance closed an idle call group, the next join registers a fresh one', async () => {
    const w = await joined();
    await w.call({ m: 'callLeave', reason: 'user' });
    w.ds.closeCallGroup(w.callGroup);
    // An old group row of the channel this device still holds active (it joined, the instance closed it).
    const stale = idOf(0x9c, 200);
    w.model.seedGroup(stale, 1, COMMUNITY, VOICE);
    w.server.reset();
    await w.call(JOIN(w));
    const fresh = w.model.groups().filter((g) => g.kind === 1 && g.state === 2);
    expect(fresh).toHaveLength(1);
    expect(toHex(fresh[0].groupId)).not.toBe(toHex(w.callGroup));
    expect(toHex(fresh[0].groupId)).not.toBe(toHex(stale));
    expect(w.server.routes()).toEqual(['GET channel call', 'POST group', 'POST calls']);
    expect(w.slice('call').phase).toBe('in');
  });

  it('a POST calls answered 503 (the start raced the idle close) re-reads once, then registers afresh; no third attempt', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    w.server.answer('POST calls', 503, 'E_UNAVAILABLE', 5);
    // The close the 503 reports: L-HTTP-100 no longer names the group.
    const held = w.server.hold('POST calls');
    const id = w.send(JOIN(w));
    await held.reached;
    w.ds.closeCallGroup(w.callGroup);
    held.release();
    for (let i = 0; i < 40 && w.ret(id) === undefined; i++) await settle();
    expect(w.ret(id)).toMatchObject({ ok: true });
    expect(w.server.routes()).toEqual(['GET channel call', 'GET group info', 'GET group tree', 'POST resync', 'POST calls',
      'GET channel call', 'POST group', 'POST calls']);
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    expect(w.pushes('callKeys')).toHaveLength(1);
    // Twice: the second 503 fails the join.
    const v = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    v.server.answer('POST calls', 503, 'E_UNAVAILABLE', 5);
    v.server.answer('POST calls', 503, 'E_UNAVAILABLE', 5);
    v.server.reset();
    expect(await codeOf(v.call(JOIN(v)))).toBe('E_UNAVAILABLE');
    expect(v.server.routes().filter((r) => r === 'POST calls')).toHaveLength(2);
    expect(v.slice('call').phase).toBe('failed');
    expect(v.pushes('callKeys')).toHaveLength(0);
  });

  // Attacker: the instance (or a member) adds a forged device to the call group before this device joins. Nothing is
  // posted for that tree, and no state of the refused join is left behind.
  it('a block present at join time posts nothing and leaves no call group behind', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.model.addFailedLeaf(w.callGroup, 'E_DEVICE_UNLISTED');
    w.server.reset();
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_DEVICE_UNLISTED');
    expect(w.server.routes()).toEqual(['GET channel call', 'GET group info', 'GET group tree']);
    expect(w.model.group(w.callGroup)).toBeUndefined();
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', error: { code: 'E_DEVICE_UNLISTED' }, changedUser: null });
    expect(w.pushes('callKeys')).toHaveLength(0);
  });

  // Attacker: the instance adds a device whose user's list this device never read, hoping the pending verdict lets audio
  // reach it (RV-1). The list is read first; if it cannot be read, nothing is posted.
  it('a pending leaf is settled by a list refresh before the join commit is posted', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.model.setCallTree(w.callGroup, [{ leafIndex: 0, user: PEER.user, device: PEER.device }, { leafIndex: 1, user: THIRD.user, device: THIRD.device }]);
    w.server.reset();
    await w.call(JOIN(w));
    const routes = w.server.routes();
    const list = w.server.requests.findIndex((q) => q.route === 'GET device list' && toHex(q.args[0] as Id) === toHex(THIRD.user));
    expect(list).toBeGreaterThanOrEqual(0);
    expect(list).toBeGreaterThan(routes.indexOf('GET group tree'));
    expect(list).toBeLessThan(routes.indexOf('POST resync'));
    expect(w.slice('call').phase).toBe('in');

    const v = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    v.model.setCallTree(v.callGroup, [{ leafIndex: 0, user: PEER.user, device: PEER.device }, { leafIndex: 1, user: THIRD.user, device: THIRD.device }]);
    v.server.reset();
    v.server.answer('GET device list', 503);
    v.server.answer('GET device list', 503);   // bob's list too: the refresh reads every leaf of the group
    expect(await codeOf(v.call(JOIN(v)))).toBe('E_DEVICE_LIST_STALE');
    expect(v.slice('call')).toMatchObject({ phase: 'failed', error: { code: 'E_DEVICE_LIST_STALE' } });
    expect(v.server.routes()).not.toContain('POST resync');
    expect(v.model.group(v.callGroup)).toBeUndefined();
  });

  // Decision 4: a DM call is refused while the DM's text group is send-blocked; the refusal names the changed contact
  // (ruling I2), and nothing reaches the instance.
  it('a DM call refuses while the DM text group is send-blocked', async () => {
    const w = await harness({ dm: true });
    const text = w.dms[0].text;
    w.model.addPendingLeaf(text, 1, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
    w.model.changeIdentity(PEER.user);
    w.server.reset();
    expect(await codeOf(w.call({ m: 'callJoin', channelId: w.dm, vdec: '' }))).toBe('E_UMK_CHANGED');
    expect(w.server.routes()).toEqual([]);
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', error: { code: 'E_UMK_CHANGED' }, changedUser: toHex(PEER.user) });
  });

  it('refuses a second join while in a call, and a channel that is neither a voice channel nor a DM', async () => {
    const w = await joined();
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_CALL_ACTIVE');
    expect(await codeOf(w.call({ m: 'callJoin', channelId: 'ab'.repeat(16), vdec: '' }))).toBe('E_BAD_INPUT');
  });

  it('E_CALL_FULL leaves the joined group with a self-Remove and forgets it', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    w.server.answer('POST calls', 409, 'E_CALL_FULL');
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_CALL_FULL');
    expect(lastCallRoute(w)).toBe('POST proposal');
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    expect(w.slice('call')).toMatchObject({ phase: 'failed', error: { code: 'E_CALL_FULL' } });
    expect(w.pushes('callKeys')).toHaveLength(0);
  });
});

describe('keys after the join (L-TS-56 step 2)', () => {
  it('pushes callKeys once per changed epoch and leaves no key bytes in the worker after posting', async () => {
    const w = await joined();
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    const pushes = w.pushes('callKeys');
    expect(pushes).toHaveLength(1);
    expect(CALL_KEYS(pushes[0]).epoch).toBe((BigInt(w.result.epoch) + 1n).toString());
    expect(CALL_KEYS(pushes[0]).baseKey.byteLength).toBe(0);          // detached by the transfer
    expect(pushes[0].transfer).toEqual([CALL_KEYS(pushes[0]).baseKey]);
    expect(w.model.lastEpochBytes(w.callGroup).every((b) => b === 0)).toBe(true);
    expect(w.slice('call').epoch).toBe(CALL_KEYS(pushes[0]).epoch);
    // A batch that changes no epoch (an application message, skipped in a call group) and a trust refresh post nothing.
    w.ds.peerSend(w.callGroup, PEER, 'not media');
    await settled(w);
    await w.call({ m: 'refreshTrust' });
    await settled(w);
    expect(w.pushes('callKeys')).toHaveLength(1);
  });

  // Attacker: the instance adds a forged leaf mid-call. No key for the new epoch; the device leaves and says why.
  it('a blocking leaf after the join stops every later key and leaves', async () => {
    const w = await joined();
    // The leave's proposal is held in flight, so a later epoch arrives while this device is leaving.
    const held = w.server.hold('POST proposal');
    w.model.addFailedLeaf(w.callGroup, 'E_DEVICE_UNLISTED');
    await w.deliverHandshakes(w.callGroup);
    await held.reached;
    expect(w.pushes('callKeys')).toHaveLength(0);
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', error: { code: 'E_DEVICE_UNLISTED' } });
    expect(lastCallRoute(w)).toBe('POST proposal');
    // The forged leaf is swept and one more epoch arrives before the leave completes: a blocked device posts no key.
    w.model.setCallTree(w.callGroup, [{ leafIndex: 0, user: PEER.user, device: PEER.device }]);
    w.model.removeLeaf(w.callGroup, 2);
    await w.deliverHandshakes(w.callGroup);
    expect(w.pushes('callKeys')).toHaveLength(0);
    held.release();
    await settled(w);
    expect(w.pushes('callKeys')).toHaveLength(0);
    expect(w.slice('call').phase).toBe('blocked');
    expect(w.model.group(w.callGroup)?.state).toBe(4);
  });

  it('an unlisted leaf mid-call still leaves (only an identity change pauses)', async () => {
    const w = await joined();
    w.model.addFailedLeaf(w.callGroup, 'E_DEVICE_UNLISTED');
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', changedUser: null });
    expect(lastCallRoute(w)).toBe('POST proposal');
    expect(w.model.group(w.callGroup)?.state).toBe(4);
  });

  // RV-3 (Jonas). Attacker question: can a member trigger the pause against an honest party? Yes: any member whose
  // identity changes (honestly or not) pauses everyone who has not verified them, but only on their own devices and
  // without ending the call; no key reaches the changed device either way, which is the property.
  it('a mid-call identity change pauses without leaving and resumes after verification or when that member leaves', async () => {
    // A change a trust refresh finds within the current epoch pauses too; verified, the call resumes on the key the page
    // already holds: no key is posted while paused, nor again for that epoch.
    {
      const w = await joined();
      w.model.changeIdentity(PEER.user);
      await w.call({ m: 'refreshTrust' });
      await settled(w);
      expect(w.slice('call')).toMatchObject({ phase: 'paused', epoch: w.result.epoch, changedUser: toHex(PEER.user) });
      await w.call({ m: 'verifyContact', userId: toHex(PEER.user), umk: 'c4'.repeat(32) });
      await settled(w, 8);
      expect(w.slice('call')).toMatchObject({ phase: 'in', epoch: w.result.epoch, changedUser: null });
      expect(w.pushes('callKeys')).toHaveLength(0);
    }
    for (const resume of ['verify', 'leaves'] as const) {
      const w = await joined();
      w.gateway.push(voiceFrame(ME, VOICE, 1));
      const e = BigInt(w.result.epoch);
      w.model.changeIdentity(PEER.user);
      await w.deliverHandshakes(w.callGroup, THIRD);
      await settled(w);
      expect(w.pushes('callKeys')).toHaveLength(0);
      expect(w.slice('call')).toMatchObject({ phase: 'paused', error: { code: 'E_UMK_CHANGED' }, changedUser: toHex(PEER.user) });
      expect(w.server.routes()).not.toContain('POST proposal');
      expect(w.model.calls.some((c) => c.m === 'callGroupForget')).toBe(false);
      expect(w.slice('voice')[w.voice]?.participants.some((p) => p.own)).toBe(true);
      // While paused: no rotation commit, and no share.
      await w.clock.advance(3_600_000);
      await settled(w);
      expect(w.server.routes()).not.toContain('POST commit');
      expect(await codeOf(w.call({ m: 'callShare', on: true }))).toBe('E_UNAVAILABLE');
      expect(await codeOf(w.call({ m: 'callShare', on: false }))).toBe('ok');
      if (resume === 'verify') {
        await w.call({ m: 'verifyContact', userId: toHex(PEER.user), umk: 'c4'.repeat(32) });
        await settled(w, 8);
        expect(keysPosted(w).map((k) => k.epoch)).toEqual([(e + 1n).toString()]);
      } else {
        w.model.removeLeaf(w.callGroup, 0);
        await w.deliverHandshakes(w.callGroup, THIRD);
        await settled(w);
        expect(keysPosted(w).map((k) => k.epoch)).toEqual([(e + 2n).toString()]);
      }
      expect(w.slice('call')).toMatchObject({ phase: 'in', error: null, changedUser: null });
    }
  });

  // Product walk I8: removed while in voice. The own entry leaves the voice slice at once (no stale "in harbour" with
  // oneself), the channel's call is read again, and when the instance no longer lets this device see it (404, the
  // membership is gone) the voice entry goes and the removal says why (error E_NOT_FOUND): the page holds the join.
  it('a removal drops the own entry and, when the channel is gone for this device, says so', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    w.gateway.push(voiceFrame(PEER, VOICE, 1));
    expect(w.slice('voice')[w.voice]?.participants.map((p) => p.own)).toEqual(expect.arrayContaining([true]));
    w.ds.inject('getChannelCall', { fail: new DillaHttpError({ status: 404, code: 'E_NOT_FOUND', detail: '', retryAfterMs: null, extra: [] }) });
    w.ds.propose(w.callGroup, 'remove', ME.device);
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.slice('call')).toMatchObject({ phase: 'removed', channelId: w.voice, error: { code: 'E_NOT_FOUND' } });
    expect(w.slice('voice')[w.voice]).toBeUndefined();
  });

  it('a removal from a call still visible keeps the others and drops only the own entry', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    w.gateway.push(voiceFrame(PEER, VOICE, 1));
    // The read again is held: the own entry is gone before it answers, not only because of it.
    let release: () => void = () => undefined;
    w.ds.inject('getChannelCall', { gate: new Promise<void>((resolve) => { release = resolve; }) });
    w.ds.propose(w.callGroup, 'remove', ME.device);
    await w.deliverHandshakes(w.callGroup);
    await settle();
    expect(w.slice('call')).toMatchObject({ phase: 'removed', error: null });
    expect((w.slice('voice')[w.voice]?.participants ?? []).map((p) => p.own)).toEqual([false]);
    release();
    await settled(w);
    expect((w.slice('voice')[w.voice]?.participants ?? []).some((p) => p.own)).toBe(false);
  });

  it('the own leaf removed by another member sets phase removed', async () => {
    const w = await joined();
    w.ds.propose(w.callGroup, 'remove', ME.device);
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.slice('call').phase).toBe('removed');
    expect(w.pushes('callKeys')).toHaveLength(0);
  });
});

// web3.4 C4, review focus 2: removed from the server (kicked or banned) while in one of its calls. The server's re-read
// drops it and tells once; the call itself ends here at once through the existing removed phase (C4 fix round 1), not
// only when the own leaf's Remove lands.
describe('removed from the server while busy (web3.4 C4)', () => {
  const changed: Frame = { op: 54, n: 0n, groupId: null, payload: [COMMUNITY] as CborValue[] };
  const told = [{ id: 1, communityId: toHex(COMMUNITY), name: 'friends' }];
  async function removedFromServer(w: World): Promise<void> {
    LISTED.value = false;
    w.gateway.push(changed);
    await w.clock.advance(COMMUNITY_RELOAD_DEBOUNCE_MS);
    await settled(w);
  }
  const voiceAfterRemoval = (w: World): VoiceState[] => {
    const all = w.posted().map((p) => p.message);
    const at = all.findIndex((m) => m.t === 'slice' && m.name === 'communities' && (m.value as unknown[]).length === 0);
    return all.slice(at).filter((m): m is Extract<FromWorker, { t: 'slice' }> => m.t === 'slice' && m.name === 'voice').map((m) => m.value as VoiceState);
  };

  it('during a call: nothing throws, no voice state of the server is published, and the call ends as removed', async () => {
    const w = await joined();
    try {
      w.gateway.push(voiceFrame(ME, VOICE, 1));
      w.gateway.push(voiceFrame(PEER, VOICE, 1));
      expect(w.slice('voice')[w.voice]?.participants).toHaveLength(2);
      await removedFromServer(w);
      expect(w.slice('removals')).toEqual(told);
      expect(w.slice('communities')).toEqual([]);
      expect(w.slice('voice')[w.voice]).toBeUndefined();
      // The instance's frames for the call no longer land in the slice.
      w.gateway.push(voiceFrame(PEER, VOICE, 1));
      w.ds.propose(w.callGroup, 'remove', ME.device);
      await w.deliverHandshakes(w.callGroup);
      await settled(w);
      expect(w.slice('call')).toMatchObject({ phase: 'removed', channelId: w.voice });
      expect(voiceAfterRemoval(w).length).toBeGreaterThan(0);
      expect(voiceAfterRemoval(w).every((v) => !(w.voice in v))).toBe(true);
      expect(w.slice('removals')).toEqual(told);
    } finally {
      LISTED.value = true;
    }
  });

  it('while the engine commits an own Remove it was elected for (op 17): nothing throws', async () => {
    const w = await joined();
    try {
      w.ds.propose(w.callGroup, 'remove', ME.device);
      await settled(w);
      const held = w.server.hold('POST commit');
      w.ds.elect(w.callGroup, ME.device, 1n);
      await held.reached;
      await removedFromServer(w);
      expect(w.slice('removals')).toEqual(told);
      held.release();
      await settled(w, 8);
      expect(w.slice('removals')).toEqual(told);
      expect(w.slice('communities')).toEqual([]);
      expect(await w.call({ m: 'refreshCommunities' })).toBeNull();
      // Fix 7: the call ended as removed, and the group reached state 4 (the engine's commit first, then the forget).
      expect(w.slice('call')).toMatchObject({ phase: 'removed', channelId: w.voice });
      expect(w.model.group(w.callGroup)?.state).toBe(4);
      expect(w.pushes('callKeys')).toHaveLength(0);
    } finally {
      LISTED.value = true;
    }
  });

  it('fix 5: the call of a dropped server ends here at once: removed, its group forgotten, no rotation, no key', async () => {
    const w = await joined();
    try {
      w.gateway.push(voiceFrame(PEER, VOICE, 1));
      await removedFromServer(w);
      // No Remove commit has landed: the drop alone ends the call.
      expect(w.slice('call')).toMatchObject({ phase: 'removed', channelId: w.voice, callId: null, epoch: null, roster: [],
        error: { code: 'E_NOT_FOUND' } });
      expect(w.slice('voice')[w.voice]).toBeUndefined();
      expect(w.model.calls.some((c) => c.m === 'callGroupForget')).toBe(true);
      expect(w.model.group(w.callGroup)?.state).toBe(4);
      w.server.reset();
      await w.clock.advance(CALL_ROTATE_MS);
      await settled(w);
      expect(w.server.routes()).not.toContain('POST commit');
      expect(w.pushes('callKeys')).toHaveLength(0);
      // The page's leave after it keeps the removed phase.
      expect(await w.call({ m: 'callLeave', reason: 'user' })).toBeNull();
      expect(w.slice('call').phase).toBe('removed');
    } finally {
      LISTED.value = true;
    }
  });

  it('fix 6: a rejoin does not bring back the participants the voice book held before the drop', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    try {
      w.gateway.push(voiceFrame(THIRD, VOICE, 1));
      expect(w.slice('voice')[w.voice]?.participants.map((p) => p.name)).toEqual(['carol']);
      await removedFromServer(w);
      expect(w.slice('voice')[w.voice]).toBeUndefined();
      LISTED.value = true;
      expect(await w.call({ m: 'refreshCommunities' })).toBeNull();
      expect(await w.call({ m: 'selectCommunity', communityId: toHex(COMMUNITY) })).toBeNull();
      await settled(w);
      w.gateway.push(voiceFrame(PEER, VOICE, 1));
      expect(w.slice('voice')[w.voice]?.participants.map((p) => p.name)).toEqual(['bob']);
    } finally {
      LISTED.value = true;
    }
  });

  it('round 2: a join in flight into a dropped server is cancelled: removed, no key, no live call, no rotation', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    try {
      const held = w.server.hold('POST calls');
      const id = w.send(JOIN(w));
      await held.reached;
      await removedFromServer(w);
      held.release();
      await settled(w, 8);
      expect(w.ret(id)).toMatchObject({ ok: false });
      expect(w.pushes('callKeys')).toHaveLength(0);
      expect((w.controller as unknown as { calls: { live: unknown } }).calls.live).toBeNull();
      expect(w.slice('call')).toMatchObject({ phase: 'removed', channelId: w.voice, error: { code: 'E_NOT_FOUND' } });
      w.server.reset();
      await w.clock.advance(CALL_ROTATE_MS);
      await settled(w);
      expect(w.server.routes()).not.toContain('POST commit');
    } finally {
      LISTED.value = true;
    }
  });
});

describe('leave and rotation (L-TS-56 steps 4 and 5)', () => {
  it('callLeave posts the self-Remove, forgets the group and goes idle', async () => {
    const w = await joined();
    expect(await w.call({ m: 'callLeave', reason: 'user' })).toBeNull();
    expect(w.server.routes()).toEqual(['POST proposal']);
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    expect(w.slice('call')).toEqual(expect.objectContaining({ phase: 'idle', callId: null, epoch: null, code: null, roster: [] }));
    // A leave already pending at the instance is fine (409 E_REMOVE_PENDING), and a leave with no call is a no-op.
    expect(await w.call({ m: 'callLeave', reason: 'pagehide' })).toBeNull();
  });

  it('rotates the leaf after CALL_ROTATE_MS and retries after a conflict at the next tick', async () => {
    const w = await joined();
    await w.clock.advance(3_600_000);
    await settled(w);
    expect(w.server.routes()).toContain('POST commit');
    expect(w.model.calls.filter((c) => c.m === 'commitConfirm' && c.g === toHex(w.callGroup))).toHaveLength(1);
    expect(keysPosted(w)).toHaveLength(1);                                  // the rotation's own new epoch
    w.server.reset();
    w.server.answer('POST commit', 409, 'E_COMMIT_CONFLICT');
    await w.clock.advance(3_600_000);
    await settled(w);
    expect(w.model.calls.filter((c) => c.m === 'commitAbort' && c.g === toHex(w.callGroup))).toHaveLength(1);
    await w.clock.advance(60_000);
    await settled(w);
    expect(w.server.routes().filter((r) => r === 'POST commit')).toHaveLength(2);
  });

  // Progress note of task 5: call_rotate cancels a pending leave, so a worker that has posted its self-Remove must
  // never rotate (it forgets instead).
  it('never rotates after callLeave has posted its self-Remove', async () => {
    const w = await joined();
    const held = w.server.hold('POST proposal');
    const id = w.send({ m: 'callLeave', reason: 'user' });
    await held.reached;
    await w.clock.advance(3_600_000);
    await settled(w);
    expect(w.model.calls.some((c) => c.m === 'callRotate')).toBe(false);
    held.release();
    for (let i = 0; i < 20 && w.ret(id) === undefined; i++) await settle();
    await w.clock.advance(7_200_000);
    await settled(w);
    expect(w.model.calls.some((c) => c.m === 'callRotate')).toBe(false);
    expect(w.server.routes()).not.toContain('POST commit');
  });

  it('uses only the typed WSS path for call stats and drops an unavailable sample', async () => {
    const w = await joined();
    w.statsTransport.owned = true;
    await w.call({ m: 'callStats', report: [1,null,40,0,0,10] });
    expect(w.statsTransport.sent).toEqual([{ callId: w.result.callId, report: [1,null,40,0,0,10] }]);
    expect(w.server.routes()).not.toContain('POST stats');
    w.statsTransport.owned = false;
    await w.call({ m: 'callStats', report: [1,null,50,0,0,10] });
    expect(w.server.routes()).not.toContain('POST stats');
    expect(w.statsTransport.sent).toHaveLength(1);
  });

  it('share and refresh use their routes; coarse stats stay on WSS; no call is E_NO_CALL', async () => {
    const w = await joined();
    await w.call({ m: 'callShare', on: true });
    await w.call({ m: 'callShare', on: false });
    const fresh = await w.call({ m: 'callRefresh' }) as { token: string; iceServers: unknown[] };
    expect(fresh).toEqual({ token: expect.any(String) as string, iceServers: expect.any(Array) as unknown[] });
    w.statsTransport.owned = true;
    expect(await w.call({ m: 'callStats', report: [1, null, 40, 0, 0, 10] })).toBeNull();
    await w.call({ m: 'callStats', report: [1, null, 40, 0, 0, 10] });
    expect(w.server.routes()).toEqual(['POST share', 'DELETE share', 'POST calls']);
    expect(w.statsTransport.sent).toHaveLength(2);
    await w.call({ m: 'callLeave', reason: 'user' });
    for (const command of [{ m: 'callShare', on: true }, { m: 'callRefresh' }, { m: 'callStats', report: [0, null, 0, 0, 0, 0] }]) {
      expect(await codeOf(w.call(command))).toBe('E_NO_CALL');
    }
  });

  // Task 11 (8c minor): a refused RTT report says so, so the page does not count it as sent.
  it('callRtt answers true once stored and false on a 429 or a 404, never throwing for those', async () => {
    const w = await joined();
    expect(await w.call({ m: 'callRtt', ms: 24 })).toBe(true);
    w.server.answer('POST rtt', 429, 'E_RATE_LIMITED', 4000);
    expect(await w.call({ m: 'callRtt', ms: 31 })).toBe(false);
    w.server.answer('POST rtt', 404, 'E_NOT_FOUND', 0);
    expect(await w.call({ m: 'callRtt', ms: null })).toBe(false);
    expect(await w.call({ m: 'callRtt', ms: null })).toBe(true);
    expect(w.server.routes().filter((r) => r === 'POST rtt')).toHaveLength(4);
  });

  it('resign leaves the call and clears rings before it releases the store', async () => {
    const w = await joined({ dm: true });
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toHaveLength(1);
    await w.call({ m: 'resign', nonce: 'ab'.repeat(16) });
    await settled(w);
    expect(w.server.routes()).toContain('POST proposal');
    const posted = w.posted().map((p) => p.message);
    const released = posted.findIndex((m) => m.t === 'released');
    expect(released).toBeGreaterThan(0);
    const before = posted.slice(0, released);
    const lastCall = before.filter((m): m is Extract<FromWorker, { t: 'slice' }> => m.t === 'slice' && m.name === 'call').at(-1);
    const lastRings = before.filter((m): m is Extract<FromWorker, { t: 'slice' }> => m.t === 'slice' && m.name === 'rings').at(-1);
    expect((lastCall?.value as CallState).phase).toBe('idle');
    expect(lastRings?.value).toEqual([]);
    const proposal = w.server.routes().indexOf('POST proposal');
    expect(proposal).toBeGreaterThanOrEqual(0);
  });

  // Task 3 review (controller ruling): the instance keeps a device's own mute/deafen flags only once it is in the call's
  // room, so a voice_self sent before the join webhook is lost. The worker re-sends the last one once per join, when it
  // sees its own voice_state join for that call.
  it('re-sends the own voice_self once when its own voice_state join arrives, and once more per later join', async () => {
    const w = await joined();
    await w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 });
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 1 }]);
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 1 }, { callId: w.voice, flags: 1 }]);
    w.gateway.push(voiceFrame(ME, VOICE, 3));
    w.gateway.push(voiceFrame(PEER, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(2);
    w.gateway.push(voiceFrame(ME, VOICE, 0));
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(3);
  });

  // web3.8 VM: the instance refuses a voice_self that reaches it before the SFU lists the device, and the join transition
  // may already have passed by then. While the own voice_state for that call lacks the self bits the last voice_self asked
  // for, the worker sends it again: a few times per call, only while online.
  it('a refused early voice_self is sent again when the own voice_state arrives without its bits', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    await w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 });
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 1 }]);
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 1 }, { callId: w.voice, flags: 1 }]);
    // Another call's own voice_state, and a peer's in this call, never send it.
    w.gateway.push(voiceFrame(ME, DM, 1));
    w.gateway.push(voiceFrame(PEER, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(2);
  });

  it('no voice_self is sent again once the own voice_state carries its bits', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    await w.call({ m: 'voiceSelf', callId: w.voice, flags: 3 });
    // voice_state flags: bit 0 in_call, bit 1 self_mute, bit 2 self_deaf.
    w.gateway.push(voiceFrame(ME, VOICE, 1 | 2 | 4));
    w.gateway.push(voiceFrame(ME, VOICE, 1 | 2 | 4));
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 3 }]);
    // Bits that differ (only muted, not deafened) are sent again.
    w.gateway.push(voiceFrame(ME, VOICE, 1 | 2));
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 3 }, { callId: w.voice, flags: 3 }]);
  });

  it('a server that never takes the voice_self gets it at most three more times per call, and never while offline', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    await w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 });
    for (let i = 0; i < 10; i++) w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(1 + 3);
    // A new join of the call starts a new count.
    w.gateway.push(voiceFrame(ME, VOICE, 0));
    for (let i = 0; i < 10; i++) w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(1 + 3 + 3);
    // Offline nothing is sent, and nothing is spent.
    const v = await joined();
    v.gateway.push(voiceFrame(ME, VOICE, 1));
    await v.call({ m: 'voiceSelf', callId: v.voice, flags: 1 });
    v.offline();
    for (let i = 0; i < 5; i++) v.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(v.voiceSelfSent).toHaveLength(1);
    v.online();
    for (let i = 0; i < 10; i++) v.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(v.voiceSelfSent).toHaveLength(1 + 3);
  });

  // web3.8 MUTE: the worker never infers a voice_self from voice.self. The page's start reads the setting (and again before
  // it publishes, and at connected); its attach reconcile sends the voice_self, and the re-send above delivers it. Page and
  // worker therefore never disagree for a call.
  it('a mute pressed while the join is starting sends nothing by itself: the page’s voice_self is what is sent and re-sent', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    const held = w.server.hold('POST calls');
    const id = w.send(JOIN(w));
    await held.reached;
    await w.call({ m: 'setSetting', key: 'voice.self', value: 'muted' });
    held.release();
    for (let i = 0; i < 40 && w.ret(id) === undefined; i++) await settle();
    expect(w.ret(id)).toMatchObject({ ok: true });
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toEqual([]);
    // The page's reconcile says muted; the instance has not taken it yet, so the next own voice_state brings it again.
    await w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 });
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    w.gateway.push(voiceFrame(ME, VOICE, 1 | 2));
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 1 }, { callId: w.voice, flags: 1 }]);
  });

  it('a mute then an unmute around the in_call frame: nothing the page did not say reaches the others', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    const held = w.server.hold('POST calls');
    const id = w.send(JOIN(w));
    await held.reached;
    await w.call({ m: 'setSetting', key: 'voice.self', value: 'muted' });
    held.release();
    for (let i = 0; i < 40 && w.ret(id) === undefined; i++) await settle();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    await w.call({ m: 'setSetting', key: 'voice.self', value: null });
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toEqual([]);
  });

  it('a setting that did not change during the join sends nothing of its own', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    await w.call({ m: 'setSetting', key: 'voice.self', value: 'muted' });
    await w.call(JOIN(w));
    await settled(w);
    // The page started muted and says so itself; the worker adds nothing for a setting.
    w.gateway.push(voiceFrame(ME, VOICE, 1 | 2));
    expect(w.voiceSelfSent).toEqual([]);
  });

  it('a voice_self made while offline is kept, and sent again when the own voice_state after the reconnect lacks it', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    w.offline();
    await expect(w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 })).rejects.toMatchObject({ code: 'E_NOT_READY' });
    expect(w.voiceSelfSent).toEqual([]);
    w.online();
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 1 }]);
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toEqual([{ callId: w.voice, flags: 1 }, { callId: w.voice, flags: 1 }]);
    w.gateway.push(voiceFrame(ME, VOICE, 1 | 2));
    expect(w.voiceSelfSent).toHaveLength(2);
  });

  it('a voice_self made while offline is sent by the reconnect itself, once, from its own budget', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    // The normal re-send budget is spent by a server that never takes an earlier voice_self.
    await w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 });
    for (let i = 0; i < 5; i++) w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(1 + 3);
    w.offline();
    await expect(w.call({ m: 'voiceSelf', callId: w.voice, flags: 3 })).rejects.toMatchObject({ code: 'E_NOT_READY' });
    w.online();
    // No own voice_state arrives after the reconnect: the reconnect sends it anyway.
    expect(w.voiceSelfSent).toHaveLength(5);
    expect(w.voiceSelfSent[4]).toEqual({ callId: w.voice, flags: 3 });
    // Once: another reconnect sends nothing more.
    w.offline();
    w.online();
    expect(w.voiceSelfSent).toHaveLength(5);
  });

  it('a voice_self made while offline for a call that ended meanwhile is not sent on the reconnect', async () => {
    const w = await joined();
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    w.offline();
    await expect(w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 })).rejects.toMatchObject({ code: 'E_NOT_READY' });
    await w.call({ m: 'callLeave', reason: 'user' }).catch(() => undefined);
    for (let i = 0; i < 40; i++) await settle();
    w.online();
    expect(w.voiceSelfSent).toEqual([]);
  });
});

describe('rings (L-TS-56 step 8)', () => {
  it('a ring for a DM shows once, busy while in a call, quiet when the DM is muted, and ends on state 0', async () => {
    const w = await harness({ dm: true });
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toEqual([expect.objectContaining({ callId: w.dm, busy: false, quiet: false })]);
    expect(w.slice('rings')[0]).toMatchObject({ channelId: w.dm, callerUser: toHex(PEER.user), callerName: 'bob', ringTs: 1_700_000_030 });
    w.gateway.push(ring(0));
    expect(w.slice('rings')).toEqual([]);
    await w.call({ m: 'setSetting', key: `mute.channel.${w.dm}`, value: '1' });
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toEqual([expect.objectContaining({ quiet: true, busy: false })]);
    w.gateway.push(ring(0));
    // In a call elsewhere, a ring is busy.
    const v = await joined({ dm: true });
    v.gateway.push(ring(1));
    expect(v.slice('rings')).toEqual([expect.objectContaining({ busy: true })]);
  });

  // Attacker: a contact whose identity changed calls; the ring must not tempt a join the core would refuse.
  // (Can the attacker turn this refusal on an honest party? Only the instance could, by poisoning the DM's text group,
  // which already pauses that DM's composer; it can withhold op 53 anyway.)
  it('no ring entry at all for a DM whose text or call group is send-blocked', async () => {
    const w = await harness({ dm: true });
    const text = w.dms[0].text;
    w.model.blockGroup(text, 'E_UMK_CHANGED');
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toEqual([]);
    w.model.unblockGroup(text);
    const dmCall = idOf(0x9e, 3);
    w.model.seedGroup(dmCall, 1, null, DM);
    w.model.blockGroup(dmCall, 'E_DEVICE_UNLISTED');
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toEqual([]);
    // The control: unblocked, the same ring shows.
    w.model.unblockGroup(dmCall);
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toHaveLength(1);
  });

  // Attacker: the instance rings in the name of a channel the person does not share with the caller, or as themself.
  it('drops a ring whose caller is not another participant of a known DM, and one for an unknown channel', async () => {
    const w = await harness({ dm: true });
    w.gateway.push(ring(1, { user: THIRD }));
    w.gateway.push(ring(1, { user: ME }));
    w.gateway.push(ring(1, { channel: idOf(0x77, 7) }));
    expect(w.slice('rings')).toEqual([]);
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toHaveLength(1);
  });

  it('keeps one ring per channel and at most eight in total', async () => {
    const w = await harness({ dm: true, extraDms: 9 });
    w.gateway.push(ring(1, { expires: 100 }));
    w.gateway.push(ring(1, { expires: 130 }));
    expect(w.slice('rings')).toEqual([expect.objectContaining({ channelId: w.dm, ringTs: 130 })]);
    for (const dm of w.dms.slice(1)) w.gateway.push(ring(1, { channel: dm.channel, user: dm.members[0] }));
    expect(w.slice('rings')).toHaveLength(8);
    expect(w.slice('rings').map((r) => r.channelId)).not.toContain(toHex(w.dms.at(-1)!.channel));
  });

  it('callDecline posts the decline with the ring timestamp and drops the ring even when the answer is 404', async () => {
    const w = await harness({ dm: true });
    w.gateway.push(ring(1, { expires: 130 }));
    w.server.reset();
    w.server.answer('POST decline', 404);
    expect(await w.call({ m: 'callDecline', callId: w.dm, ringTs: 130 })).toBeNull();
    expect(w.server.requests).toEqual([{ route: 'POST decline', args: [DM, 130] }]);
    expect(w.slice('rings')).toEqual([]);
  });

  it('an outgoing DM call lists who is ringing and appends decliners from state 2', async () => {
    const w = await harness({ dm: 'group' });
    await w.call({ m: 'callJoin', channelId: w.dm, vdec: '' });
    expect(w.slice('call').outgoing).toEqual({ ringing: ['bob', 'carol'], declined: [], notRung: [] });
    w.gateway.push(voiceFrame(THIRD, DM, 1));
    expect(w.slice('call').outgoing).toEqual({ ringing: ['bob'], declined: [], notRung: [] });
    w.gateway.push(ring(2, { user: PEER }));
    expect(w.slice('call').outgoing).toEqual({ ringing: [], declined: ['bob'], notRung: [] });
  });

  // WB product walk I6: the instance answers whom the start rang; a participant its pair rule held back is never listed
  // as ringing, but as not rung, so the caller's strip says so instead of "ringing".
  it('an outgoing DM call lists only the participants the instance rang', async () => {
    const w = await harness({ dm: 'group' });
    w.ds.ringHeld.add(toHex(PEER.user));
    await w.call({ m: 'callJoin', channelId: w.dm, vdec: '' });
    expect(w.slice('call').outgoing).toEqual({ ringing: ['carol'], declined: [], notRung: ['bob'] });
    // bob joins after all (from the DM's call button): he is no longer "not rung".
    w.gateway.push(voiceFrame(PEER, DM, 1));
    expect(w.slice('call').outgoing).toEqual({ ringing: ['carol'], declined: [], notRung: [] });
  });

  it('a ring expires locally at expiresAt when no state 0 arrives', async () => {
    const w = await harness({ dm: true });
    const receivedAt = w.clock.now();
    // A lying instance: five minutes. The ring ends 30 s after it arrived.
    w.gateway.push(ring(1, { expires: Math.floor(receivedAt / 1000) + 300 }));
    expect(w.slice('rings')[0]?.expiresAt).toBe(receivedAt + 30_000);
    await w.clock.advance(29_999);
    expect(w.slice('rings')).toHaveLength(1);
    await w.clock.advance(1);
    expect(w.slice('rings')).toEqual([]);
  });

  // Web-3 task 10, concern 5: expires_ts is the instance's clock, not the page's. A page clock 60 s ahead of the instance
  // (the ring's expires_ts already past on the page) still rings for 30 s from the frame's arrival; the decline still
  // echoes the instance's expires_ts.
  it('a ring lasts 30 s from its arrival whatever the page clock says of its expires_ts', async () => {
    const w = await harness({ dm: true });
    const receivedAt = w.clock.now();
    const expires = Math.floor(receivedAt / 1000) - 30;
    w.gateway.push(ring(1, { expires }));
    expect(w.slice('rings')[0]).toMatchObject({ ringTs: expires, expiresAt: receivedAt + 30_000 });
    await w.clock.advance(29_999);
    expect(w.slice('rings')).toHaveLength(1);
    await w.clock.advance(1);
    expect(w.slice('rings')).toEqual([]);
  });

  // f10app m8: a repeated state 1 of the same ring (the same call and expires_ts) is that ring again, not a new 30 s.
  it('a repeated ring frame with the same expires_ts keeps the ring\'s first 30 s; a new expires_ts starts its own', async () => {
    const w = await harness({ dm: true });
    const receivedAt = w.clock.now();
    w.gateway.push(ring(1, { expires: 1_700_000_030 }));
    await w.clock.advance(20_000);
    w.gateway.push(ring(1, { expires: 1_700_000_030 }));
    expect(w.slice('rings')[0]).toMatchObject({ expiresAt: receivedAt + 30_000 });
    await w.clock.advance(10_000);
    expect(w.slice('rings')).toEqual([]);
    // A later ring of the same DM (another expires_ts) is a new ring, with its own 30 s.
    w.gateway.push(ring(1, { expires: 1_700_000_090 }));
    await w.clock.advance(20_000);
    w.gateway.push(ring(1, { expires: 1_700_000_120 }));
    await w.clock.advance(10_000);
    expect(w.slice('rings')).toHaveLength(1);
  });

  it('joining the DM call clears its ring', async () => {
    const w = await harness({ dm: true });
    w.gateway.push(ring(1));
    await w.call({ m: 'callJoin', channelId: w.dm, vdec: '' });
    expect(w.slice('rings')).toEqual([]);
  });
});

// ---------------------------------------------------------------------------------------------
// Task 6 review, fix round 1 (task-6-review.md I1, M1-M6).
// ---------------------------------------------------------------------------------------------

const waitRet = async (w: World, id: number): Promise<void> => {
  for (let i = 0; i < 60 && w.ret(id) === undefined; i++) { await settle(); await w.clock.advance(0); }
};
const keyZeroed = (w: World): boolean => {
  const m = w.model.lastMedia(w.callGroup);
  return m !== undefined && m.baseKey.byteLength === 16 && new Uint8Array(m.baseKey).every((b) => b === 0);
};

// I1. Attacker question: none (a race of the person's own commands). The property: after a leave, a sign-out, a forget or
// a revocation, no key reaches the page, the joined key is zero-filled in the worker, and the device does not stay a leaf
// of the call group without its self-Remove (except a revoked device, which can send nothing and forgets locally).
// Web-3 task 10, concern 2: a quick rejoin while this device's own Remove is outstanding. The instance keeps R25's guard 1
// whole (a Remove's target may not resync), so the client ends a call it is alone in as it leaves, and a rejoin refused
// with 409 E_REMOVE_PENDING waits, bounded, until another member has committed the Remove or the call has ended.
describe('a rejoin while the own Remove is outstanding (task 10, concern 2, and its fix round)', () => {
  /** Runs the clock in steps until the command answers or `ms` has passed. */
  async function runFor(w: World, id: number, ms: number, step = 1_000): Promise<void> {
    for (let t = 0; t < ms && w.ret(id) === undefined; t += step) { await settle(); await w.clock.advance(step); }
    await settle();
  }
  const resyncs = (w: World): number => w.server.routes().filter((r) => r === 'POST resync').length;

  it('a person alone in the call ends it as they leave (the conditional end), and the rejoin registers a fresh call group at once', async () => {
    const w = await harness({ voiceChannel: true });
    const first = await w.call(JOIN(w)) as CallJoinResult;
    await settled(w);
    w.server.reset();
    await w.call({ m: 'callLeave', reason: 'user' });
    expect(w.server.routes()).toContain('DELETE call');
    const again = await w.call(JOIN(w)) as CallJoinResult;
    expect(again.groupId).not.toBe(first.groupId);
    expect(w.slice('call')).toMatchObject({ phase: 'in' });
  });

  it('a leave from a call whose keyed roster holds another device does not end it', async () => {
    const w = await joined();
    await w.call({ m: 'callLeave', reason: 'user' });
    expect(w.server.routes()).not.toContain('DELETE call');
  });

  // m4: the room-snapshot branch of alone(): the roster keyed here holds only this device, but the room shows another.
  it('a leave whose roster holds only this device but whose room shows another does not end the call', async () => {
    const w = await harness({ voiceChannel: true });
    await w.call(JOIN(w));
    await settled(w);
    w.ds.voice(VOICE, PEER, 1);
    await settled(w);
    w.server.reset();
    await w.call({ m: 'callLeave', reason: 'user' });
    expect(w.server.routes()).toContain('GET channel call');
    expect(w.server.routes()).not.toContain('DELETE call');
  });

  // I1: a device whose external commit has landed, not yet keyed here nor in the room: the instance keeps the call
  // (409 E_CALL_NOT_ALONE) and this device just leaves.
  it('a leave beside a device that has just joined sends only the conditional end, and the call goes on', async () => {
    const w = await harness({ voiceChannel: true });
    const first = await w.call(JOIN(w)) as CallJoinResult;
    await settled(w);
    w.ds.join(fromHex(first.groupId), PEER.device);
    w.server.reset();
    await w.call({ m: 'callLeave', reason: 'user' });
    expect(w.server.routes()).toContain('DELETE call');
    expect(w.slice('call')).toMatchObject({ phase: 'idle' });
    const still = await w.ds.routesFor(PEER.device).getChannelCall(VOICE);
    expect(still.callId).not.toBeNull();
    expect(toHex(still.groupId ?? new Uint8Array(16))).toBe(first.groupId);
  });

  // m7: a blocked leave by a person alone ends the call too (still a current leaf; ending exposes nothing).
  it('a blocked leave by a person alone ends the call with the conditional end', async () => {
    const w = await harness({ voiceChannel: true });
    const first = await w.call(JOIN(w)) as CallJoinResult;
    await settled(w);
    w.server.reset();
    w.model.addFailedLeaf(fromHex(first.groupId), 'E_DEVICE_UNLISTED');
    await w.deliverHandshakes(fromHex(first.groupId));
    await settled(w);
    expect(w.slice('call')).toMatchObject({ phase: 'blocked' });
    expect(w.server.routes()).toContain('DELETE call');
    expect((await w.ds.routesFor(PEER.device).getChannelCall(VOICE)).callId).toBeNull();
  });

  it('a rejoin refused because its own Remove stands (409 E_REMOVE_PENDING) waits, says so, and joins once the Remove is committed', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.removeTarget.add(toHex(ME.device));
    const id = w.send(JOIN(w));
    await runFor(w, id, 7_000);
    expect(w.ret(id)).toBeUndefined();
    expect(w.slice('call')).toMatchObject({ phase: 'joining', rejoining: true });
    // Nothing changed at the instance: the join is not tried again, and each step is two reads (at most one a second).
    expect(resyncs(w)).toBe(1);
    expect(w.server.routes().filter((r) => r === 'GET channel call' || r === 'GET group info').length).toBeLessThanOrEqual(2 + 7);
    // Bob commits the Remove: the group moves on, and the join is tried again and lands.
    w.ds.removeTarget.delete(toHex(ME.device));
    w.ds.peerCommit(w.callGroup, PEER);
    await runFor(w, id, 6_000);
    expect(w.ret(id)).toMatchObject({ ok: true });
    expect(resyncs(w)).toBe(2);
    expect(w.slice('call')).toMatchObject({ phase: 'in', rejoining: false });
  });

  // The retry on a moved group is for a stale commit only (422 E_COMMIT_INVALID, 409 E_COMMIT_CONFLICT). Any other
  // refusal keeps its outcome even when a peer commit moved the group at the same time: one resync, no rebuild.
  it.each([
    ['409 E_REMOVE_PENDING', 409, 'E_REMOVE_PENDING', 'waits'],
    ['403 E_FORBIDDEN', 403, 'E_FORBIDDEN', 'fails'],
    ['422 that is not a stale commit', 422, 'E_INVALID_REQUEST', 'fails'],
  ] as const)('a %s while a peer commit moves the group is not retried as a stale commit', async (_name, status, code, outcome) => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    const held = w.server.hold('POST resync');
    w.server.answer('POST resync', status, code);
    const id = w.send(JOIN(w));
    await held.reached;
    w.ds.peerCommit(w.callGroup, PEER);
    held.release();
    await runFor(w, id, 1_000);
    expect(resyncs(w)).toBe(1);
    if (outcome === 'waits') {
      expect(w.ret(id)).toBeUndefined();
      expect(w.slice('call')).toMatchObject({ phase: 'joining', rejoining: true });
      await runFor(w, id, 6_000);   // the wait sees the moved group and the join lands
      expect(w.ret(id)).toMatchObject({ ok: true });
    } else {
      expect(w.ret(id)).toMatchObject({ ok: false, error: { code } });
      expect(w.slice('call')).toMatchObject({ phase: 'failed' });
    }
  });

  it('any other refusal of the rejoin fails at once (a 403 is not waited out)', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.denyJoin.add(toHex(ME.device));
    const id = w.send(JOIN(w));
    await runFor(w, id, 1_000);
    expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN' } });
    expect(resyncs(w)).toBe(1);
    expect(w.slice('call')).toMatchObject({ phase: 'failed', rejoining: false });
  });

  it('the wait is bounded by 30 s of elapsed time: still waiting at 29 s, failed by 31 s', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.removeTarget.add(toHex(ME.device));
    const id = w.send(JOIN(w));
    await runFor(w, id, 29_000);
    expect(w.ret(id)).toBeUndefined();
    expect(w.slice('call')).toMatchObject({ phase: 'joining' });
    await runFor(w, id, 2_000);
    expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_REMOVE_PENDING' } });
    expect(w.slice('call')).toMatchObject({ phase: 'failed' });
  });

  it('the bound is elapsed time, not a count of steps: slow reads do not stretch it', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.removeTarget.add(toHex(ME.device));
    const id = w.send(JOIN(w));
    await runFor(w, id, 3_000);
    // Each later call read takes 4 s of the worker's clock.
    for (let i = 0; i < 20; i++) w.ds.inject('getChannelCall', { before: () => { void w.clock.advance(4_000); } });
    await runFor(w, id, 31_000);
    expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_REMOVE_PENDING' } });
    expect(w.server.routes().filter((r) => r === 'GET channel call').length).toBeLessThan(10);
  });

  it('a leave during the wait stops it: no read and no resync after the leave', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.removeTarget.add(toHex(ME.device));
    const id = w.send(JOIN(w));
    await runFor(w, id, 3_000);
    expect(w.slice('call')).toMatchObject({ phase: 'joining', rejoining: true });
    const leave = w.send({ m: 'callLeave', reason: 'user' });
    await settle();
    const before = w.server.routes().length;
    await runFor(w, id, 10_000);
    expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_NO_CALL' } });
    expect(w.ret(leave)).toMatchObject({ ok: true });
    expect(w.server.routes().slice(before).filter((r) => r === 'GET channel call' || r === 'GET group info' || r === 'POST resync')).toEqual([]);
    expect(w.slice('call')).toMatchObject({ phase: 'idle', rejoining: false });
  });
});
describe('a join cancelled while it runs (review I1)', () => {
  async function joining(): Promise<{ w: World; join: number; release: () => void }> {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    const held = w.server.hold('POST calls');
    const join = w.send(JOIN(w));
    await held.reached;
    return { w, join, release: held.release };
  }
  async function cancelledBy(w: World, join: number, release: () => void, entry: number | null, proposal: boolean): Promise<void> {
    release();
    await waitRet(w, join);
    if (entry !== null) await waitRet(w, entry);
    await settled(w);
    expect(w.ret(join)).toMatchObject({ ok: false, error: { code: 'E_NO_CALL' } });
    expect(w.pushes('callKeys')).toHaveLength(0);
    expect(keyZeroed(w)).toBe(true);
    expect(w.server.routes().includes('POST proposal')).toBe(proposal);
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    expect(w.slice('call').phase).toBe('idle');
    // Nothing is armed for the call it left: an hour later no rotation runs.
    await w.clock.advance(3_600_000);
    await settled(w);
    expect(w.server.routes()).not.toContain('POST commit');
  }

  it('callLeave while joining answers after the self-Remove, and the join posts no key', async () => {
    const { w, join, release } = await joining();
    const leave = w.send({ m: 'callLeave', reason: 'user' });
    await settled(w);
    expect(w.ret(leave)).toBeUndefined();                 // it waits for the join's teardown
    await cancelledBy(w, join, release, leave, true);
    expect(w.ret(leave)).toMatchObject({ ok: true, value: null });
    expect(w.server.routes().indexOf('POST proposal')).toBeGreaterThan(w.server.routes().indexOf('POST calls'));
  });

  it('forgetBrowser while joining sends the self-Remove first', async () => {
    const { w, join, release } = await joining();
    const forget = w.send({ m: 'forgetBrowser' });
    await settled(w);
    await cancelledBy(w, join, release, forget, true);
  });

  it('signOutRevoke while joining sends the self-Remove first', async () => {
    const { w, join, release } = await joining();
    const out = w.send({ m: 'signOutRevoke', recoveryKey: 'QRST'.repeat(13) });
    await settled(w);
    await cancelledBy(w, join, release, out, true);
  });

  it('a revoked socket while joining forgets the group locally and sends nothing (reset)', async () => {
    const { w, join, release } = await joining();
    w.sessionState.ok = false;
    w.revokeSocket();
    await settled(w);
    expect((w.slice('account') as { phase: string }).phase).toBe('revoked');
    await cancelledBy(w, join, release, null, false);
  });
});

describe('the rotation and the leave share the group lane (review M1, M7)', () => {
  it('a leave waits for a rotation in flight; its self-Remove is built after the rotation committed', async () => {
    const w = await joined();
    const held = w.server.hold('POST commit');
    await w.clock.advance(3_600_000);
    await held.reached;
    const leave = w.send({ m: 'callLeave', reason: 'user' });
    await settled(w);
    expect(w.server.routes()).not.toContain('POST proposal');
    held.release();
    await waitRet(w, leave);
    expect(w.server.routes().filter((r) => r !== 'GET device list')).toEqual(['POST commit', 'POST proposal']);
    const calls = w.model.calls.filter((c) => c.g === toHex(w.callGroup)).map((c) => c.m);
    expect(calls.indexOf('callLeaveProposal')).toBeGreaterThan(calls.indexOf('commitConfirm'));
  });

  it('a pause that lands while the rotation waits on the lane commits nothing', async () => {
    const w = await joined();
    // The engine's catch-up holds the lane: a frame is lost, the next one shows the gap.
    const held = w.server.holdRaw('getHandshakes');
    w.ds.dropNext(ME.device, 1);
    w.ds.peerSend(w.callGroup, PEER, 'a');
    w.ds.peerSend(w.callGroup, PEER, 'b');
    await held.reached;
    await w.clock.advance(3_600_000);                    // the rotation passes its first check and waits for the lane
    w.model.changeIdentity(PEER.user);
    await w.call({ m: 'refreshTrust' });
    expect(w.slice('call').phase).toBe('paused');
    held.release();
    await settled(w, 8);
    expect(w.server.routes()).not.toContain('POST commit');
    expect(w.model.calls.some((c) => c.m === 'callRotate')).toBe(false);
  });
});

describe('review M2-M6', () => {
  // M2. Attacker question: none; a stale decline (L-HTTP-104 acts only on the matching expires_ts) must not hide the ring
  // the instance still runs.
  it('a decline of an older ring keeps the newer ring of the same DM', async () => {
    const w = await harness({ dm: true });
    w.gateway.push(ring(1, { expires: 130 }));
    w.gateway.push(ring(1, { expires: 160 }));
    await w.call({ m: 'callDecline', callId: w.dm, ringTs: 130 });
    expect(w.slice('rings')).toEqual([expect.objectContaining({ channelId: w.dm, ringTs: 160 })]);
    expect(w.server.requests.filter((q) => q.route === 'POST decline').map((q) => q.args[1])).toEqual([130]);
    await w.call({ m: 'callDecline', callId: w.dm, ringTs: 160 });
    expect(w.slice('rings')).toEqual([]);
  });

  it('the last voice_self belongs to its call: after a leave a later join does not re-send it (M3)', async () => {
    const w = await joined();
    await w.call({ m: 'voiceSelf', callId: w.voice, flags: 1 });
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(2);
    await w.call({ m: 'callLeave', reason: 'user' });
    w.gateway.push(voiceFrame(ME, VOICE, 0));
    await w.call(JOIN(w));
    w.gateway.push(voiceFrame(ME, VOICE, 1));
    expect(w.voiceSelfSent).toHaveLength(2);
  });

  // M4. Attacker question: none; the banner must name a member of this call, never an unrelated contact.
  // Web-3 task 10, concern 3: the core reports which leaf of the call tree is the changed contact (the refusal's detail),
  // so the banner names bob though he holds a leaf of the call group and never entered the room.
  it('changedUser after a refused voice join names the contact the core reports from the call tree, never an unrelated one', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer', dm: true });
    // Dave changed his identity somewhere else entirely (a DM text group).
    w.model.addPendingLeaf(w.dms[0].text, 2, FOURTH.user, FOURTH.device, fakeDskPub(FOURTH.device), 1);
    const row = w.ds.listRow(FOURTH.user)!;
    w.model.trustList(FOURTH.user, row.version, row.blob);
    w.model.changeIdentity(FOURTH.user);
    // Bob's identity changed too (seen in the DM): his leaf in the lounge's call group now refuses the join.
    w.model.addPendingLeaf(w.dms[0].text, 1, PEER.user, PEER.device, fakeDskPub(PEER.device), 1);
    w.model.changeIdentity(PEER.user);
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_UMK_CHANGED');
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', changedUser: toHex(PEER.user) });
    // With bob in the live call too, the refusal still names him.
    await w.ds.routesFor(PEER.device).startCall(VOICE, '');
    w.ds.voice(VOICE, PEER, 1);
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_UMK_CHANGED');
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', changedUser: toHex(PEER.user) });
  });

  // M5. Attacker: the instance, the only source of voice_state, floods a call with devices.
  it('a call lists at most 1,024 devices, and a read flooded past 1,024 frames keeps the live state', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    const many = (n: number): Peer[] => Array.from({ length: n }, (_, i) => ({ user: idOf(0xe7, i + 1), device: idOf(0xd7, i + 1) }));
    for (const p of many(1025)) w.gateway.push(voiceFrame(p, VOICE, 1));
    expect(w.slice('voice')[w.voice]?.participants).toHaveLength(1024);
    const v = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    // The snapshot (taken before the flood) names bob only; no frame ever named him.
    const held = v.server.hold('GET channel call');
    v.server.reply('GET channel call', { groupId: v.callGroup, callId: VOICE, participants: [{ userId: PEER.user, deviceId: PEER.device, flags: 1 }] });
    const id = v.send({ m: 'loadVoice', communityId: null, channelId: v.voice });
    await held.reached;
    for (const p of many(1100)) v.gateway.push(voiceFrame(p, VOICE, 1));
    held.release();
    await waitRet(v, id);
    const rows = v.slice('voice')[v.voice]?.participants ?? [];
    expect(rows).toHaveLength(1024);
    expect(rows.some((p) => p.deviceId === toHex(PEER.device))).toBe(false);
  });

  // M6. Attacker question: can the instance turn a lost call-group state into a commit? No: a call group is never
  // re-joined by the engine's text resync; the call ends here as removed and the group is forgotten.
  it('a call group whose state is lost is never resynced by the engine; the call is removed', async () => {
    const w = await joined();
    w.ds.garbageCommit(w.callGroup);
    await settled(w, 8);
    expect(w.server.routes()).not.toContain('GET group info');
    expect(w.server.routes()).not.toContain('POST resync');
    expect((w.slice('call') as CallState | undefined)?.phase).toBe('removed');
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    expect(w.pushes('callKeys')).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------------------------
// Task 6 re-review hardening (note 1 and the two unpinned guards).
// ---------------------------------------------------------------------------------------------

describe('re-review hardening', () => {
  // Re-review note 1 (probe P5): a re-join during a blocked leave would race the old leave's self-Remove and forget, which
  // could overtake the new join. A join is refused while a leave is in flight.
  it('refuses a callJoin while a leave is in flight, and allows it once the leave is done', async () => {
    const w = await joined();
    const held = w.server.hold('POST proposal');
    w.model.addFailedLeaf(w.callGroup, 'E_DEVICE_UNLISTED');
    await w.deliverHandshakes(w.callGroup);
    await held.reached;
    expect(w.slice('call').phase).toBe('blocked');
    const before = w.server.routes().filter((r) => r !== 'GET device list');
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_CALL_ACTIVE');
    expect(w.server.routes().filter((r) => r !== 'GET device list')).toEqual(before);
    held.release();
    await settled(w);
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    expect(w.slice('call').phase).toBe('blocked');
    // Done leaving, a join may run again (the forged leaf keeps refusing it: no key either way).
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_DEVICE_UNLISTED');
    expect(w.pushes('callKeys')).toHaveLength(0);
  });

  // The in-lane guard: a rotation that waited for the group lane while a leave was posted never commits (call_rotate would
  // cancel the posted self-Remove).
  it('a rotation waiting on the lane when a leave starts commits nothing', async () => {
    const w = await joined();
    const held = w.server.holdRaw('getHandshakes');
    w.ds.dropNext(ME.device, 1);
    w.ds.peerSend(w.callGroup, PEER, 'a');
    w.ds.peerSend(w.callGroup, PEER, 'b');
    await held.reached;
    await w.clock.advance(3_600_000);                    // the rotation passes its first checks and waits for the lane
    const leave = w.send({ m: 'callLeave', reason: 'user' });
    held.release();
    await waitRet(w, leave);
    await settled(w);
    expect(w.model.calls.some((c) => c.m === 'callRotate')).toBe(false);
    expect(w.server.routes()).not.toContain('POST commit');
    expect(w.server.routes()).toContain('POST proposal');
  });

  // Ruling I2 and review M4 on the pause path of a voice channel: the candidates are the keyed roster's users. Carol's leaf
  // turns up under another key (a stored verdict, no pending key change); dave changed elsewhere and is not in the call.
  it('a pause in a voice channel names a changed member of the keyed roster', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer', dm: true });
    const carol = w.ds.listRow(THIRD.user)!;
    w.model.trustList(THIRD.user, carol.version, carol.blob);
    w.model.setCallTree(w.callGroup, [{ leafIndex: 0, user: PEER.user, device: PEER.device }, { leafIndex: 2, user: THIRD.user, device: THIRD.device }]);
    w.model.addPendingLeaf(w.dms[0].text, 2, FOURTH.user, FOURTH.device, fakeDskPub(FOURTH.device), 1);
    const dave = w.ds.listRow(FOURTH.user)!;
    w.model.trustList(FOURTH.user, dave.version, dave.blob);
    w.model.changeIdentity(FOURTH.user);
    await w.call(JOIN(w));
    expect(w.slice('call').roster.map((r) => r.userId)).toContain(toHex(THIRD.user));
    w.model.forceVerdict(w.callGroup, 2, 'E_UMK_CHANGED');
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.slice('call')).toMatchObject({ phase: 'paused', changedUser: toHex(THIRD.user) });
  });
});

// ---------------------------------------------------------------------------------------------
// Task 8b review I1: the page's callLeave after the worker's own blocked or removed end keeps that phase.
// ---------------------------------------------------------------------------------------------

describe("the page's leave after a block or a removal (task 8b review I1)", () => {
  const proposals = (w: World): number => w.server.routes().filter((r) => r === 'POST proposal').length;

  it('a callLeave after the worker finished its blocked leave keeps blocked, its error and changedUser', async () => {
    const w = await joined();
    w.model.addFailedLeaf(w.callGroup, 'E_DEVICE_UNLISTED');
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    const blocked = w.slice('call');
    expect(blocked).toMatchObject({ phase: 'blocked', error: { code: 'E_DEVICE_UNLISTED' } });
    expect(await w.call({ m: 'callLeave', reason: 'user' })).toBeNull();
    expect(w.slice('call')).toEqual(blocked);
    expect(proposals(w)).toBe(1);
  });

  it('a callLeave while the worker\'s blocked leave is in flight awaits it and sends no second self-Remove', async () => {
    const w = await joined();
    const held = w.server.hold('POST proposal');
    w.model.addFailedLeaf(w.callGroup, 'E_DEVICE_UNLISTED');
    await w.deliverHandshakes(w.callGroup);
    await held.reached;
    const leave = w.send({ m: 'callLeave', reason: 'user' });
    await settled(w);
    expect(w.ret(leave)).toBeUndefined();
    held.release();
    await waitRet(w, leave);
    await settled(w);
    expect(w.ret(leave)).toMatchObject({ ok: true, value: null });
    expect(proposals(w)).toBe(1);
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', error: { code: 'E_DEVICE_UNLISTED' } });
    const phases = w.posted().map((p) => p.message)
      .filter((m): m is Extract<FromWorker, { t: 'slice' }> => m.t === 'slice' && m.name === 'call').map((m) => (m.value as CallState).phase);
    expect(phases.slice(phases.indexOf('blocked'))).not.toContain('leaving');
    expect(phases.slice(phases.indexOf('blocked'))).not.toContain('idle');
  });

  it('a callLeave after a removal keeps removed', async () => {
    const w = await joined();
    w.ds.propose(w.callGroup, 'remove', ME.device);
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    const removed = w.slice('call');
    expect(removed.phase).toBe('removed');
    expect(await w.call({ m: 'callLeave', reason: 'user' })).toBeNull();
    expect(w.slice('call')).toEqual(removed);
    expect(proposals(w)).toBe(0);
  });

  // RV-3: the page pauses media on a paused slice and resumes on a non-paused one; it installs a key only if the call
  // slice that follows its callKeys says the call is not paused. So every callKeys post is followed at once, with nothing
  // in between, by a call slice that is not paused: at the join, at a new epoch, at a rotation and at a resume.
  it('every callKeys post is followed at once by a call slice that is not paused', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    await w.call(JOIN(w));
    await settled(w);
    await w.deliverHandshakes(w.callGroup);                              // a new epoch
    await settled(w);
    await w.clock.advance(3_600_000);                                     // a rotation
    await settled(w);
    w.model.changeIdentity(PEER.user);                                    // a pause, then a resume on a new epoch
    await w.deliverHandshakes(w.callGroup, THIRD);
    await settled(w);
    expect(w.slice('call').phase).toBe('paused');
    await w.call({ m: 'verifyContact', userId: toHex(PEER.user), umk: 'c4'.repeat(32) });
    await settled(w, 8);
    const posted = w.posted().map((p) => p.message);
    const keys = posted.flatMap((m, i) => (m.t === 'callKeys' ? [i] : []));
    expect(keys.length).toBeGreaterThanOrEqual(4);
    for (const i of keys) {
      const next = posted[i + 1];
      expect(next?.t === 'slice' && next.name === 'call', `after callKeys #${String(i)}`).toBe(true);
      expect(((next as Extract<FromWorker, { t: 'slice' }>).value as CallState).phase).toBe('in');
      expect(((next as Extract<FromWorker, { t: 'slice' }>).value as CallState).epoch)
        .toBe((posted[i] as Extract<FromWorker, { t: 'callKeys' }>).keys.epoch);
    }
  });
});

// ---------------------------------------------------------------------------------------------
// Integration cards (task 9 review m8 and concern 1): the worker acts on every sendBlocks change.
// ---------------------------------------------------------------------------------------------

describe('a send block that comes or goes (integration cards)', () => {
  // Attacker (decision 4 extended to rings): a contact whose identity changes while their ring is showing. The ring must
  // not keep tempting an answer the core would refuse; it goes as soon as the worker sees the DM's block.
  it('a ring ends when its DM becomes send-blocked mid-ring (m8)', async () => {
    const w = await harness({ dm: true, extraDms: 1 });
    const other = w.dms[1];
    w.gateway.push(ring(1));
    w.gateway.push(ring(1, { channel: other.channel, user: other.members[0] }));
    expect(w.slice('rings')).toHaveLength(2);
    // The control: a trust refresh with no block keeps both rings.
    await w.call({ m: 'refreshTrust' });
    expect(w.slice('rings')).toHaveLength(2);
    w.model.blockGroup(w.dms[0].text, 'E_UMK_CHANGED');
    await w.call({ m: 'refreshTrust' });
    expect(w.slice('sendBlocks')).toEqual({ [w.dm]: { code: 'E_UMK_CHANGED', own: false } });
    expect(w.slice('rings').map((r) => r.channelId)).toEqual([toHex(other.channel)]);
  });

  it('a DM call blocked by its text group goes idle once that block clears, so a join is offered again', async () => {
    const w = await harness({ dm: true });
    const text = w.dms[0].text;
    w.model.blockGroup(text, 'E_UMK_CHANGED');
    await w.call({ m: 'refreshTrust' });
    expect(await codeOf(w.call({ m: 'callJoin', channelId: w.dm, vdec: '' }))).toBe('E_UMK_CHANGED');
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', channelId: w.dm });
    // Still blocked: a refresh keeps the phase.
    await w.call({ m: 'refreshTrust' });
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', channelId: w.dm });
    w.model.unblockGroup(text);
    await w.call({ m: 'refreshTrust' });
    expect(w.slice('sendBlocks')).toEqual({});
    expect(w.slice('call')).toEqual({ phase: 'idle', channelId: null, callId: null, epoch: null, code: null, error: null, roster: [],
      outgoing: null, changedUser: null, rejoining: false });
    expect(await codeOf(w.call({ m: 'callJoin', channelId: w.dm, vdec: '' }))).toBe('ok');
  });

  it('a DM call blocked mid-call by its text group goes idle after its leave once that block clears', async () => {
    const w = await harness({ dm: true });
    const text = w.dms[0].text;
    await w.call({ m: 'callJoin', channelId: w.dm, vdec: '' });
    await settled(w);
    expect(w.slice('call').phase).toBe('in');
    w.model.blockGroup(text, 'E_DEVICE_UNLISTED');
    await w.call({ m: 'refreshTrust' });
    await settled(w);
    expect(w.slice('call')).toMatchObject({ phase: 'blocked', channelId: w.dm });
    w.model.unblockGroup(text);
    await w.call({ m: 'refreshTrust' });
    await settled(w);
    expect(w.slice('call').phase).toBe('idle');
  });

  // A block only of the call group (the group is left and forgotten): nothing the worker holds can show it cleared, so
  // the phase stays blocked and the page keeps join held; a removal stays removed (only a new join leaves either).
  it('a block of the call group alone stays blocked, and removed stays removed, across a trust refresh', async () => {
    const w = await joined();
    w.model.addFailedLeaf(w.callGroup, 'E_DEVICE_UNLISTED');
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.slice('call').phase).toBe('blocked');
    await w.call({ m: 'refreshTrust' });
    await settled(w);
    expect(w.slice('call').phase).toBe('blocked');
    const r = await joined();
    r.ds.propose(r.callGroup, 'remove', ME.device);
    await r.deliverHandshakes(r.callGroup);
    await settled(r);
    expect(r.slice('call').phase).toBe('removed');
    await r.call({ m: 'refreshTrust' });
    await settled(r);
    expect(r.slice('call').phase).toBe('removed');
  });
});

// ---------------------------------------------------------------------------------------------
// Whole-branch fix wave (wb-security.md I1, wb-client.md I4 and m1-m7, task-10-fix-review.md N1 and N2).
// ---------------------------------------------------------------------------------------------

describe('a refused mid-call rekey holds sending (wb-security I1)', () => {
  // Attacker: the instance plus one colluding member K. The instance keeps a leaf pending (it withholds the list), then
  // a member commits K's Remove. The new epoch gets no key, and the page must not go on sending under the previous
  // epoch's key, which K's removed leaf still holds: the call holds (phase paused, no changed contact) until a key for
  // the new epoch is posted.
  it('a commit to a new epoch with a pending leaf holds the call, posts no key, and resumes once the leaf validates', async () => {
    const w = await joined();
    // The instance withholds carol's list (her new device's leaf stays pending), and a commit moves the epoch on.
    const lists = (w.ds as unknown as { lists: Map<string, unknown> }).lists;
    const carol = lists.get(toHex(THIRD.user));
    lists.delete(toHex(THIRD.user));
    w.model.setCallTree(w.callGroup, [{ leafIndex: 0, user: PEER.user, device: PEER.device }, { leafIndex: 2, user: THIRD.user, device: THIRD.device }]);
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.server.routes()).toContain('GET device list');
    expect(w.pushes('callKeys')).toHaveLength(0);
    expect(w.slice('call')).toMatchObject({ phase: 'paused', changedUser: null, error: { code: 'E_DEVICE_LIST_STALE' }, epoch: w.result.epoch });
    expect(w.server.routes()).not.toContain('POST proposal');
    // A trust refresh in the meantime keeps the hold.
    await w.call({ m: 'refreshTrust' });
    await settled(w);
    expect(w.slice('call').phase).toBe('paused');
    expect(w.pushes('callKeys')).toHaveLength(0);
    // The list is served again: the next epoch's refresh reads it, that epoch's key is posted, and the call goes on.
    lists.set(toHex(THIRD.user), carol);
    await w.deliverHandshakes(w.callGroup);
    await settled(w, 8);
    expect(keysPosted(w).map((k) => k.epoch)).toEqual([(BigInt(w.result.epoch) + 2n).toString()]);
    expect(w.slice('call')).toMatchObject({ phase: 'in', error: null, changedUser: null });
  });

  it('any other core refusal of a new epoch fails closed: the call holds, no key, no leave, no rotation', async () => {
    const w = await joined();
    const real = w.model.callMediaEpoch.bind(w.model);
    w.model.callMediaEpoch = (): never => { throw new CoreError('E_CORE_STATE', 'unexpected'); };
    await w.deliverHandshakes(w.callGroup);
    await settled(w);
    expect(w.pushes('callKeys')).toHaveLength(0);
    expect(w.slice('call')).toMatchObject({ phase: 'paused', changedUser: null, error: { code: 'E_CORE_STATE' } });
    expect(w.server.routes()).not.toContain('POST proposal');
    await w.clock.advance(3_600_000);
    await settled(w);
    expect(w.server.routes()).not.toContain('POST commit');
    w.model.callMediaEpoch = real;
    await w.call({ m: 'refreshTrust' });
    await settled(w, 8);
    expect(keysPosted(w)).toHaveLength(1);
    expect(w.slice('call')).toMatchObject({ phase: 'in', error: null });
  });
});

describe('a hold the page dropped its keys for ends only with a key (re-review N-m1)', () => {
  // The fail-closed hold: a group read that throws while the epoch did not move. The page drops its keys, and the media
  // pipeline bars that epoch as a sender, so the hold must not end on the same epoch with no key ("in", and silent): it
  // stays held and a rotation moves the epoch on, whose key ends it.
  it('a fail-closed hold at an unchanged epoch stays held and rotates to a fresh epoch, whose key ends it', async () => {
    const w = await joined();
    const realMedia = w.model.callMediaEpoch.bind(w.model);
    const realRow = w.model.groupRow.bind(w.model);
    w.model.callMediaEpoch = (): never => { throw new CoreError('E_CORE_STATE', 'unexpected'); };
    w.model.groupRow = (): never => { throw new CoreError('E_CORE_STATE', 'unexpected'); };
    await w.call({ m: 'refreshTrust' });
    await settled(w);
    expect(w.slice('call')).toMatchObject({ phase: 'paused', error: { code: 'E_CORE_STATE' }, epoch: w.result.epoch });
    w.model.callMediaEpoch = realMedia;
    w.model.groupRow = realRow;
    w.server.reset();
    await w.call({ m: 'refreshTrust' });
    await w.clock.advance(1_000);
    await settled(w, 8);
    // Never "in" on the old epoch with no key: the way out is a fresh epoch, keyed.
    expect(w.server.routes()).toContain('POST commit');
    expect(keysPosted(w).map((k) => BigInt(k.epoch) > BigInt(w.result.epoch))).toEqual([true]);
    expect(w.slice('call')).toMatchObject({ phase: 'in', error: null });
  });
});

describe('the hold\'s way-out rotation never runs while the gate refuses (wave 2 security review m1)', () => {
  it('a changed identity seen after the gate passed again cancels the way-out rotation', async () => {
    const w = await joined();
    const realMedia = w.model.callMediaEpoch.bind(w.model);
    const realRow = w.model.groupRow.bind(w.model);
    w.model.callMediaEpoch = (): never => { throw new CoreError('E_CORE_STATE', 'unexpected'); };
    w.model.groupRow = (): never => { throw new CoreError('E_CORE_STATE', 'unexpected'); };
    await w.call({ m: 'refreshTrust' });
    await settle();
    expect(w.slice('call')).toMatchObject({ phase: 'paused', error: { code: 'E_CORE_STATE' } });
    // The gate passes at the unchanged epoch (the way-out rotation is armed), then a changed identity is seen before it
    // runs: the gate refuses again, and no rotation may commit while it does.
    w.model.groupRow = realRow;
    w.model.callMediaEpoch = realMedia;
    await w.call({ m: 'refreshTrust' });
    w.model.callMediaEpoch = (): never => { throw new CoreError('E_UMK_CHANGED', 'changed'); };
    await w.call({ m: 'refreshTrust' });
    await settle();
    expect(w.slice('call').phase).toBe('paused');
    w.server.reset();
    await w.clock.advance(2 * 3_600_000);
    await settled(w);
    expect(w.server.routes()).not.toContain('POST commit');
  });
});

describe('a failed start on a group this device is alone in (wb-client I4)', () => {
  // The start fails after the join registered a fresh group (or joined one whose tree holds only this device). Nobody can
  // commit a self-Remove of the only leaf, and the core refuses to rejoin the same group at an epoch it has held, so a
  // self-Remove and a forget would lock this device out of the channel's voice until the idle close (RV-2, 10 min).
  it('a start refused after a fresh registration leaves no self-Remove, and the next join works at once', async () => {
    const w = await harness({ voiceChannel: true });
    w.server.reset();
    w.server.answer('POST calls', 500, 'E_INTERNAL');
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_INTERNAL');
    expect(w.slice('call')).toMatchObject({ phase: 'failed' });
    expect(w.server.routes()).toEqual(['GET channel call', 'POST group', 'POST calls', 'DELETE call']);
    expect(w.pushes('callKeys')).toHaveLength(0);
    const again = await w.call(JOIN(w)) as CallJoinResult;
    expect(w.slice('call')).toMatchObject({ phase: 'in', callId: again.callId });
    expect(keysPosted(w)).toHaveLength(1);
  });

  it('a device that joined meanwhile (the conditional end answers 409) gets the self-Remove after all', async () => {
    const w = await harness({ voiceChannel: true });
    w.server.reset();
    w.server.answer('POST calls', 500, 'E_INTERNAL');
    w.server.reply('DELETE call', false);
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_INTERNAL');
    expect(w.server.routes()).toEqual(['GET channel call', 'POST group', 'POST calls', 'DELETE call', 'POST proposal']);
  });

  // Re-review N-m3: only a 404 means there is no call to end. A 5xx (or a lost answer) leaves it open whether another
  // device joined, so the self-Remove is sent, as for a 409 (a self-Remove never locks the rejoin, guard 1).
  it('a conditional end that fails with a server error sends the self-Remove; a 404 does not', async () => {
    const w = await harness({ voiceChannel: true });
    w.server.reset();
    w.server.answer('POST calls', 500, 'E_INTERNAL');
    w.server.answer('DELETE call', 503, 'E_UNAVAILABLE');
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_INTERNAL');
    expect(w.server.routes()).toEqual(['GET channel call', 'POST group', 'POST calls', 'DELETE call', 'POST proposal']);
    const v = await harness({ voiceChannel: true });
    v.server.reset();
    v.server.answer('POST calls', 500, 'E_INTERNAL');
    v.server.answer('DELETE call', 404, 'E_NOT_FOUND');
    expect(await codeOf(v.call(JOIN(v)))).toBe('E_INTERNAL');
    expect(v.server.routes()).toEqual(['GET channel call', 'POST group', 'POST calls', 'DELETE call']);
  });

  it('a start refused on a joined group with another member still sends the self-Remove', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.server.reset();
    w.server.answer('POST calls', 500, 'E_INTERNAL');
    expect(await codeOf(w.call(JOIN(w)))).toBe('E_INTERNAL');
    expect(lastCallRoute(w)).toBe('POST proposal');
    expect(w.model.group(w.callGroup)?.state).toBe(4);
  });
});

describe('the hourly leaf rotation survives a throwing commitConfirm (wb-client m7)', () => {
  const rotations = (w: World): number => w.model.calls.filter((c) => c.m === 'callRotate').length;

  // protocol/01 obliges a fresh call leaf at least hourly: a throw after the commit was posted must not stop it for the
  // rest of the call. The instance took the commit, so the engine adopts it as this device's own when it arrives (the
  // call stays in, on the new epoch), and the rotation is armed again.
  it('a commitConfirm that throws keeps the call on the adopted commit and rotates again', async () => {
    const w = await joined();
    const real = w.model.commitConfirm.bind(w.model);
    let throws = 1;
    w.model.commitConfirm = (g: Id) => { if (throws > 0) { throws -= 1; throw new CoreError('E_CORE_STATE', 'merge failed'); } return real(g); };
    await w.clock.advance(3_600_000);
    await settled(w, 8);
    expect(rotations(w)).toBe(1);
    expect(w.slice('call')).toMatchObject({ phase: 'in', epoch: (BigInt(w.result.epoch) + 1n).toString() });
    await w.clock.advance(60_000);
    await settled(w, 8);
    expect(rotations(w)).toBe(2);
    expect(w.slice('call')).toMatchObject({ phase: 'in', epoch: (BigInt(w.result.epoch) + 2n).toString() });
  });

  // The bound: a rotation that keeps failing is retried CALL_ROTATE_RETRIES times a minute apart, then hourly again.
  it('a rotation that keeps failing retries a bounded number of times, then waits for the next hour', async () => {
    const w = await joined();
    w.model.commitConfirm = (): never => { throw new CoreError('E_CORE_STATE', 'merge failed'); };
    await w.clock.advance(3_600_000);
    await settled(w);
    for (let i = 0; i < 10; i++) { await w.clock.advance(60_000); await settled(w); }
    expect(rotations(w)).toBe(1 + CALL_ROTATE_RETRIES);
    await w.clock.advance(3_600_000);
    await settled(w);
    expect(rotations(w)).toBeGreaterThan(1 + CALL_ROTATE_RETRIES);
    expect(w.slice('call').phase).toBe('in');
  });
});

describe('a hand-over is not held up by call work (wb-client m5)', () => {
  // The new tab gives up after 5 s (handover.ts): resign must release the store well inside that.
  const MS = 4_000;
  async function resignWithin(w: World): Promise<number> {
    const start = w.clock.now();
    w.send({ m: 'resign', nonce: 'ab'.repeat(16) });
    for (let i = 0; i < 40 && w.released() === 0; i++) { await settle(); await w.clock.advance(250); }
    return w.clock.now() - start;
  }

  it('a join waiting out its earlier Remove is cancelled at once by resign', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.removeTarget.add(toHex(ME.device));
    w.send(JOIN(w));
    for (let i = 0; i < 4; i++) { await settle(); await w.clock.advance(1_000); }
    expect(w.slice('call')).toMatchObject({ phase: 'joining', rejoining: true });
    const took = await resignWithin(w);
    expect(w.released()).toBe(1);
    expect(took).toBeLessThan(MS);
  });

  it('a paced voice-driven member read does not hold resign for its pacing sleep', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    const late: Peer = { user: idOf(0xe9, 1), device: idOf(0xd9, 1) };
    w.gateway.push(voiceFrame(late, VOICE, 1));
    for (let i = 0; i < 4; i++) await settle();
    // A second unknown user right after: its read is paced 5 s behind the first.
    w.gateway.push(voiceFrame({ user: idOf(0xe9, 2), device: idOf(0xd9, 2) }, VOICE, 1));
    await settle();
    const took = await resignWithin(w);
    expect(w.released()).toBe(1);
    expect(took).toBeLessThan(MEMBER_REREAD_MIN_MS);
  });

  it('a leave that cannot finish holds resign for a bounded time only', async () => {
    const w = await joined();
    w.server.hold('POST proposal');
    const took = await resignWithin(w);
    expect(w.released()).toBe(1);
    expect(took).toBeLessThan(MS);
  });
});

describe('a room that ended on its own (wb-client I3)', () => {
  it("callLeave 'lost' leaves as a person's leave and ends in phase removed with E_DISCONNECTED", async () => {
    const w = await joined();
    expect(await w.call({ m: 'callLeave', reason: 'lost' })).toBeNull();
    expect(w.server.routes()).toContain('POST proposal');
    expect(w.model.group(w.callGroup)?.state).toBe(4);
    expect(w.slice('call')).toMatchObject({ phase: 'removed', channelId: w.voice, error: { code: 'E_DISCONNECTED' } });
    // A join is offered again from there.
    await w.call(JOIN(w));
    expect(w.slice('call').phase).toBe('in');
  });
});

describe('a ring follows whether this device is busy (wb-client m6)', () => {
  it('a ring that arrived during a call is no longer busy once the call is left, and busy again in the next', async () => {
    const w = await joined({ dm: true });
    w.gateway.push(ring(1));
    expect(w.slice('rings')).toEqual([expect.objectContaining({ channelId: w.dm, busy: true })]);
    await w.call({ m: 'callLeave', reason: 'user' });
    expect(w.slice('rings')).toEqual([expect.objectContaining({ channelId: w.dm, busy: false })]);
    await w.call(JOIN(w));
    expect(w.slice('rings')).toEqual([expect.objectContaining({ channelId: w.dm, busy: true })]);
  });
});

describe('the rejoin wait and a leave during its reads (task-10 fix review N1, N2)', () => {
  // N1: a leave that lands while one of the wait's two reads is in flight stops the join: nothing is posted, and no
  // call starts or rings afterwards.
  for (const label of ['GET channel call', 'GET group info'] as const) {
    it(`a leave while the wait's ${label} is in flight posts nothing afterwards`, async () => {
      const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
      w.ds.removeTarget.add(toHex(ME.device));
      const id = w.send(JOIN(w));
      for (let i = 0; i < 4 && w.slice('call').rejoining !== true; i++) { await settle(); await w.clock.advance(0); }
      expect(w.slice('call')).toMatchObject({ phase: 'joining', rejoining: true });
      const held = w.server.hold(label);
      // The read answers as if the Remove had been committed (the group moved on), which would send the join on.
      w.ds.removeTarget.delete(toHex(ME.device));
      w.ds.peerCommit(w.callGroup, PEER);
      for (let i = 0; i < 4; i++) { await settle(); await w.clock.advance(1_000); }
      await held.reached;
      const leave = w.send({ m: 'callLeave', reason: 'user' });
      await settle();
      const before = w.server.routes().length;
      held.release();
      await waitRet(w, id);
      await waitRet(w, leave);
      await settled(w);
      expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_NO_CALL' } });
      expect(w.server.routes().slice(before).filter((r) => r === 'POST resync' || r === 'POST calls' || r === 'POST group')).toEqual([]);
      expect(w.pushes('callKeys')).toHaveLength(0);
      expect(w.slice('call')).toMatchObject({ phase: 'idle' });
    });
  }

  // Re-review N-m2: the check before startCall, alone. A leave while the media epoch's list refresh is in flight (a
  // pending leaf: callMediaEpoch answers E_DEVICE_LIST_STALE first) posts no start, so no call opens and nobody is rung.
  it('a leave while the media epoch waits for a list refresh starts no call', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    // The media epoch's first read finds a pending leaf (E_DEVICE_LIST_STALE): the leave lands right then, while its list
    // refresh runs; the second read succeeds, so only the check before startCall stands between the leave and a start.
    const real = w.model.callMediaEpoch.bind(w.model);
    let leave = -1;
    let first = true;
    w.model.callMediaEpoch = (g: Id) => {
      if (first) {
        first = false;
        leave = w.send({ m: 'callLeave', reason: 'user' });
        throw new CoreError('E_DEVICE_LIST_STALE', 'pending');
      }
      return real(g);
    };
    const id = w.send(JOIN(w));
    await waitRet(w, id);
    await waitRet(w, leave);
    await settled(w);
    expect(first).toBe(false);
    expect(w.ret(id)).toMatchObject({ ok: false });
    expect(w.server.routes()).not.toContain('POST calls');
    expect(w.pushes('callKeys')).toHaveLength(0);
    expect(w.slice('call')).toMatchObject({ phase: 'idle' });
  });

  // N2: the 30 s bound is elapsed time on a monotonic clock: a wall clock stepped back an hour (NTP, a resumed laptop)
  // does not stretch it, and one stepped forward does not cut it short.
  async function runFor(w: World, id: number, ms: number): Promise<void> {
    for (let t = 0; t < ms && w.ret(id) === undefined; t += 1_000) { await settle(); await w.clock.advance(1_000); }
    await settle();
  }
  it('a wall clock stepped back does not stretch the wait past 30 s', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.removeTarget.add(toHex(ME.device));
    const id = w.send(JOIN(w));
    await runFor(w, id, 3_000);
    w.wall.skew = -3_600_000;
    await runFor(w, id, 29_000);
    expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_REMOVE_PENDING' } });
  });
  it('a wall clock stepped forward does not cut the wait short', async () => {
    const w = await harness({ voiceChannel: true, callGroupBy: 'peer' });
    w.ds.removeTarget.add(toHex(ME.device));
    const id = w.send(JOIN(w));
    await runFor(w, id, 3_000);
    w.wall.skew = 3_600_000;
    await runFor(w, id, 10_000);
    expect(w.ret(id)).toBeUndefined();
    expect(w.slice('call')).toMatchObject({ phase: 'joining', rejoining: true });
  });
});

// ---------------------------------------------------------------------------------------------
// web3.7 3a-client: the boot sweep leaves a call group a page of this browser left behind, before any join.
// ---------------------------------------------------------------------------------------------

describe('the boot sweep of leftover call groups (web3.7 3a-client, R32)', () => {
  type Before = NonNullable<HarnessOptions['before']>;
  /** A reload mid-call: this device alone in the lounge's live call, its call group still held active in the store. */
  function leftover(more: Before = () => {}): { group: () => Id; before: Before } {
    let group: Id = new Uint8Array(16);
    return {
      group: () => group,
      before: async (b) => {
        group = b.ds.peerCreateCall(ME, VOICE);
        await b.ds.routesFor(ME.device).startCall(VOICE, '');
        b.ds.voice(VOICE, ME, 1);
        b.model.seedGroup(group, 1, COMMUNITY, VOICE);
        await more(b);
      },
    };
  }
  const coreCalls = (w: World, m: string, g: Id): number => w.model.calls.filter((c) => c.m === m && c.g === toHex(g)).length;
  const count = (w: World, route: string): number => w.server.routes().filter((r) => r === route).length;

  it('a rejoin during the boot sweep waits for it, and the sweep never touches the group the join registers', async () => {
    let held = { release: (): void => {}, reached: Promise.resolve() };
    const seed = leftover((b) => { held = b.server.hold('GET channel call'); });
    const w = await harness({ voiceChannel: true, before: seed.before });
    await held.reached;
    const join = w.send(JOIN(w));
    await settled(w);
    expect(w.ret(join)).toBeUndefined();
    expect(w.server.routes().filter((r) => r !== 'GET members')).toEqual(['GET channel call']);
    held.release();
    for (let i = 0; i < 40 && w.ret(join) === undefined; i++) await settled(w);
    const result = w.ret(join);
    expect(result).toMatchObject({ ok: true });
    const joinedGroup = (result as { value: CallJoinResult }).value.groupId;
    expect(joinedGroup).not.toBe(toHex(seed.group()));
    const routes = w.server.routes();
    // The sweep: the read, the conditional end (alone), then the forget only; then the join registers afresh.
    expect(routes.indexOf('DELETE call')).toBeLessThan(routes.indexOf('POST group'));
    expect(coreCalls(w, 'callGroupForget', seed.group())).toBe(1);
    expect(coreCalls(w, 'callLeaveProposal', seed.group())).toBe(0);
    expect(coreCalls(w, 'callGroupForget', fromHex(joinedGroup))).toBe(0);
    expect(coreCalls(w, 'callLeaveProposal', fromHex(joinedGroup))).toBe(0);
    expect(w.slice('call').phase).toBe('in');
  });

  it('sign-out during the boot sweep stops it', async () => {
    let held = { release: (): void => {}, reached: Promise.resolve() };
    const seed = leftover((b) => { held = b.server.hold('GET channel call'); });
    const w = await harness({ voiceChannel: true, before: seed.before });
    await held.reached;
    const out = w.send({ m: 'signOutRevoke', recoveryKey: 'QRST'.repeat(13) });
    await settled(w);
    held.release();
    await settled(w);
    expect(w.ret(out)).toBeDefined();
    expect(count(w, 'DELETE call')).toBe(0);
    expect(count(w, 'POST proposal')).toBe(0);
    expect(coreCalls(w, 'callGroupForget', seed.group())).toBe(0);
  });

  it('a handover stops the released tab\'s sweep; the tab that leads then sweeps, once', async () => {
    let held = { release: (): void => {}, reached: Promise.resolve() };
    const seed = leftover((b) => { held = b.server.hold('GET channel call'); });
    const w = await harness({ voiceChannel: true, before: seed.before });
    await held.reached;
    // "Use this tab" elsewhere: this tab resigns; the harness's lock hands it straight back, so it leads again.
    await w.call({ m: 'resign', nonce: 'ab'.repeat(16) });
    for (let i = 0; i < 40 && coreCalls(w, 'callGroupForget', seed.group()) === 0; i++) await settled(w);
    expect(w.released()).toBe(1);
    // The released tab's read answers late: that sweep sends nothing more.
    held.release();
    await settled(w);
    expect(count(w, 'GET channel call')).toBe(2);
    expect(count(w, 'DELETE call')).toBe(1);
    expect(coreCalls(w, 'callGroupForget', seed.group())).toBe(1);
  });

  it('a gateway ready before the phase is ready starts no sweep; the end of the ready does (review m1)', async () => {
    let held = { release: (): void => {}, reached: Promise.resolve() };
    const seed = leftover((b) => { held = b.server.hold('GET members'); });
    // The socket reports ready while the ready path still waits for the member list.
    const w = await harness({ voiceChannel: true, before: seed.before });
    await held.reached;
    w.reconnect();
    await settled(w);
    expect((w.slice('account') as { phase: string }).phase).not.toBe('ready');
    expect(count(w, 'GET channel call')).toBe(0);
    held.release();
    for (let i = 0; i < 40 && coreCalls(w, 'callGroupForget', seed.group()) === 0; i++) await settled(w);
    expect(count(w, 'GET channel call')).toBe(1);
    expect(count(w, 'DELETE call')).toBe(1);
  });

  it('an offline boot forgets nothing and sweeps at the first online; the join is not held meanwhile', async () => {
    const seed = leftover((b) => { b.server.refuseAll('GET channel call', 0, 'E_NETWORK'); });
    const w = await harness({ voiceChannel: true, before: seed.before });
    await settled(w);
    expect(count(w, 'GET channel call')).toBeGreaterThan(0);
    expect(count(w, 'DELETE call')).toBe(0);
    expect(coreCalls(w, 'callGroupForget', seed.group())).toBe(0);
    // A join meanwhile is not held by the deferred sweep: it reads at once (and is refused, still offline).
    const reads = count(w, 'GET channel call');
    const join = w.send(JOIN(w));
    await settled(w);
    expect(w.ret(join)).toMatchObject({ ok: false });
    expect(count(w, 'GET channel call')).toBe(reads + 1);
    expect(w.slice('call').phase).toBe('failed');
    // The first online: the sweep runs again and leaves the call.
    w.server.allow('GET channel call');
    w.reconnect();
    await settled(w);
    expect(count(w, 'DELETE call')).toBe(1);
    expect(coreCalls(w, 'callGroupForget', seed.group())).toBe(1);
    // A later reconnect sweeps nothing more.
    w.reconnect();
    await settled(w);
    expect(count(w, 'GET channel call')).toBe(reads + 2);
  });
});

export { fromHex };
