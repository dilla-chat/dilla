// Signal task 10b: the input stage's AudioWorklet. It sits after the input gain (a GainNode) and before the sender, so what
// it closes is never sent: a closed gate outputs zeros, which the call encrypts and sends as any other frame (the mute
// path's silence; no new path out of the device). Per 10 ms frame it measures the level, asks the SensitivityGate, and
// ramps its gain toward open or closed; the audio is delayed one frame so the frame that opened the gate is heard whole.
import { SensitivityGate, SENSITIVITY, dbOf, type SensitivityConfig } from './sensitivity';

declare const sampleRate: number;
declare function registerProcessor(name: string, ctor: unknown): void;
declare abstract class AudioWorkletProcessor {
  readonly port: MessagePort;
  constructor(options?: unknown);
}

/** Post the level every 5 frames (50 ms): the meter's refresh. */
const LEVEL_EVERY = 5;

class DillaInputStage extends AudioWorkletProcessor {
  private readonly gate: SensitivityGate;
  private readonly frame = Math.max(1, Math.round(sampleRate * SENSITIVITY.frameMs / 1000));
  private readonly ring: Float32Array;
  private readonly attackStep = 1 / Math.max(1, sampleRate * SENSITIVITY.attackRampMs / 1000);
  private readonly releaseStep = 1 / Math.max(1, sampleRate * SENSITIVITY.releaseRampMs / 1000);
  private at = 0;
  private sum = 0;
  private n = 0;
  private env = 0;
  private target = 0;
  private allowed: boolean;
  private frames = 0;
  private peak: number = SENSITIVITY.minDb;
  private samples = 0;
  private stopped = false;
  /** Fix round 1: level messages only while someone reads them (Settings' Let's check); the call's stage posts none. */
  private readonly report: boolean;

  constructor(options: { processorOptions?: { gate?: SensitivityConfig; allowed?: boolean; report?: boolean } }) {
    super();
    this.gate = new SensitivityGate(options.processorOptions?.gate);
    this.allowed = options.processorOptions?.allowed ?? false;
    this.report = options.processorOptions?.report === true;
    this.ring = new Float32Array(Math.max(1, Math.round(sampleRate * SENSITIVITY.lookaheadMs / 1000)));
    this.port.onmessage = (e: MessageEvent) => this.onMessage(e.data as { kind: string; gate?: SensitivityConfig; allowed?: boolean });
  }

  private onMessage(d: { kind: string; gate?: SensitivityConfig; allowed?: boolean }): void {
    if (d.kind === 'gate' && d.gate) this.gate.configure(d.gate);
    else if (d.kind === 'capture') {
      this.allowed = d.allowed === true;
      // Nothing captured before a mute is sent after it: the delay line and the gate start over.
      this.clear();
      if (!this.allowed && this.report) this.port.postMessage({ kind: 'level', db: SENSITIVITY.minDb, thresholdDb: this.gate.threshold(), open: false });
    } else if (d.kind === 'restart') {
      // A new capture (another device or other processing): silence first, and the room is measured again.
      this.clear();
      this.gate.resetFloor();
    } else if (d.kind === 'destroy') { this.stopped = true; this.clear(); }
  }

  private clear(): void {
    this.ring.fill(0); this.at = 0; this.sum = 0; this.n = 0; this.env = 0; this.target = 0; this.gate.reset();
  }

  process(inputs: Float32Array[][], outputs: Float32Array[][]): boolean {
    const out = outputs[0][0];
    const input = inputs[0]?.[0];
    if (this.stopped) return false;
    if (!this.allowed || input === undefined) { out.fill(0); return true; }
    for (let i = 0; i < out.length; i++) {
      const x = input[i] ?? 0;
      this.sum += x * x;
      this.n += 1;
      this.samples += 1;
      if (this.n >= this.frame) {
        const db = dbOf(Math.sqrt(this.sum / this.n));
        this.target = this.gate.update(db, this.samples * 1000 / sampleRate) ? 1 : 0;
        this.sum = 0; this.n = 0;
        if (db > this.peak) this.peak = db;
        if (++this.frames % LEVEL_EVERY === 0 && this.report) {
          this.port.postMessage({ kind: 'level', db: this.peak, thresholdDb: this.gate.threshold(), open: this.target === 1 });
          this.peak = SENSITIVITY.minDb;
        }
      }
      const delayed = this.ring[this.at];
      this.ring[this.at] = x;
      this.at = (this.at + 1) % this.ring.length;
      if (this.env < this.target) this.env = Math.min(this.target, this.env + this.attackStep);
      else if (this.env > this.target) this.env = Math.max(this.target, this.env - this.releaseStep);
      out[i] = delayed * this.env;
    }
    return true;
  }
}

registerProcessor('dilla-input-stage', DillaInputStage);
