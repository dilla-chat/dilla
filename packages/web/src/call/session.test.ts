/* eslint-disable @typescript-eslint/require-await -- the doubles answer as the async functions they stand for do */
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CallJoinResult, CallKeys, Command } from '@dilla/client-core';
import { screenAudioOptions, type EpochKeys, type InputStageOptions, type JoinCallOptions, type SensitivityConfig } from '@dilla/media';
import { getBrowser } from 'livekit-client';
import { concurrentCapture, rttOf, startPageCall, type PageCall, type PageCallDeps, type PageCallOptions } from './session.ts';

const CH = 'c1'.repeat(16);
const callId = 'a1'.repeat(16);
const GROUP = 'b2'.repeat(16);
const ME = 'd0'.repeat(16);
const ALICE = 'd1'.repeat(16);
const DEV_D = 'd4'.repeat(16);
const BOB = 'e1'.repeat(16);
const BOB_DEV = 'd2'.repeat(16);
const CAROL = 'e2'.repeat(16);
const CAROL_DEV = 'd3'.repeat(16);
const ME_USER = 'e0'.repeat(16);
const ALICE_USER = 'e5'.repeat(16);

type Row = { leaf: number; deviceId: string; userId: string };
const BASE_ROSTER: Row[] = [
  { leaf: 0, deviceId: ME, userId: ME_USER },
  { leaf: 1, deviceId: BOB_DEV, userId: BOB },
  { leaf: 2, deviceId: CAROL_DEV, userId: CAROL },
];

function keysFor(epoch: bigint, roster: Row[] = BASE_ROSTER): CallKeys {
  return { groupId: GROUP, epoch: epoch.toString(), baseKey: new Uint8Array(16).fill(0x42).buffer, selfLeaf: 0, roster };
}
const zero = (b: Uint8Array) => b.every((x) => x === 0);
const flush = async () => { for (let i = 0; i < 5; i++) await new Promise<void>((r) => setTimeout(r, 0)); };

class Emitter {
  private readonly map = new Map<string, Set<(...a: unknown[]) => void>>();
  on(e: string, f: (...a: unknown[]) => void) { (this.map.get(e) ?? this.map.set(e, new Set()).get(e)!).add(f); return this; }
  off(e: string, f: (...a: unknown[]) => void) { this.map.get(e)?.delete(f); return this; }
  emit(e: string, ...a: unknown[]) { for (const f of [...(this.map.get(e) ?? [])]) f(...a); return true; }
  count(e: string) { return this.map.get(e)?.size ?? 0; }
}

interface FakeTrack { kind: string; muted: boolean; stopped: boolean; mute: () => void; unmute: () => void; stop: () => void;
  mediaStreamTrack: { kind: string; stop: () => void; addEventListener: () => void; removeEventListener: () => void };
  mediaStream?: unknown; setProcessor?: (p: unknown) => Promise<void> }

function fakeLocalTrack(kind: string): FakeTrack {
  const t: FakeTrack = {
    kind, muted: false, stopped: false,
    mute() { t.muted = true; }, unmute() { t.muted = false; }, stop() { t.stopped = true; },
    mediaStreamTrack: { kind, stop: () => { t.stopped = true; }, addEventListener: () => undefined, removeEventListener: () => undefined },
    setProcessor: vi.fn(async () => undefined),
  };
  return t;
}

/** A remote track as livekit hands it over: attach() makes an element (as RemoteAudioTrack’s, a fresh <audio>), detach(el)
 *  lets it go; every element ever made is kept in `made`. */
function fakeRemoteTrack(kind: 'audio' | 'video', sid: string) {
  const t = {
    kind, sid, attached: [] as HTMLMediaElement[], made: [] as HTMLMediaElement[],
    /** The MediaStreamTrack livekit puts on the element it attaches (web3.8 V1: the level meter reads it). */
    mst: { kind, id: sid } as unknown as MediaStreamTrack,
    attach: vi.fn((el?: HTMLMediaElement) => {
      const out = el ?? document.createElement(kind);
      if (kind === 'audio') out.srcObject = { getAudioTracks: () => [t.mst] } as unknown as MediaStream;
      t.attached.push(out);
      t.made.push(out);
      return out;
    }),
    detach: vi.fn((el?: HTMLMediaElement) => {
      const out = el === undefined ? t.attached.splice(0) : t.attached.filter((x) => x === el);
      t.attached = t.attached.filter((x) => !out.includes(x));
      return el === undefined ? out : el;
    }),
  };
  return t;
}
type FakeRemoteTrack = ReturnType<typeof fakeRemoteTrack>;

/** The session’s own AudioContext (PageCallDeps.audioContext): its state, statechange, resume, close and setSinkId. */
class FakeAudioContext extends EventTarget {
  state: 'running' | 'suspended' | 'closed' = 'running';
  readonly resume = vi.fn(async () => { this.to('running'); });
  readonly close = vi.fn(async () => { this.to('closed'); });
  readonly setSinkId = vi.fn(async (id: string) => { void id; });
  to(s: 'running' | 'suspended' | 'closed') { this.state = s; this.dispatchEvent(new Event('statechange')); }
}

function fakeRemote(identity: string) {
  const gains = { microphone: 1, screen_share_audio: 1 } as Record<string, number>;
  const p = {
    identity, isSpeaking: false, attributes: {},
    gains,
    get gain() { return { value: gains.microphone }; },
    setVolume: vi.fn((v: number, source: string = 'microphone') => { gains[source] = v; }),
    trackPublications: new Map<string, unknown>(),
    getTrackPublication: (source: string) => [...p.trackPublications.values()].find((x) => (x as { source: string }).source === source),
  };
  return p;
}

interface Options {
  joinEpoch?: bigint;
  joinCallError?: Error;
  iceUsername?: string;
  now?: number;
  getUserMedia?: () => Promise<MediaStream>;
  devices?: MediaDeviceInfo[];
  statsError?: Error;
}

function fakes(o: Options = {}) {
  const joinEpoch = o.joinEpoch ?? 1n;
  const order: string[] = [];
  const calls: Command[] = [];
  // Task 11: what the worker answers to callRtt, in order (true once the queue is empty).
  const rttAnswers: boolean[] = [];
  const listeners = new Set<(id: string, k: CallKeys) => void>();
  const pushKeys = (id: string, k: CallKeys) => { for (const l of [...listeners]) l(id, k); };

  // A manual clock: setTimer queues, advance() runs what falls due in order and lets promises settle.
  let now = o.now ?? 1_000_000;
  let nextTimer = 1;
  const timers = new Map<number, { at: number; fn: () => void }>();
  const advance = async (ms: number) => {
    const end = now + ms;
    for (;;) {
      const due = [...timers.entries()].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
      if (!due) break;
      timers.delete(due[0]);
      now = due[1].at;
      due[1].fn();
      for (let i = 0; i < 20; i++) await Promise.resolve();
    }
    now = end;
    await flush();
  };

  const verified = new Set<string>();
  const installs: EpochKeys[] = [];
  const manager = Object.assign(new Emitter(), {
    installs,
    installEpoch: vi.fn(async (k: EpochKeys) => { installs.push(k); }),
    dispose: vi.fn(() => { order.push('manager.dispose'); }),
    clearKeys: vi.fn(() => { order.push('manager.clearKeys'); }),
    verifiedIdentities: vi.fn(async () => new Set(verified)),
    stats: vi.fn(async () => ({ encrypted: {}, dropped: {} })),
  });

  const localTracks: FakeTrack[] = [];
  const room = Object.assign(new Emitter(), {
    remoteParticipants: new Map<string, ReturnType<typeof fakeRemote>>(),
    engine: { rtcConfig: { iceServers: [] as RTCIceServer[] }, pcManager: { publisher: { getStats: async () => new Map() } } },
    localParticipant: Object.assign(new Emitter(), {
      identity: ME,
      audioLevel: 0,
      permissions: { canPublish: true, canPublishSources: [2] as number[] },
      publishTrack: vi.fn(async (track: MediaStreamTrack, opts: { source: string }) => {
        const t = fakeLocalTrack(track.kind);
        localTracks.push(t);
        return { source: opts.source, track: t, trackSid: `TR_${localTracks.length}` };
      }),
      unpublishTrack: vi.fn(async (track: unknown) => { order.push('unpublish'); void track; }),
    }),
    disconnect: vi.fn(async () => { order.push('room.disconnect'); }),
    // The browser’s autoplay rule: false while it holds call audio back; startAudio() is the one press that lifts it.
    canPlaybackAudio: true,
    startAudio: vi.fn(async () => undefined),
  });
  // The hidden container the session attaches remote audio elements into (PageCallDeps.audioHost).
  const audioHost = document.createElement('div');
  // Every AudioContext the session made, newest last; `contextState` is the state a new one starts in.
  const contexts: FakeAudioContext[] = [];
  const audio = { contextState: 'running' as 'running' | 'suspended', noSinkId: false };

  const share = (on: boolean) => {
    room.localParticipant.permissions = { canPublish: true, canPublishSources: on ? [1, 2, 3, 4] : [2] };
    room.emit('participantPermissionsChanged', undefined, room.localParticipant);
  };

  let joinOptions: JoinCallOptions | null = null;
  const joinCall = vi.fn(async (opts: JoinCallOptions) => {
    joinOptions = opts;
    if (o.joinCallError) throw o.joinCallError;
    await manager.installEpoch(opts.epoch);
    return {
      room, manager,
      release: async () => {
        manager.dispose();
        await room.disconnect();
        order.push('worker.terminate');
        order.push('lock.release');
      },
    };
  });

  const join: CallJoinResult = {
    callId, groupId: GROUP, livekitUrl: 'wss://sfu.example/rtc', token: 'tok',
    iceServers: [[['turn:sfu.example:3478'], o.iceUsername ?? '1700003600:dev:1700000000', 'cred']],
    caps: [64_000, 2_500_000, 0], epoch: joinEpoch.toString(),
  };
  let refreshAnswer = { token: 'tok2', iceServers: [[['turn:sfu.example:3478'], '1700007200:dev:1700003300', 'cred2']] };
  const bridge = {
    onCallKeys: vi.fn((l: (id: string, k: CallKeys) => void) => {
      order.push('onCallKeys');
      listeners.add(l);
      return () => { listeners.delete(l); };
    }),
    call: vi.fn(async (cmd: Command) => {
      order.push(cmd.m);
      calls.push(cmd);
      switch (cmd.m) {
        case 'callJoin':
          pushKeys(callId, keysFor(joinEpoch));
          return join;
        case 'callShare':
          if (cmd.on) share(true);
          return null;
        case 'callRefresh':
          return refreshAnswer;
        case 'callStats':
          if (o.statsError) throw o.statsError;
          return null;
        case 'callRtt':
          return rttAnswers.length > 0 ? rttAnswers.shift() : true;
        default:
          return null;
      }
    }),
  };

  const gum = vi.fn(o.getUserMedia ?? (async () => ({
    getTracks: () => [{ kind: 'audio', stop: () => undefined }],
    getAudioTracks: () => [{ kind: 'audio', stop: () => undefined }],
    getVideoTracks: () => [{ kind: 'video', stop: () => undefined }],
  }) as unknown as MediaStream));
  const media = {
    getUserMedia: gum,
    enumerateDevices: vi.fn(async () => o.devices ?? [{ kind: 'audioinput', deviceId: 'default' } as MediaDeviceInfo]),
    getDisplayMedia: vi.fn(async () => ({
      getTracks: () => [{ kind: 'video', stop: () => undefined }],
      getAudioTracks: () => [],
      getVideoTracks: () => [{ kind: 'video', stop: () => undefined, addEventListener: () => undefined, removeEventListener: () => undefined }],
    }) as unknown as MediaStream),
  };

  const deps: PageCallDeps = {
    bridge,
    joinCall: joinCall as unknown as PageCallDeps['joinCall'],
    now: () => now,
    setTimer: (fn, ms) => { const id = nextTimer++; timers.set(id, { at: now + ms, fn }); return id; },
    clearTimer: (id) => { timers.delete(id as number); },
    media,
    audioHost: () => audioHost,
    audioContext: () => {
      const ctx = new FakeAudioContext();
      ctx.state = audio.contextState;
      if (audio.noSinkId) Object.defineProperty(ctx, 'setSinkId', { value: undefined });
      contexts.push(ctx);
      return ctx as unknown as AudioContext;
    },
  };
  const addRemote = (identity: string) => {
    const p = fakeRemote(identity);
    room.remoteParticipants.set(identity, p);
    room.emit('participantConnected', p);
    return p;
  };
  type Remote = ReturnType<typeof fakeRemote>;
  type Pub = { source: string; kind: string; trackSid: string; track?: FakeRemoteTrack };
  /** `p` publishes `source`, and this device is subscribed to it: the track is on the publication, and with `emit` the
   *  room says so (trackSubscribed). Without `emit` it was subscribed before anyone listened. */
  const subscribeTrack = (p: Remote, source: string, emit = true) => {
    const kind = source === 'camera' || source === 'screen_share' ? 'video' : 'audio';
    const trackSid = `TR_${p.identity.slice(0, 4)}_${source}`;
    const pub = (p.trackPublications.get(trackSid) as Pub | undefined) ?? { source, kind, trackSid };
    pub.track ??= fakeRemoteTrack(kind, trackSid);
    p.trackPublications.set(trackSid, pub);
    if (emit) room.emit('trackSubscribed', pub.track, pub, p);
    return { track: pub.track, pub };
  };
  /** The SFU stopped sending `pub` to this device (SpeakerPolicy’s exclusion, or the publisher’s unpublish). */
  const unsubscribeTrack = (p: Remote, pub: Pub) => {
    const track = pub.track;
    delete pub.track;
    room.emit('trackUnsubscribed', track, pub, p);
  };
  return {
    deps, order, calls, rttAnswers, pushKeys, manager, room, localTracks, advance, verified, addRemote, share, timers, gum,
    audioHost, subscribeTrack, unsubscribeTrack, contexts, audio, media,
    get joinOptions() { return joinOptions; },
    setRefresh: (a: typeof refreshAnswer) => { refreshAnswer = a; },
    setNow: (t: number) => { now = t; },
  };
}

const opts: PageCallOptions = { instanceId: 'inst', input: '', output: '', mode: 'vad', noise: 'off', volumes: {} };

