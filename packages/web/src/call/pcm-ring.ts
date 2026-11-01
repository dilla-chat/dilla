/** Bounded stereo queue shared with the native-audio worklet; old audio is dropped instead of adding latency. */
export class PCMRing {
  private readonly samples: Float32Array;
  private head = 0;
  queued = 0;
  dropped = 0;
  underruns = 0;
  constructor(capacity = 48000 / 5 * 2) { this.samples = new Float32Array(capacity); }
  push(frame: Float32Array): void {
    if (frame.length !== 960) return;
    const discard = Math.max(0, this.queued + frame.length - this.samples.length);
    this.head = (this.head + discard) % this.samples.length; this.queued -= discard; this.dropped += discard / 2;
    let tail = (this.head + this.queued) % this.samples.length;
    for (const sample of frame) { this.samples[tail] = Number.isFinite(sample) ? Math.max(-1, Math.min(1, sample)) : 0; tail = (tail + 1) % this.samples.length; }
    this.queued += frame.length;
  }
  read(left: Float32Array, right: Float32Array): void {
    for (let i = 0; i < left.length; i++) {
      if (this.queued >= 2) {
        left[i] = this.samples[this.head]; right[i] = this.samples[(this.head + 1) % this.samples.length];
        this.head = (this.head + 2) % this.samples.length; this.queued -= 2;
      } else { left[i] = 0; right[i] = 0; this.underruns++; }
    }
  }
}
