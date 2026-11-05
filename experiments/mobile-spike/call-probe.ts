// The M0 background call-audio probe (separate page from the DILLA_PROBE capability probe).
// A loopback of two RTCPeerConnections carries one audio track. The sender's encoded transform XORs each frame's
// payload and the receiver's reverses it (RTCRtpScriptTransform on WebKit, createEncodedStreams on Chromium, chosen by
// the repo's own isVoiceSupported()). Every 10 s it logs:
//   DILLA_CALL {"t":seconds,"framesOut":n,"framesIn":n}
// plus a DILLA_CALL_DETAIL line (source, mic level, visibility, track and AudioContext state) that is not part of the
// contract. On iOS, "Start call" first starts a CallKit outgoing call through the native CallProbe plugin
// (ios/App/App/CallProbe.swift) and waits for provider(_:didActivate:) before starting the loopback.
import { Capacitor, registerPlugin } from '@capacitor/core';
import { isVoiceSupported } from '../../packages/media/src/support';

interface CallProbePlugin {
  startCall(): Promise<{ activated: boolean; category: string; mode: string }>;
  endCall(): Promise<{ ended: boolean }>;
}
const CallProbe = registerPlugin<CallProbePlugin>('CallProbe');

const KEY = 0x5a;
const counts = { framesOut: 0, framesIn: 0 };
const detail: Record<string, unknown> = {};
let started = 0;
let timer: ReturnType<typeof setInterval> | undefined;
let pcs: RTCPeerConnection[] = [];
let streams: MediaStream[] = [];
let ctx: AudioContext | undefined;
let analyser: AnalyserNode | undefined;
let worker: Worker | undefined;
let audioEl: HTMLAudioElement | undefined;
let nativeCall = false;

const out = document.getElementById('out')!;
const detailEl = document.getElementById('detail')!;
const startBtn = document.getElementById('start') as HTMLButtonElement;
const endBtn = document.getElementById('end') as HTMLButtonElement;

function micLevel(): number | null {
  if (!analyser) return null;
  const buf = new Float32Array(analyser.fftSize);
  analyser.getFloatTimeDomainData(buf);
  let peak = 0;
  for (const v of buf) peak = Math.max(peak, Math.abs(v));
  return Math.round(peak * 1e4) / 1e4;
}

/**
 * The mic track when it carries signal; otherwise (silent emulator mic, or no mic at all) a quiet 440 Hz tone from a
 * MediaStreamDestination. The mic stays open either way, so the capture side behaves like a real call.
 */
async function pickSource(): Promise<MediaStreamTrack> {
  ctx = new AudioContext({ sampleRate: 48_000 });
  try {
    const mic = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: false, autoGainControl: false } });
    streams.push(mic);
    analyser = ctx.createAnalyser();
    analyser.fftSize = 2048;
    ctx.createMediaStreamSource(mic).connect(analyser);
    let peak = 0;
    for (let i = 0; i < 15; i++) {
      await new Promise((r) => setTimeout(r, 100));
      peak = Math.max(peak, micLevel() ?? 0);
    }
    detail.micPeakAtStart = peak;
    if (peak > 0) {
      detail.source = 'mic';
      return mic.getAudioTracks()[0];
    }
    detail.source = 'oscillator (mic silent)';
  } catch (e) {
    detail.source = `oscillator (getUserMedia failed: ${String(e)})`;
  }
  const osc = ctx.createOscillator();
  const gain = ctx.createGain();
  gain.gain.value = 0.05;
  const dest = ctx.createMediaStreamDestination();
  osc.frequency.value = 440;
  osc.connect(gain).connect(dest);
  osc.start();
  streams.push(dest.stream);
  return dest.stream.getAudioTracks()[0];
}

function xorTransform(count: 'framesOut' | 'framesIn'): TransformStream {
  return new TransformStream({
    transform(frame: { data: ArrayBuffer }, ctrl) {
      const bytes = new Uint8Array(frame.data.slice(0));
      for (let i = 0; i < bytes.length; i++) bytes[i] ^= KEY;
      frame.data = bytes.buffer;
      counts[count]++;
      ctrl.enqueue(frame);
    },
  });
}

type EncodedStreams = { readable: ReadableStream; writable: WritableStream };
type WithEncoded = { createEncodedStreams(): EncodedStreams };