describe('the page call session (L-WEB-50)', () => {
  const open: PageCall[] = [];
  afterEach(async () => {
    for (const c of open.splice(0)) await c.leave('user').catch(() => undefined);
  });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };

  it('subscribes before callJoin, installs the pushed join key, then every later key, and none after leave()', async () => {
    const { deps, pushKeys, manager, order } = fakes({ joinEpoch: 1n });
    const call = await startPageCall(deps, { channelId: CH, vdec: 'vp8' }, opts);
    expect(order.slice(0, 2)).toEqual(['onCallKeys', 'callJoin']);
    expect(manager.installs.map((k) => [k.epoch, k.minEpoch])).toEqual([[1n, 1n]]);
    pushKeys(callId, keysFor(2n));
    expect(manager.installs.map((k) => k.epoch)).toEqual([1n, 2n]);
    await call.leave('user');
    pushKeys(callId, keysFor(3n));
    expect(manager.installs.map((k) => k.epoch)).toEqual([1n, 2n]);
  });

  it('leave("user") posts callLeave before it disconnects the room', async () => {
    const { deps, order } = fakes({ joinEpoch: 1n });
    const call = await startPageCall(deps, { channelId: CH, vdec: 'vp8' }, opts);
    order.length = 0;
    await call.leave('user');
    expect(order).toEqual(['callLeave', 'manager.dispose', 'room.disconnect', 'worker.terminate', 'lock.release']);
  });

  it('joins with the join key, the join epoch as minEpoch, the call group lock and the dilla room options', async () => {
    const f = fakes({ joinEpoch: 4n });
    await start(f, { output: 'spk' });
    const j = f.joinOptions!;
    expect(f.calls[0]).toEqual({ m: 'callJoin', channelId: CH, vdec: 'vp8' });
    expect(j.livekitUrl).toBe('wss://sfu.example/rtc');
    expect(j.token).toBe('tok');
    expect(j.lock).toEqual({ instanceId: 'inst', callGroupId: GROUP });
    expect(j.epoch.epoch).toBe(4n);
    expect(j.epoch.minEpoch).toBe(4n);
    // Fix round 1 (I1, M2): the session’s own AudioContext is the mixer.
    expect(f.contexts).toHaveLength(1);
    expect(j.roomOptions?.webAudioMix).toEqual({ audioContext: f.contexts[0] });
    expect(j.roomOptions?.publishDefaults?.audioPreset?.maxBitrate).toBe(64_000);
    expect(j.roomOptions?.audioOutput).toEqual({ deviceId: 'spk' });
    // The join copy is zero once joinCall settled.
    expect(zero(j.epoch.baseKey)).toBe(true);
  });

  it('the key copy is zero-filled once installEpoch settles, and a key for another call is zero-filled and never installed', async () => {
    const f = fakes();
    await start(f);
    f.pushKeys(callId, keysFor(2n));
    const copy = f.manager.installs[1].baseKey;
    expect(zero(copy)).toBe(false);
    await flush();
    expect(zero(copy)).toBe(true);
    const foreign = keysFor(9n);
    f.pushKeys('ff'.repeat(16), foreign);
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n, 2n]);
    expect(zero(new Uint8Array(foreign.baseKey))).toBe(true);
  });

  it('leave("blocked") disconnects first and then posts callLeave', async () => {
    const f = fakes();
    const call = await start(f);
    f.order.length = 0;
    await call.leave('blocked');
    expect(f.order).toEqual(['manager.dispose', 'room.disconnect', 'worker.terminate', 'lock.release', 'callLeave']);
    expect(f.calls.at(-1)).toEqual({ m: 'callLeave', reason: 'user' });
    expect(call.state.phase).toBe('ended');
  });

  // fix/firefox-misrouted-audio: a misroute the manager recovered is noted for Diagnostics (callNote), while in the call.
  it('notes each recovered misroute for Diagnostics, and none after the leave', async () => {
    const f = fakes();
    const call = await start(f);
    f.manager.emit('misrouteRecovered', BOB_DEV);
    await flush();
    expect(f.calls.filter((c) => c.m === 'callNote')).toEqual([{ m: 'callNote', code: 'E_MEDIA_MISROUTE' }]);
    await call.leave('user');
    f.manager.emit('misrouteRecovered', BOB_DEV);
    await flush();
    expect(f.calls.filter((c) => c.m === 'callNote')).toHaveLength(1);
  });

  it('a denied microphone joins listen-only with mic "denied" and no publish', async () => {
    const f = fakes({ getUserMedia: async () => { throw Object.assign(new Error('no'), { name: 'NotAllowedError' }); } });
    const call = await start(f);
    expect(call.state.mic).toBe('denied');
    expect(call.state.phase).toBe('connected');
    expect(f.room.localParticipant.publishTrack).not.toHaveBeenCalled();
  });

  it('no microphone joins listen-only with mic "missing"', async () => {
    const f = fakes({ getUserMedia: async () => { throw Object.assign(new Error('no'), { name: 'NotFoundError' }); } });
    const call = await start(f);
    expect(call.state.mic).toBe('missing');
    expect(call.state.phase).toBe('connected');
    expect(f.room.localParticipant.publishTrack).not.toHaveBeenCalled();
    const g = fakes({ devices: [] });
    const call2 = await start(g);
    expect(call2.state.mic).toBe('missing');
    expect(g.room.localParticipant.publishTrack).not.toHaveBeenCalled();
    expect(g.gum).not.toHaveBeenCalled();
  });

  it('publishes the microphone with the instance caps', async () => {
    const f = fakes();
    const call = await start(f);
    expect(call.state.mic).toBe('ok');
    expect(f.room.localParticipant.publishTrack).toHaveBeenCalledTimes(1);
    const [, o] = f.room.localParticipant.publishTrack.mock.calls[0];
    expect(o).toMatchObject({ source: 'microphone', dtx: true, red: false, audioPreset: { maxBitrate: 64_000 } });
  });

  it('noise "on" attaches the RNNoise processor in the chosen mode', async () => {
    const f = fakes();
    const made: string[] = [];
    f.deps.createNoiseProcessor = (mode) => { made.push(mode); return { name: 'fake', setPttActive: vi.fn() } as never; };
    await start(f, { noise: 'on', mode: 'ptt' });
    expect(made).toEqual(['ptt']);
    expect(f.localTracks[0].setProcessor).toHaveBeenCalledTimes(1);
  });

  it('task 8c: a tile carries the RTT its device publishes (dilla.rtt), updated on an attribute change; nothing else counts', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    const carol = f.addRemote(CAROL_DEV);
    Object.assign(carol, { attributes: { rtt: '12', 'dilla.vdec': 'vp8' } });
    await f.advance(1_000);
    const tile = (d: string) => call.state.tiles.find((t) => t.deviceId === d)!;
    expect(tile(BOB_DEV).rttMs).toBeNull();
    expect(tile(CAROL_DEV).rttMs).toBeNull();
    Object.assign(bob, { attributes: { 'dilla.rtt': '31' } });
    f.room.emit('participantAttributesChanged', {}, bob);
    expect(tile(BOB_DEV).rttMs).toBe(31);
  });

  it('a tile is "unverified" for a room participant in no roster and "ok" once verifiedIdentities has it', async () => {
    const f = fakes();
    const call = await start(f);
    f.addRemote(BOB_DEV);
    f.addRemote(DEV_D);
    await f.advance(1_000);
    const tile = (d: string) => call.state.tiles.find((t) => t.deviceId === d)!;
    expect(tile(DEV_D).state).toBe('unverified');
    expect(tile(DEV_D).video).toBeNull();
    expect(tile(BOB_DEV).state).toBe('not-heard');
    expect(tile(BOB_DEV).userId).toBe(BOB);
    f.pushKeys(callId, keysFor(2n, [...BASE_ROSTER, { leaf: 3, deviceId: DEV_D, userId: ALICE_USER }]));
    f.verified.add(DEV_D);
    await f.advance(1_000);
    expect(tile(DEV_D).state).toBe('ok');
    expect(tile(DEV_D).userId).toBe(ALICE_USER);
    expect(tile(BOB_DEV).state).toBe('not-heard');
    // A roster member LiveKit has not shown yet is 'connecting'; one's own device is never a tile.
    expect(tile(CAROL_DEV).state).toBe('connecting');
    expect(call.state.tiles.some((t) => t.deviceId === ME)).toBe(false);
    void ALICE;
  });

  it('an ok tile carries its camera stream; an unverified one never does', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    const d = f.addRemote(DEV_D);
    const cam = { id: 'cam' };
    bob.trackPublications.set('v', { source: 'camera', track: { mediaStream: cam } });
    d.trackPublications.set('v', { source: 'camera', track: { mediaStream: { id: 'dcam' } } });
    f.verified.add(BOB_DEV);
    f.verified.add(DEV_D);
    await f.advance(1_000);
    expect(call.state.tiles.find((t) => t.deviceId === BOB_DEV)!.video).toBe(cam);
    expect(call.state.tiles.find((t) => t.deviceId === DEV_D)!.video).toBeNull();
  });

  it('I2 review M5: a roster device not heard yet gets no camera or screen stream until it is verified', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    const cam = { id: 'cam' };
    const scr = { id: 'scr' };
    bob.trackPublications.set('v', { source: 'camera', track: { mediaStream: cam } });
    bob.trackPublications.set('s', { source: 'screen_share', track: { mediaStream: scr } });
    await f.advance(1_000);
    const bobTile = () => call.state.tiles.find((t) => t.deviceId === BOB_DEV)!;
    expect(bobTile().state).toBe('not-heard');
    expect(bobTile().video).toBeNull();
    expect(bobTile().screen).toBeNull();
    f.verified.add(BOB_DEV);
    await f.advance(1_000);
    expect(bobTile().state).toBe('ok');
    expect(bobTile().video).toBe(cam);
    expect(bobTile().screen).toBe(scr);
  });

  it('setPaused (RV-3) stops sending and playing, installs no key, and restores on resume', async () => {
    const f = fakes();
    const call = await start(f, { volumes: { [BOB]: 150 } });
    const bob = f.addRemote(BOB_DEV);
    const carol = f.addRemote(CAROL_DEV);
    bob.trackPublications.set('v', { source: 'camera', track: { mediaStream: { id: 'cam' } } });
    f.verified.add(BOB_DEV);
    await call.setCamera(true);
    expect(call.state.camera).toBe(true);
    await f.advance(1_000);
    expect(call.state.tiles.find((t) => t.deviceId === BOB_DEV)!.video).not.toBeNull();
    const mic = f.localTracks[0];
    f.calls.length = 0;
    await call.setPaused(true);
    expect(mic.muted).toBe(true);
    expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledTimes(1);
    expect(f.calls).toContainEqual({ m: 'callShare', on: false });
    expect(call.state.camera).toBe(false);
    expect(bob.gains.microphone).toBe(0);
    expect(carol.gains.microphone).toBe(0);
    expect(call.state.tiles.every((t) => t.video === null && t.screen === null)).toBe(true);
    await f.advance(1_000);
    expect(call.state.tiles.every((t) => t.video === null)).toBe(true);
    const k2 = keysFor(2n);
    f.pushKeys(callId, k2);
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n]);
    expect(zero(new Uint8Array(k2.baseKey))).toBe(true);
    f.pushKeys(callId, keysFor(3n));
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n]);
    await call.setPaused(false);
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n, 3n]);
    expect(bob.gains.microphone).toBe(1.5);
    expect(carol.gains.microphone).toBe(1);
    expect(mic.muted).toBe(false);
    expect(call.state.camera).toBe(false);
    expect(call.state.paused).toBe(false);
  });

  // WB security I1: the hold after a refused rekey drops the media keys, so nothing more is sent under the old
  // epoch; a key held from before the drop is wiped, and the next epoch's key installs when the hold ends.
  it('dropKeys clears every media key at once and wipes a held key; the next epoch installs on resume', async () => {
    const f = fakes();
    const call = await start(f);
    await call.setPaused(true);
    const held = keysFor(2n);
    f.pushKeys(callId, held);
    call.dropKeys();
    expect(f.manager.clearKeys).toHaveBeenCalledTimes(1);
    f.pushKeys(callId, keysFor(3n));
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n]);
    await call.setPaused(false);
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n, 3n]);
    await call.leave('user');
    call.dropKeys();
    expect(f.manager.clearKeys).toHaveBeenCalledTimes(1);
  });

  it('a muted person stays muted after resume, and camera or screen cannot start while paused', async () => {
    const f = fakes();
    const call = await start(f);
    call.setMuted(true);
    await call.setPaused(true);
    await expect(call.setCamera(true)).rejects.toMatchObject({ code: 'E_UNAVAILABLE' });
    await expect(call.setSharing(true)).rejects.toMatchObject({ code: 'E_UNAVAILABLE' });
    expect(f.calls.some((c) => c.m === 'callShare' && c.on)).toBe(false);
    await call.setPaused(false);
    expect(f.localTracks[0].muted).toBe(true);
    expect(call.state.muted).toBe(true);
  });

  it('deafen mutes every remote GainNode and sends voiceSelf flags 3; undeafen restores stored volumes', async () => {
    const f = fakes();
    const call = await start(f, { volumes: { [BOB]: 150 } });
    const bob = f.addRemote(BOB_DEV);
    const carol = f.addRemote(CAROL_DEV);
    await f.advance(1_000);
    expect(bob.gains.microphone).toBe(1.5);
    call.setDeafened(true);
    expect(bob.gains.microphone).toBe(0);
    expect(carol.gains.microphone).toBe(0);
    expect(f.calls.at(-1)).toEqual({ m: 'voiceSelf', callId, flags: 3 });
    expect(call.state.muted).toBe(true);
    expect(call.state.deafened).toBe(true);
    call.setDeafened(false);
    expect(bob.gains.microphone).toBe(1.5);
    expect(carol.gains.microphone).toBe(1);
    expect(call.state.muted).toBe(false);
    expect(f.calls.at(-1)).toEqual({ m: 'voiceSelf', callId, flags: 0 });
  });

  it('mute sends voiceSelf flags 1 after callJoin, never before, and mutes the mic track', async () => {
    const f = fakes();
    const call = await start(f);
    expect(f.calls.some((c) => c.m === 'voiceSelf')).toBe(false);
    call.setMuted(true);
    expect(f.calls.at(-1)).toEqual({ m: 'voiceSelf', callId, flags: 1 });
    expect(f.localTracks[0].muted).toBe(true);
    expect(f.calls.findIndex((c) => c.m === 'callJoin')).toBeLessThan(f.calls.findIndex((c) => c.m === 'voiceSelf'));
  });

  it('setVolume stores 0..200 and drives that person’s gain', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    await f.advance(1_000);
    call.setVolume(BOB, 50);
    expect(bob.gains.microphone).toBe(0.5);
    expect(bob.gains.screen_share_audio).toBe(1); // T5: the voice volume does not move the stream
    expect(() => call.setVolume(BOB, 55)).toThrow(/E_INVALID/);
    expect(() => call.setVolume(BOB, 55, 'stream')).toThrow(/E_INVALID/);
  });

  it('push to talk mutes the microphone unless the key is held', async () => {
    const f = fakes();
    const call = await start(f, { mode: 'ptt' });
    const mic = f.localTracks[0];
    expect(mic.muted).toBe(true);
    call.pushToTalk(true);
    expect(mic.muted).toBe(false);
    call.pushToTalk(false);
    expect(mic.muted).toBe(true);
    const g = fakes();
    const vad = await start(g);
    vad.pushToTalk(false);
    expect(g.localTracks[0].muted).toBe(false);
  });

  // Task-9 review I2: the input mode follows Settings → Voice during a call; push to talk closes the microphone at once.
  it('setMode("ptt") closes the microphone at once; the key then opens it; setMode("vad") reopens it', async () => {
    const f = fakes();
    const call = await start(f);
    const mic = f.localTracks[0];
    expect(mic.muted).toBe(false);
    call.setMode('ptt');
    expect(mic.muted).toBe(true);
    expect(call.state.mode).toBe('ptt');
    call.pushToTalk(true);
    expect(mic.muted).toBe(false);
    call.pushToTalk(false);
    call.setMode('vad');
    expect(mic.muted).toBe(false);
    expect(call.state.mode).toBe('vad');
    // A muted person stays muted across a mode change.
    call.setMuted(true);
    call.setMode('ptt');
    call.setMode('vad');
    expect(mic.muted).toBe(true);
  });

  it('with RNNoise built for voice activity, push to talk gates the track; built for push to talk, voice activity holds it open', async () => {
    const f = fakes();
    const made: { mode: string; ptt: boolean[] }[] = [];
    f.deps.createNoiseProcessor = (mode) => { const p = { mode, ptt: [] as boolean[] }; made.push(p); return { setPttActive: (a: boolean) => { p.ptt.push(a); } }; };
    const call = await start(f, { noise: 'on' });
    const mic = f.localTracks[0];
    call.setMode('ptt');
    expect(mic.muted).toBe(true);
    call.pushToTalk(true);
    expect(mic.muted).toBe(false);
    const g = fakes();
    const made2: boolean[] = [];
    g.deps.createNoiseProcessor = () => ({ setPttActive: (a: boolean) => { made2.push(a); } });
    const ptt = await start(g, { noise: 'on', mode: 'ptt' });
    ptt.setMode('vad');
    expect(made2.at(-1)).toBe(true);
    expect(g.localTracks[0].muted).toBe(false);
  });

  // Queued from the task-8b review (N2, N6): a pagehide while joinCall is still running.
  it('a pagehide while joinCall is pending zero-fills every key copy, installs nothing, releases the session and ends E_ABORTED', async () => {
    const f = fakes();
    const copies: Uint8Array[] = [];
    f.deps.copyKey = (buffer) => { const c = new Uint8Array(buffer.slice(0)); copies.push(c); return c; };
    let resolveJoin: () => void = () => undefined;
    const released = vi.fn(async () => undefined);
    f.deps.joinCall = vi.fn(() => new Promise((resolve) => {
      resolveJoin = () => resolve({ room: f.room, manager: f.manager, release: released });
    })) as unknown as PageCallDeps['joinCall'];
    const outcome = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts).catch((e: unknown) => e);
    await flush();
    expect(f.deps.joinCall).toHaveBeenCalledTimes(1);
    // A newer key arrives while joinCall runs, and is held for after the join.
    const newer = keysFor(2n);
    const pushed = new Uint8Array(newer.baseKey);
    f.pushKeys(callId, newer);
    window.dispatchEvent(new Event('pagehide'));
    resolveJoin();
    expect(await outcome).toMatchObject({ code: 'E_ABORTED' });
    expect(copies.length).toBeGreaterThanOrEqual(2);
    expect(copies.every(zero)).toBe(true);
    expect(zero(pushed)).toBe(true);
    expect(f.manager.installEpoch).not.toHaveBeenCalled();
    expect(released).toHaveBeenCalled();
    expect(f.calls.filter((c) => c.m === 'callLeave')).toEqual([{ m: 'callLeave', reason: 'pagehide' }]);
  });

  it('refreshes ICE 300 s before the credential expiry the username carries', async () => {
    const f = fakes({ iceUsername: '1700003600:dev:1700000000', now: 1_700_000_000_000 });
    await start(f);
    await f.advance(3_300_000 - 1);
    expect(f.calls.some((c) => c.m === 'callRefresh')).toBe(false);
    await f.advance(1);
    expect(f.calls.filter((c) => c.m === 'callRefresh')).toHaveLength(1);
    expect(f.room.engine.rtcConfig.iceServers).toEqual([{ urls: ['turn:sfu.example:3478'], username: '1700007200:dev:1700003300', credential: 'cred2' }]);
    // The next refresh follows the new expiry: 1700007200 − 300 s.
    await f.advance(3_600_000 - 1);
    expect(f.calls.filter((c) => c.m === 'callRefresh')).toHaveLength(1);
    await f.advance(1);
    expect(f.calls.filter((c) => c.m === 'callRefresh')).toHaveLength(2);
  });

  it('keeps one coarse report in flight when the worker is slow, and drops missed ticks', async () => {
    const f = fakes();
    const original = f.deps.bridge.call.bind(f.deps.bridge);
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let started = 0;
    f.deps.bridge.call = command => {
      if (command.m === 'callStats') { started++; return gate.then(() => original(command)); }
      return original(command);
    };
    await start(f);
    await f.advance(90_000);
    expect(started).toBe(1);
    release(); await flush();
    await f.advance(10_000);
    expect(started).toBe(2);
  });

  it('sends one callStats report per 10 s while connected', async () => {
    const f = fakes({ statsError: Object.assign(new Error('E_RATE_LIMITED'), { code: 'E_RATE_LIMITED', status: 429 }) });
    const call = await start(f);
    await f.advance(30_000);
    expect(f.calls.filter((c) => c.m === 'callStats')).toHaveLength(3);
    expect(call.state.error).toBeNull();
    f.room.emit('reconnecting');
    expect(call.state.phase).toBe('reconnecting');
    await f.advance(10_000);
    expect(f.calls.filter((c) => c.m === 'callStats')).toHaveLength(3);
    f.room.emit('reconnected');
    expect(call.state.phase).toBe('connected');
    await f.advance(10_000);
    expect(f.calls.filter((c) => c.m === 'callStats')).toHaveLength(4);
    const report = f.calls.find((c) => c.m === 'callStats') as Extract<Command, { m: 'callStats' }>;
    expect(report.report).toHaveLength(6);
    expect(call.state.error).toBeNull();
  });

  /** MP5: a call whose peer connections report what the test sets; `stats.pub` or `stats.sub` may be a function that throws or hangs. */
  const linkFakes = () => {
    const f = fakes();
    const s = {
      sent: 0, received: 0, rtt: 0.024,
      pub: undefined as undefined | (() => Promise<Map<string, unknown>>),
      sub: undefined as undefined | (() => Promise<Map<string, unknown>>),
    };
    const pc = f.room.engine.pcManager as unknown as { publisher: { getStats: () => Promise<Map<string, unknown>> }; subscriber: { getStats: () => Promise<Map<string, unknown>> } };
    pc.publisher.getStats = () => s.pub?.() ?? Promise.resolve(new Map<string, unknown>([
      ['T', { id: 'T', type: 'transport', selectedCandidatePairId: 'P' }],
      ['P', { id: 'P', type: 'candidate-pair', localCandidateId: 'L', currentRoundTripTime: s.rtt }],
      ['L', { id: 'L', type: 'local-candidate', candidateType: 'host' }],
      ['O', { id: 'O', type: 'outbound-rtp', bytesSent: s.sent }],
    ]));
    pc.subscriber = { getStats: () => s.sub?.() ?? Promise.resolve(new Map<string, unknown>([['I', { id: 'I', type: 'inbound-rtp', bytesReceived: s.received }]])) };
    return { f, s };
  };

  it('MP5: a local 2 s poll publishes call.state.link while connected, apart from the 10 s server report; the first read is a baseline; leave clears it', async () => {
    const { f, s } = linkFakes();
    const call = await start(f);
    expect(call.state.link).toBeUndefined();
    await f.advance(2_000);
    expect(call.state.link).toBeUndefined(); // the baseline: no sample, the dock keeps "— ms"
    s.sent = 12_000;
    s.received = 24_000;
    await f.advance(2_000);
    expect(call.state.link).toEqual({ rttMs: 24, upKbps: 48, downKbps: 96, lossPct: 0 });
    expect(f.calls.filter((c) => c.m === 'callStats')).toHaveLength(0); // the server report is still every 10 s
    f.room.emit('reconnecting');
    expect(call.state.link).toBeUndefined();
    f.room.emit('reconnected');
    await f.advance(2_000);
    expect(call.state.link).toBeUndefined(); // a fresh baseline after every reconnect
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ rttMs: 24, upKbps: 0, downKbps: 0 });
    await call.leave('user');
    expect(call.state.link).toBeUndefined();
  });

  // Task 8c fix round 2: this device's own RTT goes to the instance (callRtt), only when it moved by RTT_STEP_MS or more, and
  // is cleared while the call reconnects; no LiveKit attribute is written by the page.
  it('task 8c: the own RTT is reported through the worker, rounded, on a change of 5 ms or more, and cleared on reconnecting', async () => {
    const { f, s } = linkFakes();
    const rtts = () => f.calls.filter((c) => c.m === 'callRtt').map((c) => (c as { ms: number | null }).ms);
    await start(f);
    await f.advance(2_000); // the baseline: no sample, nothing reported
    expect(rtts()).toEqual([]);
    await f.advance(2_000);
    expect(rtts()).toEqual([24]);
    s.rtt = 0.0274;
    await f.advance(2_000);
    expect(rtts()).toEqual([24]); // 27 is within 5 ms of 24
    s.rtt = 0.0312;
    await f.advance(2_000);
    expect(rtts()).toEqual([24, 31]);
    s.rtt = 9;
    await f.advance(2_000);
    expect(rtts()).toEqual([24, 31, 5000]); // clamped to the range the instance accepts
    f.room.emit('reconnecting');
    await f.advance(2_000);
    expect(rtts()).toEqual([24, 31, 5000, null]);
    await f.advance(2_000);
    expect(rtts()).toEqual([24, 31, 5000, null]); // one clear
  });

  // Task 11 (8c minor): a report the instance refused (false: 429 or 404) is not counted as sent, and a reconnect that no
  // tick saw still re-sends the RTT.
  it('task 11: a refused RTT report is sent again on the next sample, and a fast reconnect re-sends it', async () => {
    const { f, s } = linkFakes();
    const rtts = () => f.calls.filter((c) => c.m === 'callRtt').map((c) => (c as { ms: number | null }).ms);
    await start(f);
    await f.advance(2_000);
    f.rttAnswers.push(false);
    await f.advance(2_000);
    expect(rtts()).toEqual([24]);
    await f.advance(2_000);
    expect(rtts()).toEqual([24, 24]); // refused, so the same value goes again
    await f.advance(2_000);
    expect(rtts()).toEqual([24, 24]); // stored: nothing new within 5 ms
    f.room.emit('reconnecting');
    f.room.emit('reconnected');
    s.rtt = 0.026;
    await f.advance(4_000); // the first read after a reconnect is a fresh baseline
    expect(rtts()).toEqual([24, 24, 26]); // the reconnect forgot what was sent
  });

  it('MP5: a failed getStats keeps the previous totals, so the next good read is no spike', async () => {
    const { f, s } = linkFakes();
    s.sent = 10_000_000;
    const call = await start(f);
    await f.advance(2_000);
    s.pub = () => Promise.reject(new Error('closed'));
    await f.advance(2_000);
    s.pub = undefined;
    s.sent = 10_000_000 + 8_000;
    await f.advance(2_000);
    // the publisher missed one interval: 8000 bytes over 4 s = 16 kb/s, not the whole 10 MB counter
    expect(call.state.link).toMatchObject({ upKbps: 16 });
  });

  it('L5: a read with no round-trip time clears the link (the dock says "— ms") instead of keeping the last numbers', async () => {
    const { f, s } = linkFakes();
    const call = await start(f);
    await f.advance(2_000);
    s.sent = 12_000;
    s.received = 24_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ rttMs: 24, upKbps: 48 });
    s.rtt = undefined as unknown as number; // the selected pair has no RTT, as during an ICE restart
    s.sent = 24_000;
    await f.advance(2_000);
    expect(call.state.link).toBeUndefined();
    s.rtt = 0.03;
    s.sent = 36_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ rttMs: 30, upKbps: 48 });
  });

  it('L5: when one direction cannot be read it keeps its previous value for one tick, then shows nothing for it; the other direction stays live', async () => {
    const { f, s } = linkFakes();
    const call = await start(f);
    await f.advance(2_000);
    s.sent = 12_000;
    s.received = 24_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ upKbps: 48, downKbps: 96 });
    s.pub = () => Promise.reject(new Error('closed'));
    s.sub = () => Promise.resolve(new Map<string, unknown>([ // the subscriber alone still gives the ping
      ['T', { id: 'T', type: 'transport', selectedCandidatePairId: 'P' }],
      ['P', { id: 'P', type: 'candidate-pair', localCandidateId: 'L', currentRoundTripTime: s.rtt }],
      ['L', { id: 'L', type: 'local-candidate', candidateType: 'host' }],
      ['I', { id: 'I', type: 'inbound-rtp', bytesReceived: s.received }],
    ]));
    s.received = 36_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ upKbps: 48, downKbps: 48 }); // up is last tick's value, once
    s.received = 60_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ upKbps: null, downKbps: 96 });
    s.pub = undefined;
    s.sent = 36_000;
    s.received = 84_000;
    await f.advance(2_000);
    expect(call.state.link?.upKbps).toBe(Math.round(24_000 * 8 / 6_000)); // the lost ticks are spanned, not a spike
  });

  it('FD1: with no subscriber connection (nothing subscribed) the down rate is 0, not a dash', async () => {
    const { f, s } = linkFakes();
    (f.room.engine.pcManager as unknown as { subscriber?: unknown }).subscriber = undefined;
    const call = await start(f);
    await f.advance(2_000);
    s.sent = 12_000;
    await f.advance(2_000);
    expect(call.state.link).toEqual({ rttMs: 24, upKbps: 48, downKbps: 0, lossPct: 0 });
  });

  it('FD1: a subscriber getStats that rejects is no read: the down rate is kept one tick, then a dash', async () => {
    const { f, s } = linkFakes();
    const call = await start(f);
    await f.advance(2_000);
    s.received = 24_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ downKbps: 96 });
    s.sub = () => Promise.reject(new Error('closed'));
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ downKbps: 96 });
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ downKbps: null });
  });

  it('FD1: single-peer-connection inbound on the publisher report is the down rate, counted once', async () => {
    const { f, s } = linkFakes();
    (f.room.engine.pcManager as unknown as { subscriber?: unknown }).subscriber = undefined;
    s.pub = () => Promise.resolve(new Map<string, unknown>([
      ['T', { id: 'T', type: 'transport', selectedCandidatePairId: 'P' }],
      ['P', { id: 'P', type: 'candidate-pair', localCandidateId: 'L', currentRoundTripTime: s.rtt }],
      ['L', { id: 'L', type: 'local-candidate', candidateType: 'host' }],
      ['O', { id: 'O', type: 'outbound-rtp', bytesSent: s.sent }],
      ['I', { id: 'I', type: 'inbound-rtp', bytesReceived: s.received }],
    ]));
    const call = await start(f);
    await f.advance(2_000);
    s.sent = 12_000;
    s.received = 24_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ upKbps: 48, downKbps: 96 });
  });

  it('MP5: the poll stops while the call is paused and restarts with a fresh baseline', async () => {
    const { f, s } = linkFakes();
    const call = await start(f);
    await f.advance(2_000);
    s.sent = 2_000;
    await f.advance(2_000);
    expect(call.state.link).toBeDefined();
    const reads = vi.fn(f.room.engine.pcManager.publisher.getStats);
    f.room.engine.pcManager.publisher.getStats = reads;
    await call.setPaused(true);
    await f.advance(5_000); // under the 10 s server report, which reads the publisher too
    expect(reads).not.toHaveBeenCalled();
    expect(call.state.link).toBeUndefined();
    await call.setPaused(false);
    await f.advance(2_000);
    expect(call.state.link).toBeUndefined();
    s.sent = 4_000;
    await f.advance(2_000);
    expect(call.state.link).toMatchObject({ upKbps: 8 });
  });

  it('MP5: an unchanged sample keeps the state as it is (no new object, no re-render)', async () => {
    const { f } = linkFakes();
    const call = await start(f);
    await f.advance(4_000);
    const first = call.state.link;
    expect(first).toBeDefined();
    await f.advance(6_000);
    expect(call.state.link).toBe(first);
  });

  it('MP5: a getStats that does not settle in 5 s shows no stats and starts no second read', async () => {
    const { f, s } = linkFakes();
    const call = await start(f);
    await f.advance(2_000);
    s.sent = 2_000;
    await f.advance(2_000);
    expect(call.state.link).toBeDefined();
    let calls = 0;
    s.pub = () => { calls += 1; return new Promise<Map<string, unknown>>(() => undefined); };
    await f.advance(3_000); // the tick at 6 s starts the hanging read, the next ticks find it busy
    expect(calls).toBe(1);
    expect(call.state.link).toBeDefined();
    await f.advance(5_000); // 6 s since the read started
    expect(calls).toBeLessThanOrEqual(2); // the second is the 10 s server report, not a link read
    expect(call.state.link).toBeUndefined();
  });

  it('E_CALL_IN_OTHER_TAB ends the attempt with that code and calls callLeave', async () => {
    const f = fakes({ joinCallError: new Error('E_CALL_IN_OTHER_TAB') });
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toMatchObject({ code: 'E_CALL_IN_OTHER_TAB' });
    expect(f.calls).toContainEqual({ m: 'callLeave', reason: 'user' });
    expect(zero(f.joinOptions!.epoch.baseKey)).toBe(true);
    // No listener is left behind: a later key is never installed.
    f.pushKeys(callId, keysFor(2n));
    expect(f.manager.installs).toEqual([]);
  });

  it('asks for the microphone before callJoin and stops it when the join fails', async () => {
    const stop = vi.fn();
    const f = fakes({ joinCallError: new Error('E_CALL_IN_OTHER_TAB'),
      getUserMedia: async () => ({ getTracks: () => [], getAudioTracks: () => [{ kind: 'audio', stop }], getVideoTracks: () => [] }) as unknown as MediaStream });
    const asked: string[] = [];
    f.gum.mockImplementationOnce(async () => {
      asked.push(`gum after ${f.order.length} bridge steps`);
      return { getTracks: () => [], getAudioTracks: () => [{ kind: 'audio', stop }], getVideoTracks: () => [] } as unknown as MediaStream;
    });
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toMatchObject({ code: 'E_CALL_IN_OTHER_TAB' });
    expect(asked).toEqual(['gum after 0 bridge steps']);
    expect(stop).toHaveBeenCalled();
  });

  it('pagehide during startup posts callLeave("pagehide"), releases the room and ends the attempt (review m1)', async () => {
    const f = fakes();
    let unblock: () => void = () => undefined;
    f.room.localParticipant.publishTrack.mockImplementationOnce(() => new Promise((resolve) => {
      unblock = () => resolve({ source: 'microphone', track: fakeLocalTrack('audio'), trackSid: 'TR_X' });
    }));
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts);
    const outcome = started.catch((e: unknown) => e);
    await flush();
    expect(f.room.localParticipant.publishTrack).toHaveBeenCalledTimes(1);
    window.dispatchEvent(new Event('pagehide'));
    expect(f.calls).toContainEqual({ m: 'callLeave', reason: 'pagehide' });
    expect(f.room.disconnect).toHaveBeenCalled();
    expect(f.manager.dispose).toHaveBeenCalled();
    f.pushKeys(callId, keysFor(2n));
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n]);
    unblock();
    expect(await outcome).toMatchObject({ code: 'E_ABORTED' });
    expect(f.order).toContain('lock.release');
    expect(f.calls.filter((c) => c.m === 'callLeave')).toHaveLength(1);
    expect(f.timers.size).toBe(0);
    // The listener is gone with the attempt.
    window.dispatchEvent(new Event('pagehide'));
    expect(f.calls.filter((c) => c.m === 'callLeave')).toHaveLength(1);
  });

  it('a failure after joinCall resolved tears the call down and posts callLeave (review m1)', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => { throw new Error('E_WASM: no worklet'); };
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on' })).rejects.toMatchObject({ code: 'E_WASM' });
    expect(f.order).toEqual(expect.arrayContaining(['manager.dispose', 'room.disconnect', 'worker.terminate', 'lock.release']));
    expect(f.order.indexOf('lock.release')).toBeLessThan(f.order.lastIndexOf('callLeave'));
    expect(f.calls.at(-1)).toEqual({ m: 'callLeave', reason: 'user' });
    f.pushKeys(callId, keysFor(2n));
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n]);
    window.dispatchEvent(new Event('pagehide'));
    expect(f.calls.filter((c) => c.m === 'callLeave')).toHaveLength(1);
  });

  it('a microphone that cannot be published joins listen-only with mic "failed" and E_MIC_PUBLISH (review m4)', async () => {
    const f = fakes();
    f.room.localParticipant.publishTrack.mockImplementationOnce(async () => { throw new Error('publish timed out'); });
    const call = await start(f);
    expect(call.state.mic).toBe('failed');
    expect(call.state.error).toBe('E_MIC_PUBLISH');
    expect(call.state.phase).toBe('connected');
  });

  it('a refused callJoin rejects with its code, posts no callLeave, and leaves no key listener', async () => {
    const f = fakes();
    f.deps.bridge.call = vi.fn(async (cmd: Command) => {
      f.calls.push(cmd);
      if (cmd.m === 'callJoin') throw Object.assign(new Error('E_CALL_FULL'), { code: 'E_CALL_FULL' });
      return null;
    });
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toMatchObject({ code: 'E_CALL_FULL' });
    expect(f.calls.map((c) => c.m)).toEqual(['callJoin']);
    expect(f.deps.joinCall).not.toHaveBeenCalled();
  });

  it('a join answer without its key fails closed with E_NO_EPOCH and leaves', async () => {
    const f = fakes({ joinEpoch: 2n });
    const original = f.deps.bridge.call;
    f.deps.bridge.call = vi.fn(async (cmd: Command) => {
      if (cmd.m === 'callJoin') {
        const r = (await original(cmd)) as CallJoinResult;
        return { ...r, epoch: '3' };
      }
      return original(cmd);
    });
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toMatchObject({ code: 'E_NO_EPOCH' });
    expect(f.deps.joinCall).not.toHaveBeenCalled();
    expect(f.calls).toContainEqual({ m: 'callLeave', reason: 'user' });
  });

  it('pagehide posts callLeave("pagehide") and disconnects without awaiting the answer', async () => {
    const f = fakes();
    const call = await start(f);
    let settled = false;
    f.deps.bridge.call = vi.fn((cmd: Command) => {
      f.calls.push(cmd);
      return new Promise(() => undefined).finally(() => { settled = true; });
    });
    window.dispatchEvent(new Event('pagehide'));
    expect(f.calls).toContainEqual({ m: 'callLeave', reason: 'pagehide' });
    expect(f.room.disconnect).toHaveBeenCalled();
    expect(f.manager.dispose).toHaveBeenCalled();
    expect(settled).toBe(false);
    f.pushKeys(callId, keysFor(2n));
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n]);
    expect(call.state.phase).toBe('ended');
  });

  it('camera: callShare on, then the permission, then the publish; off sends callShare off', async () => {
    const f = fakes();
    const call = await start(f);
    f.order.length = 0;
    f.room.localParticipant.publishTrack.mockClear();
    const permissionSeen: number[] = [];
    f.room.on('participantPermissionsChanged', () => permissionSeen.push(f.room.localParticipant.publishTrack.mock.calls.length));
    await call.setCamera(true);
    expect(f.order.indexOf('callShare')).toBe(0);
    expect(permissionSeen).toEqual([0]);
    expect(f.room.localParticipant.publishTrack).toHaveBeenCalledTimes(1);
    expect(f.room.localParticipant.publishTrack.mock.calls[0][1]).toMatchObject({ source: 'camera', videoCodec: 'vp8' });
    await call.setSharing(true);
    expect(f.calls.filter((c) => c.m === 'callShare')).toHaveLength(1);
    await call.setCamera(false);
    expect(f.calls.filter((c) => c.m === 'callShare')).toHaveLength(1);
    await call.setSharing(false);
    expect(f.calls.filter((c) => c.m === 'callShare').at(-1)).toEqual({ m: 'callShare', on: false });
    expect(call.state.camera).toBe(false);
    expect(call.state.sharing).toBe(false);
  });

  it('a share permission that never comes fails E_UNAVAILABLE after 10 s and sends callShare off', async () => {
    const f = fakes();
    const call = await start(f);
    f.deps.bridge.call = vi.fn(async (cmd: Command) => { f.calls.push(cmd); return null; });
    const p = call.setCamera(true);
    const caught = p.catch((e: unknown) => e);
    await flush();
    await f.advance(10_000);
    expect(await caught).toMatchObject({ code: 'E_UNAVAILABLE' });
    expect(f.calls.at(-1)).toEqual({ m: 'callShare', on: false });
    expect(call.state.camera).toBe(false);
  });

  it('an unexpected room disconnect ends the call and posts callLeave("lost") (WB client I3)', async () => {
    const f = fakes();
    const call = await start(f);
    f.room.emit('disconnected');
    await flush();
    expect(call.state.phase).toBe('ended');
    expect(call.state.error).toBe('E_DISCONNECTED');
    expect(f.calls).toContainEqual({ m: 'callLeave', reason: 'lost' });
    f.pushKeys(callId, keysFor(2n));
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n]);
  });

  it('subscribe() reports state changes and leave() stops every timer and listener', async () => {
    const f = fakes();
    const call = await start(f);
    const seen: boolean[] = [];
    const off = call.subscribe(() => seen.push(call.state.muted));
    call.setMuted(true);
    off();
    call.setMuted(false);
    expect(seen).toEqual([true]);
    await call.leave('user');
    expect(f.timers.size).toBe(0);
    expect(f.room.count('participantPermissionsChanged')).toBe(0);
    await call.leave('user');
    expect(f.calls.filter((c) => c.m === 'callLeave')).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------------------------
// Whole-branch fix wave (wb-client.md I1, I2, I3, m2, m3).
// ---------------------------------------------------------------------------------------------

function videoTrack(): MediaStreamTrack & { enabled: boolean; stopped: boolean } {
  const t = { kind: 'video', enabled: true, stopped: false, stop() { t.stopped = true; }, addEventListener: () => undefined, removeEventListener: () => undefined };
  return t as unknown as MediaStreamTrack & { enabled: boolean; stopped: boolean };
}
const streamOf = (video: MediaStreamTrack) => ({ getTracks: () => [video], getAudioTracks: () => [], getVideoTracks: () => [video] }) as unknown as MediaStream;
const later = <T,>() => {
  let resolve: (v: T) => void = () => undefined;
  let reject: (e: unknown) => void = () => undefined;
  const promise = new Promise<T>((ok, no) => { resolve = ok; reject = no; });
  return { promise, resolve, reject };
};

describe('the whole-branch fix wave', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };

  // I1: a join is cancellable from the page, the microphone prompt included.
  it('an abort while the microphone prompt is open ends the start at once; the late track is stopped; nothing is joined', async () => {
    const f = fakes();
    const mic = later<MediaStream>();
    const track = { kind: 'audio', stop: vi.fn() };
    f.gum.mockImplementationOnce(() => mic.promise);
    const abort = new AbortController();
    const outcome = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, signal: abort.signal }).catch((e: unknown) => e);
    await flush();
    abort.abort();
    expect(await outcome).toMatchObject({ code: 'E_ABORTED' });
    expect(f.calls.some((c) => c.m === 'callJoin')).toBe(false);
    mic.resolve({ getTracks: () => [track], getAudioTracks: () => [track], getVideoTracks: () => [] } as unknown as MediaStream);
    await flush();
    expect(track.stop).toHaveBeenCalled();
  });

  it('an abort while callJoin is pending posts callLeave("user") at once, stops the microphone and ends E_ABORTED', async () => {
    const f = fakes();
    const stop = vi.fn();
    f.gum.mockImplementationOnce(async () => ({ getTracks: () => [], getAudioTracks: () => [{ kind: 'audio', stop }], getVideoTracks: () => [] }) as unknown as MediaStream);
    const answer = later<unknown>();
    const original = f.deps.bridge.call;
    f.deps.bridge.call = vi.fn((cmd: Command) => (cmd.m === 'callJoin' ? (f.calls.push(cmd), answer.promise) : original(cmd)));
    const abort = new AbortController();
    const outcome = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, signal: abort.signal }).catch((e: unknown) => e);
    await flush();
    expect(f.calls.map((c) => c.m)).toEqual(['callJoin']);
    abort.abort();
    expect(f.calls).toContainEqual({ m: 'callLeave', reason: 'user' });
    expect(stop).toHaveBeenCalled();
    answer.reject(Object.assign(new Error('E_NO_CALL'), { code: 'E_NO_CALL' }));
    expect(await outcome).toMatchObject({ code: 'E_ABORTED' });
    expect(f.deps.joinCall).not.toHaveBeenCalled();
  });

  // I2: RV-3 says nothing is sent while paused. The camera and screen stop sending at once, even while a camera or
  // share operation holds the lane (the screen picker left open), and an operation that finishes during the pause
  // publishes nothing.
  it('a pause disables a published camera at once while the screen picker is open, and the share then publishes nothing', async () => {
    const f = fakes();
    const call = await start(f);
    const cam = videoTrack();
    f.gum.mockImplementationOnce(async () => streamOf(cam));
    await call.setCamera(true);
    expect(call.state.camera).toBe(true);
    const picker = later<MediaStream>();
    f.deps.media.getDisplayMedia = vi.fn(() => picker.promise);
    const sharing = call.setSharing(true).catch((e: unknown) => e);
    await flush();
    expect(f.deps.media.getDisplayMedia).toHaveBeenCalled();
    const paused = call.setPaused(true);
    expect(cam.enabled).toBe(false);
    const screen = videoTrack();
    picker.resolve(streamOf(screen));
    await paused;
    expect(await sharing).toMatchObject({ code: 'E_UNAVAILABLE' });
    expect(f.room.localParticipant.publishTrack.mock.calls.filter(([, o]) => o.source === 'screen_share')).toHaveLength(0);
    expect(screen.stopped).toBe(true);
    expect(cam.stopped).toBe(true);
    expect(call.state.camera).toBe(false);
    expect(call.state.sharing).toBe(false);
  });

  it('a camera whose publish was in flight when the pause came is unpublished and stopped', async () => {
    const f = fakes();
    const call = await start(f);
    f.share(true);
    const cam = videoTrack();
    f.gum.mockImplementationOnce(async () => streamOf(cam));
    const publish = later<unknown>();
    f.room.localParticipant.publishTrack.mockImplementationOnce(() => publish.promise as never);
    const camera = call.setCamera(true).catch((e: unknown) => e);
    await flush();
    void call.setPaused(true);
    publish.resolve({ source: 'camera', track: fakeLocalTrack('video'), trackSid: 'TR_C' });
    expect(await camera).toMatchObject({ code: 'E_UNAVAILABLE' });
    expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledWith(cam);
    expect(cam.stopped).toBe(true);
    expect(call.state.camera).toBe(false);
  });

  // I3: the session says why it ended on its own, and tells the worker it was lost.
  it.each([[5, 'E_CALL_ENDED'], [4, 'E_REMOVED'], [1, 'E_DISCONNECTED'], [undefined, 'E_DISCONNECTED']] as const)(
    'a disconnect with LiveKit reason %s ends the call with %s', async (reason, code) => {
      const f = fakes();
      const call = await start(f);
      f.room.emit('disconnected', reason);
      await flush();
      expect(call.state).toMatchObject({ phase: 'ended', error: code });
      expect(f.calls.filter((c) => c.m === 'callLeave')).toEqual([{ m: 'callLeave', reason: 'lost' }]);
    });

  // m2: the room's own end is heard from the moment joinCall resolves, not only after the microphone is published.
  it('a disconnect while the microphone is being published ends the start; it never reports connected', async () => {
    const f = fakes();
    const publish = later<unknown>();
    f.room.localParticipant.publishTrack.mockImplementationOnce(() => publish.promise as never);
    const outcome = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts).catch((e: unknown) => e);
    await flush();
    f.room.emit('disconnected', 1);
    publish.resolve({ source: 'microphone', track: fakeLocalTrack('audio'), trackSid: 'TR_M' });
    const r = await outcome;
    expect(r).toMatchObject({ code: 'E_DISCONNECTED' });
    expect(f.calls).toContainEqual({ m: 'callLeave', reason: 'user' });
    expect(f.order).toContain('lock.release');
  });

  // m3: a leave while a share waits for the SFU's permission settles the waiting camera (no promise kept forever).
  it('a leave while a share waits for its permission settles setCamera', async () => {
    const f = fakes();
    const call = await start(f);
    f.deps.bridge.call = vi.fn(async (cmd: Command) => { f.calls.push(cmd); return null; });
    const camera = call.setCamera(true).then(() => 'resolved', (e: unknown) => e);
    await flush();
    await call.leave('user');
    await flush();
    expect(await Promise.race([camera, new Promise((r) => setTimeout(() => r('pending'), 50))])).toMatchObject({ code: 'E_UNAVAILABLE' });
    expect(f.room.count('participantPermissionsChanged')).toBe(0);
  });
});

