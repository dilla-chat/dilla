// The M0 background call-audio probe (separate page from the DILLA_PROBE capability probe).
// A loopback of two RTCPeerConnections carries one audio track. The sender's encoded transform prepends a 2-byte magic
// and XORs the frame; the receiver XORs it back, checks and strips the magic (RTCRtpScriptTransform on WebKit,
// createEncodedStreams on Chromium, chosen by the repo's own isVoiceSupported()). Every 10 s it logs:
//   DILLA_CALL {"mode":m,"run":n,"t":seconds,"framesOut":n,"framesIn":n,"framesOk":n,"framesBad":n,"packetsSent":n,"packetsReceived":n}
// plus a DILLA_CALL_DETAIL line (source, mic level and track state, visibility, AudioContext state, inbound-rtp
// audioLevel / concealedSamples / totalSamplesReceived) that is not part of the contract. The page also keeps a
// history of the lines, so they can be read on screen after an unlock.
//
// Diagnostics after the iPhone hang of 2026-11-05 (startCall resolved, then nothing): every await in the start path is
// a traced step,
//   DILLA_CALL_STEP {"mode":m,"run":n,"step":name,"phase":"start|ok|err|timeout|late-ok|late-err","ms":n,"at":n,"detail":...}
// with an 8 s timeout, so a hang always names its step (late-ok / late-err: the step settled after its timeout).
// Events (AudioContext statechange, mic track mute/ended, PC state changes, visibility, pagehide, errors, a 5 s
// heartbeat until the first DILLA_CALL line) are DILLA_CALL_EVENT lines. All of them go through console.log, which
// Capacitor's iOS console bridge forwards to native stdout ("⚡️  [log] - ...", visible in devicectl --console).
// Three start orders (buttons on call.html; one per app launch):
//   no-callkit     AudioContext in the tap, getUserMedia, resume, loopback; no CallKit (does the WebView pipeline work?)
//   gum-first      AudioContext in the tap, getUserMedia, resume, then CallKit startCall, then loopback
//   callkit-first  CallKit startCall in the tap; AudioContext created only after didActivate (no user gesture),
//                  getUserMedia, resume, loopback (the order that hung, minus the early AudioContext)
// A fatal step timeout aborts the run and ends the CallKit call (CallProbe.endCall), as do "End call" and pagehide, so
// a hung run never leaves a call behind (CallKit error 7, maximumCallGroupsReached, on the next start).
//
// Force-mic follow-up (2026-11-05: micPeakAtStart 0 in every mode on the iPhone): is the WKWebView mic really silent?
//   no-callkit+session  the native plugin sets .playAndRecord/.voiceChat and activates the session (no CallKit) in
//                       the tap; AudioContext created after that (no gesture), getUserMedia, resume, loopback
//   "force mic" checkbox (all modes): never fall back to the oscillator; the raw getUserMedia track goes through the
//   loopback. The analyser (connected to the destination through a zero gain, so the graph is pulled) is sampled every
//   50 ms for the whole run: DILLA_CALL_DETAIL micPeak0_2 / micPeak2_5 (peak over 0-2 s / 2-5 s after the analyser
//   was attached) and micLevel (peak since the previous line). Sender-independent evidence: outbound media-source
//   audioLevel / totalAudioEnergy (sender A) and inbound-rtp audioLevel / totalAudioEnergy (receiver B decodes what
//   the mic sent), with the energy delta per 10 s line. After getUserMedia: track getSettings / getCapabilities and,
//   on iOS, the native AVAudioSession (CallProbe.audioSessionInfo).
//
// Capture-state round 4 (2026-11-05): after CallKit's didActivate, WebKit mutes the in-page mic (callkit-first: track
// muted from the start; gum-first: mic.mute at didActivate). Hypothesis: WebKit treats a foreground CallKit call as an
// interrupting call. Native KVO on WKWebView.microphoneCaptureState / cameraCaptureState arrives as "captureState"
// events (DILLA_CALL_EVENT capture.change). Two checkboxes (callkit modes):
//   "force capture active"  after didActivate (and after getUserMedia in callkit-first, where no capture existed at
//                           didActivate), CallProbe.setMicCapture("active"); again whenever KVO reports mic=muted while
//                           the call is up, max 5 retries per run (each logged). Also a manual button.
//   "re-gUM after didActivate"  once the loopback is up, a second getUserMedia; the new track replaces the old one on
//                           the sender (replaceTrack) and feeds the analyser; logs whether the new track is muted.
import { Capacitor, registerPlugin, type PluginListenerHandle } from '@capacitor/core';
import { isVoiceSupported } from '../../packages/media/src/support';

interface CallProbePlugin {
  startCall(): Promise<{ activated: boolean; category: string; mode: string; record?: string; route?: string }>;
  endCall(opts: { reason: string }): Promise<{ ended: boolean; results?: string[] }>;
  status(): Promise<Record<string, unknown>>;
  requestMic(): Promise<{ before: string; granted: boolean; now: string }>;
  audioSessionInfo(opts: { label: string }): Promise<Record<string, unknown>>;
  configureSession(): Promise<Record<string, unknown>>;
  captureState(): Promise<{ mic: string; camera: string; app: string }>;
  setMicCapture(opts: { state: 'active' | 'muted' | 'none'; reason: string }): Promise<{ requested: string; before: string; after: string }>;
  addListener(event: 'nativeLog', cb: (e: { line: string }) => void): Promise<PluginListenerHandle>;
  addListener(event: 'captureState', cb: (e: { mic: string; camera: string; changed: string; app: string }) => void): Promise<PluginListenerHandle>;
}
const CallProbe = registerPlugin<CallProbePlugin>('CallProbe');
const isIos = Capacitor.getPlatform() === 'ios';

