// Capture for a call (flow 06 § States): the microphone, with listen-only outcomes instead of errors, the camera and
// the screen with whatever sound the browser gives (#37). Nothing here publishes; the session does.
import { MIC_CAPTURE, SCREEN_AUDIO_CAPTURE, SCREEN_DISPLAY_HINTS } from '@dilla/media';
import { DEFAULT_SHARE_OPTIONS, shareVideoConstraints, type ShareOptions } from './share-options.ts';

export interface CaptureMedia {
  getUserMedia(constraints: MediaStreamConstraints): Promise<MediaStream>;
  enumerateDevices(): Promise<MediaDeviceInfo[]>;
}
export interface DisplayMedia {
  getDisplayMedia(options: DisplayMediaStreamOptions): Promise<MediaStream>;
}

export type MicResult = { track: MediaStreamTrack } | { mic: 'denied' | 'missing' };

const stopAll = (s: MediaStream): void => { for (const t of s.getTracks()) t.stop(); };

/** Signal task 10b: `processing` is Settings → Voice's echo cancellation and automatic gain (both on by default). */
function micConstraints(input: string, noise = true, processing?: { echo: boolean; agc: boolean }): MediaStreamConstraints {
  const capture = { echoCancellation: processing?.echo ?? MIC_CAPTURE.echoCancellation, noiseSuppression: noise,
    autoGainControl: processing?.agc ?? MIC_CAPTURE.autoGainControl } as MediaTrackConstraints;
  return { audio: input === '' ? capture : { deviceId: { exact: input }, ...capture } };
}

/**
 * The microphone: 'denied' when the person or the browser refused it, 'missing' when there is none (none listed, not
 * found, or unreadable). A chosen input that is gone falls back to the system default once.
 */
export async function acquireMic(media: CaptureMedia, input: string, noise = true, processing?: { echo: boolean; agc: boolean }): Promise<MicResult> {
  let devices: MediaDeviceInfo[] = [];
  try {
    devices = await media.enumerateDevices();
  } catch {
    devices = [];
  }
  if (!devices.some((d) => d.kind === 'audioinput')) return { mic: 'missing' };
  const attempt = async (id: string): Promise<MicResult | 'retry'> => {
    try {
      const stream = await media.getUserMedia(micConstraints(id, noise, processing));
      const track = stream.getAudioTracks()[0];
      if (track === undefined) {
        stopAll(stream);
        return { mic: 'missing' };
      }
      for (const t of stream.getTracks()) if (t !== track) t.stop();
      return { track };
    } catch (err) {
      const name = (err as { name?: unknown } | null)?.name;
      if (name === 'NotAllowedError' || name === 'SecurityError') return { mic: 'denied' };
      if (name === 'OverconstrainedError' && id !== '') return 'retry';
      return { mic: 'missing' };
    }
  };
  const first = await attempt(input);
  if (first !== 'retry') return first;
  const second = await attempt('');
  return second === 'retry' ? { mic: 'missing' } : second;
}

export async function acquireCamera(media: CaptureMedia): Promise<MediaStreamTrack> {
  const stream = await media.getUserMedia({ audio: false, video: true });
  const track = stream.getVideoTracks()[0];
  if (track === undefined) {
    stopAll(stream);
    throw Object.assign(new Error('E_UNAVAILABLE: no camera track'), { name: 'NotFoundError' });
  }
  return track;
}

/** What sound a share carries: a tab's, the system's (a screen or a window), or none (#37). */
export type ShareAudioKind = 'tab' | 'system' | 'app' | 'none';
export interface ScreenCapture { video: MediaStreamTrack; audio: MediaStreamTrack | null; audioKind: ShareAudioKind }

/**
 * The screen, with whatever sound the browser gives for the surface picked (MDN browser-compat-data,
 * getDisplayMedia `audio_capture_support`; https://developer.chrome.com/docs/web-platform/screen-sharing-controls):
 * - Chrome and Edge give a tab's sound everywhere.
 * - On Windows and ChromeOS they give a screen's (and, Chrome 141+, a window's) system sound when the person ticks
 *   "Also share system audio"; on macOS 14.2+ with Chrome 141+ too.
 * - On Linux only a tab's sound; Firefox and Safari give none (https://bugzilla.mozilla.org/show_bug.cgi?id=1541425).
 * The call's own playback is asked to be left out (`restrictOwnAudio`, Chrome 141+) and never checked (D5). The audio
 * hints are stated, not left to defaults. Whatever audio comes back is returned, and the session publishes it.
 *
 * Signal task 8c: the picker's options shape the request: the surface the browser's picker opens on, the largest frame and
 * the frame rate (`shareVideoConstraints`), and sound only with `systemAudio` (off: `audio: false`, and the system and window
 * sound hints say exclude, so the browser offers none).
 */
export async function acquireScreen(media: DisplayMedia, o: ShareOptions = DEFAULT_SHARE_OPTIONS): Promise<ScreenCapture> {
  const hints = o.systemAudio ? SCREEN_DISPLAY_HINTS : { ...SCREEN_DISPLAY_HINTS, systemAudio: 'exclude' as const, windowAudio: 'exclude' as const };
  const stream = await media.getDisplayMedia({ video: shareVideoConstraints(o), audio: o.systemAudio ? { ...SCREEN_AUDIO_CAPTURE } : false, ...hints });
  const video = stream.getVideoTracks()[0];
  if (video === undefined) {
    stopAll(stream);
    throw Object.assign(new Error('E_UNAVAILABLE: no screen track'), { name: 'NotFoundError' });
  }
  const audio = stream.getAudioTracks()[0] ?? null;
  if (audio === null) return { video, audio: null, audioKind: 'none' };
  return { video, audio, audioKind: video.getSettings().displaySurface === 'browser' ? 'tab' : 'system' };
}

export { shareAudioAdvice, type ShareAudioAdvice } from './share-audio-advice.ts';
