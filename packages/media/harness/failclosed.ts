// Task 17 fix round 1, C1: the real DillaE2EEManager and dilla-media worker on a loopback pair of
// RTCPeerConnections in one page, no SFU, so a spec can drive the failure cases a real browser decides: a sender
// whose codec label the SFU chose, a sender whose frames have no prefix rule, a sender with no slot, and a receiver
// whose transform cannot be attached. Room, LocalParticipant and RTCEngine are stand-ins that emit the same events
// livekit-client 2.22.3 emits (LocalSenderCreated before negotiation, MediaTrackAdded inside ontrack,
// TrackSubscribed after it); the PeerConnection, the transforms, the worker and the wasm are real.
import { EventEmitter } from 'events';
import type { Room } from 'livekit-client';
import { createMediaWorker, DillaE2EEManager, type EpochKeys } from '../src/manager';
import type { DillaMediaStats } from '../src/protocol';

const DEV_A = 'a1'.repeat(16);
const DEV_B = 'b2'.repeat(16);
const ROSTER = [{ leaf: 0, deviceId: DEV_A }, { leaf: 1, deviceId: DEV_B }];
const UNPUBLISH_DELAY_MS = 2_000;

export interface FailClosedScenario {
  /**
   * dilla: the manager attaches the sender. plain: no manager, no transform (plaintext; on Chromium its PC has no
   * encodedInsertableStreams flag, so it really sends).
   */
  send: 'dilla' | 'plain';
  /** The label livekit would put on track.codec (the SFU chooses it through enabledPublishCodecs). */
  label?: string;
  /** setCodecPreferences on the sending transceiver, e.g. 'video/AV1'; omitted: the browser's default (VP8). */
  sendCodec?: string;
  source?: string;
  /** dilla: the manager attaches the receiver. none: no transform. */
  recv: 'dilla' | 'none';
  /** Make the receiver's attach fail: 'options' refuses the dilla options only, 'all' refuses every transform. */
  recvFailure?: 'options' | 'all';
  /**
   * N5: fail this side's worker `killAfterMs` into the call by dispatching an `error` event on it (what the browser
   * does when the worker throws), so the manager's own failWorker path runs; the counters are then read again
   * KILL_SETTLE_MS later and at the end, and the result reports what moved in between.
   */
  kill?: 'sender' | 'receiver';
  killAfterMs?: number;
  ms: number;
}

export interface AfterKill {
  bytesSent: number;
  framesDecoded: number;
  rendered: number;
  workerTerminated: boolean;
}

export interface FailClosedResult {
  path: 'script-transform' | 'insertable-streams';
  negotiatedCodec: string;
  transport?: { sender: string; receiver: string; iceSender: string; iceReceiver: string; sourceMuted: boolean };
  bytesSent: number;
  framesDecoded: number;
  rendered: number;
  remoteTrackEnded: boolean;
  senderTrackEnded: boolean;
  unpublished: string[];
  errors: string[];
  sender: DillaMediaStats | null;
  receiver: DillaMediaStats | null;
  /** With `kill`: what was sent, decoded and rendered after the worker failed (from KILL_SETTLE_MS on). */
  afterKill?: AfterKill;
}

const KILL_SETTLE_MS = 500;

class StandIns {
  readonly room: EventEmitter & { localParticipant: EventEmitter; remoteParticipants: Map<string, unknown> };
  readonly engine = new EventEmitter();
  readonly unpublished: string[] = [];

  constructor(identity: string) {
    const lp = Object.assign(new EventEmitter(), {
      identity,
      isE2EEEnabled: true,
      unpublishTrack: async (t: { mediaStreamID: string; mediaStreamTrack: MediaStreamTrack }) => {
        this.unpublished.push(t.mediaStreamID);
        // livekit's unpublishTrack first awaits the pending publish (LocalParticipant.ts:1578-1587); the delay keeps
        // the track live long enough for its frames to meet the blocking transform.
        await new Promise((r) => setTimeout(r, UNPUBLISH_DELAY_MS));
        t.mediaStreamTrack.stop(); // stopLocalTrackOnUnpublish, livekit's default
      },
    });
    this.room = Object.assign(new EventEmitter(), { localParticipant: lp, remoteParticipants: new Map<string, unknown>() });
  }
}

function epoch(selfLeaf: number): EpochKeys {
  return { groupId: 'failclosed', epoch: 7n, baseKey: new Uint8Array(16).fill(0x5c), selfLeaf, minEpoch: 7n, roster: ROSTER };
}

