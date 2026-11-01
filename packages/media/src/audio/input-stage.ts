/// <reference types="vite/client" />
import type { AudioProcessorOptions, Track, TrackProcessor } from 'livekit-client';
import { DEFAULT_SENSITIVITY, sensitivityConfig, type SensitivityConfig } from './sensitivity';

/** What the stage needs of the processor before it (the noise processor), if any. */
export interface StageInner {
  processedTrack?: MediaStreamTrack;
  init(opts: AudioProcessorOptions): Promise<void>;
  restart(opts: AudioProcessorOptions): Promise<void>;
  destroy(): Promise<void>;
}

export interface StageLevel { db: number; thresholdDb: number; open: boolean }

/**
 * Where the stage stands: `active` (gain and gate run), `unsupported` (no AudioWorklet: the audio passes unchanged),
 * `failed` (the worklet would not start: unchanged, except that a manual threshold under voice activity fails closed), or
 * `pending` before init.
 */
export type StageState = 'pending' | 'active' | 'unsupported' | 'failed';

export interface InputStageOptions {
  /** The input volume as a gain, 0..2 (0–200 %). */
  gain?: number;
  gate?: SensitivityConfig;
  /** Starts closed until the session allows capture (fail closed). */
  allowed?: boolean;
  /** The level after the gain, every 50 ms, with the threshold in force and whether the gate is open. Without it the
   *  worklet posts nothing. */
  onLevel?(level: StageLevel): void;
  /** Test seam / the microphone test's own context; a private 48 kHz interactive context by default. */
  createContext?: () => AudioContext;
  workletUrl?: string;
}

const defaultWorkletUrl = async (): Promise<string> => (await import('./input-stage-worklet.ts?worker&url')).default;
const GAIN_RAMP_S = 0.03;
/** A gain the GainNode may take: finite, 0..2; anything else reads as 1. */
export const stageGain = (g: number | undefined): number => (typeof g === 'number' && Number.isFinite(g) ? Math.max(0, Math.min(2, g)) : 1);

/** Whether this browser can run the stage (an AudioWorklet in an AudioContext). */
export function inputStageSupported(): boolean {
  return typeof AudioContext !== 'undefined' && typeof AudioWorkletNode !== 'undefined';
}

/**
 * Signal task 10b: the last step of the microphone before the sender, as a livekit-client audio processor that wraps the
 * noise processor (or the raw capture when noise suppression is off): input gain (a GainNode), then the voice-activity
 * gate (input-stage-worklet). Its output is one stable track, so the sender's target never changes across restarts.
 * Fix round 1 (M1): without an AudioWorklet the audio passes through unchanged (`unsupported`); when the worklet fails to
 * start (`failed`) it passes through too, except under a manual threshold with the gate on, where it sends nothing.
 */
export class DillaInputStage implements TrackProcessor<Track.Kind.Audio, AudioProcessorOptions> {
  readonly name = 'dilla-input-stage';
  processedTrack?: MediaStreamTrack;
  /** The capture the stage was given last (the raw microphone track). */
  input?: MediaStreamTrack;
  private stateNow: StageState = 'pending';
  private ctx?: AudioContext;
  private source?: MediaStreamAudioSourceNode;
  private gainNode?: GainNode;
  private node?: AudioWorkletNode;
  private passThrough?: MediaStreamTrack;
  private gain: number;
  private gate: SensitivityConfig;
  private allowed: boolean;
  private destroyed = false;

  constructor(readonly inner: StageInner | null, private readonly opts: InputStageOptions = {}) {
    this.gain = stageGain(opts.gain);
    this.gate = sensitivityConfig(opts.gate ?? DEFAULT_SENSITIVITY);
    this.allowed = opts.allowed === true;
  }

  get state(): StageState { return this.stateNow; }
  /** True while gain and gate run. */
  get active(): boolean { return this.stateNow === 'active'; }
  /** The worklet's output, for a local monitor (Let's check plays it in the same context). */
  get output(): AudioNode | undefined { return this.node; }
  get context(): AudioContext | undefined { return this.ctx; }

