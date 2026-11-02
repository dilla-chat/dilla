import { expect, it } from 'vitest';
import { accumulateAudioTonePeaks, APPLICATION_AUDIO_TONES } from '../harness/application-audio-tones.ts';

const SAMPLE_RATE = 48000, FFT_SIZE = 8192;
function signal(mixHz = 0, burst = false, gaps = true): Float32Array {
  const samples = new Float32Array(FFT_SIZE);
  let sourceFrame = 0;
  for (let frame = 0; frame < samples.length; frame++) {
    // A short cluster of worklet-sized underruns pauses the source while the
    // output clock continues. There is no unrelated application in this input.
    if (gaps && [3200, 3456, 3712].some(start => frame >= start && frame < start + 128)) continue;
    const mixed = mixHz && (!burst || sourceFrame >= 4000 && sourceFrame < 4480);
    samples[frame] = .366 * (Math.sin(2 * Math.PI * 1000 * sourceFrame / SAMPLE_RATE)
      + (mixed ? Math.sin(2 * Math.PI * mixHz * sourceFrame / SAMPLE_RATE) : 0));
    sourceFrame++;
  }
  return samples;
}
function observe(samples: Float32Array) {
  // Web Audio's unsmoothed Blackman-windowed, 1/N-normalized FFT. Compute only
  // the bins the real harness consumes; the browser characterization checks
  // this model against an actual AnalyserNode.
  const data = new Float32Array(FFT_SIZE / 2).fill(-Infinity);
  for (const hz of Object.values(APPLICATION_AUDIO_TONES)) {
    const centre = Math.round(hz * FFT_SIZE / SAMPLE_RATE);
    for (let bin = centre - 2; bin <= centre + 2; bin++) {
      let real = 0, imaginary = 0;
      for (let frame = 0; frame < FFT_SIZE; frame++) {
        const window = .42 - .5 * Math.cos(2 * Math.PI * frame / FFT_SIZE) + .08 * Math.cos(4 * Math.PI * frame / FFT_SIZE);
        const phase = 2 * Math.PI * bin * frame / FFT_SIZE;
        real += samples[frame] * window * Math.cos(phase);
        imaginary -= samples[frame] * window * Math.sin(phase);
      }
      data[bin] = 20 * Math.log10(Math.hypot(real, imaginary) / FFT_SIZE);
    }
  }
  const peaks = { selected: -200, call: -200, other: -200 };
  accumulateAudioTonePeaks(peaks, data, FFT_SIZE, SAMPLE_RATE);
  return peaks;
}

it('does not mistake a selected-only tone interrupted by a short gap cluster for another application', () => {
  const peaks = observe(signal());
  expect(peaks.selected).toBeGreaterThan(-35);
  expect(peaks.call).toBeLessThan(-45);
  expect(peaks.other).toBeLessThan(-45);
});

for (const [name, hz] of [['call', 440], ['other', 1600]] as const) {
  it(`detects a continuous ${name} application mixed before the same gaps`, () => {
    expect(observe(signal(hz))[name]).toBeGreaterThan(-35);
  });
  it(`detects even a 10 ms ${name} application burst mixed before the same gaps`, () => {
    expect(observe(signal(hz, true))[name]).toBeGreaterThan(-45);
  });
  it(`detects an isolated ${name} application without the selected source`, () => {
    const samples = Float32Array.from({ length: FFT_SIZE }, (_, frame) => .366 * Math.sin(2 * Math.PI * hz * frame / SAMPLE_RATE));
    expect(observe(samples)[name]).toBeGreaterThan(-35);
  });
}