type Mode = 'no-callkit' | 'gum-first' | 'callkit-first' | 'no-callkit+session';
const STEP_TIMEOUT_MS = 8_000;
const KEY = 0x5a;
const MAGIC = [0xd1, 0x1a];
const counts = { framesOut: 0, framesIn: 0, framesOk: 0, framesBad: 0 };
const detail: Record<string, unknown> = {};
/** The first receive-side frame without the magic, from either path; logged with `at` relative to the call start. */
let firstBadFrame: { bytes: number; head: number[]; atEpoch: number } | null = null;
let mode: Mode | null = null;
let run = 0;
let runStart = 0;
let runActive = false;
let started = 0;
let timer: ReturnType<typeof setInterval> | undefined;
let heartbeat: ReturnType<typeof setInterval> | undefined;
let pcs: RTCPeerConnection[] = [];
let streams: MediaStream[] = [];
let micTrack: MediaStreamTrack | undefined;
let ctx: AudioContext | undefined;
let analyser: AnalyserNode | undefined;
let worker: Worker | undefined;
let workerReady = false;
let audioEl: HTMLAudioElement | undefined;
let forceMic = false;
let forceCapture = false;
let reGum = false;
/** True from CallKit's didActivate (startCall resolved) until the run ends. */
let callUp = false;
const MAX_CAPTURE_RETRIES = 5;
let captureRetries = 0;
/** Every KVO capture change of this run, with `at` (ms since the tap). */
let captureChanges: { at: number; mic: string; camera: string; changed: string; app: string }[] = [];
let sender: RTCRtpSender | undefined;
let micSourceNode: MediaStreamAudioSourceNode | undefined;
/** Continuous analyser sampling (every 50 ms from analyser attach to run end). */
let sampler: ReturnType<typeof setInterval> | undefined;
let samplerStart = 0;
const micPeaks = { p0_2: 0, p2_5: 0, sinceLine: 0, samples: 0 };
/** totalAudioEnergy at the previous DILLA_CALL line, for the per-line delta. */
let prevEnergy: { inbound: number | null; source: number | null } = { inbound: null, source: null };
/** Steps started and not yet settled (name -> start epoch ms), shown by the heartbeat. */
const pending = new Map<string, number>();

const out = document.getElementById('out')!;
const detailEl = document.getElementById('detail')!;
const historyEl = document.getElementById('history')!;
const stepsEl = document.getElementById('steps')!;
const startBtns = Array.from(document.querySelectorAll<HTMLButtonElement>('button[data-mode]'));
const endBtn = document.getElementById('end') as HTMLButtonElement;
const resumeBtn = document.getElementById('resume') as HTMLButtonElement;
const statusBtn = document.getElementById('status') as HTMLButtonElement;
const micBtn = document.getElementById('mic') as HTMLButtonElement;
const forceMicEl = document.getElementById('force-mic') as HTMLInputElement;
const forceCaptureEl = document.getElementById('force-capture') as HTMLInputElement;
const reGumEl = document.getElementById('regum') as HTMLInputElement;
const forceCaptureBtn = document.getElementById('force-capture-btn') as HTMLButtonElement;
const captureBtn = document.getElementById('capture-state') as HTMLButtonElement;

// ---------------------------------------------------------------------------------------------------------------
// Logging

const screenLines: string[] = [];
function screen(line: string): void {
  screenLines.push(`${new Date().toISOString().slice(11, 23)} ${line}`);
  if (screenLines.length > 400) screenLines.shift();
  stepsEl.textContent = screenLines.join('\n');
  stepsEl.scrollTop = stepsEl.scrollHeight;
}

/** console.log (-> Capacitor console bridge -> native stdout) and the on-page step list. */
function emit(prefix: string, obj: Record<string, unknown>): void {
  const line = `${prefix} ${JSON.stringify(obj)}`;
  console.log(line);
  screen(line);
}

function since(): number {
  return runStart ? Date.now() - runStart : 0;
}

/** Audio-side state attached to every step result: the facts the hypotheses turn on. */
function snap(): Record<string, unknown> {
  const ua = (navigator as unknown as { userActivation?: { isActive: boolean; hasBeenActive: boolean } }).userActivation;
  return {
    ctx: ctx ? { state: ctx.state, time: Math.round(ctx.currentTime * 1000) / 1000, rate: ctx.sampleRate } : null,
    mic: micTrack ? { readyState: micTrack.readyState, muted: micTrack.muted, enabled: micTrack.enabled } : null,
    vis: document.visibilityState,
    focus: document.hasFocus(),
    activation: ua ? { active: ua.isActive, been: ua.hasBeenActive } : null,
  };
}

function stepLog(step: string, phase: string, ms: number, extra?: unknown): void {
  emit('DILLA_CALL_STEP', { mode, run, step, phase, ms, at: since(), detail: extra === undefined ? snap() : { ...snap(), info: extra } });
}

