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
import { accumulateAudioTonePeaks } from './application-audio-tones.ts';

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

export interface IceDeliveryDiagnostics {
  applied: number[];
  endOfCandidates: number[];
  failures: string[];
  candidates: { side: number; type: string; protocol: string; addressKind: 'mdns' | 'ipv4' | 'ipv6' | 'other' }[];
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
  transport?: { sender: string; receiver: string; iceSender: string; iceReceiver: string; sourceMuted: boolean; gatheringSender?: string; gatheringReceiver?: string; candidates?: number[]; iceErrors?: number[]; delivery?: IceDeliveryDiagnostics };
  bytesSent: number;
  framesDecoded: number;
  rendered: number;
  remoteTrackEnded: boolean;
  senderTrackEnded: boolean;
  unpublished: string[];
  errors: string[];
  sender: DillaMediaStats | null;
  receiver: DillaMediaStats | null;
  /** Only present when a caller supplies a real native video track. */
  nativeVideo?: { input: VideoObservation; receiver: VideoObservation; observationMs: number };
  /** With `kill`: what was sent, decoded and rendered after the worker failed (from KILL_SETTLE_MS on). */
  afterKill?: AfterKill;
}

export interface VideoObservation {
  frames: number; spanMs: number; width: number; height: number;
  patternedFrames: number; distinctSamples: number; maxRedFraction: number; maxCyanFraction: number;
}

/** Observe decoded pixels; never substitutes a source or feeds pixels into capture. */
function observeVideo(video: HTMLVideoElement): { snapshot(): VideoObservation; stop(): void } {
  const canvas = document.createElement('canvas'); canvas.width = 16; canvas.height = 12;
  const context = canvas.getContext('2d', { willReadFrequently: true })!;
  let running = true, first = 0, last = 0;
  const samples = new Set<string>();
  const result: VideoObservation = { frames: 0, spanMs: 0, width: 0, height: 0, patternedFrames: 0, distinctSamples: 0, maxRedFraction: 0, maxCyanFraction: 0 };
  const frame: VideoFrameRequestCallback = now => {
    if (!running) return;
    result.frames++; if (!first) first = now; last = now;
    result.width = video.videoWidth; result.height = video.videoHeight;
    context.drawImage(video, 0, 0, canvas.width, canvas.height);
    const data = context.getImageData(0, 0, canvas.width, canvas.height).data;
    let red = 0, cyan = 0;
    const signature: string[] = [];
    for (let i = 0; i < data.length; i += 4) {
      if (data[i] > 140 && data[i + 1] < 130 && data[i + 2] < 170) red++;
      if (data[i] < 130 && data[i + 1] > 140 && data[i + 2] > 140) cyan++;
      signature.push(`${data[i] >> 5}${data[i + 1] >> 5}${data[i + 2] >> 5}`);
    }
    const redFraction = red / (data.length / 4), cyanFraction = cyan / (data.length / 4);
    result.maxRedFraction = Math.max(result.maxRedFraction, redFraction); result.maxCyanFraction = Math.max(result.maxCyanFraction, cyanFraction);
    if (redFraction > 0.1 && cyanFraction > 0.1) result.patternedFrames++;
    if (samples.size < 64) samples.add(signature.join(''));
    video.requestVideoFrameCallback(frame);
  };
  video.requestVideoFrameCallback(frame);
  return { snapshot: () => ({ ...result, spanMs: last - first, distinctSamples: samples.size }), stop: () => { running = false; } };
}

const KILL_SETTLE_MS = 500;

class StandIns {
  readonly room: EventEmitter & { localParticipant: EventEmitter; remoteParticipants: Map<string, unknown> };
  readonly engine = new EventEmitter();
  readonly unpublished: string[] = [];

  constructor(identity: string, pendingPublish?: Promise<void>) {
    const lp = Object.assign(new EventEmitter(), {
      identity,
      isE2EEEnabled: true,
      unpublishTrack: async (t: { mediaStreamID: string; mediaStreamTrack: MediaStreamTrack }) => {
        this.unpublished.push(t.mediaStreamID);
        // livekit's unpublishTrack first awaits the pending publish (LocalParticipant.ts:1578-1587).
        // Native tracks provide an explicit completion barrier; canvas video retains its fixed delay.
        await (pendingPublish ?? new Promise((r) => setTimeout(r, UNPUBLISH_DELAY_MS)));
        t.mediaStreamTrack.stop(); // stopLocalTrackOnUnpublish, livekit's default
      },
    });
    this.room = Object.assign(new EventEmitter(), { localParticipant: lp, remoteParticipants: new Map<string, unknown>() });
  }
}

