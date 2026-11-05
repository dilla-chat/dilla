// The M0 WebView capability probe. It renders and console.logs exactly one line:
//   DILLA_PROBE {"opfsSyncHandle":bool,"opfsPersisted":bool,"webLocks":bool,"encodedTransform":...,"audioWorklet":bool,"rnnoise":bool,"storagePersist":bool,"ua":string}
// plus one DILLA_PROBE_DETAIL line with the reasons behind any false.
// Real repo code: packages/media/src/support.ts (encoded-transform kind), packages/media/src/audio/rnnoise.ts (the
// RNNoise worklet + wasm), packages/client-core/src/worker/runtime-readiness.ts (in probe-worker.ts).
import type { AudioProcessorOptions } from 'livekit-client';
import { isVoiceSupported } from '../../packages/media/src/support';
import { DillaRnnoiseProcessor } from '../../packages/media/src/audio/rnnoise';

interface Probe {
  opfsSyncHandle: boolean;
  opfsPersisted: boolean;
  webLocks: boolean;
  encodedTransform: 'script-transform' | 'encoded-streams' | null;
  audioWorklet: boolean;
  rnnoise: boolean;
  storagePersist: boolean;
  ua: string;
}

const detail: Record<string, unknown> = { isSecureContext: globalThis.isSecureContext, origin: location.origin };

function opfs(trigger: string): Promise<{ opfsSyncHandle: boolean; opfsPersisted: boolean }> {
  return new Promise((resolve) => {
    let worker: Worker;
    const timer = setTimeout(() => { detail.worker = 'timeout'; worker?.terminate(); resolve({ opfsSyncHandle: false, opfsPersisted: false }); }, 20_000);
    try {
      worker = new Worker(new URL('./probe-worker.ts', import.meta.url), { type: 'module' });
    } catch (e) {
      clearTimeout(timer);
      detail.worker = String(e);
      resolve({ opfsSyncHandle: false, opfsPersisted: false });
      return;
    }
    worker.onmessage = (e: MessageEvent<Record<string, unknown>>) => {
      clearTimeout(timer);
      Object.assign(detail, { worker: e.data });
      worker.terminate();
      resolve({ opfsSyncHandle: e.data.opfsSyncHandle === true, opfsPersisted: e.data.opfsPersisted === true });
    };
    worker.postMessage({ trigger });
    worker.onerror = (e) => { clearTimeout(timer); detail.worker = `error: ${e.message}`; resolve({ opfsSyncHandle: false, opfsPersisted: false }); };
  });
}

async function webLocks(): Promise<boolean> {
  try {
    return await navigator.locks.request('dilla-probe', async (lock) => lock !== null);
  } catch (e) {
    detail.webLocks = String(e);
    return false;
  }
}

function encodedTransform(): Probe['encodedTransform'] {
  const v = isVoiceSupported();
  detail.voiceSupport = v;
  if (!v.ok) return null;
  return v.path === 'script-transform' ? 'script-transform' : 'encoded-streams';
}

/** A 48 kHz test tone as the input track, so no microphone permission is needed. */
async function rnnoise(): Promise<{ audioWorklet: boolean; rnnoise: boolean }> {
  const audioWorklet = typeof AudioWorkletNode !== 'undefined' && typeof AudioContext !== 'undefined' && 'audioWorklet' in AudioContext.prototype;
  if (!audioWorklet) return { audioWorklet, rnnoise: false };
  const src = new AudioContext({ sampleRate: 48_000 });
  const osc = src.createOscillator();
  const dest = src.createMediaStreamDestination();
  osc.connect(dest);
  osc.start();
  const processor = new DillaRnnoiseProcessor({ onFailure: (r) => { detail.rnnoiseFailure = r; } });
  try {
    await processor.init({ track: dest.stream.getAudioTracks()[0], kind: 'audio' } as unknown as AudioProcessorOptions);
    detail.rnnoiseProbe = await processor.probe(1000);
    return { audioWorklet, rnnoise: true };
  } catch (e) {
    detail.rnnoise = String(e);
    return { audioWorklet, rnnoise: false };
  } finally {
    await processor.destroy().catch(() => undefined);
    osc.stop();
    await src.close().catch(() => undefined);
  }
}

async function storagePersist(): Promise<boolean> {
  try {
    const granted = await navigator.storage.persist();
    detail.persistedAfter = await navigator.storage.persisted();
    return granted;
  } catch (e) {
    detail.storagePersist = String(e);
    return false;
  }
}

async function run(trigger: string): Promise<void> {
  detail.trigger = trigger;
  const out = document.getElementById('out')!;
  out.textContent = 'probing…';
  const [o, locks, audio, persist] = [await opfs(trigger), await webLocks(), await rnnoise(), await storagePersist()];
  const probe: Probe = {
    opfsSyncHandle: o.opfsSyncHandle,
    opfsPersisted: o.opfsPersisted,
    webLocks: locks,
    encodedTransform: encodedTransform(),
    audioWorklet: audio.audioWorklet,
    rnnoise: audio.rnnoise,
    storagePersist: persist,
    ua: navigator.userAgent,
  };
  const line = `DILLA_PROBE ${JSON.stringify(probe)}`;
  console.log(line);
  console.log(`DILLA_PROBE_DETAIL ${JSON.stringify(detail)}`);
  out.textContent = line;
  document.getElementById('detail')!.textContent = JSON.stringify(detail, null, 2);
}

document.getElementById('again')!.addEventListener('click', () => { void run('tap'); });
void run('launch');
