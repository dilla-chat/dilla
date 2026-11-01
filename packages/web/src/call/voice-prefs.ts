// Signal task 10b (README §5.9 Voice): the voice settings the call and Let's check apply, read from the settings slice with
// client-core's validation; a value the key does not admit reads as its default.
import { SETTINGS, VOICE_DEFAULTS, isSettingValue } from '@dilla/client-core';

export interface VoicePrefs {
  /** 0..200 %. */
  inputVolume: number;
  outputVolume: number;
  sensitivity: { auto: boolean; thresholdDb: number };
  /** Push to talk keeps sending this long after the key is let go. */
  pttReleaseMs: number;
  processing: { echo: boolean; agc: boolean };
}

const read = (settings: Record<string, string>, key: string, fallback: string): string => {
  const v = settings[key];
  return v !== undefined && isSettingValue(key, v) ? v : fallback;
};

export function voicePrefs(settings: Record<string, string>): VoicePrefs {
  return {
    inputVolume: Number(read(settings, SETTINGS.voiceInputVolume, VOICE_DEFAULTS.inputVolume)),
    outputVolume: Number(read(settings, SETTINGS.voiceOutputVolume, VOICE_DEFAULTS.outputVolume)),
    sensitivity: {
      auto: read(settings, SETTINGS.voiceSensitivityAuto, VOICE_DEFAULTS.sensitivityAuto) === 'on',
      thresholdDb: Number(read(settings, SETTINGS.voiceSensitivity, VOICE_DEFAULTS.sensitivity)),
    },
    pttReleaseMs: Number(read(settings, SETTINGS.voicePttRelease, VOICE_DEFAULTS.pttRelease)),
    processing: {
      echo: read(settings, SETTINGS.voiceEcho, VOICE_DEFAULTS.echo) === 'on',
      agc: read(settings, SETTINGS.voiceAgc, VOICE_DEFAULTS.agc) === 'on',
    },
  };
}

export const DEFAULT_VOICE_PREFS: VoicePrefs = voicePrefs({});

/**
 * Ruling AR pattern: a processing switch the browser cannot apply is not shown. getSupportedConstraints lists what the
 * engine honours (Safari has no autoGainControl).
 */
export function processingSupport(media: { getSupportedConstraints?(): MediaTrackSupportedConstraints } | undefined =
  typeof navigator === 'undefined' ? undefined : navigator.mediaDevices): { echo: boolean; agc: boolean } {
  let s: MediaTrackSupportedConstraints = {};
  try { s = media?.getSupportedConstraints?.() ?? {}; } catch { s = {}; }
  return { echo: s.echoCancellation === true, agc: s.autoGainControl === true };
}