function event(name: string, info?: unknown): void {
  emit('DILLA_CALL_EVENT', { mode, run, event: name, at: since(), info: info ?? null, ...snap() });
}

class StepTimeout extends Error {
  constructor(readonly step: string) { super(`step timeout: ${step}`); }
}

/**
 * Runs one traced step. `fn` is called synchronously (before any await), so a step started inside a tap keeps the user
 * activation. Logs start, then ok / err / timeout; if the work settles after its timeout, also late-ok / late-err.
 * Throws the step's error, or a StepTimeout; the caller decides whether that aborts the run.
 */
async function step<T>(name: string, fn: () => Promise<T> | T, opts: { timeoutMs?: number; describe?: (v: T) => unknown } = {}): Promise<T> {
  const t0 = Date.now();
  const timeoutMs = opts.timeoutMs ?? STEP_TIMEOUT_MS;
  stepLog(name, 'start', 0);
  pending.set(name, t0);
  let timedOut = false;
  let work: Promise<T>;
  try {
    work = Promise.resolve(fn());
  } catch (e) {
    work = Promise.reject(e);
  }
  let tid: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, reject) => {
    tid = setTimeout(() => { timedOut = true; reject(new StepTimeout(name)); }, timeoutMs);
  });
  work.then(
    (v) => { if (timedOut) stepLog(name, 'late-ok', Date.now() - t0, opts.describe ? opts.describe(v) : null); },
    (e: unknown) => { if (timedOut) stepLog(name, 'late-err', Date.now() - t0, errText(e)); },
  );
  try {
    const v = await Promise.race([work, timeout]);
    stepLog(name, 'ok', Date.now() - t0, opts.describe ? opts.describe(v) : undefined);
    return v;
  } catch (e) {
    stepLog(name, e instanceof StepTimeout ? 'timeout' : 'err', Date.now() - t0, errText(e));
    throw e;
  } finally {
    clearTimeout(tid);
    pending.delete(name);
  }
}

/** A step whose failure or timeout does not abort the run: returns undefined instead. */
async function softStep<T>(name: string, fn: () => Promise<T> | T, opts: { timeoutMs?: number; describe?: (v: T) => unknown } = {}): Promise<T | undefined> {
  try {
    return await step(name, fn, opts);
  } catch {
    return undefined;
  }
}

function errText(e: unknown): string {
  if (e instanceof Error) return `${e.name}: ${e.message}`;
  return String(e);
}

/** Resolves when `cond` holds (polled every 50 ms); the surrounding step supplies the timeout. */
function waitFor(cond: () => boolean): Promise<void> {
  return new Promise((resolve) => {
    const check = (): void => { if (cond()) resolve(); else setTimeout(check, 50); };
    check();
  });
}

// ---------------------------------------------------------------------------------------------------------------
// Audio source

function micLevel(): number | null {
  if (!analyser) return null;
  const buf = new Float32Array(analyser.fftSize);
  analyser.getFloatTimeDomainData(buf);
  let peak = 0;
  for (const v of buf) peak = Math.max(peak, Math.abs(v));
  return Math.round(peak * 1e4) / 1e4;
}

function startSampler(): void {
  if (sampler) clearInterval(sampler);
  samplerStart = Date.now();
  micPeaks.p0_2 = 0; micPeaks.p2_5 = 0; micPeaks.sinceLine = 0; micPeaks.samples = 0;
  sampler = setInterval(() => {
    const level = micLevel();
    if (level === null) return;
    micPeaks.samples++;
    const t = Date.now() - samplerStart;
    if (t < 2_000) micPeaks.p0_2 = Math.max(micPeaks.p0_2, level);
    else if (t < 5_000) micPeaks.p2_5 = Math.max(micPeaks.p2_5, level);
    micPeaks.sinceLine = Math.max(micPeaks.sinceLine, level);
  }, 50);
}

function createContext(where: string): AudioContext {
  const t0 = Date.now();
  const c = new AudioContext({ sampleRate: 48_000 });
  c.onstatechange = () => { event('ctx.statechange', c.state); };
  ctx = c;
  stepLog(`ctx.create (${where})`, 'ok', Date.now() - t0);
  return c;
}

async function getMic(): Promise<MediaStream | undefined> {
  const mic = await softStep('getUserMedia', () => navigator.mediaDevices.getUserMedia({
    audio: { echoCancellation: true, noiseSuppression: false, autoGainControl: false },
  }), {
    describe: (s) => s.getAudioTracks().map((t) => ({ label: t.label, readyState: t.readyState, muted: t.muted, enabled: t.enabled })),
  });
  if (!mic) return undefined;
  streams.push(mic);
  const track = mic.getAudioTracks()[0];
  micTrack = track;
  if (track) {
    watchTrack(track, 'mic');
    try {
      event('mic.settings', { settings: track.getSettings(), capabilities: track.getCapabilities?.() ?? 'absent', constraints: track.getConstraints() });
    } catch (e) {
      event('mic.settings', errText(e));
    }
  }
  if (isIos) await softStep('native.audioSessionInfo (after gUM)', () => CallProbe.audioSessionInfo({ label: 'after gUM' }), { timeoutMs: 3_000, describe: (r) => r });
  return mic;
}