// ---------------------------------------------------------------------------------------------
// Whole-branch fix wave 2 (review/wb-product-walk.md I3, M1, M4).
// ---------------------------------------------------------------------------------------------

describe('the product walk fixes', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };
  const withViews = (f: ReturnType<typeof fakes>) => {
    f.deps.viewOf = (track) => ({ id: 'view', track }) as unknown as MediaStream;
    return f;
  };

  it('I3: the own camera and screen are in the state as local views, gone when they stop and at the leave', async () => {
    const f = withViews(fakes());
    const cam = videoTrack();
    const screenTrack = videoTrack();
    f.deps.media.getUserMedia = vi.fn(async (c: MediaStreamConstraints) => (c.video !== undefined && c.video !== false ? streamOf(cam) : {
      getTracks: () => [], getAudioTracks: () => [{ kind: 'audio', stop: () => undefined }], getVideoTracks: () => [] }) as unknown as MediaStream);
    f.deps.media.getDisplayMedia = vi.fn(async () => streamOf(screenTrack));
    const call = await start(f);
    expect(call.state.localCamera).toBeNull();
    await call.setCamera(true);
    expect((call.state.localCamera as unknown as { track: unknown }).track).toBe(cam);
    await call.setSharing(true);
    expect((call.state.localScreen as unknown as { track: unknown }).track).toBe(screenTrack);
    await call.setCamera(false);
    expect(call.state.localCamera).toBeNull();
    expect(call.state.localScreen).not.toBeNull();
    await call.leave('user');
    expect(call.state.localScreen).toBeNull();
  });

  describe('web3.8 V1: speaking follows the audio itself', () => {
    /** Fake levels: a meter per track, its RMS set by the test; every meter's close() is counted. */
    const meters = () => {
      const levels = new Map<unknown, number>();
      const made: Array<{ track: unknown; close: ReturnType<typeof vi.fn> }> = [];
      const create = vi.fn((_ctx: AudioContext, track: MediaStreamTrack) => {
        const close = vi.fn();
        made.push({ track, close });
        return { read: () => levels.get(track) ?? 0, close };
      });
      return { levels, made, create };
    };
    const withMeters = (f: ReturnType<typeof fakes>) => {
      const m = meters();
      f.deps.createLevelMeter = m.create;
      return m;
    };
    const ownTrack = (f: ReturnType<typeof fakes>) => f.localTracks[0].mediaStreamTrack;
    const tileOf = (call: PageCall, id: string) => call.state.tiles.find((t) => t.deviceId === id)!;
    const heard = (f: ReturnType<typeof fakes>, id = BOB_DEV) => {
      const p = f.addRemote(id);
      f.verified.add(id);
      const { track, pub } = f.subscribeTrack(p, 'microphone');
      return { p, track, pub };
    };

    it('M4: the own speaking comes on after 50 ms of level and goes off 300 ms after it ends', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      expect(call.state.speaking).toBe(false);
      m.levels.set(ownTrack(f), 0.05);
      // The first sample that hears the level starts the run: 25 ms of it is not speaking, 50 ms is.
      await f.advance(25);
      expect(call.state.speaking).toBe(false);
      await f.advance(25);
      expect(call.state.speaking).toBe(false);
      await f.advance(25);
      expect(call.state.speaking).toBe(true);
      m.levels.set(ownTrack(f), 0);
      await f.advance(25);
      await f.advance(275);
      expect(call.state.speaking).toBe(true);
      await f.advance(25);
      expect(call.state.speaking).toBe(false);
    });

    it('M4: the SFU flag no longer decides the own speaking while a meter reads', async () => {
      const f = fakes();
      withMeters(f);
      const call = await start(f);
      (f.room.localParticipant as unknown as { isSpeaking: boolean }).isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [f.room.localParticipant]);
      await f.advance(100);
      expect(call.state.speaking).toBe(false);
    });

    it('the own speaking is false whatever the level while muted, deafened, paused or push to talk is up', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      m.levels.set(ownTrack(f), 0.2);
      await f.advance(100);
      expect(call.state.speaking).toBe(true);
      call.setMuted(true);
      expect(call.state.speaking).toBe(false);
      await f.advance(200);
      expect(call.state.speaking).toBe(false);
      expect(call.state.level).toBe(0);
      call.setMuted(false);
      await f.advance(100);
      expect(call.state.speaking).toBe(true);
      call.setDeafened(true);
      await f.advance(100);
      expect(call.state.speaking).toBe(false);
      call.setDeafened(false);
      call.setMuted(false);
      await f.advance(100);
      expect(call.state.speaking).toBe(true);
      await call.setPaused(true);
      expect(call.state.speaking).toBe(false); // at once, before the next sample
      await f.advance(100);
      expect(call.state.speaking).toBe(false);
      await call.setPaused(false);
      await f.advance(100);
      expect(call.state.speaking).toBe(true);
      call.setMode('ptt');
      await f.advance(100);
      expect(call.state.speaking).toBe(false);
      call.pushToTalk(true);
      await f.advance(100);
      expect(call.state.speaking).toBe(true);
      call.pushToTalk(false);
      await f.advance(50);
      expect(call.state.speaking).toBe(false);
    });

    it('a remote tile with an attached track follows its meter and ignores the SFU flag', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      const { p, track } = heard(f);
      await f.advance(100);
      p.isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [p]);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
      m.levels.set(track.mst, 0.1);
      await f.advance(75);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      p.isSpeaking = false;
      await f.advance(100);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      m.levels.set(track.mst, 0);
      await f.advance(325);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
    });

    it('web3.8 FX I1: screen-share audio never counts as speaking; only the microphone does, as the SFU fallback', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      // Bob is muted (no microphone track) and shares a screen with loud audio.
      const p = f.addRemote(BOB_DEV);
      f.verified.add(BOB_DEV);
      const screenAudio = f.subscribeTrack(p, 'screen_share_audio').track;
      f.subscribeTrack(p, 'screen_share');
      m.levels.set(screenAudio.mst, 0.3);
      await f.advance(200);
      expect(screenAudio.attach).toHaveBeenCalled(); // it is still played
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
      // He unmutes: his microphone's meter decides, the screen audio still does not.
      const mic = f.subscribeTrack(p, 'microphone').track;
      await f.advance(100);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
      m.levels.set(mic.mst, 0.1);
      await f.advance(100);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      m.levels.set(mic.mst, 0);
      await f.advance(400);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
    });

    it('a track that is not attached (deafened) falls back to the SFU flag; paused gives false for every tile', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f, { self: 'deafened' });
      const { p, track } = heard(f);
      expect(track.attach).not.toHaveBeenCalled();
      expect(m.made.filter((x) => x.track === track.mst)).toHaveLength(0);
      p.isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [p]);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      await call.setPaused(true);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
      await call.setPaused(false);
      p.isSpeaking = false;
      f.room.emit('activeSpeakersChanged', []);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
    });

    it('a track attached before the volume went to 0 falls back to the SFU flag from then on', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      const { p, track } = heard(f);
      m.levels.set(track.mst, 0.1);
      await f.advance(100);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      call.setVolume(BOB, 0);
      await f.advance(25);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
      p.isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [p]);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
    });

    it('with no AudioContext both fall back to the SFU flags', async () => {
      const f = fakes();
      f.deps.audioContext = () => null;
      const m = withMeters(f);
      const call = await start(f);
      const { p } = heard(f);
      expect(m.create).not.toHaveBeenCalled();
      p.isSpeaking = true;
      (f.room.localParticipant as unknown as { isSpeaking: boolean }).isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [p, f.room.localParticipant]);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      expect(call.state.speaking).toBe(true);
      call.setMuted(true);
      expect(call.state.speaking).toBe(false);
    });

    it('I1: with push to talk and the key up the own speaking is false whatever the level, also with a ptt processor', async () => {
      const f = fakes();
      const m = withMeters(f);
      f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
      const call = await start(f, { mode: 'ptt', noise: 'on' });
      m.levels.set(ownTrack(f), 0.2);
      await f.advance(150);
      expect(call.state.speaking).toBe(false);
      expect(call.state.level).toBe(0);
      call.pushToTalk(true);
      await f.advance(150);
      expect(call.state.speaking).toBe(true);
      call.pushToTalk(false);
      expect(call.state.speaking).toBe(false); // at once, no 300 ms hold after the release
      await f.advance(150);
      expect(call.state.speaking).toBe(false);
    });

    it('I2: a suspended AudioContext falls back to the SFU flag', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      const { p, track } = heard(f);
      m.levels.set(track.mst, 0.1);
      m.levels.set(ownTrack(f), 0.1);
      f.contexts[0].to('suspended');
      await f.advance(150);
      expect(tileOf(call, BOB_DEV).speaking).toBe(false);
      expect(call.state.speaking).toBe(false);
      p.isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [p]);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
    });

    it('m2: deafening silences the own speaking at once', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      m.levels.set(ownTrack(f), 0.2);
      await f.advance(150);
      expect(call.state.speaking).toBe(true);
      call.setDeafened(true);
      expect(call.state.speaking).toBe(false);
    });

    it('m3: the own fallback (no context) is also false while push to talk is up', async () => {
      const f = fakes();
      f.deps.audioContext = () => null;
      const call = await start(f, { mode: 'ptt' });
      (f.room.localParticipant as unknown as { isSpeaking: boolean }).isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [f.room.localParticipant]);
      expect(call.state.speaking).toBe(false);
      call.pushToTalk(true);
      f.room.emit('activeSpeakersChanged', [f.room.localParticipant]);
      expect(call.state.speaking).toBe(true);
    });

    it('m6: a remote meter that appears mid-call arms the sampler, and the last detach idles it', async () => {
      const f = fakes();
      const m = withMeters(f);
      f.deps.createLevelMeter = (ctx, track) => ((track as unknown) === ownTrack(f) ? null : m.create(ctx, track));
      const call = await start(f);
      const idle = f.timers.size;
      const { p, track, pub } = heard(f);
      expect(f.timers.size).toBe(idle + 1);
      m.levels.set(track.mst, 0.1);
      await f.advance(100);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      expect(p.isSpeaking).toBe(false);
      f.unsubscribeTrack(p, pub);
      await f.advance(25);
      expect(f.timers.size).toBe(idle);
    });

    it('m4: the sampler idles when no meter exists, and runs once one does', async () => {
      const f = fakes();
      f.deps.createLevelMeter = () => null;
      await start(f);
      const idle = f.timers.size;
      const g = fakes();
      withMeters(g);
      await start(g);
      expect(g.timers.size).toBe(idle + 1);
    });

    it('a meter that cannot be made means the fallback, never an error', async () => {
      const f = fakes();
      f.deps.createLevelMeter = () => null;
      const call = await start(f);
      const { p } = heard(f);
      p.isSpeaking = true;
      f.room.emit('activeSpeakersChanged', [p]);
      expect(tileOf(call, BOB_DEV).speaking).toBe(true);
      expect(call.state.error).toBeNull();
      const g = fakes();
      g.deps.createLevelMeter = () => { throw new Error('no analyser'); };
      const other = await start(g);
      expect(other.state.error).toBeNull();
      expect(other.state.phase).toBe('connected');
    });

    it('the level (0..12) comes from the own meter', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      m.levels.set(ownTrack(f), 0.1);
      await f.advance(100);
      expect(call.state.level).toBe(5);
      m.levels.set(ownTrack(f), 0.9);
      await f.advance(100);
      expect(call.state.level).toBe(12);
    });

    it('a speaking flip is published at once, a level change at most every 100 ms', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      const seen: Array<[boolean, number]> = [];
      call.subscribe(() => { seen.push([call.state.speaking, call.state.level]); });
      m.levels.set(ownTrack(f), 0.1);
      await f.advance(75);
      expect(seen.some(([s]) => s)).toBe(true);
      seen.length = 0;
      // The level wanders every tick; the state changes at most once per 100 ms.
      for (let i = 0; i < 8; i++) {
        m.levels.set(ownTrack(f), 0.1 + (i % 2 === 0 ? 0.02 : 0.04));
        await f.advance(25);
      }
      expect(seen.length).toBeGreaterThan(0);
      expect(seen.length).toBeLessThanOrEqual(2);
    });

    it('every meter is closed on detach and on leave("user"), and no timer is left after ended', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      const bob = heard(f);
      const carol = heard(f, CAROL_DEV);
      const closed = (t: unknown) => m.made.find((x) => x.track === t)!.close;
      expect(closed(ownTrack(f))).not.toHaveBeenCalled();
      f.unsubscribeTrack(bob.p, bob.pub);
      expect(closed(bob.track.mst)).toHaveBeenCalledTimes(1);
      expect(closed(carol.track.mst)).not.toHaveBeenCalled();
      await call.leave('user');
      expect(closed(ownTrack(f))).toHaveBeenCalledTimes(1);
      expect(closed(carol.track.mst)).toHaveBeenCalledTimes(1);
      expect(f.timers.size).toBe(0);
    });

    it('every meter is closed when the room ends', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f);
      const bob = heard(f);
      f.room.emit('disconnected', 1);
      await f.advance(10);
      expect(call.state.phase).toBe('ended');
      expect(m.made.some((x) => x.track === bob.track.mst)).toBe(true);
      for (const x of m.made) expect(x.close).toHaveBeenCalledTimes(1);
      expect(f.timers.size).toBe(0);
    });

    it('the own meter is closed when a start fails after the publish', async () => {
      const f = fakes();
      const m = withMeters(f);
      (f.room as unknown as { state: string }).state = 'disconnected';
      await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toMatchObject({ code: 'E_DISCONNECTED' });
      expect(m.made.length).toBeGreaterThan(0);
      for (const x of m.made) expect(x.close).toHaveBeenCalledTimes(1);
    });

    it('the own meter is replaced when the noise processor swaps the track', async () => {
      const f = fakes();
      const m = withMeters(f);
      const call = await start(f, { noise: 'on' });
      const first = m.made.length;
      const mic = f.localTracks[0] as FakeTrack & { stopProcessor?: () => Promise<void> };
      mic.stopProcessor = vi.fn(async () => undefined);
      await call.setNoise('off');
      expect(m.made.length).toBe(first + 1);
      expect(m.made[first - 1].close).toHaveBeenCalledTimes(1);
    });

    it('no level reaches the bridge', async () => {
      const f = fakes();
      const m = withMeters(f);
      await start(f);
      m.levels.set(ownTrack(f), 0.1);
      await f.advance(200);
      expect(JSON.stringify(f.calls)).not.toMatch(/rms|level|speaking/i);
    });
  });

  it('Off reopens a capture with browser suppression disabled when live constraints are immutable',async()=>{
    const f=fakes();f.deps.createNoiseProcessor=()=>({setPttActive:vi.fn()});
    const call=await start(f,{noise:'lightweight',mode:'ptt'});
    const mic=f.localTracks[0] as unknown as {stopProcessor:()=>Promise<void>;restartTrack:ReturnType<typeof vi.fn>;mediaStreamTrack:MediaStreamTrack};
    mic.stopProcessor=async()=>{};
    const raw=f.room.localParticipant.publishTrack.mock.calls[0][0];
    raw.getSettings=()=>({noiseSuppression:true,deviceId:'chosen'});raw.getConstraints=()=>({});raw.applyConstraints=async()=>{};mic.mediaStreamTrack=raw;
    const fresh={enabled:true,stop:vi.fn(),getSettings:()=>({noiseSuppression:false}),getConstraints:()=>({}),applyConstraints:async()=>{}} as unknown as MediaStreamTrack;
    mic.restartTrack=vi.fn(async()=>{mic.mediaStreamTrack=fresh;});
    await call.setNoise('off');
    // web3.8 MUTE (R2): push to talk with the key up must not send, so the reopen waits for the key.
    expect(mic.restartTrack).not.toHaveBeenCalled();
    call.pushToTalk(true);
    await flush();
    expect(mic.restartTrack).toHaveBeenCalledWith(expect.objectContaining({noiseSuppression:false,echoCancellation:true,autoGainControl:true}));
    call.pushToTalk(false);
    expect(fresh.enabled).toBe(false);
  });

  it('replays held PTT after replacing the noise processor', async () => {
    const f = fakes();
    const made: Array<{setPttActive: ReturnType<typeof vi.fn>;setMode: ReturnType<typeof vi.fn>}> = [];
    f.deps.createNoiseProcessor = () => { const p={setPttActive:vi.fn(),setMode:vi.fn()};made.push(p);return p; };
    const call=await start(f,{noise:'lightweight',mode:'ptt'});
    call.pushToTalk(true);
    await call.setNoise('automatic');
    expect(made[1].setPttActive).toHaveBeenLastCalledWith(true);
    expect(made[1].setMode).toHaveBeenLastCalledWith('ptt');
  });
  it('uses the current mode after asynchronous processor initialization', async () => {
    const f=fakes(),p={setPttActive:vi.fn(),setMode:vi.fn()};
    f.deps.createNoiseProcessor=()=>p;
    const call=await start(f,{noise:'off',mode:'ptt'});
    let finish!:()=>void;
    f.localTracks[0].setProcessor=vi.fn(()=>new Promise<void>(resolve=>{finish=resolve;}));
    const enable=call.setNoise('lightweight');await flush();
    call.setMode('vad');finish();await enable;
    expect(p.setMode).toHaveBeenLastCalledWith('vad');
  });

  it('M1: noise suppression switches during the call', async () => {
    const f = fakes();
    const made: string[] = [];
    f.deps.createNoiseProcessor = (mode) => { made.push(mode); return { setPttActive: vi.fn() }; };
    const call = await start(f, { noise: 'on' });
    const mic = f.localTracks[0] as FakeTrack & { stopProcessor?: () => Promise<void> };
    mic.stopProcessor = vi.fn(async () => undefined);
    expect(mic.setProcessor).toHaveBeenCalledTimes(1);
    await call.setNoise('off');
    expect(mic.stopProcessor).toHaveBeenCalledTimes(1);
    await call.setNoise('off');
    expect(mic.stopProcessor).toHaveBeenCalledTimes(1);
    call.setMode('ptt');
    await call.setNoise('on');
    expect(mic.setProcessor).toHaveBeenCalledTimes(2);
    expect(made).toEqual(['vad', 'ptt']);
    expect(call.state.noise).toBe('on');
  });
});

