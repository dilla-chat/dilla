import { Track } from 'livekit-client';
import { DillaInputStage, DillaMicrophoneProcessor, processingConstraints, type NoisePreference, type ProcessorStatus, type SensitivityConfig, type StageLevel } from '@dilla/media';

/** Signal task 10b (README §5.9 "Let's check"): what the test applies, as the call would. */
export interface LoopbackSettings {
  input: string;
  /** The output device ('' the system default); used where the context can choose one. */
  output: string;
  noise: NoisePreference;
  processing: { echo: boolean; agc: boolean };
  /** 0..200 %. */
  inputVolume: number;
  outputVolume: number;
  sensitivity: { auto: boolean; thresholdDb: number };
  /** Fix round 1: the call's input mode; under push to talk the gate is off, as in the call. */
  mode: 'vad' | 'ptt';
}

export interface LoopbackEvents {
  level(level: StageLevel): void;
  status(status: ProcessorStatus): void;
  /** The ten seconds ran out (or the microphone ended): the test is over and everything is released. */
  ended(): void;
}

export const LOOPBACK_MS = 10_000;
const SETUP_TIMEOUT_MS = 15_000;
const gateOf = (s: Pick<LoopbackSettings, 'sensitivity' | 'mode'>): SensitivityConfig => ({ enabled: s.mode !== 'ptt', auto: s.sensitivity.auto, thresholdDb: s.sensitivity.thresholdDb });
/** Fix round 1 (L3): the loopback never plays louder than 100 %, whatever the output volume (feedback through speakers). */
export const loopbackGain = (outputVolume: number): number => (Number.isFinite(outputVolume) ? Math.max(0, Math.min(100, outputVolume)) : 100) / 100;

/**
 * Let's check: ten seconds in which you hear yourself as the others would, through the same chain the call sends (the
 * capture with your echo cancellation and automatic gain, the noise processor, the input volume and the voice-activity
 * gate), played on your own output at your output volume. Nothing is recorded or sent: the only path is this context's
 * destination, and everything is released at the end, on stop and on close.
 */
export class LocalMicrophoneTest {
  private closed = false;
  private stream?: MediaStream;
  private processor?: DillaMicrophoneProcessor;
  private stage?: DillaInputStage;
  private ctx?: AudioContext;
  private out?: GainNode;
  private deadline?: ReturnType<typeof setTimeout>;
  constructor(private readonly events: LoopbackEvents) {}

  async start(settings: LoopbackSettings): Promise<void> {
    let watchdog: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([this.setup(settings), new Promise<never>((_, reject) => {
        watchdog = setTimeout(() => { void this.close(); reject(new Error('Microphone test setup timed out')); }, SETUP_TIMEOUT_MS);
      })]);
    } finally { clearTimeout(watchdog); }
  }

  private async setup(s: LoopbackSettings): Promise<void> {
    // Start/resume inside the explicit button press before awaiting microphone permission.
    const ctx = this.ctx = new AudioContext({ sampleRate: 48000, latencyHint: 'interactive' });
    const resumed = ctx.resume();
    void resumed.catch(() => undefined);
    try {
      const stream = this.stream = await navigator.mediaDevices.getUserMedia({ audio: { deviceId: s.input ? { exact: s.input } : undefined,
        ...processingConstraints(s.processing), noiseSuppression: false, channelCount: 1, sampleRate: 48000 } });
      if (this.closed) { await this.release(); return; }
      await resumed;
      if (this.closed) { await this.release(); return; }
      const track = stream.getAudioTracks()[0];
      track.addEventListener?.('ended', () => { void this.finish(); });
      const processor = this.processor = new DillaMicrophoneProcessor({ preference: s.noise, mode: 'open', allowed: true, onStatus: status => this.events.status(status), processing: s.processing });
      const stage = this.stage = new DillaInputStage(processor, { gain: s.inputVolume / 100, gate: gateOf(s), allowed: true,
        onLevel: l => { if (!this.closed) this.events.level(l); }, createContext: () => ctx });
      await stage.init({ track, kind: Track.Kind.Audio, audioContext: ctx });
      if (this.closed) { await this.release(); return; }
      // Only an active stage is heard: without a worklet there is nothing the call would send to compare with.
      if (stage.output === undefined) throw new Error('E_UNAVAILABLE: no input stage');
      const sink = ctx as unknown as { setSinkId?(id: string): Promise<void> };
      if (s.output !== '' && typeof sink.setSinkId === 'function') await sink.setSinkId(s.output).catch(() => undefined);
      if (this.closed) { await this.release(); return; }
      const out = this.out = ctx.createGain();
      out.gain.value = loopbackGain(s.outputVolume);
      stage.output.connect(out);
      out.connect(ctx.destination);
      this.deadline = setTimeout(() => { void this.finish(); }, LOOPBACK_MS);
    } catch (error) { await this.close(); throw error; }
  }

  /** Settings changed during the test: applied at once, as the call would. */
  update(s: Partial<LoopbackSettings>): void {
    if (this.closed) return;
    if (s.inputVolume !== undefined) this.stage?.setGain(s.inputVolume / 100);
    if (s.sensitivity !== undefined && s.mode !== undefined) this.stage?.setGate(gateOf({ sensitivity: s.sensitivity, mode: s.mode }));
    if (s.outputVolume !== undefined && this.out !== undefined && this.ctx !== undefined) this.out.gain.setTargetAtTime(loopbackGain(s.outputVolume), this.ctx.currentTime, 0.01);
  }

  private async finish(): Promise<void> {
    if (this.closed) return;
    await this.close();
    this.events.ended();
  }

  private async release(): Promise<void> {
    clearTimeout(this.deadline);
    const stream = this.stream, stage = this.stage, processor = this.processor, ctx = this.ctx, out = this.out;
    this.stream = undefined; this.stage = undefined; this.processor = undefined; this.ctx = undefined; this.out = undefined;
    for (const track of stream?.getTracks() ?? []) track.stop();
    out?.disconnect();
    await Promise.allSettled([stage !== undefined ? stage.destroy() : processor?.destroy(), ctx !== undefined && ctx.state !== 'closed' ? ctx.close() : undefined]);
  }

  /** Stop, or leaving the section: everything is released; nothing is kept. */
  async close(): Promise<void> {
    this.closed = true;
    await this.release();
  }
}