async function resumeCtx(name: string): Promise<void> {
  if (!ctx) return;
  const c = ctx;
  await softStep(name, () => c.resume(), { describe: () => ({ stateAfter: c.state }) });
}

/**
 * The mic track when the mic shows signal, or when the AudioContext is not running (then its level cannot be read,
 * and WebRTC does not need it); otherwise a quiet 440 Hz tone from a MediaStreamDestination. The mic stays open.
 */
async function pickSource(mic: MediaStream | undefined): Promise<MediaStreamTrack> {
  const audio = ctx!;
  const track = mic?.getAudioTracks()[0];
  if (track) {
    await softStep('analyser.setup', () => {
      analyser = audio.createAnalyser();
      analyser.fftSize = 2048;
      micSourceNode = audio.createMediaStreamSource(mic!);
      micSourceNode.connect(analyser);
      // Pull the graph: an analyser with no path to the destination may not be processed on every engine.
      const mute = audio.createGain();
      mute.gain.value = 0;
      analyser.connect(mute).connect(audio.destination);
    });
    startSampler();
    if (forceMic) {
      detail.source = `mic (forced; AudioContext ${audio.state})`;
      stepLog('source', 'ok', 0, detail.source);
      return track;
    }
    if (audio.state !== 'running') {
      detail.source = `mic (AudioContext ${audio.state}, level not measurable)`;
      stepLog('source', 'ok', 0, detail.source);
      return track;
    }
    const peak = await softStep('mic.sample', async () => {
      let p = 0;
      for (let i = 0; i < 15; i++) {
        await new Promise((r) => setTimeout(r, 100));
        p = Math.max(p, micLevel() ?? 0);
      }
      return p;
    }, { describe: (p) => ({ peak: p }) });
    detail.micPeakAtStart = peak ?? null;
    if ((peak ?? 0) > 0) {
      detail.source = 'mic';
      stepLog('source', 'ok', 0, detail.source);
      return track;
    }
    detail.source = 'oscillator (mic silent)';
  } else {
    detail.source = 'oscillator (no mic: see getUserMedia step)';
    if (forceMic) throw new Error('force mic: getUserMedia gave no audio track');
  }
  const dest = await step('oscillator.setup', () => {
    const osc = audio.createOscillator();
    const gain = audio.createGain();
    gain.gain.value = 0.05;
    const d = audio.createMediaStreamDestination();
    osc.frequency.value = 440;
    osc.connect(gain).connect(d);
    osc.start();
    return d;
  });
  streams.push(dest.stream);
  stepLog('source', 'ok', 0, detail.source);
  return dest.stream.getAudioTracks()[0];
}

// ---------------------------------------------------------------------------------------------------------------
// Loopback

/** Chromium send side: magic + XOR. Same frame format as call-transform-worker.ts. */
function sealTransform(): TransformStream {
  return new TransformStream({
    transform(frame: { data: ArrayBuffer }, ctrl) {
      const src = new Uint8Array(frame.data);
      const sealed = new Uint8Array(src.length + 2);
      sealed[0] = MAGIC[0];
      sealed[1] = MAGIC[1];
      sealed.set(src, 2);
      for (let i = 0; i < sealed.length; i++) sealed[i] ^= KEY;
      frame.data = sealed.buffer;
      counts.framesOut++;
      ctrl.enqueue(frame);
    },
  });
}

/** Chromium receive side: XOR back, check and strip the magic; a frame without it passes through unchanged. */
function openTransform(): TransformStream {
  return new TransformStream({
    transform(frame: { data: ArrayBuffer }, ctrl) {
      counts.framesIn++;
      const src = new Uint8Array(frame.data);
      if (src.length >= 2 && (src[0] ^ KEY) === MAGIC[0] && (src[1] ^ KEY) === MAGIC[1]) {
        const plain = new Uint8Array(src.length - 2);
        for (let i = 0; i < plain.length; i++) plain[i] = src[i + 2] ^ KEY;
        frame.data = plain.buffer;
        counts.framesOk++;
      } else {
        counts.framesBad++;
        firstBadFrame ??= { bytes: src.length, head: Array.from(src.slice(0, 4)), atEpoch: Date.now() };
      }
      ctrl.enqueue(frame);
    },
  });
}

type EncodedStreams = { readable: ReadableStream; writable: WritableStream };
type WithEncoded = { createEncodedStreams(): EncodedStreams };

function watchTrack(track: MediaStreamTrack, label: string): void {
  track.onmute = () => { event(`${label}.mute`); };
  track.onunmute = () => { event(`${label}.unmute`); };
  track.onended = () => { event(`${label}.ended`); };
}

// ---------------------------------------------------------------------------------------------------------------
// Capture state (round 4)

/** CallProbe.setMicCapture("active"); a soft step, so a failure never aborts the run. */
async function forceCaptureActive(reason: string): Promise<void> {
  if (!isIos) { event('capture.force', { reason, skipped: 'not iOS' }); return; }
  await softStep(`native.setMicCapture(active) ${reason}`, () => CallProbe.setMicCapture({ state: 'active', reason }), { timeoutMs: 3_000, describe: (r) => r });
}