async function manager(identity: string, selfLeaf: number, errors: string[]): Promise<{ m: DillaE2EEManager; s: StandIns; worker: Worker; terminated: () => boolean }> {
  const worker = createMediaWorker();
  let terminated = false;
  const terminate = worker.terminate.bind(worker);
  worker.terminate = (): void => { terminated = true; terminate(); };
  const m = new DillaE2EEManager(worker, { initTimeoutMs: 60_000 }); // unbundled dev-server worker on a slow runner
  m.on('encryptionError', (e: Error) => errors.push(`${identity.slice(0, 4)}: ${e.message}`));
  const s = new StandIns(identity);
  m.setup(s.room as unknown as Room);
  m.setupEngine(s.engine);
  await m.installEpoch(epoch(selfLeaf));
  return { m, s, worker, terminated: () => terminated };
}

async function counters(a: RTCPeerConnection, b: RTCPeerConnection): Promise<{ bytesSent: number; framesDecoded: number; negotiatedCodec: string }> {
  let bytesSent = 0;
  let framesDecoded = 0;
  let negotiatedCodec = '';
  const codecs = new Map<string, string>();
  const sent = await a.getStats();
  sent.forEach((s: { type: string; id: string; mimeType?: string }) => { if (s.type === 'codec' && s.mimeType !== undefined) codecs.set(s.id, s.mimeType); });
  sent.forEach((s: { type: string; kind?: string; bytesSent?: number; codecId?: string }) => {
    if (s.type === 'outbound-rtp' && s.kind === 'video') {
      bytesSent += s.bytesSent ?? 0;
      if (s.codecId !== undefined) negotiatedCodec = codecs.get(s.codecId) ?? negotiatedCodec;
    }
  });
  (await b.getStats()).forEach((s: { type: string; kind?: string; framesDecoded?: number }) => {
    if (s.type === 'inbound-rtp' && s.kind === 'video') framesDecoded += s.framesDecoded ?? 0;
  });
  return { bytesSent, framesDecoded, negotiatedCodec };
}

const statsOrNull = (m: DillaE2EEManager): Promise<DillaMediaStats | null> => m.stats().catch(() => null);

function canvasTrack(): { track: MediaStreamTrack; stop: () => void } {
  const canvas = document.createElement('canvas');
  canvas.width = 320;
  canvas.height = 240;
  document.body.append(canvas);
  const ctx = canvas.getContext('2d');
  let n = 0;
  const timer = setInterval(() => {
    if (ctx === null) return;
    ctx.fillStyle = `hsl(${(n++ * 7) % 360} 70% 50%)`;
    ctx.fillRect(0, 0, 320, 240);
  }, 33);
  return { track: canvas.captureStream(30).getVideoTracks()[0], stop: () => { clearInterval(timer); canvas.remove(); } };
}

/** Swaps RTCRtpScriptTransform / createEncodedStreams for ones that throw while `fn` runs. */
function withFailingReceiveAttach(receiver: RTCRtpReceiver, failure: 'options' | 'all', fn: () => void): void {
  const g = globalThis as { RTCRtpScriptTransform?: new (w: Worker, o: unknown) => object };
  const Original = g.RTCRtpScriptTransform;
  const streams = 'createEncodedStreams' in RTCRtpReceiver.prototype;
  if (streams) {
    Object.defineProperty(receiver, 'createEncodedStreams', { configurable: true, value: () => { throw new DOMException('refused by the spec', 'InvalidStateError'); } });
    fn();
    return;
  }
  if (Original === undefined) throw new Error('no RTCRtpScriptTransform');
  g.RTCRtpScriptTransform = class extends Original {
    constructor(w: Worker, o: unknown) {
      if (failure === 'all' || (o as { block?: unknown }).block !== true) throw new DOMException('refused by the spec', 'DataCloneError');
      super(w, o);
    }
  };
  try {
    fn();
  } finally {
    g.RTCRtpScriptTransform = Original;
  }
}

async function negotiate(a: RTCPeerConnection, b: RTCPeerConnection): Promise<void> {
  a.onicecandidate = (e) => { if (e.candidate) void b.addIceCandidate(e.candidate); };
  b.onicecandidate = (e) => { if (e.candidate) void a.addIceCandidate(e.candidate); };
  await a.setLocalDescription(await a.createOffer());
  await b.setRemoteDescription(a.localDescription as RTCSessionDescriptionInit);
  await b.setLocalDescription(await b.createAnswer());
  await a.setRemoteDescription(b.localDescription as RTCSessionDescriptionInit);
}