describe('product walk M5: the own mute and deafen kept across a reload', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });

  it('a start muted sends nothing from the microphone and says so to the others at once', async () => {
    const f = fakes();
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, self: 'muted' });
    open.push(call);
    expect(call.state).toMatchObject({ muted: true, deafened: false });
    expect(f.localTracks[0].muted).toBe(true);
    // The microphone track was already off when it was published: no frame goes out before the mute lands.
    expect((f.room.localParticipant.publishTrack.mock.calls[0][0] as unknown as { enabled?: boolean }).enabled).toBe(false);
    expect(f.calls).toContainEqual({ m: 'voiceSelf', callId, flags: 1 });
  });

  it('a start deafened plays nothing and is muted too', async () => {
    const f = fakes();
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, self: 'deafened' });
    open.push(call);
    const bob = f.addRemote(BOB_DEV);
    expect(call.state).toMatchObject({ muted: true, deafened: true });
    expect(f.localTracks[0].muted).toBe(true);
    expect(bob.gains.microphone).toBe(0);
    expect(f.calls).toContainEqual({ m: 'voiceSelf', callId, flags: 3 });
    // Undeafening keeps the microphone muted (fix round 3, m1: the kept mute is never lost to a reload).
    call.setDeafened(false);
    expect(call.state).toMatchObject({ muted: true, deafened: false });
  });
});