function onCaptureState(e: { mic: string; camera: string; changed: string; app: string }): void {
  const at = since();
  captureChanges.push({ at, mic: e.mic, camera: e.camera, changed: e.changed, app: e.app });
  detail.capture = { mic: e.mic, camera: e.camera, changes: captureChanges.length, retries: captureRetries };
  event('capture.change', e);
  if (e.changed !== 'mic' || e.mic !== 'muted' || !callUp || !forceCapture || !runActive) return;
  if (captureRetries >= MAX_CAPTURE_RETRIES) {
    if (captureRetries === MAX_CAPTURE_RETRIES) event('capture.retry.exhausted', { retries: captureRetries });
    captureRetries++;
    return;
  }
  captureRetries++;
  event('capture.retry', { n: captureRetries, of: MAX_CAPTURE_RETRIES });
  void forceCaptureActive(`retry ${captureRetries} (KVO muted)`);
}

/**
 * Re-gUM: a second getUserMedia once the loopback is up; the new track replaces the old one on the sender and feeds
 * the analyser. Logs the new track's muted state at once and 1 s later. The old track is stopped after the swap.
 */
async function reacquireMic(): Promise<void> {
  if (!sender) { event('regum', 'no sender'); return; }
  const old = micTrack;
  const fresh = await softStep('regum.getUserMedia', () => navigator.mediaDevices.getUserMedia({
    audio: { echoCancellation: true, noiseSuppression: false, autoGainControl: false },
  }), {
    describe: (s) => s.getAudioTracks().map((t) => ({ label: t.label, readyState: t.readyState, muted: t.muted, enabled: t.enabled })),
  });
  const track = fresh?.getAudioTracks()[0];
  if (!fresh || !track) { event('regum', 'no track'); return; }
  streams.push(fresh);
  watchTrack(track, 'regum.mic');
  event('regum.track', { muted: track.muted, readyState: track.readyState, oldMuted: old?.muted ?? null, oldReadyState: old?.readyState ?? null });
  const s = sender;
  await softStep('regum.replaceTrack', () => s.replaceTrack(track));
  micTrack = track;
  if (ctx && analyser) {
    const c = ctx;
    const a = analyser;
    await softStep('regum.analyser', () => {
      micSourceNode?.disconnect();
      micSourceNode = c.createMediaStreamSource(fresh);
      micSourceNode.connect(a);
    });
  }
  old?.stop();
  detail.regum = { mutedAtStart: track.muted };
  setTimeout(() => {
    detail.regum = { mutedAtStart: (detail.regum as { mutedAtStart: boolean }).mutedAtStart, mutedAfter1s: track.muted, readyState: track.readyState };
    event('regum.track+1s', { muted: track.muted, readyState: track.readyState });
  }, 1_000);
}

function watchPc(label: string, pc: RTCPeerConnection): void {
  pc.oniceconnectionstatechange = () => { event(`pc${label}.ice`, pc.iceConnectionState); };
  pc.onconnectionstatechange = () => { event(`pc${label}.connection`, pc.connectionState); };
  pc.onsignalingstatechange = () => { event(`pc${label}.signaling`, pc.signalingState); };
  pc.onicegatheringstatechange = () => { event(`pc${label}.gathering`, pc.iceGatheringState); };
}