  async init(opts: AudioProcessorOptions): Promise<void> {
    this.destroyed = false;
    this.input = opts.track;
    if (this.inner !== null) await this.inner.init(opts);
    const from = this.inner?.processedTrack ?? opts.track;
    if (!inputStageSupported() && this.opts.createContext === undefined) { this.usePassThrough(from, 'unsupported'); return; }
    const ctx = (this.opts.createContext ?? (() => new AudioContext({ sampleRate: 48_000, latencyHint: 'interactive' })))();
    this.ctx = ctx;
    try {
      // Merge of Task 10b: never wait on resume. A browser that holds audio back (no gesture yet) leaves the promise pending,
      // which held the join at "connecting"; the graph is built now and runs once the context does (the call's "turn on
      // audio" press resumes it, PageCall.startAudio).
      void ctx.resume().catch(() => undefined);
      await ctx.audioWorklet.addModule(this.opts.workletUrl ?? await defaultWorkletUrl());
      if (this.destroyed) return;
      const node = new AudioWorkletNode(ctx, 'dilla-input-stage', {
        numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1], channelCount: 1, channelCountMode: 'explicit',
        processorOptions: { gate: this.gate, allowed: this.allowed, report: this.opts.onLevel !== undefined },
      });
      node.port.onmessage = (e: MessageEvent) => {
        const d = e.data as { kind: string } & StageLevel;
        if (d.kind === 'level') this.opts.onLevel?.({ db: d.db, thresholdDb: d.thresholdDb, open: d.open });
      };
      const gainNode = ctx.createGain();
      gainNode.gain.value = this.gain;
      const dest = ctx.createMediaStreamDestination();
      dest.channelCount = 1;
      gainNode.connect(node);
      node.connect(dest);
      this.node = node; this.gainNode = gainNode;
      this.connect(from);
      this.processedTrack = dest.stream.getAudioTracks()[0];
      this.processedTrack.enabled = this.allowed;
      this.stateNow = 'active';
    } catch {
      this.node?.disconnect(); this.node = undefined; this.gainNode = undefined;
      if (this.opts.createContext === undefined) await ctx.close().catch(() => undefined);
      this.ctx = undefined;
      this.usePassThrough(from, 'failed');
    }
  }

  /** M1: the worklet failed and a manual threshold under voice activity is asked for: nothing is sent. */
  private failClosed(): boolean { return this.stateNow === 'failed' && this.gate.enabled && !this.gate.auto; }

  private usePassThrough(from: MediaStreamTrack, state: 'unsupported' | 'failed'): void {
    this.passThrough?.stop();
    this.stateNow = state;
    this.passThrough = from.clone();
    this.processedTrack = this.passThrough;
    this.passThrough.enabled = this.allowed && !this.failClosed();
  }

  private connect(track: MediaStreamTrack): void {
    if (this.ctx === undefined || this.gainNode === undefined) return;
    this.source?.disconnect();
    this.source = this.ctx.createMediaStreamSource(new MediaStream([track]));
    this.source.connect(this.gainNode);
  }

  async restart(opts: AudioProcessorOptions): Promise<void> {
    this.input = opts.track;
    if (this.inner !== null) await this.inner.restart(opts);
    const from = this.inner?.processedTrack ?? opts.track;
    if (this.stateNow === 'active') {
      // A new capture starts from silence, and the room is measured again (another device, other processing).
      this.node?.port.postMessage({ kind: 'restart' });
      this.connect(from);
    } else if (this.passThrough !== undefined && (this.stateNow === 'unsupported' || this.stateNow === 'failed')) {
      // LiveKit puts processedTrack on the sender after a restart; the old clone is replaced (and stopped).
      this.usePassThrough(from, this.stateNow);
    }
  }

  setGain(gain: number): void {
    if (!Number.isFinite(gain)) return;
    this.gain = stageGain(gain);
    const g = this.gainNode;
    if (g === undefined || this.ctx === undefined) return;
    g.gain.cancelScheduledValues(this.ctx.currentTime);
    g.gain.setTargetAtTime(this.gain, this.ctx.currentTime, GAIN_RAMP_S / 3);
  }

  setGate(gate: SensitivityConfig): void {
    this.gate = sensitivityConfig(gate);
    this.node?.port.postMessage({ kind: 'gate', gate: this.gate });
    if (this.passThrough !== undefined) this.passThrough.enabled = this.allowed && !this.failClosed();
  }

  /** False: the stage outputs nothing (and its delay line is emptied), whatever reaches it. */
  setCaptureAllowed(allowed: boolean): void {
    if (this.processedTrack !== undefined) this.processedTrack.enabled = allowed && !this.failClosed();
    if (allowed === this.allowed) return;
    this.allowed = allowed;
    this.node?.port.postMessage({ kind: 'capture', allowed });
  }

  async destroy(): Promise<void> {
    this.destroyed = true;
    this.allowed = false;
    this.node?.port.postMessage({ kind: 'destroy' });
    this.source?.disconnect(); this.gainNode?.disconnect(); this.node?.disconnect();
    this.processedTrack?.stop();
    this.passThrough = undefined; this.processedTrack = undefined; this.source = undefined; this.node = undefined; this.gainNode = undefined;
    this.stateNow = 'pending';
    const ctx = this.ctx;
    this.ctx = undefined;
    await Promise.allSettled([this.inner?.destroy(), ctx !== undefined && this.opts.createContext === undefined && ctx.state !== 'closed' ? ctx.close() : undefined]);
  }
}
