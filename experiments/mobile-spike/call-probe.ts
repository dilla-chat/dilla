// The M0 background call-audio probe (separate page from the DILLA_PROBE capability probe).
// A loopback of two RTCPeerConnections carries one audio track. The sender's encoded transform prepends a 2-byte magic
// and XORs the frame; the receiver XORs it back, checks and strips the magic (RTCRtpScriptTransform on WebKit,
// createEncodedStreams on Chromium, chosen by the repo's own isVoiceSupported()). Every 10 s it logs:
//   DILLA_CALL {"t":seconds,"framesOut":n,"framesIn":n,"framesOk":n,"framesBad":n,"packetsSent":n,"packetsReceived":n}
// plus a DILLA_CALL_DETAIL line (source, mic level and track state, visibility, AudioContext state, inbound-rtp
// audioLevel / concealedSamples / totalSamplesReceived) that is not part of the contract. The page also keeps a
// history of the lines, so they can be read on screen after an unlock. On iOS, "Start call" first starts a CallKit
// outgoing call through the native CallProbe plugin (ios/App/App/CallProbe.swift) and waits for
// provider(_:didActivate:) before starting the loopback.
import { Capacitor, registerPlugin } from '@capacitor/core';
import { isVoiceSupported } from '../../packages/media/src/support';

interface CallProbePlugin {
  startCall(): Promise<{ activated: boolean; category: string; mode: string }>;
  endCall(): Promise<{ ended: boolean }>;
}
const CallProbe = registerPlugin<CallProbePlugin>('CallProbe');

const KEY = 0x5a;
const MAGIC = [0xd1, 0x1a];
const counts = { framesOut: 0, framesIn: 0, framesOk: 0, framesBad: 0 };
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
const historyEl = document.getElementById('history')!;
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
 * `audio` is the AudioContext created synchronously in the click handler (user activation).
 */
async function pickSource(audio: AudioContext): Promise<MediaStreamTrack> {
  try {
    const mic = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: false, autoGainControl: false } });
    await audio.resume().catch((e: unknown) => { detail.resumeError = String(e); });
    streams.push(mic);
    analyser = audio.createAnalyser();
    analyser.fftSize = 2048;
    audio.createMediaStreamSource(mic).connect(analyser);
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
    await audio.resume().catch(() => undefined);
    detail.source = `oscillator (getUserMedia failed: ${String(e)})`;
  }
  const osc = audio.createOscillator();
  const gain = audio.createGain();
  gain.gain.value = 0.05;
  const dest = audio.createMediaStreamDestination();
  osc.frequency.value = 440;
  osc.connect(gain).connect(dest);
  osc.start();
  streams.push(dest.stream);
  return dest.stream.getAudioTracks()[0];
}

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
        if (!detail.firstBadFrame) detail.firstBadFrame = { bytes: src.length, head: Array.from(src.slice(0, 4)), at: Date.now() - started };
      }
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
      s.readable.pipeThrough(openTransform()).pipeTo(s.writable).catch((err: unknown) => { detail.pipeError = `receive: ${String(err)}`; });
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
    s.readable.pipeThrough(sealTransform()).pipeTo(s.writable).catch((err: unknown) => { detail.pipeError = `send: ${String(err)}`; });
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

async function rtpStats(): Promise<{ packetsSent: number | null; packetsReceived: number | null; inbound: Record<string, unknown> | null }> {
  let packetsSent: number | null = null;
  let packetsReceived: number | null = null;
  let inbound: Record<string, unknown> | null = null;
  try {
    const [a, b] = pcs;
    (await a.getStats()).forEach((s: Record<string, unknown>) => {
      if (s.type === 'outbound-rtp' && s.kind === 'audio') packetsSent = s.packetsSent as number;
    });
    (await b.getStats()).forEach((s: Record<string, unknown>) => {
      if (s.type === 'inbound-rtp' && s.kind === 'audio') {
        packetsReceived = s.packetsReceived as number;
        inbound = {
          audioLevel: s.audioLevel,
          concealedSamples: s.concealedSamples,
          totalSamplesReceived: s.totalSamplesReceived,
          packetsLost: s.packetsLost,
        };
      }
    });
  } catch (e) {
    detail.statsError = String(e);
  }
  return { packetsSent, packetsReceived, inbound };
}

async function log(): Promise<void> {
  const t = Math.round((Date.now() - started) / 1000);
  const stats = await rtpStats();
  const line = `DILLA_CALL ${JSON.stringify({ t, ...counts, packetsSent: stats.packetsSent, packetsReceived: stats.packetsReceived })}`;
  const mic = streams[0]?.getAudioTracks()[0];
  Object.assign(detail, {
    t,
    at: new Date().toISOString(),
    visibility: document.visibilityState,
    micLevel: micLevel(),
    micTrack: mic ? { readyState: mic.readyState, muted: mic.muted, enabled: mic.enabled } : null,
    audioContext: ctx?.state ?? null,
    audioPaused: audioEl?.paused ?? null,
    inboundRtp: stats.inbound,
  });
  console.log(line);
  console.log(`DILLA_CALL_DETAIL ${JSON.stringify(detail)}`);
  out.textContent = line;
  historyEl.textContent = `${new Date().toISOString().slice(11, 19)} ${line}\n${historyEl.textContent ?? ''}`;
  detailEl.textContent = JSON.stringify(detail, null, 2);
}

async function start(audio: AudioContext): Promise<void> {
  out.textContent = 'starting…';
  try {
    if (Capacitor.getPlatform() === 'ios') {
      detail.callKit = await CallProbe.startCall();
      nativeCall = true;
    } else {
      detail.callKit = 'n/a (no foreground service on Android in this spike)';
    }
    const track = await pickSource(audio);
    await loopback(track);
    started = Date.now();
    await log();
    timer = setInterval(() => { void log(); }, 10_000);
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
  await log();
  for (const pc of pcs) pc.close();
  for (const s of streams) for (const tr of s.getTracks()) tr.stop();
  worker?.terminate();
  audioEl?.pause();
  await ctx?.close().catch(() => undefined);
  if (nativeCall) detail.callKitEnd = await CallProbe.endCall().catch((e: unknown) => String(e));
  console.log(`DILLA_CALL_END ${JSON.stringify(detail)}`);
  pcs = []; streams = []; worker = undefined; analyser = undefined; ctx = undefined; nativeCall = false;
  for (const k of Object.keys(counts) as (keyof typeof counts)[]) counts[k] = 0;
  startBtn.disabled = false;
}

startBtn.addEventListener('click', () => {
  startBtn.disabled = true;
  // Created synchronously inside the tap, before any await, so it starts with user activation (WebKit autoplay rules).
  ctx = new AudioContext({ sampleRate: 48_000 });
  void start(ctx);
});
endBtn.addEventListener('click', () => { void end(); });
