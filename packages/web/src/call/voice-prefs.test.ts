import { describe, expect, it } from 'vitest';
import { DEFAULT_VOICE_PREFS, processingSupport, voicePrefs } from './voice-prefs.ts';
import { gainOf, isPercent } from './volume.ts';

describe('voicePrefs (task 10b)', () => {
  it('reads the defaults from an empty slice', () => {
    expect(DEFAULT_VOICE_PREFS).toEqual({ inputVolume: 100, outputVolume: 100, sensitivity: { auto: true, thresholdDb: -50 },
      pttReleaseMs: 200, processing: { echo: true, agc: true } });
  });

  it('reads stored values', () => {
    expect(voicePrefs({ 'voice.inputVolume': '57', 'voice.outputVolume': '200', 'voice.sensitivityAuto': 'off', 'voice.sensitivity': '-35',
      'voice.pttRelease': '0', 'voice.echo': 'off', 'voice.agc': 'off' })).toEqual({ inputVolume: 57, outputVolume: 200,
      sensitivity: { auto: false, thresholdDb: -35 }, pttReleaseMs: 0, processing: { echo: false, agc: false } });
  });

  it('a value the key does not admit reads as its default', () => {
    expect(voicePrefs({ 'voice.inputVolume': '900', 'voice.sensitivity': '5', 'voice.pttRelease': '30', 'voice.echo': 'maybe' }))
      .toEqual(DEFAULT_VOICE_PREFS);
  });
});

describe('processingSupport (Ruling AR pattern)', () => {
  it('follows getSupportedConstraints; no answer hides both', () => {
    expect(processingSupport({ getSupportedConstraints: () => ({ echoCancellation: true }) })).toEqual({ echo: true, agc: false });
    expect(processingSupport({ getSupportedConstraints: () => ({ echoCancellation: true, autoGainControl: true }) })).toEqual({ echo: true, agc: true });
    expect(processingSupport(undefined)).toEqual({ echo: false, agc: false });
    expect(processingSupport({ getSupportedConstraints: () => { throw new Error('x'); } })).toEqual({ echo: false, agc: false });
  });
});

describe('the output volume (task 10b)', () => {
  it('multiplies into the person gain, deafen still wins', () => {
    expect(gainOf('u', { u: 150 }, { deafened: false, paused: false, output: 50 })).toBe(0.75);
    expect(gainOf('u', {}, { deafened: false, paused: false, output: 200 })).toBe(2);
    expect(gainOf('u', {}, { deafened: true, paused: false, output: 200 })).toBe(0);
    expect(gainOf('u', {}, { deafened: false, paused: false, output: 999 })).toBe(1);
  });
  it('isPercent admits whole 0..200', () => {
    expect([0, 1, 200].every(isPercent)).toBe(true);
    expect([-1, 201, 1.5, Number.NaN].some(isPercent)).toBe(false);
  });
});