async function loopback(track: MediaStreamTrack): Promise<void> {
  const support = isVoiceSupported();
  detail.transformPath = support.ok ? support.path : support.reason;
  if (!support.ok) throw new Error(`no encoded transform: ${support.reason}`);
  const script = support.path === 'script-transform';
  // Chromium needs the PC flag before createEncodedStreams works (as livekit-client sets it).
  const config = (script ? {} : { encodedInsertableStreams: true }) as RTCConfiguration;
  const [a, b] = await step('pc.create', () => [new RTCPeerConnection(config), new RTCPeerConnection(config)] as const, {
    describe: () => ({ path: support.path }),
  });
  pcs = [a, b];
  watchPc('A', a);
  watchPc('B', b);
  let candidates = 0;
  a.onicecandidate = (e) => {
    if (!e.candidate) return;
    candidates++;
    b.addIceCandidate(e.candidate).catch((err: unknown) => { event('pcB.addIceCandidate.err', errText(err)); });
  };
  b.onicecandidate = (e) => {
    if (!e.candidate) return;
    candidates++;
    a.addIceCandidate(e.candidate).catch((err: unknown) => { event('pcA.addIceCandidate.err', errText(err)); });
  };
  a.onconnectionstatechange = () => { detail.pcState = a.connectionState; event('pcA.connection', a.connectionState); };

  if (script) {
    worker = new Worker(new URL('./call-transform-worker.ts', import.meta.url), { type: 'module' });
    worker.onerror = (e) => { event('worker.error', e.message); };
    worker.onmessage = (e: MessageEvent<{ pipeError?: string; counts?: typeof counts; firstBadFrame?: typeof firstBadFrame }>) => {
      workerReady = true;
      if (e.data.pipeError) { detail.pipeError = e.data.pipeError; event('worker.pipeError', e.data.pipeError); }
      if (e.data.counts) Object.assign(counts, e.data.counts);
      if (e.data.firstBadFrame) firstBadFrame ??= e.data.firstBadFrame;
    };
    // The worker posts its counts once a second from load, so the first message means it runs.
    await step('worker.ready', () => waitFor(() => workerReady));
  }
  const Script = (globalThis as unknown as { RTCRtpScriptTransform: new (w: Worker, o: unknown) => unknown }).RTCRtpScriptTransform;

  let gotTrack = false;
  b.ontrack = (e) => {
    gotTrack = true;
    try {
      if (script) {
        (e.receiver as unknown as { transform: unknown }).transform = new Script(worker!, { side: 'receive' });
      } else {
        const s = (e.receiver as unknown as WithEncoded).createEncodedStreams();
        s.readable.pipeThrough(openTransform()).pipeTo(s.writable).catch((err: unknown) => { detail.pipeError = `receive: ${String(err)}`; });
      }
      stepLog('transform.attach.receive', 'ok', 0);
    } catch (err) {
      stepLog('transform.attach.receive', 'err', 0, errText(err));
    }
    // Play the received audio like a real call would (and so the iPhone run can hear whether it comes back).
    audioEl = new Audio();
    audioEl.srcObject = new MediaStream([e.track]);
    const el = audioEl;
    void softStep('audio.play', () => el.play(), { describe: () => ({ paused: el.paused }) });
  };

  sender = await step('addTrack', () => a.addTrack(track, new MediaStream([track])));
  await step('transform.attach.send', () => {
    if (script) {
      (sender! as unknown as { transform: unknown }).transform = new Script(worker!, { side: 'send' });
    } else {
      const s = (sender! as unknown as WithEncoded).createEncodedStreams();
      s.readable.pipeThrough(sealTransform()).pipeTo(s.writable).catch((err: unknown) => { detail.pipeError = `send: ${String(err)}`; });
    }
  });

  // No DTX: Opus keeps sending 20 ms frames on silence, so the counters measure the pipeline, not the room noise.
  const offer = await step('createOffer', () => a.createOffer());
  offer.sdp = offer.sdp?.replace(/;usedtx=1/g, '');
  await step('setLocalDescription(A offer)', () => a.setLocalDescription(offer));
  await step('setRemoteDescription(B offer)', () => b.setRemoteDescription(offer));
  const answer = await step('createAnswer', () => b.createAnswer());
  answer.sdp = answer.sdp?.replace(/;usedtx=1/g, '');
  await step('setLocalDescription(B answer)', () => b.setLocalDescription(answer));
  await step('setRemoteDescription(A answer)', () => a.setRemoteDescription(answer));
  await softStep('ontrack', () => waitFor(() => gotTrack));
  await softStep('ice', () => waitFor(() => ['connected', 'completed'].includes(a.iceConnectionState) && ['connected', 'completed'].includes(b.iceConnectionState)), {
    describe: () => ({ a: a.iceConnectionState, b: b.iceConnectionState, candidates }),
  });
}

/** Informational, after the call is up: when the first frames pass each side. Never aborts the run. */
function watchFirstFrames(): void {
  void softStep('firstFrame.out', () => waitFor(() => counts.framesOut > 0), { describe: () => ({ ...counts }) });
  void softStep('firstFrame.in', () => waitFor(() => counts.framesIn > 0), { describe: () => ({ ...counts }) });
  void softStep('firstFrame.ok', () => waitFor(() => counts.framesOk > 0), { describe: () => ({ ...counts }) });
}

function num(v: unknown): number | null {
  return typeof v === 'number' ? v : null;
}

function round(v: number | null, digits = 6): number | null {
  return v === null ? null : Math.round(v * 10 ** digits) / 10 ** digits;
}

async function rtpStats(): Promise<{ packetsSent: number | null; packetsReceived: number | null; inbound: Record<string, unknown> | null; source: Record<string, unknown> | null }> {
  let packetsSent: number | null = null;
  let packetsReceived: number | null = null;
  let inbound: Record<string, unknown> | null = null;
  let source: Record<string, unknown> | null = null;
  try {
    const [a, b] = pcs;
    if (!a || !b) return { packetsSent, packetsReceived, inbound, source };
    (await a.getStats()).forEach((s: Record<string, unknown>) => {
      if (s.type === 'outbound-rtp' && s.kind === 'audio') packetsSent = s.packetsSent as number;
      // The sender's own view of the mic track (before encoding).
      if (s.type === 'media-source' && s.kind === 'audio') {
        const energy = num(s.totalAudioEnergy);
        source = {
          audioLevel: s.audioLevel ?? null,
          totalAudioEnergy: round(energy),
          energyDelta: energy !== null && prevEnergy.source !== null ? round(energy - prevEnergy.source) : null,
          totalSamplesDuration: s.totalSamplesDuration ?? null,
        };
        prevEnergy.source = energy;
      }
    });
    (await b.getStats()).forEach((s: Record<string, unknown>) => {
      if (s.type === 'inbound-rtp' && s.kind === 'audio') {
        packetsReceived = s.packetsReceived as number;
        const energy = num(s.totalAudioEnergy);
        inbound = {
          audioLevel: s.audioLevel ?? null,
          totalAudioEnergy: round(energy),
          // > 0 per 10 s means the decoded RTP audio carries signal; 0 means the mic sent digital silence.
          energyDelta: energy !== null && prevEnergy.inbound !== null ? round(energy - prevEnergy.inbound) : null,
          totalSamplesDuration: s.totalSamplesDuration ?? null,
          concealedSamples: s.concealedSamples,
          totalSamplesReceived: s.totalSamplesReceived,
          packetsLost: s.packetsLost,
        };
        prevEnergy.inbound = energy;
      }
    });
  } catch (e) {
    detail.statsError = String(e);
  }
  return { packetsSent, packetsReceived, inbound, source };
}