describe('web3.8 MUTE: a mute pressed while the start runs is in force from the first published frame', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const deferred = <T,>() => {
    let resolve: (v: T) => void = () => undefined;
    const promise = new Promise<T>((r) => { resolve = r; });
    return { promise, resolve };
  };
  const micStream = () => {
    const track = { kind: 'audio', stop: () => undefined } as { kind: string; stop: () => void; enabled?: boolean };
    return { track, stream: { getTracks: () => [track], getAudioTracks: () => [track], getVideoTracks: () => [] } as unknown as MediaStream };
  };
  /** Records what the microphone looked like at the moment it was handed to the publish (`enabled`) and when the noise
   *  processor was set on the published track (`mutedAtNoise`); `publish` and `noise` hold those steps when given. */
  const watch = (f: ReturnType<typeof fakes>, hold: { publish?: Promise<void>; noise?: Promise<void> } = {}) => {
    const seen: { enabled: boolean | undefined; mutedAtNoise: boolean | undefined; publishing: boolean } = {
      enabled: undefined, mutedAtNoise: undefined, publishing: false,
    };
    const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
      seen.publishing = true;
      seen.enabled = (track as unknown as { enabled?: boolean }).enabled !== false;
      await hold.publish;
      const pub = await real(track, o);
      const t = pub.track;
      t.setProcessor = vi.fn(async () => { seen.mutedAtNoise = t.muted; await hold.noise; });
      return pub;
    });
    return seen;
  };

  it('a mute pressed during the microphone prompt: the track is published disabled, and the others are told', async () => {
    const prompt = deferred<MediaStream>();
    const f = fakes({ getUserMedia: () => prompt.promise });
    const seen = watch(f);
    let kept: 'muted' | 'deafened' | null = null;
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, self: null, selfNow: () => kept });
    await flush();
    kept = 'muted';
    prompt.resolve(micStream().stream);
    const call = await started;
    open.push(call);
    expect(seen.enabled).toBe(false);
    expect(call.state).toMatchObject({ muted: true, deafened: false });
    expect(f.localTracks[0].muted).toBe(true);
    expect(f.calls).toContainEqual({ m: 'voiceSelf', callId, flags: 1 });
  });

  it('a mute pressed while the room connects: the track is published disabled', async () => {
    const f = fakes();
    const seen = watch(f);
    const connect = deferred<void>();
    const join = f.deps.joinCall;
    f.deps.joinCall = async (o: JoinCallOptions) => { await connect.promise; return join(o); };
    let kept: 'muted' | 'deafened' | null = null;
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, selfNow: () => kept });
    await flush();
    kept = 'muted';
    connect.resolve();
    const call = await started;
    open.push(call);
    expect(seen.enabled).toBe(false);
    expect(f.localTracks[0].muted).toBe(true);
  });

  /** The page's side of watchSelf: `press` sets what selfNow answers and pushes it to the starting session at once. */
  const pressing = () => {
    const page = { kept: null as 'muted' | 'deafened' | null, apply: null as (() => void) | null, registered: 0, unregistered: 0 };
    const o: Partial<PageCallOptions> = {
      selfNow: () => page.kept,
      watchSelf: (apply) => { page.apply = apply; page.registered += 1; return () => { page.apply = null; page.unregistered += 1; }; },
    };
    const press = (v: 'muted' | 'deafened') => { page.kept = v; page.apply?.(); };
    return { page, o, press };
  };

  it('a mute pressed during the publish closes the microphone at the press, and LiveKit cannot open it again', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const mic = micStream();
    f.deps.media.getUserMedia = vi.fn(async () => mic.stream);
    const publish = deferred<void>();
    const seen = watch(f, { publish: publish.promise });
    const { page, o, press } = pressing();
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on', ...o });
    await flush();
    // The track went to the publish open: the person had not pressed anything yet.
    expect(seen).toMatchObject({ publishing: true, enabled: true });
    expect(page.registered).toBe(1);
    press('muted');
    // At the press, with the publish still pending: closed now, not at the next step of the start.
    expect(mic.track.enabled).toBe(false);
    // As LiveKit's LocalTrack does while it publishes: it enables the track it was handed.
    (mic.track as { enabled: boolean }).enabled = true;
    expect(mic.track.enabled).toBe(false);
    publish.resolve();
    const call = await started;
    open.push(call);
    expect(seen.mutedAtNoise).toBe(true);
    expect(call.state).toMatchObject({ muted: true });
    expect(f.calls).toContainEqual({ m: 'voiceSelf', callId, flags: 1 });
  });

  it('without watchSelf, a mute pressed during the publish is read when the publish returns, before the noise processor loads', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const publish = deferred<void>();
    const seen = watch(f, { publish: publish.promise });
    let kept: 'muted' | 'deafened' | null = null;
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on', selfNow: () => kept });
    await flush();
    expect(seen).toMatchObject({ publishing: true, enabled: true });
    kept = 'muted';
    publish.resolve();
    const call = await started;
    open.push(call);
    expect(seen.mutedAtNoise).toBe(true);
    expect(call.state).toMatchObject({ muted: true });
  });

  it('a mute pressed while the noise processor loads closes the microphone at the press, LiveKit mute included', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const mic = micStream();
    f.deps.media.getUserMedia = vi.fn(async () => mic.stream);
    const noise = deferred<void>();
    const seen = watch(f, { noise: noise.promise });
    const { page, o, press } = pressing();
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on', ...o });
    await flush();
    expect(seen.mutedAtNoise).toBe(false);
    expect(f.localTracks[0].muted).toBe(false);
    press('muted');
    // Still loading the processor: the published track is LiveKit-muted and the capture disabled already.
    expect(f.localTracks[0].muted).toBe(true);
    expect(mic.track.enabled).toBe(false);
    noise.resolve();
    const call = await started;
    open.push(call);
    expect(call.state).toMatchObject({ muted: true });
    expect(f.calls).toContainEqual({ m: 'voiceSelf', callId, flags: 1 });
    // Connected: the start no longer listens; the page's attach reconcile and the session's own toggles take over.
    expect(page.unregistered).toBe(1);
    expect(page.apply).toBeNull();
  });

  it('a deafen pressed while the noise processor loads closes the microphone at the press, and an undeafen keeps it muted', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const noise = deferred<void>();
    watch(f, { noise: noise.promise });
    const { o, press } = pressing();
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on', ...o });
    await flush();
    press('deafened');
    expect(f.localTracks[0].muted).toBe(true);
    noise.resolve();
    const call = await started;
    open.push(call);
    expect(call.state).toMatchObject({ muted: true, deafened: true });
    call.setDeafened(false);
    expect(call.state).toMatchObject({ muted: true, deafened: false });
  });

  it('a press after a failed start, and an unmute pushed during the start, change nothing', async () => {
    const f = fakes({ joinCallError: new Error('E_SFU') });
    const { page, o, press } = pressing();
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o })).rejects.toBeTruthy();
    expect(page.unregistered).toBe(page.registered);
    press('muted');
    const g = fakes();
    const noise = deferred<void>();
    g.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    watch(g, { noise: noise.promise });
    const p2 = pressing();
    const started = startPageCall(g.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on', self: 'muted', ...p2.o });
    await flush();
    p2.page.kept = null;
    p2.page.apply?.();
    noise.resolve();
    const call = await started;
    open.push(call);
    expect(call.state).toMatchObject({ muted: true });
    expect(g.localTracks[0].muted).toBe(true);
  });

  it('a deafen pressed during the join, then an undeafen, leaves the microphone muted', async () => {
    const prompt = deferred<MediaStream>();
    const f = fakes({ getUserMedia: () => prompt.promise });
    const seen = watch(f);
    let kept: 'muted' | 'deafened' | null = null;
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, selfNow: () => kept });
    await flush();
    kept = 'deafened';
    prompt.resolve(micStream().stream);
    const call = await started;
    open.push(call);
    expect(seen.enabled).toBe(false);
    expect(call.state).toMatchObject({ muted: true, deafened: true });
    expect(f.calls).toContainEqual({ m: 'voiceSelf', callId, flags: 3 });
    call.setDeafened(false);
    expect(call.state).toMatchObject({ muted: true, deafened: false });
    expect(f.localTracks[0].muted).toBe(true);
  });

  it('without watchSelf, a deafen while the noise processor loads is read at connected, and an undeafen leaves the microphone muted', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const noise = deferred<void>();
    watch(f, { noise: noise.promise });
    let kept: 'muted' | 'deafened' | null = null;
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on', selfNow: () => kept });
    await flush();
    kept = 'deafened';
    noise.resolve();
    const call = await started;
    open.push(call);
    const bob = f.addRemote(BOB_DEV);
    expect(call.state).toMatchObject({ muted: true, deafened: true });
    expect(bob.gains.microphone).toBe(0);
    expect(f.calls).toContainEqual({ m: 'voiceSelf', callId, flags: 3 });
    call.setDeafened(false);
    expect(call.state).toMatchObject({ muted: true, deafened: false });
  });

  it('only toward privacy: a setting less private than the start never opens it', async () => {
    const f = fakes();
    const seen = watch(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, self: 'deafened', selfNow: () => null });
    open.push(call);
    expect(call.state).toMatchObject({ muted: true, deafened: true });
    expect(seen.enabled).toBe(false);
    expect(f.localTracks[0].muted).toBe(true);
    const g = fakes();
    const muted = await startPageCall(g.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, self: 'deafened', selfNow: () => 'muted' });
    open.push(muted);
    expect(muted.state).toMatchObject({ muted: true, deafened: true });
    expect(g.localTracks[0].muted).toBe(true);
  });

  it('the capture track the noise start reopens is the one the mute closes', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn(), status: { effective: 'browser', browserNoise: 'off' } } as unknown as ReturnType<NonNullable<PageCallDeps['createNoiseProcessor']>>);
    const fresh = { kind: 'audio', enabled: true, stop: vi.fn(), getSettings: () => ({ noiseSuppression: true }), getConstraints: () => ({}),
      applyConstraints: async () => undefined } as unknown as MediaStreamTrack;
    const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
      const raw = track as unknown as Record<string, unknown>;
      raw.getSettings = () => ({ noiseSuppression: false, deviceId: 'chosen' }); raw.getConstraints = () => ({}); raw.applyConstraints = async () => undefined;
      const pub = await real(track, o);
      const t = pub.track as unknown as { mediaStreamTrack: unknown; restartTrack: () => Promise<void> };
      t.mediaStreamTrack = track;
      t.restartTrack = async () => { t.mediaStreamTrack = fresh; };
      return pub;
    });
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on', self: 'muted' });
    open.push(call);
    expect(fresh.enabled).toBe(false);
  });

  /** As LiveKit's LocalTrack: the track it is handed is enabled (`enabled = !isMuted`, isMuted false) while it publishes. */
  const liveKitEnables = (f: ReturnType<typeof fakes>) => {
    const seen: { during?: boolean } = {};
    const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
      await Promise.resolve();
      (track as unknown as { enabled: boolean }).enabled = true;
      seen.during = (track as unknown as { enabled: boolean }).enabled;
      return real(track, o);
    });
    return seen;
  };

  it('LiveKit enabling the track during the publish never opens a muted microphone', async () => {
    const f = fakes();
    const seen = liveKitEnables(f);
    const prompt = deferred<MediaStream>();
    f.deps.media.getUserMedia = vi.fn(() => prompt.promise);
    let kept: 'muted' | 'deafened' | null = null;
    const started = startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, selfNow: () => kept });
    await flush();
    kept = 'muted';
    const mic = micStream();
    prompt.resolve(mic.stream);
    const call = await started;
    open.push(call);
    expect(seen.during).toBe(false);
    expect(mic.track.enabled).toBe(false);
    // An unmute opens it again (the guard follows the session, it never holds a microphone the person opened).
    call.setMuted(false);
    expect(mic.track.enabled).toBe(true);
    (mic.track as { enabled: boolean }).enabled = false;
    (mic.track as { enabled: boolean }).enabled = true;
    expect(mic.track.enabled).toBe(true);
    call.setMuted(true);
    (mic.track as { enabled: boolean }).enabled = true;
    expect(mic.track.enabled).toBe(false);
  });

  it('LiveKit enabling the track during the publish never opens push to talk with the key up, and an open call stays open', async () => {
    const f = fakes();
    const seen = liveKitEnables(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, mode: 'ptt' });
    open.push(call);
    expect(seen.during).toBe(false);
    const g = fakes();
    const open2 = liveKitEnables(g);
    const vad = await startPageCall(g.deps, { channelId: CH, vdec: 'vp8' }, opts);
    open.push(vad);
    expect(open2.during).toBe(true);
    expect((g.room.localParticipant.publishTrack.mock.calls[0][0] as unknown as { enabled?: boolean }).enabled).toBe(true);
  });

  /** A published microphone whose capture cannot switch noise suppression live (the settings keep saying `on`), so the
   *  browser path reopens it with restartTrack; the reopened track is `fresh`, a MediaStreamTrack-like object whose
   *  `enabled` lives on its prototype, as a real track's does. */
  const reopening = (f: ReturnType<typeof fakes>) => {
    const fresh = Object.assign(Object.create({ enabled: true }) as { enabled: boolean }, {
      kind: 'audio', stop: vi.fn(), getSettings: () => ({ noiseSuppression: false, deviceId: 'chosen' }), getConstraints: () => ({}),
      applyConstraints: async () => undefined,
    });
    const restarts = { n: 0 };
    const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
      const raw = track as unknown as Record<string, unknown>;
      raw.getSettings = () => ({ noiseSuppression: true, deviceId: 'chosen' }); raw.getConstraints = () => ({}); raw.applyConstraints = async () => undefined;
      const pub = await real(track, o);
      const t = pub.track as unknown as { mediaStreamTrack: unknown; restartTrack: () => Promise<void> };
      t.mediaStreamTrack = track;
      // As LiveKit: the sender gets the new capture, which starts enabled.
      t.restartTrack = async () => { restarts.n += 1; t.mediaStreamTrack = fresh; };
      return pub;
    });
    return { fresh, restarts };
  };

  it('R2: a capture restart waits while the microphone must not send, and runs when it opens', async () => {
    const f = fakes();
    const { fresh, restarts } = reopening(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    await flush();
    // Muted: no new (enabled) capture goes onto the sender.
    expect(restarts.n).toBe(0);
    call.setMuted(false);
    await flush();
    expect(restarts.n).toBe(1);
    expect(fresh.enabled).toBe(true);
  });

  it('R2: a reopened capture is guarded too: muted afterwards, LiveKit cannot enable it', async () => {
    const f = fakes();
    const { fresh, restarts } = reopening(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off' });
    open.push(call);
    await flush();
    expect(restarts.n).toBe(1);
    call.setMuted(true);
    expect(fresh.enabled).toBe(false);
    fresh.enabled = true;
    expect(fresh.enabled).toBe(false);
  });

  /** web3.8 FX I3: fake meters, their level set per track by the test; `last()` is the track the newest meter reads. */
  const ownMeters = (f: ReturnType<typeof fakes>) => {
    const levels = new Map<unknown, number>();
    const made: Array<{ track: unknown; closed: boolean }> = [];
    f.deps.createLevelMeter = (_ctx, track) => {
      const entry = { track: track as unknown, closed: false };
      made.push(entry);
      return { read: () => (entry.closed ? 0 : levels.get(track) ?? 0), close: () => { entry.closed = true; } };
    };
    return { levels, made, last: () => made.filter((m) => !m.closed).at(-1)?.track };
  };

  it('FX I3: after the browser-path capture restart at the start, the own meter reads the new capture', async () => {
    const f = fakes();
    const { fresh, restarts } = reopening(f);
    const m = ownMeters(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off' });
    open.push(call);
    await flush();
    expect(restarts.n).toBe(1);
    expect(m.last()).toBe(fresh);
    m.levels.set(fresh, 0.2);
    await f.advance(150);
    expect(call.state.speaking).toBe(true);
    expect(call.state.level).toBeGreaterThan(0);
  });

  it('FX I3: after the deferred restart on unmute, the own meter reads the new capture', async () => {
    const f = fakes();
    const { fresh, restarts } = reopening(f);
    const m = ownMeters(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    await flush();
    expect(restarts.n).toBe(0);
    call.setMuted(false);
    await flush();
    expect(restarts.n).toBe(1);
    expect(m.last()).toBe(fresh);
    m.levels.set(fresh, 0.2);
    await f.advance(150);
    expect(call.state.speaking).toBe(true);
  });

  /** web3.9 VOICE-FIX: as `reopening`, and the published LiveKit track can take a capture the session acquired itself
   *  (LocalAudioTrack.replaceTrack). `steps` records the order of the pre-acquire, the replace and the old capture's stop;
   *  `atReplace` is the new capture's `enabled` at the moment it reaches the sender. As LiveKit, the replace then sets
   *  `enabled = true` on it (`!isMuted`, isMuted false). The pre-acquire is `pre`, installed once the call is up. */
  const handingOver = (f: ReturnType<typeof fakes>) => {
    const steps: string[] = [];
    const { fresh, restarts } = reopening(f);
    // Fix round 1 (I1): the hand-over runs only where the engine keeps two captures of one microphone live.
    f.deps.concurrentCapture = true;
    const next = Object.assign(Object.create({ enabled: true }) as { enabled: boolean }, {
      kind: 'audio', stop: vi.fn(() => { steps.push('new.stop'); }), getSettings: () => ({ noiseSuppression: false, deviceId: 'chosen' }),
      getConstraints: () => ({}), applyConstraints: async () => undefined,
    });
    const seen: { atReplace?: boolean; userProvided?: boolean } = {};
    const holdReplace: { promise?: Promise<void> } = {};
    let old: { stop: ReturnType<typeof vi.fn>; track: unknown } | null = null;
    /** Every track handed to replaceTrack, in order; `onReplace` runs as the replace resolves (fix round 1, M1). */
    const replaced: unknown[] = [];
    const hooks: { onReplace?: (n: MediaStreamTrack) => void } = {};
    const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
      const stop = vi.fn(() => { steps.push('old.stop'); });
      (track as unknown as { stop: unknown }).stop = stop;
      old = { stop, track };
      const pub = await real(track, o);
      const t = pub.track as unknown as { mediaStreamTrack: unknown; replaceTrack: (n: MediaStreamTrack, u?: boolean) => Promise<void> };
      t.replaceTrack = vi.fn(async (n: MediaStreamTrack, u?: boolean) => {
        steps.push('replaceTrack');
        replaced.push(n);
        seen.atReplace = (n as unknown as { enabled: boolean }).enabled;
        seen.userProvided = u;
        await holdReplace.promise;
        t.mediaStreamTrack = n;
        (n as unknown as { enabled: boolean }).enabled = true;
        hooks.onReplace?.(n);
      });
      return pub;
    });
    const pre = vi.fn(async (c: MediaStreamConstraints) => {
      void c;
      steps.push('getUserMedia');
      return { getTracks: () => [next], getAudioTracks: () => [next], getVideoTracks: () => [] } as unknown as MediaStream;
    });
    const install = () => { f.deps.media.getUserMedia = pre; };
    return { steps, restarts, fresh, next, seen, holdReplace, pre, install, replaced, hooks, old: () => old };
  };

  it('a deferred restart keeps the old capture until the new one is on the sender', async () => {
    const f = fakes();
    const h = handingOver(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    h.install();
    await flush();
    expect(h.steps).toEqual([]);
    call.setMuted(false);
    await flush();
    expect(h.steps).toEqual(['getUserMedia', 'replaceTrack', 'old.stop']);
    expect(h.restarts.n).toBe(0);
    expect(h.seen.userProvided).toBe(true);
    expect(h.pre.mock.calls[0][0]).toMatchObject({ audio: { deviceId: { exact: 'chosen' }, noiseSuppression: false, echoCancellation: true, autoGainControl: true } });
    // The new capture is the one the session's mute governs from here on.
    expect(h.next.enabled).toBe(true);
    call.setMuted(true);
    expect(h.next.enabled).toBe(false);
  });

  it('the pre-acquired track is guarded before it reaches the sender', async () => {
    const f = fakes();
    const h = handingOver(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    const prompt = deferred<void>();
    const inner = h.pre.getMockImplementation()!;
    h.pre.mockImplementation(async (c) => { const s = await inner(c); await prompt.promise; return s; });
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    expect(h.pre).toHaveBeenCalledTimes(1);
    // Muted again before the new capture is on the sender.
    call.setMuted(true);
    prompt.resolve();
    await flush();
    expect(h.steps).toEqual(['getUserMedia', 'replaceTrack', 'old.stop']);
    expect(h.seen.atReplace).toBe(false);
    // LiveKit's `enabled = !isMuted` after the replace does not open it either: the end state is disabled.
    expect(h.next.enabled).toBe(false);
    h.next.enabled = true;
    expect(h.next.enabled).toBe(false);
  });

  it('the pre-acquired track stays closed when a mute lands while the replace runs', async () => {
    const f = fakes();
    const h = handingOver(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    const replace = deferred<void>();
    h.holdReplace.promise = replace.promise;
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    expect(h.steps).toEqual(['getUserMedia', 'replaceTrack']);
    call.setMuted(true);
    replace.resolve();
    await flush();
    expect(h.steps).toEqual(['getUserMedia', 'replaceTrack', 'old.stop']);
    expect(h.next.enabled).toBe(false);
  });

  it('a failed pre-acquire keeps the old capture and does not loop', async () => {
    const f = fakes();
    const h = handingOver(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    h.pre.mockImplementationOnce(async () => { h.steps.push('getUserMedia'); throw Object.assign(new Error('busy'), { name: 'NotReadableError' }); });
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    expect(h.steps).toEqual(['getUserMedia']);
    expect(h.old()!.stop).not.toHaveBeenCalled();
    expect(h.restarts.n).toBe(0);
    for (let i = 0; i < 2; i++) {
      call.setMuted(true);
      await flush();
      call.setMuted(false);
      await flush();
    }
    expect(h.pre).toHaveBeenCalledTimes(1);
    expect(h.old()!.stop).not.toHaveBeenCalled();
    expect(h.restarts.n).toBe(0);
  });

  it('a pre-acquired capture that has already ended is not handed over', async () => {
    const f = fakes();
    const h = handingOver(f);
    (h.next as unknown as { readyState: string }).readyState = 'ended';
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    expect(h.steps).toEqual(['getUserMedia', 'new.stop']);
    expect(h.old()!.stop).not.toHaveBeenCalled();
  });

  it('a leave while the pre-acquire runs stops the new capture and never hands it over', async () => {
    const f = fakes();
    const h = handingOver(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    const prompt = deferred<void>();
    const inner = h.pre.getMockImplementation()!;
    h.pre.mockImplementation(async (c) => { const s = await inner(c); await prompt.promise; return s; });
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    await call.leave('user');
    prompt.resolve();
    await flush();
    expect(h.steps).toEqual(['getUserMedia', 'old.stop', 'new.stop']);
  });

  for (const flag of [false, undefined]) {
    it(`a deferred restart on an engine without concurrent capture stops first (concurrentCapture ${String(flag)})`, async () => {
      const f = fakes();
      const h = handingOver(f);
      if (flag === undefined) delete f.deps.concurrentCapture; else f.deps.concurrentCapture = flag;
      const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
      open.push(call);
      h.install();
      await flush();
      expect(h.restarts.n).toBe(0);
      call.setMuted(false);
      await flush();
      expect(h.restarts.n).toBe(1);
      expect(h.replaced).toEqual([]);
      expect(h.pre).not.toHaveBeenCalled();
      // The web3.8 guard still holds on that path.
      call.setMuted(true);
      expect(h.fresh.enabled).toBe(false);
      h.fresh.enabled = true;
      expect(h.fresh.enabled).toBe(false);
    });
  }

  it('a noise toggle during the hand-over leaves every acquired capture stopped after leave', async () => {
    const f = fakes();
    const h = handingOver(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    const prompt = deferred<void>();
    const inner = h.pre.getMockImplementation()!;
    h.pre.mockImplementation(async (c) => { const s = await inner(c); await prompt.promise; return s; });
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    expect(h.pre).toHaveBeenCalledTimes(1);
    // While the pre-acquire waits, noise goes on and off again: the off reopens the capture through restartTrack (X).
    await call.setNoise('lightweight');
    await call.setNoise('off');
    await flush();
    expect(h.restarts.n).toBe(1);
    prompt.resolve();
    await flush();
    await call.leave('user');
    expect(h.old()!.stop).toHaveBeenCalled();
    expect(h.fresh.stop).toHaveBeenCalled();
    expect(h.next.stop).toHaveBeenCalled();
  });

  it('a restart queued behind the replace is not undone by a give-back', async () => {
    const f = fakes();
    const h = handingOver(f);
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    const replace = deferred<void>();
    h.holdReplace.promise = replace.promise;
    // Y: the capture a noise toggle's restartTrack brings, queued behind the replace (LiveKit's trackChangeLock). As
    // LiveKit's restart, it stops its current track first (by then `next`), then puts Y on the sender.
    const y = Object.assign(Object.create({ enabled: true }) as { enabled: boolean }, {
      kind: 'audio', stop: vi.fn(), getSettings: () => ({ noiseSuppression: false, deviceId: 'chosen' }), getConstraints: () => ({}),
      applyConstraints: async () => undefined,
    });
    const lk = f.localTracks[0] as unknown as { mediaStreamTrack: unknown; restartTrack: () => Promise<void> };
    lk.restartTrack = async () => {
      h.restarts.n += 1;
      await replace.promise;
      const cur = lk.mediaStreamTrack as { stop: () => void; readyState?: string };
      cur.readyState = 'ended';
      cur.stop();
      lk.mediaStreamTrack = y;
    };
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    expect(h.steps).toEqual(['getUserMedia', 'replaceTrack']);
    // Noise on and off again: the off reopens the capture through restartTrack, which waits for the replace.
    await call.setNoise('lightweight');
    const off = call.setNoise('off');
    await flush();
    expect(h.restarts.n).toBe(1);
    replace.resolve();
    await off;
    await flush();
    // No give-back: Y stays on the sender and is the capture the session's mute governs.
    expect(h.replaced).toEqual([h.next]);
    expect(lk.mediaStreamTrack).toBe(y);
    expect(h.old()!.stop).toHaveBeenCalled();
    call.setMuted(true);
    expect(y.enabled).toBe(false);
    await call.leave('user');
    expect(h.next.stop).toHaveBeenCalled();
    expect(y.stop).toHaveBeenCalled();
  });

  it('a capture that ends during the replace gives the sender back the old one', async () => {
    const f = fakes();
    const h = handingOver(f);
    h.hooks.onReplace = (n) => { if (n === (h.next as unknown)) (n as unknown as { readyState: string }).readyState = 'ended'; };
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off', self: 'muted' });
    open.push(call);
    h.install();
    await flush();
    call.setMuted(false);
    await flush();
    expect(h.replaced).toEqual([h.next, h.old()!.track]);
    expect(h.replaced[1]).toBe(h.old()!.track);
    expect(h.steps).toEqual(['getUserMedia', 'replaceTrack', 'replaceTrack', 'new.stop']);
    expect(h.old()!.stop).not.toHaveBeenCalled();
    // The old capture is still the one the mute governs.
    call.setMuted(true);
    expect((h.old()!.track as { enabled: boolean }).enabled).toBe(false);
  });

  it('a getter that throws changes nothing and fails no start', async () => {
    const f = fakes();
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, self: 'muted', selfNow: () => { throw new Error('gone'); } });
    open.push(call);
    expect(call.state).toMatchObject({ muted: true });
  });
});

describe('re-review N-m1: a drop never takes the key that ends the hold', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });

  it('a hold of epoch N drops nothing once the key of a newer epoch arrived; a drop of the held epoch clears', async () => {
    const f = fakes({ joinEpoch: 1n });
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts);
    open.push(call);
    f.pushKeys(callId, keysFor(2n));
    await flush();
    expect(f.manager.installs.map((k) => k.epoch)).toEqual([1n, 2n]);
    call.dropKeys(1n);
    expect(f.manager.clearKeys).not.toHaveBeenCalled();
    call.dropKeys(2n);
    expect(f.manager.clearKeys).toHaveBeenCalledTimes(1);
  });
});

describe('fix round 3 (wb-fix2-review-client I-1, m1, m2)', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });

  it('I-1: noise off under push to talk keeps the microphone muted with the key up, before the processor stops', async () => {
    const f = fakes();
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, mode: 'ptt', noise: 'on' });
    open.push(call);
    const mic = f.localTracks[0] as FakeTrack & { stopProcessor?: () => Promise<void> };
    // Track gating now enforces PTT independently of the denoiser.
    expect(mic.muted).toBe(true);
    const order: string[] = [];
    const mute = mic.mute.bind(mic);
    mic.mute = () => { order.push('mute'); mute(); };
    mic.stopProcessor = vi.fn(async () => { order.push(`stop (muted ${String(mic.muted)})`); });
    await call.setNoise('off');
    expect(order).toEqual(['stop (muted true)']);
    expect(mic.muted).toBe(true);
  });

  it('m1: a call started deafened stays muted when undeafened (the kept mute is not lost)', async () => {
    const f = fakes();
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, self: 'deafened' });
    open.push(call);
    call.setDeafened(false);
    expect(call.state).toMatchObject({ muted: true, deafened: false });
  });

  it('m2: noise on then off before the processor is set leaves no processor running', async () => {
    const f = fakes();
    let finish: () => void = () => undefined;
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'off' });
    open.push(call);
    const mic = f.localTracks[0] as FakeTrack & { stopProcessor?: () => Promise<void>; setProcessor: ReturnType<typeof vi.fn> };
    mic.setProcessor = vi.fn(() => new Promise<void>((resolve) => { finish = resolve; }));
    mic.stopProcessor = vi.fn(async () => undefined);
    const on = call.setNoise('on');
    const off = call.setNoise('off');
    await flush();
    finish();
    await on;
    await off;
    expect(call.state.noise).toBe('off');
    expect(mic.setProcessor).toHaveBeenCalledTimes(1);
    expect(mic.stopProcessor).toHaveBeenCalledTimes(1);
  });
});

describe('web3.3: remote call audio plays (attach, gains, autoplay, detach)', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };
  const elementsIn = (host: HTMLElement) => [...host.querySelectorAll('audio, video')];
  const orderOf = (fn: { mock: { invocationCallOrder: number[] } }) => fn.mock.invocationCallOrder;
  /** True when `p.setVolume(volume, source)` ran after `track.attach` (livekit applies a volume only to a gain node that exists). */
  const setAfterAttach = (p: ReturnType<typeof fakeRemote>, track: FakeRemoteTrack, volume: number, source = 'microphone') => {
    const attachedAt = orderOf(track.attach)[0];
    return attachedAt !== undefined
      && p.setVolume.mock.calls.some(([v, s], i) => v === volume && s === source && orderOf(p.setVolume)[i] > attachedAt);
  };

  it('a remote microphone that subscribes is attached once, into the audio host; a camera is not', async () => {
    const f = fakes();
    await start(f);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toEqual([track.made[0]]);
    const cam = f.subscribeTrack(bob, 'camera');
    expect(cam.track.attach).not.toHaveBeenCalled();
    expect(elementsIn(f.audioHost)).toHaveLength(1);
  });

  it('screen-share audio is attached as the microphone is', async () => {
    const f = fakes();
    await start(f);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'screen_share_audio');
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toEqual([track.made[0]]);
  });

  it('T5: the voice volume does not move the stream, and the stream volume does not move the voice', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'microphone');
    f.subscribeTrack(bob, 'screen_share_audio');
    call.setVolume(BOB, 40);
    expect(bob.gains.microphone).toBe(0.4);
    expect(bob.gains.screen_share_audio).toBe(1);
    call.setVolume(BOB, 100);
    call.setVolume(BOB, 150, 'stream');
    expect(bob.gains.microphone).toBe(1);
    expect(bob.gains.screen_share_audio).toBe(1.5);
  });

  it('T5: a stream volume of 0 holds the share audio back and leaves the voice playing; it attaches when it rises', async () => {
    const f = fakes();
    const call = await start(f, { streamVolumes: { [BOB]: 0 } });
    const bob = f.addRemote(BOB_DEV);
    const voice = f.subscribeTrack(bob, 'microphone');
    const stream = f.subscribeTrack(bob, 'screen_share_audio');
    expect(voice.track.attach).toHaveBeenCalledTimes(1);
    expect(stream.track.attach).not.toHaveBeenCalled();
    bob.setVolume.mockClear();
    call.setVolume(BOB, 50, 'stream');
    expect(stream.track.attach).toHaveBeenCalledTimes(1);
    expect(setAfterAttach(bob, stream.track, 0.5, 'screen_share_audio')).toBe(true);
    call.setVolume(BOB, 0, 'stream');
    expect(bob.gains.screen_share_audio).toBe(0);
    expect(bob.gains.microphone).toBe(1);
  });

  it('T5: deafen still silences the stream at any stream volume, and undeafen restores it', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'screen_share_audio');
    call.setVolume(BOB, 200, 'stream');
    call.setDeafened(true);
    expect(bob.gains.screen_share_audio).toBe(0);
    call.setVolume(BOB, 120, 'stream');
    expect(bob.gains.screen_share_audio).toBe(0);
    call.setDeafened(false);
    expect(bob.gains.screen_share_audio).toBe(1.2);
  });

  it('T5: the stream mute is local and per person: only that person\'s share audio is held back, and nothing is sent', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    const carol = f.addRemote(CAROL_DEV);
    f.subscribeTrack(bob, 'screen_share_audio');
    f.subscribeTrack(carol, 'screen_share_audio');
    const sent = f.calls.length;
    call.setVolume(BOB, 0, 'stream');
    expect(bob.gains.screen_share_audio).toBe(0);
    expect(carol.gains.screen_share_audio).toBe(1);
    expect(f.calls.length).toBe(sent);
  });

  it('T5: a muted stream keeps the no-sound mark off (the mark is about the stream, not the viewer\'s choice)', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    f.verified.add(BOB_DEV);
    f.subscribeTrack(bob, 'screen_share');
    f.subscribeTrack(bob, 'screen_share_audio');
    await f.advance(1_000);
    call.setVolume(BOB, 0, 'stream');
    expect(call.state.tiles.find((t) => t.deviceId === BOB_DEV)!.screenAudio).toBe(true);
  });

  it('Review Focus 1: a track subscribed before the listeners existed is attached once at connect, never twice', async () => {
    const f = fakes();
    const bob = fakeRemote(BOB_DEV);
    f.room.remoteParticipants.set(BOB_DEV, bob);
    const { track, pub } = f.subscribeTrack(bob, 'microphone', false);
    const call = await start(f);
    expect(call.state.phase).toBe('connected');
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
    // The room’s own trackSubscribed for the same track, late: still one element.
    f.room.emit('trackSubscribed', track, pub, bob);
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
  });

  it('Review Focus 1: a track subscribed while the join was still publishing the microphone is attached at connect', async () => {
    const f = fakes();
    const carol = fakeRemote(CAROL_DEV);
    let early: FakeRemoteTrack | null = null;
    const publish = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementationOnce(async (track, o) => {
      f.room.remoteParticipants.set(CAROL_DEV, carol);
      early = f.subscribeTrack(carol, 'microphone').track; // heard by nobody yet
      return publish(track, o);
    });
    await start(f);
    expect(early).not.toBeNull();
    expect(early!.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
  });


  it('a per-person volume set before the attach is applied after it', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    call.setVolume(BOB, 50);
    bob.setVolume.mockClear();
    const { track } = f.subscribeTrack(bob, 'microphone');
    expect(setAfterAttach(bob, track, 0.5)).toBe(true);
  });

  it('a track attached by the connect sweep gets its gain after the attach', async () => {
    const f = fakes();
    const bob = fakeRemote(BOB_DEV);
    f.room.remoteParticipants.set(BOB_DEV, bob);
    const { track } = f.subscribeTrack(bob, 'microphone', false);
    await start(f, { volumes: { [BOB]: 50 } });
    expect(setAfterAttach(bob, track, 0.5)).toBe(true);
  });

  // Fix round 2 (Minor 3): the room’s canPlaybackAudio speaks only where the session has no AudioContext of its own.
  it('Review Focus 3: without a session context, the room holding audio back sets audioBlocked, at connect and on audioPlaybackChanged', async () => {
    const f = fakes();
    f.deps.audioContext = () => null;
    f.room.canPlaybackAudio = false;
    const call = await start(f);
    expect(call.state.audioBlocked).toBe(true);
    const g = fakes();
    g.deps.audioContext = () => null;
    const other = await start(g);
    expect(other.state.audioBlocked).toBe(false);
    const seen = vi.fn();
    other.subscribe(seen);
    g.room.canPlaybackAudio = false;
    g.room.emit('audioPlaybackChanged', false);
    expect(other.state.audioBlocked).toBe(true);
    expect(seen).toHaveBeenCalled();
    g.room.canPlaybackAudio = true;
    g.room.emit('audioPlaybackChanged', true);
    expect(other.state.audioBlocked).toBe(false);
  });

  it('without a session context, startAudio() asks the room to start audio, then reads canPlaybackAudio again', async () => {
    const f = fakes();
    f.deps.audioContext = () => null;
    f.room.canPlaybackAudio = false;
    const call = await start(f);
    f.room.startAudio.mockImplementation(async () => { f.room.canPlaybackAudio = true; });
    await call.startAudio();
    expect(f.room.startAudio).toHaveBeenCalledTimes(1);
    expect(call.state.audioBlocked).toBe(false);
  });

  it('without a session context, a refused startAudio() leaves audioBlocked true and does not throw', async () => {
    const f = fakes();
    f.deps.audioContext = () => null;
    f.room.canPlaybackAudio = false;
    const call = await start(f);
    f.room.startAudio.mockRejectedValue(new Error('NotAllowedError'));
    await expect(call.startAudio()).resolves.toBeUndefined();
    expect(call.state.audioBlocked).toBe(true);
  });

  it('Review Focus 5: trackUnsubscribed detaches and removes that element; the speaker coming back attaches once', async () => {
    const f = fakes();
    await start(f);
    const bob = f.addRemote(BOB_DEV);
    const carol = f.addRemote(CAROL_DEV);
    const first = f.subscribeTrack(bob, 'microphone');
    const kept = f.subscribeTrack(carol, 'microphone');
    const el = first.track.made[0];
    f.unsubscribeTrack(bob, first.pub);
    expect(first.track.detach).toHaveBeenCalledWith(el);
    expect(el.isConnected).toBe(false);
    expect(elementsIn(f.audioHost)).toEqual([kept.track.made[0]]);
    // SpeakerPolicy lets bob back in: a new subscription (heard twice), one new element.
    const again = f.subscribeTrack(bob, 'microphone');
    f.room.emit('trackSubscribed', again.track, again.pub, bob);
    expect(again.track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toHaveLength(2);
  });

  it('a participant who leaves leaves no element of theirs', async () => {
    const f = fakes();
    await start(f);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    f.room.remoteParticipants.delete(BOB_DEV);
    f.room.emit('participantDisconnected', bob);
    expect(track.detach).toHaveBeenCalledWith(track.made[0]);
    expect(elementsIn(f.audioHost)).toEqual([]);
  });

  for (const reason of ['user', 'switch', 'pagehide', 'lost'] as const) {
    it(`Review Focus 4: leave('${reason}') detaches every attached track and empties the container`, async () => {
      const f = fakes();
      const call = await start(f);
      const bob = f.addRemote(BOB_DEV);
      const carol = f.addRemote(CAROL_DEV);
      const tracks = [f.subscribeTrack(bob, 'microphone').track, f.subscribeTrack(carol, 'microphone').track,
        f.subscribeTrack(bob, 'screen_share_audio').track];
      expect(elementsIn(f.audioHost)).toHaveLength(3);
      await call.leave(reason);
      expect(elementsIn(f.audioHost)).toEqual([]);
      for (const t of tracks) expect(t.detach).toHaveBeenCalledWith(t.made[0]);
      // Nothing is attached after the leave.
      f.subscribeTrack(fakeRemote(DEV_D), 'microphone');
      expect(elementsIn(f.audioHost)).toEqual([]);
    });
  }

  it('Review Focus 4: a second call after a leave starts from an empty container, one element per track', async () => {
    const f = fakes();
    const bob = fakeRemote(BOB_DEV);
    f.room.remoteParticipants.set(BOB_DEV, bob);
    const { track } = f.subscribeTrack(bob, 'microphone', false);
    const first = await start(f);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
    await first.leave('switch');
    expect(elementsIn(f.audioHost)).toEqual([]);
    await start(f);
    expect(track.attach).toHaveBeenCalledTimes(2);
    expect(track.attached).toHaveLength(1);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
  });

  it('the attached elements sit under aria-hidden and carry no controls', async () => {
    const f = fakes();
    await start(f);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    const el = track.made[0];
    expect(el.controls).toBe(false);
    expect(el.getAttribute('aria-hidden')).toBe('true');
  });
});