export async function runFailClosed(sc: FailClosedScenario): Promise<FailClosedResult> {
  const streamsPath = 'createEncodedStreams' in RTCRtpSender.prototype;
  const errors: string[] = [];
  const A = sc.send === 'dilla' ? await manager(DEV_A, 0, errors) : null;
  const B = sc.recv === 'dilla' ? await manager(DEV_B, 1, errors) : null;
  // livekit forces encodedInsertableStreams whenever a manager exists (RTCEngine.ts:795-804).
  const a = new RTCPeerConnection(streamsPath && A !== null ? ({ encodedInsertableStreams: true } as RTCConfiguration) : {});
  const b = new RTCPeerConnection(streamsPath && B !== null ? ({ encodedInsertableStreams: true } as RTCConfiguration) : {});
  const source = canvasTrack();
  const video = document.createElement('video');
  video.muted = true;
  video.autoplay = true;
  document.getElementById('media')?.append(video);
  let remote: MediaStreamTrack | null = null;

  b.ontrack = (e: RTCTrackEvent) => {
    remote = e.track;
    if (B !== null) {
      const emit = (): void => { B.s.engine.emit('mediaTrackAdded', e.track, e.streams[0], e.receiver); };
      if (sc.recvFailure !== undefined) withFailingReceiveAttach(e.receiver, sc.recvFailure, emit);
      else emit();
      B.s.room.emit('trackSubscribed', { kind: 'video', mediaStreamID: e.track.id }, { source: 'camera', trackInfo: { mimeType: 'video/VP8', encryption: 1 } }, { identity: DEV_A });
    }
    video.srcObject = new MediaStream([e.track]);
  };

  const transceiver = a.addTransceiver(source.track, { direction: 'sendonly', streams: [new MediaStream([source.track])] });
  if (sc.sendCodec !== undefined) {
    const wanted = sc.sendCodec.toLowerCase();
    const codecs = (RTCRtpSender.getCapabilities('video')?.codecs ?? []).filter((c) => c.mimeType.toLowerCase() === wanted);
    if (codecs.length === 0) throw new Error(`${sc.sendCodec} is not offered by this browser`);
    transceiver.setCodecPreferences(codecs);
  }
  if (A !== null) {
    // LocalSenderCreated: after addTransceiver, before negotiate (LocalParticipant.ts:1240-1289).
    A.s.room.localParticipant.emit('localSenderCreated', transceiver.sender, {
      source: sc.source ?? 'camera', kind: 'video', mediaStreamID: source.track.id, codec: sc.label, mediaStreamTrack: source.track,
    });
  }
  await negotiate(a, b);
  // SDP exchange completes before ICE. Give each platform a complete connected
  // measurement interval rather than counting frames while transport is starting.
  const deadline = performance.now() + 10000;
  while (a.connectionState !== 'connected' && a.connectionState !== 'failed' && performance.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, 50));
  }

  let rendered = 0;
  let counting = true;
  const tick = (): void => { if (!counting) return; rendered++; video.requestVideoFrameCallback(tick); };
  video.requestVideoFrameCallback(tick);
  const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
  let killed: { bytesSent: number; framesDecoded: number; rendered: number; terminated: () => boolean } | null = null;
  if (sc.kill !== undefined) {
    const victim = sc.kill === 'sender' ? A : B;
    if (victim === null) throw new Error(`kill ${sc.kill}: that side has no manager`);
    const killAt = sc.killAfterMs ?? 1_500;
    await sleep(killAt);
    victim.worker.dispatchEvent(new Event('error')); // as the browser does when the worker throws
    await sleep(KILL_SETTLE_MS);
    const c = await counters(a, b);
    killed = { bytesSent: c.bytesSent, framesDecoded: c.framesDecoded, rendered, terminated: victim.terminated };
    await sleep(Math.max(0, sc.ms - killAt - KILL_SETTLE_MS));
  } else {
    await sleep(sc.ms);
  }
  counting = false;

  const { bytesSent, framesDecoded, negotiatedCodec } = await counters(a, b);
  const result: FailClosedResult = {
    path: streamsPath ? 'insertable-streams' : 'script-transform',
    transport: { sender: a.connectionState, receiver: b.connectionState, iceSender: a.iceConnectionState, iceReceiver: b.iceConnectionState, sourceMuted: source.track.muted },
    negotiatedCodec,
    bytesSent,
    framesDecoded,
    rendered,
    remoteTrackEnded: (remote as MediaStreamTrack | null)?.readyState === 'ended',
    senderTrackEnded: source.track.readyState === 'ended',
    unpublished: [...(A?.s.unpublished ?? [])],
    errors,
    sender: A === null ? null : await statsOrNull(A.m),
    receiver: B === null ? null : await statsOrNull(B.m),
  };
  if (killed !== null) {
    result.afterKill = {
      bytesSent: bytesSent - killed.bytesSent,
      framesDecoded: framesDecoded - killed.framesDecoded,
      rendered: rendered - killed.rendered,
      workerTerminated: killed.terminated(),
    };
  }
  source.stop();
  source.track.stop();
  a.close();
  b.close();
  video.remove();
  for (const x of [A, B]) {
    if (x === null) continue;
    x.m.dispose();
    x.worker.terminate();
  }
  return result;
}