async function log(): Promise<void> {
  const t = Math.round((Date.now() - started) / 1000);
  const stats = await rtpStats();
  const line = `DILLA_CALL ${JSON.stringify({ mode, run, t, ...counts, packetsSent: stats.packetsSent, packetsReceived: stats.packetsReceived })}`;
  Object.assign(detail, {
    mode,
    run,
    t,
    at: new Date().toISOString(),
    visibility: document.visibilityState,
    forceMic,
    forceCapture,
    reGum,
    captureRetries,
    micLevel: analyser ? micPeaks.sinceLine : null,
    micLevelNow: micLevel(),
    micPeak0_2: analyser ? micPeaks.p0_2 : null,
    micPeak2_5: analyser ? micPeaks.p2_5 : null,
    micSamples: micPeaks.samples,
    micTrack: micTrack ? { readyState: micTrack.readyState, muted: micTrack.muted, enabled: micTrack.enabled } : null,
    audioContext: ctx?.state ?? null,
    audioPaused: audioEl?.paused ?? null,
    inboundRtp: stats.inbound,
    mediaSource: stats.source,
    // A 0-byte first bad frame is Chromium's empty-payload bypass packet (harmless); a non-empty one is a real failure.
    firstBadFrame: firstBadFrame && { bytes: firstBadFrame.bytes, head: firstBadFrame.head, at: firstBadFrame.atEpoch - started },
  });
  micPeaks.sinceLine = 0;
  console.log(line);
  console.log(`DILLA_CALL_DETAIL ${JSON.stringify(detail)}`);
  out.textContent = line;
  historyEl.textContent = `${new Date().toISOString().slice(11, 19)} ${line}\n${historyEl.textContent ?? ''}`;
  detailEl.textContent = JSON.stringify(detail, null, 2);
}

// ---------------------------------------------------------------------------------------------------------------
// Runs

async function startCallKit(): Promise<void> {
  if (!isIos) {
    stepLog('callkit.startCall', 'ok', 0, 'skipped (not iOS)');
    detail.callKit = 'n/a (no foreground service on Android in this spike)';
    return;
  }
  detail.callKit = await step('callkit.startCall', () => CallProbe.startCall(), { describe: (r) => r });
  callUp = true;
  if (forceCapture) await forceCaptureActive('after didActivate');
}

/** Optional facts that must not delay the gesture-bound steps: run after them. */
async function environment(): Promise<void> {
  const perms = (navigator as unknown as { permissions?: { query(d: { name: string }): Promise<{ state: string }> } }).permissions;
  if (perms) {
    await softStep('permissions.microphone', () => perms.query({ name: 'microphone' }), { timeoutMs: 3_000, describe: (r) => r.state });
  } else {
    stepLog('permissions.microphone', 'ok', 0, 'navigator.permissions absent');
  }
  await softStep('enumerateDevices', () => navigator.mediaDevices.enumerateDevices(), {
    timeoutMs: 3_000,
    describe: (d) => d.map((x) => `${x.kind}:${x.label || '(no label)'}`),
  });
  if (isIos) await softStep('native.status', () => CallProbe.status(), { timeoutMs: 3_000, describe: (s) => ({ ...s, lines: (s.lines as unknown[] | undefined)?.length }) });
}

/**
 * Called synchronously from the tap. Each mode's first step is invoked before any await, so it runs with the user
 * activation; `ctx` is created synchronously in the tap except in callkit-first.
 */
async function startRun(m: Mode): Promise<void> {
  mode = m;
  forceMic = forceMicEl.checked;
  forceCapture = forceCaptureEl.checked;
  reGum = reGumEl.checked;
  captureRetries = 0;
  captureChanges = [];
  run++;
  runStart = Date.now();
  runActive = true;
  for (const btn of startBtns) btn.disabled = true;
  out.textContent = `starting (${m})…`;
  event('run.start', { ua: navigator.userAgent, platform: Capacitor.getPlatform(), forceMic, forceCapture, reGum });
  if (isIos) void softStep('native.captureState (run start)', () => CallProbe.captureState(), { timeoutMs: 3_000, describe: (r) => r });
  heartbeat = setInterval(() => {
    if (started) return;
    event('heartbeat', { pending: Array.from(pending.entries()).map(([k, t0]) => `${k} ${Date.now() - t0}ms`) });
  }, 5_000);
  try {
    let mic: MediaStream | undefined;
    if (m === 'callkit-first') {
      await startCallKit();
      createContext('after didActivate, no gesture');
      mic = await getMic();
      // No capture existed at didActivate, so force once more now that the track exists.
      if (forceCapture) await forceCaptureActive('after gUM');
      await resumeCtx('ctx.resume (no gesture)');
    } else if (m === 'no-callkit+session') {
      // The native call is made synchronously in the tap; the AudioContext follows the app-configured session.
      if (isIos) {
        await step('native.configureSession', () => CallProbe.configureSession(), { describe: (r) => r });
      } else {
        stepLog('native.configureSession', 'ok', 0, 'skipped (not iOS)');
      }
      createContext('after configureSession, no gesture');
      mic = await getMic();
      await resumeCtx('ctx.resume (no gesture)');
    } else {
      createContext('in tap');
      mic = await getMic();
      await resumeCtx('ctx.resume');
      if (m === 'gum-first') await startCallKit();
    }
    await environment();
    const track = await pickSource(mic);
    await loopback(track);
    started = Date.now();
    watchFirstFrames();
    await log();
    timer = setInterval(() => { void log(); }, 10_000);
    // Off iOS there is no CallKit: run it anyway, so a desktop smoke run exercises the path.
    if (reGum && (callUp || !isIos)) await reacquireMic();
    if (isIos) void softStep('native.audioSessionInfo (call up)', () => CallProbe.audioSessionInfo({ label: 'call up' }), { timeoutMs: 3_000, describe: (r) => r });
  } catch (e) {
    detail.error = errText(e);
    console.log(`DILLA_CALL_DETAIL ${JSON.stringify({ ...detail, mode, run })}`);
    out.textContent = `failed (${m}): ${errText(e)}`;
    await endRun(e instanceof StepTimeout ? `timeout:${e.step}` : 'error');
  }
}