describe('web3.3: the default audio host', () => {
  it('is one hidden, aria-hidden container on document.body, created on first use', async () => {
    const f = fakes();
    delete f.deps.audioHost;
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    const again = f.subscribeTrack(bob, 'screen_share_audio');
    const hosts = document.querySelectorAll('[data-dilla-call-audio]');
    expect(hosts).toHaveLength(1);
    const host = hosts[0] as HTMLElement;
    expect(host.parentElement).toBe(document.body);
    expect(host.hidden).toBe(true);
    expect(host.getAttribute('aria-hidden')).toBe('true');
    expect(track.made[0].parentElement).toBe(host);
    expect(again.track.made[0].parentElement).toBe(host);
    await call.leave('user');
    expect(host.children).toHaveLength(0);
  });
});

describe('web3.3 fix round 1: the session’s AudioContext (I1, M2), no blip while silent (I3), abandon (M3)', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };
  const elementsIn = (host: HTMLElement) => [...host.querySelectorAll('audio, video')];
  /** The order of `track.attach` against the first later `p.setVolume(volume)`: attach first, then the volume. */
  const attachThenVolume = (p: ReturnType<typeof fakeRemote>, track: FakeRemoteTrack, volume: number) => {
    const at = track.attach.mock.invocationCallOrder[0];
    return at !== undefined && p.setVolume.mock.calls.some(([v, s], i) => v === volume && s === 'microphone'
      && p.setVolume.mock.invocationCallOrder[i] > at);
  };

  it('I1: the output device goes to the session’s AudioContext, where remote audio plays', async () => {
    const f = fakes();
    await start(f, { output: 'spk' });
    expect(f.contexts[0].setSinkId).toHaveBeenCalledWith('spk');
    const g = fakes();
    await start(g);
    expect(g.contexts[0].setSinkId).not.toHaveBeenCalled();
    // A browser whose AudioContext cannot choose an output (Firefox) joins all the same, on the system default.
    const h = fakes();
    h.audio.noSinkId = true;
    const call = await start(h, { output: 'spk' });
    expect(call.state.phase).toBe('connected');
  });

  it('I1: a refused setSinkId does not end the call', async () => {
    const f = fakes();
    f.deps.audioContext = () => {
      const ctx = new FakeAudioContext();
      ctx.setSinkId.mockRejectedValue(new Error('NotFoundError'));
      f.contexts.push(ctx);
      return ctx as unknown as AudioContext;
    };
    const call = await start(f, { output: 'gone' });
    await flush();
    expect(call.state.phase).toBe('connected');
  });

  it('V1: setOutput mid-call sends the new device to the session’s AudioContext', async () => {
    const f = fakes();
    const call = await start(f, { output: 'spk' });
    await call.setOutput('headset');
    expect(f.contexts[0].setSinkId).toHaveBeenLastCalledWith('headset');
    // '' is the system default, which setSinkId takes as it is.
    await call.setOutput('');
    expect(f.contexts[0].setSinkId).toHaveBeenLastCalledWith('');
  });

  it('V1: setOutput on a context that cannot choose an output (Firefox) is a no-op', async () => {
    const f = fakes();
    f.audio.noSinkId = true;
    const call = await start(f);
    await expect(call.setOutput('headset')).resolves.toBeUndefined();
    expect(call.state.phase).toBe('connected');
  });

  it('V1: a refused setOutput rejects, so the shell can say so, and the call goes on', async () => {
    const f = fakes();
    const call = await start(f);
    f.contexts[0].setSinkId.mockRejectedValueOnce(new Error('NotFoundError'));
    await expect(call.setOutput('gone')).rejects.toThrow('NotFoundError');
    expect(call.state.phase).toBe('connected');
  });

  it('V1: setOutput after the leave touches nothing', async () => {
    const f = fakes();
    const call = await start(f);
    await call.leave('user');
    f.contexts[0].setSinkId.mockClear();
    await call.setOutput('headset');
    expect(f.contexts[0].setSinkId).not.toHaveBeenCalled();
  });

  it('M2: audioBlocked follows the context: suspended at connect, running on statechange', async () => {
    const f = fakes();
    f.audio.contextState = 'suspended';
    const call = await start(f);
    expect(call.state.audioBlocked).toBe(true);
    const seen = vi.fn();
    call.subscribe(seen);
    f.contexts[0].to('running');
    expect(call.state.audioBlocked).toBe(false);
    expect(seen).toHaveBeenCalled();
    f.contexts[0].to('suspended');
    expect(call.state.audioBlocked).toBe(true);
  });

  it('M2: startAudio() resumes the context and starts the room’s audio within the press, before any await', async () => {
    const f = fakes();
    f.audio.contextState = 'suspended';
    const call = await start(f);
    const ctx = f.contexts[0];
    const pending = call.startAudio();
    // Both ran synchronously inside the click: nothing was awaited first.
    expect(ctx.resume).toHaveBeenCalledTimes(1);
    expect(f.room.startAudio).toHaveBeenCalledTimes(1);
    await pending;
    expect(call.state.audioBlocked).toBe(false);
  });

  it('M2: a context that will not resume leaves audioBlocked true and startAudio() does not throw', async () => {
    const f = fakes();
    f.audio.contextState = 'suspended';
    const call = await start(f);
    f.contexts[0].resume.mockRejectedValue(new Error('NotAllowedError'));
    await expect(call.startAudio()).resolves.toBeUndefined();
    expect(call.state.audioBlocked).toBe(true);
  });

  for (const reason of ['user', 'switch', 'pagehide', 'lost', 'blocked', 'removed'] as const) {
    it(`I1: leave('${reason}') closes the session’s AudioContext`, async () => {
      const f = fakes();
      const call = await start(f);
      await call.leave(reason);
      expect(f.contexts[0].close).toHaveBeenCalledTimes(1);
      expect(f.contexts[0].state).toBe('closed');
    });
  }

  it('I1: a failed start closes its AudioContext (joinCall failed, callJoin refused, an abort at the microphone prompt)', async () => {
    const f = fakes({ joinCallError: new Error('E_CALL_IN_OTHER_TAB') });
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toThrow();
    expect(f.contexts.every((c) => c.state === 'closed')).toBe(true);
    const g = fakes();
    g.deps.bridge.call = vi.fn(async () => { throw Object.assign(new Error('E_FORBIDDEN'), { code: 'E_FORBIDDEN' }); });
    await expect(startPageCall(g.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toThrow();
    expect(g.contexts.every((c) => c.state === 'closed')).toBe(true);
    const h = fakes({ getUserMedia: () => new Promise<MediaStream>(() => undefined) });
    const abort = new AbortController();
    const started = startPageCall(h.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, signal: abort.signal });
    await flush();
    abort.abort();
    await expect(started).rejects.toThrow(/E_ABORTED/);
    expect(h.contexts.every((c) => c.state === 'closed')).toBe(true);
  });

  it('I3: deafened, a newcomer is not attached; undeafen attaches it once, then sets its volume', async () => {
    const f = fakes();
    const call = await start(f);
    call.setDeafened(true);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    expect(track.attach).not.toHaveBeenCalled();
    expect(elementsIn(f.audioHost)).toEqual([]);
    bob.setVolume.mockClear();
    call.setDeafened(false);
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(attachThenVolume(bob, track, 1)).toBe(true);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
  });

  it('V2: state.audio counts the subscribed remote audio tracks (expected) and the attached ones', async () => {
    const f = fakes();
    const call = await start(f);
    expect(call.state.audio).toEqual({ attached: 0, expected: 0 });
    const seen = vi.fn();
    call.subscribe(() => { seen(call.state.audio); });
    const bob = f.addRemote(BOB_DEV);
    const mic = f.subscribeTrack(bob, 'microphone');
    expect(call.state.audio).toEqual({ attached: 1, expected: 1 });
    // A camera is not audio.
    f.subscribeTrack(bob, 'camera');
    expect(call.state.audio).toEqual({ attached: 1, expected: 1 });
    // Deafened: a new track is subscribed but held back, so it is expected and not attached; the one attached before
    // stays attached (its gain goes to 0).
    call.setDeafened(true);
    const screenAudio = f.subscribeTrack(bob, 'screen_share_audio');
    expect(call.state.audio).toEqual({ attached: 1, expected: 2 });
    call.setDeafened(false);
    expect(call.state.audio).toEqual({ attached: 2, expected: 2 });
    f.unsubscribeTrack(bob, screenAudio.pub);
    expect(call.state.audio).toEqual({ attached: 1, expected: 1 });
    f.unsubscribeTrack(bob, mic.pub);
    expect(call.state.audio).toEqual({ attached: 0, expected: 0 });
    expect(seen).toHaveBeenCalledWith({ attached: 1, expected: 2 });
  });

  it('V2: a pending track that leaves with its participant is no longer expected; the leave counts nothing', async () => {
    const f = fakes();
    const call = await start(f);
    call.setDeafened(true);
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'microphone');
    expect(call.state.audio).toEqual({ attached: 0, expected: 1 });
    f.room.remoteParticipants.delete(BOB_DEV);
    f.room.emit('participantDisconnected', bob);
    expect(call.state.audio).toEqual({ attached: 0, expected: 0 });
    call.setDeafened(false);
    const carol = f.addRemote(BOB_DEV);
    f.subscribeTrack(carol, 'microphone');
    expect(call.state.audio).toEqual({ attached: 1, expected: 1 });
    await call.leave('user');
    expect(call.state.audio).toEqual({ attached: 0, expected: 0 });
  });

  it('I3: a per-person volume of 0 holds the attach back until it rises to 50', async () => {
    const f = fakes();
    const call = await start(f);
    call.setVolume(BOB, 0);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    expect(track.attach).not.toHaveBeenCalled();
    bob.setVolume.mockClear();
    call.setVolume(BOB, 50);
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(attachThenVolume(bob, track, 0.5)).toBe(true);
  });

  it('I3: paused, a newcomer is not attached; the resume attaches it once', async () => {
    const f = fakes();
    const call = await start(f);
    await call.setPaused(true);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    expect(track.attach).not.toHaveBeenCalled();
    bob.setVolume.mockClear();
    await call.setPaused(false);
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(attachThenVolume(bob, track, 1)).toBe(true);
  });

  it('I3: a SpeakerPolicy re-subscribe while deafened attaches nothing; one element once undeafened', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    const first = f.subscribeTrack(bob, 'microphone');
    expect(first.track.attach).toHaveBeenCalledTimes(1);
    call.setDeafened(true);
    f.unsubscribeTrack(bob, first.pub);
    expect(elementsIn(f.audioHost)).toEqual([]);
    const again = f.subscribeTrack(bob, 'microphone');
    expect(again.track.attach).not.toHaveBeenCalled();
    // Excluded again before the undeafen: nothing is left pending for it.
    f.unsubscribeTrack(bob, again.pub);
    const third = f.subscribeTrack(bob, 'microphone');
    call.setDeafened(false);
    expect(again.track.attach).not.toHaveBeenCalled();
    expect(third.track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
  });

  it('I3: a call started deafened attaches nothing at connect; a pending track that leaves is never attached', async () => {
    const f = fakes();
    const bob = fakeRemote(BOB_DEV);
    f.room.remoteParticipants.set(BOB_DEV, bob);
    const { track } = f.subscribeTrack(bob, 'microphone', false);
    const call = await start(f, { self: 'deafened' });
    expect(track.attach).not.toHaveBeenCalled();
    f.room.remoteParticipants.delete(BOB_DEV);
    f.room.emit('participantDisconnected', bob);
    call.setDeafened(false);
    expect(track.attach).not.toHaveBeenCalled();
    await call.leave('user');
    expect(elementsIn(f.audioHost)).toEqual([]);
  });

  it('I3: the tile of an unattached (deafened) speaker still turns ok: verification is the media worker’s, not the element’s', async () => {
    const f = fakes();
    const call = await start(f);
    call.setDeafened(true);
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'microphone');
    f.verified.add(BOB_DEV);
    await f.advance(1_000);
    expect(call.state.tiles.find((t) => t.deviceId === BOB_DEV)?.state).toBe('ok');
  });

  it('M3: a start that fails after the room listeners were registered unregisters them and attaches nothing', async () => {
    const f = fakes();
    const bob = fakeRemote(BOB_DEV);
    f.room.remoteParticipants.set(BOB_DEV, bob);
    const { track } = f.subscribeTrack(bob, 'microphone', false);
    const publish = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementationOnce(async (t, o) => {
      const out = await publish(t, o);
      (f.room as unknown as { state: string }).state = 'disconnected';
      return out;
    });
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts)).rejects.toThrow(/E_DISCONNECTED/);
    for (const e of ['trackSubscribed', 'trackUnsubscribed', 'participantConnected', 'participantDisconnected',
      'activeSpeakersChanged', 'audioPlaybackChanged']) expect(f.room.count(e), e).toBe(0);
    f.room.emit('trackSubscribed', track, undefined, bob);
    expect(track.attach).not.toHaveBeenCalled();
    expect(elementsIn(f.audioHost)).toEqual([]);
    expect(f.contexts[0].state).toBe('closed');
  });
});

describe('web3.3 fix round 2: the passed context, newcomers before the roster, the lingering line, a failed attach', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };
  const elementsIn = (host: HTMLElement) => [...host.querySelectorAll('audio, video')];

  it('Minor 1: a context passed in (made in the press) is the one used and closed; none of its own is made', async () => {
    const f = fakes();
    const passed = new FakeAudioContext();
    const call = await start(f, { audioContext: passed as unknown as AudioContext, output: 'spk' });
    expect(f.contexts).toHaveLength(0);
    expect(f.joinOptions?.roomOptions?.webAudioMix).toEqual({ audioContext: passed });
    expect(passed.setSinkId).toHaveBeenCalledWith('spk');
    await call.leave('user');
    expect(passed.close).toHaveBeenCalledTimes(1);
  });

  it('Minor 1: a passed context is closed by a start that fails', async () => {
    const f = fakes({ joinCallError: new Error('E_CALL_IN_OTHER_TAB') });
    const passed = new FakeAudioContext();
    await expect(startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, audioContext: passed as unknown as AudioContext }))
      .rejects.toThrow();
    expect(passed.state).toBe('closed');
  });

  it('Minor 2: a track subscribed before its device is in the roster waits; muted by its volume it never plays; at 50 it attaches once', async () => {
    const f = fakes();
    const call = await start(f, { volumes: { [ALICE_USER]: 0 } });
    const alice = f.addRemote(ALICE);
    const { track } = f.subscribeTrack(alice, 'microphone');
    // Not in the roster yet: its gain is unknown, so it is not attached at full gain.
    expect(track.attach).not.toHaveBeenCalled();
    f.pushKeys(callId, keysFor(2n, [...BASE_ROSTER, { leaf: 3, deviceId: ALICE, userId: ALICE_USER }]));
    await flush(); // the roster moves once the media worker holds the epoch’s key
    // In the roster at volume 0: still nothing.
    expect(track.attach).not.toHaveBeenCalled();
    alice.setVolume.mockClear();
    call.setVolume(ALICE_USER, 50);
    expect(track.attach).toHaveBeenCalledTimes(1);
    const at = track.attach.mock.invocationCallOrder[0];
    expect(alice.setVolume.mock.calls.some(([v, s], i) => v === 0.5 && s === 'microphone' && alice.setVolume.mock.invocationCallOrder[i] > at))
      .toBe(true);
  });

  it('Minor 2: a newcomer the roster maps later at full volume is attached by that roster change', async () => {
    const f = fakes();
    await start(f);
    const alice = f.addRemote(ALICE);
    const { track } = f.subscribeTrack(alice, 'microphone');
    expect(track.attach).not.toHaveBeenCalled();
    f.pushKeys(callId, keysFor(2n, [...BASE_ROSTER, { leaf: 3, deviceId: ALICE, userId: ALICE_USER }]));
    await flush(); // the roster moves once the media worker holds the epoch’s key
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(f.audioHost)).toHaveLength(1);
  });

  it('Minor 3: with a session context, the line goes once the context runs, whatever canPlaybackAudio still says', async () => {
    const f = fakes();
    f.audio.contextState = 'suspended';
    f.room.canPlaybackAudio = false;
    const call = await start(f);
    expect(call.state.audioBlocked).toBe(true);
    f.contexts[0].to('running');
    expect(call.state.audioBlocked).toBe(false);
    f.room.emit('audioPlaybackChanged', false);
    expect(call.state.audioBlocked).toBe(false);
  });

  it('Nit 1: no host yet, or an attach that throws, keeps the track pending; the next gain change attaches it once', async () => {
    const f = fakes();
    const host = f.audioHost;
    let hostReady = false;
    f.deps.audioHost = () => (hostReady ? host : null);
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    const { track } = f.subscribeTrack(bob, 'microphone');
    expect(track.attach).not.toHaveBeenCalled();
    hostReady = true;
    track.attach.mockImplementationOnce(() => { throw new Error('NotSupportedError'); });
    call.setVolume(BOB, 90);
    expect(track.attach).toHaveBeenCalledTimes(1);
    expect(elementsIn(host)).toEqual([]);
    call.setVolume(BOB, 80);
    expect(track.attach).toHaveBeenCalledTimes(2);
    expect(elementsIn(host)).toHaveLength(1);
    call.setVolume(BOB, 70);
    expect(track.attach).toHaveBeenCalledTimes(2);
  });
});

it('PTT privacy is enforced by the track even when the denoiser is present', async () => {
  const f=fakes(), allowed:boolean[]=[];
  f.deps.createNoiseProcessor=()=>({setPttActive:()=>{},setCaptureAllowed:(a:boolean)=>allowed.push(a)});
  const call=await startPageCall(f.deps,{channelId:CH,vdec:'vp8'},{...opts,noise:'on',mode:'ptt'});
  try {
    expect(f.localTracks[0].muted).toBe(true);
    call.pushToTalk(true);expect(allowed.at(-1)).toBe(true);
    call.setMuted(true);expect(allowed.at(-1)).toBe(false);
    call.pushToTalk(false);call.setMuted(false);expect(allowed.at(-1)).toBe(false);
  } finally {await call.leave('user');}
});

describe('web3.9 VOICE-FIX fix round 1 (I1): concurrentCapture', () => {
  const WEBKIT = 'AppleWebKit/605.1.15 (KHTML, like Gecko)';
  const CHROMIUM = 'AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36';
  const IPHONE = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)';
  const rows: Array<[string, string, boolean]> = [
    ['Chrome on Linux', `Mozilla/5.0 (X11; Linux x86_64) ${CHROMIUM}`, true],
    ['Chrome on Windows', `Mozilla/5.0 (Windows NT 10.0; Win64; x64) ${CHROMIUM}`, true],
    ['Chrome on macOS', `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) ${CHROMIUM}`, true],
    ['Edge on Windows', `Mozilla/5.0 (Windows NT 10.0; Win64; x64) ${CHROMIUM} Edg/130.0.0.0`, true],
    ['Firefox on Linux', 'Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0', true],
    ['Firefox on macOS', 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:131.0) Gecko/20100101 Firefox/131.0', true],
    ['Safari on macOS', `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) ${WEBKIT} Version/18.0 Safari/605.1.15`, false],
    ['Safari on iPhone', `${IPHONE} ${WEBKIT} Version/18.0 Mobile/15E148 Safari/604.1`, false],
    ['iPad Safari in desktop mode', `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) ${WEBKIT} Version/18.0 Safari/605.1.15`, false],
    ['Chrome on iOS (CriOS)', `${IPHONE} ${WEBKIT} CriOS/130.0.6723.90 Mobile/15E148 Safari/604.1`, false],
    ['Firefox on iOS (FxiOS)', `${IPHONE} ${WEBKIT} FxiOS/131.0 Mobile/15E148 Safari/605.1.15`, false],
    ['Edge on iOS (EdgiOS)', `${IPHONE} ${WEBKIT} EdgiOS/130.0.2849.80 Version/18.0 Mobile/15E148 Safari/604.1`, false],
    ['an unknown user agent', 'curl/8.10.1', false],
    // Final review I-1: getBrowser sets no os on Android, so the user agent itself rules phones and tablets out.
    ['Chrome on Android', `Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36`, false],
    ['Chrome on an Android tablet', `Mozilla/5.0 (Linux; Android 14; SM-X710) ${CHROMIUM}`, false],
    ['Firefox on Android', 'Mozilla/5.0 (Android 14; Mobile; rv:131.0) Gecko/131.0 Firefox/131.0', false],
    ['Samsung Internet', 'Mozilla/5.0 (Linux; Android 14; SM-S921B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/26.0 Chrome/122.0.0.0 Mobile Safari/537.36', false],
  ];
  for (const [name, ua, want] of rows) {
    it(`${name}: ${String(want)}`, () => {
      expect(concurrentCapture(getBrowser(ua, true), ua)).toBe(want);
    });
  }
  it('no browser details: false', () => {
    expect(concurrentCapture(undefined, '')).toBe(false);
  });
});