function epoch(selfLeaf: number): EpochKeys {
  return { groupId: 'failclosed', epoch: 7n, baseKey: new Uint8Array(16).fill(0x5c), selfLeaf, minEpoch: 7n, roster: ROSTER };
}

async function manager(identity: string, selfLeaf: number, errors: string[], pendingPublish?: Promise<void>): Promise<{ m: DillaE2EEManager; s: StandIns; worker: Worker; terminated: () => boolean }> {
  const worker = createMediaWorker();
  let terminated = false;
  const terminate = worker.terminate.bind(worker);
  worker.terminate = (): void => { terminated = true; terminate(); };
  const m = new DillaE2EEManager(worker, { initTimeoutMs: 60_000 }); // unbundled dev-server worker on a slow runner
  m.on('encryptionError', (e: Error) => errors.push(`${identity.slice(0, 4)}: ${e.message}`));
  const s = new StandIns(identity, pendingPublish);
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

async function negotiate(a: RTCPeerConnection, b: RTCPeerConnection): Promise<IceDeliveryDiagnostics> {
  const delivery: IceDeliveryDiagnostics = { applied: [0, 0], endOfCandidates: [0, 0], failures: [], candidates: [] };
  const ready = [false, false], queues: (RTCIceCandidate | null)[][] = [[], []];
  const chains = [Promise.resolve(), Promise.resolve()];
  const peers = [b, a];
  const forward = (side: number, candidate: RTCIceCandidate | null): void => {
    if (!ready[side]) { queues[side].push(candidate); return; }
    chains[side] = chains[side].then(async () => {
      try { await peers[side].addIceCandidate(candidate); if (candidate) delivery.applied[side]++; else delivery.endOfCandidates[side]++; }
      catch (error) { delivery.failures.push(`${side}:${error instanceof DOMException ? error.name : 'Error'}`); }
    });
  };
  const receive = (side: number, candidate: RTCIceCandidate | null): void => {
    if (candidate) {
      const address = candidate.address ?? '';
      const addressKind = address.endsWith('.local') ? 'mdns' : /^\d+\.\d+\.\d+\.\d+$/.test(address) ? 'ipv4' : address.includes(':') ? 'ipv6' : 'other';
      // Deliberately exclude addresses, ports, SDP, usernames and credentials.
      delivery.candidates.push({ side, type: candidate.type ?? 'unknown', protocol: candidate.protocol ?? 'unknown', addressKind });
    }
    forward(side, candidate);
  };
  a.onicecandidate = event => receive(0, event.candidate);
  b.onicecandidate = event => receive(1, event.candidate);
  const descriptionReady = (side: number): void => { ready[side] = true; for (const candidate of queues[side].splice(0)) forward(side, candidate); };
  await a.setLocalDescription(await a.createOffer());
  await b.setRemoteDescription(a.localDescription as RTCSessionDescriptionInit); descriptionReady(0);
  await b.setLocalDescription(await b.createAnswer());
  await a.setRemoteDescription(b.localDescription as RTCSessionDescriptionInit); descriptionReady(1);
  await Promise.all(chains);
  return delivery;
}

export async function runFailClosed(sc: FailClosedScenario, suppliedVideoTrack?: MediaStreamTrack, iceServers?: RTCIceServer[]): Promise<FailClosedResult> {
  const streamsPath = 'createEncodedStreams' in RTCRtpSender.prototype;
  const errors: string[] = [];
  let completePublish!: () => void;
  const pendingPublish = suppliedVideoTrack ? new Promise<void>(resolve => { completePublish = resolve; }) : undefined;
  const A = sc.send === 'dilla' ? await manager(DEV_A, 0, errors, pendingPublish) : null;
  const B = sc.recv === 'dilla' ? await manager(DEV_B, 1, errors) : null;
  // livekit forces encodedInsertableStreams whenever a manager exists (RTCEngine.ts:795-804).
  const a = new RTCPeerConnection({ ...(iceServers ? { iceServers } : {}), ...(streamsPath && A !== null ? { encodedInsertableStreams: true } : {}) } as RTCConfiguration);
  const b = new RTCPeerConnection({ ...(iceServers ? { iceServers } : {}), ...(streamsPath && B !== null ? { encodedInsertableStreams: true } : {}) } as RTCConfiguration);
  const candidates = [0, 0], iceErrors: number[] = [];
  a.addEventListener('icecandidate', event => { if (event.candidate) candidates[0]++; });
  b.addEventListener('icecandidate', event => { if (event.candidate) candidates[1]++; });
  a.addEventListener('icecandidateerror', event => { iceErrors.push(event.errorCode); });
  b.addEventListener('icecandidateerror', event => { iceErrors.push(event.errorCode); });
  const source = suppliedVideoTrack ? { track: suppliedVideoTrack, stop: () => {} } : canvasTrack();
  const video = document.createElement('video');
  video.muted = true;
  video.autoplay = true;
  document.getElementById('media')?.append(video);
  let remote: MediaStreamTrack | null = null;
  const input = suppliedVideoTrack ? document.createElement('video') : null;
  if (input) {
    input.muted = true; input.autoplay = true; input.srcObject = new MediaStream([source.track]);
    document.getElementById('media')?.append(input); await input.play();
  }
  const inputObservation = input ? observeVideo(input) : null;
  const receiverObservation = suppliedVideoTrack ? observeVideo(video) : null;

  b.ontrack = (e: RTCTrackEvent) => {
    remote = e.track;
    if (B !== null) {
      const emit = (): void => { B.s.engine.emit('mediaTrackAdded', e.track, e.streams[0], e.receiver); };
      if (sc.recvFailure !== undefined) withFailingReceiveAttach(e.receiver, sc.recvFailure, emit);
      else emit();
      B.s.room.emit('trackSubscribed', { kind: 'video', mediaStreamID: e.track.id }, { source: sc.source ?? 'camera', trackInfo: { mimeType: 'video/VP8', encryption: 1 } }, { identity: DEV_A });
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
  const delivery = await negotiate(a, b);
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
  const observationStarted = performance.now();
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
    transport: { sender: a.connectionState, receiver: b.connectionState, iceSender: a.iceConnectionState, iceReceiver: b.iceConnectionState, sourceMuted: source.track.muted, gatheringSender: a.iceGatheringState, gatheringReceiver: b.iceGatheringState, candidates, iceErrors, delivery },
    negotiatedCodec,
    bytesSent,
    framesDecoded,
    rendered,
    remoteTrackEnded: (remote as MediaStreamTrack | null)?.readyState === 'ended',
    senderTrackEnded: source.track.readyState === 'ended',
    unpublished: [...(A?.s.unpublished ?? [])],
    errors: [...errors, ...delivery.failures.map(error => `ICE candidate delivery ${error}`)],
    sender: A === null ? null : await statsOrNull(A.m),
    receiver: B === null ? null : await statsOrNull(B.m),
  };
  if (inputObservation && receiverObservation) {
    result.nativeVideo = { input: inputObservation.snapshot(), receiver: receiverObservation.snapshot(), observationMs: performance.now() - observationStarted };
    inputObservation.stop(); receiverObservation.stop();
  }
  if (killed !== null) {
    result.afterKill = {
      bytesSent: bytesSent - killed.bytesSent,
      framesDecoded: framesDecoded - killed.framesDecoded,
      rendered: rendered - killed.rendered,
      workerTerminated: killed.terminated(),
    };
  }
  completePublish?.();
  source.stop();
  source.track.stop();
  a.close();
  b.close();
  video.remove();
  input?.remove();
  for (const x of [A, B]) {
    if (x === null) continue;
    x.m.dispose();
    x.worker.terminate();
  }
  return result;
}

/** Native capture uses an explicit pending-publish barrier so blocked frames
 * remain available throughout ICE setup and the complete observation interval. */
export async function runProtectedVideo(track: MediaStreamTrack, options: { source?: string; receiver?: boolean; ms?: number; iceServers?: RTCIceServer[] } = {}): Promise<FailClosedResult> {
  try {
    return await runFailClosed({ send: 'dilla', recv: options.receiver === false ? 'none' : 'dilla', source: options.source ?? 'screen_share', label: 'vp8', ms: options.ms ?? 5000 }, track, options.iceServers);
  } finally { track.stop(); }
}

/** Real audio transport/worker/decoder acceptance. The caller supplies a track
 * acquired through the shipping native application-capture capability. */
export async function runProtectedAudio(track: MediaStreamTrack, options: { source?: string; receiver?: boolean; ms?: number } = {}) {
  const errors: string[] = [];
  // Keep livekit's pending-publish stand-in pending until the observation ends.
  // A wall-clock unpublish delay can stop capture before ICE or native audio
  // starts, leaving the blocking transform with no frames to inspect.
  let completePublish!: () => void;
  const pendingPublish = new Promise<void>(resolve => { completePublish = resolve; });
  const A = await manager(DEV_A, 0, errors, pendingPublish);
  const B = options.receiver === false ? null : await manager(DEV_B, 1, errors);
  const a = new RTCPeerConnection({ encodedInsertableStreams: true } as RTCConfiguration);
  const b = new RTCPeerConnection(B ? { encodedInsertableStreams: true } as RTCConfiguration : {});
  const context = new AudioContext({ sampleRate: 48000 });
  const analyser = context.createAnalyser(); analyser.fftSize = 8192; analyser.smoothingTimeConstant = 0;
  const mute = context.createGain(); mute.gain.value = 0; analyser.connect(mute).connect(context.destination);
  const inputAnalyser = context.createAnalyser(); inputAnalyser.fftSize = 8192; inputAnalyser.smoothingTimeConstant = 0;
  context.createMediaStreamSource(new MediaStream([track])).connect(inputAnalyser).connect(mute);
  await context.resume();
  const element = document.createElement('audio'); element.autoplay = true; element.volume = 0; document.body.append(element);
  b.ontrack = event => {
    element.srcObject = new MediaStream([event.track]);
    void element.play().catch(error => errors.push(String(error)));
    B?.s.engine.emit('mediaTrackAdded', event.track, event.streams[0], event.receiver);
    B?.s.room.emit('trackSubscribed', { kind: 'audio', mediaStreamID: event.track.id }, { source: 'screen_share_audio', trackInfo: { mimeType: 'audio/opus', encryption: 1 } }, { identity: DEV_A });
    context.createMediaStreamSource(new MediaStream([event.track])).connect(analyser);
  };
  const transceiver = a.addTransceiver(track, { direction: 'sendonly', streams: [new MediaStream([track])] });
  // Match the shipping E2EE publish defaults: RED/PCMU/PCMA are forbidden.
  transceiver.setCodecPreferences((RTCRtpSender.getCapabilities('audio')?.codecs ?? []).filter(codec => codec.mimeType.toLowerCase() === 'audio/opus'));
  A.s.room.localParticipant.emit('localSenderCreated', transceiver.sender, { source: options.source ?? 'screen_share_audio', kind: 'audio', mediaStreamID: track.id, mediaStreamTrack: track });
  try {
    await negotiate(a, b);
    const connectionDeadline = performance.now() + 10000;
    while (a.connectionState !== 'connected' && a.connectionState !== 'failed' && performance.now() < connectionDeadline) {
      await new Promise(done => setTimeout(done, 50));
    }
    const started = performance.now(), deadline = started + (options.ms ?? 6000);
    const peaks = { selected: -200, call: -200, other: -200 };
    const inputPeaks = { selected: -200, call: -200, other: -200 };
    let firstAudioMs: number | null = null;
    const data = new Float32Array(analyser.frequencyBinCount);
    while (performance.now() < deadline) {
      analyser.getFloatFrequencyData(data);
      accumulateAudioTonePeaks(peaks, data, analyser.fftSize, context.sampleRate);
      inputAnalyser.getFloatFrequencyData(data);
      accumulateAudioTonePeaks(inputPeaks, data, inputAnalyser.fftSize, context.sampleRate);
      if (firstAudioMs === null && peaks.selected > -40) firstAudioMs = performance.now() - started;
      await new Promise(done => setTimeout(done, 50));
    }
    let bytesSent = 0, packetsSent = 0;
    const inbound: unknown[] = [];
    (await b.getStats()).forEach(row => { if (row.type === 'inbound-rtp' && row.kind === 'audio') inbound.push(row); });
    (await a.getStats()).forEach((row: { type: string; kind?: string; bytesSent?: number; packetsSent?: number }) => {
      if (row.type === 'outbound-rtp' && row.kind === 'audio') { bytesSent += row.bytesSent ?? 0; packetsSent += row.packetsSent ?? 0; }
    });
    return { peaks, inputPeaks, firstAudioMs, bytesSent, packetsSent, inbound, trackID: track.id, senderTrackEnded: track.readyState === 'ended', unpublished: [...A.s.unpublished], audioContext: { state: context.state, time: context.currentTime, sampleRate: context.sampleRate }, transport: [a.connectionState, b.connectionState], errors, sender: await statsOrNull(A.m), receiver: B ? await statsOrNull(B.m) : null };
  } finally { completePublish(); track.stop(); a.close(); b.close(); element.remove(); await context.close(); A.worker.terminate(); B?.worker.terminate(); }
}