/** Ends the run whatever state it is in; idempotent. Always asks CallKit to end any active call on iOS. */
async function endRun(reason: string): Promise<void> {
  event('run.end', { reason, captureChanges, captureRetries });
  callUp = false;
  if (timer) clearInterval(timer);
  if (heartbeat) clearInterval(heartbeat);
  if (sampler) clearInterval(sampler);
  sampler = undefined;
  timer = undefined;
  heartbeat = undefined;
  if (started) await log();
  for (const pc of pcs) pc.close();
  for (const s of streams) for (const tr of s.getTracks()) tr.stop();
  worker?.terminate();
  audioEl?.pause();
  if (ctx) await softStep('ctx.close', () => ctx!.close(), { timeoutMs: 3_000 });
  if (isIos) detail.callKitEnd = await softStep('callkit.endCall', () => CallProbe.endCall({ reason }), { describe: (r) => r });
  console.log(`DILLA_CALL_END ${JSON.stringify({ ...detail, mode, run, reason })}`);
  prevEnergy = { inbound: null, source: null };
  pcs = []; streams = []; worker = undefined; workerReady = false; firstBadFrame = null; delete detail.firstBadFrame;
  analyser = undefined; micSourceNode = undefined; sender = undefined; ctx = undefined; micTrack = undefined; audioEl = undefined; started = 0;
  for (const k of Object.keys(counts) as (keyof typeof counts)[]) counts[k] = 0;
  runActive = false;
  for (const btn of startBtns) btn.disabled = false;
}

// ---------------------------------------------------------------------------------------------------------------
// Wiring

for (const btn of startBtns) {
  btn.addEventListener('click', () => {
    if (runActive) return;
    void startRun(btn.dataset.mode as Mode);
  });
}
endBtn.addEventListener('click', () => { void endRun('button'); });
// A tap gives the context a user activation: does that unstick a resume that hung without one?
resumeBtn.addEventListener('click', () => {
  if (!ctx) { event('ctx.resume (tap)', 'no AudioContext'); return; }
  const c = ctx;
  void softStep('ctx.resume (tap)', () => c.resume(), { describe: () => ({ stateAfter: c.state }) });
});
statusBtn.addEventListener('click', () => {
  void environment();
  if (isIos) void softStep('native.captureState (status)', () => CallProbe.captureState(), { timeoutMs: 3_000, describe: (r) => r });
  if (isIos) void softStep('native.audioSessionInfo (status)', () => CallProbe.audioSessionInfo({ label: 'status' }), { timeoutMs: 3_000, describe: (r) => r });
  event('status', { pending: Array.from(pending.keys()), counts: { ...counts } });
});
forceCaptureBtn.addEventListener('click', () => { void forceCaptureActive('manual'); });
captureBtn.addEventListener('click', () => {
  if (!isIos) { event('native.captureState', 'not iOS'); return; }
  void softStep('native.captureState', () => CallProbe.captureState(), { timeoutMs: 3_000, describe: (r) => r });
});
micBtn.addEventListener('click', () => {
  if (!isIos) { event('native.requestMic', 'not iOS'); return; }
  void softStep('native.requestMic', () => CallProbe.requestMic(), { timeoutMs: 30_000, describe: (r) => r });
});

document.addEventListener('visibilitychange', () => { event('visibility', document.visibilityState); });
window.addEventListener('pageshow', (e) => { event('pageshow', { persisted: e.persisted }); });
window.addEventListener('pagehide', (e) => {
  event('pagehide', { persisted: e.persisted });
  // Fire and forget: the page may be gone before it settles. A leftover call blocks the next start (CallKit error 7).
  if (isIos) void CallProbe.endCall({ reason: 'pagehide' }).catch(() => undefined);
});
window.addEventListener('error', (e) => { event('window.error', `${e.message} @ ${e.filename}:${e.lineno}:${e.colno}`); });
window.addEventListener('unhandledrejection', (e) => { event('unhandledrejection', errText(e.reason)); });

if (isIos) {
  // Native lines are already on native stdout (NSLog); here they only go to the on-page list, not console.log again.
  void CallProbe.addListener('nativeLog', (e) => { screen(e.line); });
  void CallProbe.addListener('captureState', onCaptureState);
}
event('page.load', { platform: Capacitor.getPlatform(), voice: isVoiceSupported() });