describe('web3.9 ruling V-1: the session owns page leave, LiveKit does not', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });

  it('a beforeunload alone leaves the call joined', async () => {
    const f = fakes();
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts);
    open.push(call);
    f.order.length = 0;
    window.dispatchEvent(new Event('beforeunload'));
    await flush();
    expect(call.state.phase).toBe('connected');
    expect(f.calls.filter((c) => c.m === 'callLeave')).toEqual([]);
    expect(f.room.disconnect).not.toHaveBeenCalled();
    expect(f.order).toEqual([]);
  });

  it('pagehide still leaves with callLeave("pagehide") (the worker’s self-Remove), exactly once', async () => {
    const f = fakes();
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts);
    window.dispatchEvent(new Event('beforeunload'));
    window.dispatchEvent(new Event('pagehide'));
    window.dispatchEvent(new Event('pagehide'));
    await flush();
    expect(call.state.phase).toBe('ended');
    expect(f.calls.filter((c) => c.m === 'callLeave')).toEqual([{ m: 'callLeave', reason: 'pagehide' }]);
    expect(f.manager.dispose).toHaveBeenCalled();
  });
});

describe('#37: a call knows whether a share carries sound, on both sides', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };
  /** What getDisplayMedia hands back for a surface: its video track (displaySurface) and, when `withAudio`, one audio track. */
  const picked = (surface: 'browser' | 'monitor' | 'window', withAudio: boolean) => {
    const video = { kind: 'video', stop: vi.fn(), addEventListener: () => undefined, removeEventListener: () => undefined,
      getSettings: () => ({ displaySurface: surface }) } as unknown as MediaStreamTrack;
    const audioStop = vi.fn();
    const endedFns = new Set<() => void>();
    const audio = withAudio ? { kind: 'audio', stop: audioStop, getSettings: () => ({}),
      addEventListener: (_t: string, fn: () => void) => { endedFns.add(fn); },
      removeEventListener: (_t: string, fn: () => void) => { endedFns.delete(fn); } } as unknown as MediaStreamTrack : null;
    const tracks = audio === null ? [video] : [video, audio];
    const stream = { getTracks: () => tracks, getAudioTracks: () => tracks.filter((t) => t.kind === 'audio'),
      getVideoTracks: () => [video] } as unknown as MediaStream;
    return { video, audio, audioStop, stream, endAudio: () => { for (const fn of [...endedFns]) fn(); } };
  };
  const sources = (f: ReturnType<typeof fakes>) => f.room.localParticipant.publishTrack.mock.calls.map((c) => c[1].source);

  it('a share without sound says so', async () => {
    const f = fakes();
    const call = await start(f);
    expect(call.state.shareAudio).toBeNull();
    f.deps.media.getDisplayMedia = vi.fn(async () => picked('monitor', false).stream);
    f.room.localParticipant.publishTrack.mockClear();
    await call.setSharing(true);
    expect(call.state.sharing).toBe(true);
    expect(call.state.shareAudio).toBe('none');
    expect(sources(f)).toEqual(['screen_share']);
  });

  it('a tab share with sound publishes it as screen_share_audio', async () => {
    const f = fakes();
    const call = await start(f);
    const s = picked('browser', true);
    f.deps.media.getDisplayMedia = vi.fn(async () => s.stream);
    f.room.localParticipant.publishTrack.mockClear();
    await call.setSharing(true);
    expect(call.state.shareAudio).toBe('tab');
    expect(f.room.localParticipant.publishTrack).toHaveBeenCalledTimes(2);
    expect(f.room.localParticipant.publishTrack.mock.calls[1][0]).toBe(s.audio);
    // The fixture's caps are [64 000, 2 500 000, 0]: screenAudioOptions(caps) holds F2's 96 kbit/s to the instance's 64.
    expect(f.room.localParticipant.publishTrack.mock.calls[1][1])
      .toEqual(screenAudioOptions({ maxAudioBitrateBps: 64_000, maxShareBitrateBps: 2_500_000, vp9: false }));
  });

  it('m1: a share whose audio track ends while the video goes on turns to none, and the audio is unpublished', async () => {
    const f = fakes();
    const call = await start(f);
    const s = picked('browser', true);
    f.deps.media.getDisplayMedia = vi.fn(async () => s.stream);
    await call.setSharing(true);
    expect(call.state.shareAudio).toBe('tab');
    const seen = vi.fn();
    call.subscribe(seen);
    s.endAudio();
    await f.advance(0);
    expect(call.state.shareAudio).toBe('none');
    expect(call.state.sharing).toBe(true);
    expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledWith(s.audio);
    expect(seen).toHaveBeenCalled();
    await call.setSharing(false);
    expect(call.state.shareAudio).toBeNull();
  });

  it('a system share with sound is published as is', async () => {
    // D5: no restrictOwnAudio check; whatever audio the browser returned is published.
    const f = fakes();
    const call = await start(f);
    const s = picked('window', true);
    f.deps.media.getDisplayMedia = vi.fn(async () => s.stream);
    f.room.localParticipant.publishTrack.mockClear();
    await call.setSharing(true);
    expect(call.state.shareAudio).toBe('system');
    expect(sources(f)).toEqual(['screen_share', 'screen_share_audio']);
    expect(s.audioStop).not.toHaveBeenCalled();
  });

  it('a share whose audio the grant does not allow is a share without sound', async () => {
    const f = fakes();
    const call = await start(f);
    const s = picked('browser', true);
    f.deps.media.getDisplayMedia = vi.fn(async () => s.stream);
    const answer = f.deps.bridge.call.bind(f.deps.bridge);
    f.deps.bridge.call = vi.fn(async (cmd: Command) => {
      if (cmd.m !== 'callShare' || !cmd.on) return answer(cmd);
      f.room.localParticipant.permissions = { canPublish: true, canPublishSources: [1, 2, 3] };
      f.room.emit('participantPermissionsChanged', undefined, f.room.localParticipant);
      return null;
    });
    f.room.localParticipant.publishTrack.mockClear();
    await call.setSharing(true);
    expect(sources(f)).toEqual(['screen_share']);
    expect(call.state.shareAudio).toBe('none');
    expect(s.audioStop).toHaveBeenCalled();
  });

  it('stopping the share clears shareAudio, and so does the leave', async () => {
    const f = fakes();
    const call = await start(f);
    f.deps.media.getDisplayMedia = vi.fn(async () => picked('browser', true).stream);
    await call.setSharing(true);
    expect(call.state.shareAudio).toBe('tab');
    await call.setSharing(false);
    expect(call.state.sharing).toBe(false);
    expect(call.state.shareAudio).toBeNull();
    await call.setSharing(true);
    expect(call.state.shareAudio).toBe('tab');
    await call.leave('user');
    expect(call.state.shareAudio).toBeNull();
  });

  it('a tile says whether its screen carries sound', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'screen_share');
    f.verified.add(BOB_DEV);
    await f.advance(1_000);
    const bobTile = () => call.state.tiles.find((t) => t.deviceId === BOB_DEV)!;
    expect(bobTile().state).toBe('ok');
    expect(bobTile().screenAudio).toBe(false);
    f.subscribeTrack(bob, 'screen_share_audio');
    expect(bobTile().screenAudio).toBe(true);
    // An unverified tile, or a paused call, never says a screen carries sound (as `screen` is null then).
    const d = f.addRemote(DEV_D);
    f.subscribeTrack(d, 'screen_share_audio');
    expect(call.state.tiles.find((t) => t.deviceId === DEV_D)!.screenAudio).toBe(false);
    await call.setPaused(true);
    expect(bobTile().screenAudio).toBe(false);
  });

  it('the tile mark follows a late audio publication and an unpublish', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'screen_share');
    f.verified.add(BOB_DEV);
    await f.advance(1_000);
    const bobTile = () => call.state.tiles.find((t) => t.deviceId === BOB_DEV)!;
    expect(bobTile().screenAudio).toBe(false);
    // Published, not subscribed yet (no trackSubscribed): the room's trackPublished turns the mark at once, no poll.
    const pub = { source: 'screen_share_audio', kind: 'audio', trackSid: 'TR_late' };
    bob.trackPublications.set(pub.trackSid, pub);
    f.room.emit('trackPublished', pub, bob);
    expect(bobTile().screenAudio).toBe(true);
    // livekit drops the publication before it says trackUnpublished.
    bob.trackPublications.delete(pub.trackSid);
    f.room.emit('trackUnpublished', pub, bob);
    expect(bobTile().screenAudio).toBe(false);
  });

  it('the mark does not change when the viewer deafens', async () => {
    const f = fakes();
    const call = await start(f);
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'screen_share');
    f.verified.add(BOB_DEV);
    await f.advance(1_000);
    call.setDeafened(true);
    const { track } = f.subscribeTrack(bob, 'screen_share_audio');
    await f.advance(1_000);
    expect(call.state.tiles.find((t) => t.deviceId === BOB_DEV)!.screenAudio).toBe(true);
    // Deafened: the share audio is held back, never attached, and its gain is 0.
    expect(track.attach).not.toHaveBeenCalled();
    expect(bob.gains.screen_share_audio).toBe(0);
  });

  describe('FX2: a "sound from" device as the share\'s sound', () => {
    const deviceTrack = () => {
      const stop = vi.fn();
      const track = { kind: 'audio', enabled: true, stop, getSettings: () => ({}), addEventListener: () => undefined, removeEventListener: () => undefined } as unknown as MediaStreamTrack;
      const stream = { getTracks: () => [track], getAudioTracks: () => [track], getVideoTracks: () => [] } as unknown as MediaStream;
      return { track, stop, stream };
    };
    /** A call sharing without sound, whose getUserMedia hands out `devs` in turn for the picked device. */
    const sharing = async (f: ReturnType<typeof fakes>, surface: 'browser' | 'monitor' = 'monitor', withAudio = false) => {
      const call = await start(f);
      const s = picked(surface, withAudio);
      f.deps.media.getDisplayMedia = vi.fn(async () => s.stream);
      await call.setSharing(true);
      return call;
    };

    it('publishes the chosen device as screen_share_audio, opened raw and stereo', async () => {
      const f = fakes();
      const call = await sharing(f);
      expect(call.state.shareSource).toBeNull();
      const d = deviceTrack();
      const gum = vi.fn(async () => d.stream);
      f.deps.media.getUserMedia = gum;
      f.room.localParticipant.publishTrack.mockClear();
      await call.setShareSource('monitor-1');
      expect(gum).toHaveBeenCalledWith({ audio: { deviceId: { exact: 'monitor-1' },
        echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2 } });
      expect(sources(f)).toEqual(['screen_share_audio']);
      expect(f.room.localParticipant.publishTrack.mock.calls[0][0]).toBe(d.track);
      expect(f.room.localParticipant.publishTrack.mock.calls[0][1])
        .toEqual(screenAudioOptions({ maxAudioBitrateBps: 64_000, maxShareBitrateBps: 2_500_000, vp9: false }));
      expect(call.state.shareSource).toBe('monitor-1');
      expect(call.state.shareAudio).toBe('device');
    });

    it('replaces the device: the old track is unpublished and stopped, the new one published', async () => {
      const f = fakes();
      const call = await sharing(f);
      const a = deviceTrack();
      const b = deviceTrack();
      f.deps.media.getUserMedia = vi.fn().mockResolvedValueOnce(a.stream).mockResolvedValueOnce(b.stream);
      await call.setShareSource('one');
      f.room.localParticipant.publishTrack.mockClear();
      await call.setShareSource('two');
      expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledWith(a.track);
      expect(a.stop).toHaveBeenCalled();
      expect(f.room.localParticipant.publishTrack.mock.calls[0][0]).toBe(b.track);
      expect(call.state.shareSource).toBe('two');
    });

    it('none unpublishes it, and the share is silent again', async () => {
      const f = fakes();
      const call = await sharing(f);
      const a = deviceTrack();
      f.deps.media.getUserMedia = vi.fn(async () => a.stream);
      await call.setShareSource('one');
      await call.setShareSource(null);
      expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledWith(a.track);
      expect(a.stop).toHaveBeenCalled();
      expect(call.state.shareSource).toBeNull();
      expect(call.state.shareAudio).toBe('none');
      expect(call.state.sharing).toBe(true);
    });

    it('the device track ends with the share', async () => {
      const f = fakes();
      const call = await sharing(f);
      const a = deviceTrack();
      f.deps.media.getUserMedia = vi.fn(async () => a.stream);
      await call.setShareSource('one');
      await call.setSharing(false);
      expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledWith(a.track);
      expect(a.stop).toHaveBeenCalled();
      expect(call.state.shareSource).toBeNull();
      expect(call.state.shareAudio).toBeNull();
    });

    it('a device track that ends on its own leaves a silent share', async () => {
      const f = fakes();
      const call = await sharing(f);
      const fns = new Set<() => void>();
      const a = deviceTrack();
      (a.track as unknown as { addEventListener: (t: string, fn: () => void) => void }).addEventListener = (_t, fn) => { fns.add(fn); };
      f.deps.media.getUserMedia = vi.fn(async () => a.stream);
      await call.setShareSource('one');
      for (const fn of [...fns]) fn();
      await f.advance(0);
      expect(call.state.shareSource).toBeNull();
      expect(call.state.shareAudio).toBe('none');
    });

    it('a browser sound that is already there wins: the device is refused and never opened', async () => {
      const f = fakes();
      const call = await sharing(f, 'browser', true);
      expect(call.state.shareSource).toBe('browser');
      const gum = vi.fn(async () => deviceTrack().stream);
      f.deps.media.getUserMedia = gum;
      await expect(call.setShareSource('one')).rejects.toMatchObject({ code: 'E_UNAVAILABLE' });
      expect(gum).not.toHaveBeenCalled();
      expect(call.state.shareAudio).toBe('tab');
    });

    it('needs a running share', async () => {
      const f = fakes();
      const call = await start(f);
      await expect(call.setShareSource('one')).rejects.toMatchObject({ code: 'E_UNAVAILABLE' });
    });

    it('lists the audio inputs, flags monitors by label, and leaves out the call microphone', async () => {
      const dev = (deviceId: string, label: string, kind = 'audioinput') => ({ kind, deviceId, label }) as MediaDeviceInfo;
      const mic = { kind: 'audio', stop: vi.fn(), getSettings: () => ({ deviceId: 'mic-1' }) } as unknown as MediaStreamTrack;
      const f = fakes({
        devices: [dev('default', 'Default'), dev('mic-1', 'Built-in Microphone'), dev('mon-1', 'Monitor of Built-in Audio Analog Stereo'),
          dev('app-1', 'Browser sink'), dev('out-1', 'Speakers', 'audiooutput')],
        getUserMedia: async () => ({ getTracks: () => [mic], getAudioTracks: () => [mic], getVideoTracks: () => [] }) as unknown as MediaStream,
      });
      const call = await start(f);
      expect(await call.shareSources()).toEqual({ labeled: true, sources: [
        { id: 'mon-1', label: 'Monitor of Built-in Audio Analog Stereo', monitor: true },
        { id: 'app-1', label: 'Browser sink', monitor: false },
      ] });
    });

    it('M4: switching devices replaces the track on the existing publication, never a second sound track', async () => {
      const f = fakes();
      const call = await sharing(f);
      const a = deviceTrack();
      const b = deviceTrack();
      f.deps.media.getUserMedia = vi.fn().mockResolvedValueOnce(a.stream).mockResolvedValueOnce(b.stream);
      const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
      const replaceTrack = vi.fn(async () => undefined);
      f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
        const pub = await real(track, o);
        (pub.track as unknown as { replaceTrack: unknown }).replaceTrack = replaceTrack;
        return pub;
      });
      await call.setShareSource('one');
      f.room.localParticipant.publishTrack.mockClear();
      f.room.localParticipant.unpublishTrack.mockClear();
      await call.setShareSource('two');
      expect(replaceTrack).toHaveBeenCalledWith(b.track, true);
      expect(f.room.localParticipant.publishTrack).not.toHaveBeenCalled();
      expect(f.room.localParticipant.unpublishTrack).not.toHaveBeenCalled();
      expect(a.stop).toHaveBeenCalled();
      expect(call.state.shareSource).toBe('two');
      await call.setSharing(false);
      expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledWith(b.track);
      expect(b.stop).toHaveBeenCalled();
    });

    it('I2: a pause silences the published device track at once, while a pick waits on the browser', async () => {
      const f = fakes();
      const call = await sharing(f);
      const a = deviceTrack();
      f.deps.media.getUserMedia = vi.fn(async () => a.stream);
      await call.setShareSource('one');
      let release!: (s: MediaStream) => void;
      f.deps.media.getUserMedia = vi.fn(() => new Promise<MediaStream>((r) => { release = r; }));
      const pick = call.setShareSource('two').catch(() => undefined);
      await f.advance(0);
      void call.setPaused(true);
      expect(a.track.enabled).toBe(false);
      release(deviceTrack().stream);
      await pick;
    });

    it('I1: the call microphone is left out when the call uses the default input (its real entry shares the alias’s group)', async () => {
      const dev = (deviceId: string, label: string, groupId: string) => ({ kind: 'audioinput', deviceId, label, groupId }) as MediaDeviceInfo;
      const mic = { kind: 'audio', stop: vi.fn(), getSettings: () => ({ deviceId: 'default', groupId: 'g1' }) } as unknown as MediaStreamTrack;
      const f = fakes({
        devices: [dev('default', 'Default - Built-in Microphone', 'g1'), dev('communications', 'Communications - Built-in Microphone', 'g1'),
          dev('real-1', 'Built-in Microphone', 'g1'), dev('mon-1', 'Monitor of Built-in Audio', 'g1'), dev('usb-1', 'USB Mic', 'g2')],
        getUserMedia: async () => ({ getTracks: () => [mic], getAudioTracks: () => [mic], getVideoTracks: () => [] }) as unknown as MediaStream,
      });
      const call = await start(f);
      expect((await call.shareSources()).sources.map((d) => d.id)).toEqual(['mon-1', 'usb-1']);
    });

    it('Firefox device shapes: the mic by its real id, monitors flagged, outputs ignored', async () => {
      const dev = (kind: string, deviceId: string, label: string, groupId: string) => ({ kind, deviceId, label, groupId }) as MediaDeviceInfo;
      const mic = { kind: 'audio', stop: vi.fn(), getSettings: () => ({ deviceId: 'jVrkpdHLALC5tU+5cm/3rUPh5c489GuVQ+7GzLImvpI=', groupId: 'eZoF6d9F=' }) } as unknown as MediaStreamTrack;
      const f = fakes({
        devices: [dev('audioinput', 'jVrkpdHLALC5tU+5cm/3rUPh5c489GuVQ+7GzLImvpI=', 'Built-in Audio Analog Stereo', 'eZoF6d9F='),
          dev('audioinput', 'Qm9v+monitor/1=', 'Monitor of Built-in Audio Analog Stereo', 'Doj8bPZA='),
          dev('audioinput', 'Qm9v+monitor/2=', 'Monitor of dilla-share', 'X+IaQfmE='),
          dev('audiooutput', 'y4ZTovCCOc4/', 'Speakers', 'Doj8bPZA=')],
        getUserMedia: async () => ({ getTracks: () => [mic], getAudioTracks: () => [mic], getVideoTracks: () => [] }) as unknown as MediaStream,
      });
      const call = await start(f);
      expect(await call.shareSources()).toEqual({ labeled: true, sources: [
        { id: 'Qm9v+monitor/1=', label: 'Monitor of Built-in Audio Analog Stereo', monitor: true },
        { id: 'Qm9v+monitor/2=', label: 'Monitor of dilla-share', monitor: false },
      ] });
    });

    /** A call sharing with device `one` published, whose publication's replaceTrack is held until `release()`. */
    const heldSwap = async (f: ReturnType<typeof fakes>) => {
      const call = await sharing(f);
      const a = deviceTrack();
      const b = deviceTrack();
      f.deps.media.getUserMedia = vi.fn().mockResolvedValueOnce(a.stream).mockResolvedValueOnce(b.stream);
      const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
      let release!: () => void;
      const held = new Promise<void>((r) => { release = r; });
      f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
        const pub = await real(track, o);
        (pub.track as unknown as { replaceTrack: unknown }).replaceTrack = vi.fn(async (n: MediaStreamTrack) => {
          await held;
          (pub.track as unknown as { mediaStreamTrack: unknown }).mediaStreamTrack = n;
        });
        return pub;
      });
      await call.setShareSource('one');
      const swap = call.setShareSource('two').catch(() => undefined);
      await f.advance(0);
      return { call, a, b, release, swap };
    };

    it('N-M2: a pause while the swap is pending leaves the new capture stopped once it lands', async () => {
      const f = fakes();
      const h = await heldSwap(f);
      void h.call.setPaused(true);
      h.release();
      await h.swap;
      expect(h.b.stop).toHaveBeenCalled();
    });

    it('N-M2: a leave while the swap is pending leaves no capture running', async () => {
      const f = fakes();
      const h = await heldSwap(f);
      const left = h.call.leave('user');
      h.release();
      await h.swap;
      await left;
      expect(h.b.stop).toHaveBeenCalled();
      expect(h.a.stop).toHaveBeenCalled();
    });

    it('N-N2: a replaceTrack that threw after the sender took the new track counts as replaced, never republished', async () => {
      const f = fakes();
      const call = await sharing(f);
      const a = deviceTrack();
      const b = deviceTrack();
      f.deps.media.getUserMedia = vi.fn().mockResolvedValueOnce(a.stream).mockResolvedValueOnce(b.stream);
      const real = f.room.localParticipant.publishTrack.getMockImplementation()!;
      f.room.localParticipant.publishTrack.mockImplementation(async (track, o) => {
        const pub = await real(track, o);
        (pub.track as unknown as { replaceTrack: unknown }).replaceTrack = vi.fn(async (n: MediaStreamTrack) => {
          (pub.track as unknown as { mediaStreamTrack: unknown }).mediaStreamTrack = n;
          throw new Error('resumeUpstream');
        });
        return pub;
      });
      await call.setShareSource('one');
      f.room.localParticipant.publishTrack.mockClear();
      await call.setShareSource('two');
      expect(f.room.localParticipant.publishTrack).not.toHaveBeenCalled();
      expect(a.stop).toHaveBeenCalled();
      expect(call.state.shareSource).toBe('two');
    });

    it('N-M1: a real-id microphone hides only itself: a same-group "Broadcast Stream Mix" stays', async () => {
      const dev = (deviceId: string, label: string, groupId: string) => ({ kind: 'audioinput', deviceId, label, groupId }) as MediaDeviceInfo;
      const mic = { kind: 'audio', stop: vi.fn(), getSettings: () => ({ deviceId: 'chat-mic', groupId: 'goxlr' }) } as unknown as MediaStreamTrack;
      const f = fakes({
        devices: [dev('chat-mic', 'GoXLR Chat Mic', 'goxlr'), dev('stream-mix', 'GoXLR Broadcast Stream Mix', 'goxlr'), dev('usb', 'USB Mic', 'g2')],
        getUserMedia: async () => ({ getTracks: () => [mic], getAudioTracks: () => [mic], getVideoTracks: () => [] }) as unknown as MediaStream,
      });
      const call = await start(f);
      expect((await call.shareSources()).sources.map((d) => d.id)).toEqual(['stream-mix', 'usb']);
    });

    it('N-M1: behind the default alias only the entry with the alias label goes; a same-group "Stereo Mix" stays', async () => {
      const dev = (deviceId: string, label: string, groupId: string) => ({ kind: 'audioinput', deviceId, label, groupId }) as MediaDeviceInfo;
      const mic = { kind: 'audio', stop: vi.fn(), getSettings: () => ({ deviceId: 'default', groupId: 'g1' }) } as unknown as MediaStreamTrack;
      const f = fakes({
        devices: [dev('default', 'Default - Realtek Microphone', 'g1'), dev('real', 'Realtek Microphone', 'g1'), dev('mix', 'Stereo Mix (Realtek)', 'g1')],
        getUserMedia: async () => ({ getTracks: () => [mic], getAudioTracks: () => [mic], getVideoTracks: () => [] }) as unknown as MediaStream,
      });
      const call = await start(f);
      expect((await call.shareSources()).sources.map((d) => d.id)).toEqual(['mix']);
    });

    it('N-M1: when no entry carries the alias label, the same-group non-monitors go (the fallback)', async () => {
      const dev = (deviceId: string, label: string, groupId: string) => ({ kind: 'audioinput', deviceId, label, groupId }) as MediaDeviceInfo;
      const mic = { kind: 'audio', stop: vi.fn(), getSettings: () => ({ deviceId: 'default', groupId: 'g1' }) } as unknown as MediaStreamTrack;
      const f = fakes({
        devices: [dev('default', 'Default', 'g1'), dev('real', 'Built-in Microphone', 'g1'), dev('mon', 'Monitor of Built-in', 'g1')],
        getUserMedia: async () => ({ getTracks: () => [mic], getAudioTracks: () => [mic], getVideoTracks: () => [] }) as unknown as MediaStream,
      });
      const call = await start(f);
      expect((await call.shareSources()).sources.map((d) => d.id)).toEqual(['mon']);
    });

    it('M1: Chromium without microphone permission lists one unlabeled input with an empty id: the labels are missing', async () => {
      const f = fakes({ devices: [{ kind: 'audioinput', deviceId: '', label: '', groupId: '' } as MediaDeviceInfo] });
      const call = await start(f);
      expect(await call.shareSources()).toEqual({ labeled: false, sources: [] });
    });

    it('M5: the monitor of the docs’ dilla-share sink is not warned about, another monitor is', async () => {
      const dev = (deviceId: string, label: string) => ({ kind: 'audioinput', deviceId, label }) as MediaDeviceInfo;
      const f = fakes({ devices: [dev('m1', 'Monitor of dilla-share'), dev('m2', 'Monitor of Built-in Audio Analog Stereo')] });
      const call = await start(f);
      expect((await call.shareSources()).sources.map((d) => d.monitor)).toEqual([false, true]);
    });

    it('says the labels are missing when the browser hides them', async () => {
      const dev = (deviceId: string) => ({ kind: 'audioinput', deviceId, label: '' }) as MediaDeviceInfo;
      const f = fakes({ devices: [dev('a'), dev('b')] });
      const call = await start(f);
      expect((await call.shareSources()).labeled).toBe(false);
    });
  });

});

