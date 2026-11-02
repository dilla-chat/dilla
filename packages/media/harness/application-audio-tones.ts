/** Space the independent fixtures apart: interruptions in the selected 1000 Hz
 * PCM produce sidelobes near 880 Hz that can exceed the exclusion gate without
 * any second application. The FFT, peak-over-all-windows and -45 dB gate stay
 * unchanged; real 1600 Hz and 440 Hz mixtures must still fail that gate. */
export const APPLICATION_AUDIO_TONES = { selected: 1000, call: 440, other: 1600 } as const;
export type AudioTonePeaks = Record<keyof typeof APPLICATION_AUDIO_TONES, number>;

export function accumulateAudioTonePeaks(peaks: AudioTonePeaks, data: Float32Array, fftSize: number, sampleRate: number): void {
  for (const name of ['selected', 'call', 'other'] as const) {
    const bin = Math.round(APPLICATION_AUDIO_TONES[name] * fftSize / sampleRate);
    peaks[name] = Math.max(peaks[name], ...data.subarray(bin - 2, bin + 3));
  }
}