async function loopback(track: MediaStreamTrack): Promise<void> {
  const support = isVoiceSupported();
  detail.transformPath = support.ok ? support.path : support.reason;
  if (!support.ok) throw new Error(`no encoded transform: ${support.reason}`);
  const script = support.path === 'script-transform';
  // Chromium needs the PC flag before createEncodedStreams works (as livekit-client sets it).
  const config = (script ? {} : { encodedInsertableStreams: true }) as RTCConfiguration;
  const a = new RTCPeerConnection(config);
  const b = new RTCPeerConnection(config);
  pcs = [a, b];
  a.onicecandidate = (e) => { if (e.candidate) void b.addIceCandidate(e.candidate); };
  b.onicecandidate = (e) => { if (e.candidate) void a.addIceCandidate(e.candidate); };
  a.onconnectionstatechange = () => { detail.pcState = a.connectionState; };

  if (script) {
    worker = new Worker(new URL('./call-transform-worker.ts', import.meta.url), { type: 'module' });
    worker.onmessage = (e: MessageEvent<Record<string, unknown>>) => {
      if ('pipeError' in e.data) detail.pipeError = e.data.pipeError;
      else Object.assign(counts, e.data);
    };
  }
  const Script = (globalThis as unknown as { RTCRtpScriptTransform: new (w: Worker, o: unknown) => unknown }).RTCRtpScriptTransform;

  b.ontrack = (e) => {
    if (script) {
      (e.receiver as unknown as { transform: unknown }).transform = new Script(worker!, { side: 'receive' });
    } else {
      const s = (e.receiver as unknown as WithEncoded).createEncodedStreams();
      s.readable.pipeThrough(xorTransform('framesIn')).pipeTo(s.writable).catch((err: unknown) => { detail.pipeError = `receive: ${String(err)}`; });
    }
    // Play the received audio like a real call would (and so the iPhone run can hear whether it comes back).
    audioEl = new Audio();
    audioEl.srcObject = new MediaStream([e.track]);
    audioEl.play().catch((err: unknown) => { detail.playError = String(err); });
  };

  const sender = a.addTrack(track, new MediaStream([track]));
  if (script) {
    (sender as unknown as { transform: unknown }).transform = new Script(worker!, { side: 'send' });
  } else {
    const s = (sender as unknown as WithEncoded).createEncodedStreams();
    s.readable.pipeThrough(xorTransform('framesOut')).pipeTo(s.writable).catch((err: unknown) => { detail.pipeError = `send: ${String(err)}`; });
  }

  // No DTX: Opus keeps sending 20 ms frames on silence, so the counters measure the pipeline, not the room noise.
  const offer = await a.createOffer();
  offer.sdp = offer.sdp?.replace(/;usedtx=1/g, '');
  await a.setLocalDescription(offer);
  await b.setRemoteDescription(offer);
  const answer = await b.createAnswer();
  answer.sdp = answer.sdp?.replace(/;usedtx=1/g, '');
  await b.setLocalDescription(answer);
  await a.setRemoteDescription(answer);
}

function log(): void {
  const t = Math.round((Date.now() - started) / 1000);
  const line = `DILLA_CALL ${JSON.stringify({ t, framesOut: counts.framesOut, framesIn: counts.framesIn })}`;
  const mic = streams[0]?.getAudioTracks()[0];
  Object.assign(detail, {
    t,
    at: new Date().toISOString(),
    visibility: document.visibilityState,
    micLevel: micLevel(),
    micTrack: mic ? { readyState: mic.readyState, muted: mic.muted, enabled: mic.enabled } : null,
    audioContext: ctx?.state ?? null,
    audioPaused: audioEl?.paused ?? null,
  });
  console.log(line);
  console.log(`DILLA_CALL_DETAIL ${JSON.stringify(detail)}`);
  out.textContent = line;
  detailEl.textContent = JSON.stringify(detail, null, 2);
}

async function start(): Promise<void> {
  startBtn.disabled = true;
  out.textContent = 'starting…';
  try {
    if (Capacitor.getPlatform() === 'ios') {
      detail.callKit = await CallProbe.startCall();
      nativeCall = true;
    } else {
      detail.callKit = 'n/a (no foreground service on Android in this spike)';
    }
    const track = await pickSource();
    await loopback(track);
    started = Date.now();
    log();
    timer = setInterval(log, 10_000);
    endBtn.disabled = false;
  } catch (e) {
    detail.error = String(e);
    console.log(`DILLA_CALL_DETAIL ${JSON.stringify(detail)}`);
    out.textContent = `failed: ${String(e)}`;
    startBtn.disabled = false;
  }
}

async function end(): Promise<void> {
  endBtn.disabled = true;
  if (timer) clearInterval(timer);
  log();
  for (const pc of pcs) pc.close();
  for (const s of streams) for (const tr of s.getTracks()) tr.stop();
  worker?.terminate();
  audioEl?.pause();
  await ctx?.close().catch(() => undefined);
  if (nativeCall) detail.callKitEnd = await CallProbe.endCall().catch((e: unknown) => String(e));
  console.log(`DILLA_CALL_END ${JSON.stringify(detail)}`);
  pcs = []; streams = []; worker = undefined; analyser = undefined; ctx = undefined; nativeCall = false;
  counts.framesOut = 0; counts.framesIn = 0;
  startBtn.disabled = false;
}

startBtn.addEventListener('click', () => { void start(); });
endBtn.addEventListener('click', () => { void end(); });