// ---------------------------------------------------------------------------------------------
// Signal task 8c: the share picker's options reach the capture and the screen track's encoding, through the dilla transform's publish path.
// ---------------------------------------------------------------------------------------------

describe('task 8c fix round 1: another device\'s published RTT', () => {
  it.each([
    ['24', 24], ['0', 0], ['5000', 5000], ['5001', null], ['024', null], ['-1', null], ['2.5', null], ['', null], ['24 ms', null], ['99999', null],
  ] as const)('dilla.rtt %j reads as %j', (v, want) => {
    expect(rttOf({ 'dilla.rtt': v })).toBe(want);
  });
  it('no attribute, or only others, is no value', () => {
    expect(rttOf(undefined)).toBeNull();
    expect(rttOf({ 'dilla.vdec': 'vp8,vp9', rtt: '20' })).toBeNull();
  });
});

describe('task 8c: screen-share options', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, opts);
    open.push(call);
    return call;
  };
  /** A screen track that records applyConstraints, and a publication whose sender records setParameters. */
  const screenSetup = (f: ReturnType<typeof fakes>) => {
    const applyConstraints = vi.fn<(c: MediaTrackConstraints) => Promise<void>>(async () => undefined);
    const track = Object.assign(videoTrack(), { applyConstraints });
    f.deps.media.getDisplayMedia = vi.fn(async () => streamOf(track));
    const params = { encodings: [{ rid: 'q', scaleResolutionDownBy: 2, maxBitrate: 500_000, maxFramerate: 30 }, { rid: 'f', maxBitrate: 2_500_000, maxFramerate: 30 }] };
    const sender = { getParameters: vi.fn(() => structuredClone(params)), setParameters: vi.fn(async () => undefined) };
    const publish = f.room.localParticipant.publishTrack.getMockImplementation()!;
    f.room.localParticipant.publishTrack.mockImplementation(async (t: MediaStreamTrack, o: { source: string }) => {
      const pub = await publish(t, o);
      if (o.source === 'screen_share') Object.assign(pub.track, { sender });
      return pub;
    });
    return { track, sender, applyConstraints };
  };
  const surfaceOf = (f: ReturnType<typeof fakes>, call: number) =>
    ((vi.mocked(f.deps.media.getDisplayMedia!).mock.calls[call]?.[0] as { video?: { displaySurface?: string } } | undefined)?.video?.displaySurface);
  const OPTS = { surface: 'window', resolution: '720p', fps: 15, systemAudio: false } as const;

  it('a share starts with the picker’s capture options and its encoding, capped by the instance', async () => {
    const f = fakes();
    screenSetup(f);
    const call = await start(f);
    expect(call.state.shareCapBps).toBe(2_500_000);
    await call.setSharing(true, OPTS);
    expect(f.deps.media.getDisplayMedia).toHaveBeenCalledWith(expect.objectContaining({
      video: { displaySurface: 'window', width: { max: 1280 }, height: { max: 720 }, frameRate: { ideal: 15, max: 15 } }, audio: false,
    }));
    const screenPublish = f.room.localParticipant.publishTrack.mock.calls.find(([, o]) => o.source === 'screen_share');
    // 720p at 15 fps is 1.5 Mb/s, under the 2.5 Mb/s cap.
    expect(screenPublish?.[1]).toEqual({ source: 'screen_share', screenShareEncoding: { maxBitrate: 1_500_000, maxFramerate: 15 } });
  });

  it('without options a share uses the defaults (1080p at 30 fps → 4.5 Mb/s, capped to the instance’s 2.5)', async () => {
    const f = fakes();
    screenSetup(f);
    const call = await start(f);
    await call.setSharing(true);
    const screenPublish = f.room.localParticipant.publishTrack.mock.calls.find(([, o]) => o.source === 'screen_share');
    expect(screenPublish?.[1]).toEqual({ source: 'screen_share', screenShareEncoding: { maxBitrate: 2_500_000, maxFramerate: 30 } });
  });

  it('Apply with a new resolution or rate reshapes the running capture and the same sender: no new track, no new publish', async () => {
    const f = fakes();
    const { sender, applyConstraints } = screenSetup(f);
    const call = await start(f);
    await call.setSharing(true, OPTS);
    const publishes = f.room.localParticipant.publishTrack.mock.calls.length;
    await call.setShareOptions({ ...OPTS, resolution: '1080p', fps: 60 });
    expect(applyConstraints).toHaveBeenCalledWith({ width: { max: 1920 }, height: { max: 1080 }, frameRate: { ideal: 60, max: 60 } });
    expect(sender.setParameters).toHaveBeenCalledWith({ encodings: [
      { rid: 'q', scaleResolutionDownBy: 2, maxBitrate: 500_000, maxFramerate: 30 },
      { rid: 'f', maxBitrate: 2_500_000, maxFramerate: 60 },
    ] });
    expect(f.room.localParticipant.publishTrack).toHaveBeenCalledTimes(publishes);
    expect(f.deps.media.getDisplayMedia).toHaveBeenCalledTimes(1);
    expect(call.state.sharing).toBe(true);
  });

  it('Apply with a new source or sound choice asks the browser again and publishes the new capture through publishTrack', async () => {
    const f = fakes();
    const { track } = screenSetup(f);
    const call = await start(f);
    await call.setSharing(true, OPTS);
    await call.setShareOptions({ ...OPTS, surface: 'browser', systemAudio: true });
    expect(f.deps.media.getDisplayMedia).toHaveBeenCalledTimes(2);
    expect(surfaceOf(f, 1)).toBe('browser');
    expect(f.room.localParticipant.unpublishTrack).toHaveBeenCalledWith(track);
    expect(f.room.localParticipant.publishTrack.mock.calls.filter(([, o]) => o.source === 'screen_share')).toHaveLength(2);
    expect(call.state.sharing).toBe(true);
  });

  it('Apply with a new source whose browser picker is cancelled leaves the running share as it was', async () => {
    const f = fakes();
    const { track } = screenSetup(f);
    const call = await start(f);
    await call.setSharing(true, OPTS);
    const first = f.deps.media.getDisplayMedia;
    f.deps.media.getDisplayMedia = vi.fn(async () => { throw Object.assign(new Error('cancelled'), { name: 'NotAllowedError' }); });
    await expect(call.setShareOptions({ ...OPTS, surface: 'browser' })).rejects.toBeDefined();
    expect(f.room.localParticipant.unpublishTrack).not.toHaveBeenCalledWith(track);
    expect(call.state.sharing).toBe(true);
    void first;
  });

  it('Apply while not sharing keeps the options for the next start', async () => {
    const f = fakes();
    screenSetup(f);
    const call = await start(f);
    await call.setShareOptions(OPTS);
    expect(f.deps.media.getDisplayMedia).not.toHaveBeenCalled();
    await call.setSharing(true);
    expect(surfaceOf(f, 0)).toBe('window');
  });

  it('tileStats reads the own microphone and screen from their senders', async () => {
    const f = fakes();
    screenSetup(f);
    const call = await start(f);
    await call.setSharing(true, OPTS);
    const results = f.room.localParticipant.publishTrack.mock.results.map((r) => r.value as Promise<{ source: string; track: Record<string, unknown> }>);
    const pubs = await Promise.all(results);
    const mic = pubs.find((p) => p.source === 'microphone');
    const screen = pubs.find((p) => p.source === 'screen_share');
    const report = (e: Array<Record<string, unknown>>) => new Map(e.map((x) => [x.id, x]));
    Object.assign(mic!.track, { getRTCStatsReport: async () => report([{ id: 'c', type: 'codec', mimeType: 'audio/opus', clockRate: 48000 }, { id: 'o', type: 'outbound-rtp', kind: 'audio', codecId: 'c' }]) });
    Object.assign(screen!.track, { getRTCStatsReport: async () => report([{ id: 'v', type: 'outbound-rtp', kind: 'video', frameHeight: 720, framesPerSecond: 15 }]) });
    expect(await call.tileStats()).toEqual({ ownAudio: { codec: 'opus', clockRate: 48000 }, ownScreen: { height: 720, fps: 15, audio: false }, screens: {} });
  });
});

describe('task 10b: input stage, gate, volumes, push-to-talk release and echo/AGC', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };
  type FakeStage = { inner: unknown; options: InputStageOptions; gains: number[]; gates: SensitivityConfig[]; allowed: boolean[];
    setGain(g: number): void; setGate(c: SensitivityConfig): void; setCaptureAllowed(a: boolean): void; input?: MediaStreamTrack };
  const withStage = (f: ReturnType<typeof fakes>) => {
    const made: FakeStage[] = [];
    f.deps.createInputStage = (inner, options) => {
      const s: FakeStage = { inner, options, gains: [], gates: [], allowed: [],
        setGain(g) { s.gains.push(g); }, setGate(c) { s.gates.push(c); }, setCaptureAllowed(a) { s.allowed.push(a); } };
      made.push(s);
      return s;
    };
    return made;
  };

  it('sets the stage around the noise processor, with the input volume, the automatic gate and capture closed until allowed', async () => {
    const f = fakes();
    const stages = withStage(f);
    const noise = { setPttActive: vi.fn() };
    f.deps.createNoiseProcessor = () => noise;
    const call = await start(f, { noise: 'on', inputVolume: 150, sensitivity: { auto: true, thresholdDb: -50 } });
    expect(stages).toHaveLength(1);
    expect(stages[0].inner).toBe(noise);
    expect(f.localTracks[0].setProcessor).toHaveBeenCalledWith(stages[0]);
    expect(stages[0].options).toMatchObject({ gain: 1.5, gate: { enabled: true, auto: true, thresholdDb: -50 } });
    expect(stages[0].allowed.at(-1)).toBe(true);
    call.setMuted(true);
    expect(stages[0].allowed.at(-1)).toBe(false);
  });

  it('noise off still gets the stage alone, so the volume and the gate apply', async () => {
    const f = fakes();
    const stages = withStage(f);
    await start(f, { noise: 'off' });
    expect(stages).toHaveLength(1);
    expect(stages[0].inner).toBeNull();
    expect(f.localTracks[0].setProcessor).toHaveBeenCalledWith(stages[0]);
  });

  it('input volume, sensitivity and the input mode reach the stage during the call; push to talk turns the gate off', async () => {
    const f = fakes();
    const stages = withStage(f);
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const call = await start(f);
    call.setInputVolume(80);
    expect(stages[0].gains).toEqual([0.8]);
    call.setInputVolume(250);
    call.setInputVolume(12.5);
    expect(stages[0].gains).toEqual([0.8]);
    call.setSensitivity({ auto: false, thresholdDb: -35 });
    expect(stages[0].gates.at(-1)).toEqual({ enabled: true, auto: false, thresholdDb: -35 });
    call.setSensitivity({ auto: false, thresholdDb: -400 });
    expect(stages[0].gates.at(-1)).toEqual({ enabled: true, auto: false, thresholdDb: -100 });
    call.setMode('ptt');
    expect(stages[0].gates.at(-1)).toMatchObject({ enabled: false });
    call.setMode('vad');
    expect(stages[0].gates.at(-1)).toMatchObject({ enabled: true });
  });

  it('the output volume multiplies every remote voice volume', async () => {
    const f = fakes();
    const call = await start(f, { outputVolume: 50, volumes: { [BOB]: 150 } });
    const bob = f.addRemote(BOB_DEV);
    f.subscribeTrack(bob, 'microphone');
    expect(bob.gains.microphone).toBe(0.75);
    call.setOutputVolume(200);
    expect(bob.gains.microphone).toBe(3);
    call.setOutputVolume(201);
    expect(bob.gains.microphone).toBe(3);
  });

  it('push to talk keeps sending for the release delay after the key is let go; a new press inside it keeps it open', async () => {
    const f = fakes();
    const call = await start(f, { mode: 'ptt', pttReleaseMs: 200 });
    const mic = f.localTracks[0];
    call.pushToTalk(true);
    expect(mic.muted).toBe(false);
    call.pushToTalk(false);
    await f.advance(150);
    expect(mic.muted).toBe(false);
    call.pushToTalk(true);
    call.pushToTalk(false);
    await f.advance(199);
    expect(mic.muted).toBe(false);
    await f.advance(2);
    expect(mic.muted).toBe(true);
    call.setPttRelease(0);
    call.pushToTalk(true);
    call.pushToTalk(false);
    expect(mic.muted).toBe(true);
  });

  it('a mute during the release delay closes the microphone at once', async () => {
    const f = fakes();
    const call = await start(f, { mode: 'ptt', pttReleaseMs: 1000 });
    const mic = f.localTracks[0];
    call.pushToTalk(true);
    call.pushToTalk(false);
    call.setMuted(true);
    expect(mic.muted).toBe(true);
  });

  it('the microphone is captured with the echo cancellation and automatic gain asked for', async () => {
    const f = fakes();
    await start(f, { processing: { echo: false, agc: true } });
    expect((f.gum.mock.calls[0] as unknown[])[0]).toMatchObject({ audio: { echoCancellation: false, autoGainControl: true } });
  });

  it('echo/AGC changes capture the microphone again without leaving; while muted with no stage the capture waits', async () => {
    const f = fakes();
    const call = await start(f, { noise: 'off' });
    const mic = f.localTracks[0] as unknown as { restartTrack: ReturnType<typeof vi.fn>; mediaStreamTrack: MediaStreamTrack };
    const raw = f.room.localParticipant.publishTrack.mock.calls[0][0];
    raw.getSettings = () => ({ deviceId: 'chosen', noiseSuppression: false }); raw.getConstraints = () => ({}); mic.mediaStreamTrack = raw;
    const fresh = { enabled: true, stop: vi.fn(), getSettings: () => ({}), getConstraints: () => ({}) } as unknown as MediaStreamTrack;
    mic.restartTrack = vi.fn(async () => { mic.mediaStreamTrack = fresh; });
    call.setMuted(true);
    await call.setProcessing({ echo: false, agc: false });
    expect(mic.restartTrack).not.toHaveBeenCalled();
    call.setMuted(false);
    await flush();
    expect(mic.restartTrack).toHaveBeenCalledWith(expect.objectContaining({ deviceId: { exact: 'chosen' }, echoCancellation: false, autoGainControl: false, noiseSuppression: false }));
    expect(call.state.phase).toBe('connected');
    await call.setProcessing({ echo: false, agc: false });
    expect(mic.restartTrack).toHaveBeenCalledTimes(1);
  });

  it('with a stage the new capture is taken at once even while muted (the stage holds it closed)', async () => {
    const f = fakes();
    const stages = withStage(f);
    const call = await start(f, { noise: 'off' });
    const mic = f.localTracks[0] as unknown as { restartTrack: ReturnType<typeof vi.fn>; mediaStreamTrack: MediaStreamTrack };
    const raw = f.room.localParticipant.publishTrack.mock.calls[0][0];
    raw.getSettings = () => ({ deviceId: 'chosen' }); raw.getConstraints = () => ({}); mic.mediaStreamTrack = raw;
    stages[0].input = raw;
    mic.restartTrack = vi.fn(async () => undefined);
    call.setMuted(true);
    await call.setProcessing({ echo: true, agc: false });
    expect(mic.restartTrack).toHaveBeenCalledWith(expect.objectContaining({ echoCancellation: true, autoGainControl: false }));
    expect(stages[0].allowed.at(-1)).toBe(false);
  });
});

describe('task 10b fix round 1: the stage starts closed, bad levels never reach it, failures release it, its state is reported', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });
  type Fake = { options: InputStageOptions; allowed: boolean[]; gains: number[]; destroyed: number; state: 'active' | 'failed';
    setGain(g: number): void; setGate(): void; setCaptureAllowed(a: boolean): void; destroy(): Promise<void> };
  const stageMaker = (f: ReturnType<typeof fakes>, state: Fake['state'] = 'active') => {
    const made: Fake[] = [];
    f.deps.createInputStage = (_inner, options) => {
      const s: Fake = { options, allowed: [], gains: [], destroyed: 0, state,
        setGain(g) { s.gains.push(g); }, setGate() {}, setCaptureAllowed(a) { s.allowed.push(a); }, destroy() { s.destroyed += 1; return Promise.resolve(); } };
      made.push(s);
      return s;
    };
    return made;
  };
  const start = async (f: ReturnType<typeof fakes>, o: Partial<PageCallOptions> = {}) => {
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, ...o });
    open.push(call);
    return call;
  };

  it('L1: the stage is made closed; applyMic opens it', async () => {
    const f = fakes();
    const made = stageMaker(f);
    await start(f, { noise: 'off' });
    expect(made[0].options.allowed).toBe(false);
    expect(made[0].allowed.at(-1)).toBe(true);
  });

  it('L2: non-finite or out-of-range levels read as the defaults', async () => {
    const f = fakes();
    const made = stageMaker(f);
    const call = await start(f, { noise: 'off', inputVolume: Number.NaN, sensitivity: { auto: false, thresholdDb: Number.POSITIVE_INFINITY }, pttReleaseMs: Number.NaN });
    expect(made[0].options.gain).toBe(1);
    expect(made[0].options.gate).toEqual({ enabled: true, auto: false, thresholdDb: -50 });
    call.setInputVolume(Number.NaN);
    call.setSensitivity({ auto: false, thresholdDb: Number.NaN });
    expect(made[0].gains).toEqual([]);
  });

  it('a stage whose setProcessor fails is destroyed (with the noise processor inside it)', async () => {
    const f = fakes();
    const made = stageMaker(f);
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    f.localTracks.length = 0;
    const real = f.room.localParticipant.publishTrack;
    f.room.localParticipant.publishTrack = vi.fn(async (track: unknown, o: unknown) => {
      const pub = await (real as (t: unknown, o: unknown) => Promise<{ track: { setProcessor: unknown } }>)(track, o);
      pub.track.setProcessor = vi.fn(async () => { throw new Error('E_AUDIO'); });
      return pub;
    }) as typeof real;
    await start(f, { noise: 'on' });
    expect(made.length).toBeGreaterThan(0);
    expect(made.every(s => s.destroyed === 1)).toBe(true);
  });

  it('M1: the stage state reaches Settings (active, failed), and clears when the call ends', async () => {
    const { getStageStatus } = await import('./processor-status.ts');
    const f = fakes();
    stageMaker(f, 'failed');
    const call = await start(f, { noise: 'off' });
    expect(getStageStatus()).toBe('failed');
    await call.leave('user');
    expect(getStageStatus()).toBeNull();
  });
});

describe('task 10b fix round 2: a noise switch never sends the ungated microphone', () => {
  const open: PageCall[] = [];
  afterEach(async () => { for (const c of open.splice(0)) await c.leave('user').catch(() => undefined); });

  it('keeps the capture closed while the new stage is set, and reports no stale stage state', async () => {
    const { getStageStatus } = await import('./processor-status.ts');
    const f = fakes();
    const states: Array<string | null> = [];
    f.deps.createInputStage = () => ({ state: 'active', setGain() {}, setGate() {}, setCaptureAllowed() {}, destroy: () => Promise.resolve() });
    f.deps.createNoiseProcessor = () => ({ setPttActive: vi.fn() });
    const call = await startPageCall(f.deps, { channelId: CH, vdec: 'vp8' }, { ...opts, noise: 'on' });
    open.push(call);
    const mic = f.localTracks[0] as FakeTrack & { stopProcessor?: () => Promise<void> };
    mic.stopProcessor = vi.fn(async () => { states.push(getStageStatus()); });
    const raw = f.room.localParticipant.publishTrack.mock.calls[0][0] as { enabled: boolean };
    (mic as unknown as { mediaStreamTrack: unknown }).mediaStreamTrack = raw;
    expect(raw.enabled).toBe(true);
    let finish!: () => void;
    let during: boolean | null = null;
    mic.setProcessor = vi.fn(() => new Promise<void>((resolve) => { during = raw.enabled; finish = resolve; }));
    const switching = call.setNoise('off');
    await flush();
    expect(during).toBe(false);
    raw.enabled = true; // LiveKit enabling the track meanwhile is refused while the switch holds it
    expect(raw.enabled).toBe(false);
    finish();
    await switching;
    expect(states).toEqual([null]);
    expect(raw.enabled).toBe(true);
    expect(getStageStatus()).toBe('active');
  });
});
